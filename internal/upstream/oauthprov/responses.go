package oauthprov

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// 本文件实现 OpenAI Chat Completions ⇄ Responses API 的翻译。
//
// ChatGPT 的订阅线路（Codex）只提供 Responses API，而本代理对外的契约是
// Chat Completions。翻译只覆盖两套协议的标准交集：system→instructions、
// 文本/图片消息、function 工具、tool 调用与结果、reasoning 增量、usage。

// reservedChatFields 是请求体里已由转换逻辑消费的字段：不能作为 extra
// 原样透传，否则会与转换后的字段冲突。
//
// 只列真正冲突的键。采样参数（temperature / top_p / stop / seed /
// presence_penalty / frequency_penalty）在 Responses 里同名同义，必须
// 透传——把它们列进来会让用户设的采样参数静默失效。
var reservedChatFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"tools": true, "tool_choice": true, "functions": true, "function_call": true,
	"max_tokens": true, "max_completion_tokens": true, "response_format": true,
	"n": true, "logprobs": true, "top_logprobs": true, "metadata": true,
	"parallel_tool_calls": true, "reasoning_effort": true,
}

// OpenAIChatToResponses 把 Chat Completions 请求体翻译成 Responses 请求体。
//
// codexEndpoint 区分两条 Responses 线路：
//   - true （ChatGPT 订阅）：上游完全拒绝 max_output_tokens，不发送；
//   - false（xAI 等标准 Responses）：保留输出上限，否则用户设的
//     max_tokens 会被静默丢弃，回复长度不受控。
//
// instructions 承载 system/developer 消息，其余消息转成 input 条目。
func OpenAIChatToResponses(payload map[string]any, model string, codexEndpoint bool) map[string]any {
	out := map[string]any{
		"model":  model,
		"stream": true,
		// Codex 线路不落库：请求内容只在本次调用内使用。
		"store": false,
	}
	if !codexEndpoint {
		if v := maxOutputTokensOf(payload); v > 0 {
			out["max_output_tokens"] = v
		}
	}

	var instructions []string
	var input []any
	for _, raw := range sliceOf(payload["messages"]) {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			if text := messageText(msg); text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			input = append(input, responsesMessage("user", msg["content"], "input_text"))
		case "assistant":
			// 文本与工具调用在同一条 assistant 消息里时，Responses 要求拆成
			// 独立条目：先消息（若有文本），再逐个 function_call。
			if text := messageText(msg); text != "" {
				input = append(input, responsesMessage("assistant", text, "output_text"))
			}
			for _, call := range sliceOf(msg["tool_calls"]) {
				item, ok := call.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := item["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				id, _ := item["id"].(string)
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   id,
					"name":      name,
					"arguments": args,
				})
			}
		case "tool":
			callID, _ := msg["tool_call_id"].(string)
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  messageText(msg),
			})
		}
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n")
	}
	if input == nil {
		input = []any{}
	}
	out["input"] = input

	if tools := convertChatToolsToResponses(payload["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc := convertChatToolChoice(payload["tool_choice"]); tc != nil {
			out["tool_choice"] = tc
		}
	}
	if v, ok := payload["parallel_tool_calls"].(bool); ok {
		out["parallel_tool_calls"] = v
	}
	// reasoning_effort 在 Chat 里是顶层字符串，Responses 里是
	// {"reasoning":{"effort":...}}。直接透传会被上游忽略。
	if effort, ok := payload["reasoning_effort"].(string); ok && strings.TrimSpace(effort) != "" {
		out["reasoning"] = map[string]any{"effort": strings.TrimSpace(effort)}
	}
	if format := convertChatResponseFormat(payload["response_format"]); format != nil {
		out["text"] = map[string]any{"format": format}
	}
	// 其余未消费字段原样透传（各家扩展参数靠这条路）。
	for key, value := range payload {
		if reservedChatFields[key] {
			continue
		}
		out[key] = value
	}
	return out
}

// responsesMessage 构造一条 Responses input 消息。content 是字符串时包成
// 对应类型的文本块，数组则逐块映射（图片走 input_image）。
func responsesMessage(role string, content any, textType string) map[string]any {
	parts := []any{}
	switch v := content.(type) {
	case string:
		if v != "" {
			parts = append(parts, map[string]any{"type": textType, "text": v})
		}
	case []any:
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if converted := convertContentPart(part, textType); converted != nil {
				parts = append(parts, converted)
			}
		}
	}
	return map[string]any{"type": "message", "role": role, "content": parts}
}

// convertContentPart 把 Chat 的内容块映射成 Responses 的 input 块。
func convertContentPart(part map[string]any, textType string) map[string]any {
	kind, _ := part["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "text", "input_text", "output_text":
		text, _ := part["text"].(string)
		if text == "" {
			return nil
		}
		return map[string]any{"type": textType, "text": text}
	case "image_url":
		url := imageURLOf(part)
		if url == "" {
			return nil
		}
		return map[string]any{"type": "input_image", "image_url": url}
	case "input_image":
		return part
	default:
		return nil
	}
}

