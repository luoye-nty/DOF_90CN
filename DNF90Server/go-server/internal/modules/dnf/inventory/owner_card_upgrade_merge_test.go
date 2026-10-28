package inventory

import (
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// A pile of identical cards upgrades against itself, so the target and the
// material are the same slot and each attempt spends two cards. The last
// attempt empties that slot, and the result used to be dropped straight back
// into it instead of joining the stack it belonged to -- the new card sat in
// its own cell until a full bag refresh merged it.
//
// Operator repro: ten 制造者卢克光 cards, upgraded in pairs; the first results
// merged into one cell and the final one took a cell of its own.
func TestMonsterCardUpgradeMergesTheLastResultIntoTheExistingStack(t *testing.T) {
	const cardID int64 = 4001
	policy := alignedcmd.MonsterCardUpgradeResolution{
		Rarity: 3, CalculateConst: 10, StackLimit: 99, SlotStart: 121, SlotEnd: 180,
	}
	card := func(count int64, upgrades byte) dnfrepo.ItemStack {
		stack := dnfrepo.ItemStack{ItemID: cardID, Count: count, Extra: map[string]string{
			"item_kind":             "stackable",
			"enchant_upgrade_count": strconv.Itoa(int(upgrades)),
		}}
		return stack
	}

	// Two base cards left in the pile and one upgraded card already parked
	// elsewhere: the next upgrade consumes the pile entirely.
	items := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 121): card(2, 0),
		slotKey(listTypeMain, 130): card(1, 1),
	}
	resultSlot, changed, err := applyMonsterCardUpgradeAttempt(items, 121, 121, policy, true)
	if err != nil {
		t.Fatal(err)
	}
	if resultSlot != 130 {
		t.Fatalf("result slot=%d want 130 (merge into the existing upgraded stack)", resultSlot)
	}
	if _, occupied := items[slotKey(listTypeMain, 121)]; occupied {
		t.Fatalf("the emptied pile slot kept a row: %+v", items[slotKey(listTypeMain, 121)])
	}
	if got := items[slotKey(listTypeMain, 130)].Count; got != 2 {
		t.Fatalf("merged stack count=%d want 2", got)
	}
	// Both the emptied slot and the merge target must be announced, or the
	// client keeps drawing the old pile.
	if len(changed) != 2 || changed[0] != 121 || changed[1] != 130 {
		t.Fatalf("changed slots=%v want [121 130]", changed)
	}
}

// With no stack to join, the result stays in the slot the player was working
// in rather than jumping to the front of the tab.
func TestMonsterCardUpgradeKeepsTheWorkingSlotWhenNothingToMergeWith(t *testing.T) {
	const cardID int64 = 4001
	policy := alignedcmd.MonsterCardUpgradeResolution{
		Rarity: 3, CalculateConst: 10, StackLimit: 99, SlotStart: 121, SlotEnd: 180,
	}
	items := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 140): {ItemID: cardID, Count: 2, Extra: map[string]string{
			"item_kind": "stackable", "enchant_upgrade_count": "0",
		}},
	}
	resultSlot, _, err := applyMonsterCardUpgradeAttempt(items, 140, 140, policy, true)
	if err != nil {
		t.Fatal(err)
	}
	if resultSlot != 140 {
		t.Fatalf("result slot=%d want 140", resultSlot)
	}
	if got := items[slotKey(listTypeMain, 140)].Count; got != 1 {
		t.Fatalf("result count=%d want 1", got)
	}
	if got := monsterCardUpgradeCountOf(items[slotKey(listTypeMain, 140)]); got != 1 {
		t.Fatalf("result upgrade count=%d want 1", got)
	}
}

// A partly spent pile must not merge the result back into its own leftovers:
// those still carry the old upgrade count and are a different card.
func TestMonsterCardUpgradeNeverMergesIntoItsOwnLeftovers(t *testing.T) {
	const cardID int64 = 4001
	policy := alignedcmd.MonsterCardUpgradeResolution{
		Rarity: 3, CalculateConst: 10, StackLimit: 99, SlotStart: 121, SlotEnd: 180,
	}
	items := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 121): {ItemID: cardID, Count: 10, Extra: map[string]string{
			"item_kind": "stackable", "enchant_upgrade_count": "0",
		}},
	}
	resultSlot, _, err := applyMonsterCardUpgradeAttempt(items, 121, 121, policy, true)
	if err != nil {
		t.Fatal(err)
	}
	if resultSlot == 121 {
		t.Fatal("the upgraded card merged into the base pile it came from")
	}
	if got := items[slotKey(listTypeMain, 121)].Count; got != 8 {
		t.Fatalf("pile count=%d want 8 (one target plus one material)", got)
	}
	if got := monsterCardUpgradeCountOf(items[slotKey(listTypeMain, resultSlot)]); got != 1 {
		t.Fatalf("result upgrade count=%d want 1", got)
	}
	// The invariant that actually broke in production: whatever happens to the
	// result, the cards left behind must still be what they were. A live pile
	// came back as eleven cards all reading +1 after one upgrade.
	if got := monsterCardUpgradeCountOf(items[slotKey(listTypeMain, 121)]); got != 0 {
		t.Fatalf("the leftover pile inherited upgrade count %d; it must stay 0", got)
	}
	// And the result must be a single card, never the pile's worth of them.
	if got := items[slotKey(listTypeMain, resultSlot)].Count; got != 1 {
		t.Fatalf("result count=%d want 1", got)
	}
}
