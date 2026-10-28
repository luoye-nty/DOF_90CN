package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerOrdinaryReinforceAndAmplifyChargeResolvedGoldAtomically(t *testing.T) {
	for _, mode := range []string{"reinforce", "amplify"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{
				CharacterID: "77",
				AccountID:   "acc",
				Stats:       map[string]int64{"gold": 1_000},
			})
			extra := map[string]string{
				"item_kind":      "equipment",
				"durability":     "45",
				"max_durability": "45",
			}
			if mode == "amplify" {
				extra["amplify_type"] = "3"
				extra["amplify_value"] = "12"
			}
			saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, 9): {ItemID: 700, Count: 1, Extra: extra},
				},
			})
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			cmd := NewUpgradeCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UpgradeItemRequest{
				Mode: mode, TargetSlotIndex: 9, TargetItemTemplateID: 700,
				MaterialSlotIndex: -1, OptionalTicketSlotIndex: -1,
			})
			cmd.UpgradeSuccessWeight = currentUpgradeSuccessBase
			cmd.UpgradeGoldCost = 300
			cmd.UpgradeGoldCostResolved = true
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil {
				t.Fatalf("Upgrade: %v", err)
			}
			if !result.Success || !result.UpgradeSucceeded || result.GoldCost != 300 || result.UpdatedGold != 700 {
				t.Fatalf("result=%+v", result)
			}
			character, found, err := repos.Character.Load(ctx, "77")
			if err != nil || !found || character.Stats["gold"] != 700 {
				t.Fatalf("character=%+v found=%t err=%v", character, found, err)
			}
			if got := upgradeLevelOf(loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 9)]); got != 1 {
				t.Fatalf("upgrade level=%d want=1", got)
			}
		})
	}
}

func TestOwnerOrdinaryUpgradeInsufficientResolvedGoldRollsBack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Stats: map[string]int64{"gold": 299},
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9): {
				ItemID: 700, Count: 1,
				Extra: map[string]string{"item_kind": "equipment", "durability": "45", "max_durability": "45"},
			},
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewUpgradeCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UpgradeItemRequest{
		Mode: "reinforce", TargetSlotIndex: 9, TargetItemTemplateID: 700,
		MaterialSlotIndex: -1, OptionalTicketSlotIndex: -1,
	})
	cmd.UpgradeSuccessWeight = currentUpgradeSuccessBase
	cmd.UpgradeGoldCost = 300
	cmd.UpgradeGoldCostResolved = true
	result, err := owner.Upgrade(ctx, cmd)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if result.Success || result.Changed || result.ErrorCode != upgradeErrorInsufficientGold || result.GoldCost != 300 {
		t.Fatalf("result=%+v", result)
	}
	character, found, err := repos.Character.Load(ctx, "77")
	if err != nil || !found || character.Stats["gold"] != 299 {
		t.Fatalf("character=%+v found=%t err=%v", character, found, err)
	}
	if got := upgradeLevelOf(loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 9)]); got != 0 {
		t.Fatalf("upgrade level mutated to %d", got)
	}
}
