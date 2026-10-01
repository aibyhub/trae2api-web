// checkinlog.go 签到结果日志（data/checkin.jsonl）。
//
// 目的：面板要能回答「每天的自动签到到底正常吗」——每个账号每次签到的
// 时间 / 结果 / 失败原因都留档，重启后仍在（内存环形 + jsonl 追加，>4MB 轮转 .1）。
package scheduler

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CheckinLogEntry 一次签到结果。
type CheckinLogEntry struct {
	TS       int64  `json:"ts"` // unix 秒
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Status   string `json:"status"` // claimed/already/checkin_off/skipped/error
	Error    string `json:"error,omitempty"`
	Manual   bool   `json:"manual,omitempty"` // true = 面板手动触发；false = 定时自动
}

const (
	checkinLogMaxEntries  = 1000
	checkinLogMaxFileSize = 4 << 20
)

// CheckinLog 签到日志存储。
type CheckinLog struct {
	mu      sync.Mutex
	entries []CheckinLogEntry // 时间升序
	max     int
	path    string
}

// NewCheckinLog 构建；path 非空时加载历史并追加写。
func NewCheckinLog(path string) *CheckinLog {
	l := &CheckinLog{max: checkinLogMaxEntries, path: path}
	if path != "" {
		l.loadFile()
	}
	return l
}

func (l *CheckinLog) loadFile() {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	for _, line := range splitLogLines(raw) {
		var e CheckinLogEntry
		if json.Unmarshal(line, &e) == nil && e.TS > 0 {
			l.entries = append(l.entries, e)
		}
	}
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
}

// splitLogLines 按行切分（jsonl），跳过空行。
func splitLogLines(raw []byte) [][]byte {
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

// Add 记录一次（内存 + 落盘）。
func (l *CheckinLog) Add(e CheckinLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
	if l.path == "" {
		return
	}
	if st, err := os.Stat(l.path); err == nil && st.Size() > checkinLogMaxFileSize {
		_ = os.Rename(l.path, l.path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		log.Printf("checkin log mkdir: %v", err)
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("checkin log write: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// Since 返回 ts >= since 的全部记录（副本，时间升序）。
func (l *CheckinLog) Since(since int64) []CheckinLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]CheckinLogEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.TS >= since {
			out = append(out, e)
		}
	}
	return out
}

// Recent 返回最近 n 条（倒序：最新在前）。
func (l *CheckinLog) Recent(n int) []CheckinLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.entries) {
		n = len(l.entries)
	}
	out := make([]CheckinLogEntry, 0, n)
	for i := len(l.entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

// Prune 按保留期裁剪（内存 + 落盘重写），返回删除数与剩余数。
// keepDays：< 0 = 不动作；0 = 清空全部历史；> 0 = 只保留最近 N 天。
// 轮转文件 .1 也一并处理（全部过期则删除）。
func (l *CheckinLog) Prune(keepDays int) (removed, kept int, err error) {
	if keepDays < 0 {
		l.mu.Lock()
		defer l.mu.Unlock()
		return 0, len(l.entries), nil
	}
	var cutoff int64
	if keepDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -keepDays).Unix()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make([]CheckinLogEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if keepDays == 0 || e.TS < cutoff {
			continue
		}
		next = append(next, e)
	}
	removed = len(l.entries) - len(next)
	l.entries = next
	if l.path != "" {
		if err = rewriteCheckinFile(l.path, next); err != nil {
			return removed, len(next), err
		}
		pruneRotatedByTS(l.path+".1", cutoff, func(raw []byte) (int64, bool) {
			var e CheckinLogEntry
			if json.Unmarshal(raw, &e) != nil {
				return 0, false
			}
			return e.TS, true
		})
	}
	return removed, len(next), nil
}

// Stats 返回日志概况（条数与最早/最新时间，unix 秒）。
func (l *CheckinLog) Stats() (entries int, oldest, newest int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries = len(l.entries)
	if entries > 0 {
		oldest, newest = l.entries[0].TS, l.entries[entries-1].TS
	}
	return entries, oldest, newest
}

// rewriteCheckinFile 原子重写签到日志。
func rewriteCheckinFile(path string, entries []CheckinLogEntry) error {
	var buf bytes.Buffer
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pruneRotatedByTS 按 ts 裁剪轮转文件；解析失败的行保留（不丢数据），全空则删除文件。
// tsOf 返回该行的 ts 与是否解析成功。
func pruneRotatedByTS(path string, cutoff int64, tsOf func([]byte) (int64, bool)) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	for _, line := range splitLogLines(raw) {
		if ts, ok := tsOf(line); ok && (cutoff == 0 || ts < cutoff) {
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

// StartOfToday 本地时区当天 00:00 的 unix 秒。
func StartOfToday(now time.Time) int64 {
	t := now.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).Unix()
}
