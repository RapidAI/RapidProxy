// Package config 负责 RapidProxy 的配置定义、默认值与持久化。
//
// 配置优先级由低到高：内置默认值 < 配置文件 < RAPIDPROXY_* 环境变量。
package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/fsx"
)

const (
	// AppName 是程序名，用于目录、窗口标题等。
	AppName = "RapidProxy"

	// DefaultListen 是 OpenAI 兼容服务的默认监听地址。
	DefaultListen = "127.0.0.1:8787"

	// DefaultUserAgent 伪装成官方 CLI 客户端，上游会校验。
	DefaultUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"

	// DefaultPlatform 是登录接口 ?platform= 参数。
	DefaultPlatform = "CLI"

	// DefaultProduct 是 X-Product 请求头。
	DefaultProduct = "SaaS"

	// DefaultSyncHours 是模型列表自动同步间隔（小时）。
	DefaultSyncHours = 6

	envPrefix = "RAPIDPROXY_"
)

// 协议族常量：决定登录流程、令牌刷新与对话转发的实现方式。
const (
	// KindWorkBuddy 是 WorkBuddy 协议族（CodeBuddy 同族，共享一套端点约定）。
	KindWorkBuddy = "workbuddy"
	// KindLobsterAI 是网易有道 LobsterAI。
	KindLobsterAI = "lobsterai"
	// KindTrae 是 Trae（国内/国际双区）。
	KindTrae = "trae"
	// KindQoder 是 Qoder（国内/国际双版）。
	KindQoder = "qoder"
	// KindOpenCode 是 OpenCode Zen（opencode.ai），API Key 制。
	KindOpenCode = "opencode"
	// KindCustom 是通用「URL + API Key + 线协议(OpenAI/Anthropic)」上游：
	// xAI / Claude / ChatGPT / 智谱编程 以及用户自建的任意 OpenAI、Anthropic
	// 兼容端点都归到这一族，按 Profile.Protocol 区分线协议。登录是 API Key 制。
	KindCustom = "custom"
)

// Edition 区分同一服务商的国内/国际版。
const (
	EditionCN     = "cn"
	EditionGlobal = "global"
)

// 登录方式（Profile.Auth）。
const (
	// AuthAPIKey 是粘贴 API Key 登录（默认）。
	AuthAPIKey = "apikey"
	// AuthOAuth 是浏览器 OAuth 授权登录。
	AuthOAuth = "oauth"
)

// OAuth 实现标识（Profile.OAuthProvider）。
const (
	OAuthClaude  = "claude"
	OAuthChatGPT = "chatgpt"
	OAuthXAI     = "xai"
	OAuthZCode   = "zcode"
)

// Profile 描述一个上游服务（一个可登录的分组）。
//
// 同一服务商分国内/国际版时是两个独立的 Profile（两个分组），各自维护
// 自己的账号、模型与轮询，例如 trae-cn / trae-global。
type Profile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	BaseURL   string `json:"base_url"`
	Origin    string `json:"origin"`
	UserAgent string `json:"user_agent"`
	// Kind 是协议族，见 Kind* 常量；为空时按 ID 推断（兼容旧配置文件）。
	Kind string `json:"kind,omitempty"`
	// Protocol 仅对 KindCustom 有效，表示上游线协议："openai"（默认）或
	// "anthropic"。其它协议族忽略此字段。空值按 "openai" 处理。
	Protocol string `json:"protocol,omitempty"`
	// Auth 是登录方式："apikey"（默认，粘贴密钥）或 "oauth"（浏览器授权）。
	// 仅对 KindCustom 有效；OAuth 上游的请求线路由 OAuthProvider 决定，
	// 与线协议字段无关。
	Auth string `json:"auth,omitempty"`
	// OAuthProvider 指明走哪套 OAuth 实现：claude / chatgpt / xai / zcode。
	// 空值时按 ID 推断（claude→claude、chatgpt→chatgpt、xai→xai、
	// zhipu/zcode→zcode）。
	OAuthProvider string `json:"oauth_provider,omitempty"`
	// Edition 区分国内/国际版（trae、qoder 使用）：cn / global。
	Edition string `json:"edition,omitempty"`
	// Proxy 为空时跟随系统环境变量；显式设置则强制走该代理。
	Proxy string `json:"proxy,omitempty"`
	// ExtraModels 追加到内置白名单之后的模型名。
	ExtraModels []string `json:"extra_models,omitempty"`
	// DisabledModels 从白名单中剔除的模型名。
	DisabledModels []string `json:"disabled_models,omitempty"`
	// SyncModels 是否从上游同步模型元信息，默认 true。
	SyncModels *bool `json:"sync_models,omitempty"`
}

// ProxyRecommended 报告是否建议为该上游配置代理（界面据此展示提示）。
//
// 国际版上游从国内直连通常不可达或极慢；国内版无需代理。
func (p Profile) ProxyRecommended() bool {
	if p.Edition == EditionGlobal || p.Kind == KindOpenCode || p.Kind == KindCustom {
		return true
	}
	return strings.Contains(p.BaseURL, "workbuddy.ai")
}

// UsesOAuth 报告该上游是否走浏览器 OAuth 登录（而不是粘贴 API Key）。
func (p Profile) UsesOAuth() bool {
	return p.Kind == KindCustom && strings.EqualFold(strings.TrimSpace(p.Auth), AuthOAuth)
}

