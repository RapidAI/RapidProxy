package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if cfg.Listen != DefaultListen {
		t.Errorf("默认监听地址 = %q", cfg.Listen)
	}
	if len(cfg.Profiles) != 13 {
		t.Fatalf("默认应有 13 个上游，实际 %d", len(cfg.Profiles))
	}
	kinds := map[string]string{}
	for _, p := range cfg.Profiles {
		kinds[p.ID] = p.Kind
	}
	if kinds["lobsterai"] != KindLobsterAI || kinds["trae-cn"] != KindTrae ||
		kinds["trae-global"] != KindTrae || kinds["qoder-cn"] != KindQoder ||
		kinds["qoder-global"] != KindQoder || kinds["workbuddy"] != KindWorkBuddy ||
		kinds["opencode"] != KindOpenCode {
		t.Errorf("上游协议族标注不符合预期: %v", kinds)
	}
	for _, id := range []string{"xai", "claude", "chatgpt", "zhipu", "zcode"} {
		if kinds[id] != KindCustom {
			t.Errorf("上游 %s 应标注为 %s，实际 %q", id, KindCustom, kinds[id])
		}
	}
	enabled := cfg.EnabledProfiles()
	if len(enabled) != 1 || enabled[0].ID != "workbuddy" {
		t.Errorf("默认应只启用 workbuddy，实际 %+v", enabled)
	}
	if !cfg.AutoStartEnabled() || !cfg.CORSEnabled() || !cfg.SanitizeEnabled() {
		t.Error("默认开关状态不符合预期")
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("配置文件不存在时不应报错: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("监听地址 = %q", cfg.Listen)
	}
	cfg.Normalize()
}

func TestSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "0.0.0.0:9000"
	cfg.ModelSyncHours = 3
	cfg.AddAPIKey("sk-test-123")
	if err := cfg.Save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Listen != "0.0.0.0:9000" {
		t.Errorf("监听地址 = %q", reloaded.Listen)
	}
	if reloaded.ModelSyncHours != 3 {
		t.Errorf("同步间隔 = %d", reloaded.ModelSyncHours)
	}
	if !reloaded.CheckAPIKey("sk-test-123") {
		t.Error("API Key 未正确持久化")
	}
	if reloaded.CheckAPIKey("wrong") {
		t.Error("错误的 API Key 不应通过校验")
	}
}

func TestNormalizeFillsMissingFields(t *testing.T) {
	c := &Config{Listen: "8080"}
	c.Normalize()
	if c.Listen != "127.0.0.1:8080" {
		t.Errorf("端口应补全主机名，实际 %q", c.Listen)
	}
	if c.Platform != DefaultPlatform || c.Product != DefaultProduct {
		t.Error("platform / product 未补默认值")
	}
	if len(c.Profiles) != 13 {
		t.Errorf("上游未补默认值: %d", len(c.Profiles))
	}
}

// OpenCode 预设：设置里保存的 User-Agent 恒为裸 "opencode"——连线时改写成
// opencode/<semver> 只发生在上游侧，不能落进配置文件，否则按精确匹配识别
// opencode 客户端的 GLM 会读到被改写过的值。
func TestOpenCodePresetKeepsBareUserAgent(t *testing.T) {
	preset := OpenCodePreset()
	if preset.ID != "opencode" || preset.Kind != KindOpenCode {
		t.Fatalf("预设基本信息异常: %+v", preset)
	}
	if preset.UserAgent != "opencode" {
		t.Errorf("设置里保存的 User-Agent 应为裸 opencode，实际 %q", preset.UserAgent)
	}
	if preset.BaseURL != "https://opencode.ai/zen/v1" {
		t.Errorf("BaseURL = %q", preset.BaseURL)
	}
	if preset.Enabled {
		t.Error("OpenCode 预设默认应为关闭状态")
	}

	// 旧配置里只有 ID 的 opencode 条目：归一化补全字段，但不动用户显式
	// 填写的 User-Agent。
	c := &Config{Listen: "127.0.0.1:8787", Profiles: []Profile{{ID: "opencode"}}}
	c.Normalize()
	byID := map[string]Profile{}
	for _, p := range c.Profiles {
		byID[p.ID] = p
	}
	p, ok := byID["opencode"]
	if !ok {
		t.Fatal("归一化后缺少 opencode 预设")
	}
	if p.Kind != KindOpenCode || p.BaseURL != "https://opencode.ai/zen/v1" {
		t.Errorf("归一化未补全 opencode 字段: %+v", p)
	}
	if p.UserAgent != "opencode" {
		t.Errorf("归一化后 User-Agent = %q，应为裸 opencode", p.UserAgent)
	}

	c2 := &Config{Listen: "127.0.0.1:8787", Profiles: []Profile{{ID: "opencode", UserAgent: "custom-agent/9.9"}}}
	c2.Normalize()
	for _, got := range c2.Profiles {
		if got.ID == "opencode" && got.UserAgent != "custom-agent/9.9" {
			t.Errorf("用户自定义 User-Agent 被覆盖: %q", got.UserAgent)
		}
	}
}

