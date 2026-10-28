package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/proxysrv"
	"github.com/znsoftm/RapidProxy/internal/store"
	"github.com/znsoftm/RapidProxy/internal/upstream"
	"github.com/znsoftm/RapidProxy/internal/winfit"
)

func TestScreenDescOmitsScaleAt100Percent(t *testing.T) {
	got := screenDesc(winfit.Size{Width: 1920, Height: 1080}, winfit.Size{Width: 1920, Height: 1080})
	if got != "1920x1080" {
		t.Errorf("100%% 缩放时不需要附带缩放说明，实际 %q", got)
	}
}

func TestScreenDescShowsScale(t *testing.T) {
	// 3840x2160 的显示器按 150% 缩放，对程序来说就是 2560x1440 个逻辑像素。
	got := screenDesc(winfit.Size{Width: 2560, Height: 1440}, winfit.Size{Width: 3840, Height: 2160})
	if !strings.Contains(got, "3840x2160") || !strings.Contains(got, "150%") {
		t.Errorf("应同时给出物理尺寸与缩放比例，实际 %q", got)
	}
}

func TestScreenDescFallsBackWhenPhysicalUnknown(t *testing.T) {
	got := screenDesc(winfit.Size{Width: 1280, Height: 720}, winfit.Size{})
	if got != "1280x720" {
		t.Errorf("物理尺寸未知时只输出逻辑尺寸，实际 %q", got)
	}
}

func TestWindowDefaultsAreSane(t *testing.T) {
	if windowDefaultWidth < windowMinWidth || windowDefaultHeight < windowMinHeight {
		t.Errorf("默认尺寸 %dx%d 不应小于最小尺寸 %dx%d",
			windowDefaultWidth, windowDefaultHeight, windowMinWidth, windowMinHeight)
	}
	if windowMinWidth <= 0 || windowMinHeight <= 0 {
		t.Error("最小尺寸必须为正数")
	}
}

// maskKey 处理的是用户输入的密钥，长度不可信，短 key 不能越界 panic。
func TestMaskKeyHandlesShortKeys(t *testing.T) {
	for _, key := range []string{"", "a", "ab", "sk-1", "sk-wbp-0123456789abcdef0123456789abcdef"} {
		got := maskKey(key)
		if !strings.Contains(got, "****") {
			t.Errorf("maskKey(%q) = %q，未做掩码", key, got)
		}
	}
	if got := maskKey("sk-wbp-0123456789abcdef0123456789abcdef"); got != "sk-wbp-012"+"****"+"cdef" {
		t.Errorf("长密钥掩码不符合预期: %q", got)
	}
}

// 终态粘滞：密钥制登录（OpenCode）由 SubmitLoginKey 先置 success 并取消轮询
// 上下文，轮询 goroutine 随后会在 <-ctx.Done() 分支醒来。没有粘滞守卫，
// 「登录成功」会被那句迟到的「登录超时或已取消」覆盖。
func TestFinishLoginLockedKeepsFirstTerminalState(t *testing.T) {
	a := &App{log: applog.New("", 50)}
	_, cancel := context.WithCancel(context.Background())
	run := &loginRun{
		provider: "opencode",
		session:  &upstream.LoginSession{NeedKey: true, AuthURL: "https://opencode.ai/auth"},
		cancel:   cancel,
		status:   "pending",
		message:  "等待粘贴密钥",
	}
	a.loginMu.Lock()
	a.login = run
	a.loginMu.Unlock()

	if !a.finishLoginLocked(run, "success", "登录成功", "opencode-abc") {
		t.Fatal("首次终态应生效")
	}
	if run.status != "success" || run.message != "登录成功" || run.accountID != "opencode-abc" {
		t.Fatalf("终态写入异常: %+v", run)
	}
	// 轮询 goroutine 的迟到失败被挡下。
	if a.finishLoginLocked(run, "failed", "登录超时或已取消", "") {
		t.Error("迟到终态应被粘滞守卫挡下")
	}
	if run.status != "success" || run.message != "登录成功" {
		t.Errorf("成功态被覆盖: status=%q message=%q", run.status, run.message)
	}

	// 用户取消了流程（a.login 被清空）之后，旧 run 的终态同样被忽略，
	// 不能把界面上的「已取消」再改成别的。
	a.loginMu.Lock()
	a.login = nil
	a.loginMu.Unlock()
	if a.finishLoginLocked(run, "failed", "取消后的迟到终态", "") {
		t.Error("已取消失误的终态应被忽略")
	}

	// 新流程自身的终态正常生效。
	run2 := &loginRun{provider: "workbuddy", cancel: func() {}, status: "pending"}
	a.loginMu.Lock()
	a.login = run2
	a.loginMu.Unlock()
	if !a.finishLoginLocked(run2, "failed", "授权失败", "") {
		t.Error("新流程的终态应生效")
	}
	if run2.status != "failed" {
		t.Errorf("run2 status = %q", run2.status)
	}
}

