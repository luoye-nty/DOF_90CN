package quest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

func TestOwnerApplyFinishSettlementCommitsAndReplaysOneReceipt(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	tables := finishOwnerProgression(t)
	input := finishOwnerInput(tables, finishOwnerAllocator(nil))

	first, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AtomicCommitted || first.Replayed || first.CompletionKey != "run-19/op117/426" || first.QuestID != 3145 {
		t.Fatalf("first result identity=%+v", first)
	}
	if first.ExperienceDelta != 100 || first.PreviousLevel != 1 || first.NewLevel != 2 || first.NewExperience != 100 || first.SPDelta != 30 {
		t.Fatalf("first progression=%+v", first)
	}
	if len(first.Items) != 1 || first.Items[0].ItemID != 10403 || first.Items[0].Delta != 2 || first.Items[0].PostCount != 2 {
		t.Fatalf("first items=%+v", first.Items)
	}
	if len(first.Currency) != 1 || first.Currency[0].Name != "gold" || first.Currency[0].Delta != 10 || first.Currency[0].PostValue != 10 {
		t.Fatalf("first currency=%+v", first.Currency)
	}
	questRecord, ok, err := repos.Quest.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatalf("load quest ok=%t err=%v", ok, err)
	}
	state := questRecord.States[3145]
	if state.Status != "completed" || state.Extra["reward_state"] != "granted" || state.Extra["completion_key"] != "run-19/op117/426" || state.Extra[finishSettlementReceiptKey] == "" {
		t.Fatalf("persisted quest=%+v", state)
	}

	second, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AtomicCommitted || !second.Replayed || second.ExperienceDelta != first.ExperienceDelta || second.SPDelta != first.SPDelta || len(second.Items) != 1 || second.Items[0].PostCount != 2 {
		t.Fatalf("replay result=%+v", second)
	}
	if len(second.Currency) != 1 || second.Currency[0].PostValue != 10 {
		t.Fatalf("replay currency=%+v", second.Currency)
	}
	inventory, ok, err := repos.Inventory.Load(ctx, "19")
	if err != nil || !ok || inventory.Slots["0:65"].Count != 2 {
		t.Fatalf("replay duplicated inventory ok=%t err=%v inventory=%+v", ok, err, inventory)
	}
	character, ok, err := repos.Character.Load(ctx, "19")
	if err != nil || !ok || character.Stats["gold"] != 10 {
		t.Fatalf("replay duplicated gold ok=%t err=%v character=%+v", ok, err, character)
	}
}

func TestPlanFinishProgressionPinsQuestExperienceAtAbsoluteLevel90Cap(t *testing.T) {
	var thresholds strings.Builder
	var skillPoints strings.Builder
	for level := 1; level <= progression.MaximumCharacterLevel+1; level++ {
		fmt.Fprintf(&thresholds, "%d ", level*1000)
		fmt.Fprintf(&skillPoints, "%d 0\n", level)
	}
	tables, err := progression.Load(context.Background(), finishProgressionSource{
		progression.ExperienceTablePath: thresholds.String(),
		progression.SkillPointTablePath: "[sp table]\n" + skillPoints.String() + "[/sp table]\n[tp table]\n91 0\n[/tp table]\n",
		progression.QuestParameterPath:  "[difficulty]\n`N` 100\n[/difficulty]\n[exp reward table]\n100 -1\n[gold reward table]\n0\n[green level penalty]\n80\n[grey level penalty]\n30\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	entryLevel90 := uint32((progression.MaximumCharacterLevel - 1) * 1000)
	for _, test := range []struct {
		name       string
		level      int
		experience uint32
		gain       uint32
	}{
		{name: "level 89 crosses only into 90", level: 89, experience: 88_000, gain: 5_000},
		{name: "level 90 cannot advance to 91", level: 90, experience: entryLevel90, gain: 5_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := planFinishProgression(
				tables,
				dnfrepo.CharacterRecord{Level: test.level, Stats: map[string]int64{"exp": int64(test.experience)}},
				dnfrepo.SkillRecord{Points: dnfrepo.SkillPointState{SyncedLevel: test.level}},
				test.gain,
			)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Experience.NewLevel != progression.MaximumCharacterLevel ||
				plan.Experience.NewExperience != entryLevel90 || !plan.Experience.Saturated {
				t.Fatalf("plan = %+v, want level=%d exp=%d saturated", plan.Experience, progression.MaximumCharacterLevel, entryLevel90)
			}
			if plan.SkillPoints.New.SyncedLevel != progression.MaximumCharacterLevel {
				t.Fatalf("skill points = %+v", plan.SkillPoints)
			}
		})
	}
}

func TestApplyFinishRewardCurrencyUsesQuestLevelGoldWithoutPlayerGapPenalty(t *testing.T) {
	tables := finishOwnerLevel60GoldProgression(t)
	for _, ignoreLevel := range []bool{false, true} {
		character := dnfrepo.CharacterRecord{
			Level: 90,
			Stats: map[string]int64{"gold": 100},
		}
		currency, err := applyFinishRewardCurrency(&character, tables, FinishRewardPlan{
			QuestLevel:      60,
			IgnoreLevel4Exp: ignoreLevel,
			HasGoldReward:   true,
			GoldMultiple:    10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(currency) != 1 || currency[0].Delta != 442 || currency[0].PostValue != 542 || character.Stats["gold"] != 542 {
			t.Fatalf("ignore_level=%t currency=%+v stats=%v", ignoreLevel, currency, character.Stats)
		}
	}
}

func TestAllocateFinishRewardItemsPreservesEquipmentInstancesAndMergesActualStacks(t *testing.T) {
	record := dnfrepo.InventoryRecord{CharacterID: "19", Slots: make(map[string]dnfrepo.ItemStack)}
	nextEquipmentSlot := uint16(9)
	allocator := func(record *dnfrepo.InventoryRecord, request FinishItemGrantRequest) (FinishCommittedItem, error) {
		var slot uint16
		var postCount int64
		switch request.ItemID {
		case 101040191:
			if request.Count != 1 {
				return FinishCommittedItem{}, fmt.Errorf("equipment count=%d want=1", request.Count)
			}
			slot = nextEquipmentSlot
			nextEquipmentSlot++
			postCount = 1
		case 10403:
			slot = 65
			if existing, ok := record.Slots[fmt.Sprintf("0:%d", slot)]; ok {
				postCount = existing.Count
			}
			postCount += request.Count
		default:
			return FinishCommittedItem{}, fmt.Errorf("unexpected item=%d", request.ItemID)
		}
		key := fmt.Sprintf("0:%d", slot)
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint16(raw[0:2], slot)
		binary.LittleEndian.PutUint32(raw[2:6], uint32(request.ItemID))
		binary.LittleEndian.PutUint32(raw[6:10], uint32(postCount))
		record.Slots[key] = dnfrepo.ItemStack{ItemID: request.ItemID, Count: postCount, RawEntry: append([]byte(nil), raw...)}
		return FinishCommittedItem{
			SlotKey: key, SlotIndex: slot, ItemID: request.ItemID, Delta: request.Count,
			PostCount: postCount, CountOrSeed: uint32(request.Count), RawEntry: raw,
		}, nil
	}

	items, err := allocateFinishRewardItems(&record, []RewardItemRule{
		{ItemID: 101040191, Count: 1},
		{ItemID: 101040191, Count: 1},
		{ItemID: 10403, Count: 2},
		{ItemID: 10403, Count: 3},
	}, allocator, FinishItemGrantRequest{QuestID: 4728, CompletionKey: "quest-finish/19/4728"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].SlotKey != "0:9" || items[1].SlotKey != "0:10" ||
		items[0].ItemID != 101040191 || items[1].ItemID != 101040191 ||
		items[0].Delta != 1 || items[1].Delta != 1 {
		t.Fatalf("equipment receipts=%+v", items)
	}
	stack := items[2]
	if stack.SlotKey != "0:65" || stack.ItemID != 10403 || stack.Delta != 5 ||
		stack.PostCount != 5 || stack.CountOrSeed != 5 || binary.LittleEndian.Uint32(stack.RawEntry[6:10]) != 5 {
		t.Fatalf("stack receipt=%+v raw=%x", stack, stack.RawEntry)
	}
}

func TestOwnerApplyFinishSettlementPersistsHellPartyPermissionAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	definition := Definition{
		ID: 2900, Path: "metrocenter/hell.qst", Type: "[clear map]", LevelMin: 1, LevelMax: 99,
		Difficulty: "N", RewardType: "[hell challenge]", RewardIntData: []int64{1},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	questRecord, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("seed quest load found=%t err=%v", found, err)
	}
	questRecord.States[2900] = dnfrepo.QuestState{Status: "active", Extra: map[string]string{
		"completion_key": "run-19/op117/2900", "completion_kind": "clear_map", "reward_state": "pending",
	}}
	if err := repos.Quest.Save(ctx, questRecord); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 2900, Multiplier: 1,
		ExpectedCompletionKey: "run-19/op117/2900", CommittedAt: time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC),
		Progression: finishOwnerProgression(t), HellPartyUnlocks: []HellPartyUnlock{{AreaID: 290, QuestID: 2900}},
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil || !first.AtomicCommitted || first.Replayed {
		t.Fatalf("first hell settlement=%+v err=%v", first, err)
	}
	permission, found, err := repos.HellPartyPermission.Load(ctx, "19")
	if err != nil || !found || len(permission.Entries) != 1 || permission.Entries[0].AreaID != 290 || permission.Entries[0].QuestID != 2900 {
		t.Fatalf("persisted hell permission=%+v found=%t err=%v", permission, found, err)
	}
	completed, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found || completed.States[2900].Extra["reward_state"] != finishRewardGranted {
		t.Fatalf("persisted hell quest=%+v found=%t err=%v", completed.States[2900], found, err)
	}
	replayed, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil || !replayed.Replayed {
		t.Fatalf("hell replay=%+v err=%v", replayed, err)
	}
	permission, _, err = repos.HellPartyPermission.Load(ctx, "19")
	if err != nil || len(permission.Entries) != 1 {
		t.Fatalf("replay duplicated hell permission=%+v err=%v", permission, err)
	}
}

func TestOwnerApplyFinishSettlementCompletesInfernoChallengeFromMeetNPC(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		10104: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
	}}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 10104, Path: "Hell/achievement_70_Hell_inferno_2.qst", Type: "[meet npc]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N",
		RewardType: "[inferno challenge]", RewardIntData: []int64{7527},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 10104, Multiplier: 1,
		CommittedAt: time.Date(2026, 8, 23, 14, 1, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	})
	if err != nil || !result.AtomicCommitted || result.Replayed {
		t.Fatalf("inferno challenge settlement=%+v err=%v", result, err)
	}
	stored, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found || !IsCompleted(stored, 10104) || stored.States[10104].Extra["reward_state"] != finishRewardGranted {
		t.Fatalf("persisted inferno challenge quest=%+v found=%t err=%v", stored.States[10104], found, err)
	}
}

