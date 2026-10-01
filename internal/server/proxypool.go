// proxypool.go 代理池：集中添加若干代理，账号按名称选用（面板下拉）。
// 池子只存 名称→地址 + 最近一次健康探测（出口 IP / 延迟 / 状态码）；
// 账号 auth 文件里落盘的仍是解析后的完整地址（见 accounts.go 的 resolveProxyInput）。
// GET 返回脱敏地址，凭据不回显。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

func trimSpace(s string) string { return strings.TrimSpace(s) }

func errBadRequest(msg string) error { return errors.New(msg) }

// proxyErrHint 代理地址错误提示（scheme 规则统一见 upstream.ValidProxyScheme）。
const proxyErrHint = "代理地址需为 http/https/socks5/socks5h，如 socks5://user:pass@1.2.3.4:1080"

// proxyOK 把探测结果折算成「这个出口现在能用吗」：
// 经代理拿到公网回显（ipify 2xx + IP）最强；退一步，经代理想拿到上游 HTTP 响应
// （哪怕是 404——api.trae.cn 根路径本来就 404）也算连通。
func proxyOK(r upstream.ProxyProbeResult) bool {
	return r.OK || (r.Err == nil && r.Status != 0)
}

// ProxyEntry 代理池条目（含最近一次探测缓存，落盘 data/proxies.json）。
type ProxyEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`

	// Enabled 停用后不参与新账号自动分配（已在用的账号不受影响）；nil = 启用（向后兼容）。
	Enabled *bool `json:"enabled,omitempty"`

	CheckedAt   int64  `json:"checked_at,omitempty"` // 最近探测时间（unix 秒）；0 = 从未
	LastOK      bool   `json:"last_ok,omitempty"`
	LastStatus  int    `json:"last_status,omitempty"`
	LastLatency int64  `json:"last_latency_ms,omitempty"`
	LastExitIP  string `json:"last_exit_ip,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// IsEnabled 报告条目是否启用（nil = 启用）。
