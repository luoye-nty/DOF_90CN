package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOnlineItemTradeProjectsOfferAndAtomicallyCommitsOnBothReady(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:97": {ItemID: 10008073, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstConn, secondConn := &bufferConn{}, &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	first := &gameSession{conn: firstConn, channel: channel, selectedCharacterID: 1}
	second := &gameSession{conn: secondConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(first, 1)
	service.bindGameSessionCharacter(second, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 38, AreaID: 0, Session: first})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 38, AreaID: 0, Session: second})
	service.beginOnlineItemTrade(first, second)

	// op19 carries an opaque live-item instance field, not the PVF item ID or a
	// durable repository quantity. A mismatch must not reject the authoritative
	// source slot.
	request := buildOnlineItemTradeMoveRequest(0, 97, 0x12345678, 1, currentItemTradeListType, 0)
	handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg, request)
	if err != nil || !handled {
		t.Fatalf("offer handled=%t err=%v", handled, err)
	}
	ack, rest := splitGameServerUpperPacket(t, firstConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != currentItemTradeMoveMsg ||
		!bytes.Equal(ack.Body, []byte{1, 0, 97, 0, 1, 0, 0, 0, 4, 3, 0}) || len(rest) != 0 {
		t.Fatalf("offer ack class=%d msg=%d body=% X trailing=%d", ack.Header.Classification, ack.Header.MsgID, ack.Body, len(rest))
	}
	change, rest := splitGameServerUpperPacket(t, secondConn.write.Bytes())
	if change.Header.Classification != 0 || change.Header.MsgID != currentItemTradeChangeMsg || len(change.Body) != currentItemListEntryWireSize ||
		binary.LittleEndian.Uint16(change.Body[:2]) != 3 || binary.LittleEndian.Uint32(change.Body[2:6]) != 10008073 || len(rest) != 0 {
		t.Fatalf("peer offer class=%d msg=%d body_len=%d slot=%d item=%d trailing=%d", change.Header.Classification, change.Header.MsgID,
			len(change.Body), binary.LittleEndian.Uint16(change.Body[:2]), binary.LittleEndian.Uint32(change.Body[2:6]), len(rest))
	}

	firstConn.write.Reset()
	secondConn.write.Reset()
	for _, participant := range []*gameSession{first, second} {
		handled, err = service.handleAlignedGameCommand(participant, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{currentItemTradeAddRequest})
		if err != nil || !handled {
			t.Fatalf("adding done char=%d handled=%t err=%v", participant.selectedCharacterID, handled, err)
		}
	}
	if service.currentOnlineItemTrade(1) == nil || service.currentOnlineItemTrade(5) == nil {
		t.Fatal("both adding-done states committed the trade before confirmation")
	}
	assertOnlineItemTradeStatePacket(t, firstConn.write.Bytes(), 1, currentItemTradeAddingDone)
	assertOnlineItemTradeStatePacket(t, firstConn.write.Bytes(), 5, currentItemTradeAddingDone)
	assertOnlineItemTradeStatePacket(t, secondConn.write.Bytes(), 1, currentItemTradeAddingDone)
	assertOnlineItemTradeStatePacket(t, secondConn.write.Bytes(), 5, currentItemTradeAddingDone)

	firstConn.write.Reset()
	secondConn.write.Reset()
	handled, err = service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{currentItemTradeOfferAck})
	if err != nil || !handled {
		t.Fatalf("offer-change ack handled=%t err=%v", handled, err)
	}
	offerAck, rest := splitGameServerUpperPacket(t, firstConn.write.Bytes())
	if offerAck.Header.Classification != dnfproto.DefaultChannelClassification || offerAck.Header.MsgID != currentItemTradeReadyMsg ||
		!bytes.Equal(offerAck.Body, []byte{1}) || len(rest) != 0 {
		t.Fatalf("offer-change ack class=%d msg=%d body=% X trailing=%d", offerAck.Header.Classification, offerAck.Header.MsgID, offerAck.Body, len(rest))
	}
	if len(secondConn.write.Bytes()) != 0 || service.currentOnlineItemTrade(1).states[1] != currentItemTradeAddingDone {
		t.Fatalf("offer-change ack leaked to peer or mutated state: peer=%x state=%d", secondConn.write.Bytes(), service.currentOnlineItemTrade(1).states[1])
	}

	firstConn.write.Reset()
	secondConn.write.Reset()
	handled, err = service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{currentItemTradeReady})
	if err != nil || !handled {
		t.Fatalf("first ready handled=%t err=%v", handled, err)
	}
	if service.currentOnlineItemTrade(1) == nil || service.currentOnlineItemTrade(5) == nil {
		t.Fatal("first final confirmation committed before the peer confirmed")
	}
	assertOnlineItemTradeStateAbsent(t, firstConn.write.Bytes(), currentItemTradeReady)
	assertOnlineItemTradeStateAbsent(t, secondConn.write.Bytes(), currentItemTradeReady)
	handled, err = service.handleAlignedGameCommand(second, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{currentItemTradeReady})
	if err != nil || !handled {
		t.Fatalf("second ready handled=%t err=%v", handled, err)
	}
	assertOnlineItemTradeStateAbsent(t, firstConn.write.Bytes(), currentItemTradeReady)
	assertOnlineItemTradeStateAbsent(t, secondConn.write.Bytes(), currentItemTradeReady)

	firstInventory, _, err := repositories.Inventory.Load(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	secondInventory, _, err := repositories.Inventory.Load(ctx, "5")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := firstInventory.Slots["0:97"]; present {
		t.Fatalf("source inventory still contains traded item: %+v", firstInventory.Slots)
	}
	if got := secondInventory.Slots["0:65"]; got.ItemID != 10008073 || got.Count != 1 {
		t.Fatalf("recipient item = %+v", got)
	}
	assertOnlineItemTradeFinishPacket(t, firstConn.write.Bytes(), nil)
	assertOnlineItemTradeFinishPacket(t, secondConn.write.Bytes(), [][2]uint16{{3, 65}})
	if service.currentOnlineItemTrade(1) != nil || service.currentOnlineItemTrade(5) != nil {
		t.Fatal("completed trade remained registered")
	}
}

