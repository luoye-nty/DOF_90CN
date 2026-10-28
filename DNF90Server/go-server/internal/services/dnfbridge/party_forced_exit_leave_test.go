package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	channelcatalog "longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// newForcedExitLeaveTestParty builds a live managed party of the requested
// size and returns the service plus the member that will abandon its run.
func newForcedExitLeaveTestParty(t *testing.T, memberCount int) (*Service, *gameSession, []*gameSession) {
	t.Helper()
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 11, Type: 1, Name: "ch.11", Port: 10011}
	sessions := make([]*gameSession, 0, memberCount)
	for index := 0; index < memberCount; index++ {
		characterID := uint16(101 + index)
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "forced-exit-leave",
			channel:             channel,
			residentChannel:     channel,
			selectedCharacterID: characterID,
		}
		service.bindGameSessionCharacter(session, characterID)
		sessions = append(sessions, session)
	}
	for _, joiner := range sessions[1:] {
		if _, result, joined := service.createOrJoinManagedRuntimeParty(joiner, sessions[0]); !joined || !result.OK {
			t.Fatalf("managed party join=%t result=%+v", joined, result)
		}
	}
	return service, sessions[0], sessions
}

// TestAbandonedDungeonExitLeavesAPartyThatStillHasOtherMembers pins the
// operator's rule: force quitting a dungeon removes the quitter from the party
// while other members remain. A run admitted as a shared party dungeon is
// already covered by detachRuntimePartyDungeonMemberForTownReturn; this is the
// solo-entry half, where no runtimePartyDungeonOwner exists.
func TestAbandonedDungeonExitLeavesAPartyThatStillHasOtherMembers(t *testing.T) {
	service, quitter, sessions := newForcedExitLeaveTestParty(t, 2)
	survivor := sessions[1]
	state := runtimePartyStateSnapshot(quitter)
	if members := runtimePartyMembers(state); len(members) != 2 {
		t.Fatalf("party has %d members before the exit, want 2", len(members))
	}
	survivor.conn.(*bufferConn).write.Reset()

	if err := service.leaveRuntimePartyOnAbandonedDungeonExit(quitter, state, "test_abandoned_run"); err != nil {
		t.Fatalf("forced exit leave: %v", err)
	}

	if after := runtimePartyStateSnapshot(quitter); after.PartyID != 0 {
		t.Fatalf("the quitter is still projected into party %d", after.PartyID)
	}
	identity, bound := service.boundGameSessionCharacterSnapshot(quitter)
	if !bound {
		t.Fatal("the quitter lost its session binding")
	}
	if _, found := service.runtimePartyManagerForService().SnapshotByUser(identity.character, identity.generation); found {
		t.Fatal("the manager still holds the quitter in a party")
	}
	if survivor.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the remaining member was never told the quitter left")
	}
}

// TestAbandonedDungeonExitDisbandsTheSurvivingRoom pins the single-member
// disband rule (operator, 2026-09-22: 「一个人不成队」) for this path: the member
// left behind by a forced dungeon exit was itself the last member, so it is
// detached as well and its client cache is cleared. The native kind-2
// multiplayer-to-solo row is still published before that clear -- see
// publishRuntimePartyDisbandToSurvivors.
func TestAbandonedDungeonExitDisbandsTheSurvivingRoom(t *testing.T) {
	service, quitter, sessions := newForcedExitLeaveTestParty(t, 2)
	survivor := sessions[1]
	state := runtimePartyStateSnapshot(quitter)
	survivor.conn.(*bufferConn).write.Reset()
	if err := service.leaveRuntimePartyOnAbandonedDungeonExit(quitter, state, "test_first_exit"); err != nil {
		t.Fatalf("first forced exit leave: %v", err)
	}
	after := runtimePartyStateSnapshot(survivor)
	if after.PartyID != 0 || len(runtimePartyMembers(after)) != 0 {
		t.Fatalf("the survivor still projects party %d with %d members",
			after.PartyID, len(runtimePartyMembers(after)))
	}
	survivorIdentity, bound := service.boundGameSessionCharacterSnapshot(survivor)
	if !bound {
		t.Fatal("the survivor lost its session binding")
	}
	if snapshot, found := service.runtimePartyManagerForService().SnapshotByUser(
		survivorIdentity.character, survivorIdentity.generation,
	); found {
		t.Fatalf("the manager still holds the survivor in a party: %+v", snapshot)
	}
	packets := splitAllCurrentGameServerUpperPackets(t, survivor.conn.(*bufferConn).write.Bytes())
	if len(packets) == 0 {
		t.Fatal("the survivor was never told its party dissolved")
	}
	var soloRow, cleared bool
	for _, packet := range packets {
		if packet.Header.Classification != 0 {
			continue
		}
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketRecoverStamina):
			if len(packet.Body) >= 11 && packet.Body[9] == 2 && packet.Body[10] == 1 {
				soloRow = true
			}
			if len(packet.Body) == 10 && packet.Body[9] == currentSceneOp9ActorRemoveKind {
				cleared = true
			}
		}
	}
	if !soloRow || !cleared {
		t.Fatalf("survivor dissolve projection solo_row=%t cleared=%t packets=%d",
			soloRow, cleared, len(packets))
	}
}