func TestOwnerApplyFinishSettlementRejectsStaleHellPartyReplayWhenPermissionMissing(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	definition := Definition{ID: 2900, Path: "metrocenter/hell.qst", Type: "[clear map]", LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[hell challenge]", RewardIntData: []int64{1}}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	questRecord, _, _ := repos.Quest.Load(ctx, "19")
	questRecord.States[2900] = dnfrepo.QuestState{Status: "active", Extra: map[string]string{"completion_key": "stale-2900", "completion_kind": "clear_map", "reward_state": "pending"}}
	if err := repos.Quest.Save(ctx, questRecord); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{AccountID: "acc", CharacterID: "19", QuestID: 2900, Multiplier: 1, ExpectedCompletionKey: "stale-2900", CommittedAt: time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC), Progression: finishOwnerProgression(t), HellPartyUnlocks: []HellPartyUnlock{{AreaID: 290, QuestID: 2900}}}
	if _, err := owner.ApplyFinishSettlement(ctx, catalog, input); err != nil {
		t.Fatal(err)
	}
	if err := repos.HellPartyPermission.(dnfrepo.HellPartyPermissionRestorer).Delete(ctx, "19", 290); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ApplyFinishSettlement(ctx, catalog, input); !errors.Is(err, ErrFinishQuestSettlementStale) {
		t.Fatalf("stale replay error=%v", err)
	}
}

func TestOwnerApplyFinishSettlementAppliesAndReceiptsGrowthContractQuestExperience(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := finishOwnerInput(finishOwnerProgression(t), finishOwnerAllocator(nil))
	input.ExperienceBonusPercent = 20

	first, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.BaseExperienceDelta != 100 ||
		first.ExperienceBonusDelta != 20 ||
		first.ExperienceDelta != 120 ||
		first.NewExperience != 120 {
		t.Fatalf("growth-contract quest progression=%+v, want base=100 bonus=20 total=120", first)
	}

	// The durable receipt owns the committed total. Expiry before a reconnect
	// replay must neither remove nor duplicate the already-granted bonus.
	input.ExperienceBonusPercent = 0
	replayed, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed ||
		replayed.BaseExperienceDelta != 100 ||
		replayed.ExperienceBonusDelta != 20 ||
		replayed.ExperienceDelta != 120 ||
		replayed.NewExperience != 120 {
		t.Fatalf("growth-contract quest replay=%+v", replayed)
	}
}

