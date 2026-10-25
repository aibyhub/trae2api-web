// deviceproof.go ExchangeToken v3 的设备证明（DeviceProof）与设备信息（DeviceInfo）。
//
// 协议逆向自真机 1.107.1 out/main.js（docs/RESEARCH-CLIENT-REVERSE.md §3.4）：
//   - 设备首次使用时生成 EC P-256 密钥对（ZDe()：pkcs8 私钥 / spki 公钥，PEM），
//     客户端持久化在本地 UserStorage，网关对应落盘 data/device-key.json（0600）；
//   - DeviceProof.Signature = base64(ECDSA-SHA256(canonical, 私钥))，
//     canonical = "METHOD\nPATH\nClientID\nRefreshToken\nTimestamp\nNonce"
//     （eTe()：Timestamp=unix 秒，Nonce=16 随机字节的 hex）；
//   - DeviceInfo 携带 DevicePublicKey 等设备画像（j()），作为 JSON 对象随请求体提交；
//     header 形态（x-icube-device）才是 RSA-OAEP 加密，body 内为明文对象（真机实测）。
//
// 旧端点（EpExchange，无签名）当前仍工作：本文件全部逻辑只在 TW2A_EXCHANGE_V3
// 开启且设备密钥可用时启用，默认关闭、行为零变化。
package upstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"trae2api-web/internal/auth"
)

// exchangeV3Enabled TW2A_EXCHANGE_V3 开关（进程内读一次缓存，与 tlsFPEnabled 同风格）。
// 默认关（旧端点仍工作）；"1"/"true"/"on"/"yes" 开启。开启但设备密钥不可用时
// 仍回退旧端点（见 refreshLocked）。
var (
	exchangeV3Once sync.Once
	exchangeV3Val  bool
)

func exchangeV3Enabled() bool {
	exchangeV3Once.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("TW2A_EXCHANGE_V3")))
		exchangeV3Val = v == "1" || v == "true" || v == "on" || v == "yes"
	})
	return exchangeV3Val
}

// DeviceCredentials 安装级设备密钥对（EC P-256，PEM 形态，与真机 ZDe() 一致）。
type DeviceCredentials struct {
	PrivateKeyPEM string `json:"private_key_pem"`
	PublicKeyPEM  string `json:"public_key_pem"`
}

// LoadOrCreateDeviceCredentials 从 path 加载设备密钥对；不存在则生成并原子落盘
// （tmp+rename，0600）。path 为空返回错误（调用方决定是否降级）。
func LoadOrCreateDeviceCredentials(path string) (*DeviceCredentials, error) {
	if path == "" {
		return nil, errors.New("device key path empty")
	}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		var dc DeviceCredentials
		if json.Unmarshal(raw, &dc) == nil && dc.PrivateKeyPEM != "" && dc.PublicKeyPEM != "" {
			return &dc, nil
		}
		// 损坏文件走重建（下面统一逻辑）
	}
	dc, err := generateDeviceCredentials()
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return nil, err
	}
	if dir := dirOf(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return dc, nil
}

func generateDeviceCredentials() (*DeviceCredentials, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ec key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return &DeviceCredentials{
		PrivateKeyPEM: string(privPEM),
		PublicKeyPEM:  string(pubPEM),
	}, nil
}

// DeviceProof ExchangeToken v3 的设备证明三元组（真机 eTe() 返回形态）。
type DeviceProof struct {
	Signature string `json:"Signature"`
	Timestamp int64  `json:"Timestamp"`
	Nonce     string `json:"Nonce"`
}

// SignDeviceProof 构造 DeviceProof：
// canonical = METHOD \n PATH \n ClientID \n refreshToken \n Timestamp \n Nonce（\n 连接），
// Signature = base64(ECDSA-SHA256(canonical, 设备私钥))。
// method/path/clientID/refreshToken 任一为空即报错（签名语义不完整）。
func (dc *DeviceCredentials) SignDeviceProof(method, path, clientID, refreshToken string) (*DeviceProof, error) {
	if dc == nil || dc.PrivateKeyPEM == "" {
		return nil, errors.New("device credentials not loaded")
	}
	if method == "" || path == "" || clientID == "" || refreshToken == "" {
		return nil, errors.New("device proof: empty canonical component")
	}
	block, _ := pem.Decode([]byte(dc.PrivateKeyPEM))
	if block == nil {
		return nil, errors.New("device proof: bad private key pem")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("device proof: parse private key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("device proof: private key not EC")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	nonceHex := hex.EncodeToString(nonce)
	canonical := []byte(method + "\n" + path + "\n" + clientID + "\n" + refreshToken + "\n" +
		fmt.Sprintf("%d", now) + "\n" + nonceHex)
	digest := sha256.Sum256(canonical)
	sig, err := ecdsa.SignASN1(rand.Reader, ecKey, digest[:])
	if err != nil {
		return nil, err
	}
	return &DeviceProof{
		Signature: base64.StdEncoding.EncodeToString(sig),
		Timestamp: now,
		Nonce:     nonceHex,
	}, nil
}

// BuildDeviceInfo 构造 ExchangeToken v3 的 DeviceInfo 对象（真机 j() 字段集）。
// DevicePublicKey 为设备公钥 PEM 原文；平台画像用网关既有常量（DeviceBrand/OSVersion）。
func BuildDeviceInfo(a *auth.Auth, dc *DeviceCredentials) map[string]any {
	deviceName := "trae2api-web"
	platformCode := "SOLO_PC"
	info := map[string]any{
		"DeviceID":      a.DeviceID,
		"MachineID":     a.MachineID,
		"PlatformCode":  platformCode,
		"DeviceType":    "PC",
		"DeviceName":    deviceName,
		"DeviceModel":   DeviceBrand,
		"ClientVersion": IdeVersion,
		"DeviceBrand":   DeviceBrand,
		"DeviceCPU":     "",
		"OSInfo":        DeviceOSName,
		"OSVersion":     OSVersion,
	}
	if dc != nil {
		info["DevicePublicKey"] = dc.PublicKeyPEM
	}
	return info
}

// dirOf 返回路径目录部分（filepath.Dir 的包内薄封装，便于测试）。
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return ""
}

// parsePEMPubKey 解析 SPKI PEM → *ecdsa.PublicKey（验签/联调用）。
func parsePEMPubKey(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("bad public key pem")
	}
	anyKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ecKey, ok := anyKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("public key not EC")
	}
	return ecKey, nil
}
