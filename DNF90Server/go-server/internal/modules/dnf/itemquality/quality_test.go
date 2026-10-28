package itemquality

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestApplyWritesOnlyDedicatedQualityFields(t *testing.T) {
	raw := make([]byte, currentEquipmentRawEntrySize)
	binary.LittleEndian.PutUint32(raw[2:6], 1001)
	raw[0x0A] = 0x12 // packed reinforce must survive a seed write
	stack := dnfrepo.ItemStack{
		ItemID:   1001,
		Count:    1,
		RawEntry: raw,
		Extra:    map[string]string{"count_or_instance": "77"},
	}
	if err := Apply(&stack, 123456); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stack.Extra["quality_seed"] != "123456" ||
		stack.Extra["count_or_instance"] != "77" ||
		binary.LittleEndian.Uint32(stack.RawEntry[6:10]) != 123456 ||
		stack.RawEntry[0x0A] != 0x12 {
		t.Fatalf("stack = %+v raw=%x", stack, stack.RawEntry)
	}
}

// TestApplyDoesNotTouchLegacyRowPackedReinforce reproduces the reported bug:
// an ordinary grade box randomly changed the upgrade level of equipment stored
// in the legacy constructor layout. A 43-byte legacy row keeps its create
// scalar at +5:9 and its packed reinforce byte at +9. The old Apply wrote the
// whole seed dword at [6:10] for any row with len>=10, so the random seed's
// high byte (seed>>24, 0..59) landed on +9 and silently rewrote the upgrade
// level. Apply must leave legacy rows untouched below +10.
func TestApplyDoesNotTouchLegacyRowPackedReinforce(t *testing.T) {
	// buildLegacyStarterRow layout: u8 slot, u32 item id at +1, u32 create
	// scalar at +5, u8 packed reinforce at +9, u16 durability at +10.
	raw := make([]byte, 43)
	raw[0] = 15
	binary.LittleEndian.PutUint32(raw[1:5], 12400)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	raw[9] = 0x2A // +10 reinforce, high 3 bits are the flag bits
	binary.LittleEndian.PutUint16(raw[10:12], 27)

	stack := dnfrepo.ItemStack{
		ItemID:   12400,
		Count:    1,
		RawEntry: raw,
		Extra:    map[string]string{"durability": "27", "max_durability": "27"},
	}
	seed := uint32(0x12ABCDEF) // seed>>24 == 0x12 would rewrite the level
	if err := Apply(&stack, seed); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stack.Extra["quality_seed"] != "313249263" { // 0x12ABCDEF
		t.Fatalf("quality_seed = %q, want decimal 0x12ABCDEF", stack.Extra["quality_seed"])
	}
	if stack.RawEntry[0x09] != 0x2A {
		t.Fatalf("legacy packed reinforce byte at +9 rewritten to 0x%02X, want 0x2A", stack.RawEntry[0x09])
	}
	if binary.LittleEndian.Uint32(stack.RawEntry[5:9]) != 1 {
		t.Fatalf("legacy create scalar at +5 rewritten: %x", stack.RawEntry[5:9])
	}
	if binary.LittleEndian.Uint16(stack.RawEntry[10:12]) != 27 {
		t.Fatalf("legacy durability at +10 rewritten: %d", binary.LittleEndian.Uint16(stack.RawEntry[10:12]))
	}
}

func TestNewRandomSeedExcludesTopSentinel(t *testing.T) {
	for range 32 {
		seed, err := NewRandomSeed()
		if err != nil {
			t.Fatalf("NewRandomSeed: %v", err)
		}
		if seed == 0 || seed > RandomSeedCount {
			t.Fatalf("seed = %d", seed)
		}
	}
}

// equipmentRow builds a complete current 0x77 row: item id at +2, seed at +6.
func equipmentRow(itemID uint32, seed uint32) []byte {
	raw := make([]byte, currentEquipmentRawEntrySize)
	binary.LittleEndian.PutUint32(raw[2:6], itemID)
	binary.LittleEndian.PutUint32(raw[6:10], seed)
	return raw
}

// TestStackSeedResolvesTheDurableGradeWord pins the resolution order both the
// login projection and the inventory op13/op14 re-encoders rely on. Reading the
// stack count here instead is what made an enchant repaint the whole bag with
// the lowest grade until the next select-character list rebuild.
func TestStackSeedResolvesTheDurableGradeWord(t *testing.T) {
	const (
		itemID = uint32(100390003)
		seed   = uint32(998506240)
	)
	tests := []struct {
		name  string
		stack dnfrepo.ItemStack
		want  uint32
	}{
		{
			name:  "dedicated quality_seed wins without any item kind",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, Extra: map[string]string{"quality_seed": "998506240"}},
			want:  seed,
		},
		{
			name: "quality_seed wins over a stale legacy alias",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, Extra: map[string]string{
				"item_kind": "equipment", "quality_seed": "998506240", "amount_or_count": "1",
			}},
			want: seed,
		},
		{
			name:  "equipment amount alias keeps imported rows",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, Extra: map[string]string{"item_kind": "equipment", "amount_or_count": "345678901"}},
			want:  345678901,
		},
		{
			name:  "equipment row without an alias falls back to raw+0x06",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, RawEntry: equipmentRow(itemID, seed), Extra: map[string]string{"item_kind": "equipment"}},
			want:  seed,
		},
		{
			name:  "a stackable count is never read as a grade",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 7, RawEntry: equipmentRow(itemID, seed), Extra: map[string]string{"item_kind": "stackable", "amount_or_count": "7"}},
			want:  0,
		},
		{
			name:  "a stackable with no kind stays unknown",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 7, RawEntry: equipmentRow(itemID, seed)},
			want:  0,
		},
		{
			name:  "a row that does not echo its own item id is not trusted",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, RawEntry: equipmentRow(itemID+1, seed), Extra: map[string]string{"item_kind": "equipment"}},
			want:  0,
		},
		{
			name:  "a legacy short row keeps its create scalar",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, RawEntry: equipmentRow(itemID, seed)[:43], Extra: map[string]string{"item_kind": "equipment"}},
			want:  0,
		},
		{
			name:  "the top sentinel is a legal grade",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, Extra: map[string]string{"item_kind": "equipment", "quality_seed": "999999998"}},
			want:  TopSeed,
		},
		{
			name:  "an out of range alias is refused",
			stack: dnfrepo.ItemStack{ItemID: int64(itemID), Count: 1, Extra: map[string]string{"item_kind": "equipment", "quality_seed": "999999999"}},
			want:  0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := StackSeed(test.stack); got != test.want {
				t.Fatalf("StackSeed = %d, want %d", got, test.want)
			}
		})
	}
}
