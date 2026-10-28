package dnfbridge

import (
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func (s *Service) sendCurrentCeraShopPurchaseFailure(session *gameSession) error {
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		buildCurrentCeraShopPurchaseFailureBody(),
		dnfproto.DefaultChannelClassification,
	)
}

// 90CN 商城**购买失败**回执码表：客户端按 `269 - code` 查这张表（**反序**）。
//
// ★ 这条规律是两次真机试码试出来的，不是抄来的：
//
//	2026-09-27 17:55  服务端回 code=11 → 客户端弹「错误 : 点券不足」= 字符串 258
//	2026-09-27 18:1x  服务端回 code=12 → 客户端弹「错误 : 胜点不足」= 字符串 257
//
// 码 +1、字符串 −1 ⇒ `字符串id = 269 - code`（等价地 `code = 269 - 字符串id`）。
// 我先前猜的 `247 + code` 在 code 12 上应该给出 259「Lv不足」，被第二次实测否掉。
//
// 客户端能显示的整张表（字符串 253..265，把 269 减过去就是码）：
//
//	id 253 无法购买的物品        → code 16
//	id 254 错误的商品编号        → code 15
//	id 255 物品栏空间不足        → code 14
//	id 256 金币不足              → code 13
//	id 257 胜点不足              → code 12   ← 实测确认
//	id 258 点券不足              → code 11   ← 实测确认
//	id 259 Lv不足                → code 10
//	id 260 不存在的物品          → code  9
//	id 261 使用期限设置错误      → code  8
//	id 262 物品功能设置错误      → code  7
//	id 263 付费服务器错误        → code  6
//	id 264 请求购买信息错误      → code  5
//	id 265 未知错误              → code  4
//
// ⚠⚠ 另一张表 `21969 + code`（21979 该商品无法赠送 / 21980 超出赠送金额上限 /
// 21981 年龄不符合赠送条件 …）**op64 的回包够不到**：全客户端只有一处引用 21972
// （RVA 0x00B0494D），而且在一个 `arg2 == 13` 的充值/商城页对话框里，不是按 code
// 索引的购买回执表。用它的码去回，客户端会查到上面这张表里的**另一句话**。
//
// 后果：op64 阶段**表达不了赠送专用文案**（没有"赠送金额上限""年龄不符合赠送条件"
// 这种句子）。所以约定是：**精确文案一律放在 op136 那一步**（那张表有
// 70107/70108/70109 这些专用句子，好友 7 天、每日 10 万额度都在那一步回），
// op64 只回这张表里语义最接近的一条，真实原因看服务端日志的 failure_code + reason。
const (
	// currentCeraShopPurchaseFailureCodeUnknown 4 = 未知错误（畸形包、内部错误）。
	currentCeraShopPurchaseFailureCodeUnknown byte = 4
	// currentCeraShopPurchaseFailureCodeConnection 5 = 请求购买信息错误（瞬时故障）。
	currentCeraShopPurchaseFailureCodeConnection byte = 5
	// currentCeraShopPurchaseFailureCodeWrongCommodity 15 = 错误的商品编号。
	currentCeraShopPurchaseFailureCodeWrongCommodity byte = 15
	// currentCeraShopPurchaseFailureCodeInventoryFull 14 = 物品栏空间不足。
	currentCeraShopPurchaseFailureCodeInventoryFull byte = 14
	// currentCeraShopPurchaseFailureCodeInsufficientGold 13 = 金币不足。
	currentCeraShopPurchaseFailureCodeInsufficientGold byte = 13
	// currentCeraShopPurchaseFailureCodeInsufficientCera 11 = 点券不足（实测确认）。
	currentCeraShopPurchaseFailureCodeInsufficientCera byte = 11
	// currentCeraShopPurchaseFailureCodeLevelTooLow 10 = Lv不足。
	currentCeraShopPurchaseFailureCodeLevelTooLow byte = 10
	// currentCeraShopPurchaseFailureCodeCannotBuy 16 = 无法购买的物品。
	// 这是**兜底**码：语义最宽的"这一单买不成"，任何没被单独映射的失败都用它。
	currentCeraShopPurchaseFailureCodeCannotBuy byte = 16
)