func TestOnlineItemTradeCancelReturnsAssignedItemRowAndClearsState(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:97": {ItemID: 10008073, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstConn, secondConn := &bufferConn{}, &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	first := &gameSession{conn: firstConn, channel: channel, selectedCharacterID: 1}
	second := &gameSession{conn: secondConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(first, 1)
	service.bindGameSessionCharacter(second, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 38, AreaID: 0, Session: first})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 38, AreaID: 0, Session: second})
	service.beginOnlineItemTrade(first, second)

	request := buildOnlineItemTradeMoveRequest(0, 97, 0x12345678, 1, currentItemTradeListType, 0)
	if handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg, request); err != nil || !handled {
		t.Fatalf("offer handled=%t err=%v", handled, err)
	}
	firstConn.write.Reset()
	secondConn.write.Reset()
	service.cancelOnlineItemTrade(service.currentOnlineItemTrade(1), "test_cancel")

	firstCancel, rest := splitGameServerUpperPacket(t, firstConn.write.Bytes())
	if firstCancel.Header.Classification != 0 || firstCancel.Header.MsgID != currentItemTradeCancelMsg ||
		!bytes.Equal(firstCancel.Body, []byte{1, 0, 3, 0, 0, 97, 0}) || len(rest) != 0 {
		t.Fatalf("offerer cancel class=%d msg=%d body=% X trailing=%d", firstCancel.Header.Classification, firstCancel.Header.MsgID, firstCancel.Body, len(rest))
	}
	secondCancel, rest := splitGameServerUpperPacket(t, secondConn.write.Bytes())
	if secondCancel.Header.Classification != 0 || secondCancel.Header.MsgID != currentItemTradeCancelMsg ||
		!bytes.Equal(secondCancel.Body, []byte{0, 0}) || len(rest) != 0 {
		t.Fatalf("peer cancel class=%d msg=%d body=% X trailing=%d", secondCancel.Header.Classification, secondCancel.Header.MsgID, secondCancel.Body, len(rest))
	}
	if service.currentOnlineItemTrade(1) != nil || service.currentOnlineItemTrade(5) != nil {
		t.Fatal("cancelled trade remained registered")
	}
}

