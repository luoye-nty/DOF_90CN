package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestParseCurrentDungeonDiscardRequestMatchesLiveOp47(t *testing.T) {
	body := []byte{
		0x29, 0x04,
		0x0e, 0x01,
		0x00,
		0x47, 0x00,
		0x01, 0x00, 0x00, 0x00,
		0x00,
	}
	request, err := parseCurrentDungeonDiscardRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.PositionX != 1065 || request.PositionY != 270 ||
		request.ListType != 0 || request.SourceSlot != 71 ||
		request.Count != 1 || request.SceneMode != 0 ||
		!bytes.Equal(request.AckPayload, []byte{0, 0x47, 0, 1, 0, 0, 0}) {
		t.Fatalf("request=%+v ack=%x", request, request.AckPayload)
	}

	for _, malformed := range [][]byte{
		body[:len(body)-1],
		append(append([]byte(nil), body...), 0),
		buildCurrentDungeonDiscardTestRequest(1065, 270, 0, 71, 0, 0),
		buildCurrentDungeonDiscardTestRequest(1065, 270, 1, 71, 1, 0),
	} {
		if _, err := parseCurrentDungeonDiscardRequest(malformed); !errors.Is(err, errCurrentDungeonDiscardMalformed) {
			t.Fatalf("malformed body accepted body=%x err=%v", malformed, err)
		}
	}

	// 负重超限时客户端发的是同一版请求、只有 body[11] 变成 1。这一位没有服务端
	// 消费者（op40 的模式字节由调用方传常量），所以必须原样接受并只留作取证；
	// 按值硬校验会让超重状态下的丢弃整包被判 malformed，而 malformed 分支不回包。
	for _, clientMode := range []byte{0, 1} {
		overweight := buildCurrentDungeonDiscardTestRequest(1065, 270, 0, 71, 1, clientMode)
		parsed, err := parseCurrentDungeonDiscardRequest(overweight)
		if err != nil {
			t.Fatalf("client mode=%d rejected: %v", clientMode, err)
		}
		if parsed.SourceSlot != 71 || parsed.Count != 1 || parsed.SceneMode != clientMode ||
			!bytes.Equal(parsed.AckPayload, []byte{0, 0x47, 0, 1, 0, 0, 0}) {
			t.Fatalf("client mode=%d request=%+v ack=%x", clientMode, parsed, parsed.AckPayload)
		}
	}
}

func TestBuildCurrentDungeonDiscardSceneBodyMatchesCurrentEXEOp40Reader(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID: 3037,
		Count:  1,
		Extra: map[string]string{
			"item_kind":      string(dungeonDropItemStackable),
			"stackable_type": "[material]",
		},
	}
	body, entry, err := buildCurrentDungeonDiscardSceneBody(
		401,
		733,
		244,
		0x11223344,
		17,
		stack,
		dungeonDropItemDefinition{
			ItemID:        3037,
			Kind:          dungeonDropItemStackable,
			AttachType:    "[free]",
			StackableType: "[material]",
		},
		1,
		currentDungeonDiscardSceneMode,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != currentDungeonDiscardSceneBodySize ||
		binary.LittleEndian.Uint16(body[0:2]) != 401 ||
		binary.LittleEndian.Uint16(body[2:4]) != 733 ||
		binary.LittleEndian.Uint16(body[4:6]) != 244 ||
		binary.LittleEndian.Uint32(body[6:10]) != 0x11223344 ||
		!bytes.Equal(body[10:10+currentItemListEntryWireSize], entry.data[:]) ||
		body[10+currentItemListEntryWireSize] != currentDungeonDiscardSceneMode ||
		binary.LittleEndian.Uint16(body[len(body)-2:]) != currentDungeonDiscardUnscopedContext {
		t.Fatalf("body=%x entry=%x", body, entry.data)
	}
	if binary.LittleEndian.Uint16(entry.data[0:2]) != 17 ||
		binary.LittleEndian.Uint32(entry.data[2:6]) != 3037 ||
		binary.LittleEndian.Uint32(entry.data[6:10]) != 1 {
		t.Fatalf("scene item row=%x", entry.data)
	}
}

