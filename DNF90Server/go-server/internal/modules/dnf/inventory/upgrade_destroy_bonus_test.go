package inventory

import (
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func upgradeDestroyMaterialResolver(itemID int64) (alignedcmd.StackMergeResolution, error) {
	if itemID != 3244 {
		return alignedcmd.StackMergeResolution{}, errors.New("unexpected item")
	}
	return alignedcmd.StackMergeResolution{
		ItemID:        3244,
		Kind:          "stackable",
		PVFPath:       "stackable/material/crystal_pure.stk",
		StackableType: "[material]",
		StackLimit:    0,
		SlotStart:     121,
		SlotEnd:       176,
	}, nil
}

func TestGrantUpgradeDestroyBonusMergesExistingMaterialStack(t *testing.T) {
	owner := &Owner{stackMerge: upgradeDestroyMaterialResolver}
	items := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 160): {
			ItemID: 3244,
			Count:  16,
			Extra:  map[string]string{"source": "legacy_grant"},
		},
	}

	changed, err := owner.grantUpgradeDestroyBonus(items, 3244, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != 160 {
		t.Fatalf("changed slots = %v, want [160]", changed)
	}
	stack := items[slotKey(listTypeMain, 160)]
	if stack.Count != 17 || stack.Extra["item_kind"] != "stackable" ||
		stack.Extra["pvf_path"] != "stackable/material/crystal_pure.stk" ||
		stack.Extra["stackable_type"] != "[material]" {
		t.Fatalf("merged stack = %+v", stack)
	}
	for slot := int16(9); slot <= 69; slot++ {
		if _, found := items[slotKey(listTypeMain, slot)]; found {
			t.Fatalf("destroy bonus leaked into equipment/quick slot %d", slot)
		}
	}
}

func TestGrantUpgradeDestroyBonusUsesFirstMaterialSlot(t *testing.T) {
	owner := &Owner{stackMerge: upgradeDestroyMaterialResolver}
	items := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 121): {ItemID: 9000, Count: 1},
	}

	changed, err := owner.grantUpgradeDestroyBonus(items, 3244, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != 122 {
		t.Fatalf("changed slots = %v, want [122]", changed)
	}
	stack := items[slotKey(listTypeMain, 122)]
	if stack.ItemID != 3244 || stack.Count != 3 || stack.Extra["source"] != "upgrade_destroy_bonus" {
		t.Fatalf("new material stack = %+v", stack)
	}
	if _, found := items[slotKey(listTypeMain, 60)]; found {
		t.Fatal("destroy bonus used legacy equipment-page slot 60")
	}
}
