// Package zcode 实现智谱 ZCode 在线登录（"zcode login bigmodel"）：用户在浏览器
// 于智谱授权页完成审批，客户端轮询同一端点直到授权落地，再用 OAuth 访问令牌
// 向智谱业务接口换出 coding-plan API Key（"{id}.{secret}"）。
//
// 换出的 Key 与手动创建的 Key 完全等价，可直接用于 open.bigmodel.cn 的
// Anthropic 兼容网关；但请求必须带上「编程客户端」身份的 User-Agent（见
// config.ZhipuPreset 的 UserAgent="opencode"），智谱才会把额度计入 ZCode /
// 编程套餐赠送部分——这正是本包存在的原因：让 WorkBuddy 代理以 ZCode 客户端
// 身份登录并使用赠送额度。
//
// 国内站（zcode.chatglm.site）优先，国际站（zcode.z.ai）兜底；一个流程必须在
// 创建它的那个 origin 上轮询。
package zcode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 登录端点，协议与开源 ZCode CLI 一致。国内站优先，国际站兜底。
var endpointOrigins = []string{
	"https://zcode.chatglm.site",
	"https://zcode.z.ai",
}

// bigModelOrigin 承载把 OAuth 访问令牌换成 coding-plan API Key 的业务接口。
var bigModelOrigin = "https://bigmodel.cn"

const (
	apiBasePath     = "/api/v1"
	OAuthProviderID = "bigmodel"

	customerInfoPath   = "/api/biz/customer/getCustomerInfo"
	apiKeyListFmt      = "/api/biz/v1/organization/%s/projects/%s/api_keys"
	codingPlanKeyName  = "zcode-api-key"
	defaultOrgName     = "默认机构"
	defaultProjectName = "默认项目"

	pollTokenBytes       = 32
	maxPollLifetime      = 5 * time.Minute
	minPollInterval      = time.Second
	pollIntervalFallback = 2 * time.Second
	initAttemptTimeout   = 8 * time.Second
)

// UserAgent 标识本代理登录端与业务端点。
const UserAgent = "MaClaw-ZCodeLogin/1.0"

// Login 是一次进行中的 ZCode 浏览器审批流。
type Login struct {
	origin       string
	pollToken    string
	flowID       string
	authorizeURL string
	expiresAt    time.Time
	pollInterval time.Duration
}

// AuthURL 是用户需在浏览器打开的智谱在线登录页。
func (l *Login) AuthURL() string {
	if l == nil {
		return ""
	}
	return l.authorizeURL
}

// Lifetime 返回审批窗口剩余时长（收敛到合理范围，避免服务端 deadline 异常）。
func (l *Login) Lifetime() time.Duration {
	if l == nil || l.expiresAt.IsZero() {
		return maxPollLifetime
	}
	remaining := time.Until(l.expiresAt)
	if remaining <= 0 {
		return minPollInterval
	}
	if remaining > maxPollLifetime {
		return maxPollLifetime
	}
	return remaining
}

