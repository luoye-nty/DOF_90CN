package main

// 主窗口尺寸自适应的回归测试。
//
// 背景：窗口尺寸写死 1280x768 时，在 768 高的屏幕上"主界面底部按钮点不到"。
// 这里钉住三件事：
//  1. 小屏（1366x768 / 1280x720）算出来的高度必须**小于屏幕高度**（留出任务栏+标题栏）；
//  2. 大屏不能铺满（保持原来的观感，不超过 1280x760）；
//  3. 最小尺寸永远不大于默认尺寸（否则 MinSize 会把窗口顶出屏幕）。

import "testing"

func TestClampWindowSize(t *testing.T) {
	cases := []struct {
		name     string
		screenW  int
		screenH  int
		wantW    int
		wantH    int
		wantMinW int
		wantMinH int
	}{
		{"1920x1080：用默认上限 1280x760", 1920, 1080, 1280, 760, 900, 520},
		{"1366x768：必须比屏高小一截", 1366, 768, 1280, 688, 900, 520},
		{"1280x720：宽度留边距后是 1240", 1280, 720, 1240, 640, 900, 520},
		{"1024x600 小屏：跟着屏幕缩", 1024, 600, 984, 520, 900, 520},
		{"800x600 极小屏：绝不能比屏幕还大", 800, 600, 760, 520, 760, 520},
		{"640x480 极小屏：最小尺寸也一起缩", 640, 480, 600, 400, 600, 400},
	}
	for _, tc := range cases {
		size, minSize := clampWindowSize(tc.screenW, tc.screenH)
		if size.Width != tc.wantW || size.Height != tc.wantH {
			t.Fatalf("%s: size = %dx%d，期望 %dx%d", tc.name, size.Width, size.Height, tc.wantW, tc.wantH)
		}
		if minSize.Width != tc.wantMinW || minSize.Height != tc.wantMinH {
			t.Fatalf("%s: minSize = %dx%d，期望 %dx%d", tc.name, minSize.Width, minSize.Height, tc.wantMinW, tc.wantMinH)
		}
		// 最关键的不变量：窗口高度必须比屏幕矮（否则底部被任务栏挡住）。
		if size.Height > tc.screenH {
			t.Fatalf("%s: 窗口高 %d 超过屏幕高 %d", tc.name, size.Height, tc.screenH)
		}
		if tc.screenH > winFallbackHeight && size.Height >= tc.screenH {
			t.Fatalf("%s: 窗口高 %d 没有给任务栏留余量（屏高 %d）", tc.name, size.Height, tc.screenH)
		}
		// MinSize 不能大于 Size，否则窗口会被顶出屏幕。
		if minSize.Width > size.Width || minSize.Height > size.Height {
			t.Fatalf("%s: minSize(%dx%d) 大于 size(%dx%d)", tc.name,
				minSize.Width, minSize.Height, size.Width, size.Height)
		}
	}
}

// TestDesktopWindowSizeSane：本机取到的尺寸必须落在预设区间内（取不到屏幕时兜底也不能是 0）。
func TestDesktopWindowSizeSane(t *testing.T) {
	size, minSize := desktopWindowSize()
	if size.Width < winMinWidth || size.Height < winMinHeight {
		t.Fatalf("默认尺寸过小: %dx%d", size.Width, size.Height)
	}
	if size.Width > winDefaultMaxWidth || size.Height > winDefaultMaxHeight {
		t.Fatalf("默认尺寸超过上限: %dx%d", size.Width, size.Height)
	}
	if minSize.Width > size.Width || minSize.Height > size.Height {
		t.Fatalf("minSize(%dx%d) 大于 size(%dx%d)", minSize.Width, minSize.Height, size.Width, size.Height)
	}
	t.Logf("本机窗口尺寸 = %dx%d，最小 = %dx%d", size.Width, size.Height, minSize.Width, minSize.Height)
}
