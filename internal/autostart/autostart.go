// Package autostart 管理「开机自动启动」：
//
//   - Windows：当前用户注册表 Run 键
//     HKCU\Software\Microsoft\Windows\CurrentVersion\Run
//   - macOS：~/Library/LaunchAgents/<plist>（RunAtLoad）
//   - Linux：~/.config/autostart/<desktop>（XDG 自启动规范）
//
// 自启动命令统一附带 --hidden 参数：开机后程序静默运行在系统托盘，
// 不弹出主窗口打扰用户。
//
// 条目在每次程序启动时按配置重写一次，这样程序升级（安装路径变化）
// 之后自启动指向也能自动修正。
package autostart

import (
	"os"
	"path/filepath"
)

// EntryName 是自启动条目的名称（注册表值名 / plist 文件名主体 / desktop 文件名）。
const EntryName = "RapidProxy"

// HiddenArg 是自启动命令附加的参数：静默启动到托盘。
const HiddenArg = "--hidden"

// exePath 返回当前可执行文件的绝对路径。
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

// commandLine 生成自启动使用的完整命令行（路径带引号 + 静默参数）。
func commandLine() (string, error) {
	exe, err := exePath()
	if err != nil {
		return "", err
	}
	return quotePath(exe) + " " + HiddenArg, nil
}
