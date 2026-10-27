package proxysrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/stats"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// virtualStub 是一个按模型名与令牌决定成败的假上游。
//
// failModels 里的模型名返回 failStatus，failToken 命中的账号同样失败；
// 其余请求返回 200 + 一段带 usage 的 SSE。
type virtualStub struct {
	*httptest.Server
	// hits 记录每个模型被请求的次数（用于断言负载分摊）。
	// httptest 的 handler 在独立 goroutine 里跑，并发测试下必须加锁。
	mu   sync.Mutex
	hits map[string]int
	// syncModels 非空时作为 /v3/config 的模型清单返回，
	// 用于验证「模型同步是否覆盖到该上游」。
	syncModels []map[string]any
	// configHits 记录 /config 端点被请求的次数（按账号令牌区分）。
	configHits map[string]int
}

// hit 返回某模型被请求的次数。
func (v *virtualStub) hit(model string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hits[model]
}

// configHit 返回含指定令牌片段的 /config 请求次数。
// 实际请求头是 "Bearer <token>"，所以按包含匹配而不是精确相等。
func (v *virtualStub) configHit(tokenPart string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for auth, c := range v.configHits {
		if strings.Contains(auth, tokenPart) {
			n += c
		}
	}
	return n
}

// snapshot 返回当前计数副本，便于断言整体分布。
func (v *virtualStub) snapshot() map[string]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]int, len(v.hits))
	for k, n := range v.hits {
		out[k] = n
	}
	return out
}

func newVirtualStub(t *testing.T, failModels map[string]int, failToken string) *virtualStub {
	t.Helper()
	st := &virtualStub{hits: map[string]int{}, configHits: map[string]int{}}
	st.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			st.mu.Lock()
			st.configHits[r.Header.Get("Authorization")]++
			models := st.syncModels
			st.mu.Unlock()
			if models == nil {
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
				return
			}
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"models": models}})
			_, _ = w.Write(raw)
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		st.mu.Lock()
		st.hits[payload.Model]++
		st.mu.Unlock()

		if failToken != "" && strings.Contains(r.Header.Get("Authorization"), failToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":429,"msg":"rate limit exceeded"}`))
			return
		}
		if status, ok := failModels[payload.Model]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"model not available for this account"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(st.Close)
	return st
}

// newVirtualServer 构造一个所有上游都指向同一假上游、并登录了指定账号的服务。
func newVirtualServer(t *testing.T, up string, vms []config.VirtualModel, providers []string) *Server {
	t.Helper()
	srv := newTestServerIdle(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up
			cfg.Profiles[i].Origin = up
			cfg.Profiles[i].SyncModels = &off
			cfg.Profiles[i].Enabled = false
		}
		for _, id := range providers {
			p, ok := cfg.ProfileByID(id)
			if !ok {
				t.Fatalf("测试引用了未知上游 %s", id)
			}
			p.Enabled = true
			cfg.SetProfile(p)
		}
		cfg.VirtualModels = vms
		cfg.Normalize()
	})
	for _, id := range providers {
		cred := &upstream.Credential{
			Provider: id, ID: id + "-acct",
			Auth:    upstream.Tokens{AccessToken: "token-" + id, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: id, Nickname: id},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatalf("写入凭据失败: %v", err)
		}
	}
	srv.InvalidateAccounts()
	return srv
}

// 虚拟组名命中后应解析成虚拟组，而不是被当成未知模型落到默认上游。
func TestResolveRouteFindsVirtualGroup(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}, []string{"workbuddy"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, _, vm, err := srv.resolveRoute("fast", req)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if vm == nil || vm.Name != "fast" {
		t.Fatalf("应解析到虚拟组 fast，实际 %v", vm)
	}
}

// 显式强制路由的优先级高于虚拟组：客户端写了上游前缀就该打到那个上游。
func TestResolveRouteForcedBeatsVirtual(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}, []string{"workbuddy"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-RapidProxy-Provider", "workbuddy")
	profile, model, vm, err := srv.resolveRoute("fast", req)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if vm != nil {
		t.Fatalf("强制路由时不应命中虚拟组，实际 %v", vm)
	}
	if profile.ID != "workbuddy" || model != "fast" {
		t.Fatalf("应按强制路由转发 workbuddy/fast，实际 %s/%s", profile.ID, model)
	}
}

// 停用的虚拟组不应被解析：名字应回落到常规匹配路径。
func TestResolveRouteSkipsDisabledVirtual(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: false,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}, []string{"workbuddy"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, _, vm, err := srv.resolveRoute("fast", req)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if vm != nil {
		t.Fatal("停用的虚拟组不应被解析")
	}
}

