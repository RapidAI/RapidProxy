package trae

import (
	"bytes"
	"context"
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

	"github.com/znsoftm/RapidProxy/internal/upstream/hx"
)

const exchangeAttempts = 3

// LoginSession 承载一次进行中的浏览器授权。授权 URL 与令牌交换共享同一
// machine/device 身份，交换不能与登录页对设备认知不一致。
type LoginSession struct {
	TraceID   string
	MachineID string
	DeviceID  string

	mu      sync.Mutex
	details *CallbackInfo
	err     error
	done    bool
	notify  chan struct{}
}

// Wait 阻塞直到浏览器重定向落地、流程取消或 ctx 结束。
func (s *LoginSession) Wait(ctx context.Context) (*CallbackInfo, error) {
	if s == nil {
		return nil, errors.New("没有正在进行的 Trae 登录")
	}
	select {
	case <-s.notify:
	case <-ctx.Done():
		return nil, fmt.Errorf("Trae 登录已取消或超时: %w", ctx.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.details == nil {
		return nil, errors.New("Trae 登录回调未携带凭据")
	}
	return s.details, nil
}

// Cancel 取消进行中的登录，Wait 立即解除阻塞。
func (s *LoginSession) Cancel() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	s.err = errors.New("Trae 登录已取消")
	close(s.notify)
}

// finish 关闭会话；close(channel) 唤醒所有等待者。
func (s *LoginSession) finish(details *CallbackInfo, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.details = details
	s.err = err
	s.done = true
	close(s.notify)
}

// CallbackInfo 是授权页回交的账号视图。AuthCode / RefreshToken /
// UserJwtToken 三者之一携带凭据。
type CallbackInfo struct {
	AuthCode     string
	RefreshToken string
	UserJwtToken string
	Host         string
	UserRegion   string
	LoginTraceID string
	UserID       string
	EnterpriseID string
	DisplayName  string
	// DenyReason 携带控制台自己的拒绝文案（error / error_description）。
	DenyReason string
}

// Token 是一份 Trae 访问凭据。RefreshToken 在刷新时轮换，调用方必须把
// 两个非空值都持久化。MachineID/DeviceID 是登录期设备对，永不轮换。
type Token struct {
	AccessToken           string
	RefreshToken          string
	ExpiresAt             int64 // unix 秒；上游沉默时为 0
	RefreshTokenExpiresAt int64
	UserID                string
	EnterpriseID          string
	DisplayName           string
	MachineID             string
	DeviceID              string
}

// StartLogin 为指定区构建待授权会话并启动本地重定向监听。
// 返回的 URL 交给浏览器打开；Wait 阻塞到重定向落地。
func StartLogin(profile Profile, port int) (*LoginSession, string, error) {
	if profile.ConsoleBase == "" {
		return nil, "", errors.New("未知的 Trae 版本")
	}
	machineID, deviceID := randomHex(32), randomHex(16)
	if machineID == "" || deviceID == "" {
		return nil, "", errors.New("Trae 登录初始化失败: 随机标识生成失败")
	}
	traceID := randomHex(8)
	if traceID == "" {
		traceID = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	session := &LoginSession{
		TraceID:   traceID,
		MachineID: machineID,
		DeviceID:  deviceID,
		notify:    make(chan struct{}),
	}
	listener, callbackPort, err := listenCallback(port, session)
	if err != nil {
		return nil, "", err
	}
	loginURL := BuildAuthorizeURL(profile, LoginOptions{
		MachineID: session.MachineID,
		DeviceID:  session.DeviceID,
		TraceID:   session.TraceID,
		Port:      callbackPort,
	})
	// 控制台会探测固定本地端口确认原生客户端在线；尽力应答，避免页面
	// 卡在「认证中」。端口被占用（如另一个客户端实例也在监听）时按
	// 实测行为继续——回调端口仍然可用。
	listenProbePort(session)
	go func() {
		<-session.notify
		listener.Close()
	}()
	return session, loginURL, nil
}

// LoginOptions 是写进授权 URL 的设备身份。
type LoginOptions struct {
	MachineID string
	DeviceID  string
	TraceID   string
	Port      int
}

// BuildAuthorizeURL 复刻社区可用运行器的简化原生形态：18 参数、
// 不带 PKCE challenge。带 code_challenge 会把控制台引向新协议，其交换
// 要求已注册设备证明，无法生成。改动前必须重新抓包。
func BuildAuthorizeURL(profile Profile, options LoginOptions) string {
	port := options.Port
	if port <= 0 {
		port = CallbackPort
	}
	query := url.Values{}
	query.Set("login_version", "1")
	query.Set("auth_from", "solo")
	query.Set("login_channel", "native_ide")
	query.Set("plugin_version", LoginPluginVersion)
	query.Set("auth_type", "local")
	query.Set("client_id", profile.ClientID)
	query.Set("redirect", "0")
	query.Set("login_trace_id", options.TraceID)
	query.Set("auth_callback_url", fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath))
	query.Set("machine_id", options.MachineID)
	query.Set("device_id", options.DeviceID)
	query.Set("x_device_id", options.DeviceID)
	query.Set("x_machine_id", options.MachineID)
	query.Set("x_device_brand", "PC")
	query.Set("x_device_type", "PC")
	query.Set("x_os_version", "1.0")
	query.Set("x_app_version", profile.IDEVersion)
	query.Set("x_app_type", "stable")
	return strings.TrimRight(profile.ConsoleBase, "/") + AuthorizePath + "?" + query.Encode()
}

// listenCallback 绑定重定向端口（被占用时上移）并服务回调直到会话结束。
func listenCallback(port int, session *LoginSession) (*net.TCPListener, int, error) {
	if port < 0 {
		port = 0
	}
	var raw net.Listener
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		raw, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+attempt))
		if err == nil {
			port += attempt
			break
		}
		if !isAddrInUse(err) {
			return nil, 0, fmt.Errorf("本地回调端口 %d 无法监听: %w", port, err)
		}
	}
	if raw == nil {
		return nil, 0, fmt.Errorf("本地回调端口 %d-%d 均被占用，无法监听授权回调", port, port+7)
	}
	listener, ok := raw.(*net.TCPListener)
	if !ok {
		_ = raw.Close()
		return nil, 0, fmt.Errorf("本地回调监听类型异常")
	}
	// 临时绑定的真实端口在这里回读：登录 URL 必须宣告真正在监听的端口，
	// 否则控制台会重定向到默认端口而后面什么都没有。
	if port <= 0 {
		if addr, isTCP := listener.Addr().(*net.TCPAddr); isTCP {
			port = addr.Port
		}
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveCallbackConn(conn, session)
		}
	}()
	return listener, port, nil
}

