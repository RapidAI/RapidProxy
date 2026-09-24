//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/znsoftm/RapidProxy/internal/fsx"
)

// desktopPath 返回 XDG 自启动条目路径。
func desktopPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart", strings.ToLower(EntryName)+".desktop"), nil
}

// quotePath 给路径加双引号，容忍安装路径里的空格。
func quotePath(p string) string { return `"` + p + `"` }

// desktopEntry 生成 XDG autostart 桌面条目内容。
func desktopEntry(execLine string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=" + EntryName + "\n" +
		"Comment=OpenAI-compatible API proxy for WorkBuddy / CodeBuddy\n" +
		"Exec=" + execLine + "\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
}

// Enabled 返回是否已安装自启动条目。
func Enabled() bool {
	path, err := desktopPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Set 安装或卸载 XDG 自启动条目（登录时静默启动到托盘）。
func Set(enable bool) error {
	path, err := desktopPath()
	if err != nil {
		return err
	}
	if !enable {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	cmd, err := commandLine()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsx.AtomicWrite(path, []byte(desktopEntry(cmd)), 0o644)
}