// 90CN.dll 在交易窗口的复位路径上补state 0,而该复位在「打开窗口」时同样
// 执行。若刚建立且完全为空的交易收到 state 0 就取交易窗口会在打开瞬间
// 自己取消(实测现象:"刚进入交易界面就提示物品交易已被取消")。因此空交+
// 刚创建的 state 0 必须被忽且不得向任何一方下op16。
func TestOnlineItemTradeIgnoresWindowOpenResetOnFreshEmptyTrade(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:97": {ItemID: 10008073, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstConn, secondConn := &bufferConn{}, &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	first := &gameSession{conn: firstConn, channel: channel, selectedCharacterID: 1}
	second := &gameSession{conn: secondConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(first, 1)
	service.bindGameSessionCharacter(second, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 38, AreaID: 0, Session: first})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 38, AreaID: 0, Session: second})
	service.beginOnlineItemTrade(first, second)

	handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand),
		currentItemTradeReadyMsg, []byte{currentItemTradeWindowClosed})
	if err != nil || !handled {
		t.Fatalf("window-open reset handled=%t err=%v", handled, err)
	}
	if service.currentOnlineItemTrade(1) == nil || service.currentOnlineItemTrade(5) == nil {
		t.Fatal("window-open reset cancelled a freshly opened trade")
	}
	assertOnlineItemTradeCancelAbsent(t, firstConn.write.Bytes())
	assertOnlineItemTradeCancelAbsent(t, secondConn.write.Bytes())
}

// 一旦交易承载了内容,收到state 0 就是用户真的关窗,必须整单取消并向双方
// 下发 op16:否则对方窗口不关、双方卡在交易NPC 商店拒绝购买)、客户端
// 搬进交易容器的物品不回背包。
func TestOnlineItemTradeCancelsOnWindowClosedAfterContent(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:97": {ItemID: 10008073, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "5",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstConn, secondConn := &bufferConn{}, &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	first := &gameSession{conn: firstConn, channel: channel, selectedCharacterID: 1}
	second := &gameSession{conn: secondConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(first, 1)
	service.bindGameSessionCharacter(second, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 38, AreaID: 0, Session: first})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 38, AreaID: 0, Session: second})
	service.beginOnlineItemTrade(first, second)

	request := buildOnlineItemTradeMoveRequest(0, 97, 0x12345678, 1, currentItemTradeListType, 0)
	if handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand),
		currentItemTradeMoveMsg, request); err != nil || !handled {
		t.Fatalf("offer handled=%t err=%v", handled, err)
	}
	firstConn.write.Reset()
	secondConn.write.Reset()

	handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand),
		currentItemTradeReadyMsg, []byte{currentItemTradeWindowClosed})
	if err != nil || !handled {
		t.Fatalf("window close handled=%t err=%v", handled, err)
	}
	if service.currentOnlineItemTrade(1) != nil || service.currentOnlineItemTrade(5) != nil {
		t.Fatal("window close left the trade registered")
	}
	assertOnlineItemTradeCancelPresent(t, firstConn.write.Bytes())
	assertOnlineItemTradeCancelPresent(t, secondConn.write.Bytes())
}

func assertOnlineItemTradeCancelPresent(t *testing.T, data []byte) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentItemTradeCancelMsg {
			return
		}
		data = rest
	}
	t.Fatalf("expected item-trade op%d cancel packet, got % X", currentItemTradeCancelMsg, data)
}

