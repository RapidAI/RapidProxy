package upstream

// ModelSpec 是上游模型白名单中的一条记录。
//
// 白名单是"可用模型"的权威来源：上游 /v3/config 会漏掉部分实际可调用的模型
// （例如 deepseek-v4.1-flash、hy4-preview），所以本地白名单始终保留，
// /v3/config 只用于补充名称与上下文长度。
type ModelSpec struct {
	ID            string
	Name          string
	ContextLength int64
	MaxOutput     int64
}

// WorkBuddySpecs 是 WorkBuddy 国际版（www.workbuddy.ai）的内置白名单。
var WorkBuddySpecs = []ModelSpec{
	{"default-model", "Auto", 176000, 24000},
	{"fast-model", "Fast", 200000, 32000},
	{"balanced-model", "Balanced", 256000, 32000},
	{"primary-model", "Primary", 272000, 72000},
	{"deep-model", "Deep", 176000, 24000},
	{"gpt-5.6-terra", "GPT-5.6-Terra", 1000000, 128000},
	{"gpt-5.6-luna", "GPT-5.6-Luna", 1000000, 128000},
	{"gpt-5.5", "GPT-5.5", 1000000, 72000},
	{"gpt-5.4", "GPT-5.4", 272000, 128000},
	{"gpt-5.3-codex", "GPT-5.3-Codex", 272000, 128000},
	{"gemini-3.1-pro", "Gemini-3.1-Pro", 400000, 64000},
	{"gemini-3.5-flash", "Gemini-3.5-Flash", 1000000, 65536},
	{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", 1000000, 128000},
	{"glm-5.3", "GLM-5.3", 1000000, 48000},
	{"glm-5.2", "GLM-5.2", 1000000, 48000},
	{"hy3", "Hy3", 192000, 64000},
	{"hy4-preview", "Hy4-Preview", 192000, 64000},
	{"hy4-preview-x", "Hy4-Preview-X", 192000, 64000},
	{"kimi-k3", "Kimi-K3", 1000000, 32000},
	{"kimi-k2.7", "Kimi-K2.7", 256000, 32000},
	{"kimi-k2.6", "Kimi-K2.6", 256000, 32000},
	{"kimi-k2.5", "Kimi-K2.5", 164000, 32000},
	{"minimax-m3", "MiniMax-M3", 512000, 128000},
}

// CodeBuddySpecs 是腾讯 CodeBuddy 国内版（copilot.tencent.com）的内置白名单。
var CodeBuddySpecs = []ModelSpec{
	{"default", "Default", 200000, 24000},
	{"deepseek-v4-pro", "DeepSeek-V4-Pro", 1000000, 50000},
	{"deepseek-v4-flash", "DeepSeek-V4-Flash", 1000000, 50000},
	{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", 1000000, 128000},
	{"deepseek-v3-2-volc", "DeepSeek-V3.2", 96000, 32000},
	{"minimax-m3", "MiniMax-M3", 512000, 128000},
	{"minimax-m2.7", "MiniMax-M2.7", 200000, 48000},
	{"minimax-m2.5", "MiniMax-M2.5", 200000, 48000},
	{"glm-5.3", "GLM-5.3", 1000000, 48000},
	{"glm-5.3-flash", "GLM-5.3-Flash", 1000000, 32000},
	{"glm-5.2", "GLM-5.2", 1000000, 48000},
	{"glm-5.1", "GLM-5.1", 200000, 48000},
	{"glm-5.0", "GLM-5.0", 200000, 48000},
	{"glm-5.0-turbo", "GLM-5.0-Turbo", 200000, 48000},
	{"glm-5v-turbo", "GLM-5v-Turbo", 200000, 64000},
	{"glm-4.7", "GLM-4.7", 200000, 48000},
	{"glm-4.6", "GLM-4.6", 168000, 32000},
	{"glm-4.6v", "GLM-4.6V", 128000, 32000},
	{"kimi-k3-1", "Kimi-K3", 1000000, 32000},
	{"kimi-k2.7", "Kimi-K2.7-Code", 256000, 32000},
	{"kimi-k2.6", "Kimi-K2.6", 256000, 32000},
	{"kimi-k2.5", "Kimi-K2.5", 164000, 32000},
	{"kimi-k2-thinking", "Kimi-K2-Thinking", 164000, 32000},
	{"hy3", "Hy3", 192000, 64000},
	{"hy3-x", "Hy3", 192000, 64000},
	{"hy4-preview", "Hy4 preview", 1000000, 64000},
	{"hy4-preview-x", "Hy4 preview", 1000000, 64000},
	{"hunyuan-chat", "Hunyuan-Turbos", 200000, 8192},
}

// LobsterAISpecs 是 LobsterAI（网易有道 龙虾）的内置白名单。
// 与 upstream/lobsterai 的 allowlist 一致；目录同步成功后以上游为准。
var LobsterAISpecs = []ModelSpec{
	{"glm-5.3", "GLM-5.3", 1000000, 48000},
	{"glm-5.3-flash", "GLM-5.3-Flash", 1000000, 32000},
	{"glm-5.3-flashx", "GLM-5.3-FlashX", 1000000, 32000},
	{"glm-5.2", "GLM-5.2", 1000000, 48000},
	{"deepseek-flash", "DeepSeek-V4.1-Flash", 1000000, 128000},
	{"deepseek-v4-pro", "DeepSeek-V4-Pro", 1000000, 50000},
	{"deepseek-v4-flash", "DeepSeek-V4-Flash", 1000000, 50000},
}

// TraeCNSpecs 是 Trae 国内版的内置白名单。SOLO 目录必须登录后经
// get_detail_param 同步，这里只保留默认模型做未同步时的兜底。
var TraeCNSpecs = []ModelSpec{
	{"glm-5.2", "GLM-5.2", 1000000, 48000},
}

// TraeGlobalSpecs 是 Trae 国际版的内置白名单。
var TraeGlobalSpecs = []ModelSpec{
	{"gpt-5", "GPT-5", 1000000, 32000},
}

// QoderCNSpecs / QoderGlobalSpecs 是 Qoder 的内置白名单（真实目录 key，
// 见 qoder.canonicalQoderModelKey 的映射说明）。
var QoderCNSpecs = []ModelSpec{
	{"qmodel_38max", "Qwen3.8-Max", 1000000, 64000},
	{"qfmodel", "Qwen3.8-Flash", 1000000, 32000},
}

// QoderGlobalSpecs 与国内版共用同一套模型 key。
var QoderGlobalSpecs = QoderCNSpecs

// OpenCodeSpecs 是 OpenCode Zen 的内置兜底目录，只保留官方默认模型。
// Zen 目录以远端 /models 为准，同步失败时也不至于列表为空。
var OpenCodeSpecs = []ModelSpec{
	{"big-pickle", "Big Pickle", 128000, 0},
}

// Allowlist 返回指定上游的内置白名单。
func Allowlist(profileID string) []ModelSpec {
	switch profileID {
	case "workbuddy":
		return WorkBuddySpecs
	case "codebuddy":
		return CodeBuddySpecs
	case "lobsterai":
		return LobsterAISpecs
	case "trae-cn":
		return TraeCNSpecs
	case "trae-global":
		return TraeGlobalSpecs
	case "qoder-cn":
		return QoderCNSpecs
	case "qoder-global":
		return QoderGlobalSpecs
	case "opencode":
		return OpenCodeSpecs
	default:
		return nil
	}
}
