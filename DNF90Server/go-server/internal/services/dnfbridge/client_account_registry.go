package dnfbridge

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const clientAccountRegistrationTTL = 24 * time.Hour

const clientAccountRegistrationGrace = time.Second

type clientAccountRegistration struct {
	accountID                    string
	expiresAt                    time.Time
	remoteClientPID              uint32
	spendTimeEventInfoAuthorized bool
	spendTimeEventInfoSendState  uint8
	selectedChannelKnown         bool
	selectedChannelID            int
	selectedChannelType          uint8
	// selectedCharacterID is the character this client most recently selected.
	// A reconnect transport that reports a different one is a fresh login.
	selectedCharacterID uint16
	// launcherIdentityAttested records that this registration was accompanied
	// by a valid launcher identity proof. It is the only flag the enforcing
	// game-connection gate accepts, so an operator (or a player) who can reach
	// the loopback admin token but does not hold the launcher's private key
	// still cannot seat a session.
	launcherIdentityAttested bool
}

const (
	spendTimeEventInfoUnsent uint8 = iota
	spendTimeEventInfoSending
	spendTimeEventInfoSent
)

type clientAccountRegistry struct {
	mu         sync.Mutex
	byPID      map[uint32]clientAccountRegistration
	byRemoteIP map[netip.Addr]clientAccountRegistration
}

func newClientAccountRegistry() clientAccountRegistry {
	return clientAccountRegistry{
		byPID:      make(map[uint32]clientAccountRegistration),
		byRemoteIP: make(map[netip.Addr]clientAccountRegistration),
	}
}

func normalizeRemoteClientAccountIP(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, errors.New("remote client IP is invalid")
	}
	address = address.Unmap()
	if !address.Is4() || !address.IsPrivate() || address.IsLoopback() {
		return netip.Addr{}, errors.New("remote client IP must be a private non-loopback IPv4 address")
	}
	return address, nil
}

