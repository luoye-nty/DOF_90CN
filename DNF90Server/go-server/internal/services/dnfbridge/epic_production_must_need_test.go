package dnfbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// TestCommitCurrentEpicProductionCompoundChargesMustNeedItemPerUnit covers the
// live batch that made 1000 catalysts for a single legendary soul.
//
// The catalyst recipe reads `[must need item] 10099775 1` next to
// `[need item count] 1` and `[min make item count] 1`, so the soul is a
// per-unit cost: a request whose contribution yields 1000 units owes 1000
// souls. Charging it once per request let one soul mint the whole batch.
func TestCommitCurrentEpicProductionCompoundChargesMustNeedItemPerUnit(t *testing.T) {
	ctx := context.Background()
	const (
		catalystID     = uint32(10163134)
		contributionID = uint32(10163136)
		legendarySoul  = uint32(10099775)
		soulSlot       = int16(364)
	)
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                   "",
		dungeonDropStackableList:                "10163134 `catalyst/dimension.stk`\n10163136 `material/shard.stk`\n10099775 `material/legendary_soul.stk`\n",
		dungeonDropEquipmentList:                "",
		"stackable/catalyst/dimension.stk":      "[stackable type] `[material]`\n[stack limit] 9999\n",
		"stackable/material/shard.stk":          "[stackable type] `[material]`\n[stack limit] 9999\n",
		"stackable/material/legendary_soul.stk": "[stackable type] `[material]`\n[stack limit] 9999\n",
	}
	items, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	output, err := items.ResolveItem(catalystID)
	if err != nil {
		t.Fatal(err)
	}
	plan := currentEpicProductionCompoundPlan{
		Recipe: currentEpicProductionCatalyst{
			ItemID: catalystID, GroupKey: 0, LevelLimit: 1,
			NeedCount: 1, MinMakeCount: 1, MaxMakeCount: 1,
			MustNeedItemID: legendarySoul, MustNeedItemCount: 1,
		},
		OutputDefinition: output,
		Contribution:     1000,
	}
	request := currentEpicProductionCompoundRequest{
		CatalystItemID: catalystID,
		Materials:      []currentEpicProductionCompoundMaterial{{ItemID: contributionID, Slot: 125, Count: 1000}},
	}

	seed := func(soulCount int64) (dnfrepo.Group, *Service, *gameSession) {
		repositories := dnfrepomemory.NewMemoryGroup()
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: "1", AccountID: "account-1", Level: 90, Stats: map[string]int64{},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: "1",
			Slots:       map[string]dnfrepo.ItemStack{"0:125": {ItemID: int64(contributionID), Count: 1000}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
			AccountID: "account-1",
			Slots: map[string]dnfrepo.ItemStack{
				dnfrepo.AccountSharedInventorySlotKey(soulSlot): {ItemID: int64(legendarySoul), Count: soulCount},
			},
		}); err != nil {
			t.Fatal(err)
		}
		service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
		return repositories, service, &gameSession{accountID: "account-1", selectedCharacterID: 1}
	}

	repositories, service, session := seed(2004)
	result, err := service.commitCurrentEpicProductionCompound(ctx, session, plan, request, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputCount != 1000 || result.MustNeedItemCount != 1000 || result.MustNeedItemID != legendarySoul {
		t.Fatalf("compound result output=%d must_need=%d/%d", result.OutputCount, result.MustNeedItemID, result.MustNeedItemCount)
	}
	account, _, _ := repositories.AccountInventory.Load(ctx, "account-1")
	if got := account.Slots[dnfrepo.AccountSharedInventorySlotKey(soulSlot)].Count; got != 1004 {
		t.Fatalf("legendary souls left=%d, want 1004 (2004 - 1000)", got)
	}

	// Not enough souls for the whole batch must refuse outright rather than
	// mint 1000 catalysts for whatever is in the warehouse.
	repositories, service, session = seed(999)
	if _, err := service.commitCurrentEpicProductionCompound(ctx, session, plan, request, time.Now().UTC()); !errors.Is(err, errCurrentEpicProductionMaterial) {
		t.Fatalf("short soul stock err=%v, want errCurrentEpicProductionMaterial", err)
	}
	account, _, _ = repositories.AccountInventory.Load(ctx, "account-1")
	inventory, _, _ := repositories.Inventory.Load(ctx, "1")
	if account.Slots[dnfrepo.AccountSharedInventorySlotKey(soulSlot)].Count != 999 ||
		inventory.Slots["0:125"].ItemID != int64(contributionID) ||
		inventory.Slots["0:125"].Count != 1000 {
		t.Fatalf("refused compound mutated state: account=%+v inventory=%+v", account.Slots, inventory.Slots)
	}
}