// 虚拟组要出现在 /v1/models 里，否则客户端根本发现不了这个别名。
func TestVirtualGroupAppearsInModelList(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4"},
			{Provider: "codebuddy", Model: "glm-5.3"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	var found bool
	for _, m := range srv.AllModels() {
		if m.ID == "fast" {
			found = true
			if m.Provider != "virtual" {
				t.Errorf("虚拟条目应以 provider=virtual 标记，实际 %s", m.Provider)
			}
		}
	}
	if !found {
		t.Fatalf("虚拟组 fast 应出现在模型列表，实际 %v", srv.ModelIDs())
	}
}

// 没有成员的上游都不可用（未登录）时，虚拟组不应出现在列表里。
func TestVirtualGroupHiddenWithoutUsableMember(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	// 只登录 workbuddy，成员却指向未启用的 codebuddy。
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "codebuddy", Model: "glm-5.3"}},
	}}, []string{"workbuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	for _, m := range srv.AllModels() {
		if m.ID == "fast" {
			t.Fatal("没有可用成员的虚拟组不应暴露")
		}
	}
}

// 核心行为：成员之间按权重分摊。
func TestVirtualGroupBalancesAcrossMembers(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4"},
			{Provider: "codebuddy", Model: "glm-5.3"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	for i := 0; i < 20; i++ {
		status, body := postJSON(t, base+"/v1/chat/completions",
			`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
		if status != http.StatusOK {
			t.Fatalf("请求失败 %d: %s", status, body)
		}
	}
	// 同权两个成员，20 次请求应各拿到约 10 次。
	a, b := up.hit("gpt-5.4"), up.hit("glm-5.3")
	if a+b != 20 {
		t.Fatalf("总请求数应为 20，实际 %d", a+b)
	}
	if a < 5 || b < 5 {
		t.Fatalf("两个成员应大致均分，实际 gpt-5.4=%d glm-5.3=%d", a, b)
	}
}

// 某个成员在上游侧失败时，请求应自动换到另一个成员，而不是把错误抛给下游。
func TestVirtualGroupSkipsFailingMember(t *testing.T) {
	// gpt-5.4 在假上游里固定返回 400。
	up := newVirtualStub(t, map[string]int{"gpt-5.4": http.StatusBadRequest}, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4"},
			{Provider: "codebuddy", Model: "glm-5.3"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	for i := 0; i < 6; i++ {
		status, body := postJSON(t, base+"/v1/chat/completions",
			`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
		if status != http.StatusOK {
			t.Fatalf("成员失败时应换人成功，实际 %d: %s", status, body)
		}
		if !strings.Contains(string(body), "ok") {
			t.Fatalf("应拿到健康成员的应答，实际 %s", body)
		}
	}
	if up.hit("glm-5.3") < 6 {
		t.Fatalf("所有请求都应由 glm-5.3 承接，实际 %v", up.snapshot())
	}
}

// 全部成员都不可用时，返回可排查的错误，而不是 200 + 空 body。
func TestVirtualGroupAllMembersFail(t *testing.T) {
	up := newVirtualStub(t, map[string]int{
		"gpt-5.4": http.StatusBadRequest,
		"glm-5.3": http.StatusBadRequest,
	}, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4"},
			{Provider: "codebuddy", Model: "glm-5.3"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("客户端错误应按原状态码带回，实际 %d: %s", status, body)
	}
	if !strings.Contains(string(body), "error") {
		t.Fatalf("应返回错误体，实际 %s", body)
	}
}

// 虚拟组维度的流量要单独可查，并给出成员分摊明细。
func TestVirtualGroupTrafficIsRecorded(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}, []string{"workbuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	// 非流式请求会把上游的 SSE 折叠成完整响应，usage 随之被记录。
	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("请求失败 %d: %s", status, body)
	}

	st := srv.Stats()
	if st == nil {
		t.Fatal("流量记录器应可用")
	}
	res := st.Query("fast", "", "day", time.Now())
	if res.Kind != "virtual" {
		t.Errorf("应按虚拟维度查询，实际 kind=%s", res.Kind)
	}
	if res.Summary.Total != 15 {
		t.Errorf("虚拟组应记到 15 个 token，实际 %d", res.Summary.Total)
	}
	if len(res.Members) != 1 {
		t.Fatalf("应有一个成员明细，实际 %v", res.Members)
	}
	if res.Members[0].Provider != "workbuddy" || res.Members[0].Model != "gpt-5.4" {
		t.Errorf("成员明细指向错误: %+v", res.Members[0])
	}

	// 真实上游维度同样要有记录：额度核对靠它。
	real := st.Query("workbuddy", "gpt-5.4", "day", time.Now())
	if real.Summary.Total != 15 {
		t.Errorf("真实维度也应记到 15 个 token，实际 %d", real.Summary.Total)
	}

	// 列表里能选到虚拟组，且标记为 virtual。
	var found bool
	for _, s := range st.Scopes() {
		if s.Kind == "virtual" && s.Provider == "fast" {
			found = true
		}
	}
	if !found {
		t.Errorf("Scopes 应包含虚拟组 fast，实际 %+v", st.Scopes())
	}
}

// 改名时历史流量要跟着迁移，否则用户会看到新组从零开始。
func TestVirtualRenameMigratesTraffic(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, nil, []string{"workbuddy"})
	st := srv.Stats()

	st.Record(statsEntry("fast", "workbuddy", "gpt-5.4", 100))
	st.RenameVirtual("fast", "quick")

	if got := st.Query("fast", "", "day", time.Now()).Summary.Total; got != 0 {
		t.Errorf("旧名不应再有数据，实际 %d", got)
	}
	res := st.Query("quick", "", "day", time.Now())
	if res.Summary.Total != 100 {
		t.Errorf("新名应继承 100 个 token，实际 %d", res.Summary.Total)
	}
	if len(res.Members) != 1 || res.Members[0].Model != "gpt-5.4" {
		t.Errorf("成员明细应随迁移保留，实际 %+v", res.Members)
	}
}

// 删除虚拟组时清掉历史流量，避免重建同名组时继承旧数据。
func TestVirtualDropClearsTraffic(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, nil, []string{"workbuddy"})
	st := srv.Stats()

	st.Record(statsEntry("fast", "workbuddy", "gpt-5.4", 100))
	st.DropVirtual("fast")

	if got := st.Query("fast", "", "day", time.Now()).Summary.Total; got != 0 {
		t.Errorf("删除后不应再有数据，实际 %d", got)
	}
}

