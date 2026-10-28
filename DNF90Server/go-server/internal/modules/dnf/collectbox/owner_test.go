package collectbox

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerInsertRemovePersistsInventoryAndState(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "7", AccountID: "account-7"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "7",
		Slots:       map[string]dnfrepo.ItemStack{"0:12": {ItemID: 7001, Count: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{Index: 2, Slots: []Slot{{ItemID: 7001, Count: 2}}}
	inserted, err := owner.Insert(ctx, InsertCommand{CharacterID: "7", Definition: definition, SourceSlot: 12, ItemID: 7001})
	if err != nil {
		t.Fatal(err)
	}
	if inserted.SourceRemainingCount != 0 || !inserted.SourceRemoved {
		t.Fatalf("insert result = %+v", inserted)
	}
	state, err := LoadState(ctx, repos.Settings, "7")
	if err != nil {
		t.Fatal(err)
	}
	if got := Snapshot(definition, state); len(got) != 1 || got[0] != 7001 {
		t.Fatalf("snapshot after insert = %v", got)
	}
	removed, err := owner.Remove(ctx, RemoveCommand{CharacterID: "7", Definition: definition, ItemID: 7001, ReturnPlacement: ReturnPlacement{SlotStart: 121, SlotEnd: 176, StackLimit: 999}})
	if err != nil {
		t.Fatal(err)
	}
	if removed.TargetSlot != 121 || removed.TargetStack.Count != 2 {
		t.Fatalf("remove result = %+v", removed)
	}
	state, err = LoadState(ctx, repos.Settings, "7")
	if err != nil {
		t.Fatal(err)
	}
	if got := Snapshot(definition, state); len(got) != 0 {
		t.Fatalf("snapshot after remove = %v", got)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "7")
	if err != nil || !found {
		t.Fatalf("inventory load = found=%t err=%v", found, err)
	}
	if _, occupied := inventory.Slots["0:12"]; occupied {
		t.Fatalf("fully consumed source row should be gone: %+v", inventory.Slots)
	}
	if inventory.Slots["0:121"].ItemID != 7001 || inventory.Slots["0:121"].Count != 2 {
		t.Fatalf("inventory after remove = %+v", inventory.Slots)
	}
}

// 归还同样受"一个容器里同一道具 id 只能有一行"约束：背包里已经有这件道具的行、
// 而这一行不在 PVF 归还页范围内时，不能再开第二行，整次归还被拒绝（道具留在
// 收藏箱里，不静默丢失）。
func TestOwnerRemoveRefusesSecondRowWhenBagAlreadyHoldsItem(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "11", AccountID: "account-11"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "11", Slots: map[string]dnfrepo.ItemStack{
		"0:12": {ItemID: 7011, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{Index: 5, Slots: []Slot{{ItemID: 7011, Count: 2}}}
	if err := SaveState(ctx, repos.Settings, "11", State{Boxes: map[string]map[string]uint32{"5": {"0": 7011}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Remove(ctx, RemoveCommand{
		CharacterID:     "11",
		Definition:      definition,
		ItemID:          7011,
		ReturnPlacement: ReturnPlacement{SlotStart: 121, SlotEnd: 176, StackLimit: 999},
	}); !errors.Is(err, ErrInventoryFull) {
		t.Fatalf("remove error = %v, want ErrInventoryFull", err)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "11")
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Slots) != 1 || inventory.Slots["0:12"].Count != 1 {
		t.Fatalf("refused return mutated the bag: %+v", inventory.Slots)
	}
	if _, occupied := inventory.Slots["0:121"]; occupied {
		t.Fatalf("refused return opened a second row: %+v", inventory.Slots)
	}
	state, err := LoadState(ctx, repos.Settings, "11")
	if err != nil {
		t.Fatal(err)
	}
	if got := Snapshot(definition, state); len(got) != 1 || got[0] != 7011 {
		t.Fatalf("refused return must keep the box item, snapshot = %v", got)
	}
}

func TestOwnerRemoveReturnsOnlyToPVFMaterialPage(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "10", AccountID: "account-10"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "10", Slots: map[string]dnfrepo.ItemStack{
		"0:15":  {ItemID: 7010, Count: 8}, // stale pre-fix equipment-page stack: must not be reused.
		"0:121": {ItemID: 7010, Count: 990},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{Index: 4, Slots: []Slot{{ItemID: 7010, Count: 1}}}
	if err := SaveState(ctx, repos.Settings, "10", State{Boxes: map[string]map[string]uint32{"4": {"0": 7010}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	result, err := owner.Remove(ctx, RemoveCommand{
		CharacterID:     "10",
		Definition:      definition,
		ItemID:          7010,
		ReturnPlacement: ReturnPlacement{SlotStart: 121, SlotEnd: 176, StackLimit: 999},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetSlot != 121 || result.TargetStack.Count != 991 {
		t.Fatalf("remove target = %+v, want material page stack slot 121", result)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "10")
	if err != nil || !found {
		t.Fatalf("inventory load = found=%t err=%v", found, err)
	}
	if inventory.Slots["0:15"].Count != 8 || inventory.Slots["0:121"].Count != 991 {
		t.Fatalf("inventory after material return = %+v", inventory.Slots)
	}
}

func TestOwnerExtendConsumesTicketAndPersistsExpiry(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "8", AccountID: "account-8"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "8", Slots: map[string]dnfrepo.ItemStack{"0:12": {ItemID: 10007344, Count: 2}}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2034, 12, 30, 6, 0, 0, 0, time.Local)
	definition := Definition{Index: 1, BaseExpirationDate: "2034-12-30 06:00:00", MaxExpirationDate: "2035-02-01 06:00:00", ExtendItemID: 10007344, ExtendItemCount: 1, Slots: []Slot{{ItemID: 1, Count: 1}}}
	if err := SaveState(ctx, repos.Settings, "8", State{Expiration: map[string]time.Time{"1": base}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	result, err := owner.Extend(ctx, ExtendCommand{CharacterID: "8", Definition: definition, ItemID: 10007344, UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Expiration.Equal(base.Add(24*time.Hour)) || result.SourceRemaining != 1 {
		t.Fatalf("result = %+v", result)
	}
	state, err := LoadState(ctx, repos.Settings, "8")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Expiration["1"].Equal(base.Add(24 * time.Hour)) {
		t.Fatalf("state = %+v", state)
	}
}

func TestOwnerInsertDefaultsZeroRequirementToOne(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "9", AccountID: "account-9"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "9", Slots: map[string]dnfrepo.ItemStack{"0:12": {ItemID: 7002, Count: 1}}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Insert(ctx, InsertCommand{
		CharacterID: "9",
		Definition:  Definition{Index: 3, Slots: []Slot{{ItemID: 7002}}},
		SourceSlot:  12,
		ItemID:      7002,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequiredCount != 1 || !result.SourceRemoved || result.SourceRemainingCount != 0 {
		t.Fatalf("insert result = %+v", result)
	}
}

func TestOwnerSelectPersistsPerCharacterBox(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "11", AccountID: "account-11"},
		{CharacterID: "12", AccountID: "account-11"},
	} {
		if err := repos.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	state := State{Boxes: map[string]map[string]uint32{"2": {"0": 10007548}}}
	raw, err := EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-11",
		Metadata:  map[string]string{MetadataKey: raw},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Select(ctx, SelectCommand{CharacterID: "11", BoxIndex: 2}); err != nil {
		t.Fatal(err)
	}
	// Idempotent: the same box must not rewrite the record.
	if err := owner.Select(ctx, SelectCommand{CharacterID: "11", BoxIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Select(ctx, SelectCommand{CharacterID: "12", BoxIndex: 5}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := LoadAccountState(ctx, repos.Account, "account-11")
	if err != nil || !found {
		t.Fatalf("load state found=%t err=%v", found, err)
	}
	if got := stored.SelectedBox("11"); got != 2 {
		t.Fatalf("character 11 selection = %d, want 2", got)
	}
	if got := stored.SelectedBox("12"); got != 5 {
		t.Fatalf("character 12 selection = %d, want 5", got)
	}
	// The recorded box survives the boxes themselves being re-read, and the
	// item state is untouched.
	if got := Snapshot(Definition{Index: 2, Slots: []Slot{{ItemID: 10007548}}}, stored); len(got) != 1 || got[0] != 10007548 {
		t.Fatalf("boxes after select = %v", got)
	}
	if err := owner.Select(ctx, SelectCommand{CharacterID: "11", BoxIndex: 0}); !errors.Is(err, ErrDefinitionInvalid) {
		t.Fatalf("zero box index error = %v", err)
	}
}
