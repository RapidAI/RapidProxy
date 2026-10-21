package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/znsoftm/RapidProxy/internal/config"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(config.WorkBuddyPreset(), config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	return c
}

// 上游 /v3/config 常常漏掉一部分实际可调用的模型，这时必须以内置白名单里的
// 名称与上下文/输出上限为准，不能退化成 0 与默认值。
func TestBuildModelListKeepsBuiltinSpecs(t *testing.T) {
	c := newTestClient(t)

	models := c.BuildModelList(nil, nil, nil)
	if len(models) == 0 {
		t.Fatal("内置白名单不应为空")
	}

	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	specs := Allowlist("workbuddy")
	if len(models) != len(specs) {
		t.Errorf("模型数量应为白名单长度 %d，实际 %d", len(specs), len(models))
	}
	for _, spec := range specs {
		got, ok := byID[spec.ID]
		if !ok {
			t.Fatalf("白名单模型 %s 缺失", spec.ID)
		}
		if got.Name != spec.Name {
			t.Errorf("%s 名称应为 %q，实际 %q", spec.ID, spec.Name, got.Name)
		}
		if got.ContextLength != spec.ContextLength {
			t.Errorf("%s 上下文应为 %d，实际 %d", spec.ID, spec.ContextLength, got.ContextLength)
		}
		if got.MaxOutputTokens != spec.MaxOutput {
			t.Errorf("%s 输出上限应为 %d，实际 %d", spec.ID, spec.MaxOutput, got.MaxOutputTokens)
		}
	}
}

// 上游返回的信息只用于补充/覆盖，不能把白名单里已有的值抹掉。
func TestBuildModelListMergesUpstreamInfo(t *testing.T) {
	c := newTestClient(t)

	upstream := []UpstreamModel{
		{ID: "gpt-5.4", Name: "GPT-5.4 上游名", MaxInputTokens: 400000, MaxOutputTok: 64000, SupportsImages: true},
		{ID: "brand-new-model", Name: "全新模型", MaxInputTokens: 128000, MaxOutputTok: 16000},
	}
	models := c.BuildModelList(nil, nil, upstream)
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	if got := byID["gpt-5.4"]; got.Name != "GPT-5.4 上游名" || got.ContextLength != 400000 ||
		got.MaxOutputTokens != 64000 || !got.SupportsImages {
		t.Errorf("上游信息未生效: %+v", got)
	}
	if got, ok := byID["brand-new-model"]; !ok {
		t.Error("上游新增模型未出现在列表里")
	} else if got.MaxOutputTokens != 16000 {
		t.Errorf("新增模型输出上限应为 16000，实际 %d", got.MaxOutputTokens)
	}
}

func TestBuildModelListHonoursDisabledAndExtra(t *testing.T) {
	c := newTestClient(t)

	models := c.BuildModelList([]string{"custom-model", "gpt-5.4"}, []string{"gpt-5.5"}, nil)
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if _, ok := byID["gpt-5.5"]; ok {
		t.Error("被禁用的模型不应出现")
	}
	if got, ok := byID["custom-model"]; !ok {
		t.Error("额外模型未加入")
	} else if got.MaxOutputTokens != defaultMaxOutput {
		t.Errorf("额外模型应有兜底输出上限 %d，实际 %d", defaultMaxOutput, got.MaxOutputTokens)
	}
	// 重复出现不应产生两条
	if len(byID) != len(models) {
		t.Errorf("模型列表存在重复: %d 条 / %d 个 ID", len(models), len(byID))
	}
}

