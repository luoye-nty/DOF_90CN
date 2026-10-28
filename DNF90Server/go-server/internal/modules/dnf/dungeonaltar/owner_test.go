package dungeonaltar

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerReservesEndlessAdmissionWithEscalatingFeeAndReplay(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "account-1", Level: 90, Stats: map[string]int64{"gold": 100_000},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	rule := testAdmissionRule()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	first, err := owner.ReserveEndless(ctx, "7", rule, "attempt-1", now)
	if err != nil || first.Fee != 22_000 || first.GoldAfter != 78_000 || first.DailyEnterCount != 1 || first.WeeklyEnterCount != 1 ||
		first.LottoPointEarned != 1 || first.LottoGoldSpent != 10_000 || first.TotalLottoPoints != 1 || first.TotalLottoGold != 10_000 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	replay, err := owner.ReserveEndless(ctx, "7", rule, "attempt-1", now)
	if err != nil || !replay.Replayed || replay != (AdmissionSnapshot{
		CharacterID: "7", Fee: 22_000, GoldAfter: 78_000, DailyEnterCount: 1, WeeklyEnterCount: 1,
		LottoPointEarned: 1, LottoGoldSpent: 10_000, TotalLottoPoints: 1, TotalLottoGold: 10_000, Replayed: true,
	}) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	second, err := owner.ReserveEndless(ctx, "7", rule, "attempt-2", now)
	if err != nil || second.Fee != 29_200 || second.GoldAfter != 48_800 || second.DailyEnterCount != 2 || second.WeeklyEnterCount != 2 ||
		second.LottoPointEarned != 3 || second.LottoGoldSpent != 17_200 || second.TotalLottoPoints != 4 || second.TotalLottoGold != 27_200 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestOwnerRestoresEndlessAdmissionAndRejectsReceiptReuse(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "8", AccountID: "account-1", Level: 60, Stats: map[string]int64{"gold": 50_000},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	reserved, err := owner.ReserveEndless(ctx, "8", testAdmissionRule(), "attempt", now)
	if err != nil || reserved.Fee != 22_000 {
		t.Fatalf("reserved=%+v err=%v", reserved, err)
	}
	restored, err := owner.RestoreEndless(ctx, "8", "attempt", now)
	if err != nil || restored.GoldAfter != 50_000 || restored.DailyEnterCount != 0 || restored.WeeklyEnterCount != 0 ||
		restored.TotalLottoPoints != 0 || restored.TotalLottoGold != 0 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	if _, err := owner.ReserveEndless(ctx, "8", testAdmissionRule(), "attempt", now); !errors.Is(err, ErrReceiptRestored) {
		t.Fatalf("receipt reuse error=%v", err)
	}
	character, _, err := repositories.Character.Load(ctx, "8")
	if err != nil || character.Stats["gold"] != 50_000 {
		t.Fatalf("character=%+v err=%v", character, err)
	}
}

func TestOwnerReservesUltimateDailyEntryIndependentlyWithoutGoldFee(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "12", AccountID: "account-1", Level: 90, Stats: map[string]int64{"gold": 100_000},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	rule := testAdmissionRule()
	rule.DailyLimit = 3

	endless, err := owner.ReserveEndless(ctx, "12", rule, "endless-1", now)
	if err != nil || endless.DailyEnterCount != 1 || endless.GoldAfter != 78_000 {
		t.Fatalf("endless=%+v err=%v", endless, err)
	}
	ultimate, err := owner.ReserveUltimate(ctx, "12", 3, "ultimate-1", now)
	if err != nil || ultimate.DailyEnterCount != 1 || ultimate.Fee != 0 || ultimate.GoldAfter != 78_000 ||
		ultimate.LottoPointEarned != 0 || ultimate.LottoGoldSpent != 0 {
		t.Fatalf("ultimate=%+v err=%v", ultimate, err)
	}
	secondUltimate, err := owner.ReserveUltimate(ctx, "12", 3, "ultimate-2", now)
	if err != nil || secondUltimate.DailyEnterCount != 2 || secondUltimate.GoldAfter != 78_000 {
		t.Fatalf("second ultimate=%+v err=%v", secondUltimate, err)
	}
	restored, err := owner.RestoreUltimate(ctx, "12", "ultimate-2", now)
	if err != nil || restored.DailyEnterCount != 1 || restored.GoldAfter != 78_000 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	settings, found, err := repositories.Settings.Load(ctx, SettingsScope("12"))
	if err != nil || !found {
		t.Fatalf("settings found=%t err=%v", found, err)
	}
	if settings.Values[endlessDailyCountKey] != "1" || settings.Values[ultimateDailyCountKey] != "1" {
		t.Fatalf("independent counters endless=%q ultimate=%q", settings.Values[endlessDailyCountKey], settings.Values[ultimateDailyCountKey])
	}
	character, _, err := repositories.Character.Load(ctx, "12")
	if err != nil || character.Stats["gold"] != 78_000 {
		t.Fatalf("character=%+v err=%v", character, err)
	}
}

func TestOwnerRejectsFourthUltimateDailyEntry(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "13", AccountID: "account-1", Level: 90, Stats: map[string]int64{"gold": 100_000},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := owner.ReserveUltimate(ctx, "13", 3, "ultimate-"+string(rune('0'+attempt)), now); err != nil {
			t.Fatalf("ultimate attempt %d: %v", attempt, err)
		}
	}
	if _, err := owner.ReserveUltimate(ctx, "13", 3, "ultimate-4", now); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("fourth ultimate error=%v", err)
	}
}

