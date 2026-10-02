package server

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestApplyPromptPolicyMergesSingleSystem(t *testing.T) {
	cfg := PromptConfig{Mode: "trae"}
	body := []byte(`{"model":"glm-5.2","messages":[
		{"role":"system","content":"旧系统提示"},
		{"role":"system","content":"第二条系统"},
		{"role":"user","content":"你好"}]}`)
	out := applyPromptPolicy(body, cfg, "")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	sysCount := 0
	for _, m := range msgs {
		if mm := m.(map[string]any); mm["role"] == "system" {
			sysCount++
			c := mm["content"].(string)
			if len(c) < len(defaultSystemPrompt) {
				t.Fatalf("merged system too short: %d", len(c))
			}
			if !bytes.Contains([]byte(c), []byte("旧系统提示")) || !bytes.Contains([]byte(c), []byte("第二条系统")) {
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
	// user 消息保留
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Fatal("user message order changed")
	}
}

func TestApplyPromptPolicyOff(t *testing.T) {
	cfg := PromptConfig{Mode: "off"}
	body := []byte(`{"messages":[{"role":"system","content":"keep"},{"role":"user","content":"x"}]}`)
	if string(applyPromptPolicy(body, cfg, "")) != string(body) {
		t.Fatal("off mode should pass through untouched")
	}
}

func TestApplyPromptPolicyNoSystem(t *testing.T) {
	cfg := PromptConfig{Mode: "trae"}
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