func (e ProxyEntry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// ProxyView 面板展示视图（地址脱敏 + 引用账号数 + 探测缓存）。
type ProxyView struct {
	Name        string `json:"name"`
	URLMask     string `json:"url_masked"`
	Enabled     bool   `json:"enabled"`
	Accounts    int    `json:"accounts"` // 引用该地址的账号数
	CheckedAt   int64  `json:"checked_at,omitempty"`
	LastOK      bool   `json:"last_ok"`
	LastStatus  int    `json:"last_status,omitempty"`
	LastLatency int64  `json:"last_latency_ms,omitempty"`
	LastExitIP  string `json:"last_exit_ip,omitempty"`
	LastError   string `json:"last_error,omitempty"`
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

// Add 新增（名称唯一，地址需 http/https/socks5/socks5h）。
func (p *ProxyPool) Add(name, rawURL string) error {
	name = trimSpace(name)
	rawURL = trimSpace(rawURL)
	if name == "" {
		return errBadRequest("名称不能为空")
	}
	if !validProxyURL(rawURL) {
		return errBadRequest(proxyErrHint)
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

// ProxyUpdate 编辑代理的可选字段：空字符串 = 不改；Enabled 非 nil = 设置启用状态。
type ProxyUpdate struct {
	Name    string
	URL     string
	Enabled *bool
}

// Update 修改池内条目（改名 / 改地址 / 启用停用），返回更新后的条目。
// 名称与地址都必须唯一（排除自身）；地址变了则清空旧探测缓存。
// 已引用旧地址的账号是否同步改写由调用方决定（rewriteAccountProxy）。
func (p *ProxyPool) Update(oldName string, up ProxyUpdate) (ProxyEntry, error) {
	newName := trimSpace(up.Name)
	newURL := trimSpace(up.URL)
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := -1
	for i, it := range p.items {
		if it.Name == oldName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProxyEntry{}, errBadRequest("代理不存在：" + oldName)
	}
	cur := p.items[idx]
	if newName == "" {
		newName = cur.Name
	}
	if newURL == "" {
		newURL = cur.URL
	}
	if !validProxyURL(newURL) {
		return ProxyEntry{}, errBadRequest(proxyErrHint)
	}
	for i, it := range p.items {
		if i == idx {
			continue
		}
		if it.Name == newName {
			return ProxyEntry{}, errBadRequest("名称已存在：" + newName)
		}
		if it.URL == newURL {
			return ProxyEntry{}, errBadRequest("该代理地址已存在（名称 " + it.Name + "）")
		}
	}
	if newURL != cur.URL {
		cur.CheckedAt, cur.LastOK, cur.LastStatus, cur.LastLatency, cur.LastExitIP, cur.LastError = 0, false, 0, 0, "", ""
	}
	cur.Name, cur.URL = newName, newURL
	if up.Enabled != nil {
		cur.Enabled = up.Enabled
	}
	p.items[idx] = cur
	if err := p.saveLocked(); err != nil {
		return ProxyEntry{}, err
	}
	return cur, nil
}

// AddBulk 批量添加（每行一条：`名称 地址` / `名称,地址` / `名称=地址` / 纯地址自动命名 代理N）。
// 每行独立处理，返回成功名称与失败明细（含原因），支持 # 注释与空行。
func (p *ProxyPool) AddBulk(text string) (added []string, failed []string) {
	auto := 0
	for _, raw := range strings.Split(text, "\n") {
		line := trimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawURL := splitBulkLine(line)
		if name == "" {
			for {
				auto++
				name = fmt.Sprintf("代理%d", auto)
				if _, ok := p.Resolve(name); !ok {
					break
				}
			}
		}
		if err := p.Add(name, rawURL); err != nil {
			failed = append(failed, line+" → "+err.Error())
			continue
		}
		added = append(added, name)
	}
	return added, failed
}

// splitBulkLine 拆分一行批量输入；只有地址时名称为空。
// 只在「分隔符右侧是合法代理地址」时才认为左侧是名称，避免把地址里的 = / , 误判。
func splitBulkLine(line string) (name, rawURL string) {
	for _, sep := range []string{"\t", " ", ",", "=", "|"} {
		i := strings.Index(line, sep)
		if i <= 0 || i+len(sep) >= len(line) {
			continue
		}
		l := trimSpace(line[:i])
		r := trimSpace(line[i+len(sep):])
		if validProxyURL(r) {
			return l, r
		}
	}
	return "", trimSpace(line)
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

// NameFor 反查地址对应的池内名称（账号列表展示用）；不存在返回 ""。
func (p *ProxyPool) NameFor(rawURL string) string {
	rawURL = trimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range p.items {
		if it.URL == rawURL {
			return it.Name
		}
	}
	return ""
}

// SetHealth 记录某代理最近一次探测结果并落盘（面板显示「出口 IP / 上次检测 / 状态」）。
func (p *ProxyPool) SetHealth(name string, r upstream.ProxyProbeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, it := range p.items {
		if it.Name != name {
			continue
		}
		it.CheckedAt = time.Now().Unix()
		it.LastOK = proxyOK(r)
		it.LastStatus = r.Status
		it.LastLatency = r.LatencyMs
		if r.ExitIP != "" {
			it.LastExitIP = r.ExitIP
		}
		// 只记录「拿不到响应」的传输层错误；上游 4xx/5xx 属正常回包，状态码单独展示。
		it.LastError = ""
		if r.Err != nil {
			it.LastError = r.Err.Error()
		}
		p.items[i] = it
		_ = p.saveLocked()
		return
	}
}

// LeastUsed 返回被账号引用次数最少的池内地址（新账号自动均衡分配用）；
// 空池返回 false。
func (p *ProxyPool) LeastUsed(inUse map[string]int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, bestN := "", -1
	for _, it := range p.items {
		if !it.IsEnabled() {
			continue // 停用的代理不再参与新账号自动分配
		}
		n := inUse[it.URL]
		if bestN == -1 || n < bestN {
			best, bestN = it.URL, n
		}
	}
	return best, bestN >= 0
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// adminProxiesGet GET /admin/api/proxies：池列表（地址脱敏 + 引用数 + 探测缓存）。
func (h *Handler) adminProxiesGet(w http.ResponseWriter, r *http.Request) {
	h.writeProxyList(w)
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
	// 新增后立即探测一次（短超时），面板马上能看到出口 IP / 延迟 / 健康。
	if pr, ok := h.probeAndStore(trimSpace(req.Name), 8*time.Second); ok {
		log.Printf("proxy added %s: ok=%v exit_ip=%s err=%v", req.Name, proxyOK(pr), pr.ExitIP, pr.Err)
	}
	h.writeProxyList(w)
}

// adminProxiesUpdate PUT /admin/api/proxies/{name} {name?, url?, also_accounts?}：
// 改名 / 改地址；also_accounts=true 时把引用旧地址的账号一并改写成新地址（含落盘）。
func (h *Handler) adminProxiesUpdate(w http.ResponseWriter, r *http.Request) {
	oldName := r.PathValue("name")
	var req struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		Enabled      *bool  `json:"enabled"`
		AlsoAccounts bool   `json:"also_accounts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	oldURL, _ := h.proxies.Resolve(oldName)
	updated, err := h.proxies.Update(oldName, ProxyUpdate{Name: req.Name, URL: req.URL, Enabled: req.Enabled})
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	changed := 0
	if req.AlsoAccounts && oldURL != "" && updated.URL != "" && oldURL != updated.URL {
		changed = h.rewriteAccountProxy(oldURL, updated.URL)
	}
	// 地址变了就重新探测一次，保证面板里的出口 IP / 延迟不是旧值。
	if oldURL != updated.URL {
		h.probeAndStore(updated.Name, 8*time.Second)
	}
	log.Printf("proxy updated: %s → %s（地址%s，启用=%v，同步账号 %d 个）",
		oldName, updated.Name, maskProxy(updated.URL), updated.IsEnabled(), changed)
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

// adminProxiesTest POST /admin/api/proxies/test {name} 或 {url}：经代理实探一次。
// 走 upstream.ProbeProxy（与真实请求同一套客户端构造），返回连通性 / 延迟 / 真实出口 IP。
// {name} 的结果会写回池内缓存，供面板长期展示。
func (h *Handler) adminProxiesTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	name := trimSpace(req.Name)
	raw := trimSpace(req.URL)
	if raw == "" && name != "" {
		resolved, ok := h.proxies.Resolve(name)
		if !ok {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "proxy not found: "+name)
			return
		}
		raw = resolved
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !upstream.ValidProxyScheme(u.Scheme) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "代理地址非法："+proxyErrHint)
		return
	}
	pr := h.cfg.Upstream.ProbeProxy(raw, 15*time.Second)
	if name != "" {
		h.proxies.SetHealth(name, pr)
	}
	out := map[string]any{
		"ok":         proxyOK(pr),
		"status":     pr.Status,
		"latency_ms": pr.LatencyMs,
		"exit_ip":    pr.ExitIP,
	}
	if name != "" {
		out["name"] = name
	}
	if pr.Err != nil {
		out["error"] = pr.Err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// adminProxiesTestAll POST /admin/api/proxies/test_all：并发探测池内全部代理（面板「全部检测」）。
func (h *Handler) adminProxiesTestAll(w http.ResponseWriter, r *http.Request) {
	h.ProbeAllProxies()
	h.writeProxyList(w)
}

// probeAndStore 探测单个池内代理并把结果写回缓存（同步）。
func (h *Handler) probeAndStore(name string, timeout time.Duration) (upstream.ProxyProbeResult, bool) {
	raw, ok := h.proxies.Resolve(name)
	if !ok {
		return upstream.ProxyProbeResult{}, false
	}
	pr := h.cfg.Upstream.ProbeProxy(raw, timeout)
	h.proxies.SetHealth(name, pr)
	return pr, true
}

// adminProxyURLGet GET /admin/api/proxies/{name}/url：按需回显完整地址（含凭据）。
// 列表接口一律脱敏；只有已通过 API Key 鉴权的显式请求才回显，用于面板「编辑」预填当前地址。
func (h *Handler) adminProxyURLGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	raw, ok := h.proxies.Resolve(name)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "proxy not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "url": raw})
}

// adminProxiesAddBulk POST /admin/api/proxies/bulk {text}：一次粘贴多行批量添加。
// 探测在后台异步进行（批量可能有几十条），面板稍后刷新即可看到出口 IP。
func (h *Handler) adminProxiesAddBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	if trimSpace(req.Text) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "内容为空")
		return
	}
	added, failed := h.proxies.AddBulk(req.Text)
	if len(added) > 0 {
		go func(names []string) {
			for _, n := range names {
				h.probeAndStore(n, 10*time.Second)
			}
		}(added)
	}
	log.Printf("proxy bulk add: +%d, failed=%d", len(added), len(failed))
	usage := h.proxyUsage()
	items := h.proxies.All()
	out := make([]ProxyView, 0, len(items))
	for _, it := range items {
		out = append(out, ProxyView{
			Name: it.Name, URLMask: maskProxy(it.URL), Enabled: it.IsEnabled(), Accounts: usage[it.URL],
			CheckedAt: it.CheckedAt, LastOK: it.LastOK, LastStatus: it.LastStatus,
			LastLatency: it.LastLatency, LastExitIP: it.LastExitIP, LastError: it.LastError,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "failed": failed, "proxies": out})
}

// ProbeAllProxies 并发探测池内全部代理并写回缓存（启动自检 + 面板「全部检测」）。
// 并发上限 4，单个探测 10s 超时；结果只写缓存，不影响任何账号状态。
func (h *Handler) ProbeAllProxies() {
	items := h.proxies.All()
	if len(items) == 0 {
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		go func(name, raw string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pr := h.cfg.Upstream.ProbeProxy(raw, 10*time.Second)
			h.proxies.SetHealth(name, pr)
			log.Printf("proxy probe %s: ok=%v status=%d exit_ip=%s latency=%dms err=%v",
				name, proxyOK(pr), pr.Status, pr.ExitIP, pr.LatencyMs, pr.Err)
		}(it.Name, it.URL)
	}
	wg.Wait()
}

// writeProxyList 输出池列表（脱敏 + 引用数 + 探测缓存）。
func (h *Handler) writeProxyList(w http.ResponseWriter) {
	usage := h.proxyUsage()
	items := h.proxies.All()
	out := make([]ProxyView, 0, len(items))
	for _, it := range items {
		out = append(out, ProxyView{
			Name:        it.Name,
			URLMask:     maskProxy(it.URL),
			Enabled:     it.IsEnabled(),
			Accounts:    usage[it.URL],
			CheckedAt:   it.CheckedAt,
			LastOK:      it.LastOK,
			LastStatus:  it.LastStatus,
			LastLatency: it.LastLatency,
			LastExitIP:  it.LastExitIP,
			LastError:   it.LastError,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}

// proxyUsage 统计每个代理地址被多少账号引用（面板显示「账号数」，便于均衡分配）。
func (h *Handler) proxyUsage() map[string]int {
	out := map[string]int{}
	for _, st := range h.cfg.Pool.List() {
		if a := h.cfg.Pool.AuthByUID(st.UID); a != nil && strings.TrimSpace(a.ProxyURL) != "" {
			out[strings.TrimSpace(a.ProxyURL)]++
		}
	}
	return out
}

// rewriteAccountProxy 把所有引用 old 地址的账号改写成 new（含落盘），返回改动数。
func (h *Handler) rewriteAccountProxy(old, new string) int {
	n := 0
	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || strings.TrimSpace(a.ProxyURL) != old {
			continue
		}
		a.ProxyURL = new
		if a.FilePath == "" {
			a.FilePath = auth.FilePathFor(h.cfg.AuthDir, st.UID)
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("proxy rewrite uid=%s: save failed: %v", st.UID, err)
			continue
		}
		n++
	}
	return n
}