func TestBuildModelListIsStableAndUnique(t *testing.T) {
	c := newTestClient(t)
	first := c.BuildModelList(nil, nil, nil)
	second := c.BuildModelList(nil, nil, nil)
	if len(first) != len(second) {
		t.Fatalf("两次构建结果长度不一致: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("第 %d 项顺序不稳定: %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
}

// 应用内 iframe 登录依赖官方 embed=iframe 协议：保留原有参数、追加 embed 与 parent_origin。
func TestEmbeddedAuthURL(t *testing.T) {
	s := &LoginSession{AuthURL: "https://www.workbuddy.ai/login?platform=CLI&state=abc123"}
	got := s.EmbeddedAuthURL("http://wails.localhost")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("构造的链接不合法: %v", err)
	}
	q := u.Query()
	if u.Scheme != "https" || u.Host != "www.workbuddy.ai" || u.Path != "/login" {
		t.Errorf("基础部分被破坏: %s", got)
	}
	if q.Get("platform") != "CLI" || q.Get("state") != "abc123" {
		t.Errorf("原有参数丢失: %s", got)
	}
	if q.Get("embed") != "iframe" || q.Get("parent_origin") != "http://wails.localhost" {
		t.Errorf("embed 参数缺失: %s", got)
	}

	// 非法原链接原样返回
	broken := &LoginSession{AuthURL: "::::not-a-url"}
	if broken.EmbeddedAuthURL("http://x") != "::::not-a-url" {
		t.Error("非法 AuthURL 应原样返回")
	}
	// 空 origin 时不应写入空参数
	empty := &LoginSession{AuthURL: "https://www.workbuddy.ai/login?state=s"}
	if strings.Contains(empty.EmbeddedAuthURL(""), "parent_origin") {
		t.Error("空 origin 不应写入 parent_origin")
	}
}

// truncate 必须按字符切而不是按字节切。
//
// 上游错误正文基本都是中文，按字节切会在中间截断 UTF-8 序列，
// 传到客户端和日志里就是乱码。
func TestTruncateKeepsValidUTF8(t *testing.T) {
	msg := "上游额度已用尽，请充值后再试"
	got := truncate(msg, 6)
	if got != "上游额度已用" {
		t.Fatalf("截断结果不对: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断产生了非法 UTF-8: %q", got)
	}
	if truncate("abc", 10) != "abc" {
		t.Error("短于上限时不应截断")
	}
	if truncate("abc", 0) != "" {
		t.Error("上限为 0 时应返回空串")
	}
}

// -----------------------------------------------------------------------------
// OpenCode 协议族
// -----------------------------------------------------------------------------

func TestProfileKindOpenCode(t *testing.T) {
	if got := ProfileKind(config.OpenCodePreset()); got != KindOpenCode {
		t.Errorf("ProfileKind = %q, want %q", got, KindOpenCode)
	}
	// 兼容旧配置：没写 Kind 的 opencode 条目按 ID 推断。
	if got := ProfileKind(config.Profile{ID: "opencode"}); got != KindOpenCode {
		t.Errorf("按 ID 推断 = %q, want %q", got, KindOpenCode)
	}
}

func newOpenCodeClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(config.OpenCodePreset(), config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if c.kind != KindOpenCode {
		t.Fatalf("协议族 = %q, want %q", c.kind, KindOpenCode)
	}
	return c
}

// newOpenCodeClientAt 与 newOpenCodeClient 相同，但把基址指向测试桩，
// 避免单元测试打到真实的 opencode.ai。
func newOpenCodeClientAt(t *testing.T, baseURL string) *Client {
	t.Helper()
	profile := config.OpenCodePreset()
	profile.BaseURL = baseURL
	c, err := NewClient(profile, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if c.kind != KindOpenCode {
		t.Fatalf("协议族 = %q, want %q", c.kind, KindOpenCode)
	}
	return c
}

func TestNormalizeOpenCodeKey(t *testing.T) {
	c := newOpenCodeClient(t)
	key, err := c.NormalizeOpenCodeKey("  \"sk-opencode-abcdef\" ")
	if err != nil {
		t.Fatalf("合法密钥被拒: %v", err)
	}
	if key != "sk-opencode-abcdef" {
		t.Errorf("归一化结果 = %q", key)
	}
	if _, err := c.NormalizeOpenCodeKey("short"); !errors.Is(err, ErrOpenCodeKeyInvalid) {
		t.Errorf("过短密钥的错误应为 ErrOpenCodeKeyInvalid，实际 %v", err)
	}
	// 别家上游不能走这条路径：避免把 GLM 等的密钥格式套到 OpenCode 上。
	if _, err := newTestClient(t).NormalizeOpenCodeKey("sk-opencode-abcdef"); err == nil {
		t.Error("非 OpenCode 客户端应拒绝密钥归一化")
	}
}

func TestOpenCodeCredential(t *testing.T) {
	c := newOpenCodeClient(t)
	cred := c.OpenCodeCredential("sk-opencode-abcdef1234")
	if cred.Provider != "opencode" {
		t.Errorf("Provider = %q", cred.Provider)
	}
	if cred.Auth.AccessToken != "sk-opencode-abcdef1234" {
		t.Errorf("AccessToken = %q", cred.Auth.AccessToken)
	}
	if !strings.HasPrefix(cred.ID, "opencode-") {
		t.Errorf("凭据 ID = %q", cred.ID)
	}
	// 同一密钥得到同一个 ID：重复导入是更新而不是堆积。
	if again := c.OpenCodeCredential("sk-opencode-abcdef1234"); again.ID != cred.ID {
		t.Errorf("同一密钥的 ID 不稳定: %q vs %q", cred.ID, again.ID)
	}
	// 不同密钥不同 ID。
	if other := c.OpenCodeCredential("sk-opencode-9999999999"); other.ID == cred.ID {
		t.Error("不同密钥得到了同一个 ID")
	}
	// 密钥不过期：令牌没有过期时间戳，EnsureToken 不会尝试刷新。
	if cred.Auth.ExpiresAt != 0 {
		t.Error("API Key 制凭据不应有过期时间戳")
	}
	if cred.NeedsRefresh() {
		t.Error("API Key 制凭据不应触发刷新")
	}
}

// 本机导入不再自动落库：会话停在「等待确认」的状态。Poll 只占位——不
// 返回凭据、也不打上游校验（校验改在用户确认时由 SubmitLoginKey 同步做），
// 想换钥匙的用户有机会先粘贴新密钥，不必先去删本机的 auth.json。
func TestStartOpenCodeLoginImportsLocalKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("等待确认期间不应向上游发请求")
	}))
	defer srv.Close()

	t.Setenv("OPENCODE_API_KEY", "sk-local-0123456789")
	c := newOpenCodeClientAt(t, srv.URL)
	session, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !session.SkipBrowser {
		t.Error("本机已有密钥时不应打开浏览器")
	}
	// NeedKey 常驻：粘贴框可见，想换一把钥匙的用户可以直接覆盖，不必先删本机文件。
	if !session.NeedKey {
		t.Error("本地导入时粘贴框应可见（可供覆盖）")
	}
	if session.State != "local" {
		t.Errorf("State = %q, want local", session.State)
	}
	if session.LocalKey != "sk-local-0123456789" {
		t.Errorf("LocalKey = %q，应带上待确认的导入密钥", session.LocalKey)
	}
	if session.LocalKeyLabel != "OpenCode · 6789" {
		t.Errorf("LocalKeyLabel = %q，应为脱敏展示名", session.LocalKeyLabel)
	}
	// 反复轮询都只占位：不自动落库，也不会每 2 秒打一次上游。
	for i := 0; i < 3; i++ {
		cred, err := session.Poll(context.Background())
		if err != nil || cred != nil {
			t.Fatalf("等待确认期间 Poll 应占位，实际 cred=%+v err=%v", cred, err)
		}
	}
}

