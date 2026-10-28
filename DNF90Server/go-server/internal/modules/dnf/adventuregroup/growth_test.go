package adventuregroup

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func growthTestConfig() GrowthConfig {
	// Three levels; entering level 2 costs 100, level 3 costs 200.
	return GrowthConfig{
		MaximumLevel:       3,
		RateParts:          growthRateScale, // 1:1, so gains map straight through
		RequiredExperience: []uint64{0, 0, 100, 200},
		LevelBenefits:      make([]Benefits, 4),
	}
}

func TestApplyGrowthExperienceLevelsAndCarriesRemainder(t *testing.T) {
	c := growthTestConfig()
	account := dnfrepo.AccountRecord{AccountID: "a", Metadata: map[string]string{}}
	now := time.Unix(1_800_000_000, 0)

	state, err := ApplyGrowthExperience(&account, c, Summary{}, 60, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.Level != 1 || state.Experience != 60 {
		t.Fatalf("first grant level=%d exp=%d", state.Level, state.Experience)
	}
	// 60 + 60 crosses the 100 needed for level 2 with 20 left over.
	state, err = ApplyGrowthExperience(&account, c, Summary{}, 60, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.Level != 2 || state.Experience != 20 {
		t.Fatalf("level-up level=%d exp=%d, want 2/20", state.Level, state.Experience)
	}
	// Idempotent read: the ledger is durable, so a fresh read agrees.
	read, err := readGrowthState(account, c, Summary{})
	if err != nil {
		t.Fatal(err)
	}
	if read != state {
		t.Fatalf("persisted=%+v in-memory=%+v", read, state)
	}
	// At the cap the bar and the fraction are both cleared.
	state, err = ApplyGrowthExperience(&account, c, Summary{}, 1000, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.Level != 3 || state.Experience != 0 || state.Remainder != 0 {
		t.Fatalf("capped level=%d exp=%d rem=%d", state.Level, state.Experience, state.Remainder)
	}
}

// A sub-unit rate must carry the fraction so many small grants add up to the
// same total as one large one.
func TestApplyGrowthExperienceCarriesSubUnitRateRemainder(t *testing.T) {
	c := growthTestConfig()
	c.RateParts = growthRateScale / 2 // every grant counts half
	account := dnfrepo.AccountRecord{AccountID: "a", Metadata: map[string]string{}}
	now := time.Unix(1_800_000_000, 0)
	if _, err := ApplyGrowthExperience(&account, c, Summary{}, 100, now); err != nil {
		t.Fatal(err)
	}
	state, err := ApplyGrowthExperience(&account, c, Summary{}, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	// 100 halved twice = 100 total, i.e. exactly level 2.
	if state.Level != 2 || state.Experience != 0 {
		t.Fatalf("level=%d exp=%d, want 2/0 (the half-unit must not be dropped)", state.Level, state.Experience)
	}
}

func TestSeedGrowthStateMigratesLegacyLevelOnce(t *testing.T) {
	c := growthTestConfig()
	account := dnfrepo.AccountRecord{AccountID: "a", Metadata: map[string]string{}}
	now := time.Unix(1_800_000_000, 0)
	seeded, err := SeedGrowthState(&account, c, Summary{ManageLevel: 2, TotalPoint: 999}, now)
	if err != nil || !seeded {
		t.Fatalf("seed=%t err=%v", seeded, err)
	}
	state, err := readGrowthState(account, c, Summary{})
	if err != nil {
		t.Fatal(err)
	}
	// The legacy level is carried over, but its point scale is the roster's and
	// must not be reused as growth progress.
	if state.Level != 2 || state.Experience != 0 {
		t.Fatalf("migrated level=%d exp=%d, want 2/0", state.Level, state.Experience)
	}
	again, err := SeedGrowthState(&account, c, Summary{ManageLevel: 1}, now)
	if err != nil || again {
		t.Fatalf("second seed=%t err=%v, want no-op", again, err)
	}
}
