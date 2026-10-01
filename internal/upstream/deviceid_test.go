// deviceid_test.go 十进制设备号生成/校验（claim 9074 根因：hex32 不被上游接受）。
package upstream

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"trae2api-web/internal/auth"
)

func TestNewDeviceIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewDeviceID()
		if err != nil {
			t.Fatalf("NewDeviceID: %v", err)
		}
		if !IsDecimalDeviceID(id) {
			t.Fatalf("generated %q not accepted by IsDecimalDeviceID", id)
		}
		if len(id) != 16 || id[0] < '2' || id[0] > '7' {
			t.Fatalf("generated %q violates 16-digit / 2-7 leading rule", id)
		}
		seen[id] = true
	}
	if len(seen) != 200 {
		t.Errorf("expected 200 unique ids, got %d", len(seen))
	}
}

func TestIsDecimalDeviceID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"4003784254113003", true},                  // 真机 aha 设备号（16 位）
		{"3187245960124379", true},                  // 16 位
		{"123456789012", true},                      // 下限 12 位
		{"12345678901234567890", true},              // 上限 20 位
		{"24499c08d1a80d56921b532d94ca02c7", false}, // 旧 hex32 → 9074
		{"12345678901", false},                      // 11 位
		{"123456789012345678901", false},            // 21 位
		{"", false},
	}
	for _, c := range cases {
		if got := IsDecimalDeviceID(c.in); got != c.want {
			t.Errorf("IsDecimalDeviceID(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

// TestClientForProxy 无代理走默认客户端；配置代理后得到独立缓存客户端；
// 非法代理回退默认（且缓存，不刷日志）。
func TestClientForProxy(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at"}

	std, stream := c.clientFor(a)
	if std != c.HTTP || stream != c.StreamHTTP {
		t.Fatal("no proxy: should return default clients")
	}

	a.ProxyURL = "socks5://127.0.0.1:1080"
	p1Std, _ := c.clientFor(a)
	if p1Std == c.HTTP {
		t.Fatal("proxy: should return a dedicated client")
	}
	p1Std2, _ := c.clientFor(a)
	if p1Std != p1Std2 {
		t.Fatal("proxy client should be cached")
	}

	a.ProxyURL = "http://127.0.0.1:8888"
	p2Std, _ := c.clientFor(a)
	if p2Std == p1Std || p2Std == c.HTTP {
		t.Fatal("different proxy should yield a different client")
	}

	a.ProxyURL = "not a url"
	badStd, _ := c.clientFor(a)
	if badStd != c.HTTP {
		t.Fatal("invalid proxy should fall back to default")
	}

	// socks5h 必须被接受（与校验层同一套规则），不得静默回退直连。
	a.ProxyURL = "socks5h://127.0.0.1:1080"
	p3Std, _ := c.clientFor(a)
	if p3Std == c.HTTP {
		t.Fatal("socks5h must be accepted as a proxy, not silently fall back to direct")
	}
}

// TestValidProxyScheme 校验层与传输层必须共用同一套 scheme 规则。
func TestValidProxyScheme(t *testing.T) {
	ok := []string{"http", "https", "socks5", "socks5h"}
	bad := []string{"", "socks4", "ftp", "file", "SOCKS5x"}
	for _, s := range ok {
		if !ValidProxyScheme(s) {
			t.Errorf("ValidProxyScheme(%q)=false want true", s)
		}
	}
	for _, s := range bad {
		if ValidProxyScheme(s) {
			t.Errorf("ValidProxyScheme(%q)=true want false", s)
		}
	}
}

// TestIsTransportError 出口/网络故障不应被当成账号错误惩罚。
func TestIsTransportError(t *testing.T) {
	transport := []error{
		&url.Error{Op: "Post", URL: "https://api.trae.cn/x", Err: errors.New("net/http: HTTP/1.x transport connection broken: malformed HTTP response \"\\x00\\x00\\x12\\x04\"")},
		&url.Error{Op: "Post", URL: "https://api.trae.cn/x", Err: errors.New("proxyconnect tcp: dial tcp 10.0.0.1:10529: connect: connection refused")},
		errors.New("socks connect tcp 10.0.0.1:1080->api.trae.cn:443: connection reset by peer"),
	}
	for _, e := range transport {
		if !IsTransportError(e) {
			t.Errorf("IsTransportError(%v)=false want true", e)
		}
	}
	upstreamErr := &Error{Kind: ErrSoftRate, Status: 429, Msg: "too many requests"}
	if IsTransportError(upstreamErr) {
		t.Error("upstream HTTP error must not be treated as a transport error")
	}
	if IsTransportError(nil) {
		t.Error("nil must not be a transport error")
	}
}

// TestProxyTransportPinsHTTP1 回归（v1.2.8）：走代理的 transport 必须只谈 HTTP/1.1。
// 起因见 clientFor 注释：Clone() 会把带 "h2" 的 TLSClientConfig 复制过来却丢掉
// TLSNextProto，导致「宣告 h2、没有 h2 实现」，上游 h2 帧被按 HTTP/1.1 解析
// （malformed HTTP response "\x00\x00\x12\x04..."）——走代理的账号签到/额度全挂。
func TestProxyTransportPinsHTTP1(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", ProxyURL: "socks5://127.0.0.1:1080"}
	std, _ := c.clientFor(a)
	tr, ok := std.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("proxy client transport = %T, want *http.Transport", std.Transport)
	}
	if tr.TLSNextProto == nil {
		t.Fatal("proxy transport must set a non-nil TLSNextProto to disable auto HTTP/2")
	}
	if tr.TLSClientConfig != nil {
		for _, p := range tr.TLSClientConfig.NextProtos {
			if p == "h2" {
				t.Fatalf("proxy transport must not advertise h2 in ALPN, got %v", tr.TLSClientConfig.NextProtos)
			}
		}
	}
}