// OAuthKind 返回该上游使用的 OAuth 实现标识；不走 OAuth 时返回空串。
//
// 未显式配置 oauth_provider 时按 ID 推断，兼容旧配置文件与用户手写的
// profiles（只写 auth=oauth 也能跑）。
func (p Profile) OAuthKind() string {
	if !p.UsesOAuth() {
		return ""
	}
	if kind := strings.ToLower(strings.TrimSpace(p.OAuthProvider)); kind != "" {
		return kind
	}
	switch p.ID {
	case "claude":
		return OAuthClaude
	case "chatgpt":
		return OAuthChatGPT
	case "xai":
		return OAuthXAI
	case "zhipu", "zcode":
		return OAuthZCode
	default:
		return ""
	}
}

// isValidProtocol 报告线协议名是否被 custom 上游接受。
func isValidProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai", "anthropic", "":
		return true
	}
	return false
}

// setIfEmpty 用预设把 p 的空字段补齐；同名预设不存在时不动 p。
func setIfEmpty(p *Profile, preset Profile) {
	if p.BaseURL == "" {
		p.BaseURL = preset.BaseURL
	}
	if p.Origin == "" {
		p.Origin = preset.Origin
	}
	if p.Name == "" {
		p.Name = preset.Name
	}
	if p.Protocol == "" {
		p.Protocol = preset.Protocol
	}
	if p.Auth == "" {
		p.Auth = preset.Auth
	}
	if p.OAuthProvider == "" {
		p.OAuthProvider = preset.OAuthProvider
	}
	// 智谱编程（zhipu / zcode）预设带「编程客户端」身份 OpenCode：智谱据此
	// 把请求计入 ZCode / 编程套餐赠送额度。其它预设该字段为空，不覆盖。
	if p.UserAgent == "" {
		p.UserAgent = preset.UserAgent
	}
}

// migrateZhipuCoding 让智谱编程预设从旧配置升级到正确的编程套餐组合。
//
// 早期版本把智谱编程错配成「普通 Anthropic 网关 + 小写 opencode 身份」，请求
// 会命中按量付费通道，在无余额账号上返回 [1113] 余额不足。编程套餐的正确
// 组合是「/api/coding/paas/v4 网关 + OpenCode 客户端身份」。这里仅在值仍为
// 旧默认（Anthropic 网关或普通开放平台网关）时迁移，用户自定义过的端点 /
// 身份保持不动。
func migrateZhipuCoding(p *Profile, preset Profile) {
	base := strings.TrimSpace(p.BaseURL)
	// 旧默认：普通 Anthropic 网关，或普通开放平台网关（缺少 "coding/" 段）。
	legacyAnthropic := strings.Contains(base, "open.bigmodel.cn/api/anthropic")
	legacyOpenPlatform := strings.Contains(base, "open.bigmodel.cn/api/paas/v4")
	if base == "" || legacyAnthropic || legacyOpenPlatform {
		p.BaseURL = preset.BaseURL
		p.Protocol = preset.Protocol
		p.UserAgent = preset.UserAgent
		// 迁移分支同样要补齐 auth：旧配置里没有这个字段，直接 return 会让
		// 老用户永远停在 apikey，界面上看不到 ZCode 浏览器登录入口。
		if p.Auth == "" {
			p.Auth = preset.Auth
		}
		if p.OAuthProvider == "" {
			p.OAuthProvider = preset.OAuthProvider
		}
		return
	}
	setIfEmpty(p, preset)
	// 身份大小写归一到 "OpenCode"：与 aicoder 中已验证的编程套餐客户端一致。
	if strings.EqualFold(strings.TrimSpace(p.UserAgent), "opencode") {
		p.UserAgent = preset.UserAgent
	}
}

// migrateChatGPTCodex 让 ChatGPT 预设从旧的「平台 API 端点」升级到 Codex
// 订阅线路。
//
// 早期版本把 chatgpt 当普通 API Key 上游（`api.openai.com/v1`）。改成 OAuth
// 登录后，OpenAI 的 OAuth 令牌只能在 Codex 订阅线路
// （`chatgpt.com/backend-api/codex`）使用，指向平台端点会一律 401。这里只在
// 值仍是旧默认（api.openai.com）时迁移，用户自定义过的反代端点保持不动。
func migrateChatGPTCodex(p *Profile, preset Profile) {
	base := strings.ToLower(strings.TrimSpace(p.BaseURL))
	legacyPlatform := strings.Contains(base, "api.openai.com") || base == ""
	if legacyPlatform {
		p.BaseURL = preset.BaseURL
		p.Origin = preset.Origin
	}
	setIfEmpty(p, preset)
}

// VirtualMember 是虚拟模型组的一个成员：指向某个上游的具体模型。
//
// Provider 是上游 ID（如 qoder-cn），Model 是该上游的原始模型 ID
// （如 glm-5.3，不带 "上游ID:" 这类对外前缀）——对外暴露的名字会随
// 登录了哪些上游而变化，存原始 ID 才能在任意登录组合下都解析得回来。
type VirtualMember struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Weight 是组内负载均衡权重；未设置或 <=0 时按 1 处理。
	Weight int `json:"weight,omitempty"`
}

// EffectiveWeight 返回参与负载均衡的权重，未设置（<=0）时为 1。
func (m VirtualMember) EffectiveWeight() int {
	if m.Weight > 0 {
		return m.Weight
	}
	return 1
}

// Key 返回成员在负载均衡状态表里的键。
func (m VirtualMember) Key() string { return m.Provider + "/" + m.Model }

// VirtualModel 是用户自定义的虚拟模型名。
//
// 客户端只请求 Name，代理在 Members 之间按权重分摊；某个成员不可用
// （上游未登录、账号全暂停、限流冷却、上游报错）就自动换下一个。
// 成员引用的是「上游 + 原始模型 ID」，因此一个虚拟组可以横跨不同服务商。
type VirtualModel struct {
	Name    string          `json:"name"`
	Enabled bool            `json:"enabled"`
	Members []VirtualMember `json:"members"`
}

