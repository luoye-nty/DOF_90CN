package dnfbridge

import (
	"bytes"
	"encoding/hex"
	"testing"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
)

// The 1:1 rules below are the ones the 86 reference's self-test pins
// (A21OneToOneChatSelfTest.cs), restated on this client's wire:
//
//   - one line per reader, both naming the other party (that name is the window
//     key), and flagged differently: flag 0 is the line that arrived, flag 1 is the
//     reader's own line, which this client does not draw by itself;
//   - the speaker is named from this server's own record, never from the name or
//     id the request supplied;
//   - the peer of an unnamed request comes from the conversation the server
//     remembers;
//   - one window per pair, a fresh id after close, and a closed window's traffic
//     is inert.

func TestCurrentOneToOneChatLineFillsBothSlots(t *testing.T) {
	for _, mode := range []byte{currentTownChatOneToOneMode, currentTownChatPeerMenuMode} {
		t.Run(hex.EncodeToString([]byte{mode}), func(t *testing.T) {
			service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
			request := buildCurrentPrivateTownChatRequestForTest(mode, target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
			if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, request); err != nil {
				t.Fatal(err)
			}
			// The sender: ack, the window binding for its peer, then its own line.
			ack, sourceRest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
			if ack.Header.Classification != 1 || ack.Header.MsgID != currentPeerChatStateOpcode || !bytes.Equal(ack.Body, []byte{1}) {
				t.Fatalf("source ack header=%+v body=%x", ack.Header, ack.Body)
			}
			own, sourceRest := splitGameServerUpperPacket(t, sourceRest)
			// Both copies name the other party: that name is the window key, so the
			// sender's own copy names its peer (not itself) and only the flag differs.
			wantOwn := buildCurrentOneToOneChatStateBody(mode, currentChatIncomingLineFlag, rosterNameBytes("source1"), rosterNameBytes(currentSocialTestFriendName), []byte("hello"))
			if own.Header.Classification != 0 || own.Header.MsgID != currentTownChatOneToOneStateMsgID ||
				!bytes.Equal(own.Body, wantOwn) || len(sourceRest) != 0 {
				t.Fatalf("sender copy header=%+v body=%x want=%x rest=%x", own.Header, own.Body, wantOwn, sourceRest)
			}
			// The peer: the window binding for the sender, then the incoming line.
			line, targetRest := splitGameServerUpperPacket(t, targetConn.write.Bytes())
			wantIncoming := buildCurrentOneToOneChatStateBody(mode, currentChatIncomingLineFlag, rosterNameBytes("source1"), rosterNameBytes("source1"), []byte("hello"))
			if line.Header.Classification != 0 || line.Header.MsgID != currentTownChatOneToOneStateMsgID ||
				!bytes.Equal(line.Body, wantIncoming) || len(targetRest) != 0 {
				t.Fatalf("peer copy header=%+v body=%x want=%x rest=%x", line.Header, line.Body, wantIncoming, targetRest)
			}
		})
	}
}

// Live capture 2026-09-22 20:16:02 (packet_log.txt): the client whose window was
// opened by an *incoming* line sends `mode 0x24 | u16 0 | u32 0 | text | nameLen
// 0 | 0`. The parser rejected that shape outright, which is exactly why the
// peer's answers never reached anybody. The peer now comes from the conversation
// this server remembers.
func TestCurrentOneToOneChatReplyWithoutPeerResolvesThroughTheConversation(t *testing.T) {
	service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
	open := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
	if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, open); err != nil {
		t.Fatal(err)
	}
	sourceConn.write.Reset()
	targetConn.write.Reset()

	reply := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, 0, []byte("hi"), nil)
	if _, err := service.handleAlignedGameCommand(target, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, reply); err != nil {
		t.Fatal(err)
	}
	line, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	want := buildCurrentOneToOneChatStateBody(currentTownChatPeerMenuMode, currentChatIncomingLineFlag, rosterNameBytes(currentSocialTestFriendName), rosterNameBytes(currentSocialTestFriendName), []byte("hi"))
	if line.Header.Classification != 0 || line.Header.MsgID != currentTownChatOneToOneStateMsgID || !bytes.Equal(line.Body, want) || len(rest) != 0 {
		t.Fatalf("reply line header=%+v body=%x want=%x rest=%x", line.Header, line.Body, want, rest)
	}
	ack, replyRest := splitGameServerUpperPacket(t, targetConn.write.Bytes())
	if ack.Header.Classification != 1 || ack.Header.MsgID != currentPeerChatStateOpcode || !bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("replier ack header=%+v body=%x", ack.Header, ack.Body)
	}
	own, replyRest := splitGameServerUpperPacket(t, replyRest)
	wantOwn := buildCurrentOneToOneChatStateBody(currentTownChatPeerMenuMode, currentChatIncomingLineFlag, rosterNameBytes(currentSocialTestFriendName), rosterNameBytes("source1"), []byte("hi"))
	if own.Header.Classification != 0 || own.Header.MsgID != currentTownChatOneToOneStateMsgID ||
		!bytes.Equal(own.Body, wantOwn) || len(replyRest) != 0 {
		t.Fatalf("replier own copy header=%+v body=%x want=%x rest=%x", own.Header, own.Body, wantOwn, replyRest)
	}
}

