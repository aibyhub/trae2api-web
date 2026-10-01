// proxypool_test.go 代理池 CRUD / 引用名 / 探测缓存 / 代理输入解析（v1.2.9）。
package server

import (
	"path/filepath"
	"strings"
	"testing"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

func TestValidProxyURL(t *testing.T) {
	ok := []string{
		"http://1.2.3.4:8080",
		"https://user:pass@proxy.example:443",
		"socks5://user:pass@1.2.3.4:1080",
		"socks5h://user:pass@1.2.3.4:1080", // 与传输层统一后必须接受（旧版本会静默直连）
	}
	for _, s := range ok {
		if !validProxyURL(s) {
			t.Errorf("validProxyURL(%q)=false want true", s)
		}
	}
	bad := []string{"", "1.2.3.4:1080", "socks4://1.2.3.4:1080", "ftp://1.2.3.4"}
	for _, s := range bad {
		if validProxyURL(s) {
			t.Errorf("validProxyURL(%q)=true want false", s)
		}
	}
}

func TestProxyPoolCRUDAndHealth(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "proxies.json")
	p := NewProxyPool(fp)
	u1 := "socks5h://u:p@1.2.3.4:1080"
	u2 := "socks5://u:p@5.6.7.8:1080"
	if err := p.Add("RN", u1); err != nil {
		t.Fatalf("add RN: %v", err)
	}
	if err := p.Add("RN", u2); err == nil {
		t.Fatal("duplicate name must fail")
	}
	if err := p.Add("dr", u1); err == nil {
		t.Fatal("duplicate url must fail")
	}
	if err := p.Add("do", "1.2.3.4:1080"); err == nil {
		t.Fatal("scheme-less url must fail")
	}
	if err := p.Add("do", u2); err != nil {
		t.Fatalf("add do: %v", err)
	}
	if got := p.NameFor(u1); got != "RN" {
		t.Fatalf("NameFor=%q want RN", got)
	}
	if got := p.NameFor(""); got != "" {
		t.Fatalf("NameFor(empty)=%q want empty", got)
	}

	// 探测缓存写盘 / 重载
	p.SetHealth("RN", upstream.ProxyProbeResult{Status: 404, LatencyMs: 123, ExitIP: "9.9.9.9"})
	p2 := NewProxyPool(fp)
	seen := false
	for _, it := range p2.All() {
		if it.Name == "RN" {
			seen = true
			if it.LastExitIP != "9.9.9.9" || !it.LastOK || it.CheckedAt == 0 || it.LastLatency != 123 {
				t.Fatalf("health cache not persisted: %+v", it)
			}
		}
	}
	if !seen {
		t.Fatal("RN missing after reload")
	}

	// 改名 + 改地址：地址变了旧探测缓存必须清空
	upd, err := p.Update("RN", ProxyUpdate{Name: "RN2", URL: "socks5://u:p@7.7.7.7:1080"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.URL != "socks5://u:p@7.7.7.7:1080" || upd.Name != "RN2" {
		t.Fatalf("update=%+v", upd)
	}
	if _, ok := p.Resolve("RN"); ok {
		t.Fatal("old name must be gone after rename")
	}
	if got := p.NameFor(upd.URL); got != "RN2" {
		t.Fatalf("NameFor(new)=%q want RN2", got)
	}
	for _, it := range p.All() {
		if it.Name == "RN2" && (it.CheckedAt != 0 || it.LastExitIP != "") {
			t.Fatalf("health cache must reset when url changes: %+v", it)
		}
	}
	if _, err := p.Update("RN2", ProxyUpdate{Name: "do"}); err == nil {
		t.Fatal("renaming onto an existing name must fail")
	}
	if _, err := p.Update("RN2", ProxyUpdate{URL: "https://u:p@8.8.8.8:8443"}); err != nil {
		t.Fatalf("update url only: %v", err)
	}
	if got, _ := p.Resolve("RN2"); got != "https://u:p@8.8.8.8:8443" {
		t.Fatalf("resolve=%q", got)
	}
	// 只改名称，地址保持不变
	if upd2, err := p.Update("RN2", ProxyUpdate{Name: "RN3"}); err != nil || upd2.URL != "https://u:p@8.8.8.8:8443" {
		t.Fatalf("rename only: %+v, %v", upd2, err)
	}
	if _, err := p.Update("不存在", ProxyUpdate{Name: "x"}); err == nil {
		t.Fatal("updating a missing entry must fail")
	}
}

// TestProxyEnabledAndBulk 停用不参与自动分配 + 批量添加解析。
func TestProxyEnabledAndBulk(t *testing.T) {
	p := NewProxyPool(filepath.Join(t.TempDir(), "proxies.json"))
	u1 := "socks5://u:p@1.1.1.1:1080"
	if err := p.Add("a", u1); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !p.All()[0].IsEnabled() {
		t.Fatal("new entry must default to enabled")
	}
	// 停用后不再参与新账号自动分配
	no := false
	if _, err := p.Update("a", ProxyUpdate{Enabled: &no}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if p.All()[0].IsEnabled() {
		t.Fatal("entry must be disabled")
	}
	if _, ok := p.LeastUsed(map[string]int{}); ok {
		t.Fatal("disabled proxy must not be auto-assigned")
	}
	if _, ok := p.Resolve("a"); !ok {
		t.Fatal("disabled entry must still resolve (在用账号不受影响)")
	}
	yes := true
	if _, err := p.Update("a", ProxyUpdate{Enabled: &yes}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got, ok := p.LeastUsed(map[string]int{}); !ok || got != u1 {
		t.Fatalf("LeastUsed=%q,%v want %q", got, ok, u1)
	}

	// 批量添加：注释/空行忽略，名称可用 空白 / , / = 分隔，纯地址自动命名
	text := strings.Join([]string{
		"# 注释行",
		"",
		"b socks5://u:p@2.2.2.2:1080",
		"c,socks5://u:p@3.3.3.3:1080",
		"d=socks5h://u:p@4.4.4.4:1080",
		"socks5://u:p@5.5.5.5:1080",
		"bad 不是地址",
	}, "\n")
	added, failed := p.AddBulk(text)
	if len(added) != 4 {
		t.Fatalf("added=%v want 4", added)
	}
	if len(failed) != 1 {
		t.Fatalf("failed=%v want 1", failed)
	}
	if got := p.NameFor("socks5h://u:p@4.4.4.4:1080"); got != "d" {
		t.Fatalf("NameFor(d)=%q", got)
	}
	autoNamed := false
	for _, it := range p.All() {
		if strings.HasPrefix(it.Name, "代理") {
			autoNamed = true
		}
	}
	if !autoNamed {
		t.Fatal("a bare url line should be auto-named 代理N")
	}
}

func newProxyHandler(t *testing.T) *Handler {
	t.Helper()
	pp := NewProxyPool(filepath.Join(t.TempDir(), "proxies.json"))
	if err := pp.Add("RN", "socks5h://u:p@1.1.1.1:1080"); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", ProxyURL: "socks5h://u:p@9.9.9.9:1080"})
	pl.Add(&auth.Auth{UID: "u2"})
	return &Handler{cfg: Config{Pool: pl}, proxies: pp}
}

// TestResolveProxyInput 面板/API 的代理值解析：池名 / 原始地址 / 直连 / 非法。
func TestResolveProxyInput(t *testing.T) {
	h := newProxyHandler(t)
	if got, err := h.resolveProxyInput("   "); err != nil || got != "" {
		t.Fatalf("empty → %q, %v", got, err)
	}
	if got, err := h.resolveProxyInput("RN"); err != nil || got != "socks5h://u:p@1.1.1.1:1080" {
		t.Fatalf("pool name → %q, %v", got, err)
	}
	if got, err := h.resolveProxyInput("http://8.8.8.8:3128"); err != nil || got != "http://8.8.8.8:3128" {
		t.Fatalf("raw url → %q, %v", got, err)
	}
	if _, err := h.resolveProxyInput("没有这个名字"); err == nil {
		t.Fatal("unknown name must be rejected")
	}
}

// TestPickImportProxy 导入/登录的出口决策：显式 > 保留已有 > 池内均衡。
func TestPickImportProxy(t *testing.T) {
	h := newProxyHandler(t)

	// 1) 已有账号的出口必须保留（重新导入/重登不得清空）
	got, err := h.pickImportProxy("", "u1")
	if err != nil || got != "socks5h://u:p@9.9.9.9:1080" {
		t.Fatalf("existing proxy not preserved: %q, %v", got, err)
	}
	// 2) 显式指定（池名）优先
	got, err = h.pickImportProxy("RN", "u1")
	if err != nil || got != "socks5h://u:p@1.1.1.1:1080" {
		t.Fatalf("explicit pool name → %q, %v", got, err)
	}
	// 3) 新账号从池内自动分配
	got, err = h.pickImportProxy("", "u2")
	if err != nil || got != "socks5h://u:p@1.1.1.1:1080" {
		t.Fatalf("auto assign → %q, %v", got, err)
	}
	// 4) 非法显式值报错
	if _, err := h.pickImportProxy("乱写的", "u2"); err == nil {
		t.Fatal("invalid explicit proxy must error")
	}
}