// 本机密钥形态不对（过短、粘贴噪声）：按粘贴流程走，且不发起任何请求。
func TestStartOpenCodeLoginLocalKeyMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("形态不对的密钥不应发起上游请求")
	}))
	defer srv.Close()

	t.Setenv("OPENCODE_API_KEY", "garbage")
	c := newOpenCodeClientAt(t, srv.URL)
	session, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !session.NeedKey {
		t.Error("本机密钥不可用时应等待用户粘贴")
	}
	if session.SkipBrowser {
		t.Error("应引导用户去官方密钥页")
	}
	if cred, err := session.Poll(context.Background()); err != nil || cred != nil {
		t.Errorf("粘贴前应占位等待，实际 cred=%+v err=%v", cred, err)
	}
}

func TestStartOpenCodeLoginWaitsForKey(t *testing.T) {
	// 把 HomeDir 与各候选目录指到一个空的临时目录，确保本机没有 auth.json。
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("APPDATA", tmp)
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("OPENCODE_API_KEY", "")

	c := newOpenCodeClient(t)
	session, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !session.NeedKey {
		t.Error("本机无密钥时应等待用户粘贴")
	}
	if session.SkipBrowser {
		t.Error("无本地凭据时应引导用户去密钥页")
	}
	if session.AuthURL != "https://opencode.ai/auth" {
		t.Errorf("AuthURL = %q", session.AuthURL)
	}
	// 粘贴前 Poll 只占位：返回空凭据、不报错。
	cred, err := session.Poll(context.Background())
	if err != nil {
		t.Fatalf("占位轮询不应报错: %v", err)
	}
	if cred != nil {
		t.Errorf("粘贴前不应有凭据: %+v", cred)
	}
}

