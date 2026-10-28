// Package fatigue owns daily fatigue-limit reconciliation rules shared by
// character selection and live account-entitlement activation.
package fatigue

const (
	DefaultLimit                int64 = 156
	BlackDiamondLimit           int64 = 188
	FatigueBatteryStatKey             = "fatigue_battery"
	FatigueBatteryChargeStatKey       = "fatigue_battery_charge"
	FatigueBatteryChargeLimit   int64 = 20000
	FatigueBatteryActivation    int64 = 156
	FatigueBatteryPerBuff       int64 = 5
	FatigueBatteryDayCapacity1  int64 = 78
	FatigueBatteryDayCapacity2  int64 = 117
	FatigueBatteryDayCapacity3  int64 = 156
	FatigueBatteryDayCapacity4  int64 = 234
)

// RemainingStat reads a character's remaining fatigue out of its stats map.
//
// Exported because more than one projection needs the same number the dungeon
// entry path consumes: the op7 mode-10 peer prompt renders 剩余疲劳值, and
// re-listing the keys at that call site is how the two would drift apart. See
// remainingStat for the legacy spellings it also accepts.
func RemainingStat(stats map[string]int64) (int64, bool) {
	return remainingStat(stats)
}

// BatteryBuffCount is the native counter: the tooltip displays count - 1.
// sub_2691300 also disables the bonus at the current client's level cap.
func BatteryBuffCount(remaining int64, level int, roomActive bool) uint16 {
	if level >= 90 {
		return 0
	}
	remaining = min(max(remaining, 0), BatteryCapacity(level))
	if remaining == 0 && !roomActive {
		return 0
	}
	return uint16(remaining + 1)
}

// BatteryCapacity returns the PVF fatigue-battery display capacity for a
// character level. The level bands are the 99-entry serverparameter table:
// 1-19, 20-29, 30-39, and 40+.
func BatteryCapacity(level int) int64 {
	switch {
	case level >= 40:
		return FatigueBatteryDayCapacity4
	case level >= 30:
		return FatigueBatteryDayCapacity3
	case level >= 20:
		return FatigueBatteryDayCapacity2
	case level > 0:
		return FatigueBatteryDayCapacity1
	default:
		// Legacy records without a level must not be truncated during migration.
		return FatigueBatteryDayCapacity4
	}
}

// Consume deducts up to amount from the persisted remaining fatigue without
// allowing a negative value. The consumed return value is the exact durable
// delta and is zero for exhausted characters or non-positive requests.
func Consume(remaining, amount int64) (next, consumed int64) {
	if remaining < 0 {
		remaining = 0
	}
	if amount <= 0 || remaining == 0 {
		return remaining, 0
	}
	if amount > remaining {
		amount = remaining
	}
	return remaining - amount, amount
}

// DesiredBlackDiamondLimit changes only the two known native membership
// limits. Custom/event limits are preserved instead of being overwritten.
func DesiredBlackDiamondLimit(current int64, active bool) int64 {
	if current <= 0 {
		current = DefaultLimit
	}
	if active && current == DefaultLimit {
		return BlackDiamondLimit
	}
	if !active && current == BlackDiamondLimit {
		return DefaultLimit
	}
	return current
}

// ReconcileLimit preserves consumed fatigue while switching the maximum.
// Example: 120/156 -> 152/188 -> 120/156.
func ReconcileLimit(remaining, currentLimit, desiredLimit int64) (int64, int64, bool) {
	if desiredLimit <= 0 {
		return remaining, currentLimit, false
	}
	originalRemaining, originalLimit := remaining, currentLimit
	if currentLimit <= 0 {
		currentLimit = DefaultLimit
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > currentLimit {
		remaining = currentLimit
	}
	if currentLimit == desiredLimit {
		return remaining, currentLimit, remaining != originalRemaining || currentLimit != originalLimit
	}
	used := currentLimit - remaining
	if used < 0 {
		used = 0
	}
	nextRemaining := desiredLimit - used
	if nextRemaining < 0 {
		nextRemaining = 0
	}
	return nextRemaining, desiredLimit, true
}
