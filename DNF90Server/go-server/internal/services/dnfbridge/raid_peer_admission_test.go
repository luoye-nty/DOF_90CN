package dnfbridge

import (
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

func TestResolveRuntimeRaidPeerAdmissionDirection(t *testing.T) {
	cases := []struct {
		name                                    string
		requesterInRaid, targetInRaid, isLeader bool
		wantApplication, wantOK                 bool
	}{
		{name: "leader invites outsider", requesterInRaid: true, isLeader: true, wantOK: true},
		{name: "member cannot invite", requesterInRaid: true},
		{name: "outsider applies", targetInRaid: true, wantApplication: true, wantOK: true},
		{name: "two outsiders"},
		{name: "two different raids", requesterInRaid: true, targetInRaid: true, isLeader: true},
	}
	for _, tc := range cases {
		application, ok := resolveRuntimeRaidPeerAdmission(tc.requesterInRaid, tc.targetInRaid, tc.isLeader)
		if application != tc.wantApplication || ok != tc.wantOK {
			t.Fatalf("%s: application=%t ok=%t want application=%t ok=%t", tc.name, application, ok, tc.wantApplication, tc.wantOK)
		}
	}
}

type runtimeRaidPeerFixture struct {
	service *Service
	raidKey uint32
	leader  *gameSession
	member  *gameSession
	outside *gameSession
}

func newRuntimeRaidPeerFixture(t *testing.T) runtimeRaidPeerFixture {
	t.Helper()
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:         make(map[uint32]*runtimeRaidState),
		onlinePlayers: newOnlinePlayerManager(),
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10200}
	newSession := func(id uint16) *gameSession {
		session := &gameSession{conn: &bufferConn{}, channel: channel}
		service.bindGameSessionCharacter(session, id)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: id, TownID: 19, AreaID: 0, Session: session})
		return session
	}
	fixture := runtimeRaidPeerFixture{
		service: service,
		raidKey: 0x00c80500,
		leader:  newSession(1001),
		member:  newSession(1002),
		outside: newSession(1003),
	}
	service.raids[fixture.raidKey] = &runtimeRaidState{
		RaidKey:   fixture.raidKey,
		LeaderID:  1001,
		TailFlag:  5,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	return fixture
}

func runtimeRaidPeerRequestBody(target uint16) []byte {
	body := make([]byte, 13)
	binary.LittleEndian.PutUint16(body[0:2], target)
	body[2] = runtimeRaidPeerRequestMode
	binary.LittleEndian.PutUint32(body[3:7], 0x11223344)
	binary.LittleEndian.PutUint16(body[7:9], 0)
	binary.LittleEndian.PutUint32(body[9:13], 0)
	return body
}

func runtimeRaidPeerResponseBody(requester uint16, declined bool) []byte {
	body := make([]byte, 7, 9)
	binary.LittleEndian.PutUint16(body[0:2], requester)
	body[2] = runtimeRaidPeerRequestMode
	if declined {
		body = append(body, 0, 0)
	}
	return body
}

func runtimeRaidPeerTake(t *testing.T, session *gameSession) []dnfproto.ChannelPacket {
	t.Helper()
	conn := session.conn.(*bufferConn)
	raw := append([]byte(nil), conn.write.Bytes()...)
	conn.write.Reset()
	var packets []dnfproto.ChannelPacket
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		packets = append(packets, packet)
	}
	return packets
}

func runtimeRaidPeerFind(packets []dnfproto.ChannelPacket, msgID uint16) (dnfproto.ChannelPacket, bool) {
	for _, packet := range packets {
		if packet.Header.MsgID == msgID {
			return packet, true
		}
	}
	return dnfproto.ChannelPacket{}, false
}

func assertRuntimeRaidPeerPrompt(t *testing.T, packets []dnfproto.ChannelPacket, wantRequester uint16) {
	t.Helper()
	prompt, ok := runtimeRaidPeerFind(packets, 0x0007)
	if !ok {
		t.Fatal("responder received no op7 prompt")
	}
	if len(prompt.Body) < 3 || binary.LittleEndian.Uint16(prompt.Body[0:2]) != wantRequester || prompt.Body[2] != runtimeRaidPeerRequestMode {
		t.Fatalf("op7 prompt body=%x want requester=%d mode=%d", prompt.Body, wantRequester, runtimeRaidPeerRequestMode)
	}
}

