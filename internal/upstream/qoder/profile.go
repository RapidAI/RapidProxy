// Package qoder 实现官方 qoder CLI 发布的设备登录与签名对话协议：
//
//	浏览器设备登录携带 S256 PKCE challenge，CLI 每秒轮询 OpenAPI 主机直到
//	批准落地为 token；/algo 推理请求需要官方 wasm 生成的 COSY 签名头族
//	（cosy 子包在 wazero 沙箱里运行官方 qoder_auth_wasm），POST 体由同一
//	wasm 自定义编码。响应是 SSE 信封（body 字段包裹标准 OpenAI chunk），
//	本包把它解回标准 OpenAI 流。
package qoder

import (
	"strings"
	"time"
)

const (
	// NameCN / NameGlobal 是国内/国际版显示名。
	NameCN     = "Qoder 国内版"
	NameGlobal = "Qoder 国际版"

	// StoreCN / StoreGlobal 是凭据存储标识。
	StoreCN     = "qoder-cn"
	StoreGlobal = "qoder-global"

	// DeviceClientID 是双版共用的公开设备登录 client。qoderclicn 1.1.64
	// （qoder.cn）与 qodercli 1.1.65（qoder.com）都用 e883ade2 打开
	// selectAccounts；旧的 e93fe488 已不被 qoder.cn 接受（参数无效）。
	DeviceClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

	// ClientVersion 对齐官方 CLI 版本号以便上游区分客户端家族。
	ClientVersion = "1.1.64"
	UserAgent     = "qoder/" + ClientVersion

	// DefaultModel 是官方 CLI 的硬编码兜底模型（Qwen 3.8 Max，1M 上下文）。
	// 实际对话 key 是 qmodel_38max，见 canonicalQoderModelKey。
	DefaultModel = "qwen3.8-max"

	// ChatBase 是双版共享的模型服务器（官方 CLI 从固定表取，而非站点主机）。
	ChatBase = "https://api2-v2.qoder.sh/model/v1"

	pollLifetime    = 5 * time.Minute // 服务端批准窗口
	refreshAttempts = 3
)

// LoginLifetime 是浏览器批准可以等待的时长。
func LoginLifetime() time.Duration { return pollLifetime }

// Profile 是一个上游版（国内/国际）。
type Profile struct {
	ID          string
	Name        string
	StoreID     string
	WebOrigin   string // 浏览器登录页 origin
	OpenAPIBase string // 账号/令牌 API（设备轮询与刷新）
	InferBase   string // 模型目录主机
	// ChatOrigin 是签名 /algo agent_chat_generation 主机。国内版用推理网关，
	// 国际版用 api3.qoder.sh。不是共享的 OpenAI 前端（ChatBase）——那个主机
	// 不服务 /algo，对它发对话会得到 HTTP 404。
	ClientID   string
	ChatOrigin string
}

// CNProfile 是国内版（qoder.cn）。
func CNProfile() Profile {
	return Profile{
		ID:          "cn",
		Name:        NameCN,
		StoreID:     StoreCN,
		WebOrigin:   "https://qoder.cn",
		OpenAPIBase: "https://openapi.qoder.com.cn",
		InferBase:   "https://gateway.qoder.com.cn",
		ChatOrigin:  "https://gateway.qoder.com.cn",
		ClientID:    DeviceClientID,
	}
}

// GlobalProfile 是国际版（qoder.com）。
func GlobalProfile() Profile {
	return Profile{
		ID:          "global",
		Name:        NameGlobal,
		StoreID:     StoreGlobal,
		WebOrigin:   "https://qoder.com",
		OpenAPIBase: "https://openapi.qoder.sh",
		InferBase:   "https://api2.qoder.sh",
		ChatOrigin:  "https://api3.qoder.sh",
		ClientID:    DeviceClientID,
	}
}

// ProfileByStoreID 按凭据存储标识解析版。
func ProfileByStoreID(id string) (Profile, bool) {
	switch strings.TrimSpace(id) {
	case StoreCN:
		return CNProfile(), true
	case StoreGlobal:
		return GlobalProfile(), true
	default:
		return Profile{}, false
	}
}

// canonicalQoderModelKey 把展示型旧名映射到真实目录 key；未知名透传
// （agent_chat 节点会校验）。
func canonicalQoderModelKey(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "qwen3.8-max", "qwen 3.8 max", "qwen3.8 max":
		return "qmodel_38max"
	case "qwen3.8-flash", "qwen 3.8 flash", "qwen3.8 flash":
		return "qfmodel"
	default:
		return strings.TrimSpace(model)
	}
}