func isAddrInUse(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "address already in use") ||
		strings.Contains(text, "only one usage of each socket") ||
		strings.Contains(text, "10048")
}

// listenProbePort 尽力服务控制台的固定在线探测口。
func listenProbePort(session *LoginSession) *net.TCPListener {
	raw, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ProbePort))
	if err != nil {
		return nil
	}
	listener, ok := raw.(*net.TCPListener)
	if !ok {
		_ = raw.Close()
		return nil
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go respondPlain(conn)
		}
	}()
	go func() {
		<-session.notify
		listener.Close()
	}()
	return listener
}

// respondPlain 以 "ok" 加 CORS 全开应答在线探测；text/plain 防止中间层嗅探。
func respondPlain(conn net.Conn) {
	defer conn.Close()
	readRequestHead(conn)
	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: 2\r\n" +
		"Cache-Control: no-store\r\n" +
		"Access-Control-Allow-Origin: *\r\n" +
		"Access-Control-Allow-Methods: GET, POST, OPTIONS\r\n" +
		"Access-Control-Allow-Headers: *\r\n" +
		"Connection: close\r\n\r\nok"))
}

func readRequestHead(conn net.Conn) {
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		n, err := conn.Read(buf)
		if err != nil || n == 0 {
			_ = conn.SetReadDeadline(time.Time{})
			return
		}
		if bytes.Contains(buf[:n], []byte("\r\n\r\n")) {
			_ = conn.SetReadDeadline(time.Time{})
			return
		}
	}
}

