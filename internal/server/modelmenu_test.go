package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

// netStall 模拟出口黑溺的传输层错误（被 http.Client 包装后 IsTransportError=true）。
var netStall = &url.Error{Op: "Post", URL: "https://fake.example/api/agent/v1/llm_utils_chat", Err: errors.New("net/http: timeout awaiting response headers")}

// ---- 缓存注入 / 清空 ----

func primeMenuCache(t *testing.T, ids ...string) {
	t.Helper()
	infos := make([]upstream.ModelInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, upstream.ModelInfo{ID: id, Rate: 1})
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

func clearMenuCache(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
}

// detailOK / detailBad get_detail_param 的假应答：合法清单 / 垃圾（解析失败）。
const detailOK = `{"config_info_list":[` +
	`{"config_name":"glm-5.2","is_custom_model":false,"is_invisible_to_user":false,` +
	`"display_config":{"display_name":"GLM 5.2","fee_model_level":3},` +
	`"display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.5}}}",` +
	`"model_detail_list":[{"model_name":"glm-5.2"}]},` +
	`{"config_name":"m1"}` +
	`]}`
const detailBad = `<html>gateway junk</html>`

// newCountingUpstream 覆盖所有上游请求的假客户端，NULL 返回 err 前先计数。
// 本测试只关心「哪些路径被调用、各调几次」，应答形态由 cb 决定。
func newCountingUpstream(t *testing.T, cb func(r *http.Request) (*http.Response, error)) (*upstream.Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return cb(r)
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	return client, &calls
}

func jsonResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestMenuRefreshUpdatesCacheRefreshModelMenuUpdatesCache refreshModelMenu 成功路径：
// 解析 config_info_list → 缓存更新、倍率可见。
func TestMenuRefreshUpdatesCache(t *testing.T) {
	clearMenuCache(t)
	defer clearMenuCache(t)
	up, _ := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		return jsonResp(detailOK), nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	n, err := h.refreshModelMenu()
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v want 2/nil", n, err)
	}
	if got := len(cachedModelInfos()); got != 2 {
		t.Fatalf("cache=%d want 2", got)
	}
	if rate := h.modelRate("glm-5.2"); rate != 0.5 {
		t.Errorf("modelRate=%v want 0.5", rate)
	}
}

// TestMenuRefreshFailureKeepsStaleMenu 拉取失败不清缓存：旧菜单继续兜服务
// （/v1/models 与聊天裁决都还能看到旧 id）。
func TestMenuRefreshFailureKeepsStaleMenu(t *testing.T) {
	clearMenuCache(t)
	defer clearMenuCache(t)
	primeMenuCache(t, "glm-5.2")
	up, _ := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		return jsonResp(detailBad), nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	if _, err := h.refreshModelMenu(); err == nil {
		t.Fatal("garbage roster must error")
	}
	if got := cachedModelInfos(); len(got) != 1 || got[0].ID != "glm-5.2" {
		t.Fatalf("stale cache disturbed: %+v", got)
	}
	// /v1/models 走旧菜单（动态缓存），一步拉到 stale 条目。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "glm-5.2") {
		t.Fatalf("stale serve broken: code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestChatZeroFetchOnHotPath 聊天热路径零菜单 IO（1.5.8 事故根源回归锁）：
// 缓存为空 → 聊天一律走静态兜底表；整个 chat 请求只允许 1 笔上游调用（chat 本体），
// 绝不出现 get_detail_param。
func TestChatZeroFetchOnHotPath(t *testing.T) {
	clearMenuCache(t)
	defer clearMenuCache(t)
	up, calls := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, upstream.EpModels) {
			return nil, errors.New("menu fetch must NEVER be called from the chat path")
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(soloSSE)),
		}, nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3-flash","messages":[]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("alias chat (cache empty) code=%d body=%s", rec.Code, rec.Body)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("upstream calls=%d want exactly 1 (chat only)", c)
	}
}

// TestMenuDailyRefresherScheduled 启动偏移只在定时器到点后拉取：ctx 取消 → 立即退出，
// 全程零上游调用。
func TestMenuDailyRefresherScheduled(t *testing.T) {
	clearMenuCache(t)
	defer clearMenuCache(t)
	up, calls := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		return jsonResp(detailOK), nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		h.RunModelsRefresher(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunModelsRefresher did not exit on canceled ctx")
	}
	if c := calls.Load(); c != 0 {
		t.Fatalf("canceled refresher must not fetch, got %d calls", c)
	}
}

