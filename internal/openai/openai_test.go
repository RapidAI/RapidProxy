package openai

import (
	"encoding/json"
	"strings"
	"testing"
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