// 等待确认期间轮询 goroutine 只占位：不落库、不碰终态。用户在此期间
// 粘贴新密钥并提交后流程已终态，迟到的轮询唤醒也不会把它改回失败
// （终态粘滞）——本机导入的密钥改由 SubmitLoginKey 的空提交收口，
// 见 TestSubmitLoginKeyEmptyConfirmsImportedKey。
func TestPollLoginSkipsCredentialAfterTakeover(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"big-pickle","name":"Big Pickle"}]}`)
	}))
	defer stub.Close()

	profile := config.OpenCodePreset()
	profile.BaseURL = stub.URL
	client, err := upstream.NewClient(profile, config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatal(err)
	}
	// 环境变量优先于 auth.json：本机导入路径稳定拿到这把钥匙。
	t.Setenv("OPENCODE_API_KEY", "sk-local-0123456789")

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger), store: credStore}

	session, err := client.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := &loginRun{provider: "opencode", session: session, cancel: func() {}, status: "pending"}
	a.loginMu.Lock()
	a.login = run
	a.loginMu.Unlock()
	// 用户粘贴了另一把密钥并提交：终态 + 取消轮询。
	if !a.finishLoginLocked(run, "success", "登录成功", "opencode-pasted") {
		t.Fatal("粘贴接管应生效")
	}

	// 轮询 goroutine 随后醒来（上下文已取消）：不落库、不覆盖终态。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.pollLogin(ctx, run)
	creds, err := credStore.List("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 0 {
		t.Errorf("等待确认期间不应有凭据落库，实际 %d 个: %+v", len(creds), creds)
	}
	if view := a.loginState(); view.Status != "success" {
		t.Errorf("迟到的轮询不应覆盖终态，实际 status=%q", view.Status)
	}
}

// 空提交 = 确认使用本机导入的密钥：凭据落库、上游自动启用、流程收口。
// 没有等待中的本地导入会话时，空提交按空密钥报错（防误触）。
func TestSubmitLoginKeyEmptyConfirmsImportedKey(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("确认前应联机校验 /models，实际 %q", r.URL.Path)
		}
		io.WriteString(w, `{"data":[{"id":"big-pickle","name":"Big Pickle"}]}`)
	}))
	defer stub.Close()

	// 配置里的 opencode 指向桩服务器：确认时的联机校验不能打真实上游。
	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	profile, _ := cfg.ProfileByID("opencode")
	profile.BaseURL = stub.URL
	cfg.SetProfile(profile)
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger), store: credStore}

	client, err := a.srv.ClientFor("opencode")
	if err != nil {
		t.Fatal(err)
	}
	// 环境变量优先于 auth.json：本机导入路径稳定拿到这把钥匙。
	t.Setenv("OPENCODE_API_KEY", "sk-local-0123456789")

	session, err := client.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := &loginRun{provider: "opencode", session: session, cancel: func() {}, status: "pending"}
	a.loginMu.Lock()
	a.login = run
	a.loginMu.Unlock()

	// 空提交：确认使用本机导入的密钥。
	if err := a.SubmitLoginKey("opencode", ""); err != nil {
		t.Fatalf("空提交应确认本机密钥，实际 %v", err)
	}
	creds, err := credStore.List("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("应落库一份凭据，实际 %d 个", len(creds))
	}
	if creds[0].Auth.AccessToken != "sk-local-0123456789" {
		t.Errorf("落库的密钥不对: %q", creds[0].Auth.AccessToken)
	}
	// 流程收口为成功。
	if view := a.loginState(); view.Status != "success" {
		t.Errorf("确认后流程应为 success，实际 %q", view.Status)
	}
	// 登录即表达使用意图：预设默认关闭的上游被自动启用并落盘。
	if p, _ := a.settings().ProfileByID("opencode"); !p.Enabled {
		t.Error("opencode 应随登录自动启用")
	}
	persisted, err := config.Load(a.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := persisted.ProfileByID("opencode"); !p.Enabled {
		t.Error("自动启用应落盘，重启后仍生效")
	}

	// 没有等待中的本地导入会话时，空提交按空密钥报错。
	if err := a.SubmitLoginKey("opencode", ""); err == nil {
		t.Error("无待确认导入密钥时，空提交应报错")
	}
}

// needKey 必须透传到视图：前端靠它决定显示密钥输入框还是浏览器授权提示。
func TestLoginStateMapsNeedKey(t *testing.T) {
	a := &App{log: applog.New("", 50)}
	a.loginMu.Lock()
	a.login = &loginRun{
		provider: "opencode",
		session: &upstream.LoginSession{
			AuthURL:   "https://opencode.ai/auth",
			State:     "paste",
			NeedKey:   true,
			ExpiresAt: time.Now().Add(10 * time.Minute),
		},
		cancel:  func() {},
		status:  "pending",
		message: "请在浏览器中创建 OpenCode 密钥，并粘贴到下方输入框",
	}
	a.loginMu.Unlock()

	view := a.loginState()
	if !view.NeedKey {
		t.Error("needKey 未映射到 LoginView")
	}
	if !view.Active || view.Provider != "opencode" {
		t.Errorf("active/provider 异常: %+v", view)
	}
	if view.URL != "https://opencode.ai/auth" || view.State != "paste" {
		t.Errorf("url/state 异常: %+v", view)
	}
	if view.Message == "" {
		t.Error("message 不应为空")
	}

	// 空闲态：没有流程时 needKey 为 false。
	idle := (&App{log: applog.New("", 50)}).loginState()
	if idle.Status != "idle" || idle.NeedKey {
		t.Errorf("空闲态异常: %+v", idle)
	}
}

// 四家服务商都改成 OAuth 登录后，登录面板必须显示各自的授权文案（带服务商
// 名），而不是 OpenCode 的粘贴密钥界面。Claude 的回调页由 Anthropic 托管，
// 因此它额外要求「粘贴授权码」；其余三家走浏览器自动收口。
func TestStartLoginOAuthPerProvider(t *testing.T) {
	cases := []struct {
		id       string
		want     string
		wantCode bool
	}{
		{"xai", "xAI", false},
		{"claude", "Claude", true},
		{"chatgpt", "ChatGPT", false},
		{"zhipu", "智谱编程", false},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
		logger := applog.New(filepath.Join(dir, "logs"), 3)
		t.Cleanup(func() { _ = logger.Close() }) // Windows 上不关句柄会卡住 TempDir 清理
		a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger)}

		view, err := a.StartLogin(tc.id, "")
		if err != nil {
			a.CancelLogin()
			// 智谱要联网创建登录流；断网环境下跳过它的联网断言，但配置侧
			// （auth=oauth）仍由 config 包的测试覆盖。
			if tc.id == "zhipu" {
				continue
			}
			t.Fatalf("%s: 发起登录失败: %v", tc.id, err)
		}
		a.CancelLogin()

		if view.NeedKey {
			t.Errorf("%s: OAuth 上游不应走粘贴密钥分支", tc.id)
		}
		if view.NeedCode != tc.wantCode {
			t.Errorf("%s: needCode = %v, want %v", tc.id, view.NeedCode, tc.wantCode)
		}
		if view.URL == "" {
			t.Errorf("%s: OAuth 上游应返回授权链接", tc.id)
		}
		if !strings.Contains(view.Message, tc.want) {
			t.Errorf("%s: 提示应包含服务商名 %q，实际 %q", tc.id, tc.want, view.Message)
		}
		if strings.Contains(view.Message, "OpenCode") {
			t.Errorf("%s: 提示不应出现 OpenCode 字样，实际 %q", tc.id, view.Message)
		}
	}
}

func TestSameListenAddr(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:8787", "127.0.0.1:8787", true},
		{":8787", "[::]:8787", true}, // 都表示监听所有网卡
		{"0.0.0.0:8787", "[::]:8787", true},
		{"127.0.0.1:8787", "[::]:8787", false},
		{"127.0.0.1:8787", "127.0.0.1:8788", false},
		{"127.0.0.1:8787", "not-an-address", false},
	}
	for _, tc := range cases {
		if got := sameListenAddr(tc.a, tc.b); got != tc.want {
			t.Errorf("sameListenAddr(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// 登录成功后要把仍未启用的上游自动拉起：只登录不启用时凭据存了却
// 什么都不发生（模型页与 /v1/models 只看「已启用 ∧ 已登录」）。
func TestEnableProviderIfDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() }) // Windows 上不关句柄会卡住 TempDir 清理
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger)}

	// lobsterai 预设默认未启用：登录后应被拉起，且落盘。
	a.enableProviderIfDisabled("lobsterai")
	got, ok := a.settings().ProfileByID("lobsterai")
	if !ok || !got.Enabled {
		t.Fatalf("lobsterai 应随登录自动启用: enabled=%v ok=%v", got.Enabled, ok)
	}
	persisted, err := config.Load(a.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := persisted.ProfileByID("lobsterai"); !p.Enabled {
		t.Fatal("自动启用应落盘，重启后仍生效")
	}
	// 其它未登录上游不受影响。
	if p, _ := a.settings().ProfileByID("qoder-cn"); p.Enabled {
		t.Fatal("未登录的上游不应被连带启用")
	}

	// 已启用的上游是空操作（不重写配置）。
	a.enableProviderIfDisabled("lobsterai")
	if p, _ := a.settings().ProfileByID("lobsterai"); !p.Enabled {
		t.Fatal("已启用的上游不应被关闭")
	}

	// 未知 provider 静默返回，不 panic。
	a.enableProviderIfDisabled("no-such-provider")
}

// 粘贴授权码输错是中间失败：流程保持 pending，用户能重新粘贴，
// 不该被判成「登录失败」从头再来。
//
// 用桩会话（pollFn 直接返回可重试错误）而不是真实上游：这条逻辑属于
// 轮询循环的分支处理，不该依赖外网。
func TestPollLoginKeepsFlowOnRetryableCodeError(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	polls := 0
	session := &upstream.LoginSession{
		AuthURL:  "https://claude.ai/oauth/authorize",
		State:    "claude",
		NeedCode: true,
	}
	session.SetPollFunc(func(context.Context) (*upstream.Credential, error) {
		polls++
		if polls <= 2 {
			return nil, upstream.ErrLoginCodeRejected
		}
		// 第 3 次起结束测试：取消上下文，轮询循环会走超时分支。
		cancel()
		return nil, context.Canceled
	})
	session.SetExpiry(time.Now().Add(time.Minute))

	run := &loginRun{provider: "claude", session: session, cancel: cancel, status: "pending", message: "等待授权码"}
	a.loginMu.Lock()
	a.login = run
	a.loginMu.Unlock()

	a.pollLogin(ctx, run)

	if polls < 3 {
		t.Errorf("可重试错误应继续轮询，实际只轮询 %d 次", polls)
	}
}

// 自定义分组按「协议 + 地址 + 密钥」各自成组：换地址或换密钥就是另一个
// 分组，三项都相同才复用同一组（重复提交幂等）。
func TestAddCustomProviderGroupsByIdentity(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 联机校验会打 /models：桩服务器一律放行，避免测试依赖外网。
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"demo-model","name":"Demo"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger), store: credStore}

	first, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai", Model: "demo-model",
	})
	if err != nil {
		t.Fatalf("新增自定义分组失败: %v", err)
	}
	profile, ok := a.settings().ProfileByID(first)
	if !ok {
		t.Fatalf("新增的分组 %s 未进配置", first)
	}
	if profile.Protocol != "openai" || profile.BaseURL != stub.URL {
		t.Errorf("分组字段不对: %+v", profile)
	}
	// 名称留空时按域名生成，且带上协议后缀，便于和另一协议的组区分。
	if profile.Name == "" || !strings.Contains(profile.Name, "OpenAI") {
		t.Errorf("留空名称应按域名生成并带协议后缀，实际 %q", profile.Name)
	}

	// 同一组重复提交：幂等，仍是一个分组。
	again, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai", Model: "demo-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("同地址同密钥应复用同一分组: %s != %s", again, first)
	}

	// 同地址换密钥：另一个分组。
	otherKey, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-beta", Protocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	if otherKey == first {
		t.Error("不同 API Key 应各自成组")
	}

	// 同地址换线协议：也是另一个分组。
	anthropic, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "anthropic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if anthropic == first {
		t.Error("不同线协议应各自成组")
	}
	if p, _ := a.settings().ProfileByID(anthropic); p.Protocol != "anthropic" {
		t.Errorf("Anthropic 分组的协议字段不对: %+v", p)
	}

	// 地址归一：尾斜杠与大小写差异不该造出新分组。
	same, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: strings.ToUpper(stub.URL) + "/", APIKey: "sk-alpha", Protocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	if same != first {
		t.Errorf("地址仅大小写 / 尾斜杠不同应视为同一分组: %s != %s", same, first)
	}

	// 每个分组各自落库了凭据。
	creds, err := credStore.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 3 {
		t.Errorf("应有 3 个分组的凭据（重复提交不新增），实际 %d 个", len(creds))
	}
}

// 删除自定义分组会连带清掉该组的凭据；内置预设不允许删除。
func TestDeleteCustomProvider(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"demo-model"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger), store: credStore}

	id, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := credStore.Count(id); n != 1 {
		t.Fatalf("新增后应有 1 份凭据，实际 %d", n)
	}

	if err := a.DeleteCustomProvider(id); err != nil {
		t.Fatalf("删除自定义分组失败: %v", err)
	}
	if _, ok := a.settings().ProfileByID(id); ok {
		t.Error("分组应从配置里移除")
	}
	if n := credStore.Count(id); n != 0 {
		t.Errorf("分组凭据应一并删除，实际还剩 %d 份", n)
	}

	// 内置预设不可删（否则下次 normalize 又会补回来，删了等于没删）。
	if err := a.DeleteCustomProvider("claude"); err == nil {
		t.Error("内置预设应拒绝删除")
	}
	if _, ok := a.settings().ProfileByID("claude"); !ok {
		t.Error("内置预设不应被删除")
	}

	// 未知分组报错而不是静默成功。
	if err := a.DeleteCustomProvider("custom-nosuchgroup"); err == nil {
		t.Error("未知分组应报错")
	}
}

// 删掉分组后，引用它的虚拟组成员要一并清理，否则会留下永远走不通的成员。
func TestDeleteCustomProviderPrunesVirtualMembers(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"demo-model"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, nil, logger), store: credStore}

	id, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai", Model: "demo-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg = a.settings().Clone()
	cfg.VirtualModels = []config.VirtualModel{{
		Name:    "mix",
		Enabled: true,
		Members: []config.VirtualMember{{Provider: id, Model: "demo-model", Weight: 1}},
	}}
	cfg.Normalize()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)

	if err := a.DeleteCustomProvider(id); err != nil {
		t.Fatal(err)
	}
	vm, ok := a.settings().VirtualModelByName("mix")
	if !ok {
		t.Fatal("虚拟组本身应保留（用户可能还要加别的成员）")
	}
	if len(vm.Members) != 0 {
		t.Errorf("指向已删分组的成员应被清理，实际还剩 %+v", vm.Members)
	}
}

// 自定义分组的「可用模型」支持一次填多个：上游没有 /models 目录时，
// 这是用户登记模型的唯一途径。逗号（中英文）、分号、空白都算分隔。
func TestAddCustomProviderAcceptsMultipleModels(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 上游没有 /models：同步拿不到清单，只能靠手填。
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	id, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL:  stub.URL,
		APIKey:   "sk-alpha",
		Protocol: "anthropic",
		Model:    "claude-opus-4-5, claude-sonnet-4-5；claude-haiku-4-5",
	})
	if err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	p, _ := a.settings().ProfileByID(id)
	want := []string{"claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"}
	if len(p.ExtraModels) != len(want) {
		t.Fatalf("应解析出 %d 个模型，实际 %v", len(want), p.ExtraModels)
	}
	for i, m := range want {
		if p.ExtraModels[i] != m {
			t.Errorf("第 %d 个模型 = %q，期望 %q", i, p.ExtraModels[i], m)
		}
	}

	// 再次提交只新增模型，不重复累积。
	if _, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "anthropic",
		Model: "claude-opus-4-5, claude-3-5-haiku",
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = a.settings().ProfileByID(id)
	if len(p.ExtraModels) != len(want)+1 {
		t.Errorf("重复提交应只并入新模型，实际 %v", p.ExtraModels)
	}
}

// splitModels 处理各种分隔符与空白。
func TestSplitModels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a，b", []string{"a", "b"}},
		{"a;b", []string{"a", "b"}},
		{"a b", []string{"a", "b"}},
		{" a ,, b \n c ", []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		got := splitModels(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitModels(%q) = %v，期望 %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitModels(%q) = %v，期望 %v", c.in, got, c.want)
				break
			}
		}
	}
}

// 同一把密钥的不同粘贴形式必须复用同一分组：落库的凭据存的是归一化后的
// 密钥，分组 ID 也必须基于归一化值，否则 "Bearer sk-x" 与 "sk-x" 会各建一组。
func TestAddCustomProviderNormalizesKeyForIdentity(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	first, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"Bearer sk-alpha", "`sk-alpha`", `"sk-alpha"`, "  sk-alpha  "} {
		id, err := a.AddCustomProvider(CustomProviderInput{
			BaseURL: stub.URL, APIKey: variant, Protocol: "openai",
		})
		if err != nil {
			t.Fatalf("提交 %q 失败: %v", variant, err)
		}
		if id != first {
			t.Errorf("%q 应复用同一分组，实际 %s != %s", variant, id, first)
		}
	}
	if n := credStore.Count(first); n != 1 {
		t.Errorf("同一密钥应只有一份凭据，实际 %d", n)
	}
}

// 无密钥的提交不能并进同地址的已有分组：那样会把对方的模型并过来，
// 而用户的本意是建一个独立的、稍后再登录的分组。
func TestAddCustomProviderEmptyKeyDoesNotHijackGroup(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	keyed, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai", Model: "only-this",
	})
	if err != nil {
		t.Fatal(err)
	}
	keyless, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "", Protocol: "openai", Model: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if keyed == keyless {
		t.Fatal("无密钥分组不应与同地址的有密钥分组合并")
	}
	p, _ := a.settings().ProfileByID(keyed)
	if len(p.ExtraModels) != 1 || p.ExtraModels[0] != "only-this" {
		t.Errorf("有密钥分组的模型被污染: %v", p.ExtraModels)
	}
}

// 自动生成的显示名不能重名：同域名、同路径、不同密钥最容易撞。
func TestAddCustomProviderAutoNameIsUnique(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	seen := map[string]string{}
	for _, key := range []string{"sk-alpha-1111", "sk-beta-2222", "sk-gamma-3333"} {
		id, err := a.AddCustomProvider(CustomProviderInput{
			BaseURL: stub.URL, APIKey: key, Protocol: "openai",
		})
		if err != nil {
			t.Fatal(err)
		}
		p, _ := a.settings().ProfileByID(id)
		if prev, dup := seen[p.Name]; dup {
			t.Errorf("显示名重复: %q 同时用于 %s 与 %s", p.Name, prev, id)
		}
		seen[p.Name] = id
	}
}

// 用户手填的名字原样保留，不因重名被改写。
func TestAddCustomProviderKeepsExplicitName(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	for _, key := range []string{"sk-a-1111", "sk-b-2222"} {
		id, err := a.AddCustomProvider(CustomProviderInput{
			Name: "我的网关", BaseURL: stub.URL, APIKey: key, Protocol: "openai",
		})
		if err != nil {
			t.Fatal(err)
		}
		if p, _ := a.settings().ProfileByID(id); p.Name != "我的网关" {
			t.Errorf("用户指定的名称被改写为 %q", p.Name)
		}
	}
}

// 地址归一化只影响 scheme / host 的大小写，路径保持大小写敏感。
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://API.Example.com/v1", "https://api.example.com/v1"},
		{"https://api.example.com/v1/", "https://api.example.com/v1"},
		{"https://api.example.com/API/v1", "https://api.example.com/API/v1"},
		{"  https://api.example.com/v1  ", "https://api.example.com/v1"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1"},
	}
	for _, c := range cases {
		if got := normalizeBaseURL(c.in); got != c.want {
			t.Errorf("normalizeBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 路径大小写不同必须是不同端点，否则会把两个真实不同的接口撞成一组。
	if normalizeBaseURL("https://a.com/API") == normalizeBaseURL("https://a.com/api") {
		t.Error("路径大小写敏感，不应归一")
	}
}

// 删除分组：配置先落盘、凭据后清理；残留凭据文件不应让删除报错。
func TestDeleteCustomProviderSucceedsDespiteMissingCreds(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	id, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-alpha", Protocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 手动删掉凭据文件，模拟「配置还在、凭据已丢」的不一致状态。
	creds, _ := credStore.List(id)
	for _, c := range creds {
		if err := credStore.Delete(id, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DeleteCustomProvider(id); err != nil {
		t.Fatalf("凭据已缺失时删除分组仍应成功: %v", err)
	}
	if _, ok := a.settings().ProfileByID(id); ok {
		t.Error("分组应从配置里移除")
	}
}

// 自动名称要能区分不同端点，同时不因「末尾多写 /v1」而看起来是两个端点：
// 上游客户端会自动补 /v1，所以 host/path 与 host/path/v1 是同一个端点。
func TestCustomProviderNameConsistency(t *testing.T) {
	same := [][2]string{
		{"https://api.deepseek.com", "https://api.deepseek.com/v1"},
		{"https://relay.example.com/anthropic", "https://relay.example.com/anthropic/v1"},
		{"https://relay.example.com/anthropic", "https://relay.example.com/anthropic/v1/"},
	}
	for _, pair := range same {
		a := customProviderName("", "openai", pair[0])
		b := customProviderName("", "openai", pair[1])
		if a != b {
			t.Errorf("%q 与 %q 应生成同一名称，实际 %q != %q", pair[0], pair[1], a, b)
		}
	}

	// 不同端点必须能区分。
	differ := [][2]string{
		{"https://relay.example.com/openai", "https://relay.example.com/anthropic"},
		{"https://a.example.com/v1", "https://b.example.com/v1"},
	}
	for _, pair := range differ {
		if customProviderName("", "openai", pair[0]) == customProviderName("", "openai", pair[1]) {
			t.Errorf("%q 与 %q 应生成不同名称", pair[0], pair[1])
		}
	}

	// 线协议不同，名称也要不同（同域名挂 OpenAI 与 Anthropic 两个端点很常见）。
	if customProviderName("", "openai", "https://relay.example.com") ==
		customProviderName("", "anthropic", "https://relay.example.com") {
		t.Error("不同线协议应生成不同名称")
	}

	// 用户给了名字就原样用。
	if got := customProviderName("我的网关", "openai", "https://relay.example.com"); got != "我的网关" {
		t.Errorf("用户指定名称被改写为 %q", got)
	}
}

// 非 /v1 的版本段（如智谱的 /v4）要保留：它确实是路径的一部分，不能当成
// 可省略的版本后缀剥掉。
func TestCustomProviderNameKeepsNonV1Version(t *testing.T) {
	got := customProviderName("", "openai", "https://open.bigmodel.cn/api/coding/paas/v4")
	if !strings.Contains(got, "/api/coding/paas/v4") {
		t.Errorf("非 /v1 的版本段应保留在名称里，实际 %q", got)
	}
}

// displayPath 的剥离规则。
func TestDisplayPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://a.com", ""},
		{"https://a.com/v1", ""},
		{"https://a.com/v1/", ""},
		{"https://a.com/api", "api"},
		{"https://a.com/api/v1", "api"},
		{"https://a.com/api/v1/v1", "api"},
		{"https://a.com/API/v1", "API"},
	}
	for _, c := range cases {
		if got := displayPath(c.in); got != c.want {
			t.Errorf("displayPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 用户常常整条粘贴对话端点（文档 / 代码里复制的完整 URL），而 base_url 的
// 语义是前缀，程序会自己拼 /chat/completions。不剥掉就会拼出
// .../chat/completions/v1/chat/completions，请求全部 404。
func TestNormalizeBaseURLInput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.deepseek.com", "https://api.deepseek.com"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1/chat/completions", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/chat/completions", "https://api.deepseek.com"},
		{"https://relay.example.com/anthropic/v1/messages", "https://relay.example.com/anthropic/v1"},
		{"https://host/v1/responses", "https://host/v1"},
		// fragment 与 query 必须去掉：拼上子路径后它们会把路径吞掉。
		{"https://api.deepseek.com?api-version=2", "https://api.deepseek.com"},
		{"https://api.deepseek.com/v1#frag", "https://api.deepseek.com/v1"},
		// 大小写不敏感地识别端点尾巴。
		{"https://host/V1/CHAT/COMPLETIONS", "https://host/V1"},
	}
	for _, c := range cases {
		if got := normalizeBaseURLInput(c.in); got != c.want {
			t.Errorf("normalizeBaseURLInput(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 单独的 /completions 可能真是自建网关的 base，不能剥。
	if got := normalizeBaseURLInput("https://host/api/completions"); got != "https://host/api/completions" {
		t.Errorf("单独的 /completions 不应被剥掉，实际 %q", got)
	}
}

// 粘贴完整端点后，分组必须能正常工作：地址被收敛、身份按收敛后的地址计算。
func TestAddCustomProviderAcceptsPastedEndpointURL(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var gotPath string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	// 用户粘贴的是完整对话端点。
	id, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL:  stub.URL + "/v1/chat/completions",
		APIKey:   "sk-alpha",
		Protocol: "openai",
	})
	if err != nil {
		t.Fatalf("粘贴完整端点应被接受: %v", err)
	}
	p, _ := a.settings().ProfileByID(id)
	if p.BaseURL != stub.URL+"/v1" {
		t.Errorf("地址应被收敛到 %q，实际 %q", stub.URL+"/v1", p.BaseURL)
	}
	// 校验请求应打到 .../v1/models，而不是 .../v1/chat/completions/v1/models。
	if gotPath != "/v1/models" {
		t.Errorf("请求路径应为 /v1/models，实际 %q", gotPath)
	}
}

// 并发添加不同分组时不能丢配置：cfgMu 只保护指针，挡不住两个写者各自
// 克隆同一份旧配置再依次覆盖（实测 6 个并发只落盘 1 个）。cfgWriteMu 把
// 「读-改-写」整体串行化后才正确。
func TestAddCustomProviderConcurrent(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = a.AddCustomProvider(CustomProviderInput{
				BaseURL: stub.URL, APIKey: fmt.Sprintf("sk-key-%d", i), Protocol: "openai",
			})
		}(i)
	}
	wg.Wait()

	count := func(profiles []config.Profile) int {
		n := 0
		for _, p := range profiles {
			if p.IsUserCustomProfile() {
				n++
			}
		}
		return n
	}
	persisted, err := config.Load(a.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := count(persisted.Profiles); got != n {
		t.Errorf("落盘应有 %d 个自定义分组，实际 %d", n, got)
	}
	if got := count(a.settings().Profiles); got != n {
		t.Errorf("内存应有 %d 个自定义分组，实际 %d", n, got)
	}
}

// 并发增删混跑后，内存与落盘必须一致，且删掉的那个真的不在了。
func TestAddDeleteCustomProviderConcurrent(t *testing.T) {
	dir := t.TempDir()
	logger := applog.New(filepath.Join(dir, "logs"), 3)
	t.Cleanup(func() { _ = logger.Close() })
	credStore, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer stub.Close()

	cfg := config.DefaultAt(filepath.Join(dir, "config.json"))
	a := &App{cfg: cfg, log: logger, srv: proxysrv.New(cfg, credStore, logger), store: credStore}

	victim, err := a.AddCustomProvider(CustomProviderInput{
		BaseURL: stub.URL, APIKey: "sk-victim", Protocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); _ = a.DeleteCustomProvider(victim) }()
	for i := 0; i < 4; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = a.AddCustomProvider(CustomProviderInput{
				BaseURL: stub.URL, APIKey: fmt.Sprintf("sk-new-%d", i), Protocol: "openai",
			})
		}(i)
	}
	wg.Wait()

	if _, ok := a.settings().ProfileByID(victim); ok {
		t.Error("被删的分组不应还在内存配置里")
	}
	persisted, err := config.Load(a.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProfileByID(victim); ok {
		t.Error("被删的分组不应还在落盘配置里")
	}
	n := 0
	for _, p := range persisted.Profiles {
		if p.IsUserCustomProfile() {
			n++
		}
	}
	if n != 4 {
		t.Errorf("应有 4 个自定义分组，实际 %d", n)
	}
}
