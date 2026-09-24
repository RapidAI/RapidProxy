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

// Allowlist 返回指定上游的内置白名单。
func Allowlist(profileID string) []ModelSpec {
	switch profileID {
	case "workbuddy":
		return WorkBuddySpecs
	case "codebuddy":
		return CodeBuddySpecs
	default:
		return nil
	}
}
