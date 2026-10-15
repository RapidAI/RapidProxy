package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Model 是一条 OpenCode Zen 目录条目。
type Model struct {
	ID   string
	Name string
}

// Chat 把 OpenAI 形状的请求体转发到 OpenCode Zen 对话端点。
//
// base 是配置里的上游基址（默认 https://opencode.ai/zen/v1）；只有连往
// OpenCode 主机时才会带上官方客户端身份（见 ApplyWireHeaders）。请求体由
// 代理服务层原样交本适配层（该层恒定 stream:true），这里不再改动。
func Chat(ctx context.Context, hc *http.Client, base, apiKey, savedUA string, body []byte) (*http.Response, error) {
	req, err := BuildChatRequest(ctx, base, apiKey, savedUA, body)
	if err != nil {
		return nil, err
	}
	return hc.Do(req)
}

// BuildChatRequest 构造连往 base 的对话请求，并补齐官方客户端身份。
func BuildChatRequest(ctx context.Context, base, apiKey, savedUA string, body []byte) (*http.Request, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(base), "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream, */*")
	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// 先按设置里保存的身份发送（与其它协议族的 commonHeaders 一致）；
	// 只有连往 OpenCode 主机时，ApplyWireHeaders 才把它改写成官方客户端
	// 形态——裸 "opencode" 会被网关拒绝，而 GLM 那条精确匹配要的正是原值。
	if ua := strings.TrimSpace(savedUA); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	ApplyWireHeaders(req, savedUA, ConversationSeed(body))
	return req, nil
}

// ListModels 读取 OpenCode Zen 模型目录（OpenAI 兼容形状）。
func ListModels(ctx context.Context, hc *http.Client, base, apiKey, savedUA string) ([]Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("需要已登录的访问令牌")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	endpoint := strings.TrimRight(strings.TrimSpace(base), "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if ua := strings.TrimSpace(savedUA); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	ApplyWireHeaders(req, savedUA, "")

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
		// 401/403 是「密钥不认」的明确信号：包上 ErrKeyRejected，让登录流程
		// 能给出针对性提示，而不是把用户留给首次对话时的一句「认证失败」。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %w", ErrKeyRejected, statusErr)
		}
		return nil, statusErr
	}
	var envelope struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// 容错：个别兼容端点直接返回数组。
		var list []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return nil, fmt.Errorf("模型目录响应无法解析: %w", err)
		}
		envelope.Data = list
	}
	models := make([]Model, 0, len(envelope.Data))
	for _, entry := range envelope.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = id
		}
		models = append(models, Model{ID: id, Name: name})
	}
	return models, nil
}

// -----------------------------------------------------------------------------
// API Key 登录
// -----------------------------------------------------------------------------

// LocalKey 尝试从本机已安装的 OpenCode CLI 导入密钥：OPENCODE_API_KEY 环境
// 变量优先，其次依次检查各候选位置的 auth.json。找不到返回空串。
func LocalKey() string {
	if key := strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")); key != "" {
		return key
	}
	for _, path := range authCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var auth map[string]any
		if json.Unmarshal(data, &auth) != nil || auth == nil {
			continue
		}
		if key := extractKey(auth); key != "" {
			return key
		}
	}
	return ""
}

// authCandidates 列出 OpenCode CLI 可能保存 auth.json 的位置（与官方
// XDG / Windows 约定一致）。
func authCandidates() []string {
	var paths []string
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		add(filepath.Join(xdg, "opencode", "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".local", "share", "opencode", "auth.json"))
		add(filepath.Join(home, ".opencode", "auth.json"))
	}
	if appdata := strings.TrimSpace(os.Getenv("APPDATA")); appdata != "" {
		add(filepath.Join(appdata, "opencode", "auth.json"))
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		add(filepath.Join(local, "opencode", "auth.json"))
	}
	return paths
}

// extractKey 从 auth.json 的内容里取 Zen 密钥：官方形态是
// {"opencode": {"type": "api", "key": "..."}}，也兼容几个常见字段名。
func extractKey(auth map[string]any) string {
	if provider, ok := auth["opencode"].(map[string]any); ok {
		for _, name := range []string{"key", "apiKey", "access", "access_token"} {
			if key := strings.TrimSpace(stringValue(provider[name])); key != "" {
				return key
			}
		}
	}
	return strings.TrimSpace(stringValue(auth["OPENCODE_API_KEY"]))
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// ErrKeyInvalid 表示用户提交的文本无法解析成合法形态的 API Key。上层可
// 用它区分「密钥格式不对」与其它错误，给出本地化提示。
var ErrKeyInvalid = errors.New("invalid api key")

// ErrKeyRejected 表示上游明确拒绝了密钥（401/403）。与 ErrKeyInvalid 的
// 区别：格式没问题，但网关不认——通常是密钥输错、已吊销或欠费。
var ErrKeyRejected = errors.New("api key rejected")

// StatusError 是上游返回的非 200 响应，携带状态码与正文摘要。
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.Code, strings.TrimSpace(e.Body))
}

// minKeyLength 是 Zen 密钥的最小可接受长度：短于这个值的一律视为粘贴噪声。
const minKeyLength = 12

// NormalizeKey 清洗用户粘贴的密钥：去掉首尾空白与引号、兼容整段 JSON 粘贴
// 与 "bearer " 前缀。过短、含空白、或是取不出密钥的 JSON，都视为无效。
func NormalizeKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	key = strings.Trim(key, `"'`)
	// 整段 JSON 粘贴（官方 auth.json 或从文档里复制的片段）：必须能取出
	// 已知字段，否则这段 JSON 本身就是粘贴噪声，不能原样当成密钥存下去。
	if strings.HasPrefix(key, "{") || strings.HasPrefix(key, "[") {
		extracted := keyFromJSON(key)
		if extracted == "" {
			return "", ErrKeyInvalid
		}
		key = strings.Trim(strings.TrimSpace(extracted), `"'`)
	}
	if len(key) >= 7 && strings.EqualFold(key[:7], "bearer ") {
		key = strings.Trim(strings.TrimSpace(key[7:]), `"'`)
	}
	if len(key) < minKeyLength || strings.ContainsAny(key, " \t\r\n") {
		return "", ErrKeyInvalid
	}
	return key, nil
}

// keyFromJSON 从粘贴的 JSON 里取密钥：对象按已知字段名查（官方形态是
// {"opencode": {...}}），数组逐个元素找。
func keyFromJSON(raw string) string {
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	return keyFromValue(payload)
}

func keyFromValue(value any) string {
	switch v := value.(type) {
	case map[string]any:
		if nested, ok := v["opencode"].(map[string]any); ok {
			if key := firstKey(nested); key != "" {
				return key
			}
		}
		return firstKey(v)
	case []any:
		for _, item := range v {
			if key := keyFromValue(item); key != "" {
				return key
			}
		}
	}
	return ""
}

func firstKey(fields map[string]any) string {
	for _, name := range []string{"key", "apiKey", "access", "access_token", "OPENCODE_API_KEY"} {
		if key := strings.TrimSpace(stringValue(fields[name])); key != "" {
			return key
		}
	}
	return ""
}

// KeyLabel 是账号列表里展示的密钥备注，只露出末尾 4 位。
func KeyLabel(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return "OpenCode Key"
	}
	return "OpenCode · " + key[len(key)-4:]
}
