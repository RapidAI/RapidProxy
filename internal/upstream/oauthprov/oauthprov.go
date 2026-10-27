// Package oauthprov 实现 xAI / Claude / ChatGPT 三家的浏览器 OAuth 登录，
// 以及 OAuth 之后各自的请求线路。
//
// 三家共用 PKCE 基础（见 upstream/oauthflow），但差异很大：
//
//	claude  ：固定远程回调页 console.anthropic.com，用户复制授权码粘贴回来
//	          （code#state 形态）；token 交换用 JSON。
//	xai     ：loopback 回调 + OIDC discovery，授权额外带 referrer=grok-build。
//	chatgpt ：loopback localhost:1455，额外带 codex_cli_simplified_flow /
//	          originator / prompt=login，token 交换用 form。
//
// OAuth 拿到的都是 access token（不是 API Key），因此请求线路也各不相同：
// Claude 走 Anthropic Messages、ChatGPT 走 Codex Responses、xAI 走 OpenAI
// Chat Completions 但要带 X-XAI-Token-Auth 头。这些差异在本包内收口，
// 代理服务层仍只看到 OpenAI SSE。
package oauthprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream/oauthflow"
)

// Provider 是一套 OAuth 实现的标识。
type Provider string

const (
	// ProviderClaude 是 Anthropic Claude（Pro/Max 订阅）。
	ProviderClaude Provider = "claude"
	// ProviderChatGPT 是 OpenAI ChatGPT（Codex 订阅）。
	ProviderChatGPT Provider = "chatgpt"
	// ProviderXAI 是 xAI Grok（Grok Build 客户端身份）。
	ProviderXAI Provider = "xai"
)

// DefaultHTTPTimeout 是 token 交换 / 刷新的请求超时。
const DefaultHTTPTimeout = 60 * time.Second

// LoginLifetime 是一次浏览器授权的等待窗口。
const LoginLifetime = 10 * time.Minute

// Claude 的 OAuth 参数（Claude Code 注册的公开 client）。
const (
	claudeClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAuthURL     = "https://claude.ai/oauth/authorize"
	claudeTokenURL    = "https://console.anthropic.com/v1/oauth/token"
	claudeRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	claudeAPIBase     = "https://api.anthropic.com"
	// claudeOAuthBeta 是用 OAuth bearer 令牌访问 Messages API 时必须声明的
	// beta 标记；缺了它请求会被当作普通 API Key 调用而拒绝。
	claudeOAuthBeta = "oauth-2025-04-20"
)

// ChatGPT（Codex CLI）的 OAuth 参数。
const (
	chatgptClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatgptIssuer       = "https://auth.openai.com"
	chatgptAuthURL      = chatgptIssuer + "/oauth/authorize"
	chatgptTokenURL     = chatgptIssuer + "/oauth/token"
	chatgptCallbackPath = "/auth/callback"
	// chatgptCallbackPort 是 Codex CLI 的固定回调端口。OpenAI 对 redirect_uri
	// 做严格匹配，且只认 localhost（不是 127.0.0.1）。
	chatgptCallbackPort = 1455
	chatgptAPIBase      = "https://chatgpt.com/backend-api/codex"
)

// xAI（Grok Build）的 OAuth 参数。端点经 OIDC discovery 解析，
// 这里的默认值只在 discovery 不可用时兜底。
const (
	xaiClientID     = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiIssuer       = "https://auth.x.ai"
	xaiCallbackPath = "/callback"
	xaiAPIBase      = "https://api.x.ai/v1"
)

var (
	claudeScopes = []string{"org:create_api_key", "user:profile", "user:inference"}
	// ChatGPT 的 scope 与 Codex CLI 一致；api.connectors.* 是调用
	// Responses API 所必需的。
	chatgptScopes = []string{
		"openid", "profile", "email", "offline_access",
		"api.connectors.read", "api.connectors.invoke",
	}
	xaiScopes = []string{
		"openid", "profile", "email", "offline_access", "grok-cli:access",
		"api:access", "conversations:read", "conversations:write",
		"workspaces:read", "workspaces:write",
	}
)

