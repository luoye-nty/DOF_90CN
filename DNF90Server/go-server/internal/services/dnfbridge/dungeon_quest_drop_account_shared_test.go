package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 任务怪掉落这条路原本用 owner.MutateInventory —— **只给角色背包**，所以这里
// 把事务换成了 dnfdungeon.Owner.MutateAccountCharacterInventory（账号仓 + 角色背包
// 同事务），再走统一入口 grantCurrentAccountSharedFirst。
//
// # 关于这条路的真实现状（实测真实 Script.pvf）
//
// 9487 个 .qst 里，[enemy reward item] 3876 行 + [monster reward item] 5024 行，
// 指向六个晶块/六个灵魂的行是 **0 行** —— 也就是说 90CN 数据里这条路目前根本发不出
// 晶块。改造它的意义是"逻辑归位 + 以后 PVF 加了掉落行不会再漏"，
// 而**真正给晶块的任务侧来源是任务通关奖励**（[reward int data]，全服 90 行命中晶块，
// 见 currentQuestFinishItemAllocator）—— 那条路是另一个待办。
//
// 因此本测试用一个"PVF 里不存在但格式合法"的掉落行来验证落点：只要落点判定是对的，
// 夹具怎么造都不影响结论。
func prepareCurrentQuestDropCubeTest(t *testing.T) (*Service, *gameSession, *runtimeDungeonState, dnfrepo.Group) {
	t.Helper()
	ctx := context.Background()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, currentDungeonUltimateAltarPVFTestSource())
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       70,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	// 现场形态：账号晶体仓里已经堆着 43890 个无色。
	// StackLimit 0 与真实 PVF 一致（这 12 个道具都没有 [stack limit]，所以这个格子
	// 本来就是不封顶的，现场那 43890 也正是这么堆起来的）。
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 43890}},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:      newFakeCurrentDungeonDeathTimerQueue(),
		// 掉落物的定义必须能解析出来，否则掉落会被 game-dungeon-quest-drop-skipped-*
		// 挡掉、测不到落点。真实运行时这个目录来自 Script.pvf，测试里给个等价的最小定义。
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: bridgePVFSource{},
			itemCache: map[uint32]dungeonDropItemDefinition{
				3037: {
					ItemID: 3037, Kind: dungeonDropItemStackable, StackableType: "[material]",
					StackLimit: 0, SlotStart: 1, SlotEnd: 120,
					PVFPath: "stackable/material/cubepiece_clear.stk",
				},
			},
		},
	}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "quest-drop-cube-test",
		selectedCharacterID: 99,
		characterGeneration: 1,
	}
	runtime, _, err := service.prepareDungeonRuntime(ctx, session, dungeoncmd.SelectDungeonRequest{DungeonID: 11007, Difficulty: 1})
	if err != nil {
		t.Fatal(err)
	}
	runtime.lifecycleToken = 1
	// 奖赏行按真实 quest 5651 的形状写（dungeon_id 11006），所以把运行时的副本
	// 也冻到 11006 —— 与 altar_quest_drop_test.go 里那两处做法一致。
	runtime.Dungeon.ID = 11006
	runtime.Request.DungeonID = 11006
	session.dungeon.runtime = runtime
	return service, session, runtime, repositories
}

func currentQuestDropCubeTestCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := bridgePVFSource{
		dnfquest.DefaultList: "5651 `altar_5651.qst`\n",
		"n_quest/altar_5651.qst": "[grade]\n`[normal]`\n" +
			"[type]\n`[seeking]`\n" +
			"[int data]\n3037 1\n" +
			"[enemy reward item]\n56004 1 11006 -1 3037 1 100 999\n[/enemy reward item]\n",
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

func TestCurrentDungeonQuestDropRoutesCubeIntoAccountCell(t *testing.T) {
	ctx := context.Background()
	service, session, runtime, repositories := prepareCurrentQuestDropCubeTest(t)
	service.questCatalog = currentQuestDropCubeTestCatalog(t)
	// 接的任务必须是任务目录里真有的那一条：匹配是按"在做的任务"去查目录的，
	// id 对不上就一条候选都没有（早期版本这里写了个不存在的 999999，
	// 于是掉落一次都没触发、测试假绿）。
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "99",
		States:      map[int64]dnfrepo.QuestState{5651: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.commitCurrentDungeonAltarQuestDropsLocked(session, runtime, []currentDungeonAltarDeathSequence{{
		Sequence: 1,
		Owner: currentDungeonAltarSequenceOwner{
			RoomGeneration:  runtime.altar.Room.Generation,
			RoundGeneration: 2,
			RoomKey:         runtime.altar.Room.Key,
			MonsterCode:     56004,
			QuestDropSeed:   1,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	account, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	stack, exists := account.Slots["0:358"]
	if !exists || stack.ItemID != 3037 || stack.Count <= 43890 {
		t.Fatalf("account crystal cell=%+v exists=%t want 3037 with more than 43890 (the quest drop must land here)", stack, exists)
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "99")
	if err != nil {
		t.Fatal(err)
	}
	for key, bagStack := range inventory.Slots {
		if bagStack.ItemID == 3037 {
			t.Fatalf("the quest-dropped cube must not sit in the backpack: %s=%+v", key, bagStack)
		}
	}
	// 客户端那一半：ACK 必须报 358 格。只落库不发这一行，玩家要等下一次
	// 列表刷新才看得到 —— 而 2026-09-21 那次事故的可见症状正是"背包里那行显示成仓库数量"。
	result, found := runtime.altar.questDropResults[1]
	if !found || len(result.Body) == 0 {
		t.Fatalf("altar quest drop produced no update body: %+v found=%t", result, found)
	}
	assertCurrentQuestDropUpdateRow(t, result.Body, 358, 3037, 43891)
	assertCurrentQuestDropBagEmpty(t, result.Body, 3037)
}

// 账号优先那条事务不可用时（这里：角色记录没有 AccountID），掉落必须**照旧发放**到背包 ——
// 也就是改造前 owner.MutateInventory 的行为。
//
// 这条守卫是 2026-09-21 闸门抓出来的真回归：被动物件掉落（enemy_type=3）的既有夹具里
// 角色没有 AccountID，切事务边界后 MutateAccountCharacterInventory 整体报错，
// 那条路一次都没发出去（4 个测试同时红）。教训：**账号仓不可用只能降级，不能失败**。
func TestCurrentDungeonQuestMonsterDropFallsBackToBackpackWithoutAccountID(t *testing.T) {
	ctx := context.Background()
	service, session, runtime, repositories := prepareCurrentQuestDropCubeTest(t)
	service.questCatalog = currentQuestDropCubeTestCatalog(t)
	character, found, err := repositories.Character.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.AccountID = ""
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "99",
		States:      map[int64]dnfrepo.QuestState{5651: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	var update []byte
	service.executeCurrentDungeonQuestDrops(
		ctx,
		session,
		currentDungeonQuestDropPlan{
			characterID:    99,
			characterKey:   "99",
			accountID:      "account-1",
			dungeonID:      runtime.Dungeon.ID,
			difficulty:     1,
			enemyCode:      56004,
			enemyType:      1,
			includeMonster: true,
		},
		func(body []byte) error {
			update = append([]byte(nil), body...)
			return nil
		},
	)

	account, _, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if stack, exists := account.Slots["0:358"]; exists && stack.Count != 43890 {
		t.Fatalf("the account cell changed although the account route was unavailable: %+v", stack)
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "99")
	if err != nil {
		t.Fatal(err)
	}
	dropped := int64(0)
	for _, bagStack := range inventory.Slots {
		if bagStack.ItemID == 3037 {
			dropped += bagStack.Count
		}
	}
	if dropped == 0 {
		t.Fatal("the drop was lost entirely: the fallback must still grant it to the backpack")
	}
	if len(update) == 0 {
		t.Fatal("fallback drop produced no op14 update body")
	}
	slot, itemID, count := currentQuestDropFirstUpdateRow(t, update)
	if dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(slot)) {
		t.Fatalf("fallback reported account slot %d although the account route was unavailable", slot)
	}
	if itemID != 3037 || count != uint32(dropped) {
		t.Fatalf("fallback update row = item %d count %d, want item 3037 count %d", itemID, count, dropped)
	}
}

// currentQuestDropFirstUpdateRow 解出 op14 更新体里的第一行，用来断言"客户端被告知的是哪一格"。
func currentQuestDropFirstUpdateRow(t *testing.T, body []byte) (uint16, uint32, uint32) {
	t.Helper()
	rowSize := currentItemListEntrySizeForType(dnfrepo.MainInventoryListType)
	if len(body) < 3+rowSize || body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("unexpected update body: want list=%d with at least one %d-byte row, got % x",
			dnfrepo.MainInventoryListType, rowSize, body)
	}
	row := body[3 : 3+rowSize]
	return binary.LittleEndian.Uint16(row[0x00:0x02]),
		binary.LittleEndian.Uint32(row[0x02:0x06]),
		binary.LittleEndian.Uint32(row[0x06:0x0a])
}

// assertCurrentQuestDropUpdateRow 断言更新体里第 index 行就是要报给客户端的那一格。
func assertCurrentQuestDropUpdateRowAt(t *testing.T, body []byte, index int, wantSlot uint16, wantItemID uint32, wantCount uint32) {
	t.Helper()
	rowSize := currentItemListEntrySizeForType(dnfrepo.MainInventoryListType)
	rows := (len(body) - 3) / rowSize
	if index >= rows {
		t.Fatalf("update body has %d rows, want at least %d: % x", rows, index+1, body)
	}
	row := body[3+index*rowSize : 3+(index+1)*rowSize]
	slot := binary.LittleEndian.Uint16(row[0x00:0x02])
	itemID := binary.LittleEndian.Uint32(row[0x02:0x06])
	amount := binary.LittleEndian.Uint32(row[0x06:0x0a])
	if slot != wantSlot || itemID != wantItemID || amount != wantCount {
		t.Fatalf("update row %d = slot %d item %d count %d, want slot %d item %d count %d",
			index, slot, itemID, amount, wantSlot, wantItemID, wantCount)
	}
}

func assertCurrentQuestDropUpdateRow(t *testing.T, body []byte, wantSlot uint16, wantItemID uint32, wantCount uint32) {
	t.Helper()
	assertCurrentQuestDropUpdateRowAt(t, body, 0, wantSlot, wantItemID, wantCount)
}

// assertCurrentQuestDropBagEmpty 断言更新体里没有任何一行报的是背包格里的同一道具。
func assertCurrentQuestDropBagEmpty(t *testing.T, body []byte, itemID uint32) {
	t.Helper()
	rowSize := currentItemListEntrySizeForType(dnfrepo.MainInventoryListType)
	for offset := 3; offset+rowSize <= len(body); offset += rowSize {
		row := body[offset : offset+rowSize]
		if binary.LittleEndian.Uint32(row[0x02:0x06]) != itemID {
			continue
		}
		slot := binary.LittleEndian.Uint16(row[0x00:0x02])
		if dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(slot)) {
			continue
		}
		t.Fatalf("update body reports item %d in backpack slot %d: % x", itemID, slot, body)
	}
}

// 普通地图杀怪（executeCurrentDungeonQuestDrops）与祭坛 op255 是**两份**历史实现，
// 落点规则必须两份都走 currentQuestDropPlaceCandidate。这条测的是普通那份。
func TestCurrentDungeonQuestMonsterDropRoutesCubeIntoAccountCell(t *testing.T) {
	ctx := context.Background()
	service, session, runtime, repositories := prepareCurrentQuestDropCubeTest(t)
	service.questCatalog = currentQuestDropCubeTestCatalog(t)
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "99",
		States:      map[int64]dnfrepo.QuestState{5651: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	var update []byte
	service.executeCurrentDungeonQuestDrops(
		ctx,
		session,
		currentDungeonQuestDropPlan{
			characterID:    99,
			characterKey:   "99",
			accountID:      "account-1",
			dungeonID:      runtime.Dungeon.ID,
			difficulty:     1,
			enemyCode:      56004,
			enemyType:      1,
			includeMonster: true,
		},
		func(body []byte) error {
			update = append([]byte(nil), body...)
			return nil
		},
	)

	account, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	stack, exists := account.Slots["0:358"]
	if !exists || stack.ItemID != 3037 || stack.Count <= 43890 {
		t.Fatalf("account crystal cell=%+v exists=%t want 3037 with more than 43890 (the monster drop must land here)", stack, exists)
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "99")
	if err != nil {
		t.Fatal(err)
	}
	for key, bagStack := range inventory.Slots {
		if bagStack.ItemID == 3037 {
			t.Fatalf("the monster-dropped cube must not sit in the backpack: %s=%+v", key, bagStack)
		}
	}
	if len(update) == 0 {
		t.Fatal("monster quest drop produced no op14 update body")
	}
	assertCurrentQuestDropUpdateRow(t, update, 358, 3037, 43891)
	assertCurrentQuestDropBagEmpty(t, update, 3037)
}
