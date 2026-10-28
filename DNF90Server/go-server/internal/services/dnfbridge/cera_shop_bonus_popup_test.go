package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// `class0/op852`（商城赠品到账弹窗）的包体布局与开关。
//
// ⚠ opcode 是 **852（0x0354）**，取自注册点 `0x01D92F3F: push 0x354`。
// 早先记录写的 "0x02D2" 是**从枚举名表推出来的错误值** —— 722 在本 build 里没有注册，
// 发它等于石沉大海（现场就是这么卡了两轮）。下面第一条断言专门钉死这一点。
//
// 依据（两条独立证据互相印证）：
//  1. 本 build 反编译 `FUN_01d81250`：读 1 字节 flag，非 0 时再批量读 12 字节，
//     把第 2、3 个 u32 当 itemIndex/itemNumber 打进跟踪串
//     （"세라보너스 아이템 =  itemIndex %d itemNumber %d"），再交给
//     `FUN_016c1080(manager, itemIndex, itemNumber, A)`，其中 A 是
//     `switch (A) { case 0..7: 普通路径; default: 专用奖励活动面板 }`。
//  2. 86JP 参考服务端 `CeraShopBonusItemBuilder`：`WriteByte(1)` +
//     `WriteInt32(RewardActivityDisplayType=8)` + `itemTemplateId` + `itemCount`，
//     注释明确"0..7 走普通商城结果上下文，>7 打开专用的原生奖励活动面板"。
//
// ⇒ 13 字节，A = 8，itemIndex = 奖励道具号，itemNumber = 奖励数量。

func TestBuildCurrentCeraShopBonusItemBodyLayout(t *testing.T) {
	// opcode 必须是注册点给出的 852。这一条专门防"又有人按枚举名表推成 722" ——
	// 722 在本 build 里未注册，发出去客户端直接丢。
	if currentCeraShopBonusItemMsgID != 0x0354 {
		t.Fatalf("opcode=0x%04X，必须是从注册点读到的 0x0354（852）",
			currentCeraShopBonusItemMsgID)
	}
	if currentCeraShopBonusItemMsgID == 0x02D2 {
		t.Fatal("0x02D2(722) 在本 build 里**没有注册**，发它客户端不会处理")
	}
	if currentCeraShopBonusItemBodySize != 13 {
		t.Fatalf("常量长度=%d want=13（1 字节 flag + 3 个 u32）", currentCeraShopBonusItemBodySize)
	}
	body := buildCurrentCeraShopBonusItemBody(currentCeraShopBonusItemRewardActivityDisplayType, 490702364, 1)
	if len(body) != 13 {
		t.Fatalf("包体长度=%d want=13", len(body))
	}
	// flag 必须是 1：handler 只在非 0 时才处理后面 12 字节。
	if body[0] != 1 {
		t.Fatalf("flag=%d want=1", body[0])
	}
	// 显示类型必须 > 7，否则客户端走普通商城结果路径、不会弹奖励面板。
	if got := binary.LittleEndian.Uint32(body[1:5]); got != 8 {
		t.Fatalf("显示类型=%d want=8（<=7 不会打开奖励活动面板）", got)
	}
	if got := binary.LittleEndian.Uint32(body[5:9]); got != 490702364 {
		t.Fatalf("itemIndex(道具号)=%d", got)
	}
	if got := binary.LittleEndian.Uint32(body[9:13]); got != 1 {
		t.Fatalf("itemNumber(数量)=%d", got)
	}
}

func TestCurrentCeraShopBonusItemPopupSendsBody(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{}
	session := &gameSession{conn: connection, connID: "bonus-popup", selectedCharacterID: 19}
	if err := service.sendCurrentCeraShopBonusItemPopup(session, 490702364, 1, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("弹窗后有多余字节: %x", trailing)
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCeraShopBonusItemMsgID {
		t.Fatalf("header=%+v want=class0/op%#x", packet.Header, currentCeraShopBonusItemMsgID)
	}
	body := packet.Body
	if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:] // server16 头模式在包体前带 3 个固定 0 字节
	}
	if len(body) != 13 || body[0] != 1 ||
		binary.LittleEndian.Uint32(body[1:5]) != 8 ||
		binary.LittleEndian.Uint32(body[5:9]) != 490702364 ||
		binary.LittleEndian.Uint32(body[9:13]) != 1 {
		t.Fatalf("弹窗体=%x", packet.Body)
	}
}

