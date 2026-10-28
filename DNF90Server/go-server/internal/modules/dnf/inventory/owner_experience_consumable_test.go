package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/experiencebonus"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerUseExperienceConsumableCommitsItemAndCharacterScopedAccountEffectAtomically(t *testing.T) {
	ctx := context.Background()
	repos := dnfmemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	const pvfPath = "stackable/event/chn_event_20150730_growup/chn_3exp_10005931.stk"
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 4): {
				ItemID: 10005931,
				Count:  2,
				Extra: map[string]string{
					"item_kind":      "stackable",
					"pvf_path":       pvfPath,
					"stackable_type": "[waste]",
				},
			},
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	resolver := func(itemID int64) (alignedcmd.ExperienceConsumableResolution, error) {
		return alignedcmd.ExperienceConsumableResolution{
			ItemID: itemID, BonusPercent: 200, DurationSeconds: 1800, PVFPath: pvfPath,
		}, nil
	}
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 4, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 10005931,
	})
	result, err := owner.UseStackableWithResolvers(ctx, cmd, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExperienceBonusActivated || result.ExperienceBonusPercent != 200 || result.RemainingCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	account, found, err := repos.Account.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	if effect, active := experiencebonus.ActiveForCharacter(account, "77", time.Now().UTC()); !active || effect.ItemID != 10005931 || effect.BonusPercent != 200 {
		t.Fatalf("effect=%+v active=%t metadata=%+v", effect, active, account.Metadata)
	}
	if _, active := experiencebonus.ActiveForCharacter(account, "78", time.Now().UTC()); active {
		t.Fatalf("experience effect leaked to another character: metadata=%+v", account.Metadata)
	}

	if _, err := owner.UseStackableWithResolvers(ctx, cmd, nil, resolver); !errors.Is(err, experiencebonus.ErrEffectActive) {
		t.Fatalf("second activation error=%v", err)
	}
	stack := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 4)]
	if stack.Count != 1 {
		t.Fatalf("active-effect retry consumed item: %+v", stack)
	}
}

func TestOwnerUseUnlimitedExperienceConsumableActivatesWithoutDecrement(t *testing.T) {
	ctx := context.Background()
	repos := dnfmemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	const pvfPath = "stackable/cash/chn_490700690.stk"
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 4): {
				ItemID: 490700690,
				Count:  1,
				Extra: map[string]string{
					"item_kind":      "stackable",
					"pvf_path":       pvfPath,
					"stackable_type": "[unlimited waste]",
				},
			},
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	resolver := func(itemID int64) (alignedcmd.ExperienceConsumableResolution, error) {
		return alignedcmd.ExperienceConsumableResolution{
			ItemID: itemID, BonusPercent: 100, DurationSeconds: 3600, PVFPath: pvfPath, Reusable: true,
		}, nil
	}
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 4, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 490700690,
	})
	result, err := owner.UseStackableWithResolvers(ctx, cmd, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExperienceBonusActivated || result.RemainingCount != 1 || !result.Changed {
		t.Fatalf("result=%+v", result)
	}
	stack := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 4)]
	if stack.Count != 1 {
		t.Fatalf("unlimited item was decremented: %+v", stack)
	}
	if _, err := owner.UseStackableWithResolvers(ctx, cmd, nil, resolver); !errors.Is(err, experiencebonus.ErrEffectActive) {
		t.Fatalf("second activation error=%v", err)
	}
	stack = loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 4)]
	if stack.Count != 1 {
		t.Fatalf("active-effect retry changed unlimited item: %+v", stack)
	}
}
