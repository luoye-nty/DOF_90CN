package fatigue

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestReconcileDailyStatsCrossesBeijingSixAndResetsEveryPotion(t *testing.T) {
	before := time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC)
	after := time.Date(2026, 8, 30, 22, 1, 0, 0, time.UTC)
	stats := map[string]int64{
		"fatigue": 0, "fatigue_limit": BlackDiamondLimit, "fatigue_display_update": BlackDiamondLimit,
		DailyRecoveryDayStatKey:             premium.ServiceDayID(before),
		PotionUsageDayStatKey:               premium.ServiceDayID(before),
		PotionUsageStatPrefix + "490702338": 3,
		PotionUsageStatPrefix + "490702339": 2,
		// The generic 每天可用次数 counters of [total usable count] items move
		// with the same boundary, so they must be reset by this call too.
		ItemUsageDayStatKey:               premium.ServiceDayID(before),
		ItemUsageStatPrefix + "490006906": 3,
		ItemUsageStatPrefix + "10153501":  1,
	}
	result := ReconcileDailyStats(stats, after)
	if !result.Changed || !result.FatigueRestored || !result.PotionUsageReset || !result.ItemUsageReset {
		t.Fatalf("result=%+v", result)
	}
	if stats["fatigue"] != BlackDiamondLimit || stats["fatigue_limit"] != BlackDiamondLimit ||
		stats["fatigue_display_update"] != 0 ||
		stats[PotionUsageStatPrefix+"490702338"] != 0 || stats[PotionUsageStatPrefix+"490702339"] != 0 ||
		stats[ItemUsageStatPrefix+"490006906"] != 0 || stats[ItemUsageStatPrefix+"10153501"] != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	wantDay := premium.ServiceDayID(after)
	if stats[DailyRecoveryDayStatKey] != wantDay || stats[PotionUsageDayStatKey] != wantDay ||
		stats[ItemUsageDayStatKey] != wantDay {
		t.Fatalf("service days recovery=%d potion=%d item=%d want=%d",
			stats[DailyRecoveryDayStatKey], stats[PotionUsageDayStatKey], stats[ItemUsageDayStatKey], wantDay)
	}
}

func TestReconcileDailyStatsDoesNotRepeatWithinServiceDay(t *testing.T) {
	now := time.Date(2026, 8, 30, 23, 0, 0, 0, time.UTC)
	dayID := premium.ServiceDayID(now)
	stats := map[string]int64{
		"fatigue": 77, "fatigue_limit": BlackDiamondLimit, "fatigue_display_update": 111,
		DailyRecoveryDayStatKey:             dayID,
		PotionUsageDayStatKey:               dayID,
		PotionUsageStatPrefix + "490702338": 2,
		ItemUsageDayStatKey:                 dayID,
		ItemUsageStatPrefix + "490006906":   2,
	}
	result := ReconcileDailyStats(stats, now.Add(2*time.Hour))
	if result.Changed || result.FatigueRestored || result.PotionUsageReset || result.ItemUsageReset {
		t.Fatalf("result=%+v", result)
	}
	if stats["fatigue"] != 77 || stats["fatigue_display_update"] != 111 ||
		stats[PotionUsageStatPrefix+"490702338"] != 2 || stats[ItemUsageStatPrefix+"490006906"] != 2 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestReconcileDailyStatsUsesDefaultLimitWhenMissing(t *testing.T) {
	now := time.Date(2026, 8, 30, 22, 1, 0, 0, time.UTC)
	stats := map[string]int64{"fatigue": 0}
	result := ReconcileDailyStats(stats, now)
	if !result.FatigueRestored || stats["fatigue"] != DefaultLimit || stats["fatigue_limit"] != DefaultLimit {
		t.Fatalf("result=%+v stats=%+v", result, stats)
	}
}

func TestBatteryCapacityMatchesServerLevelBands(t *testing.T) {
	for _, test := range []struct {
		level int
		want  int64
	}{
		{1, 78}, {19, 78}, {20, 117}, {29, 117}, {30, 156}, {39, 156}, {40, 234}, {90, 234},
	} {
		if got := BatteryCapacity(test.level); got != test.want {
			t.Fatalf("BatteryCapacity(%d)=%d, want %d", test.level, got, test.want)
		}
	}
}

func TestReconcileDailyStatsStoresPreviousServiceDayFatigueInBattery(t *testing.T) {
	before := time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC)
	after := time.Date(2026, 8, 30, 22, 1, 0, 0, time.UTC)
	stats := map[string]int64{
		"fatigue": 150, "fatigue_limit": 188,
		DailyRecoveryDayStatKey: premium.ServiceDayID(before),
	}
	result := ReconcileDailyStats(stats, after, 90)
	if !result.Changed || stats["fatigue"] != 188 || stats[FatigueBatteryChargeStatKey] != 150 || stats[FatigueBatteryStatKey] != 0 {
		t.Fatalf("result=%+v stats=%+v", result, stats)
	}
}

