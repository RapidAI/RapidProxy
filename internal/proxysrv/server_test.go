package proxysrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/openai"
	"github.com/znsoftm/RapidProxy/internal/store"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// newTestServer 启动一个监听随机端口的代理服务，返回服务实例与访问地址。
//
// 供需要真实 HTTP 端点的测试使用。它会启动后台模型同步协程，
// 因此断言模型同步本身的测试请用 newTestServerIdle。
func newTestServer(t *testing.T, mutate func(*config.Config)) (*Server, string) {
	t.Helper()
	srv := newTestServerIdle(t, mutate)
	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, "http://" + srv.Addr()
}

// newTestServerIdle 构造服务但不启动，因此没有后台同步协程。
//
// 测试模型同步行为时必须用它：一旦走了 Start()，后台那轮同步会与测试
// 手动发起的同步争抢同一道闸门，时序随机器负载变化，断言会随机地空跑。
func newTestServerIdle(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	if mutate != nil {
		mutate(cfg)
	}
	cfg.Normalize()

	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("初始化凭据仓库失败: %v", err)
	}
	return New(cfg, st, applog.New("", 100))
}

// login 往凭据仓库里写一份账号，模拟某个上游完成登录。
func login(t *testing.T, srv *Server, provider, uid string) *upstream.Credential {
	t.Helper()
	cred := &upstream.Credential{
		Provider:  provider,
		ID:        provider + "-" + uid,
		Label:     provider + " · " + uid,
		Auth:      upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account:   upstream.Account{UID: uid, Nickname: "u-" + uid},
		CreatedAt: time.Now(),
	}
	if err := srv.store.Save(cred); err != nil {
		t.Fatalf("写入测试凭据失败: %v", err)
	}
	srv.InvalidateAccounts()
	return cred
}

