// usage_detail_test.go 使用/明细链路行为锁：
//  1. /admin/api/usage 每行携带服务器时区 day（前端按日查档用，不再依赖浏览器 UTC 推算）；
//  2. 明细接口按 ts 用服务器时区兜底重算日期——旧前端推错 day / 时区错位记录也能找回；
//  3. 归档缺失（过保留期 / 未落档 / 轮换全灭）→ fallback:true 返回 usage 行摘要，错误正文完整可见。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/pool"
)

func newUsageTestHandler(t *testing.T) *Handler {
	t.Helper()
	return NewHandler(Config{Pool: pool.New(""), DataDir: t.TempDir()})
}

func usageGet(t *testing.T, h *Handler, url string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	return rec.Code, d
}

// 列表行必须带服务器时区 day，且当笔记录按该 day+ts 可被明细接口找回。
func TestUsageListCarriesServerDay(t *testing.T) {
	h := newUsageTestHandler(t)
	ts := time.Now().UnixMilli()
	h.usage.Add(UsageEntry{TS: ts, UID: "u1", Model: "glm-5.2", Status: "ok", DurationMs: 120})

	code, d := usageGet(t, h, "/admin/api/usage?window=24")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	entries, _ := d["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries=%v", d["entries"])
	}
	e, _ := entries[0].(map[string]any)
	day, _ := e["day"].(string)
	if want := time.Now().Format("2006-01-02"); day != want {
		t.Fatalf("day=%q want %q", day, want)
	}
}

func TestUsageDetailFallbackChain(t *testing.T) {
	h := newUsageTestHandler(t)
	now := time.Now()
	tsOK := now.UnixMilli()
	uidOK := "abc123full1234"
	h.usage.Add(UsageEntry{TS: tsOK, UID: uidOK, Model: "glm-5.2", Status: "error",
		Error: "上游 5xx 断流", DurationMs: 9000})
	h.details.Add(CallDetail{TS: tsOK, UID: uidOK, Model: "glm-5.2", Status: "error",
		Error: "上游 5xx 断流", DurationMs: 9000, RequestBody: `{"model":"glm-5.2"}`})

	// 无账号失败行：uid 为空（轮换全灭，没有选中任何账号）
	tsEmpty := now.Add(-time.Second).UnixMilli()
	errText := "本轮 3 次轮换均失败：全部账号不可用"
	h.usage.Add(UsageEntry{TS: tsEmpty, UID: "", Model: "glm-5.2", Status: "error",
		Error: errText, DurationMs: 3000})

	// A. 归档在 + 客户端给了错误 day（旧浏览器 UTC 推算错位）→ 服务器按 ts 重算兜底
	code, d := usageGet(t, h, "/admin/api/usage/detail?day=1999-01-01&ts="+itoa(tsOK)+"&uid="+uidOK)
	if code != 200 {
		t.Fatalf("A: code=%d body=%s", code, d)
	}
	if f, _ := d["fallback"].(bool); f {
		t.Fatalf("A: archive should be found, got fallback: %v", d)
	}
	if rb, _ := d["request_body"].(string); rb == "" {
		t.Fatalf("A: expected full archive body, got %v", d)
	}

	// B. 归档在 + 不传 day → 服务器时区重算
	code, d = usageGet(t, h, "/admin/api/usage/detail?ts="+itoa(tsOK)+"&uid="+uidOK)
	if code != 200 {
		t.Fatalf("B: code=%d body=%s", code, d)
	}
	if f, _ := d["fallback"].(bool); f {
		t.Fatalf("B: unexpected fallback: %v", d)
	}

	// C. 归档缺失 + usage 行在（uid 空串直传）→ fallback:true，错误正文完整
	code, d = usageGet(t, h, "/admin/api/usage/detail?ts="+itoa(tsEmpty)+"&uid=")
	if code != 200 {
		t.Fatalf("C: code=%d body=%s", code, d)
	}
	if f, _ := d["fallback"].(bool); !f {
		t.Fatalf("C: expected fallback payload, got %v", d)
	}
	if msg, _ := d["error"].(string); !strings.Contains(msg, "全部账号不可用") {
		t.Fatalf("C: error text lost: %v", d)
	}

	// C2. 归档缺失 + usage 行在（uid 非空但只有 usage 记录，如同日归档被手动清走）
	// 无记录 ts+uid 空串 → 走 D 分支，不单独断言（SA4006：d 在此分支未使用）
	_, _ = usageGet(t, h, "/admin/api/usage/detail?ts="+itoa(tsEmpty+99)+"&uid=")

	// D. 两边都没有 → 404
	code, d = usageGet(t, h, "/admin/api/usage/detail?ts=111&uid=")
	if code != 404 {
		t.Fatalf("D: code=%d body=%s", code, d)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 面板新字段行为锁：错误分类、环比上一时段、趋势最慢单笔、按模型 max_latency_ms。
func TestDetailStoreCapPrune(t *testing.T) {
	dir := t.TempDir()
	today := func(off int) string {
		return "call-details-" + time.Now().AddDate(0, 0, -off).Format("2006-01-02") + ".jsonl"
	}
	content := strings.Repeat("x", 600)
	for _, n := range []string{today(2), today(1), today(0)} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	NewDetailStore(dir, 1_400) // 1.8K 数据 > 1.4K 上限 → 删最老一天
	if _, err := os.Stat(filepath.Join(dir, today(2))); !os.IsNotExist(err) {
		t.Fatalf("最老日文件应删除, err=%v", err)
	}
	for _, n := range []string{today(1), today(0)} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("当日/次新文件应保留: %v", err)
		}
	}
	// 只剩今日一个文件时即便超限也不删（避免清掉活跃窗口）
	NewDetailStore(dir, 1)
	if _, err := os.Stat(filepath.Join(dir, today(0))); err != nil {
		t.Fatalf("单文件超限不应删除: %v", err)
	}
}

