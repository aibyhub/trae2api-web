// reasoning_gate_test.go 非思考模型的 reasoning 参数剥离（2026-10-25 门控）。
package server

import (
	"encoding/json"
	"testing"

	"trae2api-web/internal/upstream"
)

func TestStripReasoningParams(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"high","reasoning_effort_level":"medium","reasoning":{"x":1},"temperature":0.5}`)
	out := stripReasoningParams(src)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("remarshal broken: %v", err)
	}
	for _, k := range []string{"reasoning_effort", "reasoning_effort_level", "reasoning"} {
		if _, ok := obj[k]; ok {
			t.Fatalf("%s not stripped", k)
		}
	}
	if obj["temperature"] != 0.5 {
		t.Fatalf("other fields damaged: %v", obj["temperature"])
	}
	// 无 reasoning 参数时必须原样返回（不做无谓重编解码）
	if out := stripReasoningParams([]byte(`{"model":"m"}`)); string(out) != `{"model":"m"}` {
		t.Fatalf("body without reasoning params should pass through unchanged: %s", out)
	}
	// 非 JSON 原样返回
	if out := stripReasoningParams([]byte(`not-json`)); string(out) != `not-json` {
		t.Fatalf("non-json should pass through: %s", out)
	}
}

func TestModelSupportsThinking(t *testing.T) {
	// 菜单为空（冷启动）→ 保守 true（不误剥参数）
	if !modelSupportsThinking("glm-5.2") {
		t.Fatal("unknown model must default to supported")
	}
	// 注入菜单缓存：support_thinking=false 的模型识别为不支持
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.Unlock()
	}()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "glm-5.2", SupportThinking: false},
		{ID: "glm-5.3", SupportThinking: true},
	}
	dynamicModelsCache.Unlock()
	if modelSupportsThinking("glm-5.2") {
		t.Fatal("glm-5.2 (support_thinking=false) should be gated")
	}
	if !modelSupportsThinking("glm-5.3") {
		t.Fatal("glm-5.3 should keep reasoning params")
	}
	if !modelSupportsThinking("unknown-model") {
		t.Fatal("model not in menu must default to supported")
	}
}
