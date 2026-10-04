package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestApplyPromptPolicyAutoPassthrough(t *testing.T) {
	// auto：调用方自带 system（agent 框架）→ 原样透传，不注入不叠加
	cfg := PromptConfig{Mode: "auto"}
	body := []byte(`{"messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},{"role":"user","content":"你好"}]}`)
	if string(applyPromptPolicy(body, cfg, "")) != string(body) {
		t.Fatal("auto mode: caller system should pass through untouched (不叠加)")
	}
}

func TestApplyPromptPolicyAutoInjectsWhenBare(t *testing.T) {
	cfg := PromptConfig{Mode: "auto"}
	body := []byte(`{"messages":[{"role":"user","content":"你好"}]}`)
	out := applyPromptPolicy(body, cfg, "")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("bare call should get injected system, got %v", msgs)
	}
	if c := msgs[0].(map[string]any)["content"].(string); !strings.Contains(c, "你是 Trae") {
		t.Fatalf("injected content unexpected: %q", c[:50])
	}
}

func TestApplyPromptPolicyTraeAlwaysInjects(t *testing.T) {
	cfg := PromptConfig{Mode: "trae"}
	body := []byte(`{"messages":[{"role":"system","content":"旧系统提示"},{"role":"user","content":"你好"}]}`)
	out := applyPromptPolicy(body, cfg, "")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	sysCount := 0
	for _, m := range msgs {
		if mm := m.(map[string]any); mm["role"] == "system" {
			sysCount++
			c := mm["content"].(string)
			if len(c) < len(defaultSystemPrompt) {
				t.Fatalf("merged system too short: %d", len(c))
			}
			if !strings.Contains(c, "旧系统提示") {
				t.Fatal("caller system content lost")
			}
			if strings.Count(c, defaultSystemPrompt) > 1 {
				t.Fatal("duplicated fixed prompt")
			}
		}
	}
	if sysCount != 1 {
		t.Fatalf("system count=%d want 1 (不叠加)", sysCount)
	}
}

func TestApplyPromptPolicyOff(t *testing.T) {
	cfg := PromptConfig{Mode: "off"}
	body := []byte(`{"messages":[{"role":"system","content":"keep"},{"role":"user","content":"x"}]}`)
	if string(applyPromptPolicy(body, cfg, "")) != string(body) {
		t.Fatal("off mode should pass through untouched")
	}
}

func TestApplyPromptPolicyAutoNoSystem(t *testing.T) {
	cfg := PromptConfig{Mode: "auto"}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := applyPromptPolicy(body, cfg, "")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("fixed system should be prepended, got %v", msgs)
	}
}

func TestFixedPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/prompt.txt"
	os.WriteFile(f, []byte("自定义提示词内容"), 0o600)
	cfg := PromptConfig{Mode: "trae", File: "prompt.txt"}
	if got := fixedPrompt(cfg, dir); got != "自定义提示词内容" {
		t.Fatalf("file override not applied: %q", got)
	}
}

// TestApplyPromptPolicyReplaceMode replace 模式：调用方 system 一律丢弃，
// 恒定只发一份固定的 Trae 风格提示词（与 workbuddy2api custom 同语义）。
func TestApplyPromptPolicyReplaceMode(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"You are an AI agent powered by DeepSeek Harness. 超长框架提示词"},
		{"role":"system","content":"第二条 system"},
		{"role":"user","content":"帮我写个脚本"}]}`)
	out := applyPromptPolicy(body, PromptConfig{Mode: "replace"}, t.TempDir())
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) != 2 {
		t.Fatalf("want 2 messages (1 system + 1 user), got %d", len(obj.Messages))
	}
	if obj.Messages[0].Role != "system" {
		t.Fatalf("first message role=%s", obj.Messages[0].Role)
	}
	sys, _ := obj.Messages[0].Content.(string)
	if sys != defaultSystemPrompt {
		t.Fatalf("system must be exactly the fixed Trae prompt (len %d), got len %d", len(defaultSystemPrompt), len(sys))
	}
	if strings.Contains(string(out), "DeepSeek Harness") || strings.Contains(string(out), "第二条 system") {
		t.Fatal("caller system content must be dropped entirely in replace mode")
	}
	if obj.Messages[1].Role != "user" {
		t.Fatalf("user message must be preserved, role=%s", obj.Messages[1].Role)
	}
}