func TestCurrentDungeonDiscardTradeableUsesRuntimePVFAttachType(t *testing.T) {
	for _, test := range []struct {
		attach string
		want   bool
	}{
		// 白名单只有 `[free]` 与 `[sealing]`；品级不参与判断。
		{attach: "`[free]`", want: true},
		{attach: "`[sealing]`", want: true},
		{attach: "`[trade]`", want: false},
		{attach: "`[account]`", want: false},
		{attach: "`[trade delete]`", want: false},
		{attach: "", want: false},
	} {
		if got := currentDungeonDiscardTradeable(dungeonDropItemDefinition{AttachType: test.attach}); got != test.want {
			t.Fatalf("attach=%q tradeable=%t want=%t", test.attach, got, test.want)
		}
	}
}

func TestCurrentDungeonDiscardMainInventoryCreatesGroundDropAndRepicks(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	stack := currentDungeonDiscardTestStack(3227, 5)
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	inventory.Slots["0:121"] = stack
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-main-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(733, 244, 6, 100)); err != nil {
		t.Fatal(err)
	}
	requestBody := buildCurrentDungeonDiscardTestRequest(733, 244, 0, 121, 2, 0)
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketDropItem),
		requestBody,
	); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	scenePacket, rest := splitGameServerUpperPacket(t, rest)
	update, trailing := splitGameServerUpperPacket(t, rest)
	if ack.Header.MsgID != currentDungeonDiscardOpcode ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(ack.Body) != currentDungeonDiscardAckSize+1 ||
		ack.Body[0] != 1 ||
		!bytes.Equal(ack.Body[1:], []byte{0, 121, 0, 2, 0, 0, 0}) {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	if scenePacket.Header.MsgID != currentDungeonDiscardSceneOpcode ||
		scenePacket.Header.Classification != 0 ||
		len(scenePacket.Body) != currentDungeonDiscardSceneBodySize ||
		len(trailing) != 0 {
		t.Fatalf("scene header=%+v body=%x trailing=%x", scenePacket.Header, scenePacket.Body, trailing)
	}
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		update.Header.Classification != 0 ||
		len(update.Body) != 3+currentItemListEntryWireSize ||
		update.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(update.Body[1:3]) != 1 ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 121 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != 3227 ||
		binary.LittleEndian.Uint32(update.Body[9:13]) != 3 {
		t.Fatalf("discard item update header=%+v body=%x", update.Header, update.Body)
	}
	if binary.LittleEndian.Uint16(scenePacket.Body[2:4]) != 733 ||
		binary.LittleEndian.Uint16(scenePacket.Body[4:6]) != 244 ||
		binary.LittleEndian.Uint32(scenePacket.Body[10+6:10+10]) != 2 {
		t.Fatalf("scene body=%x", scenePacket.Body)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil || inventory.Slots["0:121"].Count != 3 {
		t.Fatalf("discard inventory=%+v err=%v", inventory.Slots, err)
	}
	dropObjectKey := binary.LittleEndian.Uint32(scenePacket.Body[6:10])
	drop := runtime.DropOwner.byObjectKey[dropObjectKey]
	if drop == nil || drop.Amount != 2 || drop.DiscardOrigin == nil ||
		drop.DiscardOrigin.AccountOwned || drop.DiscardOrigin.SourceSlot != 121 ||
		drop.DiscardOrigin.Stack.Count != 2 || drop.Status != runtimeDungeonDropAvailable {
		t.Fatalf("runtime drop=%+v", drop)
	}

	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
		t.Fatal(err)
	}
	pickup, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	update, trailing = splitGameServerUpperPacket(t, rest)
	if pickup.Header.MsgID != currentDungeonPickupResultOpcode || len(trailing) != 0 ||
		binary.LittleEndian.Uint16(pickup.Body[16:18]) != 121 {
		t.Fatalf("pickup header=%+v body=%x trailing=%x", pickup.Header, pickup.Body, trailing)
	}
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 121 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != 3227 ||
		binary.LittleEndian.Uint32(update.Body[9:13]) != 5 {
		t.Fatalf("repick item update header=%+v body=%x", update.Header, update.Body)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil || inventory.Slots["0:121"].Count != 5 {
		t.Fatalf("repick inventory=%+v err=%v", inventory.Slots, err)
	}
}

func TestCurrentDungeonDiscardAccountSharedRoundTripsSameSlot(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3037)
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): currentDungeonDiscardTestStack(3037, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-account-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(500, 250, 6, 100)); err != nil {
		t.Fatal(err)
	}
	body := buildCurrentDungeonDiscardTestRequest(500, 250, 0, 358, 2, 0)
	handled, err := service.handleCurrentDungeonDiscard(session, body)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	scenePacket, rest := splitGameServerUpperPacket(t, rest)
	update, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("scene trailing=%x", trailing)
	}
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 358 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != 3037 ||
		binary.LittleEndian.Uint32(update.Body[9:13]) != 8 {
		t.Fatalf("account discard update header=%+v body=%x", update.Header, update.Body)
	}
	account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found || account.Slots["0:358"].Count != 8 {
		t.Fatalf("account after discard found=%t slots=%+v err=%v", found, account.Slots, err)
	}
	dropObjectKey := binary.LittleEndian.Uint32(scenePacket.Body[6:10])
	drop := runtime.DropOwner.byObjectKey[dropObjectKey]
	if drop == nil || drop.DiscardOrigin == nil || !drop.DiscardOrigin.AccountOwned ||
		drop.DiscardOrigin.SourceSlot != 358 {
		t.Fatalf("account drop=%+v", drop)
	}

	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
		t.Fatal(err)
	}
	pickup, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	update, trailing = splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || binary.LittleEndian.Uint16(pickup.Body[16:18]) != 358 {
		t.Fatalf("pickup body=%x trailing=%x", pickup.Body, trailing)
	}
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 358 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != 3037 ||
		binary.LittleEndian.Uint32(update.Body[9:13]) != 10 {
		t.Fatalf("account repick update header=%+v body=%x", update.Header, update.Body)
	}
	account, found, err = repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found || account.Slots["0:358"].Count != 10 {
		t.Fatalf("account after repick found=%t slots=%+v err=%v", found, account.Slots, err)
	}
}