// ValidVirtualName 报告虚拟模型名是否可用作请求里的 model 字段。
//
// 禁止 ":" 与 "/"：这两个是 resolveModel 里「上游ID:模型名」强制路由的
// 分隔符，虚拟名里带上会被拆成上游前缀，永远匹配不到组。
// 禁止空白：客户端几乎不会传带空格的模型名，且空白会让日志与统计难以辨认。
func ValidVirtualName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.ContainsAny(name, ":/ \t\r\n") {
		return false
	}
	// "intl" 是国际版上游的别名，占用它会让 intl:xxx 的强制路由失效。
	if strings.EqualFold(name, "intl") {
		return false
	}
	return true
}

// Config 是持久化到 <dataDir>/config.json 的全部设置。
type Config struct {
	Listen string `json:"listen"`
	// APIKeys 是下游客户端访问本代理所需的密钥；为空表示不校验。
	APIKeys []string `json:"api_keys"`
	// Platform / Product 一般无需修改。
	Platform string `json:"platform"`
	Product  string `json:"product"`
	// CORS 允许浏览器页面直接调用本代理。
	CORS *bool `json:"cors,omitempty"`
	// SanitizeTemplates 对上游内容审核的固定模板句做最小改写（默认开启）。
	SanitizeTemplates *bool `json:"sanitize_templates,omitempty"`
	// ForceMaxThinking 对 hy3 系列强制 reasoning_effort=high（默认开启）。
	ForceMaxThinking *bool `json:"force_max_thinking,omitempty"`
	// ModelSyncHours 模型列表同步间隔，<=0 表示不同步。
	ModelSyncHours int `json:"model_sync_hours"`
	// AutoStart 打开程序时自动启动代理服务。
	AutoStart *bool `json:"auto_start,omitempty"`
	// LaunchAtLogin 开机自动启动程序（登录系统后静默运行在托盘），默认开启。
	LaunchAtLogin *bool `json:"launch_at_login,omitempty"`
	// Language 界面语言：system 跟随系统 / zh 简体中文 / en 英文。
	// 保存的是「偏好」，实际语言由各侧自行解析（Go 侧见 internal/locale）。
	Language string `json:"language,omitempty"`
	// Profiles 是上游服务列表，顺序决定同名模型的优先级。
	Profiles []Profile `json:"profiles"`
	// VirtualModels 是用户自定义的虚拟模型组（一个名字聚合多个上游的模型）。
	VirtualModels []VirtualModel `json:"virtual_models,omitempty"`

	path string
}

// WorkBuddyPreset 返回 workbuddy（www.workbuddy.ai）预设。
func WorkBuddyPreset() Profile {
	return Profile{
		ID:        "workbuddy",
		Name:      "WorkBuddy（国际版）",
		Enabled:   true,
		Kind:      KindWorkBuddy,
		BaseURL:   "https://www.workbuddy.ai",
		Origin:    "https://www.workbuddy.ai",
		UserAgent: DefaultUserAgent,
	}
}

// CodeBuddyPreset 返回 codebuddy（copilot.tencent.com，腾讯 CodeBuddy）预设。
// 与 WorkBuddy 同一协议族的国内部署。
func CodeBuddyPreset() Profile {
	return Profile{
		ID:        "codebuddy",
		Name:      "CodeBuddy（国内版）",
		Enabled:   false,
		Kind:      KindWorkBuddy,
		Edition:   EditionCN,
		BaseURL:   "https://copilot.tencent.com",
		Origin:    "https://www.codebuddy.cn",
		UserAgent: DefaultUserAgent,
	}
}

// LobsterAIPreset 返回 lobsterai（网易有道 龙虾）预设。
// 无国内/国际之分，单一分组。
func LobsterAIPreset() Profile {
	return Profile{
		ID:      "lobsterai",
		Name:    "LobsterAI",
		Enabled: false,
		Kind:    KindLobsterAI,
		// 与 upstream/lobsterai.APIBase 一致；对话与账号后端主机。
		BaseURL: "https://lobsterai-server.youdao.com",
		Origin:  "https://lobsterai.youdao.com",
	}
}

// TraeCNPreset 返回 trae-cn（Trae 国内版）预设。
func TraeCNPreset() Profile {
	return Profile{
		ID:      "trae-cn",
		Name:    "Trae（国内版）",
		Enabled: false,
		Kind:    KindTrae,
		Edition: EditionCN,
		// 与 upstream/trae.CNProfile().ChatHost 一致。
		BaseURL: "https://trae-api-cn.mchost.guru",
		Origin:  "https://www.trae.cn",
	}
}

// TraeGlobalPreset 返回 trae-global（Trae 国际版）预设。
func TraeGlobalPreset() Profile {
	return Profile{
		ID:      "trae-global",
		Name:    "Trae（国际版）",
		Enabled: false,
		Kind:    KindTrae,
		Edition: EditionGlobal,
		BaseURL: "https://coresg-normal.trae.ai",
		Origin:  "https://www.trae.ai",
	}
}

// QoderCNPreset 返回 qoder-cn（Qoder 国内版）预设。
func QoderCNPreset() Profile {
	return Profile{
		ID:      "qoder-cn",
		Name:    "Qoder（国内版）",
		Enabled: false,
		Kind:    KindQoder,
		Edition: EditionCN,
		// 与 upstream/qoder.CNProfile().ChatOrigin 一致（签名对话网关）。
		BaseURL: "https://gateway.qoder.com.cn",
		Origin:  "https://qoder.cn",
	}
}