func get(t *testing.T, url string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func postJSON(t *testing.T, url, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestModelsEndpointFallsBackToAllowlist(t *testing.T) {
	srv, base := newTestServer(t, nil)
	login(t, srv, "workbuddy", "wb1")

	status, body := get(t, base+"/v1/models", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 %s", status, body)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q", list.Object)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
		if m.Provider == "" {
			t.Errorf("模型 %s 缺少 provider 字段", m.ID)
		}
	}
	for _, want := range []string{"gpt-5.4", "hy3", "deepseek-v4.1-flash"} {
		if !ids[want] {
			t.Errorf("内置白名单应包含 %s", want)
		}
	}
	// 未启用的 codebuddy 的独有模型不应出现
	if ids["hunyuan-chat"] {
		t.Error("未启用上游的模型不应出现在列表中")
	}
}

func TestChatWithoutAccountReturnsClearError(t *testing.T) {
	_, base := newTestServer(t, nil)

	status, body := postJSON(t, base+"/v1/chat/completions",
		`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，响应 %s", status, body)
	}
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if !strings.Contains(errResp.Error.Message, "登录") {
		t.Errorf("错误信息未提示登录: %q", errResp.Error.Message)
	}
}

func TestChatRejectsBadRequests(t *testing.T) {
	_, base := newTestServer(t, nil)

	if status, _ := get(t, base+"/v1/chat/completions", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("GET 应返回 405，实际 %d", status)
	}
	if status, _ := postJSON(t, base+"/v1/chat/completions", `not json`, nil); status != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d", status)
	}
	if status, _ := postJSON(t, base+"/v1/chat/completions", `{"messages":[]}`, nil); status != http.StatusBadRequest {
		t.Errorf("缺少 model 应返回 400，实际 %d", status)
	}
}

func TestAPIKeyEnforcement(t *testing.T) {
	_, base := newTestServer(t, func(c *config.Config) {
		c.APIKeys = []string{"sk-unit-test"}
	})

	if status, _ := get(t, base+"/v1/models", nil); status != http.StatusUnauthorized {
		t.Errorf("未带密钥应返回 401，实际 %d", status)
	}
	if status, _ := get(t, base+"/v1/models", map[string]string{"Authorization": "Bearer sk-unit-test"}); status != http.StatusOK {
		t.Errorf("正确 Bearer 密钥应返回 200，实际 %d", status)
	}
	if status, _ := get(t, base+"/v1/models", map[string]string{"x-api-key": "sk-unit-test"}); status != http.StatusOK {
		t.Errorf("x-api-key 形式应返回 200，实际 %d", status)
	}
	if status, _ := get(t, base+"/v1/models", map[string]string{"Authorization": "Bearer wrong"}); status != http.StatusUnauthorized {
		t.Errorf("错误密钥应返回 401，实际 %d", status)
	}
}

func TestResolveModel(t *testing.T) {
	srv, _ := newTestServer(t, func(c *config.Config) {
		if p, ok := c.ProfileByID("codebuddy"); ok {
			p.Enabled = true
			c.SetProfile(p)
		}
	})
	// 两个版本都登录，模型才会出现在列表里（未登录的上游不暴露模型）。
	login(t, srv, "workbuddy", "wb1")
	login(t, srv, "codebuddy", "cb1")

	cases := []struct {
		model    string
		header   string
		wantProv string
		wantID   string
	}{
		{"gpt-5.4", "", "workbuddy", "gpt-5.4"},           // 仅 workbuddy 有
		{"hunyuan-chat", "", "codebuddy", "hunyuan-chat"}, // 仅 codebuddy 有
		{"codebuddy:glm-5.3", "", "codebuddy", "glm-5.3"}, // 前缀强制指定
		{"workbuddy/glm-5.3", "", "workbuddy", "glm-5.3"},
		{"glm-5.3", "codebuddy", "codebuddy", "glm-5.3"}, // 请求头强制指定
		{"未知模型", "", "workbuddy", "未知模型"},                // 未匹配时落到首个启用上游
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, "http://example/", nil)
		if tc.header != "" {
			req.Header.Set("X-RapidProxy-Provider", tc.header)
		}
		profile, id, err := srv.resolveModel(tc.model, req)
		if err != nil {
			t.Errorf("resolveModel(%q) 报错: %v", tc.model, err)
			continue
		}
		if profile.ID != tc.wantProv || id != tc.wantID {
			t.Errorf("resolveModel(%q, header=%q) = (%s, %s)，期望 (%s, %s)",
				tc.model, tc.header, profile.ID, id, tc.wantProv, tc.wantID)
		}
	}
}

func TestHealthAndIndex(t *testing.T) {
	_, base := newTestServer(t, nil)

	if status, body := get(t, base+"/healthz", nil); status != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Errorf("健康检查异常: %d %s", status, body)
	}
	status, body := get(t, base+"/", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "/v1/models") {
		t.Errorf("首页未展示接入信息: %d %s", status, body)
	}
	if status, _ := get(t, base+"/not-found", nil); status != http.StatusNotFound {
		t.Errorf("未知路径应返回 404，实际 %d", status)
	}
}

func TestCORSHeaders(t *testing.T) {
	_, base := newTestServer(t, nil)

	req, _ := http.NewRequest(http.MethodOptions, base+"/v1/chat/completions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("非浏览器请求（无 Origin）应返回 *，实际 %q", got)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("预检请求应返回 204，实际 %d", resp.StatusCode)
	}
}

// 代理默认监听 127.0.0.1，如果无差别允许任意来源，用户浏览的任意网页都能
// 调用本代理并读到响应（等于把已登录账号开放给互联网）。
func TestCORSRestrictsRemoteOriginWithoutAPIKey(t *testing.T) {
	cases := []struct {
		origin string
		want   string
	}{
		{"http://localhost:5173", "http://localhost:5173"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://[::1]:3000", "http://[::1]:3000"},
		{"null", "null"}, // file:// 本地页面
		{"https://evil.example.com", ""},
		{"http://192.168.1.20:3000", ""},
	}
	for _, tc := range cases {
		_, base := newTestServer(t, func(c *config.Config) { c.APIKeys = nil })
		req, _ := http.NewRequest(http.MethodOptions, base+"/v1/chat/completions", nil)
		req.Header.Set("Origin", tc.origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := resp.Header.Get("Access-Control-Allow-Origin")
		resp.Body.Close()
		if got != tc.want {
			t.Errorf("Origin=%q 应返回 %q，实际 %q", tc.origin, tc.want, got)
		}
	}
}

// 配置了 API Key 之后，持有密钥的请求即视为授权，来源不再受限。
func TestCORSAllowsAnyOriginWhenAPIKeyConfigured(t *testing.T) {
	_, base := newTestServer(t, func(c *config.Config) {
		c.APIKeys = []string{"sk-test-123"}
	})
	req, _ := http.NewRequest(http.MethodOptions, base+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("已配置密钥时应回显来源，实际 %q", got)
	}
}

// 客户端常把 base_url 配成 http://host/v1/models 再拼 /chat/completions，
// 服务端要按路径后缀容错，而不是返回 404。
func TestTolerantPathSuffixRouting(t *testing.T) {
	_, base := newTestServer(t, nil)

	// GET <任意前缀>/models 都按模型列表处理
	status, _ := get(t, base+"/v1/models/models", nil)
	if status != http.StatusOK {
		t.Errorf("/v1/models/models 应按 /models 处理，实际 %d", status)
	}
	status, _ = get(t, base+"/whatever/models/", nil)
	if status != http.StatusOK {
		t.Errorf("/whatever/models/ 应按 /models 处理，实际 %d", status)
	}

	// POST <任意前缀>/chat/completions 不应 404（无上游凭据时是别的业务错误）
	status, body := postJSON(t, base+"/v1/models/chat/completions",
		`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status == http.StatusNotFound {
		t.Errorf("/v1/models/chat/completions 不应 404: %s", body)
	}
	status, _ = postJSON(t, base+"/x/y/chat/completions", `{"model":"m"}`, nil)
	if status == http.StatusNotFound {
		t.Error("/x/y/chat/completions 不应 404")
	}

	// 后缀匹配但方法不对 → 405
	status, _ = get(t, base+"/v1/models/chat/completions", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("对 chat 端点发 GET 应 405，实际 %d", status)
	}

	// 完全未知的路径仍 404，且提示 base_url
	status, body = get(t, base+"/no/such/path", nil)
	if status != http.StatusNotFound {
		t.Errorf("未知路径应 404，实际 %d", status)
	}
	if !strings.Contains(string(body), "/v1") {
		t.Errorf("404 提示里应包含 base_url 提示: %s", body)
	}
}

// 多上游启用时：国际版模型统一加 intl: 前缀，国内版保持裸 ID（默认路由）。
func TestAllModelsTagsSharedModelIDs(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	login(t, srv, "workbuddy", "wb1")
	login(t, srv, "codebuddy", "cb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{
		{ID: "shared-model", Name: "Shared"},
		{ID: "only-wb", Name: "OnlyWB"},
	})
	srv.setModels("codebuddy", []upstream.ModelInfo{
		{ID: "shared-model", Name: "Shared"},
		{ID: "only-cb", Name: "OnlyCB"},
	})

	got := map[string]string{} // id -> provider
	for _, m := range srv.AllModels() {
		got[m.ID] = m.Provider
	}

	if got["intl:shared-model"] != "workbuddy" || got["intl:only-wb"] != "workbuddy" {
		t.Fatalf("国际版模型应统一带 intl: 前缀，实际 %v", got)
	}
	if got["shared-model"] != "codebuddy" || got["only-cb"] != "codebuddy" {
		t.Fatalf("国内版模型应保持裸 ID，实际 %v", got)
	}
	if _, ok := got["workbuddy:shared-model"]; ok {
		t.Error("不应再使用 workbuddy: 前缀（国际版统一为 intl:）")
	}
}

// 仅启用单个上游时，所有模型保持裸 ID。
func TestAllModelsPlainWhenSingleProvider(t *testing.T) {
	srv, _ := newTestServer(t, nil) // 默认仅 workbuddy 启用
	login(t, srv, "workbuddy", "wb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{
		{ID: "solo", Name: "Solo"},
	})
	for _, m := range srv.AllModels() {
		if m.ID != "solo" {
			t.Fatalf("单上游时不应加前缀，实际 %s", m.ID)
		}
	}
}

