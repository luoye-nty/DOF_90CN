package dnfbridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// lidTestSigner stands in for the launcher: it holds the private half and signs
// the canonical statement the same way .NET's ECDsa.SignData does.
type lidTestSigner struct {
	key *ecdsa.PrivateKey
}

func newLIDTestSigner(t *testing.T) *lidTestSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &lidTestSigner{key: key}
}

func (s *lidTestSigner) encodedPublicKey(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func (s *lidTestSigner) sign(t *testing.T, statement string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(statement))
	signature, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Base64 with both alphabets is accepted on the way back in; StdEncoding is
	// what .NET Convert.ToBase64String produces.
	return statement + "|" + base64.StdEncoding.EncodeToString(signature)
}

func newLIDTestVerifier(t *testing.T, mode string, signer *lidTestSigner) *launcherIdentityVerifier {
	t.Helper()
	verifier, err := newLauncherIdentityVerifier(mode, []string{signer.encodedPublicKey(t)})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return verifier
}

func TestLauncherIdentityDisabledModeAcceptsNoProof(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier, err := newLauncherIdentityVerifier("off", []string{signer.encodedPublicKey(t)})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	if verifier.enabled() || verifier.enforcing() {
		t.Fatal("off mode must be neither enabled nor enforcing")
	}
	if err := verifier.verifyLocalLease(1, "dnf:1", launcherIdentityLease{}, time.Now()); err == nil {
		t.Fatal("off mode must reject everything rather than silently accept")
	}
}

func TestLauncherIdentityMissingKeyIsAConfigurationError(t *testing.T) {
	for _, mode := range []string{LauncherIdentityModeAudit, LauncherIdentityModeEnforce} {
		if _, err := newLauncherIdentityVerifier(mode, nil); err == nil {
			t.Fatalf("mode %q with no key must fail loudly", mode)
		}
		if _, err := newLauncherIdentityVerifier(mode, []string{"not-a-key"}); err == nil {
			t.Fatalf("mode %q with a broken key must fail loudly", mode)
		}
	}
}

func TestLauncherIdentityLocalLeaseRoundTrip(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier := newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)
	expiresAt := time.Now().Add(time.Hour).Unix()

	lease := signer.sign(t, LauncherIdentityLocalStatement(verifier.serverID, 4242, "dnf:abc", expiresAt))
	parsed, err := parseLauncherIdentityLease(lease)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if parsed.scope != launcherIdentityScopeLocal || parsed.pid != 4242 || parsed.accountID != "dnf:abc" {
		t.Fatalf("parsed lease fields = %+v", parsed)
	}
	if err := verifier.verifyLocalLease(4242, "dnf:abc", parsed, time.Now()); err != nil {
		t.Fatalf("verify local lease: %v", err)
	}
}

// The binding must cover the process and the account, not just "some launcher
// was here": a proof minted for one PID or one account must not seat another.
func TestLauncherIdentityLocalLeaseIsBoundToPIDAccountAndServer(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier := newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)
	expiresAt := time.Now().Add(time.Hour).Unix()

	parse := func(t *testing.T, statement string) launcherIdentityLease {
		t.Helper()
		parsed, err := parseLauncherIdentityLease(signer.sign(t, statement))
		if err != nil {
			t.Fatalf("parse lease: %v", err)
		}
		return parsed
	}

	lease := parse(t, LauncherIdentityLocalStatement(verifier.serverID, 100, "dnf:a", expiresAt))
	if err := verifier.verifyLocalLease(101, "dnf:a", lease, time.Now()); err == nil {
		t.Fatal("a lease for PID 100 must not authorize PID 101")
	}
	if err := verifier.verifyLocalLease(100, "dnf:b", lease, time.Now()); err == nil {
		t.Fatal("a lease for dnf:a must not authorize dnf:b")
	}

	otherServer := parse(t, LauncherIdentityLocalStatement("00112233445566778899aabbccddeeff", 100, "dnf:a", expiresAt))
	if err := verifier.verifyLocalLease(100, "dnf:a", otherServer, time.Now()); err == nil {
		t.Fatal("a lease from another server instance must not be replayed")
	}
}