// TestAdminModelsRefreshEndpoint 面板手动刷新：成功 200+count；失败 502+原菜单生效
// 语义信封；同一时刻重复点击 → 202 busy。
func TestAdminModelsRefreshEndpoint(t *testing.T) {
	clearMenuCache(t)
	defer clearMenuCache(t)
	buf := detailOK
	up, _ := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		return jsonResp(buf), nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "test-key",
	})
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/admin/api/models/refresh", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	r1 := call()
	if r1.Code != 200 {
		t.Fatalf("refresh code=%d body=%s want 200 ok", r1.Code, r1.Body)
	}
	// 手动刷新占坑（模拟 in-flight）后另一点击 → 202 busy。
	if !modelsRefreshInFlight.CompareAndSwap(false, true) {
		t.Fatal("precondition: banner guard should be free")
	}
	r2 := call()
	if r2.Code != 202 {
		t.Fatalf("busy refresh code=%d want 202", r2.Code)
	}
	modelsRefreshInFlight.Store(false)
	// 失败（垃圾清单）→ 502 且错误信封说明原菜单继续生效。
	buf = detailBad
	r3 := call()
	if r3.Code != 502 {
		t.Fatalf("failed refresh code=%d body=%s want 502", r3.Code, r3.Body)
	}
	var e map[string]any
	if err := json.Unmarshal(r3.Body.Bytes(), &e); err != nil || e["status"] != "error" {
		t.Fatalf("envelope=%s err=%v", r3.Body, err)
	}
	// 失败不清缓存： 成功后缓存 2 条；失败保留 = count 仍为 2（stale menu preserved）。
	if got := e["count"].(float64); got != 2 {
		t.Fatalf("count after failed refresh=%v want 2 (stale menu preserved)", got)
	}
}

// TestHangGuardUnitRamp 观察窗纯逻辑：单笔不熔断，两笔熔断，成功清零。
func TestHangGuardUnitRamp(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	if h.hangBlocked("glm-5.2") {
		t.Fatal("clean start must not be blocked")
	}
	h.noteModelHang("glm-5.2")
	if h.hangBlocked("glm-5.2") {
		t.Fatal("single stall must not trip (threshold=2)")
	}
	h.noteModelHang("glm-5.2")
	if !h.hangBlocked("glm-5.2") {
		t.Fatal("second stall within window must trip hold")
	}
	h.noteModelOK("glm-5.2")
	if h.hangBlocked("glm-5.2") {
		t.Fatal("success must clear hold")
	}
	// 其他模型不受牵连。
	h.noteModelHang("glm-5.2")
	h.noteModelHang("glm-5.2")
	if h.hangBlocked("glm-5.3-flash") {
		t.Fatal("hang scope must be per-model")
	}
}

// TestHangGuardFastFailsEntry 熔断保持期内的请求：入口秒回 503 专用信封，
// 上游一笔不出。
func TestHangGuardFastFailsEntry(t *testing.T) {
	up, calls := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		t.Error("upstream called despite hang hold")
		return nil, errors.New("should not happen")
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	h.hangMu.Lock()
	h.hangUntil["glm-5.2"] = time.Now().Add(time.Minute)
	h.hangMu.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want fast-fail 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_temporarily_down") {
		t.Fatalf("body=%s want model_temporarily_down envelope", rec.Body)
	}
	if c := calls.Load(); c != 0 {
		t.Fatalf("upstream calls=%d want 0 during hold", c)
	}
}

// TestHangGuardNotesOnTransportError transport 故障分支确实给观察窗记账
// （接线锁）：出口坏账号单号池 + 两笔请求 → hits=1/请求。
func TestHangGuardNotesOnTransportError(t *testing.T) {
	up, calls := newCountingUpstream(t, func(r *http.Request) (*http.Response, error) {
		return nil, netStall
	})
	h := NewHandler(Config{
		Pool:              testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:          up,
		TransportCooldown: time.Millisecond,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("SSE header already committed, code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") &&
		!strings.Contains(rec.Body.String(), "all accounts unavailable") {
		t.Fatalf("want in-stream terminal error: %s", rec.Body)
	}
	h.hangMu.Lock()
	hits := len(h.hangHits["glm-5.2"])
	h.hangMu.Unlock()
	if hits != 1 {
		t.Fatalf("one stalled request (1 account, deduped) must record exactly 1 hit, got %d", hits)
	}
	if c := calls.Load(); c < 1 {
		t.Fatalf("transport branch requires >=1 upstream attempt, got %d", c)
	}
}
