package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// newSoloReturnSameDungeonFixture builds the live 2026-09-22 shape: a
// two-member party (leader 99 returning, follower 100 still inside) admitted to
// one shared party dungeon, with the leader's run cleared.
func newSoloReturnSameDungeonFixture(t *testing.T) (*Service, *gameSession, *runtimeDungeonState, *gameSession, *runtimePartyDungeonOwner) {
	t.Helper()
	service, leader, leaderRuntime := newBackToVillageRuntime(t)
	service.bindGameSessionCharacter(leader, 99)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "100",
		AccountID:   "account-1",
		Name:        "follower",
		Job:         "0",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 7, "area_id": 3,
			"pos_x": 500, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatalf("save follower character: %v", err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "100",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save follower inventory: %v", err)
	}
	enableTownMoveSkillProjection(t, service, repositories, "100")
	follower := &gameSession{
		conn:                &bufferConn{},
		connID:              "solo-return-follower",
		channel:             leader.channel,
		residentChannel:     leader.residentChannel,
		selectedCharacterID: 100,
	}
	service.bindGameSessionCharacter(follower, 100)

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
	owner := &runtimePartyDungeonOwner{
		partyID: 9, leaderID: 99, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 99, generation: leader.characterGeneration},
			{slot: 1, userID: 100, generation: follower.characterGeneration},
		},
	}
	leaderRuntime.partyDungeonOwner = owner
	leaderRuntime.partyMemberIndexed = true
	leaderRuntime.partyMemberIndex = 0
	follower.dungeon.runtime = &runtimeDungeonState{
		Session:            leaderRuntime.Session,
		partyDungeonOwner:  owner,
		partyMemberIndexed: true,
		partyMemberIndex:   1,
		townReturnOrigin: currentTownPositionSnapshot{
			CharacterID: 100, TownID: 7, AreaID: 3,
			PositionX: 500, PositionY: 234, MovementCode: 5, PositionValid: true,
		},
	}
	if err := leaderRuntime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	return service, leader, leaderRuntime, follower, owner
}

// TestCompletedDungeonSoloReturnLeavesThePartyInsideTheSameRun is the regression
// for the reported 2026-09-22 defect: the leader cleared the dungeon, returned
// to town alone through the settlement route, and the follower kept rendering
// its dungeon actor while party chat still reached both. The completed route now
// applies the same rule the abandoned route already did: 「只要是自己单独回城了
// 视作直接退出队伍」.
func TestCompletedDungeonSoloReturnLeavesThePartyInsideTheSameRun(t *testing.T) {
	service, leader, leaderRuntime, follower, owner := newSoloReturnSameDungeonFixture(t)
	leader.conn.(*bufferConn).write.Reset()
	follower.conn.(*bufferConn).write.Reset()
	transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), 99, leader)
	if err != nil {
		t.Fatal(err)
	}

	if err := service.sendCurrentCompletedDungeonReturnToTown(
		leader,
		leaderRuntime,
		transition,
		uint16(dnfenum.CmdPacketGiveupGame),
		"test_op31_completed_settlement_timed_op42",
		false,
	); err != nil {
		t.Fatalf("completed dungeon town route: %v", err)
	}

	// The returning member left the party...
	if got := runtimePartyStateSnapshot(leader); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
		t.Fatalf("returning leader kept a party projection: %+v", got)
	}
	// ...and so did the member left inside: a party that drops to one member is
	// not a party (operator rule, same day).
	if got := runtimePartyStateSnapshot(follower); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
		t.Fatalf("follower kept a one-member aggregate: %+v", got)
	}
	manager := service.runtimePartyManagerForService()
	for _, characterID := range []uint16{99, 100} {
		identity, online := service.onlineGameSessionCharacterSnapshot(characterID)
		if !online {
			t.Fatalf("character %d lost its session", characterID)
		}
		if snapshot, found := manager.SnapshotByUser(characterID, identity.generation); found {
			t.Fatalf("character %d stayed in the managed party: %+v", characterID, snapshot)
		}
	}
	// The shared dungeon roster still hands the dungeon host to the member that
	// stayed inside.
	roster := owner.roster()
	if len(roster) != 1 || roster[0].userID != 100 || owner.leaderID != 100 || owner.leaderSlot != 1 {
		t.Fatalf("shared dungeon roster=%+v leader=%d/%d", roster, owner.leaderID, owner.leaderSlot)
	}

	// The follower loses the returning actor from its dungeon scene, and gets the
	// dissolve projection plus the replacement dungeon host.
	followerRest := assertRuntimePartySoloRowThenCleared(
		t, follower.conn.(*bufferConn).write.Bytes(), 1, 9)
	host, followerRest := splitGameServerUpperPacket(t, followerRest)
	if host.Header.MsgID != 26 || len(host.Body) != 1 || host.Body[0] != 1 {
		t.Fatalf("follower replacement dungeon host projection=%+v", host)
	}
	leave, followerRest := splitCurrentGameServerUpperPacketAuto(t, followerRest)
	if leave.Header.MsgID != currentTownUserLeaveMsgID ||
		!bytes.Equal(leave.Body, buildTownUserLeaveBody(99)) || len(followerRest) != 0 {
		t.Fatalf("follower actor retirement=%+v trailing=%d", leave, len(followerRest))
	}
	if follower.dungeon.runtime == nil || follower.dungeon.runtime.partyDungeonOwner != owner {
		t.Fatal("the follower's own dungeon runtime was detached")
	}

	// The leader's own client is told the party is gone as well.
	if !partyFrameClearedFor(t, leader.conn.(*bufferConn).write.Bytes(), 9) {
		t.Fatal("the returning leader was never told its party frame is gone")
	}
}