func TestLauncherIdentityLeaseLifetimeIsEnforced(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier := newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)
	now := time.Now()

	expired := signer.sign(t, LauncherIdentityLocalStatement(verifier.serverID, 7, "dnf:a", now.Add(-time.Minute).Unix()))
	parsed, err := parseLauncherIdentityLease(expired)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyLocalLease(7, "dnf:a", parsed, now); err == nil {
		t.Fatal("an expired lease must be rejected")
	}

	// A long-lived lease would turn a one-off leak into a permanent backdoor.
	forever := signer.sign(t, LauncherIdentityLocalStatement(
		verifier.serverID, 7, "dnf:a", now.Add(7*24*time.Hour).Unix()))
	parsed, err = parseLauncherIdentityLease(forever)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyLocalLease(7, "dnf:a", parsed, now); err == nil {
		t.Fatal("a lease beyond the maximum lifetime must be rejected")
	}
}

func TestLauncherIdentityRejectsTamperedStatement(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier := newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)
	expiresAt := time.Now().Add(time.Hour).Unix()

	lease := signer.sign(t, LauncherIdentityLocalStatement(verifier.serverID, 5, "dnf:a", expiresAt))
	// Re-encode a different account under the same signature.
	tampered := strings.Replace(lease, encodeLauncherIdentityField("dnf:a"), encodeLauncherIdentityField("dnf:b"), 1)
	if tampered == lease {
		t.Fatal("test setup failed to tamper with the lease")
	}
	parsed, err := parseLauncherIdentityLease(tampered)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyLocalLease(5, "dnf:b", parsed, time.Now()); err == nil {
		t.Fatal("a re-encoded account must invalidate the signature")
	}
}

func TestLauncherIdentityAcceptsEveryKeyInTheWhitelist(t *testing.T) {
	first := newLIDTestSigner(t)
	second := newLIDTestSigner(t)
	verifier, err := newLauncherIdentityVerifier(LauncherIdentityModeEnforce, []string{
		first.encodedPublicKey(t),
		second.encodedPublicKey(t),
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	expiresAt := time.Now().Add(time.Hour).Unix()
	lease := second.sign(t, LauncherIdentityLocalStatement(verifier.serverID, 9, "dnf:a", expiresAt))
	parsed, err := parseLauncherIdentityLease(lease)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyLocalLease(9, "dnf:a", parsed, time.Now()); err != nil {
		t.Fatalf("a whitelisted key must verify: %v", err)
	}

	outsider := newLIDTestSigner(t)
	rejected := outsider.sign(t, LauncherIdentityLocalStatement(verifier.serverID, 9, "dnf:a", expiresAt))
	parsed, err = parseLauncherIdentityLease(rejected)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyLocalLease(9, "dnf:a", parsed, time.Now()); err == nil {
		t.Fatal("a key that is not pinned must not verify")
	}
}

func TestLauncherIdentityRemoteLeaseBindsTheTicket(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier := newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)
	expiresAt := time.Now().Add(time.Hour).Unix()
	ticket := strings.Repeat("ab", 32)

	lease := signer.sign(t, LauncherIdentityRemoteStatement(ticket, expiresAt))
	parsed, err := parseLauncherIdentityLease(lease)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyRemoteLease(ticket, parsed, time.Now()); err != nil {
		t.Fatalf("verify remote lease: %v", err)
	}
	// The lease is signed for one ticket; presenting another must fail.
	if err := verifier.verifyRemoteLease(strings.Repeat("cd", 32), parsed, time.Now()); err == nil {
		t.Fatal("a lease bound to ticket ab.. must not authorize ticket cd..")
	}
	if accountID, ok := parsed.leaseAccountID(); ok || accountID != "" {
		t.Fatalf("a ticket lease must not report an account, got %q", accountID)
	}
}

func TestLauncherIdentityRemoteLeaseCanCarryAnAccount(t *testing.T) {
	signer := newLIDTestSigner(t)
	verifier := newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)
	expiresAt := time.Now().Add(time.Hour).Unix()

	lease := signer.sign(t, LauncherIdentityRemoteStatement(
		launcherIdentityAccountSubjectPrefix+"dnf:z", expiresAt))
	parsed, err := parseLauncherIdentityLease(lease)
	if err != nil {
		t.Fatalf("parse lease: %v", err)
	}
	if err := verifier.verifyRemoteLease("", parsed, time.Now()); err != nil {
		t.Fatalf("a ticketless lease must verify without a frame ticket: %v", err)
	}
	accountID, ok := parsed.leaseAccountID()
	if !ok || accountID != "dnf:z" {
		t.Fatalf("lease account = %q ok=%t", accountID, ok)
	}
	if ticket, ok := parsed.leaseTicket(); ok || ticket != "" {
		t.Fatalf("a ticketless lease must not report a ticket, got %q", ticket)
	}
}

