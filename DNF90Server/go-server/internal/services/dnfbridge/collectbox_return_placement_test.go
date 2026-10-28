package dnfbridge

import (
	"testing"

	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
)

func TestCollectboxReturnPlacementUsesPVFMaterialPage(t *testing.T) {
	placement, err := collectboxReturnPlacementFromPVF(dungeonDropItemDefinition{
		ItemID: 7010, Kind: dungeonDropItemStackable, StackableType: "[material]",
		SlotStart: 121, SlotEnd: 176, StackLimit: 999,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := dnfcollectbox.ReturnPlacement{SlotStart: 121, SlotEnd: 176, StackLimit: 999}
	if placement != want {
		t.Fatalf("placement = %+v, want %+v", placement, want)
	}
}

func TestCollectboxReturnPlacementRejectsEquipmentPage(t *testing.T) {
	_, err := collectboxReturnPlacementFromPVF(dungeonDropItemDefinition{
		ItemID: 7011, Kind: dungeonDropItemEquipment, SlotStart: 9, SlotEnd: 64,
	})
	if err == nil {
		t.Fatal("equipment placement was accepted")
	}
}
