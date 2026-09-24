package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/proxysrv"
	"github.com/znsoftm/RapidProxy/internal/store"
	"github.com/znsoftm/RapidProxy/internal/tray"
	"github.com/znsoftm/RapidProxy/internal/upstream"
	"github.com/znsoftm/RapidProxy/internal/winfit"
)

// 事件名（前端通过 window.runtime.EventsOn 订阅）。
const (
	eventState = "state"
	eventLogin = "login"
	eventLog   = "log"
)

// Version 是程序版本号。
const Version = "1.0.0"

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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Context  int64  `json:"context"`
	MaxOut   int64  `json:"maxOut"`
	Images   bool   `json:"images"`
}

// SettingsView 是设置面板数据。
type SettingsView struct {
	Listen         string `json:"listen"`
	ModelSyncHours int    `json:"modelSyncHours"`
	CORS           bool   `json:"cors"`
	Sanitize       bool   `json:"sanitize"`
	MaxThinking    bool   `json:"maxThinking"`
	AutoStart      bool   `json:"autoStart"`
	ConfigPath     string `json:"configPath"`
	DataDir        string `json:"dataDir"`
	LogPath        string `json:"logPath"`
	AccountDir     string `json:"accountDir"`
	Version        string `json:"version"`
	Platform       string `json:"platform"`
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
	Settings   SettingsView   `json:"settings"`
	Logs       []string       `json:"logs"`
	Login      LoginView      `json:"login"`
}

