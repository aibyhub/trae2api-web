// usage.go 使用日志（每笔模型请求一条）+ 官方模型倍率查询。
//
// 日志：内存环形缓冲 + data/usage.jsonl 追加持久化（>8MB 轮转为 .1）。
// 倍率：来自上游 get_detail_param 的 consumption_rate（官方定义，只读），
// 消耗估算 = total_tokens × 官方倍率（相对值，非上游真实计费结果）。
package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UsageEntry 一笔模型请求的日志。
type UsageEntry struct {
	TS               int64   `json:"ts"` // unix ms
	UID              string  `json:"uid"`
	Model            string  `json:"model"`
	Stream           bool    `json:"stream"`
	Status           string  `json:"status"` // ok | error
	Error            string  `json:"error,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	CacheRead        int64   `json:"cache_read,omitempty"`     // 提示词缓存命中 tokens
	CacheCreation    int64   `json:"cache_creation,omitempty"` // 缓存创建 tokens
	Rate             float64 `json:"rate"`
	Cost             float64 `json:"cost"` // total_tokens × rate（相对估算）
	DurationMs       int64   `json:"duration_ms"`
}

// UsageStore 环形日志存储。
type UsageStore struct {
	mu      sync.Mutex
	entries []UsageEntry // 时间升序，尾部最新
	max     int
	path    string // jsonl 落盘路径；空 = 仅内存
}

const (
	usageMaxEntries  = 5000 // 低于此前 2000：面板图表需要更长的窗口（约 9 天的调用量）
	usageMaxFileSize = 8 << 20
)

// NewUsageStore 构建；path 非空时加载已有 jsonl 尾部并追加写。
func NewUsageStore(path string) *UsageStore {
	s := &UsageStore{max: usageMaxEntries, path: path}
	if path != "" {
		s.loadFile()
	}
	return s
}

func (s *UsageStore) loadFile() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // 首次无文件
	}
	for _, line := range splitLines(raw) {
		var e UsageEntry
		if json.Unmarshal(line, &e) == nil && e.TS > 0 {
			s.entries = append(s.entries, e)
		}
	}
	// 常规追加天然升序；但轮转副本回写 / 手工整理过文件可能乱序，
	// 趋势分桶以首末条为界，这里统一按 ts 升序兜底。
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].TS < s.entries[j].TS })
	if len(s.entries) > s.max {
		s.entries = s.entries[len(s.entries)-s.max:]
	}
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

// Path 返回日志落盘路径（空 = 仅内存）。
func (s *UsageStore) Path() string { return s.path }

// Stats 返回日志概况（条数与最早/最新时间，unix 秒）。
func (s *UsageStore) Stats() (entries int, oldest, newest int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries = len(s.entries)
	if entries > 0 {
		oldest, newest = s.entries[0].TS/1000, s.entries[entries-1].TS/1000
	}
	return entries, oldest, newest
}

// Prune 按保留期裁剪：keepDays < 0 = 不动作；0 = 清空；>0 = 只保留最近 N 天。
// 内存与落盘同时生效；轮转文件 .1 一并处理（全部过期则删除）。
// keepDays：< 0 = 不动作；0 = 清空；> 0 = 只保留最近 N 天。
func (s *UsageStore) Prune(keepDays int) (removed, kept int, err error) {
	if keepDays < 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return 0, len(s.entries), nil
	}
	var cutoff int64
	if keepDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -keepDays).UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]UsageEntry, 0, len(s.entries))
	for _, e := range s.entries {
		if keepDays == 0 || e.TS < cutoff {
			continue
		}
		next = append(next, e)
	}
	removed = len(s.entries) - len(next)
	s.entries = next
	if s.path != "" {
		if err = s.rewriteLocked(); err != nil {
			return removed, len(next), err
		}
		pruneRotatedUsage(s.path+".1", cutoff)
	}
	return removed, len(next), nil
}

// rewriteLocked 原子重写使用日志。
func (s *UsageStore) rewriteLocked() error {
	var buf bytes.Buffer
	for _, e := range s.entries {
		raw, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// pruneRotatedUsage 裁剪轮转文件（ts 为毫秒）；cutoff<=0 表示全删。
func pruneRotatedUsage(path string, cutoff int64) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	for _, line := range splitLines(raw) {
		var e UsageEntry
		if json.Unmarshal(line, &e) == nil && (cutoff == 0 || e.TS < cutoff) {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if buf.Len() == 0 {
		_ = os.Remove(path)
		return
	}
	_ = os.WriteFile(path, buf.Bytes(), 0o644)
}

// Add 记录一笔（内存 + 落盘）。
func (s *UsageStore) Add(e UsageEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	if len(s.entries) > s.max {
		s.entries = s.entries[len(s.entries)-s.max:]
	}
	if s.path == "" {
		return
	}
	if err := s.appendFileLocked(e); err != nil {
		log.Printf("usage log write: %v", err)
	}
}

func (s *UsageStore) appendFileLocked(e UsageEntry) error {
	if st, err := os.Stat(s.path); err == nil && st.Size() > usageMaxFileSize {
		_ = os.Rename(s.path, s.path+".1") // 轮转，覆盖上一代
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// Recent 返回最新 n 条（时间降序）。
func (s *UsageStore) Recent(n int) []UsageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.entries) {
		n = len(s.entries)
	}
	out := make([]UsageEntry, n)
	// 尾部往前拷贝 → 最新在前
	for i := 0; i < n; i++ {
		out[i] = s.entries[len(s.entries)-1-i]
	}
	return out
}

// Since 返回 ts >= cutoff 的条目（时间升序，调用方据此假设分桶/最新在前）。
// 拷贝后排序，不依赖写入顺序（轮转副本回写、手补历史都可能乱序）。
func (s *UsageStore) Since(cutoff int64) []UsageEntry {
	return s.Range(cutoff, 0)
}

// Range 返回 from <= ts < to 的条目（时间升序）；to<=0 表示不设上界。
// 环比窗口（前一时段）需要双重边界，普通查询用 Since。
func (s *UsageStore) Range(from, to int64) []UsageEntry {
	s.mu.Lock()
	out := make([]UsageEntry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.TS >= from && (to <= 0 || e.TS < to) {
			out = append(out, e)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

// Find 按 ts（unix ms）+ uid 定位单条使用记录（detail 兜底展示用）；找不到返回 ok=false。
func (s *UsageStore) Find(ts int64, uid string) (UsageEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].TS == ts && s.entries[i].UID == uid {
			return s.entries[i], true
		}
	}
	return UsageEntry{}, false
}

// UsageToday 今日（本地时区自然日）汇总。
type UsageToday struct {
	Requests         int     `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
}

