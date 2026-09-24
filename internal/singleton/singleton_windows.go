//go:build windows

package singleton

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// mutexName 用户会话级命名互斥体：同一 Windows 会话内全局唯一，
// 不同用户会话互不影响（每人可以各跑一份）。
func mutexName(name string) string {
	return `Local\` + name + "-SingleInstance"
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

// NotifyAlreadyRunning 弹出系统对话框提示用户程序已在运行。
func NotifyAlreadyRunning(name string) {
	title, err16 := windows.UTF16PtrFromString(name)
	text, err2 := windows.UTF16PtrFromString(fmt.Sprintf("%s 已经在运行了（见系统托盘）。\n无需重复启动。", name))
	if err16 != nil || err2 != nil {
		return
	}
	const flags = windows.MB_OK | windows.MB_ICONINFORMATION | windows.MB_SETFOREGROUND | windows.MB_TOPMOST
	windows.MessageBox(0, text, title, flags)
}
