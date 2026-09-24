//go:build windows

package singleton

import "testing"

// 同一进程内二次 Acquire 应失败（与跨进程行为一致：同名互斥体已存在）。
func TestAcquireTwice(t *testing.T) {
	if !Acquire("RapidProxy-Test") {
		t.Skip("另一个实例已持有测试互斥体")
	}
	if Acquire("RapidProxy-Test") {
		t.Fatal("第二次 Acquire 应返回 false（互斥体已存在）")
	}
}

// 互斥体名称应带 Local\ 前缀（用户会话级）。
func TestMutexName(t *testing.T) {
	got := mutexName("RapidProxy")
	want := `Local\RapidProxy-SingleInstance`
	if got != want {
		t.Fatalf("mutexName = %q, want %q", got, want)
	}
}