func TestOpenCodeRefreshIsNoop(t *testing.T) {
	c := newOpenCodeClient(t)
	if err := c.Refresh(context.Background(), &Credential{}); err != nil {
		t.Errorf("API Key 制上游的刷新应为空操作，实际 %v", err)
	}
}

// 粘贴密钥时的联机校验：只有上游明确拒绝（401/403）才判定密钥无效；
// 5xx、404、联机错误都不阻止保存——密钥本身可能没问题。
func TestVerifyOpenCodeKey(t *testing.T) {
	c := newOpenCodeClient(t)

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("校验应打 /models，实际 %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-opencode-123456" {
			t.Errorf("Authorization = %q", got)
		}
		io.WriteString(w, `{"data":[{"id":"big-pickle","name":"Big Pickle"}]}`)
	}))
	defer ok.Close()

	good := config.OpenCodePreset()
	good.BaseURL = ok.URL
	goodClient, err := NewClient(good, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatal(err)
	}
	if err := goodClient.VerifyOpenCodeKey(context.Background(), "sk-opencode-123456"); err != nil {
		t.Errorf("可用密钥应通过校验: %v", err)
	}

	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		bad := config.OpenCodePreset()
		bad.BaseURL = srv.URL
		badClient, err := NewClient(bad, config.DefaultPlatform, config.DefaultProduct)
		if err != nil {
			t.Fatal(err)
		}
		err = badClient.VerifyOpenCodeKey(context.Background(), "sk-opencode-123456")
		srv.Close()
		if !errors.Is(err, ErrOpenCodeKeyRejected) {
			t.Errorf("HTTP %d 应判定为密钥被拒，实际 %v", code, err)
		}
	}

	// 5xx 与联机中断：报错但不判定为密钥被拒。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	broken := config.OpenCodePreset()
	broken.BaseURL = srv.URL
	brokenClient, err := NewClient(broken, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatal(err)
	}
	if err := brokenClient.VerifyOpenCodeKey(context.Background(), "sk-opencode-123456"); err == nil || errors.Is(err, ErrOpenCodeKeyRejected) {
		t.Errorf("500 不应判定为密钥被拒，实际 %v", err)
	}
	srv.Close()

	dead := config.OpenCodePreset()
	dead.BaseURL = "http://127.0.0.1:1"
	deadClient, err := NewClient(dead, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatal(err)
	}
	if err := deadClient.VerifyOpenCodeKey(context.Background(), "sk-opencode-123456"); err == nil || errors.Is(err, ErrOpenCodeKeyRejected) {
		t.Errorf("联机失败不应判定为密钥被拒，实际 %v", err)
	}

	// 格式错误与别家上游：在校验之前就被挡掉。
	if err := c.VerifyOpenCodeKey(context.Background(), "short"); !errors.Is(err, ErrOpenCodeKeyInvalid) {
		t.Errorf("格式错误应为 ErrOpenCodeKeyInvalid，实际 %v", err)
	}
	if err := newTestClient(t).VerifyOpenCodeKey(context.Background(), "sk-opencode-123456"); err == nil {
		t.Error("非 OpenCode 客户端应拒绝校验")
	}
}

// 分发链端到端：proxysrv 调 client.Chat，OpenCode 分支必须用配置里的
// BaseURL 与保存的 User-Agent，并把密钥作为 Bearer 带上。
func TestClientChatDispatchesToOpenCode(t *testing.T) {
	var gotUA, gotAuth, gotPath, gotSession string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotSession = r.Header.Get("x-opencode-session")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	profile := config.OpenCodePreset()
	profile.BaseURL = srv.URL
	c, err := NewClient(profile, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := c.Chat(context.Background(), &Credential{Auth: Tokens{AccessToken: "sk-key"}}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/chat/completions" {
		t.Errorf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer sk-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if string(gotBody) != string(body) {
		t.Errorf("请求体被改动: %s", gotBody)
	}
	// httptest 监听 127.0.0.1：不是 OpenCode 主机。保存的 UA 原样发出，
	// 不携带会话头——GLM 那条精确匹配要的就是这个值。
	if gotUA != "opencode" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotSession != "" {
		t.Errorf("不应携带 x-opencode-session: %q", gotSession)
	}
}
