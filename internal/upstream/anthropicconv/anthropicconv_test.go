package anthropicconv

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestOpenAIToAnthropicExtractsSystemAndMessages(t *testing.T) {
	body := map[string]any{
		"model":      "ignored",
		"max_tokens": float64(1024),
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
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
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
		},
	}
	req := RequestOpenAIToAnthropic(body, "claude-x")

	if req["model"] != "claude-x" {
		t.Fatalf("model = %v", req["model"])
	}
	if req["system"] != "be brief" {
		t.Fatalf("system = %v", req["system"])
	}
	if req["stream"] != true {
		t.Fatalf("stream = %v", req["stream"])
	}
	if req["max_tokens"] != float64(1024) {
		t.Fatalf("max_tokens = %v", req["max_tokens"])
	}
	msgs, _ := req["messages"].([]any)
	// system 被提取，剩下 user / assistant / tool(user) 三条。
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d (%v)", len(msgs), msgs)
	}
	asst, _ := msgs[1].(map[string]any)
	content, _ := asst["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("assistant blocks = %v", content)
	}
	toolUse, _ := content[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["name"] != "lookup" {
		t.Fatalf("tool_use = %v", toolUse)
	}
	if _, ok := toolUse["input"].(map[string]any); !ok {
		t.Fatalf("tool input should be object, got %T", toolUse["input"])
	}
	last, _ := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool role mapped to %v", last["role"])
	}
	blocks, _ := last["content"].([]any)
	tr, _ := blocks[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result = %v", tr)
	}
}

func TestAnthropicToOpenAIResponseTextAndTool(t *testing.T) {
	resp := map[string]any{
		"id":          "msg_1",
		"stop_reason": "tool_use",
		"content": []any{
			map[string]any{"type": "text", "text": "let me check"},
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "lookup", "input": map[string]any{"q": "x"}},
		},
		"usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)},
	}
	out := AnthropicToOpenAIResponse(resp, "claude-x")
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", choices)
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "let me check" {
		t.Fatalf("content = %v", msg["content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", tcs)
	}
	u, _ := out["usage"].(map[string]any)
	if u["prompt_tokens"] != 10 || u["completion_tokens"] != 5 || u["total_tokens"] != 15 {
		t.Fatalf("usage = %v", u)
	}
}

func TestTranslateStreamTextAndTool(t *testing.T) {
	upstream := strings.Join([]string{
		"event: message_start\n",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`, "\n\n",
		"event: content_block_start\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, "\n\n",
		"event: content_block_delta\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`, "\n\n",
		"event: content_block_delta\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`, "\n\n",
		"event: content_block_stop\n",
		`data: {"type":"content_block_stop","index":0}`, "\n\n",
		"event: content_block_start\n",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_9","name":"lookup"}}`, "\n\n",
		"event: content_block_delta\n",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`, "\n\n",
		"event: content_block_stop\n",
		`data: {"type":"content_block_stop","index":1}`, "\n\n",
		"event: message_delta\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`, "\n\n",
		"event: message_stop\n",
		`data: {"type":"message_stop"}`, "\n\n",
	}, "")

	var out bytes.Buffer
	if err := TranslateStream(strings.NewReader(upstream), &out, "claude-x"); err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		`"role":"assistant"`,
		`"content":"Hel"`,
		`"content":"lo"`,
		`"tool_calls"`,
		`"name":"lookup"`,
		`"arguments":"{\"q\":\"x\"}"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":7`,
		`"completion_tokens":3`,
		`data: [DONE]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in translated stream:\n%s", want, got)
		}
	}
}

func TestTranslateStreamErrorEvent(t *testing.T) {
	upstream := strings.Join([]string{
		"event: error\n",
		`data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, "\n\n",
	}, "")
	var out bytes.Buffer
	if err := TranslateStream(strings.NewReader(upstream), &out, "claude-x"); err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	var frame struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	line := strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])
	line = strings.TrimPrefix(line, "data: ")
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("error frame not JSON: %v (%s)", err, line)
	}
	if frame.Error.Message != "busy" {
		t.Fatalf("error message = %q", frame.Error.Message)
	}
}

