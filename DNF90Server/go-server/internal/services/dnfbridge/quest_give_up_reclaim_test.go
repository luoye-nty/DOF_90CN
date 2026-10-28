package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// giveUpQuestTestItemCatalog 造一个只认识指定道具的最小物品目录。
func giveUpQuestTestItemCatalog(items ...dungeonDropItemDefinition) *pvfDungeonDropCatalog {
	cache := make(map[uint32]dungeonDropItemDefinition, len(items))
	for _, item := range items {
		cache[uint32(item.ItemID)] = item
	}
	return &pvfDungeonDropCatalog{source: bridgePVFSource{}, itemCache: cache}
}

func giveUpQuestTestCatalog(t *testing.T, defaultList, path, definition string) *dnfquest.Catalog {
	t.Helper()
	index, err := dnfpvf.Build(context.Background(), questListTestSource{
		dnfquest.DefaultList: defaultList,
		path:                 definition,
	}, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func openGiveUpQuestTestService(
	t *testing.T,
	repositories dnfrepo.Group,
	catalog *dnfquest.Catalog,
	itemCatalog *pvfDungeonDropCatalog,
) (*Service, *gameSession, *bufferConn) {
	t.Helper()
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		pvfItemCatalog:     itemCatalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "give-up-quest-reclaim-test",
		channel:             channelcatalog.Channel{ID: 10},
		selectedCharacterID: 1,
	}
	return service, session, connection
}

func giveUpQuestTestRequest(t *testing.T, questID uint16) []byte {
	t.Helper()
	body := []byte{0x20, 0x00, 0x00, 0x00}
	binary.LittleEndian.PutUint16(body[2:4], questID)
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketGiveupQuest), body, 1, dnfproto.DefaultChannelClassification)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// 现场形状（2026-09-22 角色 1111 / 任务 8557）：收集类任务 + 可交易普通材料
// ⇒ 必须能放弃，而且材料一件都不许动。
func TestCurrentGiveUpQuestAllowsSeekingQuestWithOrdinaryMaterial(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "1",
		States:      map[int64]dnfrepo.QuestState{8557: {Status: "active", ProgressValue: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{"0:121": {ItemID: 10094602, Count: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := giveUpQuestTestCatalog(t, "8557 `daily.qst`\n", "n_quest/daily.qst",
		"[grade]\n`[daily]`\n[type]\n`[seeking]`\n[int data]\n10094602 10\n[reward type]\n`[item]`\n")
	itemCatalog := giveUpQuestTestItemCatalog(dungeonDropItemDefinition{
		ItemID: 10094602, Kind: dungeonDropItemStackable, StackableType: "[material]",
		StackLimit: 1000, SlotStart: 121, SlotEnd: 176,
	})
	service, session, connection := openGiveUpQuestTestService(t, repositories, catalog, itemCatalog)

	if err := service.handleGameUpper(session, giveUpQuestTestRequest(t, 8557)); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := repositories.Quest.Load(ctx, "1")
	if err != nil || !ok {
		t.Fatalf("load quest ok=%t err=%v", ok, err)
	}
	if _, exists := stored.States[8557]; exists {
		t.Fatalf("seeking quest 8557 still active: %+v", stored.States[8557])
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if stack := inventory.Slots["0:121"]; stack.ItemID != 10094602 || stack.Count != 10 {
		t.Fatalf("ordinary material changed on give-up: %+v", stack)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketGiveupQuest) {
		t.Fatalf("give-up ACK header=%+v", packet.Header)
	}
	if want := []byte{0x01, 0x6d, 0x21}; !bytes.Equal(packet.Body, want) {
		t.Fatalf("give-up ACK body=%x want=%x", packet.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("ordinary-material give-up emitted extra packet=%x", rest)
	}
}

// 任务专属道具（`[quest]` 家族）：放弃成功 + 同事务回收 + 给客户端发 op14 增量行。
func TestCurrentGiveUpQuestReclaimsQuestFamilyItemAndPushesItemUpdate(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "1",
		States:      map[int64]dnfrepo.QuestState{5760: {Status: "active", ProgressValue: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:177": {ItemID: 4363, Count: 5},
			"0:121": {ItemID: 3033, Count: 7},
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := giveUpQuestTestCatalog(t, "5760 `questitem.qst`\n", "n_quest/questitem.qst",
		"[grade]\n`[epic]`\n[type]\n`[seeking]`\n[int data]\n4363 5\n")
	itemCatalog := giveUpQuestTestItemCatalog(
		dungeonDropItemDefinition{
			ItemID: 4363, Kind: dungeonDropItemStackable, StackableType: "[quest]",
			StackLimit: 999, SlotStart: 177, SlotEnd: 232,
		},
		dungeonDropItemDefinition{
			ItemID: 3033, Kind: dungeonDropItemStackable, StackableType: "[material]",
			StackLimit: 999, SlotStart: 121, SlotEnd: 176,
		},
	)
	service, session, connection := openGiveUpQuestTestService(t, repositories, catalog, itemCatalog)

	if err := service.handleGameUpper(session, giveUpQuestTestRequest(t, 5760)); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := repositories.Quest.Load(ctx, "1")
	if err != nil || !ok {
		t.Fatalf("load quest ok=%t err=%v", ok, err)
	}
	if _, exists := stored.States[5760]; exists {
		t.Fatalf("quest 5760 still active: %+v", stored.States[5760])
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:177"]; exists {
		t.Fatalf("quest-family row survived give-up: %+v", inventory.Slots["0:177"])
	}
	if stack := inventory.Slots["0:121"]; stack.ItemID != 3033 || stack.Count != 7 {
		t.Fatalf("unrelated material changed: %+v", stack)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketGiveupQuest) {
		t.Fatalf("give-up ACK header=%+v", ack.Header)
	}
	if want := []byte{0x01, 0x80, 0x16}; !bytes.Equal(ack.Body, want) {
		t.Fatalf("give-up ACK body=%x want=%x", ack.Body, want)
	}
	update, trailing := splitGameServerUpperPacket(t, rest)
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || update.Header.Classification != 0 {
		t.Fatalf("reclaim item update header=%+v", update.Header)
	}
	// body = listType(1) + count(2) + one 0x77 row
	if len(update.Body) != 1+2+currentItemListEntryWireSize || update.Body[0] != 0 || update.Body[1] != 1 {
		t.Fatalf("reclaim item update body len=%d head=%x", len(update.Body), update.Body[:3])
	}
	if slot := binary.LittleEndian.Uint16(update.Body[3:5]); slot != 177 {
		t.Fatalf("reclaim item update slot=%d want 177", slot)
	}
	if marker := binary.LittleEndian.Uint32(update.Body[5:9]); marker != ^uint32(0) {
		t.Fatalf("reclaim item update delete marker=%#x want 0xFFFFFFFF", marker)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing packet=%x", trailing)
	}
}