func TestLauncherIdentityParsesRejectsMalformedLeases(t *testing.T) {
	cases := []string{
		"",
		"|",
		"no-separator",
		"|only-signature",
		"DNF90-LID-1|local|a|1|b64|999|notbase64!!",
		"DNF90-LID-1|bogus|a|1|b64|999|" + base64.StdEncoding.EncodeToString([]byte("x")),
		"DNF90-LID-1|local|a|0|b64|999|" + base64.StdEncoding.EncodeToString([]byte("x")),
		"DNF90-LID-1|local|a|notanumber|b64|999|" + base64.StdEncoding.EncodeToString([]byte("x")),
		"DNF90-LID-1|local|a|1||999|" + base64.StdEncoding.EncodeToString([]byte("x")),
	}
	for _, value := range cases {
		if _, err := parseLauncherIdentityLease(value); err == nil {
			t.Fatalf("lease %q must not parse", value)
		}
	}
}

// The LANA version 2 frame must round-trip, and a version 1 frame must keep
// meaning exactly what it meant before the lease existed.
func TestLauncherAuthFrameRoundTrip(t *testing.T) {
	ticket := strings.Repeat("1f", 32)
	machine := "90CN"

	writeV1 := func() []byte {
		body := append([]byte(ticket), machine...)
		frame := make([]byte, launcherAuthHeaderLen+len(body))
		copy(frame, launcherAuthMagicBytes)
		frame[4] = launcherAuthFrameVersion
		binary.BigEndian.PutUint16(frame[5:7], uint16(len(body)))
		copy(frame[launcherAuthHeaderLen:], body)
		return frame
	}
	decoded, err := readLauncherAuthFrame(&lidScriptedConn{read: writeV1()})
	if err != nil {
		t.Fatalf("read v1 frame: %v", err)
	}
	if decoded.Version != launcherAuthFrameVersion || decoded.Ticket != ticket || decoded.MachineID != machine {
		t.Fatalf("v1 frame = %+v", decoded)
	}
	if decoded.Lease != "" {
		t.Fatal("a v1 frame must not carry a lease")
	}

	lease := "DNF90-LID-1|remote|" + encodeLauncherIdentityField(ticket) + "|999|" + base64.StdEncoding.EncodeToString([]byte("sig"))
	writeV2 := func(leaseText string) []byte {
		body := append([]byte(ticket), machine...)
		body = append(body, leaseText...)
		// The lease length is the last field in the body, mirroring the server
		// reader: machine_id then lease then its length.
		body = binary.BigEndian.AppendUint16(body, uint16(len(leaseText)))
		frame := make([]byte, launcherAuthHeaderLen+len(body))
		copy(frame, launcherAuthMagicBytes)
		frame[4] = launcherAuthFrameVersionLease
		binary.BigEndian.PutUint16(frame[5:7], uint16(len(body)))
		copy(frame[launcherAuthHeaderLen:], body)
		return frame
	}
	decoded, err = readLauncherAuthFrame(&lidScriptedConn{read: writeV2(lease)})
	if err != nil {
		t.Fatalf("read v2 frame: %v", err)
	}
	if decoded.Version != launcherAuthFrameVersionLease || decoded.MachineID != machine {
		t.Fatalf("v2 frame = %+v", decoded)
	}
	if decoded.Lease != lease {
		t.Fatalf("v2 lease = %q want %q", decoded.Lease, lease)
	}
	if decoded.LeaseParsed.scope != launcherIdentityScopeRemote {
		t.Fatalf("v2 lease scope = %q", decoded.LeaseParsed.scope)
	}

	// A version 1 capable client challenged by a version 2 server reports an
	// empty lease rather than a frame error, so enforce mode can log a precise
	// reason instead of a parse failure.
	decoded, err = readLauncherAuthFrame(&lidScriptedConn{read: writeV2("")})
	if err != nil {
		t.Fatalf("read v2 frame without a lease: %v", err)
	}
	if decoded.Lease != "" || decoded.MachineID != machine {
		t.Fatalf("v2 frame without lease = %+v", decoded)
	}

	// An unknown version must be refused, not read as its predecessor.
	bad := writeV1()
	bad[4] = 3
	if _, err := readLauncherAuthFrame(&lidScriptedConn{read: bad}); err == nil {
		t.Fatal("an unknown frame version must be rejected")
	}
}

