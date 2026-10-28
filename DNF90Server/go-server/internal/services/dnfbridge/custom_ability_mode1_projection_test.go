package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentMode1EquippedCustomAbilityFallsBackToCurrentItemRaw(t *testing.T) {
	repos := testRepositoryGroup()
	rawWeapon := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(rawWeapon[0x00:0x02], 11)
	binary.LittleEndian.PutUint32(rawWeapon[0x02:0x06], 900001)
	binary.LittleEndian.PutUint32(rawWeapon[0x06:0x0a], 123456)
	binary.LittleEndian.PutUint16(rawWeapon[0x0b:0x0d], 27)
	rawWeapon[currentEpicProductionAbilityTailOffset+0] = 4
	rawWeapon[currentEpicProductionAbilityTailOffset+1] = 20
	if err := repos.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"11": {
				SlotIndex: 11,
				ItemID:    900001,
				RawEntry:  rawWeapon,
				Extra: map[string]string{
					"current_exe_equipment_type": "12",
					"mode1_create_enabled":       "1",
				},
			},
		},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	service := Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
	reader := csharpLegacyUserInfoReader{ctx: context.Background(), service: &service, characterID: "77"}

	rows := currentMode1EquipmentCreateRows(reader.currentMode1EquipmentObjectRows())
	if len(rows) != 1 {
		t.Fatalf("mode1 equipment rows = %d, want 1", len(rows))
	}
	want := []byte{4, 20, 0, 0, 0}
	if !bytes.Equal(rows[0].lateRawE, want) {
		t.Fatalf("mode1 late rawE = %x, want %x", rows[0].lateRawE, want)
	}

	var writer packetWriter
	writeCurrentMode1EquipmentCreateRow(&writer, rows[0])
	wire := writer.bytes()
	blockStart := len(wire) - len(want) - currentMode1EquipmentRawBlockHeaderSize
	if blockStart < 0 || binary.LittleEndian.Uint32(wire[blockStart:blockStart+4]) != uint32(len(want)) {
		t.Fatalf("mode1 late rawE block invalid: %x", wire)
	}
	if got := wire[blockStart+4:]; !bytes.Equal(got, want) {
		t.Fatalf("mode1 late rawE wire = %x, want %x", got, want)
	}
}