// QoderGlobalPreset 返回 qoder-global（Qoder 国际版）预设。
func QoderGlobalPreset() Profile {
	return Profile{
		ID:      "qoder-global",
		Name:    "Qoder（国际版）",
		Enabled: false,
		Kind:    KindQoder,
		Edition: EditionGlobal,
		BaseURL: "https://api3.qoder.sh",
		Origin:  "https://qoder.com",
	}
}

// OpenCodePreset 返回 opencode（OpenCode Zen，opencode.ai）预设。
//
// UserAgent 保存的是裸 "opencode"：这是设置里的身份，原样落到其它上游
// （GLM 等按精确匹配识别 opencode 客户端的场景不受影响）；连线 OpenCode
// 时由 upstream/opencode 改写成官方客户端的 opencode/1.18.31。
func OpenCodePreset() Profile {
	return Profile{
		ID:        "opencode",
		Name:      "OpenCode",
		Enabled:   false,
		Kind:      KindOpenCode,
		BaseURL:   "https://opencode.ai/zen/v1",
		Origin:    "https://opencode.ai",
		UserAgent: "opencode",
	}
}

// XAIPreset 返回 xAI（api.x.ai，Grok 系列）预设，OAuth 登录（Grok Build
// 客户端身份：OAuth 令牌必须带 X-XAI-Token-Auth 头）。
//
// 用户仍可在 config.json 里把 auth 改成 apikey 并填 xAI 的 API Key——
// 两条路都支持，只是默认走浏览器授权（订阅额度无需手建 Key）。
func XAIPreset() Profile {
	return Profile{
		ID:       "xai",
		Name:     "xAI（Grok）",
		Enabled:  false,
		Kind:     KindCustom,
		Protocol: "openai",
		Auth:     AuthOAuth,
		// 与 upstream/custom 的默认 OpenAI 端点构造一致：base 已是 /v1 形态。
		BaseURL: "https://api.x.ai/v1",
		Origin:  "https://x.ai",
	}
}

// ClaudePreset 返回 Anthropic Claude 预设，OAuth 登录（Claude Pro/Max 订阅）。
// 授权页由 Anthropic 托管、无本地回调，用户在页面上复制授权码粘贴回来。
func ClaudePreset() Profile {
	return Profile{
		ID:       "claude",
		Name:     "Claude（Anthropic）",
		Enabled:  false,
		Kind:     KindCustom,
		Protocol: "anthropic",
		Auth:     AuthOAuth,
		BaseURL:  "https://api.anthropic.com/v1",
		Origin:   "https://www.anthropic.com",
	}
}

// ChatGPTPreset 返回 OpenAI ChatGPT 预设，OAuth 登录（Codex 订阅线路）。
//
// OAuth 令牌只能走 Codex 的 Responses 接口（chatgpt.com/backend-api/codex），
// 不是 api.openai.com 的 Chat Completions；协议翻译在 upstream/oauthprov。
func ChatGPTPreset() Profile {
	return Profile{
		ID:       "chatgpt",
		Name:     "ChatGPT（OpenAI）",
		Enabled:  false,
		Kind:     KindCustom,
		Protocol: "openai",
		Auth:     AuthOAuth,
		BaseURL:  "https://chatgpt.com/backend-api/codex",
		Origin:   "https://chat.openai.com",
	}
}

// ZhipuPreset 返回智谱编程（bigmodel，GLM 系列）预设，OpenAI 线协议、
// ZCode 在线 OAuth 登录（浏览器授权 → 换出 coding-plan API Key）。
//
// BaseURL 必须指向智谱的 **编程套餐专用** OpenAI 网关 `/api/coding/paas/v4`
// （不是普通开放平台的 `/api/paas/v4`），并以编程客户端身份（User-Agent
// "OpenCode"）调用——两者缺一都会命中「按量付费」通道，在无余额的账号上
// 返回 [1113] 余额不足。这是 aicoder 中 OpenCode+GLM 的已验证组合。
//
// 登录方式：浏览器授权后换出一把普通 coding-plan API Key 落库，因此对话
// 线路与「粘贴 API Key」完全一致；把 auth 改回 apikey 仍可手动粘 Key。
func ZhipuPreset() Profile {
	return Profile{
		ID:            "zhipu",
		Name:          "智谱编程（GLM）",
		Enabled:       false,
		Kind:          KindCustom,
		Protocol:      "openai",
		Auth:          AuthOAuth,
		OAuthProvider: OAuthZCode,
		BaseURL:       "https://open.bigmodel.cn/api/coding/paas/v4",
		Origin:        "https://open.bigmodel.cn",
		UserAgent:     "OpenCode",
	}
}

// ZCodePreset 返回智谱 ZCode 登录的智谱编程预设，OpenAI 线协议、ZCode 在线
// OAuth 登录（浏览器授权 → 换 coding-plan API Key）。与 ZhipuPreset 共用同一
// 套编程套餐网关与客户端身份，区别仅在于显示名（两者都是 OAuth 登录）。
func ZCodePreset() Profile {
	return Profile{
		ID:            "zcode",
		Name:          "智谱编程（ZCode 登录）",
		Enabled:       false,
		Kind:          KindCustom,
		Protocol:      "openai",
		Auth:          AuthOAuth,
		OAuthProvider: OAuthZCode,
		BaseURL:       "https://open.bigmodel.cn/api/coding/paas/v4",
		Origin:        "https://open.bigmodel.cn",
		UserAgent:     "OpenCode",
	}
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Listen:         DefaultListen,
		Platform:       DefaultPlatform,
		Product:        DefaultProduct,
		ModelSyncHours: DefaultSyncHours,
		Profiles: []Profile{
			WorkBuddyPreset(),
			CodeBuddyPreset(),
			LobsterAIPreset(),
			TraeCNPreset(),
			TraeGlobalPreset(),
			QoderCNPreset(),
			QoderGlobalPreset(),
			OpenCodePreset(),
			XAIPreset(),
			ClaudePreset(),
			ChatGPTPreset(),
			ZhipuPreset(),
			ZCodePreset(),
		},
	}
}

