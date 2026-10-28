package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestBuildCurrentActorMode0AppearanceSnapshotGoldenRuntimeSlots(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	entries := make(map[string]dnfrepo.EquipmentEntry, currentActorMode0AppearanceSlotCount)
	for slot := 0; slot <= 11; slot++ {
		entries[strconv.Itoa(slot)] = dnfrepo.EquipmentEntry{
			SlotIndex: int16(slot),
			ItemID:    int64(414500098 + slot),
		}
	}
	entries["weapon"] = dnfrepo.EquipmentEntry{SlotIndex: 12, ItemID: 414010046}
	entries["title"] = dnfrepo.EquipmentEntry{SlotIndex: 13, ItemID: 400330121}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: entries}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}

	body, found, err := buildCurrentActorMode0AppearanceSnapshot(ctx, repos.Equipment, "19")
	if err != nil {
		t.Fatalf("build appearance snapshot: %v", err)
	}
	if !found {
		t.Fatalf("equipment record not found")
	}
	const wantHex = "0e" +
		"0002c5b4180000000000000000000000000000" +
		"0103c5b4180000000000000000000000000000" +
		"0204c5b4180000000000000000000000000000" +
		"0305c5b4180000000000000000000000000000" +
		"0406c5b4180000000000000000000000000000" +
		"0507c5b4180000000000000000000000000000" +
		"0608c5b4180000000000000000000000000000" +
		"0709c5b4180000000000000000000000000000" +
		"080ac5b4180000000000000000000000000000" +
		"090bc5b4180000000000000000000000000000" +
		"0a0cc5b4180000000000000000000000000000" +
		"0b0dc5b4180000000000000000000000000000" +
		"0cbe4aad180000000000000000000000000000" +
		"0d898ddc170000000000000000000000000000"
	want := mustDecodeCurrentActorMode0AppearanceGolden(t, wantHex)
	if !bytes.Equal(body, want) {
		t.Fatalf("appearance snapshot = %x, want %x", body, want)
	}
	if got, wantLen := len(body), 1+currentActorMode0AppearanceSlotCount*currentActorMode0AppearanceRowBytes; got != wantLen {
		t.Fatalf("appearance snapshot len = %d, want %d", got, wantLen)
	}
}

func TestBuildCurrentActorMode0AppearanceSnapshotGoldenClearsUnwornSlots(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"coat": {SlotIndex: 3, ItemID: 414500098},
		},
	}); err != nil {
		t.Fatalf("save worn equipment: %v", err)
	}
	worn, found, err := buildCurrentActorMode0AppearanceSnapshot(ctx, repos.Equipment, "19")
	if err != nil || !found {
		t.Fatalf("build worn appearance snapshot found=%v err=%v", found, err)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, worn, 3); got != 414500098 {
		t.Fatalf("worn slot 3 item id = %d, want 414500098", got)
	}

	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19"}); err != nil {
		t.Fatalf("save empty equipment: %v", err)
	}
	body, found, err := buildCurrentActorMode0AppearanceSnapshot(ctx, repos.Equipment, "19")
	if err != nil || !found {
		t.Fatalf("build empty appearance snapshot found=%v err=%v", found, err)
	}
	const wantHex = "0e" +
		"00ffffffff0000000000000000000000000000" +
		"01ffffffff0000000000000000000000000000" +
		"02ffffffff0000000000000000000000000000" +
		"03ffffffff0000000000000000000000000000" +
		"04ffffffff0000000000000000000000000000" +
		"05ffffffff0000000000000000000000000000" +
		"06ffffffff0000000000000000000000000000" +
		"07ffffffff0000000000000000000000000000" +
		"08ffffffff0000000000000000000000000000" +
		"09ffffffff0000000000000000000000000000" +
		"0affffffff0000000000000000000000000000" +
		"0bffffffff0000000000000000000000000000" +
		"0cffffffff0000000000000000000000000000" +
		"0dffffffff0000000000000000000000000000"
	want := mustDecodeCurrentActorMode0AppearanceGolden(t, wantHex)
	if !bytes.Equal(body, want) {
		t.Fatalf("empty appearance snapshot = %x, want %x", body, want)
	}
}

