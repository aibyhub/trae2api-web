// deviceregister.go 向上游注册独立设备号（aha applog 协议）。
//
// 协议逆向自 @byted/device-register（macOS 版 JS 实现，与 Windows 原生实现同源，
// 本地 storage.json 的 iCubeAuthInfo blob 亦使用同一 applog 封装格式）：
//
//	registry: POST {registryUrl}?aid&channel&os&os_version&device_platform&version_code&pc_uuid&pc_serial
//	          headers: Content-Type: application/json, User-Agent: TTNetwork PC（无签名）
//	          body:    applogDecorated(gzip(JSON{header:{设备指纹...}, _gen_time, magic_tag:"ss_app_log"}))
//	          resp:    {"device_id":..,"install_id":..,"new_user":..}
//	active:   POST {activeUrl}?aid&app_name&version_code&channel&os&device_platform&device_id&iid&pc_uuid&pc_serial
//
// applogDecorated：payload' = gzip(json)；plain = SHA512(payload') || payload'；
// key = 32 字节随机；aesKey = H[0:16]、iv = H[16:32]，其中
// H = SHA512( SHA512(key) || salt )，salt = salt_1 ^ salt_2（64 字节固定表）；
// 输出 = magic(74 63 05 10 00 00) || key || AES-128-CBC-PKCS7(plain)。
package upstream

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"trae2api-web/internal/auth"
)

const (
	drRegistryURL = "https://log.snssdk.com/service/2/desktop/device_register/"
	drActiveURL   = "https://log.snssdk.com/service/2/app_alert_check/"
	drAid         = 787976
	drChannel     = "stable"
	drPackage     = "com.trae.app"
	drDisplayName = "SOLO CN"
	drRegion      = "CN"
	drLanguage    = "zh-CN"
	drOSName      = "Windows"
	drOSPlatform  = "PC"
	drOSVersion   = "10.0.26200"
	drSDKName     = "0.0.2"
	drSDKCode     = 2
)

// applog salt 表（salt = salt_1 ^ salt_2），逐字取自 @byted/device-register。
var drSalt1 = [64]byte{
	0x52, 0x09, 0x6a, 0xd5, 0x30, 0x36, 0xa5, 0x38, 0xbf, 0x40, 0xa3, 0x9e, 0x81, 0xf3, 0xd7, 0xfb,
	0x7c, 0xe3, 0x39, 0x82, 0x9b, 0x2f, 0xff, 0x87, 0x34, 0x8e, 0x43, 0x44, 0xc4, 0xde, 0xe9, 0xcb,
	0x54, 0x7b, 0x94, 0x32, 0xa6, 0xc2, 0x23, 0x3d, 0xee, 0x4c, 0x95, 0x0b, 0x42, 0xfa, 0xc3, 0x4e,
	0x08, 0x2e, 0xa1, 0x66, 0x28, 0xd9, 0x24, 0xb2, 0x76, 0x5b, 0xa2, 0x49, 0x6d, 0x8b, 0xd1, 0x25,
}
var drSalt2 = [64]byte{
	0x1f, 0xdd, 0xa8, 0x33, 0x88, 0x07, 0xc7, 0x31, 0xb1, 0x12, 0x10, 0x59, 0x27, 0x80, 0xec, 0x5f,
	0x60, 0x51, 0x7f, 0xa9, 0x19, 0xb5, 0x4a, 0x0d, 0x2d, 0xe5, 0x7a, 0x9f, 0x93, 0xc9, 0x9c, 0xef,
	0xa0, 0xe0, 0x3b, 0x4d, 0xae, 0x2a, 0xf5, 0xb0, 0xc8, 0xeb, 0xbb, 0x3c, 0x83, 0x53, 0x99, 0x61,
	0x17, 0x2b, 0x04, 0x7e, 0xba, 0x77, 0xd6, 0x26, 0xe1, 0x69, 0x14, 0x63, 0x55, 0x21, 0x0c, 0x7d,
}

