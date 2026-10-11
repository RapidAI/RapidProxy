package openai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareRequestBodyForcesStreamAndSanitizes(t *testing.T) {
	payload := []byte(`{
		"model": "hy3",
		"messages": [
			{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude."},
			{"role": "user", "content": [{"type": "text", "text": "Main branch (you will usually use this for PRs)"}]}
		]
	}`)

	body, obj, err := PrepareRequestBody(payload, TransformOptions{Sanitize: true, ForceMaxThinking: true})
	if err != nil {
		t.Fatalf("PrepareRequestBody 失败: %v", err)
	}
	if stream, _ := obj["stream"].(bool); !stream {
		t.Fatal("应强制 stream=true")
	}
	if eff, _ := obj["reasoning_effort"].(string); eff != "high" {
		t.Fatalf("hy3 应强制 reasoning_effort=high，实际 %q", eff)
	}

	text := string(body)
	if strings.Contains(text, "official CLI for Claude.") {
		t.Error("system 模板句未被改写")
	}
	if !strings.Contains(text, "official CLI tool for Claude.") {
		t.Error("未生成预期的改写结果")
	}
	if strings.Contains(text, "Main branch (you will usually use this for PRs)") {
		t.Error("多模态 part 中的模板句未被改写")
	}
	if !strings.Contains(text, "Default branch (you will usually use this for PRs)") {
		t.Error("未生成预期的分支名改写")
	}
}

func TestPrepareRequestBodyKeepsOtherModelsUntouched(t *testing.T) {
	body, obj, err := PrepareRequestBody([]byte(`{"model":"gpt-5.4","messages":[]}`), TransformOptions{ForceMaxThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["reasoning_effort"]; ok {
		t.Error("非 hy3 模型不应被写入 reasoning_effort")
	}
	if string(body) == "" {
		t.Error("请求体不应为空")
	}
}

func TestCleanChunkStripsEmptyFieldsButKeepsToolCalls(t *testing.T) {
	in := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"","function_call":null,"tool_calls":[]},"finish_reason":""}]}`
	out := CleanChunk(in)

	var chunk map[string]any
	if err := json.Unmarshal([]byte(out), &chunk); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, ok := delta["function_call"]; ok {
		t.Error("空的 function_call 应被清理")
	}
	if _, ok := delta["content"]; ok {
		t.Error("空的 content 应被清理")
	}
	// 空数组同样是噪音：有些客户端遇到 "tool_calls":[] 会去解析第 0 项而报错。
	if _, ok := delta["tool_calls"]; ok {
		t.Error("空的 tool_calls 数组应被清理")
	}
	if _, ok := delta["role"]; !ok {
		t.Error("有内容的 role 不应被清理")
	}

	// 非空的工具调用块必须原样保留（arguments 的空字符串是增量拼接的一部分）
	withTools := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":""}}]}}]}`
	kept := CleanChunk(withTools)
	if !strings.Contains(kept, `"id":"call_1"`) || !strings.Contains(kept, `"arguments":""`) {
		t.Errorf("工具调用块被破坏: %s", kept)
	}

	// 同一块里既有工具调用又有空字段时：工具调用保留，空字段照常清理
	mixed := `{"choices":[{"delta":{"content":"","tool_calls":[{"index":0,"function":{"arguments":""}}]}}]}`
	mixedOut := CleanChunk(mixed)
	if strings.Contains(mixedOut, `"content"`) {
		t.Errorf("空 content 应被清理: %s", mixedOut)
	}
	if !strings.Contains(mixedOut, `"tool_calls"`) || !strings.Contains(mixedOut, `"arguments":""`) {
		t.Errorf("工具调用块不应被清理: %s", mixedOut)
	}
}

func TestAggregateFoldsStreamIntoCompletion(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"思考中"},"finish_reason":""}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"你"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"好"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"search","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := Aggregate(strings.NewReader(stream), "fallback")
	if err != nil {
		t.Fatalf("Aggregate 失败: %v", err)
	}
	var completion map[string]any
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v", err)
	}
	if got := completion["object"]; got != "chat.completion" {
		t.Errorf("object = %v", got)
	}
	if got := completion["model"]; got != "gpt-5.4" {
		t.Errorf("model = %v", got)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if got := choice["finish_reason"]; got != "tool_calls" {
		t.Errorf("finish_reason = %v", got)
	}
	message := choice["message"].(map[string]any)
	if got := message["content"]; got != "你好" {
		t.Errorf("content = %v", got)
	}
	if got := message["reasoning_content"]; got != "思考中" {
		t.Errorf("reasoning_content = %v", got)
	}
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量 = %d", len(calls))
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("工具名 = %v", fn["name"])
	}
	if fn["arguments"] != `{"q":"go"}` {
		t.Errorf("工具参数未正确拼接: %v", fn["arguments"])
	}
}

func TestAggregateFallsBackModel(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	out, err := Aggregate(strings.NewReader(stream), "my-model")
	if err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	_ = json.Unmarshal(out, &completion)
	if completion["model"] != "my-model" {
		t.Errorf("应回退到请求的模型名，实际 %v", completion["model"])
	}
}

