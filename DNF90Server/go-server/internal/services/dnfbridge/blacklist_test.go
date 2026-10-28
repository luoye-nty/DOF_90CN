package dnfbridge

import (
	"bytes"
	"context"
	"testing"
	"time"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// The blacklist answer (class-1 opcode 120) is
//
//	u8 flag | u8 count | count * ( u32 nameLength | name | u8 year | u8 month | u8 day )
//
// with the flag byte consumed as the reader's second argument before the payload.
// These tests pin that layout, the store round-trip behind it, and the two rejection
// statuses of the 1:1 gate that reads the same store.

func TestCurrentBlacklistValueRoundTripsNamesAndDates(t *testing.T) {
	records := []currentBlacklistRecord{
		{Name: "alpha", At: 1758499200},
		{Name: "beta"},
		{Name: "gamma", At: 1758585600},
	}
	encoded := formatCurrentBlacklistValue(records)
	if encoded != "alpha\t1758499200\nbeta\ngamma\t1758585600" {
		t.Fatalf("encoded value = %q", encoded)
	}
	decoded := parseCurrentBlacklistValue(encoded)
	if len(decoded) != len(records) {
		t.Fatalf("decoded = %+v", decoded)
	}
	for index, want := range records {
		if decoded[index] != want {
			t.Fatalf("decoded[%d] = %+v want %+v", index, decoded[index], want)
		}
	}
}

// Saves written before the block date was tracked hold bare names.
func TestCurrentBlacklistValueAcceptsDateLessLegacyLines(t *testing.T) {
	decoded := parseCurrentBlacklistValue("alpha\n\n  beta  \n")
	if len(decoded) != 2 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if decoded[0].Name != "alpha" || decoded[0].At != 0 {
		t.Fatalf("decoded[0] = %+v", decoded[0])
	}
	if decoded[1].Name != "beta" || decoded[1].At != 0 {
		t.Fatalf("decoded[1] = %+v", decoded[1])
	}
}

func TestCurrentBlacklistDateFieldsProjectYearMonthDay(t *testing.T) {
	at := time.Date(2026, time.September, 22, 13, 4, 5, 0, time.Local).Unix()
	year, month, day := currentBlacklistDateFields(at)
	if year != 26 || month != 9 || day != 22 {
		t.Fatalf("date fields = %d/%d/%d want 26/9/22", year, month, day)
	}
	if year, month, day := currentBlacklistDateFields(0); year != 0 || month != 0 || day != 0 {
		t.Fatalf("unknown date must project to zeros, got %d/%d/%d", year, month, day)
	}
}

func TestSaveAndLoadCurrentBlacklistRecordsRoundTrip(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	ctx := context.Background()
	want := []currentBlacklistRecord{
		{Name: "alpha", At: 1758499200},
		{Name: "beta", At: 1758585600},
	}
	if err := service.saveCurrentBlacklistRecords(ctx, 12, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := service.loadCurrentBlacklistRecords(ctx, 12)
	sorted := currentBlacklistRecordsSorted(want)
	if len(got) != len(sorted) {
		t.Fatalf("loaded = %+v want %+v", got, sorted)
	}
	for index := range sorted {
		if got[index] != sorted[index] {
			t.Fatalf("loaded[%d] = %+v want %+v", index, got[index], sorted[index])
		}
	}
	if names := service.loadCurrentBlacklist(ctx, 12); len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("names = %v", names)
	}
	if other := service.loadCurrentBlacklistRecords(ctx, 13); len(other) != 0 {
		t.Fatalf("character 13 must have its own list, got %+v", other)
	}
}

// The empty answer must stay byte for byte what this path sent before the layout
// was known: flag 0 makes the client's reader return without touching its list.
func TestBuildCurrentBlacklistListBodyEmptyKeepsLegacyBytes(t *testing.T) {
	if body := buildCurrentBlacklistListBody(nil); !bytes.Equal(body, currentRequestBlacklistResponseBody) {
		t.Fatalf("empty body = %x want legacy %x", body, currentRequestBlacklistResponseBody)
	}
	if body := buildCurrentBlacklistListBody([]currentBlacklistRecord{}); !bytes.Equal(body, []byte{0, 0}) {
		t.Fatalf("empty body = %x want 0000", body)
	}
	if len(currentRequestBlacklistResponseBody) != 2 {
		t.Fatalf("legacy empty body len = %d want 2", len(currentRequestBlacklistResponseBody))
	}
}

func TestBuildCurrentBlacklistListBodyLaysOutFlagCountAndEntries(t *testing.T) {
	at := time.Date(2026, time.September, 22, 13, 4, 5, 0, time.Local).Unix()
	body := buildCurrentBlacklistListBody([]currentBlacklistRecord{{Name: "test", At: at}})
	name := rosterNameBytes("test")

	want := []byte{1, 1}
	want = append(want, byte(len(name)), 0, 0, 0)
	want = append(want, name...)
	want = append(want, 26, 9, 22)
	if !bytes.Equal(body, want) {
		t.Fatalf("body = %x want %x", body, want)
	}

	// Two entries repeat the same record shape back to back, with no padding.
	second := buildCurrentBlacklistListBody([]currentBlacklistRecord{
		{Name: "test", At: at},
		{Name: "beta"},
	})
	beta := rosterNameBytes("beta")
	wantTwo := append([]byte{}, want...)
	wantTwo[1] = 2
	wantTwo = append(wantTwo, byte(len(beta)), 0, 0, 0)
	wantTwo = append(wantTwo, beta...)
	wantTwo = append(wantTwo, 0, 0, 0)
	if !bytes.Equal(second, wantTwo) {
		t.Fatalf("two-entry body = %x want %x", second, wantTwo)
	}
	// An entry with no stored date still carries its three bytes.
	if len(second) != len(want)+4+len(beta)+3 {
		t.Fatalf("two-entry body len = %d", len(second))
	}
}

// The register ack is `flag(1) | u32 nameLength | name` -- 9 bytes for a
// four-character name. It used to write a second, redundant length field, which the
// client consumed as the name itself and rendered as a garbled row.
func TestHandleCurrentBlacklistRegisterAckWritesFlagThenSingleNameLength(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	request := append([]byte{4, 0, 0, 0}, []byte("test")...)
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRegisiterToBlacklist), request); err != nil {
		t.Fatalf("register: %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.Classification != 1 || ack.Header.MsgID != uint16(dnfenum.CmdPacketRegisiterToBlacklist) {
		t.Fatalf("ack header = %+v", ack.Header)
	}
	want := append([]byte{1, 4, 0, 0, 0}, []byte("test")...)
	if !bytes.Equal(ack.Body, want) {
		t.Fatalf("ack body = %x want %x", ack.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("register trailing bytes = %x", rest)
	}
	if records := service.loadCurrentBlacklistRecords(context.Background(), source.selectedCharacterID); len(records) != 1 || records[0].Name != "test" {
		t.Fatalf("stored records = %+v", records)
	}
}

// The headline fix: a blocked name survives a relog because the answer the new
// session gets carries the stored list instead of the fixed empty body.
func TestRequestBlacklistAfterRelogReturnsTheStoredList(t *testing.T) {
	service, source, _, _, _ := newCurrentSocialPeerTest(t)
	register := append([]byte{4, 0, 0, 0}, []byte("test")...)
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRegisiterToBlacklist), register); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A fresh session for the same character is what a relog looks like. The scene
	// tail has committed by the time the client asks again, which is the moment the
	// answer is allowed out (see sendRequestBlacklistState).
	relogConn := &bufferConn{}
	relog := &gameSession{conn: relogConn, channel: source.channel, accountID: "dnf:1", selectedCharacterID: 1,
		sceneBootstrapTailSent: true}
	if err := service.handleGameCommand(relog, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRequestBlacklist), make([]byte, 16)); err != nil {
		t.Fatalf("request blacklist: %v", err)
	}
	packet, _ := splitGameServerUpperPacket(t, relogConn.write.Bytes())
	if packet.Header.Classification != 1 || packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestBlacklist) {
		t.Fatalf("answer header = %+v", packet.Header)
	}
	want := buildCurrentBlacklistListBody(service.loadCurrentBlacklistRecords(context.Background(), 1))
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("answer body = %x want %x", packet.Body, want)
	}
	if len(packet.Body) < 3 || packet.Body[0] != 1 || packet.Body[1] != 1 {
		t.Fatalf("answer body = %x want flag 1 and count 1", packet.Body)
	}
	name := rosterNameBytes("test")
	nameAt := 2
	if got := packet.Body[nameAt : nameAt+4]; !bytes.Equal(got, []byte{byte(len(name)), 0, 0, 0}) {
		t.Fatalf("name length field = %x want %x", got, []byte{byte(len(name)), 0, 0, 0})
	}
	if got := packet.Body[nameAt+4 : nameAt+4+len(name)]; !bytes.Equal(got, name) {
		t.Fatalf("name field = %x want %x", got, name)
	}
	year, month, day := currentBlacklistDateFields(service.loadCurrentBlacklistRecords(context.Background(), 1)[0].At)
	tail := packet.Body[nameAt+4+len(name):]
	if !bytes.Equal(tail, []byte{year, month, day}) {
		t.Fatalf("date tail = %x want %x", tail, []byte{year, month, day})
	}
}

