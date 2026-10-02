// usagefmt.go token_usage 字段规范化：上游为 Anthropic 风格键，
// 在保留原字段的同时补齐 OpenAI 标准结构，客户端可直接读取缓存命中数。
package upstream

// NormalizeUsage 补齐 OpenAI 标准字段（就地补，不丢上游原字段）：
//   - prompt_tokens_details.cached_tokens    ← cache_read_input_tokens
//   - completion_tokens_details.reasoning_tokens ← reasoning_tokens
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
	if _, ok := u["prompt_tokens_details"]; !ok {
		if cr := g("cache_read_input_tokens"); cr > 0 {
			u["prompt_tokens_details"] = map[string]any{"cached_tokens": cr}
		}
	}
	if _, ok := u["completion_tokens_details"]; !ok {
		if rt := g("reasoning_tokens"); rt > 0 {
			u["completion_tokens_details"] = map[string]any{"reasoning_tokens": rt}
		}
	}
	return u
}
