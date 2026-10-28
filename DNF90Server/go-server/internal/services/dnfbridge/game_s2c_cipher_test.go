package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	testS2CNonceHex    = "00112233445566778899aabbccddeeff"
	testS2CMasterKey   = "ToXJkeLbeccbEN0a2fK2DjRhct6nx8KTYisGp49Levs="
	testS2CBodyHex     = "0b000000ff00cafe0102030405060708090a"
	testS2CCipherHex   = "e32cd1aa9c3ac15f46145c774a701eb6bfe0"
	testS2CMsgID       = uint16(38)
	testS2CSeq         = uint16(4242)
	testS2CTicketHex   = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	testS2CMachineID   = "MACHINE-TEST"
	testS2CLeaseLength = 0
)

// challenge 的 S2C 标记必须是**纯 opt-in**：off/shadow 一个字节都不许多，
// 否则旧 DLL（完全不读 body 内容）之外的一切假设都要重算。
func TestBuildLauncherAuthChallengeS2CFlagIsOptIn(t *testing.T) {
	serverIP := []byte{10, 0, 0, 1}
	nonce := make([]byte, launcherAuthNonceLen)

	plain := buildLauncherAuthChallenge(serverIP, nonce, launcherAuthFrameVersion, false)
	if got := len(plain) - launcherAuthHeaderLen; got != launcherAuthChallengeBody {
		t.Fatalf("body len = %d, want %d", got, launcherAuthChallengeBody)
	}
	if version := plain[4]; version != launcherAuthFrameVersion {
		t.Fatalf("version = %d", version)
	}
	if bodyLen := int(binary.BigEndian.Uint16(plain[5:7])); bodyLen != launcherAuthChallengeBody {
		t.Fatalf("declared body len = %d", bodyLen)
	}

	flagged := buildLauncherAuthChallenge(serverIP, nonce, launcherAuthFrameVersionLease, true)
	if got := len(flagged) - launcherAuthHeaderLen; got != launcherAuthChallengeBody+launcherAuthChallengeS2CFlag {
		t.Fatalf("flagged body len = %d, want %d", got, launcherAuthChallengeBody+launcherAuthChallengeS2CFlag)
	}
	if bodyLen := int(binary.BigEndian.Uint16(flagged[5:7])); bodyLen != len(flagged)-launcherAuthHeaderLen {
		t.Fatalf("declared body len = %d, actual %d", bodyLen, len(flagged)-launcherAuthHeaderLen)
	}
	if last := flagged[len(flagged)-1]; last != 1 {
		t.Fatalf("flag byte = %d, want 1", last)
	}
	// 旧客户端要求 body 长度落在 20..1024（它不读内容），21 必须仍在范围内。
	if bodyLen := len(flagged) - launcherAuthHeaderLen; bodyLen < 20 || bodyLen > 1024 {
		t.Fatalf("old clients reject body len %d", bodyLen)
	}
}

// version 3 的 body 布局必须与 version 2 **完全一致**：它只是"客户端读到了 S2C 标记"的确认位。
func TestReadLauncherAuthFrameAcceptsVersion3(t *testing.T) {
	for _, version := range []byte{launcherAuthFrameVersionLease, launcherAuthFrameVersionS2C} {
		frame := buildClientAuthFrame(t, version, "")
		server, client := net.Pipe()
		defer func() { _ = server.Close(); _ = client.Close() }()
		go func() {
			_, _ = client.Write(frame)
		}()
		parsed, err := readLauncherAuthFrame(server)
		if err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
		if parsed.Version != version {
			t.Fatalf("version = %d, want %d", parsed.Version, version)
		}
		if parsed.Ticket != testS2CTicketHex {
			t.Fatalf("version %d ticket = %q", version, parsed.Ticket)
		}
		if parsed.MachineID != testS2CMachineID {
			t.Fatalf("version %d machine = %q", version, parsed.MachineID)
		}
	}
	// 未知版本仍然必须拒绝（不许猜）。
	frame := buildClientAuthFrame(t, 9, "")
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	go func() {
		_, _ = client.Write(frame)
	}()
	if _, err := readLauncherAuthFrame(server); err == nil {
		t.Fatal("an unknown frame version must be rejected")
	}
}