func TestRequestBlacklistWithoutBlocksKeepsLegacyBytes(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	source.sceneBootstrapTailSent = true
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRequestBlacklist), make([]byte, 16)); err != nil {
		t.Fatalf("request blacklist: %v", err)
	}
	packet, _ := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if !bytes.Equal(packet.Body, currentRequestBlacklistResponseBody) {
		t.Fatalf("answer body = %x want %x", packet.Body, currentRequestBlacklistResponseBody)
	}
}

// A request that arrives while the select-character scene tail is still pending must
// not be answered: a row the client builds in that window loses its 黑名单日期 (live
// 2026-09-22 23:46, see sendRequestBlacklistState). The answer is flushed when the
// tail commits instead.
func TestRequestBlacklistIsHeldUntilTheSceneTailCommits(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	ctx := context.Background()
	if err := service.saveCurrentBlacklistRecords(ctx, source.selectedCharacterID,
		[]currentBlacklistRecord{{Name: "test", At: 1790091427}}); err != nil {
		t.Fatalf("store blacklist: %v", err)
	}
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestBlacklist), make([]byte, 16)); err != nil {
		t.Fatalf("request blacklist: %v", err)
	}
	if sent := sourceConn.write.Bytes(); len(sent) != 0 {
		t.Fatalf("the answer must be held while the tail is pending, got %x", sent)
	}
	if !source.blacklistListAnswerPending {
		t.Fatal("the held answer was not recorded")
	}

	// The tail commit flushes it with the stored list.
	source.sceneBootstrapTailSent = true
	if err := service.sendCurrentBlacklistListAnswer(source, "test_after_scene_tail", nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	want := buildCurrentBlacklistListBody(service.loadCurrentBlacklistRecords(ctx, source.selectedCharacterID))
	if packet.Header.Classification != 1 || packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestBlacklist) ||
		!bytes.Equal(packet.Body, want) || len(rest) != 0 {
		t.Fatalf("flushed answer header=%+v body=%x want=%x rest=%x", packet.Header, packet.Body, want, rest)
	}
	if source.blacklistListAnswerPending {
		t.Fatal("the pending flag must clear once the answer is sent")
	}
}