func (s *UsageStore) Today() UsageToday {
	midnight := time.Now().Truncate(24 * time.Hour).UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	var t UsageToday
	for _, e := range s.entries {
		if e.TS < midnight {
			continue
		}
		t.Requests++
		t.PromptTokens += e.PromptTokens
		t.CompletionTokens += e.CompletionTokens
		t.Cost += e.Cost
	}
	t.Cost = round2(t.Cost)
	return t
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// sortEntriesByTimeDesc 面板展示辅助（Recent 已保证，留作 API 兜底）。
func sortEntriesByTimeDesc(es []UsageEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].TS > es[j].TS })
}

// parseTokenUsage 解析上游 token_usage（Anthropic 风格键为主，兼容 OpenAI 风格），
// 并带出提示词缓存的创建/命中 tokens。
func parseTokenUsage(m map[string]any) (prompt, completion, total, cacheRead, cacheCreation int64) {
	if m == nil {
		return 0, 0, 0, 0, 0
	}
	g := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := m[k].(float64); ok {
				return int64(v)
			}
		}
		return 0
	}
	prompt = g("input_tokens", "prompt_tokens")
	completion = g("output_tokens", "completion_tokens")
	total = g("total_tokens")
	cacheRead = g("cache_read_input_tokens")
	cacheCreation = g("cache_creation_input_tokens")
	if total < prompt+completion {
		total = prompt + completion
	}
	return prompt, completion, total, cacheRead, cacheCreation
}

// ---------------------------------------------------------------------------
// admin API
// ---------------------------------------------------------------------------

