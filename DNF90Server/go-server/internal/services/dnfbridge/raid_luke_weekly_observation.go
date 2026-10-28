package dnfbridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
)

// raid_luke_weekly_observation.go —— 「周卢克 %d/%d」的观测层。
//
// etc/raid/luke/luke.etc declares [WEEKLY ENTER COUNT] `20 2`, and the current
// client draws a weekly counter of its own (dstr 75388, "周卢克 %d/%d"). Nothing
// in this tree consumed that pair until now: `周卢克 %d/%d` never moved and never
// refused, so a Luke raid could be run without limit.
//
// This file deliberately does NOT start refusing. The document declares the two
// operands but NOT the reset weekday, and the node the live service rolls on is
// still unconfirmed -- three candidate nodes are attested (a Sunday node for the
// ticket-box count, a Thursday 06:00 node for the weekly reward cap, and the
// 2017-06 "opens Tuesday and Saturday" schedule). Enforcing a wrongly guessed
// node locks players out of a raid they are entitled to enter, which is far worse
// than the current unbounded state. So the server observes first.
//
// What it produces:
//
//   - one structured line per successful Luke gate entry carrying the character,
//     the gate, the entries counted so far, and the exact window the entry was
//     charged to -- i.e. the node the server currently believes in, printed as
//     `window_start`/`window_end`/`reset_weekday` so the real node can be read
//     straight off production logs;
//   - a distinct rollover line the first time a character's computed window
//     moves, carrying prev/next, which is the event that actually settles the node.
//
// The counter is in-memory on purpose. It is an observation aid, not durable
// state: persisting it before the node is confirmed would freeze a wrong window
// into the database. The authoritative owner is the catalog Policy
// (dungeonadmission.LukeWeeklyCharacterPolicyKey), which already derives the
// window from service time; when the node is confirmed, the durable counter and
// its commit point go there.
//
// NOT WIRED YET (deliberate): the durable counter is never committed. Committing
// it needs a decision this file cannot make on its own -- whether a "weekly
// count" is charged on raid CLEAR (the official wording: "通关2次后，该角色无法
// 进入") or on gate ENTRY. The clear-side hook lives in the phase machine, which
// this change is explicitly scoped out of.

const (
	// runtimeRaidLukeWeeklyPolicyKey is the catalog policy that owns Luke's
	// per-character weekly count.
	runtimeRaidLukeWeeklyPolicyKey = dnfdungeonadmission.LukeWeeklyCharacterPolicyKey
)

// runtimeRaidLukeWeeklyObservationEntry is one character's observed position in
// one weekly window.
type runtimeRaidLukeWeeklyObservationEntry struct {
	WindowStart time.Time
	Entries     uint32
}

type runtimeRaidLukeWeeklyObservationRegistry struct {
	mu      sync.Mutex
	entries map[uint16]runtimeRaidLukeWeeklyObservationEntry
}

var runtimeRaidLukeWeeklyObservations = runtimeRaidLukeWeeklyObservationRegistry{}

// record charges one entry and reports what stood before it. rolledOver is true
// when the character already had a record in a DIFFERENT window, which is the
// signal that the server's node has just moved; the count then restarts.
func (r *runtimeRaidLukeWeeklyObservationRegistry) record(
	characterID uint16,
	windowStart time.Time,
) (previous runtimeRaidLukeWeeklyObservationEntry, hadPrevious bool, rolledOver bool, entries uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[uint16]runtimeRaidLukeWeeklyObservationEntry)
	}
	previous, hadPrevious = r.entries[characterID]
	rolledOver = hadPrevious && !previous.WindowStart.Equal(windowStart)
	base := uint32(0)
	if hadPrevious && !rolledOver {
		base = previous.Entries
	}
	entries = base + 1
	r.entries[characterID] = runtimeRaidLukeWeeklyObservationEntry{WindowStart: windowStart, Entries: entries}
	return previous, hadPrevious, rolledOver, entries
}

// snapshotForTest reports one character's observed entry without charging it.
func (r *runtimeRaidLukeWeeklyObservationRegistry) snapshotForTest(characterID uint16) (runtimeRaidLukeWeeklyObservationEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[characterID]
	return entry, ok
}

// resetRuntimeRaidLukeWeeklyObservationsForTest drops the in-memory table so one
// test cannot observe another's counts.
func resetRuntimeRaidLukeWeeklyObservationsForTest() {
	runtimeRaidLukeWeeklyObservations.mu.Lock()
	runtimeRaidLukeWeeklyObservations.entries = nil
	runtimeRaidLukeWeeklyObservations.mu.Unlock()
}

// runtimeRaidLukeWeeklyLimitEnforced reports whether the operator has switched
// the weekly cap from observation to enforcement.
//
// OFF by default, and it stays off until the reset node has been read off real
// traffic: turning it on before that refuses entries the official service would
// have allowed. See the file header.
func (s *Service) runtimeRaidLukeWeeklyLimitEnforced() bool {
	return s != nil && s.options.raidLukeWeeklyLimitEnforced
}

