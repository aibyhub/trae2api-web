package tlsfp

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 真实客户端基线：2026-10-02 透明中继实测（hello.jsonl raw_hex 解析）。
// JA3 字符串为标准十进制格式（Salesforce 规范，与 peet.ws/ja3er 一致），
// cipher/扩展顺序逐位敏感。
const (
	wantTraeJA3String = "771,49196-49195-49200-49199-49188-49187-49192-49191-49162-49161-49172-49171-157-156-61-60-53-47,0-10-11-13-35-23-65281,29-23-24,0"
	wantTraeJA3MD5    = "6a5d235ee78c6aede6a61448b4e9ff1e" // peet.ws 实测复核
	wantTraeSigAlgs   = "0804-0805-0806-0401-0501-0201-0403-0503-0203-0202-0601-0603"
)

// helloInfo 从 ClientHello 原始字节提取 JA3 相关字段。
type helloInfo struct {
	ciphers []string
	exts    []int
	curves  []int
	epf     []int
	sigAlgs []string
	hasALPN bool
	hasSVer bool
	rawHex  string
}

// captureHelloBytes 用 net.Pipe 收下 UConn 发出的第一条 TLS 记录（ClientHello）。
// 管道对端不回应，握手会失败——字节已到手，纯离线无需网络。
// serverName 用真实上游域名：SNI 扩展只有在域名字段下才会发出（IP 会省略，
// 与真实客户端/标准库行为一致）。
func captureHelloBytes(t *testing.T, serverName string) helloInfo {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	type result struct {
		buf []byte
	}
	ch := make(chan result, 1)
	go func() {
		defer server.Close()
		head := make([]byte, 5)
		if err := server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		if _, err := io.ReadFull(server, head); err != nil {
			return
		}
		recLen := int(head[3])<<8 | int(head[4])
		rest := make([]byte, recLen)
		if _, err := io.ReadFull(server, rest); err != nil {
			return
		}
		ch <- result{append(append([]byte{}, head...), rest...)}
	}()

	uconn, err := newFingerprintUConn(client, serverName)
	if err != nil {
		t.Fatal(err)
	}
	_ = uconn.SetDeadline(time.Now().Add(5 * time.Second))
	// 握手发出 ClientHello 后等待对端回应；对端不回，超时即达标。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = uconn.HandshakeContext(ctx)

	select {
	case r := <-ch:
		return parseHelloBytes(t, r.buf)
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for ClientHello capture")
		return helloInfo{}
	}
}

func parseHelloBytes(t *testing.T, buf []byte) helloInfo {
	t.Helper()
	if len(buf) < 5 || buf[0] != 0x16 {
		t.Fatalf("not a TLS handshake record: % x", buf[:min(8, len(buf))])
	}
	recLen := int(buf[3])<<8 | int(buf[4])
	hs := buf[5 : 5+recLen]
	if hs[0] != 0x01 {
		t.Fatalf("not ClientHello: type=%d", hs[0])
	}
	ch := hs[4:]
	p := 2 + 32
	sidLen := int(ch[p]); p += 1 + sidLen
	csLen := int(binary.BigEndian.Uint16(ch[p : p+2])); p += 2
	var ciphers []string
	for i := 0; i < csLen; i += 2 {
		c := binary.BigEndian.Uint16(ch[p+i : p+i+2])
		ciphers = append(ciphers, strconv.Itoa(int(c)))
	}
	p += csLen
	compLen := int(ch[p]); p += 1 + compLen
	extLen := int(binary.BigEndian.Uint16(ch[p : p+2])); p += 2
	end := p + extLen
	info := helloInfo{ciphers: ciphers}
	for p+4 <= end {
		et := int(binary.BigEndian.Uint16(ch[p : p+2]))
		el := int(binary.BigEndian.Uint16(ch[p+2 : p+4]))
		data := ch[p+4 : p+4+el]
		p += 4 + el
		info.exts = append(info.exts, et)
		switch et {
		case 10: // supported_groups
			n := int(binary.BigEndian.Uint16(data[0:2]))
			for i := 0; i < n; i += 2 {
				info.curves = append(info.curves, int(binary.BigEndian.Uint16(data[2+i:2+i+2])))
			}
		case 11: // ec_point_formats
			n := int(data[0])
			for i := 0; i < n; i++ {
				info.epf = append(info.epf, int(data[1+i]))
			}
		case 13: // signature_algorithms
			n := int(binary.BigEndian.Uint16(data[0:2]))
			for i := 0; i < n; i += 2 {
				info.sigAlgs = append(info.sigAlgs, hex.EncodeToString(data[2+i:2+i+2]))
			}
		case 16:
			info.hasALPN = true
		case 43:
			info.hasSVer = true
		}
	}
	info.rawHex = hex.EncodeToString(buf)
	return info
}

