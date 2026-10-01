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
	usageMaxEntries  = 2000
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

// Since 返回 ts >= cutoff 的条目（时间升序）。
func (s *UsageStore) Since(cutoff int64) []UsageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UsageEntry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.TS >= cutoff {
			out = append(out, e)
		}
	}
	return out
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

// parseTokenUsage 解析上游 token_usage（Anthropic 风格键为主，兼容 OpenAI 风格）。
func parseTokenUsage(m map[string]any) (prompt, completion, total int64) {
	if m == nil {
		return 0, 0, 0
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
	if total < prompt+completion {
		total = prompt + completion
	}
	return prompt, completion, total
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
	Cost       float64 `json:"cost"`
	AvgLatency int64   `json:"avg_latency_ms"`
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
		totals.Requests++
		totals.Prompt += e.PromptTokens
		totals.Completion += e.CompletionTokens
		totals.Total += e.TotalTokens
		totals.Cost += e.Cost
		totals.AvgLatency += e.DurationMs
		if e.Status != "ok" {
			totals.Fails++
		}
		acc, ok := accAgg[e.UID]
		if !ok {
			acc = &usageAgg{Key: e.UID}
			accAgg[e.UID] = acc
		}
		acc.Requests++
		acc.Prompt += e.PromptTokens
		acc.Completion += e.CompletionTokens
		acc.Total += e.TotalTokens
		acc.Cost += e.Cost
		acc.AvgLatency += e.DurationMs
		if e.Status != "ok" {
			acc.Fails++
		}
		mm, ok := modelAgg[e.Model]
		if !ok {
			mm = &usageAgg{Key: e.Model, Rate: e.Rate}
			modelAgg[e.Model] = mm
		}
		mm.Requests++
		mm.Prompt += e.PromptTokens
		mm.Completion += e.CompletionTokens
		mm.Total += e.TotalTokens
		mm.Cost += e.Cost
		mm.AvgLatency += e.DurationMs
		if e.Status != "ok" {
			mm.Fails++
		}
	}
	finalize := func(a *usageAgg, n int) {
		a.Cost = round2(a.Cost)
		if n > 0 {
			a.AvgLatency = a.AvgLatency / int64(n)
		}
	}
	finalize(&totals, totals.Requests)
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

	// 明细：窗口内最新在前，截断到 limit
	recent := make([]UsageEntry, 0, len(entries))
	for i := len(entries) - 1; i >= 0 && len(recent) < limit; i-- {
		recent = append(recent, entries[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window_hours": window,
		"totals":       totals,
		"by_account":   byAccount,
		"by_model":     byModel,
		"entries":      recent,
	})
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
