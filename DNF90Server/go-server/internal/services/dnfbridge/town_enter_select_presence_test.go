package dnfbridge

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// newSelectorPresenceFixture registers two characters in one public town area
// and returns the service plus both sessions. The first one is the selector
// opener.
func newSelectorPresenceFixture(t *testing.T) (*Service, *gameSession, *gameSession) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	characters := []dnfrepo.CharacterRecord{
		{CharacterID: "17", AccountID: "account-opener", Name: "opener", Job: "2", Level: 35, Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "23", AccountID: "account-peer", Name: "peer", Job: "5", Level: 48, Stats: map[string]int64{"grow_type": 2}},
	}
	for _, character := range characters {
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 11, Type: 1, Name: "ch.11", Port: 10011}
	opener := &gameSession{
		conn: &bufferConn{}, connID: "selector-opener", channel: channel, residentChannel: channel,
		selectedCharacterID: 17, accountID: "account-opener", townActorOwnerChannel: 11,
	}
	peer := &gameSession{
		conn: &bufferConn{}, connID: "selector-peer", channel: channel, residentChannel: channel,
		selectedCharacterID: 23, accountID: "account-peer", townActorOwnerChannel: 11,
	}
	service := &Service{
		options:       options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
		gameSessions:  make(map[uint16]*gameSession),
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	service.bindGameSessionCharacter(opener, 17)
	service.bindGameSessionCharacter(peer, 23)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 17, AccountID: "account-opener", Name: "opener", TownID: 40, AreaID: 3,
		PositionX: 640, PositionY: 240, Direction: 5, AreaState: 3, Session: opener,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 23, AccountID: "account-peer", Name: "peer", TownID: 40, AreaID: 3,
		PositionX: 300, PositionY: 400, Direction: 1, AreaState: 3, Session: peer,
	})
	opener.conn.(*bufferConn).write.Reset()
	peer.conn.(*bufferConn).write.Reset()
	return service, opener, peer
}

// countTownLeaveNotifications counts class0 op6 town-leave records in a write
// buffer.
func countTownLeaveNotifications(t *testing.T, buffered []byte) int {
	t.Helper()
	count := 0
	rest := buffered
	for len(rest) > 0 {
		packet, remainder := splitGameServerUpperPacket(t, rest)
		rest = remainder
		if packet.Header.MsgID == currentTownUserLeaveMsgID {
			count++
		}
	}
	return count
}

// TestDungeonSelectorRetiresTownPresenceAndDestroysTheActorForOutsiders pins
// the operator product decision: a character sitting on the dungeon-selection
// page is not visible in town. The external release does the opposite - the
// capture retires presence only at dungeon entry - so this behaviour is
// deliberate and is asserted rather than assumed.
func TestDungeonSelectorRetiresTownPresenceAndDestroysTheActorForOutsiders(t *testing.T) {
	service, opener, peer := newSelectorPresenceFixture(t)

	service.leaveTownPresenceForDungeonSelector(opener, "test_selector_open")

	if _, registered := service.onlinePlayers.PlayerForCharacter(17); registered {
		t.Fatal("the selector opener is still registered in its town area")
	}
	if got := countTownLeaveNotifications(t, peer.conn.(*bufferConn).write.Bytes()); got != 1 {
		t.Fatalf("the outsider received %d op6 town-leave records, want 1", got)
	}
	// The peer keeps its own registration; only the opener is retired.
	if _, registered := service.onlinePlayers.PlayerForCharacter(23); !registered {
		t.Fatal("the retirement evicted the wrong character")
	}
}

// TestDungeonSelectorRetirementWithholdsOp6FromPartyMembers keeps the page from
// disbanding the party that opened it: this client answers op6 on a party
// member with LEAVE_PARTY, so a teammate must never receive the destroy.
func TestDungeonSelectorRetirementWithholdsOp6FromPartyMembers(t *testing.T) {
	service, opener, peer := newSelectorPresenceFixture(t)
	partyState := alignedcmd.PartyState{
		PartyID:    9,
		UserID:     17,
		IsLeader:   true,
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 17, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 23, Slot: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(opener, partyState)
	storeRuntimePartyState(peer, partyState)

	service.leaveTownPresenceForDungeonSelector(opener, "test_selector_open_in_party")

	if _, registered := service.onlinePlayers.PlayerForCharacter(17); registered {
		t.Fatal("the selector opener is still registered in its town area")
	}
	if got := countTownLeaveNotifications(t, peer.conn.(*bufferConn).write.Bytes()); got != 0 {
		t.Fatalf("a party member received %d op6 town-leave records, want 0", got)
	}
}

// TestSelectorPresenceRestoreReRegistersTheRetiredRecord is the other half of
// the pair. Without it the selector is a one-way trip into permanent
// invisibility: the op132 selector return deliberately preserves the finalized
// town scene and never runs a presence restore of its own.
func TestSelectorPresenceRestoreReRegistersTheRetiredRecord(t *testing.T) {
	service, opener, peer := newSelectorPresenceFixture(t)
	service.leaveTownPresenceForDungeonSelector(opener, "test_selector_open")
	opener.conn.(*bufferConn).write.Reset()
	peer.conn.(*bufferConn).write.Reset()

	if !service.restoreTownPresenceAfterDungeonSelector(opener, "test_selector_return") {
		t.Fatal("the selector return did not restore the retired presence")
	}
	presence, registered := service.onlinePlayers.PlayerForCharacter(17)
	if !registered {
		t.Fatal("the selector return left the character unregistered")
	}
	if presence.Session != opener || presence.TownID != 40 || presence.AreaID != 3 ||
		presence.PositionX != 640 || presence.PositionY != 240 ||
		presence.Direction != 5 || presence.AreaState != 3 {
		t.Fatalf("restored presence=%+v, want the exact retired record", presence)
	}
	if !service.onlinePlayers.PeerInSameArea(17, 23) {
		t.Fatal("the restored actor is not co-present with its area peer")
	}
	// Co-presence is published in both directions, the same way a newcomer is.
	if opener.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the returning client received no peer actors")
	}
	if peer.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the area peer was never given the returning actor back")
	}
}

