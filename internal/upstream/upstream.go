// Package upstream 实现对各上游服务商协议的调用：登录、令牌刷新、模型
// 同步与对话转发。本文件承载 WorkBuddy 协议族的实现与统一的凭据/分发层；
// 其它协议族由子包适配：
//
//	workbuddy / codebuddy（本文件，与官方 CLI 插件共用同一套协议）：
//	  POST {base}/v2/plugin/auth/state?platform=CLI     发起登录，返回 state 与 authUrl
//	  GET  {base}/v2/plugin/auth/token?state=<state>    轮询登录结果（未完成时返回 code 11217）
//	  GET  {base}/v2/plugin/login/account?state=<state> 登录完成后取账号信息
//	  POST {base}/v2/plugin/auth/token/refresh          刷新访问令牌
//	  GET  {base}/v3/config                             模型与配额配置
//	  POST {base}/v2/chat/completions                   对话（仅支持流式）
//
//	lobsterai（internal/upstream/lobsterai）：浏览器回调登录 + OpenAI 形状对话。
//	trae      （internal/upstream/trae）：浏览器授权回调 + SOLO 协议翻译。
//	qoder     （internal/upstream/qoder）：PKCE 设备登录 + wasm COSY 签名对话。
//	opencode  （internal/upstream/opencode）：API Key 登录 + OpenAI 形状对话，
//	          连线时改写 User-Agent 与 x-opencode-session（官方网关校验）。
//
// Client 按 Profile.Kind 把六个协议动作（StartLogin/Poll/Refresh/
// EnsureToken/FetchUpstreamModels/Chat）分发到对应实现；所有对话路径在
// 适配包边界上统一翻译成 OpenAI SSE，服务层与 Failover 逻辑对上游无感知。
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
	"unicode/utf8"

	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/upstream/lobsterai"
	"github.com/znsoftm/RapidProxy/internal/upstream/opencode"
	"github.com/znsoftm/RapidProxy/internal/upstream/qoder"
	"github.com/znsoftm/RapidProxy/internal/upstream/trae"
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
//
// 前四个字段是 WorkBuddy 协议族；其余字段服务于新协议族（按 Kind 取用），
// 均为 omitempty，对既有凭据文件完全向后兼容。
type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	// UUID / FirstKeyfrom 是 LobsterAI 的续期身份对（随凭据持久化，
	// 刷新时必须回传）。
	UUID         string `json:"uuid,omitempty"`
	FirstKeyfrom string `json:"firstKeyfrom,omitempty"`
	// MachineID / DeviceID 是 Trae 的登录设备对（refresh token 系于其上，
	// 永不轮换）；MachineID 同时是 Qoder 的每安装设备 id。
	MachineID string `json:"machineId,omitempty"`
	DeviceID  string `json:"deviceId,omitempty"`
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
	// Weight 是同组（同一上游）内负载均衡的权重；
	// 未设置或 <=0 时按 1 处理，即组内默认同权。
	Weight int `json:"weight,omitempty"`
	// Paused 为 true 时该账号暂不参与转发，但不删除、保留凭据。
	Paused bool `json:"paused,omitempty"`
}