// Clone 必须是深拷贝：改副本不能影响原配置，尤其是指针字段。
func TestCloneIsDeep(t *testing.T) {
	sync := true
	cfg := Default()
	cfg.APIKeys = []string{"sk-a"}
	cfg.Profiles[0].SyncModels = &sync
	cfg.Profiles[0].ExtraModels = []string{"extra-1"}

	cp := cfg.Clone()
	cp.APIKeys[0] = "sk-b"
	cp.Profiles[0].ExtraModels[0] = "extra-2"
	*cp.Profiles[0].SyncModels = false

	if cfg.APIKeys[0] != "sk-a" {
		t.Errorf("APIKeys 未深拷贝: %v", cfg.APIKeys)
	}
	if cfg.Profiles[0].ExtraModels[0] != "extra-1" {
		t.Errorf("ExtraModels 未深拷贝: %v", cfg.Profiles[0].ExtraModels)
	}
	if !*cfg.Profiles[0].SyncModels {
		t.Error("SyncModels 未深拷贝：改副本影响了原配置")
	}
}

// 配置文件损坏时用默认配置兜底，但路径必须保留，否则界面上显示的路径会是空的。
func TestDefaultAtKeepsPath(t *testing.T) {
	cfg := DefaultAt("/tmp/rapidproxy/config.json")
	if cfg.Path() != "/tmp/rapidproxy/config.json" {
		t.Errorf("路径 = %q", cfg.Path())
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("默认值未生效: %q", cfg.Listen)
	}
}
func TestModelSyncHoursZeroDisablesSync(t *testing.T) {
	c := Default()
	if c.ModelSyncHours != DefaultSyncHours {
		t.Fatalf("默认配置同步间隔应为 %d，实际 %d", DefaultSyncHours, c.ModelSyncHours)
	}
	if c.SyncInterval() <= 0 {
		t.Fatalf("默认配置应开启同步，实际间隔 %v", c.SyncInterval())
	}

	c.ModelSyncHours = 0
	c.Normalize()
	if c.ModelSyncHours != 0 {
		t.Errorf("显式设置的 0 不应被重置，实际 %d", c.ModelSyncHours)
	}
	if c.SyncInterval() != 0 {
		t.Errorf("0 应表示关闭同步，实际间隔 %v", c.SyncInterval())
	}

	c.ModelSyncHours = -3
	c.Normalize()
	if c.ModelSyncHours != 0 {
		t.Errorf("负数应被收敛为 0，实际 %d", c.ModelSyncHours)
	}
}

func TestBaseURLRewritesWildcardHost(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8787": "http://127.0.0.1:8787",
		"0.0.0.0:8787":   "http://127.0.0.1:8787",
		"[::]:8787":      "http://127.0.0.1:8787",
	}
	for listen, want := range cases {
		c := &Config{Listen: listen}
		if got := c.BaseURL(); got != want {
			t.Errorf("Listen=%q BaseURL()=%q want=%q", listen, got, want)
		}
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	cfg := Default()
	if cfg.RequiresAPIKey() {
		t.Error("默认不应要求 API Key")
	}
	key := GenerateAPIKey()
	if len(key) < 20 || key[:7] != "sk-wbp-" {
		t.Errorf("生成的密钥格式异常: %q", key)
	}
	if !cfg.AddAPIKey(key) {
		t.Error("添加密钥失败")
	}
	if cfg.AddAPIKey(key) {
		t.Error("重复添加应返回 false")
	}
	if !cfg.RequiresAPIKey() {
		t.Error("设置密钥后应要求校验")
	}
	if !cfg.RemoveAPIKey(key) {
		t.Error("删除密钥失败")
	}
	if cfg.RequiresAPIKey() {
		t.Error("删除全部密钥后不应再要求校验")
	}
}