func TestOwnerApplyFinishSettlementFillsPVFMainQuestLevelGapAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 10
	character.Stats["exp"] = 80000
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	skill, found, err := repos.Skill.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load skill found=%t err=%v", found, err)
	}
	skill.Points = dnfrepo.SkillPointState{SyncedLevel: 10}
	if err := repos.Skill.Save(ctx, skill); err != nil {
		t.Fatal(err)
	}
	completionKey := "run-19/op117/3148"
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		3148: {Status: "active", Extra: map[string]string{
			"completion_key": completionKey, "completion_kind": "clear_map", "reward_state": "pending",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	definitions := []Definition{
		{ID: 3148, Path: "n_quest/quest_renewal/epic_quest/elvengard/elvengard_epic_04.qst", Grade: "[epic]", Type: "[clear map]", LevelMin: 8, LevelMax: 99, ExposedByNPC: -1, Difficulty: "N"},
		{ID: 3149, Path: "n_quest/quest_renewal/epic_quest/elvengard/elvengard_epic_05.qst", Grade: "[epic]", Type: "[clear map]", LevelMin: 11, LevelMax: 99, ExposedByNPC: -1, Difficulty: "N", PreRequiredGroups: [][]int64{{3148}}},
	}
	catalog := &Catalog{ordered: definitions, byID: map[int64]Definition{3148: definitions[0], 3149: definitions[1]}}
	progressionTables, err := progression.Load(ctx, finishProgressionSource{
		progression.ExperienceTablePath: "1000 2035 3315 5260 8500 13875 22435 35440 54360 80875 116875 164460 225940 303835 400875 520000\n",
		progression.SkillPointTablePath: "[sp table]\n1 0\n2 30\n3 30\n4 30\n5 30\n6 30\n7 30\n8 30\n9 30\n10 30\n11 30\n12 30\n13 30\n14 30\n15 30\n16 30\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n",
		progression.QuestParameterPath:  "[difficulty]\n`N` 100\n[/difficulty]\n[exp reward table]\n100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1 100 -1\n[gold reward table]\n0\n[green level penalty]\n80\n[grey level penalty]\n30\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 3148, Multiplier: 1,
		ExpectedCompletionKey: completionKey, CommittedAt: time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC),
		Progression: progressionTables,
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AtomicCommitted || first.Replayed || first.BaseExperienceDelta != 100 ||
		first.ContinuityExperienceDelta != 775 || first.ExperienceDelta != 875 ||
		first.PreviousLevel != 10 || first.NewLevel != 11 || first.NewExperience != 80875 || first.SPDelta != 30 {
		t.Fatalf("early-main progression=%+v", first)
	}
	replayed, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ContinuityExperienceDelta != 775 ||
		replayed.ExperienceDelta != 875 || replayed.NewLevel != 11 || replayed.NewExperience != 80875 {
		t.Fatalf("early-main replay=%+v", replayed)
	}
}

func TestOwnerApplyFinishSettlementFillsHeavenMainQuestLevelGap(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 63
	character.Stats["exp"] = 62900000
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	skill, found, err := repos.Skill.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load skill found=%t err=%v", found, err)
	}
	skill.Points = dnfrepo.SkillPointState{SyncedLevel: 63}
	if err := repos.Skill.Save(ctx, skill); err != nil {
		t.Fatal(err)
	}
	completionKey := "run-19/op117/3520"
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		3520: {Status: "active", Extra: map[string]string{
			"completion_key": completionKey, "completion_kind": "clear_map", "reward_state": "pending",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	definitions := []Definition{
		{ID: 3520, Path: "n_quest/quest_renewal/epic_quest/antber/antber_epic_09.qst", Grade: "[epic]", Type: "[clear map]", LevelMin: 63, LevelMax: 99, ExposedByNPC: -1, Difficulty: "N"},
		{ID: 3521, Path: "n_quest/quest_renewal/epic_quest/antber/antber_epic_10.qst", Grade: "[epic]", Type: "[clear map]", LevelMin: 64, LevelMax: 99, ExposedByNPC: -1, Difficulty: "N", PreRequiredGroups: [][]int64{{3520}}},
	}
	catalog := testContinuityCatalog(definitions)
	thresholdRows := make([]string, 65)
	spRows := make([]string, 65)
	tpRows := make([]string, 65)
	questRows := make([]string, 65)
	for index := range thresholdRows {
		level := index + 1
		thresholdRows[index] = fmt.Sprint(level * 1000000)
		spRows[index] = fmt.Sprintf("%d 30", level)
		tpRows[index] = fmt.Sprintf("%d 0", level)
		questRows[index] = "100 -1"
	}
	progressionTables, err := progression.Load(ctx, finishProgressionSource{
		progression.ExperienceTablePath: strings.Join(thresholdRows, " ") + "\n",
		progression.SkillPointTablePath: "[sp table]\n" + strings.Join(spRows, "\n") + "\n[/sp table]\n[tp table]\n" + strings.Join(tpRows, "\n") + "\n[/tp table]\n",
		progression.QuestParameterPath:  "[difficulty]\n`N` 100\n[/difficulty]\n[exp reward table]\n" + strings.Join(questRows, "\n") + "\n[gold reward table]\n0\n[green level penalty]\n80\n[grey level penalty]\n30\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 3520, Multiplier: 1,
		ExpectedCompletionKey: completionKey, CommittedAt: time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC),
		Progression: progressionTables,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseExperienceDelta != 100 || result.ContinuityExperienceDelta != 99900 ||
		result.ExperienceDelta != 100000 || result.PreviousLevel != 63 || result.NewLevel != 64 ||
		result.NewExperience != 63000000 || result.SPDelta != 30 {
		t.Fatalf("heaven-main progression=%+v", result)
	}
}

func TestOwnerApplyFinishSettlementConsumesSeekingMaterialsAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 7, 23, 15, 40, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		600: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
	}}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[0:2], 8)
	binary.LittleEndian.PutUint32(raw[2:6], 9001)
	binary.LittleEndian.PutUint32(raw[6:10], 30)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:8": {ItemID: 9001, Count: 30, RawEntry: raw, Extra: map[string]string{"count": "30"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 600, Path: "n_quest/submit_material.qst", Type: "[seeking]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", IntData: []int64{9001, 30},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 23, 15, 41, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AtomicCommitted || first.Replayed || len(first.ConsumedItems) != 1 {
		t.Fatalf("first result=%+v consumed=%+v", first, first.ConsumedItems)
	}
	consumed := first.ConsumedItems[0]
	if consumed.SlotKey != "0:8" || consumed.SlotIndex != 8 || consumed.ItemID != 9001 || consumed.Delta != 30 || consumed.PostCount != 0 || len(consumed.RawEntry) != 0 {
		t.Fatalf("consumed material=%+v", consumed)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:8"]; exists {
		t.Fatalf("material slot was not removed: %+v", inventory.Slots["0:8"])
	}

	second, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || len(second.ConsumedItems) != 1 || second.ConsumedItems[0].Delta != 30 {
		t.Fatalf("replay consumed=%+v", second.ConsumedItems)
	}
	inventory, found, err = repos.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("reload inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:8"]; exists {
		t.Fatalf("replay consumed material twice or left stale slot: %+v", inventory.Slots["0:8"])
	}
}

