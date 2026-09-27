//go:build !windows

package singleton

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/znsoftm/RapidProxy/internal/locale"
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
// 该入口在配置加载之前（第二实例即将退出），语言只能按系统探测。
func NotifyAlreadyRunning(name string) {
	tryNotify(locale.T("dlg.alreadyRunning", name))
}

// activateSocketPath 返回激活监听 Unix 域 socket 的路径（与锁文件同目录）。
func activateSocketPath(name string) (string, error) {
	home, err := osUserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, lockDirName(name), "activate.sock"), nil
}

// Listen 启动激活监听：之后每个连上 socket 的客户端都会触发一次 onActivate。
// 调用契约：仅在 Acquire 成功后调用一次；此时锁在手，锁目录里残留的
// socket 文件必是上次崩溃留下的死文件，先删再监听。
// 监听随进程存活，无需停止。
func Listen(name string, onActivate func()) {
	if onActivate == nil {
		return
	}
	path, err := activateSocketPath(name)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return // 监听失败（极罕见）：后续实例会走提示框兜底。
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // 监听已关闭（进程退出）。
			}
			go serveActivationConn(conn, onActivate)
		}
	}()
}

// serveActivationConn 处理一个激活连接：限时读一条命令后回调。
// 内容忽略——连接即激活，读取只是为了不让客户端的写入悬着。
func serveActivationConn(conn net.Conn, onActivate func()) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	_, _ = conn.Read(buf)
	onActivate()
}

// Activate 连接已有实例的激活 socket，通知它把界面弹到前台。
// 返回 false 表示通知失败，调用方应退化为 NotifyAlreadyRunning 提示框。
func Activate(name string) bool {
	path, err := activateSocketPath(name)
	if err != nil {
		return false
	}
	// 最多重试 5 次（约 0.5s）：覆盖首实例刚启动、监听尚未就绪的空档。
	for attempt := 0; attempt < 5; attempt++ {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, werr := conn.Write([]byte(activateCmd))
			conn.Close()
			if werr == nil {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