// 协商矩阵：只有"服务端请求（且加解密器已备好）+ 客户端回 v3"才允许加密，其余一律明文。
func TestNegotiateGameS2CCipherMatrix(t *testing.T) {
	nonce, err := hex.DecodeString(testS2CNonceHex)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	masterKey, err := dnfproto.ParseS2CMasterKey([]byte(testS2CMasterKey))
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	cases := []struct {
		name        string
		mode        string
		requested   bool
		frameVer    byte
		wantCipher  bool
		wantEvent   string
		description string
	}{
		{"off leaves everything alone", gameS2CCipherOff, true, launcherAuthFrameVersionS2C, false, "", "off 不该产生任何事件"},
		{"shadow only observes", gameS2CCipherShadow, true, launcherAuthFrameVersionS2C, false, "game-s2c-cipher-shadow", "shadow 只记账"},
		{"on with v3 client enables", gameS2CCipherOn, true, launcherAuthFrameVersionS2C, true, "game-s2c-cipher-enabled", ""},
		{"on with legacy client stays plaintext", gameS2CCipherOn, true, launcherAuthFrameVersionLease, false, "game-s2c-cipher-skipped", "旧 DLL 不加密"},
	}
	for _, testCase := range cases {
		service := &Service{options: options{gameS2CCipher: testCase.mode}}
		session := &gameSession{connID: "game-s2c-test"}
		var prepared *dnfproto.S2CCipher
		if testCase.requested {
			built, buildErr := dnfproto.NewS2CCipher(masterKey, nonce)
			if buildErr != nil {
				t.Fatalf("new cipher: %v", buildErr)
			}
			prepared = built
		}
		var events []string
		service.negotiateGameS2CCipher(session, launcherAuthFrame{Version: testCase.frameVer}, prepared,
			func(event string, _ ...any) { events = append(events, event) })
		if got := session.s2cCipher != nil; got != testCase.wantCipher {
			t.Fatalf("%s: cipher=%t want %t (%s)", testCase.name, got, testCase.wantCipher, testCase.description)
		}
		if testCase.wantEvent == "" {
			if len(events) != 0 {
				t.Fatalf("%s: unexpected events %v", testCase.name, events)
			}
			continue
		}
		if len(events) != 1 || events[0] != testCase.wantEvent {
			t.Fatalf("%s: events = %v, want [%s]", testCase.name, events, testCase.wantEvent)
		}
	}
}

// 关键顺序：**加解密器必须在发 challenge 之前建好**。
// 主密钥坏了的时候绝不能请求加密——否则客户端已按 v3 确认会解密，而服务端还在发明文，
// 结果是全乱码（= 客户端卡住）。这条测试就钉住这个顺序。
func TestPrepareGameS2CCipherRefusesBeforeChallenge(t *testing.T) {
	nonce := make([]byte, launcherAuthNonceLen)
	broken := &Service{options: options{
		gameS2CCipher:          gameS2CCipherOn,
		gameS2CMasterKeyBase64: "not-a-valid-key",
	}}
	var events []string
	cipher, want := broken.prepareGameS2CCipher(&gameSession{}, nonce,
		func(event string, _ ...any) { events = append(events, event) })
	if cipher != nil || want {
		t.Fatal("an invalid master key must not request encryption")
	}
	if len(events) != 1 || events[0] != "game-s2c-cipher-skipped" {
		t.Fatalf("events = %v, want [game-s2c-cipher-skipped]", events)
	}

	// 正常主密钥：备好加解密器，且此时还没有任何连接级状态被写进会话。
	healthy := &Service{options: options{gameS2CCipher: gameS2CCipherOn}}
	session := &gameSession{}
	prepared, want := healthy.prepareGameS2CCipher(session, nonce, func(string, ...any) {})
	if prepared == nil || !want {
		t.Fatal("a valid master key must prepare a cipher")
	}
	if session.s2cCipher != nil {
		t.Fatal("preparing must not attach the cipher before the client acknowledges")
	}
}

