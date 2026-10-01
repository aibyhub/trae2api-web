// usage.go 使用日志（每笔模型请求一条）+ 模型倍率配置。
//
// 日志：内存环形缓冲 + data/usage.jsonl 追加持久化（>8MB 轮转为 .1）。
// 倍率：data/model_rates.json，{model: rate}，rate 为相对系数（默认 1），
// 消耗估算 = total_tokens × rate（用于模型间相对成本对比，非上游真实计费）。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// RateStore 模型倍率。
type RateStore struct {
	mu    sync.Mutex
	path  string
	rates map[string]float64
}

// NewRateStore 加载；文件不存在时全默认 1。
func NewRateStore(path string) *RateStore {
	r := &RateStore{path: path, rates: map[string]float64{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &r.rates)
	}
	return r
}

// Get 模型倍率，未配置 = 1。
func (r *RateStore) Get(model string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.rates[model]; ok && v >= 0 {
		return v
	}
	return 1
}

// All 返回副本。
func (r *RateStore) All() map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]float64, len(r.rates))
	for k, v := range r.rates {
		out[k] = v
	}
	return out
}

// Set 全量替换（校验：模型名非空，0 <= rate <= 1000）。
func (r *RateStore) Set(rates map[string]float64) error {
	clean := make(map[string]float64, len(rates))
	for m, v := range rates {
		if m == "" || v < 0 || v > 1000 {
			return os.ErrInvalid
		}
		clean[m] = v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path != "" {
		if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(clean, "", "  ")
		if err != nil {
			return err
		}
		tmp := r.path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, r.path); err != nil {
			return err
		}
	}
	r.rates = clean
	return nil
}

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

// adminUsage GET /admin/api/usage?limit=100：最近请求日志 + 今日汇总。
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= usageMaxEntries {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": h.usage.Recent(limit),
		"today":   h.usage.Today(),
	})
}

// adminRatesGet GET /admin/api/rates：模型倍率表。
func (h *Handler) adminRatesGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rates": h.rates.All()})
}

// adminRatesPut PUT /admin/api/rates：全量替换倍率表 {rates:{model:rate}}。
func (h *Handler) adminRatesPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	if err := h.rates.Set(req.Rates); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"倍率非法：模型名不能为空，倍率需在 0~1000 之间")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rates": h.rates.All(), "saved": true})
}
