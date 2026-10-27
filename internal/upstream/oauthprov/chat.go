package oauthprov

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/znsoftm/RapidProxy/internal/upstream/anthropicconv"
)

// 各家的请求线路：
//
//	claude  ：Anthropic Messages（{base}/v1/messages），Authorization: Bearer
//	          + anthropic-beta；请求体经 anthropicconv 翻译，响应 SSE 再翻译
//	          回 OpenAI SSE。
//	xai     ：Responses API（{base}/v1/responses），额外带
//	          X-XAI-Token-Auth: xai-grok-cli（Grok Build 的 OAuth 标记）。
//	          注意不是 chat/completions——xAI 的第一方 OAuth 线路只开在
//	          Responses 上（与 aicoder 的 xAI-Grok 预设一致）。
//	chatgpt ：Codex Responses（{base}/responses），请求体与 SSE 都要翻译。

// Chat 把一份 OpenAI Chat Completions 请求转发到 OAuth 上游，
// 返回的响应恒为 OpenAI 形状（OpenAI SSE 或 OpenAI 错误 JSON）。
func Chat(ctx context.Context, hc *http.Client, provider Provider, base, accessToken, accountID, userAgent string, body []byte) (*http.Response, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("缺少访问令牌，请重新登录")
	}
	switch provider {
	case ProviderClaude:
		return chatClaude(ctx, hc, base, accessToken, userAgent, body)
	case ProviderXAI:
		return chatXAI(ctx, hc, base, accessToken, userAgent, body)
	case ProviderChatGPT:
		return chatChatGPT(ctx, hc, base, accessToken, accountID, userAgent, body)
	default:
		return nil, fmt.Errorf("未知的 OAuth 服务商: %s", provider)
	}
}

// chatXAI 走 xAI 的 Responses API。
//
// xAI 的第一方 OAuth 线路只开在 Responses 上（参照实现里 xAI-Grok 预设的
// wire_api 恒为 responses）；打 chat/completions 会拿不到订阅额度。请求体
// 与响应流都复用与 ChatGPT 相同的翻译层，只是端点与请求头不同。
func chatXAI(ctx context.Context, hc *http.Client, base, accessToken, userAgent string, body []byte) (*http.Response, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	model, _ := payload["model"].(string)
	// xAI 是标准 Responses 线路：保留 max_output_tokens。
	reqBody, err := json.Marshal(OpenAIChatToResponses(payload, model, false))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesEndpoint(base), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	// Grok Build 用这个头把「第一方用户 OAuth 令牌」与普通 API Key 区分开；
	// 缺了它，OAuth token 会被当成无效凭据。
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return responsesErrorToOpenAI(resp)
	}
	if !isEventStream(resp) {
		return foldResponsesResponse(resp, model)
	}
	pr, pw := io.Pipe()
	go pipeTranslated(ctx, resp.Body, pw, func(src io.Reader, dst io.Writer) error {
		return TranslateResponsesStream(src, dst, model)
	}, "xAI")
	return synthesizedSSE(pr), nil
}

// chatClaude 走 Anthropic Messages 线协议。
func chatClaude(ctx context.Context, hc *http.Client, base, accessToken, userAgent string, body []byte) (*http.Response, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	model, _ := payload["model"].(string)
	reqBody, err := json.Marshal(anthropicconv.RequestOpenAIToAnthropic(payload, model))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesEndpoint(base), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	// OAuth 令牌只能走 Authorization: Bearer——它不是平台 API Key，
	// 同时带 x-api-key 会让网关按 API Key 校验而失败。
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-version", "2023-06-01")
	// 用 OAuth bearer 令牌访问 Messages API 必须声明这个 beta，
	// 否则请求会被当作普通 API Key 调用而拒绝（Anthropic 官方 SDK 在
	// bearer 认证路径上同样会自动补这个头）。合并而不是覆盖：以后若再加
	// 别的 beta（如 thinking），不能把已有的挤掉。
	addAnthropicBeta(req, claudeOAuthBeta)
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return anthropicErrorToOpenAI(resp)
	}
	if !isEventStream(resp) {
		return foldAnthropicMessage(resp, model)
	}
	pr, pw := io.Pipe()
	go pipeTranslated(ctx, resp.Body, pw, func(src io.Reader, dst io.Writer) error {
		return anthropicconv.TranslateStream(src, dst, model)
	}, "Claude")
	return synthesizedSSE(pr), nil
}

// pipeTranslated 在独立协程里把上游流翻译成 OpenAI SSE 写入管道。
//
// 三个收口缺一不可：
//   - panic 收口成流错误，坏上游不能崩掉整个代理进程；
//   - 上游响应体必须关闭，否则连接池泄漏；
//   - ctx 取消时主动关闭读端，让阻塞在 Write 上的翻译立即退出。
func pipeTranslated(
	ctx context.Context,
	src io.ReadCloser,
	pw *io.PipeWriter,
	translate func(src io.Reader, dst io.Writer) error,
	label string,
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				_ = pw.CloseWithError(fmt.Errorf("翻译 %s 流失败: %v", label, r))
			}
		}()
		// 下游断开（客户端取消）时，Write 会永久阻塞：主动关闭管道把
		// 翻译协程从 Write 里踢出来。
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
			case <-done:
			}
		}()
		err := translate(src, pw)
		_ = src.Close()
		_ = pw.CloseWithError(err)
	}()
}

