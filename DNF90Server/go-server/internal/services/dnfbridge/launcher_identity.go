// launcher_identity.go implements the Launcher Identity (LID) binding: a game
// connection is accepted only when the peer can prove it was started by a
// launcher that holds the deployment's private key.
//
// Two carriers exist because the two deployment shapes put different things in
// the network path:
//
//	local  mode: DNF.exe dials 127.0.0.1:100xx directly, so the launcher proves
//	             itself out of band on the loopback admin API
//	             (GET /local/lid-info + POST /local/client-account). The signed
//	             statement covers this server instance's ID, the game process
//	             PID and the account, and the game connection is later matched
//	             back to that PID through the Windows TCP owner table.
//	remote mode: DNF.exe dials the public server address and 90CN.dll answers
//	             the in-band LANA challenge. The launcher hands the game
//	             process a short-lived signed lease, and the lease rides in the
//	             LANA frame.
//
// The identity is an ECDSA P-256 key pair. The server stores only public keys,
// so a distributed server package can never mint a proof. The launcher keeps the
// private key and only ever *offers* a proof, so the same launcher build still
// connects to servers that do not use LID at all.
package dnfbridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Launcher identity modes. "audit" keeps every existing client working and only
// records whether a proof would have been accepted, so an operator can roll the
// binding out before it starts rejecting connections.
const (
	LauncherIdentityModeOff     = "off"
	LauncherIdentityModeAudit   = "audit"
	LauncherIdentityModeEnforce = "enforce"
)

// launcherIdentityLeasePrefix versions the signed statement. Changing the field
// layout must bump it so an old launcher's proof is rejected rather than
// silently reinterpreted.
const launcherIdentityLeasePrefix = "DNF90-LID-1"

const (
	launcherIdentityScopeLocal  = "local"
	launcherIdentityScopeRemote = "remote"
	// launcherIdentityAccountSubjectPrefix marks a remote lease that carries the
	// account directly instead of a launcher-issued ticket. That is the shape
	// used when the deployment has no ops-tool ticket endpoint at all.
	launcherIdentityAccountSubjectPrefix = "acct:"
	// launcherIdentityMaxLeaseBytes bounds the LANA trailer; the longest legal
	// lease (128-byte account, 108-byte signature) is well under half of it.
	launcherIdentityMaxLeaseBytes = 640
)

// launcherIdentityMaxLeaseLifetime bounds how long a launcher may claim a
// session is still its own. A lease that outlives this is treated as malformed
// rather than trusted, so a leaked lease cannot become a permanent backdoor.
const launcherIdentityMaxLeaseLifetime = 24 * time.Hour

var (
	errLauncherIdentityDisabled    = errors.New("dnfbridge launcher identity is disabled")
	errLauncherIdentityMalformed   = errors.New("dnfbridge launcher identity proof malformed")
	errLauncherIdentityUnsupported = errors.New("dnfbridge launcher identity proof uses an unsupported key")
	errLauncherIdentitySignature   = errors.New("dnfbridge launcher identity signature mismatch")
	errLauncherIdentityExpired     = errors.New("dnfbridge launcher identity lease expired")
	errLauncherIdentityLifetime    = errors.New("dnfbridge launcher identity lease lifetime out of range")
	errLauncherIdentityScope       = errors.New("dnfbridge launcher identity lease scope mismatch")
	errLauncherIdentityServerID    = errors.New("dnfbridge launcher identity lease server mismatch")
)

// launcherIdentityVerifier holds the pinned public keys and the mode for one
// server process. The zero value is a disabled verifier, which keeps every
// caller inert when LID is not configured.
type launcherIdentityVerifier struct {
	mode     string
	serverID string
	keys     []*ecdsa.PublicKey
}

func normalizeLauncherIdentityMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case LauncherIdentityModeAudit, "log", "dry-run", "dryrun":
		return LauncherIdentityModeAudit
	case LauncherIdentityModeEnforce, "on", "true", "yes", "required":
		return LauncherIdentityModeEnforce
	default:
		return LauncherIdentityModeOff
	}
}

// newLauncherIdentityVerifier parses the pinned public keys. A mode other than
// "off" with no usable key is an error: silently degrading to "off" would ship a
// server that looks bound but accepts anyone.
func newLauncherIdentityVerifier(mode string, encodedKeys []string) (*launcherIdentityVerifier, error) {
	normalized := normalizeLauncherIdentityMode(mode)
	if normalized == LauncherIdentityModeOff {
		return &launcherIdentityVerifier{mode: LauncherIdentityModeOff}, nil
	}
	keys := make([]*ecdsa.PublicKey, 0, len(encodedKeys))
	var failures []string
	for _, encoded := range encodedKeys {
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			continue
		}
		key, err := parseLauncherIdentityPublicKey(encoded)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		if len(failures) > 0 {
			return nil, fmt.Errorf("launcher identity mode %q has no valid public key: %s",
				normalized, strings.Join(failures, "; "))
		}
		return nil, fmt.Errorf("launcher identity mode %q requires at least one public key", normalized)
	}
	serverID, err := newLauncherIdentityServerID()
	if err != nil {
		return nil, err
	}
	return &launcherIdentityVerifier{mode: normalized, serverID: serverID, keys: keys}, nil
}

