package town

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Runtime PVF: HendonMyre's main map (town 39 area 0) gates the training-room
// passage on quest 10100 through a [quest condition] block. This is the shape
// the quest-finish scene refresh keys on, so the loader must surface it.
func TestRealScriptPVFHendonMyreDeclaresTrainingRoomQuestCondition(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify town map quest conditions")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	table, err := Load(context.Background(), archive, Options{})
	if err != nil {
		t.Fatalf("load real town catalog: %v", err)
	}
	area, ok := table.FindArea(39, 0)
	if !ok || area.MapPath != "Cataclysm/Town/HendonMyre/new_Hendon_Main.map" {
		t.Fatalf("town 39 area 0=(%+v,%t)", area, ok)
	}
	found := false
	for _, questID := range area.QuestConditions {
		if questID == 10100 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("town 39 area 0 quest conditions=%v, want 10100 (training room gate)", area.QuestConditions)
	}
	// At least one other town area must declare none, proving the field is not
	// populated by accident from unrelated sections.
	plainSeen := false
	for _, town := range table.Towns() {
		for _, candidate := range town.Areas {
			if len(candidate.QuestConditions) == 0 {
				plainSeen = true
				break
			}
		}
		if plainSeen {
			break
		}
	}
	if !plainSeen {
		t.Fatal("every town area declares quest conditions; the loader is not reading the [quest condition] section selectively")
	}
}
