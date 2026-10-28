package itemquality

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// TopSeed is the current-client top-quality sentinel used by the gold
	// kaleido box.
	TopSeed uint32 = 999999998
	// RandomSeedCount excludes the dedicated top-quality sentinel.
	RandomSeedCount uint32 = TopSeed - 1
)

func NewRandomSeed() (uint32, error) {
	const acceptLimit = ^uint32(0) - (^uint32(0) % RandomSeedCount)
	var raw [4]byte
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, fmt.Errorf("generate equipment quality seed: %w", err)
		}
		value := binary.LittleEndian.Uint32(raw[:])
		if value < acceptLimit {
			return value%RandomSeedCount + 1, nil
		}
	}
}

func Valid(seed uint32) bool {
	return seed > 0 && seed <= TopSeed
}

// currentEquipmentRawEntrySize is the full current 0x77 wire row length. Only
// that row carries the quality seed at raw+0x06; a shorter legacy constructor
// row (u8 slot, u32 item id at +1, u32 create scalar at +5, u8 packed reinforce
// at +9, u16 durability at +10) must never be written at [6:10], or the seed's
// high byte overwrites the packed reinforce byte at +9 and randomly rewrites
// the item's upgrade level.
const currentEquipmentRawEntrySize = 0x77

// Apply persists a quality seed in both the relational item metadata and a
// present raw item projection. It never writes the independent stack-count or
// instance-value aliases. The raw projection is written only when the row is a
// complete current 0x77 row whose [6:10] is the seed slot; legacy rows keep
// their create scalar and packed reinforce byte intact and expose the seed
// through Extra["quality_seed"] alone (the projection layer reads it from
// Extra before ever consulting a legacy row's bytes).
func Apply(stack *dnfrepo.ItemStack, seed uint32) error {
	if stack == nil || !Valid(seed) {
		return fmt.Errorf("invalid equipment quality seed: %d", seed)
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 4)
	}
	stack.Extra["quality_seed"] = strconv.FormatUint(uint64(seed), 10)
	if len(stack.RawEntry) == currentEquipmentRawEntrySize {
		binary.LittleEndian.PutUint32(stack.RawEntry[6:10], seed)
	}
	return nil
}

// equipmentItemKind is the durable item_kind that marks a row as equipment.
// dnfbridge writes it as its dungeonDropItemEquipment kind.
const equipmentItemKind = "equipment"

// equipmentSeedAliases are the Extra keys a durable equipment row can carry
// its quality seed under. quality_seed is the dedicated current key;
// amount_or_count and count_or_instance are the legacy aliases older rows use.
var equipmentSeedAliases = []string{"quality_seed", "amount_or_count", "count_or_instance"}

// StackSeed returns the quality seed (品级) one durable row carries, or 0 when
// the row is not an equipment instance that owns a seed. It is the single
// source of truth for the raw+0x06 word of a class0 item-list row:
//
//   - the dedicated quality_seed Extra key wins outright, so rows imported
//     before item_kind normalization keep their grade;
//   - every other alias is equipment metadata only when the durable kind says
//     equipment, because amount_or_count is shared with stackables;
//   - a complete current 0x77 row that echoes its own item id is read at
//     raw+0x06, where the current client keeps the grade seed.
//
// Both the login/select-character projection and every op13/op14 re-encoder
// must resolve that word through this function. Encoding the stack count there
// instead (an equipment row always carries Count 1) makes the client repaint
// the item with the grade of seed 1 until the next full character list load.
func StackSeed(stack dnfrepo.ItemStack) uint32 {
	if seed := seedFromExtra(stack.Extra, "quality_seed"); Valid(seed) {
		return seed
	}
	if !strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), equipmentItemKind) {
		return 0
	}
	if seed := seedFromExtra(stack.Extra, equipmentSeedAliases...); Valid(seed) {
		return seed
	}
	if stack.ItemID > 0 && len(stack.RawEntry) == currentEquipmentRawEntrySize &&
		binary.LittleEndian.Uint32(stack.RawEntry[2:6]) == uint32(stack.ItemID) {
		if seed := binary.LittleEndian.Uint32(stack.RawEntry[6:10]); Valid(seed) {
			return seed
		}
	}
	return 0
}

// seedFromExtra reads the first present, parseable Extra alias as the unsigned
// word the wire row carries. A present-but-zero alias still ends the scan, so
// the resolution matches the item-list projection's sceneInventoryExtraUint32
// exactly instead of silently falling through to a later alias.
func seedFromExtra(extra map[string]string, keys ...string) uint32 {
	for _, key := range keys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseUint(raw, 0, 64)
		if err != nil {
			continue
		}
		if value > uint64(^uint32(0)) {
			value = uint64(^uint32(0))
		}
		return uint32(value)
	}
	return 0
}
