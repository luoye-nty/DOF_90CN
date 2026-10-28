// launcher_auth.go implements the external game-connection authentication
// gate (launcher ticket -> loopback launcher verifier). Local PID /
// WireGuard / Wine fast paths never reach this file: handleGameConn only
// enters the gate when launcherAuthEnabled is set AND no local binding won.
package dnfbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Launcher auth wire protocol. Frame layout:
//
//	[4B magic "LANA"][1B version][2B body len BE][body]
//
// Server challenge body: server_public_ip(4B BE) + random_nonce(16B).
// Version 1 client auth body: ticket(32B hex, 64 ASCII) + machine_id(<=128 ASCII, optional).
//
// Version 2 exists only to carry the launcher identity lease. It keeps the
// version 1 prefix byte-for-byte so the ticket and machine_id parsing is shared,
// and appends a length-suffixed lease:
//
//	machine_id(rest) + lease(lease_len ASCII bytes) + lease_len(2B BE)
//
// The length is deliberately last. machine_id is the only field whose length is
// not known up front, so putting the trailer's own length at the very end lets
// the reader recover every field by working backwards, without adding a
// machine_id length field that would invalidate the version 1 layout.
//
// The server advertises which version it wants in the challenge. A client that
// cannot answer version 2 (an older 90CN.dll) still works against a version 1
// challenge, which is what audit mode sends, so a rollout can be observed
// before it starts rejecting connections.
const (
	launcherAuthFrameMagic        = "LANA"
	launcherAuthFrameVersion      = 1
	launcherAuthFrameVersionLease = 2
	// launcherAuthFrameVersionS2C 只可能是**客户端**回的版本：它表示"我看懂了你
	// challenge 里的 S2C 标记，本连接可以加密回包"。body 布局与 version 2 完全一致。
	//
	// 为什么用"客户端回一个更高的版本号"来确认能力：版本号是这条握手里唯一的
	// 客户端→服务端信号位，而 challenge body 只要加一个字节就会让**旧 DLL** 忽略它
	// （旧 DLL 只校验 body 长度区间 20..1024，且完全不读 body 内容），所以：
	//   · 旧服务端（body 20 字节）→ 新客户端看不到标记 → 回 v2 → 明文（不炸）
	//   · 新服务端+旧客户端 → 客户端忽略标记、回 v2 → 服务端按 v2 处理 → 明文（不炸）
	//   · 新服务端+新客户端 → 回 v3 → 加密
	// 三种组合都不会把"没准备好解密的一端"推进密文里。
	launcherAuthFrameVersionS2C  = 3
	launcherAuthHeaderLen        = 4 + 1 + 2
	launcherAuthTicketHexLen     = 64 // 32 random bytes, ASCII hex
	launcherAuthMaxMachineID     = 128
	launcherAuthNonceLen         = 16
	launcherAuthChallengeBody    = 4 + launcherAuthNonceLen // server_public_ip + nonce
	launcherAuthChallengeS2CFlag = 1                        // 尾部 1 字节：请求 S2C 加密
	launcherAuthMaxBodyLen       = launcherAuthTicketHexLen + launcherAuthMaxMachineID
	launcherAuthMaxVerifyBody    = 32 << 10
	launcherAuthLeaseTrailerLen  = 2
	launcherAuthMaxBodyLenLease  = launcherAuthMaxBodyLen + launcherAuthLeaseTrailerLen + launcherIdentityMaxLeaseBytes
)

var launcherAuthMagicBytes = []byte(launcherAuthFrameMagic)

var (
	errLauncherAuthFrameInvalid = errors.New("dnfbridge launcher auth frame invalid")
	errLauncherAuthRejected     = errors.New("dnfbridge launcher auth rejected")
	errLauncherAuthTimeout      = errors.New("dnfbridge launcher auth timeout")
	errLauncherAuthVerifyError  = errors.New("dnfbridge launcher auth verify error")
)

type launcherAuthVerifyRequest struct {
	Ticket      string `json:"ticket"`
	MachineID   string `json:"machine_id,omitempty"`
	RemoteIP    string `json:"remote_ip,omitempty"`
	ChannelID   int    `json:"channel_id"`
	ServerNonce string `json:"server_nonce"`
	OnlineCount int    `json:"online_count,omitempty"`
}

