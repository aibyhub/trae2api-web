// deviceid_test.go 十进制设备号生成/校验（claim 9074 根因：hex32 不被上游接受）。
package upstream

import (
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
		{"4003784254113003", true},  // 真机 aha 设备号（16 位）
		{"3187245960124379", true},  // 16 位
		{"123456789012", true},      // 下限 12 位
		{"12345678901234567890", true}, // 上限 20 位
		{"24499c08d1a80d56921b532d94ca02c7", false}, // 旧 hex32 → 9074
		{"12345678901", false},      // 11 位
		{"123456789012345678901", false}, // 21 位
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
}