// TestOwnerApplyFinishSettlementChargesQuestGoldCost 锁住服主口径 2026-10-03：
// **[int data] 是 (ID, 数量) 成对，ID 0 = 金币**，完成任务要**扣**这笔钱。
//
// 现场：所有消耗金币的任务都不扣钱。例如开耳环槽位的任务 [int data] 是
// `0 1000000 3034 1000 ...`，那 100 万金币从来没扣过 —— 旧代码把 itemID <= 0 的 pair
// 当成非法数据整条丢掉了。全库 68 个任务受影响（46 个是纯金币交付）。
//
// 三个子用例：正常扣款、金币不足整笔拒绝（不扣钱也不吃材料）、重放不重复扣。
func TestOwnerApplyFinishSettlementChargesQuestGoldCost(t *testing.T) {
	const goldCost int64 = 1000000
	createQuest := func(t *testing.T, ctx context.Context, repos dnfrepo.Group, id int64, gold int64) *Catalog {
		t.Helper()
		character, found, err := repos.Character.Load(ctx, "19")
		if err != nil || !found {
			t.Fatalf("load character found=%t err=%v", found, err)
		}
		character.Stats["gold"] = gold
		if err := repos.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
			id: {Status: "active", ProgressValue: 0, Extra: map[string]string{"completion_key": "run-19/op117/gold", "completion_kind": "clear_map", "reward_state": "pending"}},
		}}); err != nil {
			t.Fatal(err)
		}
		definition := Definition{
			ID: id, Path: "n_quest/gold_cost.qst", Type: "[seeking]",
			LevelMin: 1, LevelMax: 99, Difficulty: "N",
			// 开耳环槽位那种形状：金币 + 若干材料。
			IntData: []int64{0, goldCost, 3034, 1000},
		}
		return &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{id: definition}}
	}

	t.Run("金币被扣且材料照常消耗", func(t *testing.T) {
		ctx := context.Background()
		repos := finishOwnerSeed(t, ctx)
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint16(raw[0:2], 70)
		binary.LittleEndian.PutUint32(raw[2:6], 3034)
		binary.LittleEndian.PutUint32(raw[6:10], 1000)
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: "19",
			Slots: map[string]dnfrepo.ItemStack{
				"0:70": {ItemID: 3034, Count: 1000, RawEntry: raw, Extra: map[string]string{"count": "1000"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		catalog := createQuest(t, ctx, repos, 910, 2000000)
		owner, err := NewOwner(repos)
		if err != nil {
			t.Fatal(err)
		}
		result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
			AccountID: "acc", CharacterID: "19", QuestID: 910, Multiplier: 1,
			CommittedAt: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
		})
		if err != nil {
			t.Fatalf("ApplyFinishSettlement error = %v", err)
		}
		if !result.AtomicCommitted {
			t.Fatalf("result=%+v", result)
		}
		if len(result.Currency) != 1 || result.Currency[0].Name != "gold" ||
			result.Currency[0].Delta != -goldCost || result.Currency[0].PostValue != 1000000 {
			t.Fatalf("currency=%+v，期望扣 %d 后余 1000000", result.Currency, goldCost)
		}
		character, _, err := repos.Character.Load(ctx, "19")
		if err != nil {
			t.Fatal(err)
		}
		if character.Stats["gold"] != 1000000 {
			t.Fatalf("gold=%d want 1000000", character.Stats["gold"])
		}
		if len(result.ConsumedItems) != 1 || result.ConsumedItems[0].ItemID != 3034 {
			t.Fatalf("材料应照常消耗: %+v", result.ConsumedItems)
		}
		inventory, _, err := repos.Inventory.Load(ctx, "19")
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := inventory.Slots["0:70"]; exists {
			t.Fatalf("材料格应被搬空: %+v", inventory.Slots)
		}
		// 金币绝不能被当成"ID 0 的道具"记进消耗表 —— 客户端会去背包找一件不存在的道具。
		for _, item := range result.ConsumedItems {
			if item.ItemID <= 0 {
				t.Fatalf("金币混进了道具消耗表: %+v", item)
			}
		}
	})

	t.Run("金币不足整笔拒绝且不吃材料", func(t *testing.T) {
		ctx := context.Background()
		repos := finishOwnerSeed(t, ctx)
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint16(raw[0:2], 70)
		binary.LittleEndian.PutUint32(raw[2:6], 3034)
		binary.LittleEndian.PutUint32(raw[6:10], 1000)
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: "19",
			Slots: map[string]dnfrepo.ItemStack{
				"0:70": {ItemID: 3034, Count: 1000, RawEntry: raw, Extra: map[string]string{"count": "1000"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		catalog := createQuest(t, ctx, repos, 911, goldCost-1)
		owner, err := NewOwner(repos)
		if err != nil {
			t.Fatal(err)
		}
		_, err = owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
			AccountID: "acc", CharacterID: "19", QuestID: 911, Multiplier: 1,
			CommittedAt: time.Date(2026, 10, 3, 9, 5, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
		})
		if !errors.Is(err, ErrFinishRequiredItemsMissing) {
			t.Fatalf("error = %v, want ErrFinishRequiredItemsMissing", err)
		}
		character, _, err := repos.Character.Load(ctx, "19")
		if err != nil {
			t.Fatal(err)
		}
		if character.Stats["gold"] != goldCost-1 {
			t.Fatalf("拒绝后金币不许变动: %d", character.Stats["gold"])
		}
		inventory, _, err := repos.Inventory.Load(ctx, "19")
		if err != nil {
			t.Fatal(err)
		}
		if stack := inventory.Slots["0:70"]; stack.ItemID != 3034 || stack.Count != 1000 {
			t.Fatalf("拒绝后材料不许被吃掉: %+v", stack)
		}
	})

	t.Run("重放不重复扣金币", func(t *testing.T) {
		ctx := context.Background()
		repos := finishOwnerSeed(t, ctx)
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint16(raw[0:2], 70)
		binary.LittleEndian.PutUint32(raw[2:6], 3034)
		binary.LittleEndian.PutUint32(raw[6:10], 1000)
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: "19",
			Slots: map[string]dnfrepo.ItemStack{
				"0:70": {ItemID: 3034, Count: 1000, RawEntry: raw, Extra: map[string]string{"count": "1000"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		catalog := createQuest(t, ctx, repos, 912, 2000000)
		owner, err := NewOwner(repos)
		if err != nil {
			t.Fatal(err)
		}
		input := FinishCommitInput{
			AccountID: "acc", CharacterID: "19", QuestID: 912, Multiplier: 1,
			CommittedAt: time.Date(2026, 10, 3, 9, 10, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
		}
		first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
		if err != nil {
			t.Fatal(err)
		}
		second, err := owner.ApplyFinishSettlement(ctx, catalog, input)
		if err != nil {
			t.Fatal(err)
		}
		if !second.Replayed {
			t.Fatalf("第二次必须是重放: %+v", second)
		}
		if len(second.Currency) != len(first.Currency) ||
			second.Currency[0].Delta != -goldCost || second.Currency[0].PostValue != first.Currency[0].PostValue {
			t.Fatalf("重放收据=%+v want=%+v", second.Currency, first.Currency)
		}
		character, _, err := repos.Character.Load(ctx, "19")
		if err != nil {
			t.Fatal(err)
		}
		if character.Stats["gold"] != 1000000 {
			t.Fatalf("重放把金币扣了两次: %d", character.Stats["gold"])
		}
	})
}

// TestOwnerApplyFinishSettlementChargesGoldAnywhereInIntData 覆盖"金币 pair 不在首位"的写法。
//
// 服主 2026-10-03 追问：`[int data]` 写成
//
//	3034 1000 3033 1000 3035 1000 3036 1000 0 1000000
//
// 服务端还能不能识别并扣除？口径是 (ID, 数量) 成对、ID 0 = 金币，**位置不该有语义**。
// 这条用例同时钉住三个容易写错的地方：
//  1. 金币在任意位置都要被认出来（不能只读 IntData[0]）；
//  2. [int data] 里其余材料照常消耗；
//  3. **不能重复计费** —— `finishDefinitionGoldCost` 自己遍历一遍、
//     `finishRequiredItemRules` 又遍历一遍，两边都必须只认金币一次。
func TestOwnerApplyFinishSettlementChargesGoldAnywhereInIntData(t *testing.T) {
	const goldCost int64 = 1000000
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["gold"] = 2000000
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	slots := map[string]dnfrepo.ItemStack{}
	for index, itemID := range []int64{3034, 3033, 3035, 3036} {
		slot := int16(70 + index)
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint16(raw[0:2], uint16(slot))
		binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
		binary.LittleEndian.PutUint32(raw[6:10], 1000)
		slots[fmt.Sprintf("0:%d", slot)] = dnfrepo.ItemStack{
			ItemID: itemID, Count: 1000, RawEntry: raw, Extra: map[string]string{"count": "1000"},
		}
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
		t.Fatal(err)
	}
	// 金币写在**最后**：材料在前、金币在后，正是服主举的那种写法。
	definition := Definition{
		ID: 913, Path: "n_quest/gold_last.qst", Type: "[seeking]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N",
		IntData: []int64{3034, 1000, 3033, 1000, 3035, 1000, 3036, 1000, 0, goldCost},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}

	// 规划层：GoldCost 必须认出来（位置无关）。
	plan, err := catalog.PlanFinishReward(CharacterEligibility{Level: 1, Job: 2}, definition.ID, ^uint16(0), false)
	if err != nil {
		t.Fatalf("PlanFinishReward error = %v", err)
	}
	if plan.GoldCost != goldCost {
		t.Fatalf("GoldCost=%d want %d（金币不在首位也必须认出来）", plan.GoldCost, goldCost)
	}
	// 金币绝不能出现在道具规则里，否则客户端会去找一件 id=0 的道具。
	for _, item := range plan.Items {
		if item.ItemID <= 0 {
			t.Fatalf("金币混进了奖励/道具规则: %+v", item)
		}
	}

	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		definition.ID: {Status: "active", ProgressValue: 0, Extra: map[string]string{
			"completion_key": "run-19/op117/gold-last", "completion_kind": "clear_map", "reward_state": "pending",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	})
	if err != nil {
		t.Fatalf("ApplyFinishSettlement error = %v", err)
	}
	// 只扣一次：2,000,000 - 1,000,000 = 1,000,000。重复计费会掉到 0。
	if len(result.Currency) != 1 || result.Currency[0].Delta != -goldCost || result.Currency[0].PostValue != 1000000 {
		t.Fatalf("currency=%+v，期望只扣一次 %d 后余 1000000", result.Currency, goldCost)
	}
	character, _, err = repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if character.Stats["gold"] != 1000000 {
		t.Fatalf("金币=%d want 1000000（被扣了不止一次？）", character.Stats["gold"])
	}
	// 四种材料都要被消耗（金币在末位不能影响它们）。
	if len(result.ConsumedItems) != 4 {
		t.Fatalf("消耗材料=%d 条，want 4: %+v", len(result.ConsumedItems), result.ConsumedItems)
	}
	for _, item := range result.ConsumedItems {
		if item.ItemID <= 0 || item.Delta != 1000 {
			t.Fatalf("材料消耗行不对: %+v", item)
		}
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	for slot := int16(70); slot <= 73; slot++ {
		if _, exists := inventory.Slots[fmt.Sprintf("0:%d", slot)]; exists {
			t.Fatalf("材料格 %d 应被搬空: %+v", slot, inventory.Slots)
		}
	}
}

func TestOwnerApplyFinishSettlementConsumesAccountSharedSeekingMaterialsAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		602: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
	}}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[0:2], 358)
	binary.LittleEndian.PutUint32(raw[2:6], 3037)
	binary.LittleEndian.PutUint32(raw[6:10], 130)
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {ItemID: 3037, Count: 130, RawEntry: raw, Extra: map[string]string{"count": "130"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 602, Path: "n_quest/account_shared_submit_material.qst", Type: "[seeking]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", IntData: []int64{3037, 100},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 27, 16, 1, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AtomicCommitted || first.Replayed || len(first.ConsumedItems) != 1 {
		t.Fatalf("first result=%+v consumed=%+v", first, first.ConsumedItems)
	}
	consumed := first.ConsumedItems[0]
	if consumed.SlotKey != "0:358" || consumed.SlotIndex != 358 || consumed.ItemID != 3037 || consumed.Delta != 100 || consumed.PostCount != 30 {
		t.Fatalf("shared consumed material=%+v", consumed)
	}
	account, found, err := repos.AccountInventory.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(358)]
	if stack.Count != 30 || binary.LittleEndian.Uint32(stack.RawEntry[6:10]) != 30 || stack.Extra["count"] != "30" {
		t.Fatalf("shared material after finish=%+v", stack)
	}
	if got := first.PostCommitAccountInventory.Slots["0:358"].Count; got != 30 {
		t.Fatalf("post-commit shared count=%d want=30", got)
	}

	second, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || len(second.ConsumedItems) != 1 || second.ConsumedItems[0].Delta != 100 {
		t.Fatalf("replay consumed=%+v", second.ConsumedItems)
	}
	account, _, _ = repos.AccountInventory.Load(ctx, "acc")
	if got := account.Slots["0:358"].Count; got != 30 {
		t.Fatalf("replay consumed shared material twice: count=%d", got)
	}
}

func TestOwnerApplyFinishSettlementRollsBackAccountSharedMaterialsOnAllocatorFailure(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 7, 27, 16, 2, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		603: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
	}}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[0:2], 358)
	binary.LittleEndian.PutUint32(raw[2:6], 3037)
	binary.LittleEndian.PutUint32(raw[6:10], 100)
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 100, RawEntry: raw}},
	}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 603, Path: "n_quest/account_shared_rollback.qst", Type: "[seeking]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", IntData: []int64{3037, 100},
		RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 10403, Count: 1}},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("reward allocation rejected")
	_, err = owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 27, 16, 3, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
		AllocateItem: finishOwnerAllocator(wantErr),
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ApplyFinishSettlement error=%v want=%v", err, wantErr)
	}
	account, _, _ := repos.AccountInventory.Load(ctx, "acc")
	if got := account.Slots["0:358"].Count; got != 100 {
		t.Fatalf("failed settlement changed shared material count=%d", got)
	}
	quests, _, _ := repos.Quest.Load(ctx, "19")
	if state := quests.States[603]; state.Status != "active" || state.Extra[finishSettlementReceiptKey] != "" {
		t.Fatalf("failed settlement changed quest=%+v", state)
	}
}

