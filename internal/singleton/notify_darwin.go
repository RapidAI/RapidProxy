//go:build darwin

package singleton

import (
	"os/exec"
	"strings"
)

// tryNotify 用 AppleScript 弹出对话框（macOS 自带，无需额外依赖）。
func tryNotify(msg string) {
	script := `display dialog "` + strings.ReplaceAll(msg, `"`, `'`) + `" with title "RapidProxy" buttons {"好"} default button 1`
	_ = exec.Command("osascript", "-e", script).Run()
}
