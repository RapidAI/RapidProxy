package oauthprov

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// translateResponsesStream 是 Responses SSE → OpenAI Chat SSE 的翻译主体。
//
// 状态机只需要跟踪「当前正在累积的工具调用」：文本与思考增量可以逐帧直出，
// 工具调用需要把 id/name（来自 output_item.added）与参数增量（来自
// function_call_arguments.delta）拼成 OpenAI 的 tool_calls 帧。
func translateResponsesStream(src io.Reader, dst io.Writer, model string) error {
	reader := bufio.NewReader(src)

	var (
		responseID  string
		sentRole    bool
		textEmitted bool // 是否已经发过文本增量（用于 done 事件的兜底判重）
		toolIndex   = -1
		toolByItem  = map[int]int{} // Responses 的 output_index → OpenAI 的 tool 下标
		usage       map[string]any
		lastFinish  = "stop"
	)

	write := func(v map[string]any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(dst, "data: %s\n\n", data)
		return err
	}
	chunk := func(delta map[string]any) map[string]any {
		return map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}
	}
	ensureRole := func() error {
		if sentRole {
			return nil
		}
		sentRole = true
		return write(chunk(map[string]any{"role": "assistant"}))
	}
	emitText := func(text string) error {
		if text == "" {
			return nil
		}
		if err := ensureRole(); err != nil {
			return err
		}
		textEmitted = true
		return write(chunk(map[string]any{"content": text}))
	}
	emitReasoning := func(text string) error {
		if text == "" {
			return nil
		}
		if err := ensureRole(); err != nil {
			return err
		}
		return write(chunk(map[string]any{"reasoning_content": text}))
	}
	// emitToolStart 在工具调用首次出现时发一帧 id/name。
	emitToolStart := func(outputIndex int, callID, name string) error {
		if _, seen := toolByItem[outputIndex]; seen {
			return nil
		}
		if err := ensureRole(); err != nil {
			return err
		}
		toolIndex++
		toolByItem[outputIndex] = toolIndex
		lastFinish = "tool_calls"
		return write(chunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index": toolIndex,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": "",
				},
			}},
		}))
	}
	emitToolArgs := func(outputIndex int, args string) error {
		if args == "" {
			return nil
		}
		idx, ok := toolByItem[outputIndex]
		if !ok {
			// 没收到 output_item.added 就直接来了参数增量：补一帧起始。
			if err := emitToolStart(outputIndex, "", ""); err != nil {
				return err
			}
			idx = toolByItem[outputIndex]
		}
		return write(chunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index": idx,
				"function": map[string]any{
					"arguments": args,
				},
			}},
		}))
	}

	for {
		event, data, err := readSSE(reader)
		if err != nil {
			if err == io.EOF {
				break
			}
			// 流被截断：补齐结束帧，别让客户端挂死。
			break
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		var evt map[string]any
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		kind := strings.TrimSpace(event)
		if kind == "" {
			kind, _ = evt["type"].(string)
		}
		// 记录响应 ID（首帧即可确定，后续不应变化）。
		if responseID == "" {
			if id, _ := evt["response"].(map[string]any); id != nil {
				if v, _ := id["id"].(string); v != "" {
					responseID = v
				}
			}
		}
		if responseID == "" {
			if v, _ := evt["id"].(string); v != "" {
				responseID = v
			}
		}

		switch kind {
		case "response.output_text.delta":
			if err := emitText(stringOf(evt["delta"])); err != nil {
				return err
			}
		case "response.output_text.done":
			// 部分实现只在 done 里给全文；已发过文本增量时不重复。
			// 注意不能拿 sentRole 判断：只发过 reasoning 时它也是 true，
			// 那样会把唯一的文本内容丢掉。
			if !textEmitted {
				if err := emitText(stringOf(evt["text"])); err != nil {
					return err
				}
			}
		case "response.reasoning_summary_text.delta",
			"response.reasoning_text.delta",
			"response.reasoning_content.delta",
			"response.reasoning.delta":
			if err := emitReasoning(stringOf(evt["delta"])); err != nil {
				return err
			}
		case "response.output_item.added":
			item, _ := evt["item"].(map[string]any)
			if item == nil {
				continue
			}
			if t, _ := item["type"].(string); t == "function_call" {
				outputIndex := intOf(evt["output_index"])
				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				if err := emitToolStart(outputIndex, callID, name); err != nil {
					return err
				}
			}
		case "response.function_call_arguments.delta":
			if err := emitToolArgs(intOf(evt["output_index"]), stringOf(evt["delta"])); err != nil {
				return err
			}
		case "response.function_call_arguments.done":
			// 某些实现只在 done 里给完整参数；delta 缺失时补发。
			outputIndex := intOf(evt["output_index"])
			if _, ok := toolByItem[outputIndex]; !ok {
				if err := emitToolArgs(outputIndex, stringOf(evt["arguments"])); err != nil {
					return err
				}
			}
		case "response.completed", "response.incomplete":
			response, _ := evt["response"].(map[string]any)
			if response == nil {
				response = evt
			}
			if u := responsesUsage(response["usage"]); u != nil {
				usage = u
			}
			// incomplete 表示被截断（长度上限）；已有工具调用时仍以
			// tool_calls 收尾——客户端要靠它决定是否继续跑工具循环。
			if kind == "response.incomplete" && lastFinish != "tool_calls" {
				lastFinish = "length"
			}
			// completed 里也可能带完整文本（没有增量帧的兜底）。
			if !textEmitted {
				if text := outputTextOf(response); text != "" {
					if err := emitText(text); err != nil {
						return err
					}
				}
			}
		case "response.failed", "error":
			msg := "上游流式响应失败"
			// 错误可能挂在顶层 error、顶层 message，或 response.error 里
			// （response.failed 的常规形状）。
			if m := errorMessageOf(evt); m != "" {
				msg = m
			}
			errBody, _ := json.Marshal(map[string]any{
				"error": map[string]any{"message": msg, "type": "upstream_error"},
			})
			_, _ = fmt.Fprintf(dst, "data: %s\n\n", errBody)
			_, _ = io.WriteString(dst, "data: [DONE]\n\n")
			return nil
		}
	}

	if !sentRole {
		if err := ensureRole(); err != nil {
			return err
		}
	}
	final := map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": lastFinish}},
	}
	if usage != nil {
		final["usage"] = usage
	}
	if err := write(final); err != nil {
		return err
	}
	_, err := io.WriteString(dst, "data: [DONE]\n\n")
	return err
}

// outputTextOf 从 response.output 里取出全部文本（兜底路径）。
func outputTextOf(response map[string]any) string {
	var b strings.Builder
	for _, raw := range sliceOf(response["output"]) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := item["type"].(string); t != "message" {
			continue
		}
		for _, part := range sliceOf(item["content"]) {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if v, _ := p["text"].(string); v != "" {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

// stringOf 把任意值读成字符串。
func stringOf(raw any) string {
	if v, ok := raw.(string); ok {
		return v
	}
	return ""
}

// errorMessageOf 从错误事件里取出人类可读的消息。
//
// 三种形状都要认：{"error":{"message":...}}（流内 error 事件）、
// {"message":...}，以及 response.failed 的 {"response":{"error":{...}}}。
func errorMessageOf(evt map[string]any) string {
	if e, _ := evt["error"].(map[string]any); e != nil {
		if m := stringOf(e["message"]); m != "" {
			return m
		}
	}
	if m := stringOf(evt["message"]); m != "" {
		return m
	}
	if resp, _ := evt["response"].(map[string]any); resp != nil {
		if e, _ := resp["error"].(map[string]any); e != nil {
			if m := stringOf(e["message"]); m != "" {
				return m
			}
		}
	}
	return ""
}
