package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/autostart"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/locale"
	"github.com/znsoftm/RapidProxy/internal/proxysrv"
	"github.com/znsoftm/RapidProxy/internal/stats"
	"github.com/znsoftm/RapidProxy/internal/store"
	"github.com/znsoftm/RapidProxy/internal/tray"
	"github.com/znsoftm/RapidProxy/internal/update"
	"github.com/znsoftm/RapidProxy/internal/upstream"
	"github.com/znsoftm/RapidProxy/internal/winfit"
)

// 事件名（前端通过 window.runtime.EventsOn 订阅）。
const (
	eventState  = "state"
	eventLogin  = "login"
	eventLog    = "log"
	eventUpdate = "update"
)

// Version 是程序版本号。
const Version = "1.6.0"

// 窗口尺寸（逻辑像素，与 Wails 的屏幕尺寸单位一致）。
//
// 这只是「期望值」，实际尺寸会在启动时按当前屏幕收敛，见 App.fitWindow。
const (
	windowDefaultWidth  = 1080
	windowDefaultHeight = 740
	windowMinWidth      = 940
	windowMinHeight     = 600
)

// -----------------------------------------------------------------------------
// 前端视图模型
// -----------------------------------------------------------------------------

// AccessView 是给用户复制到其它软件的接入信息。
type AccessView struct {
	BaseURL    string   `json:"baseUrl"`
	OpenAIURL  string   `json:"openaiUrl"`
	APIKeys    []string `json:"apiKeys"`
	RequireKey bool     `json:"requireKey"`
	Models     []string `json:"models"`
	Running    bool     `json:"running"`
	Sample     string   `json:"sample"`
}

// AccountView 是账号列表项。
type AccountView struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	Nickname     string `json:"nickname"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	ExpiresAt    string `json:"expiresAt"`
	ExpiresIn    string `json:"expiresIn"`
	Expired      bool   `json:"expired"`
	CreatedAt    string `json:"createdAt"`
	// Weight 是组内负载均衡权重，Paused 为 true 时不参与转发。
	Weight int  `json:"weight"`
	Paused bool `json:"paused"`
	// Cooldown 是限流/额度冷却的剩余时间（秒），0 表示正常。
	Cooldown int `json:"cooldown"`
	// CooldownReason 说明冷却原因：rate（限流）或 credit（额度耗尽）。
	CooldownReason string `json:"cooldownReason,omitempty"`
}

// ProviderView 是上游配置项。
type ProviderView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	BaseURL      string `json:"baseUrl"`
	Origin       string `json:"origin"`
	Proxy        string `json:"proxy"`
	AccountCount int    `json:"accountCount"`
	ModelCount   int    `json:"modelCount"`
	NeedsProxy   bool   `json:"needsProxy"`
}

// ModelView 是模型列表项。
type ModelView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`   // 上游显示名
	ProviderID string `json:"providerId"` // 上游 ID（统计与路由使用）
	Context    int64  `json:"context"`
	MaxOut     int64  `json:"maxOut"`
	Images     bool   `json:"images"`
	// Accounts 是当前能承接该模型请求的账号数，TotalAccounts 是该版本已登录的总数。
	// 多个账号时同名模型会在它们之间按权重分摊，这个数字就是分摊的宽度。
	Accounts      int `json:"accounts"`
	TotalAccounts int `json:"totalAccounts"`
	// Virtual 为 true 表示这是一条虚拟模型组条目（ProviderID 恒为 "virtual"）。
	Virtual bool `json:"virtual,omitempty"`
}

// VirtualMemberView 是虚拟组里的一个成员。
type VirtualMemberView struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	Model        string `json:"model"`
	Weight       int    `json:"weight"`
	// Accounts 是该成员此刻可承接请求的账号数，0 表示这条路径暂时走不通。
	Accounts int `json:"accounts"`
	// Available 为 true 表示该成员现在能被选中（上游启用、已登录、有可用账号）。
	Available bool `json:"available"`
}

// VirtualModelView 是虚拟模型组。
type VirtualModelView struct {
	Name    string              `json:"name"`
	Enabled bool                `json:"enabled"`
	Members []VirtualMemberView `json:"members"`
	// Available 是此刻可承接请求的成员数。
	Available int `json:"available"`
}

// VirtualMemberInput 是保存虚拟组时的成员入参。
type VirtualMemberInput struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Weight   int    `json:"weight"`
}

// VirtualModelInput 是保存虚拟组的入参。
type VirtualModelInput struct {
	Name    string               `json:"name"`
	Enabled bool                 `json:"enabled"`
	Members []VirtualMemberInput `json:"members"`
	// OriginalName 非空表示这是对已有组的修改（允许改名）。
	OriginalName string `json:"originalName"`
}

// SettingsView 是设置面板数据。
type SettingsView struct {
	Listen         string `json:"listen"`
	ModelSyncHours int    `json:"modelSyncHours"`
	CORS           bool   `json:"cors"`
	Sanitize       bool   `json:"sanitize"`
	MaxThinking    bool   `json:"maxThinking"`
	AutoStart      bool   `json:"autoStart"`
	LaunchAtLogin  bool   `json:"launchAtLogin"`
	// Language 是界面语言偏好：system / zh / en（前端据此渲染界面）。
	Language   string `json:"language"`
	ConfigPath string `json:"configPath"`
	DataDir    string `json:"dataDir"`
	LogPath    string `json:"logPath"`
	AccountDir string `json:"accountDir"`
	Version    string `json:"version"`
	Platform   string `json:"platform"`
}

// StateView 是界面一次拉取的全部状态。
type StateView struct {
	Running    bool           `json:"running"`
	Listen     string         `json:"listen"`
	BaseURL    string         `json:"baseUrl"`
	OpenAIURL  string         `json:"openaiUrl"`
	RequireKey bool           `json:"requireKey"`
	APIKeys    []string       `json:"apiKeys"`
	Providers  []ProviderView `json:"providers"`
	Accounts   []AccountView  `json:"accounts"`
	Models     []ModelView    `json:"models"`
	// VirtualModels 是用户自定义的虚拟模型组。
	VirtualModels []VirtualModelView `json:"virtualModels"`
	Settings      SettingsView       `json:"settings"`
	Logs          []string           `json:"logs"`
	Login         LoginView          `json:"login"`
}

// LoginView 是登录流程状态。
type LoginView struct {
	Active    bool   `json:"active"`
	Provider  string `json:"provider"`
	URL       string `json:"url"`
	EmbedURL  string `json:"embedUrl"`
	State     string `json:"state"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	ExpiresAt string `json:"expiresAt"`
	AccountID string `json:"accountId"`
	// NeedKey 表示这次登录等待用户在界面上粘贴密钥（OpenCode 的 API Key 制登录）。
	NeedKey bool `json:"needKey"`
}

// UpdateView 是在线更新的检查结果与安装进度。
type UpdateView struct {
	Phase     string `json:"phase"` // idle / checking / available / none / downloading / installing / done / failed
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"hasUpdate"`
	Page      string `json:"page"`      // Release 页面
	AssetName string `json:"assetName"` // 将下载的安装包
	Notes     string `json:"notes"`     // Release 说明
	Progress  int    `json:"progress"`  // 0-100（下载时）
	Message   string `json:"message"`   // 给用户看的提示
}

// ProfileInput 是保存设置时的上游开关。
type ProfileInput struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Proxy   string `json:"proxy"`
}

// SettingsInput 是保存设置的入参。
type SettingsInput struct {
	Listen         string         `json:"listen"`
	ModelSyncHours int            `json:"modelSyncHours"`
	CORS           bool           `json:"cors"`
	Sanitize       bool           `json:"sanitize"`
	MaxThinking    bool           `json:"maxThinking"`
	AutoStart      bool           `json:"autoStart"`
	LaunchAtLogin  bool           `json:"launchAtLogin"`
	Profiles       []ProfileInput `json:"profiles"`
}

