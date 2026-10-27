package oauthprov

import (
	"encoding/json"
	"strings"
	"testing"
)

// Chat 请求要转成 Responses 形状：system 进 instructions、assistant 的
// tool_calls 变 function_call 条目、tool 结果变 function_call_output。
func TestOpenAIChatToResponses(t *testing.T) {
	payload := map[string]any{
		"model": "gpt-5.6-luna",
		"messages": []any{
			map[string]any{"role": "system", "content": "be nice"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{
				"role":    "assistant",
				"content": "calling",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "lookup",
							"arguments": `{"q":"x"}`,
						},
					},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": "look things up",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
		"tool_choice":          "auto",
		"max_tokens":           100,
		"parallel_tool_calls":  true,
		"some_provider_option": "keep-me",
	}

	out := OpenAIChatToResponses(payload, "gpt-5.6-luna", true)

	if out["instructions"] != "be nice" {
		t.Errorf("instructions = %v, want \"be nice\"", out["instructions"])
	}
	if _, ok := out["max_output_tokens"]; ok {
		t.Error("Codex 线路不接受 max_output_tokens，不应出现在请求体里")
	}
	if out["store"] != false {
		t.Error("Codex 线路应显式 store=false")
	}
	if out["some_provider_option"] != "keep-me" {
		t.Error("未消费的扩展字段应原样透传")
	}
	if out["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", out["tool_choice"])
	}

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数量 = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "lookup" {
		t.Errorf("工具名应摊平到顶层，实际 %+v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Error("工具不应保留 function 包装")
	}

	input, _ := out["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input 条目数 = %d, want 4（user + assistant 消息 + function_call + output）", len(input))
	}
	assistant := input[1].(map[string]any)
	if assistant["type"] != "message" || assistant["role"] != "assistant" {
		t.Errorf("assistant 文本应作为独立消息条目，实际 %+v", assistant)
	}
	call := input[2].(map[string]any)
	if call["type"] != "function_call" {
		t.Errorf("assistant 的 tool_calls 应转成 function_call，实际 %+v", call)
	}
	if call["call_id"] != "call_1" || call["name"] != "lookup" {
		t.Errorf("function_call 字段异常: %+v", call)
	}
	output := input[3].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" {
		t.Errorf("tool 结果应转成 function_call_output，实际 %+v", output)
	}
}

// 多模态消息：文本与图片块要映射成 input_text / input_image。
func TestOpenAIChatToResponsesMultimodal(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what is this"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
				},
			},
		},
	}
	out := OpenAIChatToResponses(payload, "m", false)
	input, _ := out["input"].([]any)
	parts := input[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("内容块数 = %d, want 2", len(parts))
	}
	if parts[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("文本块应为 input_text，实际 %+v", parts[0])
	}
	if parts[1].(map[string]any)["type"] != "input_image" {
		t.Errorf("图片块应为 input_image，实际 %+v", parts[1])
	}
	if parts[1].(map[string]any)["image_url"] != "https://x/y.png" {
		t.Errorf("图片 URL 未透传: %+v", parts[1])
	}
}