// drSalt 预计算 salt_1 ^ salt_2。
var drSalt = func() [64]byte {
	var out [64]byte
	for i := range out {
		out[i] = drSalt1[i] ^ drSalt2[i]
	}
	return out
}()

// applogDecorate 按 aha applog 协议封装：gzip → SHA512 前缀 → AES-128-CBC → 魔数头+随机密钥。
func applogDecorate(payload []byte) ([]byte, error) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(payload); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	gzipped := gz.Bytes()

	sum := sha512.Sum512(gzipped)
	plain := make([]byte, 0, sha512.Size+len(gzipped))
	plain = append(plain, sum[:]...)
	plain = append(plain, gzipped...)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	kh := sha512.Sum512(key)
	buf := make([]byte, 0, sha512.Size+64)
	buf = append(buf, kh[:]...)
	buf = append(buf, drSalt[:]...)
	bh := sha512.Sum512(buf)
	aesKey, iv := bh[:16], bh[16:32]

	// PKCS7 填充（与 node createCipheriv 默认一致）
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)

	// header: magic(6) || key(32)
	header := []byte{0x74, 0x63, 0x05, 0x10, 0x00, 0x00}
	return append(header, append(key, out...)...), nil
}

// applogDecrypt 逆变换（测试/调试用）：校验头、解 AES、验 SHA512、解 gzip。
func applogDecrypt(data []byte) ([]byte, error) {
	if len(data) < 6+32+aes.BlockSize {
		return nil, fmt.Errorf("payload too short")
	}
	if data[0] != 0x74 || data[1] != 0x63 || data[2] != 0x05 || data[3] != 0x10 || data[4] != 0x00 || data[5] != 0x00 {
		return nil, fmt.Errorf("bad magic")
	}
	key := data[6 : 6+32]
	kh := sha512.Sum512(key)
	buf := make([]byte, 0, sha512.Size+64)
	buf = append(buf, kh[:]...)
	buf = append(buf, drSalt[:]...)
	bh := sha512.Sum512(buf)
	aesKey, iv := bh[:16], bh[16:32]
	body := data[6+32:]
	if len(body)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bad length")
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, body)
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(plain) {
		return nil, fmt.Errorf("bad padding")
	}
	plain = plain[:len(plain)-pad]
	if len(plain) < sha512.Size {
		return nil, fmt.Errorf("missing hash")
	}
	sum := sha512.Sum512(plain[sha512.Size:])
	if !bytes.Equal(sum[:], plain[:sha512.Size]) {
		return nil, fmt.Errorf("hash mismatch")
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain[sha512.Size:]))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// drFingerprint 每次注册生成一套独立硬件指纹（账号间互不关联）。
type drFingerprint struct {
	UUID       string
	Serial     string
	MAC        string
	Model      string
	Resolution string
}

var drModels = []string{"83DG", "Legion_R9000P", "ROG_Strix_G16", "MateBook_X_Pro", "Surface_Pro_9"}
var drResolutions = []string{"2560x1440", "1920x1080", "3840x2160"}

