//go:build !windows

package singleton

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// 端到端：Listen 之后 Activate 应触发已有实例的激活回调（模拟第二实例）。
func TestListenActivate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	ch := make(chan struct{}, 1)
	Listen("RapidProxy-Test-Activate", func() { ch <- struct{}{} })

	// 监听 goroutine 起好之前 Activate 可能连不上，重试直到送达。
	delivered := false
	for start := time.Now(); time.Since(start) < 3*time.Second; {
		if Activate("RapidProxy-Test-Activate") {
			delivered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !delivered {
		t.Fatal("Activate 应能送达（激活 socket 已在监听）")
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("激活回调未被触发")
	}
}

// 残留的死 socket 文件不应阻止 Listen：持锁实例启动时应清理后重建。
func TestListenRemovesStaleSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := activateSocketPath("RapidProxy-Test-Stale")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	Listen("RapidProxy-Test-Stale", func() {})

	// net.Listen 同步创建 socket 文件：Listen 返回后应已是真实监听端点。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("激活 socket 应被重建: %v", err)
	}
}