// Blocking is one-way: A blocking B writes A's list only, and B's scope must not even
// be created. (Operator note 2026-09-22: "拉黑是单向操作 A拉黑了B B的黑名单列表内不应该出现A".)
func TestBlacklistIsOneWay(t *testing.T) {
	service, source, target, _, _ := newCurrentSocialPeerTest(t)
	ctx := context.Background()
	request := append([]byte{byte(len(currentSocialTestFriendName)), 0, 0, 0},
		rosterNameBytes(currentSocialTestFriendName)...)
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRegisiterToBlacklist), request); err != nil {
		t.Fatalf("register: %v", err)
	}
	blocked := service.loadCurrentBlacklistRecords(ctx, source.selectedCharacterID)
	if len(blocked) != 1 || blocked[0].Name != currentSocialTestFriendName {
		t.Fatalf("blocker's list = %+v", blocked)
	}
	if peer := service.loadCurrentBlacklistRecords(ctx, target.selectedCharacterID); len(peer) != 0 {
		t.Fatalf("the blocked character's list must stay empty, got %+v", peer)
	}
	repos, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("no repository")
	}
	if _, found, err := repos.Settings.Load(ctx, currentBlacklistScope(target.selectedCharacterID)); err != nil || found {
		t.Fatalf("the blocked character's scope must not be created: found=%v err=%v", found, err)
	}
	// The gate is still symmetric: the blocked character cannot message the blocker
	// either, and the blocker's own delete only clears the blocker's list.
	if _, err := service.handleAlignedGameCommand(target, byte(dnfenum.GameCmdCommand),
		currentPeerChatStateOpcode,
		buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, source.selectedCharacterID,
			[]byte("hello"), rosterNameBytes("source1"))); err != nil {
		t.Fatalf("blocked peer chat: %v", err)
	}
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketDeleteToBlacklist), request); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if left := service.loadCurrentBlacklistRecords(ctx, source.selectedCharacterID); len(left) != 0 {
		t.Fatalf("delete must clear the blocker's list, got %+v", left)
	}
	if peer := service.loadCurrentBlacklistRecords(ctx, target.selectedCharacterID); len(peer) != 0 {
		t.Fatalf("delete must not touch the other character, got %+v", peer)
	}
}

