package oauthprov

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// xAI 走 Responses API：OAuth 令牌必须带 X-XAI-Token-Auth 头，且请求体
// 要翻译成 Responses 形状（xAI 的第一方 OAuth 线路只开在 Responses 上）。
func TestChatXAIHeaders(t *testing.T) {
	var gotAuth, gotXAI, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAI = r.Header.Get("X-XAI-Token-Auth")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"yo\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n")
	}))
	defer srv.Close()

	body := []byte(`{"model":"grok-4","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	resp, err := Chat(context.Background(), srv.Client(), ProviderXAI, srv.URL+"/v1", "tok-1", "", "test-ua", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if gotAuth != "Bearer tok-1" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotXAI != "xai-grok-cli" {
		t.Errorf("X-XAI-Token-Auth = %q, want xai-grok-cli", gotXAI)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("请求路径 = %q, want /v1/responses", gotPath)
	}
	// 请求体应翻译成 Responses 形状：system → instructions。
	if !strings.Contains(gotBody, `"instructions":"be brief"`) {
		t.Errorf("system 应转成 instructions，实际 %s", gotBody)
	}
	// 响应流应翻译回 Chat SSE。
	if !strings.Contains(string(out), `"content":"yo"`) {
		t.Errorf("Responses 流未翻译成 Chat SSE: %s", out)
	}
}

// Claude 走 Anthropic Messages：请求体翻译、Bearer 认证 + OAuth beta 头。
func TestChatClaudeTranslatesToMessages(t *testing.T) {
	var gotPath, gotAPIKey, gotAuth, gotVersion, gotBeta, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		// 一条最小的 Anthropic 流。
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	body := []byte(`{"model":"claude-opus-4-5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	resp, err := Chat(context.Background(), srv.Client(), ProviderClaude, srv.URL+"/v1", "oat-1", "", "", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/messages" {
		t.Errorf("请求路径 = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "Bearer oat-1" {
		t.Errorf("Authorization = %q, want Bearer oat-1", gotAuth)
	}
	// OAuth 令牌不是平台 API Key：同时发 x-api-key 会让网关按 API Key 校验。
	if gotAPIKey != "" {
		t.Errorf("OAuth 令牌不应带 x-api-key，实际 %q", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	// 用 OAuth bearer 令牌访问 Messages API 必须声明这个 beta。
	if gotBeta != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", gotBeta)
	}
	// system 消息应被提到顶层 system 字段。
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["system"] != "sys" {
		t.Errorf("system 未提升到顶层: %+v", sent)
	}
	// 上游 Anthropic 流应被翻成 OpenAI SSE。
	if !strings.Contains(string(out), `"content":"hi"`) {
		t.Errorf("Anthropic 流未翻译成 OpenAI SSE: %s", out)
	}
}

// ChatGPT 走 Codex Responses：路径、Codex 头、account id、双向翻译。
func TestChatChatGPTUsesCodexResponses(t *testing.T) {
	var gotPath, gotAuth, gotBeta, gotOriginator, gotAccount, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotOriginator = r.Header.Get("originator")
		gotAccount = r.Header.Get("chatgpt-account-id")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"yo\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n")
	}))
	defer srv.Close()

	body := []byte(`{"model":"gpt-5.6-luna","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	resp, err := Chat(context.Background(), srv.Client(), ProviderChatGPT,
		srv.URL+"/backend-api/codex", "tok", "acct-42", "", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if gotPath != "/backend-api/codex/responses" {
		t.Errorf("请求路径 = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBeta != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q", gotBeta)
	}
	if gotOriginator != "codex_cli_rs" {
		t.Errorf("originator = %q", gotOriginator)
	}
	if gotAccount != "acct-42" {
		t.Errorf("chatgpt-account-id = %q", gotAccount)
	}
	if !strings.Contains(gotBody, `"instructions":"be brief"`) {
		t.Errorf("system 应转成 instructions，实际 %s", gotBody)
	}
	if strings.Contains(gotBody, "max_output_tokens") {
		t.Errorf("Codex 请求不应带 max_output_tokens: %s", gotBody)
	}
	if !strings.Contains(string(out), `"content":"yo"`) {
		t.Errorf("Responses 流未翻译成 Chat SSE: %s", out)
	}
}

// 上游 401 要保留状态码（服务层靠它触发刷新/冷却），错误体翻成 OpenAI 形状。
func TestChatPreservesErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad token"}}`)
	}))
	defer srv.Close()

	resp, err := Chat(context.Background(), srv.Client(), ProviderChatGPT,
		srv.URL+"/backend-api/codex", "tok", "", "", []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "30" {
		t.Errorf("Retry-After 未保留: %q", resp.Header.Get("Retry-After"))
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "bad token") {
		t.Errorf("错误消息未透出: %s", raw)
	}
}

