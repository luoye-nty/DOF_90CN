package dnfbridge

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// unequippedStarterStack mirrors what equip.stackFromEntry persists when a
// worn item moves into the bag: the stored row verbatim, plus the same row
// mirrored into Extra as raw_entry_hex.
func unequippedStarterStack(equipped dnfrepo.EquipmentEntry) dnfrepo.ItemStack {
	extra := make(map[string]string, len(equipped.Extra)+2)
	for key, value := range equipped.Extra {
		extra[key] = value
	}
	extra["raw_entry_hex"] = hex.EncodeToString(equipped.RawEntry)
	extra["equipment_slot"] = strconv.Itoa(int(equipped.SlotIndex))
	return dnfrepo.ItemStack{
		ItemID:   equipped.ItemID,
		Count:    1,
		Bind:     equipped.Bind,
		ExpireAt: equipped.ExpireAt,
		RawEntry: append([]byte(nil), equipped.RawEntry...),
		Extra:    extra,
	}
}

func entryDurability(entry currentItemListEntry) uint16 {
	return binary.LittleEndian.Uint16(entry.data[0x0B:0x0D])
}

// A level-1 starter item is stored in the legacy constructor layout, whose
// durability sits one byte below the current row's packed reinforce field.
// Zero-padding that row into the current frame projected the low durability
// byte as a reinforce level, so unequipping a +0 starter item showed +27 on
// the pants, +1 on the coat and +2..+16 on the weapon.
func TestCurrentItemListLegacyStarterRowKeepsZeroReinforce(t *testing.T) {
	cases := []struct {
		name       string
		slot       int16
		itemID     int64
		durability uint16
	}{
		{"pants", 15, 12400, 27},
		{"coat", 13, 10400, 33},
		{"weapon", 11, 101010912, 45},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := []initialEquipmentEntry{{
				Slot:       tc.slot,
				ItemID:     tc.itemID,
				Durability: tc.durability,
				PVFPath:    "equipment/character/common/x.equ",
				RawEntry:   buildInitialEquipmentRawEntry(tc.slot, tc.itemID, tc.durability),
			}}
			record := initialEquipmentRecord("1", entries, time.Now().UTC())
			equipped := record.Entries[strconv.Itoa(int(tc.slot))]

			worn, ok := currentItemListEntryFromEquipment(equipped)
			if !ok {
				t.Fatalf("worn projection refused the starter row")
			}
			if worn.data[0x0A] != 0 {
				t.Fatalf("worn packed byte = 0x%02X, want 0x00", worn.data[0x0A])
			}

			stack := unequippedStarterStack(equipped)
			bag := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 30, stack)
			if bag.data[0x0A] != 0 {
				t.Errorf("bag packed byte = 0x%02X (reinforce +%d), want 0x00",
					bag.data[0x0A], bag.data[0x0A]&0x1F)
			}
			if got := entryDurability(bag); got != tc.durability {
				t.Errorf("bag durability = %d, want %d", got, tc.durability)
			}

			// Re-equipping must not read the mirrored legacy row back as a
			// current row either, otherwise the wrong level becomes sticky.
			reEquipped := dnfrepo.EquipmentEntry{
				SlotIndex: tc.slot,
				ItemID:    stack.ItemID,
				RawEntry:  append([]byte(nil), stack.RawEntry...),
				Extra:     stack.Extra,
			}
			again, ok := currentItemListEntryFromEquipment(reEquipped)
			if !ok {
				t.Fatalf("re-equip projection refused the starter row")
			}
			if again.data[0x0A] != 0 {
				t.Errorf("re-equipped packed byte = 0x%02X (reinforce +%d), want 0x00",
					again.data[0x0A], again.data[0x0A]&0x1F)
			}
			if got := entryDurability(again); got != tc.durability {
				t.Errorf("re-equipped durability = %d, want %d", got, tc.durability)
			}
		})
	}
}

// The legacy branch must carry a real reinforce level and durability across an
// unequip, not merely zero the field.
func TestCurrentItemListLegacyRowPreservesReinforceAndDurability(t *testing.T) {
	const (
		slot       int16  = 11
		itemID     int64  = 101010912
		durability uint16 = 45
	)
	raw := buildInitialEquipmentRawEntry(slot, itemID, durability)
	raw[currentLegacyRowPackedOffset] = 0x0A // +10 reinforce, no attribute bits

	stack := dnfrepo.ItemStack{
		ItemID:   itemID,
		Count:    1,
		RawEntry: raw,
		Extra:    map[string]string{"source": "pvf_create_equipment_list"},
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 30, stack)
	if entry.data[0x0A] != 0x0A {
		t.Errorf("packed byte = 0x%02X, want 0x0A", entry.data[0x0A])
	}
	if got := entryDurability(entry); got != durability {
		t.Errorf("durability = %d, want %d", got, durability)
	}
}