// Regression for the operator report 2026-09-22 23:25: both characters had blocked each
// other (packet_log: blocked_by_target=true blocked_by_source=true status=54), so the
// gate answers with the sender's own block (dstr 4771) inside the peer-menu window.
// Routing the peer's block first was tried and rolled back on the operator's call.
func TestCurrentOneToOneBlacklistGateReportsTheSendersOwnBlock(t *testing.T) {
	service, source, target, sourceConn, _ := newCurrentSocialPeerTest(t)
	ctx := context.Background()
	for _, blocked := range []struct {
		characterID uint16
		name        string
	}{
		{characterID: target.selectedCharacterID, name: "source1"},
		{characterID: source.selectedCharacterID, name: currentSocialTestFriendName},
	} {
		if err := service.saveCurrentBlacklistRecords(ctx, blocked.characterID,
			[]currentBlacklistRecord{{Name: blocked.name, At: 1758499200}}); err != nil {
			t.Fatalf("store blacklist for %d: %v", blocked.characterID, err)
		}
	}
	request := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode,
		target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
	if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, request); err != nil {
		t.Fatalf("peer chat: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	var want packetWriter
	want.writeByte(currentTownChatPeerMenuMode)
	want.writeByte(currentBlacklistStatusSenderBlockedTarget)
	want.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
	want.writeByte(currentChatIncomingLineFlag)
	want.writeRawDstr([]byte("hello"))
	want.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentTownChatOneToOneStateMsgID ||
		!bytes.Equal(packet.Body, want.bytes()) || len(rest) != 0 {
		t.Fatalf("blocked notice header=%+v body=%x want=%x rest=%x",
			packet.Header, packet.Body, want.bytes(), rest)
	}
}

// The 1:1 gate reports the two directions with the client's own status codes, and
// every other mode keeps the historical failure ack instead of a packet the client
// would not understand.
func TestCurrentChatRejectionNoticeCoversBothBlacklistStatuses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status byte
	}{
		{name: "peer blocked the sender", status: currentBlacklistStatusTargetBlockedSender},
		{name: "sender blocked the peer", status: currentBlacklistStatusSenderBlockedTarget},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := &bufferConn{}
			service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
			session := &gameSession{conn: conn, selectedCharacterID: 1}
			if err := service.sendCurrentChatRejectionNotice(session, currentTownChatPeerMenuMode, test.status, "test", []byte("hello")); err != nil {
				t.Fatalf("notice: %v", err)
			}
			packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
			if packet.Header.Classification != 0 || packet.Header.MsgID != currentTownChatOneToOneStateMsgID {
				t.Fatalf("notice header = %+v", packet.Header)
			}
			var want packetWriter
			want.writeByte(currentTownChatPeerMenuMode)
			want.writeByte(test.status)
			want.writeRawDstr(rosterNameBytes("test"))
			want.writeByte(currentChatIncomingLineFlag)
			want.writeRawDstr([]byte("hello"))
			want.writeRawDstr(rosterNameBytes("test"))
			if !bytes.Equal(packet.Body, want.bytes()) {
				t.Fatalf("notice body = %x want %x", packet.Body, want.bytes())
			}
			if len(rest) != 0 {
				t.Fatalf("notice trailing bytes = %x", rest)
			}
		})
	}

	// A typed whisper has its own rejected branch in the client, whose body stops
	// after the text dstr (the trailing "extra" one is read only in the peer-menu
	// window).
	for _, status := range []byte{currentBlacklistStatusTargetBlockedSender, currentBlacklistStatusSenderBlockedTarget} {
		conn := &bufferConn{}
		service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
		session := &gameSession{conn: conn, selectedCharacterID: 1}
		if err := service.sendCurrentChatRejectionNotice(session, currentTownChatWhisperMode, status, "test", []byte("hello")); err != nil {
			t.Fatalf("whisper notice: %v", err)
		}
		packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentTownChatOneToOneStateMsgID {
			t.Fatalf("whisper notice header = %+v", packet.Header)
		}
		var want packetWriter
		want.writeByte(currentTownChatWhisperMode)
		want.writeByte(status)
		want.writeRawDstr(rosterNameBytes("test"))
		want.writeByte(currentChatIncomingLineFlag)
		want.writeRawDstr([]byte("hello"))
		if !bytes.Equal(packet.Body, want.bytes()) {
			t.Fatalf("whisper notice body = %x want %x", packet.Body, want.bytes())
		}
		if len(rest) != 0 {
			t.Fatalf("whisper notice trailing bytes = %x", rest)
		}
	}

	// Modes whose client handler has no rejected branch keep the plain failure ack.
	for _, mode := range []byte{currentTownChatOneToOneMode, currentTownChatPartyMode} {
		conn := &bufferConn{}
		service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
		session := &gameSession{conn: conn, selectedCharacterID: 1}
		if err := service.sendCurrentChatRejectionNotice(session, mode, currentBlacklistStatusTargetBlockedSender, "test", []byte("hello")); err != nil {
			t.Fatalf("notice mode %#x: %v", mode, err)
		}
		packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.Classification != 1 || packet.Header.MsgID != currentPeerChatStateOpcode ||
			!bytes.Equal(packet.Body, []byte{0, currentTownChatGenericError}) {
			t.Fatalf("mode %#x ack header=%+v body=%x", mode, packet.Header, packet.Body)
		}
		if len(rest) != 0 {
			t.Fatalf("mode %#x trailing bytes = %x", mode, rest)
		}
	}
}

