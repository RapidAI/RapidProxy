//go:build !windows

package singleton

import (
	"os"
	"path/filepath"
	"testing"
)

// 同一进程内持锁后再次 Acquire 应失败（flock LOCK_NB 返回 EWOULDBLOCK）。
func TestAcquireTwice(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // darwin/linux 的 os.UserHomeDir 都读 HOME
	if !Acquire("RapidProxy-Test") {
		t.Skip("另一个实例已持有测试锁")
	}
	if Acquire("RapidProxy-Test") {
		t.Fatal("第二次 Acquire 应返回 false（锁已被本进程持有）")
	}
}

// 锁文件应落在 ~/.<小写应用名>/singleton.lock。
func TestLockFilePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := lockFilePath("RapidProxy")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".rapidproxy", "singleton.lock")
	if got != want {
		t.Fatalf("lockFilePath = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		// Acquire 内部负责建目录，这里仅验证路径拼接正确
		t.Logf("锁目录尚未创建（正常，由 Acquire 创建）: %v", err)
	}
}
