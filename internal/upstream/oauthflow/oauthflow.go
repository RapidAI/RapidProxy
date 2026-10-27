// Package oauthflow 实现各家上游共用的 OAuth 基础设施：PKCE 参数生成、
// 本地回调监听、授权码清洗与 token 响应解析。
//
// 只承载「与具体服务商无关」的部分。各家的授权端点、client_id、scope、
// 额外查询参数与请求体格式差异很大（OpenAI 用 form、Anthropic 用 JSON，
// 智谱是服务端托管流程），因此具体流程留在 upstream 各适配实现里。
package oauthflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// verifierChars 是 RFC 7636 允许的 unreserved 字符集 [A-Za-z0-9-._~]。
const verifierChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// verifierLength 是 code_verifier 长度；RFC 7636 要求 43-128 字符。
const verifierLength = 64

// GenerateVerifier 生成一个 64 字符的随机 code_verifier。
func GenerateVerifier() (string, error) {
	buf := make([]byte, verifierLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = verifierChars[int(buf[i])%len(verifierChars)]
	}
	return string(buf), nil
}

// Challenge 计算 code_challenge：SHA256 后 base64url 无填充（S256）。
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// GenerateState 生成 32 字节随机 state（base64url 无填充）。
func GenerateState() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败在桌面端等同于系统故障；用时间兜底也比空 state 好。
		return fmt.Sprintf("s%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// RandomHex 返回 n 字节随机数的十六进制串（智谱的 poll token 是 32 字节）。
func RandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ErrCancelled 表示用户在授权完成前取消了登录。
var ErrCancelled = errors.New("登录已取消")

// NormalizeCode 清洗用户粘贴的授权码：既接受裸 code，也接受整段回调 URL
// 或 "code#state" 形态（Anthropic 的回调页就是 code#state）。
func NormalizeCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.Index(raw, "code="); idx >= 0 {
		if u, err := url.Parse(raw); err == nil {
			if code := strings.TrimSpace(u.Query().Get("code")); code != "" {
				return code
			}
		}
		if values, err := url.ParseQuery(raw); err == nil {
			if code := strings.TrimSpace(values.Get("code")); code != "" {
				return code
			}
		}
	}
	// Anthropic 的回调页显示 "code#state"。
	if idx := strings.Index(raw, "#"); idx > 0 {
		raw = raw[:idx]
	}
	return raw
}

// Callback 是一个本地回调监听器，接收 OAuth 重定向。
//
// 与浏览器授权页的交互只有一次：命中回调路径且带回 code（或 error）后
// 立即放行等待方，并把结果从 channel 交给调用方。
type Callback struct {
	listeners []net.Listener
	servers   []*http.Server
	path      string
	port      int

	result chan CallbackResult
	once   sync.Once
	stop   chan struct{}
}

// CallbackResult 是一次回调的结果。
type CallbackResult struct {
	Code  string
	State string
	Error error
}

// StartCallback 在回环地址上启动回调监听。port=0 时由系统分配；指定端口
// 被占用时向上顺延最多 8 个（ChatGPT 的 1455 常常被 Codex CLI 占着，顺延
// 让两个客户端可以共存）。
//
// 同时监听 127.0.0.1 与 ::1：浏览器把 localhost 解析成哪个是系统决定的，
// 只绑一个族会让另一族的回调连不上（Windows 上尤其常见）。
func StartCallback(path string, port int) (*Callback, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	cb := &Callback{
		path:   path,
		result: make(chan CallbackResult, 1),
		stop:   make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, cb.handle)

	// 主监听决定端口；另一族尽力绑定同一端口，失败不影响主流程。
	primary, actualPort, err := listenLoopback("127.0.0.1", port)
	if err != nil {
		return nil, err
	}
	cb.port = actualPort
	cb.listeners = append(cb.listeners, primary)
	if v6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", actualPort)); err == nil {
		cb.listeners = append(cb.listeners, v6)
	}

	servers := make([]*http.Server, 0, len(cb.listeners))
	for _, ln := range cb.listeners {
		server := &http.Server{Handler: mux}
		servers = append(servers, server)
		go func(s *http.Server, l net.Listener) {
			_ = s.Serve(l)
		}(server, ln)
	}
	cb.servers = servers
	return cb, nil
}

// listenLoopback 绑定回环地址；指定端口被占用时向上顺延。
func listenLoopback(host string, port int) (net.Listener, int, error) {
	if port <= 0 {
		ln, err := net.Listen("tcp", host+":0")
		if err != nil {
			return nil, 0, fmt.Errorf("本地回调端口无法监听: %w", err)
		}
		return ln, ln.Addr().(*net.TCPAddr).Port, nil
	}
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port+attempt))
		if err == nil {
			return ln, ln.Addr().(*net.TCPAddr).Port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("本地回调端口 %d-%d 均被占用: %w", port, port+7, lastErr)
}

// Port 返回实际监听的端口。
func (c *Callback) Port() int { return c.port }

// RedirectURI 返回可填进授权请求的 redirect_uri。
func (c *Callback) RedirectURI(host string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d%s", host, c.port, c.path)
}

