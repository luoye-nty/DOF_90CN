package dnfbridge

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestCurrentItemListEntryFromStackWritesRandomTitleEnchantWithoutAStoredRow
// pins the 2026-09-27 fix. A 0x77 row has two possible bases: a stored full row
// (stack.RawEntry or raw_entry_hex) or a field-by-field build over an all-zero
// entry. The random title enchant overlay used to run on the raw base only, so
// every row the server synthesised itself silently lost +0x61/+0x63 — which is
// exactly the row an [upgrade limit cube] equipment template swap writes, since
// the owner deliberately clears RawEntry when the template changes.
//
// The same failure mode as 「称号附魔进图就消失」 (2026-09-25): a durable Extra
// field that only one projection path knows how to render.
func TestCurrentItemListEntryFromStackWritesRandomTitleEnchantWithoutAStoredRow(t *testing.T) {
	enchanted := dnfrepo.ItemStack{
		ItemID: 400330118,
		Count:  1,
		Bind:   true,
		Extra: map[string]string{
			"item_kind":                      "equipment",
			"equipment_type":                 "[title name]",
			"pvf_path":                       "equipment/character/common/title/chn_400330118.equ",
			"quality_seed":                   "959062881",
			"value_a":                        "0",
			"enchant_card_id":                "0",
			"byte_12":                        "0",
			"enchant_upgrade_count":          "0",
			"random_title_enchant_config_id": "10001",
			"random_title_enchant_row_id":    "72",
			"seal_flag":                      "1",
		},
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 18, enchanted)
	if got := binary.LittleEndian.Uint16(entry.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset]); got != 10001 {
		t.Fatalf("synthesised row config=%d want 10001; row=%x", got, entry.data)
	}
	if got := binary.LittleEndian.Uint16(entry.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd]); got != 72 {
		t.Fatalf("synthesised row id=%d want 72; row=%x", got, entry.data)
	}
	if !currentItemListEntryCarriesRandomTitleEnchant(entry) {
		t.Fatalf("synthesised row does not report carrying the enchant; row=%x", entry.data)
	}
	// The ordinary bead enchant half of the same block must stay zeroed, exactly
	// as setRandomTitleEnchantFields persisted it.
	for offset := 0x0E; offset <= 0x12; offset++ {
		if entry.data[offset] != 0 {
			t.Fatalf("synthesised row +0x%02X=%d want 0; row=%x", offset, entry.data[offset], entry.data)
		}
	}
	if got := binary.LittleEndian.Uint32(entry.data[6:10]); got != 959062881 {
		t.Fatalf("synthesised row quality seed=%d want 959062881; row=%x", got, entry.data)
	}
	if entry.data[0x0D] != 1 {
		t.Fatalf("synthesised row seal flag=%d want 1; row=%x", entry.data[0x0D], entry.data)
	}

	// Reverse nail: a title with no enchant keeps the whole block at zero, and a
	// half-written pair must not be rendered as an enchant.
	for _, extra := range []map[string]string{
		{"item_kind": "equipment"},
		{"item_kind": "equipment", "random_title_enchant_config_id": "10001"},
		{"item_kind": "equipment", "random_title_enchant_row_id": "72"},
		{"item_kind": "equipment", "random_title_enchant_config_id": "0", "random_title_enchant_row_id": "72"},
	} {
		plain := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 18,
			dnfrepo.ItemStack{ItemID: 400330118, Count: 1, Extra: extra})
		if currentItemListEntryCarriesRandomTitleEnchant(plain) {
			t.Fatalf("extras %+v rendered an enchant; row=%x", extra, plain.data)
		}
	}
}