// -----------------------------------------------------------------------------
// App
// -----------------------------------------------------------------------------

type loginRun struct {
	provider     string
	session      *upstream.LoginSession
	cancel       context.CancelFunc
	status       string
	message      string
	accountID    string
	parentOrigin string
}

// App 是绑定给前端的应用对象。
type App struct {
	dataDir string

	ctx   context.Context
	ctxMu sync.RWMutex
	log   *applog.Logger
	store *store.Store
	srv   *proxysrv.Server
	tray  *tray.Tray

	cfgMu sync.RWMutex
	cfg   *config.Config

	quitting    atomic.Bool
	headless    atomic.Bool
	trayHint    atomic.Bool
	pendingShow atomic.Bool // 窗口就绪前收到的显示请求，startup 末尾补显
	unsubLog    func()
	loginMu     sync.Mutex
	login       *loginRun

	updMu sync.Mutex
	upd   *update.Release // 最近一次检查到的可用更新
}

// NewApp 初始化应用。
func NewApp() *App {
	dataDir := config.DefaultDataDir()
	if custom := strings.TrimSpace(os.Getenv("RAPIDPROXY_DATA_DIR")); custom != "" {
		dataDir = custom
	}

	logger := applog.New(dataDir, 800)

	cfgPath := config.DefaultPath(dataDir)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Errorf("加载配置失败，使用默认配置: %v", err)
		// 带上路径，否则界面上的「配置文件路径」会是空的，保存时也会写到别处。
		cfg = config.DefaultAt(cfgPath)
	}
	// 界面语言尽早定下来：托盘菜单、更新提示等 Go 侧文案在窗口出现前后都会用到。
	locale.Set(locale.FromPref(cfg.Language))

	credStore, err := store.New(dataDir)
	if err != nil {
		logger.Errorf("初始化凭据仓库失败: %v", err)
	}

	app := &App{
		dataDir: dataDir,
		log:     logger,
		store:   credStore,
		cfg:     cfg,
	}
	app.srv = proxysrv.New(cfg, credStore, logger)
	return app
}

// startup 在窗口创建后调用。
func (a *App) startup(ctx context.Context) {
	a.ctxMu.Lock()
	a.ctx = ctx
	a.ctxMu.Unlock()

	a.log.Infof("RapidProxy %s 已启动（数据目录 %s）", Version, a.dataDir)
	a.unsubLog = a.log.Subscribe(func(line string) {
		a.emit(eventLog, line)
	})

	// 按配置同步系统自启动条目：既是首次启用的默认值落位，
	// 也是程序升级（安装路径变化）后自动修正自启动指向。
	a.applyLaunchAtLogin(a.cfg.LaunchAtLoginEnabled())
	// 在界面真正显示出来之前收敛窗口尺寸（见 fitWindow 的注释）。
	a.fitWindow(ctx)

	if a.cfg.AutoStartEnabled() {
		if err := a.StartService(); err != nil {
			a.log.Errorf("自动启动失败: %v", err)
		}
	}
	a.pushState()

	// 启动期间收到的激活请求（第二份程序抢在窗口就绪前启动了本实例）
	// 在这里补上：窗口几何已在 fitWindow 收敛完毕，直接显示即可。
	if a.pendingShow.CompareAndSwap(true, false) {
		wailsrt.WindowShow(ctx)
	}

	// 后台静默检查一次更新（失败不打扰用户，结果通过 update 事件推送）
	go func(ctx context.Context) {
		select {
		case <-time.After(4 * time.Second):
		case <-ctx.Done():
			// 程序已在 4 秒窗口内退出/关闭：放弃检查，不再发起网络请求。
			return
		}
		if a.quitting.Load() {
			return
		}
		client := &http.Client{Timeout: 20 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if rel, has, err := update.Check(ctx, client, "", update.DefaultRepo, Version); err == nil && has {
			a.updMu.Lock()
			a.upd = rel
			a.updMu.Unlock()
			a.emitUpdate(UpdateView{
				Phase: "available", Current: Version, Latest: rel.Version,
				HasUpdate: true, Page: rel.Page, AssetName: rel.Asset.Name, Notes: rel.Notes,
				Message: locale.T("upd.availableSettings", rel.Version),
			})
		}
	}(ctx)
}

func (a *App) context() context.Context {
	a.ctxMu.RLock()
	defer a.ctxMu.RUnlock()
	return a.ctx
}

func (a *App) emit(name string, payload any) {
	ctx := a.context()
	if ctx == nil {
		return
	}
	wailsrt.EventsEmit(ctx, name, payload)
}

func (a *App) pushState() { a.emit(eventState, a.GetState()) }

func (a *App) pushLogin() { a.emit(eventLogin, a.loginState()) }

func (a *App) settings() *config.Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

// startTray 初始化托盘图标（必须在 wails.Run 之前调用）。
func (a *App) startTray() {
	a.tray = tray.Start(tray.Callbacks{
		OnShow:        a.ShowWindow,
		OnResetWindow: a.ResetWindow,
		OnToggle:      a.ToggleService,
		OnCopyURL: func() {
			ctx := a.context()
			if ctx == nil {
				return
			}
			url := a.settings().OpenAIBaseURL()
			wailsrt.ClipboardSetText(ctx, url)
			a.log.Infof("已复制接口地址: %s", url)
		},
		OnQuit: a.QuitApp,
	})
	a.syncTray()
}

func (a *App) syncTray() {
	if a.tray == nil {
		return
	}
	running := false
	addr := a.settings().Listen
	if a.srv != nil {
		running = a.srv.Running()
		addr = a.srv.Addr()
	}
	a.tray.SetState(running, addr)
}

// beforeClose 拦截窗口关闭：默认只最小化到托盘，退出只能通过托盘菜单。
func (a *App) beforeClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	wailsrt.WindowHide(ctx)
	a.log.Infof("窗口已最小化到系统托盘（右键托盘图标可退出）")
	go a.hintTrayOnce(ctx)
	return true
}

func (a *App) hintTrayOnce(ctx context.Context) {
	if !a.trayHint.CompareAndSwap(false, true) {
		return
	}
	_, _ = wailsrt.MessageDialog(ctx, wailsrt.MessageDialogOptions{
		Type:    wailsrt.InfoDialog,
		Title:   locale.T("dlg.minimized.title"),
		Message: locale.T("dlg.minimized.msg"),
		Buttons: []string{locale.T("dlg.minimized.ok")},
	})
}

// -----------------------------------------------------------------------------
// 窗口尺寸自适应
// -----------------------------------------------------------------------------

// fitWindow 在启动时把主窗口收敛到当前屏幕的可见范围内。
//
// 为什么需要：Wails 窗口的宽高是「逻辑像素」，创建窗口时会按屏幕 DPI 换算成
// 物理像素。系统缩放 150% 时，逻辑高度 740 会被放大成 1110 物理像素，在
// 1920x1080 的屏幕上就超出了屏幕高度，窗口上下都会跑到屏幕外看不到。所以在
// 窗口显示之前按屏幕（逻辑）尺寸重新算一遍尺寸并居中。
func (a *App) fitWindow(ctx context.Context) {
	preferred := winfit.Size{Width: windowDefaultWidth, Height: windowDefaultHeight}
	floorMin := winfit.Size{Width: windowMinWidth, Height: windowMinHeight}

	screen, physical, ok := currentScreenSize(ctx)
	if !ok {
		a.log.Warnf("未能读取屏幕尺寸，窗口保持默认尺寸 %dx%d", preferred.Width, preferred.Height)
		wailsrt.WindowSetMinSize(ctx, floorMin.Width, floorMin.Height)
		wailsrt.WindowSetSize(ctx, preferred.Width, preferred.Height)
		wailsrt.WindowCenter(ctx)
		return
	}

	size, min := winfit.Fit(preferred, floorMin, screen)

	// 先设最小尺寸再设实际尺寸：Wails 在设置尺寸时会用最小尺寸做钳制，
	// 屏幕比默认最小尺寸还小时必须先放宽下限，否则窗口压不下去。
	wailsrt.WindowSetMinSize(ctx, min.Width, min.Height)
	wailsrt.WindowSetSize(ctx, size.Width, size.Height)
	wailsrt.WindowCenter(ctx)

	desc := screenDesc(screen, physical)
	if size != preferred {
		a.log.Infof("屏幕 %s，窗口尺寸收敛为 %dx%d（最小 %dx%d）",
			desc, size.Width, size.Height, min.Width, min.Height)
		return
	}
	a.log.Infof("屏幕 %s，窗口尺寸 %dx%d", desc, size.Width, size.Height)
}

