// prompt.go 系统提示词策略：所有调用统一注入一份固定的 Trae 风格系统提示词，
// 保证「单一、干净、不叠加」——每个请求恰好一条 system 消息：
//
//	固定提示词（缓存友好的稳定前缀）
//	＋（可选）调用方自带 system 内容作为「用户附加指令」段追加在同一条消息内
//
// 这样既让上游提示词缓存（cache_read）能跨请求命中（前缀字节级一致），
// 又不会出现多条 system 叠加导致的提示词膨胀。
// mode: "trae"（默认，注入）/ "off"（透传调用方原文）。
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// defaultSystemPrompt 内置的 Trae 风格系统提示词。
// 长度刻意保持在 ~2000 字（跨过上游提示词缓存的建缓存门槛），
// 内容为通用编程助手准则——不绑定具体项目上下文。
const defaultSystemPrompt = `你是 Trae，一款 AI IDE 内置的智能编程助手，服务于开发者日常的编码、调试、重构与学习场景。

# 身份与语言
- 始终以专业、克制、务实的口吻回答。
- 默认使用简体中文思考和作答；代码、命令、标识符保持英文原文。
- 用户使用其他语言提问时，跟随用户的语言。

# 核心能力
- 阅读、解释与审查代码：说明逻辑、指出缺陷、评估复杂度与边界条件。
- 编写与修改代码：给出可直接运行的完整片段，标注所需依赖与版本。
- 调试排错：先陈述对根因的判断与依据，再给出最小修复，最后说明验证方式。
- 架构与选型：比较候选方案的取舍（性能、可维护性、生态、成本），给出明确建议而非罗列。

# 行为准则
- 先理解再动手：信息不足时，先列出关键假设；假设会改变结论时，主动提问。
- 事实与推测分开表述：确证的给结论与证据，推测的标注「推测」。
- 不确定的内容明确说不知道，不编造 API、库名或参数。
- 修改代码时尊重既有风格：命名、注释密度、错误处理方式与周边代码保持一致。
- 涉及删除、覆盖、迁移等破坏性操作前，先确认影响范围。

# 代码规范
- 代码块标注语言与文件路径（如已知）。
- 关键改动附带一句话说明意图；避免无意义的逐行注释。
- 给出最小可验证示例；避免大段与问题无关的样板代码。

# 输出格式
- 默认使用紧凑的 Markdown：短段落 + 列表 + 代码块。
- 结论先行：第一句话回答用户最关心的问题，细节随后展开。
- 数值对比使用表格；流程性内容使用有序列表。

# 安全与隐私
- 不输出密钥、令牌、口令等敏感凭据的明文；引用时使用占位符。
- 不协助任何违法或明显有害的请求；对模糊的灰色请求先澄清意图。`

// PromptConfig 提示词策略配置。
type PromptConfig struct {
	Mode string // "auto"（默认）= 调用方有 system 则透传、无则注入；"trae" = 总是注入（调用方 system 合并）；"replace" = 统一替换（调用方 system 丢弃）；"off" = 总是透传
	File string // 自定义提示词文件路径；空 = 内置默认
}

// fixedPrompt 返回生效的固定提示词（File 优先，其次内置默认）。
func fixedPrompt(cfg PromptConfig, dataDir string) string {
	path := strings.TrimSpace(cfg.File)
	if path != "" && !strings.Contains(path, string(os.PathSeparator)) {
		path = filepath.Join(dataDir, path)
	}
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
			return strings.TrimSpace(string(raw))
		}
	}
	return defaultSystemPrompt
}

// applyPromptPolicy 系统提示词策略（默认 auto）：
//
//	auto：调用方自带 system（agent 框架如 DeepSeek Harness/Cline）→ 原样透传，
//	      不注入不叠加（它们的提示词本身就是稳定的缓存前缀；叠加两套 agent
//	      提示词会导致行为混乱且成倍浪费 token）。裸调用（无 system）→ 注入
//	      固定的 Trae 风格提示词补足身份与行为基线。
//	trae：总是注入（调用方 system 合并为「用户附加指令」段）——指纹最大化。
//	replace：统一替换——调用方 system 全部丢弃（不透传、不合并），恒定只发
//	      一份固定的 Trae 风格提示词；行为交给对话内容决定，最小化 system 干扰。
//	      与 workbuddy2api 的 custom 模式同语义。
//	off ：总是透传。
func applyPromptPolicy(body []byte, cfg PromptConfig, dataDir string) []byte {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "off" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body
	}
	// 收集调用方全部 system 文本；其余消息原样保留（保持顺序）
	var callerSys []string
	kept := make([]any, 0, len(msgs))
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			kept = append(kept, mi)
			continue
		}
		if role, _ := m["role"].(string); role == "system" {
			if s := strings.TrimSpace(extractMessageText(m["content"])); s != "" {
				callerSys = append(callerSys, s)
			}
			continue
		}
		kept = append(kept, mi)
	}
	hasCallerSystem := len(callerSys) > 0
	if mode == "auto" && hasCallerSystem {
		// agent 调用方：保留其原 system（不注入、不叠加、不浪费 token）
		obj["messages"] = msgs
		return body
	}
	fixed := fixedPrompt(cfg, dataDir)
	merged := fixed
	// 仅 trae 模式合并调用方内容；replace/auto-裸调用 恒定只发固定提示词
	// （调用方 system 已在上面被剥离丢弃）。
	if mode == "trae" {
		for _, s := range callerSys {
			if s != fixed {
				merged += "\n\n# 用户附加指令\n\n" + s
			}
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": merged}}, kept...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// extractMessageText 从 OpenAI 消息 content（string 或多模态数组）提取纯文本。
func extractMessageText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if pm, ok := part.(map[string]any); ok {
				if t, _ := pm["type"].(string); t == "text" {
					if txt, _ := pm["text"].(string); txt != "" {
						if b.Len() > 0 {
							b.WriteString("\n")
						}
						b.WriteString(txt)
					}
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// extractSystemPrompt 从请求体提取（注入后的）系统提示词文本——明细展示用。
func extractSystemPrompt(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return ""
	}
	for _, mi := range msgs {
		if m, ok := mi.(map[string]any); ok {
			if role, _ := m["role"].(string); role == "system" {
				return extractMessageText(m["content"])
			}
		}
	}
	return ""
}