// serveCallbackConn 解析一次浏览器重定向。不带凭据参数的请求是控制台的
// 在线探测，礼貌应答。
func serveCallbackConn(conn net.Conn, session *LoginSession) {
	defer conn.Close()
	requestLine, err := readCallbackHead(conn)
	if err != nil {
		return
	}
	method, target := splitRequestLine(requestLine)
	if method == "" || target == "" {
		respondPlain(conn)
		return
	}
	params := parseQueryParams(queryOf(target))
	if credentialMarker(params) == "" {
		respondPlain(conn)
		return
	}
	denied := session.receiveCallback(params)
	writeCallbackPage(conn, denied)
}

func readCallbackHead(conn net.Conn) (string, error) {
	buf := make([]byte, 8192)
	total := 0
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			break
		}
		if idx := bytes.Index(buf[:total], []byte("\r\n")); idx >= 0 {
			return string(buf[:idx]), nil
		}
	}
	if total == 0 {
		return "", io.EOF
	}
	return string(buf[:total]), nil
}

func splitRequestLine(line string) (string, string) {
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func queryOf(target string) string {
	if idx := strings.Index(target, "?"); idx >= 0 {
		return target[idx+1:]
	}
	return ""
}

func credentialMarker(params map[string]string) string {
	for _, marker := range []string{"authCodeInfo", "code", "refreshToken", "refresh_token", "accessToken", "access_token", "userJwt"} {
		if _, ok := params[marker]; ok {
			return marker
		}
	}
	return ""
}

// parseQueryParams 解析回调 query，容忍双重编码：先按 & 切分再反复解码，
// 然后在第一个 = 处切开键值（JWT 值里合法地含 =）。
func parseQueryParams(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if idx := strings.Index(pair, "="); idx >= 0 {
			key, value = pair[:idx], pair[idx+1:]
		}
		out[repeatDecode(key)] = repeatDecode(value)
	}
	return out
}

func repeatDecode(raw string) string {
	text := strings.TrimSpace(raw)
	for deep := 0; deep < 3; deep++ {
		decoded, err := url.QueryUnescape(text)
		if err != nil || decoded == text {
			break
		}
		text = decoded
	}
	return strings.TrimSpace(text)
}

// receiveCallback 校验 CSRF 并把凭据停靠在会话上。交换由上层执行，
// 便于过期后重新发起。返回回调是否被拒绝，供本地页面展示。
func (s *LoginSession) receiveCallback(params map[string]string) bool {
	details := parseCallbackDetails(params)
	details.LoginTraceID = firstValue(params, "loginTraceID", "login_trace_id")
	// CSRF 防线：loginTraceID 是发起登录时写进授权 URL 的 8 字节随机数，
	// 真正的控制台重定向会原样带回。本机回调端口对浏览器里任何页面可达，
	// 凡是不带或对不上 trace 的回调都不能采信——当作拒绝处理会把会话
	// 整个打断，等于给恶意页面一个一键打断登录（或投递它自己的
	// refreshToken 顶替用户账号）的开关；这里选择忽略，会话继续等待
	// 真正的回调，直到自然超时。
	if s.TraceID != "" && details.LoginTraceID != s.TraceID {
		return false
	}
	if details.RejectReason() != "" {
		s.finish(details, fmt.Errorf("授权被拒绝: %s", details.RejectReason()))
		return true
	}
	s.finish(details, nil)
	return false
}