// currentCeraShopPurchaseLimitFailureCode 是「商城限购已满」回给客户端的失败码。
//
// 限购在这张表里没有专用文案，用兜底码（无法购买的物品）：客户端会弹一个明确提示
// 而不是静默。真实原因在 `game-cera-shop-purchase-limit-rejected` 日志里可查。
const currentCeraShopPurchaseLimitFailureCode byte = currentCeraShopPurchaseFailureCodeCannotBuy

// currentCeraShopPurchaseFailureCodeGiftUnsupported 是「这件商品不能赠送」的回执码。
//
// 这张表里没有"该商品无法赠送"这句话（那句在另一张表、op64 够不到），只能用兜底码。
const currentCeraShopPurchaseFailureCodeGiftUnsupported byte = currentCeraShopPurchaseFailureCodeCannotBuy

// 赠送规则在 op64 阶段的回执码：
//
//	除"每日额度用完"外 → 16（无法购买的物品）
//
// ⚠ **op64 阶段表达不了赠送专用的文案**：能查的那张表只有 253..265 这 13 句通用购买
// 错误（见文件顶部），"该商品无法赠送""年龄不符合赠送条件"这些句子在另一张表里、
// op64 的回包够不到。所以约定是：**精确文案一律在 op136 阶段给**（0xA4 好友不满 7 天、
// 0xA5 不是好友、0xA3 每日额度用完，各有专用句子），op64 只回语义最接近的一条，
// 真实原因看服务端日志（`failure_code` + `reason`）。
const (
	// currentCeraShopPurchaseFailureCodeGiftNotGiftable 商品不可邮寄 / 收件人职业不符。
	currentCeraShopPurchaseFailureCodeGiftNotGiftable byte = currentCeraShopPurchaseFailureCodeCannotBuy
	// currentCeraShopPurchaseFailureCodeGiftAccountTooNew 赠送方账号不满 7 天。
	currentCeraShopPurchaseFailureCodeGiftAccountTooNew byte = currentCeraShopPurchaseFailureCodeCannotBuy
	// currentCeraShopPurchaseFailureCodeGiftForbidden 其余赠送判定失败（好友/大区/自身）。
	currentCeraShopPurchaseFailureCodeGiftForbidden byte = currentCeraShopPurchaseFailureCodeCannotBuy
	// currentCeraShopPurchaseFailureCodeGiftAmountCap 每日赠送额度用完。
	//
	// 这张表里没有"超出赠送金额上限"这句话（那句在另一张表、op64 够不到），
	// 所以用兜底码「无法购买的物品」。精确的"每天最多10万"提示在 op136 阶段给（0xA3）。
	currentCeraShopPurchaseFailureCodeGiftAmountCap byte = currentCeraShopPurchaseFailureCodeCannotBuy
)

// sendCurrentCeraShopPurchaseFailureWithCode 用指定码回 op64 失败包。
func (s *Service) sendCurrentCeraShopPurchaseFailureWithCode(session *gameSession, code byte) error {
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		buildCurrentCeraShopPurchaseFailureBodyWithCode(code),
		dnfproto.DefaultChannelClassification,
	)
}

// sendCurrentCeraShopGiftFailure 用指定的失败码回 op64 失败包（赠送路径专用）。
//
// 支持用试码文件临时覆盖（见 cera_shop_gift_limit.go 的说明）：真机定位"哪个码显示
// 哪句话"时不必重新编译。每次发送都会把**实际发出的码**记进日志，方便和玩家看到的
// 提示一一对应。
func (s *Service) sendCurrentCeraShopGiftFailure(session *gameSession, code byte) error {
	sent := code
	if override, ok := currentCeraShopGiftFailureCodeProbe(); ok {
		sent = override
	}
	if s != nil {
		s.logGameEvent(session, "game-cera-shop-gift-failure-acked",
			"char_id", 0,
			"mapped_code", code,
			"sent_code", sent,
			"probe_override", sent != code,
			"body_hex", fmt.Sprintf("%x", buildCurrentCeraShopPurchaseFailureBodyWithCode(sent)))
	}
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		buildCurrentCeraShopPurchaseFailureBodyWithCode(sent),
		dnfproto.DefaultChannelClassification,
	)
}