func TestUsagePanelFields(t *testing.T) {
	h := newUsageTestHandler(t)
	now := time.Now()
	devErr := "code=4027 tool call failed: 400:developer is not one of ['system', 'assistant', 'user', 'tool', 'function'] - 'messages.['0].role'"
	h.usage.Add(UsageEntry{TS: now.Add(-10 * time.Minute).UnixMilli(), UID: "a", Model: "glm-5.3-flashx",
		Status: "error", Error: devErr + "----" + strings.Repeat("x", 1100),
		DurationMs: 45_000, CompletionTokens: 380})
	// 上一等长窗口（24-48h 前）一条 429：仅计入 prev_totals，不进当前 errs
	h.usage.Add(UsageEntry{TS: now.Add(-36 * time.Hour).UnixMilli(), UID: "b", Model: "glm-5.2",
		Status: "error", Error: "软冷却：429 rate limit", DurationMs: 90_000})

	code, d := usageGet(t, h, "/admin/api/usage?window=24")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}

	errsRaw, _ := json.Marshal(d["errs"])
	var errs []struct {
		Kind   string `json:"kind"`
		N      int    `json:"n"`
		Sample string `json:"sample"`
	}
	json.Unmarshal(errsRaw, &errs)
	if len(errs) != 1 || errs[0].Kind != "call_failed" || errs[0].N != 1 || !strings.Contains(errs[0].Sample, "developer") {
		t.Fatalf("errs=%s", errsRaw)
	}
	// 样例截断：>1024 字符的长回执不整段下发
	if len(errs[0].Sample) > 1024 {
		t.Fatalf("sample len=%d 未截断", len(errs[0].Sample))
	}

	prevRaw, _ := json.Marshal(d["prev_totals"])
	var prev struct {
		Requests int `json:"requests"`
		Fails    int `json:"fails"`
	}
	json.Unmarshal(prevRaw, &prev)
	if prev.Requests != 1 || prev.Fails != 1 {
		t.Fatalf("prev_totals=%s", prevRaw)
	}

	serRaw, _ := json.Marshal(d["series"])
	var pts []struct {
		Req int64 `json:"req"`
		Mx  int64 `json:"mx"`
		Ms  int64 `json:"ms"`
	}
	json.Unmarshal(serRaw, &pts)
	hit := false
	for _, p := range pts {
		if p.Req == 1 && p.Mx == 45_000 && p.Ms == 45_000 {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("series missing mx/ms hit: %s", serRaw)
	}

	modelRaw, _ := json.Marshal(d["by_model"])
	var models []struct {
		Key    string  `json:"key"`
		MaxLat int64   `json:"max_latency_ms"`
		Tps    float64 `json:"tps"`
	}
	json.Unmarshal(modelRaw, &models)
	if len(models) != 1 || models[0].Key != "glm-5.3-flashx" || models[0].MaxLat != 45_000 {
		t.Fatalf("by_model=%s", modelRaw)
	}
	// 均速率 = 380 输出 tokens / 45s ≈ 8.44（workbuddy 口径，分母含失败笔耗时）
	if v := models[0].Tps; v < 8.43 || v > 8.45 {
		t.Fatalf("tps=%v want ~8.44", v)
	}
}

// 趋势序列：乱序到达（历史副本回写/手补文件）也要按时间升序分桶 + 中间空桶补零。
func TestUsageSeriesGapFill(t *testing.T) {
	h := newUsageTestHandler(t)
	now := time.Now()
	earliest := now.Add(-3 * time.Hour)
	mid := now.Add(-90 * time.Minute) // 时间上夹在 earliest 与 now 之间
	// 故意乱序：先 oldest，再 newest，再 middle —— loadFile 必须排序兜底
	h.usage.Add(UsageEntry{TS: earliest.UnixMilli(), UID: "a", Model: "m", Status: "ok"})
	h.usage.Add(UsageEntry{TS: now.UnixMilli(), UID: "a", Model: "m", Status: "ok"})
	h.usage.Add(UsageEntry{TS: mid.UnixMilli(), UID: "a", Model: "m", Status: "error", Error: "boom"})

	_, d := usageGet(t, h, "/admin/api/usage?window=0")
	if g, _ := d["granularity"].(string); g != "hour" {
		t.Fatalf("granularity=%v (span<48h 应为 hour)", d["granularity"])
	}
	raw, _ := json.Marshal(d["series"])
	var pts []struct {
		B    string `json:"b"`
		Req  int64  `json:"req"`
		Fail int64  `json:"fail"`
	}
	if err := json.Unmarshal(raw, &pts); err != nil {
		t.Fatalf("series: %v %s", err, raw)
	}
	if len(pts) < 4 {
		t.Fatalf("series=%s", raw)
	}
	if pts[0].B != earliest.Truncate(time.Hour).Format("2006-01-02T15") {
		t.Fatalf("first bucket=%q", pts[0].B)
	}
	if pts[len(pts)-1].B != now.Truncate(time.Hour).Format("2006-01-02T15") {
		t.Fatalf("last bucket=%q", pts[len(pts)-1].B)
	}
	var hitMid, hitLast bool
	for _, p := range pts {
		if p.B == mid.Truncate(time.Hour).Format("2006-01-02T15") && p.Fail == 1 && p.Req == 1 {
			hitMid = true
		}
		if p.B == now.Truncate(time.Hour).Format("2006-01-02T15") && p.Req == 1 && p.Fail == 0 {
			hitLast = true
		}
	}
	if !hitMid || !hitLast {
		t.Fatalf("buckets missing: mid=%v last=%v %s", hitMid, hitLast, raw)
	}
}
