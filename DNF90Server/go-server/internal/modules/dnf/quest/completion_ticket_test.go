package quest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestPlanCompletionTicketCoversEveryDirectPVFActionFamily(t *testing.T) {
	catalog := completionTicketTestCatalog()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		action     string
		values     []int64
		growType   int
		want       []int64
		wantGrow   int64
		profession bool
	}{
		{name: "any exact", action: "[any quest clear]", values: []int64{300}, growType: 1, want: []int64{300}, wantGrow: 1},
		{name: "achievement", action: "[achievement quest clear]", growType: 1, want: []int64{200}, wantGrow: 1},
		{name: "side", action: "[side quest clear]", growType: 1, want: []int64{200, 201}, wantGrow: 1},
		{name: "main and ordinary", action: "[quest clear]", growType: 1, want: []int64{100, 101, 201, 300}, wantGrow: 1},
		{name: "gm main", action: "[gm quest clear]", growType: 1, want: []int64{100, 101}, wantGrow: 1},
		{name: "level main achievement", action: "[lv clear]", values: []int64{90}, growType: 1, want: []int64{100, 101, 200}, wantGrow: 1},
		{name: "first awakening", action: "[first awaken clear]", growType: 1, want: []int64{400}, wantGrow: 0x11, profession: true},
		{name: "second awakening", action: "[second awaken clear]", growType: 0x11, want: []int64{401}, wantGrow: 0x21, profession: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			character := dnfrepo.CharacterRecord{
				CharacterID: "77",
				Job:         "2",
				Level:       90,
				Stats:       map[string]int64{"grow_type": int64(test.growType)},
			}
			plan, err := catalog.PlanCompletionTicket(
				dnfrepo.QuestRecord{CharacterID: "77"},
				character,
				CharacterEligibility{Level: 90, Job: 2, GrowType: test.growType},
				CompletionTicketSpec{ActionType: test.action, ActionValues: test.values, MinimumLevel: 1, MaximumLevel: 90},
				now,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.CompletedQuestIDs, test.want) {
				t.Fatalf("completed=%v want=%v", plan.CompletedQuestIDs, test.want)
			}
			if plan.Character.Stats["grow_type"] != test.wantGrow || plan.ProfessionChanged != test.profession {
				t.Fatalf("character=%+v profession_changed=%t", plan.Character, plan.ProfessionChanged)
			}
			for _, questID := range test.want {
				state := plan.Quest.States[questID]
				if state.Status != "completed" || state.Extra["reward_state"] != finishRewardGranted ||
					state.Extra["auto_complete_action"] != test.action {
					t.Fatalf("quest %d state=%+v", questID, state)
				}
			}
		})
	}
}

func TestPlanCompletionTicketSelectionCouponStaysOnSelectorOwnedPath(t *testing.T) {
	catalog := completionTicketTestCatalog()
	_, err := catalog.PlanCompletionTicket(
		dnfrepo.QuestRecord{CharacterID: "77"},
		dnfrepo.CharacterRecord{CharacterID: "77", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 1}},
		CharacterEligibility{Level: 90, Job: 2, GrowType: 1},
		CompletionTicketSpec{ActionType: "[quest clear coupon]"},
		time.Now().UTC(),
	)
	if !errors.Is(err, ErrCompletionTicketSelectionRequired) {
		t.Fatalf("error=%v want selection required", err)
	}
}

func TestPlanCompletionTicketRejectsLevelBoundsWithoutMutation(t *testing.T) {
	catalog := completionTicketTestCatalog()
	_, err := catalog.PlanCompletionTicket(
		dnfrepo.QuestRecord{CharacterID: "77"},
		dnfrepo.CharacterRecord{CharacterID: "77", Job: "2", Level: 89, Stats: map[string]int64{"grow_type": 1}},
		CharacterEligibility{Level: 89, Job: 2, GrowType: 1},
		CompletionTicketSpec{ActionType: "[gm quest clear]", MinimumLevel: 90},
		time.Now().UTC(),
	)
	if !errors.Is(err, ErrCompletionTicketLevelInvalid) {
		t.Fatalf("error=%v want level invalid", err)
	}
}

