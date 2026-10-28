package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// The live 2026-09-20 incident behind runtimePartySelectionTargetActive: a
// two-member party lost one member, the survivor kept a one-member aggregate,
// and the town interaction menu then offered 加入 instead of 邀请 for him. He
// could neither be grouped with nor leave the party his own client never drew.
// These tests pin both halves of the click answer the menu reads: the op9 party
// link projected to the clicking client and the mode-15 selection marker.

// newPeerSelectionGhostPartyFixture returns the service plus a two-member party
// (leader, leftover) and an outsider standing beside them, all in one town area.
func newPeerSelectionGhostPartyFixture(t *testing.T) (*Service, *gameSession, *gameSession, *gameSession) {
	t.Helper()
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 23, Type: 1, Name: "ch.23", Port: 10023}
	sessions := make([]*gameSession, 0, 3)
	for index, characterID := range []uint16{99, 100, 101} {
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "peer-selection-ghost",
			channel:             channel,
			residentChannel:     channel,
			selectedCharacterID: characterID,
		}
		service.bindGameSessionCharacter(session, characterID)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: characterID,
			ChannelID:   uint16(channel.ID),
			TownID:      19,
			AreaID:      1,
			PositionX:   uint16(400 + index),
			PositionY:   300,
			Session:     session,
		})
		sessions = append(sessions, session)
	}
	leader, leftover, outsider := sessions[0], sessions[1], sessions[2]
	if _, result, joined := service.createOrJoinManagedRuntimeParty(leftover, leader); !joined || !result.OK {
		t.Fatalf("managed party join=%t result=%+v", joined, result)
	}
	state := runtimePartyStateSnapshot(leader)
	if members := runtimePartyMembers(state); len(members) != 2 {
		t.Fatalf("fixture party has %d members, want 2: %+v", len(members), state)
	}
	return service, leader, leftover, outsider
}

// peerSelectionMode15RequestBody is the current EXE's town-click request shape:
// u16 target + u8 mode + u32 value0 + u16 value1 + u32 value2.
func peerSelectionMode15RequestBody(t *testing.T, targetID uint16) []byte {
	t.Helper()
	body := make([]byte, 13)
	binary.LittleEndian.PutUint16(body[0:2], targetID)
	body[2] = 15
	return body
}

// clickTownPeerForSelection runs one mode-15 click and returns the party marker
// the server answered with plus every packet the clicker received.
func clickTownPeerForSelection(
	t *testing.T,
	service *Service,
	clicker *gameSession,
	targetID uint16,
) (uint16, []dnfproto.ChannelPacket) {
	t.Helper()
	clicker.conn.(*bufferConn).write.Reset()
	if err := service.handleOnlineRequestPeer(
		clicker,
		uint16(dnfenum.CmdPacketRequestPeer),
		peerSelectionMode15RequestBody(t, targetID),
	); err != nil {
		t.Fatalf("mode-15 click on %d: %v", targetID, err)
	}
	packets := runtimeRaidPeerTake(t, clicker)
	notice, ok := runtimeRaidPeerFind(packets, 0x0007)
	if !ok {
		t.Fatalf("click on %d produced no op7 selection notice (%d packets)", targetID, len(packets))
	}
	if len(notice.Body) != 9 || notice.Body[2] != 15 {
		t.Fatalf("selection notice body=% X, want target:u16/mode:u8/value0:u32/marker:u16", notice.Body)
	}
	return binary.LittleEndian.Uint16(notice.Body[7:9]), packets
}

// peerSelectionOp9Record decodes the single-record op9 party frame the clicking
// client received: the pair of (object key, record kind) its menu link is built
// from.
func peerSelectionOp9Record(t *testing.T, packets []dnfproto.ChannelPacket) (uint16, byte) {
	t.Helper()
	for _, packet := range packets {
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) {
			continue
		}
		if len(packet.Body) < 10 {
			t.Fatalf("op9 body=% X is too short for a record header", packet.Body)
		}
		if count := binary.LittleEndian.Uint16(packet.Body[0:2]); count != 1 {
			t.Fatalf("op9 record count=%d, want the single-record form", count)
		}
		return binary.LittleEndian.Uint16(packet.Body[4:6]), packet.Body[9]
	}
	t.Fatal("the clicking client received no op9 party frame")
	return 0, 0
}