// 客户端不带 system 消息时应自动补齐首条 system（上游 code=11128）。
func TestPrepareRequestBodyEnsuresSystemPrompt(t *testing.T) {
	base := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}]}`

	out, _, err := PrepareRequestBody([]byte(base), TransformOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("首条消息应为 system，实际 %v", first["role"])
	}
	if len(msgs) != 2 {
		t.Fatalf("应保留原有 user 消息，实际 %d 条", len(msgs))
	}
	if role, _ := msgs[1].(map[string]any)["role"].(string); role != "user" {
		t.Fatalf("第二条应为 user，实际 %v", msgs[1])
	}
}

// 已有 system 开头时不应重复补齐，也不应打乱顺序。
func TestPrepareRequestBodyKeepsExistingSystemPrompt(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"system","content":"You are Claude Code."},{"role":"user","content":"hi"}]}`
	out, _, err := PrepareRequestBody([]byte(base), TransformOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息数量不应变化，实际 %d", len(msgs))
	}
	if role, _ := msgs[0].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("首条应保持 system，实际 %v", msgs[0])
	}
}

// 上游可能用 message（整段）而不是 delta（增量）下发内容。
// 只认 delta 会把 message 形态的内容整个丢掉，客户端收到空回复。
func TestAggregateAcceptsMessageShape(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"前段\"}}]}\n\n" +
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"后段\"},\"finish_reason\":\"stop\"}]}\n\n"
	out, err := Aggregate(strings.NewReader(stream), "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Choices[0].Message.Content; got != "前段后段" {
		t.Fatalf("message 形态的内容应被保留，实际 %q", got)
	}
}

// 上游会以 200 + SSE 的形式回报失败（状态码正常，流里夹带 error 事件）。
// 静默跳过的话客户端拿到 200 + 空回复，以为成功却什么都没发生。
func TestAggregateReportsStreamError(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"开头\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	out, err := Aggregate(strings.NewReader(stream), "m")
	if err == nil {
		t.Fatalf("流中 error 事件应导致失败，实际返回了 %s", out)
	}
	var serr *StreamError
	if !errors.As(err, &serr) {
		t.Fatalf("应返回 *StreamError，实际 %T: %v", err, err)
	}
	if serr.Error() != "upstream exploded" {
		t.Errorf("错误文案不对: %q", serr.Error())
	}
	if out != nil {
		t.Errorf("出错时不应同时返回响应体: %s", out)
	}
}

// 客户端传 n>1 时每个 chunk 带多个 choices，必须按 index 分开累积，
// 否则不同 choice 的内容会拼进同一条消息。
func TestAggregateSeparatesChoicesByIndex(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"}}," +
		"{\"index\":1,\"delta\":{\"content\":\"B\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"B2\"},\"finish_reason\":\"stop\"}]}\n\n"
	out, err := Aggregate(strings.NewReader(stream), "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 2 {
		t.Fatalf("应返回 2 个 choice，实际 %d", len(resp.Choices))
	}
	// 必须按 index 升序，且内容不串。
	if resp.Choices[0].Index != 0 || resp.Choices[0].Message.Content != "A" {
		t.Errorf("choice 0 应为 \"A\"，实际 index=%d content=%q",
			resp.Choices[0].Index, resp.Choices[0].Message.Content)
	}
	if resp.Choices[1].Index != 1 || resp.Choices[1].Message.Content != "BB2" {
		t.Errorf("choice 1 应为 \"BB2\"，实际 index=%d content=%q",
			resp.Choices[1].Index, resp.Choices[1].Message.Content)
	}
}

// 空的 SSE 流也要给出结构完整的响应，客户端解析不至于炸。
func TestAggregateEmptyStreamStillValid(t *testing.T) {
	out, err := Aggregate(strings.NewReader("data: [DONE]\n\n"), "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("空流应返回合法 JSON: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("空流也应返回一个 choice，实际 %d", len(choices))
	}
}

// error 事件的各种形态：带了可读文案才算错误，null / 空对象 / 只有 code
// 这类没有文案的不能当成失败（某些上游的正常 chunk 会带这些）。
func TestExtractErrorText(t *testing.T) {
	cases := []struct {
		in    any
		want  string
		isErr bool
	}{
		{"plain failure", "plain failure", true},
		{map[string]any{"message": "额度已用尽"}, "额度已用尽", true},
		{map[string]any{"msg": "登录失效"}, "登录失效", true},
		{map[string]any{"code": 123}, "", false},
		{map[string]any{}, "", false},
		{nil, "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		msg, ok := extractErrorText(tc.in)
		if ok != tc.isErr || msg != tc.want {
			t.Errorf("输入 %v => (%q, %v)，期望 (%q, %v)", tc.in, msg, ok, tc.want, tc.isErr)
		}
	}
}

// 流里夹带 "error": null 或空对象时，Aggregate 不能把它当失败——
// 否则一次成功的请求会被凭空打成错误返回。
func TestAggregateIgnoresNullErrorField(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		``,
		`data: {"error":null}`,
		``,
		`data: {"error":{}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	out, err := Aggregate(strings.NewReader(stream), "m")
	if err != nil {
		t.Fatalf("带 null/空 error 字段的流不应报错，实际: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("应返回合法 JSON: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("应保留一条正常 choice，实际 %d", len(choices))
	}
}

// truncateText 按字符截断，中文不能被切出非法 UTF-8。
func TestTruncateTextKeepsUTF8(t *testing.T) {
	got := truncateText("上游额度已用尽，请充值", 6)
	if got != "上游额度已用" {
		t.Errorf("截断结果不对: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("截断产生了非法 UTF-8: %q", got)
	}
	if truncateText("abc", 10) != "abc" || truncateText("abc", 0) != "" {
		t.Error("边界处理不对")
	}
}
