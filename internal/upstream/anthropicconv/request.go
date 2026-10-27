// Package anthropicconv 实现 OpenAI Chat Completions 与 Anthropic Messages
// 两套协议之间的请求/响应互译，使本代理能把 OpenAI 风格的对话请求转发到
// Anthropic 线协议的上游（如 api.anthropic.com / 智谱编程的 Anthropic 网关），
// 并把上游的 Anthropic SSE 流实时翻译成 OpenAI SSE 回给客户端。
//
// 翻译只覆盖两套协议的标准交集：system 提取、多模态文本块、tools /
// tool_choice、thinking（extended thinking）、usage。专属于某一侧的扩展
// （如 OpenAI 的 response_format、Anthropic 的 citations）做尽量安全的丢弃
// 或透传，保证流不被中断。
package anthropicconv

import (
	"encoding/json"
	"strings"
)

// RequestOpenAIToAnthropic 把一份已解析的 OpenAI Chat Completions 请求体翻译成
// Anthropic Messages 请求体。model 是上游实际模型名（OpenAI 请求体里的 model
// 在本代理边界已被改写成上游模型名）。
//
// 返回的 map 已包含 Anthropic 所需字段：model、stream、max_tokens、messages、
// system（若有）、tools（若有）、tool_choice（若有）、thinking（若启用）。
func RequestOpenAIToAnthropic(body map[string]any, model string) map[string]any {
	req := map[string]any{
		"model":  model,
		"stream": true,
	}

	// max_tokens 缺省给一个对工具循环安全的兜底值（Anthropic 强制要求该字段）。
	if mt, ok := num(body["max_tokens"]); ok && mt > 0 {
		req["max_tokens"] = mt
	} else if len(sliceOf(body["tools"])) > 0 {
		req["max_tokens"] = 8192
	} else {
		req["max_tokens"] = 4096
	}

	// system 从首条 role=system 消息中提取，并在 messages 中剔除该条。
	system, messages := splitSystem(body["messages"])
	if system != "" {
		req["system"] = system
	}
	req["messages"] = messages

	if tools := sliceOf(body["tools"]); len(tools) > 0 {
		req["tools"] = convertTools(tools)
		// tool_choice 的两种协议形状不同，必须翻译（见 convertToolChoice）。
		if tc := body["tool_choice"]; tc != nil {
			if converted := convertToolChoice(tc); converted != nil {
				req["tool_choice"] = converted
			}
		}
	}
	// 常见采样参数：Anthropic 支持 temperature / top_p / stop_sequences。
	if v, ok := num(body["temperature"]); ok {
		req["temperature"] = v
	}
	if v, ok := num(body["top_p"]); ok {
		req["top_p"] = v
	}
	if stop := convertStop(body["stop"]); stop != nil {
		req["stop_sequences"] = stop
	}
	// 透传 thinking（extended thinking）：OpenAI 侧若带了 thinking 块，原样
	// 携带到 Anthropic——Anthropic 的 extended thinking 是原生概念。
	// 但 Anthropic 要求 max_tokens > budget_tokens，客户端给反了会被 400 拒绝，
	// 这里按预算上调 max_tokens 或下调 budget 兜底。
	if thinking := mapOf(body["thinking"]); thinking != nil {
		if clampThinkingBudget(thinking, req) {
			req["thinking"] = thinking
		}
	}
	return req
}

// clampThinkingBudget 保证 Anthropic 的「max_tokens > budget_tokens >= 1024」约束。
// 返回是否应下发 thinking 块。仅在 type=enabled 时校验；其它类型原样返回 true。
func clampThinkingBudget(thinking map[string]any, req map[string]any) bool {
	if !strings.EqualFold(strings.TrimSpace(strOf(thinking["type"])), "enabled") {
		return true
	}
	maxTokens, ok := num(req["max_tokens"])
	if !ok || maxTokens <= 0 {
		maxTokens = 4096
		req["max_tokens"] = maxTokens
	}
	budget, hasBudget := num(thinking["budget_tokens"])
	if !hasBudget || budget <= 0 {
		budget = 4096
	}
	// 预算必须严格小于 max_tokens；且不低于 1024。max_tokens 太小时先抬 max_tokens。
	const minBudget = 1024
	if maxTokens <= minBudget {
		maxTokens = minBudget + 1024
		req["max_tokens"] = maxTokens
	}
	if budget >= maxTokens {
		budget = maxTokens - 1
	}
	if budget < minBudget {
		budget = minBudget
	}
	thinking["budget_tokens"] = budget
	return true
}

