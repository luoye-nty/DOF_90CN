package dnfbridge

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// These tests pin the town-visibility invariants a party must satisfy after the
// settlement exit. They exist because a manual test cycle was the only oracle for
// too long: the field report is "after 通关 -> 选择其他地下城 -> 从选图界面返回城镇 the two
// members cannot see each other and each other's movement never arrives", while the
// same return straight from the settlement (and the same return without a completed
// run) works.
//
// The invariants asserted here are the ones every later packet path depends on:
//  1. both characters hold a live town presence registration,
//  2. they are in the same area key (channel+instance+town+area), which is what
//     every movement relay and later actor install is filtered by,
//  3. a position update from one of them is relayed to the other,
//  4. each client is told about the other's actor (op2 / mode0).
func assertPartyTownVisibilityInvariants(
	t *testing.T,
	service *Service,
	leader *gameSession,
	follower *gameSession,
	leaderConn *bufferConn,
	followerConn *bufferConn,
	stage string,
) {
	t.Helper()
	leaderID := leader.selectedCharacterID
	followerID := follower.selectedCharacterID

	if _, ok := service.onlinePlayers.PlayerForCharacter(leaderID); !ok {
		t.Fatalf("%s: leader %d has no town presence registration", stage, leaderID)
	}
	if _, ok := service.onlinePlayers.PlayerForCharacter(followerID); !ok {
		t.Fatalf("%s: follower %d has no town presence registration", stage, followerID)
	}
	if !service.onlinePlayers.PeerInSameArea(leaderID, followerID) {
		t.Fatalf("%s: %d and %d are not in the same area key", stage, leaderID, followerID)
	}
	// The movement relay is the invariant the field report sees as "A walks, B does
	// not move": if this returns nothing, no peer ever learns about the movement.
	peers := service.onlinePlayers.UpdatePosition(leaderID, 640, 320)
	found := false
	for _, peer := range peers {
		if peer.CharacterID == followerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s: movement of %d is not relayed to %d (peers=%d)",
			stage, leaderID, followerID, len(peers))
	}
	// And each client must have been told about the other's actor record.
	for name, probe := range map[string]struct {
		session *gameSession
		conn    *bufferConn
		peerID  uint16
	}{
		"leader":   {leader, leaderConn, followerID},
		"follower": {follower, followerConn, leaderID},
	} {
		if probe.conn == nil {
			continue
		}
		installSeen := false
		for _, packet := range splitAllCurrentGameServerUpperPackets(t, probe.conn.write.Bytes()) {
			if packet.Header.MsgID == 2 {
				installSeen = true
				break
			}
		}
		if !installSeen {
			t.Fatalf("%s: %s never received the peer %d actor install (op2)", stage, name, probe.peerID)
		}
	}
}

func newPartyTownVisibilityFixture(t *testing.T) (service *Service, leader, follower *gameSession,
	leaderConn, followerConn *bufferConn) {
	t.Helper()
	base, session, _ := newBackToVillageRuntime(t)
	base.onlinePlayers = newOnlinePlayerManager()
	service = base
	leader = session
	leader.selectedCharacterID = 99
	leaderConn = leader.conn.(*bufferConn)
	leader.dungeon.runtime = nil
	service.bindGameSessionCharacter(leader, 99)
	bindDungeonSelectorOriginForTestAt(t, service, leader, 7, 3, 474, 234)
	leader.enterSelectDungeonSent = true
	leader.enterSelectDungeonAckSent = true
	leader.enterSelectDungeonContextSent = true

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "100",
		AccountID:   "account-1",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 7, "area_id": 3,
			"pos_x": 500, "pos_y": 240, "direction": 6, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	follower = &gameSession{
		conn:                &bufferConn{},
		connID:              "party-visibility-follower",
		channel:             leader.channel,
		residentChannel:     leader.residentChannel,
		selectedCharacterID: 100,
	}
	followerConn = follower.conn.(*bufferConn)
	service.bindGameSessionCharacter(follower, 100)
	bindDungeonSelectorOriginForTestAt(t, service, follower, 7, 3, 500, 240)
	follower.enterSelectDungeonSent = true
	follower.enterSelectDungeonPassiveParty = true
	follower.enterSelectDungeonContextSent = true

	partyState := alignedcmd.PartyState{
		PartyID: 9,
		UserID:  99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, Slot: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, Slot: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, partyState)
	storeRuntimePartyState(follower, partyState)
	// Both characters are already registered in the town, which is the state the
	// party is in right before it enters the dungeon.
	service.registerTownPresenceForTest(t, leader, 99, 7, 3, 474, 234)
	service.registerTownPresenceForTest(t, follower, 100, 7, 3, 500, 240)
	return service, leader, follower, leaderConn, followerConn
}

// The invariant must already hold while the party stands in town, so a failure
// here means the fixture itself is wrong rather than the return path.
func TestPartyVisibilityInvariantsHoldInTown(t *testing.T) {
	service, leader, follower, leaderConn, followerConn := newPartyTownVisibilityFixture(t)
	assertPartyTownVisibilityInvariants(t, service, leader, follower, leaderConn, followerConn, "town")
}

// The reported flow: the leader closes the shared selector with 返回城镇 and the
// passive follower is returned with it. After that both must satisfy the same
// invariants -- that is exactly what the field report says does not happen.
func TestPartyVisibilityInvariantsSurviveSelectorReturn(t *testing.T) {
	service, leader, follower, leaderConn, followerConn := newPartyTownVisibilityFixture(t)
	leaderConn.write.Reset()
	followerConn.write.Reset()

	service.onlinePlayers.LeaveArea(follower.selectedCharacterID)
	if err := service.handleDungeonBackToVillage(leader, nil); err != nil {
		t.Fatalf("leader selector return: %v", err)
	}
	assertPartyTownVisibilityInvariants(t, service, leader, follower, leaderConn, followerConn,
		"after selector return")
}

// The settlement exit itself (选择其他地下城) must not break the invariants either.
func TestPartyVisibilityInvariantsSurviveSettlementSelectOther(t *testing.T) {
	service, leader, follower, leaderConn, followerConn := newPartyTownVisibilityFixture(t)
	if err := service.handleDungeonEplpCommand(leader, []byte{1, 1}); err != nil {
		t.Fatalf("leader select-other: %v", err)
	}
	assertPartyTownVisibilityInvariants(t, service, leader, follower, leaderConn, followerConn,
		"after select-other")
}

// registerTownPresenceForTest files a character in the town area table exactly like
// an area enter does, which is the state the party must be in before the flow under
// test starts (and the state the return path has to restore).
func (s *Service) registerTownPresenceForTest(
	t *testing.T,
	session *gameSession,
	characterID uint16,
	townID, areaID byte,
	posX, posY uint16,
) {
	t.Helper()
	if s == nil || session == nil || s.onlinePlayers == nil {
		t.Fatal("town presence fixture requires a service with an online player manager")
	}
	info := &onlinePlayerInfo{
		CharacterID: characterID,
		ChannelID:   townPresenceChannelID(session),
		TownID:      townID,
		AreaID:      areaID,
		PositionX:   posX,
		PositionY:   posY,
		Session:     session,
	}
	others := s.onlinePlayers.EnterArea(info)
	if len(others) > 0 {
		s.broadcastTownPlayerEnter(info, others)
	}
}