func assertOnlineItemTradeCancelAbsent(t *testing.T, data []byte) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.MsgID == currentItemTradeCancelMsg {
			t.Fatalf("unexpected item-trade op%d cancel body=% X", currentItemTradeCancelMsg, packet.Body)
		}
		data = rest
	}
}

func TestOnlineItemTradeProjectsAndAtomicallyCommitsGold(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Stats: map[string]int64{"gold": 250_000}},
		{CharacterID: "5", AccountID: "b", Stats: map[string]int64{"gold": 40_000}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstConn, secondConn := &bufferConn{}, &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	first := &gameSession{conn: firstConn, channel: channel, selectedCharacterID: 1}
	second := &gameSession{conn: secondConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(first, 1)
	service.bindGameSessionCharacter(second, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 38, AreaID: 0, Session: first})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 38, AreaID: 0, Session: second})
	service.beginOnlineItemTrade(first, second)

	request := buildOnlineItemTradeMoveRequest(0, 0, 0, 100_000, currentItemTradeListType, 0)
	handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg, request)
	if err != nil || !handled {
		t.Fatalf("gold offer handled=%t err=%v", handled, err)
	}
	ack, rest := splitGameServerUpperPacket(t, firstConn.write.Bytes())
	if ack.Header.MsgID != currentItemTradeMoveMsg || ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, []byte{1, 0, 0, 0, 0xa0, 0x86, 0x01, 0, 4, 0, 0}) || len(rest) != 0 {
		t.Fatalf("gold ack class=%d msg=%d body=% X trailing=%d", ack.Header.Classification, ack.Header.MsgID, ack.Body, len(rest))
	}
	change, rest := splitGameServerUpperPacket(t, secondConn.write.Bytes())
	if change.Header.MsgID != currentItemTradeChangeMsg || change.Header.Classification != 0 || len(change.Body) != currentItemListEntryWireSize ||
		binary.LittleEndian.Uint16(change.Body[0:2]) != 0 || binary.LittleEndian.Uint32(change.Body[2:6]) != 0 ||
		binary.LittleEndian.Uint32(change.Body[6:10]) != 100_000 || len(rest) != 0 {
		t.Fatalf("peer gold row class=%d msg=%d body=% X trailing=%d", change.Header.Classification, change.Header.MsgID, change.Body[:10], len(rest))
	}

	firstConn.write.Reset()
	secondConn.write.Reset()
	for _, state := range []byte{currentItemTradeAddRequest, currentItemTradeReady} {
		if _, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{state}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.handleAlignedGameCommand(second, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{state}); err != nil {
			t.Fatal(err)
		}
	}
	firstCharacter, _, _ := repositories.Character.Load(ctx, "1")
	secondCharacter, _, _ := repositories.Character.Load(ctx, "5")
	if firstCharacter.Stats["gold"] != 150_000 || secondCharacter.Stats["gold"] != 140_000 {
		t.Fatalf("gold after trade first=%d second=%d", firstCharacter.Stats["gold"], secondCharacter.Stats["gold"])
	}
	// 角色 5 收到了 100000 金币 -> 它的完成包体里多一条金币伪行 (0,0)。
	assertOnlineItemTradeFinishPacket(t, firstConn.write.Bytes(), nil)
	assertOnlineItemTradeFinishPacket(t, secondConn.write.Bytes(), [][2]uint16{{0, 0}})
}

