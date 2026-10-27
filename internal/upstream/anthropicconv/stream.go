package anthropicconv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// TranslateStream 把 Anthropic Messages 的 SSE 流逐事件翻译成 OpenAI
// Chat Completions 的 SSE 流，写入 dst。
//
// 上游的 Anthropic SSE 形如：
//
//	event: message_start
//	data: {"type":"message_start","message":{...}}
//	event: content_block_delta
//	data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}
//	event: message_delta
//	data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{...}}
//	event: message_stop
//	data: {"type":"message_stop"}
//
// 翻译后写入 OpenAI 形状的事件流（role / delta.content / delta.tool_calls /
// usage / [DONE]）。dst 一般由 *io.PipeWriter 驱动，使调用方拿到的
// *http.Response 已经是 OpenAI SSE，可直接被代理服务层透传或按首事件换号。
//
// 上游 HTTP 状态 >= 400 并非 SSE，应在调用本函数之前由 Chat 层单独处理
// （把 Anthropic 错误体翻译成 OpenAI 错误 JSON）。
func TranslateStream(src io.Reader, dst io.Writer, model string) error {
	reader := bufio.NewReader(src)

	var (
		curToolID   string
		curToolName string
		toolIndex   int  // 已开始的工具调用计数（含当前未闭合的）
		toolOpen    bool // 当前是否处于一个工具调用参数累积中
		toolInput   strings.Builder
		textBuf     strings.Builder
		sentRole    bool
		inputTokens int // 来自 message_start.message.usage.input_tokens
	)

	write := func(v map[string]any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(dst, "data: %s\n\n", data)
		return err
	}
	emitOpen := func() error {
		if sentRole {
			return nil
		}
		sentRole = true
		return write(map[string]any{
			"id":      "chatcmpl-translate",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}},
			},
		})
	}
	emitText := func(text string) error {
		if text == "" {
			return nil
		}
		if err := emitOpen(); err != nil {
			return err
		}
		return write(map[string]any{
			"id":      "chatcmpl-translate",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": text}},
			},
		})
	}
	// emitReasoning 把 Anthropic 的 thinking 增量暴露成 OpenAI 的
	// delta.reasoning_content（与本项目其它上游的思考内容透传口径一致）。
	emitReasoning := func(text string) error {
		if text == "" {
			return nil
		}
		if err := emitOpen(); err != nil {
			return err
		}
		return write(map[string]any{
			"id":      "chatcmpl-translate",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": text}},
			},
		})
	}
	// flushTool 把当前已累积参数的工具调用作为一帧 tool_calls 发出。
	flushTool := func() error {
		if !toolOpen {
			return nil
		}
		if err := emitOpen(); err != nil {
			return err
		}
		args := toolInput.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if err := write(map[string]any{
			"id":      "chatcmpl-translate",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": toolIndex - 1,
								"id":    curToolID,
								"type":  "function",
								"function": map[string]any{
									"name":      curToolName,
									"arguments": args,
								},
							},
						},
					},
				},
			},
		}); err != nil {
			return err
		}
		toolOpen = false
		toolInput.Reset()
		return nil
	}
	flushText := func() error {
		t := textBuf.String()
		textBuf.Reset()
		return emitText(t)
	}

	for {
		eventType, data, err := readSSEEvent(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			// 流被截断：把已缓冲内容刷出并补一个完整结束帧，避免客户端挂死。
			_ = flushText()
			_ = flushTool()
			_ = writeDone(dst, model)
			return nil
		}
		if eventType == "" || data == "" {
			continue
		}
		switch eventType {
		case "message_start":
			// 记录输入 token 数（Anthropic 在 message_start 里给 input_tokens，
			// message_delta 通常只给 output_tokens）；不产出 OpenAI 事件。
			var evt struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(data), &evt) == nil && evt.Message.Usage.InputTokens > 0 {
				inputTokens = evt.Message.Usage.InputTokens
			}
		case "content_block_start":
			var evt struct {
				Index int `json:"index"`
				Block struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(data), &evt); err == nil && evt.Block.Type == "tool_use" {
				// 新工具调用：先刷掉之前缓冲的文本与工具。
				if err := flushText(); err != nil {
					return err
				}
				if err := flushTool(); err != nil {
					return err
				}
				curToolID = evt.Block.ID
				curToolName = evt.Block.Name
				toolIndex++
				toolOpen = true
				toolInput.Reset()
			}
		case "content_block_delta":
			var evt struct {
				Delta json.RawMessage `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				continue
			}
			var dtype struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			if err := json.Unmarshal(evt.Delta, &dtype); err != nil {
				continue
			}
			switch dtype.Type {
			case "text_delta":
				// 防御：若上一个工具块还没闭合就来了文本增量，先把工具刷出去，
				// 否则工具参数会被后续文本覆盖丢失。
				if toolOpen {
					if err := flushTool(); err != nil {
						return err
					}
				}
				if err := emitText(dtype.Text); err != nil {
					return err
				}
			case "thinking_delta":
				if err := emitReasoning(dtype.Thinking); err != nil {
					return err
				}
			case "input_json_delta":
				// 工具参数增量：先刷掉文本块（若有），再进入工具增量。
				if !toolOpen {
					if err := flushText(); err != nil {
						return err
					}
					toolOpen = true
					toolIndex++
					curToolID = fmt.Sprintf("tool_%d", toolIndex)
					curToolName = "function"
				}
				toolInput.WriteString(dtype.PartialJSON)
			}
		case "content_block_stop":
			if toolOpen {
				if err := flushTool(); err != nil {
					return err
				}
			}
		case "message_delta":
			var evt struct {
				Delta json.RawMessage `json:"delta"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				continue
			}
			if err := flushText(); err != nil {
				return err
			}
			if err := flushTool(); err != nil {
				return err
			}
			promptTokens := evt.Usage.InputTokens
			if promptTokens == 0 {
				promptTokens = inputTokens
			}
			if err := emitOpen(); err != nil {
				return err
			}
			_ = write(map[string]any{
				"id":      "chatcmpl-translate",
				"object":  "chat.completion.chunk",
				"created": 0,
				"model":   model,
				"choices": []any{
					map[string]any{
						"index":         0,
						"delta":         map[string]any{},
						"finish_reason": stopReasonToFinish(deltaStopReason(evt.Delta)),
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     promptTokens,
					"completion_tokens": evt.Usage.OutputTokens,
					"total_tokens":      promptTokens + evt.Usage.OutputTokens,
				},
			})
		case "message_stop":
			if err := flushText(); err != nil {
				return err
			}
			if err := flushTool(); err != nil {
				return err
			}
			_ = writeDone(dst, model)
			return nil
		case "error":
			if err := flushText(); err != nil {
				return err
			}
			if err := flushTool(); err != nil {
				return err
			}
			var e struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(data), &e)
			_ = write(map[string]any{
				"error": map[string]any{
					"message": e.Error.Message,
					"type":    e.Error.Type,
				},
			})
			_ = writeDone(dst, model)
			return nil
		default:
			// 未知事件类型：安全忽略，绝不中断流。
		}
	}
	// 流未以 message_stop 正常结束：兜底结束。
	if err := flushText(); err != nil {
		return err
	}
	if err := flushTool(); err != nil {
		return err
	}
	_ = writeDone(dst, model)
	return nil
}