func TestCloneIsolatesMutations(t *testing.T) {
	cfg := Default()
	clone := cfg.Clone()
	clone.Listen = "changed:1"
	clone.Profiles[0].Enabled = false
	clone.AddAPIKey("sk-abcdefgh")

	if cfg.Listen == "changed:1" {
		t.Error("Clone 未隔离 Listen")
	}
	if !cfg.Profiles[0].Enabled {
		t.Error("Clone 未隔离 Profiles")
	}
	if len(cfg.APIKeys) != 0 {
		t.Error("Clone 未隔离 APIKeys")
	}
}

// 旧版本配置文件里只有当时的上游清单：加载后必须补齐新增预设，
// 否则升级用户在界面上永远看不到新增的服务商。
func TestOldConfigGainsNewPresets(t *testing.T) {
	raw := []byte(`{"listen":"127.0.0.1:8787","profiles":[` +
		`{"id":"workbuddy","name":"WorkBuddy（国际版）","enabled":true,"base_url":"https://www.workbuddy.ai"},` +
		`{"id":"codebuddy","name":"CodeBuddy（国内版）","enabled":false,"base_url":"https://copilot.tencent.com"}]}`)
	cfg := DefaultAt(filepath.Join(t.TempDir(), "config.json"))
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	if len(cfg.Profiles) != 13 {
		t.Fatalf("升级后应有 13 个上游，实际 %d", len(cfg.Profiles))
	}
	byID := map[string]Profile{}
	for _, p := range cfg.Profiles {
		byID[p.ID] = p
	}
	for _, id := range []string{"lobsterai", "trae-cn", "trae-global", "qoder-cn", "qoder-global", "opencode", "xai", "claude", "chatgpt", "zhipu", "zcode"} {
		p, ok := byID[id]
		if !ok {
			t.Fatalf("升级后缺少预设 %s", id)
		}
		if p.Kind == "" {
			t.Fatalf("补齐的预设 %s 未标注协议族", id)
		}
	}
	// 用户原有配置不被覆盖：开关与自定义字段保持原样。
	if !byID["workbuddy"].Enabled || byID["codebuddy"].Enabled {
		t.Fatal("升级不应改动既有上游的启用状态")
	}
}

// intl 是国际版上游的别名，占用它会让 intl:模型名 的强制路由失效。
// 错误信息必须点明「保留名」，而不是复用「不能为空 / 不能含分隔符」那条——
// 后者会让用户一头雾水。
func TestValidateVirtualModelRejectsIntl(t *testing.T) {
	cfg := Default()
	err := cfg.ValidateVirtualModel(VirtualModel{Name: "intl"}, "")
	if err == nil {
		t.Fatal("intl 应被拒绝")
	}
	if !strings.Contains(err.Error(), "保留") {
		t.Errorf("错误信息应说明 intl 是保留名，实际 %q", err.Error())
	}
	// 大小写不同也应拒绝。
	if err := cfg.ValidateVirtualModel(VirtualModel{Name: "INTL"}, ""); err == nil {
		t.Fatal("INTL 也应被拒绝")
	}
}

// 手写配置可能塞进与上游 ID 同名的虚拟组。normalize 必须丢弃它，
// 否则 resolveRoute 的「虚拟组优先」会把该上游的裸模型名全部截走。
func TestNormalizeDropsVirtualNameCollidingWithProvider(t *testing.T) {
	cfg := Default()
	cfg.VirtualModels = []VirtualModel{
		{Name: "workbuddy", Enabled: true, Members: []VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}}},
		{Name: "ok", Enabled: true, Members: []VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}}},
	}
	cfg.Normalize()

	for _, vm := range cfg.VirtualModels {
		if vm.Name == "workbuddy" {
			t.Fatal("与上游 ID 同名的虚拟组应被丢弃")
		}
	}
	if len(cfg.VirtualModels) != 1 || cfg.VirtualModels[0].Name != "ok" {
		t.Errorf("合法的那条应保留，实际 %+v", cfg.VirtualModels)
	}
}

