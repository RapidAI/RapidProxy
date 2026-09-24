package winfit

import "testing"

func TestFitKeepsPreferredOnLargeScreen(t *testing.T) {
	preferred := Size{Width: 1080, Height: 740}
	floorMin := Size{Width: 940, Height: 600}

	// 2560x1440 @100%：空间充足，尺寸保持不变。
	size, min := Fit(preferred, floorMin, Size{Width: 2560, Height: 1440})
	if size != preferred {
		t.Errorf("大屏上尺寸应保持 %v，实际 %v", preferred, size)
	}
	if min != floorMin {
		t.Errorf("大屏上最小尺寸应保持 %v，实际 %v", floorMin, min)
	}
}

func TestFitShrinksOnHighDPIScreen(t *testing.T) {
	preferred := Size{Width: 1080, Height: 740}
	floorMin := Size{Width: 940, Height: 600}

	// 1920x1080 @150% 缩放：逻辑可用区 1280x720。
	// 原本 740 的逻辑高度会被放大到 1110 物理像素（超过 1080），必须收敛。
	size, min := Fit(preferred, floorMin, Size{Width: 1280, Height: 720})

	if size.Width != 1080 {
		t.Errorf("宽度应保持 1080，实际 %d", size.Width)
	}
	if size.Height != 720-MarginY {
		t.Errorf("高度应为 %d，实际 %d", 720-MarginY, size.Height)
	}
	if size.Height > 720 {
		t.Errorf("高度 %d 超出屏幕逻辑高度 720", size.Height)
	}
	if min.Height > size.Height || min.Width > size.Width {
		t.Errorf("最小尺寸 %v 不应大于实际尺寸 %v", min, size)
	}
	if min.Width != floorMin.Width {
		t.Errorf("屏幕够宽时最小宽度应保持 %d，实际 %d", floorMin.Width, min.Width)
	}
}

func TestFitRelaxesFloorOnTinyScreen(t *testing.T) {
	preferred := Size{Width: 1080, Height: 740}
	floorMin := Size{Width: 940, Height: 600}

	// 1366x768 @150% 缩放：逻辑可用区只有 911x512，比期望最小尺寸还小。
	size, min := Fit(preferred, floorMin, Size{Width: 911, Height: 512})

	if size.Width != 911-MarginX || size.Height != 512-MarginY {
		t.Fatalf("应完全收敛到可用区，实际 %v", size)
	}
	if min.Width > size.Width || min.Height > size.Height {
		t.Errorf("最小尺寸 %v 必须不大于实际尺寸 %v", min, size)
	}
	if min.Width != size.Width {
		t.Errorf("小屏上最小宽度应放宽到 %d，实际 %d", size.Width, min.Width)
	}
}

func TestFitNeverExceedsScreen(t *testing.T) {
	preferred := Size{Width: 1080, Height: 740}
	floorMin := Size{Width: 940, Height: 600}

	screens := []Size{
		{Width: 2560, Height: 1440},
		{Width: 1920, Height: 1080},
		{Width: 1440, Height: 900},
		{Width: 1366, Height: 768},
		{Width: 1280, Height: 800},
		{Width: 1024, Height: 768},
		{Width: 800, Height: 600},
	}
	for _, screen := range screens {
		size, min := Fit(preferred, floorMin, screen)
		if size.Width > screen.Width || size.Height > screen.Height {
			t.Errorf("屏幕 %v 上得到尺寸 %v，超出屏幕", screen, size)
		}
		if size.Width <= 0 || size.Height <= 0 {
			t.Errorf("屏幕 %v 上得到非正尺寸 %v", screen, size)
		}
		if min.Width > size.Width || min.Height > size.Height {
			t.Errorf("屏幕 %v 上最小尺寸 %v 大于实际尺寸 %v", screen, min, size)
		}
	}
}

func TestFitFallsBackWhenScreenUnknown(t *testing.T) {
	preferred := Size{Width: 1080, Height: 740}
	floorMin := Size{Width: 940, Height: 600}

	// 取不到屏幕信息时（Linux 无显示器、权限异常等）保持原尺寸。
	size, min := Fit(preferred, floorMin, Size{})
	if size != preferred {
		t.Errorf("未知屏幕时应保持 %v，实际 %v", preferred, size)
	}
	if min != floorMin {
		t.Errorf("未知屏幕时应保持最小尺寸 %v，实际 %v", floorMin, min)
	}
}