func TestOwnerApplyFinishSettlementRejectsMissingSeekingMaterials(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		601: {Status: "active", ProgressValue: 0, UpdatedAt: time.Date(2026, 7, 23, 15, 42, 0, 0, time.UTC)},
	}}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[0:2], 9)
	binary.LittleEndian.PutUint32(raw[2:6], 9001)
	binary.LittleEndian.PutUint32(raw[6:10], 29)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9": {ItemID: 9001, Count: 29, RawEntry: raw},
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 601, Path: "n_quest/missing_submit_material.qst", Type: "[seeking]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", IntData: []int64{9001, 30},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyFinishSettlement(ctx, &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 23, 15, 43, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	})
	if !errors.Is(err, ErrFinishRequiredItemsMissing) {
		t.Fatalf("missing material error=%v", err)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "19")
	if got := inventory.Slots["0:9"].Count; got != 29 {
		t.Fatalf("missing material rollback count=%d want=29", got)
	}
	quests, _, _ := repos.Quest.Load(ctx, "19")
	if state := quests.States[601]; state.Status != "active" || state.Extra[finishSettlementReceiptKey] != "" {
		t.Fatalf("missing material mutated quest=%+v", state)
	}
}

func TestFinishRequiredItemAvailableCountTreatsEmptyAccountSharedCellAsZero(t *testing.T) {
	stack := dnfrepo.ItemStack{ItemID: 3037, Count: 0}
	if got := finishRequiredItemAvailableCount(stack, true); got != 0 {
		t.Fatalf("empty account-shared material count=%d want=0", got)
	}
	if got := finishRequiredItemAvailableCount(stack, false); got != 1 {
		t.Fatalf("ordinary single-item count=%d want=1", got)
	}
}

func TestOwnerApplyFinishSettlementRollsBackLateAllocatorFailure(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("second asset write rejected")
	_, err = owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), finishOwnerInput(finishOwnerProgression(t), finishOwnerAllocator(wantErr)))
	if !errors.Is(err, wantErr) {
		t.Fatalf("ApplyFinishSettlement error=%v want=%v", err, wantErr)
	}
	character, _, _ := repos.Character.Load(ctx, "19")
	questRecord, _, _ := repos.Quest.Load(ctx, "19")
	skill, _, _ := repos.Skill.Load(ctx, "19")
	inventory, _, _ := repos.Inventory.Load(ctx, "19")
	if character.Level != 1 || character.Stats["exp"] != 0 || character.Stats["gold"] != 0 || skill.Points.SyncedLevel != 1 || skill.Points.TotalSP != 0 || len(inventory.Slots) != 0 {
		t.Fatalf("rollback aggregates character=%+v skill=%+v inventory=%+v", character, skill, inventory)
	}
	state := questRecord.States[3145]
	if state.Status != "active" || state.ProgressValue != 0 || state.Extra["reward_state"] != "pending" {
		t.Fatalf("rollback quest=%+v", state)
	}
}

func TestOwnerApplyFinishSettlementRejectsConflictingAndStaleReplay(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := finishOwnerInput(finishOwnerProgression(t), finishOwnerAllocator(nil))
	if _, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input); err != nil {
		t.Fatal(err)
	}
	conflict := input
	conflict.ExpectedCompletionKey = "different-run"
	if _, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), conflict); !errors.Is(err, ErrFinishQuestCompletionConflict) {
		t.Fatalf("conflicting replay error=%v", err)
	}
	inventory, ok, err := repos.Inventory.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatal(err)
	}
	inventory = dnfrepo.CloneInventory(inventory)
	stack := inventory.Slots["0:65"]
	stack.Count++
	inventory.Slots["0:65"] = stack
	if err := repos.Inventory.Save(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input); !errors.Is(err, ErrFinishQuestSettlementStale) {
		t.Fatalf("stale replay error=%v", err)
	}
}

