// 浏览器设备登录（逆向自官方 qoder CLI）：浏览器页面携带一次性 PKCE
// challenge，本端按秒轮询 OpenAPI 主机直到批准以 token 形态落地。
package qoder

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream/hx"
)

// ErrPending 表示浏览器端批准尚未落地，应继续轮询。
var ErrPending = errors.New("Qoder 登录尚未批准")

// DeviceLogin 是一次进行中的浏览器批准。
type DeviceLogin struct {
	nonce     string // 把授权页与轮询会话绑定的随机 id
	verifier  string // 每次轮询都回传的 PKCE verifier
	MachineID string // 稳定的每安装 machine id（上游仅作参考）
}

// AuthURL 是用户需要在浏览器打开的页面。官方 CLI 以固定顺序传递 query
// （challenge、challenge_method、nonce、machine_id、client_id——按序排列会
// 把 client_id 与 machine_id 排到最前），这里输出完全相同的顺序。
func (l *DeviceLogin) AuthURL(profile Profile) string {
	values := []struct{ key, value string }{
		{"challenge", pkceChallenge(l.verifier)},
		{"challenge_method", "S256"},
		{"nonce", l.nonce},
		{"machine_id", l.MachineID},
		{"client_id", profile.ClientID},
	}
	parts := make([]string, 0, len(values))
	for _, kv := range values {
		parts = append(parts, url.QueryEscape(kv.key)+"="+url.QueryEscape(kv.value))
	}
	return strings.TrimRight(profile.WebOrigin, "/") + "/device/selectAccounts?" + strings.Join(parts, "&")
}

// Nonce 是展示给用户做关联的登录会话 id。
func (l *DeviceLogin) Nonce() string { return l.nonce }

// StartLogin 生成新的 PKCE 会话。不接触网络。
func StartLogin(machineID string) (*DeviceLogin, error) {
	verifier, err := pkceVerifier()
	if err != nil {
		return nil, err
	}
	return &DeviceLogin{
		nonce:     newUUID(),
		verifier:  verifier,
		MachineID: strings.TrimSpace(machineID),
	}, nil
}

// Token 是一份设备登录凭据。刷新时 access 与 refresh 两个 token 都会轮换，
// 调用方必须把新值都持久化。
type Token struct {
	AccessToken           string
	RefreshToken          string
	UserID                string
	UserName              string
	ExpiresAt             int64 // unix 秒；0 = 服务端未报告
	RefreshTokenExpiresAt int64 // unix 秒（刷新时总是轮换）
}

// Poll 执行一次登录轮询。404 表示批准尚未落地；传输层抖动（状态码 0）
// 与官方 CLI 一致按「继续等待」处理，避免一次网络抖动毁掉用户已打开的
// 授权页；其它 HTTP 状态终止登录。
func Poll(ctx context.Context, hc *http.Client, profile Profile, login *DeviceLogin) (Token, error) {
	if login == nil || login.nonce == "" || login.verifier == "" {
		return Token{}, errors.New("没有正在进行的 Qoder 登录")
	}
	endpoint := strings.TrimRight(profile.OpenAPIBase, "/") + "/api/v1/deviceToken/poll"
	token, status, err := pollOnce(ctx, hc, endpoint, login)
	if err != nil {
		if status == 0 || status == http.StatusNotFound {
			return Token{}, ErrPending
		}
		return Token{}, err
	}
	return token, nil
}

func pollOnce(ctx context.Context, hc *http.Client, endpoint string, login *DeviceLogin) (Token, int, error) {
	params := url.Values{}
	params.Set("nonce", login.nonce)
	params.Set("verifier", login.verifier)
	params.Set("challenge_method", "S256")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return Token{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return Token{}, 0, fmt.Errorf("轮询 Qoder 登录失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Token{}, resp.StatusCode, fmt.Errorf("读取 Qoder 登录响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, resp.StatusCode, fmt.Errorf("轮询 Qoder 登录失败 (HTTP %d): %s", resp.StatusCode, truncateForError(body))
	}
	var payload pollResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, resp.StatusCode, fmt.Errorf("Qoder 登录响应无法解析: %w", err)
	}
	token := tokenFromPoll(payload, time.Now())
	if token.AccessToken == "" {
		return Token{}, resp.StatusCode, errors.New("Qoder 登录响应缺少 token")
	}
	return token, resp.StatusCode, nil
}

// pollResponse 对应 /api/v1/deviceToken/poll 载荷。每个字段都带 CLI 宽容
// 解析器接受的键别名。
type pollResponse struct {
	Token                    string         `json:"token"`
	DeviceToken              string         `json:"device_token"`
	AccessToken              string         `json:"access_token"`
	RefreshToken             string         `json:"refresh_token"`
	RefreshTokenSpelled      string         `json:"refreshToken"`
	UserID                   string         `json:"user_id"`
	UserIDAlt                string         `json:"userId"`
	UID                      string         `json:"uid"`
	UserName                 string         `json:"user_name"`
	UserNameAlt              string         `json:"userName"`
	Name                     string         `json:"name"`
	ExpiresAt                stringOrNumber `json:"expires_at"`
	ExpiresAtAlt             stringOrNumber `json:"expiresAt"`
	ExpiresIn                stringOrNumber `json:"expires_in"`
	ExpiresInAlt             stringOrNumber `json:"expiresIn"`
	RefreshTokenExpiresAt    stringOrNumber `json:"refresh_token_expires_at"`
	RefreshTokenExpiresAtAlt stringOrNumber `json:"refreshTokenExpiresAt"`
	RefreshTokenExpiresIn    stringOrNumber `json:"refresh_token_expires_in"`
}