func newLauncherIdentityServerID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate launcher identity server id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// enabled reports whether the operator asked for any LID handling at all.
func (v *launcherIdentityVerifier) enabled() bool {
	return v != nil && v.mode != LauncherIdentityModeOff && len(v.keys) > 0
}

// enforcing reports whether a missing or invalid proof terminates the
// connection. Audit mode records the same events but lets the legacy paths run.
func (v *launcherIdentityVerifier) enforcing() bool {
	return v != nil && v.mode == LauncherIdentityModeEnforce && len(v.keys) > 0
}

func (v *launcherIdentityVerifier) modeName() string {
	if v == nil {
		return LauncherIdentityModeOff
	}
	return v.mode
}

// launcherIdentityWantsInBandChallenge reports whether one game connection
// should be challenged in band with a LANA frame.
//
// Enforce mode always challenges: nothing gets a seat without a lease. Audit
// mode deliberately leaves loopback peers alone. A local client is already
// covered by the out-of-band signed PID bind, whose verdict the admin endpoint
// records, so challenging it would only inject a LANA frame into the operator's
// own local tooling and delay it by the challenge timeout — for no diagnostic
// value that the local carrier has not already reported.
func (s *Service) launcherIdentityWantsInBandChallenge(conn net.Conn) bool {
	if s == nil || !s.launcherIdentity.enabled() {
		return false
	}
	if s.launcherIdentity.enforcing() {
		return true
	}
	if conn == nil {
		return false
	}
	address, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		// An address shape this filter cannot classify is treated as remote, so
		// audit still observes it rather than silently skipping the check.
		return true
	}
	return !address.IP.IsLoopback()
}

// parseLauncherIdentityPublicKey accepts either a PEM block or the base64 of a
// DER SubjectPublicKeyInfo, which is what DNF90Control's keygen command writes.
func parseLauncherIdentityPublicKey(encoded string) (*ecdsa.PublicKey, error) {
	var der []byte
	if strings.Contains(encoded, "-----BEGIN") {
		block, _ := pem.Decode([]byte(encoded))
		if block == nil {
			return nil, errors.New("public key PEM block is malformed")
		}
		der = block.Bytes
	} else {
		decoded, err := decodeLauncherIdentityBase64(encoded)
		if err != nil {
			return nil, fmt.Errorf("public key is neither PEM nor base64: %w", err)
		}
		der = decoded
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: %T is not an ECDSA key", errLauncherIdentityUnsupported, parsed)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: curve %s is not P-256", errLauncherIdentityUnsupported, key.Curve.Params().Name)
	}
	return key, nil
}

func decodeLauncherIdentityBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("value is not valid base64")
}

