// Package singleton 保证同一时刻只运行一个 RapidProxy 实例。
//
// 实现方式：
//   - Windows：用户会话级命名互斥体（Local\ 前缀），进程退出时由系统自动释放；
//   - macOS / Linux：对数据目录下的 singleton.lock 加 flock 排它锁，
//     进程退出（含崩溃）时由内核自动释放，不会留下“死锁文件”。
//
// Acquire 成功后锁随进程存活，无需（也没有必要）显式释放。
package singleton

import (
	"os"
	"path/filepath"
	"strings"
)

// osUserHomeDir 返回当前用户主目录。
func osUserHomeDir() (string, error) {
	return os.UserHomeDir()
}

// lockDirName 返回锁文件所在目录（与默认数据目录一致：~/.rapidproxy）。
func lockDirName(name string) string {
	return "." + strings.ToLower(name)
}

// lockFilePath 返回 unix 平台的锁文件绝对路径。
func lockFilePath(name string) (string, error) {
	home, err := osUserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, lockDirName(name), "singleton.lock"), nil
}