func TestPlanCompletionTicketCanPreserveSideStoriesAndHellPartyUnlocks(t *testing.T) {
	definitions := []Definition{
		{ID: 100, Path: "n_quest/main.qst", Grade: "[epic]", LevelMin: 1, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 200, Path: "n_quest/side.qst", Grade: "[side]", LevelMin: 1, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 250, Path: "n_quest/external.qst", Grade: "[common unique]", LevelMin: 1, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 260, Path: "n_quest/slot.qst", Grade: "[epic]", LevelMin: 1, LevelMax: 99, Job: "[all]", ExposedByNPC: -1, RewardType: "[slot expansion]", RewardIntData: []int64{2}},
		{ID: 300, Path: "n_quest/hell.qst", Grade: "[epic]", LevelMin: 1, LevelMax: 99, Job: "[all]", ExposedByNPC: -1, RewardType: "[hell challenge]", RewardIntData: []int64{1}},
	}
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	character := dnfrepo.CharacterRecord{CharacterID: "77", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 1}}
	eligibility := CharacterEligibility{Level: 90, Job: 2, GrowType: 1}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	plan, err := catalog.PlanCompletionTicket(
		dnfrepo.QuestRecord{CharacterID: "77"},
		character,
		eligibility,
		CompletionTicketSpec{ActionType: "[quest clear]", MaximumLevel: 90, PreserveSideQuests: true, PreserveHellPartyUnlocks: true},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.CompletedQuestIDs, []int64{100}) {
		t.Fatalf("protected completion=%v want only main progression", plan.CompletedQuestIDs)
	}

	plan, err = catalog.PlanCompletionTicket(
		dnfrepo.QuestRecord{CharacterID: "77"},
		character,
		eligibility,
		CompletionTicketSpec{ActionType: "[quest clear]", MaximumLevel: 90},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.CompletedQuestIDs, []int64{100, 200, 260, 300}) {
		t.Fatalf("ordinary ticket completion=%v want unrestricted ticket policy", plan.CompletedQuestIDs)
	}

	plan, err = catalog.PlanCompletionTicket(
		dnfrepo.QuestRecord{CharacterID: "77"},
		character,
		eligibility,
		CompletionTicketSpec{
			ActionType:         "[any quest clear]",
			ActionValues:       []int64{100, 200, 250, 260},
			MaximumLevel:       90,
			PreserveSideQuests: true,
		},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.CompletedQuestIDs, []int64{100}) {
		t.Fatalf("protected explicit completion=%v want only main progression", plan.CompletedQuestIDs)
	}
}

func completionTicketTestCatalog() *Catalog {
	definitions := []Definition{
		{ID: 100, Path: "n_quest/main1.qst", Grade: "[epic]", LevelMin: 1, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 101, Path: "n_quest/main2.qst", Grade: "[epic]", LevelMin: 90, LevelMax: 99, Job: "[all]", ExposedByNPC: -1, PreRequiredGroups: [][]int64{{100}}},
		{ID: 200, Path: "n_quest/achievement.qst", Grade: "[achievement]", LevelMin: 10, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 201, Path: "n_quest/side.qst", Grade: "[side]", LevelMin: 10, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 300, Path: "n_quest/ordinary.qst", Grade: "[normal]", LevelMin: 10, LevelMax: 99, Job: "[all]", ExposedByNPC: -1},
		{ID: 400, Path: "n_quest/awaken1.qst", Grade: "[achievement]", LevelMin: 50, LevelMax: 99, Job: "[gunner]", GrowType: 1, HasGrowType: true, JobChangeQuest: 2, ExposedByNPC: -1},
		{ID: 401, Path: "n_quest/awaken2.qst", Grade: "[achievement]", LevelMin: 75, LevelMax: 99, Job: "[gunner]", GrowType: 1, HasGrowType: true, JobChangeQuest: 3, ExposedByNPC: -1},
	}
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	return catalog
}
