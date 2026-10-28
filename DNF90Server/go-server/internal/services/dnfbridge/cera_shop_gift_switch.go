package dnfbridge

// 商城赠送功能总开关。
//
// 2026-09-27：运营要求**暂时关闭**赠送功能 —— 关掉之后：
//
//	op136（查收件人）：不再查库、不判断任何规则，直接给一条服务端提示 + 失败回包
//	op64 （真下单）  ：同样直接拒绝，不走结账、不扣点券、不发邮件
//
// 两处都要拦：只拦 op136 的话，改过的客户端可以直接发 op64 下单（服务端本来就不该
// 信任客户端走过的步骤）。
//
// 提示走**服务端自己的公告通道**（NOTI 12，系统页洋红），而不是客户端的结果码表 ——
// 客户端那张 op136 结果码表只有 6 个槽位（无法对自身赠送礼物 / 请稍后再使用 /
// 该ID不存在 / 每天10万点券 / 成为好友7天后 / 还不是好友），**没有"功能关闭"这句话**，
// 用它只能显示一句不相干的提示。公告通道可以发任意文本。
//
// 重新开启：把下面这个变量改回 true 即可（其余代码不用动）。
//
// 是 var 而不是 const：用例要在"开/关"两种状态下各跑一遍（默认按**开启**跑，
// 开关用例自己临时关掉），生产默认值是 false。
var currentCeraShopGiftEnabled = false

// currentCeraShopGiftClosedNoticeText 是关闭期间给玩家的提示语。
const currentCeraShopGiftClosedNoticeText = "赠送功能暂时关闭，敬请期待。"

// currentCeraShopGiftClosedNoticeSurface 选系统页洋红（和"金币不足"那条同一个落点）：
// 这是需要玩家注意到的一次性拒绝，不是常规反馈。
var currentCeraShopGiftClosedNoticeSurface = noticeSurfaceSystemMagenta

// sendCurrentCeraShopGiftClosedNotice 下发"赠送功能暂时关闭"。
//
// 公告只是提示，发失败也不能影响回包：调用方拿到 error 时只记日志。
func (s *Service) sendCurrentCeraShopGiftClosedNotice(session *gameSession, source string) error {
	return s.sendNotice(session, noticeRequest{
		Surface: currentCeraShopGiftClosedNoticeSurface,
		Text:    currentCeraShopGiftClosedNoticeText,
		Source:  source,
	})
}

// currentCeraShopGiftDisabledFailureCode 是关闭期间回的 op136 结果码。
//
// 用 2（请稍后再使用）：它在客户端那张表里存在、语义就是"现在不行"。**必须回包**：
// 不回的话客户端会停在等待状态（这正是 2026-09-27 那次"确认收件人后没反应"的成因）。
const currentCeraShopGiftDisabledFailureCode = currentCeraShopGiftQueryCodeRetry
