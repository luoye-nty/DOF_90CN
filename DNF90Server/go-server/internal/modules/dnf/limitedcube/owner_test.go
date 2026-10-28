package limitedcube

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerUseConsumesTicketConditionAndSplitMaterialStacks(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	expires := time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2":  {ItemID: 9000, Count: 2},
			"0:7":  {ItemID: 9001, Count: 1, Bind: true, ExpireAt: expires, RawEntry: []byte{1}, Extra: map[string]string{"old": "item"}},
			"0:12": {ItemID: 3037, Count: 6},
			"0:13": {ItemID: 3037, Count: 7},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Use(ctx, Command{AccountID: "dnf:19", CharacterID: "19", TicketSlot: 2, TicketItemID: 9000, TargetSlot: 7, TargetItemID: 9001}, limitedCubePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result.TicketRemaining != 1 || result.InputSlot != 7 || result.InputItemID != 9001 || result.ResultItemID != 9002 || result.ResultItemID == result.InputItemID ||
		!reflect.DeepEqual(result.ChangedSlots, []int16{2, 7, 12, 13}) {
		t.Fatalf("result = %+v", result)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if got := stored.Slots["0:2"].Count; got != 1 {
		t.Fatalf("ticket count = %d, want 1", got)
	}
	changed := stored.Slots["0:7"]
	if changed.ItemID != 9002 || changed.Count != 1 || !changed.Bind || !changed.ExpireAt.Equal(expires) || len(changed.RawEntry) != 0 || changed.Extra["pvf_path"] != "stackable/bead/result.stk" {
		t.Fatalf("changed bead = %+v", changed)
	}
	if _, found := stored.Slots["0:12"]; found {
		t.Fatalf("first material stack was not consumed: %+v", stored.Slots)
	}
	if got := stored.Slots["0:13"].Count; got != 3 {
		t.Fatalf("second material count = %d, want 3", got)
	}
}

// TestSelectWeightedResultUsesPVFWeightsWithoutFilteringTheConsumedItem pins the
// 2026-09-27 decision: the draw follows the PVF weights exactly as published, so
// a 变更券 may hand back the very item it consumed. The pool below is 50/50, so
// over 256 draws the consumed item has to come back many times; a filter that
// removed it from the pool could never return it once.
func TestSelectWeightedResultUsesPVFWeightsWithoutFilteringTheConsumedItem(t *testing.T) {
	candidates := []WeightedResult{
		{Stack: dnfrepo.ItemStack{ItemID: 9001, Count: 1}, Weight: 1000},
		{Stack: dnfrepo.ItemStack{ItemID: 9002, Count: 1}, Weight: 1000},
	}
	returned := map[int64]int{}
	for draw := 0; draw < 256; draw++ {
		selected, err := selectWeightedResult(candidates, 9001)
		if err != nil {
			t.Fatal(err)
		}
		returned[selected.Stack.ItemID]++
	}
	if returned[9001] == 0 || returned[9002] == 0 {
		t.Fatalf("consumed item filtered out of the pool: %v", returned)
	}
}

// TestOwnerUseRejectsResultPoolContainingOnlyInputItemWithoutMutatingInventory
// keeps the single guard that survives the weight change: a document whose every
// candidate is the consumed template can never produce a change, so it is
// refused instead of burning the ticket and the material for an identical item.
func TestOwnerUseRejectsResultPoolContainingOnlyInputItemWithoutMutatingInventory(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	initial := dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}
	if err := repositories.Inventory.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	policy := limitedCubePolicy()
	policy.Results[0].Stack.ItemID = 9001
	if _, err := owner.Use(ctx, limitedCubeCommand(), policy); !errors.Is(err, ErrResultSelectionFailed) {
		t.Fatalf("Use error=%v want result selection failure", err)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if !reflect.DeepEqual(stored.Slots, initial.Slots) {
		t.Fatalf("failed use mutated slots = %+v, want %+v", stored.Slots, initial.Slots)
	}
}

func TestOwnerUseRejectsMissingMaterialWithoutChangingInventory(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	initial := dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1},
			"0:9": {ItemID: 3037, Count: 9},
		},
	}
	if err := repositories.Inventory.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Use(ctx, Command{AccountID: "dnf:19", CharacterID: "19", TicketSlot: 2, TicketItemID: 9000, TargetSlot: 7, TargetItemID: 9001}, limitedCubePolicy())
	if !errors.Is(err, ErrMaterialInsufficient) {
		t.Fatalf("Use error = %v, want insufficient material", err)
	}
	stored, found, loadErr := repositories.Inventory.Load(ctx, "19")
	if loadErr != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, loadErr)
	}
	if !reflect.DeepEqual(stored.Slots, initial.Slots) {
		t.Fatalf("failed use mutated slots = %+v, want %+v", stored.Slots, initial.Slots)
	}
}

