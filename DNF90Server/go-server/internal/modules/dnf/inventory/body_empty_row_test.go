package inventory

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// An op14 row that means "this slot is now empty" has to carry item id -1.
// The current EXE's op14 handler sub_1D73120 removes a slot only when the
// row's id field is -1 (the compare at 0x1D73912); every other id, including
// 0, falls through to the in-place update path, and that path refuses to
// change an object's item id unless the object type is 2 (0x1D73AB2). An
// equipment object is not type 2, so a 0 row was discarded without a trace and
// the client kept painting the old piece until the next full list refresh --
// which is why 装备再生 looked like it returned the source item and only
// corrected itself after re-selecting the character.
func TestCommonItemListEntryEncodesAnEmptySlotAsMinusOne(t *testing.T) {
	for _, empty := range []dnfrepo.ItemStack{
		{},
		{ItemID: -1},
		{ItemID: 0, Count: 7},
	} {
		var writer packetWriter
		writeCommonItemListEntry(&writer, listTypeMain, 21, empty)
		row := writer.bytes()
		if len(row) != currentItemListEntrySize {
			t.Fatalf("row len=%d want %d", len(row), currentItemListEntrySize)
		}
		if slot := binary.LittleEndian.Uint16(row[0x00:0x02]); slot != 21 {
			t.Fatalf("slot=%d want 21", slot)
		}
		if id := binary.LittleEndian.Uint32(row[0x02:0x06]); id != ^uint32(0) {
			t.Fatalf("stack %+v encoded id=%#x want 0xFFFFFFFF", empty, id)
		}
		if count := binary.LittleEndian.Uint32(row[0x06:0x0A]); count != 0 {
			t.Fatalf("stack %+v encoded count=%d want 0", empty, count)
		}
	}

	// A real stack is untouched by the empty-row branch.
	var writer packetWriter
	writeCommonItemListEntry(&writer, listTypeMain, 21, dnfrepo.ItemStack{ItemID: 114030107, Count: 1})
	row := writer.bytes()
	if id := binary.LittleEndian.Uint32(row[0x02:0x06]); id != 114030107 {
		t.Fatalf("occupied row id=%d", id)
	}

	// The character wallet is the one legitimate zero-id row: main-list slot 0
	// with the balance in the count field. Rewriting it as an empty marker
	// would tell the client to drop the gold display entirely.
	var wallet packetWriter
	writeCommonItemListEntry(&wallet, listTypeMain, 0, dnfrepo.ItemStack{Count: 969273855})
	walletRow := wallet.bytes()
	if id := binary.LittleEndian.Uint32(walletRow[0x02:0x06]); id != 0 {
		t.Fatalf("wallet row id=%#x want 0", id)
	}
	if gold := binary.LittleEndian.Uint32(walletRow[0x06:0x0A]); gold != 969273855 {
		t.Fatalf("wallet row balance=%d", gold)
	}
}