func TestCurrentActorMode0AppearanceSlotKeepsRuntimeAndMapsLegacyPVF(t *testing.T) {
	record := dnfrepo.EquipmentRecord{Entries: map[string]dnfrepo.EquipmentEntry{
		"runtime-slot-11": {SlotIndex: 11, ItemID: 414500109},
		"legacy-weapon": {
			SlotIndex: 11,
			ItemID:    414010046,
			Extra:     map[string]string{"source": "pvf_create_equipment_list"},
		},
		"legacy-coat": {
			SlotIndex: 13,
			ItemID:    100070550,
			Extra:     map[string]string{"source": "pvf_create_equipment_list"},
		},
		"explicit-avatar": {
			SlotIndex: 14,
			ItemID:    414500098,
			Extra:     map[string]string{"appearance_slot": "3"},
		},
		"explicit-title": {
			SlotIndex: 13,
			ItemID:    400330121,
			Extra: map[string]string{
				"source":                     "pvf_create_equipment_list",
				"current_exe_equipment_type": "13",
			},
		},
	}}
	body, err := buildCurrentActorMode0AppearanceSnapshotFromEquipment(record)
	if err != nil {
		t.Fatalf("build mixed appearance snapshot: %v", err)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, body, 11); got != 414500109 {
		t.Fatalf("direct runtime slot 11 item id = %d, want 414500109", got)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, body, 3); got != 414500098 {
		t.Fatalf("explicit appearance slot 3 item id = %d, want 414500098", got)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, body, 12); got != 414010046 {
		t.Fatalf("legacy PVF weapon actor slot 12 item id = %d, want 414010046", got)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, body, 13); got != 400330121 {
		t.Fatalf("explicit title actor slot 13 item id = %d, want 400330121", got)
	}
}

func TestCurrentActorMode0AppearanceIgnoresStaleDualWeaponOffhandMarker(t *testing.T) {
	record := dnfrepo.EquipmentRecord{Entries: map[string]dnfrepo.EquipmentEntry{
		"avatar": {SlotIndex: 3, ItemID: 414500098},
		"12": {
			SlotIndex: 12,
			ItemID:    27093,
			Extra: map[string]string{
				"current_exe_equipment_type": "24",
				"current_exe_runtime_move":   "1",
			},
		},
		"24": {
			SlotIndex: 24,
			ItemID:    28228,
			Extra: map[string]string{
				"current_exe_equipment_type": "12",
				"current_exe_runtime_move":   "1",
			},
		},
	}}
	body, err := buildCurrentActorMode0AppearanceSnapshotFromEquipment(record)
	if err != nil {
		t.Fatalf("build repaired dual-weapon appearance: %v", err)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, body, 3); got != 414500098 {
		t.Fatalf("avatar slot 3 item id=%d, want 414500098", got)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, body, 12); got != 27093 {
		t.Fatalf("main weapon slot 12 item id=%d, want 27093", got)
	}
}

func TestBuildCurrentActorMode0AppearanceSummaryProjectsWeaponReinforcement(t *testing.T) {
	tests := []struct {
		name       string
		weapon     dnfrepo.EquipmentEntry
		wantPacked int64
	}{
		{
			name: "reinforced weapon",
			weapon: dnfrepo.EquipmentEntry{
				SlotIndex: 12,
				ItemID:    101030741,
				Extra:     map[string]string{"ext_data0": "5"},
			},
			wantPacked: 10,
		},
		{
			name: "unreinforced weapon",
			weapon: dnfrepo.EquipmentEntry{
				SlotIndex: 12,
				ItemID:    101030741,
			},
			wantPacked: 0,
		},
		{
			name: "reinforcement value is bounded to seven bits",
			weapon: dnfrepo.EquipmentEntry{
				SlotIndex: 12,
				ItemID:    101030741,
				Extra:     map[string]string{"ext_data0": "255"},
			},
			wantPacked: 254,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := dnfrepo.EquipmentRecord{Entries: map[string]dnfrepo.EquipmentEntry{
				"weapon": tt.weapon,
				"coat": {
					SlotIndex: 3,
					ItemID:    414500098,
					Extra:     map[string]string{"ext_data0": "9"},
				},
				"title": {
					SlotIndex: 13,
					ItemID:    400330121,
					Extra:     map[string]string{"ext_data0": "7"},
				},
			}}

			rows, err := buildCurrentActorMode0AppearanceSummaryFromEquipment(record)
			if err != nil {
				t.Fatalf("build appearance summary: %v", err)
			}
			if got := rows[currentActorMode0WeaponSlot].PackedFlags; got != tt.wantPacked {
				t.Fatalf("weapon packed flags = %d, want %d", got, tt.wantPacked)
			}
			if got := rows[3].PackedFlags; got != 0 {
				t.Fatalf("non-weapon packed flags = %d, want 0", got)
			}
			if got := rows[13].PackedFlags; got != 0 {
				t.Fatalf("title packed flags = %d, want 0", got)
			}
		})
	}
}

