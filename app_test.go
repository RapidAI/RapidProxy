package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// 本机导入的密钥被上游拒绝时，同一个 pending 流程应翻转成粘贴模式：
// 界面出现密钥输入框、说明换成密钥被拒，用户就地换一把，而不是流程结束、
// 用户不删 auth.json 就出不来。
func TestFallbackLoginToKeyPaste(t *testing.T) {
	a := &App{log: applog.New("", 50)}
	a.loginMu.Lock()
	a.login = &loginRun{
		provider: "opencode",
		session: &upstream.LoginSession{
			State:       "local",
			AuthURL:     "https://opencode.ai/auth",
			SkipBrowser: true,
			ExpiresAt:   time.Now().Add(10 * time.Minute),
		},
		cancel:  func() {},
		status:  "pending",
		message: "正在导入本机密钥",
	}
	a.loginMu.Unlock()

	if !a.fallbackLoginToKeyPaste(a.login) {
		t.Fatal("pending 的本地导入流程应能转为粘贴模式")
	}
	view := a.loginState()
	if !view.NeedKey {
		t.Error("翻转后应等待粘贴密钥")
	}
	if view.State != "paste" {
		t.Errorf("State = %q, want paste", view.State)
	}
	if view.Message == "" {
		t.Error("应给出密钥被拒的说明")
	}

	// 已结束的流程不再翻转：返回 false，状态原样。
	done := &loginRun{
		provider: "opencode",
		session:  &upstream.LoginSession{State: "local"},
		cancel:   func() {},
		status:   "success",
	}
	a.loginMu.Lock()
	a.login = done
	a.loginMu.Unlock()
	if a.fallbackLoginToKeyPaste(done) {
		t.Error("非 pending 流程不应被翻转")
	}
	if done.session.NeedKey {
		t.Error("非 pending 流程不应被改成等待粘贴")
	}
}

// 用户在本机导入进行中粘贴了另一把密钥：SubmitLoginKey 已置终态，轮询
// goroutine 随后才拿到本机密钥——这时不能再落库，否则一次登录存出两个
// 账号（本机密钥 + 粘贴密钥），流量被悄悄分走。
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

	// 轮询 goroutine 这时才校验完本机密钥：不能落库。
	a.pollLogin(context.Background(), run)
	creds, err := credStore.List("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 0 {
		t.Errorf("被接管后迟到的本机密钥不应落库，实际 %d 个: %+v", len(creds), creds)
	}

	// 对照组：流程仍是 pending 时，本机密钥正常落库。
	session2, err := client.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run2 := &loginRun{provider: "opencode", session: session2, cancel: func() {}, status: "pending"}
	a.loginMu.Lock()
	a.login = run2
	a.loginMu.Unlock()
	a.pollLogin(context.Background(), run2)

	creds, err = credStore.List("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("pending 流程的本机密钥应正常落库，实际 %d 个", len(creds))
	}
	if creds[0].Auth.AccessToken != "sk-local-0123456789" {
		t.Errorf("落库的密钥不对: %q", creds[0].Auth.AccessToken)
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