// TestOnlineItemTradeReplaysCounterpartOffersWhenBothAddingDone 覆盖「补齐双方锁定后服务端下
// 对方物品信息」这一修复：第二个参与者上报「添加完5)」使双方同时进入锁定时，服务端必须通过
// op15(class-0) 向双方各重放一次对方的报价快照；锁定之后本侧不得再改物品报价与金币报价。
func TestOnlineItemTradeReplaysCounterpartOffersWhenBothAddingDone(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:97": {ItemID: 10008073, Count: 1},
			"0:98": {ItemID: 10008075, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "5",
		Slots: map[string]dnfrepo.ItemStack{
			"0:96": {ItemID: 10008074, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Stats: map[string]int64{"gold": 250_000}},
		{CharacterID: "5", AccountID: "b", Stats: map[string]int64{"gold": 40_000}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstConn, secondConn := &bufferConn{}, &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	first := &gameSession{conn: firstConn, channel: channel, selectedCharacterID: 1}
	second := &gameSession{conn: secondConn, channel: channel, selectedCharacterID: 5}
	service.bindGameSessionCharacter(first, 1)
	service.bindGameSessionCharacter(second, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 38, AreaID: 0, Session: first})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 38, AreaID: 0, Session: second})
	service.beginOnlineItemTrade(first, second)

	// 双方各放入一件物品：放入瞬间只有「放入方 -> 对方」的逐件 op15。
	if handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg,
		buildOnlineItemTradeMoveRequest(0, 97, 0x12345678, 1, currentItemTradeListType, 0)); err != nil || !handled {
		t.Fatalf("first offer handled=%t err=%v", handled, err)
	}
	// 源槽堆叠为两件但本次只放入一件：重放行必须显示「本次放入数量。，而不是源槽的 2。
	if handled, err := service.handleAlignedGameCommand(second, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg,
		buildOnlineItemTradeMoveRequest(0, 96, 0x12345678, 1, currentItemTradeListType, 0)); err != nil || !handled {
		t.Fatalf("second offer handled=%t err=%v", handled, err)
	}
	firstConn.write.Reset()
	secondConn.write.Reset()

	// 双方先后点击「添加完成」：第二个上5 的参与者使双方同时进入锁定态，
	// 此刻服务端必须向双方各重放一次对方的报价快照。
	for _, participant := range []*gameSession{first, second} {
		if _, err := service.handleAlignedGameCommand(participant, byte(dnfenum.GameCmdCommand), currentItemTradeReadyMsg, []byte{currentItemTradeAddRequest}); err != nil {
			t.Fatal(err)
		}
	}
	assertOnlineItemTradeChangePacket(t, secondConn.write.Bytes(), 3, 10008073, 1)
	assertOnlineItemTradeChangePacket(t, firstConn.write.Bytes(), 3, 10008074, 1)

	// 锁定后报价不可再改：新增物品报价必须失败，且不得把物品行泄露给对端。
	firstConn.write.Reset()
	secondConn.write.Reset()
	if handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg,
		buildOnlineItemTradeMoveRequest(0, 98, 0x12345678, 1, currentItemTradeListType, 0)); err != nil || !handled {
		t.Fatalf("locked offer handled=%t err=%v", handled, err)
	}
	assertOnlineItemTradeMoveFailure(t, firstConn.write.Bytes())
	if len(secondConn.write.Bytes()) != 0 {
		t.Fatalf("locked offer leaked to peer: %x", secondConn.write.Bytes())
	}

	// 锁定后金币同样不可再改。
	firstConn.write.Reset()
	secondConn.write.Reset()
	if handled, err := service.handleAlignedGameCommand(first, byte(dnfenum.GameCmdCommand), currentItemTradeMoveMsg,
		buildOnlineItemTradeMoveRequest(0, 0, 0, 10_000, currentItemTradeListType, 0)); err != nil || !handled {
		t.Fatalf("locked gold handled=%t err=%v", handled, err)
	}
	assertOnlineItemTradeMoveFailure(t, firstConn.write.Bytes())
	if len(secondConn.write.Bytes()) != 0 {
		t.Fatalf("locked gold leaked to peer: %x", secondConn.write.Bytes())
	}

	trade := service.currentOnlineItemTrade(1)
	if trade == nil || trade.states[1] != currentItemTradeAddingDone || trade.states[5] != currentItemTradeAddingDone ||
		len(trade.offers[1]) != 1 || len(trade.offers[5]) != 1 || trade.gold[1] != 0 {
		t.Fatalf("locked trade mutated: %+v", trade)
	}
}

