package dnfbridge

// The magic-seal panel renders an op401/op437 failure ACK as a silent no-op, so
// the explanation goes through the shared notice router in notice.go. The
// surface is the client's own NOTI 12 on the 系统 tab in magenta: a per-player
// hint, easy to spot, and readable while the panel still has focus. The owned
// op413 overlay would cover the panel the player is looking at, which is why
// this caller stays on a chat surface even though the overlay is now available.
var currentRandomOptionGoldInsufficientNoticeSurface = noticeSurfaceSystemMagenta

func currentRandomOptionGoldInsufficientNoticeText(operation string) string {
	if operation == "change_random_option" {
		return "金币不足，无法变更魔法封印属性。"
	}
	return "金币不足，无法解除魔法封印。"
}

func (s *Service) sendCurrentRandomOptionGoldInsufficientNotice(session *gameSession, operation string) error {
	return s.sendNotice(session, noticeRequest{
		Surface: currentRandomOptionGoldInsufficientNoticeSurface,
		Text:    currentRandomOptionGoldInsufficientNoticeText(operation),
		Source:  "random_option_gold_insufficient",
	})
}

// 【op455 成功提示】
//
// 品级调整（op81）的"已成功重调该物品的品级"是客户端本地弹的：服务端只回一个
// class-1/op81 成功帧，日志里没有任何提示包。封印属性品级调整（op455）没有这个
// 待遇 —— 当前客户端给 op455 注册的处理器 0x2137820 只在失败时按错误码弹提示
// （cmp byte [ebp+0xc],0 / jne 直接 ret），成功分支是空的。所以这里由服务端补一条
// 与 op81 措辞对齐的系统提示。
//
// 落点用 系统 页（浅蓝）而不是金币不足提示用的洋红：这是"操作成功"的常规反馈，
// 要和品级调整那条一模一样地出现在同一个位置，洋红留给需要玩家注意的失败提示。
var currentRandomOptionResetSuccessNoticeSurface = noticeSurfaceSystemTab

func currentRandomOptionResetSuccessNoticeText() string {
	return "已成功重调该物品的封印属性品级。"
}

func (s *Service) sendCurrentRandomOptionResetSuccessNotice(session *gameSession) error {
	return s.sendNotice(session, noticeRequest{
		Surface: currentRandomOptionResetSuccessNoticeSurface,
		Text:    currentRandomOptionResetSuccessNoticeText(),
		Source:  "random_option_reset_success",
	})
}
