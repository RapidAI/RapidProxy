// Package upstream 实现对 CodeBuddy / WorkBuddy 上游协议的调用：
// 登录、令牌刷新、模型同步与 chat completions 转发。
//
// 上游与官方 CLI 插件共用同一套协议：
//
//	POST {base}/v2/plugin/auth/state?platform=CLI     发起登录，返回 state 与 authUrl
//	GET  {base}/v2/plugin/auth/token?state=<state>    轮询登录结果（未完成时返回 code 11217）
//	GET  {base}/v2/plugin/login/account?state=<state> 登录完成后取账号信息
//	POST {base}/v2/plugin/auth/token/refresh          刷新访问令牌
//	GET  {base}/v3/config                             模型与配额配置
//	POST {base}/v2/chat/completions                   对话（仅支持流式）
package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/config"
)

// ErrNoAccessToken 表示凭据里没有可用的访问令牌。
var ErrNoAccessToken = errors.New("凭据缺少 accessToken，请重新登录")

// ErrLoginTimeout 表示登录会话已过期。
var ErrLoginTimeout = errors.New("登录会话已过期，请重新发起登录")

// refreshLeeway 是提前刷新的安全余量。
const refreshLeeway = 5 * time.Minute

// defaultMaxOutput 是上游与白名单都没给出输出上限时的兜底值。
const defaultMaxOutput = 8192

// Tokens 是持久化的令牌信息。
type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

// Account 是账号信息。
type Account struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// Credential 是一份可用的账号凭据。
type Credential struct {
	Provider  string    `json:"provider"`
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Auth      Tokens    `json:"auth"`
	Account   Account   `json:"account"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// DisplayName 返回给界面展示的账号名。
func (c *Credential) DisplayName() string {
	if c.Account.Nickname != "" {
		return c.Account.Nickname
	}
	if c.Account.UID != "" {
		return c.Account.UID
	}
	return c.ID
}

// ExpiresAtTime 返回访问令牌过期时间。
func (c *Credential) ExpiresAtTime() time.Time {
	if c.Auth.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(c.Auth.ExpiresAt, 0)
}

// Expired 表示访问令牌已过期。
func (c *Credential) Expired() bool {
	t := c.ExpiresAtTime()
	return !t.IsZero() && time.Now().After(t)
}

// NeedsRefresh 表示访问令牌即将过期，应提前刷新。
func (c *Credential) NeedsRefresh() bool {
	t := c.ExpiresAtTime()
	if t.IsZero() {
		return false
	}
	return time.Now().After(t.Add(-refreshLeeway))
}

// ModelInfo 是暴露给 OpenAI /v1/models 的模型条目。
type ModelInfo struct {
	ID              string `json:"id"`
	Object          string `json:"object"`
	Created         int64  `json:"created"`
	OwnedBy         string `json:"owned_by"`
	Name            string `json:"display_name,omitempty"`
	ContextLength   int64  `json:"context_length,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
	SupportsImages  bool   `json:"supports_images,omitempty"`
	Provider        string `json:"provider,omitempty"`
}

// UpstreamModel 是上游 /v3/config 返回的模型条目。
type UpstreamModel struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	MaxInputTokens int64  `json:"maxInputTokens"`
	MaxOutputTok   int64  `json:"maxOutputTokens"`
	SupportsImages bool   `json:"supportsImages"`
}

// APIError 表示上游业务层错误（HTTP 200 但 code != 0）。
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("上游返回 code=%d msg=%s", e.Code, e.Msg)
}

// Client 是某一个上游服务的客户端。
type Client struct {
	profile  config.Profile
	platform string
	product  string
	http     *http.Client
}

// NewClient 按上游配置创建客户端。proxy 为空时跟随系统环境变量。
func NewClient(p config.Profile, platform, product string) (*Client, error) {
	if platform == "" {
		platform = config.DefaultPlatform
	}
	if product == "" {
		product = config.DefaultProduct
	}
	transport, err := newTransport(p.Proxy)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	return &Client{
		profile:  p,
		platform: platform,
		product:  product,
		http: &http.Client{
			Transport: transport,
			Jar:       jar,
		},
	}, nil
}

func newTransport(proxyRaw string) (*http.Transport, error) {
	t := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 20 * time.Second,
		// 只限制「等到响应头」的时间：上游挂住时不会无限等待，
		// 而已经开始下发的 SSE 流不受影响（这条超时不作用于响应体读取）。
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if strings.TrimSpace(proxyRaw) == "" {
		t.Proxy = http.ProxyFromEnvironment
		return t, nil
	}
	u, err := url.Parse(strings.TrimSpace(proxyRaw))
	if err != nil {
		return nil, fmt.Errorf("代理地址 %q 无效: %w", proxyRaw, err)
	}
	t.Proxy = http.ProxyURL(u)
	return t, nil
}

func (c *Client) endpoint(path string) string {
	return c.profile.BaseURL + path
}

func (c *Client) isolatedClient(timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: timeout, Transport: c.http.Transport, Jar: jar}
}

