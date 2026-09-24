package proxysrv

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/store"
)

// newTestServer 启动一个监听随机端口的代理服务，返回服务实例与访问地址。
func newTestServer(t *testing.T, mutate func(*config.Config)) (*Server, string) {
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
	srv := New(cfg, st, applog.New("", 100))
	if err := srv.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, "http://" + srv.Addr()
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
	_, base := newTestServer(t, nil)

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
