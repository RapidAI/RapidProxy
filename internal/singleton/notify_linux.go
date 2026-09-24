//go:build linux

package singleton

import (
	"os/exec"
	"strings"
)

// tryNotify 依次尝试 zenity / kdialog；都没有则静默（终端场景仅退出即可）。
func tryNotify(msg string) {
	msg = strings.ReplaceAll(msg, `"`, `'`)
	if _, err := exec.LookPath("zenity"); err == nil {
		_ = exec.Command("zenity", "--info", "--title=RapidProxy", "--text="+msg).Run()
		return
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		_ = exec.Command("kdialog", "--title", "RapidProxy", "--msgbox", msg).Run()
	}
}
