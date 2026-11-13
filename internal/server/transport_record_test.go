// transport_record_test.go 出口/网络故障的可观测性行为锁：
//  1. transport 失败必须落使用行（此前裸 continue → 面板 errs 透视永远空着，2026-10-18 实测缺口）；
//  2. 「出口/网络故障」文案分类：timeout / network 关键词自动归类（前端映射早已就绪）。
package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

func TestTransportErrorRecordsUsageRow(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("net/http: timeout awaiting response headers")
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	h.usage = NewUsageStore("") // 换干净 store：NewHandler 默认与全包测试共享存量数据
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 首字节超时是「上游预填慢」，不是出口坏：仍落账但不隔离账号
	// （2026-11-13 复盘：超时风暴隔离全部出口 → all accounts unavailable）。
	if st, ok := h.cfg.Pool.Status("u1"); !ok || st.TransportDown {
		t.Fatalf("header timeout must not isolate the exit: %+v", st)
	}

	es := h.usage.Since(0)
	if len(es) == 0 {
		t.Fatal("transport fault left zero usage rows")
	}
	found := false
	for _, e := range es {
		if e.Status == "error" && strings.Contains(e.Error, "出口/网络故障") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a usage row tagged 出口/网络故障, got %+v", es)
	}
	errs := errStats(es)
	if len(errs) == 0 {
		t.Fatal("errs perspective is empty after transport fault")
	}
	// 语义：分行（出口/网络故障）+ 终态 summary（all accounts unavailable: ...）可能都归入 timeout；
	// 锁「错误透视能看到 outlet 故障的原文」即可，不锁哪一行的 sample。
	kindOK := false
	for _, s := range errs {
		if s.Kind == "timeout" && strings.Contains(s.Sample, "timeout awaiting response headers") {
			kindOK = true
		}
	}
	if !kindOK {
		t.Fatalf("expected kind=timeout surfacing the transport timeout, got %+v", errs)
	}
}

func TestClassifyUsageErrTransport(t *testing.T) {
	cases := map[string]string{
		"出口/网络故障: net/http: timeout awaiting response headers":          "timeout",
		"出口/网络故障: read: connection reset by peer":                       "network",
		"出口/网络故障: proxyconnect tcp: dial tcp: connection refused":       "network",
		"出口/网络故障: tls: first record does not look like a TLS handshake": "network",
		"客户端断开（下游取消）: Post \"https://x\": context canceled":             "canceled",
	}
	for in, want := range cases {
		if got := classifyUsageErr(in); got != want {
			t.Fatalf("classify(%q)=%q want %q", in, got, want)
		}
	}
}

// TestClientCancelSkipsAccountPenalty 下游取消（context canceled）不是出口故障：
// 不隔离账号、不轮换、不计模型挂死熔断，只落一行 canceled。
// 2026-11-12 复盘：巨型 prompt 预填被客户端 120s 超时打断，曾连锁隔离全部账号。
func TestClientCancelSkipsAccountPenalty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cancel() // 模拟客户端在等上游首字节期间断开
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.usage = NewUsageStore("")
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	es := h.usage.Since(0)
	if len(es) != 1 || es[0].Status != "canceled" {
		t.Fatalf("want exactly one canceled usage row, got %+v", es)
	}
	if strings.Contains(es[0].Error, "出口/网络故障") {
		t.Fatalf("client cancel misclassified as transport fault: %q", es[0].Error)
	}
	if st, ok := p.Status("u1"); !ok || st.TransportDown {
		t.Fatalf("client cancel must not isolate account: %+v", st)
	}
	h.hangMu.Lock()
	hits, until := len(h.hangHits["glm-5.2"]), len(h.hangUntil)
	h.hangMu.Unlock()
	if hits != 0 || until != 0 {
		t.Fatalf("client cancel must not trip hang guard (hits=%d until=%d)", hits, until)
	}
}

// TestFastTransportFaultNoHangGuard 握手 EOF 属于快速出口故障：照常隔离出口/落账，
// 但不计入模型级挂死熔断（否则 10 分钟内两次出口抖动就把整模型 503 五分钟）。
func TestFastTransportFaultNoHangGuard(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New(`Post "https://fake.example": tlsfp: handshake fake.example:443: EOF`)
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.usage = NewUsageStore("")
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	es := h.usage.Since(0)
	found := false
	for _, e := range es {
		if e.Status == "error" && strings.Contains(e.Error, "出口/网络故障") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fast transport fault must still leave a 出口/网络故障 row, got %+v", es)
	}
	if st, ok := p.Status("u1"); !ok || !st.TransportDown {
		t.Fatalf("fast transport fault should still isolate the exit: %+v", st)
	}
	h.hangMu.Lock()
	hits := len(h.hangHits["glm-5.2"])
	h.hangMu.Unlock()
	if hits != 0 {
		t.Fatalf("fast transport fault must not count as model stall, hits=%d", hits)
	}
}
