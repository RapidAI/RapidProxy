//go:build windows

package singleton

import (
	"testing"
	"time"
)

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

// 激活管道名应与互斥体同前缀，便于在管道列表里排查。
func TestPipeName(t *testing.T) {
	got := pipeName("RapidProxy")
	want := `\\.\pipe\RapidProxy-SingleInstance-Activate`
	if got != want {
		t.Fatalf("pipeName = %q, want %q", got, want)
	}
}

// 端到端：Listen 之后 Activate 应触发已有实例的激活回调（模拟第二实例）。
func TestListenActivate(t *testing.T) {
	ch := make(chan struct{}, 1)
	Listen("RapidProxy-Test-Activate", func() { ch <- struct{}{} })

	// 服务端建好管道之前 Activate 可能连不上，重试直到送达。
	delivered := false
	for start := time.Now(); time.Since(start) < 3*time.Second; {
		if Activate("RapidProxy-Test-Activate") {
			delivered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !delivered {
		t.Fatal("Activate 应能送达（激活管道已在监听）")
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("激活回调未被触发")
	}
}
