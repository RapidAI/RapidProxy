package oauthprov

import (
	"bytes"
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

// exchangeClaude 用授权码换 Anthropic 的 token。
//
// Anthropic 的 token 端点要求 JSON（不是 form），且刷新时同样用 JSON。
func (s *Session) exchangeClaude(ctx context.Context, code string) (*Token, error) {
	tok, err := s.exchangeClaudeAgainst(ctx, claudeTokenURL, code)
	if err != nil {
		return nil, fmt.Errorf("Claude 授权失败: %w", err)
	}
	return tok, nil
}

// exchangeClaudeAgainst 是 exchangeClaude 的可指定端点版本（测试用）。
func (s *Session) exchangeClaudeAgainst(ctx context.Context, endpoint, code string) (*Token, error) {
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": s.verifier,
		"client_id":     claudeClientID,
		"redirect_uri":  claudeRedirectURI,
	}
	if s.state != "" {
		payload["state"] = s.state
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	tok, err := postTokenJSON(ctx, s.hc, endpoint, body, "anthropic")
	if err != nil {
		return nil, err
	}
	return tokenFrom(tok, ""), nil
}

// exchangeChatGPT 用授权码换 OpenAI 的 token（form 编码）。
func (s *Session) exchangeChatGPT(ctx context.Context, code string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", s.verifier)
	form.Set("redirect_uri", s.redirectURI)
	form.Set("client_id", chatgptClientID)
	tok, err := postTokenForm(ctx, s.hc, chatgptTokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 授权失败: %w", err)
	}
	return tokenFrom(tok, oauthflow.JWTAccountID(tok.AccessToken)), nil
}

// exchangeXAI 用授权码换 xAI 的 token（form 编码）。
func (s *Session) exchangeXAI(ctx context.Context, code string) (*Token, error) {
	tokenEndpoint := s.tokenEndpoint
	if tokenEndpoint == "" {
		_, discovered, err := discoverXAI(ctx, s.hc)
		if err != nil {
			return nil, err
		}
		tokenEndpoint = discovered
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", s.verifier)
	form.Set("redirect_uri", s.redirectURI)
	form.Set("client_id", xaiClientID)
	tok, err := postTokenForm(ctx, s.hc, tokenEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("xAI 授权失败: %w", err)
	}
	return tokenFrom(tok, ""), nil
}

// Refresh 用 refresh_token 续期。
func Refresh(ctx context.Context, provider Provider, hc *http.Client, refreshToken string) (*Token, error) {
	return refreshAgainst(ctx, hc, "", refreshToken, provider)
}

// RefreshAt 是可指定 token 端点的续期实现（endpoint 为空表示按 provider
// 用官方端点）。
func RefreshAt(ctx context.Context, provider Provider, hc *http.Client, endpoint, refreshToken string) (*Token, error) {
	return refreshAgainst(ctx, hc, endpoint, refreshToken, provider)
}

// refreshAgainst 是 Refresh 的可指定端点版本（测试用）；endpoint 为空时按
// provider 用固定端点。
func refreshAgainst(ctx context.Context, hc *http.Client, endpoint, refreshToken string, provider Provider) (*Token, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("缺少 refresh_token，请重新登录")
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	switch provider {
	case ProviderClaude:
		payload, _ := json.Marshal(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
			"client_id":     claudeClientID,
		})
		target := endpoint
		if target == "" {
			target = claudeTokenURL
		}
		tok, err := postTokenJSON(ctx, hc, target, payload, "anthropic")
		if err != nil {
			return nil, fmt.Errorf("Claude 令牌刷新失败: %w", err)
		}
		return tokenFrom(tok, ""), nil
	case ProviderChatGPT:
		target := endpoint
		if target == "" {
			target = chatgptTokenURL
		}
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		form.Set("client_id", chatgptClientID)
		tok, err := postTokenForm(ctx, hc, target, form)
		if err != nil {
			return nil, fmt.Errorf("ChatGPT 令牌刷新失败: %w", err)
		}
		return tokenFrom(tok, oauthflow.JWTAccountID(tok.AccessToken)), nil
	case ProviderXAI:
		target := endpoint
		if target == "" {
			_, discovered, err := discoverXAI(ctx, hc)
			if err != nil {
				return nil, err
			}
			target = discovered
		}
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		form.Set("client_id", xaiClientID)
		tok, err := postTokenForm(ctx, hc, target, form)
		if err != nil {
			return nil, fmt.Errorf("xAI 令牌刷新失败: %w", err)
		}
		return tokenFrom(tok, ""), nil
	default:
		return nil, fmt.Errorf("未知的 OAuth 服务商: %s", provider)
	}
}

// tokenFrom 把 token 响应折叠成 Token。旧 refresh_token 由调用方按需保留。
func tokenFrom(tok oauthflow.TokenResponse, accountID string) *Token {
	out := &Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		AccountID:    accountID,
	}
	if exp := tok.Expiry(time.Now()); !exp.IsZero() {
		out.ExpiresAt = exp.Unix()
	}
	return out
}

// postTokenForm 提交 form 编码的 token 请求。
func postTokenForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (oauthflow.TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthflow.TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return doToken(req, hc)
}

// postTokenJSON 提交 JSON 编码的 token 请求（Anthropic 专用）。
func postTokenJSON(ctx context.Context, hc *http.Client, endpoint string, body []byte, userAgent string) (oauthflow.TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return oauthflow.TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	// Anthropic 的 oauth/token 端点（含 refresh_token grant）要求带这个
	// beta 头才会被路由到 oauth-server；缺了它会 404/401。
	if strings.EqualFold(userAgent, "anthropic") {
		addAnthropicBeta(req, claudeOAuthBeta)
	}
	return doToken(req, hc)
}

// tokenStatusError 携带 token 端点的非 200 状态码，供调用方区分
// 「授权码被拒绝」（4xx）与「暂时性故障」（5xx / 网络错误）。
type tokenStatusError struct {
	StatusCode int
	Body       string
}

func (e *tokenStatusError) Error() string {
	return fmt.Sprintf("token 端点返回 HTTP %d: %s", e.StatusCode, truncate(e.Body, 300))
}

func doToken(req *http.Request, hc *http.Client) (oauthflow.TokenResponse, error) {
	ctx, cancel := context.WithTimeout(req.Context(), DefaultHTTPTimeout)
	defer cancel()
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		return oauthflow.TokenResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return oauthflow.TokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return oauthflow.TokenResponse{}, &tokenStatusError{
			StatusCode: resp.StatusCode,
			Body:       string(raw),
		}
	}
	return oauthflow.DecodeTokenResponse(raw)
}

// truncate 按 rune 截断，避免把中文错误切在 UTF-8 序列中间。
func truncate(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + "…"
}
