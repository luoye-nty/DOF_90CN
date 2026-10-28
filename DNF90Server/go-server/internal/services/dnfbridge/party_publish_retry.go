package dnfbridge

import (
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// currentPartySnapshotPublishRetryDelays bounds the re-publish attempts after a
// party formation was deferred because the peer endpoint table could not be
// built yet.
//
// The first entry is short because the client's native UDP probe answer is
// normally already in flight when the invite is accepted; the later ones cover a
// client that only probes again when its scene changes (town entry, selector
// return). The list is deliberately finite and pre-scheduled: a party formation
// must never be able to arm an unbounded chain of timers, and the operator can
// read the attempt count off the log.
var currentPartySnapshotPublishRetryDelays = []time.Duration{
	time.Second,
	3 * time.Second,
	7 * time.Second,
	15 * time.Second,
}

// scheduleManagedRuntimePartySnapshotRetry re-runs the two-phase formation frame
// after publishManagedRuntimePartySnapshots deferred it.
//
// Live 2026-09-29 on 203.0.113.12: twelve game-managed-runtime-party-snapshot-deferred
// events, all with reason native_peer_endpoint_table_incomplete_before_two_phase_publish
// (party 48, member 32), and the operator's report is the matching symptom --
// "有时候组队还组不上 显示连接中". A deferral sends nothing at all: the accepting
// player holds the class1 ACK while no member ever receives op11/op9/op153, so the
// client has nothing to resolve the peer endpoint from and says 连接中 forever.
//
// Every attempt re-reads the CURRENT manager snapshot and re-runs the same
// admission gate, so a party that has since been dissolved, re-formed or
// published is left alone. Arming a new token supersedes the previous token's
// pending attempts, and a successful publish settles the token outright -- which
// is what makes it safe to re-publish at all, because op11 is a formation
// handshake and replaying it over a live link makes the client tear the peer
// connection down and re-enter the connecting state.
func (s *Service) scheduleManagedRuntimePartySnapshotRetry(
	state alignedcmd.PartyState,
	memberID uint16,
	reason string,
) {
	if s == nil || state.PartyID <= 0 || state.PartyID >= int(^uint16(0)) {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	token := s.armManagedRuntimePartySnapshotRetry(state.PartyID)
	captured := cloneRuntimePartyState(state)
	for attempt, delay := range currentPartySnapshotPublishRetryDelays {
		attempt, delay := attempt, delay
		last := attempt == len(currentPartySnapshotPublishRetryDelays)-1
		name := fmt.Sprintf("party-snapshot-publish-retry:%d:%d", captured.PartyID, attempt)
		if err := queue.ScheduleAfter(name, delay, func(time.Time) {
			if !s.managedRuntimePartySnapshotRetryCurrent(captured.PartyID, token) {
				// Either a later deferral re-armed the sequence or the formation frame
				// has already been published for this membership.
				return
			}
			published, err := s.publishManagedRuntimePartySnapshots(captured)
			switch {
			case err != nil:
				s.logGameEvent(nil, "game-managed-runtime-party-snapshot-retry-failed",
					"party_id", captured.PartyID,
					"member_id", memberID,
					"attempt", attempt,
					"delay_seconds", int(delay/time.Second),
					"defer_reason", reason,
					"error", err)
			case published:
				s.logGameEvent(nil, "game-managed-runtime-party-snapshot-retry-published",
					"party_id", captured.PartyID,
					"member_id", memberID,
					"attempt", attempt,
					"delay_seconds", int(delay/time.Second),
					"defer_reason", reason,
					"member_count", len(runtimePartyMembers(captured)))
			case last:
				s.logGameEvent(nil, "game-managed-runtime-party-snapshot-retry-unresolved",
					"party_id", captured.PartyID,
					"member_id", memberID,
					"attempts", len(currentPartySnapshotPublishRetryDelays),
					"last_delay_seconds", int(delay/time.Second),
					"defer_reason", reason)
			}
		}); err != nil {
			s.logGameEvent(nil, "game-managed-runtime-party-snapshot-retry-schedule-failed",
				"party_id", captured.PartyID,
				"member_id", memberID,
				"attempt", attempt,
				"error", err)
			return
		}
	}
}

// armManagedRuntimePartySnapshotRetry starts a new retry epoch for a party and
// returns its token. A later arming for the same party invalidates the earlier
// token, so two deferrals cannot race each other into a double publish.
func (s *Service) armManagedRuntimePartySnapshotRetry(partyID int) uint64 {
	s.partyPublishMu.Lock()
	defer s.partyPublishMu.Unlock()
	s.partyPublishSerial++
	token := s.partyPublishSerial
	if s.partyPublishFormation == nil {
		s.partyPublishFormation = make(map[int]uint64)
	}
	s.partyPublishFormation[partyID] = token
	return token
}

// managedRuntimePartySnapshotRetryCurrent reports whether a scheduled attempt is
// still the live one for its party.
func (s *Service) managedRuntimePartySnapshotRetryCurrent(partyID int, token uint64) bool {
	if s == nil || token == 0 {
		return false
	}
	s.partyPublishMu.Lock()
	defer s.partyPublishMu.Unlock()
	return s.partyPublishFormation[partyID] == token
}

// settleManagedRuntimePartySnapshotRetry retires the pending retries for a party
// whose formation frame has been published. Every scheduled attempt for that
// party becomes a no-op, so the formation handshake is never replayed.
func (s *Service) settleManagedRuntimePartySnapshotRetry(partyID int) {
	if s == nil || partyID <= 0 {
		return
	}
	s.partyPublishMu.Lock()
	defer s.partyPublishMu.Unlock()
	delete(s.partyPublishFormation, partyID)
}