// currentScreenSize 返回主窗口所在屏幕的逻辑尺寸（含任务栏区域）。
//
// 优先取「窗口所在屏幕」，其次主屏，最后任意一块可用屏幕。取不到时返回 false，
// 调用方保持默认尺寸。第二个返回值是屏幕的物理尺寸，仅用于日志。
func currentScreenSize(ctx context.Context) (winfit.Size, winfit.Size, bool) {
	empty := winfit.Size{}
	screens, err := wailsrt.ScreenGetAll(ctx)
	if err != nil || len(screens) == 0 {
		return empty, empty, false
	}

	usable := func(i int) bool {
		return screens[i].Size.Width > 0 && screens[i].Size.Height > 0
	}

	pick := -1
	for i := range screens {
		if usable(i) && screens[i].IsCurrent {
			pick = i
			break
		}
	}
	if pick < 0 {
		for i := range screens {
			if usable(i) && screens[i].IsPrimary {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		for i := range screens {
			if usable(i) {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		return empty, empty, false
	}

	s := screens[pick]
	return winfit.Size{Width: s.Size.Width, Height: s.Size.Height},
		winfit.Size{Width: s.PhysicalSize.Width, Height: s.PhysicalSize.Height},
		true
}

// screenDesc 生成形如「2560x1440（物理 3840x2160，缩放 150%）」的描述，仅用于日志。
func screenDesc(logical, physical winfit.Size) string {
	desc := fmt.Sprintf("%dx%d", logical.Width, logical.Height)
	if physical.Width <= 0 || physical.Height <= 0 || logical.Width <= 0 {
		return desc
	}
	scale := float64(physical.Width) / float64(logical.Width) * 100
	if int(scale+0.5) == 100 {
		return desc
	}
	return fmt.Sprintf("%s（物理 %dx%d，缩放 %d%%）", desc, physical.Width, physical.Height, int(scale+0.5))
}

// ResetWindow 把窗口重新收敛到当前屏幕并居中。
//
// 用途：窗口被拖到已拔掉的显示器上、或系统改过缩放比例后跑到屏幕外时，
// 从托盘菜单一键恢复。前端也可以直接调用。
func (a *App) ResetWindow() {
	ctx := a.context()
	if ctx == nil {
		return
	}
	go func() {
		a.fitWindow(ctx)
		a.ShowWindow()
	}()
}

// -----------------------------------------------------------------------------
// 窗口 / 生命周期（供前端调用）
// -----------------------------------------------------------------------------

// ShowWindow 显示主窗口。
func (a *App) ShowWindow() {
	ctx := a.context()
	if ctx == nil {
		return
	}
	wailsrt.WindowUnminimise(ctx)
	wailsrt.WindowShow(ctx)
	a.pendingShow.Store(false)
}

// HandleSecondInstance 处理后续实例的激活请求：把主窗口带到前台。
//
// 第二份程序启动时发现已有实例，会通过 singleton 包通知本实例。若通知到达
// 时窗口尚未创建完（ctx 为空），请求会被记住，startup 末尾再补一次显示。
func (a *App) HandleSecondInstance() {
	a.pendingShow.Store(true)
	a.ShowWindow()
}

// HideWindow 隐藏主窗口到托盘。
func (a *App) HideWindow() {
	ctx := a.context()
	if ctx == nil {
		return
	}
	wailsrt.WindowHide(ctx)
}

// QuitApp 完全退出程序。
func (a *App) QuitApp() {
	a.quitting.Store(true)
	a.log.Infof("正在退出 RapidProxy")
	if err := a.StopService(); err != nil {
		a.log.Warnf("停止服务时出错: %v", err)
	}
	a.cancelLogin()
	a.log.Close()
	if a.tray != nil {
		a.tray.Quit()
	}
	if a.headless.Load() {
		// 后台模式下没有 Wails 事件循环，直接结束进程。
		os.Exit(0)
	}
	if ctx := a.context(); ctx != nil {
		wailsrt.Quit(ctx)
		return
	}
	os.Exit(0)
}

// runHeadless 在界面不可用时退化为后台服务，仅保留托盘与 OpenAI 接口。
//
// 用信号而非裸 select{} 等待：收到 SIGINT/SIGTERM（终端 Ctrl+C、服务管理器停止）
// 时先优雅停止服务（落盘 traffic.json、关闭统计协程）再退出，避免进程被强杀导致
// 最近 15 秒的统计计数丢失。
func (a *App) runHeadless() {
	a.headless.Store(true)
	if err := a.StartService(); err != nil {
		a.log.Errorf("后台模式启动代理服务失败: %v", err)
		os.Exit(1)
	}
	a.log.Infof("已进入后台模式（无界面）：接口地址 %s，可从托盘图标退出", a.settings().OpenAIBaseURL())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	a.log.Infof("收到退出信号，正在停止后台服务")
	if err := a.StopService(); err != nil {
		a.log.Warnf("停止服务时出错: %v", err)
	}
	a.log.Close()
	os.Exit(0)
}

// -----------------------------------------------------------------------------
// 状态
// -----------------------------------------------------------------------------

// GetState 返回界面所需的完整状态。
func (a *App) GetState() StateView {
	cfg := a.settings()
	view := StateView{
		Running:    a.srv.Running(),
		Listen:     cfg.Listen,
		BaseURL:    cfg.BaseURL(),
		OpenAIURL:  cfg.OpenAIBaseURL(),
		RequireKey: cfg.RequiresAPIKey(),
		APIKeys:    append([]string(nil), cfg.APIKeys...),
		Logs:       a.log.Tail(200),
		Login:      a.loginState(),
	}
	if addr := a.srv.Addr(); addr != "" {
		view.Listen = addr
	}
	// 凭据目录只读一次，供上游列表与账号列表共用（每次 GetState 都会被调用）。
	creds := []*upstream.Credential(nil)
	if a.store != nil {
		creds, _ = a.store.List("")
	}
	view.Providers = a.providerViews(creds)
	view.Accounts = a.accountViews(creds)
	// 虚拟组只算一次，模型列表直接复用：两边都要「可用成员数」，
	// 各算一遍会把凭据扫描与冷却查询翻倍。
	view.VirtualModels = a.virtualModelViews()
	view.Models = a.modelViews(view.VirtualModels)
	view.Settings = SettingsView{
		Listen:         cfg.Listen,
		ModelSyncHours: cfg.ModelSyncHours,
		CORS:           cfg.CORSEnabled(),
		Sanitize:       cfg.SanitizeEnabled(),
		MaxThinking:    cfg.MaxThinkingEnabled(),
		AutoStart:      cfg.AutoStartEnabled(),
		LaunchAtLogin:  cfg.LaunchAtLoginEnabled(),
		Language:       cfg.Language,
		ConfigPath:     cfg.Path(),
		DataDir:        a.dataDir,
		LogPath:        a.log.FilePath(),
		AccountDir:     "",
		Version:        Version,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
	}
	if a.store != nil {
		view.Settings.AccountDir = a.store.Root()
	}
	return view
}

// GetAccessInfo 返回给用户复制到其它软件的接入信息。
func (a *App) GetAccessInfo() AccessView {
	cfg := a.settings()
	models := a.srv.ModelIDs()
	requireKey := cfg.RequiresAPIKey()
	key := "<你的 API Key>"
	if !requireKey {
		key = "sk-any"
	} else if len(cfg.APIKeys) > 0 {
		key = cfg.APIKeys[0]
	}
	sample := fmt.Sprintf(
		"curl %s/chat/completions \\\n  -H \"Content-Type: application/json\" \\\n  -H \"Authorization: Bearer %s\" \\\n  -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}],\"stream\":true}'",
		cfg.OpenAIBaseURL(), key, firstModel(models),
	)
	return AccessView{
		BaseURL:    cfg.BaseURL(),
		OpenAIURL:  cfg.OpenAIBaseURL(),
		APIKeys:    append([]string(nil), cfg.APIKeys...),
		RequireKey: requireKey,
		Models:     models,
		Running:    a.srv.Running(),
		Sample:     sample,
	}
}

func firstModel(models []string) string {
	if len(models) == 0 {
		return "gpt-5.4"
	}
	return models[0]
}

func (a *App) providerViews(creds []*upstream.Credential) []ProviderView {
	cfg := a.settings()
	counts := map[string]int{}
	for _, c := range creds {
		counts[c.Provider]++
	}
	out := make([]ProviderView, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		out = append(out, ProviderView{
			ID:           p.ID,
			Name:         p.Name,
			Enabled:      p.Enabled,
			BaseURL:      p.BaseURL,
			Origin:       p.Origin,
			Proxy:        p.Proxy,
			AccountCount: counts[p.ID],
			ModelCount:   len(a.srv.ModelsFor(p.ID)),
			NeedsProxy:   p.ProxyRecommended(),
		})
	}
	return out
}

func (a *App) accountViews(creds []*upstream.Credential) []AccountView {
	cfg := a.settings()
	names := map[string]string{}
	for _, p := range cfg.Profiles {
		names[p.ID] = p.Name
	}
	out := make([]AccountView, 0, len(creds))
	for _, c := range creds {
		expiresAt := c.ExpiresAtTime()
		view := AccountView{
			ID:           c.ID,
			Provider:     c.Provider,
			ProviderName: names[c.Provider],
			Nickname:     c.DisplayName(),
			UID:          c.Account.UID,
			EnterpriseID: c.Account.EnterpriseID,
			Expired:      c.Expired(),
		}
		if view.ProviderName == "" {
			view.ProviderName = c.Provider
		}
		view.Weight = c.EffectiveWeight()
		view.Paused = c.Paused
		if left, reason := a.srv.CooldownInfo(c.ID); left > 0 {
			view.Cooldown = int(left.Seconds())
			view.CooldownReason = reason
		}
		if !expiresAt.IsZero() {
			view.ExpiresAt = expiresAt.Format("2006-01-02 15:04")
			view.ExpiresIn = humanDuration(time.Until(expiresAt))
		}
		if !c.CreatedAt.IsZero() {
			view.CreatedAt = c.CreatedAt.Format("2006-01-02 15:04")
		}
		out = append(out, view)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

// modelViews 构建模型列表。virtuals 是已算好的虚拟组视图（见 GetState），
// 虚拟条目的可用成员数直接取用，避免重复扫描凭据。
func (a *App) modelViews(virtuals []VirtualModelView) []ModelView {
	cfg := a.settings()
	names := map[string]string{}
	for _, p := range cfg.Profiles {
		names[p.ID] = p.Name
	}
	byName := make(map[string]VirtualModelView, len(virtuals))
	for _, vm := range virtuals {
		byName[vm.Name] = vm
	}
	models := a.srv.AllModels()
	// 账号统计按上游缓存：同一个版本的模型共用一组账号。
	stats := map[string]struct{ avail, total int }{}
	out := make([]ModelView, 0, len(models))
	for _, m := range models {
		// 虚拟组条目没有真实上游账号，可用宽度是「有几个成员能接」。
		if m.Provider == "virtual" {
			vm := byName[m.ID]
			out = append(out, ModelView{
				ID:            m.ID,
				Name:          m.DisplayName,
				Provider:      a.virtualProviderLabel(),
				ProviderID:    "virtual",
				Context:       m.ContextLength,
				MaxOut:        m.MaxOutputTokens,
				Images:        m.SupportsImages,
				Accounts:      vm.Available,
				TotalAccounts: len(vm.Members),
				Virtual:       true,
			})
			continue
		}
		st, ok := stats[m.Provider]
		if !ok {
			st.avail, st.total = a.srv.AccountStats(m.Provider)
			stats[m.Provider] = st
		}
		out = append(out, ModelView{
			ID:            m.ID,
			Name:          m.DisplayName,
			Provider:      names[m.Provider],
			ProviderID:    m.Provider,
			Context:       m.ContextLength,
			MaxOut:        m.MaxOutputTokens,
			Images:        m.SupportsImages,
			Accounts:      st.avail,
			TotalAccounts: st.total,
		})
	}
	return out
}

// virtualProviderLabel 返回虚拟组在界面上显示的上游名。
func (a *App) virtualProviderLabel() string {
	if locale.Current() == locale.En {
		return "Virtual group"
	}
	return "虚拟组"
}

// virtualModelViews 构建虚拟组列表（含每个成员的实时可用状态）。
func (a *App) virtualModelViews() []VirtualModelView {
	cfg := a.settings()
	names := map[string]string{}
	enabled := make(map[string]bool, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		names[p.ID] = p.Name
		enabled[p.ID] = p.Enabled
	}
	// 账号可用数按上游缓存：一个组里常有多个成员来自同一上游，
	// 每个成员各查一次会重复扫凭据（虽有 2 秒缓存，但没必要）。
	type avail struct{ n int }
	avails := map[string]avail{}

	out := make([]VirtualModelView, 0, len(cfg.VirtualModels))
	for _, vm := range cfg.VirtualModels {
		view := VirtualModelView{Name: vm.Name, Enabled: vm.Enabled}
		view.Members = make([]VirtualMemberView, 0, len(vm.Members))
		for _, m := range vm.Members {
			mv := VirtualMemberView{
				Provider:     m.Provider,
				ProviderName: names[m.Provider],
				Model:        m.Model,
				Weight:       m.EffectiveWeight(),
			}
			if mv.ProviderName == "" {
				mv.ProviderName = m.Provider
			}
			av, ok := avails[m.Provider]
			if !ok {
				av.n, _ = a.srv.AccountStats(m.Provider)
				avails[m.Provider] = av
			}
			mv.Accounts = av.n
			mv.Available = enabled[m.Provider] && av.n > 0
			if mv.Available {
				view.Available++
			}
			view.Members = append(view.Members, mv)
		}
		out = append(out, view)
	}
	return out
}

// SaveVirtualModel 新增或修改一个虚拟模型组。
//
// input.OriginalName 非空时表示修改已有组（允许改名）；改名时历史流量
// 一并迁移，否则用户会看到新组从零开始、旧数据又选不到。
func (a *App) SaveVirtualModel(input VirtualModelInput) error {
	cfg := a.settings().Clone()

	vm := config.VirtualModel{Name: strings.TrimSpace(input.Name), Enabled: input.Enabled}
	for _, m := range input.Members {
		vm.Members = append(vm.Members, config.VirtualMember{
			Provider: strings.TrimSpace(m.Provider),
			Model:    strings.TrimSpace(m.Model),
			Weight:   m.Weight,
		})
	}

	original := strings.TrimSpace(input.OriginalName)
	if err := cfg.UpsertVirtualModel(vm, original); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		a.log.Errorf("保存虚拟模型失败: %v", err)
		return err
	}

	// 顺序很重要：先把旧名的历史流量搬到新名下，再让新配置生效。
	//
	// 反过来的话会有一个窗口：新配置已经对外可见（新名字开始接流量），
	// 而 RenameVirtual 随后才把旧名整段删掉——若新名恰好等于某个刚被
	// 删除的旧组名，或改名瞬间有请求落到新键上，这段新数据会被
	// RenameVirtual 的 delete 一并抹掉。先迁移再切配置，窗口就不存在。
	renamed := original != "" && original != vm.Name
	if renamed {
		if st := a.srv.Stats(); st != nil {
			st.RenameVirtual(original, vm.Name)
		}
		// 两个名字的状态都要清：旧名的偏移对新名没有意义，而新名若曾经
		// 是个被删掉的组，表里可能还留着它的历史偏移——不清会让新组
		// 一开始的分配明显偏向某个成员。
		a.srv.ClearVirtualState(original)
		a.srv.ClearVirtualState(vm.Name)
	}

	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)

	if renamed {
		a.log.Infof("虚拟模型 %s 已改名为 %s", original, vm.Name)
	} else {
		// 成员或权重可能变了：清掉分摊状态，避免旧偏移让分配短期倾斜。
		a.srv.ClearVirtualState(vm.Name)
		a.log.Infof("虚拟模型 %s 已保存（%d 个成员）", vm.Name, len(vm.Members))
	}
	a.pushState()
	return nil
}

// DeleteVirtualModel 删除一个虚拟模型组及其历史流量。
func (a *App) DeleteVirtualModel(name string) error {
	name = strings.TrimSpace(name)
	cfg := a.settings().Clone()
	if !cfg.RemoveVirtualModel(name) {
		return fmt.Errorf("虚拟模型 %s 不存在", name)
	}
	if err := cfg.Save(); err != nil {
		a.log.Errorf("删除虚拟模型失败: %v", err)
		return err
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)
	a.srv.ClearVirtualState(name)
	// 一并清掉历史流量：否则「删掉再建同名组」会继承上一个组的数据。
	if st := a.srv.Stats(); st != nil {
		st.DropVirtual(name)
	}
	a.log.Infof("虚拟模型 %s 已删除", name)
	a.pushState()
	return nil
}

// GetLogs 返回最近日志。
func (a *App) GetLogs() []string { return a.log.Tail(300) }

// ClearLogs 清空界面上的日志。
func (a *App) ClearLogs() {
	a.log.Clear()
	a.pushState()
}

// -----------------------------------------------------------------------------
// 流量统计
// -----------------------------------------------------------------------------

// GetTrafficStats 返回按组（provider）按模型（model）的流量统计。
//
// period 取值 day / week / month（其它值按 day 处理）。model 为空表示汇总该组
// 全部模型。返回本期汇总与趋势序列（日视图为 24 个小时点，周/月为按天）。
func (a *App) GetTrafficStats(provider, model, period string) (stats.Result, error) {
	if a.srv == nil || a.srv.Stats() == nil {
		return stats.Result{}, nil
	}
	switch period {
	case "week", "month":
	default:
		period = "day"
	}
	return a.srv.Stats().Query(provider, model, period, time.Now()), nil
}

// GetTrafficScopes 返回「已有流量数据」的 (组, 模型) 组合，供界面默认选中。
func (a *App) GetTrafficScopes() ([]stats.Scope, error) {
	if a.srv == nil || a.srv.Stats() == nil {
		return []stats.Scope{}, nil
	}
	return a.srv.Stats().Scopes(), nil
}

// -----------------------------------------------------------------------------
// 服务控制
// -----------------------------------------------------------------------------

// StartService 启动代理服务。
func (a *App) StartService() error {
	if err := a.srv.Start(); err != nil {
		a.log.Errorf("启动服务失败: %v", err)
		a.syncTray()
		a.pushState()
		return err
	}
	a.syncTray()
	a.pushState()
	return nil
}

// StopService 停止代理服务。
func (a *App) StopService() error {
	err := a.srv.Stop()
	a.syncTray()
	a.pushState()
	return err
}

// ToggleService 切换服务启停，返回切换后的状态。
func (a *App) ToggleService() bool {
	if a.srv.Running() {
		_ = a.StopService()
	} else {
		_ = a.StartService()
	}
	return a.srv.Running()
}

// RestartService 重启服务（监听地址变更后使用）。
func (a *App) RestartService() error {
	if err := a.StopService(); err != nil {
		a.log.Warnf("停止服务失败: %v", err)
	}
	return a.StartService()
}

// RefreshModels 立即从上游同步模型列表。
func (a *App) RefreshModels() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a.srv.RefreshModels(ctx)
	a.pushState()
	return nil
}

// -----------------------------------------------------------------------------
// API Key
// -----------------------------------------------------------------------------

// GenerateAPIKey 生成并保存一个新的 API Key。
func (a *App) GenerateAPIKey() (string, error) {
	key := config.GenerateAPIKey()
	if err := a.AddAPIKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// AddAPIKey 添加一个自定义 API Key。
func (a *App) AddAPIKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New(locale.T("key.empty"))
	}
	cfg := a.settings().Clone()
	if !cfg.AddAPIKey(key) {
		return errors.New(locale.T("key.duplicate"))
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)
	a.log.Infof("已设置 API Key（%s）", maskKey(key))
	a.pushState()
	return nil
}

// RemoveAPIKey 删除一个 API Key。
func (a *App) RemoveAPIKey(key string) error {
	cfg := a.settings().Clone()
	if !cfg.RemoveAPIKey(key) {
		return errors.New(locale.T("key.notFound"))
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)
	a.log.Infof("已删除 API Key（%s）", maskKey(key))
	a.pushState()
	return nil
}

func maskKey(key string) string {
	// 注意：key 由用户在界面上输入，长度不可信，短 key 不能越界切片。
	switch {
	case len(key) <= 2:
		return "****"
	case len(key) <= 12:
		return key[:2] + "****"
	default:
		return key[:10] + "****" + key[len(key)-4:]
	}
}

// -----------------------------------------------------------------------------
// 账号与登录
// -----------------------------------------------------------------------------

// StartLogin 发起某个上游的登录流程。parentOrigin 是前端页面自己的 origin
// （如 http://wails.localhost），用于构造应用内 iframe 授权链接；传空则只返回外部浏览器链接。
func (a *App) StartLogin(provider, parentOrigin string) (LoginView, error) {
	cfg := a.settings()
	profile, ok := cfg.ProfileByID(provider)
	if !ok {
		return LoginView{}, fmt.Errorf("%s", locale.T("login.unknownProvider", provider))
	}
	client, err := a.srv.ClientFor(provider)
	if err != nil {
		return LoginView{}, err
	}

	a.cancelLogin()

	// 10 分钟覆盖各协议族的授权窗口（WorkBuddy 5 分钟、LobsterAI/Trae
	// 10 分钟、Qoder 5 分钟）；WorkBuddy 的会话到期由 Poll 返回
	// ErrLoginTimeout 提前结束，不会被这里拖住。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	session, err := client.StartLogin(ctx)
	if err != nil {
		cancel()
		a.log.Errorf("[%s] 发起登录失败: %v", profile.Name, err)
		return LoginView{}, err
	}

	run := &loginRun{
		provider:     provider,
		session:      session,
		cancel:       cancel,
		status:       "pending",
		parentOrigin: strings.TrimSpace(parentOrigin),
	}
	switch {
	case session.SkipBrowser:
		// 本机已找到凭据（如 OpenCode CLI 的 auth.json）：不打开浏览器，
		// 流程停在等待确认的状态——用户提交空密钥即用它登录，或直接在
		// 下方粘贴新密钥覆盖（NeedKey 亦为真），想换钥匙不必先删本机文件。
		run.message = locale.T("login.importing", session.LocalKeyLabel)
	case session.NeedKey:
		// OpenCode 是 API Key 制登录：本机没有现成密钥时，打开官方密钥页，
		// 等用户在界面上粘贴（SubmitLoginKey 收口）。
		run.message = locale.T("login.pasteKey")
	default:
		run.message = locale.T("login.browserOpened")
	}
	a.loginMu.Lock()
	a.login = run
	a.loginMu.Unlock()

	// 内嵌 iframe 授权页会被上游安全策略（X-Frame-Options / CSP frame-ancestors）
	// 拦截，无法在应用内显示；统一改用系统浏览器完成登录。
	// 登录结果仍由后端轮询自动感知，不依赖浏览器回调。
	if !session.SkipBrowser {
		if ctx := a.context(); ctx != nil {
			wailsrt.BrowserOpenURL(ctx, session.AuthURL)
			a.log.Infof("[%s] 已在系统浏览器打开登录页", profile.Name)
		} else {
			a.log.Warnf("[%s] 无法自动打开浏览器，请复制登录链接手动访问", profile.Name)
		}
	}
	a.pushLogin()

	go a.pollLogin(ctx, run)
	return a.loginState(), nil
}

func (a *App) pollLogin(ctx context.Context, run *loginRun) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.finishLogin(run, "failed", locale.T("login.timeout"), "")
			return
		case <-ticker.C:
			cred, err := run.session.Poll(ctx)
			if err != nil {
				if errors.Is(err, upstream.ErrLoginTimeout) {
					a.finishLogin(run, "failed", locale.T("login.expired"), "")
					return
				}
				a.finishLogin(run, "failed", err.Error(), "")
				return
			}
			if cred == nil {
				continue
			}
			// 凭据到手时流程可能已经不在 pending：用户改用了粘贴密钥
			// （SubmitLoginKey 已置终态并取消轮询），或取消了登录、或发起
			// 了新流程。这时迟到的凭据不该落库——否则同一次登录会存出两个
			// 账号（本机密钥 + 用户粘贴的密钥），流量被悄悄分走。
			a.loginMu.Lock()
			stillPending := a.login == run && run.status == "pending"
			a.loginMu.Unlock()
			if !stillPending {
				return
			}
			// 凭据 ID 是 provider+uid，重新登录同一账号会算出同一个 ID。
			// 用保留本地字段的写法，否则用户在界面上配的权重和「暂停」
			// 开关会在每次重新登录后被悄悄清成默认值。
			if a.store == nil {
				a.finishLogin(run, "failed", locale.T("login.saveFailed", "凭据仓库不可用"), "")
				return
			}
			if err := a.store.SavePreservingLocalFields(cred); err != nil {
				a.finishLogin(run, "failed", locale.T("login.saveFailed", err.Error()), "")
				return
			}
			// 登录是用户主动发起的授权动作，成功即代表要使用该上游。模型页与
			// /v1/models 只看「已启用 ∧ 已登录」的上游，只登录不启用时凭据存了
			// 却什么都不会发生——升级到多上游后最容易踩的坑，这里自动拉起开关。
			a.enableProviderIfDisabled(cred.Provider)
			// 凭据清单有缓存：不失效的话，紧接着的 RefreshModels 还看不这个新账号，
			// 会把这一版当成「未登录」而清空模型列表。
			a.srv.InvalidateAccounts()
			a.log.Infof("登录成功: %s（%s）", cred.DisplayName(), cred.ID)
			a.finishLogin(run, "success", locale.T("login.success"), cred.ID)
			go func() {
				refreshCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				a.srv.RefreshModels(refreshCtx)
				a.pushState()
			}()
			return
		}
	}
}