func imageURLOf(part map[string]any) string {
	switch v := part["image_url"].(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if url, ok := v["url"].(string); ok {
			return strings.TrimSpace(url)
		}
	}
	return ""
}

// convertChatToolsToResponses 把 {type:function,function:{...}} 摊平成
// Responses 的 {type:function,name,description,parameters}。
func convertChatToolsToResponses(raw any) []any {
	var out []any
	for _, item := range sliceOf(raw) {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			// 已是 Responses 形状：原样保留。
			if t, _ := tool["type"].(string); t == "function" {
				out = append(out, tool)
			}
			continue
		}
		converted := map[string]any{"type": "function"}
		if name, _ := fn["name"].(string); name != "" {
			converted["name"] = name
		}
		if desc, _ := fn["description"].(string); desc != "" {
			converted["description"] = desc
		}
		if params, ok := fn["parameters"]; ok {
			converted["parameters"] = params
		}
		if strict, ok := fn["strict"].(bool); ok {
			converted["strict"] = strict
		}
		out = append(out, converted)
	}
	return out
}

// convertChatToolChoice 把 tool_choice 转成 Responses 形状。
func convertChatToolChoice(raw any) any {
	switch v := raw.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "auto", "none", "required":
			return strings.ToLower(strings.TrimSpace(v))
		}
		return nil
	case map[string]any:
		fn, ok := v["function"].(map[string]any)
		if !ok {
			return nil
		}
		if name, _ := fn["name"].(string); name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return nil
}

// convertChatResponseFormat 把 response_format 转成 Responses 的 text.format。
func convertChatResponseFormat(raw any) map[string]any {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	kind, _ := obj["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		out := map[string]any{"type": "json_schema"}
		if schema, ok := obj["json_schema"]; ok {
			out["json_schema"] = schema
		}
		return out
	case "text":
		return map[string]any{"type": "text"}
	}
	return nil
}

// ResponsesToOpenAIChat 把非流式的 Responses 响应折叠成 Chat Completions 响应。
func ResponsesToOpenAIChat(raw []byte, model string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("解析 Codex 响应失败: %w", err)
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		if strings.TrimSpace(msg) == "" {
			msg = "upstream returned an error"
		}
		return json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": "upstream_error"},
		})
	}

	var text, reasoning strings.Builder
	var toolCalls []any
	for _, item := range sliceOf(body["output"]) {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch kind, _ := entry["type"].(string); kind {
		case "message":
			for _, part := range sliceOf(entry["content"]) {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := p["type"].(string); t == "output_text" || t == "text" {
					if v, _ := p["text"].(string); v != "" {
						text.WriteString(v)
					}
				}
			}
		case "reasoning":
			for _, part := range sliceOf(entry["summary"]) {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if v, _ := p["text"].(string); v != "" {
					reasoning.WriteString(v)
				}
			}
		case "function_call":
			name, _ := entry["name"].(string)
			args, _ := entry["arguments"].(string)
			id, _ := entry["call_id"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		}
	}

	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	if status, _ := body["status"].(string); status == "incomplete" {
		finish = "length"
	}

	out := map[string]any{
		"id":      responseID(body),
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{
			map[string]any{"index": 0, "message": message, "finish_reason": finish},
		},
	}
	if usage := responsesUsage(body["usage"]); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// TranslateResponsesStream 把 Responses SSE 流翻译成 OpenAI Chat SSE 流。
//
// 关键事件：
//
//	response.output_text.delta            → delta.content
//	response.reasoning*.delta             → delta.reasoning_content
//	response.function_call_arguments.delta→ delta.tool_calls[].function.arguments
//	response.output_item.added            → 工具调用的 id/name 帧
//	response.completed / incomplete       → 末帧 + usage + [DONE]
func TranslateResponsesStream(src io.Reader, dst io.Writer, model string) error {
	return translateResponsesStream(src, dst, model)
}

func responseID(body map[string]any) string {
	if id, ok := body["id"].(string); ok && id != "" {
		return id
	}
	return "chatcmpl-codex"
}

// responsesUsage 把 Responses 的 usage 翻成 Chat 的 usage 形状。
func responsesUsage(raw any) map[string]any {
	usage, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	input := intOf(usage["input_tokens"])
	output := intOf(usage["output_tokens"])
	if input == 0 && output == 0 {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      input + output,
	}
}

// maxOutputTokensOf 从 Chat 请求体里取输出上限（max_completion_tokens
// 优先于 max_tokens，与 OpenAI 的语义一致）。
func maxOutputTokensOf(payload map[string]any) int {
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if v := intOf(payload[key]); v > 0 {
			return v
		}
	}
	return 0
}

// messageText 取消息的纯文本内容（字符串或文本块数组）。
func messageText(msg map[string]any) string {
	switch v := msg["content"].(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := part["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// sliceOf 把任意值当成 []any（nil 返回 nil）。
func sliceOf(raw any) []any {
	if v, ok := raw.([]any); ok {
		return v
	}
	return nil
}

// intOf 把 JSON 数字（float64 / int）读成 int。
func intOf(raw any) int {
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}