func TestOwnerUseRejectsWrongTicketAndMissingCondition(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name  string
		slots map[string]dnfrepo.ItemStack
		item  int64
		want  error
	}{
		{
			name:  "wrong ticket identity",
			slots: map[string]dnfrepo.ItemStack{"0:2": {ItemID: 9999, Count: 1}, "0:7": {ItemID: 9001, Count: 1}, "0:9": {ItemID: 3037, Count: 10}},
			item:  9000,
			want:  ErrTicketMismatch,
		},
		{
			name:  "ineligible target bead",
			slots: map[string]dnfrepo.ItemStack{"0:2": {ItemID: 9000, Count: 1}, "0:7": {ItemID: 9999, Count: 1}, "0:9": {ItemID: 3037, Count: 10}},
			item:  9000,
			want:  ErrConditionItemMismatch,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: test.slots}); err != nil {
				t.Fatal(err)
			}
			owner, err := NewOwner(repositories)
			if err != nil {
				t.Fatal(err)
			}
			_, err = owner.Use(ctx, Command{AccountID: "dnf:19", CharacterID: "19", TicketSlot: 2, TicketItemID: test.item, TargetSlot: 7, TargetItemID: 9001}, limitedCubePolicy())
			if !errors.Is(err, test.want) {
				t.Fatalf("Use error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOwnerUseRejectsLockedTicketTargetAndMaterial(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name  string
		slots map[string]dnfrepo.ItemStack
		want  error
	}{
		{
			name: "ticket",
			slots: map[string]dnfrepo.ItemStack{
				"0:2": {ItemID: 9000, Count: 1, Extra: map[string]string{"equipment_lock_state": "locked"}},
				"0:7": {ItemID: 9001, Count: 1},
				"0:9": {ItemID: 3037, Count: 10},
			},
			want: ErrTicketLocked,
		},
		{
			name: "target bead",
			slots: map[string]dnfrepo.ItemStack{
				"0:2": {ItemID: 9000, Count: 1},
				"0:7": {ItemID: 9001, Count: 1, Extra: map[string]string{"equipment_lock_state": "1"}},
				"0:9": {ItemID: 3037, Count: 10},
			},
			want: ErrConditionItemLocked,
		},
		{
			name: "material",
			slots: map[string]dnfrepo.ItemStack{
				"0:2": {ItemID: 9000, Count: 1},
				"0:7": {ItemID: 9001, Count: 1},
				"0:9": {ItemID: 3037, Count: 10, Extra: map[string]string{"equipment_lock_state": "pending_unlock"}},
			},
			want: ErrMaterialInsufficient,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			initial := dnfrepo.InventoryRecord{CharacterID: "19", Slots: test.slots}
			if err := repositories.Inventory.Save(ctx, initial); err != nil {
				t.Fatal(err)
			}
			owner, err := NewOwner(repositories)
			if err != nil {
				t.Fatal(err)
			}
			_, err = owner.Use(ctx, Command{AccountID: "dnf:19", CharacterID: "19", TicketSlot: 2, TicketItemID: 9000, TargetSlot: 7, TargetItemID: 9001}, limitedCubePolicy())
			if !errors.Is(err, test.want) {
				t.Fatalf("Use error=%v want=%v", err, test.want)
			}
			stored, found, loadErr := repositories.Inventory.Load(ctx, "19")
			if loadErr != nil || !found {
				t.Fatalf("load inventory found=%t err=%v", found, loadErr)
			}
			if !reflect.DeepEqual(stored.Slots, initial.Slots) {
				t.Fatalf("locked use mutated slots=%+v want=%+v", stored.Slots, initial.Slots)
			}
		})
	}
}