// 旧版本把智谱编程错配成普通 Anthropic 网关 + 小写 opencode 身份，会走按量
// 付费并返回 [1113]。升级时必须迁移到 /api/coding/paas/v4 + OpenCode。
func TestZhipuCodingMigratesLegacyEndpoint(t *testing.T) {
	raw := []byte(`{"profiles":[` +
		`{"id":"zcode","name":"智谱编程（ZCode 登录）","enabled":true,` +
		`"kind":"custom","protocol":"anthropic",` +
		`"base_url":"https://open.bigmodel.cn/api/anthropic/v1",` +
		`"user_agent":"opencode"}]}`)
	cfg := DefaultAt(filepath.Join(t.TempDir(), "config.json"))
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	p, ok := cfg.ProfileByID("zcode")
	if !ok {
		t.Fatal("zcode profile missing")
	}
	if p.BaseURL != "https://open.bigmodel.cn/api/coding/paas/v4" {
		t.Fatalf("base_url not migrated: %q", p.BaseURL)
	}
	if p.Protocol != "openai" {
		t.Fatalf("protocol not migrated: %q", p.Protocol)
	}
	if p.UserAgent != "OpenCode" {
		t.Fatalf("user_agent not migrated: %q", p.UserAgent)
	}
	// 端点迁移走的是独立分支（直接 return，不走 setIfEmpty）：auth 必须在那里
	// 一起补齐，否则老用户升级后仍是 apikey，界面上没有浏览器登录入口。
	if !p.UsesOAuth() {
		t.Fatalf("旧配置迁移后应升级为 OAuth，实际 auth=%q", p.Auth)
	}
}

// 用户自定义过的端点不应被迁移覆盖。
func TestZhipuCodingKeepsCustomEndpoint(t *testing.T) {
	raw := []byte(`{"profiles":[` +
		`{"id":"zhipu","name":"我的智谱","enabled":true,` +
		`"kind":"custom","protocol":"openai",` +
		`"base_url":"https://my.proxy.example/v1","user_agent":"MyClient"}]}`)
	cfg := DefaultAt(filepath.Join(t.TempDir(), "config.json"))
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	p, _ := cfg.ProfileByID("zhipu")
	if p.BaseURL != "https://my.proxy.example/v1" || p.UserAgent != "MyClient" || p.Protocol != "openai" {
		t.Fatalf("custom endpoint overwritten: %+v", p)
	}
}

// 普通开放平台网关（/api/paas/v4）同样会走按量付费，升级时也要迁到编程套餐网关。
func TestZhipuCodingMigratesOpenPlatformEndpoint(t *testing.T) {
	raw := []byte(`{"profiles":[` +
		`{"id":"zhipu","name":"智谱编程（GLM）","enabled":true,` +
		`"kind":"custom","protocol":"openai",` +
		`"base_url":"https://open.bigmodel.cn/api/paas/v4","user_agent":"opencode"}]}`)
	cfg := DefaultAt(filepath.Join(t.TempDir(), "config.json"))
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	p, _ := cfg.ProfileByID("zhipu")
	if p.BaseURL != "https://open.bigmodel.cn/api/coding/paas/v4" {
		t.Fatalf("base_url not migrated: %q", p.BaseURL)
	}
	if p.UserAgent != "OpenCode" {
		t.Fatalf("user_agent not migrated: %q", p.UserAgent)
	}
}

// 四家服务商改成 OAuth 登录后，预设必须带上 auth=oauth；用户显式写
// auth=apikey 时保持粘贴密钥（两条路都支持，默认走浏览器授权）。
func TestOAuthPresets(t *testing.T) {
	cases := []struct {
		id     string
		oauth  string
		baseUI string
	}{
		{"xai", OAuthXAI, "https://api.x.ai/v1"},
		{"claude", OAuthClaude, "https://api.anthropic.com/v1"},
		// ChatGPT 的 OAuth 令牌只能走 Codex 订阅线路，不是 api.openai.com。
		{"chatgpt", OAuthChatGPT, "https://chatgpt.com/backend-api/codex"},
		{"zhipu", OAuthZCode, "https://open.bigmodel.cn/api/coding/paas/v4"},
		{"zcode", OAuthZCode, "https://open.bigmodel.cn/api/coding/paas/v4"},
	}
	cfg := Default()
	for _, tc := range cases {
		p, ok := cfg.ProfileByID(tc.id)
		if !ok {
			t.Fatalf("%s 预设缺失", tc.id)
		}
		if !p.UsesOAuth() {
			t.Errorf("%s 默认应走 OAuth 登录，实际 auth=%q", tc.id, p.Auth)
		}
		if got := p.OAuthKind(); got != tc.oauth {
			t.Errorf("%s OAuthKind = %q, want %q", tc.id, got, tc.oauth)
		}
		if p.BaseURL != tc.baseUI {
			t.Errorf("%s BaseURL = %q, want %q", tc.id, p.BaseURL, tc.baseUI)
		}
	}
}

