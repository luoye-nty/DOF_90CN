package inventory

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// The enchanter's [monstercard exp] row is the authority once it is known: it
// is indexed by the character's enchanter level and then by the target card's
// rarity. Before it was wired in, any upgrade using an identical card returned
// 100% no matter the job level, which is why a fresh enchanter never failed.
func TestMonsterCardUpgradeChanceUsesTheEnchanterTable(t *testing.T) {
	// The live level-1 row: 100/70/60/50/0.
	level1 := alignedcmd.MonsterCardUpgradePolicy{
		Known: true, Level: 1, SuccessRates: [5]int{100, 70, 60, 50, 0},
		MinimumExperienceGain: 3, MaximumExperienceGain: 6,
	}
	card := func(rarity int) alignedcmd.MonsterCardUpgradeResolution {
		return alignedcmd.MonsterCardUpgradeResolution{Rarity: rarity, CalculateConst: 10}
	}

	for rarity, wantPercent := range []int{100, 70, 60, 50, 0} {
		got := monsterCardUpgradeChanceFor(level1, card(rarity), card(rarity), true)
		want := wantPercent * (monsterCardUpgradeProbabilityBase / 100)
		if wantPercent >= 100 {
			want = monsterCardUpgradeProbabilityBase
		}
		if got != want {
			t.Fatalf("level 1 rarity %d chance=%d want %d", rarity, got, want)
		}
	}

	// An identical card no longer forces a certainty on its own.
	if got := monsterCardUpgradeChanceFor(level1, card(3), card(3), true); got == monsterCardUpgradeProbabilityBase {
		t.Fatal("a same-card upgrade is still hardcoded to always succeed")
	}

	// The live level-11 row is 100 across the board, which is why a maxed
	// enchanter legitimately never fails.
	level11 := alignedcmd.MonsterCardUpgradePolicy{
		Known: true, Level: 11, SuccessRates: [5]int{100, 100, 100, 100, 100},
		MinimumExperienceGain: 15, MaximumExperienceGain: 18,
	}
	for rarity := 0; rarity < 5; rarity++ {
		if got := monsterCardUpgradeChanceFor(level11, card(rarity), card(rarity), false); got != monsterCardUpgradeProbabilityBase {
			t.Fatalf("level 11 rarity %d chance=%d want certain", rarity, got)
		}
	}

	// Cards above the table's five tiers saturate on the last one rather than
	// reading out of range.
	if got := monsterCardUpgradeChanceFor(level1, card(6), card(6), false); got != 0 {
		t.Fatalf("rarity 6 chance=%d want the last tier's 0", got)
	}

	// Without the table, the old rarity-gap estimate still applies, so a PVF
	// failure degrades rather than blocking an upgrade outright.
	unknown := alignedcmd.MonsterCardUpgradePolicy{}
	if got := monsterCardUpgradeChanceFor(unknown, card(3), card(3), true); got != monsterCardUpgradeProbabilityBase {
		t.Fatalf("fallback chance=%d want the previous same-card certainty", got)
	}
}

// One attempt feeds the enchanter job the row's experience, win or lose.
func TestMonsterCardUpgradeExperienceGainRollsTheRowRange(t *testing.T) {
	policy := alignedcmd.MonsterCardUpgradePolicy{
		Known: true, Level: 1, MinimumExperienceGain: 3, MaximumExperienceGain: 6,
	}
	// A random that always returns 0 yields the minimum, and the span passed to
	// it must cover the inclusive maximum.
	span := -1
	got := monsterCardUpgradeExperienceGain(policy, func(n int) int { span = n; return 0 })
	if got != 3 {
		t.Fatalf("experience=%d want the minimum 3", got)
	}
	if span != 4 {
		t.Fatalf("random span=%d want 4 so that 6 is reachable", span)
	}
	if got := monsterCardUpgradeExperienceGain(policy, func(int) int { return 3 }); got != 6 {
		t.Fatalf("experience=%d want the maximum 6", got)
	}

	// A fixed row does not call the random source at all.
	fixed := alignedcmd.MonsterCardUpgradePolicy{Known: true, MinimumExperienceGain: 5, MaximumExperienceGain: 5}
	if got := monsterCardUpgradeExperienceGain(fixed, func(int) int { t.Fatal("random used for a fixed range"); return 0 }); got != 5 {
		t.Fatalf("fixed experience=%d want 5", got)
	}

	// No table, no experience.
	if got := monsterCardUpgradeExperienceGain(alignedcmd.MonsterCardUpgradePolicy{}, func(int) int { return 0 }); got != 0 {
		t.Fatalf("unknown policy granted %d experience", got)
	}
}