// parseCallbackDetails 从一次回调里提取凭据与账号视图；缺失字段留空。
func parseCallbackDetails(params map[string]string) *CallbackInfo {
	details := &CallbackInfo{
		Host:         strings.TrimRight(firstValue(params, "host"), "/"),
		UserRegion:   firstValue(params, "userRegion", "user_region"),
		LoginTraceID: firstValue(params, "loginTraceID", "login_trace_id"),
		UserID:       firstValue(params, "UserID", "userId", "user_id"),
		EnterpriseID: firstValue(params, "TenantID", "EnterpriseID", "enterprise_id"),
		DisplayName:  firstValue(params, "userName", "user_name", "nickname"),
		DenyReason:   firstValue(params, "error_description", "error_msg", "error"),
	}
	if info := params["userInfo"]; info != "" {
		var user struct {
			UserID       string `json:"UserID"`
			UserId       string `json:"userId"`
			ScreenName   string `json:"ScreenName"`
			NickName     string `json:"NickName"`
			Nickname     string `json:"nickname"`
			TenantID     string `json:"TenantID"`
			EnterpriseID string `json:"EnterpriseID"`
		}
		if json.Unmarshal([]byte(info), &user) == nil {
			details.UserID = firstNonEmpty(user.UserID, user.UserId, details.UserID)
			details.DisplayName = firstNonEmpty(user.ScreenName, user.NickName, user.Nickname, details.DisplayName)
			details.EnterpriseID = firstNonEmpty(user.TenantID, user.EnterpriseID, details.EnterpriseID)
		}
	}
	if payload := params["userJwt"]; payload != "" {
		var jwt struct {
			Token        string `json:"Token"`
			RefreshToken string `json:"RefreshToken"`
			Refresh      string `json:"refreshToken"`
		}
		if json.Unmarshal([]byte(payload), &jwt) == nil {
			details.UserJwtToken = strings.TrimSpace(jwt.Token)
			details.RefreshToken = firstNonEmpty(jwt.RefreshToken, jwt.Refresh, details.RefreshToken)
		}
	}
	if payload := params["authCodeInfo"]; payload != "" {
		var code struct {
			AuthCode string `json:"AuthCode"`
			Code     string `json:"Code"`
		}
		if json.Unmarshal([]byte(payload), &code) == nil {
			details.AuthCode = firstNonEmpty(code.AuthCode, code.Code, details.AuthCode)
		}
	}
	if details.AuthCode == "" {
		details.AuthCode = firstValue(params, "code")
	}
	if details.RefreshToken == "" {
		details.RefreshToken = firstValue(params, "refreshToken", "refresh_token", "RefreshToken")
	}
	return details
}

func firstValue(params map[string]string, keys ...string) string {
	for _, key := range keys {
		if value, ok := params[key]; ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(strings.Trim(value, `"`))
		}
	}
	return ""
}

// RejectReason 报告回调不可用的原因，可用时为空。
// 控制台侧拒绝（error / error_description）优先于笼统的缺凭据文案。
func (c *CallbackInfo) RejectReason() string {
	if c == nil {
		return "回调缺失"
	}
	if firstNonEmpty(c.AuthCode, c.RefreshToken, c.UserJwtToken) == "" {
		if c.DenyReason != "" {
			return c.DenyReason
		}
		return "回调既无 AuthCode 也无 refreshToken"
	}
	return ""
}

// writeCallbackPage 以中性状态页应答重定向。页面绝不携带凭据；
// 被拒的回调切换为拒绝提示。
func writeCallbackPage(conn net.Conn, denied bool) {
	title, message := "登录信息已接收", "正在换取登录凭证，请回到应用等待登录结果。"
	if denied {
		title, message = "登录未完成", "本次授权未提交有效登录信息，可以关闭本页并回到应用重新发起登录。"
	}
	body := fmt.Sprintf(`<!doctype html><meta charset="utf-8"><title>%s</title>`+
		`<body style="font-family:system-ui,'Segoe UI',sans-serif;max-width:420px;margin:64px auto;text-align:center">`+
		`<h2 style="font-weight:600;font-size:18px">%s</h2>`+
		`<p style="color:#555;font-size:14px">%s</p></body>`, title, title, message)
	payload := []byte(body)
	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(payload)) +
		"Cache-Control: no-store\r\n" +
		"Access-Control-Allow-Origin: *\r\n" +
		"Connection: close\r\n\r\n"))
	_, _ = conn.Write(payload)
}