// convertToolChoice 把 OpenAI 的 tool_choice 翻成 Anthropic 形状：
//
//	"auto"                    → {"type":"auto"}
//	"required"                → {"type":"any"}
//	"none"                    → nil（Anthropic 没有 none；省略等于默认 auto）
//	{"type":"function",...}   → {"type":"tool","name":...}
func convertToolChoice(raw any) any {
	switch v := raw.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		default: // none / 未知：省略，交给上游默认
			return nil
		}
	case map[string]any:
		if fn := mapOf(v["function"]); fn != nil {
			if name := strings.TrimSpace(strOf(fn["name"])); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		// 已经是 Anthropic 形状（{"type":"auto"|"any"|"tool",...}）时原样保留。
		switch strings.ToLower(strings.TrimSpace(strOf(v["type"]))) {
		case "auto", "any":
			return map[string]any{"type": strings.ToLower(strings.TrimSpace(strOf(v["type"])))}
		case "tool":
			if name := strings.TrimSpace(strOf(v["name"])); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// convertStop 把 OpenAI 的 stop（字符串或数组）翻成 Anthropic 的 stop_sequences。
func convertStop(raw any) any {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return []any{v}
		}
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if s := strOf(item); strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// AnthropicToOpenAIResponse 把一份已解析的 Anthropic Messages 响应翻译成
// OpenAI Chat Completions 响应（非流式）。用于非流式场景的兜底折叠。
func AnthropicToOpenAIResponse(body map[string]any, model string) map[string]any {
	msg := map[string]any{
		"id":      strOf(body["id"]),
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{},
	}
	if msg["id"] == "" {
		msg["id"] = "msg_unknown"
	}

	content := buildOpenAIContent(body["content"], sliceOf(body["content"]))
	choice := map[string]any{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": content},
		"finish_reason": stopReasonToFinish(strOf(body["stop_reason"])),
	}

	// tool_calls：Anthropic 的 tool_use 内容块翻译成 OpenAI 的 tool_calls。
	if tcs := extractToolCalls(sliceOf(body["content"])); len(tcs) > 0 {
		asst := choice["message"].(map[string]any)
		asst["tool_calls"] = tcs
		if _, ok := asst["content"]; !ok || asst["content"] == "" {
			asst["content"] = nil
		}
	}
	msg["choices"] = []any{choice}

	if u := mapOf(body["usage"]); u != nil {
		msg["usage"] = openAIUsage(u)
	} else {
		msg["usage"] = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	return msg
}

// ---------------------------------------------------------------------------
// 消息 / system 处理
// ---------------------------------------------------------------------------

// splitSystem 从 messages 中提取首条 system 文本，并返回剔除该条后的 messages。
// Anthropic 用顶层 system 字段，而 OpenAI 把 system 作为一条 message。
func splitSystem(rawMessages any) (string, []any) {
	msgs := sliceOf(rawMessages)
	out := make([]any, 0, len(msgs))
	var system string
	// tool_results 收集连续的 tool 消息，合并成一条 user 消息的多个 tool_result
	// 块——Anthropic 要求 tool_result 紧跟在对应 tool_use 之后、且同一轮的多条
	// 结果放在同一条 user 消息里；逐条拆开会变成连续 user 消息被拒。
	var toolResults []any
	flushTools := func() {
		if len(toolResults) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": toolResults})
		toolResults = nil
	}
	for _, m := range msgs {
		mm := mapOf(m)
		if mm == nil {
			continue
		}
		role := strOf(mm["role"])
		// 任何位置的 system 消息都归入顶层 system 字段：Anthropic 的 messages
		// 只接受 user / assistant，夹一条 system 会被拒。
		if equalFold(role, "system") || equalFold(role, "developer") {
			if text := stringifyContent(mm["content"]); text != "" {
				if system != "" {
					system += "\n\n" + text
				} else {
					system = text
				}
			}
			continue
		}
		if equalFold(role, "tool") {
			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": strOf(mm["tool_call_id"]),
				"content":     stringifyContent(mm["content"]),
			})
			continue
		}
		flushTools()
		out = append(out, convertMessage(mm))
	}
	flushTools()
	return system, mergeConsecutiveRoles(out)
}

// mergeConsecutiveRoles 合并相邻的同角色消息。Anthropic 要求 messages 严格
// 交替 user / assistant，而 OpenAI 客户端可能连续发多条同角色消息（尤其是
// 把多条 tool 结果或多次补充说明拆开），直接透传会被上游以 400 拒绝。
func mergeConsecutiveRoles(msgs []any) []any {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		m := mapOf(raw)
		if m == nil {
			continue
		}
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := mapOf(out[len(out)-1])
		if prev == nil || !equalFold(strOf(prev["role"]), strOf(m["role"])) {
			out = append(out, m)
			continue
		}
		// 同角色：把内容块拼到前一条上。
		prev["content"] = appendContent(prev["content"], m["content"])
	}
	return out
}