// TestSelectorPresenceRestoreIsInertWithoutARetirement keeps the restore from
// resurrecting an actor on a route that never retired one - most importantly
// the dungeon entry that is supposed to leave the character out of the roster.
func TestSelectorPresenceRestoreIsInertWithoutARetirement(t *testing.T) {
	service, opener, _ := newSelectorPresenceFixture(t)
	if service.restoreTownPresenceAfterDungeonSelector(opener, "test_no_retirement") {
		t.Fatal("the restore acted without a remembered retirement")
	}

	service.leaveTownPresenceForDungeonSelector(opener, "test_selector_open")
	// Dungeon entry consumes the memo instead of undoing it.
	service.leaveTownPresenceForDungeon(opener)
	if service.restoreTownPresenceAfterDungeonSelector(opener, "test_after_dungeon_entry") {
		t.Fatal("a dungeon entrant was pulled back into the town roster")
	}
	if _, registered := service.onlinePlayers.PlayerForCharacter(17); registered {
		t.Fatal("a dungeon entrant is registered in a town area")
	}
}

// TestSelectorPageRetiresAndRestoresTownPresenceAroundOp132 is the pairing
// itself, driven through the two real client requests: op15 opens the page and
// bodyless op132 closes it. If either half is missing the player is either
// still visible while browsing or permanently invisible after backing out.
func TestSelectorPageRetiresAndRestoresTownPresenceAroundOp132(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	service.onlinePlayers = newOnlinePlayerManager()
	service.bindGameSessionCharacter(session, 99)

	peer := &gameSession{
		conn: &bufferConn{}, connID: "selector-area-peer",
		channel: session.channel, residentChannel: session.residentChannel,
		selectedCharacterID: 123,
	}
	service.bindGameSessionCharacter(peer, 123)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 99, Name: "opener", TownID: 7, AreaID: 3,
		PositionX: 640, PositionY: 240, Direction: 6, AreaState: 3, Session: session,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 123, Name: "peer", TownID: 7, AreaID: 3,
		PositionX: 300, PositionY: 400, Direction: 1, AreaState: 3, Session: peer,
	})

	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, 99, 7, 3)
	session.townMu.Unlock()
	service.markCurrentTownSceneReady(session)
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(640, 240, 6, 987)); err != nil {
		t.Fatal(err)
	}
	peer.conn.(*bufferConn).write.Reset()

	if err := service.sendEnterSelectDungeonState(session, "test_selector_presence", false, true); err != nil {
		t.Fatal(err)
	}
	if !session.enterSelectDungeonContextSent {
		t.Fatal("the selector context was never committed, so the retirement never applied")
	}
	if _, registered := service.onlinePlayers.PlayerForCharacter(99); registered {
		t.Fatal("the selector opener is still visible in town")
	}
	if got := countTownLeaveNotifications(t, peer.conn.(*bufferConn).write.Bytes()); got != 1 {
		t.Fatalf("the area peer received %d op6 town-leave records, want 1", got)
	}

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	presence, registered := service.onlinePlayers.PlayerForCharacter(99)
	if !registered {
		t.Fatal("backing out of the selector left the character permanently invisible")
	}
	if presence.TownID != 7 || presence.AreaID != 3 ||
		presence.PositionX != 640 || presence.PositionY != 240 {
		t.Fatalf("restored presence=%+v, want the retired town 7 area 3 at 640,240", presence)
	}
	if !service.onlinePlayers.PeerInSameArea(99, 123) {
		t.Fatal("the returning player is not co-present with its area peer again")
	}
}

// TestSelectorPresenceRestoreSkipsAnAlreadyRegisteredCharacter keeps a route
// that re-registered on its own (SET_USER_AREA, prev-village, the completed
// dungeon return) from ending up with a duplicate actor.
func TestSelectorPresenceRestoreSkipsAnAlreadyRegisteredCharacter(t *testing.T) {
	service, opener, _ := newSelectorPresenceFixture(t)
	service.leaveTownPresenceForDungeonSelector(opener, "test_selector_open")
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 17, TownID: 41, AreaID: 0, PositionX: 10, PositionY: 20, Session: opener,
	})

	if service.restoreTownPresenceAfterDungeonSelector(opener, "test_already_registered") {
		t.Fatal("the restore duplicated a live registration")
	}
	presence, registered := service.onlinePlayers.PlayerForCharacter(17)
	if !registered || presence.TownID != 41 {
		t.Fatalf("the live registration was disturbed: %+v registered=%t", presence, registered)
	}
	// The memo is spent, so a later selector return cannot replay the old area.
	if service.restoreTownPresenceAfterDungeonSelector(opener, "test_replay") {
		t.Fatal("the retired record was replayed after it had been superseded")
	}
}
