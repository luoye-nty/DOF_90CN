// peer_notice_stats.go builds the two op7 prompt rows the requesting client
// cannot supply for itself.
//
// The mode-10 op10 request self-reports (复活币, 抗魔值, 0) -- see
// party.PeerNoticeStats for the measurement -- while the op7 prompt the
// responder's client renders reads (复活币, 疲劳值, 虚弱状态, 抗魔值). Only the
// first and the last of those four exist in the request, so echoing the request
// can never fill the middle two correctly: it put 抗魔值 on the 疲劳值 row and
// left 虚弱状态 at 0, which on the client's 0..100 scale reads as maximum
// weakness. 疲劳值 and 虚弱状态 therefore come from the requester's own
// character record, which is where the dungeon-entry path reads them too.
package dnfbridge

import (
	"context"
	"strconv"

	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/party"
)

// currentPeerNoticeModeUsesStats reports whether this op7 reply tail carries
// the 疲劳值/虚弱状态/抗魔值 rows.
//
// 0 and 12 are the ordinary party invite and its variant: they write the same
// three rows as 10 and differ only in the trailing u32, and the operator saw
// the same wrong rows on the party prompt until they were fixed together. The
// remaining modes either have no value tail (1, 4, 13) or a tail that never
// carried these rows at all (15), so they must not pay for a character read.
func currentPeerNoticeModeUsesStats(mode byte) bool {
	switch mode {
	case 0, 10, 12:
		return true
	default:
		return false
	}
}

// currentPeerNoticeStats reads the op7 prompt rows for the character that sent
// the request.
//
// Modes outside currentPeerNoticeModeUsesStats get the zero value: their tail
// does not carry these rows, so loading the character record for them would add
// a database read that changes nothing on the wire.
//
// A read failure is not fatal. The invite or admission is already authorised by
// the time this runs, and a prompt missing two numbers is better than a prompt
// that never arrives, so a failed load falls back to the values the request
// carried plus a healthy 虚弱状态.
func (s *Service) currentPeerNoticeStats(session *gameSession, req party.RequestPeerRequest) party.PeerNoticeStats {
	if !currentPeerNoticeModeUsesStats(req.Mode) {
		return party.PeerNoticeStats{}
	}
	stats := party.PeerNoticeStats{
		// 100 is the client's healthy end of the 虚弱 condition scale; see
		// weakness_state.go for why the row cannot be filled with 0.
		Weakness: uint16(currentWeaknessHealthyValue),
		// 抗魔值 is the one row this server cannot compute: nothing in this
		// tree tracks it, so the requester's own report is the only source.
		Exorcism: req.Value1,
	}
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return stats
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return stats
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		s.logGameEvent(session, "game-peer-notice-stats-load-failed",
			"char_id", session.selectedCharacterID, "found", found, "error", err)
		return stats
	}
	if remaining, remainingKnown := dnffatigue.RemainingStat(character.Stats); remainingKnown {
		stats.Fatigue = uint16(min(max(remaining, 0), 65535))
	}
	stats.Weakness = uint16(currentWeaknessCondition(currentWeaknessLevel(character)))
	s.logGameEvent(session, "game-peer-notice-stats-built",
		"char_id", session.selectedCharacterID,
		"fatigue", stats.Fatigue,
		"weakness", stats.Weakness,
		"weakness_level", currentWeaknessLevel(character),
		"exorcism", stats.Exorcism,
		"source", "character_record_fatigue_and_stamina_plus_request_value1")
	return stats
}