// 成员按权重分摊：权重 2 的成员应拿到约两倍的请求。
func TestVirtualGroupRespectsWeights(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4", Weight: 1},
			{Provider: "codebuddy", Model: "glm-5.3", Weight: 2},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	for i := 0; i < 30; i++ {
		if status, body := postJSON(t, base+"/v1/chat/completions",
			`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil); status != http.StatusOK {
			t.Fatalf("请求失败 %d: %s", status, body)
		}
	}
	a, b := up.hit("gpt-5.4"), up.hit("glm-5.3")
	if b <= a {
		t.Fatalf("权重 2 的成员应多于权重 1 的，实际 gpt-5.4=%d glm-5.3=%d", a, b)
	}
	if a < 7 || a > 13 {
		t.Fatalf("权重 1 的成员应拿到约 1/3，实际 %d/30", a)
	}
}

// 全部成员都在冷却时，应换到能用的那个（另一个上游的账号不受影响）。
func TestVirtualGroupSkipsCoolingMember(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "gpt-5.4"},
			{Provider: "codebuddy", Model: "glm-5.3"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	// 手工把 workbuddy 的账号打进长冷却。
	srv.markCooldown("workbuddy-acct", failCredit, 0)

	if n := srv.VirtualAvailableMembers("fast"); n != 1 {
		t.Fatalf("冷却中的成员不应计入可用，实际可用 %d", n)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	for i := 0; i < 5; i++ {
		if status, body := postJSON(t, base+"/v1/chat/completions",
			`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil); status != http.StatusOK {
			t.Fatalf("应换到健康成员，实际 %d: %s", status, body)
		}
	}
	if up.hit("gpt-5.4") != 0 {
		t.Fatalf("冷却中的成员不应被选中，实际命中 %d 次", up.hit("gpt-5.4"))
	}
}

// 虚拟名不能与上游 ID 冲突，也不能含强制路由分隔符。
func TestVirtualNameValidation(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"fast", true},
		{"my-model_1", true},
		{"", false},
		{"a:b", false},
		{"a/b", false},
		{"a b", false},
		{"intl", false},
		{"INTL", false},
	}
	for _, tc := range cases {
		if got := config.ValidVirtualName(tc.name); got != tc.valid {
			t.Errorf("ValidVirtualName(%q) = %v，期望 %v", tc.name, got, tc.valid)
		}
	}
}

