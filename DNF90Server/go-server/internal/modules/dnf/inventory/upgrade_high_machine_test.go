package inventory

import "testing"

// op50 sub mode 2 is the 高级装备强化机. The operator's terms: 高级炉岩炭 replaces
// the table's 无色小晶块 at a tenth of the count, and the table's success
// weight is multiplied by 1.1. etc/upgrade.etc has no high-upgrade table and
// never names item 2681644, so none of this comes from the PVF.
func TestHighUpgradeTermsReplaceFuelAndScaleSuccess(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		weight       int
		materialNeed int
		wantWeight   int
		wantCount    int
	}{
		{name: "live band 180 crystals", weight: 13000, materialNeed: 180, wantWeight: 14300, wantCount: 18},
		{name: "rate rounds down", weight: 13001, materialNeed: 10, wantWeight: 14301, wantCount: 1},
		{name: "fuel rounds up", weight: 13000, materialNeed: 181, wantWeight: 14300, wantCount: 19},
		{name: "never free", weight: 13000, materialNeed: 9, wantWeight: 14300, wantCount: 1},
		{name: "caps at guaranteed", weight: 95000, materialNeed: 1, wantWeight: currentUpgradeSuccessBase, wantCount: 1},
		{name: "already guaranteed", weight: currentUpgradeSuccessBase, materialNeed: 1, wantWeight: currentUpgradeSuccessBase, wantCount: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cmd := Command{
				UpgradeHighUpgrade:    true,
				UpgradeSuccessWeight:  testCase.weight,
				UpgradeMaterialItemID: 3037,
				UpgradeMaterialCount:  testCase.materialNeed,
				UpgradeGoldCost:       398844,
				UpgradePenaltyType:    3,
			}
			applyUpgradeHighUpgradeTerms(&cmd)
			if cmd.UpgradeSuccessWeight != testCase.wantWeight {
				t.Fatalf("success weight=%d want %d", cmd.UpgradeSuccessWeight, testCase.wantWeight)
			}
			if cmd.UpgradeMaterialItemID != highUpgradeFuelItemID {
				t.Fatalf("material item=%d want %d", cmd.UpgradeMaterialItemID, highUpgradeFuelItemID)
			}
			if cmd.UpgradeMaterialCount != testCase.wantCount {
				t.Fatalf("fuel count=%d want %d", cmd.UpgradeMaterialCount, testCase.wantCount)
			}
			// Gold and the destruction penalty are the table's and must not move.
			if cmd.UpgradeGoldCost != 398844 || cmd.UpgradePenaltyType != 3 {
				t.Fatalf("high upgrade changed terms it must not: %+v", cmd)
			}
		})
	}

	// An ordinary reinforce is untouched.
	plain := Command{UpgradeSuccessWeight: 13000, UpgradeMaterialItemID: 3037, UpgradeMaterialCount: 180}
	applyUpgradeHighUpgradeTerms(&plain)
	if plain.UpgradeSuccessWeight != 13000 || plain.UpgradeMaterialItemID != 3037 {
		t.Fatalf("ordinary reinforce was rewritten: %+v", plain)
	}

	// A table row that asks for no material keeps asking for none.
	noMaterial := Command{UpgradeHighUpgrade: true, UpgradeSuccessWeight: 13000}
	applyUpgradeHighUpgradeTerms(&noMaterial)
	if noMaterial.UpgradeMaterialItemID != 0 || noMaterial.UpgradeMaterialCount != 0 {
		t.Fatalf("material-free row gained a requirement: %+v", noMaterial)
	}
}
