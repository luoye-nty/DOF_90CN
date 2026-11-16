package dnfbridge

import (
	"testing"
	"time"
)

func TestChronicleAuraCardRewardPersistsCanonicalRawEntryOnlyForChronicleGear(t *testing.T) {
	catalog := &pvfDungeonDropCatalog{
		source: bridgePVFSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{
			545000: {
				ItemID:        545000,
				Kind:          dungeonDropItemEquipment,
				PVFPath:       "equipment/character/swordman/weapon/beamsword/beamswd_3choro1.equ",
				EquipmentType: "[weapon]",
				Rarity:        5,
				Durability:    48,
			},
			101040485: {
				ItemID:        101040485,
				Kind:          dungeonDropItemEquipment,
				PVFPath:       "equipment/character/swordman/weapon/beamsword/101040485.equ",
				EquipmentType: "[weapon]",
				Rarity:        4,
				Durability:    48,
			},
		},
	}
	now := time.Unix(1_700_000_000, 0).UTC()

	chronicle, err := currentDungeonCardItemReward(
		newCurrentDungeonDropLCG(1), catalog, 545000, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(chronicle.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("chronicle raw length=%d want=%d", len(chronicle.RawEntry), currentItemListEntryWireSize)
	}

	ordinary, err := currentDungeonCardItemReward(
		newCurrentDungeonDropLCG(2), catalog, 101040485, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary.RawEntry) != 0 {
		t.Fatalf("ordinary equipment raw entry changed: len=%d", len(ordinary.RawEntry))
	}
}