// 保存时拒绝与已有组重名（但允许「改名成自己」）。
func TestUpsertVirtualModelRejectsDuplicate(t *testing.T) {
	cfg := config.Default()
	if err := cfg.UpsertVirtualModel(config.VirtualModel{Name: "fast", Enabled: true}, ""); err != nil {
		t.Fatalf("首次新增不应失败: %v", err)
	}
	if err := cfg.UpsertVirtualModel(config.VirtualModel{Name: "fast", Enabled: true}, ""); err == nil {
		t.Fatal("重名新增应失败")
	}
	// 改自己：原名 fast，仍叫 fast，允许。
	if err := cfg.UpsertVirtualModel(config.VirtualModel{Name: "fast", Enabled: false}, "fast"); err != nil {
		t.Fatalf("改名成自己不应失败: %v", err)
	}
	// 与上游 ID 冲突。
	if err := cfg.UpsertVirtualModel(config.VirtualModel{Name: "workbuddy"}, ""); err == nil {
		t.Fatal("与上游 ID 冲突应失败")
	}
}

// normalize 丢弃坏条目，但不该让整个配置不可用。
func TestVirtualModelsNormalizeDropsInvalid(t *testing.T) {
	cfg := config.Default()
	cfg.VirtualModels = []config.VirtualModel{
		{Name: "ok", Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}}},
		{Name: "bad:name"}, // 非法名字
		{Name: "unknown-provider", Members: []config.VirtualMember{{Provider: "nope", Model: "x"}}},
		{Name: "ok", Members: []config.VirtualMember{{Provider: "workbuddy", Model: "dup"}}}, // 重名
	}
	cfg.Normalize()

	if len(cfg.VirtualModels) != 2 {
		t.Fatalf("应保留 2 个组（ok 与 unknown-provider），实际 %d: %+v", len(cfg.VirtualModels), cfg.VirtualModels)
	}
	if cfg.VirtualModels[0].Name != "ok" || len(cfg.VirtualModels[0].Members) != 1 {
		t.Errorf("第一个组应完整保留，实际 %+v", cfg.VirtualModels[0])
	}
	// 指向未知上游的成员被丢弃，组本身保留（用户可后续补成员）。
	if len(cfg.VirtualModels[1].Members) != 0 {
		t.Errorf("未知上游的成员应被丢弃，实际 %+v", cfg.VirtualModels[1].Members)
	}
}

// Clone 必须深拷贝成员切片，否则改副本会连带改到运行中的配置。
func TestCloneDeepCopiesVirtualMembers(t *testing.T) {
	cfg := config.Default()
	cfg.VirtualModels = []config.VirtualModel{{
		Name:    "fast",
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}
	cp := cfg.Clone()
	cp.VirtualModels[0].Members[0].Model = "changed"

	if cfg.VirtualModels[0].Members[0].Model != "gpt-5.4" {
		t.Fatal("Clone 未深拷贝虚拟组成员")
	}
}

// statsEntry 是构造一条测试用流量记录的辅助函数。
func statsEntry(virtual, provider, model string, total int64) stats.Entry {
	return stats.Entry{
		Virtual:  virtual,
		Provider: provider,
		Model:    model,
		Total:    total,
		In:       total,
		At:       time.Now(),
	}
}

// 虚拟组换成员时会对同一份 payload 反复调用 prepareBody。
// 若改写会累积（例如 system 提示被插多次），第二个成员拿到的请求体就是坏的。
func TestPrepareBodyDoesNotAccumulateAcrossMembers(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	payload := map[string]any{
		"model":    "x",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	}
	wb, _ := config.Default().ProfileByID("workbuddy")
	qoder, _ := config.Default().ProfileByID("qoder-cn")

	body1, err := srv.prepareBody(payload, wb)
	if err != nil {
		t.Fatal(err)
	}
	body2, err := srv.prepareBody(payload, wb)
	if err != nil {
		t.Fatal(err)
	}
	if string(body1) != string(body2) {
		t.Fatalf("同一 payload 两次准备结果应一致\n1: %s\n2: %s", body1, body2)
	}

	// 换到另一个协议族再准备，不能带上前一族的改写痕迹。
	body3, err := srv.prepareBody(payload, qoder)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body3, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["stream_options"]; ok {
		t.Error("非 WorkBuddy 族不应带 stream_options（说明前一次改写泄漏了）")
	}
}

// 组内成员全部不可用（账号被暂停）时，应回 401 而不是 502：
// 502 会让调用方当成可重试的网关抖动而反复重发同一个请求。
// 文案也要指明是哪个虚拟组，并给出能指导下一步的原因。
func TestVirtualGroupAllMembersUnavailable(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}, []string{"workbuddy"})

	cred, err := srv.store.Get("workbuddy", "workbuddy-acct")
	if err != nil {
		t.Fatal(err)
	}
	cred.Paused = true
	if err := srv.store.Save(cred); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAccounts()

	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("全部成员不可用应回 401，实际 %d: %s", status, body)
	}
	if !strings.Contains(string(body), "fast") {
		t.Errorf("文案应指明虚拟组名，实际 %s", body)
	}
}

