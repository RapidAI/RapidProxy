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
)

// Edition 区分同一服务商的国内/国际版。
const (
	EditionCN     = "cn"
	EditionGlobal = "global"
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
	if p.Edition == EditionGlobal {
		return true
	}
	return strings.Contains(p.BaseURL, "workbuddy.ai")
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
		}
	} else {
		// 版本升级带来的新预设要补齐：旧配置文件里只有当时的上游清单，
		// Unmarshal 会整体替换默认列表，不补的话升级用户看不到新增服务商。
		// （界面上的「隐藏」语义是停用开关，不是删除预设条目。）
		missing := []Profile{
			WorkBuddyPreset(), CodeBuddyPreset(), LobsterAIPreset(),
			TraeCNPreset(), TraeGlobalPreset(), QoderCNPreset(), QoderGlobalPreset(),
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