// chatChatGPT 走 Codex Responses API，并在边界完成两个方向的翻译。
func chatChatGPT(ctx context.Context, hc *http.Client, base, accessToken, accountID, userAgent string, body []byte) (*http.Response, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	model, _ := payload["model"].(string)
	// ChatGPT 走 Codex 订阅线路：该线路拒绝 max_output_tokens。
	reqBody, err := json.Marshal(OpenAIChatToResponses(payload, model, true))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesEndpoint(base), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return responsesErrorToOpenAI(resp)
	}
	if !isEventStream(resp) {
		return foldResponsesResponse(resp, model)
	}
	pr, pw := io.Pipe()
	go pipeTranslated(ctx, resp.Body, pw, func(src io.Reader, dst io.Writer) error {
		return TranslateResponsesStream(src, dst, model)
	}, "Codex")
	return synthesizedSSE(pr), nil
}

// messagesEndpoint 拼接 Anthropic Messages 端点。
func messagesEndpoint(base string) string {
	return appendVersionedPath(base, "/messages")
}

// responsesEndpoint 拼接 Responses API 端点。
//
// ChatGPT 的 base 已是 .../backend-api/codex，端点就是 {base}/responses；
// 其它（如自建反代）同样按此拼接。
func responsesEndpoint(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(trimmed, "/responses") {
		return trimmed
	}
	return trimmed + "/responses"
}

// appendVersionedPath 在 base 后拼接子路径；末段不是版本号时补 /v1。
// 与 upstream/custom 的同名函数同口径。
func appendVersionedPath(base, subPath string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if hasVersionSuffix(trimmed) {
		return trimmed + subPath
	}
	return trimmed + "/v1" + subPath
}

func hasVersionSuffix(endpoint string) bool {
	idx := strings.LastIndex(endpoint, "/")
	if idx < 0 || idx == len(endpoint)-1 {
		return false
	}
	segment := strings.ToLower(endpoint[idx+1:])
	if len(segment) < 2 || segment[0] != 'v' {
		return false
	}
	for _, r := range segment[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// addAnthropicBeta 把 beta 标记合并进 anthropic-beta 头（去重）。
func addAnthropicBeta(req *http.Request, beta string) {
	beta = strings.TrimSpace(beta)
	if beta == "" {
		return
	}
	existing := strings.TrimSpace(req.Header.Get("anthropic-beta"))
	if existing == "" {
		req.Header.Set("anthropic-beta", beta)
		return
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(part), beta) {
			return
		}
	}
	req.Header.Set("anthropic-beta", existing+","+beta)
}

// isEventStream 判断响应是否是 SSE 流。
//
// 与 upstream/custom 同口径：少数网关回的是 "application/stream+json"
// 之类，只认 text/event-stream 会把它们误判成非流式，进而按一次性 JSON
// 去解析一个真正的流。
func isEventStream(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream") || strings.Contains(ct, "stream")
}

// synthesizedSSE 把翻译后的流包装成 OpenAI SSE 响应。
//
// Body 用 CloseWithError 而不是 NopCloser：翻译协程写在管道上，下游一旦
// 断开（客户端取消、服务层提前收尾），读端关闭必须让写端立即出错退出，
// 否则协程永久阻塞在 pw.Write 上——每次中断的请求漏一个 goroutine。
func synthesizedSSE(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       body,
		Close:      true,
	}
}

// anthropicErrorToOpenAI 把 Anthropic 错误体翻成 OpenAI 错误 JSON，保留状态码。
func anthropicErrorToOpenAI(resp *http.Response) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	message := truncate(string(raw), 500)
	var aerr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &aerr) == nil && strings.TrimSpace(aerr.Error.Message) != "" {
		message = aerr.Error.Message
	}
	return openAIError(resp.StatusCode, message, resp.Header.Get("Retry-After")), nil
}

// responsesErrorToOpenAI 把 Responses API 的错误体翻成 OpenAI 错误 JSON。
func responsesErrorToOpenAI(resp *http.Response) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	message := truncate(string(raw), 500)
	var rerr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &rerr) == nil && strings.TrimSpace(rerr.Error.Message) != "" {
		message = rerr.Error.Message
	}
	return openAIError(resp.StatusCode, message, resp.Header.Get("Retry-After")), nil
}

// openAIError 构造携带 OpenAI 错误体的合成响应。
func openAIError(status int, message, retryAfter string) *http.Response {
	if status < 400 {
		status = http.StatusBadGateway
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error"},
	})
	resp := &http.Response{
		StatusCode: status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Close:      true,
	}
	if ra := strings.TrimSpace(retryAfter); ra != "" {
		resp.Header.Set("Retry-After", ra)
	}
	return resp
}

// foldAnthropicMessage 把非流式的 Anthropic 消息折叠成 OpenAI 响应。
func foldAnthropicMessage(resp *http.Response, model string) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	_ = resp.Body.Close()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("解析 Claude 响应失败: %w", err)
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		if strings.TrimSpace(msg) == "" {
			msg = "upstream returned an error"
		}
		return openAIError(http.StatusBadGateway, truncate(msg, 500), ""), nil
	}
	out, err := json.Marshal(anthropicconv.AnthropicToOpenAIResponse(body, model))
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(out)),
		Close:      true,
	}, nil
}

// foldResponsesResponse 把非流式的 Responses 响应折叠成 OpenAI Chat 响应。
func foldResponsesResponse(resp *http.Response, model string) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	_ = resp.Body.Close()
	out, err := ResponsesToOpenAIChat(raw, model)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(out)),
		Close:      true,
	}, nil
}

// readSSE 逐条读取 SSE 事件，返回 event 名与 data 正文。
//
// 与 anthropicconv 的读取逻辑同口径：容忍缺少空行分隔、以及 event 名缺失
// 时从 data 里的 type 字段推断。
func readSSE(r *bufio.Reader) (event, data string, err error) {
	for {
		line, readErr := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if readErr != nil {
			if data != "" {
				return event, data, nil
			}
			return "", "", readErr
		}
		if line == "" && data != "" {
			return event, data, nil
		}
	}
}