// A teammate that already came home (its own run retired) is not evidence of a
// solo return: the party must survive so the team can play together in town.
func TestCompletedDungeonSoloReturnKeepsPartyWhenTheTeammateCameHome(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(follower *gameSession, owner *runtimePartyDungeonOwner)
	}{
		{
			name: "teammate returned to town",
			configure: func(follower *gameSession, owner *runtimePartyDungeonOwner) {
				// releaseDungeonRuntimePayload clears exactly this on a return.
				follower.dungeon.runtime.partyDungeonOwner = nil
			},
		},
		{
			name: "teammate holds no dungeon run at all",
			configure: func(follower *gameSession, owner *runtimePartyDungeonOwner) {
				follower.dungeon.runtime = nil
			},
		},
		{
			name: "whole-party exit is marked",
			configure: func(follower *gameSession, owner *runtimePartyDungeonOwner) {
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, leader, leaderRuntime, follower, owner := newSoloReturnSameDungeonFixture(t)
			test.configure(follower, owner)
			if test.name == "whole-party exit is marked" {
				leaderRuntime.townReturnKeepParty = true
			}
			transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), 99, leader)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.sendCurrentCompletedDungeonReturnToTown(
				leader,
				leaderRuntime,
				transition,
				uint16(dnfenum.CmdPacketGiveupGame),
				"test_completed_return_keeps_party",
				false,
			); err != nil {
				t.Fatalf("completed dungeon town route: %v", err)
			}
			for _, session := range []*gameSession{leader, follower} {
				state := runtimePartyStateSnapshot(session)
				if state.PartyID == 0 || len(runtimePartyMembers(state)) != 2 {
					t.Fatalf("character %d lost the party: %+v", session.selectedCharacterID, state)
				}
			}
		})
	}
}

// partyFrameClearedFor reports whether the stream carries the op9 kind-3 record
// removal that empties a party frame keyed by wantPartyID.
func partyFrameClearedFor(t *testing.T, stream []byte, wantPartyID uint16) bool {
	t.Helper()
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, stream) {
		body := packet.Body
		if packet.Header.Classification != 0 ||
			packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
			len(body) != 10 || body[9] != currentSceneOp9ActorRemoveKind {
			continue
		}
		if uint16(body[4])|uint16(body[5])<<8 == wantPartyID {
			return true
		}
	}
	return false
}