// usageAgg 使用记录聚合行（按账号 / 按模型两个维度共用）。
type usageAgg struct {
	Key        string  `json:"key"`
	Label      string  `json:"label,omitempty"`
	Requests   int     `json:"requests"`
	Fails      int     `json:"fails"`
	Prompt     int64   `json:"prompt_tokens"`
	Completion int64   `json:"completion_tokens"`
	Total      int64   `json:"total_tokens"`
	CacheRead  int64   `json:"cache_read"` // 提示词缓存命中 tokens
	Cost       float64 `json:"cost"`
	AvgLatency int64   `json:"avg_latency_ms"`
	MaxLatency int64   `json:"max_latency_ms,omitempty"` // 最慢单笔（上游慢首字节感知）
	Tps        float64 `json:"tps,omitempty"`            // 输出速率：Σ输出 tokens / Σ耗时秒（沿用 workbuddy 口径）
	sumMs      int64   // Σ DurationMs（均耗时/均速率基数，不落 JSON）
	Rate       float64 `json:"rate,omitempty"`
}

// adminUsage GET /admin/api/usage?window=72&limit=300：
// 时间窗（小时，0=全部）内的使用记录 + 总览 + 按账号 + 按模型聚合。
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	window := 0
	if v := r.URL.Query().Get("window"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			window = n
		}
	}
	limit := 300
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= usageMaxEntries {
			limit = n
		}
	}
	cutoff := int64(0)
	if window > 0 {
		cutoff = time.Now().Add(-time.Duration(window) * time.Hour).UnixMilli()
	}
	entries := h.usage.Since(cutoff)

	totals := usageAgg{Key: "total"}
	accAgg := map[string]*usageAgg{}
	modelAgg := map[string]*usageAgg{}
	for _, e := range entries {
		aggBump(&totals, e)
		acc, ok := accAgg[e.UID]
		if !ok {
			acc = &usageAgg{Key: e.UID}
			accAgg[e.UID] = acc
		}
		aggBump(acc, e)
		mm, ok := modelAgg[e.Model]
		if !ok {
			mm = &usageAgg{Key: e.Model, Rate: e.Rate}
			modelAgg[e.Model] = mm
		}
		aggBump(mm, e)
	}
	finalize := func(a *usageAgg, n int) {
		a.Cost = round2(a.Cost)
		if n > 0 {
			a.AvgLatency = a.AvgLatency / int64(n)
		}
		if a.sumMs > 0 {
			a.Tps = round2(float64(a.Completion) / (float64(a.sumMs) / 1000))
		}
	}
	finalize(&totals, totals.Requests)

	// 环比：上一等长窗口的 totals（window=0 无可比性，不返回）。
	var prev *usageAgg
	if window > 0 {
		now := time.Now()
		pre := h.usage.Range(now.Add(-2*time.Duration(window)*time.Hour).UnixMilli(),
			now.Add(-time.Duration(window)*time.Hour).UnixMilli())
		var pt usageAgg
		for _, e := range pre {
			aggBump(&pt, e)
		}
		finalize(&pt, pt.Requests)
		if pt.Requests > 0 {
			prev = &pt
		}
	}

	byAccount := make([]usageAgg, 0, len(accAgg))
	for _, a := range accAgg {
		finalize(a, a.Requests)
		if nick := h.nicknameFor(a.Key); nick != "" {
			a.Label = nick
		}
		byAccount = append(byAccount, *a)
	}
	sort.Slice(byAccount, func(i, j int) bool { return byAccount[i].Cost > byAccount[j].Cost })
	byModel := make([]usageAgg, 0, len(modelAgg))
	for _, m := range modelAgg {
		finalize(m, m.Requests)
		if m.Total > 0 {
			m.Cost = round2(m.Cost)
		}
		byModel = append(byModel, *m)
	}
	sort.Slice(byModel, func(i, j int) bool { return byModel[i].Cost > byModel[j].Cost })

	// 明细：窗口内最新在前，截断到 limit。
	// 每行附 day（服务器时区日期，与 call-details 文件命名一致）：
	// 浏览器此前用 UTC 的 toISOString 推日期，与容器时区错位导致明细按日查找 404
	// （北京时间 0-8 点的记录在多数浏览器上会写错文件名）。day 必发。
	recent := make([]dayEntry, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(recent) < limit; i-- {
		recent = append(recent, dayEntry{
			UsageEntry: entries[i],
			Day:        time.UnixMilli(entries[i].TS).Format("2006-01-02"),
		})
	}
	// 频次序列：服务端分桶聚合（粒度自适应：窗口 ≤48h 或跨度 ≤48h → 按小时，
	// 否则按日对齐服务器时区）。entries 有容量上限（5000 条），前端自行重排会
	// 把截断外的请求丢掉——看板趋势一律用这份 series。
	span := int64(0)
	if n := len(entries); n > 0 {
		span = entries[n-1].TS - entries[0].TS
	}
	gran := "day"
	if (window > 0 && window <= 48) || (window == 0 && span <= 48*3600_000) {
		gran = "hour"
	}
	trend := buildUsageSeries(entries, gran)

	writeJSON(w, http.StatusOK, map[string]any{
		"window_hours": window,
		"granularity":  gran,
		"series":       trend,
		"errs":         errStats(entries),
		"totals":       totals,
		"prev_totals":  prev,
		"by_account":   byAccount,
		"by_model":     byModel,
		"entries":      recent,
	})
}

