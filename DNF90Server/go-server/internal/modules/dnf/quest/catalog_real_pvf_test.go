package quest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFStoryAwakeningAlias(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify story awakening aliases")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	variants := catalog.storyAwakeningByCanonical[3367]
	if !slices.Contains(variants, int64(3371)) {
		t.Fatalf("story alias q3367=%v, want q3371", variants)
	}
	if _, projected := catalog.storyAwakeningCanonicalFor[3371]; !projected {
		t.Fatal("q3371 is not registered as a hidden reward companion")
	}
	variant, found := catalog.Find(3371)
	if !found {
		t.Fatal("q3371 definition is missing")
	}
	character := CharacterEligibility{Level: variant.LevelMin, Job: 0, GrowType: variant.GrowType}
	plan, err := catalog.PlanFinishReward(character, 3367, ^uint16(0), false)
	if err != nil || !plan.HasProfession {
		t.Fatalf("canonical q3367 reward plan=%+v err=%v", plan, err)
	}
	if !catalog.FinishRewardNeedsProfessionResources(dnfrepo.QuestRecord{}, character, 3367, ^uint16(0), false) {
		t.Fatal("real canonical awakening reward did not require profession resources")
	}
}

func TestRealScriptPVFQuest1680AcceptanceEventItems(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify quest 1680 acceptance items")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(1680)
	if !found || !definition.HasDependGiveItem {
		t.Fatalf("quest 1680 definition=%+v found=%t", definition, found)
	}
	rules, err := acceptEventItemRules(definition)
	if err != nil || len(rules) == 0 {
		t.Fatalf("quest 1680 event items=%+v err=%v", rules, err)
	}
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 {
			t.Fatalf("quest 1680 invalid event item=%+v", rule)
		}
	}
}

func TestRealScriptPVFPeaceWingStoryBookQuest3142AcceptMutation(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify story-book quest 3142")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.QuestRecord{CharacterID: "77"}
	acceptedAt := time.Unix(123, 0).UTC()
	plan, err := catalog.PlanAcceptMutation(CharacterEligibility{Level: 90, Job: 11, GrowType: 33}, record, 3142, acceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	state, found := plan.Quest.States[3142]
	if plan.QuestID != 3142 || len(plan.QuestFields) != 1 || plan.QuestFields[0] != dnfrepo.QuestFieldStates ||
		!found || state.Status != "active" || state.ProgressValue != int64(plan.InitTrigger) || !state.UpdatedAt.Equal(acceptedAt) {
		t.Fatalf("story-book mutation plan=%+v state=%+v found=%t", plan, state, found)
	}
}

// Time Rift is unlocked by a normal quest settlement, not by a separate
// synthetic permission row. Keep the exact live PVF reward shape covered so
// the finish owner can persist each level-band quest's terminal state.
func TestRealScriptPVFTimeRiftInfernoChallengeQuest(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify Time Rift quest 10104")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		questID   int64
		dungeonID int64
	}{
		{questID: 10104, dungeonID: 7527},
		{questID: 10110, dungeonID: 7539},
	} {
		definition, found := catalog.Find(want.questID)
		if !found {
			t.Fatalf("Time Rift quest=%d definition is missing", want.questID)
		}
		plan, err := catalog.PlanFinishReward(
			CharacterEligibility{Level: definition.LevelMin, Job: 0, GrowType: definition.GrowType},
			want.questID,
			0,
			false,
		)
		if err != nil || plan.InfernoChallengeDungeonID != want.dungeonID {
			t.Fatalf("Time Rift quest=%d definition=%+v challenge reward plan=%+v err=%v", want.questID, definition, plan, err)
		}
	}
}

func TestRealScriptPVFMainContinuityTransitions(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify main continuity")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := dnfprogression.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for questID, want := range map[int64]struct {
		level     int
		threshold uint32
	}{
		3148: {level: 11, threshold: 80875},
		3149: {level: 13, threshold: 164460},
		3150: {level: 15, threshold: 303835},
		3520: {level: 64, threshold: 212607303},
		3524: {level: 65, threshold: 228359324},
		3527: {level: 66, threshold: 244631516},
		3533: {level: 67, threshold: 261440391},
		3538: {level: 68, threshold: 278802719},
		3543: {level: 69, threshold: 296735528},
		3548: {level: 70, threshold: 315256104},
	} {
		definition, found := catalog.Find(questID)
		if !found {
			t.Fatalf("quest %d missing", questID)
		}
		record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{questID: {Status: "active"}}}
		level, ok := catalog.MainContinuityLevelFloor(
			CharacterEligibility{Level: definition.LevelMin, Job: 0, GrowType: 0},
			record,
			questID,
		)
		if !ok || level != want.level {
			t.Fatalf("quest %d floor=(%d,%t), want (%d,true)", questID, level, ok, want.level)
		}
		threshold, err := tables.ThresholdToNext(level - 1)
		if err != nil || threshold != want.threshold {
			t.Fatalf("quest %d threshold=(%d,%v), want %d", questID, threshold, err, want.threshold)
		}
	}
}

