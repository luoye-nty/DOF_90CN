package dnfbridge

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The op611 machine-repair ACK is ignored field-for-field by the current EXE
// (sub_1D0E670 reads nothing), so the spent gold only reaches the inventory
// display through this narrow list-0 op14. Pin its shape: one reserved
// wallet row at slot 0 with item id 0 and the post-repair balance.
func TestCurrentExpertJobGoldRefreshBodyIsOneReservedWalletRow(t *testing.T) {
	const goldAfter int64 = 600

	body := buildCurrentItemUpdateBody(
		dnfrepo.MainInventoryListType,
		[]currentItemListEntry{currentGoldWalletItemListEntry(goldAfter)},
	)

	if want := 3 + currentItemListEntryWireSize; len(body) != want {
		t.Fatalf("body length = %d, want %d", len(body), want)
	}
	if body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("list type = %d, want %d", body[0], dnfrepo.MainInventoryListType)
	}
	if count := binary.LittleEndian.Uint16(body[1:3]); count != 1 {
		t.Fatalf("entry count = %d, want 1", count)
	}
	row := body[3:]
	if slot := int16(binary.LittleEndian.Uint16(row[0:2])); slot != currentGoldWalletReservedSlot {
		t.Fatalf("slot = %d, want %d", slot, currentGoldWalletReservedSlot)
	}
	if itemID := binary.LittleEndian.Uint32(row[2:6]); itemID != 0 {
		t.Fatalf("item id = %d, want 0 for the reserved wallet row", itemID)
	}
	if amount := binary.LittleEndian.Uint32(row[6:10]); int64(amount) != goldAfter {
		t.Fatalf("wallet amount = %d, want %d", amount, goldAfter)
	}
}
