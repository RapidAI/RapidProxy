// Package trae 实现 Trae（国内/国际双区）的浏览器设备登录与 SOLO agent 对话。
//
// 登录走控制台 native_ide 流程：浏览器授权页重定向回本地回调，带回
// refreshToken（或 userJwt JSON），再到账号主机换取 JWT 访问令牌。对话是
// 非开放协议（llm_utils_chat + Cloud-IDE-JWT 头族），由本包把 OpenAI 请求
// 翻译成 SOLO 形状、把上游事件流翻译回 OpenAI chunk。
//
// 协议要点（2026-10-08 真机校准）：
//   - 授权 URL 为 18 参数、不带 PKCE challenge 的简化契约；带 challenge 会
//     被引入需要设备证明的新协议（国内 400/10101、国际 401/20405）。
//   - 刷新令牌系于登录页的 machine/device 设备对，聊天头必须复用同一对。
//   - 国内版交换接口双 host 并存（api.trae.cn / api.trae.com.cn），按序探测。
package trae

import "time"

const (
	// NameCN / NameGlobal 是国内/国际版显示名。
	NameCN     = "Trae 国内版"
	NameGlobal = "Trae 国际版"

	// StoreCN / StoreGlobal 是凭据存储标识。
	StoreCN     = "trae-cn"
	StoreGlobal = "trae-global"

	// SOLOClientID 是公开的 SOLO 产品线 OAuth client，无 secret，双区共用。
	SOLOClientID = "en1oxy7wnw8j9n"

	// AppID 是对话请求 x-app-id 头携带的 SOLO 应用标识。
	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"

	// UserAgentPrefix 构造 "Trae/<版本>" 请求身份。
	UserAgentPrefix = "Trae/"

	// LoginPluginVersion 对齐社区抓包里授权 URL 的 tronBuildVersion。
	LoginPluginVersion = "2.3.62834"

	// AuthorizePath 是控制台授权页。
	AuthorizePath = "/authorization"

	// CallbackPath 是授权 URL 里写死的本地重定向端点。
	CallbackPath = "/authorize"

	// CallbackPort 是本地重定向默认端口（被占用时上移）。
	CallbackPort = 18080
	// ProbePort 是控制台授权页探测本地客户端的固定端口。
	ProbePort = 17388

	// LoginLifetime 限定一次浏览器授权的等待窗口。
	LoginLifetime = 10 * time.Minute

	// ExchangePath 把 refreshToken 换成新访问令牌（旧契约）。
	ExchangePath = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	// UserInfoPath 在新 JWT 之后读取账号资料。
	UserInfoPath = "/cloudide/api/v3/trae/GetUserInfo"

	// ChatPath 是 SOLO agent 对话端点。
	ChatPath = "/api/agent/v3/llm_utils_chat"
	// ModelsPath 是 SOLO 模型目录端点。
	ModelsPath = "/api/ide/v1/get_detail_param"
)

// Profile 是一个上游区（国内/国际）。
type Profile struct {
	ID             string
	Name           string
	StoreID        string
	ConsoleBase    string // 浏览器授权页 origin
	AuthBase       string // 账号/令牌 API（交换、刷新、用户信息）
	AuthAltBase    string // 国内版交换+用户信息在第二个主机上并存
	ChatHost       string // SOLO agent 对话主机
	ClientID       string
	IDEVersion     string
	IDEVersionCode string
	// ClientVersion 是模型主机认可的 SOLO 客户端版本：llm_utils_chat 与
	// get_detail_param 只对足够新的客户端提供目录，与授权页上的 IDE 版本
	// 不是同一个客户端。
	ClientVersion     string
	ClientVersionCode string
	DefaultModel      string
}

// AuthHosts 按探测顺序列出可承载令牌交换/用户信息的账号主机。
// 国内版的旧交换契约在 api.trae.cn 与 api.trae.com.cn 上并存。
func (p Profile) AuthHosts() []string {
	hosts := make([]string, 0, 2)
	if p.AuthBase != "" {
		hosts = append(hosts, p.AuthBase)
	}
	if p.AuthAltBase != "" {
		dup := false
		for _, host := range hosts {
			if host == p.AuthAltBase {
				dup = true
			}
		}
		if !dup {
			hosts = append(hosts, p.AuthAltBase)
		}
	}
	return hosts
}

// CNProfile 是国内区。
func CNProfile() Profile {
	return Profile{
		ID:                "cn",
		Name:              NameCN,
		StoreID:           StoreCN,
		ConsoleBase:       "https://www.trae.cn",
		AuthBase:          "https://api.trae.cn",
		AuthAltBase:       "https://api.trae.com.cn",
		ChatHost:          "https://trae-api-cn.mchost.guru",
		ClientID:          SOLOClientID,
		IDEVersion:        "3.3.67",
		IDEVersionCode:    "20260401",
		ClientVersion:     "0.1.69",
		ClientVersionCode: "20260917",
		DefaultModel:      "glm-5.2",
	}
}

// GlobalProfile 是国际区（新加坡部署）。
func GlobalProfile() Profile {
	return Profile{
		ID:                "global",
		Name:              NameGlobal,
		StoreID:           StoreGlobal,
		ConsoleBase:       "https://www.trae.ai",
		AuthBase:          "https://growsg-normal.trae.ai",
		AuthAltBase:       "",
		ChatHost:          "https://coresg-normal.trae.ai",
		ClientID:          SOLOClientID,
		IDEVersion:        "3.5.51",
		IDEVersionCode:    "20260401",
		ClientVersion:     "0.1.69",
		ClientVersionCode: "20260917",
		DefaultModel:      "gpt-5",
	}
}

// ProfileByStoreID 按凭据存储标识解析区。
func ProfileByStoreID(id string) (Profile, bool) {
	switch id {
	case StoreCN:
		return CNProfile(), true
	case StoreGlobal:
		return GlobalProfile(), true
	default:
		return Profile{}, false
	}
}