func TestRealScriptPVFExpertJobTransitionQuestDirectory(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify expert-job transition quests")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	got := catalog.ExpertJobTransitionQuestIDs()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []int64{2702, 2708, 2710, 2712, 10020, 10021, 11007, 11009, 11013, 11014, 11015, 11016, 11017, 11018, 11019}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expert-job transition quests=%v want=%v", got, want)
	}
	terminal := catalog.ExpertJobTransitionTerminalQuestIDs()
	sort.Slice(terminal, func(i, j int) bool { return terminal[i] < terminal[j] })
	if wantTerminal := []int64{10021, 11009, 11015, 11018}; !reflect.DeepEqual(terminal, wantTerminal) {
		t.Fatalf("expert-job terminal transition quests=%v want=%v", terminal, wantTerminal)
	}
}

// 副职业终局任务必须是"可放弃且放弃时无东西要回收"的：
// 它们既没有 `[cant giveup]`，也不该带 `[depend give item]` 事件道具
// （`[seeking]` 之类的收集需求现在不再拦放弃，普通材料本来就不回收）。
func TestRealScriptPVFExpertJobTerminalQuestsCanUseNoAssetGiveUpOwner(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify expert-job terminal give-up rules")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.QuestRecord{CharacterID: "18", States: map[int64]dnfrepo.QuestState{}}
	for _, questID := range []int64{10021, 11009, 11015, 11018} {
		definition, ok := catalog.Find(questID)
		if !ok {
			t.Fatalf("expert-job terminal quest %d is missing", questID)
		}
		if definition.CantGiveUp {
			t.Fatalf("expert-job terminal quest %d cannot be given up: type=%q", questID, definition.Type)
		}
		record.States[questID] = dnfrepo.QuestState{Status: "active"}
		if plan := PlanGiveUpReclaim(catalog, record, questID, nil); len(plan) != 0 {
			t.Fatalf("expert-job terminal quest %d reclaims items on give-up: %+v", questID, plan)
		}
		delete(record.States, questID)
	}
}

func TestRealScriptPVFLevelOneGunnerHasAcceptableQuest(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the real quest catalog")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := catalog.Snapshot()
	if snapshot.Definitions == 0 || snapshot.Epic == 0 {
		t.Fatalf("real quest catalog snapshot=%+v", snapshot)
	}
	result := catalog.Acceptable(CharacterEligibility{Level: 1, Job: 2, GrowType: 0}, dnfrepo.QuestRecord{})
	if len(result.IDs) == 0 || result.EpicCount == 0 {
		logged := 0
		for _, definition := range catalog.ordered {
			if definition.LevelMin > 1 || definition.LevelMax < 1 {
				continue
			}
			t.Logf("level1 candidate id=%d grade=%q level=%d..%d job=%q target=%q grow=%d/%t change=%d exposed=%d event=%t creature=%t expert=%t pre=%v answer=%v collision=%v",
				definition.ID, definition.Grade, definition.LevelMin, definition.LevelMax, definition.Job, definition.TargetCharacter,
				definition.GrowType, definition.HasGrowType, definition.JobChangeQuest, definition.ExposedByNPC, definition.IsEvent,
				definition.HasCreatureRequirement, definition.HasExpertRequirement, definition.PreRequiredGroups,
				definition.PreRequiredAnswers, definition.CollisionQuests)
			logged++
			if logged == 20 {
				break
			}
		}
		t.Fatalf("level-one gunner acceptable=%d epic=%d catalog=%+v", len(result.IDs), result.EpicCount, snapshot)
	}
	for _, questID := range result.IDs {
		definition, ok := catalog.Find(int64(questID))
		if ok && normalizeQuestTag(definition.Grade) == "epic" {
			t.Logf("level-one-gunner epic id=%d path=%q", questID, definition.Path)
		}
	}
	t.Logf("quest catalog=%+v level-one-gunner acceptable=%d epic=%d ids=%v", snapshot, len(result.IDs), result.EpicCount, result.IDs)
}

