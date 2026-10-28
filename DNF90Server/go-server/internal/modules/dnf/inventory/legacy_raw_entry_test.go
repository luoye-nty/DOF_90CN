package inventory

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// buildLegacyStarterRow reproduces the 43-byte legacy constructor summary the
// bridge stores for a level-1 starter item: u8 slot, u32 item id, u32 create
// value, u8 packed reinforce, u16 durability.
func buildLegacyStarterRow(slot byte, itemID uint32, packed byte, durability uint16) []byte {
	raw := make([]byte, 43)
	raw[0] = slot
	binary.LittleEndian.PutUint32(raw[1:5], itemID)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	raw[9] = packed
	binary.LittleEndian.PutUint16(raw[10:12], durability)
	return raw
}

// Reading a current offset out of a legacy row projected the low durability
// byte as the packed reinforce byte, so an unequipped +0 starter item was
// treated as reinforced by every upgrade, amplify and repair calculation.
func TestUpgradeLevelIgnoresShiftedLegacyRowDurability(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID:   12400,
		Count:    1,
		RawEntry: buildLegacyStarterRow(15, 12400, 0, 27),
		Extra: map[string]string{
			"source":         "pvf_create_equipment_list",
			"durability":     "27",
			"max_durability": "27",
		},
	}
	if got := packedUpgradeAttrOf(stack); got != 0 {
		t.Errorf("packedUpgradeAttrOf = 0x%02X, want 0x00", got)
	}
	if got := upgradeLevelOf(stack); got != 0 {
		t.Errorf("upgradeLevelOf = %d, want 0", got)
	}
	if ampType, ampValue := upgradeAmplifyState(stack); ampType != 0 || ampValue != 0 {
		t.Errorf("upgradeAmplifyState = (%d, %d), want (0, 0)", ampType, ampValue)
	}
}

// The proved legacy position for the packed byte is +9, and it must still be
// read from there.
func TestUpgradeLevelReadsLegacyRowPackedByte(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID:   101010912,
		Count:    1,
		RawEntry: buildLegacyStarterRow(11, 101010912, 0x2A, 45),
	}
	if got := packedUpgradeAttrOf(stack); got != 0x2A {
		t.Errorf("packedUpgradeAttrOf = 0x%02X, want 0x2A", got)
	}
	if got := upgradeLevelOf(stack); got != 10 {
		t.Errorf("upgradeLevelOf = %d, want 10", got)
	}
}

// A complete current row keeps reading its own offsets.
func TestUpgradeLevelReadsCurrentRowPackedByte(t *testing.T) {
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(raw[2:6], 101010912)
	raw[0x0A] = 0x0C
	raw[0x13] = 3
	binary.LittleEndian.PutUint16(raw[0x14:0x16], 7)

	stack := dnfrepo.ItemStack{ItemID: 101010912, Count: 1, RawEntry: raw}
	if got := upgradeLevelOf(stack); got != 12 {
		t.Errorf("upgradeLevelOf = %d, want 12", got)
	}
	ampType, ampValue := upgradeAmplifyState(stack)
	if ampType != 3 || ampValue != 7 {
		t.Errorf("upgradeAmplifyState = (%d, %d), want (3, 7)", ampType, ampValue)
	}
}

// A row whose +1 item id disagrees with the stack is not a legacy row.
func TestLegacyRawEntryRejectsMismatchedItemID(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID:   999,
		Count:    1,
		RawEntry: buildLegacyStarterRow(15, 12400, 0x1F, 27),
	}
	if _, ok := legacyRawEntry(stack); ok {
		t.Fatalf("accepted a row whose +1 item id does not match the stack")
	}
	if got := packedUpgradeAttrOf(stack); got != 0 {
		t.Errorf("packedUpgradeAttrOf = 0x%02X, want 0x00", got)
	}
}