// 端到端（真正走发送路径 + 加密包日志）：body 变密文、长度不变、
// checksum 覆盖**密文**、codec 标签变成 s2c-ctr-v1，且解回来与原文一致。
func TestGameS2CEncryptsBodyAndKeepsHeaderContract(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	service := &Service{}
	service.packetLog.Store(logger)

	session := &gameSession{conn: &bufferConn{}, connID: "game-000900", stall: newDungeonStallTracker()}
	body, err := hex.DecodeString(testS2CBodyHex)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	nonce, err := hex.DecodeString(testS2CNonceHex)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	masterKey, err := dnfproto.ParseS2CMasterKey([]byte(testS2CMasterKey))
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	cipher, err := dnfproto.NewS2CCipher(masterKey, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	session.s2cCipher = cipher
	session.upperSeq = testS2CSeq

	// 发送路径：会话没有 conn，写会失败——但包在写之前就构建并记进日志了。
	_ = service.sendGameUpperRawClassNoCodec(session, testS2CMsgID, append([]byte(nil), body...), 0)
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}

	text := packetLogPlaintext(t, anchor)
	packet := extractSentPacket(t, text, testS2CMsgID)
	if len(packet) != dnfproto.ChannelHeaderSize+len(body) {
		t.Fatalf("packet len = %d, want %d（加密必须等长）", len(packet), dnfproto.ChannelHeaderSize+len(body))
	}
	if got := binary.LittleEndian.Uint32(packet[3:7]); int(got) != len(packet) {
		t.Fatalf("declared length = %d, actual %d", got, len(packet))
	}
	if got := binary.LittleEndian.Uint16(packet[11:13]); got != testS2CSeq {
		t.Fatalf("header seq = %d, want %d", got, testS2CSeq)
	}
	sealed := packet[dnfproto.ChannelHeaderSize:]
	if hex.EncodeToString(sealed) != testS2CCipherHex {
		t.Fatalf("wire body = %s, want %s（与客户端自检向量必须一致）", hex.EncodeToString(sealed), testS2CCipherHex)
	}
	// checksum 必须覆盖**线上字节**（seq + 密文 body），否则客户端整包拒绝。
	sum, err := dnfproto.ChecksumRange(packet, 11, len(packet)-11)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if got := binary.LittleEndian.Uint32(packet[7:11]); got != sum {
		t.Fatalf("checksum = %08x, want %08x", got, sum)
	}
	plain := append([]byte(nil), sealed...)
	cipher.Apply(plain, testS2CMsgID, testS2CSeq)
	if !bytes.Equal(plain, body) {
		t.Fatalf("decrypted body = %s, want %s", hex.EncodeToString(plain), testS2CBodyHex)
	}
	if !strings.Contains(text, "body_codec="+dnfproto.S2CCipherCodecName) {
		t.Fatalf("send meta did not carry the s2c codec label:\n%s", text)
	}
	// 反向护栏：同一条日志里不许出现明文 body。
	if strings.Contains(text, "hex="+hex.EncodeToString(append(make([]byte, dnfproto.ChannelHeaderSize), body...))[:32]) {
		t.Fatal("the plaintext body leaked into the packet log")
	}
}

// 没协商成功的连接必须一字不改地走明文（这是"旧客户端不炸"的地基）。
func TestGameS2CLeavesPacketsPlaintextWithoutCipher(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	service := &Service{}
	service.packetLog.Store(logger)
	session := &gameSession{conn: &bufferConn{}, connID: "game-000901", stall: newDungeonStallTracker(), upperSeq: testS2CSeq}
	body, err := hex.DecodeString(testS2CBodyHex)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_ = service.sendGameUpperRawClassNoCodec(session, testS2CMsgID, append([]byte(nil), body...), 0)
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}
	text := packetLogPlaintext(t, anchor)
	packet := extractSentPacket(t, text, testS2CMsgID)
	if got := hex.EncodeToString(packet[dnfproto.ChannelHeaderSize:]); got != testS2CBodyHex {
		t.Fatalf("body = %s, want plaintext %s", got, testS2CBodyHex)
	}
	if strings.Contains(text, dnfproto.S2CCipherCodecName) {
		t.Fatal("a plaintext connection must not be labelled with the s2c codec")
	}
}

// buildClientAuthFrame 组装一个客户端 LANA 回复帧（version 2/3 布局相同）。
func buildClientAuthFrame(t *testing.T, version byte, lease string) []byte {
	t.Helper()
	frame := []byte(launcherAuthFrameMagic)
	frame = append(frame, version)
	body := []byte(testS2CTicketHex)
	body = append(body, []byte(testS2CMachineID)...)
	body = append(body, []byte(lease)...)
	body = append(body, byte(len(lease)>>8), byte(len(lease)))
	frame = append(frame, byte(len(body)>>8), byte(len(body)))
	return append(frame, body...)
}

// extractSentPacket 从（已解密的）包日志里取出某个 msg_id 的 SEND 包的原始字节。
func extractSentPacket(t *testing.T, text string, msgID uint16) []byte {
	t.Helper()
	pattern := regexp.MustCompile(`SEND kind=game-upper[^\n]*msg_id=` + itoaUint(msgID) + `[^\n]*hex=([0-9a-f]+)`)
	match := pattern.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("no SEND packet for msg_id=%d in:\n%s", msgID, text)
	}
	packet, err := hex.DecodeString(match[1])
	if err != nil {
		t.Fatalf("decode packet hex: %v", err)
	}
	return packet
}

func itoaUint(value uint16) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// 保证包日志里 msg_id 与 opcode 常量的绑定不漂移（dnfenum 改了要在这里发现）。
func TestGameS2CTestFixturesMatchEnums(t *testing.T) {
	if uint16(dnfenum.CmdPacketNotifyDieMonster) != testS2CMsgID {
		t.Fatalf("fixture msg id %d no longer matches dnfenum (%d)",
			testS2CMsgID, uint16(dnfenum.CmdPacketNotifyDieMonster))
	}
	if time.Duration(0) != 0 {
		t.Fatal("unreachable")
	}
}