func TestRealScriptPVFAntonRaidMissionIsAcceptableWithFivePerPhase(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify Anton raid mission 12830")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.Find(12830)
	if !ok || normalizeQuestTag(definition.Grade) != "mission" ||
		normalizeQuestTag(definition.Type) != "raid phase clear" ||
		!reflect.DeepEqual(definition.PreRequiredGroups, [][]int64{{12829}}) ||
		!reflect.DeepEqual(definition.IntData, []int64{0, 0, 5, -1, 1, 0, 5, -1}) {
		t.Fatalf("Anton raid mission definition=%+v found=%t", definition, ok)
	}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{12829: {Status: "completed"}}}
	result := catalog.Acceptable(CharacterEligibility{Level: 90, Job: 2}, record)
	if !containsQuestID(result.IDs, 12830) {
		t.Fatalf("acceptable Anton raid missions=%v want 12830", result.IDs)
	}
	plan, err := catalog.PlanAccept(CharacterEligibility{Level: 90, Job: 2}, record, 12830)
	if err != nil {
		t.Fatal(err)
	}
	if want := packTriggerChannels(5, 5); plan.InitTrigger != want {
		t.Fatalf("real Anton mission init trigger=%#x want=%#x", plan.InitTrigger, want)
	}
}

func TestTraceRealScriptPVFQuest2616(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect quest 2616")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.Find(2616)
	if !ok {
		t.Fatal("quest 2616 is missing")
	}
	t.Logf("quest=2616 path=%q grade=%q type=%q subtype=%d level=%d..%d job=%q target=%q target_rules=%+v grow=%d/%t change=%d exposed=%d event=%t creature=%t expert=%t pre=%v answer=%v collision=%v int=%v check=%v condition=%v reward=%q/%v",
		definition.Path, definition.Grade, definition.Type, definition.SubType, definition.LevelMin, definition.LevelMax,
		definition.Job, definition.TargetCharacter, definition.TargetCharacterRules, definition.GrowType, definition.HasGrowType,
		definition.JobChangeQuest, definition.ExposedByNPC, definition.IsEvent, definition.HasCreatureRequirement,
		definition.HasExpertRequirement, definition.PreRequiredGroups, definition.PreRequiredAnswers, definition.CollisionQuests,
		definition.IntData, definition.CheckCount, definition.ConditionData, definition.RewardType, definition.RewardIntData)
}

func TestRealScriptPVFQuest4728PreservesDuplicateEquipmentRewards(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify quest 4728 duplicate equipment rewards")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	reward, err := catalog.PlanFinishReward(CharacterEligibility{Level: 15, Job: 11, GrowType: 4}, 4728, ^uint16(0), false)
	if err != nil {
		t.Fatal(err)
	}
	var duplicateCount int
	for _, item := range reward.Items {
		if item.ItemID == 101040191 && item.Count == 1 {
			duplicateCount++
		}
	}
	if duplicateCount != 2 {
		t.Fatalf("quest 4728 rewards=%+v, want two independent item 101040191 entries", reward.Items)
	}
}

func TestRealScriptPVFQuest3145HasTypedAcceptPlan(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify quest 3145")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.Find(3145)
	if !ok {
		t.Fatal("real PVF quest 3145 missing")
	}
	if normalizeQuestTag(definition.Type) != "clear map" || len(definition.IntData) != 1 || definition.IntData[0] != 76126 || definition.HasDependGiveItem {
		t.Fatalf("quest 3145 definition = %+v", definition)
	}
	plan, err := catalog.PlanAccept(CharacterEligibility{Level: 1, Job: 2, GrowType: 0}, dnfrepo.QuestRecord{}, 3145)
	if err != nil {
		t.Fatalf("quest 3145 PlanAccept error = %v", err)
	}
	if plan.InitTrigger != 1 {
		t.Fatalf("quest 3145 init trigger = %d, want 1", plan.InitTrigger)
	}

	activeRecord := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3145: {Status: "active", ProgressValue: 1},
	}}
	acceptable := catalog.Acceptable(CharacterEligibility{Level: 1, Job: 2, GrowType: 0}, activeRecord)
	questList := catalog.QuestList(CharacterEligibility{Level: 1, Job: 2, GrowType: 0}, activeRecord)
	acceptableHas3145 := false
	questListHas3145 := false
	for _, questID := range acceptable.IDs {
		acceptableHas3145 = acceptableHas3145 || questID == 3145
	}
	for _, questID := range questList.IDs {
		questListHas3145 = questListHas3145 || questID == 3145
	}
	if acceptableHas3145 || !questListHas3145 || questList.ActiveCount != 1 {
		t.Fatalf("active 3145 visibility acceptable=%v list=%v active_count=%d", acceptable.IDs, questList.IDs, questList.ActiveCount)
	}
	reward, err := catalog.PlanFinishReward(CharacterEligibility{Level: 1, Job: 2, GrowType: 0}, 3145, ^uint16(0), false)
	if err != nil {
		t.Fatalf("quest 3145 PlanFinishReward error = %v", err)
	}
	want := []int64{10165055, 104010230, 10403, 12403, 14403, 16403, 18403}
	if len(reward.Items) != len(want) || reward.Difficulty != '1' {
		t.Fatalf("quest 3145 finish reward = %+v, want ids=%v difficulty=1", reward, want)
	}
	for index, itemID := range want {
		if reward.Items[index].ItemID != itemID || reward.Items[index].Count != 1 {
			t.Fatalf("quest 3145 reward[%d] = %+v, want item=%d count=1", index, reward.Items[index], itemID)
		}
	}
}

