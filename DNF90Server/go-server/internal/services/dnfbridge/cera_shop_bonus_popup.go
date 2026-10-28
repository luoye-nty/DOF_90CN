package dnfbridge

import (
	"encoding/binary"
)

// 商城"赠品到账"弹窗（**`class0/op852`**，handler `FUN_01d81250`，
// trace 名 `NOTIFUNC_ENUM_NOTIPACKET_CERASHOP_BONUS_ITEM`）。
//
// ⚠⚠ opcode 是 **852（0x0354）**，**不是 0x02D2**。
//
// 这里必须先记一笔踩坑史：早先的记录把这一包写成 "0x02D2 (=722)"，于是服务端一直在发
// **722**。而 722 在本 build 里**根本没有注册**（`tools/_bcg_registry.txt` 里是
// `op 722 -- NOT REGISTERED --`），客户端收到未注册 opcode 直接丢弃 —— 现象就是
// "日志全对、参数全对、客户端什么都不弹"。
//
// 真身来自**注册点**（指令级）：
//
//	0x01D92F3A  push 0x1D81250     ; handler = FUN_01d81250
//	0x01D92F3F  push 0x354         ; opcode = 852
//	0x01D92F44  mov  ecx, esi
//	0x01D92F46  call 0x02262980    ; 插进 class-0 表（注册器 FUN_01d927e0）
//
// 而 0x02D2 这个数字本身是**从枚举名表推出来的** —— 本规格附录 E.4 早就写过
// "枚举名对应的是名字的哈希、不是数组下标；名表推 opcode 从根上不成立，opcode 只能
// 从注册点读"。这条教训在自己身上又应验了一次：**永远从注册点取 opcode**。
//
// 包体（反编译 `FUN_01d81250`）：
//
//	local_15 = 0;
//	FUN_034e3970(&local_15, 1);        // 读 1 字节 flag
//	if (local_15 == 0) return;
//	FUN_034e3ac0(&local_14, 0xc);      // 再读 **12 字节**（3 个 u32）
//	trace("NOTIFUNC_ENUM_NOTIPACKET_CERASHOP_BONUS_ITEM",
//	      "세라보너스 아이템 =  itemIndex %d itemNumber %d", local_10, local_c);
//	if (local_10 != -1) {
//	    manager = FUN_02fd3f10();      // 商城管理器单例
//	    FUN_016c1080(manager, ...);    // 写进窗口
//	}
//
// 于是包体 = `u8 flag` + `u32` + `u32 itemIndex` + `u32 itemNumber` = **13 字节**。
//
// ⚠ 这**推翻**了更早"9 字节"的记录 —— 那个结论只数到了两个 `0x034E3A50`（4 字节读），
// 漏掉了 `0x034E3AC0(dst, 0xc)` 这个批量读原语。
//
// ⚠ 13 字节里的第一个 u32（下称 A）**已经证实**：
//
//   - 本 build 反编译：`FUN_016c1080(manager, itemIndex, itemNumber, A)` 里
//     `switch (A) { case 0..7: 普通商城结果路径; default: 专用奖励活动面板 }`；
//
//   - 86JP 参考服务端 `CeraShopBonusItemBuilder.cs`：
//
//     public const int RewardActivityDisplayType = 8;
//     // 0..7 走普通商城结果上下文；>7 打开**专用的原生奖励活动面板**
//     WriteByte(1); WriteInt32(RewardActivityDisplayType);
//     WriteInt32(itemTemplateId); WriteInt32(itemCount);
//
//     即 A = 8、`itemIndex` = 奖励**道具号**、`itemNumber` = 奖励**数量**。
//     与本 build 反编译出的"13 字节 / case 0..7 / default 特殊面板"逐字节吻合。
//
// 发送时机同样是命门（见 cera_shop_purchase.go 的调用点）：这一包**必须紧跟购买回包
// `class1/op64`**。它不是独立弹窗，而是挂到"购买结果"窗口上、从它下面**滑下来**的
// "奖励活动"面板；客户端的购买结果窗口是在收到 op64 时建立的，排在后面就没了宿主。
const (
	// currentCeraShopBonusItemMsgID 是"赠品到账"类 0 通知的 opcode，**取自注册点**。
	currentCeraShopBonusItemMsgID uint16 = 0x0354

	// currentCeraShopBonusItemNotificationEnabled 允许一键开关这个弹窗。
	currentCeraShopBonusItemNotificationEnabled = true

	// currentCeraShopBonusItemRewardActivityDisplayType 是打开"奖励活动面板"的显示类型。
	// 0..7 是普通商城结果上下文，**必须用 >7 才会开多买多送的奖励面板**。
	currentCeraShopBonusItemRewardActivityDisplayType uint32 = 8

	// currentCeraShopBonusItemBodySize 是包体长度：1 + 3*4。
	currentCeraShopBonusItemBodySize = 13

	// currentCeraShopBonusItemUnknownIndex 是 handler 显式判为"不发"的哨兵值：
	// 反编译里的 `if (local_10 != -1) { ... }`。字段是按**有符号 int** 比较的，
	// 所以这个 -1 在线上就是 0xFFFFFFFF。
	currentCeraShopBonusItemUnknownIndex uint32 = 0xFFFFFFFF
)

// buildCurrentCeraShopBonusItemBody 编码 `class0/op852` 的包体。
//
// itemIndex 传 currentCeraShopBonusItemUnknownIndex 时按 handler 的语义会被忽略，
// 所以调用方不该那样发（这里只做长度与字节序的编码，不做策略判断）。
func buildCurrentCeraShopBonusItemBody(displayType, itemIndex, itemNumber uint32) []byte {
	body := make([]byte, currentCeraShopBonusItemBodySize)
	body[0] = 1 // flag：handler 只在非 0 时才处理后续 12 字节
	binary.LittleEndian.PutUint32(body[1:5], displayType)
	binary.LittleEndian.PutUint32(body[5:9], itemIndex)
	binary.LittleEndian.PutUint32(body[9:13], itemNumber)
	return body
}

// sendCurrentCeraShopBonusItemPopup 在开出商城赠品时把弹窗推给客户端。
//
// 与其它商城通知同款：纯展示，失败只记日志，绝不影响已经落库的结账。
func (s *Service) sendCurrentCeraShopBonusItemPopup(
	session *gameSession,
	itemID, itemCount uint32,
	source string,
) error {
	if !currentCeraShopBonusItemNotificationEnabled {
		return nil
	}
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if itemID == 0 || itemCount == 0 || itemID == currentCeraShopBonusItemUnknownIndex {
		// handler 会把这种整段跳过，没必要发。
		return nil
	}
	body := buildCurrentCeraShopBonusItemBody(
		currentCeraShopBonusItemRewardActivityDisplayType, itemID, itemCount)
	if err := s.sendGameUpperRawClass(session, currentCeraShopBonusItemMsgID, body, currentCeraShopClass0); err != nil {
		return err
	}
	s.logPacketEvent("game-cera-shop-bonus-item-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"display_type", currentCeraShopBonusItemRewardActivityDisplayType,
		"item_id", itemID,
		"item_count", itemCount,
		"body_len", len(body),
		"wire", "class0/op852-u8-u32-u32-u32")
	return nil
}