func TestOwnerUseConsumesAccountSharedMaterialAfterCharacterInventory(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1},
			"0:9": {ItemID: 3037, Count: 6},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:19",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {ItemID: 3037, Count: 20},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Use(ctx, limitedCubeCommand(), limitedCubePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.ChangedSlots, []int16{2, 7, 9}) || !reflect.DeepEqual(result.AccountChangedSlots, []int16{358}) {
		t.Fatalf("changed slots = character=%v account=%v", result.ChangedSlots, result.AccountChangedSlots)
	}
	character, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character inventory found=%t err=%v", found, err)
	}
	if _, found := character.Slots["0:9"]; found {
		t.Fatalf("character material was not consumed first: %+v", character.Slots)
	}
	account, found, err := repositories.AccountInventory.Load(ctx, "dnf:19")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if got := account.Slots["0:358"].Count; got != 16 {
		t.Fatalf("account material count=%d want=16", got)
	}
}

func TestOwnerUseConsumesAccountSharedMaterialWhenCharacterHasNone(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:19",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Use(ctx, limitedCubeCommand(), limitedCubePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.ChangedSlots, []int16{2, 7}) || !reflect.DeepEqual(result.AccountChangedSlots, []int16{358}) {
		t.Fatalf("changed slots = character=%v account=%v", result.ChangedSlots, result.AccountChangedSlots)
	}
	account, found, err := repositories.AccountInventory.Load(ctx, "dnf:19")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if _, found := account.Slots["0:358"]; found {
		t.Fatalf("account material was not consumed: %+v", account.Slots)
	}
}