// EffectiveWeight 返回参与负载均衡的权重，未设置（<=0）时为 1。
func (c *Credential) EffectiveWeight() int {
	if c.Weight > 0 {
		return c.Weight
	}
	return 1
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
//
// 请求的收发与凭据管理对上游统一；协议差异（登录、刷新、模型目录、对话
// 线路）按 kind 分发到对应适配包。WorkBuddy 协议族（workbuddy / codebuddy）
// 的实现保留在本文件内。
type Client struct {
	profile  config.Profile
	kind     string
	platform string
	product  string
	http     *http.Client
	// traep / qoderp 是按 Edition 选定的区配置；仅对应协议族使用。
	traep  trae.Profile
	qoderp qoder.Profile
}

// ProfileKind 返回上游的协议族；配置未显式指定时按 ID 推断（兼容旧配置）。
func ProfileKind(p config.Profile) string {
	if k := strings.TrimSpace(p.Kind); k != "" {
		return k
	}
	switch p.ID {
	case "lobsterai":
		return KindLobsterAI
	case "trae-cn", "trae-global":
		return KindTrae
	case "qoder-cn", "qoder-global":
		return KindQoder
	case "opencode":
		return KindOpenCode
	default:
		return KindWorkBuddy
	}
}

// 协议族常量与 config 侧保持一致（避免 config ↔ upstream 循环依赖，
// 两边各自定义，值必须相同）。
const (
	KindWorkBuddy = "workbuddy"
	KindLobsterAI = "lobsterai"
	KindTrae      = "trae"
	KindQoder     = "qoder"
	KindOpenCode  = "opencode"
)

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
	kind := ProfileKind(p)
	c := &Client{
		profile:  p,
		kind:     kind,
		platform: platform,
		product:  product,
		http: &http.Client{
			Transport: transport,
			Jar:       jar,
		},
	}
	switch kind {
	case KindTrae:
		if p.Edition == config.EditionGlobal {
			c.traep = trae.GlobalProfile()
		} else {
			c.traep = trae.CNProfile()
		}
	case KindQoder:
		if p.Edition == config.EditionGlobal {
			c.qoderp = qoder.GlobalProfile()
		} else {
			c.qoderp = qoder.CNProfile()
		}
	}
	return c, nil
}

// Kind 返回客户端的协议族。
func (c *Client) Kind() string { return c.kind }

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
// WorkBuddy 协议族：登录必须复用同一个 Cookie Jar——上游把浏览器登录会话
// 与 auth/state 下发的 state 绑定，换客户端会导致轮询拿不到令牌。
// 新协议族（lobsterai / trae / qoder）通过 pollFn/cancelFn/embedFn 注入各自
// 的实现：本地回调监听或远端 PKCE 轮询。
type LoginSession struct {
	State     string
	AuthURL   string
	ExpiresAt time.Time

	// WorkBuddy 协议族专用。
	client *http.Client
	owner  *Client

	// 新协议族的注入实现。
	pollFn   func(ctx context.Context) (*Credential, error)
	cancelFn func()
	embedFn  func(parentOrigin string) string

	// NeedKey 表示这次登录等待用户在界面上粘贴密钥（OpenCode 的 API Key
	// 制登录），而非浏览器授权回调。
	NeedKey bool
	// SkipBrowser 表示不必在系统浏览器打开 AuthURL——本机已能找到凭据
	// （如导入 OpenCode CLI 保存的密钥），打开网页只会让人困惑。
	SkipBrowser bool
}

// StartLogin 发起登录，返回需要用户在浏览器打开的授权链接。
func (c *Client) StartLogin(ctx context.Context) (*LoginSession, error) {
	switch c.kind {
	case KindLobsterAI:
		return c.startLobsterLogin(ctx)
	case KindTrae:
		return c.startTraeLogin(ctx)
	case KindQoder:
		return c.startQoderLogin(ctx)
	case KindOpenCode:
		return c.startOpenCodeLogin(ctx)
	default:
		return c.startWorkBuddyLogin(ctx)
	}
}