// Token 是一份 OAuth 凭据。
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix 秒；0 表示不过期
	// AccountID 是 ChatGPT 的 chatgpt-account-id（从 JWT 取），其它家为空。
	AccountID string
	// Nickname 是登录后展示的账号名（取不到时为空）。
	Nickname string
}

// Session 是一次进行中的登录流程。
//
// 两种形态：
//   - 回调式（xai / chatgpt）：AuthURL 打开后由本地回调收口，Poll 等待回调。
//   - 粘贴式（claude）：AuthURL 打开后用户在回调页复制授权码，由
//     SubmitCode 收口；Poll 只占位等待。
type Session struct {
	Provider Provider
	AuthURL  string
	// PasteCode 为 true 表示需要用户粘贴授权码（Claude）。
	PasteCode bool
	ExpiresAt time.Time

	verifier    string
	redirectURI string
	callback    *oauthflow.Callback
	state       string
	hc          *http.Client
	// tokenEndpoint 是登录阶段解析出的 token 端点（xAI 经 OIDC discovery）。
	// 为空时按 provider 的固定端点。
	tokenEndpoint string

	// codeCh 接收用户粘贴的授权码（粘贴式流程）。
	codeCh chan string
}

// StartSession 发起一次登录。hc 用于 token 交换（应带用户的代理设置）。
func StartSession(ctx context.Context, provider Provider, hc *http.Client) (*Session, error) {
	if hc == nil {
		hc = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	switch provider {
	case ProviderClaude:
		return startClaudeSession(hc)
	case ProviderChatGPT:
		return startChatGPTSession(hc)
	case ProviderXAI:
		return startXAISession(ctx, hc)
	default:
		return nil, fmt.Errorf("未知的 OAuth 服务商: %s", provider)
	}
}

// startClaudeSession 准备 Anthropic 的授权链接。
//
// Anthropic 的回调页由它自己托管，本机收不到重定向，所以走「粘贴授权码」
// 形态：用户授权后从页面上复制 code（页面显示为 code#state）粘贴回来。
func startClaudeSession(hc *http.Client) (*Session, error) {
	verifier, err := oauthflow.GenerateVerifier()
	if err != nil {
		return nil, fmt.Errorf("生成 PKCE 参数失败: %w", err)
	}
	// Anthropic 要求 state 与 code_verifier 同值（与 Claude Code 行为一致）。
	state := verifier
	params := url.Values{}
	params.Set("code", "true")
	params.Set("response_type", "code")
	params.Set("client_id", claudeClientID)
	params.Set("redirect_uri", claudeRedirectURI)
	params.Set("scope", strings.Join(claudeScopes, " "))
	params.Set("code_challenge", oauthflow.Challenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	return &Session{
		Provider:  ProviderClaude,
		AuthURL:   claudeAuthURL + "?" + params.Encode(),
		PasteCode: true,
		ExpiresAt: time.Now().Add(LoginLifetime),
		verifier:  verifier,
		state:     state,
		hc:        hc,
		codeCh:    make(chan string, 1),
	}, nil
}

// startChatGPTSession 准备 OpenAI 的 loopback 授权流程。
func startChatGPTSession(hc *http.Client) (*Session, error) {
	verifier, err := oauthflow.GenerateVerifier()
	if err != nil {
		return nil, fmt.Errorf("生成 PKCE 参数失败: %w", err)
	}
	cb, err := oauthflow.StartCallback(chatgptCallbackPath, chatgptCallbackPort)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 本地回调启动失败: %w", err)
	}
	// OpenAI 只认 localhost；用 127.0.0.1 会被 redirect_uri 校验拒绝。
	redirectURI := cb.RedirectURI("localhost")
	state := oauthflow.GenerateState()

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", chatgptClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", strings.Join(chatgptScopes, " "))
	params.Set("code_challenge", oauthflow.Challenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	// Codex CLI 兼容参数：缺这些会被授权页当成第三方应用拒绝。
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	params.Set("originator", "codex_cli_rs")
	// 强制重新登录：浏览器里已有的 session 会让授权直接失败（unknown_error）。
	params.Set("prompt", "login")

	return &Session{
		Provider:    ProviderChatGPT,
		AuthURL:     chatgptAuthURL + "?" + params.Encode(),
		ExpiresAt:   time.Now().Add(LoginLifetime),
		verifier:    verifier,
		redirectURI: redirectURI,
		callback:    cb,
		state:       state,
		hc:          hc,
	}, nil
}

// xaiDiscovery 是 OIDC discovery 里我们关心的两个端点。
type xaiDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// startXAISession 准备 xAI 的 loopback 授权流程（端点经 OIDC discovery）。
func startXAISession(ctx context.Context, hc *http.Client) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	authEndpoint, tokenEndpoint, err := discoverXAI(ctx, hc)
	if err != nil {
		return nil, err
	}
	verifier, err := oauthflow.GenerateVerifier()
	if err != nil {
		return nil, fmt.Errorf("生成 PKCE 参数失败: %w", err)
	}
	cb, err := oauthflow.StartCallback(xaiCallbackPath, 0)
	if err != nil {
		return nil, fmt.Errorf("xAI 本地回调启动失败: %w", err)
	}
	redirectURI := cb.RedirectURI("127.0.0.1")
	state := oauthflow.GenerateState()

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", xaiClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", strings.Join(xaiScopes, " "))
	params.Set("code_challenge", oauthflow.Challenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	params.Set("referrer", "grok-build")

	return &Session{
		Provider:      ProviderXAI,
		AuthURL:       authEndpoint + "?" + params.Encode(),
		ExpiresAt:     time.Now().Add(LoginLifetime),
		verifier:      verifier,
		redirectURI:   redirectURI,
		callback:      cb,
		state:         state,
		hc:            hc,
		tokenEndpoint: tokenEndpoint,
	}, nil
}