func TestOwnerApplyFinishSettlementRecalculatesActiveQuestClearParents(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	record, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load seed quest found=%t err=%v", found, err)
	}
	record = dnfrepo.CloneQuest(record)
	record.States[4100] = dnfrepo.QuestState{Status: "active", ProgressValue: 1}
	record.Progress = map[int64]dnfrepo.QuestState{
		4101: {Status: "active", ProgressValue: 2},
	}
	if err := repos.Quest.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	catalog := finishOwnerCatalog()
	parentInStates := Definition{ID: 4100, Type: "[quest clear]", IntData: []int64{3145}}
	parentInProgress := Definition{ID: 4101, Type: "[clear quest]", IntData: []int64{3145, 9999}}
	catalog.ordered = append(catalog.ordered, parentInStates, parentInProgress)
	catalog.byID[parentInStates.ID] = parentInStates
	catalog.byID[parentInProgress.ID] = parentInProgress

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, finishOwnerInput(finishOwnerProgression(t), finishOwnerAllocator(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.PostCommitQuest.States[4100].ProgressValue; got != 0 {
		t.Fatalf("same-field parent trigger=%d want=0", got)
	}
	if got := result.PostCommitQuest.Progress[4101].ProgressValue; got != 1 {
		t.Fatalf("cross-field parent trigger=%d want=1", got)
	}
	persisted, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load persisted quest found=%t err=%v", found, err)
	}
	if got := persisted.States[4100].ProgressValue; got != 0 {
		t.Fatalf("persisted same-field parent trigger=%d want=0", got)
	}
	if got := persisted.Progress[4101].ProgressValue; got != 1 {
		t.Fatalf("persisted cross-field parent trigger=%d want=1", got)
	}
}

func TestOwnerApplyFinishSettlementRecomputesQuestClearParentBeforeFinish(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3146: {Status: "active", ProgressValue: 1},
			3157: {Status: "completed", ProgressValue: 0},
			3054: {Status: "completed", ProgressValue: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 3146, Path: "n_quest/elvengard_epic_02.qst", Type: "[quest clear]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[item]",
		IntData: []int64{3157, 3054},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID:    "acc",
		CharacterID:  "19",
		QuestID:      3146,
		Multiplier:   1,
		CommittedAt:  time.Date(2026, 7, 17, 12, 45, 0, 0, time.UTC),
		Progression:  finishOwnerProgression(t),
		AllocateItem: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := result.PostCommitQuest.States[3146]
	if state.Status != "completed" || state.ProgressValue != 0 ||
		state.Extra["completion_kind"] != "active_trigger_zero_op34" ||
		state.Extra["reward_state"] != finishRewardGranted {
		t.Fatalf("quest-clear parent state=%+v", state)
	}
}

func TestOwnerApplyFinishSettlementRejectsQuestClearParentWithMissingChildEvenIfTriggerZero(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3146: {Status: "active", ProgressValue: 0},
			3157: {Status: "completed", ProgressValue: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 3146, Path: "n_quest/elvengard_epic_02.qst", Type: "[quest clear]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[item]",
		IntData: []int64{3157, 3054},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID:    "acc",
		CharacterID:  "19",
		QuestID:      3146,
		Multiplier:   1,
		CommittedAt:  time.Date(2026, 7, 17, 12, 46, 0, 0, time.UTC),
		Progression:  finishOwnerProgression(t),
		AllocateItem: nil,
	})
	if !errors.Is(err, ErrFinishQuestNotPending) {
		t.Fatalf("finish missing-child quest-clear error=%v", err)
	}
}

func TestOwnerApplyFinishSettlementCompletesTerminalZeroTriggerLinkedSubQuest(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3249: {Status: "active", ProgressValue: 0},
			3609: {Status: "completed", ProgressValue: 0},
			3610: {Status: "active", ProgressValue: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	parent := Definition{
		ID: 3249, Path: "n_quest/alphraira_epic_26.qst", Type: "[quest clear]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[item]", RewardIntData: []int64{0, 0},
		IntData: []int64{3609, 3610},
	}
	completedChild := Definition{ID: 3609, MainQuestID: 3249, Grade: "[sub]", Type: "[meet npc]", NoExperience: true}
	terminalChild := Definition{ID: 3610, MainQuestID: 3249, Grade: "[sub]", Type: "[seek n meet npc]", NoExperience: true}
	catalog := &Catalog{
		ordered: []Definition{parent, completedChild, terminalChild},
		byID: map[int64]Definition{
			parent.ID:         parent,
			completedChild.ID: completedChild,
			terminalChild.ID:  terminalChild,
		},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID:    "acc",
		CharacterID:  "19",
		QuestID:      parent.ID,
		Multiplier:   1,
		CommittedAt:  time.Date(2026, 7, 29, 17, 50, 0, 0, time.UTC),
		Progression:  finishOwnerProgression(t),
		AllocateItem: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state := result.PostCommitQuest.States[parent.ID]; state.Status != "completed" || state.Extra["reward_state"] != finishRewardGranted {
		t.Fatalf("parent state=%+v", state)
	}
	childState := result.PostCommitQuest.States[terminalChild.ID]
	if childState.Status != "completed" || childState.ProgressValue != 0 ||
		childState.Extra["reward_state"] != finishRewardGranted ||
		childState.Extra["auto_completed_by_parent"] != "3249" ||
		childState.Extra["auto_complete_reason"] != "quest_clear_parent_terminal_zero_trigger" {
		t.Fatalf("terminal child state=%+v", childState)
	}
	persisted, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load persisted quest found=%t err=%v", found, err)
	}
	if state := persisted.States[terminalChild.ID]; state.Status != "completed" || state.Extra["auto_completed_by_parent"] != "3249" {
		t.Fatalf("persisted terminal child state=%+v", state)
	}
}

func TestOwnerApplyFinishSettlementProjectsFinalRewardChildAtomically(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			5000: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
			5001: {Status: "completed", ProgressValue: 0, UpdatedAt: acceptedAt},
			5002: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
		},
	}); err != nil {
		t.Fatal(err)
	}
	parent := Definition{
		ID: 5000, Path: "n_quest/parent.qst", Grade: "[epic]", Type: "[quest clear]", SubType: 2,
		LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[item]", RewardIntData: []int64{0, 0},
		IntData: []int64{5001, 5002},
	}
	first := Definition{ID: 5001, Path: "n_quest/first.qst", Grade: "[sub]", Type: "[meet npc]", MainQuestID: 5000, NoExperience: true}
	final := Definition{
		ID: 5002, Path: "n_quest/final.qst", Grade: "[sub]", Type: "[seek n meet npc]", MainQuestID: 5000,
		LevelMin: 1, LevelMax: 99, Difficulty: "N", NoExperience: true,
		RewardType: "[item]", RewardIntData: []int64{10403, 2}, RewardItems: []RewardItemRule{{ItemID: 10403, Count: 2}},
	}
	catalog := &Catalog{
		ordered: []Definition{parent, first, final},
		byID:    map[int64]Definition{parent.ID: parent, first.ID: first, final.ID: final},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: parent.ID, Multiplier: 1,
		CommittedAt: acceptedAt.Add(time.Minute), Progression: finishOwnerProgression(t), AllocateItem: finishOwnerAllocator(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ItemID != 10403 || result.Items[0].Delta != 2 {
		t.Fatalf("projected child rewards=%+v", result.Items)
	}
	if state := result.PostCommitQuest.States[parent.ID]; state.Status != "completed" || state.Extra["reward_state"] != finishRewardGranted {
		t.Fatalf("parent state=%+v", state)
	}
	if state := result.PostCommitQuest.States[final.ID]; state.Status != "completed" || state.Extra["reward_state"] != finishRewardGranted || state.Extra["completed_by_parent"] != "5000" {
		t.Fatalf("reward child state=%+v", state)
	}
}

func TestOwnerApplyFinishSettlementRejectsNonzeroTerminalLinkedSubQuest(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3249: {Status: "active", ProgressValue: 0},
			3609: {Status: "completed", ProgressValue: 0},
			3610: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	parent := Definition{
		ID: 3249, Path: "n_quest/alphraira_epic_26.qst", Type: "[quest clear]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[item]", RewardIntData: []int64{0, 0},
		IntData: []int64{3609, 3610},
	}
	completedChild := Definition{ID: 3609, MainQuestID: 3249, Type: "[meet npc]"}
	terminalChild := Definition{ID: 3610, MainQuestID: 3249, Type: "[seek n meet npc]"}
	catalog := &Catalog{
		ordered: []Definition{parent, completedChild, terminalChild},
		byID: map[int64]Definition{
			parent.ID:         parent,
			completedChild.ID: completedChild,
			terminalChild.ID:  terminalChild,
		},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: parent.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 29, 17, 51, 0, 0, time.UTC),
		Progression: finishOwnerProgression(t),
	})
	if !errors.Is(err, ErrFinishQuestNotPending) {
		t.Fatalf("finish nonzero terminal child error=%v", err)
	}
}

