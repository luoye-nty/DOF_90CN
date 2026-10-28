package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 任务通关奖励（PVF [reward int data]）是**真实数据里唯一还在漏的晶块来源**：
// 实测真实 Script.pvf 的 9487 个 .qst 里，命中六个晶块/六个灵魂的
// [enemy reward item] / [monster reward item] 行是 0 行，而 [reward int data]
// 命中晶块 90 行、[reward selection int data] 4 行（例如 3037 x100 / 3037 x1000）。
//
// 这条路由 services/dnfbridge/quest_finish.go 的 currentQuestFinishItemAllocator 发放，
// 它过去只调 addCurrentDungeonPickupToInventory —— 纯角色背包，完全不认账号仓，
// 于是 20 个无色又落进背包，与账号仓那 43890 个形成"同一条 list-0 报文两行同 ID"，
// 客户端把背包那行显示成仓库的数量（2026-09-21 事故的原始症状）。
//
// 本轮的接法：账号记录随 FinishItemGrantRequest 进入发放器（它本来就在结算事务里，
// finish_owner.go 早已 Load/Clone/Save 过），发放器先走统一入口落固定格。
func prepareCurrentFinishQuestCubeTest(t *testing.T) (dnfrepo.Group, *dnfquest.Catalog, string) {
	t.Helper()
	ctx := context.Background()
	repositories := finishBridgeSeed(t)
	accountID := defaultAccountPrefix + "1"
	// 现场形态：账号晶体仓里已经堆着 43890 个无色。
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 43890}},
	}); err != nil {
		t.Fatal(err)
	}
	return repositories, finishBridgeCubeRewardCatalog(t), accountID
}

// finishBridgeCubeRewardCatalog 是既有 finishBridgeCatalog 的镜像，只把 3145 的奖励
// 从 `10403 x2` 换成 `3037 x20` —— 与玩家实际遇到的那次完全同形（含后继任务 3146，
// 保证任务链推进逻辑与既有用例逐字一致）。
func finishBridgeCubeRewardCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := finishBridgePVFSource{
		dnfquest.DefaultList: "3145 `finish.qst`\n3146 `next.qst`\n",
		"n_quest/finish.qst": "[grade]\n`[epic]`\n[type]\n`[clear map]`\n[difficulty]\n`N`\n[reward type]\n`[item]`\n[reward int data]\n0 1 3037 20\n[level]\n1 99\n[job]\n`[gunner]`\n[exposed by npc]\n1\n[int data]\n76126 0 0 1\n",
		"n_quest/next.qst":   "[grade]\n`[epic]`\n[type]\n`[quest clear]`\n[difficulty]\n`N`\n[reward type]\n`[item]`\n[reward int data]\n10404 1\n[level]\n2 99\n[job]\n`[gunner]`\n[exposed by npc]\n1\n[pre required quest]\n3145\n[int data]\n3157 3054\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func finishBridgeCubeItemCatalog() *pvfDungeonDropCatalog {
	// StackLimit 0 与真实 PVF 一致：这 12 个道具都没有 [stack limit]。
	return &pvfDungeonDropCatalog{
		source: bridgePVFSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{
			3037: {
				ItemID: 3037, Kind: dungeonDropItemStackable, StackableType: "[material]",
				StackLimit: 0, SlotStart: 1, SlotEnd: 120,
				PVFPath: "stackable/material/cubepiece_clear.stk",
			},
		},
	}
}

func currentFinishQuestRequest() dnfquest.FinishQuestRequest {
	return dnfquest.FinishQuestRequest{
		QuestID:           3145,
		RewardSelectIndex: ^uint16(0),
		Multiplier:        1,
		Reserved:          dnfquest.CurrentFinishQuestObservedTailMarker,
	}
}

