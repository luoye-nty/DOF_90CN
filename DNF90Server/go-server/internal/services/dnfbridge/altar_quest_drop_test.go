package dnfbridge

import (
	"context"
	"os"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonAltarQuestMonsterDropAutomaticallyRetriesTransactionFailureOnce(t *testing.T) {
	service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	service.questCatalog = currentDungeonAltarQuestDropTestCatalog(t)
	service.pvfItemCatalog = &pvfDungeonDropCatalog{
		source: bridgePVFSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{
			4363: {
				ItemID: 4363, Kind: dungeonDropItemStackable, StackableType: "[quest]",
				StackLimit: 999, SlotStart: 177, SlotEnd: 232,
			},
		},
	}
	// Freeze the real quest-5651 altar monster identity into the server-owned
	// sequence while retaining the compact one-room runtime fixture.
	runtime.Dungeon.ID = 11006
	runtime.Request.DungeonID = 11006
	runtime.altar.boundMonsters[0].MonsterCode = 56004

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	seededCharacter, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character for reward isolation found=%t err=%v", found, err)
	}
	seededCharacter.Stats["exp"] = 54321
	seededCharacter.Stats["gold"] = 12345
	if err := repositories.Character.Save(context.Background(), seededCharacter); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			5651: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	availableRepositories := repositories
	unavailableRepositories := repositories
	unavailableRepositories.CharacterSettlement = nil
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return unavailableRepositories, true }

	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	queue.fire(queue.task(t, 0), false)
	var sequence uint16
	for candidate, owner := range runtime.altar.activeSequences {
		if owner.MonsterCode != 56004 {
			t.Fatalf("sequence=%d monster=%d", candidate, owner.MonsterCode)
		}
		sequence = candidate
	}
	if sequence == 0 {
		t.Fatal("altar monster sequence missing")
	}
	body := []byte{1, byte(sequence), byte(sequence >> 8)}
	beforeFailure := connection.write.Len()
	if err := service.handleCurrentDungeonAltarDeaths(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != beforeFailure {
		t.Fatalf("failed transaction emitted packets: %x", connection.write.Bytes()[beforeFailure:])
	}
	if _, active := runtime.altar.activeSequences[sequence]; !active {
		t.Fatal("failed quest-drop transaction permanently retired sequence")
	}
	retryTask := queue.task(t, 1)
	if retryTask.delay != currentDungeonAltarQuestDropRetryDelay {
		t.Fatalf("quest-drop retry delay=%s want=%s", retryTask.delay, currentDungeonAltarQuestDropRetryDelay)
	}
	pending := runtime.altar.questDropRetry
	if pending == nil || len(pending.Sequences) != 1 || pending.Sequences[0] != sequence ||
		len(pending.Deaths) != 1 || pending.Deaths[0].Sequence != sequence || pending.Version == 0 {
		t.Fatalf("quest-drop retry was not frozen: %+v", pending)
	}

	service.repositoryProvider = func() (dnfrepo.Group, bool) { return availableRepositories, true }
	characterBefore, found, err := availableRepositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character before death found=%t err=%v", found, err)
	}
	nextObjectKeyBefore := runtime.NextObjectKey
	beforeSuccess := connection.write.Len()
	if err := service.fireCurrentDungeonAltarQuestDropRetry(
		session,
		runtime,
		runtime.lifecycleToken+1,
		pending.Room,
		pending.Version,
		retryTask.name,
		queue.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != beforeSuccess {
		t.Fatalf("stale lifecycle retry emitted packet: %x", connection.write.Bytes()[beforeSuccess:])
	}
	if _, active := runtime.altar.activeSequences[sequence]; !active {
		t.Fatal("stale lifecycle retry retired sequence")
	}
	queue.fire(retryTask, false)
	if _, active := runtime.altar.activeSequences[sequence]; active {
		t.Fatal("server-owned retry did not retire sequence")
	}
	if _, retired := runtime.altar.retiredSequences[sequence]; !retired {
		t.Fatal("server-owned retry did not record retired sequence")
	}
	if runtime.altar.questDropRetry != nil || runtime.altar.questDropRetryTimerName != "" {
		t.Fatalf("successful server retry remained armed: pending=%+v timer=%q",
			runtime.altar.questDropRetry, runtime.altar.questDropRetryTimerName)
	}
	stream := connection.write.Bytes()[beforeSuccess:]
	itemUpdates := 0
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) {
			itemUpdates++
		} else if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonAltarDifficultyUIMsgID {
			t.Fatalf("altar quest death emitted unsupported ordinary reward packet: class=%d msg=%d body=%x",
				packet.Header.Classification, packet.Header.MsgID, packet.Body)
		}
		stream = rest
	}
	if itemUpdates != 1 {
		t.Fatalf("quest item update packets=%d", itemUpdates)
	}
	inventory, found, err := availableRepositories.Inventory.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	stack := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 177)]
	if stack.ItemID != 4363 || stack.Count != 1 {
		t.Fatalf("quest-5651 item stack=%+v", stack)
	}
	characterAfter, found, err := availableRepositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character after death found=%t err=%v", found, err)
	}
	if !reflect.DeepEqual(characterAfter.Stats, characterBefore.Stats) {
		t.Fatalf("altar task-drop path changed ordinary character rewards: before=%v after=%v", characterBefore.Stats, characterAfter.Stats)
	}
	if characterAfter.Stats["exp"] != 54321 || characterAfter.Stats["gold"] != 12345 ||
		runtime.settlementMonsterExperienceTotal != 0 {
		t.Fatalf("altar task-drop path granted ordinary exp/gold: stats=%v monster_exp_total=%d",
			characterAfter.Stats, runtime.settlementMonsterExperienceTotal)
	}
	if runtime.DropOwner != nil || runtime.NextObjectKey != nextObjectKeyBefore {
		t.Fatalf("altar task-drop path created ordinary ground drops: owner=%+v next=%d want_next=%d",
			runtime.DropOwner, runtime.NextObjectKey, nextObjectKeyBefore)
	}

	beforeStale := connection.write.Len()
	queue.fire(retryTask, true)
	if connection.write.Len() != beforeStale {
		t.Fatalf("stale completed retry emitted packet: %x", connection.write.Bytes()[beforeStale:])
	}
	inventory, found, err = availableRepositories.Inventory.Load(context.Background(), "99")
	if err != nil || !found || inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 177)].Count != 1 {
		t.Fatalf("stale completed retry duplicated quest item: inventory=%+v found=%t err=%v", inventory, found, err)
	}

	beforeDuplicate := connection.write.Len()
	if err := service.handleCurrentDungeonAltarDeaths(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != beforeDuplicate {
		t.Fatalf("duplicate sequence emitted packet: %x", connection.write.Bytes()[beforeDuplicate:])
	}
	inventory, found, err = availableRepositories.Inventory.Load(context.Background(), "99")
	if err != nil || !found || inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 177)].Count != 1 {
		t.Fatalf("duplicate sequence granted quest item twice: inventory=%+v found=%t err=%v", inventory, found, err)
	}
}

