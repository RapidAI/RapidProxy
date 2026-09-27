//go:build windows

package singleton

import (
	"time"

	"golang.org/x/sys/windows"

	"github.com/znsoftm/RapidProxy/internal/locale"
)

// mutexName 用户会话级命名互斥体：同一 Windows 会话内全局唯一，
// 不同用户会话互不影响（每人可以各跑一份）。
func mutexName(name string) string {
	return `Local\` + name + "-SingleInstance"
}

// pipeName 激活通道的命名管道名，与互斥体同前缀便于排查。
func pipeName(name string) string {
	return `\\.\pipe\` + name + "-SingleInstance-Activate"
}

// Acquire 尝试持有单实例互斥体。返回 false 表示已有实例在运行。
// 互斥体句柄保存在包级变量中，随进程存活，由系统在退出时释放。
func Acquire(name string) bool {
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(mutexName(name)))
	if err == windows.ERROR_ALREADY_EXISTS {
		// 已有实例：立即关掉新句柄，让存量实例继续持有。
		windows.CloseHandle(h)
		return false
	}
	if err != nil && err != windows.ERROR_SUCCESS {
		// 创建失败（极罕见）：放行启动，宁可重复运行也不拦着用。
		return true
	}
	held = h
	return true
}

// held 保存互斥体句柄；保持非关闭状态即持续持有锁。
var held windows.Handle

// Listen 启动激活监听：之后每个连上管道的客户端都会触发一次 onActivate。
// 调用契约：仅在 Acquire 成功后调用一次；监听 goroutine 随进程存活，
// 无需（也没有办法）停止。
func Listen(name string, onActivate func()) {
	if onActivate == nil {
		return
	}
	go serveActivation(pipeName(name), onActivate)
}

// serveActivation 循环创建管道实例并等待连接。
//
// 同步 I/O 就够了：阻塞等连接是本 goroutine 的本职；客户端的命令写进
// 管道缓冲即返回，服务端不读也不会卡住对端，因此连接本身就是激活信号。
func serveActivation(pipe string, onActivate func()) {
	pipePtr := windows.StringToUTF16Ptr(pipe)
	for {
		h, err := windows.CreateNamedPipe(
			pipePtr,
			windows.PIPE_ACCESS_INBOUND,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
			windows.PIPE_UNLIMITED_INSTANCES, 4096, 4096, 0, nil)
		if err != nil {
			// 管道创建失败（极罕见）：放弃监听，后续实例会走提示框兜底。
			return
		}
		err = windows.ConnectNamedPipe(h, nil)
		connected := err == nil || err == windows.ERROR_PIPE_CONNECTED
		if !connected {
			// 偶发失败（例如客户端连上又立刻关闭）：重开实例继续，
			// 不能放弃监听，否则本次运行期间激活功能就此失效。
			windows.CloseHandle(h)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		windows.DisconnectNamedPipe(h)
		windows.CloseHandle(h)
		onActivate()
	}
}

// Activate 连接已有实例的激活管道，通知它把界面弹到前台。
// 连接成功即视为送达（对端在连接时触发激活）。返回 false 表示通知失败，
// 调用方应退化为 NotifyAlreadyRunning 提示框。
func Activate(name string) bool {
	pipe := pipeName(name)
	// 最多重试 5 次（约 0.5s）：覆盖首实例刚启动、管道实例重建的空档，
	// 以及首实例是旧版本（没有监听管道）时的快速失败。
	pipePtr := windows.StringToUTF16Ptr(pipe)
	for attempt := 0; attempt < 5; attempt++ {
		h, err := windows.CreateFile(pipePtr, windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			// 写一条命令进管道缓冲；对端不读也能成功，失败同样不关心。
			var written uint32
			_ = windows.WriteFile(h, []byte(activateCmd), &written, nil)
			windows.CloseHandle(h)
			return true
		}
		// ERROR_PIPE_BUSY（实例正在重建）或不存在（旧版本/未就绪）：稍等重试。
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// NotifyAlreadyRunning 弹出系统对话框提示用户程序已在运行。
//
// 该入口在配置加载之前（第二实例即将退出），语言只能按系统探测。
func NotifyAlreadyRunning(name string) {
	title, err16 := windows.UTF16PtrFromString(name)
	text, err2 := windows.UTF16PtrFromString(locale.T("dlg.alreadyRunning", name))
	if err16 != nil || err2 != nil {
		return
	}
	const flags = windows.MB_OK | windows.MB_ICONINFORMATION | windows.MB_SETFOREGROUND | windows.MB_TOPMOST
	windows.MessageBox(0, text, title, flags)
}