func drRandMax(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// genDRFingerprint 生成随机但形态逼真的硬件指纹（UUID/磁盘序列号/MAC/机型/分辨率）。
func genDRFingerprint() drFingerprint {
	hexByte := func() string { return fmt.Sprintf("%02x", drRandMax(256)) }
	uid := fmt.Sprintf("%08X-%04X-%04X-%04X-%04X%04X%04X",
		drRandMax(0xFFFFFFF), drRandMax(0xFFFF), drRandMax(0xFFFF),
		drRandMax(0xFFFF), drRandMax(0xFFFF), drRandMax(0xFFFF), drRandMax(0xFFFF))
	serial := ""
	for i := 0; i < 12; i++ {
		serial += string(rune('A' + drRandMax(26)))
		if drRandMax(3) == 0 {
			serial += string(rune('0' + drRandMax(10)))
		}
	}
	mac := "02:" + hexByte() + ":" + hexByte() + ":" + hexByte() + ":" + hexByte() + ":" + hexByte()
	return drFingerprint{
		UUID:       uid,
		Serial:     serial,
		MAC:        mac,
		Model:      drModels[drRandMax(int64(len(drModels)))],
		Resolution: drResolutions[drRandMax(int64(len(drResolutions)))],
	}
}

// buildDRBody 构造注册 JSON（字段集与 @byted/device-register genRegistryRequestBody 一致）。
func buildDRBody(fp drFingerprint, now time.Time) map[string]any {
	header := map[string]any{
		"device_id":         0,
		"install_id":        0,
		"os":                drOSName,
		"device_platform":   drOSPlatform,
		"sdk_version":       drSDKName,
		"sdk_version_code":  drSDKCode,
		"aid":               drAid,
		"mc":                fp.MAC,
		"channel":           drChannel,
		"package":           drPackage,
		"language":          drLanguage,
		"app_version":       AppVersion,
		"os_version":        drOSVersion,
		"device_model":      fp.Model,
		"time_zone":         8,
		"tz_name":           "asia/shanghai",
		"tz_offset":         480,
		"resolution":        fp.Resolution,
		"app_region":        drRegion,
		"app_language":      drLanguage,
		"display_name":      drDisplayName,
		"new_user_mode":     0,
		"pc_uuid":           fp.UUID,
		"pc_serial":         fp.Serial,
	}
	return map[string]any{
		"header":    header,
		"_gen_time": now.Unix(),
		"magic_tag": "ss_app_log",
	}
}

// RegisterDevice 向上游注册一台新设备并激活，返回可用的 x-device-id。
// 传输走账号自身代理出口（clientFor），保证注册 IP 与该账号后续请求一致。
// version_code 采用 ahaNet 配置中的 "0164"（0.1.64 的 applog 形态）。
func (c *Client) RegisterDevice(a *auth.Auth) (string, error) {
	return c.registerDevice(a, time.Now())
}

func (c *Client) registerDevice(a *auth.Auth, now time.Time) (string, error) {
	fp := genDRFingerprint()
	body, _ := json.Marshal(buildDRBody(fp, now))
	payload, err := applogDecorate(body)
	if err != nil {
		return "", fmt.Errorf("decorate: %w", err)
	}
	q := url.Values{}
	q.Set("aid", fmt.Sprint(drAid))
	q.Set("channel", drChannel)
	q.Set("os", drOSName)
	q.Set("os_version", drOSVersion)
	q.Set("device_platform", drOSPlatform)
	q.Set("version_code", "0164")
	q.Set("pc_uuid", fp.UUID)
	q.Set("pc_serial", fp.Serial)

	req, err := http.NewRequest(http.MethodPost, drRegistryURL+"?"+q.Encode(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TTNetwork PC")
	hc, _ := c.clientFor(a)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("registry http %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var parsed struct {
		DeviceID  json.Number `json:"device_id"`
		InstallID json.Number `json:"install_id"`
		Message   string      `json:"message"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("registry parse: %w (%s)", err, truncate(string(raw), 120))
	}
	did := parsed.DeviceID.String()
	if did == "" || did == "0" {
		return "", fmt.Errorf("registry returned no device_id (%s)", truncate(string(raw), 120))
	}
	// 激活（best-effort，失败不影响设备号可用性）
	aq := url.Values{}
	aq.Set("aid", fmt.Sprint(drAid))
	aq.Set("app_name", "trae")
	aq.Set("version_code", "0164")
	aq.Set("channel", drChannel)
	aq.Set("os", drOSName)
	aq.Set("device_platform", drOSPlatform)
	aq.Set("device_id", did)
	aq.Set("iid", parsed.InstallID.String())
	aq.Set("pc_uuid", fp.UUID)
	aq.Set("pc_serial", fp.Serial)
	if areq, aerr := http.NewRequest(http.MethodPost, drActiveURL+"?"+aq.Encode(), nil); aerr == nil {
		areq.Header.Set("User-Agent", "TTNetwork PC")
		ahc, _ := c.clientFor(a)
		if aresp, aerr := ahc.Do(areq); aerr == nil {
			aresp.Body.Close()
		}
	}
	return did, nil
}
