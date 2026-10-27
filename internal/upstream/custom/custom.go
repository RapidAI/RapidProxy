// Package custom 实现通用「URL + API Key + 线协议(OpenAI/Anthropic)」上游：
// xAI / Claude / ChatGPT / 智谱编程 以及用户自建的任意 OpenAI、Anthropic
// 兼容端点都归到这一族，按 Protocol 区分线协议。登录是 API Key 制。
//
// 与 opencode 族不同，custom 不做按主机的身份改写（不改写会话号、不强制
// 官方客户端 UA）：请求头带 Authorization: Bearer <key>（Anthropic 另加
// x-api-key / anthropic-version），并把 Profile.UserAgent 原样作为 User-Agent
// 发送——智谱等网关按该 UA 判定「编程客户端」以适用赠送额度。
// Anthropic 线协议时，请求体先经 anthropicconv 翻成 Anthropic 形状，上游的
// Anthropic SSE 流再被实时翻译成 OpenAI SSE——代理服务层只认 OpenAI SSE，
// 因此翻译发生在本适配层边界。
package custom

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream/anthropicconv"
)

// Protocol 取值。
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
)

// ErrKeyInvalid 表示用户提交的文本无法解析成合法形态的 API Key。
var ErrKeyInvalid = errors.New("invalid api key")

// ErrKeyRejected 表示上游明确拒绝了密钥（401/403）。
var ErrKeyRejected = errors.New("api key rejected")

// StatusError 携带上游非 200 响应的状态码与正文。
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("上游返回 HTTP %d: %s", e.Code, truncate(e.Body, 200))
}