// 前缀形式的模型 ID 应能被路由解析到对应上游（含国际版 intl 别名）。
func TestResolveModelWithProviderPrefix(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	login(t, srv, "workbuddy", "wb1")
	login(t, srv, "codebuddy", "cb1")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	profile, model, err := srv.resolveModel("codebuddy:shared-model", req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "codebuddy" || model != "shared-model" {
		t.Fatalf("应解析为 codebuddy/shared-model，实际 %s/%s", profile.ID, model)
	}

	// 国际版 intl 别名
	profile, model, err = srv.resolveModel("intl:shared-model", req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "workbuddy" || model != "shared-model" {
		t.Fatalf("intl: 应解析为 workbuddy/shared-model，实际 %s/%s", profile.ID, model)
	}

	// X-RapidProxy-Provider 头同样兼容 intl
	req.Header.Set("X-RapidProxy-Provider", "intl")
	profile, model, err = srv.resolveModel("shared-model", req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "workbuddy" || model != "shared-model" {
		t.Fatalf("请求头 intl 应解析为 workbuddy，实际 %s/%s", profile.ID, model)
	}
}

// 只登录其中一个版本时，模型列表里不应出现另一个版本的内置白名单。
func TestModelsOnlyFromLoggedInEditions(t *testing.T) {
	// 两个上游都启用，但只登录国内版。
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	login(t, srv, "codebuddy", "cb1")

	for _, m := range srv.AllModels() {
		if m.Provider != "codebuddy" {
			t.Fatalf("只登录国内版时不应出现 %s（来自 %s）", m.ID, m.Provider)
		}
	}
	ids := map[string]bool{}
	for _, m := range srv.AllModels() {
		ids[m.ID] = true
	}
	if !ids["hunyuan-chat"] {
		t.Error("国内版白名单模型 hunyuan-chat 应出现")
	}
	if ids["gpt-5.4"] || ids["intl:gpt-5.4"] {
		t.Error("未登录国际版时不应列出 gpt-5.4")
	}

	// 反过来：只登录国际版。
	srv2, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	login(t, srv2, "workbuddy", "wb1")
	for _, m := range srv2.AllModels() {
		if m.Provider != "workbuddy" {
			t.Fatalf("只登录国际版时不应出现 %s（来自 %s）", m.ID, m.Provider)
		}
	}
	ids2 := map[string]bool{}
	for _, m := range srv2.AllModels() {
		ids2[m.ID] = true
	}
	if !ids2["gpt-5.4"] {
		t.Error("国际版白名单模型 gpt-5.4 应出现")
	}
	if ids2["hunyuan-chat"] {
		t.Error("未登录国内版时不应列出 hunyuan-chat")
	}
}

// 未登录任何账号时模型列表为空（而不是把内置白名单暴露出去）。
func TestNoModelsWithoutAccount(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	if got := len(srv.AllModels()); got != 0 {
		t.Fatalf("未登录时模型列表应为空，实际 %d 个", got)
	}
}

// 两个版本都登录时，同名模型的裸名归国内版、intl: 归国际版，
// 且裸名不能被顺序更靠前的国际版抢走。
func TestSharedModelIDRoutesByEdition(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	login(t, srv, "workbuddy", "wb1")
	login(t, srv, "codebuddy", "cb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "minimax-m3", Name: "MiniMax-M3"}})
	srv.setModels("codebuddy", []upstream.ModelInfo{{ID: "minimax-m3", Name: "MiniMax-M3"}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	profile, model, err := srv.resolveModel("minimax-m3", req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "codebuddy" || model != "minimax-m3" {
		t.Fatalf("裸名 minimax-m3 应路由到国内版，实际 %s/%s", profile.ID, model)
	}

	profile, model, err = srv.resolveModel("intl:minimax-m3", req)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "workbuddy" || model != "minimax-m3" {
		t.Fatalf("intl:minimax-m3 应路由到国际版，实际 %s/%s", profile.ID, model)
	}
}

// 同一上游多个账号时，选择应按权重分摊；默认（未设权重）为同权。
func TestPickCredentialWeighted(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	a := &upstream.Credential{Provider: "workbuddy", ID: "a"}
	b := &upstream.Credential{Provider: "workbuddy", ID: "b"}
	c := &upstream.Credential{Provider: "workbuddy", ID: "c"}
	c.Weight = 2
	creds := []*upstream.Credential{a, b, c}

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		picked := srv.pickCredential("workbuddy", creds, nil)
		if picked == nil {
			t.Fatal("应能挑到账号")
		}
		counts[picked.ID]++
	}
	// a:b:c 权重 1:1:2，期望 100/100/200 附近。
	if counts["c"] <= counts["a"] {
		t.Fatalf("权重 2 的 c 应多于权重 1 的 a，实际 %v", counts)
	}
	if diff := counts["a"] - counts["b"]; diff > 1 || diff < -1 {
		t.Fatalf("同权的 a/b 应大致均分，实际 %v", counts)
	}
	if counts["a"] < 80 || counts["a"] > 120 {
		t.Fatalf("a 的占比应接近 100/400，实际 %v", counts)
	}
}

// 暂停的账号不参与选择；全部暂停时挑不到账号。
func TestPickCredentialSkipsPaused(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	a := &upstream.Credential{Provider: "workbuddy", ID: "a"}
	b := &upstream.Credential{Provider: "workbuddy", ID: "b", Paused: true}
	creds := []*upstream.Credential{a, b}

	for i := 0; i < 10; i++ {
		if got := srv.pickCredential("workbuddy", creds, nil); got == nil || got.ID != "a" {
			t.Fatalf("暂停的账号不应被选中，实际 %v", got)
		}
	}
	all := []*upstream.Credential{b}
	if got := srv.pickCredential("workbuddy", all, nil); got != nil {
		t.Fatalf("全部暂停时应挑不到账号，实际选中 %s", got.ID)
	}
}

// 请求失败换号重试时，已试过的账号不再被重复挑选。
func TestPickCredentialExcludesTried(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	a := &upstream.Credential{Provider: "workbuddy", ID: "a"}
	b := &upstream.Credential{Provider: "workbuddy", ID: "b"}
	creds := []*upstream.Credential{a, b}

	first := srv.pickCredential("workbuddy", creds, nil)
	second := srv.pickCredential("workbuddy", creds, map[string]bool{first.ID: true})
	if second == nil || second.ID == first.ID {
		t.Fatalf("重试应换到另一个账号，实际 %s -> %v", first.ID, second)
	}
	if third := srv.pickCredential("workbuddy", creds, map[string]bool{first.ID: true, second.ID: true}); third != nil {
		t.Fatalf("账号用完时应返回 nil，实际 %s", third.ID)
	}
}

// newUpstreamStub 起一个假上游：命中限流列表的账号返回指定错误，其余返回 200 SSE。
func newUpstreamStub(t *testing.T, failStatus int, failBody, failToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		if failToken != "" && strings.Contains(r.Header.Get("Authorization"), failToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failStatus)
			_, _ = w.Write([]byte(failBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 上游把某个账号限流时，请求应自动落到同组另一个账号上，而不是把错误抛给下游。
func TestRateLimitFallsOverToNextAccount(t *testing.T) {
	up := newUpstreamStub(t, http.StatusTooManyRequests, `{"code":429,"msg":"rate limit exceeded"}`, "token-a")

	srv, _ := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	a := &upstream.Credential{
		Provider: "workbuddy", ID: "workbuddy-a",
		Auth:    upstream.Tokens{AccessToken: "token-a", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account: upstream.Account{UID: "a", Nickname: "A"},
	}
	b := &upstream.Credential{
		Provider: "workbuddy", ID: "workbuddy-b",
		Auth:    upstream.Tokens{AccessToken: "token-b", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account: upstream.Account{UID: "b", Nickname: "B"},
	}
	if err := srv.store.Save(a); err != nil {
		t.Fatalf("写入凭据失败: %v", err)
	}
	if err := srv.store.Save(b); err != nil {
		t.Fatalf("写入凭据失败: %v", err)
	}
	srv.InvalidateAccounts()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp, cred, err := srv.doChat(context.Background(), "workbuddy", []byte(`{"model":"x"}`), req)
	if err != nil {
		t.Fatalf("组内还有可用账号时不应报错，实际: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应换到健康账号并返回 200，实际 %d", resp.StatusCode)
	}
	if cred.ID != "workbuddy-b" {
		t.Fatalf("应由账号 b 承接，实际 %s", cred.ID)
	}
	// 被限流的账号进入冷却，短时间内不再被选中。
	if left := srv.cooldownLeft("workbuddy-a"); left <= 0 {
		t.Fatal("被限流的账号应进入冷却")
	}
}

// 所有账号都被限流时，返回一个带重试时间的错误（而不是 502）。
func TestAllAccountsRateLimitedReturnsRetryableError(t *testing.T) {
	up := newUpstreamStub(t, http.StatusTooManyRequests, `{"code":429,"msg":"too many requests"}`, "token-")

	srv, _ := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	for _, uid := range []string{"a", "b"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + uid,
			Auth:    upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: uid},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	srv.InvalidateAccounts()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, _, err := srv.doChat(context.Background(), "workbuddy", []byte(`{"model":"x"}`), req)
	if err == nil {
		t.Fatal("全部限流时应返回错误")
	}
	var fail *upstreamFailError
	if !errors.As(err, &fail) {
		t.Fatalf("应返回 upstreamFailError，实际 %T: %v", err, err)
	}
	if fail.retryAfter <= 0 {
		t.Errorf("应给出建议的重试等待时间，实际 %v", fail.retryAfter)
	}
}

// 额度耗尽比普通限流冷却得更久。
func TestCreditExhaustedCoolsLongerThanRateLimit(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	rateUntil := srv.markCooldown("c1", failRateLimit, 0)
	creditUntil := srv.markCooldown("c2", failCredit, 0)

	rateLeft := time.Until(rateUntil)
	creditLeft := time.Until(creditUntil)
	if creditLeft <= rateLeft {
		t.Fatalf("额度耗尽的冷却应明显长于限流，实际 rate=%s credit=%s", rateLeft, creditLeft)
	}
	if creditLeft < 30*time.Minute {
		t.Fatalf("额度耗尽应按小时级冷却，实际 %s", creditLeft)
	}
}

// 上游没用标准状态码时，也要能凭正文关键词认出额度耗尽。
func TestInspectUpstreamFailureDetectsCreditByBody(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   failKind
	}{
		{http.StatusPaymentRequired, "payment required", failCredit},
		{http.StatusTooManyRequests, "slow down", failRateLimit},
		{http.StatusForbidden, `{"msg":"insufficient_quota: 额度已用尽"}`, failCredit},
		{http.StatusBadRequest, "您的额度不足", failCredit},
		{http.StatusForbidden, "too many requests", failRateLimit},
		{http.StatusServiceUnavailable, "upstream down", failServer},
		{http.StatusBadGateway, "bad gateway", failServer},
		{http.StatusBadRequest, "服务繁忙，请稍后重试", failServer},
		{http.StatusBadRequest, "bad parameter", failNone},
	}
	for _, tc := range cases {
		resp := &http.Response{
			StatusCode: tc.status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(tc.body)),
		}
		got, _, _ := inspectUpstreamFailure(resp)
		if got != tc.want {
			t.Errorf("status=%d body=%q => %v，期望 %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// 成功的请求要清掉连续失败计数，让冷却时长回到起点。
func TestClearCooldownResetsBackoff(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	srv.markCooldown("c1", failRateLimit, 0)
	srv.clearCooldown("c1")
	if left := srv.cooldownLeft("c1"); left != 0 {
		t.Fatalf("成功后不应仍在冷却，实际剩余 %s", left)
	}
}

// 上游 502/503/504 时也应换到同组下一个账号，而不是把 502 直接抛给下游。
func TestServerErrorFallsOverToNextAccount(t *testing.T) {
	up := newUpstreamStub(t, http.StatusServiceUnavailable, `{"msg":"upstream unavailable"}`, "token-a")

	srv, _ := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	for _, uid := range []string{"a", "b"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + uid,
			Auth:    upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: uid},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	srv.InvalidateAccounts()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp, cred, err := srv.doChat(context.Background(), "workbuddy", []byte(`{"model":"x"}`), req)
	if err != nil {
		t.Fatalf("组内还有可用账号时不应报错，实际: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应换到健康账号并返回 200，实际 %d", resp.StatusCode)
	}
	if cred.ID != "workbuddy-b" {
		t.Fatalf("应由账号 b 承接，实际 %s", cred.ID)
	}
}

// 上游抖动只该短暂冷却，不能按额度耗尽那套小时级退避把账号锁死。
func TestServerErrorCoolsBriefly(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	until := srv.markCooldown("c1", failServer, 0)
	left := time.Until(until)
	if left <= 0 || left > time.Minute {
		t.Fatalf("上游抖动应冷却数秒级，实际 %s", left)
	}
	// 连续抖动会退避，但封顶仍然很短。
	for i := 0; i < 10; i++ {
		until = srv.markCooldown("c1", failServer, 0)
	}
	if got := time.Until(until); got > time.Minute {
		t.Fatalf("上游抖动冷却应封顶在 1 分钟内，实际 %s", got)
	}
	// 换一种失败原因时，不该沿用上一种的连续次数：
	// 上面已经连续抖动 11 次，若共用计数器，这里会被推到限流的上限。
	until = srv.markCooldown("c1", failRateLimit, 0)
	if got := time.Until(until); got > rateLimitCooldownBase || got < rateLimitCooldownBase-2*time.Second {
		t.Fatalf("限流应从基础时长重新开始，实际 %s", got)
	}
}

// 凭据清单有缓存，但每次都要返回副本：
// 一个请求刷新令牌不能改写另一个请求手上的对象。
func TestLoadCredsReturnsCopies(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	login(t, srv, "workbuddy", "wb1")
	srv.InvalidateAccounts()

	first, err := srv.loadCreds("workbuddy")
	if err != nil || len(first) != 1 {
		t.Fatalf("应读到 1 个账号，实际 %v (err=%v)", first, err)
	}
	first[0].Auth.AccessToken = "已改写"
	first[0].Paused = true

	second, err := srv.loadCreds("workbuddy")
	if err != nil || len(second) != 1 {
		t.Fatalf("第二次读取失败: %v (err=%v)", second, err)
	}
	if second[0].Auth.AccessToken == "已改写" || second[0].Paused {
		t.Fatalf("缓存里的凭据被上一次调用改写了: %+v", second[0])
	}
}

// 缓存不能让「刚登录」和「刚删除」延迟生效：
// 否则紧接着的模型同步会把新账号当成未登录而清空这一版的模型列表。
func TestCredentialCacheInvalidation(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	if srv.hasCredential("workbuddy") {
		t.Fatal("一开始不该有账号")
	}
	// 上面这一次查询把「没有账号」缓存了下来。
	login(t, srv, "workbuddy", "wb1")
	if !srv.hasCredential("workbuddy") {
		t.Fatal("登录后（缓存已失效）应能看到账号")
	}

	// 再验证删除：不加失效的话，已删的账号还会在缓存里多活两秒。
	if err := srv.store.Delete("workbuddy", "workbuddy-wb1"); err != nil {
		t.Fatal(err)
	}
	if !srv.hasCredential("workbuddy") {
		t.Fatal("删除后缓存仍会短暂命中，这里只验证删除本身生效")
	}
	srv.InvalidateAccounts()
	if srv.hasCredential("workbuddy") {
		t.Fatal("失效后不应再看到已删除的账号")
	}
}

// UpdateCredential 只改指定字段，不碰令牌等其余内容。
//
// 界面上的暂停 / 改权重和 doChat 的令牌刷新会并发写同一份凭据，
// 两者必须共用同一把账号锁，并且只能改自己要改的字段。
func TestUpdateCredentialPreservesOtherFields(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	cred := &upstream.Credential{
		Provider: "workbuddy", ID: "wb1",
		Auth:    upstream.Tokens{AccessToken: "tok-original", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account: upstream.Account{UID: "u1", Nickname: "nick"},
	}
	if err := srv.store.Save(cred); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAccounts()

	if err := srv.UpdateCredential("workbuddy", "wb1", func(c *upstream.Credential) error {
		c.Paused = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := srv.store.Get("workbuddy", "wb1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused {
		t.Error("暂停状态应已写入")
	}
	if got.Auth.AccessToken != "tok-original" || got.Account.Nickname != "nick" {
		t.Errorf("其余字段被改动了: %+v", got)
	}

	// 更新后缓存里也应是新值，否则下一次请求还会拿到旧的暂停状态。
	if err := srv.UpdateCredential("workbuddy", "wb1", func(c *upstream.Credential) error {
		c.Weight = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cached, err := srv.loadCreds("workbuddy")
	if err != nil || len(cached) != 1 {
		t.Fatalf("读取缓存失败: %v (err=%v)", cached, err)
	}
	if cached[0].Weight != 7 || !cached[0].Paused {
		t.Errorf("缓存未同步最新值: %+v", cached[0])
	}

	if err := srv.UpdateCredential("workbuddy", "missing", func(c *upstream.Credential) error { return nil }); err == nil {
		t.Error("账号不存在时应返回错误")
	}
}

// 汇总结果被缓存：模型缓存变化后必须重算，否则列表会停在旧版本上。
func TestModelEntriesCacheInvalidatesOnModelChange(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	login(t, srv, "workbuddy", "wb1")

	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "alpha"}})
	if ids := modelIDs(srv.AllModels()); len(ids) != 1 || ids[0] != "alpha" {
		t.Fatalf("首次汇总结果不对: %v", ids)
	}
	// 模型缓存换了，汇总必须跟着变（这里最容易踩的坑是复用了旧快照）。
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "alpha"}, {ID: "beta"}})
	ids := modelIDs(srv.AllModels())
	if len(ids) != 2 {
		t.Fatalf("模型更新后汇总应重算，实际 %v", ids)
	}
}

func modelIDs(models []openai.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// 截断按字符而不是字节：按字节切会把汉字切出半个 UTF-8 序列。
func TestTruncateKeepsValidUTF8(t *testing.T) {
	s := "上游额度已用尽，请充值后再试"
	got := truncate(s, 6)
	if got != "上游额度已用" {
		t.Fatalf("截断结果不对: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断产生了非法 UTF-8: %q", got)
	}
	if truncate("abc", 10) != "abc" {
		t.Error("短于上限时不应截断")
	}
}

// 上游用 text/event-stream 的 Content-Type 回 429 时，仍应被认成限流并换号。
//
// 这类响应以前会被 isSSE 挡在 inspectUpstreamFailure 之外：不换号，
// 而且 pipeSSE 会无条件写 200，客户端收到的是一个「成功但没有内容」的空流。
func TestSSETypedRateLimitStillFallsOver(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		if strings.Contains(r.Header.Get("Authorization"), "token-a") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"rate limit exceeded\"}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, _ := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	for _, uid := range []string{"a", "b"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + uid,
			Auth:    upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: uid},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	srv.InvalidateAccounts()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp, cred, err := srv.doChat(context.Background(), "workbuddy", []byte(`{"model":"x"}`), req)
	if err != nil {
		t.Fatalf("SSE 类型的限流也应触发换号，实际: %v", err)
	}
	defer resp.Body.Close()
	if cred.ID != "workbuddy-b" {
		t.Fatalf("应由账号 b 承接，实际 %s", cred.ID)
	}
	if left := srv.cooldownLeft("workbuddy-a"); left <= 0 {
		t.Fatal("被限流的账号应进入冷却")
	}
}

// SSE 格式的错误正文要还原成一句正常文案，而不是带 "data:" 前缀的半截 JSON。
func TestFirstSSEErrorMessage(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`data: {"error":{"message":"quota exhausted"}}`, "quota exhausted"},
		{`data: {"code":11217,"msg":"登录未完成"}`, "上游返回 code=11217 msg=登录未完成"},
		{`data: {"msg":"上游繁忙"}`, "上游繁忙"},
		// 首个事件没有可识别字段时，原样返回该事件而不是整段正文。
		{"data: {\"weird\":1}\n\ndata: {\"error\":{\"message\":\"x\"}}", `{"weird":1}`},
		// 完全不是 SSE 时原样返回。
		{"plain text error", "plain text error"},
	}
	for _, tc := range cases {
		if got := firstSSEErrorMessage(tc.body); got != tc.want {
			t.Errorf("body=%q => %q，期望 %q", tc.body, got, tc.want)
		}
	}
}

// 上游带着 SSE 的 Content-Type 回 4xx 时，下游必须拿到错误状态码而不是 200。
func TestSSETypedErrorNotReportedAsOK(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"bad parameter\"}}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	cred := &upstream.Credential{
		Provider: "workbuddy", ID: "workbuddy-a",
		Auth:    upstream.Tokens{AccessToken: "token-a", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account: upstream.Account{UID: "a"},
	}
	if err := srv.store.Save(cred); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAccounts()
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if status != http.StatusBadRequest {
		t.Fatalf("上游 400 应回传给下游，实际 %d，响应 %s", status, raw)
	}
	if !strings.Contains(string(raw), "bad parameter") {
		t.Errorf("响应里应包含上游的错误文案，实际 %s", raw)
	}
	if strings.Contains(string(raw), "data:") {
		t.Errorf("不应把 SSE 帧原样透传给客户端，实际 %s", raw)
	}
}

// 同步周期在运行中被改掉后要能生效；设为 0（关闭）后再改回也要恢复。
//
// 旧实现只在协程启动时读一次配置，改设置必须重启程序；
// 而且周期为 0 时直接 return，等于永久关掉了定时同步。
func TestModelSyncLoopPicksUpIntervalChanges(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	cfg := srv.Config()
	cfg.ModelSyncHours = 0 // 关闭
	srv.SetConfig(cfg)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		srv.modelSyncLoop(stop)
		close(done)
	}()

	// 关闭同步时协程必须还活着：否则改回非 0 就再也不同步了。
	select {
	case <-done:
		t.Fatal("周期为 0 时同步协程不应退出")
	case <-time.After(200 * time.Millisecond):
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("停止信号后同步协程应退出")
	}
}

// 停止服务时不能被正在进行的同步阻塞（单轮超时是 2 分钟，退出不能等它）。
//
// 直接测 refreshModelsWithTimeout：走 modelSyncLoop 会撞上 syncMu
// （newTestServer 内部 Start() 已经跑过一轮同步），测的就不是取消语义了。
func TestStopCancelsInFlightSync(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			once.Do(func() { close(entered) })
			// 卡住，直到请求被取消。
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv := newTestServerIdle(t, func(cfg *config.Config) {
		on := true
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &on
		}
	})
	login(t, srv, "workbuddy", "wb1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.refreshModelsWithTimeout(ctx)
		close(done)
	}()

	// 等请求真的发出去了再取消，否则测的是「还没开始就已经取消」。
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("同步请求没有发出，测试无效")
	}

	start := time.Now()
	cancel()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("取消后仍被同步阻塞了 %s", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消没有中断正在进行的同步")
	}
}

// 多个触发点（手动同步、保存设置、登录后、周期任务）可能同时发起同步，
// 同一时刻只应跑一轮：重复同步会白耗上游配额，还会互相争抢账号锁。
func TestRefreshModelsSkipsWhenAlreadyRunning(t *testing.T) {
	var inflight, maxInflight int32
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			cur := atomic.AddInt32(&inflight, 1)
			for {
				old := atomic.LoadInt32(&maxInflight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInflight, old, cur) {
					break
				}
			}
			atomic.AddInt32(&calls, 1)
			// 拖慢一点，让并发触发确实撞在一起。
			time.Sleep(150 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv := newTestServerIdle(t, func(cfg *config.Config) {
		// 必须打开模型同步：关掉的话 /v3/config 根本不会被调用，
		// 下面的并发断言就测不出任何东西。
		on := true
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &on
		}
	})
	login(t, srv, "workbuddy", "wb1")

	const goroutines = 5
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			srv.RefreshModels(context.Background())
		}()
	}
	wg.Wait()

	if total := atomic.LoadInt32(&calls); total == 0 {
		t.Fatal("测试本身没生效：/v3/config 一次都没被调用")
	}
	if got := atomic.LoadInt32(&maxInflight); got > 1 {
		t.Fatalf("同一时刻最多一轮同步，实际并发 %d", got)
	}
	// 被跳过的那些不会重跑，所以总调用数应少于并发数。
	if total := atomic.LoadInt32(&calls); total >= goroutines {
		t.Fatalf("重复同步未被拦截，/v3/config 被调用 %d 次（并发 %d）", total, goroutines)
	}
}

// 上游以 200 + SSE 的形式回报失败（流里夹 error 事件）时，
// 非流式请求必须拿到错误状态码，而不是 200 + 空回复。
func TestStreamErrorEventBecomesHTTPError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"开头\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"额度已用尽\"}}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	login(t, srv, "workbuddy", "wb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	// 不传 stream，客户端要非流式，走 Aggregate 分支。
	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)

	if status != http.StatusBadGateway {
		t.Fatalf("流中 error 事件应回 502，实际 %d，响应 %s", status, raw)
	}
	if !strings.Contains(string(raw), "额度已用尽") {
		t.Errorf("响应应包含上游错误文案，实际 %s", raw)
	}
}

// 上游用 message（整段）而非 delta 下发内容时，非流式聚合不能丢内容。
func TestMessageShapeContentSurvivesAggregation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"整段回复\"},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	login(t, srv, "workbuddy", "wb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 %s", status, raw)
	}
	if !strings.Contains(string(raw), "整段回复") {
		t.Errorf("message 形态的内容不应丢失，实际 %s", raw)
	}
}

// 上游返回成功（< 400）的合法 JSON、但不是 OpenAI 的 choices 形态时，
// 代理必须原样透传，绝不能改写成 502 并把原始 JSON 塞进 error.message。
//
// 这是先前的一处 bug：writeNonStreamUpstream 只透传「带 choices 的 JSON」，
// 因此上游的 200 + 其它合法 JSON 会被误判为错误。代理应当保持透明。
func TestNonStreamSuccessPassesThroughEvenWithoutChoices(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok","detail":{"a":1}}`))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	login(t, srv, "workbuddy", "wb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	// 不传 stream，走非流式分支。
	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)

	if status != http.StatusOK {
		t.Fatalf("成功响应应原样透传为 200，实际 %d，响应 %s", status, raw)
	}
	if string(raw) != `{"result":"ok","detail":{"a":1}}` {
		t.Errorf("上游响应体应原样透传，实际 %s", raw)
	}
}

