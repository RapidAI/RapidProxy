package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/znsoftm/RapidProxy/internal/config"
)

// 虚拟组要能完整落盘再读回来：成员顺序、权重、启用状态都不能丢。
func TestVirtualModelPersistsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// 必须用 DefaultAt 指定路径：Default() 没有 path，Save 会写到真实数据目录，
	// 测试就会既污染用户配置、又读不到自己刚写的那份。
	cfg := config.DefaultAt(path)
	cfg.VirtualModels = []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4", Weight: 2},
			{Provider: "codebuddy", Model: "glm-5.3"},
		},
	}}
	cfg.Normalize()
	if err := cfg.Save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(loaded.VirtualModels) != 1 {
		t.Fatalf("应读回 1 个虚拟组，实际 %d", len(loaded.VirtualModels))
	}
	vm := loaded.VirtualModels[0]
	if vm.Name != "fast" || !vm.Enabled {
		t.Errorf("组名或启用状态丢失: %+v", vm)
	}
	if len(vm.Members) != 2 {
		t.Fatalf("应读回 2 个成员，实际 %d", len(vm.Members))
	}
	if vm.Members[0].Weight != 2 {
		t.Errorf("权重应保留为 2，实际 %d", vm.Members[0].Weight)
	}
	if vm.Members[1].EffectiveWeight() != 1 {
		t.Errorf("未设权重时应按 1 处理，实际 %d", vm.Members[1].EffectiveWeight())
	}
}

// 旧版本配置文件（没有 virtual_models 字段）要能正常加载，不能报错。
func TestVirtualModelAbsentInLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := `{"listen":"127.0.0.1:9999","api_keys":[],"profiles":[{"id":"workbuddy","name":"WorkBuddy","enabled":true}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("旧配置应能正常加载: %v", err)
	}
	if len(cfg.VirtualModels) != 0 {
		t.Errorf("旧配置不应凭空出现虚拟组，实际 %+v", cfg.VirtualModels)
	}
	if len(cfg.EnabledProfiles()) == 0 {
		t.Error("上游配置应正常加载")
	}
}

// 改名的完整流程：删掉旧名、写入新名，配置里不残留旧条目。
func TestVirtualModelRenameKeepsSingleEntry(t *testing.T) {
	// 纯内存操作，不调用 Save，因此不会写到真实数据目录。
	cfg := config.Default()
	if err := cfg.UpsertVirtualModel(config.VirtualModel{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}, ""); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	if err := cfg.UpsertVirtualModel(config.VirtualModel{
		Name:    "quick",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}, "fast"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}

	if len(cfg.VirtualModels) != 1 {
		t.Fatalf("改名后应只剩 1 个组，实际 %d: %+v", len(cfg.VirtualModels), cfg.VirtualModels)
	}
	if cfg.VirtualModels[0].Name != "quick" {
		t.Errorf("应改名为 quick，实际 %s", cfg.VirtualModels[0].Name)
	}
	if _, ok := cfg.VirtualModelByName("fast"); ok {
		t.Error("旧名字不应还能查到")
	}
}

// 删除后组不应再出现在启用列表里。
func TestRemoveVirtualModel(t *testing.T) {
	// 纯内存操作，不调用 Save。
	cfg := config.Default()
	if err := cfg.UpsertVirtualModel(config.VirtualModel{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}, ""); err != nil {
		t.Fatal(err)
	}
	if len(cfg.EnabledVirtualModels()) != 1 {
		t.Fatal("删除前应有 1 个启用的虚拟组")
	}
	if !cfg.RemoveVirtualModel("fast") {
		t.Fatal("删除应成功")
	}
	if len(cfg.EnabledVirtualModels()) != 0 {
		t.Errorf("删除后不应还有启用的虚拟组，实际 %+v", cfg.EnabledVirtualModels())
	}
}

// 没有成员的组不算启用：暴露一个永远调不通的模型名只会让人困惑。
func TestEnabledVirtualModelsRequiresMembers(t *testing.T) {
	// 纯内存操作，不调用 Save。
	cfg := config.Default()
	cfg.VirtualModels = []config.VirtualModel{{Name: "empty", Enabled: true}}
	cfg.Normalize()

	if got := cfg.EnabledVirtualModels(); len(got) != 0 {
		t.Errorf("没有成员的组不应算启用，实际 %+v", got)
	}
	// 但配置里仍保留这条，用户加完成员即可生效。
	if _, ok := cfg.VirtualModelByName("empty"); !ok {
		t.Error("空组本身应保留在配置里")
	}
}