// 成员多于旧上限（6）时，排在后面的成员也必须被尝试到。
func TestVirtualGroupTriesAllMembersBeyondSix(t *testing.T) {
	// 前 6 个成员都固定失败，第 7 个正常。
	fail := map[string]int{}
	var members []config.VirtualMember
	for i := 0; i < 6; i++ {
		id := "m" + string(rune('0'+i))
		fail[id] = http.StatusBadRequest
		members = append(members, config.VirtualMember{Provider: "workbuddy", Model: id})
	}
	members = append(members, config.VirtualMember{Provider: "workbuddy", Model: "good"})

	up := newVirtualStub(t, fail, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name: "fast", Enabled: true, Members: members,
	}}, []string{"workbuddy"})

	var infos []upstream.ModelInfo
	for _, m := range members {
		infos = append(infos, upstream.ModelInfo{ID: m.Model, Name: m.Model})
	}
	srv.setModels("workbuddy", infos)

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("第 7 个成员应被尝试并成功，实际 %d: %s", status, body)
	}
	if up.hit("good") == 0 {
		t.Fatalf("最后一个成员从未被调用，实际命中 %v", up.snapshot())
	}
}

// 虚拟组的元信息应取成员中最宽的那份：上下文取最大、输出上限取最大、
// 任一成员支持图片即视为支持。
func TestVirtualEntryMergesWidestMemberInfo(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "a"}, // 上下文小、输出大、支持图片
			{Provider: "codebuddy", Model: "b"}, // 上下文大、输出小、不支持图片
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{
		{ID: "a", ContextLength: 1000, MaxOutputTokens: 8000, SupportsImages: true},
	})
	srv.setModels("codebuddy", []upstream.ModelInfo{
		{ID: "b", ContextLength: 5000, MaxOutputTokens: 100, SupportsImages: false},
	})

	for _, m := range srv.AllModels() {
		if m.ID != "fast" {
			continue
		}
		if m.ContextLength != 5000 {
			t.Errorf("上下文应取最大值 5000，实际 %d", m.ContextLength)
		}
		if m.MaxOutputTokens != 8000 {
			t.Errorf("输出上限应取最大值 8000，实际 %d", m.MaxOutputTokens)
		}
		if !m.SupportsImages {
			t.Error("任一成员支持图片即应视为支持")
		}
		return
	}
	t.Fatal("未找到虚拟组条目")
}

// 并发请求 + 同时改配置（模拟界面上编辑虚拟组）不能 panic 或竞态。
func TestVirtualGroupConcurrentWithConfigChange(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name: "fast", Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "a"},
			{Provider: "codebuddy", Model: "b"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "b", Name: "B"}})

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	var wg sync.WaitGroup
	// 并发请求。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				postJSON(t, base+"/v1/chat/completions",
					`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
			}
		}()
	}
	// 并发改配置：改成员权重、启停。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			cfg := srv.Config().Clone()
			cfg.VirtualModels[0].Members[0].Weight = (j % 3) + 1
			cfg.VirtualModels[0].Enabled = j%2 == 0
			cfg.Normalize()
			srv.SetConfig(cfg)
			srv.ClearVirtualState("fast")
		}
	}()
	// 并发读统计。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			_ = srv.AllModels()
			if st := srv.Stats(); st != nil {
				_ = st.Query("fast", "", "day", time.Now())
				_ = st.Scopes()
			}
		}
	}()
	wg.Wait()
}

// 原因判定的三种情形要各自准确：上游未启用 / 未登录 / 账号全暂停。
func TestVirtualUnavailableReasonAccurate(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, nil, []string{"workbuddy"})
	cfg := srv.Config()

	// 情形 1：上游未启用（成员指向未启用的 codebuddy）。
	vmDisabled := config.VirtualModel{
		Name:    "a",
		Members: []config.VirtualMember{{Provider: "codebuddy", Model: "x"}},
	}
	got := srv.virtualUnavailableReason(cfg, &vmDisabled)
	if !strings.Contains(got, "未启用") {
		t.Errorf("未启用上游应提示启用，实际 %q", got)
	}

	// 情形 2：上游已启用但没登录（lobsterai 默认启用？用未登录的 trae-cn 更稳）。
	cfg2 := cfg.Clone()
	if p, ok := cfg2.ProfileByID("trae-cn"); ok {
		p.Enabled = true
		cfg2.SetProfile(p)
	}
	srv.SetConfig(cfg2)
	vmNoAccount := config.VirtualModel{
		Name:    "b",
		Members: []config.VirtualMember{{Provider: "trae-cn", Model: "x"}},
	}
	got = srv.virtualUnavailableReason(srv.Config(), &vmNoAccount)
	if !strings.Contains(got, "登录") {
		t.Errorf("未登录应提示登录，实际 %q", got)
	}

	// 情形 3：账号全暂停。workbuddy 有账号，把它暂停。
	cred, _ := srv.store.Get("workbuddy", "workbuddy-acct")
	cred.Paused = true
	_ = srv.store.Save(cred)
	srv.InvalidateAccounts()
	vmPaused := config.VirtualModel{
		Name:    "c",
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "x"}},
	}
	got = srv.virtualUnavailableReason(srv.Config(), &vmPaused)
	if !strings.Contains(got, "暂停") && !strings.Contains(got, "冷却") {
		t.Errorf("账号全暂停应提示暂停，实际 %q", got)
	}
}

// X-RapidProxy-Account 钉住某个账号时，虚拟组只应使用该账号所属上游的成员，
// 其余上游的成员要跳过——否则「钉住」的语义就失效了。
func TestVirtualGroupRespectsAccountPin(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name: "fast", Enabled: true,
		Members: []config.VirtualMember{
			{Provider: "workbuddy", Model: "a"},
			{Provider: "codebuddy", Model: "b"},
		},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "b", Name: "B"}})

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	// 钉住 codebuddy 的账号，请求应全部落到 codebuddy 的成员。
	for i := 0; i < 6; i++ {
		status, body := postJSON(t, base+"/v1/chat/completions",
			`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"X-RapidProxy-Account": "codebuddy-acct"})
		if status != http.StatusOK {
			t.Fatalf("钉账号后请求应成功，实际 %d: %s", status, body)
		}
	}
	if n := up.hit("a"); n != 0 {
		t.Fatalf("钉住 codebuddy 时不应打到 workbuddy 的成员，实际命中 %d 次", n)
	}
	if n := up.hit("b"); n != 6 {
		t.Fatalf("6 次请求应全部落到 codebuddy 成员，实际 %d", n)
	}
}