// TestTownPeerSelectionOffersInviteForAOneMemberParty is the regression for the
// player nobody could group with.
//
// A two-member party must keep reading as joinable, and a one-member party -- a
// leader still alone in its lobby, which is the only way one exists since the
// single-member disband rule -- must read to every other player exactly like a
// partyless character: the op9 clearing record addressed to the party model the
// clicker was already given, plus the 0xffff marker that clears the selected
// actor's party-present flag before the menu opens. Anything else removes 邀请
// from the menu, and that player becomes unreachable.
func TestTownPeerSelectionOffersInviteForAOneMemberParty(t *testing.T) {
	service, leader, leftover, outsider := newPeerSelectionGhostPartyFixture(t)
	partyID := runtimePartyStateSnapshot(leader).PartyID

	// Positive control: while the party really has two members the menu must
	// still offer join, exactly as it did in the live incident for the removal
	// of the second member.
	marker, packets := clickTownPeerForSelection(t, service, outsider, leftover.selectedCharacterID)
	if marker != 0 {
		t.Fatalf("a two-member party answered marker=0x%04x, want 0x0000", marker)
	}
	key, kind := peerSelectionOp9Record(t, packets)
	if kind != currentSceneOp9ActorDisplayKind || key != uint16(partyID) {
		t.Fatalf("two-member projection kind=%d key=%d, want kind=%d key=%d",
			kind, key, currentSceneOp9ActorDisplayKind, partyID)
	}

	// The teammate abandons its run. Since 2026-09-22 「一个人不成队」 that drops
	// the party to a single member, so the member left behind is detached as well
	// instead of keeping the aggregate the live incident was reported against.
	state := runtimePartyStateSnapshot(leader)
	if err := service.leaveRuntimePartyOnAbandonedDungeonExit(leader, state, "test_ghost_party"); err != nil {
		t.Fatalf("teammate leave: %v", err)
	}
	if after := runtimePartyStateSnapshot(leftover); after.PartyID != 0 || len(runtimePartyMembers(after)) != 0 {
		t.Fatalf("the leftover member kept a one-member aggregate: %+v", after)
	}
	if runtimePartySelectionTargetActive(runtimePartyStateSnapshot(leftover)) {
		t.Fatal("a partyless member still reads as a party other players can join")
	}

	// A one-member party that legitimately exists is a leader waiting alone in
	// its own lobby. It has to stay joinable.
	leftoverIdentity, bound := service.boundGameSessionCharacterSnapshot(leftover)
	if !bound {
		t.Fatal("the leftover member lost its session binding")
	}
	leftoverMember, ok := service.runtimePartyMemberForIdentity(leftoverIdentity)
	if !ok {
		t.Fatal("the leftover member has no party identity")
	}
	lobby := service.runtimePartyManagerForService().EnsureLeader(
		leftoverMember, alignedcmd.PartyState{MaxMembers: 4})
	if !lobby.OK {
		t.Fatalf("one-member lobby = %+v", lobby)
	}
	lobbyState := service.publishRuntimePartySnapshot(lobby.Party)
	if len(runtimePartyMembers(lobbyState)) != 1 {
		t.Fatalf("one-member lobby projection = %+v", lobbyState)
	}

	marker, packets = clickTownPeerForSelection(t, service, outsider, leftover.selectedCharacterID)
	if marker != 0xffff {
		t.Fatalf("a one-member party answered marker=0x%04x, want 0xffff so the menu offers invite", marker)
	}
	key, kind = peerSelectionOp9Record(t, packets)
	if kind != currentSceneOp9ActorRemoveKind {
		t.Fatalf("one-member projection kind=%d, want the clearing kind %d",
			kind, currentSceneOp9ActorRemoveKind)
	}
	// The clearing record still has to be addressed somewhere the clicker's
	// client accepts. The dissolve above already published one clearing record to
	// this same-area clicker and consumed its remembered party-object identity
	// (takeRuntimePartyFrameIdentity), so this click takes the documented
	// actor-key fallback instead of repeating the old party model -- the client
	// holds no party object for the target any more either way. See
	// sendCurrentPartyActorFrameProjectionBefore.
	if key != currentSceneActorObjectKey(leftover.selectedCharacterID) {
		t.Fatalf("clearing record key=%d, want the actor key %d after the memo was consumed",
			key, currentSceneActorObjectKey(leftover.selectedCharacterID))
	}

	// The click answer must not have rewritten the party model itself: the lobby
	// leader is still where the manager put him.
	if final := runtimePartyStateSnapshot(leftover); final.PartyID != lobbyState.PartyID ||
		len(runtimePartyMembers(final)) != 1 {
		t.Fatalf("the click answer changed the party model: before=%+v after=%+v", lobbyState, final)
	}
}

// TestRuntimePartySelectionTargetActiveIgnoresOneMemberParties states the
// predicate itself, so a later change to the party model cannot silently make
// the ghost joinable again.
func TestRuntimePartySelectionTargetActiveIgnoresOneMemberParties(t *testing.T) {
	notJoinable := []struct {
		name  string
		state alignedcmd.PartyState
	}{
		{name: "partyless", state: alignedcmd.PartyState{}},
		{
			name: "one member",
			state: alignedcmd.PartyState{
				PartyID: 23, UserID: 12,
				Members: []alignedcmd.PartyMemberState{{UserID: 12, Slot: 0}},
			},
		},
		{
			name: "party id without members",
			state: alignedcmd.PartyState{
				PartyID: 23,
			},
		},
	}
	for _, tc := range notJoinable {
		if runtimePartySelectionTargetActive(tc.state) {
			t.Fatalf("%s must not read as a party that can be joined", tc.name)
		}
	}
	if !runtimePartySelectionTargetActive(alignedcmd.PartyState{
		PartyID: 23, UserID: 12,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 12, Slot: 0},
			{UserID: 13, Slot: 1},
		},
	}) {
		t.Fatal("a two-member party must read as joinable")
	}
}