// TestAbandonedDungeonExitKeepsASoleMemberInItsParty is the other half of the
// rule. Leaving a one-member party is indistinguishable from staying in it and
// would only churn the client's party window, so it is left alone.
func TestAbandonedDungeonExitKeepsASoleMemberInItsParty(t *testing.T) {
	service, sole, _ := newForcedExitLeaveTestParty(t, 1)
	sole.party.state = alignedcmd.PartyState{
		PartyID:    1,
		UserID:     101,
		IsLeader:   true,
		MaxMembers: 4,
		Members:    []alignedcmd.PartyMemberState{{UserID: 101, UserState: 1, HPPercent: 100, MPPercent: 100}},
	}
	if _, created := service.bootstrapManagedRuntimePartyForSession(sole); !created {
		t.Fatal("could not create the one-member party")
	}
	before := runtimePartyStateSnapshot(sole)
	if len(runtimePartyMembers(before)) != 1 {
		t.Fatalf("the sole member's party has %d members, want 1", len(runtimePartyMembers(before)))
	}
	sole.conn.(*bufferConn).write.Reset()

	if err := service.leaveRuntimePartyOnAbandonedDungeonExit(sole, before, "test_sole_member_exit"); err != nil {
		t.Fatalf("sole member forced exit leave: %v", err)
	}
	after := runtimePartyStateSnapshot(sole)
	if after.PartyID != before.PartyID || len(runtimePartyMembers(after)) != 1 {
		t.Fatalf("the sole member's party changed: before=%+v after=%+v", before, after)
	}
	if sole.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("the sole member was sent party packets it did not need")
	}
}

// TestAbandonedDungeonExitIsInertWithoutAParty keeps a solo run's force quit
// free of any party traffic.
func TestAbandonedDungeonExitIsInertWithoutAParty(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 11, Type: 1, Name: "ch.11", Port: 10011}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "forced-exit-solo",
		channel:             channel,
		residentChannel:     channel,
		selectedCharacterID: 101,
	}
	service.bindGameSessionCharacter(session, 101)

	if err := service.leaveRuntimePartyOnAbandonedDungeonExit(session, alignedcmd.PartyState{}, "test_solo_run"); err != nil {
		t.Fatalf("solo forced exit leave: %v", err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("a solo force quit produced party packets")
	}
}

// partyFrameOp9Record is one decoded op9 record header, in the field order the
// current EXE's sub_1D64CA0 reads it: u16 object key, u8, u8 owner channel, u8,
// u8 record kind.
type partyFrameOp9Record struct {
	ObjectKey    uint16
	OwnerChannel byte
	Kind         byte
}

