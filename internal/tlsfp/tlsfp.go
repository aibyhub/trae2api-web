// Package tlsfp 出站 TLS 指纹复刻（utls）：把网关的 ClientHello 伪装成真实 Trae
// 客户端（ahaNet 栈）的形态，消除 Go 默认 TLS 栈在 JA3/JA4 层的可识别性。
//
// 指纹参数来源：2026-10-02 对本机真实 Trae SOLO CN 的本地透明中继字节级实测
// （api.trae.com.cn 两次连接逐字节一致；采集存档 workspace tmp/trae-fp-recon-20261002/
// hello.jsonl 含 raw_hex）。任何参数调整必须先重新抓包比对——扩展顺序、密码套件
// 顺序对 JA3 都是逐位敏感的，凭记忆改动 = 指纹漂移 = 伪装失效。
//
// 形态摘要（ja3_md5 1c2c525d82e765c1db6bc2f274eff51c）：
//   TLS 1.2 封顶（无 supported_versions）、18 个纯 TLS1.2 ECDHE cipher、
//   7 个扩展（SNI/groups/points/sigalgs/ticket/EMS/renego）、曲线 x25519-p256-p384、
//   无 ALPN（即 HTTP/1.1）、无 GREASE。
package tlsfp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
	utls "github.com/refraction-networking/utls"
)

// HandshakeTimeout tlsfp 握手硬上限。自定义 DialTLSContext 生效后标准库的
// TLSHandshakeTimeout 不再兜底，必须自带；10s 对 TCP 内网+公网握手都宽裕。
const HandshakeTimeout = 10 * time.Second

// ahanetSigAlgs 真实客户端 signature_algorithms（12 项，实测原序，含 0x0202）。
var ahanetSigAlgs = []utls.SignatureScheme{
	0x0804, 0x0805, 0x0806, 0x0401, 0x0501, 0x0201,
	0x0403, 0x0503, 0x0203, 0x0202, 0x0601, 0x0603,
}

// ahanetCiphers 真实客户端密码套件（18 项，实测原序，纯 TLS1.2 ECDHE）。
var ahanetCiphers = []uint16{
	0xc02c, 0xc02b, 0xc030, 0xc02f, 0xc024, 0xc023, 0xc028, 0xc027,
	0xc00a, 0xc009, 0xc014, 0xc013, 0x009d, 0x009c, 0x003d, 0x003c,
	0x0035, 0x002f,
}

// TraeSpec 构造 ahanet 形态的 ClientHelloSpec。
// 扩展切片的顺序 = 线上扩展顺序，勿重排。
// TLSVersMax=1.2 且不设 SupportedVersions 扩展：真实客户端为 TLS 1.2 封顶形态。
func TraeSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:   tls.VersionTLS12,
		TLSVersMax:   tls.VersionTLS12,
		CipherSuites: ahanetCiphers,
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519, utls.CurveP256, utls.CurveP384,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: ahanetSigAlgs},
			&utls.SessionTicketExtension{},
			&utls.UtlsExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
		},
	}
}

// dialTLS 按 profile 拨号并完成 utls 握手：先建 TCP（直连或代理隧道），
// 再在隧道出口端做带指纹的 TLS 握手——ClientHello 字节与出口 IP 下的真实客户端一致。
type dialTLS struct {
	base     *net.Dialer // TCP 层参数（直连、代理 CONNECT）；nil = 默认拨号器
	proxyURL *url.URL    // nil = 直连
}

// NewDialTLSContext 构造 http.Transport.DialTLSContext。
// base 提供 TCP 层参数（超时/keepalive），nil 用默认拨号器。
// proxyURL 支持 socks5/socks5h/http（CONNECT）；https 代理返回错误（TLS-to-proxy
// 双层握手不做指纹，调用方应回退标准 transport——与 sub2api 同策略）。
func NewDialTLSContext(base *net.Dialer, proxyURL *url.URL) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if base == nil {
		base = &net.Dialer{}
	}
	d := dialTLS{base: base, proxyURL: proxyURL}
	if proxyURL != nil {
		switch strings.ToLower(proxyURL.Scheme) {
		case "socks5", "socks5h", "http":
		default:
			return nil, fmt.Errorf("tlsfp: unsupported proxy scheme %q (want socks5/http)", proxyURL.Scheme)
		}
	}
	return d.DialTLSContext, nil
}

// DialTLSContext 建立 TCP（按需先穿代理隧道）并做 utls 指纹握手。
func (d dialTLS) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("tlsfp: unsupported network %q", network)
	}
	raw, err := d.dialTCP(ctx, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("tlsfp: split %q: %w", addr, err)
	}
	hctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	uconn, err := newFingerprintUConn(raw, host)
	if err != nil {
		raw.Close()
		return nil, err
	}
	if err := uconn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tlsfp: handshake %s: %w", addr, err)
	}
	return uconn, nil
}

// newFingerprintUConn 构造已装配 ahanet 指纹的 utls 连接（未握手）。
// 独立成函数：测试复用同一构造路径，保证「测的」与「用的」逐字节一致。
func newFingerprintUConn(raw net.Conn, host string) (*utls.UConn, error) {
	cfg := &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	uconn := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(TraeSpec()); err != nil {
		return nil, fmt.Errorf("tlsfp: apply preset: %w", err)
	}
	return uconn, nil
}

// dialTCP 建立到 addr 的 TCP 连接：直连、SOCKS5 隧道或 HTTP CONNECT 隧道。
func (d dialTLS) dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	u := d.proxyURL
	if u == nil {
		return d.base.DialContext(ctx, "tcp", addr)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if us := u.User; us != nil {
			pw, _ := us.Password()
			auth = &proxy.Auth{User: us.Username(), Password: pw}
		}
		sd, err := proxy.SOCKS5("tcp", u.Host, auth, d.base)
		if err != nil {
			return nil, fmt.Errorf("tlsfp: socks5 dialer: %w", err)
		}
		cd, ok := sd.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("tlsfp: socks5 dialer lacks ContextDialer")
		}
		return cd.DialContext(ctx, "tcp", addr)
	case "http":
		return dialHTTPConnect(ctx, d.base, u, addr)
	default:
		return nil, fmt.Errorf("tlsfp: unsupported proxy scheme %q", u.Scheme)
	}
}

// dialHTTPConnect 经 HTTP 代理建 CONNECT 隧道，返回隧道内明文连接。
func dialHTTPConnect(ctx context.Context, base *net.Dialer, u *url.URL, targetAddr string) (net.Conn, error) {
	proxyAddr := u.Host
	if u.Port() == "" {
		proxyAddr = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := base.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("tlsfp: connect proxy: %w", err)
	}
	req := "CONNECT " + targetAddr + " HTTP/1.1\r\nHost: " + targetAddr + "\r\n"
	if us := u.User; us != nil {
		pw, _ := us.Password()
		token := base64.StdEncoding.EncodeToString([]byte(us.Username() + ":" + pw))
		req += "Proxy-Authorization: Basic " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tlsfp: send CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tlsfp: read CONNECT response: %w", err)
	}
	if !strings.Contains(status, " 200") {
		conn.Close()
		return nil, fmt.Errorf("tlsfp: CONNECT via %s rejected: %s", u.Host, strings.TrimSpace(status))
	}
	// 吃掉剩余响应头直到空行，之后 conn 内即是目标站的明文字节流。
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("tlsfp: read CONNECT headers: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return conn, nil
}
