package dnfbridge

import (
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// parseEnchantRandomTicket reads the [enchant random] block shared by the two
// random ticket families in the current PVF: the op50 reinforcement scene
// (祝福强化券 2681908/2681909) and the op438 weapon-forging scene
// (+6锻造券 2683788). Both stackables declare the same child sections, so this
// is the single place that knows their layout:
//
//	[er_grade]   rarity whitelist (empty = any)
//	[er_slot]    0 any, 1 weapon, 2 armor, 3 accessory
//	[er_seal]    1 = sealed targets only
//	[er_enchant] optional leading count, then (level, weight) pairs
//
// The second return value reports whether the stackable carries the block at
// all, so a caller can tell "not a random ticket" from "a random ticket whose
// candidate list is unusable".
func parseEnchantRandomTicket(document *dnfpvf.Document) (alignedcmd.EnchantRandomTicket, bool) {
	if document == nil {
		return alignedcmd.EnchantRandomTicket{}, false
	}
	if _, found := document.Section("enchant random"); !found {
		return alignedcmd.EnchantRandomTicket{}, false
	}
	ticket := alignedcmd.EnchantRandomTicket{}
	values := document.Ints("er_enchant")
	start := 0
	// The section may lead with an entry count; skip it when the remaining
	// ints form exactly that many (level, weight) pairs.
	if len(values) > 0 && int64(len(values)) == 1+values[0]*2 {
		start = 1
	}
	for index := start; index+1 < len(values); index += 2 {
		ticket.Entries = append(ticket.Entries, alignedcmd.UpgradeTicketRandomEntry{
			TargetLevel: values[index],
			Weight:      values[index+1],
		})
	}
	if slots := document.Ints("er_slot"); len(slots) > 0 {
		ticket.SlotRestriction = slots[0]
	}
	ticket.Rarities = document.Ints("er_grade")
	if seals := document.Ints("er_seal"); len(seals) > 0 {
		ticket.SealRestriction = seals[0]
	}
	return ticket, true
}

// currentEnchantRandomForgingActionType separates the two [enchant random]
// families. A stackable whose [action type] is `[random upgrade separate]`
// (e.g. +6锻造券 2683788) rolls the weapon-forging level and belongs to op438;
// without the marker the block rolls the reinforcement level for the op50
// ticket scene. Claiming one family in the other's resolver would move the
// wrong level.
func currentEnchantRandomForgingActionType(document *dnfpvf.Document) bool {
	if document == nil {
		return false
	}
	for _, action := range document.Texts("action type") {
		if strings.Contains(strings.ToLower(strings.Trim(action, "` []")), "upgrade separate") {
			return true
		}
	}
	return false
}