// DefaultAt 返回内置默认配置，并指定配置文件路径。
//
// 配置文件损坏、无法解析时用它兜底，这样界面里显示的「配置文件路径」不会是空的，
// 后续保存也会写回用户真正的位置。
func DefaultAt(path string) *Config {
	cfg := Default()
	cfg.path = path
	return cfg
}

// DefaultDataDir 返回默认数据目录（Windows: %USERPROFILE%\.rapidproxy）。
func DefaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "."+AppName)
	}
	return "." + AppName
}

// DefaultPath 返回默认配置文件路径。
func DefaultPath(dataDir string) string {
	return filepath.Join(dataDir, "config.json")
}

// Load 读取配置文件；文件不存在时返回默认配置而不是报错。
func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.path = path
	if strings.TrimSpace(path) == "" {
		cfg.path = DefaultPath(DefaultDataDir())
	}

	raw, err := os.ReadFile(cfg.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("配置文件 %s 解析失败: %w", cfg.path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次运行，使用默认值。
	default:
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", cfg.path, err)
	}

	applyEnv(cfg)
	cfg.normalize()
	return cfg, nil
}

// Path 返回配置文件路径。
func (c *Config) Path() string { return c.path }

// Save 把配置写回磁盘（原子写入）。
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultPath(DefaultDataDir())
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWrite(c.path, append(raw, '\n'), 0o600)
}

// normalize 补齐默认值并修正明显不合法的字段。
func (c *Config) normalize() {
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = DefaultListen
	} else if !strings.Contains(c.Listen, ":") {
		c.Listen = "127.0.0.1:" + c.Listen
	}
	if strings.TrimSpace(c.Platform) == "" {
		c.Platform = DefaultPlatform
	}
	if strings.TrimSpace(c.Product) == "" {
		c.Product = DefaultProduct
	}
	// ModelSyncHours 不在这里补默认值：默认配置 Default() 已经是 DefaultSyncHours，
	// 而配置文件里显式写 0 表示「关闭自动同步」（界面上就是这么写的）。
	// 一旦在这里把 0 重置成默认值，用户就永远关不掉同步。
	if c.ModelSyncHours < 0 {
		c.ModelSyncHours = 0
	}
	// Language 只认 system / zh / en；空值与未知值一律归一成 system（跟随系统）。
	c.Language = strings.ToLower(strings.TrimSpace(c.Language))
	if c.Language != "zh" && c.Language != "en" {
		c.Language = "system"
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []Profile{
			WorkBuddyPreset(), CodeBuddyPreset(), LobsterAIPreset(),
			TraeCNPreset(), TraeGlobalPreset(), QoderCNPreset(), QoderGlobalPreset(),
			OpenCodePreset(), XAIPreset(), ClaudePreset(), ChatGPTPreset(), ZhipuPreset(), ZCodePreset(),
		}
	} else {
		// 版本升级带来的新预设要补齐：旧配置文件里只有当时的上游清单，
		// Unmarshal 会整体替换默认列表，不补的话升级用户看不到新增服务商。
		// （界面上的「隐藏」语义是停用开关，不是删除预设条目。）
		missing := []Profile{
			WorkBuddyPreset(), CodeBuddyPreset(), LobsterAIPreset(),
			TraeCNPreset(), TraeGlobalPreset(), QoderCNPreset(), QoderGlobalPreset(),
			OpenCodePreset(), XAIPreset(), ClaudePreset(), ChatGPTPreset(), ZhipuPreset(), ZCodePreset(),
		}
		for _, preset := range missing {
			found := false
			for _, p := range c.Profiles {
				if p.ID == preset.ID {
					found = true
					break
				}
			}
			if !found {
				c.Profiles = append(c.Profiles, preset)
			}
		}
	}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.ID = strings.TrimSpace(p.ID)
		// 用户自建的 custom-<hash> 组：协议族统一归到 custom（即便旧配置里
		// Kind 为空），具体线协议在下面按 KindCustom 统一校正。
		if strings.HasPrefix(p.ID, "custom-") || p.ID == "custom" {
			p.Kind = KindCustom
		}
		switch p.ID {
		case "workbuddy":
			preset := WorkBuddyPreset()
			p.Kind = KindWorkBuddy
			p.Edition = ""
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "codebuddy":
			preset := CodeBuddyPreset()
			p.Kind = KindWorkBuddy
			p.Edition = EditionCN
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "lobsterai":
			preset := LobsterAIPreset()
			p.Kind = KindLobsterAI
			p.Edition = ""
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "trae-cn":
			preset := TraeCNPreset()
			p.Kind = KindTrae
			p.Edition = EditionCN
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "trae-global":
			preset := TraeGlobalPreset()
			p.Kind = KindTrae
			p.Edition = EditionGlobal
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "qoder-cn":
			preset := QoderCNPreset()
			p.Kind = KindQoder
			p.Edition = EditionCN
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "qoder-global":
			preset := QoderGlobalPreset()
			p.Kind = KindQoder
			p.Edition = EditionGlobal
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
		case "opencode":
			preset := OpenCodePreset()
			p.Kind = KindOpenCode
			p.Edition = ""
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Origin == "" {
				p.Origin = preset.Origin
			}
			if p.Name == "" {
				p.Name = preset.Name
			}
			// 设置里保存的身份恒为裸 "opencode"：连线时的改写只发生在
			// 上游侧（见 upstream/opencode），不落进配置文件。
			if p.UserAgent == "" {
				p.UserAgent = preset.UserAgent
			}
		case "xai", "claude", "chatgpt", "zhipu", "zcode":
			// 通用「URL + API Key + 线协议」上游的五个命名预设。Protocol、
			// BaseURL/Origin/Name 缺失时回退预设。
			p.Kind = KindCustom
			p.Edition = ""
			switch p.ID {
			case "xai":
				setIfEmpty(p, XAIPreset())
			case "claude":
				setIfEmpty(p, ClaudePreset())
			case "chatgpt":
				migrateChatGPTCodex(p, ChatGPTPreset())
			case "zhipu":
				migrateZhipuCoding(p, ZhipuPreset())
			case "zcode":
				// zcode 与 zhipu 共用同一套编程套餐网关与客户端身份，
				// 区别仅在于登录方式（ZCode 浏览器 OAuth）与显示名。
				migrateZhipuCoding(p, ZCodePreset())
			}
		}
		// 所有 Kind=custom 的上游（命名预设与用户自建组）统一校正线协议：
		// 空值与未知值一律按 openai 处理，Editon 不参与。
		if p.Kind == KindCustom {
			p.Edition = ""
			if !isValidProtocol(p.Protocol) {
				p.Protocol = "openai"
			}
			// 登录方式只认 apikey / oauth；空值与未知值按 apikey 处理
			// （用户自建组没有 OAuth 实现，粘贴密钥是唯一可行的路）。
			auth := strings.ToLower(strings.TrimSpace(p.Auth))
			if auth != AuthOAuth {
				p.Auth = AuthAPIKey
			} else {
				p.Auth = AuthOAuth
			}
			// OAuth 实现标识：空值按 ID 推断，推断不出来就退回粘贴密钥——
			// 否则会出现「要求 OAuth 但没有任何实现」的死角。
			p.OAuthProvider = p.OAuthKind()
			if p.Auth == AuthOAuth && p.OAuthProvider == "" {
				p.Auth = AuthAPIKey
			}
			if p.Auth != AuthOAuth {
				p.OAuthProvider = ""
			}
		} else {
			p.Auth = ""
			p.OAuthProvider = ""
		}
		if p.UserAgent == "" && (p.Kind == "" || p.Kind == KindWorkBuddy) {
			// WorkBuddy 协议族要求伪装官方 CLI；其它协议族的请求头由
			// 各自的适配包决定，UserAgent 字段不参与。
			p.UserAgent = DefaultUserAgent
		}
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		if p.Origin == "" {
			p.Origin = p.BaseURL
		}
		if p.Name == "" {
			p.Name = p.ID
		}
	}
	c.APIKeys = cleanKeys(c.APIKeys)
	c.normalizeVirtualModels()
}