// 上游的额度耗尽 / 限流是以 200 + SSE 回报的，状态码完全正常，
// 只有折叠流的时候才看得见。这条路径以前完全没有换号能力：
// 组里还有健康账号，请求却照样失败——正是「只要同组有一个账号可用
// 就不要把错误返回给下游」这条要求没被满足的地方。
func TestAggregateStreamErrorFallsOverToNextAccount(t *testing.T) {
	var aCalls, bCalls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(r.Header.Get("Authorization"), "token-a") {
			atomic.AddInt32(&aCalls, 1)
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"额度已用尽\"}}\n\n"))
			return
		}
		atomic.AddInt32(&bCalls, 1)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"index\":0}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	for _, uid := range []string{"a", "b"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + uid,
			Auth:    upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: uid},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	srv.InvalidateAccounts()
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	// 不传 stream，走非流式聚合分支。
	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)

	if status != http.StatusOK {
		t.Fatalf("组内还有可用账号时应换号成功，实际 %d，响应 %s", status, raw)
	}
	if !strings.Contains(string(raw), "你好") {
		t.Errorf("应返回健康账号的内容，实际 %s", raw)
	}
	if atomic.LoadInt32(&bCalls) == 0 {
		t.Error("应实际用到账号 b")
	}
	// 报额度用尽的账号要进冷却，否则下一个请求还会先打到它。
	if left := srv.cooldownLeft("workbuddy-a"); left <= 0 {
		t.Error("流内报额度耗尽的账号应进入冷却")
	}
}

