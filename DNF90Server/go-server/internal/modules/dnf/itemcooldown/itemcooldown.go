// Package itemcooldown owns the durable per-character item cooltime window.
//
// PVF contract. A stackable that declares `[cooltime maintenance]` (with a
// non-zero value) keeps its cooldown **across sessions**: the 90CN client's own
// gate is process-local memory only, so without a durable copy a 小退/重上 wipes
// it and the item becomes usable again. `[cool time]` is the window in
// milliseconds; `[cooltime group]` makes items of the same group share one
// window (0/absent = the item only blocks itself).
//
// Live report 2026-09-17: 首日度假礼盒 (490702504, [cool time] 86400000,
// [cooltime group] 66, [cooltime maintenance] 1 1 1) could be used again right
// after a relog. The 86 reference server implements exactly this contract
// (_86ref .../InventoryItemLifecycleService.cs: HasCooltimeMaintenance +
// CoolTime + ResolveCooltimeGroup, status CooltimeActive) and restores the
// client list through the item-state notification built from the persisted
// "cooltime" rows.
//
// Storage. One character-overflow stat row per item,
// `item_cooltime_<itemID>` = the expiry in unix seconds. Expired rows are
// pruned lazily on read, the same way the daily-use counters age.
package itemcooldown

import (
	"sort"
	"strconv"
	"strings"
)

// StatPrefix keys one durable cooldown window per item id.
const StatPrefix = "item_cooltime_"

// Entry is one stored cooldown window.
type Entry struct {
	ItemID   int64
	ExpireAt int64
}

// StatKey returns the character stat key that holds one item's expiry.
func StatKey(itemID int64) string {
	return StatPrefix + strconv.FormatInt(itemID, 10)
}

// Entries lists every stored window, sorted by item id. Expired rows are
// included: callers that want the live set use ActiveEntries.
func Entries(stats map[string]int64) []Entry {
	if len(stats) == 0 {
		return nil
	}
	entries := make([]Entry, 0, 4)
	for key, expireAt := range stats {
		if !strings.HasPrefix(key, StatPrefix) {
			continue
		}
		itemID, err := strconv.ParseInt(strings.TrimPrefix(key, StatPrefix), 10, 64)
		if err != nil || itemID <= 0 || expireAt <= 0 {
			continue
		}
		entries = append(entries, Entry{ItemID: itemID, ExpireAt: expireAt})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ItemID < entries[j].ItemID })
	return entries
}

// ActiveEntries lists the windows that are still running at `now` (unix
// seconds).
func ActiveEntries(stats map[string]int64, now int64) []Entry {
	all := Entries(stats)
	if len(all) == 0 {
		return nil
	}
	active := make([]Entry, 0, len(all))
	for _, entry := range all {
		if entry.ExpireAt > now {
			active = append(active, entry)
		}
	}
	if len(active) == 0 {
		return nil
	}
	return active
}

// Prune drops the windows that have expired, reporting whether anything
// changed so the caller can persist.
func Prune(stats map[string]int64, now int64) bool {
	changed := false
	for _, entry := range Entries(stats) {
		if entry.ExpireAt > now {
			continue
		}
		delete(stats, StatKey(entry.ItemID))
		changed = true
	}
	return changed
}

// Set stores one item's new expiry, replacing any previous window.
func Set(stats map[string]int64, itemID int64, expireAt int64) {
	if stats == nil || itemID <= 0 || expireAt <= 0 {
		return
	}
	stats[StatKey(itemID)] = expireAt
}

// Clear removes one item's window.
func Clear(stats map[string]int64, itemID int64) {
	if stats == nil || itemID <= 0 {
		return
	}
	delete(stats, StatKey(itemID))
}

// Blocks reports whether using `itemID` (whose PVF group is `group`) is still
// on cooldown for this character: a running window on the same item, or on any
// item that resolves to the same non-zero cooltime group.
//
// groupOf resolves the PVF cooltime group of a **stored** item id; it may be
// nil when the caller knows no two items share a group.
func Blocks(
	stats map[string]int64,
	now int64,
	itemID int64,
	group int64,
	groupOf func(itemID int64) int64,
) (Entry, bool) {
	for _, entry := range ActiveEntries(stats, now) {
		if entry.ItemID == itemID {
			return entry, true
		}
		if group > 0 && groupOf != nil && groupOf(entry.ItemID) == group {
			return entry, true
		}
	}
	return Entry{}, false
}

// ExpireAt returns the stored expiry of one item, if any.
func ExpireAt(stats map[string]int64, itemID int64) (int64, bool) {
	if len(stats) == 0 || itemID <= 0 {
		return 0, false
	}
	expireAt, found := stats[StatKey(itemID)]
	if !found || expireAt <= 0 {
		return 0, false
	}
	return expireAt, true
}

// MillisecondsToSeconds converts a PVF [cool time] (milliseconds) into the
// whole seconds the durable window and the client list both use. A positive
// sub-second window counts as one second so it can never become "no cooldown".
func MillisecondsToSeconds(milliseconds int64) int64 {
	if milliseconds <= 0 {
		return 0
	}
	seconds := milliseconds / 1000
	if milliseconds%1000 != 0 {
		seconds++
	}
	return seconds
}