// 非流式 Responses 响应折叠成 Chat 响应：文本、思考、工具调用、usage。
func TestResponsesToOpenAIChat(t *testing.T) {
	raw := []byte(`{
		"id": "resp_1",
		"status": "completed",
		"output": [
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking..."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_9","name":"lookup","arguments":"{\"q\":\"x\"}"}
		],
		"usage": {"input_tokens": 11, "output_tokens": 7}
	}`)
	out, err := ResponsesToOpenAIChat(raw, "gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != "resp_1" {
		t.Errorf("id = %v", body["id"])
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Errorf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "thinking..." {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量 = %d", len(calls))
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_9" {
		t.Errorf("工具调用 id = %v", call["id"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("有工具调用时 finish_reason 应为 tool_calls，实际 %v", choice["finish_reason"])
	}
	usage := body["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 11 || usage["completion_tokens"].(float64) != 7 {
		t.Errorf("usage 异常: %+v", usage)
	}
	if usage["total_tokens"].(float64) != 18 {
		t.Errorf("total_tokens 异常: %+v", usage)
	}
}

// 流式翻译：文本增量、思考增量、工具调用参数与末帧。
func TestTranslateResponsesStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		``,
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"plan"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"he"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"llo"}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_2","name":"lookup"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":3,"output_tokens":4}}}`,
		``,
	}, "\n")

	var out strings.Builder
	if err := TranslateResponsesStream(strings.NewReader(stream), &out, "gpt-5.6-luna"); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	for _, want := range []string{
		`"reasoning_content":"plan"`,
		`"content":"he"`,
		`"content":"llo"`,
		`"name":"lookup"`,
		`"arguments":"{\"q\":"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":3`,
		`"completion_tokens":4`,
		`data: [DONE]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("翻译结果缺少 %q\n完整输出:\n%s", want, got)
		}
	}
	// 每帧都必须是合法的 JSON（前端按 SSE data 逐行解析）。
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			t.Errorf("SSE 帧不是合法 JSON: %q (%v)", payload, err)
		}
		if probe["object"] != "chat.completion.chunk" {
			t.Errorf("帧 object 异常: %+v", probe)
		}
	}
}

// 流首报错要翻译成 OpenAI 错误帧并收尾，不能让客户端挂死。
func TestTranslateResponsesStreamUpstreamError(t *testing.T) {
	stream := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"boom\"}}}\n\n"
	var out strings.Builder
	if err := TranslateResponsesStream(strings.NewReader(stream), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "boom") {
		t.Errorf("错误消息未透出: %s", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Errorf("错误后应补 [DONE]: %s", got)
	}
}

func TestAppendVersionedPath(t *testing.T) {
	cases := []struct{ base, sub, want string }{
		{"https://api.x.ai/v1", "/models", "https://api.x.ai/v1/models"},
		{"https://api.anthropic.com/v1", "/messages", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com", "/messages", "https://api.anthropic.com/v1/messages"},
		{"https://api.x.ai/v1/", "/chat/completions", "https://api.x.ai/v1/chat/completions"},
	}
	for _, tc := range cases {
		if got := appendVersionedPath(tc.base, tc.sub); got != tc.want {
			t.Errorf("appendVersionedPath(%q, %q) = %q, want %q", tc.base, tc.sub, got, tc.want)
		}
	}
}

func TestResponsesEndpoint(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://chatgpt.com/backend-api/codex", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex/", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/codex/responses"},
	}
	for _, tc := range cases {
		if got := responsesEndpoint(tc.base); got != tc.want {
			t.Errorf("responsesEndpoint(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestCodexModelsEndpoint(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://chatgpt.com/backend-api/codex", "https://chatgpt.com/backend-api/codex/models"},
		{"https://chatgpt.com/backend-api/codex/models", "https://chatgpt.com/backend-api/codex/models"},
	}
	for _, tc := range cases {
		if got := codexModelsEndpoint(tc.base); got != tc.want {
			t.Errorf("codexModelsEndpoint(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// 只有 reasoning 增量、文本只在 output_text.done 里给出时，文本不能丢。
// （曾用 sentRole 判重，reasoning 已经把 sentRole 置真，导致整段回复丢失。）
func TestTranslateResponsesStreamTextOnlyInDone(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		``,
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","text":"final answer"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"r3"}}`,
		``,
	}, "\n")

	var out strings.Builder
	if err := TranslateResponsesStream(strings.NewReader(stream), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"reasoning_content":"thinking"`) {
		t.Errorf("思考内容缺失:\n%s", got)
	}
	if !strings.Contains(got, `"content":"final answer"`) {
		t.Errorf("只在 done 事件里给出的文本被丢弃了:\n%s", got)
	}
}

// incomplete 且已有工具调用时，finish_reason 应为 tool_calls（客户端据此
// 决定是否继续跑工具循环），而不是 length。
func TestTranslateResponsesStreamIncompleteKeepsToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"f"}}`,
		``,
		`event: response.incomplete`,
		`data: {"type":"response.incomplete","response":{"id":"r4"}}`,
		``,
	}, "\n")

	var out strings.Builder
	if err := TranslateResponsesStream(strings.NewReader(stream), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("有工具调用时应收尾为 tool_calls:\n%s", got)
	}
}

// Codex 订阅线路拒绝 max_output_tokens，标准 Responses（xAI）则要保留它——
// 一律丢弃会让 xAI 用户设的输出上限静默失效。
func TestMaxOutputTokensPerResponsesEndpoint(t *testing.T) {
	payload := map[string]any{
		"model":                 "m",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_completion_tokens": 512,
	}

	codex := OpenAIChatToResponses(payload, "m", true)
	if _, ok := codex["max_output_tokens"]; ok {
		t.Error("Codex 线路不应带 max_output_tokens")
	}

	standard := OpenAIChatToResponses(payload, "m", false)
	if got := intOf(standard["max_output_tokens"]); got != 512 {
		t.Errorf("标准 Responses 应保留 max_output_tokens=512，实际 %v", standard["max_output_tokens"])
	}

	// max_tokens 也应被识别（max_completion_tokens 缺失时）。
	legacy := map[string]any{
		"model":      "m",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 256,
	}
	if got := intOf(OpenAIChatToResponses(legacy, "m", false)["max_output_tokens"]); got != 256 {
		t.Errorf("max_tokens 应折算成 max_output_tokens=256，实际 %d", got)
	}
}

// 采样参数在 Responses 里同名同义，必须透传；reasoning_effort 要改形状。
// （曾把它们列进保留字段一并丢弃，用户设的 temperature 全部静默失效。）
func TestSamplingParamsPassThrough(t *testing.T) {
	payload := map[string]any{
		"model":             "m",
		"messages":          []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature":       0.3,
		"top_p":             0.9,
		"stop":              []any{"END"},
		"seed":              42,
		"presence_penalty":  0.1,
		"frequency_penalty": 0.2,
		"reasoning_effort":  "high",
	}
	out := OpenAIChatToResponses(payload, "m", false)

	for _, key := range []string{"temperature", "top_p", "stop", "seed", "presence_penalty", "frequency_penalty"} {
		if _, ok := out[key]; !ok {
			t.Errorf("采样参数 %q 应透传", key)
		}
	}
	if _, leaked := out["reasoning_effort"]; leaked {
		t.Error("reasoning_effort 不应原样透传（Responses 形状是 reasoning.effort）")
	}
	reasoning, ok := out["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning 字段缺失或形状不对: %+v", out["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reasoning["effort"])
	}
}
