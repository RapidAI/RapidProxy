// Package lobsterai 实现网易有道 LobsterAI（龙虾）桌面客户端协议：
//
//	浏览器登录回调带回授权码（本地 127.0.0.1 监听 + state 校验）
//	POST /api/auth/exchange   authCode 换取 Bearer 账号（双 token）
//	POST /api/auth/refresh    刷新访问令牌（双 token 轮换）
//	GET  /api/models/available 模型目录
//	POST /api/proxy/v1/chat/completions  对话（标准 OpenAI 形状，仅流式）
//
// 对话响应是标准 OpenAI SSE，但业务错误（额度 40201、未知模型 40300 等）
// 藏在 HTTP 200 的流首，由 streamerror 过滤器翻译成 OpenAI error 帧。
package lobsterai

import "time"

const (
	// StoreID 是凭据存储标识（账号目录名）。
	StoreID = "lobsterai"

	// PortalBase 是浏览器登录页。
	PortalBase = "https://lobsterai.youdao.com"
	// APIBase 是账号与对话后端。
	APIBase = "https://lobsterai-server.youdao.com"

	// LoginPath 是桌面客户端打开的 SPA 路由。
	LoginPath = "/portal#/login?source=electron"
	// ExchangePath 用授权码换取 token 对。
	ExchangePath = "/api/auth/exchange"
	// RefreshPath 刷新访问令牌。
	RefreshPath = "/api/auth/refresh"
	// ChatPath 是 OpenAI 兼容对话端点。
	ChatPath = "/api/proxy/v1/chat/completions"
	// ModelsPath 列出可对话模型。
	ModelsPath = "/api/models/available"

	// ClientCapabilities 对齐桌面客户端的能力声明，模型目录按它过滤。
	ClientCapabilities = "kimi-k3-agentic-v1"
	// ClientFeatures 额外启用思考档位控制。
	ClientFeatures = "thinking-level-control-v1"

	// ClientVersion 是对话请求的版本头；对话端接受陈旧值。
	ClientVersion = "0.1.0"

	// UserAgent 标识桌面客户端家族。
	UserAgent = "LobsterAI/" + ClientVersion

	// LoginLifetime 限定一次浏览器授权的等待窗口。
	LoginLifetime = 10 * time.Minute

	// CallbackPath 是本地重定向端点。
	CallbackPath = "/auth/callback"
)
