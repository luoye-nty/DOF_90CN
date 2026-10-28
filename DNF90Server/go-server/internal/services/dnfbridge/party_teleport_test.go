package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// livePartyTeleportRequest20260912 is the exact legacy op434 body the current
// EXE sent when the player pressed 组队传送. It is copied from the live incident
// (runtime/logs/packet_log.txt 2026-09-12T09:40:04Z, conn game-000152,
// pkt_seq 660, hex 01b2011500000018460fae9b002901170276010501), where it was
// routed into the 18-byte native path-report parser, refused, and the refusal
// closed the whole game connection.
var livePartyTeleportRequest20260912 = []byte{0x29, 0x01, 0x17, 0x02, 0x76, 0x01, 0x05, 0x01}

func TestCurrentPartyTeleportRequestIsConsumedInsteadOfRefused(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	beforeKey, beforeMTU := session.partyRoutingKey, session.partyPathMTU

	if err := service.handleCurrentPartyTeleportOrNetworkReport(session, livePartyTeleportRequest20260912); err != nil {
		t.Fatalf("party teleport request was refused instead of consumed: %v", err)
	}
	// It is not a path report, so it must not install transport state.
	if session.partyRoutingKey != beforeKey || session.partyPathMTU != beforeMTU {
		t.Fatalf("party teleport request changed the transport identity: key=%d mtu=%d",
			session.partyRoutingKey, session.partyPathMTU)
	}
}

// TestParseCurrentPartyTeleportRequestLiveSamples pins the destination layout
// recovered from the client's own op434 writer (DNF.exe VA 0x00E9FB00):
// u8 town, u8 area, u16 x, u16 y, u8 direction, u8 flag. Both bodies were sent
// by the live client (packet_log.txt 2026-09-12T09:40:04Z and 09:53:09Z).
func TestParseCurrentPartyTeleportRequestLiveSamples(t *testing.T) {
	for _, test := range []struct {
		hex        string
		town, area byte
		x, y       uint16
	}{
		{hex: "2901170276010501", town: 0x29, area: 0x01, x: 0x0217, y: 0x0176},
		{hex: "2b00d70533030501", town: 0x2b, area: 0x00, x: 0x05d7, y: 0x0333},
	} {
		body, err := hex.DecodeString(test.hex)
		if err != nil {
			t.Fatal(err)
		}
		request, err := parseCurrentPartyTeleportRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		if request.DestinationTown != test.town || request.DestinationArea != test.area ||
			request.DestinationX != test.x || request.DestinationY != test.y ||
			request.Direction != 5 || request.Flag != 1 {
			t.Fatalf("sample %s parsed=%+v", test.hex, request)
		}
	}
}

