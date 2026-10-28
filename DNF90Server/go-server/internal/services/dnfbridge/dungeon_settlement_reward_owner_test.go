package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/experiencebonus"
	"longheng.io/server/internal/modules/dnf/premium"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestStoryRecommendedDungeonRentalCatalogFailurePrecedesSettlementReceipt(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1", Metadata: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 1
	character.Stats["exp"] = 90
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "99",
		Points:      dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = dnfrepo.CloneCharacter(character)
	// 推荐副本判定读的是**进图那一刻**的等级快照，所以测试直接换掉 runtime.Character
	// 时必须同步刷新它——否则等级被改成 1、快照还停在 20，判定会得出与真实链路
	// 相反的结论（真实链路的快照在 PrepareDungeon 里一次性写入）。
	runtime.entryCharacterLevel = character.Level
	runtime.ordinaryFinalRoomClearAccepted = true
	runtime.MazeIndex = 0
	runtime.Dungeon.Mazes = []worldmap.Maze{{QuestConnection: []int64{0, 3145, -1}}}
	runtime.Dungeon.Metadata.RecommendedLevels = nil
	runtime.Dungeon.Metadata.MinimumRequiredLevel = worldmap.OptionalInt{Set: true, Value: 1}
	runtime.Dungeon.Metadata.BasisLevel = worldmap.OptionalInt{Set: true, Value: 1}
	runtime.Dungeon.Metadata.NoFatigue = false
	sentinel := errors.New("synthetic rental catalog unavailable")
	service.initialEquipmentArchive = nil
	service.initialEquipmentLoadErr = sentinel

	resources := currentDungeonSettlementTestResources(t)
	if _, err := service.commitCurrentDungeonSettlement(ctx, session, runtime, resources.Progression, 20); !errors.Is(err, sentinel) {
		t.Fatalf("commit error=%v want catalog failure", err)
	}
	character, found, err = repositories.Character.Load(ctx, "99")
	if err != nil || !found || character.Level != 1 || character.Stats["exp"] != 90 ||
		character.Stats["dungeon_settlement_receipt_version"] != 0 {
		t.Fatalf("character changed before catalog recovery=%+v found=%t err=%v", character, found, err)
	}
	skill, found, err := repositories.Skill.Load(ctx, "99")
	if err != nil || !found || skill.Points.SyncedLevel != 1 || skill.Points.TotalSP != 10 {
		t.Fatalf("skill changed before catalog recovery=%+v found=%t err=%v", skill.Points, found, err)
	}
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found || len(account.Metadata) != 0 {
		t.Fatalf("account changed before catalog recovery=%+v found=%t err=%v", account, found, err)
	}
}

func TestCurrentDungeonSettlementProducerCommitsProgressionAndFreezesRealPickupReceiptOnce(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 1
	character.Stats["exp"] = 90
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{
		CharacterID: "99",
		Points: dnfrepo.SkillPointState{
			TotalSP: 10, RemainingSP: 10, SyncedLevel: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": {ItemID: 8474, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = character
	runtime.Dungeon.Metadata.BasisLevel.Set = true
	runtime.Dungeon.Metadata.BasisLevel.Value = 1
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Set = true
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Value = 1
	runtime.Request.Difficulty = 1
	runtime.DropOwner = newRuntimeDungeonDropOwner()
	runtime.DropOwner.byObjectKey[70001] = &runtimeDungeonDrop{
		ObjectKey: 70001, Item: dungeonDropItemDefinition{ItemID: 8474}, Amount: 1,
		Status: runtimeDungeonDropConsumed, DestinationSlot: 65,
	}
	// A later pickup may legitimately reuse a slot after the earlier item was
	// consumed, moved, or replaced. That stale presentation receipt must not
	// suppress the authoritative EXP/SP settlement or the still-valid receipt.
	runtime.DropOwner.byObjectKey[70002] = &runtimeDungeonDrop{
		ObjectKey: 70002, Item: dungeonDropItemDefinition{ItemID: 9000}, Amount: 1,
		Status: runtimeDungeonDropConsumed, DestinationSlot: 66,
	}
	runtime.settlementPlayResultReceived = true
	runtime.settlementPlayResultBody = make([]byte, currentDungeonPlayResultBaseSize)
	runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset] = 73
	runtime.startedAt = time.Unix(1700000000, 0)
	runtime.clearMapCompletionAt = runtime.startedAt.Add(12*time.Second + 345*time.Millisecond)

	resources := currentDungeonSettlementTestResources(t)
	if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(
		context.Background(), session, runtime, resources,
	); err != nil {
		t.Fatal(err)
	}
	if runtime.settlementResultPlan == nil {
		t.Fatal("settlement plan was not frozen")
	}
	assertCurrentDungeonSettlementProducerPlayResultBody(t, runtime.settlementResultPlan.PlayResultBody)
	assertCurrentDungeonSettlementProducerRewardBody(t, runtime.settlementResultPlan.ClearRewardBody)
	committed, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load committed character found=%t err=%v", found, err)
	}
	if committed.Level != 3 || committed.Stats["exp"] != 310 {
		t.Fatalf("committed progression level=%d exp=%d", committed.Level, committed.Stats["exp"])
	}
	skill, found, err := repositories.Skill.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load committed skill found=%t err=%v", found, err)
	}
	if skill.Points.TotalSP != 70 || skill.Points.RemainingSP != 70 || skill.Points.SyncedLevel != 3 {
		t.Fatalf("committed skill points=%+v", skill.Points)
	}

	// Force a producer retry after the database commit. The durable receipt
	// must reconstruct the same result without granting EXP/SP a second time.
	firstPlan := *runtime.settlementResultPlan
	runtime.settlementResultPlan = nil
	if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(
		context.Background(), session, runtime, resources,
	); err != nil {
		t.Fatal(err)
	}
	replayed, _, _ := repositories.Character.Load(context.Background(), "99")
	replayedSkill, _, _ := repositories.Skill.Load(context.Background(), "99")
	if replayed.Level != 3 || replayed.Stats["exp"] != 310 || replayedSkill.Points != skill.Points {
		t.Fatalf("producer retry duplicated progression character=%+v skill=%+v", replayed, replayedSkill.Points)
	}
	if string(runtime.settlementResultPlan.PlayResultBody) != string(firstPlan.PlayResultBody) ||
		string(runtime.settlementResultPlan.CharacterBody) != string(firstPlan.CharacterBody) ||
		string(runtime.settlementResultPlan.ClearRewardBody) != string(firstPlan.ClearRewardBody) {
		t.Fatal("durable producer retry changed frozen packet plan")
	}
}