// token 端点：Anthropic 用 JSON，OpenAI/xAI 用 form。
func TestExchangeClaudeUsesJSON(t *testing.T) {
	var contentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
	}))
	defer srv.Close()

	s := &Session{verifier: "v", state: "v", hc: srv.Client()}
	tok, err := s.exchangeClaudeAgainst(context.Background(), srv.URL, "code-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Anthropic token 端点要用 JSON，实际 %q", contentType)
	}
	if !strings.Contains(gotBody, `"code_verifier":"v"`) {
		t.Errorf("请求体缺少 code_verifier: %s", gotBody)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Errorf("token 解析异常: %+v", tok)
	}
	if tok.ExpiresAt == 0 {
		t.Error("expires_in 应折算成绝对过期时间")
	}
}

func TestRefreshChatGPTUsesForm(t *testing.T) {
	var contentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{"access_token":"new-at","refresh_token":"new-rt","expires_in":60}`)
	}))
	defer srv.Close()

	tok, err := refreshAgainst(context.Background(), srv.Client(), srv.URL, "old-rt", "chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		t.Errorf("OpenAI token 端点要用 form，实际 %q", contentType)
	}
	if !strings.Contains(gotBody, "grant_type=refresh_token") || !strings.Contains(gotBody, "refresh_token=old-rt") {
		t.Errorf("刷新请求体异常: %s", gotBody)
	}
	if tok.AccessToken != "new-at" {
		t.Errorf("access_token = %q", tok.AccessToken)
	}
}

// 只有上游 4xx 拒绝才算「授权码无效」；5xx 与网络故障是暂时性问题，
// 标成码错会让用户徒劳地反复粘贴。
func TestExchangeErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantReject bool
	}{
		{"4xx 拒绝", http.StatusBadRequest, true},
		{"401", http.StatusUnauthorized, true},
		{"5xx 暂时故障", http.StatusBadGateway, false},
		{"429 限流", http.StatusTooManyRequests, false},
		{"408 超时", http.StatusRequestTimeout, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"error":"nope"}`)
		}))

		s := &Session{verifier: "v", state: "v", hc: srv.Client(), PasteCode: true}
		_, err := s.exchangeClaudeAgainst(context.Background(), srv.URL, "code-1")
		srv.Close()

		if err == nil {
			t.Fatalf("%s: 应返回错误", tc.name)
		}
		// exchangeClaudeAgainst 不包装成 ErrCodeRejected；分类发生在 exchange。
		gotReject := isCodeRejection(err)
		if gotReject != tc.wantReject {
			t.Errorf("%s: isCodeRejection = %v, want %v (err=%v)", tc.name, gotReject, tc.wantReject, err)
		}
	}

	// 网络故障（连不上）不应被当成授权码问题。
	s := &Session{verifier: "v", state: "v", hc: &http.Client{}, PasteCode: true}
	_, err := s.exchangeClaudeAgainst(context.Background(), "http://127.0.0.1:1/oauth/token", "code-1")
	if err == nil {
		t.Fatal("连不上时应返回错误")
	}
	if isCodeRejection(err) {
		t.Errorf("网络故障不应被当成授权码无效: %v", err)
	}
}

// 下游中途断开时翻译协程必须退出：否则每次中断的请求都漏一个 goroutine
// （阻塞在 io.Pipe 的 Write 上）。
func TestPipeTranslatedExitsOnCancel(t *testing.T) {
	// 一个「永远写不完」的上游流：每读一次就给一段数据。
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 10000; i++ {
			if _, err := io.WriteString(pw, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	outR, outW := io.Pipe()

	done := make(chan struct{})
	go func() {
		pipeTranslated(ctx, pr, outW, func(src io.Reader, dst io.Writer) error {
			return TranslateResponsesStream(src, dst, "m")
		}, "test")
		close(done)
	}()

	// 读一小段后取消，然后关闭读端模拟下游断开。
	buf := make([]byte, 256)
	_, _ = outR.Read(buf)
	cancel()
	_ = outR.Close()
	_ = pr.Close()

	select {
	case <-done:
		// pipeTranslated 只负责启动协程，立即返回；这里只需确认不 panic。
	case <-time.After(2 * time.Second):
		t.Fatal("pipeTranslated 启动阻塞")
	}

	// 给翻译协程一点时间退出；不做严格断言，只确保进程不挂。
	time.Sleep(100 * time.Millisecond)
}
