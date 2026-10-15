// Package opencode 实现 OpenCode Zen（opencode.ai）协议：
//
//	GET  {base}/models              模型目录（OpenAI 兼容形状）
//	POST {base}/chat/completions    对话（OpenAI 兼容形状）
//
// 登录是 API Key 制：官方密钥页 opencode.ai/auth 颁发，也可从本机已安装的
// OpenCode CLI 凭据（auth.json / OPENCODE_API_KEY 环境变量）导入。
//
// 官方网关（Zen 免费层）会校验客户端身份，连线时必须携带两个头：
//
//	User-Agent: opencode/1.18.31
//	  裸的 "opencode" 会被拒；版本号必须是不低于约 1.17.0 的正式 semver。
//	 x-opencode-session: ses_ + 12 位十六进制时间 + 14 位 base62
//	  同一段对话在一个小时内用同一个会话号；UUID 会被拒。
//
// 改写只发生在连 OpenCode（opencode.ai 及其子域）的时候：设置里保存的
// User-Agent 仍然是裸 "opencode"，照原样落到其它上游——例如按精确匹配
// 识别 opencode 客户端的 GLM，看到的值不受影响。
// x-opencode-client 与 x-opencode-request 官方网关不校验，这里不加。
package opencode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ClientVersion 是官方客户端 User-Agent 里的 semver
	//（opencode 源码 session/llm/request.ts 的 opencode/${InstallationVersion}）。
	// Zen 免费层网关拒绝裸 "opencode" 与任何低于 1.17.0 的版本号。
	ClientVersion = "1.18.31"

	// SessionHeader 是官方客户端携带的会话亲和头。
	SessionHeader = "x-opencode-session"

	// PresetUserAgent 是设置里保存的身份：裸 opencode。
	PresetUserAgent = "opencode"

	// ClientUserAgent 是连线时实际发送的 User-Agent。
	ClientUserAgent = PresetUserAgent + "/" + ClientVersion

	// APIBase 是 OpenAI 兼容端点基址；AuthURL 是官方密钥页。
	APIBase = "https://opencode.ai/zen/v1"
	// AuthURL 是领取 API Key 的官方页面。
	AuthURL = "https://opencode.ai/auth"

	// base62Chars 与官方 Identifier.create 的字符表一致。
	base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// seedByteLimit 限制对话指纹的长度，避免把整段历史都哈希进去。
	seedByteLimit = 8192
)

// IsOpenCodeHost 报告主机名是否属于 OpenCode（opencode.ai 或其子域）。
// 容忍 FQDN 的尾点（"opencode.ai."）：url.Hostname() 不会替我们去掉了。
func IsOpenCodeHost(hostname string) bool {
	host := strings.ToLower(strings.TrimSpace(hostname))
	host = strings.TrimSuffix(host, ".")
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

// IsOpenCodeURL 报告 rawURL 是否连往 OpenCode。
func IsOpenCodeURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	return IsOpenCodeHost(u.Hostname())
}

// WireUserAgent 返回连线时实际使用的 User-Agent。
//
// 保存的预设身份（空或裸 "opencode"，大小写不敏感）改写成官方客户端的
// opencode/<semver>；自定义身份（包括用户自己写的 opencode/x.y.z）保持原样。
func WireUserAgent(saved string) string {
	saved = strings.TrimSpace(saved)
	if saved == "" || strings.EqualFold(saved, PresetUserAgent) {
		return ClientUserAgent
	}
	return saved
}

// ClientSessionID 生成官方形状的会话号：ses_ + 12 位十六进制时间 + 14 位
// base62。同一组 parts 在同一个 UTC 小时内得到同一个 ID——时间部分按整点
// 截断，随机部分由 parts 的哈希决定，因此一段对话的各轮请求会话号一致，
// 跨小时自动换新。parts 全为空时退化为随机 ID（请求仍会被网关接受）。
func ClientSessionID(parts ...string) string {
	return clientSessionIDAt(time.Now().UTC().Truncate(time.Hour), parts...)
}

