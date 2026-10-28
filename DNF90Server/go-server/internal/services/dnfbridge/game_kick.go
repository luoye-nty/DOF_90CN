// game_kick.go implements GM-driven session kicking used by the launcher-ban
// flow. The ops tool writes the ban (account/IP/machine) into its own store,
// then asks this server to disconnect every online game session that matches
// the banned target through the loopback admin API.
package dnfbridge

import (
	"net/netip"
	"strings"
)

// KickBanType identifies which session dimension a ban matches.
type KickBanType string

const (
	KickBanAccount KickBanType = "account"
	KickBanIP      KickBanType = "ip"
	KickBanMachine KickBanType = "machine"
)

// sessionMatchesBan reports whether one online session should be kicked for
// the given ban. Account bans compare the authenticated account ID; machine
// bans compare the machine_id carried by the launcher auth frame; IP bans
// compare the peer address of the game connection. Zero/unknown dimensions
// never match, so a seat without launcher metadata survives IP/machine bans.
func sessionMatchesBan(session *gameSession, banType KickBanType, value string) bool {
	if session == nil {
		return false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	switch banType {
	case KickBanAccount:
		return session.accountID != "" && session.accountID == value
	case KickBanMachine:
		return session.machineID != "" && session.machineID == value
	case KickBanIP:
		if session.accountSourceIP.IsValid() {
			if want, err := netip.ParseAddr(value); err == nil {
				return session.accountSourceIP.Unmap().Compare(want.Unmap()) == 0
			}
		}
		return false
	default:
		return false
	}
}

// KickSessions disconnects every online game session matching the ban target.
// It returns the number of connections closed. Matching is best-effort: a
// target with no online session (or already disconnected) yields 0.
func (s *Service) KickSessions(banType KickBanType, value string) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	var targets []*gameSession
	for session := range s.onlineSeats {
		if sessionMatchesBan(session, banType, value) {
			targets = append(targets, session)
		}
	}
	s.mu.Unlock()

	for _, session := range targets {
		// Closing the transport is the canonical disconnect path: the session
		// reader observes the error, releaseOnlineSeat runs and the client is
		// dropped at the wire level. A second attempt would just no-op on an
		// already-closed connection.
		if session != nil && session.conn != nil {
			_ = session.conn.Close()
		}
	}
	return len(targets), nil
}