// Resolve 把完成的回调换成 Token。machine/device 身份保持授权 URL 里
// 打印的那一对。
func (s *LoginSession) Resolve(ctx context.Context, hc *http.Client, profile Profile, details *CallbackInfo) (*Token, error) {
	if details == nil {
		return nil, errors.New("登录未完成")
	}
	switch {
	case details.RefreshToken != "":
		token, err := refreshExchange(ctx, hc, profile, details.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("换取 Trae 登录凭证失败: %w", err)
		}
		token.UserID = firstNonEmpty(token.UserID, details.UserID)
		token.EnterpriseID = firstNonEmpty(token.EnterpriseID, details.EnterpriseID)
		token.DisplayName = firstNonEmpty(token.DisplayName, details.DisplayName)
		s.stampIdentity(token)
		return token, nil
	case details.UserJwtToken != "":
		// 旧回调直接交来活的 JWT；保留它并从 claims 派生稳定身份，
		// 不再重新交换。
		token := &Token{
			AccessToken:  details.UserJwtToken,
			RefreshToken: details.RefreshToken,
			UserID:       details.UserID,
			EnterpriseID: details.EnterpriseID,
			DisplayName:  details.DisplayName,
		}
		s.stampIdentity(token)
		return token, nil
	case details.AuthCode != "":
		// 授权 URL 不带 PKCE challenge，简化控制台应回发 refreshToken/userJwt。
		// 回发 AuthCode 意味着上游切换到了无法服务的设备证明契约——重试通常
		// 无济于事，如实上报。
		return nil, fmt.Errorf("控制台回调携带 AuthCode（设备证明协议），未返回 refreshToken；请重新发起一次登录重试")
	default:
		return nil, errors.New("回调不带可用的凭据")
	}
}

// stampIdentity 保留登录期指纹：refresh token 系于授权 URL 里的设备对，
// 聊天头必须复用同一对。claims 只用于补账号 ID。
func (s *LoginSession) stampIdentity(token *Token) {
	claims := parseJWTClaims(token.AccessToken)
	token.UserID = firstNonEmpty(token.UserID, claims.UserID, claims.UID)
	token.MachineID = s.MachineID
	token.DeviceID = s.DeviceID
}

// refreshExchange 在 profile 的账号主机上按探测顺序把 refreshToken 换成新
// 访问令牌。旧契约的 ClientSecret 使用上游容忍的 "-"。
func refreshExchange(ctx context.Context, hc *http.Client, profile Profile, refreshToken string) (*Token, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("refresh_token 为空，请重新登录")
	}
	var lastErr error
	for _, host := range profile.AuthHosts() {
		endpoint := strings.TrimRight(host, "/") + ExchangePath
		for attempt := 0; attempt < exchangeAttempts; attempt++ {
			if attempt > 0 {
				if err := sleepWithContext(ctx, time.Duration(attempt)*time.Second); err != nil {
					return nil, err
				}
			}
			token, err := postExchange(ctx, hc, profile, endpoint, map[string]any{
				"ClientID":     profile.ClientID,
				"RefreshToken": refreshToken,
				"ClientSecret": "-",
				"UserID":       "",
			})
			if err != nil {
				var stale *upstreamRejection
				if errors.As(err, &stale) {
					// 该主机上明确的 4xx 拒绝：不再重复打同一端点，
					// 但兄弟主机可能发布同一契约，落到下一台继续。
					lastErr = fmt.Errorf("Trae 账号主机拒绝了本次交换: %s", stale.Message)
					break
				}
				lastErr = err
				continue
			}
			if token.RefreshToken == "" {
				token.RefreshToken = refreshToken
			}
			return token, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("刷新 Trae 登录失败")
	}
	return nil, lastErr
}

// getUserInfoAt 在一个账号主机上读取账号资料。
func getUserInfoAt(ctx context.Context, hc *http.Client, host string, profile Profile, token *Token) (string, string, string, error) {
	raw, err := json.Marshal(map[string]any{
		"ReqSource":  "IDE",
		"IDEVersion": profile.IDEVersion,
	})
	if err != nil {
		return "", "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(host, "/")+UserInfoPath, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgentPrefix+profile.IDEVersion)
	req.Header.Set("X-Cloudide-Token", token.AccessToken)
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("HTTP %d %s", resp.StatusCode, summarizeBody(body))
	}
	var payload struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if json.Unmarshal(body, &payload) == nil {
		return strings.TrimSpace(payload.Result.UserID),
			strings.TrimSpace(payload.Result.ScreenName),
			strings.TrimSpace(payload.Result.EnterpriseID), nil
	}
	var fallback any
	if json.Unmarshal(body, &fallback) != nil {
		return "", "", "", errors.New("账号信息响应无法解析")
	}
	return firstKeyAny(fallback, "UserID", "userId"),
		firstKeyAny(fallback, "ScreenName", "nickname"),
		firstKeyAny(fallback, "EnterpriseID", "enterpriseId"), nil
}