// enableProviderIfDisabled 把仍处于关闭状态的上游自动启用并落盘。
//
// 升级到多上游后，新预设默认是关的：只登录不启用时，凭据已经存盘，
// 但模型页、/v1/models 与转发都只认「已启用 ∧ 已登录」的上游，
// 用户看到的现象就是「登录成功了却什么都没有」。登录成功本身已经
// 表达了使用意图，这里顺手把开关打开；已启用时是空操作。
func (a *App) enableProviderIfDisabled(providerID string) {
	cfg := a.settings().Clone()
	profile, ok := cfg.ProfileByID(providerID)
	if !ok || profile.Enabled {
		return
	}
	profile.Enabled = true
	cfg.SetProfile(profile)
	cfg.Normalize()
	if err := cfg.Save(); err != nil {
		a.log.Errorf("自动启用上游 %s 失败: %v", profile.Name, err)
		return
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)
	a.log.Infof("上游 %s 已随登录自动启用", profile.Name)
}

// finishLogin 结束登录流程。终态（success / failed）具有粘滞性：先到达的
// 终态不会被后到的覆盖。密钥制登录（OpenCode）由 SubmitLoginKey 直接置
// success 并取消轮询上下文，轮询 goroutine 随后会在 <-ctx.Done() 分支醒来，
// 没有这道守卫就会把「登录成功」改写成「登录超时或已取消」。
func (a *App) finishLogin(run *loginRun, status, message, accountID string) {
	a.loginMu.Lock()
	applied := a.finishLoginLocked(run, status, message, accountID)
	a.loginMu.Unlock()
	if !applied {
		// 终态粘滞：后来的 success/failed 不再改状态，也不必记一笔流程结束。
		return
	}
	a.log.Infof("登录流程结束: %s", message)
	a.pushLogin()
	a.pushState()
}