func TestOwnerUseExcludesLockedAccountSharedMaterial(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:19",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {
				ItemID: 3037,
				Count:  10,
				Extra:  map[string]string{"equipment_lock_state": "locked"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Use(ctx, limitedCubeCommand(), limitedCubePolicy()); !errors.Is(err, ErrMaterialInsufficient) {
		t.Fatalf("Use error=%v want insufficient material", err)
	}
	account, found, err := repositories.AccountInventory.Load(ctx, "dnf:19")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if got := account.Slots["0:358"].Count; got != 10 {
		t.Fatalf("locked account material count=%d want=10", got)
	}
}

// TestOwnerUseCarriesEquipmentInstanceStateAcrossTemplateSwap pins the
// 2026-09-27 称号随机变更券 semantics. The 90CN coupon text promises
// 「称号变更后， 附在称号上的宝珠属性仍然有效， 并且在无法交易的称号上使用变更券时， 变更后的称号
// 也无法交易。」, so the 宝珠 enchant (raw +0x0E/+0x12, or the random title enchant
// pair at +0x61/+0x63) and the 封装 mark (raw +0x0D) have to survive the swap while
// the consumed template's own row must not.
func TestOwnerUseCarriesEquipmentInstanceStateAcrossTemplateSwap(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {
				ItemID:   9101,
				Count:    1,
				Bind:     true,
				RawEntry: []byte{0xAA, 0xBB},
				Extra: map[string]string{
					"item_kind":                      "equipment",
					"equipment_type":                 "[title name]",
					"pvf_path":                       "equipment/character/common/title/title_a.equ",
					"quality_seed":                   "12345",
					"value_a":                        "10008663",
					"enchant_card_id":                "10008663",
					"byte_12":                        "2",
					"enchant_upgrade_count":          "2",
					"random_title_enchant_config_id": "10001",
					"random_title_enchant_row_id":    "72",
					"seal_flag":                      "1",
				},
			},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{
		TicketItemID: 9000,
		Conditions:   []Requirement{{ItemID: 9101, Count: 1}},
		Materials:    []Requirement{{ItemID: 3037, Count: 10}},
		Results: []WeightedResult{{
			Stack: dnfrepo.ItemStack{
				ItemID: 9102,
				Count:  1,
				Extra: map[string]string{
					"source":         "limited_cube_result",
					"item_kind":      "equipment",
					"pvf_path":       "equipment/character/common/title/title_b.equ",
					"equipment_type": "[title name]",
					"quality_seed":   "67890",
				},
			},
			Weight: 1000,
		}},
	}
	result, err := owner.Use(ctx, Command{
		AccountID: "dnf:19", CharacterID: "19",
		TicketSlot: 2, TicketItemID: 9000, TargetSlot: 7, TargetItemID: 9101,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultItemID != 9102 || result.InputItemID != 9101 || !reflect.DeepEqual(result.ChangedSlots, []int16{2, 7, 9}) {
		t.Fatalf("result = %+v", result)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	changed := stored.Slots["0:7"]
	if changed.ItemID != 9102 || changed.Count != 1 || !changed.Bind {
		t.Fatalf("changed title = %+v", changed)
	}
	// The consumed template must not leak its row into the replacement.
	if len(changed.RawEntry) != 0 {
		t.Fatalf("changed title kept the consumed row: %x", changed.RawEntry)
	}
	// The result template owns these keys outright.
	for key, want := range map[string]string{
		"source":         "limited_cube_result",
		"pvf_path":       "equipment/character/common/title/title_b.equ",
		"equipment_type": "[title name]",
		"quality_seed":   "67890",
	} {
		if got := changed.Extra[key]; got != want {
			t.Fatalf("changed title extra[%q] = %q want %q", key, got, want)
		}
	}
	// The player's instance state has to come across.
	for key, want := range map[string]string{
		"value_a":                        "10008663",
		"enchant_card_id":                "10008663",
		"byte_12":                        "2",
		"enchant_upgrade_count":          "2",
		"random_title_enchant_config_id": "10001",
		"random_title_enchant_row_id":    "72",
		"seal_flag":                      "1",
	} {
		if got := changed.Extra[key]; got != want {
			t.Fatalf("changed title extra[%q] = %q want %q (extras=%+v)", key, got, want, changed.Extra)
		}
	}
}

// TestOwnerUseDropsInstanceStateWhenTheResultIsNotEquipment is the reverse
// nail: a stackable result must keep discarding the consumed row's extras
// exactly as it did before equipment results were supported.
func TestOwnerUseDropsInstanceStateWhenTheResultIsNotEquipment(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {
				ItemID: 9001,
				Count:  1,
				Extra: map[string]string{
					"item_kind":       "stackable",
					"enchant_card_id": "10008663",
					"value_a":         "10008663",
					"byte_12":         "2",
					"seal_flag":       "1",
				},
			},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Use(ctx, limitedCubeCommand(), limitedCubePolicy()); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	changed := stored.Slots["0:7"]
	if changed.ItemID != 9002 {
		t.Fatalf("changed stackable = %+v", changed)
	}
	for _, key := range equipmentInstanceExtraKeys {
		if _, present := changed.Extra[key]; present {
			t.Fatalf("stackable result kept %q: %+v", key, changed.Extra)
		}
	}
}

func limitedCubeCommand() Command {
	return Command{AccountID: "dnf:19", CharacterID: "19", TicketSlot: 2, TicketItemID: 9000, TargetSlot: 7, TargetItemID: 9001}
}

func limitedCubePolicy() Policy {
	return Policy{
		TicketItemID: 9000,
		Conditions:   []Requirement{{ItemID: 9001, Count: 1}},
		Materials:    []Requirement{{ItemID: 3037, Count: 10}},
		Results: []WeightedResult{{
			Stack: dnfrepo.ItemStack{
				ItemID: 9002,
				Count:  1,
				Extra:  map[string]string{"item_kind": "stackable", "pvf_path": "stackable/bead/result.stk", "stackable_type": "[enchant waste]"},
			},
			Weight: 1000,
		}},
	}
}