// 换号也用尽时，必须报上游的真实错误，而不是「尚未登录账号」。
//
// 用户看到「尚未登录」会去反复检查登录状态，而实际原因是额度用尽，
// 这种误导比报 502 更糟。
func TestAggregateStreamErrorKeepsUpstreamMessageWhenNoAccountLeft(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"insufficient_quota 额度不足\"}}\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	login(t, srv, "workbuddy", "wb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)

	if status != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，响应 %s", status, raw)
	}
	if !strings.Contains(string(raw), "额度不足") {
		t.Errorf("应保留上游的错误文案，实际 %s", raw)
	}
	if strings.Contains(string(raw), "尚未登录账号") {
		t.Errorf("不应把额度问题误报成未登录: %s", raw)
	}
}

// balState 必须回收已删除的账号：否则账号删掉再登录同一 ID，
// 会带着历史偏移重新加入，短时间内连续中标，分配明显倾斜。
func TestPickCredentialDropsStaleBalanceState(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	a := &upstream.Credential{Provider: "workbuddy", ID: "a"}
	b := &upstream.Credential{Provider: "workbuddy", ID: "b"}
	ghost := &upstream.Credential{Provider: "workbuddy", ID: "ghost"}

	// 先让 ghost 参与若干轮，把它的 SWRR 偏移攒起来。
	for i := 0; i < 10; i++ {
		srv.pickCredential("workbuddy", []*upstream.Credential{a, b, ghost}, nil)
	}
	if _, ok := srv.balState["workbuddy/ghost"]; !ok {
		t.Fatal("前置条件不成立：ghost 应有 balState 条目")
	}

	// ghost 被删除后（列表里不再有它），条目应被清掉。
	for i := 0; i < 5; i++ {
		srv.pickCredential("workbuddy", []*upstream.Credential{a, b}, nil)
		srv.pruneBalanceState("workbuddy", []*upstream.Credential{a, b})
	}
	if _, ok := srv.balState["workbuddy/ghost"]; ok {
		t.Error("已删除账号的 balState 条目应被回收")
	}
}