// 钉住的账号不属于组内任何成员的上游时，应给出清晰错误，
// 而不是让每个成员都白白打一次上游再返回一堆莫名其妙的错误。
func TestVirtualGroupPinOutsideGroup(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name: "fast", Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "a"}},
	}}, []string{"workbuddy", "codebuddy"}) // codebuddy 也登录了，但不在组里

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "b", Name: "B"}})

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-RapidProxy-Account": "codebuddy-acct"})
	t.Logf("status=%d body=%s", status, body)
	if status == http.StatusOK {
		t.Fatalf("钉住的账号不在组内时不应成功: %s", body)
	}
	if !strings.Contains(string(body), "codebuddy-acct") {
		t.Errorf("错误文案应提到被钉住的账号，实际 %s", body)
	}
	if n := up.hit("a"); n != 0 {
		t.Errorf("不该打到组内成员，实际命中 %d 次", n)
	}
}

// 虚拟组名恰好等于某个真实模型 ID 时，虚拟组优先（已在 resolveRoute 里定序）。
// 但 /v1/models 里不能出现两条同 ID 的条目——客户端会困惑该用哪个。
func TestVirtualNameCollidingWithRealModel(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "gpt-5.4", // 与真实模型同名
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "codebuddy", Model: "glm-5.3"}},
	}}, []string{"workbuddy", "codebuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	var dup []string
	seen := map[string]int{}
	for _, m := range srv.AllModels() {
		seen[m.ID]++
		if seen[m.ID] == 2 {
			dup = append(dup, m.ID)
		}
	}
	t.Logf("models = %v, 重复 = %v", srv.ModelIDs(), dup)

	if len(dup) > 0 {
		t.Fatalf("模型列表不应出现重复 ID: %v", dup)
	}
}

// 只登录单个上游时模型 ID 保持裸名，此时虚拟组同名的条目会与真实模型
// 在 /v1/models 里撞 ID。
func TestVirtualNameCollidesWhenSingleProvider(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "gpt-5.4",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "gpt-5.4"}},
	}}, []string{"workbuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "gpt-5.4", Name: "GPT"}})

	seen := map[string]int{}
	for _, id := range srv.ModelIDs() {
		seen[id]++
	}
	t.Logf("models = %v", srv.ModelIDs())
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("模型 ID %s 出现了 %d 次", id, n)
		}
	}
}

