package main

import (
	"strings"
	"testing"

	"github.com/znsoftm/RapidProxy/internal/winfit"
)

func TestScreenDescOmitsScaleAt100Percent(t *testing.T) {
	got := screenDesc(winfit.Size{Width: 1920, Height: 1080}, winfit.Size{Width: 1920, Height: 1080})
	if got != "1920x1080" {
		t.Errorf("100%% 缩放时不需要附带缩放说明，实际 %q", got)
	}
}

func TestScreenDescShowsScale(t *testing.T) {
	// 3840x2160 的显示器按 150% 缩放，对程序来说就是 2560x1440 个逻辑像素。
	got := screenDesc(winfit.Size{Width: 2560, Height: 1440}, winfit.Size{Width: 3840, Height: 2160})
	if !strings.Contains(got, "3840x2160") || !strings.Contains(got, "150%") {
		t.Errorf("应同时给出物理尺寸与缩放比例，实际 %q", got)
	}
}

func TestScreenDescFallsBackWhenPhysicalUnknown(t *testing.T) {
	got := screenDesc(winfit.Size{Width: 1280, Height: 720}, winfit.Size{})
	if got != "1280x720" {
		t.Errorf("物理尺寸未知时只输出逻辑尺寸，实际 %q", got)
	}
}

func TestWindowDefaultsAreSane(t *testing.T) {
	if windowDefaultWidth < windowMinWidth || windowDefaultHeight < windowMinHeight {
		t.Errorf("默认尺寸 %dx%d 不应小于最小尺寸 %dx%d",
			windowDefaultWidth, windowDefaultHeight, windowMinWidth, windowMinHeight)
	}
	if windowMinWidth <= 0 || windowMinHeight <= 0 {
		t.Error("最小尺寸必须为正数")
	}
}

// maskKey 处理的是用户输入的密钥，长度不可信，短 key 不能越界 panic。
func TestMaskKeyHandlesShortKeys(t *testing.T) {
	for _, key := range []string{"", "a", "ab", "sk-1", "sk-wbp-0123456789abcdef0123456789abcdef"} {
		got := maskKey(key)
		if !strings.Contains(got, "****") {
			t.Errorf("maskKey(%q) = %q，未做掩码", key, got)
		}
	}
	if got := maskKey("sk-wbp-0123456789abcdef0123456789abcdef"); got != "sk-wbp-012"+"****"+"cdef" {
		t.Errorf("长密钥掩码不符合预期: %q", got)
	}
}

func TestSameListenAddr(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:8787", "127.0.0.1:8787", true},
		{":8787", "[::]:8787", true}, // 都表示监听所有网卡
		{"0.0.0.0:8787", "[::]:8787", true},
		{"127.0.0.1:8787", "[::]:8787", false},
		{"127.0.0.1:8787", "127.0.0.1:8788", false},
		{"127.0.0.1:8787", "not-an-address", false},
	}
	for _, tc := range cases {
		if got := sameListenAddr(tc.a, tc.b); got != tc.want {
			t.Errorf("sameListenAddr(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}