// normalizeVirtualModels 清理虚拟组：丢弃非法名字、重复名字、空成员与
// 指向未知上游的成员。
//
// 这里直接丢而不是报错：配置是手写 JSON 也可能被旧版本写坏，一个坏条目
// 不该让整个设置页打不开。真正需要告知用户的地方（界面保存）会在
// ValidateVirtualModel 里先挡住。
func (c *Config) normalizeVirtualModels() {
	if len(c.VirtualModels) == 0 {
		c.VirtualModels = nil
		return
	}
	known := make(map[string]bool, len(c.Profiles))
	for _, p := range c.Profiles {
		known[p.ID] = true
	}
	seen := make(map[string]bool, len(c.VirtualModels))
	out := make([]VirtualModel, 0, len(c.VirtualModels))
	for _, vm := range c.VirtualModels {
		vm.Name = strings.TrimSpace(vm.Name)
		if !ValidVirtualName(vm.Name) || seen[vm.Name] {
			continue
		}
		// 与上游 ID 同名必须丢弃：resolveRoute 里虚拟组优先于真实模型匹配，
		// 留着它会让该上游的所有裸模型名都被这个组截走。
		// 保存路径已由 ValidateVirtualModel 拦住，这里兜住手写配置的情况。
		if known[vm.Name] {
			continue
		}
		members := make([]VirtualMember, 0, len(vm.Members))
		seenMember := make(map[string]bool, len(vm.Members))
		for _, m := range vm.Members {
			m.Provider = strings.TrimSpace(m.Provider)
			m.Model = strings.TrimSpace(m.Model)
			if m.Provider == "" || m.Model == "" || !known[m.Provider] {
				continue
			}
			if seenMember[m.Key()] {
				continue
			}
			seenMember[m.Key()] = true
			if m.Weight < 0 {
				m.Weight = 0
			}
			members = append(members, m)
		}
		// 没有成员的组保留（用户可能刚建好还没加成员），但 Enabled 无意义；
		// 保留 enabled 原值，让用户加完成员即可直接生效。
		vm.Members = members
		seen[vm.Name] = true
		out = append(out, vm)
	}
	c.VirtualModels = out
}

func cleanKeys(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func applyEnv(c *Config) {
	if v := strings.TrimSpace(os.Getenv(envPrefix + "LISTEN")); v != "" {
		c.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "API_KEYS")); v != "" {
		c.APIKeys = strings.Split(v, ",")
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "PROXY")); v != "" {
		for i := range c.Profiles {
			c.Profiles[i].Proxy = v
		}
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "PLATFORM")); v != "" {
		c.Platform = v
	}
}

