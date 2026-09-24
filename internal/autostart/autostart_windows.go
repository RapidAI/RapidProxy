//go:build windows

package autostart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// quotePath 给路径加双引号，容忍安装路径里的空格。
func quotePath(p string) string { return `"` + p + `"` }

// Enabled 返回是否已注册开机自启动。
func Enabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()

	value, _, err := key.GetStringValue(EntryName)
	if err != nil {
		return false
	}
	return value != ""
}

// Set 注册（enable=true）或取消（enable=false）开机自启动。
func Set(enable bool) error {
	if !enable {
		key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
		if err != nil {
			if errors.Is(err, registry.ErrNotExist) {
				return nil
			}
			return err
		}
		defer key.Close()
		if err := key.DeleteValue(EntryName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}

	cmd, err := commandLine()
	if err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetStringValue(EntryName, cmd); err != nil {
		return fmt.Errorf("写入自启动注册表失败: %w", err)
	}
	return nil
}