func TestReconcileDailyStatsPreservesBatteryAfterSkippedServiceDay(t *testing.T) {
	now := time.Date(2026, 8, 30, 22, 1, 0, 0, time.UTC)
	stats := map[string]int64{
		"fatigue": 80, "fatigue_limit": 156, FatigueBatteryStatKey: 20,
		DailyRecoveryDayStatKey: premium.ServiceDayID(now.Add(-48 * time.Hour)),
	}
	ReconcileDailyStats(stats, now, 30)
	if stats[FatigueBatteryStatKey] != 20 || stats[FatigueBatteryChargeStatKey] != 0 {
		t.Fatalf("stats=%+v, want existing battery retained without invented skipped-day fatigue", stats)
	}
}

func TestReconcileDailyStatsDoesNotChargeCreationServiceDay(t *testing.T) {
	createdDay := time.Date(2026, 8, 29, 22, 1, 0, 0, time.UTC)
	nextDay := createdDay.Add(24 * time.Hour)
	stats := map[string]int64{
		"fatigue": 156, "fatigue_limit": 156,
		DailyRecoveryDayStatKey:  premium.ServiceDayID(createdDay),
		FatigueCreatedDayStatKey: premium.ServiceDayID(createdDay),
		FatigueBatteryStatKey:    0,
	}
	ReconcileDailyStats(stats, nextDay, 1)
	if stats[FatigueBatteryStatKey] != 0 {
		t.Fatalf("battery=%d, want 0 on first day after creation", stats[FatigueBatteryStatKey])
	}
	ReconcileDailyStats(stats, nextDay.Add(24*time.Hour), 1)
	if stats[FatigueBatteryStatKey] != 31 || stats[FatigueBatteryChargeStatKey] != 1 {
		t.Fatalf("stats=%+v, want 31 charges and 1 raw point after a complete service day", stats)
	}
}

func TestReconcileDailyStatsAccumulatesAndConvertsBattery(t *testing.T) {
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                                           string
		level                                          int
		stored, charge, unused, wantStored, wantCharge int64
	}{
		{"below threshold", 40, 0, 100, 55, 0, 155},
		{"at threshold", 40, 0, 100, 56, 31, 1},
		{"retains existing blessing", 40, 20, 100, 56, 51, 1},
		{"low level cap", 19, 70, 0, 156, 78, 116},
		{"full cap retains raw charge", 40, 234, 100, 100, 234, 200},
		{"raw storage cap", 40, 234, 19999, 188, 234, 20000},
		{"negative values", 40, -1, -1, -1, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			stats := map[string]int64{
				"fatigue": test.unused, "fatigue_limit": 188,
				FatigueBatteryStatKey: test.stored, FatigueBatteryChargeStatKey: test.charge,
				DailyRecoveryDayStatKey: premium.ServiceDayID(now) - 1,
			}
			ReconcileDailyStats(stats, now, test.level)
			if stats[FatigueBatteryStatKey] != test.wantStored || stats[FatigueBatteryChargeStatKey] != test.wantCharge {
				t.Fatalf("stats=%+v, want battery=%d charge=%d", stats, test.wantStored, test.wantCharge)
			}
			if result := ReconcileDailyStats(stats, now.Add(time.Hour), test.level); result.Changed {
				t.Fatalf("same day changed again: %+v", stats)
			}
		})
	}
}

func TestOwnerReconcileDailyPersistsOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	before := time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC)
	after := time.Date(2026, 8, 30, 22, 1, 0, 0, time.UTC)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Stats: map[string]int64{
			"fatigue": 0, "fatigue_limit": DefaultLimit,
			DailyRecoveryDayStatKey: premium.ServiceDayID(before),
			PotionUsageDayStatKey:   premium.ServiceDayID(before),
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	first, err := owner.ReconcileDaily(ctx, ReconcileDailyCommand{
		AccountID: "account-1", CharacterID: "19", UpdatedAt: after,
	})
	if err != nil || !first.Changed || first.Character.Stats["fatigue"] != DefaultLimit || !first.Character.UpdatedAt.Equal(after) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := owner.ReconcileDaily(ctx, ReconcileDailyCommand{
		AccountID: "account-1", CharacterID: "19", UpdatedAt: after.Add(time.Hour),
	})
	if err != nil || second.Changed || !second.Character.UpdatedAt.Equal(after) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}