func TestCurrentDungeonDiscardWholeStackPublishesDeleteRow(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	inventory.Slots["0:80"] = currentDungeonDiscardTestStack(3227, 1)
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-delete-update-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	handled, err := service.handleCurrentDungeonDiscard(
		session,
		buildCurrentDungeonDiscardTestRequest(500, 250, 0, 80, 1, 0),
	)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	_, rest = splitGameServerUpperPacket(t, rest)
	update, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 ||
		update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(update.Body) != 3+currentItemListEntryWireSize ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 80 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != math.MaxUint32 ||
		binary.LittleEndian.Uint32(update.Body[9:13]) != 0 {
		t.Fatalf("whole-stack delete update header=%+v body=%x trailing=%x", update.Header, update.Body, trailing)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := inventory.Slots["0:80"]; exists {
		t.Fatalf("whole-stack source slot still exists: %+v", inventory.Slots["0:80"])
	}
}

// 负重超限时客户端会把刚越线的那一件装备立刻丢回地面，这一版 op47 与普通丢弃
// 的唯一区别是 body[11]=1。它必须和 mode=0 走完全同一条权威路径：扣库存 +
// op47 成功 ACK + op40 地面对象 + op14 行更新。
//
// 这里同时钉住一个刻意的取舍：**不把这个 1 回显进 op40**。op40 的模式字节目前
// 没有客户端读法证据（仓库里没有 op40 的线上 fixture，逆向笔记也没记），而
// mode=0 那条已验证可用的路径发出去的就是 0，所以先保持出站字节不变。
func TestCurrentDungeonDiscardAcceptsOverweightClientMode(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	inventory.Slots["0:80"] = currentDungeonDiscardTestStack(3227, 1)
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-overweight-mode-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	handled, err := service.handleCurrentDungeonDiscard(
		session,
		buildCurrentDungeonDiscardTestRequest(500, 250, 0, 80, 1, 1),
	)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	scenePacket, rest := splitGameServerUpperPacket(t, rest)
	update, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	if ack.Header.MsgID != currentDungeonDiscardOpcode || ack.Body[0] != 1 ||
		!bytes.Equal(ack.Body[1:], []byte{0, 80, 0, 1, 0, 0, 0}) {
		t.Fatalf("overweight ack header=%+v body=%x", ack.Header, ack.Body)
	}
	if scenePacket.Header.MsgID != currentDungeonDiscardSceneOpcode ||
		len(scenePacket.Body) != currentDungeonDiscardSceneBodySize ||
		scenePacket.Body[10+currentItemListEntryWireSize] != currentDungeonDiscardSceneMode {
		t.Fatalf("overweight scene header=%+v body=%x", scenePacket.Header, scenePacket.Body)
	}
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 80 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != math.MaxUint32 ||
		binary.LittleEndian.Uint32(update.Body[9:13]) != 0 {
		t.Fatalf("overweight update header=%+v body=%x", update.Header, update.Body)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := inventory.Slots["0:80"]; exists {
		t.Fatalf("overweight source slot still exists: %+v", inventory.Slots["0:80"])
	}
	if runtime.DropOwner == nil || len(runtime.DropOwner.byObjectKey) != 1 {
		t.Fatalf("overweight drop owner=%+v", runtime.DropOwner)
	}
	for objectKey, drop := range runtime.DropOwner.byObjectKey {
		if drop == nil || drop.DiscardOrigin == nil || drop.DiscardOrigin.SourceSlot != 80 ||
			drop.Amount != 1 || drop.Status != runtimeDungeonDropAvailable {
			t.Fatalf("overweight drop %d=%+v", objectKey, drop)
		}
	}
}

// 弃置物躺在副本地上期间，它被丢下的那一格必须留给它。
//
// 新拾取物按"最小空格优先"分配，正好会盯上刚被丢弃腾空的那一格；装备又不能并进已占用的
// 行，所以一旦被占掉，那件弃置物就永久捡不起来（2026-09-27 实测：新拾取占了 43 格之后，
// 地面对象 504 再也拾取不了，客户端 1 分钟后重试仍然失败）。
func TestCurrentDungeonDiscardOriginSlotIsReservedFromNewPickups(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	inventory.Slots["0:121"] = currentDungeonDiscardTestStack(3227, 2)
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-reserved-slot-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(500, 250, 6, 100)); err != nil {
		t.Fatal(err)
	}
	handled, err := service.handleCurrentDungeonDiscard(
		session,
		buildCurrentDungeonDiscardTestRequest(500, 250, 0, 121, 2, 0),
	)
	if err != nil || !handled {
		t.Fatalf("discard handled=%t err=%v", handled, err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	scenePacket, rest := splitGameServerUpperPacket(t, rest)
	if _, trailing := splitGameServerUpperPacket(t, rest); len(trailing) != 0 {
		t.Fatalf("discard trailing=%x", trailing)
	}
	dropObjectKey := binary.LittleEndian.Uint32(scenePacket.Body[6:10])

	reserved := currentDungeonDiscardReservedSlots(runtime)
	if _, ok := reserved[121]; !ok {
		t.Fatalf("slot 121 must be reserved while its discard is on the ground: %v", reserved)
	}

	// 新拾取物的区间从 121 开始，而 121 此刻是空的：不保留的话它就会被占掉。
	newPickup := dungeonDropItemDefinition{
		ItemID:        3227,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/material/test_drop.stk",
		StackableType: "[material]",
		StackLimit:    999,
		SlotStart:     121,
		SlotEnd:       125,
		AttachType:    "[free]",
	}
	slot, _, err := service.grantCurrentDungeonPickupItem(
		context.Background(),
		repositories.CharacterItems,
		"99",
		newPickup,
		1,
		time.Now().UTC(),
		currentDungeonDiscardReservedSlots(runtime),
	)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 122 {
		t.Fatalf("new pickup slot=%d, want 122: 121 is promised to the discarded item", slot)
	}

	// 弃置物仍然回得去它原来的格子。
	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
		t.Fatal(err)
	}
	pickup, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	update, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("pickup trailing=%x", trailing)
	}
	if restored := binary.LittleEndian.Uint16(pickup.Body[16:18]); restored != 121 {
		t.Fatalf("discarded item restored to slot %d, want 121", restored)
	}
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 121 {
		t.Fatalf("restore update header=%+v body=%x", update.Header, update.Body)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots["0:121"].ItemID != 3227 || inventory.Slots["0:121"].Count != 2 {
		t.Fatalf("restored inventory=%+v", inventory.Slots)
	}
}

// 反例：不传保留集时就是这个 bug 本身 —— 新拾取物占掉 121，弃置物再也回不去。
// 这条测试存在的意义是证明上面那条测试真的在测保留机制，而不是碰巧通过。
//
// 新拾取物刻意换成另一个物品 id：归还时若原格位被同 id 的堆叠占着，mergeCurrentDungeonDiscardOrigin
// 会按"并进已有那一堆"放行（堆叠物品的合法路径）；只有 id 不同（或干脆是装备）时才会判冲突 ——
// 线上卡住的那件正是装备，装备完全没有合并路径。
func TestCurrentDungeonDiscardOriginSlotWithoutReservationBlocksRepick(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	inventory.Slots["0:121"] = currentDungeonDiscardTestStack(3227, 2)
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-unreserved-slot-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(500, 250, 6, 100)); err != nil {
		t.Fatal(err)
	}
	handled, err := service.handleCurrentDungeonDiscard(
		session,
		buildCurrentDungeonDiscardTestRequest(500, 250, 0, 121, 2, 0),
	)
	if err != nil || !handled {
		t.Fatalf("discard handled=%t err=%v", handled, err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	scenePacket, rest := splitGameServerUpperPacket(t, rest)
	if _, trailing := splitGameServerUpperPacket(t, rest); len(trailing) != 0 {
		t.Fatalf("discard trailing=%x", trailing)
	}
	dropObjectKey := binary.LittleEndian.Uint32(scenePacket.Body[6:10])

	newPickup := dungeonDropItemDefinition{
		ItemID:        3228,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/material/test_drop.stk",
		StackableType: "[material]",
		StackLimit:    999,
		SlotStart:     121,
		SlotEnd:       125,
		AttachType:    "[free]",
	}
	slot, _, err := service.grantCurrentDungeonPickupItem(
		context.Background(),
		repositories.CharacterItems,
		"99",
		newPickup,
		1,
		time.Now().UTC(),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 121 {
		t.Fatalf("new pickup slot=%d, want 121 without reservations", slot)
	}

	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("blocked restore still wrote %x", connection.write.Bytes())
	}
	drop := runtime.DropOwner.byObjectKey[dropObjectKey]
	if drop == nil || drop.Status != runtimeDungeonDropAvailable {
		t.Fatalf("drop=%+v, want it to stay available on the ground", drop)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots["0:121"].Count != 1 {
		t.Fatalf("slot 121 should still hold only the new pickup: %+v", inventory.Slots["0:121"])
	}
}

func TestCurrentDungeonDiscardIsDungeonOnlyAndRejectsBoundItems(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	body := buildCurrentDungeonDiscardTestRequest(500, 250, 0, 121, 1, 0)
	if handled, err := service.handleCurrentDungeonDiscard(&gameSession{selectedCharacterID: 99}, body); handled || err != nil {
		t.Fatalf("town request intercepted handled=%t err=%v", handled, err)
	}

	stack := currentDungeonDiscardTestStack(3227, 2)
	stack.Bind = true
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	inventory.Slots["0:121"] = stack
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-discard-bound-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(500, 250, 6, 100)); err != nil {
		t.Fatal(err)
	}
	handled, err := service.handleCurrentDungeonDiscard(session, body)
	if err != nil || !handled {
		t.Fatalf("bound handled=%t err=%v", handled, err)
	}
	if connection.write.Len() != 0 || runtime.DropOwner != nil {
		t.Fatalf("bound discard wrote=%x drop_owner=%+v", connection.write.Bytes(), runtime.DropOwner)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil || inventory.Slots["0:121"].Count != 2 {
		t.Fatalf("bound inventory=%+v err=%v", inventory.Slots, err)
	}
}

func buildCurrentDungeonDiscardTestRequest(
	positionX uint16,
	positionY uint16,
	listType byte,
	slot uint16,
	count uint32,
	mode byte,
) []byte {
	body := make([]byte, currentDungeonDiscardRequestSize)
	binary.LittleEndian.PutUint16(body[0:2], positionX)
	binary.LittleEndian.PutUint16(body[2:4], positionY)
	body[4] = listType
	binary.LittleEndian.PutUint16(body[5:7], slot)
	binary.LittleEndian.PutUint32(body[7:11], count)
	body[11] = mode
	return body
}

func currentDungeonDiscardTestStack(itemID int64, count int64) dnfrepo.ItemStack {
	stack := dnfrepo.ItemStack{
		ItemID: itemID,
		Count:  count,
		Extra: map[string]string{
			"item_kind":      string(dungeonDropItemStackable),
			"stackable_type": "[material]",
			"stack_limit":    "999",
			"pvf_path":       "stackable/material/test_drop.stk",
		},
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 121, stack)
	stack.RawEntry = append([]byte(nil), entry.data[:]...)
	return stack
}