// aggBump 聚合累加（totals/按账号/按模型共用）：tokens/消耗/失败/均耗时基数/最慢单笔。
func aggBump(a *usageAgg, e UsageEntry) {
	a.Requests++
	a.Prompt += e.PromptTokens
	a.Completion += e.CompletionTokens
	a.Total += e.TotalTokens
	a.CacheRead += e.CacheRead
	a.Cost += e.Cost
	a.AvgLatency += e.DurationMs
	a.sumMs += e.DurationMs
	if e.DurationMs > a.MaxLatency {
		a.MaxLatency = e.DurationMs
	}
	if e.Status != "ok" {
		a.Fails++
	}
}

// usageErrStat 错误分类统计（面板「错误透视」用）。
type usageErrStat struct {
	Kind   string `json:"kind"`
	Count  int    `json:"n"`
	LastTS int64  `json:"last_ts,omitempty"`
	Sample string `json:"sample,omitempty"` // 最新一条原文（错误全文弹窗/复制）
}

// classifyUsageErr 错误归类（面板 中文名由前端映射；这里只定 kind）。
func classifyUsageErr(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "全部账号不可用"), strings.Contains(t, "no_healthy"),
		strings.Contains(t, "无可用账号"), strings.Contains(t, "轮换预算耗尽"), strings.Contains(t, "轮换"):
		return "all_unavailable"
	case strings.Contains(t, "429"), strings.Contains(t, "rate limit"), strings.Contains(t, "限流"),
		strings.Contains(t, "冷却"):
		return "rate_limited"
	case strings.Contains(t, "tool call failed"), strings.Contains(t, "not one of"), strings.Contains(t, "400:"):
		return "call_failed"
	case strings.Contains(t, "timeout"), strings.Contains(t, "timed out"), strings.Contains(t, "deadline"),
		strings.Contains(t, "超时"):
		return "timeout"
	case strings.Contains(t, "connection"), strings.Contains(t, "reset"), strings.Contains(t, "refused"),
		strings.Contains(t, "eof"), strings.Contains(t, "handshake"), strings.Contains(t, "dial"),
		strings.Contains(t, "proxy"), strings.Contains(t, "network"), strings.Contains(t, "broken pipe"):
		return "network"
	case strings.Contains(t, "502"), strings.Contains(t, "503"), strings.Contains(t, "504"), strings.Contains(t, "5xx"),
		strings.Contains(t, "internal server"), strings.Contains(t, "bad gateway"):
		return "upstream_5xx"
	case strings.Contains(t, "401"), strings.Contains(t, "403"), strings.Contains(t, "unauthorized"),
		strings.Contains(t, "forbidden"), strings.Contains(t, "session dead"), strings.Contains(t, "token"):
		return "auth"
	case strings.Contains(t, "json"), strings.Contains(t, "parse"), strings.Contains(t, "mapping"), strings.Contains(t, "decode"):
		return "parse"
	default:
		return "other"
	}
}

