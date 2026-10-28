package dnfbridge

import (
	"bytes"
	"testing"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// The raid leader's own raid-channel speech travels as the announcement mode (56),
// and as exactly ONE packet: mode 56 already prints the line and the overhead bubble
// as well as the banner, so sending the ordinary raid line (55) alongside it made
// every message appear twice in the client's chat window.
//
// Live evidence for the doubling: 3 messages produced 6 identical chat lines, and the
// server logged the mode-55 and the mode-56 broadcast on the same microsecond with
// one recipient each. Live evidence for the surface: the banner drew "通知 | 横幅测试"
// from the raw GBK bytes.
func TestRaidLeaderSpeechIsSentOnceOnTheAnnouncementMode(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	leader := fixture.sessions[1001]
	member := fixture.sessions[1002]
	leader.conn.(*bufferConn).write.Reset()
	member.conn.(*bufferConn).write.Reset()

	if err := fixture.service.handleCurrentScopedTownChat(leader, currentTownChatRequest{
		Mode:    currentTownChatRaidMode,
		Message: []byte("集合"),
	}); err != nil {
		t.Fatalf("leader raid chat: %v", err)
	}

	// One packet per recipient, not two.
	leaderPackets := runtimeRaidPeerTake(t, leader)
	if len(leaderPackets) != 1 {
		t.Fatalf("leader received %d packets, want exactly 1: %+v", len(leaderPackets), leaderPackets)
	}
	memberPackets := runtimeRaidPeerTake(t, member)
	if len(memberPackets) != 1 {
		t.Fatalf("member received %d packets, want exactly 1: %+v", len(memberPackets), memberPackets)
	}

	// The banner mode is what selects the surface; the actor key is what lets the
	// receiver resolve the speaker, and the text stays the client's own GBK bytes.
	want := buildCurrentTownChatNotification(currentRaidAnnouncementMode, 1001, []byte("集合"))
	for name, packet := range map[string]dnfproto.ChannelPacket{
		"leader": leaderPackets[0],
		"member": memberPackets[0],
	} {
		if packet.Header.MsgID != currentTownChatNotificationMsgID {
			t.Fatalf("%s msg=%#04x want %#04x", name, packet.Header.MsgID, currentTownChatNotificationMsgID)
		}
		if packet.Body[0] != currentRaidAnnouncementMode {
			t.Fatalf("%s mode=%d want %d", name, packet.Body[0], currentRaidAnnouncementMode)
		}
		if !bytes.Equal(packet.Body, want) {
			t.Fatalf("%s body=% X want % X", name, packet.Body, want)
		}
	}
}

// A squad leader or an ordinary member keeps the plain raid line: same single packet,
// mode 55, no banner.
func TestRaidMemberSpeechKeepsThePlainRaidLine(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	leader := fixture.sessions[1001]
	member := fixture.sessions[1002]
	leader.conn.(*bufferConn).write.Reset()
	member.conn.(*bufferConn).write.Reset()

	if err := fixture.service.handleCurrentScopedTownChat(member, currentTownChatRequest{
		Mode:    currentTownChatRaidMode,
		Message: []byte("集合"),
	}); err != nil {
		t.Fatalf("member raid chat: %v", err)
	}

	leaderPackets := runtimeRaidPeerTake(t, leader)
	if len(leaderPackets) != 1 {
		t.Fatalf("leader received %d packets, want exactly 1", len(leaderPackets))
	}
	want := buildCurrentTownChatNotification(currentTownChatRaidMode, 1002, []byte("集合"))
	if !bytes.Equal(leaderPackets[0].Body, want) {
		t.Fatalf("member line body=% X want % X", leaderPackets[0].Body, want)
	}
	if leaderPackets[0].Body[0] == currentRaidAnnouncementMode {
		t.Fatal("a non-leader member raised the announcement mode")
	}
	runtimeRaidPeerTake(t, member)
}

// A non-raid chat mode never reaches the banner path, even for the raid leader.
func TestRaidAnnouncementIgnoresOtherChatModes(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	leader := fixture.sessions[1001]
	leader.conn.(*bufferConn).write.Reset()

	mode, raidKey := fixture.service.currentRaidAnnouncementModeFor(leader, currentTownChatAreaMode)
	if raidKey != 0 || mode != currentTownChatAreaMode {
		t.Fatalf("area chat promoted: mode=%d raid_key=%d", mode, raidKey)
	}

	if err := fixture.service.handleCurrentScopedTownChat(leader, currentTownChatRequest{
		Mode:    currentTownChatAreaMode,
		Message: []byte("区域喊话"),
	}); err != nil {
		t.Fatalf("area chat: %v", err)
	}
	packets := runtimeRaidPeerTake(t, leader)
	if len(packets) != 1 {
		t.Fatalf("area chat wrote %d packets, want 1", len(packets))
	}
	if packets[0].Body[0] != currentTownChatAreaMode {
		t.Fatalf("area chat mode=%d want %d", packets[0].Body[0], currentTownChatAreaMode)
	}
}

// The banner body carries the client's own bytes untouched: no GB18030 -> UTF-8 or
// UTF-16LE re-encoding. The temporary "B "/"C " probe prefixes are gone with it, so a
// leader whose message happens to start with that text keeps it verbatim.
func TestRaidAnnouncementPassesTheMessageBytesThrough(t *testing.T) {
	message := []byte("B \xd6\xd0\xce\xc4")
	body := buildCurrentRaidAnnouncementNotification(1001, message)
	want := buildCurrentTownChatNotification(currentRaidAnnouncementMode, 1001, message)
	if !bytes.Equal(body, want) {
		t.Fatalf("banner body=% X want % X", body, want)
	}
	if !bytes.HasSuffix(body, message) {
		t.Fatalf("banner body=% X lost the raw tail % X", body, message)
	}
}
