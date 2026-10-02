// usagefmt.go token_usage 字段规范化：上游为 Anthropic 风格键，
// 在保留原字段的同时补齐 OpenAI 标准结构，客户端可直接读取缓存命中数。
package upstream

// NormalizeUsage 补齐 OpenAI 标准字段（就地补，不丢上游原字段）：
//   - usage.prompt_tokens_details.cached_tokens        ← cache_read_input_tokens
//   - usage.completion_tokens_details.reasoning_tokens ← reasoning_tokens
//
// 两个 details 对象始终存在（值可为 0），保证与 OpenAI 标准响应结构一致。
func NormalizeUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	g := func(k string) int64 {
		if v, ok := u[k].(float64); ok {
			return int64(v)
		}
		return 0
	}
	ptd, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok {
		ptd = map[string]any{}
		u["prompt_tokens_details"] = ptd
	}
	if _, ok := ptd["cached_tokens"]; !ok {
		ptd["cached_tokens"] = g("cache_read_input_tokens")
	}
	ctd, ok := u["completion_tokens_details"].(map[string]any)
	if !ok {
		ctd = map[string]any{}
		u["completion_tokens_details"] = ctd
	}
	if _, ok := ctd["reasoning_tokens"]; !ok {
		ctd["reasoning_tokens"] = g("reasoning_tokens")
	}
	return u
}