// Normalize 补齐默认值；配置被外部修改后调用。
func (c *Config) Normalize() { c.normalize() }

// Clone 返回配置的深拷贝，便于在不影响运行中服务的前提下修改设置。
func (c *Config) Clone() *Config {
	cp := *c
	cp.APIKeys = append([]string(nil), c.APIKeys...)
	// 指针字段一并深拷贝，避免改副本时连带改到运行中的配置。
	if c.CORS != nil {
		v := *c.CORS
		cp.CORS = &v
	}
	if c.SanitizeTemplates != nil {
		v := *c.SanitizeTemplates
		cp.SanitizeTemplates = &v
	}
	if c.ForceMaxThinking != nil {
		v := *c.ForceMaxThinking
		cp.ForceMaxThinking = &v
	}
	if c.AutoStart != nil {
		v := *c.AutoStart
		cp.AutoStart = &v
	}
	if c.LaunchAtLogin != nil {
		v := *c.LaunchAtLogin
		cp.LaunchAtLogin = &v
	}
	cp.Profiles = make([]Profile, len(c.Profiles))
	copy(cp.Profiles, c.Profiles)
	for i := range cp.Profiles {
		cp.Profiles[i].ExtraModels = append([]string(nil), c.Profiles[i].ExtraModels...)
		cp.Profiles[i].DisabledModels = append([]string(nil), c.Profiles[i].DisabledModels...)
		// SyncModels 是指针，必须一并深拷贝，否则改副本会连带改到运行中的配置。
		if c.Profiles[i].SyncModels != nil {
			v := *c.Profiles[i].SyncModels
			cp.Profiles[i].SyncModels = &v
		}
	}
	cp.VirtualModels = make([]VirtualModel, len(c.VirtualModels))
	for i, vm := range c.VirtualModels {
		cp.VirtualModels[i] = vm
		cp.VirtualModels[i].Members = append([]VirtualMember(nil), vm.Members...)
	}
	return &cp
}