// sendCurrentCeraShopGiftUnsupportedFailure 在"赠送形态收到但本服还不能投递"时回失败。
func (s *Service) sendCurrentCeraShopGiftUnsupportedFailure(session *gameSession) error {
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseFailureCodeGiftUnsupported),
		dnfproto.DefaultChannelClassification,
	)
}

// sendCurrentCeraShopPurchaseLimitFailure 在限购已满时回失败包。
func (s *Service) sendCurrentCeraShopPurchaseLimitFailure(session *gameSession) error {
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseLimitFailureCode),
		dnfproto.DefaultChannelClassification,
	)
}

func buildCurrentCeraShopPurchaseSuccessBody(commodityID uint32, bonuses ...currentCeraShopBonusItem) []byte {
	return buildCurrentCeraShopPurchaseSuccessBodyWithCount(commodityID, 0, bonuses...)
}

// buildCurrentCeraShopPurchaseSuccessBodyWithCount keeps the three trailing
// u32 fields zero.  2026-07-25 live evidence: writing the purchased quantity
// into the first trailing u32 hangs the result dialog (client left
// unclickable), so those fields are not the row counts.  Quantity display
// remains an open investigation; the zero shape is the proven safe one.
func buildCurrentCeraShopPurchaseSuccessBodyWithCount(commodityID uint32, count uint32, bonuses ...currentCeraShopBonusItem) []byte {
	_ = count
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(0)
	writer.writeInt32(-1) // category=-1: current reader performs a safe full catalog lookup.
	writer.writeUint32(commodityID)
	writer.writeUint32(0)
	writer.writeUint32(0)
	writer.writeUint32(0)
	// Bonus display rows drive the dialog's right-side 商城购物奖励 panel
	// (one-plus-one IPG gifts, e.g. 充满爱慕的信 -> 泰迪礼盒 + 幸运魔锤).
	// Row layout u32 itemID + u32 count.
	writer.writeUint16(uint16(len(bonuses)))
	for _, bonus := range bonuses {
		writer.writeUint32(bonus.ItemID)
		writer.writeUint32(bonus.Count)
	}
	return writer.bytes()
}

func buildCurrentCeraShopPurchaseFailureBody() []byte {
	return buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseFailureCodeCannotBuy)
}

// buildCurrentCeraShopPurchaseFailureBodyWithCode 生成商城购买失败包。形状固定为
// `u8 0 + u8 失败码 + 5×u32`（见 86 参考对 sub_CD9490 失败分支的逆向：字段太少会
// 让客户端读到下一个包的垃圾当索引而崩）。
func buildCurrentCeraShopPurchaseFailureBodyWithCode(errorCode byte) []byte {
	var writer packetWriter
	writer.writeByte(0)
	writer.writeByte(errorCode)
	writer.writeInt32(-1)
	writer.writeInt32(-1)
	writer.writeUint32(0)
	writer.writeUint32(0)
	writer.writeUint32(0)
	return writer.bytes()
}

func buildCurrentCeraShopBalanceBody(cera int64) []byte {
	return buildCurrentCeraShopWalletBody(cera, 0, 0)
}

func buildCurrentCeraShopWalletBody(cera, tokenCera, happyTokenCera int64) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt32(clampCurrentCeraBalance(cera))
	writer.writeInt32(clampCurrentCeraBalance(tokenCera))
	writer.writeInt32(clampCurrentCeraBalance(happyTokenCera))
	return writer.bytes()
}

func clampCurrentCeraBalance(value int64) int {
	if value < 0 {
		return 0
	}
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(value)
}