// GetUserInfo 在新 JWT 之后跨区账号主机读取账号资料。浏览器重定向没有
// 带账号明细时用它补显示名。
func GetUserInfo(ctx context.Context, hc *http.Client, profile Profile, token *Token) (string, string, string, error) {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return "", "", "", errors.New("missing access token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var lastErr error
	for _, host := range profile.AuthHosts() {
		userID, name, ent, err := getUserInfoAt(ctx, hc, host, profile, token)
		if err == nil {
			return userID, name, ent, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("账号信息读取失败")
	}
	return "", "", "", lastErr
}

// upstreamRejection 表示重试无法挽回的拒绝（坏授权）。
type upstreamRejection struct {
	Message string
}

func (e *upstreamRejection) Error() string { return e.Message }

type upstreamEnvelope struct {
	ResponseMetadata struct {
		Error struct {
			Code    any    `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
	Code any    `json:"code"`
	Msg  string `json:"message"`
}

func postExchange(ctx context.Context, hc *http.Client, profile Profile, endpoint string, payload map[string]any) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgentPrefix+profile.IDEVersion)
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		// 5xx / 限流是暂时性故障：保持普通错误让刷新循环重试。
		return nil, fmt.Errorf("Trae 上游暂时不可用 (HTTP %d): %s", resp.StatusCode, summarizeBody(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &upstreamRejection{Message: fmt.Sprintf("HTTP %d %s", resp.StatusCode, summarizeBody(body))}
	}
	var envelope upstreamEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil {
		if metaCode := strings.TrimSpace(anyTrim(envelope.ResponseMetadata.Error.Code)); metaCode != "" && metaCode != "0" {
			return nil, &upstreamRejection{Message: fmt.Sprintf("%s: %s", metaCode, envelope.ResponseMetadata.Error.Message)}
		}
		if topCode := strings.TrimSpace(anyTrim(envelope.Code)); topCode != "" && topCode != "0" {
			return nil, &upstreamRejection{Message: fmt.Sprintf("%s: %s", topCode, envelope.Msg)}
		}
	}
	token := tokenFromExchange(body)
	if token.AccessToken == "" {
		return nil, errors.New("登录响应缺少 token")
	}
	return token, nil
}

// tokenFromExchange 容忍多种 token 形态：账号主机把 token 嵌在
// Result {Token, RefreshToken, TokenExpireAt, RefreshExpireAt} 下，
// 其它形态用 AccessToken/RefreshToken 等别名。
func tokenFromExchange(raw []byte) *Token {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return &Token{}
	}
	access := firstKeyAny(payload, "Token", "AccessToken", "access_token", "Jwt", "JWT")
	refresh := firstKeyAny(payload, "RefreshToken", "refresh_token", "refreshToken")
	if refresh != "" && refresh == access {
		refresh = ""
	}
	token := &Token{AccessToken: access, RefreshToken: refresh}
	if at := numericAt(firstKeyAnyNumber(payload, "TokenExpireAt", "expires_at", "ExpiresAt")); at > 0 {
		token.ExpiresAt = at
	}
	if token.ExpiresAt == 0 {
		if duration := firstKeyAnyNumber(payload, "TokenExpireDuration", "expires_in", "ExpiresIn"); duration > 0 {
			token.ExpiresAt = time.Now().Add(time.Duration(duration) * time.Second).Unix()
		}
	}
	if at := numericAt(firstKeyAnyNumber(payload, "RefreshExpireAt", "refresh_token_expires_at")); at > 0 {
		token.RefreshTokenExpiresAt = at
	}
	bindUserFields(payload, token)
	return token
}

func bindUserFields(payload any, token *Token) {
	user := findObject(payload, "user", "User")
	if user == nil {
		return
	}
	id := numericToStringForID(firstKeyAnyValue(user, "id", "ID", "userId", "UserID", "yid", "uid"))
	if id != "" && token.UserID == "" {
		token.UserID = id
	}
}

func findObject(payload any, keys ...string) any {
	switch tree := payload.(type) {
	case map[string]any:
		for _, key := range keys {
			if raw, ok := tree[key]; ok {
				if _, isMap := raw.(map[string]any); isMap {
					return raw
				}
			}
		}
		for _, child := range tree {
			if nested := findObject(child, keys...); nested != nil {
				return nested
			}
		}
	case []any:
		for _, child := range tree {
			if nested := findObject(child, keys...); nested != nil {
				return nested
			}
		}
	}
	return nil
}

func firstKeyAny(payload any, keys ...string) string {
	value := firstKeyAnyValue(payload, keys...)
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if number, ok := value.(float64); ok {
		return decimal(uint64(number))
	}
	return ""
}

func firstKeyAnyNumber(payload any, keys ...string) int64 {
	return coerceInt(firstKeyAnyValue(payload, keys...))
}

func firstKeyAnyValue(payload any, keys ...string) any {
	return firstKeyRecursiveDepth(payload, keys, 4)
}

func firstKeyRecursiveDepth(payload any, keys []string, depth int) any {
	if depth < 0 {
		return nil
	}
	switch tree := payload.(type) {
	case map[string]any:
		for key, value := range tree {
			for _, candidate := range keys {
				if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(key)) {
					return value
				}
			}
		}
		for _, child := range tree {
			if found := firstKeyRecursiveDepth(child, keys, depth-1); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range tree {
			if found := firstKeyRecursiveDepth(child, keys, depth-1); found != nil {
				return found
			}
		}
	}
	return nil
}

func numericToStringForID(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v > 0 {
			return decimal(uint64(v))
		}
	}
	return ""
}

func coerceInt(raw any) int64 {
	switch v := raw.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return 0
		}
		var parsed float64
		if _, err := fmt.Sscanf(text, "%g", &parsed); err != nil {
			return 0
		}
		return int64(parsed)
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	default:
		return 0
	}
}

// numericAt 优先取绝对 unix 秒（>1e12 视为毫秒）；较小的正数按相对时长处理。
func numericAt(raw any) int64 {
	number := coerceInt(raw)
	if number <= 0 {
		return 0
	}
	switch {
	case number >= 1_000_000_000_000:
		return number / 1000 // 毫秒纪元
	case number >= unixEpochFloor:
		return number
	default:
		return time.Now().Add(time.Duration(number) * time.Second).Unix()
	}
}

const unixEpochFloor = 1_000_000_000

func anyTrim(raw any) string {
	if raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == 0 {
			return "0"
		}
		return decimal(uint64(v))
	default:
		return ""
	}
}

// summarizeBody 保持错误页可读且不泄漏无关载荷。
// 先脱敏再截断：截断可能把键值对切成两半，脱敏正则会漏掉残段。
func summarizeBody(body []byte) string {
	const keep = 256
	runes := []rune(strings.TrimSpace(hx.RedactTokens(string(body))))
	if len(runes) > keep {
		runes = runes[:keep]
	}
	return strings.TrimSpace(string(runes))
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Refresh 用 refreshToken 换取新访问令牌对。两个 token 在服务端轮换，
// 调用方必须把非空值都持久化。设备对随行：传入凭据里存的那一对
// （token 家族 mint 时所系），未知时传空串。
func Refresh(ctx context.Context, hc *http.Client, profile Profile, refreshToken, machineID, deviceID string) (*Token, error) {
	token, err := refreshExchange(ctx, hc, profile, refreshToken)
	if err != nil {
		return nil, err
	}
	token.MachineID = strings.TrimSpace(machineID)
	token.DeviceID = strings.TrimSpace(deviceID)
	return token, nil
}