// 用 X-RapidProxy-Account 把流量钉到单个账号上，不能毁掉同组其它账号的
// 分摊状态。
//
// 回收存活集合如果取自 pinned 过滤之后的候选集，一次调试请求就会把整组
// （这里 4 个账号）的 SWRR 偏移全部清零：紧接着的正常请求会连续打到
// 同一个账号上，加载均衡形同失效，而且很难被联想到与调试请求有关。
func TestPinnedRequestDoesNotResetGroupBalance(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"index\":0}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})

	var all []*upstream.Credential
	for _, id := range []string{"a", "b", "c", "d"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + id,
			Auth:    upstream.Tokens{AccessToken: "token-" + id, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: id},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
		all = append(all, cred)
	}
	srv.InvalidateAccounts()
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	// 先跑 20 次正常请求，把四个账号的偏移都推高。
	for i := 0; i < 20; i++ {
		srv.pickCredential("workbuddy", all, nil)
	}
	before := len(srv.balState)
	if before < 4 {
		t.Fatalf("前置条件不成立：balState 应有 4 个账号的条目，实际 %d", before)
	}

	// 发一次钉住 a 的请求（走完整 doChatExcluding）。
	status, _ := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
			"X-RapidProxy-Account": "workbuddy-a",
		})
	if status != http.StatusOK {
		t.Fatalf("钉住账号的请求应成功，实际 %d", status)
	}

	// 关键断言：b/c/d 的偏移不能被清掉。
	for _, id := range []string{"workbuddy-b", "workbuddy-c", "workbuddy-d"} {
		if _, ok := srv.balState["workbuddy/"+id]; !ok {
			t.Errorf("钉住 a 的请求不应清掉 %s 的分摊状态（现有条目 %v）", id, srv.balState)
		}
	}
}