// 撞名时虚拟组只是不在列表里重复出现，仍然必须能被路由到
// （resolveRoute 里虚拟组优先于真实模型匹配）。
func TestCollidingVirtualGroupStillRoutable(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "gpt-5.4",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "member-model"}},
	}}, []string{"workbuddy"})

	srv.setModels("workbuddy", []upstream.ModelInfo{
		{ID: "gpt-5.4", Name: "GPT"},
		{ID: "member-model", Name: "M"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, _, vm, err := srv.resolveRoute("gpt-5.4", req)
	if err != nil {
		t.Fatal(err)
	}
	if vm == nil || vm.Name != "gpt-5.4" {
		t.Fatalf("撞名时虚拟组应优先命中，实际 %v", vm)
	}
}

// resolveModel 是「按单个上游解析」的入口，虚拟组名传进来必须显式报错，
// 不能返回一个 ID 为空的 Profile 让调用方误以为解析成功。
func TestResolveModelRejectsVirtualGroup(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "a"}},
	}}, []string{"workbuddy"})
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	profile, _, err := srv.resolveModel("fast", req)
	if err == nil {
		t.Fatal("虚拟组名应被 resolveModel 拒绝")
	}
	if profile.ID != "" {
		t.Errorf("失败时不应返回上游，实际 %s", profile.ID)
	}
	if !strings.Contains(err.Error(), "fast") {
		t.Errorf("错误应提到组名，实际 %v", err)
	}
}

// /v1/models 的 display_name 是 API 响应字段，客户端可能直接展示。
// 这里不能拼「（虚拟组）」这类中文后缀，否则英文界面的用户会看到中文。
// 本地化标签由前端按 provider=virtual 自行渲染。
func TestVirtualEntryDisplayNameIsPlain(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "a"}},
	}}, []string{"workbuddy"})
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})

	for _, m := range srv.AllModels() {
		if m.ID != "fast" {
			continue
		}
		if m.DisplayName != "fast" {
			t.Errorf("虚拟条目 display_name 应只是组名，实际 %q", m.DisplayName)
		}
		return
	}
	t.Fatal("未找到虚拟组条目")
}

// 虚拟组允许使用「已登录但全局停用」的上游：组是用户的显式选择，
// 优先级高于上游的全局开关。想掐断流量应停用组本身。
func TestVirtualGroupUsesDisabledProvider(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "codebuddy", Model: "glm-5.3"}},
	}}, []string{"codebuddy"})

	// 把 codebuddy 停用（newVirtualServer 里已设为 enabled）。
	cfg := srv.Config().Clone()
	p, _ := cfg.ProfileByID("codebuddy")
	p.Enabled = false
	cfg.SetProfile(p)
	srv.SetConfig(cfg)

	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	if n := srv.VirtualAvailableMembers("fast"); n != 1 {
		t.Fatalf("上游停用但已登录时，成员仍应可用，实际可用 %d", n)
	}

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("上游停用不应妨碍虚拟组，实际 %d: %s", status, body)
	}
	if up.hit("glm-5.3") == 0 {
		t.Error("请求应转发到该上游")
	}
}

// 全部上游都停用时，虚拟组仍应可用（不能被 resolveRoute 的前置检查挡掉）。
func TestVirtualGroupWorksWithNoEnabledProviders(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "workbuddy", Model: "a"}},
	}}, []string{"workbuddy"})

	cfg := srv.Config().Clone()
	for i := range cfg.Profiles {
		cfg.Profiles[i].Enabled = false
	}
	cfg.Normalize()
	srv.SetConfig(cfg)
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	base := "http://" + srv.Addr()

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("没有任何启用上游时虚拟组仍应可用，实际 %d: %s", status, body)
	}
}

// 上游停用时，其成员所属的模型仍应出现在 /v1/models 的虚拟条目里
// （否则用户配好的组会从列表里消失）。
func TestVirtualEntryPresentWithDisabledProvider(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, []config.VirtualModel{{
		Name:    "fast",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: "codebuddy", Model: "glm-5.3"}},
	}}, []string{"codebuddy"})

	cfg := srv.Config().Clone()
	p, _ := cfg.ProfileByID("codebuddy")
	p.Enabled = false
	cfg.SetProfile(p)
	srv.SetConfig(cfg)
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	var found bool
	for _, m := range srv.AllModels() {
		if m.ID == "fast" {
			found = true
		}
	}
	if !found {
		t.Fatalf("上游停用时虚拟组仍应出现在模型列表，实际 %v", srv.ModelIDs())
	}
}

// LoggedInModels 返回原始模型 ID，且不受上游启用状态影响。
func TestLoggedInModelsIgnoresEnabledFlag(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, nil, []string{"codebuddy"})

	cfg := srv.Config().Clone()
	p, _ := cfg.ProfileByID("codebuddy")
	p.Enabled = false
	cfg.SetProfile(p)
	srv.SetConfig(cfg)
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "glm-5.3", Name: "GLM"}})

	got := srv.LoggedInModels("codebuddy")
	if len(got) != 1 || got[0] != "glm-5.3" {
		t.Fatalf("应返回原始模型 ID，实际 %v", got)
	}
	// 未登录的上游返回空。
	if m := srv.LoggedInModels("lobsterai"); len(m) != 0 {
		t.Errorf("未登录上游应返回空，实际 %v", m)
	}
	// 未知上游返回空。
	if m := srv.LoggedInModels("nope"); len(m) != 0 {
		t.Errorf("未知上游应返回空，实际 %v", m)
	}
}

