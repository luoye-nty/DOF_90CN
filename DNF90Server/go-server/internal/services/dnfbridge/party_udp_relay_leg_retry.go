package dnfbridge

import (
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// currentPartyUDPRelayLegRetryDelays bounds the re-sync attempts after a party was
// published before every member's UDP probe answer had been observed. The first entry is
// short because the answer is normally already in flight; the later ones cover a client
// that only re-probes when its scene changes.
var currentPartyUDPRelayLegRetryDelays = []time.Duration{
	time.Second,
	3 * time.Second,
	7 * time.Second,
}

// schedulePartyUDPRelayLegRetry re-runs the relay sync a few times after a party was
// published with a member whose UDP endpoint was not registered yet.
//
// Live 2026-09-20: raid parties 15 and 16 were published at 14:21:18, carried zero
// relay packets for their whole life (both members saw each other standing still), and
// the re-formed party 17 relayed immediately -- the operator's "重新组队后 就又可以了".
// The registration is what the relay forwards to, so a party that is formed a moment too
// early must pick it up afterwards instead of staying legless.
//
// The attempts are pre-scheduled (not chained), so a party can never schedule without
// bound: every attempt re-syncs with allowRetry=false, and only a fresh publish re-arms
// the sequence.
func (s *Service) schedulePartyUDPRelayLegRetry(state alignedcmd.PartyState) {
	if s == nil || state.PartyID <= 0 || len(runtimePartyMembers(state)) < 2 {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	captured := cloneRuntimePartyState(state)
	for attempt, delay := range currentPartyUDPRelayLegRetryDelays {
		attempt := attempt
		name := fmt.Sprintf("party-relay-leg-retry:%d:%d", captured.PartyID, attempt)
		if err := queue.ScheduleAfter(name, delay, func(time.Time) {
			if !s.syncRuntimePartyUDPRelayLegs(captured, false) {
				s.logPacketEvent("game-party-udp-relay-leg-retry-unresolved",
					"party_id", captured.PartyID,
					"attempt", attempt,
					"delay_seconds", int(delay/time.Second))
				s.recordRaidEvent(nil, "relay_leg_unresolved", map[string]any{
					"party_id":       captured.PartyID,
					"attempt":        attempt,
					"delay_seconds":  int(delay / time.Second),
					"result":         "still_unresolved",
					"reason":         "the_client_udp_probe_answer_for_that_member_has_not_been_observed",
					"client_visible": "that_member_keeps_a_relay_leg_with_no_seed_endpoint_and_is_learned_from_live_traffic",
				})
			}
		}); err != nil {
			s.logPacketEvent("game-party-udp-relay-leg-retry-schedule-failed",
				"party_id", captured.PartyID,
				"attempt", attempt,
				"error", err)
			return
		}
	}
}