func buildOnlineItemTradeMoveRequest(sourceList byte, sourceSlot int16, itemID, count int32, destinationList byte, destinationSlot int16) []byte {
	body := make([]byte, 28)
	body[0] = sourceList
	binary.LittleEndian.PutUint16(body[1:3], uint16(sourceSlot))
	binary.LittleEndian.PutUint32(body[3:7], uint32(itemID))
	binary.LittleEndian.PutUint32(body[7:11], uint32(count))
	body[11] = destinationList
	binary.LittleEndian.PutUint16(body[12:14], uint16(destinationSlot))
	return body
}

// assertOnlineItemTradeFinishPacket 校验 op18「交易完成」包体：u16 行数 + 每行
// (u16 交易 u16 目标。金币伪行是 (0,0)。
func assertOnlineItemTradeFinishPacket(t *testing.T, data []byte, wantRows [][2]uint16) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentItemTradeFinishMsg {
			wantLength := 2 + 4*len(wantRows)
			if len(packet.Body) != wantLength {
				t.Fatalf("finish body = % X, want %d rows / %d bytes", packet.Body, len(wantRows), wantLength)
			}
			if got := int(binary.LittleEndian.Uint16(packet.Body[:2])); got != len(wantRows) {
				t.Fatalf("finish row count = %d, want %d", got, len(wantRows))
			}
			for index, row := range wantRows {
				offset := 2 + 4*index
				if binary.LittleEndian.Uint16(packet.Body[offset:offset+2]) != row[0] ||
					binary.LittleEndian.Uint16(packet.Body[offset+2:offset+4]) != row[1] {
					t.Fatalf("finish row %d = % X, want [%d %d]",
						index, packet.Body[offset:offset+4], row[0], row[1])
				}
			}
			return
		}
		data = rest
	}
	t.Fatal("finish packet not sent")
}

func assertOnlineItemTradeStatePacket(t *testing.T, data []byte, wantCharacterID uint16, wantState byte) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentItemTradeStateMsg &&
			len(packet.Body) == 3 && binary.LittleEndian.Uint16(packet.Body[:2]) == wantCharacterID &&
			packet.Body[2] == wantState {
			return
		}
		data = rest
	}
	t.Fatalf("trade state packet char=%d state=%d not sent", wantCharacterID, wantState)
}

// assertOnlineItemTradeChangePacket 断言收到指定槽位/物品/数量op15(class-0) 报价行。
func assertOnlineItemTradeChangePacket(t *testing.T, data []byte, wantSlot uint16, wantItemID int64, wantCount uint32) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentItemTradeChangeMsg &&
			len(packet.Body) == currentItemListEntryWireSize &&
			binary.LittleEndian.Uint16(packet.Body[:2]) == wantSlot &&
			binary.LittleEndian.Uint32(packet.Body[2:6]) == uint32(wantItemID) {
			if got := binary.LittleEndian.Uint32(packet.Body[6:10]); got != wantCount {
				t.Fatalf("op15 slot=%d item=%d count=%d want %d", wantSlot, wantItemID, got, wantCount)
			}
			return
		}
		data = rest
	}
	t.Fatalf("op15 slot=%d item=%d not sent", wantSlot, wantItemID)
}

// assertOnlineItemTradeMoveFailure 断言 op19 返回失败应答（body 首字节为 0）。
func assertOnlineItemTradeMoveFailure(t *testing.T, data []byte) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.MsgID == currentItemTradeMoveMsg && len(packet.Body) > 0 && packet.Body[0] == 0 {
			return
		}
		data = rest
	}
	t.Fatalf("op19 failure ack not sent")
}

func assertOnlineItemTradeStateAbsent(t *testing.T, data []byte, unwantedState byte) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentItemTradeStateMsg &&
			len(packet.Body) == 3 && packet.Body[2] == unwantedState {
			t.Fatalf("unexpected item-trade op17 state %d body=% X", unwantedState, packet.Body)
		}
		data = rest
	}
}
