package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 商城钱包（点券 / 代币券 / 欢乐代币券）的登录态重放。
//
// 客户端**没有任何"请求钱包"的上行包**：class0/0x0035（反编译里 handler 的 trace 名是
// NOTIFUNC_ENUM_NOTIPACKET_CERA，来自 GameCoreNetwork_NOTI_Town.cpp）一到，就把包体
// 三个 i32 直接写进客户端的三个字段：
//
//	[0] u8  flag
//	[1] i32 点券        (this+0x052B5DEC)
//	[5] i32 代币券      (this+0x052B5E0C, myCeraPoint)
//	[9] i32 欢乐代币券  (this+0x052B5E14, myEventCeraPoint)
//
// 共 13 字节。整包只有这 4 次读，没有别的分支。所以服务端不主动推送时，商城界面上
// 那三个数字永远是 0 —— 这正是现场"代币券只在背包里、商城不认"的一半原因。
//
// 本文件只做**重放**：把已经在数据库里的真实余额在合适时机再推一次。它不改余额、
// 不参与扣费（扣费走 cera_shop_purchase.go 的 Checkout），与既有的 860 限购投影同款：
// 纯展示，取不到账号就跳过，出错只记日志，绝不把回城/选角流程带崩。
//
// 时序：与 860 限购投影挂在同两个钩子上（选角进城的 op24 之后 / 副本返回城镇的 op24
// 之后），两处都是已经验证过的安全点。
const currentCeraShopWalletMsgID uint16 = 0x0035

// currentCeraShopClass0 是通知类（noti）接收表的分类号。客户端有两张独立的接收表，
// 同一个 opcode 在两张表里指向不同 handler（例：1035 在 class-0 是"多买多送已购买
// 次数"，在 class-1 是婚礼命令 ENUM_CMDPACKET_WEDDING_CHARAC 的回包）。商城这几个
// noti 全部注册在 class-0，所以这里必须发 0。
const currentCeraShopClass0 byte = 0

// sendCurrentCeraShopWalletProjection 把账号真实的三个钱包字段下发一次。
//
// 契约：
//   - session 为空 / 没选角色 / 仓储不可用 / 账号读不到 —— 一律静默返回 nil，不发包。
//     读不到账号时**必须不发**：零值会把客户端已经正确的余额覆盖成 0，比不发更糟。
//   - 发送失败返回 err，由调用方决定记日志还是吞掉。
func (s *Service) sendCurrentCeraShopWalletProjection(ctx context.Context, session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := s.accountIDForSession(session)
	if accountID == "" {
		return nil
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil || !found {
		// 读失败与新账号一样按"未知"处理：不发，保留客户端现值。
		return nil
	}
	wallet := currentAccountWallet(account)
	body := buildCurrentCeraShopWalletBody(wallet.Cera, wallet.TokenCera, wallet.HappyTokenCera)
	if err := s.sendGameUpperRawClass(session, currentCeraShopWalletMsgID, body, currentCeraShopClass0); err != nil {
		return err
	}
	s.logPacketEvent("game-cera-shop-wallet-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"account_id", accountID,
		"source", source,
		"cera", wallet.Cera,
		"token_cera", wallet.TokenCera,
		"happy_token_cera", wallet.HappyTokenCera,
		"body_len", len(body),
		"wire", "class0/op53-u8-i32-i32-i32")
	return nil
}

// sendCurrentCeraShopGoldWallet 在结账后把**扣过金币**的余额推给客户端。
//
// 金币标价商品扣的是角色金币，与上面三个点券钱包不是同一套东西，客户端也不会因为
// 收到 op64 成功包就自己去减——金币是服务端权威值，只有 op14（list0 的
// slot0/item0/count=金币，即 buildCurrentGoldStateBody）能让钱包立刻显示新余额。
// 不发它，玩家看到的是"买了东西金币一点没少"，小退或做一次别的金币操作才发现被扣。
// 这是本仓库的既有约定（拍卖 / 公会 / 邮件 / 深渊祭坛 / 私聊商店等 30 多处都发这一包）。
//
// PaidGold 为 0（点券标价商品、免费行）时**一个字节都不发**：既有链路完全不变。
func (s *Service) sendCurrentCeraShopGoldWallet(session *gameSession, result currentCeraShopCommitResult) error {
	if result.PaidGold == 0 {
		return nil
	}
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		buildCurrentGoldStateBody(result.GoldAfter),
		currentCeraShopClass0,
	)
}