// TestHandleCurrentPartyTeleportMovesTheSenderThroughTheSharedTownRoute proves
// the destination is applied with the same op36 owner the personal teleport
// uses: town 39 area 0 at (900,250) is the cross-town destination the existing
// personal-teleport test already proves the PVF route admits.
func TestHandleCurrentPartyTeleportMovesTheSenderThroughTheSharedTownRoute(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	// 公会传送次数 (decision D4): op434/435 now requires the requesting leader's guild
	// to hold an active 公会传送 (type 7) benefit, which is exactly the condition
	// upstream checks before publishing its pending state. The benefit's Value IS
	// the remaining-use counter. Granting it here is the fixture catching up with
	// that rule; every assertion about the move itself is unchanged.
	grantPartyTeleportGuild(t, service, session, repositories, 3)
	body := []byte{39, 0, 0x84, 0x03, 0xfa, 0x00, 5, 1}

	if err := service.handleCurrentPartyTeleportRequest(session, body); err != nil {
		t.Fatalf("handle party teleport: %v", err)
	}
	if session.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("party teleport wrote no scene transition")
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found ||
		stored.Stats["town_id"] != 39 ||
		stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 900 ||
		stored.Stats["pos_y"] != 250 ||
		stored.Stats["direction"] != 5 {
		t.Fatalf("party teleport location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

// grantPartyTeleportGuild puts the session's selected character into a fresh guild
// holding `uses` 公会传送 (type 7) uses, which is the precondition op434/435 now
// requires (decision D4).
func grantPartyTeleportGuild(t *testing.T, service *Service, session *gameSession, repositories dnfrepo.Group, uses uint32) {
	t.Helper()
	if repositories.Settings == nil {
		t.Fatal("party teleport fixture has no settings repository")
	}
	ctx := context.Background()
	characterID := strconv.Itoa(int(selectedCharacterID(session)))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("party teleport fixture character %s: found=%t err=%v", characterID, found, err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(character, "tpguild", serviceTestNow())
	if err != nil {
		t.Fatalf("create party teleport guild: %v", err)
	}
	guild.Benefits = map[byte]dnfguild.Benefit{7: {Type: 7, Value: uses, Mode: 1}}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatalf("save party teleport guild: %v", err)
	}
}

// TestHandleCurrentPartyTeleportRejectsNonLeader keeps the client's own rule on
// the server: "只有队长才能使用组队传送功能". A member's request must move nobody.
func TestHandleCurrentPartyTeleportRejectsNonLeader(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	storeRuntimePartyState(session, alignedcmd.PartyState{
		PartyID: 7,
		UserID:  1,
		Members: []alignedcmd.PartyMemberState{
			{Slot: 0, UserID: 1},
			{Slot: 1, UserID: selectedCharacterID(session)},
		},
	})
	before, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("fixture character: found=%t err=%v", found, err)
	}
	beforeTown := before.Stats["town_id"]

	body := []byte{39, 0, 0x84, 0x03, 0xfa, 0x00, 5, 1}
	if err := service.handleCurrentPartyTeleportRequest(session, body); err != nil {
		t.Fatalf("non-leader request errored: %v", err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("non-leader party teleport wrote a scene transition")
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != beforeTown {
		t.Fatalf("non-leader party teleport moved the character: found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

// TestBuildCurrentPartyTeleportNotifyBodyPinsTheDecodedLayout keeps the S2C 377
// fields that were read off the client stable: the destination index, the phase,
// the requester PARTY SLOT (not a character id), the per-slot character id, the
// answered-state byte and the 0xFFFF empty-slot sentinel.
func TestBuildCurrentPartyTeleportNotifyBodyPinsTheDecodedLayout(t *testing.T) {
	pending := &currentPartyTeleportSession{
		partyID:    2,
		leaderID:   1,
		leaderSlot: 0,
		request:    currentPartyTeleportRequest{DestinationTown: 38, DestinationArea: 0, DestinationX: 962, DestinationY: 287},
		plans: []currentPartyTeleportPlan{
			{characterID: 1, slot: 0, eligible: true},
			{characterID: 2, slot: 1, eligible: true},
		},
		answers: map[uint16]byte{2: currentPartyTeleportAnswerAccept},
	}
	body := buildCurrentPartyTeleportNotifyBody(pending, currentPartyTeleportPhaseRequest)
	if len(body) != currentPartyTeleportNotifyBodySize {
		t.Fatalf("notify body len=%d, want %d", len(body), currentPartyTeleportNotifyBodySize)
	}
	// The request phase that keeps the window on screen is 0, not 1: live
	// 2026-09-12 21:0x, the phase-1 prompt flashed while the next shape the retry
	// button published - phase 0, everything else identical - stayed open, and the
	// member's 接受 teleported the party. body[2] is the requester countdown flag,
	// which the retail switch leaves on by default
	// (DNF90_PARTY_TELEPORT_COOLDOWN=off removes it).
	want := []byte{
		38, 0, currentPartyTeleportSubStateActive, 0, 0,
		0x01, 0x00, currentPartyTeleportSlotPending,
		0x02, 0x00, currentPartyTeleportSlotAccepted,
		0xFF, 0xFF, 0x00,
		0xFF, 0xFF, 0x00,
		0xFF, 0xFF, 0x00,
		0xFF, 0xFF, 0x00,
		0xFF, 0xFF, 0x00,
		0xFF, 0xFF, 0x00,
		0x00,
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("notify body = % X\nwant = % X", body, want)
	}

	// The ended phase must not claim an active request and must leave the
	// requester slot alone, because the client's own path only closes the prompt
	// for the party it already knows about.
	ended := buildCurrentPartyTeleportNotifyBody(pending, currentPartyTeleportPhaseEnded)
	if ended[1] != currentPartyTeleportPhaseEnded || ended[2] != 0 || ended[3] != 0 {
		t.Fatalf("ended body header = % X", ended[:5])
	}
	// A refused or expired request publishes the client's timeout phase instead, so
	// the window stays on screen with 重新请求/取消; only a completed request carries
	// the ended phase that the client patch turns into a close.
	expired := buildCurrentPartyTeleportNotifyBody(pending, currentPartyTeleportPhaseTimeout)
	if expired[1] != currentPartyTeleportPhaseTimeout || expired[2] != 0 {
		t.Fatalf("timeout body header = % X", expired[:5])
	}
}

// TestBuildCurrentPartyTeleportMemberListBody pins the S2C 379 layout the
// client's row builder reads: `u8 mode` (3 = allocate one row per member),
// `u8 count`, then `count` u16 member character ids.
func TestBuildCurrentPartyTeleportMemberListBody(t *testing.T) {
	if currentPartyTeleportMemberListOpcode != 379 {
		t.Fatalf("member list opcode = %d, want 379", currentPartyTeleportMemberListOpcode)
	}
	members := []currentPartyTeleportPlan{
		{characterID: 1, slot: 0, eligible: true},
		{characterID: 2, slot: 1, eligible: true},
	}
	want := []byte{currentPartyTeleportMemberListModeCreate, 2, 0x01, 0x00, 0x02, 0x00}
	if got := buildCurrentPartyTeleportMemberListBody(members); !bytes.Equal(got, want) {
		t.Fatalf("member list body = % X, want % X", got, want)
	}
	if got := buildCurrentPartyTeleportMemberListBody(nil); !bytes.Equal(got, []byte{3, 0}) {
		t.Fatalf("empty member list body = % X", got)
	}
}

// TestCurrentPartyTeleportCooldownFlagIsAlwaysOnForRequests pins the countdown
// flag: the retail behaviour is unconditional now, so a request body carries it and
// a terminal body does not.
func TestCurrentPartyTeleportCooldownFlagIsAlwaysOnForRequests(t *testing.T) {
	pending := &currentPartyTeleportSession{
		partyID:    2,
		leaderID:   1,
		leaderSlot: 0,
		request:    currentPartyTeleportRequest{DestinationTown: 38},
		plans:      []currentPartyTeleportPlan{{characterID: 1, slot: 0}, {characterID: 2, slot: 1}},
	}
	if body := buildCurrentPartyTeleportNotifyBody(pending, currentPartyTeleportPhaseRequest); body[2] != currentPartyTeleportSubStateActive {
		t.Fatalf("request body[2] = %d, want the countdown flag %d",
			body[2], currentPartyTeleportSubStateActive)
	}
	if body := buildCurrentPartyTeleportNotifyBody(pending, currentPartyTeleportPhaseEnded); body[2] != 0 {
		t.Fatalf("ended body[2] = %d, want 0", body[2])
	}
}

// TestBuildCurrentPartyTeleportCooldownBody pins the S2C 378 layout the client's
// timer reader consumes: `u8 count` + `u8 index` + `u32 seconds`, with index 0 (the
// party teleport) and the ten minute retail lock.
func TestBuildCurrentPartyTeleportCooldownBody(t *testing.T) {
	if currentPartyTeleportCooldownOpcode != 378 {
		t.Fatalf("cooldown opcode = %d, want 378", currentPartyTeleportCooldownOpcode)
	}
	if currentPartyTeleportCooldownSeconds != 600 {
		t.Fatalf("cooldown seconds = %d, want the ten minute retail lock",
			currentPartyTeleportCooldownSeconds)
	}
	want := []byte{1, 0, 0x58, 0x02, 0, 0}
	if got := buildCurrentPartyTeleportCooldownBody(currentPartyTeleportCooldownSeconds); !bytes.Equal(got, want) {
		t.Fatalf("cooldown body = % X, want % X", got, want)
	}
	if got := buildCurrentPartyTeleportCooldownBody(0); !bytes.Equal(got, []byte{1, 0, 0, 0, 0, 0}) {
		t.Fatalf("zero cooldown body = % X", got)
	}
}

// TestCurrentPartyTeleportConsentCompletion covers the answer bookkeeping: the
// leader is not asked, an unanswered member blocks completion, a refusal never
// completes, and every member accepting does.
func TestCurrentPartyTeleportConsentCompletion(t *testing.T) {
	service := &Service{}
	pending := &currentPartyTeleportSession{
		partyID:  2,
		leaderID: 1,
		plans: []currentPartyTeleportPlan{
			{characterID: 1, slot: 0},
			{characterID: 2, slot: 1},
		},
		answers: map[uint16]byte{},
	}
	if members := service.currentPartyTeleportConsentMembers(pending); len(members) != 1 || members[0].characterID != 2 {
		t.Fatalf("consent members = %+v, want only the non-leader", members)
	}
	if service.currentPartyTeleportConsentComplete(pending) {
		t.Fatal("an unanswered member must not complete the consent")
	}
	pending.answers[2] = currentPartyTeleportAnswerRefuse
	if service.currentPartyTeleportConsentComplete(pending) {
		t.Fatal("a refusal must not complete the consent")
	}
	pending.answers[2] = currentPartyTeleportAnswerAccept
	if !service.currentPartyTeleportConsentComplete(pending) {
		t.Fatal("every member accepting must complete the consent")
	}
	// A one-member party has nobody to ask and resolves immediately.
	solo := &currentPartyTeleportSession{
		partyID:  3,
		leaderID: 1,
		plans:    []currentPartyTeleportPlan{{characterID: 1, slot: 0}},
		answers:  map[uint16]byte{},
	}
	if len(service.currentPartyTeleportConsentMembers(solo)) != 0 {
		t.Fatal("a one-member party must not require any answer")
	}
}

// TestCurrentPartyTeleportRetrySentinelMatchesTheLiveButton pins the shape of the
// 重新请求 button: the live 2026-09-12 19:09:50 capture is
// `ff ff ff ff ff ff 05 01` - a blank destination, three times in a row, from the
// leader's client right after the client's own 10 s timeout dialog.
func TestCurrentPartyTeleportRetrySentinelMatchesTheLiveButton(t *testing.T) {
	body, err := hex.DecodeString("ffffffffffff0501")
	if err != nil {
		t.Fatal(err)
	}
	request, err := parseCurrentPartyTeleportRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !currentPartyTeleportRequestIsRetry(request) {
		t.Fatalf("live retry body parsed=%+v, want retry", request)
	}
	ordinary, err := parseCurrentPartyTeleportRequest(livePartyTeleportRequest20260912)
	if err != nil {
		t.Fatal(err)
	}
	if currentPartyTeleportRequestIsRetry(ordinary) {
		t.Fatalf("ordinary request parsed=%+v, want destination", ordinary)
	}
}

// TestHandleCurrentPartyTeleportRetryReplaysTheRememberedDestination covers the
// button end to end: the blank request carries no destination, so the server
// replays the last one instead of refusing an unknown town.
func TestHandleCurrentPartyTeleportRetryReplaysTheRememberedDestination(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	charID := selectedCharacterID(session)
	storeRuntimePartyState(session, alignedcmd.PartyState{
		PartyID: 7,
		UserID:  charID,
		Members: []alignedcmd.PartyMemberState{{Slot: 0, UserID: charID}},
	})
	service.rememberCurrentPartyTeleportRequest(7, currentPartyTeleportRequest{
		DestinationTown: 39,
		DestinationArea: 0,
		DestinationX:    900,
		DestinationY:    250,
		Direction:       5,
		Flag:            1,
	})

	body, err := hex.DecodeString("ffffffffffff0501")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentPartyTeleportRequest(session, body); err != nil {
		t.Fatalf("retry request: %v", err)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found ||
		stored.Stats["town_id"] != 39 ||
		stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 900 ||
		stored.Stats["pos_y"] != 250 {
		t.Fatalf("retry did not replay the remembered destination: found=%t err=%v stats=%+v",
			found, err, stored.Stats)
	}
}

// TestHandleCurrentPartyTeleportRetryWithoutHistoryMovesNobody keeps the retry
// button from inventing a destination when the party never asked for one.
func TestHandleCurrentPartyTeleportRetryWithoutHistoryMovesNobody(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	charID := selectedCharacterID(session)
	storeRuntimePartyState(session, alignedcmd.PartyState{
		PartyID: 9,
		UserID:  charID,
		Members: []alignedcmd.PartyMemberState{{Slot: 0, UserID: charID}},
	})
	before, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("fixture character: found=%t err=%v", found, err)
	}
	body, err := hex.DecodeString("ffffffffffff0501")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentPartyTeleportRequest(session, body); err != nil {
		t.Fatalf("retry request: %v", err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("a retry without history wrote a scene transition")
	}
	after, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || after.Stats["town_id"] != before.Stats["town_id"] {
		t.Fatalf("a retry without history moved the character: found=%t err=%v stats=%+v",
			found, err, after.Stats)
	}
}

// TestOnlinePartyCommandRoutesLegacyOp434ThroughTheDiscriminator keeps the
// routing pinned: binding legacy op434 straight back to the 18-byte path-report
// parser is what closed the player's connection.
func TestOnlinePartyCommandRoutesLegacyOp434ThroughTheDiscriminator(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	handled, err := service.handleOnlinePartyCommand(
		session,
		uint16(dnfenum.CmdPacketPartyTeleport),
		livePartyTeleportRequest20260912,
	)
	if !handled {
		t.Fatal("legacy op434 party teleport was not taken by the party command router")
	}
	if err != nil {
		t.Fatalf("legacy op434 party teleport errored: %v", err)
	}
}

func TestCurrentPartyTeleportOrNetworkReportKeepsTheNativePathReport(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	body := make([]byte, currentPartyNetworkReportSize)
	binary.LittleEndian.PutUint32(body[0:4], 1280)
	binary.LittleEndian.PutUint32(body[10:14], currentPartyNetworkVersion)

	if err := service.handleCurrentPartyTeleportOrNetworkReport(session, body); err != nil {
		t.Fatalf("native path report was refused: %v", err)
	}
	if session.partyPathMTU != 1280 {
		t.Fatalf("native path report mtu=%d, want 1280", session.partyPathMTU)
	}
}

// TestCurrentPartyClientInputRejectionIsNonFatal pins the sentinel contract: an
// unproved client shape is refused as input, never as a session failure, because
// the fatal inbound boundary answers a session failure by closing the game
// connection.
func TestCurrentPartyClientInputRejectionIsNonFatal(t *testing.T) {
	service := &Service{}
	session := &gameSession{}

	shortReport := make([]byte, currentPartyNetworkReportSize-1)
	if err := service.handleCurrentPartyNetworkReport(session, shortReport); !errors.Is(err, errGameClientInputRejected) {
		t.Fatalf("short native path report error = %v, want a client-input rejection", err)
	}

	nonNativeMTU := make([]byte, currentPartyNetworkReportSize)
	binary.LittleEndian.PutUint32(nonNativeMTU[0:4], 1500)
	binary.LittleEndian.PutUint32(nonNativeMTU[10:14], currentPartyNetworkVersion)
	if err := service.handleCurrentPartyNetworkReport(session, nonNativeMTU); !errors.Is(err, errGameClientInputRejected) {
		t.Fatalf("non-native path MTU error = %v, want a client-input rejection", err)
	}

	if err := service.handleCurrentPartyTransportTelemetry(session, currentPartyPeerStateOpcode, []byte{1, 2}); !errors.Is(err, errGameClientInputRejected) {
		t.Fatalf("short peer-state report error = %v, want a client-input rejection", err)
	}
}