func TestRealScriptPVFQuest3249AcceptsTerminalZeroTriggerLinkedSubQuest(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify quest 3249")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	parent, parentKnown := catalog.Find(3249)
	first, firstKnown := catalog.Find(3609)
	terminal, terminalKnown := catalog.Find(3610)
	if !parentKnown || !firstKnown || !terminalKnown || normalizeQuestTag(parent.Type) != "quest clear" ||
		!reflect.DeepEqual(parent.IntData, []int64{3609, 3610}) || first.MainQuestID != 3249 || terminal.MainQuestID != 3249 {
		t.Fatalf("quest graph parent=%+v/%t first=%+v/%t terminal=%+v/%t", parent, parentKnown, first, firstKnown, terminal, terminalKnown)
	}
	record := dnfrepo.QuestRecord{
		CharacterID: "2",
		States: map[int64]dnfrepo.QuestState{
			3249: {Status: "active", ProgressValue: 0},
			3609: {Status: "completed", ProgressValue: 0},
			3610: {Status: "active", ProgressValue: 0},
		},
	}
	trigger, changedFields, known := finishQuestClearTrigger(catalog, &record, 3249, time.Date(2026, 7, 29, 17, 50, 0, 0, time.UTC))
	if !known || trigger != 0 || !reflect.DeepEqual(changedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("terminal linked result trigger=%d fields=%v known=%t", trigger, changedFields, known)
	}
	state := record.States[3610]
	if state.Status != "completed" || state.Extra["auto_completed_by_parent"] != "3249" ||
		state.Extra["auto_complete_reason"] != "quest_clear_parent_terminal_zero_trigger" {
		t.Fatalf("terminal linked state=%+v", state)
	}
	autoPlan, err := catalog.PlanAutoCompleteMainQuests(
		dnfrepo.QuestRecord{
			CharacterID: "2",
			States: map[int64]dnfrepo.QuestState{
				3248: {Status: "completed"},
				3249: {Status: "active"},
			},
		},
		CharacterEligibility{Level: 90, Job: 0},
		89,
		3249,
		time.Date(2026, 8, 25, 15, 45, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("quest 3249 auto-complete closure: %v", err)
	}
	if !reflect.DeepEqual(autoPlan.ChangedLinkedSubIDs, []int64{3609, 3610}) {
		t.Fatalf("quest 3249 auto-complete children=%v", autoPlan.ChangedLinkedSubIDs)
	}
	for _, childID := range []int64{3609, 3610} {
		childState := autoPlan.Record.States[childID]
		if childState.Status != "completed" || childState.Extra["auto_completed_by_parent"] != "3249" {
			t.Fatalf("quest 3249 auto-complete child %d state=%+v", childID, childState)
		}
	}
}

func TestRealScriptPVFQuest2635RepairsLegacySaturatedTrigger(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify quest 2635 trigger repair")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.Find(2635)
	if !ok || normalizeQuestTag(definition.Type) != "hunt monster" {
		t.Fatalf("real PVF quest 2635 definition=%+v found=%t", definition, ok)
	}
	plan, err := catalog.PlanLegacySaturatedActiveTriggerRepair(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			2635: {Status: "active", ProgressValue: 511},
		},
	}, time.Date(2026, 7, 23, 2, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repairs) != 1 || plan.Record.States[2635].ProgressValue != 0 {
		t.Fatalf("real PVF quest 2635 repair plan=%+v definition=%+v", plan, definition)
	}
}