// nextPartyFrameOp9Record pops packets off a connection's write buffer until it
// finds the next op9 frame and decodes its single record header.
func nextPartyFrameOp9Record(t *testing.T, buffered []byte) (partyFrameOp9Record, []byte) {
	t.Helper()
	rest := buffered
	for len(rest) > 0 {
		packet, remainder := splitGameServerUpperPacket(t, rest)
		rest = remainder
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) {
			continue
		}
		if len(packet.Body) < 10 {
			t.Fatalf("op9 body is %d bytes, too short for a record header: %x", len(packet.Body), packet.Body)
		}
		if count := binary.LittleEndian.Uint16(packet.Body[0:2]); count != 1 {
			t.Fatalf("op9 record count=%d, want the single-record form", count)
		}
		if scene := binary.LittleEndian.Uint16(packet.Body[2:4]); scene != currentSceneOp9StableSceneValue {
			t.Fatalf("op9 scene value=%d, want %d", scene, currentSceneOp9StableSceneValue)
		}
		return partyFrameOp9Record{
			ObjectKey:    binary.LittleEndian.Uint16(packet.Body[4:6]),
			OwnerChannel: packet.Body[7],
			Kind:         packet.Body[9],
		}, rest
	}
	t.Fatal("no op9 party frame was sent")
	return partyFrameOp9Record{}, nil
}

// TestPartyFrameClearTargetsTheBoundPartyModelObject is the regression for the
// stale party window that survived a force quit.
//
// buildCurrentSceneOp9PartyInformationBodyInContext keys the populated kind-0
// record by the shared party model id, never by a selected character's actor
// key. The current EXE resolves every non-zero-kind record through
// sub_26A1200(ownerChannel, objectKey), whose lookup key is the pair of both
// arguments, and only the kind-3 branch stuffs 0xFFFF into all eight member uid
// slots of the object it resolved. Addressing the leaver's own scene actor
// therefore resolved a different object and cleared nothing: the party window
// kept every slot it had.
func TestPartyFrameClearTargetsTheBoundPartyModelObject(t *testing.T) {
	service, quitter, sessions := newForcedExitLeaveTestParty(t, 2)
	survivor := sessions[1]

	state := runtimePartyStateSnapshot(quitter)
	if state.PartyID <= 0 {
		t.Fatalf("the fixture party never formed: %+v", state)
	}
	modelKey := uint16(state.PartyID)
	for _, session := range sessions {
		ownerChannel := currentPartySceneOwnerContext(session)
		if _, err := service.sendCurrentPartyActorFrameProjection(
			session,
			quitter,
			state,
			"test_populated_frame",
		); err != nil {
			t.Fatalf("populated party frame: %v", err)
		}
		populated, _ := nextPartyFrameOp9Record(t, session.conn.(*bufferConn).write.Bytes())
		if populated.Kind != currentSceneOp9ActorDisplayKind ||
			populated.ObjectKey != modelKey ||
			populated.OwnerChannel != ownerChannel {
			t.Fatalf("populated frame record=%+v want kind=%d key=%d owner=%d",
				populated, currentSceneOp9ActorDisplayKind, modelKey, ownerChannel)
		}
		session.conn.(*bufferConn).write.Reset()
	}

	// The force quit itself still owes the survivor the roster reduction and
	// the quitter its own empty window. It runs BEFORE the clearing projection:
	// that projection is what the fan-out sends once the roster is gone, and a
	// clear must never be replayed at an observer the party still holds -- see
	// TestPartyFrameClearNeverDestroysTheObserversOwnLiveParty.
	if err := service.leaveRuntimePartyOnAbandonedDungeonExit(quitter, state, "test_forced_exit_clear"); err != nil {
		t.Fatalf("forced exit leave: %v", err)
	}
	if quitter.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the quitter received no party projection for its own exit")
	}
	if survivor.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the survivor was never told the roster shrank")
	}
	for _, session := range sessions {
		session.conn.(*bufferConn).write.Reset()
	}

	// With the roster gone, clearing the leaving actor's retained frame still
	// has to address the party object the populated record was keyed by.
	for _, session := range sessions {
		ownerChannel := currentPartySceneOwnerContext(session)
		if _, err := service.sendCurrentPartyActorFrameProjection(
			session,
			quitter,
			alignedcmd.PartyState{},
			"test_cleared_frame",
		); err != nil {
			t.Fatalf("cleared party frame: %v", err)
		}
		removal, _ := nextPartyFrameOp9Record(t, session.conn.(*bufferConn).write.Bytes())
		if removal.Kind != currentSceneOp9ActorRemoveKind {
			t.Fatalf("removal kind=%d, want %d", removal.Kind, currentSceneOp9ActorRemoveKind)
		}
		if removal.ObjectKey != modelKey {
			t.Fatalf("removal object key=%d, want the bound party model %d", removal.ObjectKey, modelKey)
		}
		if removal.OwnerChannel != ownerChannel {
			t.Fatalf("removal owner channel=%d, want the channel the populated frame used (%d)",
				removal.OwnerChannel, ownerChannel)
		}
		session.conn.(*bufferConn).write.Reset()
	}
}