func ja3String(h helloInfo) string {
	exts := make([]string, len(h.exts))
	for i, e := range h.exts {
		exts[i] = strconv.Itoa(e)
	}
	curves := make([]string, len(h.curves))
	for i, c := range h.curves {
		curves[i] = strconv.Itoa(c)
	}
	epf := make([]string, len(h.epf))
	for i, c := range h.epf {
		epf[i] = strconv.Itoa(c)
	}
	return strings.Join([]string{
		"771",
		strings.Join(h.ciphers, "-"),
		strings.Join(exts, "-"),
		strings.Join(curves, "-"),
		strings.Join(epf, "-"),
	}, ",")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestTraeSpecOfflineJA3 离线逐字节校验：我们的 ClientHello 与真实 Trae 客户端
// ahanet 形态 JA3 完全一致（含 SNI 扩展位）。
func TestTraeSpecOfflineJA3(t *testing.T) {
	info := captureHelloBytes(t, "api.trae.com.cn")
	got := ja3String(info)
	if got != wantTraeJA3String {
		t.Fatalf("ja3 string mismatch\n got: %s\nwant: %s\nraw:  %s", got, wantTraeJA3String, info.rawHex)
	}
	sum := fmt.Sprintf("%x", md5.Sum([]byte(got)))
	if sum != wantTraeJA3MD5 {
		t.Fatalf("ja3 md5 mismatch: got %s want %s", sum, wantTraeJA3MD5)
	}
	if info.hasALPN {
		t.Errorf("unexpected ALPN extension (real client has none)")
	}
	if info.hasSVer {
		t.Errorf("unexpected supported_versions extension (real client is TLS1.2-only)")
	}
	if got := strings.Join(info.sigAlgs, "-"); got != wantTraeSigAlgs {
		t.Errorf("sig_algs mismatch: got %s want %s", got, wantTraeSigAlgs)
	}
}

// TestTraeSpecOnlineJA3 在线端到端校验（-short 跳过）：把带指纹的握手打到
// tls.peet.ws，断言探测端看到的 ja3 与真实客户端一致。
func TestTraeSpecOnlineJA3(t *testing.T) {
	if testing.Short() {
		t.Skip("online probe skipped in -short mode")
	}
	d, err := NewDialTLSContext(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{DialTLSContext: d},
		Timeout:   20 * time.Second,
	}
	resp, err := client.Get("https://tls.peet.ws/api/all")
	if err != nil {
		t.Skipf("online probe unavailable: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		TLS struct {
			JA3Hash string `json:"ja3_hash"`
			JA3     string `json:"ja3"`
		} `json:"tls"`
	}
	if err := json.NewDecoder(bufio.NewReader(resp.Body)).Decode(&out); err != nil {
		t.Fatalf("decode peet.ws response: %v", err)
	}
	if out.TLS.JA3Hash != wantTraeJA3MD5 {
		t.Fatalf("server-side ja3 mismatch: got %s want %s\nja3: %s", out.TLS.JA3Hash, wantTraeJA3MD5, out.TLS.JA3)
	}
	if out.TLS.JA3 != wantTraeJA3String {
		t.Fatalf("server-side ja3 string mismatch:\n got: %s\nwant: %s", out.TLS.JA3, wantTraeJA3String)
	}
}
