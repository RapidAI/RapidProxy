package upstream

// 会话头补全：www.workbuddy.ai 的对话网关要求每次对话携带会话标识
//（hy3/deepseek 缺失时上游以 code=11128 "conversation id missing" 拒绝，
// 官方客户端对全部模型的每次调用都带）。这里按官方客户端形态对国际版
// 全部对话补齐。逻辑与 MaClaw（CodeClaw）仓库 corelib/workbuddy 中
// 已验证的实现保持一致。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// conversationHeaderNames 是国际版要求的四个会话标识头。
var conversationHeaderNames = []string{
	"X-Conversation-ID",
	"X-Conversation-Request-ID",
	"X-Conversation-Message-ID",
	"X-Request-ID",
}

// applyConversationHeaders 为请求补齐四个会话标识头。请求上已有完整标识时
// 保留原值（下游客户端自己管理会话时以它为准）；缺哪个补哪个，每次调用
// 独立生成——不同调用方不得共用一个会话。
func applyConversationHeaders(h http.Header) {
	if h == nil || conversationHeadersComplete(h) {
		return
	}
	raw := conversationEntropy()
	setHeaderIfEmpty(h, "X-Conversation-ID", formatUUID(raw[0:16]))
	setHeaderIfEmpty(h, "X-Conversation-Request-ID", hex.EncodeToString(raw[16:32]))
	setHeaderIfEmpty(h, "X-Conversation-Message-ID", hex.EncodeToString(raw[32:48]))
	setHeaderIfEmpty(h, "X-Request-ID", hex.EncodeToString(raw[48:64]))
}

func conversationHeadersComplete(h http.Header) bool {
	for _, name := range conversationHeaderNames {
		if strings.TrimSpace(h.Get(name)) == "" {
			return false
		}
	}
	return true
}

func setHeaderIfEmpty(h http.Header, name, value string) {
	if strings.TrimSpace(h.Get(name)) != "" || strings.TrimSpace(value) == "" {
		return
	}
	h.Set(name, value)
}

// conversationEntropy 取 64 字节熵源：优先 crypto/rand；系统熵池不可用时
// 以时间纳秒派生兜底，保证四个头始终有值可填。
func conversationEntropy() []byte {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err == nil {
		return buf
	}
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	for i := 0; i < 4; i++ {
		sum := sha256.Sum256([]byte(now + ":" + strconv.Itoa(i)))
		copy(buf[i*16:], sum[:16])
	}
	return buf
}

// formatUUID 把 16 字节格式化成 RFC 4122 v4 形态的 UUID 字符串。
func formatUUID(b []byte) string {
	var u [16]byte
	copy(u[:], b)
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(u[0:4]),
		hex.EncodeToString(u[4:6]),
		hex.EncodeToString(u[6:8]),
		hex.EncodeToString(u[8:10]),
		hex.EncodeToString(u[10:16]))
}