// ProfileByID 按 ID 查找上游配置。
func (c *Config) ProfileByID(id string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// EnabledProfiles 返回已启用的上游配置（保持配置顺序）。
func (c *Config) EnabledProfiles() []Profile {
	out := make([]Profile, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// SetProfile 覆盖指定 ID 的上游配置。
func (c *Config) SetProfile(p Profile) {
	for i := range c.Profiles {
		if c.Profiles[i].ID == p.ID {
			c.Profiles[i] = p
			c.normalize()
			return
		}
	}
	c.Profiles = append(c.Profiles, p)
	c.normalize()
}

// IsUserCustomProfile 报告该上游是否是用户自建的自定义分组。
//
// 判定只认 ID 前缀 custom-：命名预设（xai / claude / chatgpt / zhipu / zcode）
// 同属 KindCustom，但它们由程序内置、不该被用户删除，也不会在界面上出现
// 「删除分组」入口。旧配置里可能残留 ID 恰为 "custom" 的条目，同样视作用户组。
func (p Profile) IsUserCustomProfile() bool {
	id := strings.TrimSpace(p.ID)
	return strings.HasPrefix(id, "custom-") || id == "custom"
}

// RemoveProfile 删除指定 ID 的上游分组。返回是否真的删掉了。
//
// 只允许删用户自建分组（见 IsUserCustomProfile）：内置预设被删掉后，
// 下一次 normalize 又会按「缺失补齐」把它加回来，删了等于没删，反而让
// 用户以为操作失败。虚拟组里引用了该上游的成员由 normalizeVirtualModels 清理。
func (c *Config) RemoveProfile(id string) bool {
	id = strings.TrimSpace(id)
	kept := make([]Profile, 0, len(c.Profiles))
	removed := false
	for _, p := range c.Profiles {
		if p.ID == id && p.IsUserCustomProfile() {
			removed = true
			continue
		}
		kept = append(kept, p)
	}
	if !removed {
		return false
	}
	c.Profiles = kept
	c.normalize()
	return true
}

// -----------------------------------------------------------------------------
// 虚拟模型组
// -----------------------------------------------------------------------------

// VirtualModelByName 按名字查找虚拟组。
func (c *Config) VirtualModelByName(name string) (VirtualModel, bool) {
	name = strings.TrimSpace(name)
	for _, vm := range c.VirtualModels {
		if vm.Name == name {
			return vm, true
		}
	}
	return VirtualModel{}, false
}

// EnabledVirtualModels 返回已启用且至少有一个成员的虚拟组。
//
// 没有成员的组不参与路由：暴露一个永远返回「无可用上游」的模型名
// 只会让客户端困惑。
func (c *Config) EnabledVirtualModels() []VirtualModel {
	out := make([]VirtualModel, 0, len(c.VirtualModels))
	for _, vm := range c.VirtualModels {
		if vm.Enabled && len(vm.Members) > 0 {
			out = append(out, vm)
		}
	}
	return out
}

// ValidateVirtualModel 校验一个待保存的虚拟组，返回可读的错误。
//
// 只做「一定不行」的检查：名字合法、不与真实模型/上游 ID 冲突、成员
// 指向已知上游。成员模型是否真的存在不在此时校验——上游模型清单随登录
// 状态变化，存的时候存在、用的时候可能已经下线，那种情况交给路由层跳过。
func (c *Config) ValidateVirtualModel(vm VirtualModel, existingName string) error {
	name := strings.TrimSpace(vm.Name)
	if strings.EqualFold(name, "intl") {
		// 单独给出原因：ValidVirtualName 也会拒绝它，但那条错误信息讲的是
		// 「不能为空 / 不能含分隔符」，用户看到会一头雾水。
		return errors.New("intl 是国际版上游的保留名，请换一个名字")
	}
	if !ValidVirtualName(name) {
		return errors.New("虚拟模型名不能为空，且不能包含 : / 或空格（这两个是强制路由分隔符）")
	}
	for _, other := range c.VirtualModels {
		// 改名时允许保留自己的名字。
		if other.Name == name && other.Name != existingName {
			return fmt.Errorf("虚拟模型名 %s 已存在", name)
		}
	}
	for _, p := range c.Profiles {
		if p.ID == name {
			return fmt.Errorf("虚拟模型名 %s 与上游 ID 冲突，请换一个名字", name)
		}
	}
	for _, m := range vm.Members {
		if _, ok := c.ProfileByID(m.Provider); !ok {
			return fmt.Errorf("成员指向未知上游: %s", m.Provider)
		}
		if strings.TrimSpace(m.Model) == "" {
			return fmt.Errorf("上游 %s 的成员缺少模型名", m.Provider)
		}
	}
	return nil
}

// UpsertVirtualModel 新增或按 existingName 改名覆盖一个虚拟组。
//
// existingName 为空表示新增；非空表示把原名 existingName 的组替换成 vm
// （允许改名）。名字冲突由调用方先用 ValidateVirtualModel 挡住。
func (c *Config) UpsertVirtualModel(vm VirtualModel, existingName string) error {
	vm.Name = strings.TrimSpace(vm.Name)
	if err := c.ValidateVirtualModel(vm, strings.TrimSpace(existingName)); err != nil {
		return err
	}
	existingName = strings.TrimSpace(existingName)
	if existingName == "" {
		c.VirtualModels = append(c.VirtualModels, vm)
		c.normalize()
		return nil
	}
	for i := range c.VirtualModels {
		if c.VirtualModels[i].Name == existingName {
			c.VirtualModels[i] = vm
			c.normalize()
			return nil
		}
	}
	return fmt.Errorf("虚拟模型 %s 不存在", existingName)
}

// RemoveVirtualModel 删除一个虚拟组，返回是否删除成功。
func (c *Config) RemoveVirtualModel(name string) bool {
	name = strings.TrimSpace(name)
	out := c.VirtualModels[:0]
	removed := false
	for _, vm := range c.VirtualModels {
		if vm.Name == name {
			removed = true
			continue
		}
		out = append(out, vm)
	}
	c.VirtualModels = out
	return removed
}

// CORSEnabled 返回是否开启跨域。
func (c *Config) CORSEnabled() bool { return c.CORS == nil || *c.CORS }

// SanitizeEnabled 返回是否改写上游内容审核模板句。
func (c *Config) SanitizeEnabled() bool { return c.SanitizeTemplates == nil || *c.SanitizeTemplates }

// MaxThinkingEnabled 返回是否强制 hy3 深度思考。
func (c *Config) MaxThinkingEnabled() bool { return c.ForceMaxThinking == nil || *c.ForceMaxThinking }

// AutoStartEnabled 返回是否随程序启动代理服务。
func (c *Config) AutoStartEnabled() bool { return c.AutoStart == nil || *c.AutoStart }

// LaunchAtLoginEnabled 返回是否开机自动启动（未配置时默认开启）。
func (c *Config) LaunchAtLoginEnabled() bool { return c.LaunchAtLogin == nil || *c.LaunchAtLogin }

// SyncInterval 返回模型同步周期，<=0 表示关闭同步。
func (c *Config) SyncInterval() time.Duration {
	if c.ModelSyncHours <= 0 {
		return 0
	}
	return time.Duration(c.ModelSyncHours) * time.Hour
}

// RequiresAPIKey 返回是否强制校验下游密钥。
func (c *Config) RequiresAPIKey() bool { return len(c.APIKeys) > 0 }

// CheckAPIKey 校验下游传入的密钥。使用恒定时间比较，避免计时侧信道。
func (c *Config) CheckAPIKey(key string) bool {
	if !c.RequiresAPIKey() {
		return true
	}
	keyB := []byte(key)
	for _, k := range c.APIKeys {
		if subtle.ConstantTimeCompare(keyB, []byte(k)) == 1 {
			return true
		}
	}
	return false
}

// AddAPIKey 新增一个下游密钥，返回是否新增成功。
func (c *Config) AddAPIKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, k := range c.APIKeys {
		if k == key {
			return false
		}
	}
	c.APIKeys = append(c.APIKeys, key)
	return true
}

// RemoveAPIKey 删除一个下游密钥。
func (c *Config) RemoveAPIKey(key string) bool {
	out := c.APIKeys[:0]
	removed := false
	for _, k := range c.APIKeys {
		if k == key {
			removed = true
			continue
		}
		out = append(out, k)
	}
	c.APIKeys = out
	return removed
}

// GenerateAPIKey 生成形如 sk-wbp-xxxx 的随机密钥。
func GenerateAPIKey() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("sk-wbp-%d", time.Now().UnixNano())
	}
	return "sk-wbp-" + hex.EncodeToString(buf)
}

// BaseURL 返回给客户端使用的基础地址（不含 /v1）。
//
// 监听在 0.0.0.0 / :: 时对外展示 127.0.0.1，避免生成无法直接访问的地址。
func (c *Config) BaseURL() string {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "http://" + c.Listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// OpenAIBaseURL 返回 OpenAI 兼容的 base_url。
func (c *Config) OpenAIBaseURL() string { return c.BaseURL() + "/v1" }