// RegisterRemoteClientAccount binds one SSH-authenticated Windows client to
// the private WireGuard source address observed by the Linux game listeners.
// The remote PID is retained only as audit metadata: it is not a process
// identity on the Linux host and may collide with another player's PID.
func (s *Service) RegisterRemoteClientAccount(pid uint32, clientIP, accountID string) error {
	if s == nil {
		return errors.New("dnf bridge is unavailable")
	}
	address, err := normalizeRemoteClientAccountIP(clientIP)
	if err != nil {
		return err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("client account ID is required")
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	if s.clientAccounts.byRemoteIP == nil {
		s.clientAccounts.byRemoteIP = make(map[netip.Addr]clientAccountRegistration)
	}
	for registeredIP, entry := range s.clientAccounts.byRemoteIP {
		if !entry.expiresAt.After(now) {
			delete(s.clientAccounts.byRemoteIP, registeredIP)
		}
	}
	entry, found := s.clientAccounts.byRemoteIP[address]
	if found && entry.expiresAt.After(now) && strings.TrimSpace(entry.accountID) == accountID &&
		entry.remoteClientPID == pid && entry.spendTimeEventInfoAuthorized &&
		entry.spendTimeEventInfoSendState == spendTimeEventInfoUnsent {
		entry.expiresAt = now.Add(clientAccountRegistrationTTL)
		s.clientAccounts.byRemoteIP[address] = entry
		return nil
	}
	s.clientAccounts.byRemoteIP[address] = clientAccountRegistration{
		accountID:                    accountID,
		expiresAt:                    now.Add(clientAccountRegistrationTTL),
		remoteClientPID:              pid,
		spendTimeEventInfoAuthorized: true,
	}
	return nil
}

func (s *Service) registeredRemoteClientAccount(address netip.Addr) (string, uint32, bool) {
	if s == nil {
		return "", 0, false
	}
	address = address.Unmap()
	if !address.Is4() || !address.IsPrivate() || address.IsLoopback() {
		return "", 0, false
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	entry, found := s.clientAccounts.byRemoteIP[address]
	if !found {
		return "", 0, false
	}
	if !entry.expiresAt.After(now) {
		delete(s.clientAccounts.byRemoteIP, address)
		return "", 0, false
	}
	accountID := strings.TrimSpace(entry.accountID)
	return accountID, entry.remoteClientPID, accountID != ""
}

func (s *Service) waitRegisteredRemoteClientAccount(
	ctx context.Context,
	address netip.Addr,
	wait time.Duration,
) (string, uint32, bool) {
	if accountID, pid, found := s.registeredRemoteClientAccount(address); found || wait <= 0 {
		return accountID, pid, found
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", 0, false
		case <-timer.C:
			return s.registeredRemoteClientAccount(address)
		case <-ticker.C:
			if accountID, pid, found := s.registeredRemoteClientAccount(address); found {
				return accountID, pid, true
			}
		}
	}
}

func (s *Service) sessionClientAccountRegistrationLocked(session *gameSession) (clientAccountRegistration, bool) {
	if session == nil {
		return clientAccountRegistration{}, false
	}
	if address := session.accountSourceIP.Unmap(); address.IsValid() {
		entry, found := s.clientAccounts.byRemoteIP[address]
		return entry, found
	}
	entry, found := s.clientAccounts.byPID[session.clientPID]
	return entry, found
}

func (s *Service) storeSessionClientAccountRegistrationLocked(session *gameSession, entry clientAccountRegistration) {
	if address := session.accountSourceIP.Unmap(); address.IsValid() {
		if s.clientAccounts.byRemoteIP == nil {
			s.clientAccounts.byRemoteIP = make(map[netip.Addr]clientAccountRegistration)
		}
		s.clientAccounts.byRemoteIP[address] = entry
		return
	}
	if s.clientAccounts.byPID == nil {
		s.clientAccounts.byPID = make(map[uint32]clientAccountRegistration)
	}
	s.clientAccounts.byPID[session.clientPID] = entry
}

func (s *Service) deleteSessionClientAccountRegistrationLocked(session *gameSession) {
	if address := session.accountSourceIP.Unmap(); address.IsValid() {
		delete(s.clientAccounts.byRemoteIP, address)
		return
	}
	delete(s.clientAccounts.byPID, session.clientPID)
}

// RegisterClientAccount binds one launcher-authenticated local DNF process to
// its repository account. The loopback authenticated admin endpoint is the
// only production caller; no password or session secret enters the bridge.
func (s *Service) RegisterClientAccount(pid uint32, accountID string) error {
	return s.RegisterClientAccountAttested(pid, accountID, false)
}

// RegisterClientAccountAttested is RegisterClientAccount plus the launcher
// identity result. attested=true is set only by the admin endpoint after it has
// verified the deployment's public key over (server_id, pid, account, expiry).
// No other writer may set it, because it is what the enforcing game gate trusts.
func (s *Service) RegisterClientAccountAttested(
	pid uint32,
	accountID string,
	attested bool,
) error {
	if s == nil {
		return errors.New("dnf bridge is unavailable")
	}
	accountID = strings.TrimSpace(accountID)
	if pid == 0 {
		return errors.New("client PID must be positive")
	}
	if accountID == "" {
		return errors.New("client account ID is required")
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	if s.clientAccounts.byPID == nil {
		s.clientAccounts.byPID = make(map[uint32]clientAccountRegistration)
	}
	for registeredPID, entry := range s.clientAccounts.byPID {
		if !entry.expiresAt.After(now) {
			delete(s.clientAccounts.byPID, registeredPID)
		}
	}
	entry, found := s.clientAccounts.byPID[pid]
	if found && entry.expiresAt.After(now) && strings.TrimSpace(entry.accountID) == accountID &&
		entry.spendTimeEventInfoAuthorized &&
		entry.spendTimeEventInfoSendState == spendTimeEventInfoUnsent {
		// A transport retry before the new process reaches op108 is idempotent.
		// Once sending has begun, a later launcher registration is a new process
		// lifecycle (including OS PID reuse) and must receive a fresh gate.
		entry.expiresAt = now.Add(clientAccountRegistrationTTL)
		// An attested registration must never be downgraded by a later
		// unattested retry of the same process; that would let a second local
		// caller strip the proof off a live session.
		entry.launcherIdentityAttested = entry.launcherIdentityAttested || attested
		s.clientAccounts.byPID[pid] = entry
		return nil
	}
	s.clientAccounts.byPID[pid] = clientAccountRegistration{
		accountID:                    accountID,
		expiresAt:                    now.Add(clientAccountRegistrationTTL),
		spendTimeEventInfoAuthorized: true,
		launcherIdentityAttested:     attested,
	}
	return nil
}

func (s *Service) registeredClientAccount(pid uint32) (string, bool) {
	entry, found := s.registeredClientAccountEntry(pid)
	if !found {
		return "", false
	}
	return strings.TrimSpace(entry.accountID), true
}

// registeredClientAccountEntry returns the live registration for one PID,
// including the launcher identity flag. Expired entries are dropped on read.
func (s *Service) registeredClientAccountEntry(pid uint32) (clientAccountRegistration, bool) {
	if s == nil || pid == 0 {
		return clientAccountRegistration{}, false
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	entry, found := s.clientAccounts.byPID[pid]
	if !found {
		return clientAccountRegistration{}, false
	}
	if !entry.expiresAt.After(now) {
		delete(s.clientAccounts.byPID, pid)
		return clientAccountRegistration{}, false
	}
	if strings.TrimSpace(entry.accountID) == "" {
		return clientAccountRegistration{}, false
	}
	return entry, true
}

// registeredClientAccountForPID resolves the TCP-owning game process back to
// the process that the launcher registered.  The current client can move its
// game sockets into a short-lived child process, so requiring an exact PID
// match silently falls back to the instance account even though the launcher
// authenticated a different local account.
func (s *Service) registeredClientAccountForPID(pid uint32) (accountID string, found bool, direct bool) {
	return s.registeredClientAccountForCandidates(clientAccountProcessLineage(pid))
}

func (s *Service) registeredClientAccountForCandidates(candidates []uint32) (accountID string, found bool, direct bool) {
	for index, candidate := range candidates {
		if accountID, found := s.registeredClientAccount(candidate); found {
			return accountID, true, index == 0
		}
	}
	return "", false, false
}

// attestedClientAccountForCandidates is the launcher-identity-aware form of
// registeredClientAccountForCandidates. It reports whether the matching
// registration carried a launcher identity proof, which is the only question the
// enforcing game gate asks.
func (s *Service) attestedClientAccountForCandidates(
	candidates []uint32,
) (accountID string, found bool, direct bool, attested bool) {
	for index, candidate := range candidates {
		entry, ok := s.registeredClientAccountEntry(candidate)
		if !ok {
			continue
		}
		return strings.TrimSpace(entry.accountID), true, index == 0, entry.launcherIdentityAttested
	}
	return "", false, false, false
}

// waitAttestedClientAccountForPID mirrors waitRegisteredClientAccountForPID but
// also reports the launcher identity result, so the game gate can wait out the
// same process-start race without giving up the binding requirement.
func (s *Service) waitAttestedClientAccountForPID(
	ctx context.Context,
	pid uint32,
	wait time.Duration,
) (accountID string, found bool, direct bool, attested bool) {
	lookup := func() (string, bool, bool, bool) {
		return s.attestedClientAccountForCandidates(clientAccountProcessLineage(pid))
	}
	if accountID, found, direct, attested = lookup(); found || pid == 0 || wait <= 0 {
		return accountID, found, direct, attested
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false, false, false
		case <-timer.C:
			return lookup()
		case <-ticker.C:
			if accountID, found, direct, attested = lookup(); found {
				return accountID, true, direct, attested
			}
		}
	}
}

// observeCurrentClientSelectedChannel records the last character-bearing game
// channel for one launcher-authenticated client process. Channel switches may
// close and unbind the old socket before the replacement selects its character,
// so the online-player/session indexes are not authoritative for detecting a
// team-channel return. The authenticated process registration survives that
// short reconnect gap and is reset naturally for a new client process.
func (s *Service) observeCurrentClientSelectedChannel(
	session *gameSession,
	characterID uint16,
) (previousID int, previousType uint8, found bool) {
	if s == nil || session == nil || (session.clientPID == 0 && !session.accountSourceIP.IsValid()) {
		return 0, 0, false
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	entry, registered := s.sessionClientAccountRegistrationLocked(session)
	if !registered || !entry.expiresAt.After(now) || strings.TrimSpace(entry.accountID) == "" {
		if registered && !entry.expiresAt.After(now) {
			s.deleteSessionClientAccountRegistrationLocked(session)
		}
		return 0, 0, false
	}
	previousID = entry.selectedChannelID
	previousType = entry.selectedChannelType
	found = entry.selectedChannelKnown
	// A new character over the reconnect transport still owns a fresh login.
	if session.channelReconnect && entry.selectedCharacterID != 0 && entry.selectedCharacterID != characterID {
		session.channelReconnectNewCharacter = true
	}
	entry.selectedCharacterID = characterID
	entry.selectedChannelKnown = true
	entry.selectedChannelID = session.channel.ID
	entry.selectedChannelType = session.channel.Type
	s.storeSessionClientAccountRegistrationLocked(session, entry)
	return previousID, previousType, found
}

// waitRegisteredClientAccount closes the small process-start race between the
// launcher obtaining DNF.exe's PID and its authenticated loopback admin POST.
// Without this grace, a fast client could reach a game listener first and be
// permanently classified as an unbound all-channel discovery probe.
func (s *Service) waitRegisteredClientAccount(
	ctx context.Context,
	pid uint32,
	wait time.Duration,
) (string, bool) {
	if accountID, found := s.registeredClientAccount(pid); found || pid == 0 || wait <= 0 {
		return accountID, found
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-timer.C:
			return s.registeredClientAccount(pid)
		case <-ticker.C:
			if accountID, found := s.registeredClientAccount(pid); found {
				return accountID, true
			}
		}
	}
}

func (s *Service) waitRegisteredClientAccountForPID(
	ctx context.Context,
	pid uint32,
	wait time.Duration,
) (accountID string, found bool, direct bool) {
	if accountID, found, direct = s.registeredClientAccountForPID(pid); found || pid == 0 || wait <= 0 {
		return accountID, found, direct
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false, false
		case <-timer.C:
			return s.registeredClientAccountForPID(pid)
		case <-ticker.C:
			if accountID, found, direct = s.registeredClientAccountForPID(pid); found {
				return accountID, true, direct
			}
		}
	}
}

// uniqueRegisteredClientAccount returns the account only when every active
// launcher registration names the same account. Wine does not consistently
// expose owning PIDs through GetExtendedTcpTable; this bounded single-account
// projection preserves authenticated login without guessing between accounts.
func (s *Service) uniqueRegisteredClientAccount() (string, bool) {
	accountID, _, found := s.uniqueRegisteredClientAccountFlag()
	return accountID, found
}

// uniqueRegisteredClientAccountFlag also reports whether every agreeing
// registration carried a launcher identity proof. A mixed set (one attested,
// one not) counts as unattested: the enforcing gate must not inherit trust from
// a proof that only covered some of the live processes.
func (s *Service) uniqueRegisteredClientAccountFlag() (string, bool, bool) {
	if s == nil {
		return "", false, false
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	accountID := ""
	attested := false
	for pid, entry := range s.clientAccounts.byPID {
		if !entry.expiresAt.After(now) {
			delete(s.clientAccounts.byPID, pid)
			continue
		}
		candidate := strings.TrimSpace(entry.accountID)
		if candidate == "" {
			continue
		}
		if accountID == "" {
			accountID = candidate
			attested = entry.launcherIdentityAttested
			continue
		}
		if accountID != candidate {
			return "", false, false
		}
		attested = attested && entry.launcherIdentityAttested
	}
	return accountID, attested, accountID != ""
}

func (s *Service) waitUniqueRegisteredClientAccount(
	ctx context.Context,
	wait time.Duration,
) (string, bool) {
	if accountID, found := s.uniqueRegisteredClientAccount(); found || wait <= 0 {
		return accountID, found
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-timer.C:
			return s.uniqueRegisteredClientAccount()
		case <-ticker.C:
			if accountID, found := s.uniqueRegisteredClientAccount(); found {
				return accountID, true
			}
		}
	}
}

// beginCurrentSpendTimeEventInfo reserves the single process-lifetime op108
// descriptor. A second game session for the same local client PID must not
// emit the first-process base-catalog grammar again. The in-flight state also keeps
// concurrent reconnect sessions from sending progress ahead of that first
// descriptor. PID=0 is not authoritative and therefore remains progress-only.
func (s *Service) beginCurrentSpendTimeEventInfo(session *gameSession) (send bool, ready bool) {
	if s == nil || session == nil {
		return false, false
	}
	if session.clientPID == 0 && !session.accountSourceIP.IsValid() {
		session.spendTime.eventInfoSent = true
		return false, true
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	entry, found := s.sessionClientAccountRegistrationLocked(session)
	if !found || !entry.expiresAt.After(now) {
		entry = clientAccountRegistration{expiresAt: now.Add(clientAccountRegistrationTTL)}
	}
	if !entry.spendTimeEventInfoAuthorized {
		// An unknown PID can be an already-running client reconnecting after a
		// server restart. Its singleton may already have consumed op108, so only
		// the launcher-authenticated new-process registration may authorize the
		// first-process grammar. Progress-only is the safe recovery behavior.
		session.spendTime.eventInfoSent = true
		s.storeSessionClientAccountRegistrationLocked(session, entry)
		return false, true
	}
	switch entry.spendTimeEventInfoSendState {
	case spendTimeEventInfoSent:
		session.spendTime.eventInfoSent = true
		s.storeSessionClientAccountRegistrationLocked(session, entry)
		return false, true
	case spendTimeEventInfoSending:
		s.storeSessionClientAccountRegistrationLocked(session, entry)
		return false, false
	default:
		entry.spendTimeEventInfoSendState = spendTimeEventInfoSending
		s.storeSessionClientAccountRegistrationLocked(session, entry)
		return true, true
	}
}

// finishCurrentSpendTimeEventInfo commits or releases a reservation. sent=true
// must be used as soon as the op108 socket write succeeds, even if the
// following op1206 write fails; retrying the first-process grammar would then
// be invalid for that client process.
func (s *Service) finishCurrentSpendTimeEventInfo(session *gameSession, sent bool) {
	if s == nil || session == nil {
		return
	}
	if sent {
		session.spendTime.eventInfoSent = true
	}
	if session.clientPID == 0 && !session.accountSourceIP.IsValid() {
		return
	}
	now := time.Now().UTC()
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	entry, found := s.sessionClientAccountRegistrationLocked(session)
	if !found || !entry.expiresAt.After(now) {
		entry = clientAccountRegistration{expiresAt: now.Add(clientAccountRegistrationTTL)}
	}
	if sent {
		entry.spendTimeEventInfoSendState = spendTimeEventInfoSent
	} else if entry.spendTimeEventInfoSendState == spendTimeEventInfoSending {
		entry.spendTimeEventInfoSendState = spendTimeEventInfoUnsent
	}
	s.storeSessionClientAccountRegistrationLocked(session, entry)
}