func encodeLauncherIdentityField(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeLauncherIdentityField(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// LauncherIdentityLocalStatement is the canonical statement a launcher signs
// when it binds a local game process to an account. Binding the server instance
// ID stops a proof captured from one server run from being replayed against the
// next, and binding the PID and account stops it from being moved onto another
// process or identity. It is exported so the keygen command can print a
// self-test vector and so tests can pin the exact bytes.
func LauncherIdentityLocalStatement(serverID string, pid uint32, accountID string, expiresAt int64) string {
	return strings.Join([]string{
		launcherIdentityLeasePrefix,
		launcherIdentityScopeLocal,
		strings.ToLower(strings.TrimSpace(serverID)),
		strconv.FormatUint(uint64(pid), 10),
		encodeLauncherIdentityField(strings.TrimSpace(accountID)),
		strconv.FormatInt(expiresAt, 10),
	}, "|")
}

// LauncherIdentityRemoteStatement is the canonical statement a launcher signs
// for one remote login. The subject is either the ops-tool ticket (when the
// deployment has one) or "acct:<account id>" for a deployment that issues no
// tickets. The statement is deliberately stateless: every game channel switch
// opens a new connection with the same lease, so the server must be able to
// check it without a per-login round trip.
func LauncherIdentityRemoteStatement(subject string, expiresAt int64) string {
	return strings.Join([]string{
		launcherIdentityLeasePrefix,
		launcherIdentityScopeRemote,
		encodeLauncherIdentityField(strings.TrimSpace(subject)),
		strconv.FormatInt(expiresAt, 10),
	}, "|")
}

// launcherIdentityLease is one parsed proof, still unverified.
type launcherIdentityLease struct {
	raw       string
	statement string
	scope     string
	serverID  string
	pid       uint32
	accountID string
	subject   string
	expiresAt int64
	signature []byte
}

// parseLauncherIdentityLease splits "<statement>|<base64 signature>". Everything
// before the final separator is the signed statement, so the signature covers
// the scope, every field and the expiry.
func parseLauncherIdentityLease(value string) (launcherIdentityLease, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > launcherIdentityMaxLeaseBytes {
		return launcherIdentityLease{}, errLauncherIdentityMalformed
	}
	separator := strings.LastIndex(value, "|")
	if separator <= 0 || separator == len(value)-1 {
		return launcherIdentityLease{}, errLauncherIdentityMalformed
	}
	statement := value[:separator]
	signature, err := decodeLauncherIdentityBase64(value[separator+1:])
	if err != nil {
		return launcherIdentityLease{}, errLauncherIdentityMalformed
	}
	fields := strings.Split(statement, "|")
	if len(fields) < 4 || fields[0] != launcherIdentityLeasePrefix {
		return launcherIdentityLease{}, errLauncherIdentityMalformed
	}
	lease := launcherIdentityLease{
		raw:       value,
		statement: statement,
		scope:     fields[1],
		signature: signature,
	}
	switch lease.scope {
	case launcherIdentityScopeLocal:
		if len(fields) != 6 {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
		parsedPID, err := strconv.ParseUint(fields[3], 10, 32)
		if err != nil || parsedPID == 0 {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
		accountID, err := decodeLauncherIdentityField(fields[4])
		if err != nil || strings.TrimSpace(accountID) == "" {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
		lease.serverID = strings.ToLower(strings.TrimSpace(fields[2]))
		lease.pid = uint32(parsedPID)
		lease.accountID = strings.TrimSpace(accountID)
		lease.expiresAt, err = strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
	case launcherIdentityScopeRemote:
		if len(fields) != 4 {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
		subject, err := decodeLauncherIdentityField(fields[2])
		if err != nil || strings.TrimSpace(subject) == "" {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
		lease.subject = strings.TrimSpace(subject)
		lease.expiresAt, err = strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return launcherIdentityLease{}, errLauncherIdentityMalformed
		}
	default:
		return launcherIdentityLease{}, errLauncherIdentityMalformed
	}
	return lease, nil
}

// leaseTicket returns the ops-tool ticket a remote lease is bound to, and
// whether the lease carries one at all. An "acct:" subject has no ticket.
func (l launcherIdentityLease) leaseTicket() (string, bool) {
	if l.scope != launcherIdentityScopeRemote {
		return "", false
	}
	if strings.HasPrefix(l.subject, launcherIdentityAccountSubjectPrefix) {
		return "", false
	}
	return l.subject, true
}

// leaseAccountID returns the account a ticketless remote lease binds directly.
func (l launcherIdentityLease) leaseAccountID() (string, bool) {
	if l.scope != launcherIdentityScopeRemote {
		return "", false
	}
	accountID, ok := strings.CutPrefix(l.subject, launcherIdentityAccountSubjectPrefix)
	accountID = strings.TrimSpace(accountID)
	if !ok || accountID == "" {
		return "", false
	}
	return accountID, true
}

// verifyLease checks the lifetime and the signature. Signature is the ASN.1 DER
// form produced by .NET's ECDsa.SignData, which is what ecdsa.VerifyASN1 wants.
func (v *launcherIdentityVerifier) verifyLease(lease launcherIdentityLease, now time.Time) error {
	if v == nil || len(v.keys) == 0 {
		return errLauncherIdentityDisabled
	}
	if len(lease.signature) == 0 || lease.statement == "" {
		return errLauncherIdentityMalformed
	}
	now = now.UTC()
	expiry := time.Unix(lease.expiresAt, 0).UTC()
	if !expiry.After(now) {
		return errLauncherIdentityExpired
	}
	if expiry.After(now.Add(launcherIdentityMaxLeaseLifetime)) {
		return errLauncherIdentityLifetime
	}
	digest := sha256.Sum256([]byte(lease.statement))
	for _, key := range v.keys {
		if ecdsa.VerifyASN1(key, digest[:], lease.signature) {
			return nil
		}
	}
	return errLauncherIdentitySignature
}

// verifyLocalLease is the loopback admin-API carrier. The caller supplies the
// account and PID it is about to register, so a proof minted for a different
// account, process or server instance cannot be replayed into this one.
func (v *launcherIdentityVerifier) verifyLocalLease(
	pid uint32,
	accountID string,
	lease launcherIdentityLease,
	now time.Time,
) error {
	if !v.enabled() {
		return errLauncherIdentityDisabled
	}
	if lease.scope != launcherIdentityScopeLocal {
		return errLauncherIdentityScope
	}
	if !strings.EqualFold(lease.serverID, v.serverID) {
		return errLauncherIdentityServerID
	}
	if lease.pid != pid {
		return errLauncherIdentityMalformed
	}
	if lease.accountID != strings.TrimSpace(accountID) {
		return errLauncherIdentityMalformed
	}
	return v.verifyLease(lease, now)
}

// verifyRemoteLease is the in-band LANA carrier. When the lease is bound to a
// ticket, the frame must carry exactly that ticket; otherwise a lease could be
// reused to admit a different session.
func (v *launcherIdentityVerifier) verifyRemoteLease(
	frameTicket string,
	lease launcherIdentityLease,
	now time.Time,
) error {
	if !v.enabled() {
		return errLauncherIdentityDisabled
	}
	if lease.scope != launcherIdentityScopeRemote {
		return errLauncherIdentityScope
	}
	if subjectTicket, ok := lease.leaseTicket(); ok && !strings.EqualFold(subjectTicket, frameTicket) {
		return errLauncherIdentityMalformed
	}
	return v.verifyLease(lease, now)
}

func isLauncherAuthTicket(ticket string) bool {
	if len(ticket) != launcherAuthTicketHexLen {
		return false
	}
	_, err := hex.DecodeString(ticket)
	return err == nil
}

// LauncherIdentityInfo is what the loopback admin API reports to a launcher so
// it can tell whether this server wants a proof and which server instance ID the
// proof must cover. Reporting it costs nothing: the ID is not a secret, and the
// public key that makes it meaningful never leaves the server.
type LauncherIdentityInfo struct {
	Enabled  bool   `json:"enabled"`
	Mode     string `json:"mode"`
	ServerID string `json:"server_id"`
}

// LauncherIdentityInfo returns the current binding state for the admin API.
func (s *Service) LauncherIdentityInfo() LauncherIdentityInfo {
	if s == nil || !s.launcherIdentity.enabled() {
		return LauncherIdentityInfo{Enabled: false, Mode: LauncherIdentityModeOff}
	}
	return LauncherIdentityInfo{
		Enabled:  true,
		Mode:     s.launcherIdentity.modeName(),
		ServerID: s.launcherIdentity.serverID,
	}
}

// VerifyLauncherIdentityLease checks a local-scope lease against the PID and
// account it claims. The bool reports whether the registration may be treated as
// launcher-attested; the error is the verdict for logging.
//
// Audit mode returns (false, err) exactly like enforce mode, because the caller
// decides whether a verdict is fatal. Keeping the decision at the call site is
// what lets the same verification run in both modes without a second code path
// that could drift.
func (s *Service) VerifyLauncherIdentityLease(
	pid uint32,
	accountID string,
	lease string,
) (bool, error) {
	if s == nil || !s.launcherIdentity.enabled() {
		return false, errLauncherIdentityDisabled
	}
	parsed, err := parseLauncherIdentityLease(lease)
	if err != nil {
		return false, err
	}
	if err := s.launcherIdentity.verifyLocalLease(pid, accountID, parsed, time.Now()); err != nil {
		return false, err
	}
	return true, nil
}

// RegisterLauncherIdentityLease records a loopback bind together with the
// launcher identity verdict. A rejected proof is only fatal when the binding is
// enforced: while it is merely observed, the registration is still written so
// the rest of the system behaves exactly as it did before, and the enforcing
// game gate is the only place that refuses it.
//
// LogLauncherIdentityVerdict records a rejected loopback proof at the level the
// mode deserves: a warning an operator must act on when the binding is enforced,
// and a plain diagnostic while it is only being observed.
func (s *Service) LogLauncherIdentityVerdict(pid uint32, accountID string, err error) {
	if s == nil || err == nil {
		return
	}
	s.logPacketEvent("game-lid-audit-verdict",
		"client_pid", pid, "account_id", accountID, "error", err)
	if s.launcherIdentity.enforcing() {
		s.logWarn("dnfbridge launcher identity rejected a loopback account bind",
			"client_pid", pid, "account_id", accountID, "error", err)
		return
	}
	s.logWarn("dnfbridge launcher identity audit: loopback account bind carried no valid proof",
		"client_pid", pid, "account_id", accountID, "error", err)
}

// RejectBareLauncherIdentityBind is the log line for an enforcing server that
// received a bind request with no proof at all. It lives here, next to the rest
// of the binding policy, so the message and the level cannot drift apart.
func (s *Service) RejectBareLauncherIdentityBind(pid uint32, accountID string) error {
	s.logPacketEvent("game-lid-rejected",
		"client_pid", pid, "account_id", accountID, "reason", "loopback-bind-without-proof")
	s.logWarn("dnfbridge launcher identity rejected a loopback account bind without a proof",
		"client_pid", pid, "account_id", accountID)
	return errLauncherIdentityMalformed
}