// 无效输入不该发包：handler 对 itemIndex == -1 会整段跳过，0 道具/0 数量也没有意义。
func TestCurrentCeraShopBonusItemPopupStaysSilentForInvalidInput(t *testing.T) {
	cases := map[string]struct{ itemID, count uint32 }{
		"zero_item":        {0, 1},
		"zero_count":       {490702364, 0},
		"unknown_sentinel": {currentCeraShopBonusItemUnknownIndex, 1},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{}
			session := &gameSession{conn: connection, connID: name, selectedCharacterID: 19}
			if err := service.sendCurrentCeraShopBonusItemPopup(session, testCase.itemID, testCase.count, "unit_test"); err != nil {
				t.Fatal(err)
			}
			if connection.write.Len() != 0 {
				t.Fatalf("无效输入不该发包: %x", connection.write.Bytes())
			}
		})
	}
}

// 无论是 session 无效还是 service 为 nil，都不能 panic 或报错。
func TestCurrentCeraShopBonusItemPopupStaysSilentWhenUnavailable(t *testing.T) {
	service := &Service{}
	if err := service.sendCurrentCeraShopBonusItemPopup(nil, 490702364, 1, "unit_test"); err != nil {
		t.Fatalf("nil session 不应报错: %v", err)
	}
	if err := service.sendCurrentCeraShopBonusItemPopup(&gameSession{}, 490702364, 1, "unit_test"); err != nil {
		t.Fatalf("未选角色不应报错: %v", err)
	}
	if err := (*Service)(nil).sendCurrentCeraShopBonusItemPopup(&gameSession{selectedCharacterID: 19}, 490702364, 1, "unit_test"); err != nil {
		t.Fatalf("nil service 不应报错: %v", err)
	}
}

// 时序断言：`class0/op852` 必须**紧跟购买回包**（class1/op64）。
//
// 这是这条功能的第二个命门，也是现场踩到的第二个坑：服务端日志显示包发出去了、
// 参数也对，但客户端什么都不弹 —— 因为客户端的"购买结果"窗口是在收到 op64 时建立的，
// op852 是挂到那个窗口上、从它下面滑下来的"奖励活动"面板。发晚了（排到道具列表 /
// 钱包 / 次数通知之后）窗口已经收尾，面板没有宿主可挂。
//
// 所以这个用例断言的**不是"有没有发"，而是"紧跟在 ACK 后面"** —— 只断言存在性是
// 不够的，那正是上一版漏掉的部分。
func TestCurrentCeraShopBonusItemPopupFollowsPurchaseAckImmediately(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource())
	if !catalog.BuyCountGiftPlan().Enabled() {
		t.Fatalf("测试目录里多买多送未启用: %q", catalog.BuyCountGiftPlan().disabledReason)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 100000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 100000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "bonus-popup-order", selectedCharacterID: 19}

	// 买活动白名单里的商品：这一笔必然跨过第 1 档，产出弹窗数据。
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestRequestBody(currentCeraShopBuyCountGiftTestCommodityID),
	); err != nil {
		t.Fatal(err)
	}

	// 拆包并记录顺序。
	type sentPacket struct {
		classification byte
		msgID          uint16
		body           []byte
	}
	var packets []sentPacket
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		var packet dnfproto.ChannelPacket
		packet, stream = splitGameServerUpperPacket(t, stream)
		body := packet.Body
		if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
			body = body[3:] // server16 头模式在包体前带 3 个固定 0 字节
		}
		packets = append(packets, sentPacket{packet.Header.Classification, packet.Header.MsgID, body})
	}

	ackIndex := -1
	for index, packet := range packets {
		if packet.msgID == uint16(dnfenum.CmdPacketBuyCerashopItem) &&
			packet.classification == dnfproto.DefaultChannelClassification {
			ackIndex = index
			break
		}
	}
	if ackIndex < 0 {
		t.Fatalf("没有找到购买回包（class1/op%d）: %d 个包", dnfenum.CmdPacketBuyCerashopItem, len(packets))
	}
	if ackIndex+1 >= len(packets) {
		t.Fatalf("购买回包之后没有任何包，弹窗缺失")
	}
	next := packets[ackIndex+1]
	if next.classification != 0 || next.msgID != currentCeraShopBonusItemMsgID {
		order := ""
		for _, packet := range packets {
			order += " " + itoaForTest(int64(packet.msgID))
		}
		t.Fatalf("购买回包之后的第一个包必须是 class0/op%d（奖励弹窗），实际是 class%d/op%d；实际顺序:%s",
			currentCeraShopBonusItemMsgID, next.classification, next.msgID, order)
	}
	if len(next.body) != 13 || next.body[0] != 1 ||
		binary.LittleEndian.Uint32(next.body[1:5]) != currentCeraShopBonusItemRewardActivityDisplayType ||
		binary.LittleEndian.Uint32(next.body[5:9]) != 490702364 ||
		binary.LittleEndian.Uint32(next.body[9:13]) != 1 {
		t.Fatalf("奖励弹窗体=%x want display_type=%d item=490702364 count=1",
			next.body, currentCeraShopBonusItemRewardActivityDisplayType)
	}
}