func TestOwnerApplyFinishSettlementCommitsNPCProfessionTransitionAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	quests := dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		500: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
	}}
	if err := repos.Quest.Save(ctx, quests); err != nil {
		t.Fatal(err)
	}
	skill, _, _ := repos.Skill.Load(ctx, "19")
	skill.Skills = map[int64]dnfrepo.SkillState{99: {Level: 3, Enabled: true}}
	skill.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 99}}
	if err := repos.Skill.Save(ctx, skill); err != nil {
		t.Fatal(err)
	}

	profiles, skillCatalog := finishOwnerProfessionResources(t)
	definition := Definition{
		ID: 500, Path: "n_quest/change.qst", Type: "[meet npc]", LevelMin: 1, LevelMax: 99,
		Difficulty: "N", JobChangeQuest: 1, RewardType: "[grow type]", RewardIntData: []int64{2},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID,
		Multiplier: 1, CommittedAt: time.Date(2026, 7, 17, 1, 1, 0, 0, time.UTC),
		Progression: finishOwnerProgression(t), ProfessionProfiles: profiles, SkillCatalog: skillCatalog,
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "quest-finish/19/500/" + fmt.Sprint(acceptedAt.UnixNano())
	if !first.AtomicCommitted || first.Replayed || !first.HasProfession || first.CompletionKey != wantKey || first.Profession.NewGrowType != 0x02 {
		t.Fatalf("profession result = %+v", first)
	}
	if first.PostCommitCharacter.Stats["grow_type"] != 0x02 || first.PostCommitSkill.Skills[1].Level != 1 ||
		first.PostCommitSkill.Skills[2].Level != 1 || first.PostCommitSkill.Skills[197].Level != 1 ||
		len(first.PostCommitSkill.Skills) != 3 {
		t.Fatalf("profession aggregate = character=%+v skill=%+v", first.PostCommitCharacter, first.PostCommitSkill)
	}
	alternate, found := dnfrepo.SkillTree(first.PostCommitSkill, 1)
	if !found || len(alternate.Skills) != 3 || alternate.Skills[1].Level != 1 ||
		alternate.Skills[2].Level != 1 || alternate.Skills[197].Level != 1 ||
		alternate.Points != first.PostCommitSkill.Points {
		t.Fatalf("profession alternate tree = %+v found=%t", alternate, found)
	}
	wantGrants := []dnfprofession.Grant{{SkillID: 1, Level: 1}, {SkillID: 2, Level: 1}, {SkillID: 197, Level: 1}}
	if !reflect.DeepEqual(first.ProfessionGrants, wantGrants) {
		t.Fatalf("profession grants = %+v want=%+v", first.ProfessionGrants, wantGrants)
	}
	if first.PostCommitSkill.Points.RemainingSP != first.PostCommitSkill.Points.TotalSP || first.PostCommitSkill.Points.RemainingTP != first.PostCommitSkill.Points.TotalTP {
		t.Fatalf("class change points were not reset = %+v", first.PostCommitSkill.Points)
	}
	committedSkill := dnfrepo.CloneSkill(first.PostCommitSkill)
	persistedQuest, found, err := repos.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load persisted profession quest found=%t err=%v", found, err)
	}
	state := persistedQuest.States[definition.ID]
	if state.Status != "completed" || state.Extra["completion_kind"] != "active_trigger_zero_op34" || state.Extra["completion_key"] != wantKey || state.Extra[finishSettlementReceiptKey] == "" {
		t.Fatalf("persisted profession quest = %+v", state)
	}
	persistedSkill, found, err := repos.Skill.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load persisted profession skill found=%t err=%v", found, err)
	}
	persistedAlternate, found := dnfrepo.SkillTree(persistedSkill, 1)
	if !found || !reflect.DeepEqual(persistedAlternate, alternate) {
		t.Fatalf("persisted profession alternate tree=%+v found=%t want=%+v", persistedAlternate, found, alternate)
	}

	second, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.CompletionKey != first.CompletionKey || second.Profession != first.Profession ||
		!reflect.DeepEqual(second.ProfessionGrants, wantGrants) || !reflect.DeepEqual(second.PostCommitSkill, committedSkill) {
		t.Fatalf("profession replay = %+v", second)
	}
}

func TestOwnerApplyFinishSettlementCommitsExpertJobAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		2710: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt},
	}}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 2710, Path: "n_quest/Expertjob/normal_60_Expert_job_disjointer_2.qst", Type: "[seeking]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N", JobChangeQuest: 20,
		RewardType: "[expert job]", RewardIntData: []int64{3},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 22, 2, 1, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AtomicCommitted || first.Replayed || !first.HasExpertJob || first.ExpertJobType != 3 || first.HasProfession {
		t.Fatalf("expert-job result = %+v", first)
	}
	if got := first.PostCommitCharacter.Stats["expert_job_type"]; got != 3 {
		t.Fatalf("expert job persisted type = %d, want 3", got)
	}
	second, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || !second.HasExpertJob || second.ExpertJobType != 3 {
		t.Fatalf("expert-job replay = %+v", second)
	}
}

func TestOwnerApplyFinishSettlementRejectsOutOfRangePackedGrowType(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	character, _, _ := repos.Character.Load(ctx, "19")
	character.Stats["grow_type"] = 256
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	quests := dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		500: {Status: "active", ProgressValue: 0, UpdatedAt: time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)},
	}}
	if err := repos.Quest.Save(ctx, quests); err != nil {
		t.Fatal(err)
	}
	profiles, skillCatalog := finishOwnerProfessionResources(t)
	definition := Definition{
		ID: 500, Path: "n_quest/change.qst", Type: "[meet npc]", LevelMin: 1, LevelMax: 99,
		Difficulty: "N", JobChangeQuest: 1, RewardType: "[grow type]", RewardIntData: []int64{2},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyFinishSettlement(ctx, &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: definition.ID, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 17, 1, 1, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
		ProfessionProfiles: profiles, SkillCatalog: skillCatalog,
	})
	if !errors.Is(err, ErrQuestNotAcceptable) {
		t.Fatalf("out-of-range grow_type error = %v", err)
	}
	persisted, _, _ := repos.Quest.Load(ctx, "19")
	state := persisted.States[definition.ID]
	if state.Status != "active" || state.Extra[finishSettlementReceiptKey] != "" {
		t.Fatalf("invalid grow_type mutated quest = %+v", state)
	}
}

func finishOwnerSeed(t *testing.T, ctx context.Context) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	for _, save := range []func() error{
		func() error {
			return repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "acc", Job: "2", Level: 1, Stats: map[string]int64{"exp": 0, "grow_type": 0}})
		},
		func() error {
			return repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 0, Extra: map[string]string{"completion_key": "run-19/op117/426", "completion_kind": "clear_map", "reward_state": "pending"}}}})
		},
		func() error {
			return repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "19", Points: dnfrepo.SkillPointState{SyncedLevel: 1}})
		},
		func() error {
			return repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}})
		},
		func() error { return repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19"}) },
	} {
		if err := save(); err != nil {
			t.Fatal(err)
		}
	}
	return repos
}