func TestCurrentOneToOneChatRequestShapeAllowsAnUnnamedPeer(t *testing.T) {
	body := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, 0, []byte("hi"), nil)
	request, ok := parseCurrentTownChatRequest(body)
	if !ok || request.TargetUniqueID != 0 || len(request.TargetName) != 0 || !request.HasTrailing {
		t.Fatalf("unnamed 1:1 request parsed=%t request=%+v", ok, request)
	}
	// A typed whisper still needs its target: only the two 1:1 window modes may
	// leave the peer to the server.
	unnamed := buildCurrentPrivateTownChatRequestForTest(currentTownChatWhisperMode, 0, []byte("hi"), nil)
	if _, ok := parseCurrentTownChatRequest(unnamed); ok {
		t.Fatal("a whisper without a target must stay invalid")
	}
	if _, ok := parseCurrentTownChatRequest(body[:len(body)-1]); ok {
		t.Fatal("a private request without the trailing byte must stay invalid")
	}
}

func TestCurrentChatConversationRegistryFollowsTheReference(t *testing.T) {
	service, source, target, _, _ := newCurrentSocialPeerTest(t)
	conversation, created, dropped := service.openCurrentChatConversation(source, target)
	if conversation == nil || !created || len(dropped) != 0 {
		t.Fatalf("first open conversation=%+v created=%t dropped=%d", conversation, created, len(dropped))
	}
	firstID := conversation.id
	if firstID == 0 {
		t.Fatal("a conversation id must never be 0")
	}
	again, createdAgain, _ := service.openCurrentChatConversation(source, target)
	if createdAgain || again != conversation || again.id != firstID {
		t.Fatalf("a duplicate open must reuse the window: created=%t id=%d want=%d", createdAgain, again.id, firstID)
	}
	// This client has one window, so pointing it at another peer retires the old
	// conversation instead of leaving two live for the same session.
	other := &gameSession{conn: &bufferConn{}, selectedCharacterID: 7}
	moved, createdMoved, dropped := service.openCurrentChatConversation(source, other)
	if !createdMoved || moved == nil || moved.id == firstID || len(dropped) != 1 || dropped[0] != conversation {
		t.Fatalf("window move created=%t dropped=%d", createdMoved, len(dropped))
	}
	if _, _, ok := service.currentChatConversationForSession(target); ok {
		t.Fatal("the replaced peer must no longer own a conversation")
	}
	// Session end drops the window; the next one is a new conversation.
	closed := service.closeCurrentChatConversationsForSession(source)
	if len(closed) != 1 || closed[0] != moved {
		t.Fatalf("close returned %d conversations", len(closed))
	}
	if _, _, ok := service.currentChatConversationForSession(source); ok {
		t.Fatal("a closed conversation must not stay reachable")
	}
	reopened, _, _ := service.openCurrentChatConversation(source, target)
	if reopened.id == moved.id || reopened.id == firstID {
		t.Fatalf("a reopened window must get a fresh id, got %d", reopened.id)
	}
}

func TestCurrentOneToOneChatRejectsSelfAndOversizedLines(t *testing.T) {
	service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
	self := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, source.selectedCharacterID, []byte("hi"), rosterNameBytes("source1"))
	if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, self); err != nil {
		t.Fatal(err)
	}
	assertCurrentTownChatFailurePacket(t, sourceConn.write.Bytes())
	if targetConn.write.Len() != 0 {
		t.Fatalf("a self-addressed line reached the peer: %x", targetConn.write.Bytes())
	}
	sourceConn.write.Reset()

	// The reference's MaximumMessageBytes, which is also what the client's own
	// op65 reader can hold.
	tooLong := bytes.Repeat([]byte("a"), currentChatConversationMaxTextBytes+1)
	oversized := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, target.selectedCharacterID, tooLong, rosterNameBytes(currentSocialTestFriendName))
	if _, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, oversized); err != nil {
		t.Fatal(err)
	}
	assertCurrentTownChatFailurePacket(t, sourceConn.write.Bytes())
	if targetConn.write.Len() != 0 {
		t.Fatalf("an oversized line reached the peer: %x", targetConn.write.Bytes())
	}
}
