// Package singleton 保证同一时刻只运行一个 RapidProxy 实例。
//
// 实现方式：
//   - Windows：用户会话级命名互斥体（Local\ 前缀），进程退出时由系统自动释放；
//   - macOS / Linux：对数据目录下的 singleton.lock 加 flock 排它锁，
//     进程退出（含崩溃）时由内核自动释放，不会留下“死锁文件”。
//
// 除互斥外还提供一条「激活通道」，让后来的实例唤醒已有实例：
//   - Windows：命名管道 \\.\pipe\<name>-SingleInstance-Activate（连接即激活）；
//   - macOS / Linux：锁目录下的 Unix 域 socket activate.sock。
//
// 用法：持有锁的实例先调用 Listen 注册回调；后来实例 Acquire 失败后调用
// Activate 通知，成功则静默退出，失败再退化为 NotifyAlreadyRunning 提示框。
//
// Acquire 成功后锁随进程存活，无需（也没有必要）显式释放。
package singleton

import (
	"os"
	"path/filepath"
	"strings"
)

// activateCmd 是激活通道上约定的命令文本。当前「连接本身」就是激活信号
// （Windows 端不读内容），命令文本仅为以后的协议扩展（如转发 --quit）预留。
const activateCmd = "show"

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
