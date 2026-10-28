package fatigue

import (
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
)

const (
	DailyRecoveryDayStatKey  = "fatigue_recovery_day_id"
	FatigueDailyDateStatKey  = "fatigue_daily_date"
	FatigueCreatedDayStatKey = "fatigue_created_day_id"
	PotionUsageDayStatKey    = "fatigue_potion_day_id"
	PotionUsageStatPrefix    = "fatigue_potion_used_"
	// ItemUsageDayStatKey / ItemUsageStatPrefix are the generic 每天可用次数
	// owner: one durable counter per item whose PVF definition declares
	// [total usable count]. They share the same 06:00 Beijing service-day
	// boundary as the fatigue counters, so a consumable's daily allowance
	// returns with the same reset the client advertises ("请在6点以后重试").
	ItemUsageDayStatKey = "item_daily_day_id"
	ItemUsageStatPrefix = "item_daily_used_"
)

type DailyStatsResult struct {
	ServiceDayID     int64
	Changed          bool
	FatigueRestored  bool
	PotionUsageReset bool
	ItemUsageReset   bool
}

// ReconcileDailyStats applies the shared 06:00 Beijing service-day boundary.
// The caller owns persistence and must provide a non-nil stats map.
func ReconcileDailyStats(stats map[string]int64, now time.Time, levels ...int) DailyStatsResult {
	result := DailyStatsResult{ServiceDayID: premium.ServiceDayID(now)}
	if stats == nil {
		return result
	}
	level := 0
	if len(levels) > 0 {
		level = levels[0]
	}
	batteryCapacity := BatteryCapacity(level)
	battery := min(max(stats[FatigueBatteryStatKey], 0), batteryCapacity)
	charge := min(max(stats[FatigueBatteryChargeStatKey], 0), FatigueBatteryChargeLimit)
	storedDay, dayInitialized := stats[DailyRecoveryDayStatKey]
	if dailyDate, ok := stats[FatigueDailyDateStatKey]; ok {
		storedDay, dayInitialized = dailyDate, true
	}
	if !dayInitialized || storedDay != result.ServiceDayID {
		previousFatigue, previousKnown := remainingStat(stats)
		limit := stats["fatigue_limit"]
		if limit <= 0 {
			limit = DefaultLimit
			if previousKnown && previousFatigue > limit {
				limit = previousFatigue
			}
			stats["fatigue_limit"] = limit
		}
		createdOnStoredDay := stats[FatigueCreatedDayStatKey] != 0 &&
			stats[FatigueCreatedDayStatKey] == storedDay
		if dayInitialized && storedDay == result.ServiceDayID-1 && previousKnown && !createdOnStoredDay {
			charge += min(max(previousFatigue, 0), FatigueBatteryChargeLimit-charge)
		}
		stats["fatigue"] = limit
		stats["fatigue_display_update"] = 0
		stats[DailyRecoveryDayStatKey] = result.ServiceDayID
		stats[FatigueDailyDateStatKey] = result.ServiceDayID
		result.Changed = true
		result.FatigueRestored = true
	}
	// Keep raw unused fatigue separate from already converted room charges.
	// Existing fatigue_battery values remain usable after upgrading.
	if charge >= FatigueBatteryActivation {
		converted := min(charge/FatigueBatteryPerBuff, batteryCapacity-battery)
		battery += converted
		charge -= converted * FatigueBatteryPerBuff
	}
	if stats[FatigueBatteryStatKey] != battery {
		stats[FatigueBatteryStatKey] = battery
		result.Changed = true
	}
	if stats[FatigueBatteryChargeStatKey] != charge {
		stats[FatigueBatteryChargeStatKey] = charge
		result.Changed = true
	}
	if stats[PotionUsageDayStatKey] != result.ServiceDayID {
		for key := range stats {
			if strings.HasPrefix(key, PotionUsageStatPrefix) {
				stats[key] = 0
			}
		}
		stats[PotionUsageDayStatKey] = result.ServiceDayID
		result.Changed = true
		result.PotionUsageReset = true
	}
	// The generic 每天可用次数 counters of every [total usable count] item move
	// with the same boundary; the client only ever learns the used byte from
	// op541, so a stale counter would silently swallow today's allowance.
	if stats[ItemUsageDayStatKey] != result.ServiceDayID {
		for key := range stats {
			if strings.HasPrefix(key, ItemUsageStatPrefix) {
				stats[key] = 0
			}
		}
		stats[ItemUsageDayStatKey] = result.ServiceDayID
		result.Changed = true
		result.ItemUsageReset = true
	}
	return result
}