func TestCurrentDungeonAltarQuestDropBatchesMaximumDeathListIntoOneTransaction(t *testing.T) {
	service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	service.questCatalog = currentDungeonAltarQuestDropTestCatalog(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			999999: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	counter := &countingCurrentDungeonAltarSettlement{base: repositories.CharacterSettlement}
	repositories.CharacterSettlement = counter
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }

	deaths := make([]currentDungeonAltarDeathSequence, 0, currentDungeonAltarMaximumDeathSequences)
	for sequence := uint16(1); sequence <= currentDungeonAltarMaximumDeathSequences; sequence++ {
		deaths = append(deaths, currentDungeonAltarDeathSequence{
			Sequence: sequence,
			Owner: currentDungeonAltarSequenceOwner{
				RoomGeneration:  runtime.altar.Room.Generation,
				RoundGeneration: 2,
				RoomKey:         runtime.altar.Room.Key,
				MonsterCode:     56004,
				QuestDropSeed:   uint32(sequence),
			},
		})
	}
	if err := service.commitCurrentDungeonAltarQuestDropsLocked(session, runtime, deaths); err != nil {
		t.Fatal(err)
	}
	if counter.calls != 1 {
		t.Fatalf("maximum op255 batch transactions=%d want=1", counter.calls)
	}
	if len(runtime.altar.questDropResults) != currentDungeonAltarMaximumDeathSequences {
		t.Fatalf("cached no-drop results=%d want=%d", len(runtime.altar.questDropResults), currentDungeonAltarMaximumDeathSequences)
	}
	for _, death := range deaths {
		if result, found := runtime.altar.questDropResults[death.Sequence]; !found || !result.Delivered || len(result.Body) != 0 {
			t.Fatalf("sequence=%d cached result=%+v found=%t", death.Sequence, result, found)
		}
	}
}

func TestRealScriptPVFQuest5651UsesTypeOneEnemyRewardItem(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	}
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE or DNF_WORLDMAP_REAL_PVF_SMOKE to verify quest 5651")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(5651)
	if !found {
		t.Fatal("real quest 5651 missing")
	}
	for _, entry := range definition.EnemyRewardItems {
		if entry.EnemyType == 1 && entry.EnemyCode == 56004 && entry.ItemID == 4363 && entry.DropRate == 100 {
			return
		}
	}
	t.Fatalf("quest 5651 enemy reward rows=%+v", definition.EnemyRewardItems)
}

type countingCurrentDungeonAltarSettlement struct {
	base  dnfrepo.CharacterSettlementUnitOfWork
	calls int
}

func (counter *countingCurrentDungeonAltarSettlement) WithinCharacterSettlement(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.Group) error,
) error {
	counter.calls++
	return counter.base.WithinCharacterSettlement(ctx, characterID, apply)
}

func currentDungeonAltarQuestDropTestCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := bridgePVFSource{
		dnfquest.DefaultList: "5651 `altar_5651.qst`\n",
		"n_quest/altar_5651.qst": "[grade]\n`[normal]`\n" +
			"[type]\n`[seeking]`\n" +
			"[int data]\n4363 1\n" +
			"[enemy reward item]\n56004 1 11006 -1 4363 1 100 999\n[/enemy reward item]\n",
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