func writeDone(dst io.Writer, _ string) error {
	_, err := io.WriteString(dst, "data: [DONE]\n\n")
	return err
}

func deltaStopReason(raw json.RawMessage) string {
	var d struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &d); err == nil {
		return d.StopReason
	}
	return ""
}

// readSSEEvent 从缓冲读取器读出一个 SSE 事件的 (event, data)。返回 io.EOF
// 表示流结束。事件由连续的 "event:" 与 "data:" 行组成，空行分隔。
func readSSEEvent(r *bufio.Reader) (string, string, error) {
	var eventType, data string
	sawField := false
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if err != nil {
			if err == io.EOF && sawField {
				applySSELine(line, &eventType, &data, &sawField)
				if sawField {
					return eventType, data, nil
				}
			}
			return "", "", io.EOF
		}
		if line == "" {
			if sawField {
				return eventType, data, nil
			}
			continue
		}
		applySSELine(line, &eventType, &data, &sawField)
	}
}

func applySSELine(line string, eventType, data *string, sawField *bool) {
	if after, ok := strings.CutPrefix(line, "event:"); ok {
		*eventType = strings.TrimSpace(after)
		*sawField = true
		return
	}
	if after, ok := strings.CutPrefix(line, "data:"); ok {
		*data = strings.TrimSpace(after)
		*sawField = true
		return
	}
	// 注释行（以 : 开头）或未知前缀：忽略。
}