func (r pollResponse) tokenValue() string {
	switch {
	case r.Token != "":
		return r.Token
	case r.DeviceToken != "":
		return r.DeviceToken
	default:
		return r.AccessToken
	}
}

func (r pollResponse) refreshTokenValue() string {
	if r.RefreshToken != "" {
		return r.RefreshToken
	}
	return r.RefreshTokenSpelled
}

func (r pollResponse) userIDValue() string {
	switch {
	case r.UserID != "":
		return r.UserID
	case r.UserIDAlt != "":
		return r.UserIDAlt
	default:
		return r.UID
	}
}

func (r pollResponse) userNameValue() string {
	switch {
	case r.UserName != "":
		return r.UserName
	case r.UserNameAlt != "":
		return r.UserNameAlt
	default:
		return r.Name
	}
}

// tokenFromPoll 把成功的轮询载荷转换成 Token。轮询以绝对 unix 秒报告时间
// （expires_at），expires_in 作为相对回退。
func tokenFromPoll(r pollResponse, now time.Time) Token {
	token := Token{
		AccessToken:  r.tokenValue(),
		RefreshToken: r.refreshTokenValue(),
		UserID:       r.userIDValue(),
		UserName:     r.userNameValue(),
	}
	if unix := numericOrUnix(firstNonEmpty(r.ExpiresAt, r.ExpiresAtAlt), relativeSeconds(firstNonEmpty(r.ExpiresIn, r.ExpiresInAlt)), now); unix > 0 {
		token.ExpiresAt = unix
	}
	if unix := numericOrUnix(firstNonEmpty(r.RefreshTokenExpiresAt, r.RefreshTokenExpiresAtAlt), relativeSeconds(firstNonEmpty(r.RefreshTokenExpiresIn)), now); unix > 0 {
		token.RefreshTokenExpiresAt = unix
	}
	return token
}

func firstNonEmpty(values ...stringOrNumber) stringOrNumber {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// relativeSeconds 读取 expires_in 型时长，容忍数字与带引号整数。
// 不可解析或非正值为 0。
func relativeSeconds(value stringOrNumber) int64 {
	text := strings.TrimSpace(string(value))
	if text == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		floatValue, floatErr := strconv.ParseFloat(text, 64)
		if floatErr != nil || !isFinite(floatValue) {
			return 0
		}
		parsed = int64(floatValue)
	}
	if parsed <= 0 {
		return 0
	}
	return parsed
}

// stringOrNumber 对一个字段同时接受 JSON 字符串或裸数字（CLI 自己的解析器
// 对两种写法一视同仁）。
type stringOrNumber string

func (s *stringOrNumber) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		*s = ""
		return nil
	}
	if text[0] == '"' {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		*s = stringOrNumber(strings.TrimSpace(decoded))
		return nil
	}
	*s = stringOrNumber(text)
	return nil
}

// numericOrUnix 读取绝对时间戳；看起来是相对值的小数字按「现在 + 时长」
// 处理。整数、带引号整数与浮点都接受；刷新/轮询端点也观察到过回答
// RFC3339 时间戳的情形。不可解析的值为 0。
func numericOrUnix(absolute stringOrNumber, relative int64, now time.Time) int64 {
	text := strings.TrimSpace(string(absolute))
	if text == "" {
		if relative > 0 {
			return now.Unix() + relative
		}
		return 0
	}
	var value int64
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		floatValue, floatErr := strconv.ParseFloat(text, 64)
		if floatErr != nil || !isFinite(floatValue) {
			// 不是数字——最后一种被接受的写法是 RFC3339 墙钟。
			if ts, terr := time.Parse(time.RFC3339, text); terr == nil {
				return ts.Unix()
			}
			return 0
		}
		value = int64(floatValue)
	} else {
		value = parsed
	}
	if value >= unixEpochFloor {
		return value
	}
	if value > 0 {
		return now.Unix() + value
	}
	return 0
}

func isFinite(value float64) bool {
	return !math.IsInf(value, 0) && !math.IsNaN(value)
}

// unixEpochFloor 区分 10 位绝对 unix 秒（2001-09-09 之后）与较小的相对时长。
const unixEpochFloor = int64(1_000_000_000)

// refreshResponse 对应 /api/v1/deviceToken/refresh 载荷。
type refreshResponse struct {
	DeviceToken           string         `json:"device_token"`
	Token                 string         `json:"token"`
	AccessToken           string         `json:"access_token"`
	RefreshToken          string         `json:"refresh_token"`
	ExpiresAt             stringOrNumber `json:"expires_at"`
	RefreshTokenExpiresAt stringOrNumber `json:"refresh_token_expires_at"`
}

