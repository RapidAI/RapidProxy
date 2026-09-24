// Package tray 封装系统托盘图标与菜单。
//
// 托盘与 Wails 主循环共存：使用 systray 的 RunWithExternalLoop，由 Wails 的
// 事件循环驱动托盘，避免两个 UI 框架争抢主线程。
package tray

import (
	_ "embed"
	"runtime"
	"sync"

	"github.com/energye/systray"
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
	statusItem *systray.MenuItem
	toggleItem *systray.MenuItem
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

	showItem := systray.AddMenuItem("显示主界面", "打开 RapidProxy 窗口")
	copyItem := systray.AddMenuItem("复制接口地址", "复制 OpenAI base_url 到剪贴板")
	resetItem := systray.AddMenuItem("重置窗口位置", "窗口超出屏幕或找不到时重置尺寸并居中")
	systray.AddSeparator()
	t.statusItem = systray.AddMenuItem("服务：未启动", "代理服务当前状态")
	toggleItem := systray.AddMenuItem("启动服务", "启动或停止代理服务")
	t.toggleItem = toggleItem
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("退出", "退出 RapidProxy")

	systray.SetOnClick(func(systray.IMenu) { t.show() })
	systray.SetOnDClick(func(systray.IMenu) { t.show() })

	show := func() { t.show() }
	showItem.Click(show)

	copyItem.Click(func() {
		if t.cb.OnCopyURL != nil {
			t.cb.OnCopyURL()
		}
	})
	resetItem.Click(func() {
		if t.cb.OnResetWindow != nil {
			t.cb.OnResetWindow()
		}
	})
	toggleItem.Click(func() {
		if t.cb.OnToggle == nil {
			return
		}
		running := t.cb.OnToggle()
		t.SetState(running, t.addr)
	})
	quitItem.Click(func() {
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

func (t *Tray) labels() (status, toggle, tooltip string) {
	if t.running {
		return "服务：运行中 " + t.addr, "停止服务", "RapidProxy · 运行中 " + t.addr
	}
	return "服务：未启动", "启动服务", "RapidProxy · 未启动"
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