// 冷却表必须无条件回收过期条目：账号数少于 33 时，
// 挂在 len>32 之后的清理代码永远不执行，用户删掉的账号其冷却状态
// 会一直留着，重新登录同一账号会凭空带着旧冷却。
func TestCooldownEntriesAreReclaimedWhenFewAccounts(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	srv.cool["old-account"] = &cooldownState{Until: time.Now().Add(-time.Hour), RateHits: 1}

	// 只有两个账号，远低于旧的 32 阈值。
	srv.markCooldown("c1", failRateLimit, 0)
	srv.markCooldown("c2", failCredit, 0)

	if _, ok := srv.cool["old-account"]; ok {
		t.Error("过期条目应被回收")
	}
}

// 换号后拿到的是客户端错误（400/404/422）时，必须把错误回给下游，
// 绝不能写出「200 + 空 body」。
//
// 聚合失败后换号，新账号可能回的是「该模型没有开通」这类 4xx。
// doChatExcluding 对 4xx 不做退避，直接把响应交回来——此时如果沿用
// 上一轮的 err（它已被 doChatExcluding 覆盖成 nil），handleChat 只判
// err != nil，于是写出 200 和一个空 body：客户端以为请求成功，
// 拿到空响应后只能报一句无头无尾的 JSON 解析错误，看不出是模型没权限。
func TestAggregateFailoverWithClientErrorIsNotEmptySuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		if strings.Contains(r.Header.Get("Authorization"), "token-a") {
			// 额度耗尽以 200 + 流内 error 回报。
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"额度已用尽\"}}\n\n"))
			return
		}
		// 账号 b 没有这个模型的权限：普通的客户端错误，不含任何冷却关键词。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"该模型未在当前账号开通"}}`))
	}))
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	for _, uid := range []string{"a", "b"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + uid,
			Auth:    upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: uid},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	srv.InvalidateAccounts()
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)

	if status == http.StatusOK {
		t.Fatalf("换号后没有可用回复时不能返回 200，实际 200，响应 %s", raw)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		t.Fatalf("错误响应不能是空 body（状态码 %d）", status)
	}
	if !strings.Contains(string(raw), "该模型未在当前账号开通") {
		t.Errorf("应把上游的真实错误带回去，实际 %s", raw)
	}
}