// sessionRefreshPath 是轮转设备 token 的 OpenAPI 端点。
func sessionRefreshPath(profile Profile) string {
	return strings.TrimRight(profile.OpenAPIBase, "/") + "/api/v1/deviceToken/refresh"
}

// Refresh 用 refreshToken 换取新的设备 token 对。两个 token 在服务端轮换，
// 调用方必须把非空值都持久化。
func Refresh(ctx context.Context, hc *http.Client, profile Profile, refreshToken, machineID string) (Token, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return Token{}, errors.New("refresh_token 为空，请重新登录")
	}
	body := map[string]string{"refresh_token": refreshToken}
	if machineID = strings.TrimSpace(machineID); machineID != "" {
		body["machine_id"] = machineID
	}
	var lastErr error
	for attempt := 0; attempt < refreshAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepWithContext(ctx, time.Duration(attempt)*time.Second); err != nil {
				return Token{}, err
			}
		}
		payload, err := postJSON(ctx, hc, sessionRefreshPath(profile), body)
		if err != nil {
			var apiErr apiError
			if errors.As(err, &apiErr) && !apiErr.retryable() {
				if apiErr.status == http.StatusUnauthorized || apiErr.status == http.StatusForbidden {
					return Token{}, fmt.Errorf("Qoder 登录已失效，请重新登录")
				}
				return Token{}, err
			}
			lastErr = err
			continue
		}
		tokenValue := payload.DeviceToken
		if tokenValue == "" {
			tokenValue = payload.Token
		}
		if tokenValue == "" {
			tokenValue = payload.AccessToken
		}
		if tokenValue == "" {
			return Token{}, errors.New("Qoder 刷新响应缺少 device_token")
		}
		now := time.Now()
		token := Token{AccessToken: tokenValue, RefreshToken: payload.RefreshToken}
		if unix := numericOrUnix(payload.ExpiresAt, 0, now); unix > 0 {
			token.ExpiresAt = unix
		}
		if unix := numericOrUnix(payload.RefreshTokenExpiresAt, 0, now); unix > 0 {
			token.RefreshTokenExpiresAt = unix
		}
		if token.RefreshToken == "" {
			token.RefreshToken = refreshToken // 服务端可能复用 token
		}
		return token, nil
	}
	if lastErr == nil {
		lastErr = errors.New("Qoder 刷新登录失败")
	}
	return Token{}, lastErr
}

type apiError struct {
	status int
	detail string
}

func (e apiError) Error() string {
	return fmt.Sprintf("Qoder 接口请求失败 (HTTP %d): %s", e.status, e.detail)
}

func (e apiError) retryable() bool {
	return e.status >= 500
}

func postJSON(ctx context.Context, hc *http.Client, endpoint string, body any) (refreshResponse, error) {
	var payload refreshResponse
	raw, err := json.Marshal(body)
	if err != nil {
		return payload, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return payload, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return payload, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return payload, err
	}
	if resp.StatusCode != http.StatusOK {
		return payload, apiError{status: resp.StatusCode, detail: truncateForError(data)}
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

// truncateForError 保留错误正文的前 512 个 rune，中文服务端消息不会切在
// 半个字符上。
func truncateForError(body []byte) string {
	text := strings.TrimSpace(string(body))
	runes := []rune(text)
	if len(runes) > 512 {
		runes = runes[:512]
	}
	return strings.TrimSpace(string(runes))
}

// ─────────────────────────────────────────────────────────────────────────────
// PKCE (S256) — verifier 字母表取自 CLI（RFC 7636 base64url 集）
// ─────────────────────────────────────────────────────────────────────────────

const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// pkceVerifier 复刻官方 CLI 的 43–128 字符窗口（RFC 7636 字母表），但用
// 拒绝采样保证均匀——裸字节取模 66 会让前 58 个字母多出最多三分之一的
// 偏重，而 RFC 7636 要求均匀 verifier。
func pkceVerifier() (string, error) {
	length := 43 + int(randByte()%86)
	buf := make([]byte, length+length/2+8) // 约 1.5 次抽取/字符，足够覆盖拒绝采样
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 0, length)
	const alphabetMax = byte(198) // 3 × 66 个可接受值
	for _, b := range buf {
		if b < alphabetMax {
			out = append(out, pkceAlphabet[b%byte(len(pkceAlphabet))])
			if len(out) == length {
				break
			}
		}
	}
	for len(out) < length {
		// 抽取量估计 >1.5× 时几乎不可达；补齐而不是返回短 verifier。
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < alphabetMax {
				out = append(out, pkceAlphabet[b%byte(len(pkceAlphabet))])
				if len(out) == length {
					break
				}
			}
		}
	}
	return string(out), nil
}

func randByte() byte {
	buf := make([]byte, 1)
	if _, err := rand.Read(buf); err != nil {
		return 0
	}
	return buf[0]
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
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
