//go:build !windows

package singleton

import (
	"os"
	"path/filepath"
	"syscall"
)

// Acquire 尝试对锁文件加 flock 排它锁（LOCK_NB 非阻塞）。
// 返回 false 表示已有实例持有锁。
// 锁随打开的文件描述符存活，进程退出（含崩溃）时内核自动释放。
func Acquire(name string) bool {
	path, err := lockFilePath(name)
	if err != nil {
		// 拿不到家目录：放行启动，宁可重复运行也不拦着用。
		return true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return true
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	held = f // 保持文件描述符打开即持续持有锁
	return true
}

// held 保存锁文件句柄；保持打开状态即持续持有锁。
var held *os.File

// NotifyAlreadyRunning 尽力弹一个图形提示；没有任何通知工具时静默退出。
func NotifyAlreadyRunning(name string) {
	_, err := osUserHomeDir()
	_ = err // 家目录仅用于构造文案，失败也不影响提示
	msg := name + " is already running (see the system tray)."
	tryNotify(msg)
}