// finishLoginLocked 是 finishLogin 的持锁版本，供已拿到 loginMu 的调用方
// 使用。返回 false 表示这次终态被粘滞守卫挡下（状态未变）。
func (a *App) finishLoginLocked(run *loginRun, status, message, accountID string) bool {
	if a.login != run || run.status != "pending" {
		return false
	}
	run.status = status
	run.message = message
	run.accountID = accountID
	if status != "pending" {
		run.cancel()
	}
	return true
}

// pendingImportedKey 返回当前等待确认的本地导入密钥（没有则返回空串）。
//
// 只有「同一上游、仍在 pending、且带着待确认导入密钥」的会话才算数：
// 用户可能在流程结束后才提交，那时没有可确认的本机密钥。
func (a *App) pendingImportedKey(provider string) string {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	run := a.login
	if run == nil || run.provider != provider || run.status != "pending" || run.session == nil {
		return ""
	}
	return strings.TrimSpace(run.session.LocalKey)
}

// SubmitLoginKey 提交用户粘贴的上游密钥（目前用于 OpenCode：Zen 是 API Key
// 制，没有浏览器授权回调）。provider 是上游 ID，key 是用户粘贴的原文，
//
//	tolerates 常见的粘贴噪声（引号、JSON、Bearer 前缀）。
//
// key 为空表示「确认使用本机导入的密钥」：本机导入不再自动落库，用户点
// 登录后流程停在等待确认的状态，此时提交空密钥就是用本机钥匙完成登录
// 的确认动作。只有当前确有等待中的本地导入会话时才合法——没有待确认的
// 导入密钥时按空密钥报错，避免误触。
func (a *App) SubmitLoginKey(provider, key string) error {
	cfg := a.settings()
	profile, ok := cfg.ProfileByID(provider)
	if !ok {
		return fmt.Errorf("%s", locale.T("login.unknownProvider", provider))
	}
	client, err := a.srv.ClientFor(provider)
	if err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		imported := a.pendingImportedKey(provider)
		if imported == "" {
			return errors.New(locale.T("login.keyEmpty"))
		}
		key = imported
	}
	normalized, err := client.NormalizeOpenCodeKey(key)
	if err != nil {
		if errors.Is(err, upstream.ErrOpenCodeKeyInvalid) {
			return errors.New(locale.T("login.keyInvalid"))
		}
		return err
	}

	// 粘贴的密钥先过一次上游校验：错键立刻提示，不会存进凭据库后等到首次
	// 对话才以一句费解的「认证失败」暴露。联机类错误放行——密钥本身可能
	// 没问题，真实故障让运行时去暴露。
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelVerify()
	if err := client.VerifyOpenCodeKey(verifyCtx, normalized); err != nil {
		if errors.Is(err, upstream.ErrOpenCodeKeyRejected) {
			return errors.New(locale.T("login.keyRejected"))
		}
		a.log.Warnf("未能联机校验 %s 密钥（仍按提交保存）: %v", profile.Name, err)
	}

	cred := client.OpenCodeCredential(normalized)
	if a.store == nil {
		return fmt.Errorf("%s", locale.T("login.saveFailed", "凭据仓库不可用"))
	}
	// 与轮询路径同一套写法：保留本地字段（权重、暂停开关），否则重复
	// 导入同一密钥会把用户配置清掉。
	if err := a.store.SavePreservingLocalFields(cred); err != nil {
		return fmt.Errorf("%s", locale.T("login.saveFailed", err.Error()))
	}
	a.enableProviderIfDisabled(cred.Provider)
	a.srv.InvalidateAccounts()
	a.log.Infof("登录成功: %s（%s）", cred.DisplayName(), cred.ID)

	// 只有当前确实有一个等待密钥的 pending 流程时才接管它；用户可能在
	// 流程已结束（超时/取消）之后才提交，那时只落盘凭据、不动登录态。
	a.loginMu.Lock()
	run := a.login
	takeover := run != nil && run.provider == provider && run.status == "pending" && run.session != nil && run.session.NeedKey
	if takeover {
		a.finishLoginLocked(run, "success", locale.T("login.success"), cred.ID)
	}
	a.loginMu.Unlock()
	if !takeover {
		a.log.Warnf("收到密钥提交，但没有等待中的 %s 登录流程，仅保存凭据", profile.Name)
	}
	a.pushLogin()
	a.pushState()

	go func() {
		refreshCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		a.srv.RefreshModels(refreshCtx)
		a.pushState()
	}()
	return nil
}