// LoginView 是登录流程状态。
type LoginView struct {
	Active    bool   `json:"active"`
	Provider  string `json:"provider"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	ExpiresAt string `json:"expiresAt"`
	AccountID string `json:"accountId"`
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
	Profiles       []ProfileInput `json:"profiles"`
}

// -----------------------------------------------------------------------------
// App
// -----------------------------------------------------------------------------

type loginRun struct {
	provider  string
	session   *upstream.LoginSession
	cancel    context.CancelFunc
	status    string
	message   string
	accountID string
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

	quitting atomic.Bool
	headless atomic.Bool
	trayHint atomic.Bool
	unsubLog func()
	loginMu  sync.Mutex
	login    *loginRun
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

	// 在界面真正显示出来之前收敛窗口尺寸（见 fitWindow 的注释）。
	a.fitWindow(ctx)

	if a.cfg.AutoStartEnabled() {
		if err := a.StartService(); err != nil {
			a.log.Errorf("自动启动失败: %v", err)
		}
	}
	a.pushState()
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
		Title:   "RapidProxy 仍在运行",
		Message: "程序已最小化到系统托盘，代理服务继续在后台运行。\n\n要重新打开界面，请点击或双击托盘图标；要完全退出，请在托盘图标右键菜单中选择「退出」。",
		Buttons: []string{"知道了"},
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
func (a *App) runHeadless() {
	a.headless.Store(true)
	if err := a.StartService(); err != nil {
		a.log.Errorf("后台模式启动代理服务失败: %v", err)
		os.Exit(1)
	}
	a.log.Infof("已进入后台模式（无界面）：接口地址 %s，可从托盘图标退出", a.settings().OpenAIBaseURL())
	select {}
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
	creds, _ := a.store.List("")
	view.Providers = a.providerViews(creds)
	view.Accounts = a.accountViews(creds)
	view.Models = a.modelViews()
	view.Settings = SettingsView{
		Listen:         cfg.Listen,
		ModelSyncHours: cfg.ModelSyncHours,
		CORS:           cfg.CORSEnabled(),
		Sanitize:       cfg.SanitizeEnabled(),
		MaxThinking:    cfg.MaxThinkingEnabled(),
		AutoStart:      cfg.AutoStartEnabled(),
		ConfigPath:     cfg.Path(),
		DataDir:        a.dataDir,
		LogPath:        a.log.FilePath(),
		AccountDir:     a.store.Root(),
		Version:        Version,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
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
			NeedsProxy:   strings.Contains(p.BaseURL, "workbuddy.ai"),
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

func (a *App) modelViews() []ModelView {
	cfg := a.settings()
	names := map[string]string{}
	for _, p := range cfg.Profiles {
		names[p.ID] = p.Name
	}
	models := a.srv.AllModels()
	out := make([]ModelView, 0, len(models))
	for _, m := range models {
		out = append(out, ModelView{
			ID:       m.ID,
			Name:     m.DisplayName,
			Provider: names[m.Provider],
			Context:  m.ContextLength,
			MaxOut:   m.MaxOutputTokens,
			Images:   m.SupportsImages,
		})
	}
	return out
}

// GetLogs 返回最近日志。
func (a *App) GetLogs() []string { return a.log.Tail(300) }

// ClearLogs 清空界面上的日志。
func (a *App) ClearLogs() {
	a.log.Clear()
	a.pushState()
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
		return errors.New("API Key 不能为空")
	}
	cfg := a.settings().Clone()
	if !cfg.AddAPIKey(key) {
		return errors.New("该 API Key 已存在")
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
		return errors.New("未找到该 API Key")
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

// StartLogin 发起某个上游的登录流程，返回需要用户打开的授权链接。
func (a *App) StartLogin(provider string) (LoginView, error) {
	cfg := a.settings()
	profile, ok := cfg.ProfileByID(provider)
	if !ok {
		return LoginView{}, fmt.Errorf("未知的上游: %s", provider)
	}
	client, err := a.srv.ClientFor(provider)
	if err != nil {
		return LoginView{}, err
	}

	a.cancelLogin()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	session, err := client.StartLogin(ctx)
	if err != nil {
		cancel()
		a.log.Errorf("[%s] 发起登录失败: %v", profile.Name, err)
		return LoginView{}, err
	}

	run := &loginRun{
		provider: provider,
		session:  session,
		cancel:   cancel,
		status:   "pending",
		message:  "等待在浏览器中完成登录",
	}
	a.loginMu.Lock()
	a.login = run
	a.loginMu.Unlock()

	a.log.Infof("[%s] 已生成登录链接，请在浏览器中完成登录", profile.Name)
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
			a.finishLogin(run, "failed", "登录超时或已取消", "")
			return
		case <-ticker.C:
			cred, err := run.session.Poll(ctx)
			if err != nil {
				if errors.Is(err, upstream.ErrLoginTimeout) {
					a.finishLogin(run, "failed", "登录链接已过期，请重新发起登录", "")
					return
				}
				a.finishLogin(run, "failed", err.Error(), "")
				return
			}
			if cred == nil {
				continue
			}
			if err := a.store.Save(cred); err != nil {
				a.finishLogin(run, "failed", "保存凭据失败: "+err.Error(), "")
				return
			}
			a.log.Infof("登录成功: %s（%s）", cred.DisplayName(), cred.ID)
			a.finishLogin(run, "success", "登录成功", cred.ID)
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

func (a *App) finishLogin(run *loginRun, status, message, accountID string) {
	a.loginMu.Lock()
	if a.login == run {
		run.status = status
		run.message = message
		run.accountID = accountID
		if status != "pending" {
			run.cancel()
		}
	}
	a.loginMu.Unlock()
	a.log.Infof("登录流程结束: %s", message)
	a.pushLogin()
	a.pushState()
}

func (a *App) cancelLogin() {
	a.loginMu.Lock()
	run := a.login
	a.login = nil
	a.loginMu.Unlock()
	if run != nil && run.cancel != nil {
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
		State:     run.session.State,
		Status:    run.status,
		Message:   run.message,
		ExpiresAt: run.session.ExpiresAt.Format("15:04:05"),
		AccountID: run.accountID,
	}
	return view
}

// DeleteAccount 删除一个账号。
func (a *App) DeleteAccount(provider, id string) error {
	if err := a.store.Delete(provider, id); err != nil {
		return err
	}
	a.log.Infof("已删除账号 %s", id)
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
	if ctx == nil || strings.TrimSpace(url) == "" {
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
		return "已过期"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d 天 %d 小时后过期", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d 小时 %d 分钟后过期", hours, minutes)
	default:
		return fmt.Sprintf("%d 分钟后过期", minutes)
	}
}
