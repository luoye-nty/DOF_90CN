package dnfbridge

import (
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) repositoryGroup() (dnfrepo.Group, bool) {
	if s == nil || s.repositoryProvider == nil {
		return dnfrepo.Group{}, false
	}
	return s.repositoryProvider()
}

// identityRequired reports whether this server refuses to fall back to the
// instance account. Both the ticket gate and an enforced launcher identity mean
// the same thing here: an unbound request is unauthenticated traffic and must
// never inherit the operator's identity.
func (s *Service) identityRequired() bool {
	if s == nil {
		return false
	}
	return s.options.launcherAuthEnabled || s.launcherIdentity.enforcing()
}

// accountID returns the instance-level account. With launcher auth or an
// enforced launcher identity there is no instance default account: a request
// that reaches this point is unauthenticated external traffic and must never
// fall back to the local dnf:1 / DNFBRIDGE_ACCOUNT_ID identity (that would
// silently merge public players into the operator account).
func (s *Service) accountID() string {
	if s.identityRequired() {
		return ""
	}
	if s != nil {
		if value := strings.TrimSpace(s.options.accountID); value != "" {
			return value
		}
		prefix := strings.TrimSpace(s.options.accountPrefix)
		if prefix == "" {
			prefix = defaultAccountPrefix
		}
		return prefix + "1"
	}
	return defaultAccountPrefix + "1"
}

func (s *Service) accountIDForSession(sessions ...*gameSession) string {
	if len(sessions) > 0 && sessions[0] != nil {
		if value := strings.TrimSpace(sessions[0].accountID); value != "" {
			return value
		}
		if value, found := s.registeredClientAccount(sessions[0].clientPID); found {
			return value
		}
		if s.identityRequired() {
			// Identity is required yet this session carries no launcher-bound
			// account. Reject the connection outright instead of leaking it
			// into the instance fallback; gameplay handlers that reach this
			// point are invoked with a session whose connection is closed.
			session := sessions[0]
			s.logPacketEvent("game-launcher-auth-rejected",
				"conn_id", session.connID,
				"remote", session.conn.RemoteAddr().String(),
				"local", session.conn.LocalAddr().String(),
				"channel_id", session.channel.ID,
				"channel_type", session.channel.Type,
				"game_port", session.channel.Port,
				"reason", "account-fallback-under-launcher-auth")
			s.logWarn("dnfbridge rejected account fallback under launcher auth",
				"conn_id", session.connID,
				"remote", session.conn.RemoteAddr().String(),
				"account_id", session.accountID)
			_ = session.conn.Close()
			return ""
		}
	}
	if s.identityRequired() {
		s.logWarn("dnfbridge account fallback rejected under launcher auth (no session)")
		return ""
	}
	return s.accountID()
}