// TestRuntimePartyPeerRuntimeInsideSameRunMatchesSharedOwnerAndRaidGate pins the
// "same 副本" identity the solo-return rule keys on: the shared party-dungeon
// owner for an admitted party run, and the raid key plus gate map for a raid gate
// (which owns no party-dungeon owner). A teammate in town, in another party
// dungeon or in another gate must never trigger it.
func TestRuntimePartyPeerRuntimeInsideSameRunMatchesSharedOwnerAndRaidGate(t *testing.T) {
	owner := &runtimePartyDungeonOwner{partyID: 9, leaderID: 99, leaderSlot: 0}
	other := &runtimePartyDungeonOwner{partyID: 10, leaderID: 101, leaderSlot: 0}
	runtime := &runtimeDungeonState{
		partyDungeonOwner: owner,
		Dungeon:           worldmap.Dungeon{ID: 210},
	}
	for _, test := range []struct {
		name      string
		peer      *runtimeDungeonState
		wantMatch bool
		wantWhy   string
	}{
		{
			name:      "same shared party dungeon roster",
			peer:      &runtimeDungeonState{partyDungeonOwner: owner},
			wantMatch: true, wantWhy: "shared_party_dungeon_roster",
		},
		{
			name: "teammate returned and released its binding",
			peer: &runtimeDungeonState{},
		},
		{
			name: "teammate is in another party dungeon",
			peer: &runtimeDungeonState{partyDungeonOwner: other},
		},
		{
			name: "teammate holds no run",
			peer: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			matched, why := runtimePartyPeerRuntimeInsideSameRun(runtime, test.peer)
			if matched != test.wantMatch || why != test.wantWhy {
				t.Fatalf("match=%t why=%q, want match=%t why=%q", matched, why, test.wantMatch, test.wantWhy)
			}
		})
	}

	raidRuntime := &runtimeDungeonState{
		Dungeon:     worldmap.Dungeon{ID: 210},
		raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10063},
	}
	for _, test := range []struct {
		name      string
		peer      *runtimeDungeonState
		wantMatch bool
	}{
		{
			name: "squadmate inside the same cleared gate",
			peer: &runtimeDungeonState{
				Dungeon:     worldmap.Dungeon{ID: 210},
				raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10063},
			},
			wantMatch: true,
		},
		{
			name: "squadmate inside a different gate",
			peer: &runtimeDungeonState{
				Dungeon:     worldmap.Dungeon{ID: 222},
				raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10063},
			},
		},
		{
			name: "squadmate in another raid's gate",
			peer: &runtimeDungeonState{
				Dungeon:     worldmap.Dungeon{ID: 210},
				raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10064},
			},
		},
		{
			name: "squadmate back in the waiting area",
			peer: &runtimeDungeonState{raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10063}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			matched, _ := runtimePartyPeerRuntimeInsideSameRun(raidRuntime, test.peer)
			if matched != test.wantMatch {
				t.Fatalf("raid gate match=%t, want %t", matched, test.wantMatch)
			}
		})
	}
}

// A raid gate owns no shared party-dungeon roster, so the no-owner half of the
// rule has to drop the returning member from the party and retire its actor for
// the squadmates still inside the same gate.
func TestSoloDungeonReturnWithoutOwnerLeavesTheRaidGateParty(t *testing.T) {
	service := &Service{
		options:      options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameSessions: make(map[uint16]*gameSession),
	}
	leader := &gameSession{conn: &bufferConn{}, connID: "raid-gate-leader", selectedCharacterID: 99}
	follower := &gameSession{conn: &bufferConn{}, connID: "raid-gate-follower", selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	state, result, joined := service.createOrJoinManagedRuntimeParty(follower, leader)
	if !joined || !result.OK || len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("create party joined=%t result=%+v state=%+v", joined, result, state)
	}
	leaderRuntime := &runtimeDungeonState{
		Dungeon:     worldmap.Dungeon{ID: 210},
		raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10063},
	}
	follower.dungeon.runtime = &runtimeDungeonState{
		Dungeon:     worldmap.Dungeon{ID: 210},
		raidDungeon: runtimeRaidDungeonBinding{RaidKey: 0x10063},
	}
	follower.conn.(*bufferConn).write.Reset()

	if err := service.leaveRuntimePartyOnSoloDungeonReturn(
		leader,
		leaderRuntime,
		state,
		100,
		"same_raid_gate_run",
		"test_raid_gate_completed_solo_return",
	); err != nil {
		t.Fatalf("raid gate solo return: %v", err)
	}

	for _, session := range []*gameSession{leader, follower} {
		if got := runtimePartyStateSnapshot(session); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
			t.Fatalf("character %d kept a party: %+v", session.selectedCharacterID, got)
		}
	}
	if snapshot, found := service.runtimePartyManagerForService().SnapshotByUser(99, leader.characterGeneration); found {
		t.Fatalf("returning leader stayed in the managed party: %+v", snapshot)
	}
	followerRest := assertRuntimePartySoloRowThenCleared(
		t, follower.conn.(*bufferConn).write.Bytes(), 1, uint16(state.PartyID))
	host, followerRest := splitGameServerUpperPacket(t, followerRest)
	if host.Header.MsgID != 26 || len(host.Body) != 1 || host.Body[0] != 1 {
		t.Fatalf("squadmate replacement dungeon host projection=%+v", host)
	}
	leave, followerRest := splitCurrentGameServerUpperPacketAuto(t, followerRest)
	if leave.Header.MsgID != currentTownUserLeaveMsgID ||
		!bytes.Equal(leave.Body, buildTownUserLeaveBody(99)) || len(followerRest) != 0 {
		t.Fatalf("squadmate actor retirement=%+v trailing=%d", leave, len(followerRest))
	}
}
