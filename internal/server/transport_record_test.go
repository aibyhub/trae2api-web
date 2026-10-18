// transport_record_test.go 出口/网络故障的可观测性行为锁：
//  1. transport 失败必须落使用行（此前裸 continue → 面板 errs 透视永远空着，2026-10-18 实测缺口）；
//  2. 「出口/网络故障」文案分类：timeout / network 关键词自动归类（前端映射早已就绪）。
package server

import (
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

	var es []UsageEntry
	es = h.usage.Since(0)
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
	}
	for in, want := range cases {
		if got := classifyUsageErr(in); got != want {
			t.Fatalf("classify(%q)=%q want %q", in, got, want)
		}
	}
}
