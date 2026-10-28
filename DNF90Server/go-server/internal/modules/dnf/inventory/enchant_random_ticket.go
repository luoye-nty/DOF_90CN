package inventory

import (
	"math"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// enchantRandomWeightBase is the er_enchant weight base: a candidate weight of
// 100000 is a certain outcome.
const enchantRandomWeightBase = 100000

// enchantRandomRestrictionAllows applies the [enchant random] er_slot /
// er_grade / er_seal target gates shared by the op50 reinforcement ticket
// scene and the op438 weapon-forging ticket family: er_slot 0 allows every
// part, 1 weapons, 2 armor, 3 accessories; an empty er_grade list allows every
// rarity; only er_seal == 1 demands a sealed target (the common er_seal 0/2
// carries no check).
func enchantRandomRestrictionAllows(ticket alignedcmd.EnchantRandomTicket, equipmentType string, rarity int64, target dnfrepo.ItemStack) bool {
	switch ticket.SlotRestriction {
	case 0:
	case 1, 2, 3:
		if enchantRandomSlotClass(equipmentType) != ticket.SlotRestriction {
			return false
		}
	default:
		return false
	}
	if len(ticket.Rarities) > 0 {
		allowed := false
		for _, candidate := range ticket.Rarities {
			if candidate == rarity {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if ticket.SealRestriction == 1 && firstExtraInt(target.Extra, 0, "seal_flag", "seal") == 0 {
		return false
	}
	return true
}

// enchantRandomSlotClass folds a PVF [equipment type] token into the er_slot
// classes: 1 weapon, 2 armor, 3 accessory, 0 anything else.
func enchantRandomSlotClass(equipmentType string) int64 {
	switch strings.ToLower(strings.Trim(equipmentType, "` ")) {
	case "[weapon]":
		return 1
	case "[coat]", "[shoulder]", "[pants]", "[shoes]", "[waist]":
		return 2
	case "[amulet]", "[wrist]", "[ring]":
		return 3
	default:
		return 0
	}
}

// selectEnchantRandomEntry rolls one er_enchant candidate. A single candidate
// keeps its own weight, so a one-entry ticket can still fail and retain the
// current level like a plain ticket; a weighted pick across several candidates
// always succeeds at the picked level.
func (o *Owner) selectEnchantRandomEntry(entries []alignedcmd.UpgradeTicketRandomEntry) (alignedcmd.UpgradeTicketRandomEntry, bool) {
	if len(entries) == 0 {
		return alignedcmd.UpgradeTicketRandomEntry{}, false
	}
	if len(entries) == 1 {
		return entries[0], true
	}
	total := int64(0)
	for _, entry := range entries {
		if entry.Weight <= 0 {
			continue
		}
		if total > math.MaxInt64-entry.Weight {
			return alignedcmd.UpgradeTicketRandomEntry{}, false
		}
		total += entry.Weight
	}
	if total <= 0 {
		return alignedcmd.UpgradeTicketRandomEntry{}, false
	}
	roll := o.enchantRandomIntn(total)
	cursor := int64(0)
	picked := alignedcmd.UpgradeTicketRandomEntry{}
	for _, entry := range entries {
		if entry.Weight <= 0 {
			continue
		}
		picked = entry
		cursor += entry.Weight
		if roll < cursor {
			break
		}
	}
	return alignedcmd.UpgradeTicketRandomEntry{TargetLevel: picked.TargetLevel, Weight: enchantRandomWeightBase}, true
}

// enchantRandomIntn draws from the injected random source so ticket rolls stay
// deterministic under test, mirroring chronicleRandomIntn.
func (o *Owner) enchantRandomIntn(limit int64) int64 {
	if limit <= 1 || o == nil || o.enchantRandom == nil {
		return 0
	}
	bounded := limit
	if bounded > math.MaxInt32 {
		bounded = math.MaxInt32
	}
	value := int64(o.enchantRandom(int(bounded)))
	if value < 0 {
		return 0
	}
	if value >= limit {
		return value % limit
	}
	return value
}