// errStats 窗口内失败聚合：kind → 计数 + 最新时刻 + 最新原文样例。
func errStats(entries []UsageEntry) []usageErrStat {
	acc := map[string]*usageErrStat{}
	for _, e := range entries {
		if e.Status == "ok" || e.Error == "" {
			continue
		}
		k := classifyUsageErr(e.Error)
		s := acc[k]
		if s == nil {
			s = &usageErrStat{Kind: k}
			acc[k] = s
		}
		s.Count++
		if e.TS > s.LastTS {
			s.LastTS = e.TS
			s.Sample = e.Error
		}
	}
	out := make([]usageErrStat, 0, len(acc))
	for _, s := range acc {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].LastTS > out[j].LastTS
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// usageSeriesPoint 趋势单桶：请求量（含失败拆分）、tokens、估算消耗、平均/最慢单笔耗时与错误抽样。
type usageSeriesPoint struct {
	Bucket    string  `json:"b"` // "2006-01-02T15"（小时桶）或 "2006-01-02"（日桶）
	Requests  int64   `json:"req"`
	Fails     int64   `json:"fail"`
	Prompt    int64   `json:"pt"`
	Completed int64   `json:"ct"`
	CacheRead int64   `json:"cr"`
	Cost      float64 `json:"cost"`
	AvgMs     int64   `json:"ms"`
	MaxMs     int64   `json:"mx,omitempty"` // 桶内最慢单笔（>60s 即上游慢首字节高发时段）
	ErrText   string  `json:"err,omitempty"`
}

// buildUsageSeries 分桶 + 补空桶（首桶到末桶之间连续），AvgMs 用桶内请求数归一。
func buildUsageSeries(entries []UsageEntry, gran string) []usageSeriesPoint {
	if len(entries) == 0 {
		return []usageSeriesPoint{}
	}
	fmtKey := "2006-01-02"
	if gran == "hour" {
		fmtKey = "2006-01-02T15"
	}
	trunc := func(t time.Time) time.Time {
		if gran == "hour" {
			return t.Truncate(time.Hour)
		}
		// 日桶必须对齐服务器时区的自然日零点（time.Truncate 按绝对时间截断，
		// UTC+8 下会切在 08:00，与 call-details 文件日期错位）。
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	}
	agg := map[string]*usageSeriesPoint{}
	for i := range entries {
		e := entries[i]
		b := trunc(time.UnixMilli(e.TS)).Format(fmtKey)
		p := agg[b]
		if p == nil {
			p = &usageSeriesPoint{Bucket: b}
			agg[b] = p
		}
		p.Requests++
		if e.Status != "ok" {
			p.Fails++
			if p.ErrText == "" && e.Error != "" {
				p.ErrText = e.Error
			}
		}
		p.Prompt += e.PromptTokens
		p.Completed += e.CompletionTokens
		p.CacheRead += e.CacheRead
		p.Cost += e.Cost
		p.AvgMs += e.DurationMs
		if e.DurationMs > p.MaxMs {
			p.MaxMs = e.DurationMs
		}
	}
	start := trunc(time.UnixMilli(entries[0].TS))
	end := trunc(time.UnixMilli(entries[len(entries)-1].TS))
	out := make([]usageSeriesPoint, 0, 32)
	for capGuard := 0; capGuard < 800; capGuard++ {
		b := start.Format(fmtKey)
		p := agg[b]
		if p == nil {
			p = &usageSeriesPoint{Bucket: b}
		} else if p.Requests > 0 {
			p.AvgMs /= p.Requests
			p.Cost = round2(p.Cost)
		}
		out = append(out, *p)
		if !start.Before(end) {
			break
		}
		if gran == "day" {
			start = start.AddDate(0, 0, 1)
		} else {
			start = start.Add(time.Hour)
		}
	}
	return out
}

// dayEntry 使用记录 + 服务器时区日期（前端按此查 call-details 当日文件）。
type dayEntry struct {
	UsageEntry
	Day string `json:"day"`
}

// nicknameFor 账号昵称（面板展示用）。
func (h *Handler) nicknameFor(uid string) string {
	if a := h.cfg.Pool.AuthByUID(uid); a != nil {
		return a.Nickname
	}
	return ""
}

// drNonOfficialModels 非 Trae 官方面向用户的 config 条目：套餐伪模型
// （free-stack/combo，无独立倍率）与内部工具模型（summary = 会话标题生成）。
// 上游 is_invisible_to_user 对它们不生效（字段缺失），按 id 显式剔除。
var drNonOfficialModels = map[string]bool{"free-stack": true, "combo": true, "summary": true}

// adminUsageDetail GET /admin/api/usage/detail?day=YYYY-MM-DD&ts=<unix ms>&uid=<uid>：
// 返回单笔请求的完整明细（上游请求参数、返回内容、系统提示词）。
// 兜底链：day 缺失/查无 → 按服务器时区从 ts 重算日期重试 → 仍无则返回 usage 行
// 兜底数据（fallback:true，弹窗至少能看到错误全文与耗时，不再空白报错）。
func (h *Handler) adminUsageDetail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	day, tsS, uid := q.Get("day"), q.Get("ts"), q.Get("uid")
	var tsN int64
	if n, err := strconv.ParseInt(tsS, 10, 64); err == nil {
		tsN = n
	}
	serverDay := ""
	if tsN > 0 {
		serverDay = time.UnixMilli(tsN).Format("2006-01-02")
	}
	if day != "" && tsN > 0 {
		if d, err := h.details.Get(day, tsN, uid); err == nil {
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	// day 缺失或按客户端给的 day 没找到：以服务器时区日期为准再试一次
	// （旧前端/时区错位文件名都会在这里被纠正）。
	if serverDay != "" && serverDay != day {
		if d, err := h.details.Get(serverDay, tsN, uid); err == nil {
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	if tsN == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "ts 参数缺失或非法（unix ms）")
		return
	}
	// 归档缺失（过保留期 / 未落档 / 归档清空）：给出 usage 行兜底，错误正文完整可见。
	if e, ok := h.usage.Find(tsN, uid); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"fallback":      true,
			"day":           serverDay,
			"model":         e.Model,
			"status":        e.Status,
			"error":         e.Error,
			"uid":           e.UID,
			"duration_ms":   e.DurationMs,
			"ts":            e.TS,
			"stream":        e.Stream,
			"prompt_tokens": e.PromptTokens, "completion_tokens": e.CompletionTokens,
			"total_tokens": e.TotalTokens, "cache_read": e.CacheRead,
			"note": "该请求的完整归档已过保留期或未落档（如全账号不可用、请求早失败不产生上游响应）；以下为使用记录中的摘要信息",
		})
		return
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found", "明细档案不存在（已过 30 天保留期，且使用记录中无对应行）")
}

