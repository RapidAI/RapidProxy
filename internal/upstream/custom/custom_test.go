package custom

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppendVersionedPath(t *testing.T) {
	cases := []struct {
		base, sub, want string
	}{
		{"https://api.x.ai/v1", "/chat/completions", "https://api.x.ai/v1/chat/completions"},
		{"https://api.openai.com/v1/", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.deepseek.com", "/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "/chat/completions", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"https://open.bigmodel.cn/api/anthropic/v1", "/messages", "https://open.bigmodel.cn/api/anthropic/v1/messages"},
		{"https://api.anthropic.com", "/messages", "https://api.anthropic.com/v1/messages"},
	}
	for _, tc := range cases {
		if got := appendVersionedPath(tc.base, tc.sub); got != tc.want {
			t.Errorf("appendVersionedPath(%q, %q) = %q, want %q", tc.base, tc.sub, got, tc.want)
		}
	}
}

func TestNormalizeAPIKey(t *testing.T) {
	cases := map[string]string{
		"  sk-abc  ":          "sk-abc",
		"Bearer sk-abc":       "sk-abc",
		"`sk-abc`":            "sk-abc",
		`{"key":"sk-abc"}`:    "sk-abc",
		`{"apiKey":"sk-abc"}`: "sk-abc",
	}
	for in, want := range cases {
		got, err := NormalizeAPIKey(in)
		if err != nil {
			t.Fatalf("NormalizeAPIKey(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := NormalizeAPIKey("   "); err == nil {
		t.Error("empty key should be invalid")
	}
}

func TestParseModelsEnvelopeAndArray(t *testing.T) {
	env := []byte(`{"object":"list","data":[{"id":"m1","name":"Model 1"},{"id":"m2"}]}`)
	models, err := parseModels(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "m1" || models[0].Name != "Model 1" {
		t.Fatalf("envelope models = %+v", models)
	}
	arr := []byte(`[{"id":"a1","name":"A"},{"id":"a2","name":"B"}]`)
	models, err = parseModels(arr)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[1].ID != "a2" || models[1].Name != "B" {
		t.Fatalf("array models = %+v", models)
	}
}

func TestChatAnthropicTranslatesStream(t *testing.T) {
	var gotPath, gotKey, gotUA, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotUA = r.Header.Get("User-Agent")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			"event: message_start\n",
			`data: {"type":"message_start","message":{"usage":{"input_tokens":4}}}`, "\n\n",
			"event: content_block_start\n",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`, "\n\n",
			"event: content_block_delta\n",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, "\n\n",
			"event: message_delta\n",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`, "\n\n",
			"event: message_stop\n",
			`data: {"type":"message_stop"}`, "\n\n",
		}, ""))
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"model":      "glm-5.3",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	})
	resp, err := Chat(context.Background(), srv.Client(), srv.URL, "sk-k", ProtocolAnthropic, "opencode", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotKey != "sk-k" || gotUA != "opencode" || gotVersion != "2023-06-01" {
		t.Errorf("headers key=%q ua=%q ver=%q", gotKey, gotUA, gotVersion)
	}
	if gotBody["model"] != "glm-5.3" || gotBody["stream"] != true {
		t.Errorf("anthropic body = %v", gotBody)
	}
	out, _ := io.ReadAll(resp.Body)
	got := string(out)
	for _, want := range []string{`"role":"assistant"`, `"content":"hi"`, `"prompt_tokens":4`, `"completion_tokens":2`, "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in translated stream:\n%s", want, got)
		}
	}
}

func TestChatAnthropicErrorKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"model": "glm", "messages": []any{}})
	resp, err := Chat(context.Background(), srv.Client(), srv.URL, "k", ProtocolAnthropic, "", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("error body not JSON: %v (%s)", err, raw)
	}
	if payload.Error.Message != "slow down" {
		t.Fatalf("error message = %q", payload.Error.Message)
	}
}

func TestChatOpenAIPassthrough(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"model": "grok-4", "stream": true})
	resp, err := Chat(context.Background(), srv.Client(), srv.URL+"/v1", "sk", ProtocolOpenAI, "", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"content":"x"`) {
		t.Errorf("passthrough body = %s", out)
	}
}

func TestFoldAnthropicResponseErrorEnvelope(t *testing.T) {
	body := `{"type":"error","error":{"type":"overloaded_error","message":"busy now"}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	out, err := foldAnthropicResponse(resp, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if out.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", out.StatusCode)
	}
	raw, _ := io.ReadAll(out.Body)
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, raw)
	}
	if payload.Error.Message != "busy now" {
		t.Fatalf("message = %q", payload.Error.Message)
	}
}