// 非流式换号后，最终成功的那次响应体必须被关闭，否则连接永远不归还连接池。
//
// 调用方的 defer 只捕获了最初那个 resp；换号拿到的响应如果没人关，
// 每次「额度耗尽 -> 换号 -> 成功」都会漏一个连接，请求量一大就把
// 上游的连接数打满。
//
// 判据用上游侧的连接状态：body 被读完并 Close 之后，连接会从 StateActive
// 转成 StateIdle；没关就永远停在 Active。
func TestAggregateFailoverClosesReplacedResponseBody(t *testing.T) {
	var mu sync.Mutex
	busy := 0
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(r.Header.Get("Authorization"), "token-a") {
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"额度已用尽\"}}\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"index\":0}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateActive:
			busy++
		case http.StateIdle, http.StateClosed:
			if busy > 0 {
				busy--
			}
		}
	}
	up.Start()
	t.Cleanup(up.Close)

	srv, base := newTestServer(t, func(cfg *config.Config) {
		off := false
		for i := range cfg.Profiles {
			cfg.Profiles[i].BaseURL = up.URL
			cfg.Profiles[i].Origin = up.URL
			cfg.Profiles[i].SyncModels = &off
		}
	})
	for _, uid := range []string{"a", "b"} {
		cred := &upstream.Credential{
			Provider: "workbuddy", ID: "workbuddy-" + uid,
			Auth:    upstream.Tokens{AccessToken: "token-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: upstream.Account{UID: uid},
		}
		if err := srv.store.Save(cred); err != nil {
			t.Fatal(err)
		}
	}
	srv.InvalidateAccounts()
	srv.setModels("workbuddy", []upstream.ModelInfo{{ID: "x"}})

	status, raw := postJSON(t, base+"/v1/chat/completions",
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("换号后应成功，实际 %d，响应 %s", status, raw)
	}

	// 状态更新是异步的，给它一点时间落定。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := busy
		mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	n := busy
	mu.Unlock()
	t.Errorf("换号后的响应体没有被关闭，上游仍有 %d 个连接停在 Active（未归还连接池）", n)
}

// 多上游（含新协议族）同时登录时的同名模型消歧：
// 裸名归第一个（配置顺序）暴露它的非国际版上游，其余上游用 "上游ID:" 前缀。
func TestAllModelsDisambiguatesThirdProviderConflict(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = true
		}
	})
	login(t, srv, "workbuddy", "wb1")
	login(t, srv, "codebuddy", "cb1")
	login(t, srv, "lobsterai", "lb1")
	srv.setModels("workbuddy", []upstream.ModelInfo{
		{ID: "glm-5.3", Name: "GLM-5.3"},
	})
	srv.setModels("codebuddy", []upstream.ModelInfo{
		{ID: "glm-5.3", Name: "GLM-5.3"},
		{ID: "hy3", Name: "Hy3"},
	})
	srv.setModels("lobsterai", []upstream.ModelInfo{
		{ID: "glm-5.3", Name: "GLM-5.3"},
		{ID: "deepseek-flash", Name: "DeepSeek"},
	})

	got := map[string]string{} // id -> provider
	for _, m := range srv.AllModels() {
		got[m.ID] = m.Provider
	}

	// 国际版约定不变：全部模型 intl: 前缀。
	if got["intl:glm-5.3"] != "workbuddy" {
		t.Fatalf("workbuddy 模型应保持 intl: 前缀，实际 %v", got)
	}
	// 配置顺序里 codebuddy 先于 lobsterai：裸名归 codebuddy。
	if got["glm-5.3"] != "codebuddy" || got["hy3"] != "codebuddy" {
		t.Fatalf("裸名应归第一个暴露它的国内上游，实际 %v", got)
	}
	// lobsterai 的同名模型改为上游 ID 前缀；非冲突模型保持裸名。
	if got["lobsterai:glm-5.3"] != "lobsterai" {
		t.Fatalf("lobsterai 的冲突模型应加上游 ID 前缀，实际 %v", got)
	}
	if got["deepseek-flash"] != "lobsterai" {
		t.Fatalf("lobsterai 独有模型应保持裸名，实际 %v", got)
	}
	// 前缀形式要能被 resolveModel 解析回对应上游。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	profile, model, err := srv.resolveModel("lobsterai:glm-5.3", req)
	if err != nil || profile.ID != "lobsterai" || model != "glm-5.3" {
		t.Fatalf("lobsterai:glm-5.3 应解析到 lobsterai，实际 %s/%s err=%v", profile.ID, model, err)
	}
}

// 仅登录新协议族上游时，模型保持裸 ID（不因 intl 前缀机制受影响）。
func TestAllModelsPlainForSingleNewProvider(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *config.Config) {
		for i := range cfg.Profiles {
			cfg.Profiles[i].Enabled = false
		}
		cfg.SetProfile(config.TraeCNPreset())
		cfg.Normalize()
	})
	login(t, srv, "trae-cn", "tr1")
	srv.setModels("trae-cn", []upstream.ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2"},
	})
	for _, m := range srv.AllModels() {
		if m.ID != "glm-5.2" {
			t.Fatalf("单上游（trae-cn）应保持裸 ID，实际 %s", m.ID)
		}
	}
}