// A typed whisper must be refused too, and must tell the sender why: only the 1:1
// window path carried the blacklist gate before (operator report 2026-09-22 23:5x:
// "悄悄话现在并不能拦截并提示").
func TestCurrentWhisperChatAppliesTheBlacklistGate(t *testing.T) {
	for _, test := range []struct {
		name      string
		blockedBy string // "target" = the recipient blocked the sender
		wantPeer  bool   // whether the recipient receives the line
	}{
		{name: "recipient blocked the sender", blockedBy: "target", wantPeer: false},
		{name: "sender blocked the recipient", blockedBy: "source", wantPeer: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
			ctx := context.Background()
			// blockedBy "target": the recipient (friend5) blocked the sender (source1).
			// blockedBy "source": the sender blocked the recipient.
			owner, blockedName := source.selectedCharacterID, currentSocialTestFriendName
			if test.blockedBy == "target" {
				owner, blockedName = target.selectedCharacterID, "source1"
			}
			if err := service.saveCurrentBlacklistRecords(ctx, owner,
				[]currentBlacklistRecord{{Name: blockedName, At: 1758499200}}); err != nil {
				t.Fatalf("store blacklist: %v", err)
			}
			request := buildCurrentPrivateTownChatRequestForTest(currentTownChatWhisperMode,
				target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
			if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, request); err != nil {
				t.Fatalf("whisper: %v", err)
			}
			packet, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
			var want packetWriter
			want.writeByte(currentTownChatWhisperMode)
			if test.blockedBy == "target" {
				want.writeByte(currentBlacklistStatusTargetBlockedSender)
			} else {
				want.writeByte(currentBlacklistStatusSenderBlockedTarget)
			}
			want.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
			want.writeByte(currentChatIncomingLineFlag)
			want.writeRawDstr([]byte("hello"))
			if packet.Header.Classification != 0 || packet.Header.MsgID != currentTownChatOneToOneStateMsgID ||
				!bytes.Equal(packet.Body, want.bytes()) || len(rest) != 0 {
				t.Fatalf("whisper rejection header=%+v body=%x want=%x rest=%x",
					packet.Header, packet.Body, want.bytes(), rest)
			}
			if delivered := targetConn.write.Bytes(); len(delivered) != 0 {
				t.Fatalf("the refused whisper still reached the recipient: %x", delivered)
			}
		})
	}
}

