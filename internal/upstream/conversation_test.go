package upstream

// 会话头补全的回归测试：国际版 workbuddy.ai 上 hy3/deepseek 对话必须携带
// 四个会话标识（缺 X-Conversation-ID 时上游以 code=11128 拒绝），其它模型
// 与国内部署不受影响，调用方自带的标识原样保留。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znsoftm/RapidProxy/internal/config"
)

// chatStub 起一个记录请求头的上游桩。返回的 getter 读到的是「最近一次请求」
// 的头（handler 运行在请求到达时，不能在起桩时就把 map 快照出去）。
func chatStub(t *testing.T) (func() http.Header, *httptest.Server) {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return func() http.Header { return got }, srv
}

func chatBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	return body
}

// globalClient 构造指向测试桩的国际版客户端（桩地址不含 workbuddy.ai，
// 按 Edition 声明国际版，与 trae/qoder 的既有测试同款）。
func globalClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	profile := config.WorkBuddyPreset()
	profile.BaseURL = baseURL
	profile.Edition = config.EditionGlobal
	c, err := NewClient(profile, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestChatWorkBuddyAddsConversationHeadersForDeepSeek(t *testing.T) {
	headers, srv := chatStub(t)
	c := globalClient(t, srv.URL)
	resp, err := c.Chat(context.Background(), &Credential{}, chatBody("deepseek-v4.1-flash"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()
	got := headers()
	for _, name := range conversationHeaderNames {
		if strings.TrimSpace(got.Get(name)) == "" {
			t.Errorf("deepseek 请求缺少 %s", name)
		}
	}
	if id := got.Get("X-Conversation-ID"); len(strings.Split(id, "-")) != 5 {
		t.Errorf("X-Conversation-ID 不是 UUID 形态: %q", id)
	}
}

func TestChatWorkBuddyAddsConversationHeadersForHy3(t *testing.T) {
	headers, srv := chatStub(t)
	c := globalClient(t, srv.URL)
	resp, err := c.Chat(context.Background(), &Credential{}, chatBody("hy3"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()
	for _, name := range conversationHeaderNames {
		if strings.TrimSpace(headers().Get(name)) == "" {
			t.Errorf("hy3 请求缺少 %s", name)
		}
	}
}

func TestChatWorkBuddySkipsConversationHeadersForOtherModels(t *testing.T) {
	for _, model := range []string{"gpt-5.4", "kimi-k3", "glm-5.3", "hy4-preview"} {
		headers, srv := chatStub(t)
		c := globalClient(t, srv.URL)
		resp, err := c.Chat(context.Background(), &Credential{}, chatBody(model))
		if err != nil {
			t.Fatalf("Chat(%s): %v", model, err)
		}
		resp.Body.Close()
		for _, name := range conversationHeaderNames {
			if headers().Get(name) != "" {
				t.Errorf("模型 %s 不应携带 %s", model, name)
			}
		}
	}
}

func TestChatCodeBuddyHostSkipsConversationHeaders(t *testing.T) {
	headers, srv := chatStub(t)
	profile := config.CodeBuddyPreset()
	profile.BaseURL = srv.URL
	c, err := NewClient(profile, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resp, err := c.Chat(context.Background(), &Credential{}, chatBody("deepseek-v4.1-flash"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp.Body.Close()
	for _, name := range conversationHeaderNames {
		if headers().Get(name) != "" {
			t.Errorf("国内部署不应携带 %s", name)
		}
	}
}

func TestApplyConversationHeadersKeepsCallerValues(t *testing.T) {
	h := http.Header{}
	h.Set("X-Conversation-ID", "caller-conv")
	h.Set("X-Conversation-Request-ID", "caller-req")
	h.Set("X-Conversation-Message-ID", "caller-msg")
	h.Set("X-Request-ID", "caller-xreq")
	applyConversationHeaders(h)
	want := map[string]string{
		"X-Conversation-ID":         "caller-conv",
		"X-Conversation-Request-ID": "caller-req",
		"X-Conversation-Message-ID": "caller-msg",
		"X-Request-ID":              "caller-xreq",
	}
	for name, value := range want {
		if h.Get(name) != value {
			t.Errorf("%s = %q, want %q（调用方自带值必须保留）", name, h.Get(name), value)
		}
	}
}

func TestModelNeedsConversationID(t *testing.T) {
	yes := []string{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", "hy3"}
	// hy4 系不在已验证的门禁内（aicoder 实证范围是 hy3* 与 deepseek*）。
	no := []string{"gpt-5.4", "kimi-k3", "glm-5.3", "gemini-3.1-pro", "hy4-preview", ""}
	for _, m := range yes {
		if !modelNeedsConversationID(m) {
			t.Errorf("%q 应触发会话头", m)
		}
	}
	for _, m := range no {
		if modelNeedsConversationID(m) {
			t.Errorf("%q 不应触发会话头", m)
		}
	}
}