// Explicit Extra metadata still wins over the legacy row.
func TestCurrentItemListLegacyRowYieldsToExplicitExtra(t *testing.T) {
	raw := buildInitialEquipmentRawEntry(11, 101010912, 45)
	raw[currentLegacyRowPackedOffset] = 0x0A

	stack := dnfrepo.ItemStack{
		ItemID:   101010912,
		Count:    1,
		RawEntry: raw,
		Extra: map[string]string{
			"ext_data0":  "3",
			"durability": "12",
		},
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 30, stack)
	if entry.data[0x0A] != 3 {
		t.Errorf("packed byte = 0x%02X, want 0x03", entry.data[0x0A])
	}
	if got := entryDurability(entry); got != 12 {
		t.Errorf("durability = %d, want 12", got)
	}
}

// A row that does not agree with the stack's own item id is not a legacy row.
func TestCurrentItemListStackLegacyRowRejectsMismatchedItemID(t *testing.T) {
	raw := buildInitialEquipmentRawEntry(11, 101010912, 45)
	stack := dnfrepo.ItemStack{ItemID: 999, Count: 1, RawEntry: raw}
	if _, ok := currentItemListStackLegacyRow(stack); ok {
		t.Fatalf("accepted a row whose +1 item id does not match the stack")
	}
}

// The guard itself: a stored row shorter than the current wire size must never
// be padded into one.
func TestCurrentItemListFullRowExtraBytesRefusesShortStoredRow(t *testing.T) {
	short := buildInitialEquipmentRawEntry(15, 12400, 27)
	if len(short) >= currentItemListEntryWireSize {
		t.Fatalf("legacy row is %d bytes, expected a short row", len(short))
	}
	extra := map[string]string{"raw_entry_hex": hex.EncodeToString(short)}
	if got := currentItemListFullRowExtraBytes(extra, "raw_entry_hex"); got != nil {
		t.Fatalf("short stored row was padded to %d bytes", len(got))
	}
	if got := currentItemListEntryFromRaw(nil, extra); got.data != (currentItemListEntry{}).data {
		t.Fatalf("short stored row leaked into the projected entry")
	}

	full := make([]byte, currentItemListEntryWireSize)
	full[0x0A] = 0x0B
	extra["raw_entry_hex"] = hex.EncodeToString(full)
	got := currentItemListFullRowExtraBytes(extra, "raw_entry_hex")
	if len(got) != currentItemListEntryWireSize || got[0x0A] != 0x0B {
		t.Fatalf("a complete stored row was not accepted verbatim")
	}
}

// Every job's real starter gear must reach the bag at +0.
func TestRealScriptPVFStarterEquipmentUnequipsAtZeroReinforce(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify starter equipment against the deployed PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	checked := 0
	for job := byte(0); job <= 12; job++ {
		entries, err := parseInitialCharacterEquipmentFromSource(archive, job)
		if err != nil {
			t.Fatalf("job %d: parse initial equipment: %v", job, err)
		}
		if len(entries) == 0 {
			continue
		}
		record := initialEquipmentRecord("1", entries, time.Now().UTC())
		for _, planned := range entries {
			equipped := record.Entries[strconv.Itoa(int(planned.Slot))]
			bag := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 30, unequippedStarterStack(equipped))
			if bag.data[0x0A] != 0 {
				t.Errorf("job=%d slot=%d item=%d durability=%d: bag packed byte = 0x%02X (reinforce +%d), want 0x00",
					job, planned.Slot, planned.ItemID, planned.Durability, bag.data[0x0A], bag.data[0x0A]&0x1F)
			}
			if got := entryDurability(bag); got != planned.Durability {
				t.Errorf("job=%d slot=%d item=%d: bag durability = %d, want %d",
					job, planned.Slot, planned.ItemID, got, planned.Durability)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatalf("no starter equipment was resolved from the deployed PVF")
	}
	t.Logf("checked %d starter rows", checked)
}
