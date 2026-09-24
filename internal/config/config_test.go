package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if cfg.Listen != DefaultListen {
		t.Errorf("默认监听地址 = %q", cfg.Listen)
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("默认应有 2 个上游，实际 %d", len(cfg.Profiles))
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
	if len(c.Profiles) != 2 {
		t.Errorf("上游未补默认值: %d", len(c.Profiles))
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