// TestRealScriptPVFQuest3146Definition records the first post-3145 main-line
// quest from the runtime PVF.  It protects the generic C#-equivalent initial
// trigger rules from silently treating this quest as an unsupported type.
func TestRealScriptPVFQuest3146Definition(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect quest 3146")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.Find(3146)
	if !ok {
		t.Fatal("real PVF quest 3146 missing")
	}
	t.Logf("quest 3146 path=%q grade=%q type=%q subtype=%d int_data=%v depend_give=%t/%v pre=%v reward=%q items=%v",
		definition.Path, definition.Grade, definition.Type, definition.SubType, definition.IntData,
		definition.HasDependGiveItem, definition.DependGiveItemData, definition.PreRequiredGroups,
		definition.RewardType, definition.RewardItems)
	plan, err := catalog.PlanAccept(CharacterEligibility{Level: 90, Job: 11, GrowType: 0}, dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3145: {Status: "completed"},
	}}, 3146)
	if err != nil {
		t.Fatalf("quest 3146 PlanAccept error = %v", err)
	}
	if plan.InitTrigger != 2 {
		t.Fatalf("quest 3146 init trigger = %d, want two outstanding PVF quest-clear prerequisites", plan.InitTrigger)
	}
	if len(plan.LinkedSubQuests) != 2 ||
		plan.LinkedSubQuests[0].QuestID != 3157 ||
		plan.LinkedSubQuests[1].QuestID != 3054 {
		t.Fatalf("quest 3146 linked subtasks = %+v, want 3157 and 3054", plan.LinkedSubQuests)
	}
	hunt, ok := catalog.Find(3157)
	if !ok {
		t.Fatal("real PVF quest 3157 missing")
	}
	if normalizeQuestTag(hunt.Grade) != "sub" || normalizeQuestTag(hunt.Type) != "hunt enemy" ||
		hunt.MainQuestID != 3146 ||
		len(hunt.IntData) != 5 || hunt.IntData[0] != 3 || hunt.IntData[1] != -1 ||
		hunt.IntData[2] != 13099 || hunt.IntData[3] != 3 || hunt.IntData[4] != 1 ||
		normalizeQuestTag(hunt.RewardType) != "" || !emptyRewardDataValid(hunt.RewardIntData) {
		t.Fatalf("quest 3157 definition = %+v", hunt)
	}
	clear, ok := catalog.Find(3054)
	if !ok {
		t.Fatal("real PVF quest 3054 missing")
	}
	if normalizeQuestTag(clear.Grade) != "sub" || normalizeQuestTag(clear.Type) != "clear map" ||
		clear.MainQuestID != 3146 ||
		len(clear.IntData) != 1 || clear.IntData[0] != 76136 ||
		normalizeQuestTag(clear.RewardType) != "" || !emptyRewardDataValid(clear.RewardIntData) {
		t.Fatalf("quest 3054 definition = %+v", clear)
	}
	huntPlan, err := catalog.PlanHuntEnemyKill(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3157: {Status: "active", ProgressValue: 1},
			3054: {Status: "completed", ProgressValue: 0},
			3146: {Status: "active", ProgressValue: 1},
		},
	}, HuntEnemyKillInput{
		DungeonID:     3,
		Difficulty:    0,
		EnemyCode:     13099,
		EnemyType:     3,
		CompletionKey: "real-pvf-3157",
		CompletedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("quest 3157 PlanHuntEnemyKill error = %v", err)
	}
	if huntPlan.Record.States[3157].Status != "completed" ||
		huntPlan.Record.States[3146].ProgressValue != 0 {
		t.Fatalf("real PVF 3157/3146 plan = %+v", huntPlan.Record.States)
	}
	clearPlan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3157: {Status: "completed", ProgressValue: 0},
			3146: {Status: "active", ProgressValue: 2},
		},
	}, ClearMapCompletionInput{
		DungeonID:     3,
		MapID:         76136,
		CompletionKey: "real-pvf-3054-stale-parent-only",
		CompletedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("quest 3054 PlanClearMapCompletion error = %v", err)
	}
	// The terminal linked child stays active at trigger zero until the current
	// client emits its native parent-finish request. Completing it here would
	// collapse quest 3146 before the finish dialogue can run.
	if clearPlan.Record.States[3054].Status != "active" ||
		clearPlan.Record.States[3054].ProgressValue != 0 ||
		clearPlan.Record.States[3054].Extra["auto_activated_by_main_quest"] != "true" ||
		clearPlan.Record.States[3146].ProgressValue != 1 {
		t.Fatalf("real PVF 3054/3146 plan = %+v", clearPlan.Record.States)
	}
}

