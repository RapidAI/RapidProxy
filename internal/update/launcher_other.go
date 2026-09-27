//go:build !windows

package update

import "errors"

// launchWindows 只有 Windows 有真实实现（见 launcher_windows.go）；
// 其他平台在 LaunchInstaller 的 GOOS 分支里不会走到这里，仅为可编译。
func launchWindows(path string) error {
	return errors.New("仅 Windows 支持 runas 启动安装包")
}