// appendContent 把两段 Anthropic 内容（字符串或内容块数组）拼接成内容块数组。
func appendContent(a, b any) any {
	blocksA := toBlocks(a)
	blocksB := toBlocks(b)
	return append(blocksA, blocksB...)
}

// toBlocks 把字符串或内容块数组统一成内容块数组。
func toBlocks(content any) []any {
	switch v := content.(type) {
	case []any:
		return v
	case string:
		if v == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": v}}
	default:
		return nil
	}
}

// convertMessage 把单条 OpenAI 消息翻译成 Anthropic 形状。
//
// 角色映射：system 不应出现在这里（已在 splitSystem 提取）；assistant 的
// tool_calls 翻译成 Anthropic 的 tool_use 内容块；tool 角色翻译成 tool_result
// 内容块；其它保持 role + content。
func convertMessage(m map[string]any) map[string]any {
	role := strOf(m["role"])
	switch role {
	case "tool":
		// OpenAI 的 tool 消息承载 tool_result。
		return map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": strOf(m["tool_call_id"]),
					"content":     stringifyContent(m["content"]),
				},
			},
		}
	case "assistant":
		out := map[string]any{"role": "assistant"}
		content := []any{}
		if c := stringifyContent(m["content"]); c != "" {
			content = append(content, map[string]any{"type": "text", "text": c})
		}
		// tool_calls → tool_use 块。
		for _, tc := range sliceOf(m["tool_calls"]) {
			tcm := mapOf(tc)
			if tcm == nil {
				continue
			}
			fn := mapOf(tcm["function"])
			args := fn["arguments"]
			if args == nil {
				args = fn["parameters"]
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    strOf(tcm["id"]),
				"name":  strOf(fn["name"]),
				"input": normalizeToolInput(args),
			})
		}
		out["content"] = content
		return out
	default:
		// user 或任何其它角色：内容块化。
		return map[string]any{
			"role":    role,
			"content": contentBlocks(m["content"]),
		}
	}
}