// The upper/class-1 transport reaches 118/119 through the aligned owner, which used
// to answer by echoing the name without storing anything -- so a block made over
// that transport was lost on relog. Both transports must now persist.
func TestUpperTransportBlacklistMutationReachesTheStore(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	request := append([]byte{4, 0, 0, 0}, []byte("test")...)
	handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRegisiterToBlacklist), request)
	if err != nil {
		t.Fatalf("aligned register: %v", err)
	}
	if !handled {
		t.Fatal("aligned register was not handled")
	}
	records := service.loadCurrentBlacklistRecords(context.Background(), source.selectedCharacterID)
	if len(records) != 1 || records[0].Name != "test" {
		t.Fatalf("aligned register did not reach the store: %+v", records)
	}
	ack, _ := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	want := append([]byte{1, 4, 0, 0, 0}, []byte("test")...)
	if !bytes.Equal(ack.Body, want) {
		t.Fatalf("aligned ack body = %x want %x", ack.Body, want)
	}

	sourceConn.write.Reset()
	if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketDeleteToBlacklist), request); err != nil {
		t.Fatalf("aligned delete: %v", err)
	}
	if records := service.loadCurrentBlacklistRecords(context.Background(), source.selectedCharacterID); len(records) != 0 {
		t.Fatalf("aligned delete did not reach the store: %+v", records)
	}
}

// A session with no repository keeps the historic ack-only echo.
func TestAlignedBlacklistMutationWithoutStoreKeepsEchoAck(t *testing.T) {
	conn := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: conn, selectedCharacterID: 1}
	var request packetWriter
	request.writeRawDstr(rosterNameBytes("friend5"))
	handled, err := service.handleAlignedGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRegisiterToBlacklist), request.bytes())
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	packet, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if want := append([]byte{1}, request.bytes()...); !bytes.Equal(packet.Body, want) {
		t.Fatalf("echo ack = %x want %x", packet.Body, want)
	}
}

// Client names arrive in the wire's GB18030 form (live 1:1 capture: c0 b4 c0 b4 c0 b4
// for 来来来). The stored name must be the decoded spelling so the repository compare
// matches, and the ack must re-encode to the very bytes the client sent.
func TestBlacklistStoresClientGB18030NamesVerbatim(t *testing.T) {
	raw := []byte{0xc0, 0xb4, 0xc0, 0xb4, 0xc0, 0xb4}
	body := append([]byte{byte(len(raw)), 0, 0, 0}, raw...)
	name, ok := parseCurrentBlacklistName(body)
	if !ok {
		t.Fatal("GB18030 name was rejected")
	}
	if name != "来来来" {
		t.Fatalf("decoded name = %q want 来来来", name)
	}
	if encoded := rosterNameBytes(name); !bytes.Equal(encoded, raw) {
		t.Fatalf("re-encoded name = %x want %x", encoded, raw)
	}

	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	if err := service.handleGameCommand(source, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRegisiterToBlacklist), body); err != nil {
		t.Fatalf("register: %v", err)
	}
	ack, _ := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	want := append([]byte{1, byte(len(raw)), 0, 0, 0}, raw...)
	if !bytes.Equal(ack.Body, want) {
		t.Fatalf("GB18030 ack = %x want %x", ack.Body, want)
	}
	listed := buildCurrentBlacklistListBody(service.loadCurrentBlacklistRecords(context.Background(), 1))
	if !bytes.Contains(listed, raw) {
		t.Fatalf("list answer lost the client's name bytes: %x", listed)
	}
	if !service.currentBlacklistHas(context.Background(), 1, "来来来") {
		t.Fatal("stored GB18030 name is not comparable with the repository spelling")
	}
}