func TestRequestConvertsToolChoiceStopAndSampling(t *testing.T) {
	base := func(tc any, stop any) map[string]any {
		return map[string]any{
			"model":       "m",
			"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "f", "parameters": map[string]any{"type": "object"}}}},
			"tool_choice": tc,
			"temperature": float64(0.4),
			"top_p":       float64(0.9),
			"stop":        stop,
		}
	}

	req := RequestOpenAIToAnthropic(base("required", []any{"END"}), "m")
	if tc, _ := req["tool_choice"].(map[string]any); tc["type"] != "any" {
		t.Errorf(`tool_choice "required" -> %v, want {"type":"any"}`, req["tool_choice"])
	}
	if req["temperature"] != float64(0.4) || req["top_p"] != float64(0.9) {
		t.Errorf("sampling params dropped: %v", req)
	}
	if seq, _ := req["stop_sequences"].([]any); len(seq) != 1 || seq[0] != "END" {
		t.Errorf("stop -> %v", req["stop_sequences"])
	}

	req = RequestOpenAIToAnthropic(base(map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}, "X"), "m")
	if tc, _ := req["tool_choice"].(map[string]any); tc["type"] != "tool" || tc["name"] != "lookup" {
		t.Errorf("function tool_choice -> %v", req["tool_choice"])
	}

	req = RequestOpenAIToAnthropic(base("none", nil), "m")
	if _, ok := req["tool_choice"]; ok {
		t.Errorf(`tool_choice "none" should be omitted, got %v`, req["tool_choice"])
	}
}

func TestRequestMergesConsecutiveSameRole(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "one"},
			map[string]any{"role": "user", "content": "two"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "assistant", "content": "more"},
			map[string]any{"role": "tool", "tool_call_id": "a", "content": "ra"},
			map[string]any{"role": "tool", "tool_call_id": "b", "content": "rb"},
		},
	}
	req := RequestOpenAIToAnthropic(body, "m")
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 alternating messages, got %d: %v", len(msgs), msgs)
	}
	first := mapOf(msgs[0])
	blocks, _ := first["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("merged user blocks = %v", blocks)
	}
	last := mapOf(msgs[2])
	lastBlocks, _ := last["content"].([]any)
	if last["role"] != "user" || len(lastBlocks) != 2 {
		t.Fatalf("merged tool results = %v", last)
	}
}

func TestImageBlockDataURLAndHTTP(t *testing.T) {
	block := imageBlock("data:image/png;base64,QUJD")
	src, _ := block["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "QUJD" {
		t.Fatalf("data URL image = %v", block)
	}
	block = imageBlock("https://example.com/x.png")
	src, _ = block["source"].(map[string]any)
	if src["type"] != "url" || src["url"] != "https://example.com/x.png" {
		t.Fatalf("http image = %v", block)
	}
}

func TestClampThinkingBudget(t *testing.T) {
	body := map[string]any{
		"model":      "m",
		"max_tokens": float64(2000),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(5000)},
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
	req := RequestOpenAIToAnthropic(body, "m")
	think, _ := req["thinking"].(map[string]any)
	budget, _ := think["budget_tokens"].(float64)
	maxTokens, _ := req["max_tokens"].(float64)
	if budget >= maxTokens {
		t.Fatalf("budget %.0f must be < max_tokens %.0f", budget, maxTokens)
	}
	if budget < 1024 {
		t.Fatalf("budget %.0f below Anthropic minimum", budget)
	}
}

func TestThinkingPassThroughNonEnabled(t *testing.T) {
	body := map[string]any{
		"model":    "m",
		"thinking": map[string]any{"type": "disabled"},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	req := RequestOpenAIToAnthropic(body, "m")
	if _, ok := req["thinking"]; !ok {
		t.Fatal("non-enabled thinking block should pass through unchanged")
	}
}
