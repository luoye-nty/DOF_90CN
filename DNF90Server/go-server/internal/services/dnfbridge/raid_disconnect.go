package dnfbridge

import (
	"fmt"
	"time"
)

// A dropped transport keeps its attack-party seat for a grace window instead of
// being removed on the spot: the roster row is marked disconnected for the
// other members, the same character may reconnect into the same seat, and only
// a grace window that goes unanswered removes the member -- exactly like the
// 退出攻坚队 the client would otherwise have to perform by hand. A leader who
// never returns hands the raid to the next member in roster order rather than
// freezing it, and a drop that empties the roster disbands the raid.
//
// The grace applies whatever the raid is doing. It used to be cut short once the
// roster was frozen, because removal was refused for a started raid; now that a
// leave always goes through, the frozen case removes the member like any other.
//
// Hypothesis source (never authority): the read-only 86JP C# server,
// Game/Raid/RaidManager.cs (RaidDisconnectGraceMilliseconds, LeaveLocked
// successor rule) and RaidHandler.cs (ExpireRaidDisconnectGraceAsync).
//
// runtimeRaidDisconnectGrace is only the fallback now: the window itself comes
// from the raid's own PVF, [DISCONN MEMBER WAIT TIME] (300 in both the shipped
// etc/raid/anton.etc and etc/raid/luke/luke.etc). See
// runtimeRaidDisconnectGraceForState.
const (
	runtimeRaidDisconnectGrace             = 5 * time.Minute
	runtimeRaidMemberUserStateOnline  byte = 1
	runtimeRaidMemberUserStateOffline byte = 0
)

// runtimeRaidDisconnectGraceForState is how long this raid holds a dropped
// member's seat: the [DISCONN MEMBER WAIT TIME] its own catalog declares, or the
// 300-second fallback when no catalog covers the state (a hand-built fixture, an
// unavailable config) or the document omits the section.
//
// Both shipped raids declare 300, which is exactly the constant it replaces, so
// wiring the PVF through changes no behaviour -- it only stops the value from
// drifting if the document is ever edited.
func runtimeRaidDisconnectGraceForState(state *runtimeRaidState) time.Duration {
	if state != nil {
		if config := runtimeRaidBaseConfig(*state); config != nil && config.DisconnMemberWaitTime > 0 {
			return time.Duration(config.DisconnMemberWaitTime) * time.Second
		}
	}
	return runtimeRaidDisconnectGrace
}

type runtimeRaidDisconnectReservation struct {
	RaidKey        uint32
	CharacterID    uint16
	Generation     uint64
	DisconnectedAt time.Time
	ExpiresAt      time.Time
}

func runtimeRaidDisconnectTimerName(raidKey uint32, characterID uint16) string {
	return fmt.Sprintf("raid-disconnect:%d:%d", raidKey, characterID)
}

// noteRuntimeRaidSessionDisconnected reserves the departing member's seat. It
// is called from the canonical session teardown, before the session leaves the
// online table, so the disconnecting socket itself is excluded from the
// roster broadcast.
func (s *Service) noteRuntimeRaidSessionDisconnected(session *gameSession, reason string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	characterID := session.selectedCharacterID
	generation := session.characterGeneration
	now := s.gameplayNow()

	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil {
		s.raidMu.Unlock()
		return
	}
	index := runtimeRaidMemberIndex(*state, characterID)
	if index < 0 {
		s.raidMu.Unlock()
		return
	}
	state.Members[index].UserState = runtimeRaidMemberUserStateOffline
	// The member's session took its dungeon run with it. Releasing the run here is
	// what keeps a client crash from squatting on a gate forever: the run can no
	// longer be completed or abandoned, so its occupancy has to go with the socket.
	// The seat itself is still held for the grace window below.
	s.releaseRuntimeRaidRunsOwnedByLocked(state, characterID)
	if s.raidDisconnects == nil {
		s.raidDisconnects = make(map[uint16]runtimeRaidDisconnectReservation)
	}
	grace := runtimeRaidDisconnectGraceForState(state)
	reservation := runtimeRaidDisconnectReservation{
		RaidKey:        state.RaidKey,
		CharacterID:    characterID,
		Generation:     generation,
		DisconnectedAt: now,
		ExpiresAt:      now.Add(grace),
	}
	s.raidDisconnects[characterID] = reservation
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	if err := s.scheduleRuntimeRaidDisconnectExpiry(reservation, grace); err != nil {
		s.logPacketEvent("game-raid-disconnect-timer-failed",
			"raid_key", snapshot.RaidKey, "char_id", characterID, "error", err)
	}
	if err := s.broadcastRuntimeRaidRefreshExcept(snapshot, characterID); err != nil {
		s.logPacketEvent("game-raid-disconnect-refresh-failed",
			"raid_key", snapshot.RaidKey, "char_id", characterID, "error", err)
	}
	s.logGameEvent(session, "game-raid-member-disconnected-reserved",
		"raid_key", snapshot.RaidKey,
		"char_id", characterID,
		"reason", reason,
		"started", snapshot.Started,
		"grace_seconds", int(grace/time.Second))
}