// LoggedInModels 的边界：空串、未知上游、无账号都不应 panic 或返回脏数据。
func TestLoggedInModelsEdgeCases(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, nil, []string{"workbuddy"})
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "a", Name: "A"}})

	for _, id := range []string{"", "  ", "nope", "lobsterai"} {
		if got := srv.LoggedInModels(id); len(got) != 0 {
			t.Errorf("LoggedInModels(%q) 应为空，实际 %v", id, got)
		}
	}
	// 带前后空格的有效 ID 也应解析（内部做了 TrimSpace）。
	if got := srv.LoggedInModels("  workbuddy  "); len(got) != 1 {
		t.Errorf("应容忍前后空格，实际 %v", got)
	}
}

// 配置里一个上游都没有时，应给出清晰错误而不是 panic 或落到空 Profile。
func TestResolveRouteWithNoProfiles(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	srv := newVirtualServer(t, up.URL, nil, nil)

	cfg := srv.Config().Clone()
	cfg.Profiles = nil
	srv.SetConfig(cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, _, _, err := srv.resolveRoute("anything", req)
	if err == nil {
		t.Fatal("没有上游时应报错")
	}
	if !strings.Contains(err.Error(), "上游") {
		t.Errorf("错误应提到上游，实际 %v", err)
	}
}

// 未启用但已登录的上游也必须参与模型同步。
//
// 虚拟组允许使用这类上游（见 usableVirtualMembers），若不同步，它的模型
// 只能来自内置白名单——白名单可能缺少上游新上线的模型，也可能残留已下线
// 的模型。用户在虚拟组编辑器里就会看到一份过时甚至错误的清单。
func TestRefreshModelsCoversDisabledProviders(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	up.syncModels = []map[string]any{
		{"id": "only-from-upstream", "name": "OnlyFromUpstream"},
	}
	srv := newVirtualServer(t, up.URL, nil, []string{"workbuddy", "codebuddy"})

	cfg := srv.Config().Clone()
	on := true
	p, _ := cfg.ProfileByID("codebuddy")
	p.Enabled = false
	p.SyncModels = &on // newVirtualServer 默认关掉同步，这里显式打开
	cfg.SetProfile(p)
	srv.SetConfig(cfg)

	srv.RefreshModels(context.Background())

	// 该上游的 /config 应被请求过（说明同步覆盖到了它）。
	if n := up.configHit("token-codebuddy"); n == 0 {
		t.Errorf("未启用但已登录的上游也应参与模型同步，实际 /config 未被请求")
	}

	// 同步结果里应包含上游真实返回的模型。
	var found bool
	for _, m := range srv.ModelsFor("codebuddy") {
		if m.ID == "only-from-upstream" {
			found = true
		}
	}
	if !found {
		t.Errorf("应包含上游返回的模型，实际 %d 个（含白名单兜底）", len(srv.ModelsFor("codebuddy")))
	}
}

// 同步失败时不应把已缓存的真实模型清单覆盖成白名单：
// 上游临时故障不该让虚拟组的模型凭空消失。
func TestSyncFailureKeepsPreviousModels(t *testing.T) {
	up := newVirtualStub(t, nil, "")
	up.syncModels = []map[string]any{{"id": "real-model", "name": "Real"}}
	srv := newVirtualServer(t, up.URL, nil, []string{"workbuddy"})

	cfg := srv.Config().Clone()
	on := true
	for i := range cfg.Profiles {
		cfg.Profiles[i].SyncModels = &on
	}
	cfg.Normalize()
	srv.SetConfig(cfg)

	// 第一轮：成功，缓存到 real-model。
	srv.RefreshModels(context.Background())
	if !hasModel(srv.ModelsFor("workbuddy"), "real-model") {
		t.Fatal("首轮同步应包含上游返回的模型")
	}

	// 第二轮：上游故障（桩返回 429），同步失败。
	up.mu.Lock()
	up.syncModels = nil
	up.mu.Unlock()
	srv.RefreshModels(context.Background())

	if !hasModel(srv.ModelsFor("workbuddy"), "real-model") {
		t.Error("同步失败后不应丢掉已缓存的模型")
	}
}

func hasModel(list []upstream.ModelInfo, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}