// currentFinishQuestUpdateRows 取出任务结算后 op14 更新报文里的全部行。
//
// 报文里第一行通常是金币行（patchCore(0,0,gold)），所以断言必须按 itemID 找目标行 ——
// 而且"同一条 list-0 报文里该 itemID 只允许出现一行"正是 2026-09-21 事故的核心不变量：
// 当时背包 154 与账号格 358 各有一行 3037，客户端把背包那行显示成了仓库数量。
func currentFinishQuestUpdateRows(t *testing.T, connection *bufferConn, marker int) [][]byte {
	t.Helper()
	packets := splitAllFinishBridgePackets(t, connection.write.Bytes()[marker:])
	var rows [][]byte
	for _, packet := range packets {
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
			continue
		}
		body := packet.Body
		if len(body) < 3 || body[0] != dnfrepo.MainInventoryListType {
			t.Fatalf("unexpected finish item update body=%x", body)
		}
		count := int(binary.LittleEndian.Uint16(body[1:3]))
		if len(body) != 3+count*currentItemListEntryWireSize {
			t.Fatalf("finish item update body rows=%d len=%d", count, len(body))
		}
		for index := 0; index < count; index++ {
			start := 3 + index*currentItemListEntryWireSize
			rows = append(rows, body[start:start+currentItemListEntryWireSize])
		}
	}
	if len(rows) == 0 {
		t.Fatalf("no item update packet in %v", finishBridgePacketIDs(packets))
	}
	return rows
}

