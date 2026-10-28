package dnfbridge

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The era-correct Hell orb family splits on the dungeon's actor level: level
// 86+ dungeons pay the 地轨中心 orb, everything lower the generic 深渊宝珠.
func TestCurrentDungeonHellPartyOrbItemIDSplitsAtLevel86(t *testing.T) {
	if got := currentDungeonHellPartyOrbItemID(85); got != currentDungeonHellGenericOrbItemID {
		t.Fatalf("level 85 orb=%d want generic %d", got, currentDungeonHellGenericOrbItemID)
	}
	if got := currentDungeonHellPartyOrbItemID(86); got != currentDungeonHellMetroOrbItemID {
		t.Fatalf("level 86 orb=%d want metro %d", got, currentDungeonHellMetroOrbItemID)
	}
	// 0.54% == 54 / 10000.
	if currentDungeonHellOrbChance*10000 != 54*currentDungeonHellOrbRollDenominator {
		t.Fatalf("orb chance %d/%d is not the documented 0.54%%", currentDungeonHellOrbChance, currentDungeonHellOrbRollDenominator)
	}
}

// Both orb items must resolve in the runtime item catalog, or a jackpot roll
// would fail the whole Hell reward plan instead of dropping the orb.
func TestRealScriptPVFHellOrbItemsResolve(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify the Hell orb items")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	monsters, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	items, err := monsters.DropCatalog()
	if err != nil {
		t.Fatal(err)
	}
	_ = context.Background()
	for _, itemID := range []uint32{currentDungeonHellGenericOrbItemID, currentDungeonHellMetroOrbItemID} {
		definition, err := items.ResolveItem(itemID)
		if err != nil {
			t.Fatalf("orb %d does not resolve: %v", itemID, err)
		}
		if definition.Name == "" {
			t.Fatalf("orb %d resolved without a name: %+v", itemID, definition)
		}
	}
}