func (a *App) cancelLogin() {
	a.loginMu.Lock()
	run := a.login
	a.login = nil
	a.loginMu.Unlock()
	if run == nil {
		return
	}
	// 先停掉协议族侧的资源（本地回调监听、探测口等），再取消轮询上下文。
	if run.session != nil {
		run.session.Cancel()
	}
	if run.cancel != nil {
		run.cancel()
	}
}

// CancelLogin 取消当前登录流程。
func (a *App) CancelLogin() {
	a.cancelLogin()
	a.log.Infof("已取消登录")
	a.pushLogin()
}

func (a *App) loginState() LoginView {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.login == nil {
		return LoginView{Status: "idle"}
	}
	run := a.login
	view := LoginView{
		Active:    run.status == "pending",
		Provider:  run.provider,
		URL:       run.session.AuthURL,
		EmbedURL:  run.session.EmbeddedAuthURL(run.parentOrigin),
		State:     run.session.State,
		Status:    run.status,
		Message:   run.message,
		ExpiresAt: run.session.ExpiresAt.Format("15:04:05"),
		AccountID: run.accountID,
		NeedKey:   run.session.NeedKey,
	}
	return view
}

// DeleteAccount 删除一个账号。
func (a *App) DeleteAccount(provider, id string) error {
	if a.store == nil {
		return errors.New("凭据仓库不可用")
	}
	if err := a.store.Delete(provider, id); err != nil {
		return err
	}
	a.log.Infof("已删除账号 %s", id)
	// 删掉的是该版本最后一个账号时，这一版的模型要从列表里消失，
	// 所以立即失效登录缓存并重建模型列表。
	a.srv.InvalidateAccounts()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		a.srv.RefreshModels(ctx)
		a.pushState()
	}()
	a.pushState()
	return nil
}

