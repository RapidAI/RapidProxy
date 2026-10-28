package proxysrv

import (
	"net/http"
	"testing"

	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// 自定义分组（custom-<hash>，Kind=custom）作为虚拟组成员时，应能正常解析与转发。
func TestVirtualGroupWithCustomProviderMember(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newTestServerIdle(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
			cfg.Profiles[i].Enabled = false
		}
		cfg.Profiles = append(cfg.Profiles, config.Profile{
			ID:         "custom-abcdef123456",
			Name:       "demo · OpenAI",
			Enabled:    true,
			Kind:       config.KindCustom,
			Protocol:   "openai",
			Auth:       config.AuthAPIKey,
			BaseURL:    up.URL,
			SyncModels: &off,
		})
		cfg.VirtualModels = []config.VirtualModel{{
			Name:    "fast",
			Enabled: true,
			Members: []config.VirtualMember{{Provider: "custom-abcdef123456", Model: "demo-model"}},
		}}
		cfg.Normalize()
	})
	cred := &upstream.Credential{
		Provider: "custom-abcdef123456", ID: "custom-abcdef123456-acct",
		Auth:    upstream.Tokens{AccessToken: "sk-demo"},
		Account: upstream.Account{UID: "demo", Nickname: "demo"},
	}
	if err := srv.store.Save(cred); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAccounts()
	srv.setModels("custom-abcdef123456", []upstream.ModelInfo{{ID: "demo-model", Name: "Demo"}})

	if n := srv.VirtualAvailableMembers("fast"); n != 1 {
		t.Fatalf("自定义分组成员应可用，实际可用 %d", n)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()

	status, body := postJSON(t, "http://"+srv.Addr()+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("虚拟组挂自定义成员应能转发，实际 %d: %s", status, body)
	}
	if up.hit("demo-model") == 0 {
		t.Error("请求应转发到自定义分组的模型")
	}
}

// 自定义分组未登录时不应算作可用成员。
func TestVirtualCustomMemberWithoutCredential(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newTestServerIdle(t, func(cfg *config.Config) {
		off := false
		cfg.Profiles = append(cfg.Profiles, config.Profile{
			ID: "custom-111111111111", Name: "x", Enabled: true,
			Kind: config.KindCustom, Protocol: "openai", Auth: config.AuthAPIKey,
			BaseURL: up.URL, SyncModels: &off,
		})
		cfg.VirtualModels = []config.VirtualModel{{
			Name: "fast", Enabled: true,
			Members: []config.VirtualMember{{Provider: "custom-111111111111", Model: "demo-model"}},
		}}
		cfg.Normalize()
	})
	srv.setModels("custom-111111111111", []upstream.ModelInfo{{ID: "demo-model"}})
	if n := srv.VirtualAvailableMembers("fast"); n != 0 {
		t.Errorf("未登录的自定义分组不应算可用，实际 %d", n)
	}
}

// 自定义上游没有 /models 端点时，用户手动登记的 ExtraModels 必须能进模型
// 清单——否则虚拟组编辑器里这个分组一个模型都选不到。
func TestCustomProviderManualModelsWithoutCatalog(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newTestServerIdle(t, func(cfg *config.Config) {
		off := false
		cfg.Profiles = append(cfg.Profiles, config.Profile{
			ID: "custom-222222222222", Name: "relay · Anthropic", Enabled: true,
			Kind: config.KindCustom, Protocol: "anthropic", Auth: config.AuthAPIKey,
			BaseURL: up.URL, SyncModels: &off,
			ExtraModels: []string{"claude-opus-4-5", "claude-sonnet-4-5"},
		})
		cfg.VirtualModels = []config.VirtualModel{{
			Name: "fast", Enabled: true,
			Members: []config.VirtualMember{{Provider: "custom-222222222222", Model: "claude-opus-4-5"}},
		}}
		cfg.Normalize()
	})
	cred := &upstream.Credential{
		Provider: "custom-222222222222", ID: "custom-222222222222-acct",
		Auth:    upstream.Tokens{AccessToken: "sk-demo"},
		Account: upstream.Account{UID: "demo", Nickname: "demo"},
	}
	if err := srv.store.Save(cred); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAccounts()
	// 同步一轮：上游没有 /models，模型清单只能来自 ExtraModels。
	srv.RefreshModels(nil)

	models := srv.LoggedInModels("custom-222222222222")
	want := map[string]bool{"claude-opus-4-5": false, "claude-sonnet-4-5": false}
	for _, m := range models {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("手填模型 %s 应出现在可用清单里，实际 %v", id, models)
		}
	}

	if n := srv.VirtualAvailableMembers("fast"); n != 1 {
		t.Fatalf("手填模型应让虚拟组成员可用，实际可用 %d", n)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	status, body := postJSON(t, "http://"+srv.Addr()+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("虚拟组应能转发到手填模型的成员，实际 %d: %s", status, body)
	}
	if up.hit("claude-opus-4-5") == 0 {
		t.Error("请求应带上手填的模型名转发")
	}
}
