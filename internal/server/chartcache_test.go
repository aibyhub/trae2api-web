// chartcache_test.go 图表按日摘要侧车行为锁（2026-10-18 起替掉全量扫描 call-details）：
//  1. 首次全扫生成摘要；此后只吸收新追加字节（consumed 偏移推进）；
//  2. 文件被裁剪去头（size < consumed）→ 整日重扫归零再累计；
//  3. 文件缺失/空转 → 历史计数保留；
//  4. 归档行瘦身：archiveBody 逐消息 6KB 截断、整体 48KB 上限。
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDetailDay(t *testing.T, dir, day string, rows ...string) {
	t.Helper()
	p := filepath.Join(dir, "call-details", "call-details-"+day+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(rows, "\n")
	if len(rows) > 0 {
		body += "\n" // 真实 add 路径每行自带终结符
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func row(uid, status string, ms int64) string {
	b, _ := json.Marshal(map[string]any{
		"ts": 1000, "uid": uid, "model": "glm-5.2", "status": status, "duration_ms": ms,
	})
	return string(b)
}

func fetchSummary(t *testing.T, dir, day string) *dailySum {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "chartdaily", day+".json"))
	if err != nil {
		t.Fatalf("summary file missing: %v", err)
	}
	var s dailySum
	if json.Unmarshal(raw, &s) != nil {
		t.Fatalf("summary not json")
	}
	return &s
}

func TestDailySummaryIncremental(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{cfg: Config{DataDir: dir}}
	day := "2026-10-18"
	writeDetailDay(t, dir, day, row("u1", "ok", 100), row("u2", "error", 50), row("u1", "ok", 300))

	got := h.chartDailyCalls()
	var d dailyCalls
	for _, x := range got {
		if x.Date == day {
			d = x
		}
	}
	if d.OK != 2 || d.Error != 1 || d.MaxMs != 300 {
		t.Fatalf("first scan wrong: %+v", d)
	}
	if d.ByUID["u1"] != 2 {
		t.Fatalf("by_uid wrong: %+v", d)
	}
	s := fetchSummary(t, dir, day)
	if s.Consumed == 0 {
		t.Fatal("consumed offset not recorded")
	}
	// 增量：追加两行后 summary 应只用新字节
	writeDetailDay(t, dir, day, row("u1", "ok", 100), row("u2", "error", 50), row("u1", "ok", 300),
		row("u1", "ok", 400), row("u3", "ok", 20))
	got = h.chartDailyCalls()
	for _, x := range got {
		if x.Date == day {
			d = x
		}
	}
	if d.OK != 4 || d.Error != 1 || d.MaxMs != 400 || d.ByUID["u3"] != 1 {
		t.Fatalf("incremental update wrong: %+v", d)
	}
}

func TestDailySummaryRescanOnShrink(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{cfg: Config{DataDir: dir}}
	day := "2026-10-18"
	writeDetailDay(t, dir, day, row("u1", "ok", 10), row("u2", "ok", 20))
	h.chartDailyCalls()
	// 模拟裁剪：只剩第一行
	writeDetailDay(t, dir, day, row("u1", "ok", 10))
	got := h.chartDailyCalls()
	var d dailyCalls
	for _, x := range got {
		if x.Date == day {
			d = x
		}
	}
	if d.OK != 1 || d.MaxMs != 10 || d.ByUID["u2"] != 0 {
		t.Fatalf("shrink must trigger full rescan: %+v", d)
	}
}

func TestDailySummaryKeepsHistoryWhenFileGone(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{cfg: Config{DataDir: dir}}
	day := "2026-10-17"
	writeDetailDay(t, dir, day, row("u1", "ok", 11), row("u2", "error", 5))
	h.chartDailyCalls()
	if err := os.Remove(filepath.Join(dir, "call-details", "call-details-"+day+".jsonl")); err != nil {
		t.Fatal(err)
	}
	got := h.chartDailyCalls()
	var d dailyCalls
	for _, x := range got {
		if x.Date == day {
			d = x
		}
	}
	if d.OK != 1 || d.Error != 1 {
		t.Fatalf("history must survive file deletion: %+v", d)
	}
	s := fetchSummary(t, dir, day)
	if s.OK != 1 || s.Error != 1 {
		t.Fatalf("summary must persist counts: %+v", s)
	}
}

func TestDailySummaryPartialLineHeldBack(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{cfg: Config{DataDir: dir}}
	day := "2026-10-18"
	p := filepath.Join(dir, "call-details", "call-details-"+day+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	first := row("u1", "ok", 33)
	second := row("u2", "ok", 44)
	// 第二行只写一半（无换行符）
	_ = os.WriteFile(p, []byte(first+"\n"+second[:12]), 0o600)
	h.chartDailyCalls()
	var d dailyCalls
	for _, x := range h.chartDailyCalls() {
		if x.Date == day {
			d = x
		}
	}
	if d.OK != 1 || d.MaxMs != 33 {
		t.Fatalf("partial line must not be counted: %+v", d)
	}
	_ = os.WriteFile(p, []byte(first+"\n"+second+"\n"), 0o600)
	h.chartDailyCalls()
	for _, x := range h.chartDailyCalls() {
		if x.Date == day {
			d = x
		}
	}
	if d.OK != 2 || d.MaxMs != 44 {
		t.Fatalf("completed line must be absorbed: %+v", d)
	}
}

func TestArchiveBodyCaps(t *testing.T) {
	big := strings.Repeat("x", 99999)
	body, _ := json.Marshal(map[string]any{"model": "glm-5.2", "messages": []map[string]any{
		{"role": "user", "content": big},
		{"role": "user", "content": big},
	}})
	out := archiveBody(body)
	if len(out) > 64<<10 {
		t.Fatalf("archived body too fat: %d bytes", len(out))
	}
	if !strings.Contains(out, "…(归档截断)") {
		t.Fatal("truncation marker missing")
	}
}