// NormalizeAPIKey 清洗用户粘贴的 API Key：去掉引号、JSON 包装与 Bearer 前缀。
func NormalizeAPIKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", ErrKeyInvalid
	}
	if after, ok := strings.CutPrefix(key, "Bearer "); ok {
		key = strings.TrimSpace(after)
	}
	if after, ok := strings.CutPrefix(key, "Bearer"); ok {
		key = strings.TrimSpace(after)
	}
	// 去掉首尾引号 / 反引号与整段 JSON（{"key":"..."} 形态）。
	key = strings.Trim(key, "`\"' \t\r\n")
	if start := strings.Index(key, "{"); start >= 0 {
		// 尝试从 JSON 里取 key / api_key 字段。
		var obj map[string]any
		if err := json.Unmarshal([]byte(key), &obj); err == nil {
			for _, name := range []string{"key", "api_key", "apiKey", "access_token", "token"} {
				if v, ok := obj[name].(string); ok && strings.TrimSpace(v) != "" {
					key = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	if strings.TrimSpace(key) == "" {
		return "", ErrKeyInvalid
	}
	return key, nil
}

// VerifyAPIKey 用一次模型目录请求校验密钥是否被上游接受。联机类错误
// （超时、DNS、5xx）原样返回但不视为密钥无效；401/403 返回 ErrKeyRejected。
func VerifyAPIKey(ctx context.Context, hc *http.Client, base, protocol, userAgent, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrKeyInvalid
	}
	// Anthropic 的 /v1/models 在不同网关支持度不一，改用一次最小对话请求
	// 校验更稳妥；这里仍优先用 /models（更轻量），失败时调用方放行即可。
	endpoint := modelsEndpoint(base, protocol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	setAuthHeader(req, protocol, key)
	resp, err := hc.Do(req)
	if err != nil {
		// 联机错误：不算密钥无效，放行保存，让运行时暴露真实问题。
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: http %d", ErrKeyRejected, resp.StatusCode)
	}
	return nil
}

// Chat 把 OpenAI 形状的请求体转发到 custom 上游。model 从 body 的 "model"
// 字段提取（Anthropic 翻译时需要），调用方只需把已改写好的 body 传入。
//
// protocol=openai：直接 POST {base}/chat/completions，原样透传上游的 SSE。
// protocol=anthropic：先把请求体翻译成 Anthropic 形状、POST {base}/v1/messages，
// 再把上游的 Anthropic SSE 流实时翻译成 OpenAI SSE——返回的 *http.Response
// 已是 OpenAI SSE，可直接被代理服务层透传 / 按首事件换号。上游非 200 时把
// Anthropic 错误体翻译成 OpenAI 错误 JSON 返回（HTTP 200 + 非 SSE，交给
// 服务层 writeNonStreamUpstream 透传）。
func Chat(ctx context.Context, hc *http.Client, base, apiKey, protocol, userAgent string, body []byte) (*http.Response, error) {
	apiKey = strings.TrimSpace(apiKey)
	if strings.EqualFold(protocol, ProtocolAnthropic) {
		return chatAnthropic(ctx, hc, base, apiKey, userAgent, body)
	}
	return chatOpenAI(ctx, hc, base, apiKey, userAgent, body)
}

func chatOpenAI(ctx context.Context, hc *http.Client, base, apiKey, userAgent string, body []byte) (*http.Response, error) {
	endpoint := appendVersionedPath(base, "/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream, */*")
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return hc.Do(req)
}

func chatAnthropic(ctx context.Context, hc *http.Client, base, apiKey, userAgent string, body []byte) (*http.Response, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	model, _ := payload["model"].(string)
	anthropicReq := anthropicconv.RequestOpenAIToAnthropic(payload, model)
	reqBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, err
	}
	endpoint := appendVersionedPath(base, "/messages")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	// 智谱等网关按 User-Agent 区分「编程客户端」：带上 profile 里保存的
	// 身份（如 opencode）才会命中 ZCode / 编程套餐赠送额度。
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// 错误体：翻译成 OpenAI 错误 JSON 并透传（HTTP 200 + 非 SSE）。
		return translateAnthropicError(resp)
	}
	if !isAnthropicSSE(resp) {
		// 极少见：非 SSE 的 200 响应（如一次性 JSON）。折叠成 OpenAI 响应。
		return foldAnthropicResponse(resp, model)
	}
	// SSE 流：用 io.Pipe 把 Anthropic 流实时翻译成 OpenAI 流。
	pr, pw := io.Pipe()
	go func() {
		err := anthropicconv.TranslateStream(resp.Body, pw, model)
		_ = resp.Body.Close()
		_ = pw.CloseWithError(err)
	}()
	out := &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       pr,
		Close:      true,
	}
	return out, nil
}

// ListModels 读取 custom 上游的模型目录（OpenAI 兼容 {data:[{id,name}]} 或
// Anthropic 兼容 {data:[{id,...}]}，均兼容裸数组）。
func ListModels(ctx context.Context, hc *http.Client, base, protocol, apiKey, userAgent string) ([]Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("需要已登录的访问令牌")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	endpoint := modelsEndpoint(base, protocol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	setAuthHeader(req, protocol, apiKey)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		statusErr := &StatusError{Code: resp.StatusCode, Body: string(raw)}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %w", ErrKeyRejected, statusErr)
		}
		return nil, statusErr
	}
	models, err := parseModels(raw)
	if err != nil {
		return nil, err
	}
	return models, nil
}

// modelsEndpoint 拼接模型目录端点。OpenAI 与 Anthropic 的目录都在 {base}/models，
// base 是否已带版本后缀（/v1、/v4、/api/anthropic/v1 等）都能正确拼接。
func modelsEndpoint(base, _ string) string {
	return appendVersionedPath(base, "/models")
}

// appendVersionedPath 在 base 后拼接子路径；base 末段不是版本号（v1/v2/v4…）
// 时自动补 /v1，与 aicoder 的 openAICompatEndpointHasVersionSuffix 同口径。
// 这样用户填 https://api.deepseek.com 或 https://api.deepseek.com/v1 都能工作，
// 而 https://open.bigmodel.cn/api/paas/v4 这类非 /v1 版本同样不会误加。
func appendVersionedPath(base, subPath string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if hasVersionSuffix(trimmed) {
		return trimmed + subPath
	}
	return trimmed + "/v1" + subPath
}