func TestRealScriptPVFProfessionQuestRewardsAndBranchZeroChains(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify profession quest chains")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	var classRewards, firstAwakeningRewards, secondAwakeningRewards, firstAwakeningEmpty int
	firstAwakeningRewardTypes := make(map[string]int)
	firstAwakeningEmptyDetails := make([]string, 0, 17)
	jobChangeQuestCounts := make(map[int]int)
	jobChangeAnswerIDs := make(map[int][]int64)
	for _, definition := range catalog.ordered {
		rewardType := normalizeQuestTag(definition.RewardType)
		if definition.JobChangeQuest != 0 {
			jobChangeQuestCounts[definition.JobChangeQuest]++
			if len(definition.PreRequiredAnswers) != 0 {
				jobChangeAnswerIDs[definition.JobChangeQuest] = append(jobChangeAnswerIDs[definition.JobChangeQuest], definition.ID)
			}
		}
		if definition.JobChangeQuest == 2 {
			firstAwakeningRewardTypes[rewardType]++
			if rewardType == "" {
				firstAwakeningEmptyDetails = append(firstAwakeningEmptyDetails, fmt.Sprintf("%d:%v", definition.ID, definition.RewardIntData))
			}
		}
		switch {
		case definition.JobChangeQuest == 1 && rewardType == "grow type":
			classRewards++
			if len(definition.RewardIntData) == 0 || definition.RewardIntData[0] < 1 || definition.RewardIntData[0] > 5 {
				t.Fatalf("class reward quest=%d data=%v", definition.ID, definition.RewardIntData)
			}
		case definition.JobChangeQuest == 2 && rewardType == "awakening type":
			firstAwakeningRewards++
			if len(definition.RewardIntData) == 0 || definition.RewardIntData[0] != 1 {
				t.Fatalf("first awakening quest=%d data=%v", definition.ID, definition.RewardIntData)
			}
		case definition.JobChangeQuest == 2 && rewardType == "" && emptyRewardDataValid(definition.RewardIntData):
			firstAwakeningEmpty++
		case definition.JobChangeQuest == 3 && rewardType == "awakening type":
			secondAwakeningRewards++
			if len(definition.RewardIntData) == 0 || definition.RewardIntData[0] != 2 {
				t.Fatalf("second awakening quest=%d data=%v", definition.ID, definition.RewardIntData)
			}
		}
	}
	if classRewards != 57 || firstAwakeningRewards != 115 || secondAwakeningRewards != 59 || firstAwakeningEmpty != 17 {
		t.Fatalf("profession reward counts class=%d first=%d second=%d first_empty=%d first_types=%v empty_details=%v", classRewards, firstAwakeningRewards, secondAwakeningRewards, firstAwakeningEmpty, firstAwakeningRewardTypes, firstAwakeningEmptyDetails)
	}
	if jobChangeQuestCounts[1] != 72 || jobChangeQuestCounts[2] != 286 || jobChangeQuestCounts[3] != 354 {
		t.Fatalf("job-change quest counts=%v", jobChangeQuestCounts)
	}
	t.Logf("job-change pre-required-answer IDs=%v", jobChangeAnswerIDs)
	unreachable := make([]string, 0)
	for _, definition := range catalog.ordered {
		if definition.JobChangeQuest < 1 || definition.JobChangeQuest > 3 || professionDefinitionHasAcceptPlan(catalog, definition) {
			continue
		}
		unreachable = append(unreachable, fmt.Sprintf("%d:%s", definition.ID, definition.Path))
	}
	if len(unreachable) != 0 {
		t.Fatalf("profession quests without any typed accept state count=%d quests=%v", len(unreachable), unreachable)
	}

	completed3427 := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{3427: {Status: "completed"}}}
	if !containsQuestID(catalog.Acceptable(CharacterEligibility{Level: 50, Job: 9, GrowType: 0x00}, completed3427).IDs, 2680) ||
		!containsQuestID(catalog.Acceptable(CharacterEligibility{Level: 50, Job: 10, GrowType: 0x00}, completed3427).IDs, 2680) {
		t.Fatal("shared branch-zero first-awakening quest 2680 is not acceptable for both PVF target tuples")
	}
	if !containsQuestID(catalog.Acceptable(CharacterEligibility{Level: 75, Job: 9, GrowType: 0x10}, dnfrepo.QuestRecord{}).IDs, 5135) ||
		!containsQuestID(catalog.Acceptable(CharacterEligibility{Level: 75, Job: 10, GrowType: 0x10}, dnfrepo.QuestRecord{}).IDs, 5139) {
		t.Fatal("branch-zero second-awakening realization entry quests are missing")
	}
	for _, test := range []struct {
		questID int64
		job     int
		grow    int
		chain   byte
		target  byte
	}{
		{questID: 6926, job: 15, grow: 0x00, chain: 1, target: 1},
		{questID: 6887, job: 15, grow: 0x01, chain: 2, target: 1},
		{questID: 6905, job: 15, grow: 0x11, chain: 2, target: 2},
		{questID: 2680, job: 9, grow: 0x00, chain: 2, target: 1},
		{questID: 5138, job: 9, grow: 0x10, chain: 2, target: 2},
		{questID: 5142, job: 10, grow: 0x10, chain: 2, target: 2},
	} {
		plan, err := catalog.PlanFinishReward(CharacterEligibility{Level: 90, Job: test.job, GrowType: test.grow}, test.questID, 0, false)
		if err != nil || !plan.HasProfession || plan.ProfessionRequest.ChainType != test.chain || plan.ProfessionRequest.GrowNumber != test.target {
			t.Fatalf("profession quest %d plan=%+v err=%v", test.questID, plan, err)
		}
	}
}