func TestCurrentDungeonSettlementProjectsOnlyVerifiedPendingQuestRows(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Quest == nil {
		t.Fatal("quest repository unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 1
	character.Stats["exp"] = 90
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{
		CharacterID: "99",
		Points: dnfrepo.SkillPointState{
			TotalSP: 10, RemainingSP: 10, SyncedLevel: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = character

	runtime.settlementPlayResultReceived = true
	runtime.settlementPlayResultBody = make([]byte, currentDungeonPlayResultBaseSize)
	runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset] = 73
	runtime.clearMapCompletionKey = "settlement-quest-3177"
	runtime.clearMapCompletionPendingQuestIDs = []int64{3178, 3177, 2177, 3179, 3180, 3177, 0, -1, math.MaxUint32 + 1}
	runtime.Dungeon.Metadata.BasisLevel.Set = true
	runtime.Dungeon.Metadata.BasisLevel.Value = 1
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Set = true
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Value = 1
	runtime.Request.Difficulty = 1
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			2177: {
				Status:        "active",
				ProgressValue: 0,
				Extra: map[string]string{
					"completion_key": "settlement-quest-3177",
					"reward_state":   "pending",
				},
			},
			3177: {
				Status:        "active",
				ProgressValue: 0,
				Extra: map[string]string{
					"completion_key": "settlement-quest-3177",
					"reward_state":   "pending",
				},
			},
			3178: {
				Status:        "active",
				ProgressValue: 0,
				Extra: map[string]string{
					"completion_key": "stale-settlement-key",
					"reward_state":   "pending",
				},
			},
			3179: {
				Status:        "active",
				ProgressValue: 1,
				Extra: map[string]string{
					"completion_key": "settlement-quest-3177",
					"reward_state":   "pending",
				},
			},
			3180: {
				Status:        "completed",
				ProgressValue: 0,
				Extra: map[string]string{
					"completion_key": "settlement-quest-3177",
					"reward_state":   "pending",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	resources := currentDungeonSettlementTestResources(t)
	if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(
		context.Background(), session, runtime, resources,
	); err != nil {
		t.Fatal(err)
	}
	if runtime.settlementResultPlan == nil {
		t.Fatal("settlement plan was not frozen")
	}
	firstBody := append([]byte(nil), runtime.settlementResultPlan.ClearRewardBody...)
	if got := currentDungeonClearRewardQuestPairsFromBody(t, firstBody); len(got) != 2 ||
		got[0] != (currentDungeonClearRewardPair{Key: 2177, Value: currentDungeonClearRewardQuestCompletedState}) ||
		got[1] != (currentDungeonClearRewardPair{Key: 3177, Value: currentDungeonClearRewardQuestCompletedState}) {
		t.Fatalf("op35 quest pairs=%+v", got)
	}

	// A transport retry rebuilds from the same persisted completion and must
	// keep the entire op35 payload byte-identical.
	runtime.settlementResultPlan = nil
	if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(
		context.Background(), session, runtime, resources,
	); err != nil {
		t.Fatal(err)
	}
	if got := runtime.settlementResultPlan.ClearRewardBody; string(got) != string(firstBody) {
		t.Fatalf("replayed op35 changed\n got=%x\nwant=%x", got, firstBody)
	}
}

func currentDungeonClearRewardQuestPairsFromBody(t *testing.T, body []byte) []currentDungeonClearRewardPair {
	t.Helper()
	reader := settlementProtocolReader{body: body}
	for range 4 {
		reader.u32()
	}
	reader.u8()
	for range currentDungeonClearRewardBonusFieldCount {
		reader.u32()
	}
	for range 2 {
		for range reader.u8() {
			reader.u32()
			reader.u32()
		}
	}
	for range currentDungeonClearRewardPostBaseFieldCount {
		reader.u32()
	}
	for range currentDungeonClearRewardScoreFieldCount {
		reader.u32()
	}
	rows := make([]currentDungeonClearRewardPair, reader.u32())
	for index := range rows {
		rows[index] = currentDungeonClearRewardPair{Key: reader.u32(), Value: reader.u32()}
	}
	return rows
}

func assertCurrentDungeonSettlementProducerPlayResultBody(t *testing.T, body []byte) {
	t.Helper()
	if len(body) != 16 {
		t.Fatalf("op34 body len=%d want=16", len(body))
	}
	reader := settlementProtocolReader{body: body}
	if reader.u8() != 60 || reader.u32() != 12345 || reader.u8() != 0 ||
		reader.u8() != 73 || reader.u8() != 1 || reader.u8() != 1 ||
		reader.u16() != 99 || reader.u32() != 12345 || reader.u8() != 0 ||
		reader.offset != len(body) {
		t.Fatalf("op34 presentation/body mismatch body=%x offset=%d", body, reader.offset)
	}
}

func assertCurrentDungeonSettlementProducerRewardBody(t *testing.T, body []byte) {
	t.Helper()
	reader := settlementProtocolReader{body: body}
	if got := reader.u32(); got != 200 {
		t.Fatalf("op35 clear base experience=%d, want base=200", got)
	}
	// Base[1] = score bonus (200 x 0.10 rank rate for rank point 73).
	if got := reader.u32(); got != 20 {
		t.Fatalf("op35 score bonus=%d, want 20", got)
	}
	for index := 2; index < 4; index++ {
		if got := reader.u32(); got != 0 {
			t.Fatalf("unknown op35 base[%d]=%d, want zero", index, got)
		}
	}
	if got := reader.u8(); got != 0 {
		t.Fatalf("unknown op35 base flag=%d, want zero", got)
	}
	for index := 0; index < currentDungeonClearRewardBonusFieldCount; index++ {
		if got := reader.u32(); got != 0 {
			t.Fatalf("unknown op35 bonus[%d]=%d, want zero", index, got)
		}
	}
	if reader.u8() != 0 || reader.u8() != 0 {
		t.Fatal("unknown op35 bonus groups are nonempty")
	}
	for index := 0; index < currentDungeonClearRewardPostBaseFieldCount; index++ {
		if got := reader.u32(); got != 0 {
			t.Fatalf("unknown op35 post-base[%d]=%d, want zero", index, got)
		}
	}
	for index := 0; index < currentDungeonClearRewardScoreFieldCount; index++ {
		if got := reader.u32(); got != 0 {
			t.Fatalf("unknown op35 score[%d]=%d, want zero", index, got)
		}
	}
	// 现场 2026-09-18（魔界材料在结算瞬间翻倍）：op35 的 Drops 段会被客户端当成
	// "本次结算发放量"再加一次，所以捡取收据一律不再投影进 op35——这里必须是 0 行。
	if reader.u32() != 0 || reader.u8() != 0 {
		t.Fatal("op35 不应再带捡取收据行（客户端会二次发放）")
	}
	for index := 0; index < currentDungeonClearRewardGroupCount; index++ {
		if reader.u8() != 0 {
			t.Fatalf("op35 card slot %d is active", index)
		}
	}
	if reader.u32() != 0 {
		t.Fatal("unknown op35 total reward is nonzero")
	}
	for index := 0; index < currentDungeonClearRewardGroupCount*2; index++ {
		if reader.u8() != 0 {
			t.Fatalf("unknown op35 aux group %d is nonempty", index)
		}
	}
	if reader.u32() != 0 {
		t.Fatal("unknown op35 pre-tail value is nonzero")
	}
	if reader.u32() != 0 || reader.u8() != 1 || reader.u8() != 0 ||
		reader.u32() != 0 || reader.u32() != 0 || reader.u32() != 0 || reader.u32() != 0 ||
		reader.offset != len(body) {
		t.Fatalf("op35 current reader tail/boundary mismatch offset=%d len=%d", reader.offset, len(body))
	}
}

func TestCurrentDungeonCommittedDropReceiptsRejectsRuntimeDBMismatch(t *testing.T) {
	runtime := &runtimeDungeonState{DropOwner: newRuntimeDungeonDropOwner()}
	runtime.DropOwner.byObjectKey[1] = &runtimeDungeonDrop{
		ObjectKey: 1, Item: dungeonDropItemDefinition{ItemID: 8474},
		Status: runtimeDungeonDropConsumed, DestinationSlot: 65,
	}
	_, err := currentDungeonCommittedDropReceipts(runtime, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": {ItemID: 9000, Count: 1},
		},
	})
	if err == nil {
		t.Fatal("runtime/DB pickup mismatch was accepted")
	}
}

func TestCurrentDungeonCommittedDropReceiptsIgnoreWholePickupThatIsCurrentlyDiscarded(t *testing.T) {
	runtime := &runtimeDungeonState{DropOwner: newRuntimeDungeonDropOwner()}
	runtime.DropOwner.byObjectKey[1] = &runtimeDungeonDrop{
		ObjectKey: 1, Item: dungeonDropItemDefinition{ItemID: 8474},
		Amount: 3, Status: runtimeDungeonDropConsumed, DestinationSlot: 65,
	}
	runtime.DropOwner.byObjectKey[2] = &runtimeDungeonDrop{
		ObjectKey: 2, Item: dungeonDropItemDefinition{ItemID: 8474},
		Status: runtimeDungeonDropAvailable,
		DiscardOrigin: &runtimeDungeonDiscardOrigin{
			ListType:   dnfrepo.MainInventoryListType,
			SourceSlot: 65,
			Stack:      dnfrepo.ItemStack{ItemID: 8474, Count: 3},
		},
	}
	receipts, err := currentDungeonCommittedDropReceipts(runtime, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("discarded pickup remained in settlement receipts: %+v", receipts)
	}
}

func TestCurrentDungeonCommittedDropReceiptsKeepPartiallyDiscardedPickupRemainder(t *testing.T) {
	runtime := &runtimeDungeonState{DropOwner: newRuntimeDungeonDropOwner()}
	runtime.DropOwner.byObjectKey[1] = &runtimeDungeonDrop{
		ObjectKey: 1, Item: dungeonDropItemDefinition{ItemID: 8474},
		Amount: 3, Status: runtimeDungeonDropConsumed, DestinationSlot: 65,
	}
	runtime.DropOwner.byObjectKey[2] = &runtimeDungeonDrop{
		ObjectKey: 2, Item: dungeonDropItemDefinition{ItemID: 8474},
		Amount: 2, Status: runtimeDungeonDropAvailable,
		DiscardOrigin: &runtimeDungeonDiscardOrigin{
			ListType:   dnfrepo.MainInventoryListType,
			SourceSlot: 65,
			Stack:      dnfrepo.ItemStack{ItemID: 8474, Count: 2},
		},
	}
	receipts, err := currentDungeonCommittedDropReceipts(runtime, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": {ItemID: 8474, Count: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].ObjectKey != 65 ||
		receipts[0].TemplateID != 8474 || receipts[0].StackCount != 1 {
		t.Fatalf("partial discard receipts=%+v, want remaining slot count 1", receipts)
	}
}

func TestCurrentDungeonCommittedDropReceiptsRejectDiscardAmountThatCannotExplainMissingPickup(t *testing.T) {
	runtime := &runtimeDungeonState{DropOwner: newRuntimeDungeonDropOwner()}
	runtime.DropOwner.byObjectKey[1] = &runtimeDungeonDrop{
		ObjectKey: 1, Item: dungeonDropItemDefinition{ItemID: 8474},
		Amount: 3, Status: runtimeDungeonDropConsumed, DestinationSlot: 65,
	}
	runtime.DropOwner.byObjectKey[2] = &runtimeDungeonDrop{
		ObjectKey: 2, Item: dungeonDropItemDefinition{ItemID: 8474},
		Amount: 2, Status: runtimeDungeonDropAvailable,
		DiscardOrigin: &runtimeDungeonDiscardOrigin{
			ListType:   dnfrepo.MainInventoryListType,
			SourceSlot: 65,
			Stack:      dnfrepo.ItemStack{ItemID: 8474, Count: 2},
		},
	}
	_, err := currentDungeonCommittedDropReceipts(runtime, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	})
	if err == nil {
		t.Fatal("unexplained missing pickup was accepted")
	}
}

func TestCurrentDungeonCommittedDropReceiptsExcludesAccountEpicBookPickup(t *testing.T) {
	runtime := &runtimeDungeonState{DropOwner: newRuntimeDungeonDropOwner()}
	runtime.DropOwner.byObjectKey[1] = &runtimeDungeonDrop{
		ObjectKey: 1,
		Item: dungeonDropItemDefinition{
			ItemID: 10150001,
		},
		Status:             runtimeDungeonDropConsumed,
		DestinationSlot:    121,
		PickupEpicBookBody: []byte{1},
	}

	receipts, err := currentDungeonCommittedDropReceipts(runtime, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("account Epic Book pickup projected as inventory receipt: %+v", receipts)
	}
}

func TestCurrentDungeonClearRewardCardSlotsMirrorFrozen86JPCardPlan(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	state, err := newDungeonCardState(mustTestDungeonCardPlan(t,
		dungeonCardRewardBundle{
			Gold: 80,
			Items: []dungeonCardItemReward{{
				ItemID: 3227, Count: 2, Stackable: true, SlotStart: 121, SlotEnd: 176, RawEntry: raw,
			}},
		},
		dungeonCardRewardBundle{Gold: 30},
	))
	if err != nil {
		t.Fatal(err)
	}
	slots := currentDungeonClearRewardCardSlotsFromRuntime(&runtimeDungeonState{
		settlementCardRewardState: state,
	})
	if len(slots[0]) != 2 ||
		slots[0][0] != (currentDungeonClearRewardPair{Key: 0, Value: 80}) ||
		slots[0][1] != (currentDungeonClearRewardPair{Key: 3227, Value: 2}) {
		t.Fatalf("free card op35 pairs=%+v", slots[0])
	}
	for index, slot := range slots {
		if index == 0 {
			continue
		}
		if len(slot) != 0 {
			t.Fatalf("unexpected card slot %d pairs=%+v", index, slot)
		}
	}
}

func TestCurrentDungeonPaidCardSlotsAggregateGoldCardEquipmentInstances(t *testing.T) {
	paid := dungeonCardRewardBundle{
		Gold: 19,
		Items: []dungeonCardItemReward{{
			ItemID: 31865, Count: 1, SlotStart: 9, SlotEnd: 64,
		}},
	}
	if err := applyCurrentDungeonGoldCardItemMultiplier(&paid); err != nil {
		t.Fatal(err)
	}
	state, err := newDungeonCardState(mustTestDungeonCardPlan(
		t,
		dungeonCardRewardBundle{},
		paid,
	))
	if err != nil {
		t.Fatal(err)
	}
	slots, _ := currentDungeonSelectedCardSlots(&runtimeDungeonState{
		settlementCardRewardState:        state,
		settlementCardSideSelectionKnown: [dungeonCardSideCount]bool{false, true},
	})
	if got := slots[0].Rewards; len(got) != 2 ||
		got[0] != (currentDungeonOp71RewardTuple{ValueA: 0, ValueB: 19}) ||
		got[1] != (currentDungeonOp71RewardTuple{ValueA: 31865, ValueB: 2}) {
		t.Fatalf("gold-card op71 equipment pairs=%+v, want item x2", got)
	}
	if got := slots[1].Rewards; len(got) != 0 {
		t.Fatalf("gold-card op71 leaked into next member seat: %+v", got)
	}
}

func TestCurrentDungeonSettlementMonsterStatisticsPopulateClassifiedScoreFields(t *testing.T) {
	runtime := &runtimeDungeonState{}
	runtime.accumulateCurrentDungeonSettlementMonsterExperience(0, 7)
	runtime.accumulateCurrentDungeonSettlementMonsterExperience(1, 11)
	runtime.accumulateCurrentDungeonSettlementMonsterExperience(2, 13)
	runtime.accumulateCurrentDungeonSettlementMonsterExperience(3, 17)
	if runtime.settlementMonsterExperienceTotal != 48 {
		t.Fatalf("monster experience total=%d", runtime.settlementMonsterExperienceTotal)
	}
	if score := currentDungeonSettlementScore(runtime); score != ([4]uint32{0, 11, 13, 17}) {
		t.Fatalf("settlement score fields=%v, want classified subtotals", score)
	}
	runtime.settlementBossExperience = ^uint32(0) - 1
	runtime.accumulateCurrentDungeonSettlementMonsterExperience(3, 10)
	if runtime.settlementBossExperience != ^uint32(0) {
		t.Fatalf("boss experience did not saturate: %d", runtime.settlementBossExperience)
	}
}

func TestCurrentDungeonClearExperienceUsesExactOptionalWeightSemantics(t *testing.T) {
	resources := currentDungeonSettlementTestResources(t)
	for _, test := range []struct {
		name      string
		set       bool
		weight    float64
		tutorial  bool
		want      uint32
		wantError bool
	}{
		{name: "absent is neutral", want: 200},
		{name: "negative is neutral", set: true, weight: -1, want: 200},
		{name: "explicit zero is zero reward and rejected", set: true, weight: 0, wantError: true},
		{name: "explicit positive is exact", set: true, weight: 1.25, want: 250},
		// Live 2026-09-23: the male Priest tutorial 7113 declares an explicit zero
		// weight, and the ordinary rejection deadlocked its whole settlement. A
		// tutorial must still settle -- with zero clear experience -- because the
		// result panel is the client's own exit condition.
		{name: "tutorial explicit zero settles with zero clear experience", set: true, weight: 0, tutorial: true, want: 0},
		{name: "tutorial explicit positive stays exact", set: true, weight: 1.25, tutorial: true, want: 250},
		{name: "tutorial absent weight stays neutral", tutorial: true, want: 200},
		{name: "tutorial negative weight stays neutral", set: true, weight: -1, tutorial: true, want: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &runtimeDungeonState{}
			runtime.Dungeon.Metadata.BasisLevel.Set = true
			runtime.Dungeon.Metadata.BasisLevel.Value = 1
			runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Set = test.set
			runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Value = test.weight
			runtime.Dungeon.Metadata.TutorialDungeon = worldmap.OptionalInt{Value: 1, Set: test.tutorial}
			runtime.Request.Difficulty = 1
			got, err := currentDungeonClearExperience(runtime, resources)
			if test.wantError {
				if err == nil {
					t.Fatalf("weight=%v produced gain=%d", test.weight, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("weight set=%t value=%v tutorial=%t gain=%d want=%d err=%v", test.set, test.weight, test.tutorial, got, test.want, err)
			}
		})
	}
}

// TestCurrentDungeonSettlementZeroAllowancesStayScoped pins the two ownership
// proofs the tutorial relaxation added. Both must be false for every dungeon
// that is not a PVF tutorial (and not the pre-existing raid-gate/Lucky-Star
// owner), otherwise an accidental zero could enter the settlement ledger or an
// empty reward body could be fabricated for an ordinary clear.
func TestCurrentDungeonSettlementZeroAllowancesStayScoped(t *testing.T) {
	tutorial := &runtimeDungeonState{}
	tutorial.Dungeon.Metadata.TutorialDungeon = worldmap.OptionalInt{Value: 1, Set: true}
	tutorialFlagTwo := &runtimeDungeonState{}
	tutorialFlagTwo.Dungeon.Metadata.TutorialDungeon = worldmap.OptionalInt{Value: 2, Set: true}
	tutorialFlagUnset := &runtimeDungeonState{}
	tutorialFlagUnset.Dungeon.Metadata.TutorialDungeon = worldmap.OptionalInt{Value: 1, Set: false}
	raidGate := &runtimeDungeonState{}
	raidGate.raidDungeon.RaidKey = 7
	zeroWeightOrdinary := &runtimeDungeonState{}
	zeroWeightOrdinary.Dungeon.ID = 7142
	zeroWeightOrdinary.Dungeon.Metadata.TutorialDungeon = worldmap.OptionalInt{}
	zeroWeightOrdinary.Dungeon.Metadata.ExperienceIncreasingPoint.Set = true

	service := &Service{}
	for _, test := range []struct {
		name    string
		runtime *runtimeDungeonState
		want    bool
	}{
		{name: "pvf tutorial", runtime: tutorial, want: true},
		{name: "raid gate keeps its own owner", runtime: raidGate, want: true},
		{name: "ordinary dungeon", runtime: zeroWeightOrdinary, want: false},
		{name: "tutorial flag other than one", runtime: tutorialFlagTwo, want: false},
		{name: "tutorial flag unset", runtime: tutorialFlagUnset, want: false},
		{name: "nil runtime", runtime: nil, want: false},
	} {
		t.Run("zero experience/"+test.name, func(t *testing.T) {
			if got := service.currentDungeonSettlementAllowsZeroExperience(test.runtime); got != test.want {
				t.Fatalf("zero clear experience allowed=%t want %t", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		name               string
		runtime            *runtimeDungeonState
		luckyStarAttempted bool
		want               bool
	}{
		{name: "pvf tutorial", runtime: tutorial, want: true},
		{name: "lucky star on an ordinary dungeon", runtime: zeroWeightOrdinary, luckyStarAttempted: true, want: true},
		{name: "ordinary dungeon", runtime: zeroWeightOrdinary, want: false},
		{name: "tutorial flag other than one", runtime: tutorialFlagTwo, want: false},
		{name: "nil runtime", runtime: nil, want: false},
	} {
		t.Run("zero reward/"+test.name, func(t *testing.T) {
			if got := currentDungeonSettlementAllowsZeroReward(test.runtime, test.luckyStarAttempted); got != test.want {
				t.Fatalf("zero clear reward allowed=%t want %t", got, test.want)
			}
		})
	}
}

func TestRealScriptPVFDungeon3SettlementExperienceSourcesAreComplete(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify real dungeon settlement experience")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(3)
	if !found {
		t.Fatal("real dungeon 3 is absent")
	}
	tables, err := progression.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	experience, err := progression.LoadMonsterExperienceSources(archive)
	if err != nil {
		t.Fatal(err)
	}
	clearRanks, err := loadCurrentDungeonClearRankCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	gain, err := currentDungeonClearExperience(&runtimeDungeonState{
		Dungeon: dungeon,
		Request: dungeoncmd.SelectDungeonRequest{DungeonID: 3, Difficulty: 0},
	}, currentDungeonSettlementRewardResources{
		Progression: tables, ExperienceSources: experience, ClearRankCatalog: clearRanks,
	})
	if err != nil {
		dungeonRate, dungeonRateFound := experience.DungeonIncreasingRate(3)
		t.Fatalf("%v: dungeon metadata=%+v difficulty_rates=%+v dungeon_rate=%v found=%t", err, dungeon.Metadata, experience.RawModifiers().DifficultyRates, dungeonRate, dungeonRateFound)
	}
	if gain == 0 {
		t.Fatal("real dungeon 3 produced zero clear experience")
	}
}

// TestRealScriptPVFPriestTutorialZeroWeightSettles is the regression for the
// live 2026-09-23 deadlock (character 123456, male Priest). Dungeon 7113
// declares `[experience increasing point] 0`; the ordinary clear-experience
// rule rejected it, the settlement plan was never produced, and the client's
// own exit handshake (op42 after the missing result panel) was refused forever
// with completed_run_missing_play_result_or_statistic_barrier.
//
// The same test pins the scope: dungeon 7142 (转职体验「领悟时间」) declares the
// same explicit zero and is NOT a tutorial, so it must keep the strict
// rejection. That keeps this fix from silently rewriting every zero-weight
// dungeon's experience and drop-rate semantics.
func TestRealScriptPVFPriestTutorialZeroWeightSettles(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify the male Priest tutorial settlement")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tables, err := progression.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	experience, err := progression.LoadMonsterExperienceSources(archive)
	if err != nil {
		t.Fatal(err)
	}
	clearRanks, err := loadCurrentDungeonClearRankCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	resources := currentDungeonSettlementRewardResources{
		Progression: tables, ExperienceSources: experience, ClearRankCatalog: clearRanks,
	}

	tutorial, found := table.FindDungeon(7113)
	if !found {
		t.Fatal("real dungeon 7113 (male Priest tutorial) is absent")
	}
	if !tutorial.Metadata.ExperienceIncreasingPoint.Set || tutorial.Metadata.ExperienceIncreasingPoint.Value != 0 {
		t.Fatalf("7113 no longer declares an explicit zero experience weight: %+v", tutorial.Metadata.ExperienceIncreasingPoint)
	}
	tutorialRuntime := &runtimeDungeonState{
		Dungeon: tutorial,
		Request: dungeoncmd.SelectDungeonRequest{DungeonID: 7113, Difficulty: 0},
	}
	if !isPVFTutorialDungeon(tutorialRuntime) {
		t.Fatalf("7113 is not a PVF tutorial dungeon: %+v", tutorial.Metadata.TutorialDungeon)
	}
	gain, err := currentDungeonClearExperience(tutorialRuntime, resources)
	if err != nil {
		t.Fatalf("tutorial 7113 clear experience was rejected again: %v", err)
	}
	if gain != 0 {
		t.Fatalf("tutorial 7113 clear experience=%d want 0", gain)
	}
	if !(&Service{}).currentDungeonSettlementAllowsZeroExperience(tutorialRuntime) {
		t.Fatal("tutorial 7113 is not allowed to settle with zero clear experience")
	}
	if !currentDungeonSettlementAllowsZeroReward(tutorialRuntime, false) {
		t.Fatal("tutorial 7113 is not allowed to settle with an empty reward snapshot")
	}

	jobChangeTrial, found := table.FindDungeon(7142)
	if !found {
		t.Fatal("real dungeon 7142 (job-change trial) is absent")
	}
	if !jobChangeTrial.Metadata.ExperienceIncreasingPoint.Set || jobChangeTrial.Metadata.ExperienceIncreasingPoint.Value != 0 {
		t.Fatalf("7142 no longer declares an explicit zero experience weight: %+v", jobChangeTrial.Metadata.ExperienceIncreasingPoint)
	}
	jobChangeRuntime := &runtimeDungeonState{
		Dungeon: jobChangeTrial,
		Request: dungeoncmd.SelectDungeonRequest{DungeonID: 7142, Difficulty: 0},
	}
	if isPVFTutorialDungeon(jobChangeRuntime) {
		t.Fatal("7142 is not a tutorial and must not inherit the tutorial relaxation")
	}
	if _, err := currentDungeonClearExperience(jobChangeRuntime, resources); err == nil {
		t.Fatal("7142 (non-tutorial, explicit zero weight) lost its strict clear-experience rejection")
	}
	if (&Service{}).currentDungeonSettlementAllowsZeroExperience(jobChangeRuntime) {
		t.Fatal("7142 (non-tutorial) gained a zero-experience allowance")
	}
	if currentDungeonSettlementAllowsZeroReward(jobChangeRuntime, false) {
		t.Fatal("7142 (non-tutorial) gained an empty-reward allowance")
	}
}

func currentDungeonSettlementTestResources(t *testing.T) currentDungeonSettlementRewardResources {
	t.Helper()
	source := bridgePVFSource{
		currentDungeonRankSystemPVFPath: "[rank grade]\n99 90 80 60 50 30 20 10\n[/rank grade]\n",
		progression.ExperienceTablePath: "100 250 500 900\n",
		progression.SkillPointTablePath: "[sp table]\n1 10\n2 30\n3 30\n4 40\n5 50\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n",
		progression.QuestParameterPath: "[difficulty]\n`N` 100\n[/difficulty]\n" +
			"[exp reward table]\n100 -1\n200 -1\n300 -1\n400 -1\n500 -1\n" +
			"[gold reward table]\n0 10\n" +
			"[green level penalty]\n80\n[grey level penalty]\n30\n",
		progression.MonsterExperienceTablePath: "1 30 2 60 3 72\n",
		progression.ServerParameterPath: "[monster exp bonusrate]\n1.00\n" +
			"[party user number exp bonusrate]\n1 2 3 4\n" +
			"[party user number exp bonusrate starter server]\n1 2 3 4\n" +
			"[dungeon difficulty exp bonusrate]\n1.30 2.00 2.50 3.00 4.00\n" +
			"[clear exp bonusrate]\n1.00\n" +
			"[clear rank exp bonusrate]\n0.05 0.10 0.12 0.15 0.20\n" +
			"[experience increasing point]\n700 1.00\n[/experience increasing point]\n",
		progression.WorldMapExperiencePenaltyPath: "[penalty table info]\n" +
			"-16 0.30 -15 0.50 -14 0.50 -13 0.50 -12 0.70 -11 0.70 -10 0.70 -9 0.80 -8 0.80 -7 0.80 -6 0.90 -5 0.90 -4 0.90 -3 1.00 -2 1.00 -1 1.00 0 1.00 1 1.00 2 1.00 3 1.00 4 1.00 5 1.00 6 0.90 7 0.90 8 0.90 9 0.80 10 0.80 11 0.80 12 0.70 13 0.70 14 0.70 15 0.50 16 0.50 17 0.50 18 0.30 19 0.30 20 0.30\n" +
			"[/penalty table info]\n",
	}
	tables, err := progression.Load(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	experience, err := progression.LoadMonsterExperienceSources(source)
	if err != nil {
		t.Fatal(err)
	}
	clearRanks, err := loadCurrentDungeonClearRankCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return currentDungeonSettlementRewardResources{
		Progression: tables, ExperienceSources: experience, ClearRankCatalog: clearRanks,
	}
}

func TestCurrentDungeonSettlementStoryQuestExperienceFillsResultTailWithoutGrantingIt(t *testing.T) {
	service, runtime, _, _, _ := prepareCompletedSettlementRuntime(t)
	questSource := questListTestSource{
		dnfquest.DefaultList: "3145 `story.qst`\n",
		"n_quest/story.qst":  "[grade]\n`[epic]`\n[level]\n1 99\n[job]\n`[gunner]`\n[difficulty]\n`N`\n",
	}
	questIndex, err := dnfpvf.Build(context.Background(), questSource, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	service.questCatalog, err = dnfquest.Load(context.Background(), questIndex)
	if err != nil {
		t.Fatal(err)
	}
	runtime.MazeIndex = 0
	runtime.Dungeon.Mazes = []worldmap.Maze{{QuestConnection: []int64{0, 3145, -1}}}
	runtime.Character.Level = 1
	runtime.Character.Stats = map[string]int64{"exp": 90}
	resources := currentDungeonSettlementTestResources(t)
	base, total, questID, err := service.currentDungeonSettlementStoryQuestExperience(
		context.Background(), runtime, resources.Progression, 200,
	)
	if err != nil {
		t.Fatal(err)
	}
	if questID != 3145 || base != 100 || total != 100 {
		t.Fatalf("story result quest=%d base=%d total=%d, want quest=3145 base=100 total=100", questID, base, total)
	}
	// A recommended clear may legitimately award Lucky Stars while its dungeon
	// EXP is zero. Story presentation still resolves from the unchanged level.
	runtime.settlementStoryQuestBaseResolved = false
	runtime.settlementStoryQuestBase = 0
	runtime.settlementStoryQuestTotal = 0
	base, total, questID, err = service.currentDungeonSettlementStoryQuestExperience(
		context.Background(), runtime, resources.Progression, 0,
	)
	if err != nil || questID != 3145 || base != 100 || total != 100 {
		t.Fatalf("zero-gain story result quest=%d base=%d total=%d err=%v", questID, base, total, err)
	}
	// The values are frozen before commit; changing the in-memory level on a
	// retry must not change the already-built result-window presentation.
	runtime.Character.Level = 30
	base, total, questID, err = service.currentDungeonSettlementStoryQuestExperience(
		context.Background(), runtime, resources.Progression, 200,
	)
	if err != nil || questID != 3145 || base != 100 || total != 100 {
		t.Fatalf("story result was not frozen quest=%d base=%d total=%d err=%v", questID, base, total, err)
	}
}

func TestCurrentDungeonScoreBonusUsesOnlyFiveRewardGrades(t *testing.T) {
	resources := currentDungeonSettlementTestResources(t)
	tests := []struct {
		name  string
		point byte
		want  uint32
	}{
		{name: "highest", point: 99, want: 200},
		{name: "grade ninety", point: 90, want: 150},
		{name: "grade eighty", point: 80, want: 120},
		{name: "grade sixty", point: 73, want: 100},
		{name: "grade fifty", point: 50, want: 50},
		{name: "below reward grades", point: 30, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := currentDungeonScoreBonusExp(1000, resources, test.point); got != test.want {
				t.Fatalf("point=%d score bonus=%d want=%d", test.point, got, test.want)
			}
		})
	}
}

func TestCurrentDungeonSettlementGrowthContractAddsTwentyPercentClearExp(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	service.premiumCatalog = &currentPremiumCatalog{
		effectsByType: map[int64]currentPremiumEffectInfo{
			premium.TypeBonusExp: {BonusExperiencePercent: 20},
		},
	}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 1
	character.Stats["exp"] = 90
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{
		CharacterID: "99",
		Points: dnfrepo.SkillPointState{
			TotalSP: 10, RemainingSP: 10, SyncedLevel: 1,
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
	future := time.Now().Add(24 * time.Hour).Unix()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"premium_expire_84": strconv.FormatInt(future, 10)},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = character
	runtime.Dungeon.Metadata.BasisLevel.Set = true
	runtime.Dungeon.Metadata.BasisLevel.Value = 1
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Set = true
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Value = 1
	runtime.Request.Difficulty = 1
	runtime.DropOwner = newRuntimeDungeonDropOwner()
	runtime.settlementPlayResultReceived = true
	runtime.settlementPlayResultBody = make([]byte, currentDungeonPlayResultBaseSize)
	runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset] = 73
	runtime.settlementMonsterExperienceTotal = 900
	runtime.settlementMonsterGrowthContractBonus = 180
	runtime.settlementMonsterFatigueBatteryBonus = 270
	runtime.settlementMonsterEquipmentBonus = 45
	runtime.settlementChampionExperience = 200
	runtime.settlementSuperChampionExperience = 100
	runtime.settlementBossExperience = 300
	runtime.settlementMonsterExperienceByObjectKey = map[uint32]uint32{
		0x195: 1054,
		0x192: 1129,
	}
	runtime.startedAt = time.Unix(1700000000, 0)
	runtime.clearMapCompletionAt = runtime.startedAt.Add(12*time.Second + 345*time.Millisecond)

	resources := currentDungeonSettlementTestResources(t)
	if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(
		context.Background(), session, runtime, resources,
	); err != nil {
		t.Fatal(err)
	}
	if runtime.settlementResultPlan == nil {
		t.Fatal("settlement plan was not frozen")
	}
	// Clear gain 200 + score bonus 20 + growth contract 20% = 240 committed.
	// op35 Base[0] = base clear exp only (200).
	reader := settlementProtocolReader{body: runtime.settlementResultPlan.ClearRewardBody}
	if got := reader.u32(); got != 200 {
		t.Fatalf("op35 clear base experience=%d, want 200 (base only)", got)
	}
	if got := reader.u32(); got != 20 {
		t.Fatalf("op35 score bonus experience=%d, want 20", got)
	}
	body := runtime.settlementResultPlan.ClearRewardBody
	bonusStart := 4*4 + 1
	if got := binary.LittleEndian.Uint32(body[bonusStart+currentDungeonClearRewardBonusClearGrowthContractIndex*4:]); got != 40 {
		t.Fatalf("clear growth-contract Bonus[%d]=%d, want 40", currentDungeonClearRewardBonusClearGrowthContractIndex, got)
	}
	if got := binary.LittleEndian.Uint32(body[bonusStart+currentDungeonClearRewardBonusClearFatigueBurnIndex*4:]); got != 0 {
		t.Fatalf("clear fatigue-burn Bonus[%d]=%d, want 0", currentDungeonClearRewardBonusClearFatigueBurnIndex, got)
	}
	if got := binary.LittleEndian.Uint32(body[bonusStart+currentDungeonClearRewardBonusMonsterGrowthContractIndex*4:]); got != 180 {
		t.Fatalf("monster growth-contract Bonus[%d]=%d, want 180", currentDungeonClearRewardBonusMonsterGrowthContractIndex, got)
	}
	if got := binary.LittleEndian.Uint32(body[bonusStart+currentDungeonClearRewardBonusMonsterEquipmentIndex*4:]); got != 45 {
		t.Fatalf("monster equipment Bonus[%d]=%d, want 45", currentDungeonClearRewardBonusMonsterEquipmentIndex, got)
	}
	// Current op35 consumes the fatigue-buff row at +81, immediately before
	// growth-contract EXP. It is already part of MonsterTotalExp.
	if got := binary.LittleEndian.Uint32(body[81:85]); got != 270 {
		t.Fatalf("monster fatigue-buff bonus=%d, want 270", got)
	}
	if got := binary.LittleEndian.Uint32(body[77:81]); got != 0 {
		t.Fatalf("monster fatigue-burn bonus=%d, want 0", got)
	}
	scoreStart := bonusStart + currentDungeonClearRewardBonusFieldCount*4 + 2 + currentDungeonClearRewardPostBaseFieldCount*4
	wantScore := [currentDungeonClearRewardScoreFieldCount]uint32{0, 200, 100, 300}
	for index, want := range wantScore {
		if got := binary.LittleEndian.Uint32(body[scoreStart+index*4:]); got != want {
			t.Fatalf("score[%d]=%d, want %d", index, got, want)
		}
	}
	if got := binary.LittleEndian.Uint32(body[len(body)-16:]); got != 1395 {
		t.Fatalf("monster total experience=%d, want 1395 (base 900 + fatigue 270 + growth 180 + equipment 45)", got)
	}
	if got := binary.LittleEndian.Uint32(body[151:155]); got != 2 {
		t.Fatalf("monster dynamic row count=%d, want 2", got)
	}
	if key, value := binary.LittleEndian.Uint32(body[155:159]), binary.LittleEndian.Uint32(body[159:163]); key != 0x192 || value != 1129 {
		t.Fatalf("first monster dynamic row key=%#x value=%d", key, value)
	}
	committed, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load committed character found=%t err=%v", found, err)
	}
	if committed.Level != 3 || committed.Stats["exp"] != 350 {
		t.Fatalf("committed progression level=%d exp=%d, want level=3 exp=350", committed.Level, committed.Stats["exp"])
	}

	// The durable receipt holds the total: a producer retry must not grant
	// the growth bonus a second time.
	runtime.settlementResultPlan = nil
	if err := service.produceCurrentDungeonSettlementPlanWithResourcesLocked(
		context.Background(), session, runtime, resources,
	); err != nil {
		t.Fatal(err)
	}
	replayed, _, _ := repositories.Character.Load(context.Background(), "99")
	if replayed.Level != 3 || replayed.Stats["exp"] != 350 {
		t.Fatalf("producer retry duplicated progression: %+v", replayed)
	}
	replayBody := runtime.settlementResultPlan.ClearRewardBody
	if binary.LittleEndian.Uint32(replayBody[81:85]) != 270 || binary.LittleEndian.Uint32(replayBody[len(replayBody)-16:]) != 1395 {
		t.Fatalf("producer retry changed fatigue-buff presentation: %x", replayBody)
	}
}

func TestCurrentDungeonAdventureGroupAndConsumableClearBonuses(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	service.adventureGroupTable = loadAdventureGroupTestTables(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 1
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	runtime.Character = character

	bonus, level, percent := service.currentDungeonAdventureGroupClearBonusExp(
		context.Background(), session, runtime, 200,
	)
	if bonus != 20 || level != 1 || percent != 10 {
		t.Fatalf("adventure bonus=%d level=%d percent=%d", bonus, level, percent)
	}
	group := currentDungeonAdventureGroupBonusGroup(bonus)
	if len(group) != 0 {
		t.Fatalf("unverified adventure result group=%+v", group)
	}

	account := dnfrepo.AccountRecord{AccountID: "account-1"}
	if _, err := experiencebonus.ActivateForCharacter(
		&account,
		"99",
		10005931,
		200,
		30*time.Minute,
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	elixirBonus, effect, active := service.currentDungeonExperienceConsumableBonusExp(
		context.Background(), "account-1", "99", 200,
	)
	if !active || elixirBonus != 400 || effect.ItemID != 10005931 || effect.BonusPercent != 200 {
		t.Fatalf("elixir bonus=%d effect=%+v active=%t", elixirBonus, effect, active)
	}
}

func TestCurrentDungeonSettlementMonsterTotalExperienceMatchesLiveResultWindow(t *testing.T) {
	runtime := &runtimeDungeonState{
		settlementMonsterExperienceTotal:     320703,
		settlementMonsterGrowthContractBonus: 71658,
		settlementMonsterEquipmentBonus:      250828,
	}
	if got := currentDungeonSettlementMonsterTotalExperience(runtime); got != 643189 {
		t.Fatalf("monster total experience=%d, want 643189", got)
	}
}

func TestCurrentBlackDiamondBonusUsesOnlyDedicatedEntitlement(t *testing.T) {
	service, _, _, _, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	future := strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
	for _, metadataKey := range []string{
		"premium_expire_1",
		"premium_expire_17",
		"premium_expire_29",
	} {
		if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
			AccountID: "account-1",
			Metadata:  map[string]string{metadataKey: future},
		}); err != nil {
			t.Fatal(err)
		}
		if got := service.currentBlackDiamondBonusExp(context.Background(), "account-1", 200); got != 0 {
			t.Fatalf("%s black diamond bonus = %d, want 0", metadataKey, got)
		}
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			premium.MetadataKey(premium.TypeBlackDiamondEntitlement): future,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := service.currentBlackDiamondBonusExp(context.Background(), "account-1", 200); got != 20 {
		t.Fatalf("dedicated black diamond bonus = %d, want 20", got)
	}
}