// TestPartyFrameClearNeverDestroysTheObserversOwnLiveParty is the regression for
// the 2026-09-27 15:50 party that appeared to dissolve by itself.
//
// A three-member party loses a member (op3 EXIT / 小退 / 顶号 -- any teardown),
// the roster is reduced correctly under the leaving member's own generation, and
// the returning character then walks into the public town the survivors stand in.
// That co-presence projection carries his EMPTY party state, and the clearing
// record it builds is addressed to the pair remembered for him -- which is the
// SHARED party object the survivors are still using. Replaying it emptied both of
// their party windows ("队伍自动解散了") while the server still held the party.
//
// The clear is therefore skipped whenever the remembered object is the
// observer's own live party: that object was already reduced by the kind-2
// membership row the leave published, so nothing stale is left on it.
func TestPartyFrameClearNeverDestroysTheObserversOwnLiveParty(t *testing.T) {
	service, quitter, sessions := newForcedExitLeaveTestParty(t, 3)
	if len(sessions) != 3 {
		t.Fatalf("fixture sessions=%d, want 3", len(sessions))
	}
	state := runtimePartyStateSnapshot(quitter)
	if state.PartyID <= 0 {
		t.Fatalf("the fixture party never formed: %+v", state)
	}
	modelKey := uint16(state.PartyID)

	// The survivors hold a populated frame for the member who is about to leave,
	// which is what remembers the shared party object against him.
	for _, session := range sessions {
		if session == quitter {
			continue
		}
		session.conn.(*bufferConn).write.Reset()
		if _, err := service.sendCurrentPartyActorFrameProjection(
			session,
			quitter,
			state,
			"test_populated_frame",
		); err != nil {
			t.Fatalf("populated party frame: %v", err)
		}
		session.conn.(*bufferConn).write.Reset()
	}
	quitterID := quitter.selectedCharacterID
	// The member goes away -- op3 EXIT, 小退, 顶号 takeover or a dropped
	// transport all reach this same teardown.
	service.detachRuntimePartySession(quitter, "test_session_teardown")
	// 3 - 1 = 2 members left, so the single-member disband rule does not apply
	// and the survivors' own party object stays live.
	survivors := make([]*gameSession, 0, len(sessions)-1)
	for _, session := range sessions {
		if session == quitter {
			continue
		}
		if projected := runtimePartyStateSnapshot(session); projected.PartyID != int(modelKey) {
			t.Fatalf("survivor %d lost the party: %+v", session.selectedCharacterID, projected)
		}
		survivors = append(survivors, session)
	}
	if len(survivors) != 2 {
		t.Fatalf("survivors=%d, want 2", len(survivors))
	}

	// Each survivor still remembers the leaving actor's party object, and that
	// remembered pair is the object its own window is bound to -- the exact state
	// that produced the incident.
	for _, session := range survivors {
		ownerChannel := currentPartySceneOwnerContext(session)
		session.party.mu.Lock()
		remembered, bound := session.party.frameIdentities[quitterID]
		session.party.mu.Unlock()
		if !bound || remembered.modelKey != modelKey || remembered.ownerChannel != ownerChannel {
			t.Fatalf("survivor %d memo=%+v bound=%t, want {%d %d}",
				session.selectedCharacterID, remembered, bound, modelKey, ownerChannel)
		}
		// The returning character stands in the survivors' area with no party at
		// all. Nothing may be written: their own live party object is not stale.
		session.conn.(*bufferConn).write.Reset()
		if _, err := service.sendCurrentPartyActorFrameProjection(
			session,
			quitter,
			alignedcmd.PartyState{},
			"test_town_copresence_actor_initial_party_state",
		); err != nil {
			t.Fatalf("empty party projection: %v", err)
		}
		if payload := session.conn.(*bufferConn).write.Bytes(); len(payload) != 0 {
			t.Fatalf("observer %d was sent a clearing record for its own live party: %x",
				session.selectedCharacterID, payload)
		}
	}

	// Control: an observer that is NOT in that party still receives the clear, so
	// the guard above is a condition on this one object and not a blanket disable
	// of the forced-exit fix.
	bystander := &gameSession{
		conn:                &bufferConn{},
		connID:              "party-frame-bystander",
		channel:             quitter.channel,
		selectedCharacterID: 104,
	}
	service.bindGameSessionCharacter(bystander, 104)
	storeRuntimePartyFrameIdentity(bystander, quitterID, modelKey, currentPartySceneOwnerContext(bystander))
	if _, err := service.sendCurrentPartyActorFrameProjection(
		bystander,
		quitter,
		alignedcmd.PartyState{},
		"test_bystander_clear",
	); err != nil {
		t.Fatalf("bystander clear: %v", err)
	}
	removal, _ := nextPartyFrameOp9Record(t, bystander.conn.(*bufferConn).write.Bytes())
	if removal.Kind != currentSceneOp9ActorRemoveKind || removal.ObjectKey != modelKey {
		t.Fatalf("bystander removal=%+v, want kind %d key %d",
			removal, currentSceneOp9ActorRemoveKind, modelKey)
	}
}