func TestRealScriptPVFExpertJobQuestRewards(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify expert-job quest chains")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}

	want := map[int64]byte{2702: 1, 2708: 2, 2710: 3, 2712: 4}
	found := make(map[int64]byte, len(want))
	for _, definition := range catalog.ordered {
		if normalizeQuestTag(definition.RewardType) != "expert job" {
			continue
		}
		jobType, expected := want[definition.ID]
		if !expected {
			t.Fatalf("unexpected expert-job reward quest id=%d path=%q data=%v", definition.ID, definition.Path, definition.RewardIntData)
		}
		plan, err := catalog.PlanFinishReward(CharacterEligibility{Level: 20, Job: 2}, definition.ID, 0, false)
		if err != nil || !plan.HasExpertJob || plan.ExpertJobType != jobType ||
			len(definition.RewardIntData) != 1 || definition.RewardIntData[0] != int64(jobType) ||
			!professionDefinitionHasAcceptPlan(catalog, definition) {
			t.Fatalf("expert-job quest id=%d definition=%+v plan=%+v err=%v", definition.ID, definition, plan, err)
		}
		found[definition.ID] = plan.ExpertJobType
	}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("expert-job reward quests=%v want=%v", found, want)
	}
}

func TestRealScriptPVFCompleted3158ExposesSuccessor3159WithoutAutoAccept(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the live 3158 -> 3159 story chain")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}

	successors := catalog.Successors(3158)
	if len(successors) != 1 || successors[0].ID != 3159 {
		t.Fatalf("quest 3158 successors=%v, want only 3159", successors)
	}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3158: {Status: "completed"},
	}}
	eligibility := CharacterEligibility{Level: 90, Job: 11, GrowType: 1}
	list := catalog.QuestList(eligibility, record)
	if !containsQuestID(list.IDs, 3159) || containsQuestID(list.IDs, 3158) {
		t.Fatalf("post-3158 quest list contains3159=%t contains3158=%t ids=%v",
			containsQuestID(list.IDs, 3159), containsQuestID(list.IDs, 3158), list.IDs)
	}
	if _, err := catalog.PlanAccept(eligibility, record, 3159); err != nil {
		t.Fatalf("quest 3159 must remain manually acceptable after 3158 completes: %v", err)
	}
	if state, exists := record.States[3159]; exists || state.Status != "" {
		t.Fatalf("successor was auto-accepted instead of remaining player-confirmed: %+v", state)
	}
}