func clientSessionIDAt(now time.Time, parts ...string) string {
	h := sha256.New()
	wrote := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if wrote {
			h.Write([]byte{0})
		}
		h.Write([]byte(part))
		wrote = true
	}
	if !wrote {
		var rnd [16]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			// crypto/rand 不可用时的兜底：用时间戳派生，保证仍是一个合法 ID。
			sum := sha256.Sum256([]byte(now.Format(time.RFC3339Nano)))
			copy(rnd[:], sum[:])
		}
		h.Write(rnd[:])
	}
	sum := h.Sum(nil)
	random := make([]byte, 14)
	same := base62Chars[int(sum[0])%len(base62Chars)]
	allSame := true
	for i := range random {
		ch := base62Chars[int(sum[i%len(sum)])%len(base62Chars)]
		random[i] = ch
		if ch != same {
			allSame = false
		}
	}
	if allSame {
		// 14 个字符完全相同虽合法但可疑，拧掉最后一个保持随机外观。
		random[len(random)-1] = base62Chars[(int(same)+1)%len(base62Chars)]
	}
	return "ses_" + sessionTimeHex(now.UnixMilli()) + string(random)
}

// sessionTimeHex 按官方 Identifier.create 的方式编码时间：(毫秒 << 12) 存成
// 6 个大端字节，即 12 个十六进制字符。
func sessionTimeHex(ms int64) string {
	shifted := uint64(ms) << 12
	var raw [6]byte
	for i := 0; i < len(raw); i++ {
		raw[i] = byte((shifted >> (40 - 8*uint(i))) & 0xff)
	}
	return hex.EncodeToString(raw[:])
}

// IsOfficialSessionID 报告 id 是否已是官方形状（ses_ + 12 位十六进制 +
// 14 位 base62）。UUID 等其它形状都不是。
func IsOfficialSessionID(id string) bool {
	const hexLen, tailLen = 12, 14
	if len(id) != len("ses_")+hexLen+tailLen || !strings.HasPrefix(id, "ses_") {
		return false
	}
	body := id[len("ses_"):]
	for i := 0; i < hexLen; i++ {
		c := body[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	for i := hexLen; i < hexLen+tailLen; i++ {
		c := body[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// WireSessionID 返回连线时实际使用的 x-opencode-session 值。
//
// 已是官方形状的 ID 原样保留；其它值（含 UUID）折叠成由它派生的官方 ID，
// 同一段对话因此在一小时内落在同一个会话上；空值直接生成新的官方 ID。
func WireSessionID(saved string) string {
	saved = strings.TrimSpace(saved)
	switch {
	case IsOfficialSessionID(saved):
		return saved
	case saved == "":
		return ClientSessionID()
	default:
		return ClientSessionID(saved)
	}
}

// ConversationSeed 从 OpenAI 请求体提取首个 user 消息的文本，作为对话指纹。
//
// 同一段对话的每一轮都带同样的首条 user 消息，因此一小时内的会话号稳定；
// 提取不到（没有 user 消息或不是合法 JSON）时返回空串。
func ConversationSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return seedFromMessages(payload["messages"])
}

func seedFromMessages(messages any) string {
	list, ok := messages.([]any)
	if !ok {
		return ""
	}
	for _, item := range list {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		if seed := stringifyContent(msg["content"]); seed != "" {
			return seed
		}
	}
	return ""
}

// stringifyContent 把 user 消息的 content 摊平成文本：纯字符串、分段数组
// （{type:text,text:...}）或嵌套 content 都支持。
func stringifyContent(value any) string {
	switch v := value.(type) {
	case string:
		return truncateSeed(v)
	case []any:
		var b strings.Builder
		for _, part := range v {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				if text, ok := p["text"].(string); ok {
					b.WriteString(text)
					continue
				}
				b.WriteString(stringifyContent(p["content"]))
			}
			if b.Len() >= seedByteLimit {
				break
			}
		}
		return truncateSeed(b.String())
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return truncateSeed(text)
		}
		return stringifyContent(v["content"])
	}
	return ""
}

// truncateSeed 按 UTF-8 边界截断对话指纹，避免把多字节字符切成乱码。
func truncateSeed(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= seedByteLimit {
		return s
	}
	cut := seedByteLimit
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut]
}

// ApplyWireHeaders 在请求连往 OpenCode 时补齐官方客户端身份。
//
// 非 OpenCode 主机直接返回、一个字节都不改：设置里保存的裸 "opencode"
// 由此原样发给其它上游（GLM 的精确匹配不受影响）。savedUA 是设置里保存的
// User-Agent，seed 是对话指纹（见 ConversationSeed）。
func ApplyWireHeaders(req *http.Request, savedUA, seed string) {
	if req == nil || req.URL == nil || !IsOpenCodeHost(req.URL.Hostname()) {
		return
	}
	req.Header.Set("User-Agent", WireUserAgent(savedUA))
	req.Header.Set(SessionHeader, WireSessionID(seed))
}
