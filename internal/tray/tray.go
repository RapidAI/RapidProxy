// Package tray 封装系统托盘图标与菜单。
//
// 托盘与 Wails 主循环共存：使用 systray 的 RunWithExternalLoop，由 Wails 的
// 事件循环驱动托盘，避免两个 UI 框架争抢主线程。
//
// 菜单文案走 internal/locale 双语；语言偏好变更后调用 Retitle 即时刷新。
package tray

import (
	_ "embed"
	"runtime"
	"sync"

	"github.com/energye/systray"

	"github.com/znsoftm/RapidProxy/internal/locale"
)

//go:embed assets/tray.ico
var iconICO []byte

//go:embed assets/tray.png
var iconPNG []byte

// Callbacks 是托盘菜单触发的动作。
type Callbacks struct {
	// OnShow 打开主界面。
	OnShow func()
	// OnResetWindow 重置窗口位置与尺寸（窗口跑到屏幕外时使用）。
	OnResetWindow func()
	// OnToggle 切换服务启停，返回切换后的运行状态。
	OnToggle func() bool
	// OnCopyURL 复制 OpenAI 接口地址。
	OnCopyURL func()
	// OnQuit 退出程序。
	OnQuit func()
}

// Tray 是一个托盘图标实例。
type Tray struct {
	cb Callbacks

	mu         sync.Mutex
	showItem   *systray.MenuItem
	copyItem   *systray.MenuItem
	resetItem  *systray.MenuItem
	statusItem *systray.MenuItem
	toggleItem *systray.MenuItem
	quitItem   *systray.MenuItem
	addr       string
	running    bool
}

// Start 创建并启动托盘图标。
//
// 必须在主 UI 事件循环启动之前调用（systray 会注册原生资源，由主循环驱动）。
func Start(cb Callbacks) *Tray {
	t := &Tray{cb: cb, addr: "-"}
	start, _ := systray.RunWithExternalLoop(t.onReady, func() {})
	start()
	return t
}

func icon() []byte {
	if runtime.GOOS == "windows" {
		return iconICO
	}
	return iconPNG
}

func (t *Tray) onReady() {
	systray.SetIcon(icon())
	systray.SetTooltip(t.tooltip())

	// 菜单项先按当前语言建标题，语言切换后由 Retitle 统一刷新。
	t.showItem = systray.AddMenuItem(locale.T("tray.show"), locale.T("tray.showTip"))
	t.copyItem = systray.AddMenuItem(locale.T("tray.copyURL"), locale.T("tray.copyURLTip"))
	t.resetItem = systray.AddMenuItem(locale.T("tray.resetWindow"), locale.T("tray.resetTip"))
	systray.AddSeparator()
	t.statusItem = systray.AddMenuItem(locale.T("tray.statusIdle"), locale.T("tray.statusTip"))
	t.toggleItem = systray.AddMenuItem(locale.T("tray.start"), locale.T("tray.startTip"))
	systray.AddSeparator()
	t.quitItem = systray.AddMenuItem(locale.T("tray.quit"), locale.T("tray.quitTip"))

	systray.SetOnClick(func(systray.IMenu) { t.show() })
	systray.SetOnDClick(func(systray.IMenu) { t.show() })

	show := func() { t.show() }
	t.showItem.Click(show)

	t.copyItem.Click(func() {
		if t.cb.OnCopyURL != nil {
			t.cb.OnCopyURL()
		}
	})
	t.resetItem.Click(func() {
		if t.cb.OnResetWindow != nil {
			t.cb.OnResetWindow()
		}
	})
	t.toggleItem.Click(func() {
		if t.cb.OnToggle == nil {
			return
		}
		running := t.cb.OnToggle()
		t.SetState(running, t.addr)
	})
	t.quitItem.Click(func() {
		if t.cb.OnQuit != nil {
			t.cb.OnQuit()
		}
	})
}

func (t *Tray) show() {
	if t.cb.OnShow != nil {
		t.cb.OnShow()
	}
}

// SetState 刷新托盘上的状态文案。
func (t *Tray) SetState(running bool, addr string) {
	t.mu.Lock()
	t.running = running
	if addr != "" {
		t.addr = addr
	}
	statusItem, toggleItem := t.statusItem, t.toggleItem
	status, toggle, tip := t.labels()
	t.mu.Unlock()

	if statusItem != nil {
		statusItem.SetTitle(status)
	}
	if toggleItem != nil {
		toggleItem.SetTitle(toggle)
	}
	systray.SetTooltip(tip)
}

// Retitle 按当前语言刷新全部菜单项标题与提示（语言切换后调用）。
func (t *Tray) Retitle() {
	t.mu.Lock()
	status, _, tip := t.labels()
	t.mu.Unlock()

	retitle := func(item *systray.MenuItem, title, tip string) {
		if item == nil {
			return
		}
		item.SetTitle(title)
		item.SetTooltip(tip)
	}
	retitle(t.showItem, locale.T("tray.show"), locale.T("tray.showTip"))
	retitle(t.copyItem, locale.T("tray.copyURL"), locale.T("tray.copyURLTip"))
	retitle(t.resetItem, locale.T("tray.resetWindow"), locale.T("tray.resetTip"))
	retitle(t.statusItem, status, locale.T("tray.statusTip"))
	retitle(t.toggleItem, locale.T("tray.start"), locale.T("tray.startTip"))
	retitle(t.quitItem, locale.T("tray.quit"), locale.T("tray.quitTip"))
	systray.SetTooltip(tip)
}

// labels 返回当前状态/语言下的状态项、启停项标题与托盘提示。
// 调用方需持有 t.mu。
func (t *Tray) labels() (status, toggle, tooltip string) {
	if t.running {
		return locale.T("tray.statusRun", t.addr), locale.T("tray.stop"), locale.T("tray.tipRun", t.addr)
	}
	return locale.T("tray.statusIdle"), locale.T("tray.start"), locale.T("tray.tipIdle")
}

func (t *Tray) tooltip() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _, tip := t.labels()
	return tip
}

// Quit 移除托盘图标。
func (t *Tray) Quit() {
	systray.Quit()
}
