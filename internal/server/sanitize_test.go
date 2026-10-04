// sanitize_test.go 黑名单指纹清洗回归：身份句最小改写、键值剥离、良性请求零改动。
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeRequestBodyRewritesDSHIdentity(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"You are an AI agent powered by DeepSeek Harness. Follow the protocol."}]}`)
	out := SanitizeRequestBody(body)
	s := string(out)
	if strings.Contains(s, "DeepSeek Harness") {
		t.Fatalf("DSH identity must be rewritten: %s", s)
	}
	if !strings.Contains(s, "Agent Harness") {
		t.Fatalf("rewritten identity missing: %s", s)
	}
	if !strings.Contains(s, "Follow the protocol.") {
		t.Fatal("rest of the content must be untouched")
	}
}

func TestSanitizeRequestBodyStripsBillingHeader(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"x-anthropic-billing-header: cc_entrypoint=cli; rest of text"}]}`)
	out := string(SanitizeRequestBody(body))
	if strings.Contains(out, "x-anthropic-billing-header") || strings.Contains(out, "cc_entrypoint=") {
		t.Fatalf("billing header fingerprint must be stripped: %s", out)
	}
	if !strings.Contains(out, "rest of text") {
		t.Fatalf("neighbouring content must survive: %s", out)
	}
}

func TestSanitizeRequestBodyBenignUntouched(t *testing.T) {
	orig := map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "普通中文对话，无指纹"}},
	}
	raw, _ := json.Marshal(orig)
	out := SanitizeRequestBody(raw)
	if string(out) != string(raw) {
		t.Fatalf("benign request must be returned as-is:\norig %s\nout  %s", raw, out)
	}
}

func TestSanitizeRequestBodyCoversToolCallArguments(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":null,
		 "tool_calls":[{"id":"1","type":"function","function":{"name":"write","arguments":"{\"content\":\"You are Claude Code, Anthropic's official CLI for Claude\"}"}}]}]}`)
	out := string(SanitizeRequestBody(body))
	if strings.Contains(out, "official CLI for Claude\"") && !strings.Contains(out, "CLI tool for Claude") {
		t.Fatalf("tool call arguments must be sanitized: %s", out)
	}
}

func TestOpenAIErrorTypeMappingTrae(t *testing.T) {
	if got := openAIErrorType(401); got != "invalid_request_error" {
		t.Errorf("401 type=%q", got)
	}
	if got := openAIErrorType(429); got != "rate_limit_error" {
		t.Errorf("429 type=%q", got)
	}
	if got := openAIErrorType(503); got != "api_error" {
		t.Errorf("503 type=%q", got)
	}
}