func professionDefinitionHasAcceptPlan(catalog *Catalog, definition Definition) bool {
	groups := definition.PreRequiredGroups
	if len(groups) == 0 {
		groups = [][]int64{nil}
	}
	level := definition.LevelMin
	if level <= 0 {
		level = 1
	}
	for _, group := range groups {
		record := dnfrepo.QuestRecord{States: make(map[int64]dnfrepo.QuestState, len(group))}
		for _, required := range group {
			if required > 0 {
				record.States[required] = dnfrepo.QuestState{Status: "completed"}
			}
		}
		for job := 0; job <= 15; job++ {
			for awakening := 0; awakening <= 2; awakening++ {
				for firstGrow := 0; firstGrow <= 5; firstGrow++ {
					growType := awakening<<4 | firstGrow
					if _, err := catalog.PlanAccept(CharacterEligibility{Level: level, Job: job, GrowType: growType}, record, definition.ID); err == nil {
						return true
					}
				}
			}
		}
	}
	return false
}

func containsQuestID(ids []int32, want int32) bool {
	for _, questID := range ids {
		if questID == want {
			return true
		}
	}
	return false
}

// TestRealScriptPVFAutoCompleteMainLeavesFactionQuestion3868ToPlayer pins the
// live faction question: task-manual bulk completion must stop before it and
// exact-target completion must reject it, because its three successors are
// gated by the player's [pre required quest answer] choice.
func TestRealScriptPVFAutoCompleteMainLeavesFactionQuestion3868ToPlayer(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the 3868 faction question")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	question, found := catalog.Find(3868)
	if !found || normalizeQuestTag(question.Type) != "question" || !catalog.questionHasAnswerBranches(3868) {
		t.Fatalf("quest 3868 definition=%+v found=%t", question, found)
	}
	completedAt := time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC)
	eligibility := CharacterEligibility{Level: 90, Job: 0}
	record := dnfrepo.QuestRecord{CharacterID: "2", States: map[int64]dnfrepo.QuestState{
		3867: {Status: "completed"},
	}}
	plan, err := catalog.PlanAutoCompleteMainQuests(record, eligibility, 89, 0, completedAt)
	if err == nil {
		if _, completed := questStateFor(plan.Record, 3868); completed {
			t.Fatal("task-manual bulk completion selected the faction question 3868")
		}
	} else if !errors.Is(err, ErrAutoCompleteTargetInvalid) {
		t.Fatal(err)
	}
	record.States[3868] = dnfrepo.QuestState{Status: "active"}
	if _, err := catalog.PlanAutoCompleteMainQuests(record, eligibility, 89, 3868, completedAt); !errors.Is(err, ErrAutoCompleteTargetInvalid) {
		t.Fatalf("exact-target completion of 3868 error=%v", err)
	}
}

func TestRealScriptPVFSloughStoryDoesNotExposeLinkedChildrenBeforeParent3748(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the live 3740 -> 3748 story chain")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}

	eligibility := CharacterEligibility{Level: 85, Job: 15, GrowType: 0x12}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3740: {Status: "completed"},
	}}
	list := catalog.QuestList(eligibility, record)
	if !containsQuestID(list.IDs, 3741) || containsQuestID(list.IDs, 3748) ||
		containsQuestID(list.IDs, 3766) || containsQuestID(list.IDs, 3767) {
		t.Fatalf("post-3740 quest list contains3741=%t contains3748=%t contains3766=%t contains3767=%t",
			containsQuestID(list.IDs, 3741), containsQuestID(list.IDs, 3748),
			containsQuestID(list.IDs, 3766), containsQuestID(list.IDs, 3767))
	}
	successors := catalog.EligibleSuccessors(3740, eligibility, record)
	if len(successors) != 1 || successors[0].ID != 3741 {
		t.Fatalf("post-3740 eligible successors=%v, want only 3741", definitionIDs(successors))
	}

	for questID := int64(3741); questID <= 3747; questID++ {
		record.States[questID] = dnfrepo.QuestState{Status: "completed"}
	}
	list = catalog.QuestList(eligibility, record)
	if !containsQuestID(list.IDs, 3748) || containsQuestID(list.IDs, 3766) || containsQuestID(list.IDs, 3767) {
		t.Fatalf("post-3747 quest list contains3748=%t contains3766=%t contains3767=%t",
			containsQuestID(list.IDs, 3748), containsQuestID(list.IDs, 3766), containsQuestID(list.IDs, 3767))
	}
	plan, err := catalog.PlanAccept(eligibility, record, 3748)
	if err != nil {
		t.Fatalf("accept quest 3748 after 3747: %v", err)
	}
	if len(plan.LinkedSubQuests) != 2 || plan.LinkedSubQuests[0].QuestID != 3766 || plan.LinkedSubQuests[1].QuestID != 3767 {
		t.Fatalf("quest 3748 linked subquests=%+v, want [3766 3767]", plan.LinkedSubQuests)
	}
}
