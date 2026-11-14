package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFRobotRecycleCandidatePools(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect robot recycle equipment pools")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		level  int64
		rarity int64
	}{
		{level: 40, rarity: 0},
		{level: 40, rarity: 1},
		{level: 45, rarity: 0},
		{level: 45, rarity: 1},
		{level: 45, rarity: 2},
		{level: 50, rarity: 0},
		{level: 50, rarity: 1},
		{level: 85, rarity: 6},
		{level: 90, rarity: 5},
		{level: 90, rarity: 6},
		{level: 95, rarity: 6},
	} {
		candidates, err := catalog.robotRecycleEquipmentCandidates(target.level, target.rarity, 0)
		if err != nil {
			t.Errorf("level=%d rarity=%d: %v", target.level, target.rarity, err)
			continue
		}
		t.Logf("level=%d rarity=%d candidates=%d", target.level, target.rarity, len(candidates))
		if (target.level == 45 && target.rarity == 1) || (target.level == 90 && target.rarity == 6) {
			if len(candidates) == 0 {
				t.Fatalf("required exchange pool is empty: level=%d rarity=%d", target.level, target.rarity)
			}
		}
	}
	for _, level := range []int64{85, 90, 95} {
		for _, rarity := range []int64{3, 4, 5, 6} {
			attachments := make(map[string]int)
			for _, weighted := range catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: level, Rarity: rarity}] {
				definition, resolveErr := catalog.ResolveItem(weighted.ItemID)
				if resolveErr != nil {
					attachments["<resolve-error>"]++
					continue
				}
				attachments[normalizeMagicBoxPVFType(definition.AttachType)]++
			}
			t.Logf("level=%d rarity=%d generic=%d attachments=%v", level, rarity,
				len(catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: level, Rarity: rarity}]), attachments)
		}
		for rarity := int64(0); rarity <= 6; rarity++ {
			t.Logf("level=%d rarity=%d generic=%d", level, rarity,
				len(catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: level, Rarity: rarity}]))
			free, candidateErr := catalog.robotRecycleEquipmentCandidates(level, rarity, 0)
			if candidateErr != nil {
				t.Errorf("free candidates level=%d rarity=%d: %v", level, rarity, candidateErr)
			}
			t.Logf("level=%d rarity=%d free=%d", level, rarity, len(free))
		}
	}
}