// -----------------------------------------------------------------------------
// HTTP 基础
// -----------------------------------------------------------------------------

func (c *Client) commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", c.profile.Origin)
	req.Header.Set("Referer", c.profile.Origin+"/")
	req.Header.Set("User-Agent", c.profile.UserAgent)
}

// backendHeaders 按上游约定补齐账号上下文；空字段用 X-No-* 显式声明。
func (c *Client) backendHeaders(req *http.Request, cred *Credential) {
	c.commonHeaders(req)
	if cred.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if cred.Account.UID != "" {
		req.Header.Set("X-User-Id", cred.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if cred.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if cred.Auth.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", cred.Auth.RefreshToken)
	}
	if cred.Auth.Domain != "" {
		req.Header.Set("X-Domain", cred.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", c.product)
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// call 发送请求并解析 {code,msg,data} 信封，返回 data 部分。
func (c *Client) call(ctx context.Context, hc *http.Client, method, rawURL string, hdr func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, err
	}
	if hdr != nil {
		hdr(req)
	} else {
		c.commonHeaders(req)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("上游 HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &APIError{Code: env.Code, Msg: env.Msg}
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// 登录
// -----------------------------------------------------------------------------

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// LoginSession 是一次进行中的登录流程。
//
// 登录必须复用同一个 Cookie Jar：上游把浏览器登录会话与 auth/state 下发的
// state 绑定，换客户端会导致轮询拿不到令牌。
type LoginSession struct {
	State     string
	AuthURL   string
	ExpiresAt time.Time

	client *http.Client
	owner  *Client
}

// StartLogin 发起登录，返回需要用户在浏览器打开的授权链接。
func (c *Client) StartLogin(ctx context.Context) (*LoginSession, error) {
	hc := c.isolatedClient(30 * time.Second)
	loginCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	raw, _, err := c.call(loginCtx, hc, http.MethodPost,
		c.endpoint("/v2/plugin/auth/state?platform="+url.QueryEscape(c.platform)),
		nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("发起登录失败: %w", err)
	}
	var st authStateData
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("登录响应解析失败: %w", err)
	}
	if st.State == "" || st.AuthURL == "" {
		return nil, errors.New("上游未返回登录链接（state / authUrl 为空）")
	}
	return &LoginSession{
		State:     st.State,
		AuthURL:   st.AuthURL,
		ExpiresAt: time.Now().Add(5 * time.Minute),
		client:    hc,
		owner:     c,
	}, nil
}

// EmbeddedAuthURL 返回可嵌入应用内 iframe 的授权链接。
//
// 官方登录页原生支持 embed=iframe 模式：登录完成后会向 parent_origin
// postMessage 通知结果（login_success / login_fail）。token 本身仍由
// Poll 轮询获取，postMessage 只用于应用内及时感知并关闭授权窗口。
func (s *LoginSession) EmbeddedAuthURL(parentOrigin string) string {
	u, err := url.Parse(s.AuthURL)
	if err != nil {
		return s.AuthURL
	}
	q := u.Query()
	q.Set("embed", "iframe")
	if parentOrigin != "" {
		q.Set("parent_origin", parentOrigin)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Poll 轮询一次登录结果。
//
// 返回 (nil, nil) 表示用户尚未完成登录，应继续轮询。
func (s *LoginSession) Poll(ctx context.Context) (*Credential, error) {
	if time.Now().After(s.ExpiresAt) {
		return nil, ErrLoginTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// auth/token 是权威的登录状态接口：未完成时返回非 0 code（11217 login ing）。
	raw, _, err := s.owner.call(reqCtx, s.client, http.MethodGet,
		s.owner.endpoint("/v2/plugin/auth/token?state="+url.QueryEscape(s.State)), nil, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return nil, nil // 业务层 code != 0，视为等待中
		}
		return nil, nil // 网络抖动同样按等待处理，由调用方控制重试节奏
	}
	var tok tokenData
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, nil
	}

	// 拿到令牌后再取账号信息；login/account 在登录完成前会被网关拒绝。
	var acct accountData
	acctRaw, _, errAcct := s.owner.call(reqCtx, s.client, http.MethodGet,
		s.owner.endpoint("/v2/plugin/login/account?state="+url.QueryEscape(s.State)),
		func(r *http.Request) {
			s.owner.commonHeaders(r)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}, nil)
	if errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	cred := &Credential{
		Provider: s.owner.profile.ID,
		Auth: Tokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       tok.Domain,
		},
		Account: Account{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	if tok.ExpiresIn <= 0 {
		cred.Auth.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
	}
	s.owner.Finalize(cred)
	return cred, nil
}

// Finalize 补齐凭据的 ID / 展示名等派生字段。
func (c *Client) Finalize(cred *Credential) {
	cred.Provider = c.profile.ID
	uid := strings.TrimSpace(cred.Account.UID)
	if uid == "" {
		uid = shortHash(cred.Auth.RefreshToken)
	}
	cred.ID = c.profile.ID + "-" + uid
	if cred.Label == "" {
		cred.Label = c.profile.Name + " · " + uid
	}
}

// Refresh 用 refreshToken 换取新的访问令牌，失败时返回错误。
func (c *Client) Refresh(ctx context.Context, cred *Credential) error {
	if strings.TrimSpace(cred.Auth.RefreshToken) == "" {
		return errors.New("凭据缺少 refreshToken，请重新登录")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	raw, _, err := c.call(reqCtx, c.http, http.MethodPost,
		c.endpoint("/v2/plugin/auth/token/refresh"),
		func(r *http.Request) {
			c.commonHeaders(r)
			r.Header.Set("X-Refresh-Token", cred.Auth.RefreshToken)
			if cred.Account.EnterpriseID != "" {
				r.Header.Set("X-Enterprise-Id", cred.Account.EnterpriseID)
			}
			r.Header.Set("X-Auth-Refresh-Source", c.profile.ID)
		}, nil)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}
	var tok tokenData
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return errors.New("刷新令牌失败: 上游未返回 accessToken")
	}
	cred.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		cred.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		cred.Auth.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		cred.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	cred.UpdatedAt = time.Now()
	return nil
}

// EnsureToken 在令牌即将过期时自动刷新。
func (c *Client) EnsureToken(ctx context.Context, cred *Credential) error {
	if strings.TrimSpace(cred.Auth.AccessToken) == "" {
		return ErrNoAccessToken
	}
	if !cred.NeedsRefresh() {
		return nil
	}
	return c.Refresh(ctx, cred)
}

// -----------------------------------------------------------------------------
// 模型同步
// -----------------------------------------------------------------------------

type v3ConfigResponse struct {
	Code int `json:"code"`
	Data struct {
		Models []UpstreamModel `json:"models"`
	} `json:"data"`
}

// FetchUpstreamModels 从 /v3/config 拉取模型列表。
func (c *Client) FetchUpstreamModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.endpoint("/v3/config"), nil)
	if err != nil {
		return nil, err
	}
	c.backendHeaders(req, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型配置失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取模型配置失败: 上游 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var cfg v3ConfigResponse
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("模型配置解析失败: %w", err)
	}
	return cfg.Data.Models, nil
}

// BuildModelList 把内置白名单与上游配置合并成最终模型列表。
//
// 白名单始终保留（部分模型可调用但不在 /v3/config 中），上游配置只补充
// 展示名与上下文长度。
func (c *Client) BuildModelList(extra, disabled []string, upstreamModels []UpstreamModel) []ModelInfo {
	upstreamIndex := make(map[string]UpstreamModel, len(upstreamModels))
	for _, m := range upstreamModels {
		if m.ID != "" {
			upstreamIndex[m.ID] = m
		}
	}
	blocked := make(map[string]bool, len(disabled))
	for _, id := range disabled {
		blocked[strings.TrimSpace(id)] = true
	}

	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(Allowlist(c.profile.ID))+len(extra))
	now := time.Now().Unix()

	// spec 是内置白名单里的记录（可能为空）：上游 /v3/config 常常漏掉一部分
	// 实际可调用的模型，这时必须以白名单里的名称与上下文/输出上限为准，
	// 否则界面和 /v1/models 会显示成 0 与默认值。
	appendModel := func(id string, spec ModelSpec) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || blocked[id] {
			return
		}
		seen[id] = true
		name := spec.Name
		ctxLen := spec.ContextLength
		maxOut := spec.MaxOutput
		supportsImages := false
		if up, ok := upstreamIndex[id]; ok {
			if up.Name != "" {
				name = up.Name
			}
			if up.MaxInputTokens > 0 {
				ctxLen = up.MaxInputTokens
			}
			if up.MaxOutputTok > 0 {
				maxOut = up.MaxOutputTok
			}
			supportsImages = up.SupportsImages
		}
		if name == "" {
			name = id
		}
		if maxOut <= 0 {
			maxOut = defaultMaxOutput
		}
		out = append(out, ModelInfo{
			ID:              id,
			Object:          "model",
			Created:         now,
			OwnedBy:         c.profile.ID,
			Name:            name,
			ContextLength:   ctxLen,
			MaxOutputTokens: maxOut,
			SupportsImages:  supportsImages,
			Provider:        c.profile.ID,
		})
	}

	for _, spec := range Allowlist(c.profile.ID) {
		appendModel(spec.ID, spec)
	}
	for _, id := range extra {
		appendModel(id, ModelSpec{})
	}
	// 上游 /v3/config 新增、白名单尚未收录的模型也要透出，
	// 否则上游上新后要等程序发版才能用。
	for _, m := range upstreamModels {
		appendModel(m.ID, ModelSpec{})
	}
	return out
}

// -----------------------------------------------------------------------------
// 对话转发
// -----------------------------------------------------------------------------

// Chat 把请求转发到上游 /v2/chat/completions。调用方负责关闭响应体。
func (c *Client) Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/v2/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.backendHeaders(req, cred)
	return c.http.Do(req)
}

// -----------------------------------------------------------------------------
// 工具函数
// -----------------------------------------------------------------------------

func shortHash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