// startWorkBuddyLogin 是 WorkBuddy 协议族的登录实现。
func (c *Client) startWorkBuddyLogin(ctx context.Context) (*LoginSession, error) {
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

// startLobsterLogin 发起 LobsterAI 登录：绑定本地回调监听，浏览器完成授权
// 后回调带回授权码，再由 /api/auth/exchange 换取 token 对。
func (c *Client) startLobsterLogin(ctx context.Context) (*LoginSession, error) {
	session, err := lobsterai.StartLogin(0)
	if err != nil {
		return nil, err
	}
	return &LoginSession{
		State:     session.State,
		AuthURL:   session.LoginURL,
		ExpiresAt: time.Now().Add(lobsterai.LoginLifetime),
		cancelFn:  session.Cancel,
		embedFn:   func(string) string { return session.LoginURL },
		pollFn: func(pollCtx context.Context) (*Credential, error) {
			code, err := session.Wait(pollCtx)
			if err != nil {
				return nil, err
			}
			token, err := session.Resolve(pollCtx, c.http, code)
			if err != nil {
				return nil, err
			}
			cred := &Credential{
				Provider: c.profile.ID,
				Auth: Tokens{
					AccessToken:  token.AccessToken,
					RefreshToken: token.RefreshToken,
					ExpiresAt:    token.ExpiresAt,
					UUID:         token.UUID,
					FirstKeyfrom: token.FirstKeyfrom,
				},
				Account: Account{
					UID:      token.UserID,
					Nickname: token.Nickname,
				},
			}
			if cred.Auth.ExpiresAt <= 0 {
				// 上游未报告过期时间：按 2 小时保守估计，401 时仍会刷新重试。
				cred.Auth.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
			}
			c.Finalize(cred)
			return cred, nil
		},
	}, nil
}

// startTraeLogin 发起 Trae 登录：本地回调监听（含 17388 探测口应答），
// 授权回调带回 refreshToken，经旧契约换取 JWT；设备对随凭据保存。
func (c *Client) startTraeLogin(ctx context.Context) (*LoginSession, error) {
	session, loginURL, err := trae.StartLogin(c.traep, trae.CallbackPort)
	if err != nil {
		return nil, err
	}
	return &LoginSession{
		State:     session.TraceID,
		AuthURL:   loginURL,
		ExpiresAt: time.Now().Add(trae.LoginLifetime),
		cancelFn:  session.Cancel,
		embedFn:   func(string) string { return loginURL },
		pollFn: func(pollCtx context.Context) (*Credential, error) {
			details, err := session.Wait(pollCtx)
			if err != nil {
				return nil, err
			}
			token, err := session.Resolve(pollCtx, c.http, c.traep, details)
			if err != nil {
				return nil, err
			}
			// 显示名尽量补齐：回调没带账号明细时读一次用户信息（尽力而为）。
			if strings.TrimSpace(token.DisplayName) == "" {
				if _, name, _, uiErr := trae.GetUserInfo(pollCtx, c.http, c.traep, token); uiErr == nil && name != "" {
					token.DisplayName = name
				}
			}
			cred := &Credential{
				Provider: c.profile.ID,
				Auth: Tokens{
					AccessToken:  token.AccessToken,
					RefreshToken: token.RefreshToken,
					ExpiresAt:    token.ExpiresAt,
					MachineID:    token.MachineID,
					DeviceID:     token.DeviceID,
				},
				Account: Account{
					UID:          token.UserID,
					EnterpriseID: token.EnterpriseID,
					Nickname:     token.DisplayName,
				},
			}
			if cred.Auth.ExpiresAt <= 0 {
				// 上游沉默时按 2 小时保守估计：NeedsRefresh 依赖这个值做
				// 主动刷新，否则只剩 401 兜底重试（与 lobsterai /
				// workbuddy 协议族的登录路径一致）。
				cred.Auth.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
			}
			c.Finalize(cred)
			return cred, nil
		},
	}, nil
}

// startQoderLogin 发起 Qoder 设备登录：浏览器打开 PKCE 授权页，这里按
// 轮询节奏调用 Poll 直到批准落地（404 = 等待中）。
func (c *Client) startQoderLogin(ctx context.Context) (*LoginSession, error) {
	login, err := qoder.StartLogin(qoder.MachineID())
	if err != nil {
		return nil, err
	}
	loginURL := login.AuthURL(c.qoderp)
	return &LoginSession{
		State:     login.Nonce(),
		AuthURL:   loginURL,
		ExpiresAt: time.Now().Add(qoder.LoginLifetime()),
		embedFn:   func(string) string { return loginURL },
		pollFn: func(pollCtx context.Context) (*Credential, error) {
			token, err := qoder.Poll(pollCtx, c.http, c.qoderp, login)
			if err != nil {
				// 404 = 浏览器端批准尚未落地，继续等待。
				if errors.Is(err, qoder.ErrPending) {
					return nil, nil
				}
				return nil, err
			}
			cred := &Credential{
				Provider: c.profile.ID,
				Auth: Tokens{
					AccessToken:  token.AccessToken,
					RefreshToken: token.RefreshToken,
					ExpiresAt:    token.ExpiresAt,
					MachineID:    qoder.MachineID(),
				},
				Account: Account{
					UID:      token.UserID,
					Nickname: token.UserName,
				},
			}
			c.Finalize(cred)
			return cred, nil
		},
	}, nil
}

// startOpenCodeLogin 发起 OpenCode 登录。Zen 是 API Key 制，没有浏览器授权
// 回调：优先导入本机 OpenCode CLI 已保存的密钥（auth.json / OPENCODE_API_KEY）；
// 找不到、或本机密钥形态不对，就打开官方密钥页，等用户在界面上粘贴（由
// App.SubmitLoginKey 收口，这里只占位等待，超时由 ExpiresAt 收口）。
func (c *Client) startOpenCodeLogin(ctx context.Context) (*LoginSession, error) {
	if raw := opencode.LocalKey(); raw != "" {
		// 本机密钥同样要过清洗：环境变量或 auth.json 里存的可能是带 Bearer
		// 前缀、引号甚至整段 JSON 的值，原样存进去就是一个坏账号。
		if key, err := opencode.NormalizeKey(raw); err == nil {
			return c.openCodeLocalSession(key), nil
		}
		// 形态不对（粘贴噪声、无法识别的格式）：按粘贴流程走，让用户手动换一把。
	}
	return &LoginSession{
		State:     "paste",
		AuthURL:   opencode.AuthURL,
		ExpiresAt: time.Now().Add(10 * time.Minute),
		NeedKey:   true,
		pollFn: func(pollCtx context.Context) (*Credential, error) {
			return nil, nil
		},
	}, nil
}

// openCodeLocalSession 用本机导入的密钥构造「直接确认」的登录会话。
//
// 密钥在第一次轮询时过一次上游校验（跑在轮询 goroutine 上，不阻塞发起登录
// 的调用方）：被 401/403 明确拒绝时 Poll 返回 ErrKeyRejected，由
// App.pollLogin 把流程转成粘贴模式——否则一把坏密钥会静默落库，而用户不删掉
// 本机 auth.json 就连粘贴框都等不到（本地导入永远优先）。联机类错误
// （超时、DNS、5xx）放行保存，让运行时暴露真实问题。
//
// NeedKey 同时置位：导入本机密钥不等于用户就想用它，粘贴框常驻可供覆盖
// （SkipBrowser 仍然为真——不替用户打开浏览器）。pollFn 的占位守卫认的是
// State=="paste"（被拒后由 App 翻转），本地导入本身不受 NeedKey 影响。
func (c *Client) openCodeLocalSession(key string) *LoginSession {
	cred := c.openCodeCredential(key)
	session := &LoginSession{
		State:       "local",
		AuthURL:     opencode.AuthURL,
		ExpiresAt:   time.Now().Add(10 * time.Minute),
		NeedKey:     true,
		SkipBrowser: true,
	}
	session.pollFn = func(pollCtx context.Context) (*Credential, error) {
		if session.NeedKey && session.State == "paste" {
			// 已因密钥被拒转成粘贴模式：占位等 SubmitLoginKey，不再重复校验。
			return nil, nil
		}
		if err := c.VerifyOpenCodeKey(pollCtx, key); err != nil {
			if errors.Is(err, opencode.ErrKeyRejected) {
				return nil, err
			}
			// 联机类错误不判定密钥无效。
		}
		return cred, nil
	}
	return session
}

// openCodeCredential 用 Zen API Key 构造凭据。密钥制、不过期（ExpiresAt 为
// 零值，NeedsRefresh 不会触发刷新）；ID 取密钥短哈希，同一密钥重复导入会
// 更新同一份凭据文件，而不是堆积多份。
func (c *Client) openCodeCredential(key string) *Credential {
	key = strings.TrimSpace(key)
	cred := &Credential{
		Provider: c.profile.ID,
		Auth:     Tokens{AccessToken: key},
		Account:  Account{UID: shortHash(key), Nickname: opencode.KeyLabel(key)},
	}
	c.Finalize(cred)
	return cred
}

// OpenCodeCredential 用 Zen API Key 构造一份可保存的 OpenCode 凭据
// （供界面粘贴密钥时使用）。
func (c *Client) OpenCodeCredential(key string) *Credential {
	return c.openCodeCredential(key)
}

// ErrOpenCodeKeyInvalid 是 OpenCode 密钥格式错误的应用层哨兵：由
// opencode.ErrKeyInvalid 透传，供界面映射成本地化文案。
var ErrOpenCodeKeyInvalid = opencode.ErrKeyInvalid

// ErrOpenCodeKeyRejected 是 OpenCode 密钥被上游拒绝（401/403）的应用层
// 哨兵：格式没问题但网关不认，供界面给出针对性提示。
var ErrOpenCodeKeyRejected = opencode.ErrKeyRejected

// NormalizeOpenCodeKey 清洗用户粘贴的 OpenCode 密钥：去掉引号、JSON 包装和
// Bearer 前缀，并做最基本的合法性校验。非 OpenCode 上游调用会返回错误，
// 避免把别家的密钥格式套到这里。
func (c *Client) NormalizeOpenCodeKey(key string) (string, error) {
	if ProfileKind(c.profile) != KindOpenCode {
		return "", fmt.Errorf("上游 %s 不使用 API Key 登录", c.profile.Name)
	}
	return opencode.NormalizeKey(key)
}

// VerifyOpenCodeKey 用一次模型目录请求校验密钥是否被上游接受。
//
// 只在登录粘贴时调用：错键立刻报错，好过存进凭据库、等到首次对话才以
// 「认证失败」的形式暴露。联机类错误（超时、DNS、5xx）原样返回但不视为
// 密钥无效——调用方应放行保存，让运行时再暴露真实问题。
func (c *Client) VerifyOpenCodeKey(ctx context.Context, key string) error {
	if ProfileKind(c.profile) != KindOpenCode {
		return fmt.Errorf("上游 %s 不使用 API Key 登录", c.profile.Name)
	}
	normalized, err := opencode.NormalizeKey(key)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = opencode.ListModels(ctx, c.http, c.profile.BaseURL, normalized, c.profile.UserAgent)
	return err
}

// EmbeddedAuthURL 返回可嵌入应用内 iframe 的授权链接。
//
// 官方登录页原生支持 embed=iframe 模式：登录完成后会向 parent_origin
// postMessage 通知结果（login_success / login_fail）。token 本身仍由
// Poll 轮询获取，postMessage 只用于应用内及时感知并关闭授权窗口。
// 新协议族不支持内嵌授权，返回原始链接。
func (s *LoginSession) EmbeddedAuthURL(parentOrigin string) string {
	if s.embedFn != nil {
		return s.embedFn(parentOrigin)
	}
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

// Cancel 取消登录流程（停止本地监听等资源）。WorkBuddy 协议族无需清理。
func (s *LoginSession) Cancel() {
	if s.cancelFn != nil {
		s.cancelFn()
	}
}

// Poll 轮询一次登录结果。
//
// 返回 (nil, nil) 表示用户尚未完成登录，应继续轮询。
func (s *LoginSession) Poll(ctx context.Context) (*Credential, error) {
	if time.Now().After(s.ExpiresAt) {
		return nil, ErrLoginTimeout
	}
	if s.pollFn != nil {
		return s.pollFn(ctx)
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
// 各协议族的刷新端点与身份字段不同，按 kind 分发。
func (c *Client) Refresh(ctx context.Context, cred *Credential) error {
	switch c.kind {
	case KindLobsterAI:
		return c.refreshLobster(ctx, cred)
	case KindTrae:
		return c.refreshTrae(ctx, cred)
	case KindQoder:
		return c.refreshQoder(ctx, cred)
	case KindOpenCode:
		// Zen 是 API Key 制、不过期：没有刷新端点。EnsureToken 也因
		// ExpiresAt 为零值而不会走到这里，保留分支只为语义完整。
		return nil
	default:
		return c.refreshWorkBuddy(ctx, cred)
	}
}

// refreshWorkBuddy 是 WorkBuddy 协议族的刷新实现。
func (c *Client) refreshWorkBuddy(ctx context.Context, cred *Credential) error {
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

// refreshLobster 刷新 LobsterAI 令牌。uuid/firstKeyfrom 身份对随凭据回传；
// 新值缺失时保留旧值。
func (c *Client) refreshLobster(ctx context.Context, cred *Credential) error {
	token, err := lobsterai.Refresh(ctx, c.http, cred.Auth.RefreshToken,
		cred.Auth.UUID, cred.Auth.FirstKeyfrom, cred.Account.UID)
	if err != nil {
		return err
	}
	cred.Auth.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		cred.Auth.RefreshToken = token.RefreshToken
	}
	if token.ExpiresAt > 0 {
		cred.Auth.ExpiresAt = token.ExpiresAt
	}
	if cred.Auth.UUID == "" {
		cred.Auth.UUID = token.UUID
	}
	if cred.Auth.FirstKeyfrom == "" {
		cred.Auth.FirstKeyfrom = token.FirstKeyfrom
	}
	cred.UpdatedAt = time.Now()
	return nil
}

// refreshTrae 刷新 Trae 令牌。双 token 在服务端轮换；登录设备对永不轮换。
func (c *Client) refreshTrae(ctx context.Context, cred *Credential) error {
	token, err := trae.Refresh(ctx, c.http, c.traep, cred.Auth.RefreshToken,
		cred.Auth.MachineID, cred.Auth.DeviceID)
	if err != nil {
		return err
	}
	cred.Auth.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		cred.Auth.RefreshToken = token.RefreshToken
	}
	if token.ExpiresAt > 0 {
		cred.Auth.ExpiresAt = token.ExpiresAt
	}
	// 设备对以凭据里存的那一对为准（token 家族 mint 时所系）。
	if cred.Auth.MachineID == "" {
		cred.Auth.MachineID = token.MachineID
	}
	if cred.Auth.DeviceID == "" {
		cred.Auth.DeviceID = token.DeviceID
	}
	cred.UpdatedAt = time.Now()
	return nil
}

// refreshQoder 刷新 Qoder 设备令牌。machine_id 使用本机稳定 id。
func (c *Client) refreshQoder(ctx context.Context, cred *Credential) error {
	token, err := qoder.Refresh(ctx, c.http, c.qoderp, cred.Auth.RefreshToken, qoder.MachineID())
	if err != nil {
		return err
	}
	cred.Auth.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		cred.Auth.RefreshToken = token.RefreshToken
	}
	if token.ExpiresAt > 0 {
		cred.Auth.ExpiresAt = token.ExpiresAt
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

// FetchUpstreamModels 拉取上游模型列表。各协议族的目录端点不同，按 kind 分发。
func (c *Client) FetchUpstreamModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
	switch c.kind {
	case KindLobsterAI:
		return c.fetchLobsterModels(ctx, cred)
	case KindTrae:
		return c.fetchTraeModels(ctx, cred)
	case KindQoder:
		return c.fetchQoderModels(ctx, cred)
	case KindOpenCode:
		return c.fetchOpenCodeModels(ctx, cred)
	default:
		return c.fetchWorkBuddyModels(ctx, cred)
	}
}

// fetchWorkBuddyModels 从 /v3/config 拉取模型列表。
func (c *Client) fetchWorkBuddyModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
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

// fetchLobsterModels 读取 LobsterAI 目录。
func (c *Client) fetchLobsterModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
	entries, err := lobsterai.ListModels(ctx, c.http, cred.Auth.AccessToken)
	if err != nil {
		return nil, err
	}
	models := make([]UpstreamModel, 0, len(entries))
	for _, e := range entries {
		models = append(models, UpstreamModel{ID: e.ID, Name: e.Name})
	}
	return models, nil
}

// fetchTraeModels 读取 SOLO 目录（并发询问全部对话池取并集）。
func (c *Client) fetchTraeModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
	entries, err := trae.ListModels(ctx, c.http, c.traep, cred.Auth.AccessToken,
		cred.Auth.MachineID, cred.Auth.DeviceID, cred.Account.UID)
	if err != nil {
		return nil, err
	}
	models := make([]UpstreamModel, 0, len(entries))
	for _, e := range entries {
		models = append(models, UpstreamModel{ID: e.ID, Name: e.DisplayName})
	}
	return models, nil
}

// fetchQoderModels 读取 Qoder 目录（签名契约）。
func (c *Client) fetchQoderModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
	entries, _, err := qoder.ListModels(ctx, c.http, c.qoderp, cred.Account.UID, cred.Auth.AccessToken)
	if err != nil {
		return nil, err
	}
	models := make([]UpstreamModel, 0, len(entries))
	for _, e := range entries {
		models = append(models, UpstreamModel{
			ID:             e.Key,
			Name:           e.DisplayName,
			MaxInputTokens: int64(e.MaxInput),
			MaxOutputTok:   int64(e.MaxOutput),
			SupportsImages: e.Vision,
		})
	}
	return models, nil
}