// TestPartyFrameClearIdentityIsPerActorAndConsumedOnce keeps a stale pair from
// being replayed against a later, unrelated party object, and keeps one area
// peer's leave from clearing another peer's party window.
func TestPartyFrameClearIdentityIsPerActorAndConsumedOnce(t *testing.T) {
	service, session, _ := newForcedExitLeaveTestParty(t, 1)
	storeRuntimePartyFrameIdentity(session, 101, 7, 11)
	storeRuntimePartyFrameIdentity(session, 202, 9, 12)

	first, bound := takeRuntimePartyFrameIdentity(session, 101)
	if !bound || first.modelKey != 7 || first.ownerChannel != 11 {
		t.Fatalf("first take=%+v bound=%t, want {7,11} true", first, bound)
	}
	if _, bound := takeRuntimePartyFrameIdentity(session, 101); bound {
		t.Fatal("the party frame identity was handed out twice")
	}
	second, bound := takeRuntimePartyFrameIdentity(session, 202)
	if !bound || second.modelKey != 9 || second.ownerChannel != 12 {
		t.Fatalf("other actor take=%+v bound=%t, want {9,12} true", second, bound)
	}

	// Without a remembered pair the clear falls back to the actor key rather
	// than inventing one, which is what an unbound receiver has always been
	// sent.
	session.conn.(*bufferConn).write.Reset()
	if _, err := service.sendCurrentPartyFrameProjection(
		session,
		alignedcmd.PartyState{},
		"test_unbound_clear",
	); err != nil {
		t.Fatalf("unbound clear: %v", err)
	}
	removal, _ := nextPartyFrameOp9Record(t, session.conn.(*bufferConn).write.Bytes())
	if removal.Kind != currentSceneOp9ActorRemoveKind ||
		removal.ObjectKey != currentSceneActorObjectKey(session.selectedCharacterID) {
		t.Fatalf("unbound clear record=%+v want the actor key %d",
			removal, currentSceneActorObjectKey(session.selectedCharacterID))
	}
}
