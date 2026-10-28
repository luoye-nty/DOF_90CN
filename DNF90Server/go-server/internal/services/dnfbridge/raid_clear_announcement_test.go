package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
)

// C2S 666 is answered with the client's own refusal surface: body {u8 result,
// u8 message code}, where a zero result makes sub_1D04620 look the code up in its
// table and open the announcement window (owner 0x298) with that string. Codes
// 0x11 and 0xD9 are followed by a u32 count and count u32 member ids.
func TestRaidStartFailureUsesTheClientsOwnMessageCodes(t *testing.T) {
	conn := &bufferConn{}
	session := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 23, Type: 23}}
	service := &Service{}

	if err := service.sendRaidStartFailure(session, 666, runtimeRaidStartFailureMinimumRoster, nil); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 || !bytes.Equal(packet.Body, []byte{0, 0x16}) {
		t.Fatalf("minimum-roster refusal body=% X rest=%d", packet.Body, len(rest))
	}

	// The entry-cost message names the members the client's string is about.
	conn.write.Reset()
	if err := service.sendRaidStartFailure(session, 666, runtimeRaidStartFailureEntryCost, []uint16{2, 7}); err != nil {
		t.Fatal(err)
	}
	packet, rest = splitGameServerUpperPacket(t, conn.write.Bytes())
	want := []byte{0, 0x11, 2, 0, 0, 0, 2, 0, 0, 0, 7, 0, 0, 0}
	if len(rest) != 0 || !bytes.Equal(packet.Body, want) {
		t.Fatalf("entry-cost refusal body=% X want=% X rest=%d", packet.Body, want, len(rest))
	}

	// A code the client does not read a list for must not carry one.
	conn.write.Reset()
	if err := service.sendRaidStartFailure(session, 666, runtimeRaidStartFailureNotLeader, []uint16{2}); err != nil {
		t.Fatal(err)
	}
	packet, _ = splitGameServerUpperPacket(t, conn.write.Bytes())
	if !bytes.Equal(packet.Body, []byte{0, 0x08}) {
		t.Fatalf("not-leader refusal body=% X", packet.Body)
	}
}

