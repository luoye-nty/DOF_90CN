package dungeonadmission

import (
	"fmt"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFAdmissionLimitCensus(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run the admission-limit census")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	assertDungeonPolicy(t, catalog, 8000, "crack_of_dimension.character.daily", 10, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 7165, "dimension_dungeon.character.daily", 10, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 7179, "dimension_dungeon.character.daily", 10, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5006, "license_dungeon.character.daily", 2, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5029, "license_dungeon.character.daily", 2, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 62, "dimension_gate.character.daily", 5, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5115, "tower_of_grief.character.daily", 2, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5214, "tower_of_grief.account.daily", 1, PeriodDaily, 0)
	for dungeonID, limit := range map[uint32]uint32{4906: 2, 4910: 1} {
		assertDungeonPolicy(t, catalog, dungeonID,
			fmt.Sprintf("account_character_dungeon.%d.character.daily", dungeonID), limit, PeriodDaily, 0)
		assertDungeonPolicy(t, catalog, dungeonID, AccountCharacterDungeonAccountPolicyKey, 5, PeriodDaily, 0)
	}
	if got := catalog.AccountCharacterDungeonDailyLimit(30); got != 10 {
		t.Fatalf("real adventure-group level 30 account-character daily limit=%d want=10", got)
	}

	towerEvidence := catalog.EvidenceForDungeon(11008)
	if len(towerEvidence) != 2 {
		t.Fatalf("real tower 11008 evidence=%+v", towerEvidence)
	}
	for _, evidence := range towerEvidence {
		if evidence.Period != PeriodDaily || !evidence.Enforced || evidence.Limit != 3 || len(evidence.DungeonIDs) != 100 || evidence.DungeonIDs[99] != 11107 {
			t.Fatalf("real tower evidence=%+v", evidence)
		}
	}
	assertDungeonPolicy(t, catalog, 11008, "tower_of_despair.character.daily", 3, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 11107, "tower_of_despair.account.daily", 3, PeriodDaily, 0)
	despair, found := catalog.TowerRoute(TowerFamilyDespair)
	if !found || len(despair.DungeonIDs) != 100 || despair.DungeonIDs[0] != 11008 || despair.DungeonIDs[99] != 11107 || len(despair.UseAllowItems) != 0 {
		t.Fatalf("real despair tower route=%+v found=%t", despair, found)
	}
	grief, found := catalog.TowerRoute(TowerFamilyGrief)
	wantGriefItems := []TowerUseItem{
		{ItemID: 10160849, Count: 10}, {ItemID: 10160850, Count: 10},
		{ItemID: 10163185, Count: -1}, {ItemID: 10163183, Count: -1},
		{ItemID: 10163184, Count: -1}, {ItemID: 10163187, Count: -1},
		{ItemID: 10163188, Count: -1}, {ItemID: 10163189, Count: -1},
	}
	if !found || len(grief.DungeonIDs) != 100 || grief.DungeonIDs[0] != 5115 || grief.DungeonIDs[99] != 5214 || len(grief.UseAllowItems) != len(wantGriefItems) {
		t.Fatalf("real grief tower route=%+v found=%t", grief, found)
	}
	for index := range wantGriefItems {
		if grief.UseAllowItems[index] != wantGriefItems[index] {
			t.Fatalf("real grief tower use item[%d]=%+v want=%+v", index, grief.UseAllowItems[index], wantGriefItems[index])
		}
	}
	for _, key := range []string{"raid.anton.account.daily", "raid.luke.account.daily", "raid.fiend_war.account.daily"} {
		policy, found := catalog.Policy(key)
		if !found || policy.Limit != 10 || len(policy.DungeonIDs) != 0 {
			t.Fatalf("real raid policy %s found=%t policy=%+v", key, found, policy)
		}
	}
	t.Logf("active PVF admission policies=%d structured evidence rows=%d", len(catalog.Policies()), len(catalog.Evidence()))
}
