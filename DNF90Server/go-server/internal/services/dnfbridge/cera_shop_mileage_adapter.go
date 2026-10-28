package dnfbridge

import "context"

// 里程商店（商城 shop type 4）在本树**尚未实现**，这里只为在线活动初始化链路
// 提供一个显式空实现。
//
// 为什么需要这个文件：本树把 online_event_spend_time.go 对齐到了参考实现，
// 而参考版在一次 op108 之后会顺带把三样"活动集合"相关的商城状态各推一次：
//
//	cera_shop_buycount.go  -> sendCurrentCeraShopBuyCountInitialState（已由本树
//	                          的 sendCurrentCeraShopBuyCountGiftProjection 承接）
//	cera_shop_mileage.go   -> sendCurrentMileageInitialState（本文件）
//	item_lock_snapshot.go  -> refreshCurrentExpiredItemLocks（本树已有等价实现）
//
// 参考版的里程实现是**一整套独立特性**：`cera_shop_mileage.go`（里程钱包/余额/
// 变更推送/购买上限）+ `cerashop` 模块层的 `ReadMileage`/`MileagePurchaseCounts`/
// `mileageLimits` + 事件 266 的 op108 base row。本树既没有这条路（模块层没有
// ReadMileage/MileagePurchaseCounts，`cerashop` 只有点券钱包 Wallet/余额/Payment），
// 也没有 event 266 的目录行，所以：
//
//   - 这里**不做降级模拟**（不发一个 427 零余额、也不发空上限表）——那会让客户端
//     渲染出一个"里程 0"的商店分类，而服务端根本不认里程消费，比不响应更误导；
//   - 真要开里程商店，应当把参考版整套（模块层 + bridge + 事件 266 目录行 + PVF
//     的里程档位）一起移植，而不是在这里补一半。
//
// 换句话说：这是"刻意留空并留痕"，不是遗漏。
func (s *Service) sendCurrentMileageInitialState(_ context.Context, _ *gameSession) error {
	return nil
}