// discoverXAI 解析 xAI 的 OIDC 端点；失败时回落到固定端点。
//
// 每次登录都重新 discovery：xAI 调整端点时不必发新版程序。discovery 失败
// （离线、被代理拦）不该让登录直接失败，用已知端点兜底更实用。
func discoverXAI(ctx context.Context, hc *http.Client) (authEndpoint, tokenEndpoint string, err error) {
	discoveryURL := strings.TrimRight(xaiIssuer, "/") + "/.well-known/openid-configuration"
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodGet, discoveryURL, nil)
	if reqErr == nil {
		if resp, doErr := hc.Do(req); doErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var d xaiDiscovery
				if jsonErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&d); jsonErr == nil {
					a := strings.TrimSpace(d.AuthorizationEndpoint)
					t := strings.TrimSpace(d.TokenEndpoint)
					if a != "" && t != "" {
						return a, t, nil
					}
				}
			}
		}
	}
	// 兜底：auth.x.ai 的常规路径。
	return xaiIssuer + "/oauth2/authorize", xaiIssuer + "/oauth2/token", nil
}

// SubmitCode 提交用户粘贴的授权码（仅粘贴式流程）。
//
// 保留最新一次：用户发现贴错了会立刻再贴一次，而轮询间隔是 2 秒，期间
// 可能来两次。丢旧的、留新的才符合直觉（通道容量 1 时直接写会丢掉新的）。
func (s *Session) SubmitCode(code string) {
	if s == nil || s.codeCh == nil {
		return
	}
	for {
		select {
		case s.codeCh <- code:
			return
		default:
		}
		// 通道满：取走一个旧值再试，保证最新一次一定被保留。
		select {
		case <-s.codeCh:
		default:
			return
		}
	}
}

