// Package winfit 根据屏幕尺寸计算窗口应该使用的尺寸。
//
// 背景：Wails 的窗口宽高是「逻辑像素」（96 DPI 空间），创建窗口时会按屏幕 DPI
// 换算成物理像素。举例：系统缩放 150%、分辨率 1920x1080 的屏幕，逻辑尺寸
// 1080x740 会被放大成 1620x1110 物理像素，高度超过屏幕，于是窗口上下都跑到
// 屏幕外面。这里按屏幕的逻辑尺寸收敛窗口尺寸，保证窗口始终完整可见。
//
// 屏幕的逻辑尺寸取自 Wails 运行时的 Screen.Size（Windows 是物理像素按 DPI
// 换算回 96 DPI，macOS/Linux 直接是逻辑坐标），单位与 WindowSetSize 一致。
package winfit

const (
	// MarginX 是窗口左右预留的逻辑像素（避免窗口贴边）。
	MarginX = 48
	// MarginY 是窗口上下预留的逻辑像素：
	// Windows 任务栏约 48、macOS 菜单栏 24 + Dock、Linux 顶栏/底栏。
	MarginY = 96
)

// Size 是窗口的逻辑尺寸。
type Size struct {
	Width  int
	Height int
}

// Fit 把 preferred 收敛到 screen 的可用范围内，同时给出对应的最小尺寸。
//
//   - screen 是屏幕的逻辑尺寸（含任务栏区域）。
//   - floorMin 是正常情况下允许的最小尺寸；屏幕过小时会自动放宽，
//     否则用户永远无法把窗口缩小到屏幕内。
//
// 返回的 size 一定不超过可用区域，min 一定不超过 size。
func Fit(preferred, floorMin, screen Size) (size Size, min Size) {
	availW := screen.Width - MarginX
	availH := screen.Height - MarginY
	// 屏幕尺寸拿不到（等于 0 或异常小）时不做收敛，保持原样。
	if availW <= 0 {
		availW = preferred.Width
	}
	if availH <= 0 {
		availH = preferred.Height
	}

	size.Width = clamp(preferred.Width, lowerBound(floorMin.Width, availW), availW)
	size.Height = clamp(preferred.Height, lowerBound(floorMin.Height, availH), availH)

	min.Width = clamp(floorMin.Width, 1, size.Width)
	min.Height = clamp(floorMin.Height, 1, size.Height)
	return size, min
}

// lowerBound 是「期望最小尺寸」与「屏幕可用尺寸」中较小的那个：
// 屏幕够大时用期望值，屏幕太小就退让到屏幕可用尺寸。
func lowerBound(want, avail int) int {
	if want < avail {
		return want
	}
	return avail
}

func clamp(v, low, high int) int {
	if high < low {
		high = low
	}
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