// The clear announcement is the client's own 0x0272 line: u32 mode, dstr raid
// name, dstr leader name, u8 solo. The client formats string 60454 with the two
// arguments and draws it in orange.
func TestRaidClearAnnouncementBodyLayout(t *testing.T) {
	raidName := []byte("我的攻坚队")
	leaderName := []byte("团长")
	body, err := raid.BuildRaidClearAnnouncementBody(raid.RaidClearAnnouncement{
		Kind:       raid.RaidClearKindAnton,
		RaidName:   raidName,
		LeaderName: leaderName,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 0}
	want = binary.LittleEndian.AppendUint32(want, uint32(len(raidName)))
	want = append(want, raidName...)
	want = binary.LittleEndian.AppendUint32(want, uint32(len(leaderName)))
	want = append(want, leaderName...)
	want = append(want, 0)
	if !bytes.Equal(body, want) {
		t.Fatalf("team announcement body=% X want=% X", body, want)
	}

	solo, err := raid.BuildRaidClearAnnouncementBody(raid.RaidClearAnnouncement{
		Kind:       raid.RaidClearKindAnton,
		RaidName:   nil,
		LeaderName: []byte("独行"),
		Solo:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if solo[len(solo)-1] != 1 {
		t.Fatalf("solo announcement body=% X", solo)
	}

	// The reader falls back to an empty string past 0x100 bytes instead of
	// truncating, so an oversized argument has to be refused here.
	long := bytes.Repeat([]byte("a"), 0x101)
	if _, err := raid.BuildRaidClearAnnouncementBody(raid.RaidClearAnnouncement{LeaderName: long}); err == nil {
		t.Fatal("an over-long leader name was accepted")
	}
}

// The announcement goes to every session on the raid's channel, and only for the
// phase that actually ends the raid.
func TestRuntimeRaidClearAnnouncementReachesTheChannelOnTheFinalPhase(t *testing.T) {
	channel := channelcatalog.Channel{ID: 23, Type: 23}
	newSession := func(character uint16) (*gameSession, *bufferConn) {
		conn := &bufferConn{}
		return &gameSession{conn: conn, channel: channel, selectedCharacterID: character}, conn
	}
	leader, leaderConn := newSession(1001)
	other, otherConn := newSession(1002)
	outsider, outsiderConn := newSession(1003)
	outsider.channel = channelcatalog.Channel{ID: 24, Type: 23}

	service := &Service{
		gameSessions:  map[uint16]*gameSession{1001: leader, 1002: other, 1003: outsider},
		onlinePlayers: newOnlinePlayerManager(),
	}
	state := runtimeRaidState{
		RaidKey:   uint32(23)<<16 | 1001,
		LeaderID:  1001,
		NameBytes: []byte("我的攻坚队"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "团长", GroupIndex: 1},
			{CharID: 1002, Name: "队员", GroupIndex: 1},
		},
		Lifecycle: runtimeRaidLifecycleState{Status: runtimeRaidLifecyclePhaseActive, Phase: runtimeRaidDungeonPhaseTwo},
	}

	if !runtimeRaidClearedByPhase(state, runtimeRaidDungeonPhaseTwo) {
		t.Fatal("phase two of a team raid is not treated as the clear")
	}
	if runtimeRaidClearedByPhase(state, runtimeRaidDungeonPhaseOne) {
		t.Fatal("phase one of a team raid is treated as the clear")
	}

	service.announceRuntimeRaidClearIfNeeded(state, runtimeRaidDungeonPhaseTwo)
	packet, rest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if packet.Header.MsgID != raid.RaidClearAnnouncementMsgID || len(rest) != 0 {
		t.Fatalf("announcement packet=%+v rest=%d", packet.Header, len(rest))
	}
	want, err := raid.BuildRaidClearAnnouncementBody(raid.RaidClearAnnouncement{
		Kind:       raid.RaidClearKindAnton,
		RaidName:   []byte("我的攻坚队"),
		LeaderName: rosterNameBytes("团长"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("announcement body=% X want=% X", packet.Body, want)
	}
	if otherConn.write.Len() == 0 {
		t.Fatal("a member on the same channel received no announcement")
	}
	if outsiderConn.write.Len() != 0 {
		t.Fatal("the announcement crossed into another channel")
	}

	// The settlement moves the phase off PhaseActive, so it can never fire twice.
	leaderConn.write.Reset()
	settled := state
	settled.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	service.announceRuntimeRaidClearIfNeeded(settled, runtimeRaidDungeonPhaseTwo)
	if leaderConn.write.Len() != 0 {
		t.Fatal("a settled raid announced its clear a second time")
	}

	// Phase one of a team raid is a phase break, not a clear.
	leaderConn.write.Reset()
	service.announceRuntimeRaidClearIfNeeded(state, runtimeRaidDungeonPhaseOne)
	if leaderConn.write.Len() != 0 {
		t.Fatal("the phase-one break announced a raid clear")
	}
}

// The current client's Fiend War branch (mode 2) has only the team sentence.
// There is no solo Fiend War string: solo=1 on mode 2 leaves the formatted
// wide-string buffer untouched and renders one garbage character. Single
// Fiend War must therefore use the valid mode-2 team sentence.
func TestRuntimeRaidFiendWarSingleClearAnnouncementUsesTheSpiralTeamSentence(t *testing.T) {
	channel := channelcatalog.Channel{ID: 23, Type: 42}
	conn := &bufferConn{}
	session := &gameSession{
		conn: conn, channel: channel, selectedCharacterID: 1001,
	}
	service := &Service{
		gameSessions:  map[uint16]*gameSession{1001: session},
		onlinePlayers: newOnlinePlayerManager(),
	}
	state := runtimeRaidState{
		RaidKey:   uint32(23)<<16 | 1001,
		RaidMode:  runtimeRaidOwnerFiendWar,
		TailFlag:  1,
		LeaderID:  1001,
		NameBytes: []byte("单人超时空"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "独行", GroupIndex: 1},
		},
	}

	if err := service.broadcastRuntimeRaidClearAnnouncement(state); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidClearAnnouncementMsgID || len(rest) != 0 {
		t.Fatalf("announcement packet=%+v rest=%d", packet.Header, len(rest))
	}
	want, err := raid.BuildRaidClearAnnouncementBody(raid.RaidClearAnnouncement{
		Kind:       raid.RaidClearKindSpiral,
		RaidName:   []byte("单人超时空"),
		LeaderName: rosterNameBytes("独行"),
		Solo:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("single Fiend War announcement body=% X want=% X", packet.Body, want)
	}
	if len(packet.Body) == 0 || packet.Body[len(packet.Body)-1] != 0 {
		t.Fatalf("single Fiend War announcement used the unsupported solo flag: % X", packet.Body)
	}
}
