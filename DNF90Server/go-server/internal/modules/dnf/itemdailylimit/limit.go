// Package itemdailylimit owns the PVF `[total usable count]` contract -- the
// 「每天可用次数」 line the client prints in an item's tooltip.
//
// The declaration belongs to the *item*, not to the route that consumes it. The
// ordinary use request (op44), the action use request (op515), opening a booster
// box (op160/op27) and expanding a package all consume a consumable, and every
// one of them has to charge the same per-character daily counter. Keeping the
// implementation in one place is what makes that true: a route only hands over
// the item id and the declared limit, and never interprets the field itself.
//
// The counter is one durable stat row per item (`item_daily_used_<itemID>`,
// plus the legacy `fatigue_potion_used_<itemID>` spelling that stays readable so
// a counter written earlier the same service day is not lost). It shares the
// 06:00 Beijing service-day boundary of the fatigue counters, and it is
// projected to the client as op541, so the tooltip follows every use -- also for
// items whose use route the server does not otherwise touch.
package itemdailylimit

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
)

// ErrLimitReached marks the "[total usable count] reached for today" refusal so
// callers can answer the ordinary failure without consuming the item.
var ErrLimitReached = errors.New("daily item use limit reached")

// LimitReachedError reports the numbers behind that refusal.
type LimitReachedError struct {
	ItemID int64
	Used   int64
	Limit  int64
}

func (e LimitReachedError) Error() string {
	return fmt.Sprintf("item=%d daily use limit reached: used=%d limit=%d", e.ItemID, e.Used, e.Limit)
}

func (e LimitReachedError) Is(target error) bool {
	return target == ErrLimitReached
}

// Key is the durable stat key of one item's counter.
func Key(itemID int64) string {
	return dnffatigue.ItemUsageStatPrefix + strconv.FormatInt(itemID, 10)
}

// LegacyKey is the fatigue-potion spelling the counter used before the generic
// key existed.
func LegacyKey(itemID int64) string {
	return dnffatigue.PotionUsageStatPrefix + strconv.FormatInt(itemID, 10)
}

// DeclaresLimit reports whether a PVF limit caps the item. Zero (or negative)
// means the .stk carries no `[total usable count]`: no cap, and nothing is read
// or written for it.
func DeclaresLimit(limit int64) bool {
	return limit > 0
}

// Used reads today's used count. An item present under both the generic and the
// legacy spelling keeps the larger value, so it can never be counted twice.
func Used(stats map[string]int64, itemID int64) int64 {
	if stats == nil || itemID <= 0 {
		return 0
	}
	used := stats[Key(itemID)]
	if legacy := stats[LegacyKey(itemID)]; legacy > used {
		used = legacy
	}
	if used < 0 {
		return 0
	}
	return used
}

// Reconcile applies the shared 06:00 Beijing service-day boundary to stats.
//
// It delegates to the fatigue counters' own boundary on purpose: 「每天可用次数」
// has to return at the same moment the game tells the player to retry after 6
// ("请在6点以后重试"), so the two must never drift apart.
func Reconcile(stats map[string]int64, now time.Time, level int) {
	dnffatigue.ReconcileDailyStats(stats, now, level)
}

// Consume charges one use of itemID against its declared limit and returns the
// new used count.
//
// limit<=0 or itemID<=0 means the PVF declares no cap: nothing is read or
// written, and used=0 is returned. The day boundary is applied first, so a use
// that happens to be the first of a new service day never sees yesterday's
// counter. Reaching the cap returns LimitReachedError (errors.Is
// ErrLimitReached) and leaves the counter untouched.
func Consume(stats map[string]int64, itemID int64, limit int64, now time.Time, level int) (int64, error) {
	if stats == nil || itemID <= 0 || !DeclaresLimit(limit) {
		return 0, nil
	}
	Reconcile(stats, now, level)
	used := Used(stats, itemID)
	if used >= limit {
		return used, LimitReachedError{ItemID: itemID, Used: used, Limit: limit}
	}
	used++
	stats[Key(itemID)] = used
	return used, nil
}

// Release gives back one charged use when the action it was reserved for did not
// commit, so a rejected command never burns part of the player's daily
// allowance. It returns the restored count.
func Release(stats map[string]int64, itemID int64) int64 {
	if stats == nil || itemID <= 0 {
		return 0
	}
	key := Key(itemID)
	used := stats[key]
	if used <= 0 {
		return 0
	}
	used--
	stats[key] = used
	return used
}
