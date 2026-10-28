package quest

import (
	"context"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerPlanReadsQuestState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "46",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "46",
		States: map[int64]dnfrepo.QuestState{
			0x1234: {Status: "active", ProgressValue: 2},
		},
	}); err != nil {
		t.Fatalf("save quest: %v", err)
	}

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	got, err := owner.Plan(ctx, NewFinishCommand(alignedRequest("acc-quest", 46), FinishQuestRequest{
		QuestID:           0x1234,
		RewardSelectIndex: 1,
		HasRewardSelect:   true,
		Multiplier:        2,
	}))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got.AccountID != "acc-quest" || got.CharacterID != "46" || !got.Known || got.Status != "active" || got.ProgressValue != 2 || got.RewardSelectIndex != 1 || got.Multiplier != 2 {
		t.Fatalf("result = %+v", got)
	}
}

func TestOwnerApplySetTriggerType1RecomputesSeekingItems(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "48",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "48",
		States: map[int64]dnfrepo.QuestState{
			0x2222: {
				Status:        "active",
				ProgressValue: 1,
				Extra: map[string]string{
					"seeking_item_ids": "9001",
				},
			},
		},
	}); err != nil {
		t.Fatalf("save quest: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "48",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9": {ItemID: 9001, Count: 1},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	got, err := owner.ApplySetTrigger(ctx, NewTriggerCommand(alignedRequest("acc-quest", 48), SetTriggerRequest{
		QuestID:     0x2222,
		TriggerType: 1,
		IsIncrement: true,
	}))
	if err != nil {
		t.Fatalf("ApplySetTrigger() error = %v", err)
	}
	if got.ProgressValue != 0 {
		t.Fatalf("progress = %d, want 0 after held seeking item", got.ProgressValue)
	}
	if !got.StateChanged {
		t.Fatalf("first trigger result = %+v, want durable state change", got)
	}
	loaded, ok, err := repos.Quest.Load(ctx, "48")
	if err != nil || !ok {
		t.Fatalf("load quest ok=%v err=%v", ok, err)
	}
	if got := loaded.States[0x2222].ProgressValue; got != 0 {
		t.Fatalf("stored progress = %d, want 0", got)
	}
	replayed, err := owner.ApplySetTrigger(ctx, NewTriggerCommand(alignedRequest("acc-quest", 48), SetTriggerRequest{
		QuestID:     0x2222,
		TriggerType: 1,
		IsIncrement: true,
	}))
	if err != nil {
		t.Fatalf("ApplySetTrigger replay error = %v", err)
	}
	if replayed.StateChanged {
		t.Fatalf("replay result = %+v, want no durable state change", replayed)
	}
}

func TestOwnerApplySetTriggerSeekAndMeetNPCRecomputesAccountSharedItems(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "50",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "50",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: int64(packTriggerChannels(304, 1))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc-quest",
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 499},
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := testAcceptCatalog(t, "[type]\n`[seek n meet npc]`\n[int data]\n3037 500 126\n")
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewTriggerCommand(alignedRequest("acc-quest", 50), SetTriggerRequest{
		QuestID: 3145, TriggerType: 0x10,
	})
	insufficient, err := owner.ApplySetTriggerWithCatalog(ctx, catalog, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if insufficient.StateChanged || insufficient.ProgressValue != int64(packTriggerChannels(304, 1)) {
		t.Fatalf("insufficient result=%+v", insufficient)
	}

	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc-quest",
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 120019},
		},
	}); err != nil {
		t.Fatal(err)
	}
	satisfied, err := owner.ApplySetTriggerWithCatalog(ctx, catalog, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !satisfied.StateChanged || satisfied.ProgressValue != int64(packTriggerChannels(0, 1)) {
		t.Fatalf("satisfied result=%+v", satisfied)
	}
	stored, ok, err := repos.Quest.Load(ctx, "50")
	if err != nil || !ok || stored.States[3145].ProgressValue != int64(packTriggerChannels(0, 1)) {
		t.Fatalf("stored quest ok=%t err=%v state=%+v", ok, err, stored.States[3145])
	}
}