type launcherAuthVerifyResponse struct {
	OK        bool   `json:"ok"`
	AccountID string `json:"account_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func newLauncherAuthVerifyClient(opts options) *http.Client {
	if !opts.launcherAuthEnabled {
		return nil
	}
	timeout := opts.launcherAuthTimeout
	if timeout <= 0 {
		timeout = defaultLauncherAuthTimeout
	}
	return &http.Client{Timeout: timeout}
}

// runLauncherAuthGate performs the full external auth handshake for one game
// connection: challenge frame -> auth frame -> loopback verifier -> bind.
// A nil return means the session now carries a launcher-issued accountID and
// the regular DNF protocol flow may continue unchanged.
func (s *Service) runLauncherAuthGate(ctx context.Context, session *gameSession) error {
	timeout := s.options.launcherAuthTimeout
	if timeout <= 0 {
		timeout = defaultLauncherAuthTimeout
	}
	verifyURL := strings.TrimSpace(s.options.launcherAuthVerifyURL)
	if verifyURL == "" {
		verifyURL = defaultLauncherAuthVerifyURL
	}
	logEvent := func(event string, extra ...any) {
		args := []any{
			"conn_id", session.connID,
			"remote", session.conn.RemoteAddr().String(),
			"local", session.conn.LocalAddr().String(),
			"channel_id", session.channel.ID,
			"channel_type", session.channel.Type,
			"game_port", session.channel.Port,
		}
		args = append(args, extra...)
		s.logPacketEvent(event, args...)
	}
	logEvent("game-launcher-auth-start",
		"verify_url", verifyURL,
		"timeout_ms", timeout.Milliseconds(),
		"lid_mode", s.launcherIdentity.modeName())

	nonce := make([]byte, launcherAuthNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		logEvent("game-launcher-auth-rejected", "reason", "nonce-generation-failed", "error", err)
		s.logWarn("dnfbridge launcher auth nonce generation failed",
			"remote", session.conn.RemoteAddr().String(), "error", err)
		return errLauncherAuthRejected
	}

	// The challenge version is the server's capability statement. Version 2 asks
	// for a launcher identity lease as well as the ticket; version 1 is the
	// original frame that an older 90CN.dll can still answer. Only enforce mode
	// raises the requirement, so audit mode can be observed on live traffic
	// without breaking a client that predates the lease.
	challengeVersion := byte(launcherAuthFrameVersion)
	if s.launcherIdentity.enforcing() {
		challengeVersion = launcherAuthFrameVersionLease
	}
	// S2C 回包加密：challenge body 末尾多一个标记字节，请求客户端在回复里确认能力。
	// 只有 on 模式才带标记（shadow 只记账，不带；off 完全不变）。
	//
	// **顺序很重要**：加解密器必须在发 challenge 之前就建好，建不出来就干脆不发标记——
	// 否则客户端已按 version 3 确认"我会解密"，而服务端还在发明文，结果就是全乱码。
	preparedS2CCipher, wantS2CCipher := s.prepareGameS2CCipher(session, nonce, logEvent)
	serverIP := serverIPBytesForLauncherChallenge(s.options.serverIP)
	challenge := buildLauncherAuthChallenge(
		serverIP[:],
		nonce,
		challengeVersion,
		wantS2CCipher,
	)
	if err := session.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		logEvent("game-launcher-auth-rejected", "reason", "challenge-deadline-failed", "error", err)
		return errLauncherAuthRejected
	}
	if err := writeLauncherAuthFull(session.conn, challenge); err != nil {
		logEvent("game-launcher-auth-rejected", "reason", "challenge-write-failed", "error", err)
		s.logWarn("dnfbridge launcher auth challenge write failed",
			"remote", session.conn.RemoteAddr().String(), "error", err)
		return errLauncherAuthRejected
	}
	_ = session.conn.SetWriteDeadline(time.Time{})

	if err := session.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		logEvent("game-launcher-auth-rejected", "reason", "auth-deadline-failed", "error", err)
		return errLauncherAuthRejected
	}
	frame, readErr := readLauncherAuthFrame(session.conn)
	_ = session.conn.SetReadDeadline(time.Time{})
	if readErr != nil {
		var netErr net.Error
		switch {
		case errors.As(readErr, &netErr) && netErr.Timeout():
			logEvent("game-launcher-auth-timeout", "error", readErr)
			s.logWarn("dnfbridge launcher auth frame timed out",
				"remote", session.conn.RemoteAddr().String(), "error", readErr)
			return errLauncherAuthTimeout
		case errors.Is(readErr, errLauncherAuthFrameInvalid):
			logEvent("game-launcher-auth-frame-invalid", "error", readErr)
			s.logWarn("dnfbridge launcher auth frame invalid",
				"remote", session.conn.RemoteAddr().String(), "error", readErr)
			return errLauncherAuthFrameInvalid
		default:
			logEvent("game-launcher-auth-rejected", "reason", "auth-frame-read-failed", "error", readErr)
			return errLauncherAuthRejected
		}
	}
	ticket := frame.Ticket
	machineID := frame.MachineID

	// S2C 回包加密的能力协商结果。客户端回 version 3 = "我读到了标记、我会解密"，
	// 于是这条连接的回包 body 用 LANA challenge 里那 16 字节随机数派生密钥加密。
	// 没回 v3（旧 DLL、或本地直连没走这个门）就保持明文——**绝不猜**。
	s.negotiateGameS2CCipher(session, frame, preparedS2CCipher, logEvent)

	// Launcher identity check. Enforce mode needs a valid lease before the ticket
	// is even looked at, so an attacker who steals a ticket still cannot open a
	// game connection; audit mode records the verdict and carries on.
	leaseAccountID, leaseErr := s.applyLauncherIdentityLease(frame, logEvent)
	if s.launcherIdentity.enforcing() && leaseErr != nil {
		// frame_version is the single most useful field here: 1 means the client
		// answered with the original frame because its 90CN.dll predates the
		// lease, which is a deployment that was not finished rather than an
		// attacker.
		logEvent("game-launcher-auth-rejected",
			"reason", "launcher-identity", "frame_version", frame.Version, "error", leaseErr)
		s.logWarn("dnfbridge launcher identity rejected a game connection",
			"remote", session.conn.RemoteAddr().String(),
			"frame_version", frame.Version, "error", leaseErr)
		return errLauncherAuthRejected
	}
	if s.launcherIdentity.enabled() && leaseErr == nil && leaseAccountID != "" {
		// A ticketless lease names its account directly. That is the shape a
		// deployment without an ops-tool ticket endpoint uses, and the account
		// came out of a signed statement, so it needs no loopback verify.
		session.accountID = leaseAccountID
		session.machineID = machineID
		if ip, err := netip.ParseAddr(remoteHostOf(session)); err == nil {
			session.accountSourceIP = ip.Unmap()
		}
		// 同上：票据摘要即外网的客户端实例身份。
		session.loginIdentity = currentLoginIdentityDigest(ticket)
		logEvent("game-launcher-auth-ok", "account_id", session.accountID, "source", "launcher-identity-lease",
			"ticket_digest", session.loginIdentity,
			"machine_id_digest", currentLoginIdentityDigest(machineID))
		return nil
	}
	if leaseErr != nil && s.launcherIdentity.enabled() {
		logEvent("game-launcher-auth-lid-audit", "error", leaseErr)
	}

	remoteHost := remoteHostOf(session)
	requestBody, err := json.Marshal(launcherAuthVerifyRequest{
		Ticket:      ticket,
		MachineID:   machineID,
		RemoteIP:    remoteHost,
		ChannelID:   session.channel.ID,
		ServerNonce: hex.EncodeToString(nonce),
		OnlineCount: s.countOnlineAccounts(),
	})
	if err != nil {
		logEvent("game-launcher-auth-verify-error", "reason", "request-encode-failed", "error", err)
		return errLauncherAuthVerifyError
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, verifyURL, bytes.NewReader(requestBody))
	if err != nil {
		logEvent("game-launcher-auth-verify-error", "reason", "request-build-failed", "error", err)
		return errLauncherAuthVerifyError
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(s.options.launcherAuthInternalToken); token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	client := s.launcherAuthHTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		logEvent("game-launcher-auth-verify-error", "reason", "verify-request-failed", "error", err)
		s.logWarn("dnfbridge launcher auth verify request failed",
			"remote", session.conn.RemoteAddr().String(), "error", err)
		return errLauncherAuthVerifyError
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logEvent("game-launcher-auth-verify-error", "reason", "verify-http-status", "status", resp.StatusCode)
		return errLauncherAuthVerifyError
	}
	var verification launcherAuthVerifyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, launcherAuthMaxVerifyBody)).Decode(&verification); err != nil {
		logEvent("game-launcher-auth-verify-error", "reason", "verify-decode-failed", "error", err)
		return errLauncherAuthVerifyError
	}
	if !verification.OK || strings.TrimSpace(verification.AccountID) == "" {
		logEvent("game-launcher-auth-rejected", "reason", verification.Reason)
		s.logWarn("dnfbridge launcher auth rejected by verifier",
			"remote", session.conn.RemoteAddr().String(),
			"reason", verification.Reason)
		return errLauncherAuthRejected
	}
	session.accountID = strings.TrimSpace(verification.AccountID)
	session.machineID = machineID
	if ip, err := netip.ParseAddr(remoteHost); err == nil {
		session.accountSourceIP = ip.Unmap()
	}
	// 票据摘要就是外网的"客户端实例"身份。
	//
	// 依据运维工具源码 internal/store/store.go:716 Login —— 每次
	// POST /launcher/v1/login 才 INSERT 一行 launcher_sessions 并签发一张 32 字节随机票;
	// :777 VerifyTicket 只按 ticket_hash 查表校验有效期，**不消费、不删除**；
	// gm_events.go:98 的心跳只顺延 expires_at，不改 ticket_hash。
	//
	// 因此：同一个登录器登录期间，该客户端的上百条连接（启动时的全频道探测、换频道、
	// 返回选角、攻坚探测）共用同一张票 → 摘要相同 → 不会被顶号误踢；
	// 两次登录器登录（双开登 A 与登 B）是两张不同的票 → 摘要不同 → 顶号成立。
	//
	// 绝不用 IP 或机器码：两者在"同一台机器双开两个账号"时都相同，分不开。
	session.loginIdentity = currentLoginIdentityDigest(ticket)
	logEvent("game-launcher-auth-ok", "account_id", session.accountID,
		"ticket_digest", session.loginIdentity,
		"machine_id_digest", currentLoginIdentityDigest(machineID))
	return nil
}

// serverIPBytesForLauncherChallenge returns the information-only public IPv4
// carried inside the challenge body. The launcher verifier does not rely on
// it for ticket decisions (the real peer address is supplied separately), so
// non-IPv4 server addresses degrade to 0.0.0.0 instead of failing the gate.
func serverIPBytesForLauncherChallenge(serverIP string) [4]byte {
	var zero [4]byte
	ip := net.ParseIP(strings.TrimSpace(serverIP))
	if ip == nil {
		return zero
	}
	if v4 := ip.To4(); v4 != nil {
		return [4]byte{v4[0], v4[1], v4[2], v4[3]}
	}
	return zero
}

// buildLauncherAuthChallenge 组装服务端 challenge 帧。
//
// 布局：`"LANA" | version(1) | bodyLen(2B BE) | server_ip(4B) | nonce(16B) [| s2cFlag(1B)]`
//
// withS2CFlag 只在 S2C 回包加密为 on 时置位。多出来的那 1 个字节对**旧客户端是透明的**：
// 它只校验 body 长度落在 20..1024 且完全不读 body 内容，因此旧 DLL 照样握手成功、
// 回答 version 2，于是服务端按明文处理——不会把没准备好的客户端推进密文。
func buildLauncherAuthChallenge(serverIP []byte, nonce []byte, version byte, withS2CFlag bool) []byte {
	bodyLen := launcherAuthChallengeBody
	if withS2CFlag {
		bodyLen += launcherAuthChallengeS2CFlag
	}
	challenge := make([]byte, 0, launcherAuthHeaderLen+bodyLen)
	challenge = append(challenge, launcherAuthMagicBytes...)
	challenge = append(challenge, version)
	challenge = binary.BigEndian.AppendUint16(challenge, uint16(bodyLen))
	challenge = append(challenge, serverIP...)
	challenge = append(challenge, nonce...)
	if withS2CFlag {
		challenge = append(challenge, 1)
	}
	return challenge
}

// launcherAuthFrame is one decoded client auth frame. Lease is empty for the
// original version 1 frame and for a version 2 frame whose client had no
// launcher identity lease to attach.
type launcherAuthFrame struct {
	Version     byte
	Ticket      string
	MachineID   string
	Lease       string
	LeaseParsed launcherIdentityLease
}

// readLauncherAuthFrame decodes either frame version. The ticket and machine_id
// prefix is byte-identical across versions, so only the version 2 trailer needs
// separate handling. An unknown version is rejected rather than guessed at,
// because guessing would let a future frame be read as its predecessor.
func readLauncherAuthFrame(conn net.Conn) (launcherAuthFrame, error) {
	var frame launcherAuthFrame
	header := make([]byte, launcherAuthHeaderLen)
	if err := readLauncherAuthFull(conn, header); err != nil {
		return frame, err
	}
	if !bytes.Equal(header[:4], launcherAuthMagicBytes) {
		return frame, errLauncherAuthFrameInvalid
	}
	frame.Version = header[4]
	switch frame.Version {
	case launcherAuthFrameVersion, launcherAuthFrameVersionLease, launcherAuthFrameVersionS2C:
	default:
		return frame, errLauncherAuthFrameInvalid
	}
	bodyLen := int(binary.BigEndian.Uint16(header[5:7]))
	maxBodyLen := launcherAuthMaxBodyLen
	if frame.Version != launcherAuthFrameVersion {
		// version 2 与 version 3 的 body 布局完全一致：version 3 只是"客户端读到了
		// S2C 标记"的确认位，不改变 ticket/machine_id/lease 的解析。
		maxBodyLen = launcherAuthMaxBodyLenLease
	}
	if bodyLen < launcherAuthTicketHexLen || bodyLen > maxBodyLen {
		return frame, errLauncherAuthFrameInvalid
	}
	body := make([]byte, bodyLen)
	if err := readLauncherAuthFull(conn, body); err != nil {
		return frame, err
	}
	ticket := body[:launcherAuthTicketHexLen]
	if _, err := hex.DecodeString(string(ticket)); err != nil {
		return frame, errLauncherAuthFrameInvalid
	}
	trailer := body[launcherAuthTicketHexLen:]
	machineEnd := len(trailer)
	if frame.Version != launcherAuthFrameVersion {
		if len(trailer) < launcherAuthLeaseTrailerLen {
			return frame, errLauncherAuthFrameInvalid
		}
		// The lease length sits at the very end of the body, so machine_id is
		// whatever precedes the lease. That is what makes the variable-length
		// machine_id field unambiguous to parse.
		lengthAt := len(trailer) - launcherAuthLeaseTrailerLen
		leaseLen := int(binary.BigEndian.Uint16(trailer[lengthAt:]))
		if leaseLen > lengthAt {
			return frame, errLauncherAuthFrameInvalid
		}
		if leaseLen > 0 {
			frame.Lease = string(trailer[lengthAt-leaseLen : lengthAt])
		}
		// A zero-length lease is legal: a version 1 capable client that was
		// challenged by a version 2 server still reports its build, and enforce
		// mode then rejects it with a precise reason instead of a frame error.
		machineEnd = lengthAt - leaseLen
	}
	machine := trailer[:machineEnd]
	for _, b := range machine {
		if b > 0x7f {
			return frame, errLauncherAuthFrameInvalid
		}
	}
	frame.Ticket = string(ticket)
	frame.MachineID = string(machine)
	if frame.Lease != "" {
		parsed, err := parseLauncherIdentityLease(frame.Lease)
		if err != nil {
			return frame, errLauncherAuthFrameInvalid
		}
		frame.LeaseParsed = parsed
	}
	return frame, nil
}

// remoteHostOf returns the peer address without its port, which is the form the
// loopback verifier and the ban filters expect.
func remoteHostOf(session *gameSession) string {
	if session == nil || session.conn == nil || session.conn.RemoteAddr() == nil {
		return ""
	}
	remoteHost := session.conn.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(remoteHost); err == nil {
		return host
	}
	return remoteHost
}

// applyLauncherIdentityLease verifies the lease attached to an in-band LANA
// frame. It returns the account the lease names directly when the lease carries
// one (the ticketless shape) and an empty string when the lease is bound to a
// ticket that still has to be verified. The returned error is the verification
// verdict and is never fatal on its own: the caller decides, because audit mode
// records the same verdict without rejecting.
func (s *Service) applyLauncherIdentityLease(
	frame launcherAuthFrame,
	logEvent func(string, ...any),
) (string, error) {
	if !s.launcherIdentity.enabled() {
		return "", errLauncherIdentityDisabled
	}
	if frame.Lease == "" {
		return "", errLauncherIdentityMalformed
	}
	lease := frame.LeaseParsed
	if err := s.launcherIdentity.verifyRemoteLease(frame.Ticket, lease, time.Now()); err != nil {
		return "", err
	}
	leaseAccountID, _ := lease.leaseAccountID()
	if leaseAccountID == "" {
		logEvent("game-launcher-auth-lid-verified", "subject", "ticket")
		return "", nil
	}
	logEvent("game-launcher-auth-lid-verified", "subject", "account")
	return leaseAccountID, nil
}

func readLauncherAuthFull(conn net.Conn, buffer []byte) error {
	for written := 0; written < len(buffer); {
		n, err := conn.Read(buffer[written:])
		written += n
		if err != nil {
			if errors.Is(err, io.EOF) && written == len(buffer) {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func writeLauncherAuthFull(conn net.Conn, payload []byte) error {
	for written := 0; written < len(payload); {
		n, err := conn.Write(payload[written:])
		written += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