func finishOwnerCatalog() *Catalog {
	definition := Definition{ID: 3145, Path: "n_quest/finish.qst", Type: "[clear map]", LevelMin: 1, LevelMax: 99, Difficulty: "N", RewardType: "[item]", HasGoldReward: true, RewardItems: []RewardItemRule{{ItemID: 10403, Count: 2}}}
	return &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{3145: definition}}
}

func finishOwnerInput(tables *progression.Tables, allocator FinishItemAllocator) FinishCommitInput {
	return FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 3145, RewardSelectIndex: ^uint16(0),
		Multiplier: 1, ExpectedCompletionKey: "run-19/op117/426",
		CommittedAt: time.Date(2026, 7, 16, 6, 30, 0, 0, time.UTC), Progression: tables, AllocateItem: allocator,
	}
}

func finishOwnerAllocator(fail error) FinishItemAllocator {
	return func(record *dnfrepo.InventoryRecord, request FinishItemGrantRequest) (FinishCommittedItem, error) {
		if record.Slots == nil {
			record.Slots = make(map[string]dnfrepo.ItemStack)
		}
		const slot = uint16(65)
		const key = "0:65"
		postCount := request.Count
		if existing, ok := record.Slots[key]; ok {
			postCount += existing.Count
		}
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint16(raw[0:2], slot)
		binary.LittleEndian.PutUint32(raw[2:6], uint32(request.ItemID))
		binary.LittleEndian.PutUint32(raw[6:10], uint32(postCount))
		record.Slots[key] = dnfrepo.ItemStack{ItemID: request.ItemID, Count: postCount, RawEntry: append([]byte(nil), raw...)}
		if fail != nil {
			return FinishCommittedItem{}, fail
		}
		return FinishCommittedItem{SlotKey: key, SlotIndex: slot, ItemID: request.ItemID, Delta: request.Count, PostCount: postCount, CountOrSeed: uint32(request.Count), RawEntry: raw}, nil
	}
}

type finishProgressionSource map[string]string

func (s finishProgressionSource) ReadText(path string) (string, error) {
	text, ok := s[path]
	if !ok {
		return "", fmt.Errorf("missing %s", path)
	}
	return text, nil
}

func finishOwnerProfessionResources(t *testing.T) (*dnfprofession.Profiles, *dnfskill.Table) {
	t.Helper()
	source := finishProgressionSource{
		dnfprofession.DefaultCharacterList: "2 `Gunner/Gunner.chr`\n",
		"character/Gunner/Gunner.chr": `[initial value]
[skill]
1 1
[/skill]
[growtype 3]
[skill]
2 1
197 1
[/skill]
`,
		dnfskill.DefaultList:       "2 `GunnerSkill.lst`\n",
		"skill/GunnerSkill.lst":    "1 `Gunner/a.skl` 2 `Gunner/b.skl` 197 `Gunner/mastery.skl`\n",
		"skill/Gunner/a.skl":       "[name]\n`a`\n[type]\n`[active]`\n",
		"skill/Gunner/b.skl":       "[name]\n`b`\n[type]\n`[passive]`\n",
		"skill/Gunner/mastery.skl": "[name]\n`mastery`\n[type]\n`[passive]`\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := dnfprofession.LoadProfiles(context.Background(), source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return profiles, catalog
}

func finishOwnerProgression(t *testing.T) *progression.Tables {
	t.Helper()
	tables, err := progression.Load(context.Background(), finishProgressionSource{
		progression.ExperienceTablePath: "100 250 500\n",
		progression.SkillPointTablePath: "[sp table]\n1 0\n2 30\n3 30\n4 40\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n",
		progression.QuestParameterPath:  "[difficulty]\n`N` 100\n[/difficulty]\n[exp reward table]\n100 -1\n200 -1\n300 -1\n[gold reward table]\n0 10\n[green level penalty]\n80\n[grey level penalty]\n30\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

func finishOwnerLevel60GoldProgression(t *testing.T) *progression.Tables {
	t.Helper()
	goldRows := make([]string, 61)
	for index := range goldRows {
		goldRows[index] = "0"
	}
	goldRows[60] = "4428"
	tables, err := progression.Load(context.Background(), finishProgressionSource{
		progression.ExperienceTablePath: "100\n",
		progression.SkillPointTablePath: "[sp table]\n1 0\n[/sp table]\n[tp table]\n1 0\n[/tp table]\n",
		progression.QuestParameterPath:  "[difficulty]\n`N` 100\n[/difficulty]\n[exp reward table]\n100 -1\n[gold reward table]\n" + strings.Join(goldRows, " ") + "\n[green level penalty]\n80\n[grey level penalty]\n12\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

func TestOwnerApplyFinishSettlementCommitsSlotExpansionAndReplays(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	acceptedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		649: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt, Extra: map[string]string{"completion_key": "run-19/op117/649", "completion_kind": "clear_map", "reward_state": "pending"}},
	}}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 649, Path: "Gent/NightAssault/epic_60_NightAssault_second_2.qst", Type: "[clear map]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N",
		RewardType: "[slot expansion]", RewardIntData: []int64{0},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 649, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 22, 10, 1, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	}
	first, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AtomicCommitted || first.Replayed || !first.HasSlotExpansion ||
		first.SlotExpansionIndex != 0 || first.SlotExpansionBit != ExEquipSlotSupport {
		t.Fatalf("slot expansion result = committed=%t replayed=%t has=%t index=%d bit=%d", first.AtomicCommitted, first.Replayed, first.HasSlotExpansion, first.SlotExpansionIndex, first.SlotExpansionBit)
	}
	if got := first.PostCommitCharacter.Stats["ex_equip_slot_stat"]; got != int64(ExEquipSlotSupport) {
		t.Fatalf("ex_equip_slot_stat = %d, want %d", got, ExEquipSlotSupport)
	}
	// Replay returns the receipt without granting twice.
	second, err := owner.ApplyFinishSettlement(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || !second.HasSlotExpansion ||
		second.SlotExpansionIndex != 0 || second.SlotExpansionBit != ExEquipSlotSupport {
		t.Fatalf("slot expansion replay = %+v", second)
	}
}

func TestOwnerApplyFinishSettlementSlotExpansionORsExistingBits(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	// Pre-set support bit already unlocked.
	character, _, _ := repos.Character.Load(ctx, "19")
	character.Stats["ex_equip_slot_stat"] = int64(ExEquipSlotSupport)
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	acceptedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		650: {Status: "active", ProgressValue: 0, UpdatedAt: acceptedAt, Extra: map[string]string{"completion_key": "run-19/op117/650", "completion_kind": "clear_map", "reward_state": "pending"}},
	}}); err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		ID: 650, Path: "LuftHafen/pirateonthetrain/epic_65_pirateonthetrain_magic_stone_2.qst", Type: "[clear map]",
		LevelMin: 1, LevelMax: 99, Difficulty: "N",
		RewardType: "[slot expansion]", RewardIntData: []int64{1},
	}
	catalog := &Catalog{ordered: []Definition{definition}, byID: map[int64]Definition{definition.ID: definition}}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyFinishSettlement(ctx, catalog, FinishCommitInput{
		AccountID: "acc", CharacterID: "19", QuestID: 650, Multiplier: 1,
		CommittedAt: time.Date(2026, 7, 22, 10, 1, 0, 0, time.UTC), Progression: finishOwnerProgression(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasSlotExpansion || result.SlotExpansionIndex != 1 || result.SlotExpansionBit != ExEquipSlotMagicStone {
		t.Fatalf("magic stone expansion = %+v", result)
	}
	want := int64(ExEquipSlotSupport | ExEquipSlotMagicStone)
	if got := result.PostCommitCharacter.Stats["ex_equip_slot_stat"]; got != want {
		t.Fatalf("ex_equip_slot_stat = %d, want %d (support|magicstone)", got, want)
	}
}
