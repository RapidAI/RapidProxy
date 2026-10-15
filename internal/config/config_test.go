package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if cfg.Listen != DefaultListen {
		t.Errorf("默认监听地址 = %q", cfg.Listen)
	}
	if len(cfg.Profiles) != 8 {
		t.Fatalf("默认应有 8 个上游，实际 %d", len(cfg.Profiles))
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
	if len(c.Profiles) != 8 {
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
	if len(cfg.Profiles) != 8 {
		t.Fatalf("升级后应有 8 个上游，实际 %d", len(cfg.Profiles))
	}
	byID := map[string]Profile{}
	for _, p := range cfg.Profiles {
		byID[p.ID] = p
	}
	for _, id := range []string{"lobsterai", "trae-cn", "trae-global", "qoder-cn", "qoder-global", "opencode"} {
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
