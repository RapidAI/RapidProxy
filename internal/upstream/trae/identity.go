package trae

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// AccountIdentity 从账号 user id 派生稳定的设备指纹。上游风控会打击
// 一个 machine id 挂 N 个号的行为，所以身份以账号 user id 为键，
// 不随访问令牌轮换。
type AccountIdentity struct {
	UserID    string
	MachineID string // 64 位 hex
	DeviceID  string // 十进制字符串，SOLO 设备 id 形态
}

// IdentityForUserID 派生一个账号的 machine/device 对。摘要带密钥
// （不是裸哈希），避免派生结果与其它工具撞车。
func IdentityForUserID(userID string) AccountIdentity {
	uid := strings.TrimSpace(userID)
	digest := func(tag string) []byte {
		mac := hmac.New(sha256.New, []byte("rapidproxy-trae-"+tag))
		mac.Write([]byte(uid))
		return mac.Sum(nil)
	}
	machine := hex.EncodeToString(digest("machine"))
	return AccountIdentity{
		UserID:    uid,
		MachineID: machine,
		DeviceID:  hashDeviceID(machine),
	}
}

// hashDeviceID 把 machine id 折叠成 SOLO 客户端的十进制设备 id 形态：
// 固定 16 位数字，首位 1-9（不补零、不超过 17 位）。
func hashDeviceID(machineID string) string {
	sum := sha256.Sum256([]byte(machineID))
	var value uint64
	for _, b := range sum[:8] {
		value = value<<8 | uint64(b)
	}
	value %= 9_000_000_000_000_000
	value += 1_000_000_000_000_000
	return decimal(value)
}

func decimal(value uint64) string {
	if value == 0 {
		return "0"
	}
	var raw [20]byte
	index := len(raw)
	for value > 0 {
		index--
		raw[index] = byte('0' + value%10)
		value /= 10
	}
	return string(raw[index:])
}

// randomHex 返回 2n 个 hex 字符；失败返回空串（登录无法在没有随机位的
// 情况下继续，调用方以此判断失败）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// jwtClaims 无签名校验地解出访问令牌的载荷——claims 只用于找回账号 ID
// （跨令牌轮换稳定）。
type jwtClaims struct {
	UID        string
	UserID     string
	Enterprise string
	Region     string
}

func parseJWTClaims(token string) jwtClaims {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return jwtClaims{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return jwtClaims{}
	}
	// claims 可能是字符串或数字，取决于部署。
	claims := jwtClaims{
		UID:        firstNonEmptyStringClaim(payload["uid"], payload["userId"], payload["user_id"], payload["sub"]),
		UserID:     firstNonEmptyStringClaim(payload["userId"], payload["user_id"]),
		Enterprise: stringClaim(payload["enterprise_id"]),
	}
	if dataRaw, ok := payload["data"].(map[string]any); ok {
		claims.UserID = firstNonEmptyStringClaim(dataRaw["id"], dataRaw["uid"], dataRaw["user_id"])
		claims.Enterprise = strings.TrimSpace(stringClaim(dataRaw["enterprise_id"]))
	}
	if strings.TrimSpace(claims.UID) == "" {
		claims.UID = claims.UserID
	}
	var region struct {
		Region string `json:"region"`
	}
	if raw, ok := payload["userRegion"].(map[string]any); ok {
		if json.Unmarshal(mustJSONValue(raw), &region) == nil {
			claims.Region = strings.TrimSpace(region.Region)
		}
	}
	return claims
}

func mustJSONValue(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// firstNonEmptyStringClaim 接受字符串或数字型 JSON 值，返回第一个非空。
func firstNonEmptyStringClaim(values ...any) string {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		case float64:
			if v > 0 {
				return decimal(uint64(v))
			}
		}
	}
	return ""
}

func stringClaim(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
