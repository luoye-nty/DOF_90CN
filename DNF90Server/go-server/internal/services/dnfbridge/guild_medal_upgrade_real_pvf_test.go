package dnfbridge

import (
	"os"
	"strings"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealPVFGuildMedalUpgradePolicy resolves the native flag_upgrade_table for
// every real 公会徽章 in the deployed Script.pvf. This resolver is the only
// consumer of that table and reads its rows positionally, so a sweep over the
// real flag catalog is what proves the tail offsets and the rarity weight index
// line up. The resolver validates each row itself, so any flag that fails to
// resolve is a real defect rather than a tolerant parse.
//
//	DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge \
//	  -run TestRealPVFGuildMedalUpgradePolicy -v
func TestRealPVFGuildMedalUpgradePolicy(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	flags, resolved := 0, 0
	named := map[string]int{}
	for itemID, reference := range catalog.itemRefs {
		// Real 公会徽章 live in the equipment catalog under
		// character/common/flag/*.equ; the flaggem/ stackables are the gems
		// that socket into them and are excluded by the kind filter.
		if reference.kind != dungeonDropItemEquipment || !strings.Contains(strings.ToLower(reference.path), "flag/") {
			continue
		}
		flags++
		for level := 0; level < 8; level++ {
			policy, err := resolveCurrentGuildMedalUpgrade(catalog, itemID, "reinforce", level)
			if err != nil {
				t.Fatalf("flag item=%d path=%q level=%d: %v", itemID, reference.path, level, err)
			}
			if !policy.GuildMedal || policy.CurrentLevel != level || policy.GoldCost < 0 ||
				policy.SuccessWeight < 0 || policy.SuccessWeight > currentUpgradeSuccessBase ||
				policy.MaterialItemID <= 0 || policy.MaterialCount <= 0 ||
				policy.PenaltyType < 0 || policy.PenaltyType > 3 {
				t.Fatalf("flag item=%d level=%d policy=%+v", itemID, level, policy)
			}
			resolved++
			if flags == 1 && level == 0 {
				t.Logf("sample flag item=%d path=%q: %+v", itemID, reference.path, policy)
			}
		}
		named[reference.path]++
	}
	if flags == 0 || resolved == 0 {
		t.Fatalf("no real guild flags found: flags=%d resolved=%d", flags, resolved)
	}
	t.Logf("real guild flags=%d resolved policies=%d", flags, resolved)
}
