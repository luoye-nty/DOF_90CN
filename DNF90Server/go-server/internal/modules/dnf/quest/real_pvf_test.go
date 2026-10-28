package quest

import (
	"context"
	"os"
	"sort"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRuntimePVFHellChallengeDefinitions is an opt-in inspection and
// compatibility guard for the live PVF.  It records every terminal quest that
// carries the native [hell challenge] reward instead of assuming one fixed
// quest ID or reward-int-data shape.
func TestRuntimePVFHellChallengeDefinitions(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect live Hell Party challenge quests")
	}
	archive, err := platformpvf.Open(path)
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
	definitions := make([]Definition, 0)
	for _, definition := range catalog.Definitions() {
		if normalizeQuestTag(definition.RewardType) == "hell challenge" {
			definitions = append(definitions, definition)
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].ID < definitions[j].ID })
	if len(definitions) == 0 {
		t.Fatal("live PVF has no [hell challenge] quest definitions")
	}
	world, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		areas := world.HellPartyAreasForQuest(definition.ID)
		areaIDs := make([]int64, 0, len(areas))
		for _, area := range areas {
			areaIDs = append(areaIDs, area.ID)
		}
		t.Logf("hell challenge id=%d path=%s grade=%q type=%q int_data=%v check_count=%v condition=%v reward_int_data=%v main=%d prerequisites=%v area_ids=%v",
			definition.ID, definition.Path, definition.Grade, definition.Type, definition.IntData,
			definition.CheckCount, definition.ConditionData, definition.RewardIntData,
			definition.MainQuestID, definition.PreRequiredGroups, areaIDs)
	}
}
