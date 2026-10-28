package guild

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestTransferCargoPreservesEquipmentAndStackCounts(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	g := &Guild{CargoLimit: 8, Members: map[string]Member{"1": {CharacterID: "1", Name: "master"}}}
	inv := &dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{
		"0:12": {ItemID: 700, Count: 3, RawEntry: make([]byte, 119), Extra: map[string]string{"item_kind": "stackable"}},
	}}
	rule := CargoRule{ItemID: 700, Allowed: true, StackLimit: 100, SlotStart: 9, SlotEnd: 64}
	if _, err := g.TransferCargo("1", inv, true, 12, 0, 700, 2, rule, now); err != nil {
		t.Fatal(err)
	}
	if inv.Slots["0:12"].Count != 1 || g.Cargo[0].Count != 2 || len(g.CargoHistory) != 1 {
		t.Fatalf("deposit inventory=%+v cargo=%+v history=%d", inv.Slots, g.Cargo, len(g.CargoHistory))
	}
	if _, err := g.TransferCargo("1", inv, false, -1, 0, 700, 1, rule, now); err != nil {
		t.Fatal(err)
	}
	if inv.Slots["0:12"].Count != 2 || g.Cargo[0].Count != 1 {
		t.Fatalf("withdraw inventory=%+v cargo=%+v", inv.Slots, g.Cargo)
	}
}

func TestExtendAreaRequiresNextLevelAndDebitsOnce(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	r := NewRegistry()
	g, err := r.Create(dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "a", Name: "master"}, "guild", now)
	if err != nil {
		t.Fatal(err)
	}
	g.GuardianAP, g.GuardianGP = 20000, 10000
	costs := map[byte]GuardianCost{1: {AP: 5000, GP: 2000}, 2: {AP: 10000, GP: 8000}}
	if err := r.ExtendArea("1", 1, costs, now); err != nil {
		t.Fatal(err)
	}
	if g.AreaLevel != 1 || g.GuardianAP != 15000 || g.GuardianGP != 8000 {
		t.Fatalf("first extension=%+v", g)
	}
	if err := r.ExtendArea("1", 1, costs, now); err == nil {
		t.Fatal("duplicate extension accepted")
	}
}