// IsAuthorizeURL 判断 raw 是否是我们愿意打开的智谱登录页（HTTPS 且 host 在
// 智谱 / ZCode 域内；localhost 仅用于测试服务器）。
func IsAuthorizeURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		switch strings.ToLower(parsed.Hostname()) {
		case "bigmodel.cn", "www.bigmodel.cn", "zcode.chatglm.site", "zcode.z.ai":
			return true
		}
	case "http":
		switch strings.ToLower(parsed.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

// StartLogin 通过 POST /oauth/cli/init 创建登录流，逐个尝试 endpoint 直到一个
// 响应。业务拒绝会立即失败（每个 origin 都会拒绝）；单个 origin 黑洞时由
// initAttemptTimeout 快速切换到兜底 origin。
func StartLogin(ctx context.Context, hc *http.Client) (*Login, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	pollToken, err := newPollToken()
	if err != nil {
		return nil, fmt.Errorf("智谱登录初始化失败: %w", err)
	}
	var lastErr error
	for _, origin := range endpointOrigins {
		attemptCtx, cancel := context.WithTimeout(ctx, initAttemptTimeout)
		login, err := initFlowAt(attemptCtx, hc, origin, pollToken)
		cancel()
		if err == nil {
			return login, nil
		}
		lastErr = err
		var bizErr *businessError
		if errors.As(err, &bizErr) {
			return nil, err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("登录端点不可用")
	}
	return nil, fmt.Errorf("智谱登录初始化失败: %w", lastErr)
}

func initFlowAt(ctx context.Context, hc *http.Client, origin, pollToken string) (*Login, error) {
	base := strings.TrimRight(origin, "/") + apiBasePath
	raw, err := postEnvelope(ctx, hc, base+"/oauth/cli/init", map[string]string{"provider": OAuthProviderID}, "Bearer "+pollToken)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("智谱登录响应数据为空")
	}
	var data struct {
		FlowID          string  `json:"flow_id"`
		AuthorizeURL    string  `json:"authorize_url"`
		ExpiresAt       unixSec `json:"expires_at"`
		PollIntervalSec float64 `json:"poll_interval_sec"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("智谱登录响应无效: %w", err)
	}
	if strings.TrimSpace(data.FlowID) == "" || !IsAuthorizeURL(data.AuthorizeURL) {
		return nil, errors.New("智谱登录响应缺少有效的授权地址")
	}
	interval := time.Duration(data.PollIntervalSec * float64(time.Second))
	if interval < minPollInterval {
		interval = pollIntervalFallback
	}
	login := &Login{
		origin:       strings.TrimRight(origin, "/"),
		pollToken:    pollToken,
		flowID:       strings.TrimSpace(data.FlowID),
		authorizeURL: strings.TrimSpace(data.AuthorizeURL),
		pollInterval: interval,
	}
	if data.ExpiresAt > 0 {
		login.expiresAt = time.Unix(int64(data.ExpiresAt), 0)
	}
	return login, nil
}

// Token 是一次完成的审批产出的凭据集。AccessToken 用于智谱业务接口；换出的
// coding-plan API Key 才是 provider 请求实际使用的。
type Token struct {
	AccessToken  string
	RefreshToken string
	JWT          string
	UserID       string
	UserName     string
	Email        string
}

type pollResponse struct {
	Status   string `json:"status"`
	Token    string `json:"token"`
	BigModel struct {
		AccessToken     string `json:"access_token"`
		AccessTokenAlt  string `json:"accessToken"`
		RefreshToken    string `json:"refresh_token"`
		RefreshTokenAlt string `json:"refreshToken"`
	} `json:"bigmodel"`
	User struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
		Avatar string `json:"avatar"`
	} `json:"user"`
}

func (r pollResponse) accessToken() string {
	if strings.TrimSpace(r.BigModel.AccessToken) != "" {
		return strings.TrimSpace(r.BigModel.AccessToken)
	}
	return strings.TrimSpace(r.BigModel.AccessTokenAlt)
}

func (r pollResponse) refreshToken() string {
	if strings.TrimSpace(r.BigModel.RefreshToken) != "" {
		return strings.TrimSpace(r.BigModel.RefreshToken)
	}
	return strings.TrimSpace(r.BigModel.RefreshTokenAlt)
}

// PollUntilReady 轮询 flow 的 origin 直到审批落地、服务端报告失败或窗口关闭。
// 待审批与网络抖动都继续轮询，已打开的授权页不会被浪费。
func PollUntilReady(ctx context.Context, hc *http.Client, login *Login) (Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	if login == nil || login.flowID == "" || login.pollToken == "" {
		return Token{}, errors.New("没有正在进行的智谱登录")
	}
	deadline := time.Now().Add(login.Lifetime())
	endpoint := login.origin + apiBasePath + "/oauth/cli/poll/" + url.PathEscape(login.flowID)
	for {
		if time.Now().After(deadline) {
			return Token{}, errors.New("智谱登录已超时，请重新登录")
		}
		token, retryable, err := pollOnce(ctx, hc, endpoint, login.pollToken)
		if err == nil {
			return token, nil
		}
		if !retryable {
			return Token{}, err
		}
		if err := sleepWithContext(ctx, login.pollInterval); err != nil {
			return Token{}, err
		}
	}
}

func pollOnce(ctx context.Context, hc *http.Client, endpoint, pollToken string) (token Token, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Token{}, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, true, fmt.Errorf("轮询智谱登录失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Token{}, true, fmt.Errorf("读取智谱登录响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, isTransientStatus(resp.StatusCode), fmt.Errorf("轮询智谱登录失败 (HTTP %d): %s", resp.StatusCode, truncateForError(body))
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Token{}, false, fmt.Errorf("智谱登录响应无法解析: %w", err)
	}
	if envelope.Code != 0 {
		return Token{}, false, &businessError{msg: strings.TrimSpace(envelope.Msg), code: envelope.Code}
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return Token{}, true, errors.New("智谱登录响应数据为空")
	}
	var payload pollResponse
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		return Token{}, false, fmt.Errorf("智谱登录数据无法解析: %w", err)
	}
	switch payload.Status {
	case "pending":
		return Token{}, true, errPendingApproval
	case "failed":
		return Token{}, false, errors.New("智谱授权失败，请重新登录")
	case "ready":
	default:
		return Token{}, false, fmt.Errorf("智谱登录返回未知状态 %q", payload.Status)
	}
	token = Token{
		AccessToken:  payload.accessToken(),
		RefreshToken: payload.refreshToken(),
		JWT:          strings.TrimSpace(payload.Token),
		UserID:       strings.TrimSpace(payload.User.UserID),
		UserName:     strings.TrimSpace(payload.User.Name),
		Email:        strings.TrimSpace(payload.User.Email),
	}
	if token.AccessToken == "" || token.UserID == "" {
		return Token{}, false, errors.New("智谱登录响应缺少访问令牌")
	}
	return token, false, nil
}

func isTransientStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return status >= 500
	}
}

// ResolveCodingPlanAPIKey 把 OAuth 访问令牌换成账号的 coding-plan API Key：
// 在 bigmodel.cn 上取默认组织/项目，找到名为 "zcode-api-key" 的 Key（没有则
// 创建），最终得到 "{id}.{secret}" 形式——与开放平台手动颁发的 Key 格式一致。
func ResolveCodingPlanAPIKey(ctx context.Context, hc *http.Client, accessToken string) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", errors.New("智谱访问令牌为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	var customer struct {
		Organizations []bizOrganization `json:"organizations"`
	}
	if err := bizRequest(ctx, hc, http.MethodGet, bigModelOrigin+customerInfoPath, nil, accessToken, &customer); err != nil {
		return "", fmt.Errorf("获取智谱账号信息失败: %w", err)
	}
	organizationID, projectID := pickOrgAndProject(customer.Organizations)
	if organizationID == "" || projectID == "" {
		return "", errors.New("未找到智谱组织或项目，请先在 bigmodel.cn 开通后重试")
	}
	listEndpoint := bigModelOrigin + fmt.Sprintf(apiKeyListFmt, url.PathEscape(organizationID), url.PathEscape(projectID))
	var keys []bizAPIKey
	if err := bizRequest(ctx, hc, http.MethodGet, listEndpoint, nil, accessToken, &keys); err != nil {
		return "", fmt.Errorf("获取智谱 API Key 列表失败: %w", err)
	}
	apiKey := ""
	for _, key := range keys {
		if strings.TrimSpace(key.Name) == codingPlanKeyName && strings.TrimSpace(key.APIKey) != "" {
			apiKey = strings.TrimSpace(key.APIKey)
			break
		}
	}
	if apiKey == "" {
		var created bizAPIKey
		if err := bizRequest(ctx, hc, http.MethodPost, listEndpoint, map[string]string{"name": codingPlanKeyName}, accessToken, &created); err != nil {
			return "", fmt.Errorf("创建智谱 API Key 失败: %w", err)
		}
		apiKey = strings.TrimSpace(created.APIKey)
	}
	if apiKey == "" {
		return "", errors.New("智谱 API Key 响应缺少密钥")
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
	}
	if err := bizRequest(ctx, hc, http.MethodGet, listEndpoint+"/copy/"+url.PathEscape(apiKey), nil, accessToken, &secret); err != nil {
		return "", fmt.Errorf("读取智谱 API Key 密钥失败: %w", err)
	}
	secretKey := strings.TrimSpace(secret.SecretKey)
	if secretKey == "" {
		return apiKey, nil
	}
	return apiKey + "." + secretKey, nil
}

type bizOrganization struct {
	OrganizationID   string       `json:"organizationId"`
	OrganizationName string       `json:"organizationName"`
	Projects         []bizProject `json:"projects"`
}

type bizProject struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
}

type bizAPIKey struct {
	APIKey string `json:"apiKey"`
	Name   string `json:"name"`
}

func pickOrgAndProject(orgs []bizOrganization) (string, string) {
	if len(orgs) == 0 {
		return "", ""
	}
	chosen := orgs[0]
	for _, org := range orgs {
		if strings.Contains(org.OrganizationName, defaultOrgName) {
			chosen = org
			break
		}
	}
	if strings.TrimSpace(chosen.OrganizationID) == "" {
		return "", ""
	}
	projectID := ""
	for _, project := range chosen.Projects {
		if strings.Contains(project.ProjectName, defaultProjectName) {
			projectID = project.ProjectID
			break
		}
	}
	if projectID == "" && len(chosen.Projects) > 0 {
		projectID = chosen.Projects[0].ProjectID
	}
	return strings.TrimSpace(chosen.OrganizationID), strings.TrimSpace(projectID)
}

// ---------------------------------------------------------------------------
// HTTP 基础
// ---------------------------------------------------------------------------

var errPendingApproval = errors.New("智谱授权等待中")

type businessError struct {
	msg  string
	code int
}

func (e *businessError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("智谱服务错误 (%d): %s", e.code, e.msg)
	}
	return fmt.Sprintf("智谱服务错误 (%d)", e.code)
}

type unixSec int64

func (t *unixSec) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		*t = 0
		return nil
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		*t = 0
		return nil
	}
	*t = unixSec(value)
	return nil
}

func postEnvelope(ctx context.Context, hc *http.Client, endpoint string, payload any, authorization string) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求智谱登录服务失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("读取智谱登录响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求智谱登录服务失败 (HTTP %d): %s", resp.StatusCode, truncateForError(raw))
	}
	return decodeEnvelope(raw)
}

func bizRequest(ctx context.Context, hc *http.Client, method, endpoint string, payload any, accessToken string, out any) error {
	var reqBody io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		reqBody = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求智谱业务接口失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("读取智谱业务响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("请求智谱业务接口失败 (HTTP %d): %s", resp.StatusCode, truncateForError(raw))
	}
	return decodeEnvelopeInto(raw, out)
}

func decodeEnvelope(raw []byte) (json.RawMessage, error) {
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("智谱响应无法解析: %w", err)
	}
	if !isSuccessfulBizCode(envelope.Code) {
		msg := strings.TrimSpace(envelope.Msg)
		if msg == "" {
			msg = fmt.Sprintf("业务错误 %s", string(envelope.Code))
		}
		code := 0
		_ = json.Unmarshal(envelope.Code, &code)
		return nil, &businessError{msg: msg, code: code}
	}
	return envelope.Data, nil
}

func decodeEnvelopeInto(raw []byte, out any) error {
	data, err := decodeEnvelope(raw)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 || string(data) == "null" {
		return nil
	}
	return json.Unmarshal(data, out)
}

// isSuccessfulBizCode 容忍成功判定：0 / 200 / 字符串形式 / 缺省 / null。
func isSuccessfulBizCode(raw json.RawMessage) bool {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch text {
	case "", "0", "200", "null":
		return true
	}
	return false
}

func newPollToken() (string, error) {
	buf := make([]byte, pollTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		d = pollIntervalFallback
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// truncateForError 按字符（rune）截断上游错误正文：智谱的错误多为中文，
// 按字节切会把 UTF-8 序列切断，界面上显示成乱码。
func truncateForError(b []byte) string {
	s := strings.TrimSpace(string(b))
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	count := 0
	for i := range s {
		if count == 200 {
			return s[:i]
		}
		count++
	}
	return s
}
