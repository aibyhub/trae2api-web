// calldetails.go 调用明细存档：每笔模型请求的上游请求参数、返回内容与
// 系统提示词完整留档，面板「使用记录」点击行即可查看。
//
// 存储：data/call-details/YYYY-MM-DD.jsonl（一天一文件），
// 保留 30 天——写入时顺手清理过期文件。敏感字段（token 等）不落盘。
package server

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CallDetail 单笔请求的完整明细。
type CallDetail struct {
	TS                int64          `json:"ts"` // unix ms
	UID               string         `json:"uid"`
	Model             string         `json:"model"`
	UpstreamURL       string         `json:"upstream_url"`
	RequestBody       string         `json:"request_body"`        // 发给上游的最终 JSON（含系统提示词）
	SystemPrompt      string         `json:"system_prompt"`       // 单独提取，方便查看
	ResponseContent   string         `json:"response_content"`    // 模型正文
	ResponseReasoning string         `json:"response_reasoning,omitempty"`
	Usage             map[string]any `json:"usage,omitempty"`
	Status            string         `json:"status"`
	Error             string         `json:"error,omitempty"`
	DurationMs        int64          `json:"duration_ms"`
}

// DetailStore 明细存储（按天分文件）。
type DetailStore struct {
	mu       sync.Mutex
	dir      string
	retain   time.Duration
	lastPrune string // YYYY-MM-DD，一天只清一次
}

// detailRetain 明细保留时长（用户要求：最多一个月）。
const detailRetain = 30 * 24 * time.Hour

// NewDetailStore 构建；启动时清理过期文件。
func NewDetailStore(dir string) *DetailStore {
	s := &DetailStore{dir: dir, retain: detailRetain, lastPrune: ""}
	s.pruneOld(time.Now())
	return s
}

func dayFile(dir string, t time.Time) string {
	return filepath.Join(dir, "call-details-"+t.Format("2006-01-02")+".jsonl")
}

// Add 追加一条明细 + 每日一次的过期清理。
func (s *DetailStore) Add(d CallDetail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.lastPrune != now.Format("2006-01-02") {
		s.pruneOld(now)
		s.lastPrune = now.Format("2006-01-02")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	f, err := os.OpenFile(dayFile(s.dir, now), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// pruneOld 删除超过保留期的日文件。
func (s *DetailStore) pruneOld(now time.Time) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-s.retain)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "call-details-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, "call-details-"), ".jsonl")
		t, err := time.ParseInLocation("2006-01-02", day, now.Location())
		if err != nil {
			continue
		}
		dayEnd := t.Add(24 * time.Hour)
		if dayEnd.Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}

// Get 按天 + ts + uid 查找单条明细。
func (s *DetailStore) Get(day string, ts int64, uid string) (*CallDetail, error) {
	if day == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.dir, "call-details-"+day+".jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var d CallDetail
		if json.Unmarshal(sc.Bytes(), &d) != nil {
			continue
		}
		if d.TS == ts && d.UID == uid {
			return &d, nil
		}
	}
	return nil, os.ErrNotExist
}

// truncateStr 明细字段截断（防超长上下文撑爆存储）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Days 列出现存的日期（面板提示用，时间降序）。
func (s *DetailStore) Days() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var days []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "call-details-") && strings.HasSuffix(name, ".jsonl") {
			days = append(days, strings.TrimSuffix(strings.TrimPrefix(name, "call-details-"), ".jsonl"))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}