func mustDecodeCurrentActorMode0AppearanceGolden(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return decoded
}

func currentActorMode0AppearanceGoldenItemID(t *testing.T, body []byte, slot int) uint32 {
	t.Helper()
	if slot < 0 || slot >= currentActorMode0AppearanceSlotCount {
		t.Fatalf("slot %d outside appearance snapshot", slot)
	}
	offset := 1 + slot*currentActorMode0AppearanceRowBytes
	if len(body) < offset+5 {
		t.Fatalf("appearance snapshot len = %d, cannot read slot %d", len(body), slot)
	}
	if body[offset] != byte(slot) {
		t.Fatalf("appearance row %d carries slot %d", slot, body[offset])
	}
	return binary.LittleEndian.Uint32(body[offset+1 : offset+5])
}

func TestBuildCurrentActorMode0AppearanceSummaryAppendsGuardianShieldSlot24(t *testing.T) {
	record := dnfrepo.EquipmentRecord{Entries: map[string]dnfrepo.EquipmentEntry{
		"coat":   {SlotIndex: 3, ItemID: 414500098},
		"weapon": {SlotIndex: 12, ItemID: 101030741},
		"24": {
			SlotIndex: 24,
			ItemID:    28228,
			Extra: map[string]string{
				"current_exe_equipment_type":      "24",
				"current_exe_runtime_move":        "1",
				"guardian_shield_deck_projection": "1",
			},
		},
	}}
	rows, err := buildCurrentActorMode0AppearanceSummaryFromEquipment(record)
	if err != nil {
		t.Fatalf("build appearance summary: %v", err)
	}
	if got, want := len(rows), currentActorMode0AppearanceSlotCount+1; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	shield := rows[currentActorMode0AppearanceSlotCount]
	if shield.Slot != 24 || shield.ItemIDOrIcon != 28228 {
		t.Fatalf("shield row = %+v, want slot 24 item 28228", shield)
	}
	if got := rows[3].ItemIDOrIcon; got != 414500098 {
		t.Fatalf("coat slot 3 shifted = %+v", rows[3])
	}
	if got := rows[12].ItemIDOrIcon; got != 101030741 {
		t.Fatalf("weapon slot 12 shifted = %+v", rows[12])
	}
}

func TestBuildCurrentActorMode0AppearanceSummarySkipsNonGuardianSlot24(t *testing.T) {
	// A Blade Dancer off-hand uses the same actor slot 24 but is not the deck
	// projection, so it must not appear as an extra appearance row.
	record := dnfrepo.EquipmentRecord{Entries: map[string]dnfrepo.EquipmentEntry{
		"24": {
			SlotIndex: 24,
			ItemID:    414500098,
			Extra: map[string]string{
				"current_exe_equipment_type": "24",
				"current_exe_runtime_move":   "1",
			},
		},
	}}
	rows, err := buildCurrentActorMode0AppearanceSummaryFromEquipment(record)
	if err != nil {
		t.Fatalf("build appearance summary: %v", err)
	}
	if got, want := len(rows), currentActorMode0AppearanceSlotCount; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
}
