// checkin_test.go 签到看板与数据维护接口（用 map 解析，避免结构体标签）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/scheduler"
)

func seedCheckinLog(t *testing.T, path string, entries ...scheduler.CheckinLogEntry) {
	t.Helper()
	var lines []string
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return m
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("key %s missing/not number in %v", key, m)
	}
	return v
}

// TestCheckinBoard 今日进度 / 自动成功率 / 待关注账号 / 近7天 / 下次窗口。
func TestCheckinBoard(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "一号"})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "二号"})
	now := time.Now()
	logPath := filepath.Join(dir, "checkin.jsonl")
	seedCheckinLog(t, logPath,
		scheduler.CheckinLogEntry{TS: now.Add(-3 * time.Hour).Unix(), UID: "u1", Nickname: "一号", Status: "claimed"},
		scheduler.CheckinLogEntry{TS: now.Add(-2 * time.Hour).Unix(), UID: "u2", Nickname: "二号", Status: "error", Error: "9074 设备未注册"},
	)
	sch := scheduler.New(scheduler.Config{Pool: pl, LogPath: logPath, CheckinHours: []int{9, 21}, JitterMinutes: 60})
	h := NewHandler(Config{Pool: pl, Sched: sch, DataDir: dir})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/checkin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("board = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := jsonOf(t, rec)
	stats := resp["stats"].(map[string]any)
	if num(t, stats, "total") != 2 || num(t, stats, "signed") != 1 || num(t, stats, "error") != 1 {
		t.Fatalf("stats=%v want total=2 signed=1 error=1", stats)
	}
	auto := stats["auto"].(map[string]any)
	if num(t, auto, "total") != 2 || num(t, auto, "ok") != 1 || num(t, auto, "fail") != 1 {
		t.Fatalf("auto=%v want total=2 ok=1 fail=1", auto)
	}
	pending := resp["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["uid"] != "u2" {
		t.Fatalf("pending=%v want only u2", pending)
	}
	if days := resp["days"].([]any); len(days) != 7 {
		t.Fatalf("days=%d want 7", len(days))
	}
	if hours := resp["hours"].([]any); len(hours) != 2 {
		t.Fatalf("hours=%v", hours)
	}
	if num(t, resp, "next_at") <= 0 {
		t.Fatalf("next_at missing: %v", resp["next_at"])
	}
}

// TestLogsApiAndPrune 日志概况 + 手动按保留期清理。
func TestLogsApiAndPrune(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	logPath := filepath.Join(dir, "checkin.jsonl")
	seedCheckinLog(t, logPath,
		scheduler.CheckinLogEntry{TS: time.Now().AddDate(0, 0, -200).Unix(), UID: "u1", Status: "claimed"},
		scheduler.CheckinLogEntry{TS: time.Now().Unix(), UID: "u1", Status: "already"},
	)
	sch := scheduler.New(scheduler.Config{Pool: pl, LogPath: logPath})
	h := NewHandler(Config{Pool: pl, Sched: sch, DataDir: dir, LogRetentionDays: 90})
	h.usage.Add(UsageEntry{TS: time.Now().AddDate(0, 0, -200).UnixMilli(), UID: "u1", Model: "glm-5.2", Status: "ok"})
	h.usage.Add(UsageEntry{TS: time.Now().UnixMilli(), UID: "u1", Model: "glm-5.2", Status: "ok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/logs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "checkin.jsonl") {
		t.Fatalf("logs api = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/api/logs/prune", strings.NewReader("{\"days\":90}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("prune = %d body=%s", rec.Code, rec.Body.String())
	}
	pr := jsonOf(t, rec)
	if pr["ok"] != true || num(t, pr, "removed_usage") != 1 || num(t, pr, "removed_checkin") != 1 {
		t.Fatalf("prune result=%v want ok + 1/1 removed", pr)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/api/logs/prune", strings.NewReader("{}")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty days = %d want 400", rec.Code)
	}
}