func assertRuntimeRaidPeerAdmitted(t *testing.T, fixture runtimeRaidPeerFixture, applicant *gameSession, applicantID uint16) {
	t.Helper()
	state := fixture.service.raids[fixture.raidKey]
	if runtimeRaidMemberIndex(*state, applicantID) < 0 {
		t.Fatalf("applicant %d is not in the roster: %+v", applicantID, state.Members)
	}
	assertRuntimeRaidGridUnique(t, *state)
	packets := runtimeRaidPeerTake(t, applicant)
	ack, ok := runtimeRaidPeerFind(packets, uint16(dnfenum.CmdPacketCreateRaid))
	if !ok {
		t.Fatal("applicant received no 664 binding ACK")
	}
	if want := raid.BuildCreateRaidResultBody(fixture.raidKey); string(ack.Body) != string(want) {
		t.Fatalf("664 binding body=%x want=%x", ack.Body, want)
	}
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidObjectMsgID); !ok {
		t.Fatal("applicant received no raid object before the binding ACK")
	}
	leaderPackets := runtimeRaidPeerTake(t, fixture.leader)
	if _, ok := runtimeRaidPeerFind(leaderPackets, raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("leader received no roster refresh after admission")
	}
}

func TestRuntimeRaidPeerLeaderInvitesOutsider(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	if err := fixture.service.handleOnlineRequestPeer(fixture.leader, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("leader invite: %v", err)
	}
	assertRuntimeRaidPeerPrompt(t, runtimeRaidPeerTake(t, fixture.outside), 1001)
	runtimeRaidPeerTake(t, fixture.leader)
	if err := fixture.service.handleOnlineResponsePeer(fixture.outside, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1001, false)); err != nil {
		t.Fatalf("outsider accept: %v", err)
	}
	assertRuntimeRaidPeerAdmitted(t, fixture, fixture.outside, 1003)
	if len(fixture.service.raidPeerAdmissions) != 0 {
		t.Fatalf("pending admission survived acceptance: %+v", fixture.service.raidPeerAdmissions)
	}
}

func TestRuntimeRaidPeerOutsiderAppliesToLeader(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	// Clicking an ordinary member routes the prompt to the current leader.
	if err := fixture.service.handleOnlineRequestPeer(fixture.outside, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1002)); err != nil {
		t.Fatalf("outsider apply: %v", err)
	}
	if packets := runtimeRaidPeerTake(t, fixture.member); func() bool { _, ok := runtimeRaidPeerFind(packets, 0x0007); return ok }() {
		t.Fatal("clicked non-leader member received the admission prompt")
	}
	assertRuntimeRaidPeerPrompt(t, runtimeRaidPeerTake(t, fixture.leader), 1003)
	runtimeRaidPeerTake(t, fixture.outside)
	if err := fixture.service.handleOnlineResponsePeer(fixture.leader, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1003, false)); err != nil {
		t.Fatalf("leader approve: %v", err)
	}
	assertRuntimeRaidPeerAdmitted(t, fixture, fixture.outside, 1003)
}

func TestRuntimeRaidPeerNonLeaderCannotInvite(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	if err := fixture.service.handleOnlineRequestPeer(fixture.member, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("member invite: %v", err)
	}
	if _, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.outside), 0x0007); ok {
		t.Fatal("outsider was prompted by a non-leader invite")
	}
	if len(fixture.service.raidPeerAdmissions) != 0 {
		t.Fatalf("non-leader invite recorded a pending admission: %+v", fixture.service.raidPeerAdmissions)
	}
}