// TestLauncherIdentityMatchesLauncherSignature is the cross-language guard: the
// launcher signs with .NET's ECDsa and this server verifies with
// ecdsa.VerifyASN1, so the statement layout, the base64url field encoding and
// the DER signature encoding all have to agree exactly. A drift in any of them
// would only show up as "every player is refused" at runtime, which is why the
// check is written down instead of trusted.
//
// It is skipped unless DNF_LID_CROSSCHECK points at the JSON produced by the
// harness in 登录器's cross-check build, so an ordinary `go test` never depends
// on a .NET toolchain.
func TestLauncherIdentityMatchesLauncherSignature(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("DNF_LID_CROSSCHECK"))
	if path == "" {
		t.Skip("set DNF_LID_CROSSCHECK to a launcher-produced lease JSON to run this check")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cross-check payload: %v", err)
	}
	var payload struct {
		PublicKey   string `json:"public_key"`
		ServerID    string `json:"server_id"`
		PID         uint32 `json:"pid"`
		AccountID   string `json:"account_id"`
		Ticket      string `json:"ticket"`
		LocalLease  string `json:"local_lease"`
		RemoteLease string `json:"remote_lease"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode cross-check payload: %v", err)
	}

	verifier, err := newLauncherIdentityVerifier(
		LauncherIdentityModeEnforce, []string{payload.PublicKey})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	// The launcher signed against the fixed ID the harness uses, so pin it here
	// rather than the random per-process one.
	verifier.serverID = payload.ServerID

	local, err := parseLauncherIdentityLease(payload.LocalLease)
	if err != nil {
		t.Fatalf("parse launcher local lease: %v", err)
	}
	if err := verifier.verifyLocalLease(payload.PID, payload.AccountID, local, time.Now()); err != nil {
		t.Fatalf("launcher-signed local lease did not verify: %v", err)
	}
	if err := verifier.verifyLocalLease(payload.PID+1, payload.AccountID, local, time.Now()); err == nil {
		t.Fatal("the cross-check lease must still be bound to its PID")
	}

	remote, err := parseLauncherIdentityLease(payload.RemoteLease)
	if err != nil {
		t.Fatalf("parse launcher remote lease: %v", err)
	}
	if err := verifier.verifyRemoteLease(payload.Ticket, remote, time.Now()); err != nil {
		t.Fatalf("launcher-signed remote lease did not verify: %v", err)
	}
}

// Audit mode must not inject a LANA challenge into the operator's own loopback
// tooling: that traffic is already covered by the out-of-band signed PID bind,
// and a stray challenge frame would only add a timeout and seven unexpected
// bytes. Enforce mode has no such exemption.
func TestLauncherIdentityInBandChallengeScope(t *testing.T) {
	signer := newLIDTestSigner(t)
	loopback := &lidScriptedConn{remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50000}}
	remote := &lidScriptedConn{remote: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 50000}}

	off := &Service{launcherIdentity: &launcherIdentityVerifier{mode: LauncherIdentityModeOff}}
	if off.launcherIdentityWantsInBandChallenge(loopback) {
		t.Fatal("an unconfigured binding must not challenge anything")
	}

	audit := &Service{launcherIdentity: newLIDTestVerifier(t, LauncherIdentityModeAudit, signer)}
	if audit.launcherIdentityWantsInBandChallenge(loopback) {
		t.Fatal("audit mode must leave loopback peers to the out-of-band carrier")
	}
	if !audit.launcherIdentityWantsInBandChallenge(remote) {
		t.Fatal("audit mode must still observe remote peers")
	}

	enforce := &Service{launcherIdentity: newLIDTestVerifier(t, LauncherIdentityModeEnforce, signer)}
	if !enforce.launcherIdentityWantsInBandChallenge(loopback) {
		t.Fatal("enforce mode must challenge loopback peers too")
	}
	if !enforce.launcherIdentityWantsInBandChallenge(remote) {
		t.Fatal("enforce mode must challenge remote peers")
	}
}

// lidScriptedConn is the smallest net.Conn that can serve a canned byte slice.
type lidScriptedConn struct {
	read   []byte
	pos    int
	remote net.Addr
}

func (c *lidScriptedConn) Read(p []byte) (int, error) {
	if c.pos >= len(c.read) {
		return 0, io.EOF
	}
	n := copy(p, c.read[c.pos:])
	c.pos += n
	return n, nil
}

func (c *lidScriptedConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *lidScriptedConn) Close() error                { return nil }
func (c *lidScriptedConn) LocalAddr() net.Addr         { return lidAddr{} }
func (c *lidScriptedConn) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return lidAddr{}
}
func (c *lidScriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *lidScriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *lidScriptedConn) SetWriteDeadline(time.Time) error { return nil }

type lidAddr struct{}

func (lidAddr) Network() string { return "tcp" }
func (lidAddr) String() string  { return "127.0.0.1:0" }