// adminRatesGet GET /admin/api/rates：官方模型倍率表（get_detail_param
// display_contact_config.consumption_rate，上游定义，只读）。
// 只列 Trae 自带模型：剔除自定义占位（is_custom_model）、内部模型
// （is_invisible_to_user，如 subagent）与套餐伪模型（free-stack/combo）；
// 按倍率升序（未知倍率垫底）。
func (h *Handler) adminRatesGet(w http.ResponseWriter, r *http.Request) {
	infos := h.fetchDynamicModels()
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		// 自定义占位：is_custom_model 标记 + custom_model_ 前缀双保险
		// （实测 CN 上游对 custom_model_* 的 is_custom_model 仍为 false）
		if mi.Custom || mi.Invisible || strings.HasPrefix(mi.ID, "custom_model_") || drNonOfficialModels[mi.ID] {
			continue
		}
		out = append(out, map[string]any{
			"id":             mi.ID,
			"name":           mi.Name,
			"rate":           mi.Rate,
			"fee_level":      mi.FeeLevel,
			"context_length": mi.ContextWindow,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := out[i]["rate"].(float64), out[j]["rate"].(float64)
		if (ri > 0) != (rj > 0) {
			return ri > 0 // 有倍率的在前
		}
		return ri < rj // 升序
	})
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "fetched_from": "upstream get_detail_param"})
}
