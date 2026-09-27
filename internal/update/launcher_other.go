//go:build !windows

package update

import (
	"fmt"

	"github.com/znsoftm/RapidProxy/internal/locale"
)

// launchWindows 只有 Windows 有真实实现（见 launcher_windows.go）；
// 其他平台在 LaunchInstaller 的 GOOS 分支里不会走到这里，仅为可编译。
func launchWindows(path string) error {
	return fmt.Errorf("%s", locale.T("launcher.notWindows"))
}
