package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfclonetitle "longheng.io/server/internal/modules/dnf/clonetitle"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCloneTitleStateReloadsForReselectAndTownProjection(t *testing.T) {
	const (
		characterID  uint16 = 19
		cloneTitleID uint32 = 400330105
		realTitleID  int64  = 400330106
	)
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Slot:        0,
		Name:        "Actor19",
		Job:         "11",
		Level:       90,
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"100:0": {ItemID: int64(cloneTitleID), Count: 1},
		},
	}); err != nil {
		t.Fatalf("save title book: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"13": {
				SlotIndex: 13,
				ItemID:    realTitleID,
				RawEntry:  buildInitialEquipmentRawEntry(13, realTitleID, 0),
				Extra:     map[string]string{"current_exe_equipment_type": "13"},
			},
		},
	}); err != nil {
		t.Fatalf("save worn title: %v", err)
	}

	owner, err := dnfclonetitle.NewOwner(repos)
	if err != nil {
		t.Fatalf("create clone-title owner: %v", err)
	}
	if _, err := owner.Apply(ctx, dnfclonetitle.Command{
		AccountID:           "account-1",
		SelectedCharacterID: characterID,
		ItemID:              int32(cloneTitleID),
	}); err != nil {
		t.Fatalf("persist clone-title selection: %v", err)
	}

	service, _, _ := newGameplayDomainBridgeFixture(repos)
	selectedID, _, selected, found, slot := service.resolveSelectedCharacter(
		ctx,
		&gameSession{},
		make([]byte, 16),
	)
	if !found || selectedID != characterID || slot != 0 {
		t.Fatalf("reselect character id=%d found=%t slot=%d", selectedID, found, slot)
	}
	if got := currentCloneTitleItemID(selected, true); got != cloneTitleID {
		t.Fatalf("reselect clone-title state=%d want=%d", got, cloneTitleID)
	}

	newSession := &gameSession{selectedCharacterID: characterID}
	loadedID, loadedName, loaded, loadedFound := service.selectedCharacterForEnter(ctx, newSession)
	if !loadedFound || loadedID != characterID {
		t.Fatalf("town reload character id=%d found=%t", loadedID, loadedFound)
	}
	if got := currentCloneTitleItemID(loaded, true); got != cloneTitleID {
		t.Fatalf("town reload clone-title state=%d want=%d", got, cloneTitleID)
	}

	tail := buildCurrentSceneObjectEntryTail(loaded, true)
	equipEnd, ok := currentSceneObjectEquipSummaryEnd(tail, 6)
	if !ok {
		t.Fatalf("parse reloaded mode0 tail: %x", tail)
	}
	if got := binary.LittleEndian.Uint32(tail[equipEnd : equipEnd+4]); got != cloneTitleID {
		t.Fatalf("reloaded mode0 clone-title state=%d want=%d", got, cloneTitleID)
	}

	mode1 := service.buildCurrentSelectedUserInfoMode1Body(ctx, newSession, nil, loaded, true, loadedID)
	reader := csharpLegacyUserInfoReader{ctx: ctx, characterID: "19", service: service}
	rows := currentMode1EquipmentCreateRows(reader.currentMode1EquipmentObjectRows())
	if len(rows) != 1 || rows[0].itemID != uint32(realTitleID) {
		t.Fatalf("reloaded mode1 title row=%+v want=%d", rows, realTitleID)
	}
	cloneStateOffset := currentMode1CreateRowsOffset + currentMode1EquipmentCreateRowWireSizeFor(rows[0])
	if got := binary.LittleEndian.Uint32(mode1[cloneStateOffset : cloneStateOffset+4]); got != cloneTitleID {
		t.Fatalf("reloaded mode1 clone-title state=%d want=%d", got, cloneTitleID)
	}
	if loadedName != "Actor19" {
		t.Fatalf("town reload name=%q", loadedName)
	}
}