// runtimeRaidLukeWeeklyWindow reports the weekly window the catalog charges Luke
// entries to, together with the per-character limit the same document declares.
// ok is false when the catalog is unavailable or does not carry the policy.
func (s *Service) runtimeRaidLukeWeeklyWindow(now time.Time) (window dnfdungeonadmission.Window, limit uint32, ok bool) {
	policy, found, err := s.currentDungeonAdmissionPolicy(runtimeRaidLukeWeeklyPolicyKey)
	if err != nil || !found || policy.Limit == 0 {
		return dnfdungeonadmission.Window{}, 0, false
	}
	resolved, err := dnfdungeonadmission.ServiceWindow(policy, now)
	if err != nil {
		return dnfdungeonadmission.Window{}, 0, false
	}
	return resolved, policy.Limit, true
}

// observeRuntimeRaidLukeWeeklyEntry logs one successful entry into a team-Luke
// gate against the weekly window the server currently believes in.
//
// It is called after the dungeon has finished loading, so it runs with no lock
// held by its caller and cannot perturb entry. Every failure path is silent: an
// observation line must never be able to fail an entry.
func (s *Service) observeRuntimeRaidLukeWeeklyEntry(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	characterID := session.selectedCharacterID
	session.dungeon.mu.Lock()
	mapID := 0
	if session.dungeon.runtime != nil {
		mapID = int(session.dungeon.runtime.Dungeon.ID)
	}
	session.dungeon.mu.Unlock()
	if mapID == 0 {
		return
	}
	// Team-Luke gates only: the weekly count is a raid-entry limit, and a
	// single-mode run is a different catalog (and a different policy owner).
	if _, isLukeGate := runtimeRaidLukeTeamDungeonPhases[mapID]; !isLukeGate {
		return
	}
	now := s.gameplayNow()
	window, limit, ok := s.runtimeRaidLukeWeeklyWindow(now)
	if !ok {
		return
	}
	previous, hadPrevious, rolledOver, entries := runtimeRaidLukeWeeklyObservations.record(characterID, window.Start)
	if rolledOver {
		s.logGameEvent(session, "game-raid-luke-weekly-rolled-over",
			"source", source,
			"char_id", characterID,
			"raid_map_id", mapID,
			"prev_window_start", previous.WindowStart.UTC().Format(time.RFC3339),
			"next_window_start", window.Start.UTC().Format(time.RFC3339),
			"prev_entries", previous.Entries,
			"next_entries", entries,
			"week_limit", limit,
			"enforced", s.runtimeRaidLukeWeeklyLimitEnforced(),
			"note", "the server's weekly window just moved for this character; this line is what settles the reset node")
	} else if !hadPrevious {
		s.logGameEvent(session, "game-raid-luke-weekly-window-first-seen",
			"source", source,
			"char_id", characterID,
			"window_start", window.Start.UTC().Format(time.RFC3339),
			"window_end", window.End.UTC().Format(time.RFC3339),
			"reset_weekday", window.Start.Weekday().String(),
			"week_limit", limit,
			"enforced", s.runtimeRaidLukeWeeklyLimitEnforced())
	}
	s.logGameEvent(session, "game-raid-luke-weekly-entry",
		"source", source,
		"char_id", characterID,
		"raid_map_id", mapID,
		"week_entries", entries,
		"week_limit", limit,
		"week_entries_over_limit", entries > limit,
		"window_start", window.Start.UTC().Format(time.RFC3339),
		"window_end", window.End.UTC().Format(time.RFC3339),
		"reset_weekday", window.Start.Weekday().String(),
		"enforced", s.runtimeRaidLukeWeeklyLimitEnforced(),
		"durable_counter_committed", false)
}

// runtimeRaidLukeWeeklyRefusal reports whether the catalog's own durable weekly
// counter already holds this character at the cap. It is consulted ONLY when
// enforcement is explicitly enabled; the default path never calls it, so no
// extra SQL runs per entry.
func (s *Service) runtimeRaidLukeWeeklyRefusal(characterID uint16) (used, limit uint32, refuse bool) {
	if s == nil || characterID == 0 || !s.runtimeRaidLukeWeeklyLimitEnforced() {
		return 0, 0, false
	}
	policy, found, err := s.currentDungeonAdmissionPolicy(runtimeRaidLukeWeeklyPolicyKey)
	if err != nil || !found || policy.Limit == 0 {
		return 0, 0, false
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return 0, policy.Limit, false
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return 0, policy.Limit, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	snapshot, ok := currentRaidDailyAvailabilitySnapshot(ctx, owner, fmt.Sprint(characterID), policy, s.gameplayNow())
	if !ok {
		return 0, policy.Limit, false
	}
	return snapshot.Used, snapshot.EffectiveLimit, snapshot.Used >= snapshot.EffectiveLimit
}
