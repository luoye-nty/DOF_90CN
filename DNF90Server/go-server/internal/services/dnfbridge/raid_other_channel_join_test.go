package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// runtimeRaidJoinFixture models the reported defect: a leader holds a recruiting
// team raid on a channel the applicant is browsing from the 寻找攻坚队 window.
type runtimeRaidJoinFixture struct {
	service   *Service
	raidKey   uint32
	leader    *gameSession
	member    *gameSession
	applicant *gameSession
}

func newRuntimeRaidJoinFixture(t *testing.T) runtimeRaidJoinFixture {
	t.Helper()
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		onlinePlayers:  newOnlinePlayerManager(),
		gameplayTimers: newFakeCurrentDungeonDeathTimerQueue(),
	}
	newSession := func(id uint16, channel channelcatalog.Channel) *gameSession {
		session := &gameSession{conn: &bufferConn{}, channel: channel}
		service.bindGameSessionCharacter(session, id)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: id, TownID: 19, AreaID: 0, Session: session})
		return session
	}
	// The raid key encodes its channel in the top u16, which is what the C2S 820
	// body splits into its u8 and u16 halves.
	fixture := runtimeRaidJoinFixture{
		service: service,
		raidKey: 23<<16 | 1,
		leader:  newSession(1001, channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 1023}),
		member:  newSession(1002, channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 1023}),
		// A different area on the same channel is the case that cannot click the
		// leader's actor; the channel type stays the team one.
		applicant: newSession(1003, channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 1023}),
	}
	service.raids[fixture.raidKey] = &runtimeRaidState{
		RaidKey:   fixture.raidKey,
		LeaderID:  1001,
		TailFlag:  0,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: runtimeRaidMemberUserStateOnline, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, GroupIndex: 1, SlotOrder: 1, UserState: runtimeRaidMemberUserStateOnline, HPPercent: 100, MPPercent: 100},
		},
	}
	return fixture
}

// runtimeRaidJoinRequestBody builds C2S 820 the way the client's sub_17B1530
// does: byte 2 of the raid key as the u8, key bits 0..15 as the u16, then the
// client's own u32 scratch value and the raid type.
func runtimeRaidJoinRequestBody(raidKey uint32, clientValue uint32) []byte {
	body := make([]byte, 9)
	body[0] = byte(raidKey >> 16)
	binary.LittleEndian.PutUint16(body[1:3], uint16(raidKey))
	binary.LittleEndian.PutUint32(body[3:7], clientValue)
	binary.LittleEndian.PutUint16(body[7:9], 955)
	return body
}

func (f runtimeRaidJoinFixture) apply(t *testing.T) {
	t.Helper()
	f.leader.conn.(*bufferConn).write.Reset()
	f.member.conn.(*bufferConn).write.Reset()
	f.applicant.conn.(*bufferConn).write.Reset()
	handled, err := f.service.handleOnlineRaidCommand(
		f.applicant,
		uint16(dnfenum.CmdPacketRaidOtherChannelRequestJoin),
		runtimeRaidJoinRequestBody(f.raidKey, 0),
	)
	if err != nil {
		t.Fatalf("820 application: %v", err)
	}
	if !handled {
		t.Fatal("820 was not consumed by the raid handler")
	}
}

func TestRuntimeRaidOtherChannelRequestJoinPromptsLeaderAndAdmits(t *testing.T) {
	fixture := newRuntimeRaidJoinFixture(t)
	fixture.apply(t)

	// The leader is prompted through the same op7 the actor-click path uses.
	assertRuntimeRaidPeerPrompt(t, runtimeRaidPeerTake(t, fixture.leader), 1003)
	// A non-leader member must not be prompted.
	if packets := runtimeRaidPeerTake(t, fixture.member); func() bool {
		_, ok := runtimeRaidPeerFind(packets, 0x0007)
		return ok
	}() {
		t.Fatal("a non-leader member received the admission prompt")
	}
	// The applicant receives nothing yet: the binding ACK only follows acceptance.
	if packets := runtimeRaidPeerTake(t, fixture.applicant); len(packets) != 0 {
		t.Fatalf("applicant received %d packets before the leader answered", len(packets))
	}

	if err := fixture.service.handleOnlineResponsePeer(
		fixture.leader,
		uint16(dnfenum.CmdPacketResponsePeer),
		runtimeRaidPeerResponseBody(1003, false),
	); err != nil {
		t.Fatalf("leader accept: %v", err)
	}

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1003) < 0 {
		t.Fatalf("applicant %d was not admitted: %+v", 1003, state.Members)
	}
	packets := runtimeRaidPeerTake(t, fixture.applicant)
	if _, ok := runtimeRaidPeerFind(packets, uint16(dnfenum.CmdPacketCreateRaid)); !ok {
		t.Fatal("applicant received no 664 binding ACK")
	}
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("applicant received no roster refresh")
	}
}

