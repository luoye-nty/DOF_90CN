package quest

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestMainContinuityLevelFloorUsesReachablePVFEpicSuccessor(t *testing.T) {
	definitions := []Definition{
		{ID: 3148, Grade: "[epic]", LevelMin: 8, LevelMax: 99, ExposedByNPC: -1},
		{ID: 3149, Grade: "[epic]", LevelMin: 11, LevelMax: 99, ExposedByNPC: -1, PreRequiredGroups: [][]int64{{3148}}},
		{ID: 3150, Grade: "[epic]", LevelMin: 13, LevelMax: 99, ExposedByNPC: -1, PreRequiredGroups: [][]int64{{3149}}},
	}
	catalog := testContinuityCatalog(definitions)
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3148: {Status: "active"},
	}}
	got, ok := catalog.MainContinuityLevelFloor(CharacterEligibility{Level: 10, Job: 0, GrowType: 0}, record, 3148)
	if !ok || got != 11 {
		t.Fatalf("floor=(%d,%t), want (11,true)", got, ok)
	}
	if got, ok := catalog.MainContinuityLevelFloor(CharacterEligibility{Level: 11, Job: 0, GrowType: 0}, record, 3148); ok || got != 0 {
		t.Fatalf("already-at-gate floor=(%d,%t)", got, ok)
	}
}

func TestMainContinuityLevelFloorRequiresAllPVFPrerequisites(t *testing.T) {
	definitions := []Definition{
		{ID: 3520, Grade: "[epic]", LevelMin: 63, LevelMax: 99, ExposedByNPC: -1},
		{ID: 9000, Grade: "[epic]", LevelMin: 63, LevelMax: 99, ExposedByNPC: -1},
		{ID: 3521, Grade: "[epic]", LevelMin: 64, LevelMax: 99, ExposedByNPC: -1, PreRequiredGroups: [][]int64{{3520, 9000}}},
	}
	catalog := testContinuityCatalog(definitions)
	character := CharacterEligibility{Level: 63, Job: 0, GrowType: 0}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{3520: {Status: "active"}}}
	if got, ok := catalog.MainContinuityLevelFloor(character, record, 3520); ok || got != 0 {
		t.Fatalf("missing prerequisite floor=(%d,%t)", got, ok)
	}
	record.States[9000] = dnfrepo.QuestState{Status: "completed"}
	if got, ok := catalog.MainContinuityLevelFloor(character, record, 3520); !ok || got != 64 {
		t.Fatalf("complete prerequisites floor=(%d,%t), want (64,true)", got, ok)
	}
}

func TestMainContinuityLevelFloorFiltersGrowBranchAndFailsClosedOnDifferentTargets(t *testing.T) {
	definitions := []Definition{
		{ID: 3356, Grade: "[epic]", LevelMin: 49, LevelMax: 99, ExposedByNPC: -1},
		{ID: 3367, Grade: "[epic]", LevelMin: 50, LevelMax: 99, ExposedByNPC: -1, GrowType: 0, HasGrowType: true, PreRequiredGroups: [][]int64{{3356}}},
		{ID: 3368, Grade: "[epic]", LevelMin: 50, LevelMax: 99, ExposedByNPC: -1, GrowType: 1, HasGrowType: true, PreRequiredGroups: [][]int64{{3356}}},
	}
	catalog := testContinuityCatalog(definitions)
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{3356: {Status: "active"}}}
	if got, ok := catalog.MainContinuityLevelFloor(CharacterEligibility{Level: 49, Job: 0, GrowType: 0}, record, 3356); !ok || got != 50 {
		t.Fatalf("grow branch floor=(%d,%t), want (50,true)", got, ok)
	}

	ambiguous := append([]Definition(nil), definitions...)
	ambiguous = append(ambiguous, Definition{
		ID: 9999, Grade: "[epic]", LevelMin: 51, LevelMax: 99, ExposedByNPC: -1,
		GrowType: 0, HasGrowType: true, PreRequiredGroups: [][]int64{{3356}},
	})
	if got, ok := testContinuityCatalog(ambiguous).MainContinuityLevelFloor(
		CharacterEligibility{Level: 49, Job: 0, GrowType: 0}, record, 3356,
	); ok || got != 0 {
		t.Fatalf("ambiguous target floor=(%d,%t)", got, ok)
	}
}

func testContinuityCatalog(definitions []Definition) *Catalog {
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	return catalog
}