// restoreRuntimeRaidPresence clears a reservation for a character whose new
// session has become canonical and republishes the row as online.
func (s *Service) restoreRuntimeRaidPresence(session *gameSession, reason string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	characterID := session.selectedCharacterID

	s.raidMu.Lock()
	reservation, reserved := s.raidDisconnects[characterID]
	if reserved {
		delete(s.raidDisconnects, characterID)
	}
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil {
		s.raidMu.Unlock()
		return
	}
	index := runtimeRaidMemberIndex(*state, characterID)
	if index < 0 || (!reserved && state.Members[index].UserState == runtimeRaidMemberUserStateOnline) {
		s.raidMu.Unlock()
		return
	}
	state.Members[index].UserState = runtimeRaidMemberUserStateOnline
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	if reserved {
		if queue := s.ensureGameplayTimeQueue(); queue != nil {
			queue.Cancel(runtimeRaidDisconnectTimerName(reservation.RaidKey, characterID))
		}
	}
	if err := s.broadcastRuntimeRaidRefresh(snapshot); err != nil {
		s.logPacketEvent("game-raid-presence-restore-refresh-failed",
			"raid_key", snapshot.RaidKey, "char_id", characterID, "error", err)
	}
	s.logGameEvent(session, "game-raid-member-presence-restored",
		"raid_key", snapshot.RaidKey,
		"char_id", characterID,
		"reason", reason,
		"had_reservation", reserved)
}

func (s *Service) scheduleRuntimeRaidDisconnectExpiry(reservation runtimeRaidDisconnectReservation, delay time.Duration) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	return queue.ScheduleAfter(
		runtimeRaidDisconnectTimerName(reservation.RaidKey, reservation.CharacterID),
		delay,
		func(due time.Time) { s.expireRuntimeRaidDisconnect(reservation, due) },
	)
}

// expireRuntimeRaidDisconnect is the grace-window callback. Only the exact
// reservation that armed it may act; a reconnect replaces the reservation and
// leaves this callback stale.
func (s *Service) expireRuntimeRaidDisconnect(armed runtimeRaidDisconnectReservation, due time.Time) {
	due = runtimeRaidNormalizeTime(due)
	s.raidMu.Lock()
	current, reserved := s.raidDisconnects[armed.CharacterID]
	if !reserved || current.RaidKey != armed.RaidKey || current.Generation != armed.Generation {
		s.raidMu.Unlock()
		return
	}
	if due.Before(current.ExpiresAt) {
		remaining := current.ExpiresAt.Sub(due)
		s.raidMu.Unlock()
		if err := s.scheduleRuntimeRaidDisconnectExpiry(current, remaining); err != nil {
			s.logPacketEvent("game-raid-disconnect-timer-failed",
				"raid_key", armed.RaidKey, "char_id", armed.CharacterID, "error", err)
		}
		return
	}
	state := s.raids[armed.RaidKey]
	if state == nil || runtimeRaidMemberIndex(*state, armed.CharacterID) < 0 {
		delete(s.raidDisconnects, armed.CharacterID)
		s.raidMu.Unlock()
		return
	}
	// The grace window is the whole of the drop rule: a member who does not come
	// back inside it leaves the raid, whether or not the raid has started. This
	// used to defer forever once the roster was frozen (StartPending/Started),
	// because removal itself was refused for a started raid back then; now that a
	// leave always goes through, the deferral would only keep a seat nobody can
	// use and leave the other members looking at somebody who is not coming back.
	delete(s.raidDisconnects, armed.CharacterID)
	previousLeader := state.LeaderID
	remaining, removed, disbanded := s.removeRuntimeRaidMemberLocked(armed.CharacterID)
	var snapshot runtimeRaidState
	if remaining != nil {
		snapshot = cloneRuntimeRaidState(*remaining)
	}
	s.raidMu.Unlock()
	if !removed {
		return
	}
	if disbanded {
		s.refreshRuntimePublicRaidDirectories("raid_disconnect_grace_expired")
		s.logPacketEvent("game-raid-disconnect-grace-expired",
			"raid_key", armed.RaidKey, "char_id", armed.CharacterID, "disbanded", true)
		return
	}
	if err := s.broadcastRuntimeRaidRefresh(snapshot); err != nil {
		s.logPacketEvent("game-raid-disconnect-grace-refresh-failed",
			"raid_key", armed.RaidKey, "char_id", armed.CharacterID, "error", err)
	}
	if err := s.broadcastRuntimeRaidBuffRegistration(snapshot, 0); err != nil {
		s.logPacketEvent("game-raid-disconnect-grace-buff-failed",
			"raid_key", armed.RaidKey, "char_id", armed.CharacterID, "error", err)
	}
	s.logPacketEvent("game-raid-disconnect-grace-expired",
		"raid_key", armed.RaidKey,
		"char_id", armed.CharacterID,
		"disbanded", false,
		"leader_changed", snapshot.LeaderID != previousLeader,
		"leader_id", snapshot.LeaderID,
		"member_count", len(snapshot.Members))
}

// runtimeRaidSuccessorLocked picks who takes over a departing leader: the first
// row of the roster the client displays, skipping anyone sitting in its own
// disconnect grace window so the raid does not land on somebody who cannot start
// it. When every survivor is inside a grace window roster order decides and a
// later reconnect/expiry settles it. Caller holds raidMu.
func (s *Service) runtimeRaidSuccessorLocked(state runtimeRaidState) uint16 {
	if len(state.Members) == 0 {
		return 0
	}
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	for _, member := range members {
		if reservation, reserved := s.raidDisconnects[member.CharID]; reserved && reservation.RaidKey == state.RaidKey {
			continue
		}
		return member.CharID
	}
	return members[0].CharID
}

// runtimeRaidMemberDisconnectedLocked reports whether a member currently holds
// a disconnect reservation in this raid. Caller holds raidMu.
func (s *Service) runtimeRaidMemberDisconnectedLocked(state runtimeRaidState, characterID uint16) bool {
	reservation, reserved := s.raidDisconnects[characterID]
	return reserved && reservation.RaidKey == state.RaidKey
}