// SetAccountWeight 设置账号在同组（同一上游）内的负载均衡权重。
//
// 权重 <=0 或未设置时按 1 处理，即组内默认同权。
func (a *App) SetAccountWeight(provider, id string, weight int) error {
	if weight < 0 {
		weight = 0
	}
	if weight > 1000 {
		weight = 1000
	}
	effective := weight
	if err := a.srv.UpdateCredential(provider, id, func(cred *upstream.Credential) error {
		cred.Weight = weight
		effective = cred.EffectiveWeight()
		return nil
	}); err != nil {
		return err
	}
	a.log.Infof("账号 %s 权重设为 %d", id, effective)
	a.pushState()
	return nil
}

// SetAccountPaused 暂停或恢复一个账号；暂停的账号保留凭据但不参与转发。
func (a *App) SetAccountPaused(provider, id string, paused bool) error {
	if err := a.srv.UpdateCredential(provider, id, func(cred *upstream.Credential) error {
		cred.Paused = paused
		return nil
	}); err != nil {
		return err
	}
	if paused {
		a.log.Infof("账号 %s 已暂停", id)
	} else {
		a.log.Infof("账号 %s 已恢复", id)
	}
	a.pushState()
	return nil
}

// -----------------------------------------------------------------------------
// 设置
// -----------------------------------------------------------------------------

// SaveSettings 保存设置并立即生效（监听地址变更时自动重启服务）。
func (a *App) SaveSettings(input SettingsInput) error {
	cfg := a.settings().Clone()

	if v := strings.TrimSpace(input.Listen); v != "" {
		cfg.Listen = v
	}
	cfg.ModelSyncHours = input.ModelSyncHours
	cfg.CORS = boolPtr(input.CORS)
	cfg.SanitizeTemplates = boolPtr(input.Sanitize)
	cfg.ForceMaxThinking = boolPtr(input.MaxThinking)
	cfg.AutoStart = boolPtr(input.AutoStart)
	cfg.LaunchAtLogin = boolPtr(input.LaunchAtLogin)

	for _, in := range input.Profiles {
		profile, ok := cfg.ProfileByID(in.ID)
		if !ok {
			continue
		}
		profile.Enabled = in.Enabled
		profile.Proxy = strings.TrimSpace(in.Proxy)
		cfg.SetProfile(profile)
	}
	cfg.Normalize()

	if err := cfg.Save(); err != nil {
		a.log.Errorf("保存设置失败: %v", err)
		return err
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.srv.SetConfig(cfg)

	a.log.Infof("设置已保存（监听 %s，上游 %d 个）", cfg.Listen, len(cfg.EnabledProfiles()))

	// 开机自启动立即生效（注册 / 注销系统条目）。
	a.applyLaunchAtLogin(cfg.LaunchAtLoginEnabled())

	if a.srv.Running() {
		if !sameListenAddr(cfg.Listen, a.srv.Addr()) {
			// 只有监听地址真的变了才重启，否则 ":8787" 与 "[::]:8787"
			// 这种等价写法会导致每次保存都重启一次服务。
			if err := a.RestartService(); err != nil {
				a.pushState()
				return err
			}
		} else {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				a.srv.RefreshModels(ctx)
				a.pushState()
			}()
		}
	}
	a.pushState()
	return nil
}

func boolPtr(v bool) *bool { return &v }

// applyLaunchAtLogin 把开机自启动设置落到系统（失败只记日志，不影响主流程）。
func (a *App) applyLaunchAtLogin(enable bool) {
	if err := autostart.Set(enable); err != nil {
		a.log.Warnf("设置开机自启动（%v）失败: %v", enable, err)
		return
	}
	a.log.Infof("开机自启动已%s", boolText(enable))
}

func boolText(v bool) string {
	if v {
		return "开启"
	}
	return "关闭"
}

// sameListenAddr 判断两个监听地址是否等价。
//
// "" / "0.0.0.0" / "::" 都表示监听所有网卡，实际绑定后会变成 "[::]:端口"，
// 直接字符串比较会误判成地址变更。
func sameListenAddr(a, b string) bool {
	if a == b {
		return true
	}
	hostA, portA, errA := net.SplitHostPort(a)
	hostB, portB, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || portA != portB {
		return false
	}
	return hostA == hostB || (isAnyHost(hostA) && isAnyHost(hostB))
}

func isAnyHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// 系统交互
// -----------------------------------------------------------------------------

// OpenURL 用系统默认浏览器打开链接。
func (a *App) OpenURL(url string) {
	ctx := a.context()
	url = strings.TrimSpace(url)
	if ctx == nil || url == "" {
		return
	}
	// 只允许 http/https，避免 javascript: 等危险 scheme 被交给系统打开。
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return
	}
	wailsrt.BrowserOpenURL(ctx, url)
}

// CopyText 复制文本到剪贴板。
func (a *App) CopyText(text string) {
	ctx := a.context()
	if ctx == nil {
		return
	}
	wailsrt.ClipboardSetText(ctx, text)
}

// -----------------------------------------------------------------------------
// 在线更新
// -----------------------------------------------------------------------------

// emitUpdate 向前端推送更新进度。
func (a *App) emitUpdate(v UpdateView) {
	a.emit(eventUpdate, v)
}

// CheckUpdate 检查 GitHub 上的新版本。有更新时结果会被记住，供 DoUpdate 使用。
func (a *App) CheckUpdate() (UpdateView, error) {
	view := UpdateView{Phase: "checking", Current: Version}
	a.emitUpdate(view)

	client := &http.Client{Timeout: 20 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	rel, has, err := update.Check(ctx, client, "", update.DefaultRepo, Version)
	if err != nil {
		view.Phase, view.Message = "failed", err.Error()
		a.emitUpdate(view)
		return view, err
	}

	view.Latest = rel.Version
	view.Page = rel.Page
	view.Notes = rel.Notes
	if !has {
		a.updMu.Lock()
		a.upd = nil
		a.updMu.Unlock()
		view.Phase, view.Message = "none", locale.T("upd.latest")
		a.emitUpdate(view)
		return view, nil
	}

	a.updMu.Lock()
	a.upd = rel
	a.updMu.Unlock()
	view.Phase = "available"
	view.HasUpdate = true
	view.AssetName = rel.Asset.Name
	view.Message = locale.T("upd.available", rel.Version, rel.Asset.Name)
	a.log.Infof("检查到新版本 v%s（当前 %s）", rel.Version, Version)
	a.emitUpdate(view)
	return view, nil
}

// DoUpdate 下载安装包并执行安装，随后退出当前程序。
//
//   - Windows：运行 NSIS 安装向导后退出
//   - macOS：用 open 打开 PKG 安装器后退出
//   - Linux：把新 AppImage 原地替换当前程序后自动重启；非 AppImage 运行方式
//     则把安装包放到数据目录并提示手动运行
func (a *App) DoUpdate() error {
	a.updMu.Lock()
	rel := a.upd
	a.updMu.Unlock()
	if rel == nil {
		return fmt.Errorf("%s", locale.T("upd.checkFirst"))
	}

	dest := filepath.Join(a.dataDir, "update", rel.Asset.Name)
	a.emitUpdate(UpdateView{
		Phase: "downloading", Current: Version, Latest: rel.Version,
		AssetName: rel.Asset.Name, Progress: -1,
		Message: locale.T("upd.downloadStart", rel.Asset.Name),
	})

	err := update.Download(context.Background(), nil, rel.Asset, dest, func(done, total int64) {
		pct := -1
		if total > 0 {
			pct = int(done * 100 / total)
		}
		a.emitUpdate(UpdateView{
			Phase: "downloading", Current: Version, Latest: rel.Version,
			AssetName: rel.Asset.Name, Progress: pct,
			Message: locale.T("upd.downloading", humanBytes(done), humanBytes(total)),
		})
	})
	if err != nil {
		a.log.Errorf("下载更新失败: %v", err)
		a.emitUpdate(UpdateView{Phase: "failed", Current: Version, Latest: rel.Version, Message: locale.T("upd.downloadFailed", err.Error())})
		return err
	}
	a.log.Infof("更新包已下载: %s", dest)

	if runtime.GOOS == "linux" {
		return a.applyLinuxUpdate(rel, dest)
	}

	a.emitUpdate(UpdateView{Phase: "installing", Current: Version, Latest: rel.Version, Message: locale.T("upd.launching")})
	a.log.Infof("启动安装程序: %s", dest)
	if err := update.LaunchInstaller(dest); err != nil {
		a.log.Errorf("启动安装程序失败: %v", err)
		a.emitUpdate(UpdateView{Phase: "failed", Current: Version, Latest: rel.Version, Message: locale.T("upd.launchFailed", err.Error())})
		return err
	}
	a.log.Infof("安装程序已启动，程序即将退出")
	a.QuitApp()
	return nil
}

// applyLinuxUpdate 处理 Linux 平台的更新：AppImage 原地替换并重启。
func (a *App) applyLinuxUpdate(rel *update.Release, dest string) error {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err == nil && strings.EqualFold(filepath.Ext(exe), ".appimage") {
		a.emitUpdate(UpdateView{Phase: "installing", Current: Version, Latest: rel.Version, Message: locale.T("upd.replacingAppImage")})
		if err := os.Chmod(dest, 0o755); err != nil {
			a.emitUpdate(UpdateView{Phase: "failed", Current: Version, Message: locale.T("upd.chmodFailed", err.Error())})
			return err
		}
		if err := os.Rename(dest, exe); err != nil {
			a.emitUpdate(UpdateView{Phase: "failed", Current: Version, Message: locale.T("upd.replaceFailed", err.Error())})
			return err
		}
		a.log.Infof("AppImage 已替换为 v%s，正在重启", rel.Version)
		a.emitUpdate(UpdateView{Phase: "done", Current: rel.Version, Latest: rel.Version, Message: locale.T("upd.doneRestarting")})
		if err := exec.Command(exe).Start(); err != nil {
			a.log.Warnf("自动重启失败，请手动启动: %v", err)
		}
		a.QuitApp()
		return nil
	}

	// 非 AppImage 运行方式（如源码/压缩包运行）：交给用户手动安装
	a.emitUpdate(UpdateView{
		Phase: "done", Current: Version, Latest: rel.Version,
		Message: locale.T("upd.manualInstall", dest),
	})
	return nil
}

// humanBytes 把字节数格式化为可读文本。
func humanBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// OpenDataDir 打开数据目录（配置文件与凭据所在位置）。
func (a *App) OpenDataDir() {
	dir := a.dataDir
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", filepath.Clean(dir))
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		a.log.Warnf("打开数据目录失败: %v", err)
	}
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return locale.T("dur.expired")
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return locale.T("dur.dayHour", days, hours)
	case hours > 0:
		return locale.T("dur.hourMin", hours, minutes)
	default:
		return locale.T("dur.minute", minutes)
	}
}

// -----------------------------------------------------------------------------
// 语言
// -----------------------------------------------------------------------------

// SetLanguage 切换界面语言（system / zh / en）。
//
// 立即写回配置并刷新托盘菜单文案；前端收到新的 state 后会同步整页翻译。
// 返回解析后的实际语言（zh / en），供前端直接应用翻译。
func (a *App) SetLanguage(lang string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(lang))
	switch v {
	case locale.PrefSystem, locale.PrefZh, locale.PrefEn:
	default:
		return "", errors.New(locale.T("lang.invalid", lang))
	}

	cfg := a.settings().Clone()
	cfg.Language = v
	if err := cfg.Save(); err != nil {
		a.log.Errorf("保存语言设置失败: %v", err)
		return "", err
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()

	locale.Set(locale.FromPref(v))
	if a.tray != nil {
		a.tray.Retitle()
	}
	a.log.Infof("界面语言已切换为 %s", locale.Current())
	a.pushState()
	return string(locale.Current()), nil
}