func TestRuntimeRaidOtherChannelRequestJoinRejectsUnknownRaid(t *testing.T) {
	fixture := newRuntimeRaidJoinFixture(t)
	fixture.applicant.conn.(*bufferConn).write.Reset()

	missing := uint32(24)<<16 | 7
	handled, err := fixture.service.handleOnlineRaidCommand(
		fixture.applicant,
		uint16(dnfenum.CmdPacketRaidOtherChannelRequestJoin),
		runtimeRaidJoinRequestBody(missing, 0),
	)
	if err != nil || !handled {
		t.Fatalf("820 for an unknown raid: handled=%t err=%v", handled, err)
	}
	packets := runtimeRaidPeerTake(t, fixture.applicant)
	join, ok := runtimeRaidPeerFind(packets, raid.RaidOtherChannelResponseJoinMsgID)
	if !ok {
		t.Fatal("no op821 refusal for an unknown raid")
	}
	// sub_1D0F150 ignores a non-zero result and renders nothing for any reason
	// outside {0x13, 0x3C, 0xBF, 0xC0}, so both bytes are load bearing.
	if len(join.Body) != 2 || join.Body[0] != 0x00 || join.Body[1] != runtimeRaidJoinReasonGeneric {
		t.Fatalf("refusal body=%x want 00 %02X", join.Body, runtimeRaidJoinReasonGeneric)
	}
	if _, prompted := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.leader), 0x0007); prompted {
		t.Fatal("leader was prompted for a raid that does not exist")
	}
}

func TestRuntimeRaidOtherChannelRequestJoinRejectsClosedRaid(t *testing.T) {
	fixture := newRuntimeRaidJoinFixture(t)
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].StartPending = true
	fixture.service.raidMu.Unlock()

	fixture.apply(t)

	packets := runtimeRaidPeerTake(t, fixture.applicant)
	join, ok := runtimeRaidPeerFind(packets, raid.RaidOtherChannelResponseJoinMsgID)
	if !ok || len(join.Body) != 2 || join.Body[0] != 0x00 {
		t.Fatalf("no refusal for a raid that already started: %x", join.Body)
	}
	if _, prompted := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.leader), 0x0007); prompted {
		t.Fatal("leader was prompted after the raid left the recruiting state")
	}
}

// The extraction of recordRuntimeRaidPeerAdmission exists for this case: the
// applicant is browsing from an ordinary channel, where the mode-10 actor-click
// path would have been refused by its team-channel gate.
func TestRuntimeRaidOtherChannelRequestJoinWorksOutsideATeamChannel(t *testing.T) {
	fixture := newRuntimeRaidJoinFixture(t)
	ordinary := channelcatalog.Channel{ID: 171, Type: 11, Name: "ch171", Port: 10171}
	fixture.applicant.channel = ordinary
	if runtimeRaidModeForSession(fixture.applicant) != 0 {
		t.Fatal("fixture channel is not an ordinary channel")
	}

	fixture.apply(t)

	assertRuntimeRaidPeerPrompt(t, runtimeRaidPeerTake(t, fixture.leader), 1003)
}

func (f runtimeRaidJoinFixture) state(t *testing.T) runtimeRaidState {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	state := f.service.raids[f.raidKey]
	if state == nil {
		return runtimeRaidState{}
	}
	return cloneRuntimeRaidState(*state)
}