func TestRuntimeRaidPeerDeclineAndMissingPromptDoNotAdmit(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	// A response with no pending prompt is ignored outright.
	if err := fixture.service.handleOnlineResponsePeer(fixture.outside, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1001, false)); err != nil {
		t.Fatalf("stray accept: %v", err)
	}
	if runtimeRaidMemberIndex(*fixture.service.raids[fixture.raidKey], 1003) >= 0 {
		t.Fatal("stray accept admitted the outsider")
	}
	// A declined prompt consumes the pending record without admitting.
	if err := fixture.service.handleOnlineRequestPeer(fixture.leader, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("leader invite: %v", err)
	}
	if err := fixture.service.handleOnlineResponsePeer(fixture.outside, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1001, true)); err != nil {
		t.Fatalf("outsider decline: %v", err)
	}
	if runtimeRaidMemberIndex(*fixture.service.raids[fixture.raidKey], 1003) >= 0 {
		t.Fatal("declined prompt admitted the outsider")
	}
	if len(fixture.service.raidPeerAdmissions) != 0 {
		t.Fatalf("declined prompt left a pending admission: %+v", fixture.service.raidPeerAdmissions)
	}
	// The same accept after a decline must not resurrect the prompt.
	if err := fixture.service.handleOnlineResponsePeer(fixture.outside, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1001, false)); err != nil {
		t.Fatalf("late accept: %v", err)
	}
	if runtimeRaidMemberIndex(*fixture.service.raids[fixture.raidKey], 1003) >= 0 {
		t.Fatal("late accept after decline admitted the outsider")
	}
}

func TestRuntimeRaidPeerPromptExpires(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	if err := fixture.service.handleOnlineRequestPeer(fixture.leader, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("leader invite: %v", err)
	}
	key := runtimeRaidPeerAdmissionKey{Responder: 1003, Requester: 1001}
	pending, found := fixture.service.raidPeerAdmissions[key]
	if !found {
		t.Fatalf("invite recorded no pending admission: %+v", fixture.service.raidPeerAdmissions)
	}
	pending.ExpiresAt = fixture.service.gameplayNow().Add(-time.Second)
	fixture.service.raidPeerAdmissions[key] = pending
	if err := fixture.service.handleOnlineResponsePeer(fixture.outside, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1001, false)); err != nil {
		t.Fatalf("expired accept: %v", err)
	}
	if runtimeRaidMemberIndex(*fixture.service.raids[fixture.raidKey], 1003) >= 0 {
		t.Fatal("expired prompt admitted the outsider")
	}
	if len(fixture.service.raidPeerAdmissions) != 0 {
		t.Fatalf("expired prompt was not pruned: %+v", fixture.service.raidPeerAdmissions)
	}
}

func TestRuntimeRaidPeerRequestRejectsFullOrStartedRaid(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	state := fixture.service.raids[fixture.raidKey]
	for i := len(state.Members); i < raid.MaxAttackPartyMembers; i++ {
		state.Members = append(state.Members, runtimeRaidMemberState{
			CharID:     uint16(2000 + i),
			GroupIndex: byte(i/runtimeRaidGroupSize) + 1,
			SlotOrder:  byte(i % runtimeRaidGroupSize),
		})
	}
	if err := fixture.service.handleOnlineRequestPeer(fixture.leader, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("full invite: %v", err)
	}
	if _, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.outside), 0x0007); ok {
		t.Fatal("full raid still prompted the outsider")
	}

	state.Members = state.Members[:2]
	state.Started = true
	if err := fixture.service.handleOnlineRequestPeer(fixture.leader, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("started invite: %v", err)
	}
	if _, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.outside), 0x0007); ok {
		t.Fatal("started raid still prompted the outsider")
	}
	if len(fixture.service.raidPeerAdmissions) != 0 {
		t.Fatalf("rejected requests recorded pending admissions: %+v", fixture.service.raidPeerAdmissions)
	}
}

func TestRuntimeRaidPeerAcceptRevalidatesLeaderAndCapacity(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	if err := fixture.service.handleOnlineRequestPeer(fixture.leader, uint16(dnfenum.CmdPacketRequestPeer), runtimeRaidPeerRequestBody(1003)); err != nil {
		t.Fatalf("leader invite: %v", err)
	}
	// Leadership moved between prompt and answer: the old leader's invite no
	// longer carries authority.
	fixture.service.raids[fixture.raidKey].LeaderID = 1002
	if err := fixture.service.handleOnlineResponsePeer(fixture.outside, uint16(dnfenum.CmdPacketResponsePeer), runtimeRaidPeerResponseBody(1001, false)); err != nil {
		t.Fatalf("accept after leader change: %v", err)
	}
	if runtimeRaidMemberIndex(*fixture.service.raids[fixture.raidKey], 1003) >= 0 {
		t.Fatal("stale leader invite admitted the outsider")
	}
}
