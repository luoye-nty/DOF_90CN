package main

// 主窗口尺寸自适应：
//
// 以前写死 Size 1280x768 / MinSize 1000x700 —— 在 768 高的屏幕上，
// 窗口高度 + 标题栏 + 任务栏已经超出屏幕，底部的按钮直接点不到。
//
// 现在的规则（纯函数 clampWindowSize，便于单测）：
//   - 默认尺寸 = 屏幕尺寸留 40px 横边距、80px 竖边距（任务栏 + 标题栏），
//     再夹到不超过 1280x760 —— 大屏不铺满（保持原来的观感），小屏一定装得下；
//   - 最小尺寸 = min(900x520, 默认尺寸)，避免 MinSize 比 Size 还大把窗口顶出屏幕。
//
// 注意这里**没有**"默认尺寸下限"：第一版写了"不小于 960x560"，结果 800x600 的屏幕
// 上算出 960x560（比屏幕还大）—— 下限必须有，但只能是"不能为负"，不能是"不能太小"。
// 单测把这条钉住了。
//
// 屏幕尺寸取主屏像素（GetSystemMetrics）。刻意不用工作区 API：lxn/win 没导出
// SPI_GETWORKAREA，为此引 unsafe 不值得，留 80px 余量足够覆盖常见任务栏高度。

import (
	decl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// 默认尺寸上限、屏幕留白、最小尺寸下限，以及屏幕信息取不到时的兜底值。
const (
	winDefaultMaxWidth  = 1280
	winDefaultMaxHeight = 760
	winSizeMarginX      = 40
	winSizeMarginY      = 80
	winMinWidth         = 900
	winMinHeight        = 520
	winFallbackWidth    = 1280
	winFallbackHeight   = 720
)

// clampWindowSize 由屏幕像素算出窗口的默认尺寸与最小尺寸（纯函数）。
func clampWindowSize(screenW, screenH int) (size decl.Size, minSize decl.Size) {
	availW := screenW - winSizeMarginX
	availH := screenH - winSizeMarginY
	if availW <= 0 {
		availW = winFallbackWidth - winSizeMarginX
	}
	if availH <= 0 {
		availH = winFallbackHeight - winSizeMarginY
	}

	w := availW
	if w > winDefaultMaxWidth {
		w = winDefaultMaxWidth
	}
	h := availH
	if h > winDefaultMaxHeight {
		h = winDefaultMaxHeight
	}

	minW, minH := winMinWidth, winMinHeight
	if w < minW {
		minW = w
	}
	if h < minH {
		minH = h
	}
	return decl.Size{Width: w, Height: h}, decl.Size{Width: minW, Height: minH}
}

// desktopWindowSize 取主屏尺寸并算出窗口尺寸；取不到时按 1280x720 兜底。
func desktopWindowSize() (size decl.Size, minSize decl.Size) {
	screenW := int(win.GetSystemMetrics(win.SM_CXSCREEN))
	screenH := int(win.GetSystemMetrics(win.SM_CYSCREEN))
	if screenW <= 0 || screenH <= 0 {
		screenW, screenH = winFallbackWidth, winFallbackHeight
	}
	return clampWindowSize(screenW, screenH)
}
