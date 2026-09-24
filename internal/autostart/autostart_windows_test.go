//go:build windows

package autostart

import (
	"strings"
	"testing"
)

// TestSetRoundtrip 直接读写真实注册表（HKCU，无需管理员权限）验证开关闭环。
func TestSetRoundtrip(t *testing.T) {
	// 保存初始状态，测试结束后还原。
	original := Enabled()
	t.Cleanup(func() {
		if err := Set(original); err != nil {
			t.Fatalf("还原自启动状态失败: %v", err)
		}
	})

	if err := Set(true); err != nil {
		t.Fatalf("注册自启动失败: %v", err)
	}
	if !Enabled() {
		t.Fatal("注册后 Enabled() 应为 true")
	}

	// 注册表里的命令必须指向当前 exe 并带 --hidden。
	key, err := openRunKey()
	if err != nil {
		t.Fatalf("打开 Run 键失败: %v", err)
	}
	value, _, err := key.GetStringValue(EntryName)
	if err != nil {
		t.Fatalf("读取注册表值失败: %v", err)
	}
	exe, err := exePath()
	if err != nil {
		t.Fatalf("获取 exe 路径失败: %v", err)
	}
	if !strings.Contains(value, exe) {
		t.Errorf("注册表值 %q 应包含 exe 路径 %q", value, exe)
	}
	if !strings.HasSuffix(value, HiddenArg) {
		t.Errorf("注册表值 %q 应以 %s 结尾（静默启动）", value, HiddenArg)
	}
	if !strings.HasPrefix(value, `"`) {
		t.Errorf("注册表值 %q 路径应带引号（兼容含空格路径）", value)
	}

	if err := Set(false); err != nil {
		t.Fatalf("取消自启动失败: %v", err)
	}
	if Enabled() {
		t.Fatal("取消后 Enabled() 应为 false")
	}

	// 取消一个不存在的条目不应报错（幂等）。
	if err := Set(false); err != nil {
		t.Fatalf("重复取消应幂等，得到: %v", err)
	}
}