// Cancel 释放本地回调监听。
func (s *Session) Cancel() {
	if s == nil {
		return
	}
	if s.callback != nil {
		s.callback.Stop()
	}
}

// CallbackPort 返回本地回调端口；粘贴式流程（Claude）没有回调，返回 0。
func (s *Session) CallbackPort() int {
	if s == nil || s.callback == nil {
		return 0
	}
	return s.callback.Port()
}

// Poll 等待授权完成并换取 token。返回 (nil, nil) 表示仍在等待。
func (s *Session) Poll(ctx context.Context) (*Token, error) {
	if s == nil {
		return nil, errors.New("没有正在进行的登录")
	}
	if time.Now().After(s.ExpiresAt) {
		return nil, ErrLoginExpired
	}
	if s.PasteCode {
		select {
		case code := <-s.codeCh:
			return s.exchange(ctx, code)
		default:
			return nil, nil
		}
	}
	if s.callback == nil {
		return nil, errors.New("登录会话缺少本地回调")
	}
	// Wait 会阻塞到回调落地：给它一个截止到 ExpiresAt 的超时，否则过期
	// 判断形同虚设——授权窗口到了也不会结束，只能等外层 10 分钟 ctx 兜底。
	waitCtx, cancel := context.WithDeadline(ctx, s.ExpiresAt)
	defer cancel()
	res, err := s.callback.Wait(waitCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrLoginExpired
		}
		return nil, err
	}
	if res.State != "" && !strings.EqualFold(res.State, s.state) {
		return nil, errors.New("授权回调 state 不匹配，请重新登录")
	}
	return s.exchange(ctx, res.Code)
}

// ErrLoginExpired 表示登录会话已过期。
var ErrLoginExpired = errors.New("登录链接已过期，请重新发起登录")

// ErrCodeRejected 表示授权码换取 token 失败（码输错、过期、已被用过）。
//
// 粘贴式流程（Claude）里这不是终态：用户可能只是复制漏了字符，应允许
// 重新粘贴。回调式流程遇到它才是真正的失败。
var ErrCodeRejected = errors.New("授权码无效或已过期")

// exchange 用授权码换取 token。
func (s *Session) exchange(ctx context.Context, rawCode string) (*Token, error) {
	code := oauthflow.NormalizeCode(rawCode)
	if code == "" {
		if s.PasteCode {
			return nil, fmt.Errorf("%w: 授权码为空", ErrCodeRejected)
		}
		return nil, errors.New("授权码为空，请重新登录")
	}
	var (
		tok *Token
		err error
	)
	switch s.Provider {
	case ProviderClaude:
		tok, err = s.exchangeClaude(ctx, code)
	case ProviderChatGPT:
		tok, err = s.exchangeChatGPT(ctx, code)
	case ProviderXAI:
		tok, err = s.exchangeXAI(ctx, code)
	default:
		return nil, fmt.Errorf("未知的 OAuth 服务商: %s", s.Provider)
	}
	if err != nil && s.PasteCode {
		// 粘贴式流程把换 token 失败标成可重试：调用方据此保留会话，
		// 让用户重新粘贴而不是从头授权。
		//
		// 但只有上游明确拒绝才算「授权码无效」；网络故障（DNS、超时、
		// 代理不通）标成码错会让用户徒劳地反复粘贴。后者用普通错误
		// 返回，界面提示的是真实原因。
		if isCodeRejection(err) {
			return nil, fmt.Errorf("%w: %v", ErrCodeRejected, err)
		}
		return nil, err
	}
	return tok, err
}

// isCodeRejection 判断 token 端点的错误是否属于「授权码本身有问题」。
//
// 只有 HTTP 4xx（除 408/429）才算：那说明请求到达了上游但被拒绝。网络层
// 错误（没有拿到响应）与 5xx 都是暂时性故障，不该让用户去改授权码。
func isCodeRejection(err error) bool {
	var statusErr *tokenStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	code := statusErr.StatusCode
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return false
	}
	return code >= 400 && code < 500
}