// hasVersionSuffix 判断 URL 末段是否是版本号（v 后跟纯数字）。
func hasVersionSuffix(endpoint string) bool {
	lastSlash := strings.LastIndex(endpoint, "/")
	if lastSlash < 0 || lastSlash == len(endpoint)-1 {
		return false
	}
	segment := strings.ToLower(endpoint[lastSlash+1:])
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

func setAuthHeader(req *http.Request, protocol, key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	if strings.EqualFold(protocol, ProtocolAnthropic) {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

func parseModels(raw []byte) ([]Model, error) {
	type entry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	toModels := func(list []entry) []Model {
		out := make([]Model, 0, len(list))
		for _, e := range list {
			id := strings.TrimSpace(e.ID)
			if id == "" {
				continue
			}
			// OpenAI 用 name，Anthropic 用 display_name；都没有则退回 ID。
			name := strings.TrimSpace(e.Name)
			if name == "" {
				name = strings.TrimSpace(e.DisplayName)
			}
			if name == "" {
				name = id
			}
			out = append(out, Model{ID: id, Name: name})
		}
		return out
	}
	var envelope struct {
		Data []entry `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Data) > 0 {
		return toModels(envelope.Data), nil
	}
	// 兼容裸露数组。
	var list []entry
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("模型目录响应无法解析: %w", err)
	}
	return toModels(list), nil
}

func isAnthropicSSE(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream") || strings.Contains(ct, "stream")
}

func translateAnthropicError(resp *http.Response) (*http.Response, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	// 解析 Anthropic 错误体（{"type":"error","error":{"type","message"}}），
	// 折叠成 OpenAI 形状；解析不出来就保留原始正文。
	message := truncate(string(raw), 500)
	var aerr struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &aerr) == nil && strings.TrimSpace(aerr.Error.Message) != "" {
		message = aerr.Error.Message
	}
	// 保留上游状态码：服务层的换号/冷却逻辑依赖它区分 401/429/5xx。响应体
	// 已是 OpenAI 错误形状，writeNonStreamUpstream 会连同状态码一起透传。
	status := resp.StatusCode
	if status < 400 {
		status = http.StatusBadGateway
	}
	out := errorResponse(status, message)
	// 保留 Retry-After：服务层据此把账号冷却到上游建议的时刻，而不是固定退避。
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		out.Header.Set("Retry-After", ra)
	}
	return out, nil
}

// errorResponse 构造一个携带 OpenAI 形状错误体的合成响应（body 用 io.Pipe，
// 交给服务层按普通响应读取）。
func errorResponse(status int, message string) *http.Response {
	errJSON, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
		},
	})
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(errJSON)
		_ = pw.Close()
	}()
	return &http.Response{
		StatusCode: status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       pr,
		Close:      true,
	}
}

func foldAnthropicResponse(resp *http.Response, model string) (*http.Response, error) {
	// 非流式响应可能很长（含大量文本/工具参数），上限给到 32MB；错误体才用 8KB。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	_ = resp.Body.Close()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 响应失败: %w", err)
	}
	// 极少数网关会用 200 + {"type":"error","error":{...}} 上报失败：此时不能
	// 当成正常消息折叠（会得到一条空回复），要按错误带回。
	if errObj, ok := body["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		msg = truncate(strings.TrimSpace(msg), 500)
		if msg == "" {
			msg = "upstream returned an error"
		}
		return errorResponse(http.StatusBadGateway, msg), nil
	}
	out := anthropicconv.AnthropicToOpenAIResponse(body, model)
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(data)
		_ = pw.Close()
	}()
	return &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       pr,
		Close:      true,
	}, nil
}

// Model 是一条 custom 上游目录条目。
type Model struct {
	ID   string
	Name string
}

// KeyLabel 返回一个不会泄露整把密钥的展示标签。
func KeyLabel(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 8 {
		return "key"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)[:8]
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}