// 用户自建组没有 OAuth 实现：即便写了 auth=oauth 也要退回粘贴密钥，
// 否则会卡在「要求 OAuth 但没有实现」的死角。
func TestOAuthFallsBackForCustomGroups(t *testing.T) {
	cfg := &Config{
		Profiles: []Profile{
			{ID: "custom-abc123", Name: "自建", Kind: KindCustom, Auth: AuthOAuth},
		},
	}
	cfg.Normalize()
	p := cfg.Profiles[0]
	if p.Auth != AuthAPIKey {
		t.Errorf("自建组应退回 apikey，实际 %q", p.Auth)
	}
	if p.OAuthProvider != "" {
		t.Errorf("退回 apikey 后不应残留 OAuthProvider，实际 %q", p.OAuthProvider)
	}
}

// 用户把预设的 auth 显式改回 apikey 时，必须尊重用户选择（粘贴密钥仍可用）。
func TestExplicitAPIKeyAuthIsRespected(t *testing.T) {
	cfg := &Config{
		Profiles: []Profile{
			{ID: "claude", Kind: KindCustom, Auth: AuthAPIKey},
		},
	}
	cfg.Normalize()
	p := cfg.Profiles[0]
	if p.UsesOAuth() {
		t.Error("显式 apikey 不应被改回 OAuth")
	}
	if p.OAuthKind() != "" {
		t.Errorf("非 OAuth 时 OAuthKind 应为空，实际 %q", p.OAuthKind())
	}
}

// 非 custom 协议族不参与 Auth 字段（它们各自有登录实现）。
func TestAuthFieldClearedForNonCustomKinds(t *testing.T) {
	cfg := &Config{
		Profiles: []Profile{
			{ID: "workbuddy", Kind: KindWorkBuddy, Auth: AuthOAuth, OAuthProvider: OAuthClaude},
		},
	}
	cfg.Normalize()
	p := cfg.Profiles[0]
	if p.Auth != "" || p.OAuthProvider != "" {
		t.Errorf("非 custom 上游应清空 auth 字段，实际 auth=%q oauth=%q", p.Auth, p.OAuthProvider)
	}
}

// 旧配置文件（auth 字段缺失）升级后必须变成 OAuth，否则老用户看不到新的
// 浏览器登录入口。
func TestLegacyConfigMigratesToOAuth(t *testing.T) {
	cfg := &Config{
		Profiles: []Profile{
			{ID: "xai", Name: "xAI（Grok）", Kind: KindCustom, Protocol: "openai", BaseURL: "https://api.x.ai/v1"},
			{ID: "claude", Name: "Claude（Anthropic）", Kind: KindCustom, Protocol: "anthropic", BaseURL: "https://api.anthropic.com/v1"},
			{ID: "chatgpt", Name: "ChatGPT（OpenAI）", Kind: KindCustom, Protocol: "openai", BaseURL: "https://api.openai.com/v1"},
			{ID: "zhipu", Name: "智谱编程（GLM）", Kind: KindCustom, Protocol: "openai", BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4"},
		},
	}
	cfg.Normalize()
	for _, id := range []string{"xai", "claude", "chatgpt", "zhipu"} {
		p, ok := cfg.ProfileByID(id)
		if !ok {
			t.Fatalf("%s 缺失", id)
		}
		if !p.UsesOAuth() {
			t.Errorf("%s 旧配置应升级为 OAuth，实际 auth=%q", id, p.Auth)
		}
		if p.OAuthKind() == "" {
			t.Errorf("%s 缺少 OAuth 实现标识", id)
		}
	}
	// ChatGPT 的旧端点（api.openai.com）不能用于 OAuth 订阅线路：必须迁移。
	if p, _ := cfg.ProfileByID("chatgpt"); p.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Errorf("chatgpt BaseURL 未迁移，实际 %q", p.BaseURL)
	}
}