// Wait 阻塞等待回调结果或上下文结束。
func (c *Callback) Wait(ctx context.Context) (CallbackResult, error) {
	select {
	case res := <-c.result:
		return res, res.Error
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	case <-c.stop:
		return CallbackResult{}, ErrCancelled
	}
}

// Stop 关闭监听器并同步释放端口。可重复调用。
//
// 必须同步关掉 listener：ChatGPT 用固定端口 1455，如果只触发异步 Shutdown，
// 紧接着的登录会绑定失败而顺延端口，多登几次端口就越漂越远。
func (c *Callback) Stop() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		close(c.stop)
		for _, ln := range c.listeners {
			_ = ln.Close()
		}
		for _, s := range c.servers {
			_ = s.Close()
		}
	})
}

func (c *Callback) handle(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	// 授权页的探活请求不带 code/error：不能拿它结束等待。
	code := strings.TrimSpace(query.Get("code"))
	errParam := strings.TrimSpace(query.Get("error"))
	if code == "" && errParam == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeResultPage(w, errParam == "")
	res := CallbackResult{Code: code, State: strings.TrimSpace(query.Get("state"))}
	if errParam != "" {
		desc := strings.TrimSpace(query.Get("error_description"))
		if desc == "" {
			desc = errParam
		}
		res.Error = fmt.Errorf("授权被拒绝: %s", desc)
	}
	select {
	case c.result <- res:
	default:
	}
}

// writeResultPage 回一个中性的结果页：授权成功/失败在应用内呈现，
// 浏览器只需要告诉用户可以关掉标签页。
func writeResultPage(w http.ResponseWriter, ok bool) {
	title := "授权成功"
	note := "请返回 RapidProxy 继续，此页面可以关闭。"
	if !ok {
		title = "授权未完成"
		note = "授权被拒绝或已取消，请回到 RapidProxy 重试。"
	}
	body := `<!doctype html><meta charset="utf-8"><title>` + title + `</title>` +
		`<body style="font-family:system-ui,'Segoe UI',sans-serif;max-width:420px;margin:64px auto;text-align:center">` +
		`<h2 style="font-weight:600;font-size:18px">` + title + `</h2>` +
		`<p style="color:#555;font-size:14px">` + note + `</p>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, body)
}

// TokenResponse 是各家 OAuth token 端点的通用响应字段。
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Expiry 返回 token 的绝对过期时间；expires_in 缺失时返回零值。
func (t TokenResponse) Expiry(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// DecodeTokenResponse 解析 token 端点响应体，并在缺少 access_token 时报错。
func DecodeTokenResponse(raw []byte) (TokenResponse, error) {
	var tok TokenResponse
	if err := decodeJSON(raw, &tok); err != nil {
		return TokenResponse{}, err
	}
	if tok.Error != "" {
		msg := tok.Error
		if tok.ErrorDesc != "" {
			msg += ": " + tok.ErrorDesc
		}
		return TokenResponse{}, errors.New(msg)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return TokenResponse{}, errors.New("token 响应缺少 access_token")
	}
	return tok, nil
}

// JWTAccountID 从 OpenAI 的 access_token 中取出 chatgpt_account_id。
//
// ChatGPT 的后端接口要求 chatgpt-account-id 头，值来自 JWT 的
// "https://api.openai.com/auth".chatgpt_account_id 声明。取不到时返回空串
// （调用方不应设置该头）。
func JWTAccountID(accessToken string) string {
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if !decodeJWTPayload(accessToken, &claims) {
		return ""
	}
	return strings.TrimSpace(claims.Auth.AccountID)
}

// JWTSubject 返回 JWT 的 sub 声明（账号的稳定标识）。
//
// 用于给凭据算一个跨登录稳定的 UID：access token 每次登录都会变，拿它做
// ID 会让同一个账号在每次重新登录后都变成一份新凭据。取不到时返回空串。
func JWTSubject(accessToken string) string {
	var claims struct {
		Subject string `json:"sub"`
	}
	if !decodeJWTPayload(accessToken, &claims) {
		return ""
	}
	return strings.TrimSpace(claims.Subject)
}

// JWTDisplayName 返回 JWT 里可读的账号标识（邮箱或名字）。
//
// 供界面区分同一上游下的多个账号：都叫「Claude（Anthropic）」的话，用户
// 没法知道哪个是哪个。取不到时返回空串。
func JWTDisplayName(accessToken string) string {
	var claims struct {
		Email         string `json:"email"`
		PreferredName string `json:"preferred_username"`
		Name          string `json:"name"`
		// OpenAI 把用户信息放在这个命名空间声明里。
		Auth struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/auth"`
	}
	if !decodeJWTPayload(accessToken, &claims) {
		return ""
	}
	for _, candidate := range []string{claims.Email, claims.Auth.Email, claims.PreferredName, claims.Name} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// decodeJWTPayload 解出 JWT 的 payload 段并反序列化；失败返回 false。
func decodeJWTPayload(token string, out any) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// 部分实现用标准 base64 编码。
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return false
		}
	}
	return json.Unmarshal(payload, out) == nil
}
