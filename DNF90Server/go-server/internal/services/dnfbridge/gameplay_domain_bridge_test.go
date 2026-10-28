package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestEmotionBridgePersistsThroughOwnerAndKeepsAck(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	var body [2]byte
	binary.LittleEndian.PutUint16(body[:], 7)
	if err := service.handleCurrentChangeEmotion(session, body[:]); err != nil {
		t.Fatalf("handle emotion: %v", err)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["mood_value"] != 7 {
		t.Fatalf("mood_value = %d", character.Stats["mood_value"])
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketChangeEmotion) ||
		len(ack.Body) != 3 ||
		ack.Body[0] != 1 ||
		binary.LittleEndian.Uint16(ack.Body[1:3]) != 7 {
		t.Fatalf("ack header=%+v body=%x rest=%d", ack.Header, ack.Body, len(rest))
	}
	projection, trailing := splitGameServerUpperPacket(t, rest)
	if projection.Header.Classification != 0 ||
		projection.Header.MsgID != currentMoodProjectionMsgID ||
		!bytes.Equal(projection.Body, buildCurrentMoodProjectionBody(7)) ||
		len(trailing) != 0 {
		t.Fatalf("mood projection header=%+v body=%x trailing=%x", projection.Header, projection.Body, trailing)
	}
}

func TestCloneTitleBridgePersistsThroughOwnerAndRefreshesAppearance(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Name:        "Actor19",
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"100:0": {ItemID: 123456, Count: 1},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"13": {SlotIndex: 13, ItemID: 654321},
		},
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	var body [4]byte
	binary.LittleEndian.PutUint32(body[:], 123456)
	if err := service.handleCurrentSetCloneTitle(session, body[:]); err != nil {
		t.Fatalf("handle clone title: %v", err)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["clone_title_item_id"] != 123456 {
		t.Fatalf("clone title = %d", character.Stats["clone_title_item_id"])
	}
	mode0, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if mode0.Header.Classification != 0 ||
		mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(mode0.Body) == 0 ||
		mode0.Body[0] != 0 {
		t.Fatalf("mode0 refresh header=%+v body=%x rest=%d", mode0.Header, mode0.Body, len(rest))
	}
	assertCurrentTypedMode0FullAppearanceForTest(t, mode0.Body, "Actor19", 13, 654321)
	mode1, trailing := splitGameServerUpperPacket(t, rest)
	if mode1.Header.Classification != 0 ||
		mode1.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(mode1.Body) <= currentMode1CreateCountOffset ||
		mode1.Body[0] != 1 ||
		binary.LittleEndian.Uint16(mode1.Body[21:23]) != 19 {
		t.Fatalf("full mode1 rebind header=%+v body=%x", mode1.Header, mode1.Body)
	}
	cloneTitleOffset := currentMode1CreateRowsOffset + currentMode1EquipmentCreateRowBaseWireSize
	if got := binary.LittleEndian.Uint32(mode1.Body[cloneTitleOffset : cloneTitleOffset+4]); got != 123456 {
		t.Fatalf("full mode1 rebind clone-title state=%d want=123456 body=%x", got, mode1.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected clone-title op13/op357/trailing=%x", trailing)
	}
}

func TestTitleBookBridgeMovesThroughOwnerAndKeepsPacketOrder(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:42": {ItemID: 9001, Count: 1},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	body := make([]byte, 20)
	binary.LittleEndian.PutUint32(body[0:4], 0)
	binary.LittleEndian.PutUint32(body[4:8], 42)
	binary.LittleEndian.PutUint32(body[8:12], 9001)
	binary.LittleEndian.PutUint32(body[12:16], 2)
	binary.LittleEndian.PutUint32(body[16:20], 7)
	if err := service.handleCurrentTitleBookPut(session, body); err != nil {
		t.Fatalf("handle title put: %v", err)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if _, found := inventory.Slots["0:42"]; found ||
		inventory.Slots["100:2007"].ItemID != 9001 {
		t.Fatalf("slots = %+v", inventory.Slots)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != currentTitleBookPutMsgID ||
		len(ack.Body) != 17 ||
		ack.Body[0] != 1 ||
		binary.LittleEndian.Uint32(ack.Body[1:5]) != 0 ||
		binary.LittleEndian.Uint32(ack.Body[5:9]) != 42 ||
		binary.LittleEndian.Uint32(ack.Body[9:13]) != 2 ||
		binary.LittleEndian.Uint32(ack.Body[13:17]) != 7 {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	update, rest := splitGameServerUpperPacket(t, rest)
	if update.Header.Classification != 0 ||
		update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("update header=%+v", update.Header)
	}
	list, trailing := splitGameServerUpperPacket(t, rest)
	if list.Header.Classification != 0 ||
		list.Header.MsgID != currentTitleBookMsgID ||
		len(trailing) != 0 ||
		len(list.Body) != 37 ||
		binary.LittleEndian.Uint32(list.Body[33:37]) != 0 {
		t.Fatalf("list header=%+v trailing=%d", list.Header, len(trailing))
	}
}

// TestTitleBookBridgeEquipSendsAckAndBookListOnly 钉住 2026-09-18 的闪退修复：
// 称号簿右键使用（op413）曾经在 ack 之后补发 22 行 op14 list-3 装备全量刷新，
// 客户端 op14 处理器因此栈溢出闪退（同日 23:41:17 客户端 90CN_trace：
// dispatch opcode=14 packet_len=2637 后立即 0xC0000005 eip=0x03B25D03，
// WER 同址 0xC0000409）。官方参考服务端 CTitleBook::getTitle(0x086425C4) 在装备
// 分支末尾只调 CUser::send_equip(0x865DD14)，而该函数对称号槽 13 直接 return，
// 所以这条路径必须一条物品列表 op14 都不发，客户端靠 ack 里的 (space, slot) 自行穿上。
func TestTitleBookBridgeEquipSendsAckAndBookListOnly(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	bookRow := make([]byte, 0x77)
	bookRow[0], bookRow[1] = 0xEA, 0x03 // list100 slot 1000+10 = 1010
	copy(bookRow[2:6], []byte{0xC8, 0xEC, 0x29, 0x00})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"100:4010": {ItemID: 2747592, Count: 1, RawEntry: bookRow},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	body := make([]byte, 20)
	binary.LittleEndian.PutUint32(body[0:4], 3)  // ENUM_ITEMSPACE：装备
	binary.LittleEndian.PutUint32(body[4:8], 13) // 客户端指定的称号槽
	binary.LittleEndian.PutUint32(body[8:12], 2747592)
	binary.LittleEndian.PutUint32(body[12:16], 4)  // category
	binary.LittleEndian.PutUint32(body[16:20], 10) // book index
	if err := service.handleCurrentTitleBookGet(session, body); err != nil {
		t.Fatalf("handle title get: %v", err)
	}

	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if stack, found := inventory.Slots["100:4010"]; found {
		t.Fatalf("book entry must move out when equipped: %+v", stack)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID == 2747592 {
			t.Fatalf("title must not leak into slot %s: %+v", key, stack)
		}
	}
	equipment, found, err := repos.Equipment.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry := equipment.Entries["13"]
	if entry.ItemID != 2747592 || entry.Extra["source"] != "title_book_equip" ||
		entry.RawEntry[0] != 13 {
		t.Fatalf("equipment slot 13 = %+v", entry)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != currentTitleBookGetMsgID ||
		len(ack.Body) != 17 ||
		ack.Body[0] != 1 ||
		binary.LittleEndian.Uint32(ack.Body[1:5]) != 3 ||
		binary.LittleEndian.Uint32(ack.Body[5:9]) != 13 ||
		binary.LittleEndian.Uint32(ack.Body[9:13]) != 4 ||
		binary.LittleEndian.Uint32(ack.Body[13:17]) != 10 {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	packets := 0
	for len(rest) > 0 {
		var packet dnfproto.ChannelPacket
		packet, rest = splitGameServerUpperPacket(t, rest)
		packets++
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) {
			t.Fatalf("equip path must not push an item list op14: body=%x", packet.Body)
		}
		if packet.Header.MsgID != currentTitleBookMsgID {
			t.Fatalf("unexpected packet after ack: %+v", packet.Header)
		}
		// 称号已经穿在身上，这一页里不许再出现它（否则玩家看到「簿里一个、身上一个」）。
		if bytes.Contains(packet.Body, []byte{0xC8, 0xEC, 0x29, 0x00}) {
			t.Fatalf("worn title must not stay in the book page: body=%x", packet.Body)
		}
	}
	if packets != 1 {
		t.Fatalf("expected exactly one book list after the ack, got %d packets", packets)
	}
}

// TestTitleBookBridgeTakeOutPutsTitleInBagAtRequestedSlot 覆盖 2026-09-18 玩家报障
// 「拿出来后显示在背包里，小退再上却自动穿戴了」：客户端「拿出来」发的是
// op413 (space=0, slot=<期望背包格>)，服务端必须回背包、并把**真实落点**写进 ack
// （客户端按 ack 里的 space/slot 摆放物品），不能再当成装备处理。
func TestTitleBookBridgeTakeOutPutsTitleInBagAtRequestedSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	bookRow := make([]byte, 0x77)
	bookRow[0], bookRow[1] = 0xEA, 0x03 // list100 slot 1000+10 = 1010
	copy(bookRow[2:6], []byte{0xC8, 0xEC, 0x29, 0x00})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"100:4010": {ItemID: 2747592, Count: 1, RawEntry: bookRow},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	body := make([]byte, 20)
	binary.LittleEndian.PutUint32(body[0:4], 0)  // ENUM_ITEMSPACE：背包
	binary.LittleEndian.PutUint32(body[4:8], 12) // 客户端期望落在背包 12 号格
	binary.LittleEndian.PutUint32(body[8:12], 2747592)
	binary.LittleEndian.PutUint32(body[12:16], 4)
	binary.LittleEndian.PutUint32(body[16:20], 10)
	if err := service.handleCurrentTitleBookGet(session, body); err != nil {
		t.Fatalf("handle title get: %v", err)
	}

	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if stack, found := inventory.Slots["100:4010"]; found {
		t.Fatalf("book entry must move out: %+v", stack)
	}
	if stack := inventory.Slots["0:12"]; stack.ItemID != 2747592 {
		t.Fatalf("title must land in the requested bag slot 12: %+v", inventory.Slots)
	}
	equipment, _, err := repos.Equipment.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load equipment: %v", err)
	}
	if entry, found := equipment.Entries["13"]; found {
		t.Fatalf("take-out must not equip the title: %+v", entry)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != currentTitleBookGetMsgID ||
		len(ack.Body) != 17 ||
		ack.Body[0] != 1 ||
		binary.LittleEndian.Uint32(ack.Body[1:5]) != 0 ||
		binary.LittleEndian.Uint32(ack.Body[5:9]) != 12 ||
		binary.LittleEndian.Uint32(ack.Body[9:13]) != 4 ||
		binary.LittleEndian.Uint32(ack.Body[13:17]) != 10 {
		t.Fatalf("ack must echo the real destination (space=0 slot=12): header=%+v body=%x", ack.Header, ack.Body)
	}
	update, rest := splitGameServerUpperPacket(t, rest)
	if update.Header.Classification != 0 ||
		update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(update.Body) < 9 ||
		update.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(update.Body[1:3]) != 1 ||
		binary.LittleEndian.Uint16(update.Body[3:5]) != 12 ||
		binary.LittleEndian.Uint32(update.Body[5:9]) != 2747592 {
		t.Fatalf("expected one list-0 op14 row for bag slot 12: header=%+v body=%x", update.Header, update.Body)
	}
	list, trailing := splitGameServerUpperPacket(t, rest)
	if list.Header.MsgID != currentTitleBookMsgID || len(trailing) != 0 {
		t.Fatalf("expected the book list last: header=%+v trailing=%d", list.Header, len(trailing))
	}
	if bytes.Contains(list.Body, []byte{0xC8, 0xEC, 0x29, 0x00}) {
		t.Fatalf("taken-out title must not stay in the book page: body=%x", list.Body)
	}
}

// TestTitleBookPageKeepsItemSealState 回归 2026-09-18「已解封的称号放进称号簿、小退
// 重登又变成封装」：称号簿页条目里 0x0D 那一位是封装标记（0=解封，1=封装），必须跟
// 背包行同源读物品自己的状态，不能写死 1。
func TestTitleBookPageKeepsItemSealState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			// 解封的称号：没有 seal extra、Bind=false → 0
			"100:4010": {ItemID: 2747592, Count: 1},
			// 再封装过的称号：reseal 会写 seal 标记 → 1（见 itemgrade/reseal.go）
			"100:3001": {ItemID: 2747593, Count: 1, Extra: map[string]string{"seal_flag": "1", "seal": "1"}},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	unsealedRows := currentRequestOverseerRowsFromInventory(ctx, repos.Inventory, "19", 0, 4)
	if len(unsealedRows) != 1 || unsealedRows[0].itemID != 2747592 || unsealedRows[0].createFlag != 0 {
		t.Fatalf("unsealed title page row = %+v, want seal byte 0", unsealedRows)
	}
	sealedRows := currentRequestOverseerRowsFromInventory(ctx, repos.Inventory, "19", 0, 3)
	if len(sealedRows) != 1 || sealedRows[0].itemID != 2747593 || sealedRows[0].createFlag != 1 {
		t.Fatalf("resealed title page row = %+v, want seal byte 1", sealedRows)
	}
}

func newGameplayDomainBridgeFixture(repos dnfrepo.Group) (*Service, *gameSession, *bufferConn) {
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repos, true
		},
	}
	session := &gameSession{
		conn:                connection,
		connID:              "gameplay-domain-bridge-test",
		selectedCharacterID: 19,
	}
	return service, session, connection
}
