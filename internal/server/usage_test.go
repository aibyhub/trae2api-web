// usage_test.go 使用日志存储 + token_usage 解析 + 倍率。
package server

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStoreAddAndRecent(t *testing.T) {
	dir := t.TempDir()
	s := NewUsageStore(filepath.Join(dir, "usage.jsonl"))
	for i := 0; i < 5; i++ {
		s.Add(UsageEntry{TS: int64(1000 + i), UID: "u", Model: "glm-5.2", Status: "ok", TotalTokens: int64(i + 1)})
	}
	got := s.Recent(3)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].TS != 1004 || got[2].TS != 1002 {
		t.Fatalf("order wrong: first=%d last=%d want desc", got[0].TS, got[2].TS)
	}
	// 持久化：新 store 读回
	s2 := NewUsageStore(filepath.Join(dir, "usage.jsonl"))
	if len(s2.Recent(0)) != 5 {
		t.Fatalf("persisted entries=%d want 5", len(s2.Recent(0)))
	}
}

func TestUsageStoreToday(t *testing.T) {
	s := NewUsageStore("")
	now := time.Now().UnixMilli()
	s.Add(UsageEntry{TS: now, Status: "ok", TotalTokens: 100, Cost: 1.5})
	s.Add(UsageEntry{TS: now - 48*3600*1000, Status: "ok", TotalTokens: 999, Cost: 9}) // 两天前
	today := s.Today()
	if today.Requests != 1 || today.PromptTokens+today.CompletionTokens != 0 {
		t.Fatalf("today=%+v", today)
	}
	if today.Cost != 1.5 {
		t.Fatalf("cost=%v want 1.5", today.Cost)
	}
}

func TestParseTokenUsage(t *testing.T) {
	p, c, tot := parseTokenUsage(map[string]any{
		"input_tokens": float64(10), "output_tokens": float64(5),
	})
	if p != 10 || c != 5 || tot != 15 {
		t.Fatalf("anthropic keys: %d %d %d", p, c, tot)
	}
	p, c, tot = parseTokenUsage(map[string]any{
		"prompt_tokens": float64(7), "completion_tokens": float64(3), "total_tokens": float64(20),
	})
	if p != 7 || c != 3 || tot != 20 {
		t.Fatalf("openai keys: %d %d %d", p, c, tot)
	}
	p, c, tot = parseTokenUsage(nil)
	if p != 0 || c != 0 || tot != 0 {
		t.Fatal("nil map should be zeros")
	}
}

func TestRateStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_rates.json")
	r := NewRateStore(path)
	if r.Get("glm-5.2") != 1 {
		t.Fatal("default rate should be 1")
	}
	if err := r.Set(map[string]float64{"glm-5.2": 2.5, "kimi-k3": 0.5}); err != nil {
		t.Fatal(err)
	}
	if r.Get("glm-5.2") != 2.5 || r.Get("kimi-k3") != 0.5 || r.Get("unknown") != 1 {
		t.Fatal("rates not applied")
	}
	r2 := NewRateStore(path) // 重新加载
	if r2.Get("glm-5.2") != 2.5 {
		t.Fatal("rates not persisted")
	}
	if err := r2.Set(map[string]float64{"bad": -1}); err == nil {
		t.Fatal("negative rate should be rejected")
	}
}