// contentBlocks 把 OpenAI 的 content（字符串或分段数组）翻译成 Anthropic 的
// 内容块数组。
func contentBlocks(content any) any {
	if arr, ok := content.([]any); ok {
		blocks := make([]any, 0, len(arr))
		for _, p := range arr {
			if pm := mapOf(p); pm != nil {
				// 文本 / 图片块：只保留 text 与 image_url。
				if t, ok := pm["text"].(string); ok {
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
					continue
				}
				if img, ok := pm["image_url"].(map[string]any); ok {
					if block := imageBlock(strOf(img["url"])); block != nil {
						blocks = append(blocks, block)
					}
					continue
				}
			}
			// 纯文本（字符串元素）。
			if s, ok := p.(string); ok {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
		}
		if len(blocks) > 0 {
			return blocks
		}
	}
	if s, ok := content.(string); ok {
		return s
	}
	return content
}

// imageBlock 把 OpenAI 的 image_url 翻成 Anthropic 的 image 内容块。data: URL
// （base64 内联）转成 base64 source——Anthropic 对它的支持面远大于 url source；
// 普通 http(s) URL 保留 url source。
func imageBlock(rawURL string) map[string]any {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if strings.HasPrefix(rawURL, "data:") {
		// data:[<mediatype>];base64,<data>
		rest := strings.TrimPrefix(rawURL, "data:")
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return nil
		}
		meta := rest[:comma]
		data := rest[comma+1:]
		mediaType := strings.TrimSuffix(meta, ";base64")
		mediaType = strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0])
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		if data == "" {
			return nil
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  rawURL,
		},
	}
}

// convertTools 把 OpenAI tools 翻译成 Anthropic tools（仅 function 类型）。
func convertTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm := mapOf(t)
		if tm == nil {
			continue
		}
		fn := mapOf(tm["function"])
		if fn == nil {
			continue
		}
		out = append(out, map[string]any{
			"name":         strOf(fn["name"]),
			"description":  strOf(fn["description"]),
			"input_schema": fn["parameters"],
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 响应回译
// ---------------------------------------------------------------------------

// buildOpenAIContent 从 Anthropic 的 content（数组或字符串）构造 OpenAI 的文本
// content。少数兼容网关会把 content 直接给成字符串，这里一并支持。
func buildOpenAIContent(raw any, blocks []any) any {
	if s, ok := raw.(string); ok {
		if s == "" {
			return nil
		}
		return s
	}
	var text string
	for _, b := range blocks {
		bm := mapOf(b)
		if bm == nil {
			continue
		}
		if strOf(bm["type"]) == "text" {
			text += strOf(bm["text"])
		}
	}
	if text == "" {
		return nil
	}
	return text
}

// extractToolCalls 从 Anthropic content 提取 tool_use 块，翻译成 OpenAI tool_calls。
func extractToolCalls(blocks []any) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		bm := mapOf(b)
		if bm == nil || strOf(bm["type"]) != "tool_use" {
			continue
		}
		rawInput, _ := json.Marshal(bm["input"])
		out = append(out, map[string]any{
			"id":   strOf(bm["id"]),
			"type": "function",
			"function": map[string]any{
				"name":      strOf(bm["name"]),
				"arguments": string(rawInput),
			},
		})
	}
	return out
}

// openAIUsage 把 Anthropic usage 翻译成 OpenAI usage。
func openAIUsage(u map[string]any) map[string]any {
	in, _ := num(u["input_tokens"])
	out, _ := num(u["output_tokens"])
	return map[string]any{
		"prompt_tokens":     int(in),
		"completion_tokens": int(out),
		"total_tokens":      int(in + out),
	}
}

func stopReasonToFinish(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// normalizeToolInput 把 OpenAI 的 tool 参数（可能是字符串 JSON 或对象）规整为
// Anthropic 期望的对象。
func normalizeToolInput(args any) any {
	switch v := args.(type) {
	case string:
		var obj map[string]any
		if err := json.Unmarshal([]byte(v), &obj); err == nil {
			return obj
		}
		return map[string]any{}
	case map[string]any:
		return v
	default:
		return map[string]any{}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func sliceOf(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func equalFold(a, b string) bool {
	return len(a) == len(b) && lowerEqual(a, b)
}

func lowerEqual(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// stringifyContent 把 OpenAI 的 content（字符串或分段数组）摊平成文本。
func stringifyContent(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b []byte
		for _, part := range v {
			switch p := part.(type) {
			case string:
				b = append(b, p...)
			case map[string]any:
				if text, ok := p["text"].(string); ok {
					b = append(b, text...)
					continue
				}
				b = append(b, stringifyContent(p["content"])...)
			}
		}
		return string(b)
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return text
		}
		return stringifyContent(v["content"])
	}
	return ""
}