// fetchOpenCodeModels 读取 OpenCode Zen 目录（OpenAI 兼容形状）。
func (c *Client) fetchOpenCodeModels(ctx context.Context, cred *Credential) ([]UpstreamModel, error) {
	entries, err := opencode.ListModels(ctx, c.http, c.profile.BaseURL, cred.Auth.AccessToken, c.profile.UserAgent)
	if err != nil {
		return nil, err
	}
	models := make([]UpstreamModel, 0, len(entries))
	for _, e := range entries {
		models = append(models, UpstreamModel{ID: e.ID, Name: e.Name})
	}
	return models, nil
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

// Chat 把请求转发到上游对话端点。调用方负责关闭响应体。
//
// 返回的响应（HTTP 200 时）是标准 OpenAI SSE——协议差异（SOLO 翻译、
// Qoder 签名信封、LobsterAI 流首错误过滤）由各适配包在边界上翻译掉，
// 服务层与 Failover 逻辑对上游无感知。
func (c *Client) Chat(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
	switch c.kind {
	case KindLobsterAI:
		return lobsterai.Chat(ctx, c.http, cred.Auth.AccessToken, body)
	case KindTrae:
		return trae.Chat(ctx, c.http, c.traep, cred.Auth.AccessToken,
			cred.Auth.MachineID, cred.Auth.DeviceID, cred.Account.UID, body)
	case KindQoder:
		return qoder.Chat(ctx, c.http, c.qoderp, cred.Account.UID, cred.Auth.AccessToken, body)
	case KindOpenCode:
		// 设置里保存的 User-Agent 是裸 "opencode"；适配层只在连往
		// OpenCode 主机时改写成 opencode/1.18.31 并带上 x-opencode-session。
		return opencode.Chat(ctx, c.http, c.profile.BaseURL, cred.Auth.AccessToken, c.profile.UserAgent, body)
	default:
		return c.chatWorkBuddy(ctx, cred, body)
	}
}

// chatWorkBuddy 把请求转发到上游 /v2/chat/completions。
func (c *Client) chatWorkBuddy(ctx context.Context, cred *Credential, body []byte) (*http.Response, error) {
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

// truncate 按字符数截断字符串，用于把上游错误正文压成一行。
//
// 必须按 rune 而不是字节切：上游返回的多半是中文错误提示，
// 按字节截会在中间切断 UTF-8 序列，界面上直接显示成乱码。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
