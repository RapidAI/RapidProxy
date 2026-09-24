//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/znsoftm/RapidProxy/internal/fsx"
)

// plistPath 返回 LaunchAgent 配置文件路径。
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", EntryName+".plist"), nil
}

// quotePath 为 plist 的字符串参数返回原样路径（XML 转义由 plistXML 处理）。
func quotePath(p string) string { return p }

// plistXML 生成 LaunchAgent 配置内容（RunAtLoad：登录时拉起）。
func plistXML(program string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n")
	b.WriteString(fmt.Sprintf("  <string>%s</string>\n", xmlEscape(EntryName)))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range strings.Fields(program) {
		b.WriteString("    <string>" + xmlEscape(strings.Trim(arg, `"`)) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Interactive</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// Enabled 返回是否已安装 LaunchAgent。
func Enabled() bool {
	path, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Set 安装或卸载 LaunchAgent（登录时静默启动到托盘）。
func Set(enable bool) error {
	path, err := plistPath()
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
	return fsx.AtomicWrite(path, []byte(plistXML(cmd)), 0o644)
}