func TestOwnerResetsDailyCountAtBeijingSixAndKeepsWeeklyCount(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "9", AccountID: "account-1", Level: 90, Stats: map[string]int64{"gold": 200_000},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	zone := time.FixedZone("CST", 8*60*60)
	before := time.Date(2026, 9, 1, 5, 59, 59, 0, zone)
	a, err := owner.ReserveEndless(ctx, "9", testAdmissionRule(), "before", before)
	if err != nil || a.DailyEnterCount != 1 || a.WeeklyEnterCount != 1 {
		t.Fatalf("before=%+v err=%v", a, err)
	}
	after := time.Date(2026, 9, 1, 6, 0, 0, 0, zone)
	b, err := owner.ReserveEndless(ctx, "9", testAdmissionRule(), "after", after)
	if err != nil || b.DailyEnterCount != 1 || b.WeeklyEnterCount != 2 || b.Fee != 22_000 {
		t.Fatalf("after=%+v err=%v", b, err)
	}
}

func TestOwnerPersistsBestRoundThenLowestTime(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "10", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	now := time.Now()
	first, err := owner.UpdateBestRecord(ctx, "10", 11006, 4, 12_000, "clear-1", now)
	if err != nil || first.BestRound != 4 || first.BestTimeMillis != 12_000 || !first.Changed {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	slower, err := owner.UpdateBestRecord(ctx, "10", 11006, 4, 13_000, "clear-2", now)
	if err != nil || slower.BestTimeMillis != 12_000 || slower.Changed {
		t.Fatalf("slower=%+v err=%v", slower, err)
	}
	faster, err := owner.UpdateBestRecord(ctx, "10", 11006, 4, 11_000, "clear-3", now)
	if err != nil || faster.BestTimeMillis != 11_000 || !faster.Changed {
		t.Fatalf("faster=%+v err=%v", faster, err)
	}
	higher, err := owner.UpdateBestRecord(ctx, "10", 11006, 5, 20_000, "clear-4", now)
	if err != nil || higher.BestRound != 5 || higher.BestTimeMillis != 20_000 || !higher.Changed {
		t.Fatalf("higher=%+v err=%v", higher, err)
	}
	replay, err := owner.UpdateBestRecord(ctx, "10", 11006, 5, 20_000, "clear-4", now)
	if err != nil || !replay.Replayed || replay.BestRound != 5 || replay.BestTimeMillis != 20_000 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestOwnerRejectsMalformedDurableAdmissionCounter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "11", AccountID: "account-1", Level: 90, Stats: map[string]int64{"gold": 100_000},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	values := map[string]string{}
	resetAdmissionWindows(values, now)
	values["daily_count"] = "not-a-number"
	values["weekly_count"] = "1"
	if err := repositories.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope:  SettingsScope("11"),
		Values: values,
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	if _, err := owner.ReserveEndless(ctx, "11", testAdmissionRule(), "corrupt", now); !errors.Is(err, ErrReceiptMismatch) {
		t.Fatalf("malformed counter error=%v", err)
	}
	character, found, err := repositories.Character.Load(ctx, "11")
	if err != nil || !found || character.Stats["gold"] != 100_000 {
		t.Fatalf("character=%+v found=%t err=%v", character, found, err)
	}
}

func testAdmissionRule() AdmissionRule {
	return AdmissionRule{
		PriceBands: []PriceBand{
			{MinimumLevel: 1, MaximumLevel: 50, Price: 8_000},
			{MinimumLevel: 51, MaximumLevel: 55, Price: 10_000},
			{MinimumLevel: 56, MaximumLevel: 99, Price: 12_000},
		},
		AdmissionIncrease: []AdmissionIncrease{
			{UpperExclusiveEnterCount: 1, Percent: 0},
			{UpperExclusiveEnterCount: 2, Percent: 60},
			{UpperExclusiveEnterCount: 3, Percent: 200},
		},
		LottoPoints: []LottoPoint{
			{UpperExclusiveEnterCount: 1, Points: 1},
			{UpperExclusiveEnterCount: 2, Points: 3},
			{UpperExclusiveEnterCount: 3, Points: 5},
		},
		LottoCost: 10_000,
	}
}
