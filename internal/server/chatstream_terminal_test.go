// chatstream_terminal_test.go 流式守护行为锁：
// 1) stream 请求一律先落 SSE 头 + 首个 ": ping" 心跳注释（防上游慢首字节期间客户端悬挂）；
// 2) 全部账号不可用（或轮换预算耗尽）时终态＝流内 event:error + OpenAI 错误对象 + [DONE]，
//    不再发 503 JSON（头已提交）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/pool"
)

func TestChatStreamEarlySSEAndTerminalError(t *testing.T) {
	h := NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true }),
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ": ping\n\n") {
		t.Errorf("missing initial heartbeat comment: %q", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Errorf("missing terminal error event: %q", body)
	}
	if !strings.Contains(body, `"code":"no_healthy_account"`) {
		t.Errorf("missing openai-style error object: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", body)
	}
}

// 非流式请求不得提前提交头：终态仍是 503 JSON（向后兼容老客户端）。
func TestChatNonStreamKeeps503JSON(t *testing.T) {
	h := NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, soloSSE, true }),
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("non-stream must stay JSON: %q", rec.Body)
	}
}