func TestOwnerApplySetTriggerUpdatesOnlyRequestedPackedChannel(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "49",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "49",
		States: map[int64]dnfrepo.QuestState{
			3193: {Status: "active", ProgressValue: int64(packTriggerChannels(1, 1))},
			3203: {Status: "active", ProgressValue: int64(packTriggerChannels(4, 6, 7))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}

	apply := func(questID uint16, triggerType byte, increment bool, want int64) {
		t.Helper()
		result, err := owner.ApplySetTrigger(ctx, NewTriggerCommand(alignedRequest("acc-quest", 49), SetTriggerRequest{
			QuestID: questID, TriggerType: triggerType, IsIncrement: increment,
		}))
		if err != nil {
			t.Fatalf("ApplySetTrigger quest=%d type=%#x: %v", questID, triggerType, err)
		}
		if !result.StateChanged || result.ProgressValue != want {
			t.Fatalf("ApplySetTrigger quest=%d type=%#x result=%+v want progress=%d", questID, triggerType, result, want)
		}
	}

	apply(3193, 0x10, false, int64(packTriggerChannels(0, 1)))
	apply(3193, 0x20, false, 0)
	apply(3203, 0x40, false, int64(packTriggerChannels(4, 6, 6)))
	apply(3203, 0x20, true, int64(packTriggerChannels(4, 7, 6)))
}

// Live incident 2026-09-12T13:32:33Z: quest 2900 held the PVF initial trigger
// 262657 = packTriggerChannels(1, 1, 1) and the client reported its objective
// with the scalar (no lane mask) op33 shape. Subtracting one from the packed
// u32 changed the durable value on every identical request, so the client kept
// re-triggering it: 3767 requests at ~30 Hz across seven logins while every
// town entry stayed frozen. The scalar shape must answer the satisfied
// objective once and then stay idempotent.
func TestOwnerApplySetTriggerScalarRemoveClearsPackedObjectiveWithoutCounterLoop(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "52",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "52",
		States: map[int64]dnfrepo.QuestState{
			2900: {Status: "active", ProgressValue: int64(packTriggerChannels(1, 1, 1))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	apply := func() PlanResult {
		t.Helper()
		result, applyErr := owner.ApplySetTrigger(ctx, NewTriggerCommand(alignedRequest("acc-quest", 52), SetTriggerRequest{QuestID: 2900}))
		if applyErr != nil {
			t.Fatalf("ApplySetTrigger quest=2900: %v", applyErr)
		}
		return result
	}
	if first := apply(); !first.StateChanged || first.ProgressValue != 0 {
		t.Fatalf("scalar remove result=%+v want state changed to 0", first)
	}
	if replay := apply(); replay.StateChanged || replay.ProgressValue != 0 {
		t.Fatalf("replayed scalar remove result=%+v want unchanged zero", replay)
	}
	stored, found, err := repos.Quest.Load(ctx, "52")
	if err != nil || !found || stored.States[2900].ProgressValue != 0 {
		t.Fatalf("persisted scalar-remove state found=%t err=%v state=%+v", found, err, stored.States[2900])
	}
}

// 婚约任务 20737 的真实 PVF 形状：[type] `[condition under clear]` + [sub type] 22 +
// [int data] `-1 -1 5`，即"跟心上人组 2 人队通关普通地下城 5 次"。
//
// 这种计数任务的进度由**服务端**在通关结算里权威地减 1（clear_map.go 的
// decrementClearTriggerChannel），而客户端每通关一次还会发一条标量 op33
// ("目标已满足")。标量通知必须原样回权威值、不再动进度，否则一次通关会被减两次
// 甚至直接清零 —— 实测就是这样"通关 1 次就完成"，客户端还显示 5/5。
//
// 对照：任务 2900 是 [hunt enemy]，仍然保持"标量即满足 → 清零"的旧语义
// （见 TestOwnerApplySetTriggerScalarRemoveClearsPackedObjectiveWithoutCounterLoop）。
func TestOwnerApplySetTriggerScalarKeepsConditionUnderClearCounter(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "61",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatal(err)
	}
	// 接取后的权威进度：5 次待通关。
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "61",
		States: map[int64]dnfrepo.QuestState{
			20737: {Status: "active", ProgressValue: int64(packTriggerChannels(5))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	source := catalogTestSource{
		DefaultList: "20737 `wedding.qst`\n",
		"n_quest/wedding.qst": questCatalogTestDefinition("[wedding]", 30, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n22\n[int data]\n-1 -1 5\n"),
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplySetTriggerWithCatalog(ctx, catalog,
		NewTriggerCommand(alignedRequest("acc-quest", 61), SetTriggerRequest{QuestID: 20737}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ProgressValue != 5 || result.StateChanged {
		t.Fatalf("标量 op33 动了 [condition under clear] 的计数器: %+v", result)
	}
	stored, found, err := repos.Quest.Load(ctx, "61")
	if err != nil || !found || stored.States[20737].ProgressValue != 5 {
		t.Fatalf("持久化的计数器被改写: found=%t err=%v state=%+v", found, err, stored.States[20737])
	}
}

func TestOwnerApplySetTriggerRaidPhaseClearLimitsEachPhaseToOncePerChinaDay(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "51",
		AccountID:   "acc-quest",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "51",
		States: map[int64]dnfrepo.QuestState{
			12830: {Status: "active", ProgressValue: int64(packTriggerChannels(5, 5))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := &Catalog{byID: map[int64]Definition{
		12830: {ID: 12830, Type: "[raid phase clear]", IntData: []int64{0, 0, 5, -1, 1, 0, 5, -1}},
	}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(triggerType byte) PlanResult {
		t.Helper()
		result, applyErr := owner.ApplySetTriggerWithCatalog(ctx, catalog, NewTriggerCommand(alignedRequest("acc-quest", 51), SetTriggerRequest{
			QuestID: 12830, TriggerType: triggerType,
		}))
		if applyErr != nil {
			t.Fatal(applyErr)
		}
		return result
	}
	if first := apply(0x10); !first.StateChanged || first.ProgressValue != int64(packTriggerChannels(4, 5)) {
		t.Fatalf("first phase-one clear=%+v", first)
	}
	if duplicate := apply(0x10); duplicate.StateChanged || duplicate.ProgressValue != int64(packTriggerChannels(4, 5)) {
		t.Fatalf("same-day phase-one replay=%+v", duplicate)
	}
	if secondPhase := apply(0x20); !secondPhase.StateChanged || secondPhase.ProgressValue != int64(packTriggerChannels(4, 4)) {
		t.Fatalf("same-day phase-two clear=%+v", secondPhase)
	}
	if unknown := apply(0x40); unknown.StateChanged || unknown.ProgressValue != int64(packTriggerChannels(4, 4)) {
		t.Fatalf("undeclared phase channel=%+v", unknown)
	}

	stored, ok, err := repos.Quest.Load(ctx, "51")
	if err != nil || !ok {
		t.Fatalf("load stored quest ok=%t err=%v", ok, err)
	}
	state := stored.States[12830]
	today := raidPhaseClearCalendarDay(time.Now())
	if state.Extra["raid_phase_clear_day_0"] != today || state.Extra["raid_phase_clear_day_1"] != today {
		t.Fatalf("daily markers=%+v want day=%s", state.Extra, today)
	}
	state.Extra["raid_phase_clear_day_0"] = "2000-01-01"
	stored.States[12830] = state
	if err := repos.Quest.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if nextDay := apply(0x10); !nextDay.StateChanged || nextDay.ProgressValue != int64(packTriggerChannels(3, 4)) {
		t.Fatalf("next-day phase-one clear=%+v", nextDay)
	}
}

func TestRaidPhaseClearCalendarDayRollsAtSixBeijing(t *testing.T) {
	before := time.Date(2026, 8, 19, 5, 59, 59, 0, raidPhaseClearCalendar)
	after := time.Date(2026, 8, 19, 6, 0, 0, 0, raidPhaseClearCalendar)
	if got := raidPhaseClearCalendarDay(before); got != "2026-08-18" {
		t.Fatalf("05:59 service day=%s want 2026-08-18", got)
	}
	if got := raidPhaseClearCalendarDay(after); got != "2026-08-19" {
		t.Fatalf("06:00 service day=%s want 2026-08-19", got)
	}
}

func TestPackedTriggerChannelArithmeticSaturatesAndPreservesReservedBits(t *testing.T) {
	const reserved = uint32(0xf8000000)
	start := reserved | packTriggerChannels(511, 6, 7)
	if got := incrementPackedTriggerChannel(start, 0); got != start {
		t.Fatalf("saturated packed trigger=%#08x want unchanged %#08x", got, start)
	}
	if got := decrementPackedTriggerChannel(reserved|packTriggerChannels(0, 6, 7), 0); got != reserved|packTriggerChannels(0, 6, 7) {
		t.Fatalf("zero packed trigger=%#08x want unchanged", got)
	}
	want := reserved | packTriggerChannels(1, 7, 7)
	if got := incrementPackedTriggerChannel(reserved|packTriggerChannels(1, 6, 7), 1); got != want {
		t.Fatalf("packed trigger increment=%#08x want=%#08x", got, want)
	}
}

func TestOwnerPlanRejectsMissingCharacter(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	_, err = owner.Plan(ctx, NewQuestIDCommand(alignedRequest("acc-quest", 47), "accept_quest", QuestIDRequest{QuestID: 1}))
	if !errors.Is(err, ErrCharacterNotFound) {
		t.Fatalf("Plan() error = %v, want ErrCharacterNotFound", err)
	}
}

func TestOwnerApplyAcceptPersistsThenReturnsIdempotentResult(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   "acc-quest",
		Job:         "2",
		Level:       1,
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	catalog := testAcceptCatalog(t, "[type]\n`[clear map]`\n[int data]\n76126\n")
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewQuestIDCommand(alignedRequest("acc-quest", 17), "accept_quest", QuestIDRequest{QuestID: 3145})
	first, err := owner.ApplyAccept(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, cmd)
	if err != nil {
		t.Fatalf("ApplyAccept first error = %v", err)
	}
	if first.QuestID != 3145 || first.InitTrigger != 1 || first.Idempotent {
		t.Fatalf("first result = %+v", first)
	}
	stored, ok, err := repos.Quest.Load(ctx, "17")
	if err != nil || !ok {
		t.Fatalf("load stored quest ok=%t err=%v", ok, err)
	}
	state := stored.States[3145]
	if state.Status != "active" || state.ProgressValue != 1 || state.Extra["pvf_path"] == "" || normalizeQuestTag(state.Extra["quest_type"]) != "clear map" {
		t.Fatalf("stored state = %+v", state)
	}
	second, err := owner.ApplyAccept(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, cmd)
	if err != nil {
		t.Fatalf("ApplyAccept retry error = %v", err)
	}
	if !second.Idempotent || second.InitTrigger != 1 {
		t.Fatalf("retry result = %+v", second)
	}
}

func TestOwnerSeekingAcceptPersistsPVFRequirementsAndReconcilesExistingState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: "acc-quest", Job: "2", Level: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "17", Slots: map[string]dnfrepo.ItemStack{
		"0:9":  {ItemID: 4821, Count: 1},
		"0:10": {ItemID: 4822, Count: 1},
		"0:11": {ItemID: 4823, Count: 1},
		"0:12": {ItemID: 4824, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	catalog := testAcceptCatalog(t, "[grade]\n`[side]`\n[type]\n`[seeking]`\n[int data]\n4821 1 4822 1 4823 1 4824 1\n")
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	command := NewQuestIDCommand(alignedRequest("acc-quest", 17), "accept_quest", QuestIDRequest{QuestID: 3145})
	if _, err := owner.ApplyAccept(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, command); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repos.Quest.Load(ctx, "17")
	if err != nil || !found {
		t.Fatalf("load accepted seeking quest found=%t err=%v", found, err)
	}
	state := stored.States[3145]
	if state.Extra["seeking_item_ids"] != "4821,4822,4823,4824" ||
		state.Extra["seeking_item_counts"] != "1,1,1,1" {
		t.Fatalf("accepted seeking requirements=%+v", state.Extra)
	}
	// Simulate a pre-fix accepted record which has no cached keys. The native
	// type-1 held-item probe must still resolve the PVF requirements and open
	// the exact terminal op34 path.
	delete(state.Extra, "seeking_item_ids")
	delete(state.Extra, "seeking_item_counts")
	stored.States[3145] = state
	if err := repos.Quest.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplySetTriggerWithCatalog(ctx, catalog, NewTriggerCommand(
		alignedRequest("acc-quest", 17),
		SetTriggerRequest{QuestID: 3145, TriggerType: 1},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !result.StateChanged || result.ProgressValue != 0 {
		t.Fatalf("seeking PVF fallback result=%+v", result)
	}
}

func TestOwnerApplyAcceptActivatesNoRewardMainSubQuests(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "acc-quest",
		Job:         "2",
		Level:       3,
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "completed"},
		},
	}); err != nil {
		t.Fatalf("save quest: %v", err)
	}
	source := catalogTestSource{
		DefaultList: "3145 `pre.qst`\n3146 `parent.qst`\n3157 `hunt.qst`\n3054 `clear.qst`\n",
		"n_quest/pre.qst": questCatalogTestDefinition("[epic]", 1, 99, "[gunner]",
			"[type]\n`[clear map]`\n[int data]\n76126\n"),
		"n_quest/parent.qst": questCatalogTestDefinition("[epic]", 3, 99, "[gunner]",
			"[type]\n`[quest clear]`\n[pre required quest]\n3145\n[int data]\n3157 3054\n"),
		"n_quest/hunt.qst": questCatalogTestDefinition("[sub]", 3, 99, "[gunner]",
			"[type]\n`[hunt enemy]`\n[main quest]\n3146\n[int data]\n3 -1 13099 3 1\n"),
		"n_quest/clear.qst": questCatalogTestDefinition("[sub]", 1, 99, "[gunner]",
			"[type]\n`[clear map]`\n[main quest]\n3146\n[int data]\n76136\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyAccept(ctx, catalog, CharacterEligibility{Level: 3, Job: 2}, NewQuestIDCommand(alignedRequest("acc-quest", 19), "accept_quest", QuestIDRequest{QuestID: 3146}))
	if err != nil {
		t.Fatalf("ApplyAccept parent error = %v", err)
	}
	if result.QuestID != 3146 || result.InitTrigger != 2 || result.Idempotent {
		t.Fatalf("parent accept result = %+v", result)
	}
	stored, ok, err := repos.Quest.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatalf("load stored quest ok=%t err=%v", ok, err)
	}
	for _, check := range []struct {
		id      int64
		trigger int64
		linked  bool
	}{
		{id: 3146, trigger: 2},
		{id: 3157, trigger: 1, linked: true},
		{id: 3054, trigger: 1, linked: true},
	} {
		state := stored.States[check.id]
		if state.Status != "active" || state.ProgressValue != check.trigger {
			t.Fatalf("quest %d state = %+v", check.id, state)
		}
		if check.linked && (state.Extra["main_quest_id"] != "3146" || state.Extra["auto_activated_by_main_quest"] != "true") {
			t.Fatalf("quest %d linked extra = %+v", check.id, state.Extra)
		}
	}
}

func TestOwnerApplyActiveQuestClearLinkedSubQuestActivationRepairsExistingParent(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "acc-quest",
		Job:         "2",
		Level:       3,
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3146: {Status: "active", ProgressValue: 2},
		},
	}); err != nil {
		t.Fatalf("save quest: %v", err)
	}
	source := catalogTestSource{
		DefaultList: "3146 `parent.qst`\n3157 `hunt.qst`\n3054 `clear.qst`\n",
		"n_quest/parent.qst": questCatalogTestDefinition("[epic]", 3, 99, "[gunner]",
			"[type]\n`[quest clear]`\n[int data]\n3157 3054\n"),
		"n_quest/hunt.qst": questCatalogTestDefinition("[sub]", 3, 99, "[gunner]",
			"[type]\n`[hunt enemy]`\n[main quest]\n3146\n[int data]\n3 -1 13099 3 1\n"),
		"n_quest/clear.qst": questCatalogTestDefinition("[sub]", 1, 99, "[gunner]",
			"[type]\n`[clear map]`\n[main quest]\n3146\n[int data]\n76136\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	activatedAt := time.Date(2026, 7, 17, 14, 20, 0, 0, time.UTC)
	result, err := owner.ApplyActiveQuestClearLinkedSubQuestActivation(ctx, catalog, "19", activatedAt)
	if err != nil {
		t.Fatalf("ApplyActiveQuestClearLinkedSubQuestActivation error = %v", err)
	}
	if result.Idempotent || len(result.Activations) != 2 || len(result.ChangedFields) != 1 || result.ChangedFields[0] != dnfrepo.QuestFieldStates {
		t.Fatalf("activation result=%+v", result)
	}
	stored, ok, err := repos.Quest.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatalf("load stored quest ok=%t err=%v", ok, err)
	}
	for _, questID := range []int64{3157, 3054} {
		state := stored.States[questID]
		if state.Status != "active" || state.ProgressValue != 1 ||
			state.Extra["main_quest_id"] != "3146" ||
			state.Extra["auto_activated_by_main_quest"] != "true" ||
			state.Extra["auto_activation_reason"] != "active_quest_clear_parent_reconcile" {
			t.Fatalf("linked quest %d state=%+v", questID, state)
		}
	}
	replay, err := owner.ApplyActiveQuestClearLinkedSubQuestActivation(ctx, catalog, "19", activatedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("replay activation error = %v", err)
	}
	if !replay.Idempotent || len(replay.Activations) != 0 || len(replay.ChangedFields) != 0 {
		t.Fatalf("replay result=%+v", replay)
	}
}

func TestOwnerApplyAcceptEventItemQuestRequiresAllocatorWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "18", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	catalog := testAcceptCatalog(t, "[type]\n`[clear map]`\n[int data]\n76126\n[depend give item]\n1001 1\n")
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewQuestIDCommand(alignedRequest("acc-quest", 18), "accept_quest", QuestIDRequest{QuestID: 3145})
	if _, err := owner.ApplyAccept(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, cmd); !errors.Is(err, ErrAcceptItemAllocatorRequired) {
		t.Fatalf("ApplyAccept error = %v", err)
	}
	if _, ok, err := repos.Quest.Load(ctx, "18"); err != nil || ok {
		t.Fatalf("event-item rejection mutated quest record ok=%t err=%v", ok, err)
	}
}

func TestOwnerApplyAcceptWithItemsAtomicallyCommitsQuestAndInventory(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "18", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	catalog := testAcceptCatalog(t, "[type]\n`[clear map]`\n[int data]\n76126\n[depend give item]\n1001 2\n")
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	allocate := func(record *dnfrepo.InventoryRecord, request FinishItemGrantRequest) (FinishCommittedItem, error) {
		if record.Slots == nil {
			record.Slots = make(map[string]dnfrepo.ItemStack)
		}
		const key = "0:65"
		stack := record.Slots[key]
		stack.ItemID = request.ItemID
		stack.Count += request.Count
		stack.RawEntry = []byte{1}
		record.Slots[key] = stack
		return FinishCommittedItem{
			SlotKey: key, SlotIndex: 65, ItemID: request.ItemID,
			Delta: request.Count, PostCount: stack.Count, CountOrSeed: uint32(request.Count), RawEntry: []byte{1},
		}, nil
	}
	cmd := NewQuestIDCommand(alignedRequest("acc-quest", 18), "accept_quest", QuestIDRequest{QuestID: 3145})
	result, err := owner.ApplyAcceptWithItems(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, cmd, allocate)
	if err != nil {
		t.Fatal(err)
	}
	if result.Idempotent || result.QuestID != 3145 || result.InitTrigger != 1 || len(result.EventItems) != 1 ||
		result.EventItems[0].ItemID != 1001 || result.EventItems[0].Delta != 2 {
		t.Fatalf("accept result=%+v", result)
	}
	questRecord, found, err := repos.Quest.Load(ctx, "18")
	if err != nil || !found || questRecord.States[3145].Status != "active" {
		t.Fatalf("quest record=%+v found=%t err=%v", questRecord, found, err)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "18")
	if err != nil || !found || inventory.Slots["0:65"].ItemID != 1001 || inventory.Slots["0:65"].Count != 2 {
		t.Fatalf("inventory=%+v found=%t err=%v", inventory, found, err)
	}
	replay, err := owner.ApplyAcceptWithItems(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, cmd, allocate)
	if err != nil || !replay.Idempotent || len(replay.EventItems) != 0 {
		t.Fatalf("idempotent replay=%+v err=%v", replay, err)
	}
	inventory, _, _ = repos.Inventory.Load(ctx, "18")
	if inventory.Slots["0:65"].Count != 2 {
		t.Fatalf("idempotent replay granted twice: %+v", inventory.Slots)
	}
}

func TestOwnerApplyAcceptWithItemsRollsBackBothAggregates(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "18", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	catalog := testAcceptCatalog(t, "[type]\n`[clear map]`\n[int data]\n76126\n[depend give item]\n1001 1 1002 1\n")
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	forced := errors.New("forced second event-item allocation failure")
	allocate := func(record *dnfrepo.InventoryRecord, request FinishItemGrantRequest) (FinishCommittedItem, error) {
		if request.ItemID == 1002 {
			return FinishCommittedItem{}, forced
		}
		if record.Slots == nil {
			record.Slots = make(map[string]dnfrepo.ItemStack)
		}
		record.Slots["0:65"] = dnfrepo.ItemStack{ItemID: request.ItemID, Count: request.Count}
		return FinishCommittedItem{SlotKey: "0:65", SlotIndex: 65, ItemID: request.ItemID, Delta: request.Count, PostCount: request.Count, CountOrSeed: uint32(request.Count), RawEntry: []byte{1}}, nil
	}
	cmd := NewQuestIDCommand(alignedRequest("acc-quest", 18), "accept_quest", QuestIDRequest{QuestID: 3145})
	if _, err := owner.ApplyAcceptWithItems(ctx, catalog, CharacterEligibility{Level: 1, Job: 2}, cmd, allocate); !errors.Is(err, forced) {
		t.Fatalf("ApplyAcceptWithItems error=%v", err)
	}
	if _, found, err := repos.Quest.Load(ctx, "18"); err != nil || found {
		t.Fatalf("rolled-back quest found=%t err=%v", found, err)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "18")
	if err != nil || !found || len(inventory.Slots) != 0 {
		t.Fatalf("rolled-back inventory=%+v found=%t err=%v", inventory, found, err)
	}
}

func TestOwnerApplyGiveUpPersistsActiveQuestRemoval(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "18",
		States: map[int64]dnfrepo.QuestState{
			3144: {Status: "completed", ProgressValue: 1},
			3145: {Status: "active", ProgressValue: 7},
			3146: {Status: "active", ProgressValue: 2},
		},
		Progress: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 7},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyGiveUp(
		ctx,
		testAcceptCatalog(t, "[type]\n`[disjoint item]`\n[int data]\n1\n"),
		NewQuestIDCommand(alignedRequest("acc-quest", 18), "giveup_quest", QuestIDRequest{QuestID: 3145}),
	)
	if err != nil {
		t.Fatalf("ApplyGiveUp() error = %v", err)
	}
	if result.AccountID != "acc-quest" || result.CharacterID != "18" || result.QuestID != 3145 {
		t.Fatalf("ApplyGiveUp() result = %+v", result)
	}
	stored, ok, err := repos.Quest.Load(ctx, "18")
	if err != nil || !ok {
		t.Fatalf("load stored quest ok=%t err=%v", ok, err)
	}
	if _, exists := stored.States[3145]; exists {
		t.Fatalf("abandoned quest remains in states: %+v", stored.States[3145])
	}
	if _, exists := stored.Progress[3145]; exists {
		t.Fatalf("abandoned quest remains in progress: %+v", stored.Progress[3145])
	}
	if stored.States[3144].Status != "completed" || stored.States[3146].Status != "active" {
		t.Fatalf("unrelated quest states changed: %+v", stored.States)
	}
	if _, err := owner.ApplyGiveUp(
		ctx,
		testAcceptCatalog(t, "[type]\n`[disjoint item]`\n[int data]\n1\n"),
		NewQuestIDCommand(alignedRequest("acc-quest", 18), "giveup_quest", QuestIDRequest{QuestID: 3145}),
	); !errors.Is(err, ErrQuestNotActive) {
		t.Fatalf("ApplyGiveUp() replay error = %v, want %v", err, ErrQuestNotActive)
	}
}

// 放弃任务的判据从这里搬到了 give_up_reclaim_test.go：
// `[cant giveup]` 仍然是唯一的拒绝理由，收集类任务改为"允许放弃 + 回收任务专属道具"。
func testAcceptCatalog(t *testing.T, extra string) *Catalog {
	t.Helper()
	source := catalogTestSource{
		DefaultList:          "3145 `accept.qst`\n",
		"n_quest/accept.qst": questCatalogTestDefinition("[epic]", 1, 99, "[gunner]", extra),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func alignedRequest(accountID string, selectedCharacterID uint16) alignedcmd.Request {
	return alignedcmd.Request{AccountID: accountID, SelectedCharacterID: selectedCharacterID}
}