// currentFinishQuestUpdateRowForItem 返回该 itemID 在更新报文里的唯一一行。
func currentFinishQuestUpdateRowForItem(t *testing.T, connection *bufferConn, marker int, itemID uint32) []byte {
	t.Helper()
	var found [][]byte
	for _, row := range currentFinishQuestUpdateRows(t, connection, marker) {
		if binary.LittleEndian.Uint32(row[2:6]) == itemID {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("update body must report item %d exactly once, got %d rows", itemID, len(found))
	}
	return found[0]
}

func TestCurrentFinishQuestRewardRoutesCubeIntoAccountCell(t *testing.T) {
	ctx := context.Background()
	repositories, catalog, accountID := prepareCurrentFinishQuestCubeTest(t)
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "finish-quest-cube", selectedCharacterID: 19}
	allocator := currentQuestFinishItemAllocator(finishBridgeCubeItemCatalog(), 24)

	marker := connection.write.Len()
	if err := service.handleCurrentFinishQuestWithResources(
		session, currentFinishQuestRequest(), repositories, catalog, finishBridgeProgression(t), allocator,
	); err != nil {
		t.Fatal(err)
	}

	// 1. 账号记录必须真的落库了（这是 finish_owner.go 里 save 条件的改动：
	//    过去只有"交材料"才存账号记录，"发奖励"改到它却不会存）。
	account, found, err := repositories.AccountInventory.Load(ctx, accountID)
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	stack, exists := account.Slots["0:358"]
	if !exists || stack.ItemID != 3037 || stack.Count != 43910 {
		t.Fatalf("account crystal cell=%+v exists=%t want 3037 x43910 (43890 + the 20 cube reward)", stack, exists)
	}
	// 2. 背包里不能再出现无色小晶块 —— 事故的可见症状就是背包里多出一行。
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	for key, bagStack := range inventory.Slots {
		if bagStack.ItemID == 3037 {
			t.Fatalf("the quest reward must not sit in the backpack: %s=%+v", key, bagStack)
		}
	}
	// 3. 客户端那一半：更新报文必须**只**报 358 格一次，报的也是并库后的 43910。
	row := currentFinishQuestUpdateRowForItem(t, connection, marker, 3037)
	slot := binary.LittleEndian.Uint16(row[0:2])
	itemID := binary.LittleEndian.Uint32(row[2:6])
	count := binary.LittleEndian.Uint32(row[6:10])
	if slot != 358 || itemID != 3037 || count != 43910 {
		t.Fatalf("finish reward update row = slot %d item %d count %d, want slot 358 item 3037 count 43910",
			slot, itemID, count)
	}

	// 4. 重放：重连后同一任务再结算一次，必须走收据重放而不是判 stale。
	//    finish_owner.go 的收据校验过去只在角色背包里找奖励行，奖励落账号格之后
	//    会直接 ErrFinishQuestSettlementStale —— 那时玩家重连就再也拿不到那张回执。
	reconnect := &gameSession{conn: connection, connID: "finish-quest-cube-replay", selectedCharacterID: 19}
	replayMarker := connection.write.Len()
	if err := service.handleCurrentFinishQuestWithResources(
		reconnect, currentFinishQuestRequest(), repositories, catalog, finishBridgeProgression(t), allocator,
	); err != nil {
		t.Fatalf("replay: %v", err)
	}
	packets := splitAllFinishBridgePackets(t, connection.write.Bytes()[replayMarker:])
	if len(packets) == 0 || packets[0].Header.MsgID != uint16(dnfenum.CmdPacketFinishQuest) ||
		len(packets[0].Body) < 2 || packets[0].Body[0] != 1 {
		t.Fatalf("replay receipt missing: %v", finishBridgePacketIDs(packets))
	}
	account, _, err = repositories.AccountInventory.Load(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if again := account.Slots["0:358"]; again.Count != 43910 {
		t.Fatalf("replay duplicated the reward: %+v", again)
	}
}

// 固定格被别人占着时（唯一会真的走不到账号仓的情况；线上这 12 个道具没有
// [stack limit]，固定格不封顶），整份奖励退回背包、占用者的行一点不动 ——
// 与翻牌 P2-2 同一口径"绝不半进半留"。
//
// 为什么必须整份退回而不是拆行：任务结算的收据一行只能描述一个格子，
// 而 allocateFinishRewardItems 要求 Delta 等于整份数量；拆行会让 PostCount 对不上
// Delta，校验直接失败并把**整笔任务结算**打挂（玩家奖励拿不到、任务也完不成）。
func TestCurrentFinishQuestRewardFallsBackToBackpackWhenCellOccupied(t *testing.T) {
	ctx := context.Background()
	repositories, catalog, accountID := prepareCurrentFinishQuestCubeTest(t)
	// 358 格里塞一个外来道具：唯一入库原语**绝不驱赶外来占用者**，只会拒绝。
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 9999, Count: 7},
			"0:359": {ItemID: 3262, Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "finish-quest-cube-occupied", selectedCharacterID: 19}
	allocator := currentQuestFinishItemAllocator(finishBridgeCubeItemCatalog(), 24)

	marker := connection.write.Len()
	if err := service.handleCurrentFinishQuestWithResources(
		session, currentFinishQuestRequest(), repositories, catalog, finishBridgeProgression(t), allocator,
	); err != nil {
		t.Fatalf("occupied cell must not fail the settlement: %v", err)
	}

	account, _, err := repositories.AccountInventory.Load(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if occupant := account.Slots["0:358"]; occupant.ItemID != 9999 || occupant.Count != 7 {
		t.Fatalf("occupant was disturbed: %+v", occupant)
	}
	if _, exists := account.Slots["0:359"]; exists {
		if crystal := account.Slots["0:359"]; crystal.ItemID != 3262 || crystal.Count != 3 {
			t.Fatalf("unrelated account cell changed: %+v", crystal)
		}
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	dropped := int64(0)
	for _, bagStack := range inventory.Slots {
		if bagStack.ItemID == 3037 {
			dropped += bagStack.Count
		}
	}
	if dropped != 20 {
		t.Fatalf("the whole reward must fall back to the backpack: got %d of 20 (cells=%+v)", dropped, inventory.Slots)
	}
	row := currentFinishQuestUpdateRowForItem(t, connection, marker, 3037)
	slot := binary.LittleEndian.Uint16(row[0:2])
	if dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(slot)) {
		t.Fatalf("update row reported account slot %d although the whole reward went to the backpack: %x", slot, row[:10])
	}
	if count := binary.LittleEndian.Uint32(row[6:10]); count != 20 {
		t.Fatalf("update row count=%d want the whole 20 in the backpack", count)
	}
}
