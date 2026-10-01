// proxypool.go 代理池：集中添加若干代理，账号按名称选用（面板下拉）。
// 池子只存 名称→地址；账号 auth 文件里落盘的仍是解析后的完整地址
// （见 accounts.go 的 resolveProxy）。GET 返回脱敏地址，凭据不回显。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func trimSpace(s string) string { return strings.TrimSpace(s) }

func errBadRequest(msg string) error { return errors.New(msg) }

// ProxyEntry 代理池条目。
type ProxyEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ProxyView 面板展示视图（地址脱敏）。
type ProxyView struct {
	Name    string `json:"name"`
	URLMask string `json:"url_masked"`
}

// ProxyPool 代理池存储（data/proxies.json）。
type ProxyPool struct {
	mu    sync.Mutex
	path  string
	items []ProxyEntry
}

// NewProxyPool 加载；文件不存在时空池。
func NewProxyPool(path string) *ProxyPool {
	p := &ProxyPool{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &p.items)
	}
	return p
}

func (p *ProxyPool) saveLocked() error {
	if p.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// All 返回副本（按名称排序）。
func (p *ProxyPool) All() []ProxyEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ProxyEntry, len(p.items))
	copy(out, p.items)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Add 新增（名称唯一，地址需 http/https/socks5）。
func (p *ProxyPool) Add(name, rawURL string) error {
	name = trimSpace(name)
	rawURL = trimSpace(rawURL)
	if name == "" {
		return errBadRequest("名称不能为空")
	}
	if !validProxyURL(rawURL) {
		return errBadRequest("代理地址需为 http/https/socks5，如 socks5://user:pass@1.2.3.4:1080")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range p.items {
		if it.Name == name {
			return errBadRequest("名称已存在：" + name)
		}
		if it.URL == rawURL {
			return errBadRequest("该代理地址已存在（名称 " + it.Name + "）")
		}
	}
	p.items = append(p.items, ProxyEntry{Name: name, URL: rawURL})
	return p.saveLocked()
}

// Remove 删除（账号已引用的地址不受影响，只是池里不再列出）。
func (p *ProxyPool) Remove(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, it := range p.items {
		if it.Name == name {
			p.items = append(p.items[:i], p.items[i+1:]...)
			return p.saveLocked() == nil
		}
	}
	return false
}

// Resolve 名称 → 地址；不存在返回 false。
func (p *ProxyPool) Resolve(name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range p.items {
		if it.Name == name {
			return it.URL, true
		}
	}
	return "", false
}

// LeastUsed 返回被账号引用次数最少的池内地址（新账号自动均衡分配用）；
// 空池返回 false。
func (p *ProxyPool) LeastUsed(inUse map[string]int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, bestN := "", -1
	for _, it := range p.items {
		n := inUse[it.URL]
		if bestN == -1 || n < bestN {
			best, bestN = it.URL, n
		}
	}
	return best, bestN >= 0
}

// adminProxiesGet GET /admin/api/proxies：池列表（地址脱敏）。
func (h *Handler) adminProxiesGet(w http.ResponseWriter, r *http.Request) {
	items := h.proxies.All()
	out := make([]ProxyView, 0, len(items))
	for _, it := range items {
		out = append(out, ProxyView{Name: it.Name, URLMask: maskProxy(it.URL)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}

// adminProxiesAdd POST /admin/api/proxies {name, url}。
func (h *Handler) adminProxiesAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	if err := h.proxies.Add(req.Name, req.URL); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	h.writeProxyList(w)
}

// adminProxiesDelete DELETE /admin/api/proxies/{name}。
func (h *Handler) adminProxiesDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.proxies.Remove(name) {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "proxy not found")
		return
	}
	h.writeProxyList(w)
}

// adminProxiesTest POST /admin/api/proxies/test {name} 或 {url}：经代理请求一次
// api.trae.cn 首页，报告连通性与延迟（不消耗任何账号）。{name} 从池内解析。
func (h *Handler) adminProxiesTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	raw := trimSpace(req.URL)
	if raw == "" && trimSpace(req.Name) != "" {
		resolved, ok := h.proxies.Resolve(trimSpace(req.Name))
		if !ok {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "proxy not found: "+req.Name)
			return
		}
		raw = resolved
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "代理地址非法")
		return
	}
	hc := &http.Client{
		Timeout:   12 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
	}
	start := time.Now()
	resp, err := hc.Get("https://api.trae.cn/")
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "latency_ms": lat, "error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": resp.StatusCode < 500, "status": resp.StatusCode, "latency_ms": lat})
}

func (h *Handler) writeProxyList(w http.ResponseWriter) {
	items := h.proxies.All()
	out := make([]ProxyView, 0, len(items))
	for _, it := range items {
		out = append(out, ProxyView{Name: it.Name, URLMask: maskProxy(it.URL)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}
