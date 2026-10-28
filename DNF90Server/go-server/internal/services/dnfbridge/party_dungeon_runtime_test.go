package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestRuntimePartyDungeonRoomMoveRejoinsDivergedPrivateObjectKeyStreams(t *testing.T) {
	service, leaderRuntime := prepareSyntheticMoveRuntime(t, false)
	_, memberRuntime := prepareSyntheticMoveRuntime(t, false)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.gameSessions = make(map[uint16]*gameSession)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	leader := &gameSession{
		ctx: ctx, conn: leaderConn, connID: "party-room-move-leader",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: leaderRuntime},
	}
	member := &gameSession{
		ctx: ctx, conn: memberConn, connID: "party-room-move-member",
		selectedCharacterID: 100, dungeon: dungeonSessionState{runtime: memberRuntime},
	}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(member, 100)
	owner := &runtimePartyDungeonOwner{
		partyID: 7, leaderID: 99, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 99, generation: leader.characterGeneration},
			{slot: 1, userID: 100, generation: member.characterGeneration},
		},
		worldRooms:  make(map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom),
		worldDeaths: make(map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact),
	}
	leaderRuntime.partyDungeonOwner, leaderRuntime.partyMemberIndexed, leaderRuntime.partyMemberIndex = owner, true, 0
	memberRuntime.partyDungeonOwner, memberRuntime.partyMemberIndexed, memberRuntime.partyMemberIndex = owner, true, 1
	// Reproduce the live failure: member-private rewards consumed one more
	// object key than the leader before both clients entered room two.
	leaderRuntime.NextObjectKey = 407
	memberRuntime.NextObjectKey = 408

	if err := service.startGameSessionEvents(ctx, member); err != nil {
		t.Fatal(err)
	}
	defer stopTestGameSessionEvents(t, service, member)
	body := make([]byte, dungeoncmd.MoveMapRequestSize)
	body[0] = 1
	if err := service.handleDungeonMoveMap(leader, body); err != nil {
		t.Fatalf("move party into shared second room: %v", err)
	}

	leaderRoom := leaderRuntime.Room.Snapshot()
	memberRoom := memberRuntime.Room.Snapshot()
	if len(leaderRoom.Monsters) != 1 || len(memberRoom.Monsters) != 1 {
		t.Fatalf("target monster counts leader=%d member=%d", len(leaderRoom.Monsters), len(memberRoom.Monsters))
	}
	if leaderRoom.Monsters[0].ObjectKey != 408 || memberRoom.Monsters[0].ObjectKey != 408 {
		t.Fatalf("shared actor keys leader=%d member=%d want=408",
			leaderRoom.Monsters[0].ObjectKey, memberRoom.Monsters[0].ObjectKey)
	}
	if leaderRuntime.NextObjectKey != 409 || memberRuntime.NextObjectKey != 409 {
		t.Fatalf("next object keys leader=%d member=%d want=409",
			leaderRuntime.NextObjectKey, memberRuntime.NextObjectKey)
	}
	leaderStart, leaderTrailing := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	memberStart, memberTrailing := splitGameServerUpperPacket(t, memberConn.write.Bytes())
	leaderTrailing = consumeOptionalCurrentDungeonFatigueRefreshForTest(t, leaderTrailing)
	memberTrailing = consumeOptionalCurrentDungeonFatigueRefreshForTest(t, memberTrailing)
	if len(leaderTrailing) != 0 || len(memberTrailing) != 0 ||
		leaderStart.Header.MsgID != currentDungeonStartNotification || memberStart.Header.MsgID != currentDungeonStartNotification ||
		!bytes.Equal(leaderStart.Body, memberStart.Body) {
		t.Fatalf("second-room op29 projections diverged:\nleader=%x\nmember=%x",
			leaderConn.write.Bytes(), memberConn.write.Bytes())
	}
}

func TestRuntimePartyDungeonRoomReloadReplaysDeadPeerState(t *testing.T) {
	service, leaderRuntime := prepareSyntheticMoveRuntime(t, false)
	_, memberRuntime := prepareSyntheticMoveRuntime(t, false)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.gameSessions = make(map[uint16]*gameSession)
	service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("party death replay repositories unavailable")
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "100",
		AccountID:   "account-1",
		Name:        "follower",
		Job:         atSwordmanTutorialJob,
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 7, "area_id": 3,
			"pos_x": 500, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "100",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	memberRuntime.Character.CharacterID = "100"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	leader := &gameSession{
		ctx: ctx, conn: leaderConn, connID: "party-death-replay-leader",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: leaderRuntime},
	}
	member := &gameSession{
		ctx: ctx, conn: memberConn, connID: "party-death-replay-member",
		selectedCharacterID: 100, dungeon: dungeonSessionState{runtime: memberRuntime},
	}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(member, 100)
	owner := &runtimePartyDungeonOwner{
		partyID: 7, leaderID: 99, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 99, generation: leader.characterGeneration},
			{slot: 1, userID: 100, generation: member.characterGeneration},
		},
		worldRooms:  make(map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom),
		worldDeaths: make(map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact),
	}
	leaderRuntime.partyDungeonOwner, leaderRuntime.partyMemberIndexed, leaderRuntime.partyMemberIndex = owner, true, 0
	memberRuntime.partyDungeonOwner, memberRuntime.partyMemberIndexed, memberRuntime.partyMemberIndex = owner, true, 1
	for _, runtime := range []*runtimeDungeonState{leaderRuntime, memberRuntime} {
		if runtime.lifecycleToken == 0 {
			runtime.lifecycleToken = 1
		}
	}
	for _, session := range []*gameSession{leader, member} {
		if err := service.startGameSessionEvents(ctx, session); err != nil {
			t.Fatal(err)
		}
		defer stopTestGameSessionEvents(t, service, session)
	}

	// The follower dies in the first room: the shared world records the dead
	// slot and projects one op32 to the living leader.
	if characterID, reason := currentDungeonDeathRuntimeOwner(memberRuntime, member); reason != "" {
		t.Fatalf("member death owner id=%d reason=%q run=%+v", characterID, reason, memberRuntime.Session.Snapshot().Run)
	}
	if err := service.handleDungeonDieCharacter(
		member,
		make([]byte, currentDungeonDieCharacterBodySize),
	); err != nil {
		t.Fatal(err)
	}
	if owner.deadSlots != 0b10 || !memberRuntime.deathReturnWaiting {
		t.Fatalf("member death dead_slots=%08b waiting=%t",
			owner.deadSlots, memberRuntime.deathReturnWaiting)
	}

	// Both clients reload the next room. Every client respawns the party actors
	// from its own room payload, so the leader must be told again that the
	// follower is still a ghost. The dead member must not receive a replay of
	// its own death, which would restart its revive window.
	if err := service.callGameSession(
		ctx, leader, "party-death-replay-projection-barrier", func() error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	resetDungeonEntrySceneGates(leader)
	resetDungeonEntrySceneGates(member)
	// The party is already inside room one: model that completed initial load
	// cycle without replaying the whole entry barrier.
	if cycle := owner.beginRoomLoading(); cycle != 1 {
		t.Fatalf("initial party loading cycle=%d want=1", cycle)
	}
	if cycle := owner.beginRoomLoading(); cycle != 2 {
		t.Fatalf("second party loading cycle=%d want=2", cycle)
	}
	leaderConn.write.Reset()
	memberConn.write.Reset()
	if err := service.sendFinishLoadingStatus(
		leader,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatal(err)
	}
	if err := service.sendFinishLoadingStatus(
		member,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatal(err)
	}

	want := buildCurrentDungeonDeathStateBody(currentSceneActorObjectKey(100), 0, 0)
	replayed := 0
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, leaderConn.write.Bytes()) {
		if packet.Header.MsgID != currentDungeonDeathStateMsgID {
			continue
		}
		if !bytes.Equal(packet.Body, want) {
			t.Fatalf("leader death replay body=%x want=%x", packet.Body, want)
		}
		replayed++
	}
	if replayed != 1 {
		t.Fatalf("leader death replay count=%d want=1 packets=%x",
			replayed, leaderConn.write.Bytes())
	}
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, memberConn.write.Bytes()) {
		if packet.Header.MsgID == currentDungeonDeathStateMsgID {
			t.Fatalf("dead member received its own death replay body=%x", packet.Body)
		}
	}
}

func TestRuntimePartyDungeonLoadingBarrierPublishesRaidAttemptLoaded(t *testing.T) {
	service, runtime := prepareSyntheticMoveRuntime(t, false)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.gameSessions = make(map[uint16]*gameSession)
	service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
	service.onlinePlayers = newOnlinePlayerManager()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connection := &bufferConn{}
	session := &gameSession{
		ctx: ctx, conn: connection, connID: "party-raid-loaded",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
	}
	service.bindGameSessionCharacter(session, 99)
	if runtime.lifecycleToken == 0 {
		runtime.lifecycleToken = 1
	}
	owner := &runtimePartyDungeonOwner{
		partyID: 7, leaderID: 99, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 99, generation: session.characterGeneration},
		},
		worldRooms:  make(map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom),
		worldDeaths: make(map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact),
	}
	runtime.partyDungeonOwner, runtime.partyMemberIndexed, runtime.partyMemberIndex = owner, true, 0

	// This party run sits inside a raid gate opened by this very session, so the
	// binding token matches and the attempt is the opener's own.
	token := runtime.lifecycleToken
	runtime.raidDungeon = runtimeRaidDungeonBinding{
		RaidKey:          0x10063,
		Phase:            runtimeRaidDungeonPhaseOne,
		MapID:            210,
		LifecycleToken:   token,
		selectedNotified: true,
	}
	service.raids = map[uint32]*runtimeRaidState{
		0x10063: {
			RaidKey: 0x10063, LeaderID: 99, Started: true, EntryCostPaid: true,
			Members:       []runtimeRaidMemberState{{CharID: 99, GroupIndex: 1}},
			FrozenMembers: []runtimeRaidMemberState{{CharID: 99, GroupIndex: 1}},
			Lifecycle: runtimeRaidLifecycleState{
				Status: runtimeRaidLifecyclePhaseActive,
				Phase:  runtimeRaidDungeonPhaseOne,
				Dungeons: map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{
					{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}: {
						Phase: runtimeRaidDungeonPhaseOne, MapID: 210,
						Seat:           runtimeRaidDungeonSeat{Group: 1},
						CharacterID:    99,
						LifecycleToken: token,
						Status:         runtimeRaidDungeonProgressSelected,
					},
				},
			},
		},
	}
	if err := service.startGameSessionEvents(ctx, session); err != nil {
		t.Fatal(err)
	}
	defer stopTestGameSessionEvents(t, service, session)

	resetDungeonEntrySceneGates(session)
	// The initial cycle installs op19 and needs the real job/skill PVF; the
	// shared release path is the same for every cycle, so drive the room-reload
	// cycle that this fixture can serve.
	if cycle := owner.beginRoomLoading(); cycle != 1 {
		t.Fatalf("initial party loading cycle=%d want=1", cycle)
	}
	if cycle := owner.beginRoomLoading(); cycle != 2 {
		t.Fatalf("second party loading cycle=%d want=2", cycle)
	}
	connection.write.Reset()
	if err := service.sendFinishLoadingStatus(
		session,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatal(err)
	}

	// The shared party barrier must publish the same "loaded" event the solo op37
	// path does; without it the attempt stays at selected and the opener's own
	// completion is refused by the raid state machine as out of order.
	if !runtime.raidDungeon.loadedNotified {
		t.Fatalf("party loading barrier skipped the raid attempt loaded event: %+v",
			runtime.raidDungeon)
	}
	service.raidMu.Lock()
	progress := service.raids[0x10063].Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]
	service.raidMu.Unlock()
	if progress.Status != runtimeRaidDungeonProgressLoaded {
		t.Fatalf("authoritative raid attempt status=%d want=%d",
			progress.Status, runtimeRaidDungeonProgressLoaded)
	}
}

// newPartyDungeonTownReturnFixture builds a three-member admitted party dungeon
// (leader 99 in slot 0, members 100 and 101 in slots 1 and 2) so a town return
// leaves TWO survivors behind: the party is only reduced, not dissolved, and the
// survivor projections (op9 kind2 + 0x0099 + op26 host transfer + scene op6) stay
// observable. The two-member dissolve case is covered by
// TestRuntimePartyDungeonTownReturnDisbandsATwoMemberParty.
func newPartyDungeonTownReturnFixture(t *testing.T) (*Service, map[uint16]*gameSession, *runtimePartyDungeonOwner, alignedcmd.PartyState) {
	t.Helper()
	service := &Service{
		options:      options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameSessions: make(map[uint16]*gameSession),
	}
	sessions := make(map[uint16]*gameSession, 3)
	for _, characterID := range []uint16{99, 100, 101} {
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              fmt.Sprintf("party-return-%d", characterID),
			selectedCharacterID: characterID,
		}
		service.bindGameSessionCharacter(session, characterID)
		sessions[characterID] = session
	}
	if _, result, joined := service.createOrJoinManagedRuntimeParty(sessions[100], sessions[99]); !joined || !result.OK {
		t.Fatalf("create party result=%+v", result)
	}
	state, result, joined := service.createOrJoinManagedRuntimeParty(sessions[101], sessions[99])
	if !joined || !result.OK || len(runtimePartyMembers(state)) != 3 {
		t.Fatalf("third join joined=%t result=%+v state=%+v", joined, result, state)
	}
	snapshot, found := service.runtimePartyManagerForService().SnapshotByID(uint16(state.PartyID))
	if !found {
		t.Fatal("managed party snapshot unavailable")
	}
	owner, err := newRuntimePartyDungeonOwner(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for index, characterID := range []uint16{99, 100, 101} {
		sessions[characterID].dungeon.runtime = &runtimeDungeonState{
			partyDungeonOwner:  owner,
			partyMemberIndexed: true,
			partyMemberIndex:   byte(index),
		}
	}
	return service, sessions, owner, state
}

func TestRuntimePartyDungeonTownReturnRemovesOnlyReturningMember(t *testing.T) {
	for _, test := range []struct {
		name          string
		returningID   uint16
		newLeaderID   uint16
		newLeaderSlot byte
		hostTransfer  bool
	}{
		{
			name: "leader returns and leadership transfers", returningID: 99,
			newLeaderID: 100, newLeaderSlot: 1, hostTransfer: true,
		},
		{
			name: "follower returns and leader remains", returningID: 100,
			newLeaderID: 99, newLeaderSlot: 0, hostTransfer: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, sessions, owner, state := newPartyDungeonTownReturnFixture(t)
			returning := sessions[test.returningID]
			previous := runtimePartyStateSnapshot(returning)
			if err := service.detachRuntimePartyDungeonMemberForTownReturn(
				returning, owner, previous, "test_midrun_town_return",
			); err != nil {
				t.Fatal(err)
			}

			if got := runtimePartyStateSnapshot(returning); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
				t.Fatalf("returning member retained party projection: %+v", got)
			}
			if _, found := service.runtimePartyManagerForService().SnapshotByUser(
				test.returningID, returning.characterGeneration,
			); found {
				t.Fatal("returning member remained in managed party")
			}
			roster := owner.roster()
			if len(roster) != 2 || owner.leaderID != test.newLeaderID ||
				owner.leaderSlot != test.newLeaderSlot {
				t.Fatalf("shared dungeon roster=%+v leader=%d/%d", roster, owner.leaderID, owner.leaderSlot)
			}

			for _, survivorID := range []uint16{99, 100, 101} {
				if survivorID == test.returningID {
					continue
				}
				survivor := sessions[survivorID]
				survivorState := runtimePartyStateSnapshot(survivor)
				if survivorState.PartyID != state.PartyID || survivorState.UserID != test.newLeaderID ||
					len(runtimePartyMembers(survivorState)) != 2 {
					t.Fatalf("survivor %d party state=%+v", survivorID, survivorState)
				}
				packets := splitAllCurrentGameServerUpperPackets(t, survivor.conn.(*bufferConn).write.Bytes())
				wantPackets := 3
				if test.hostTransfer {
					wantPackets = 4
				}
				if len(packets) != wantPackets {
					t.Fatalf("survivor %d projections=%+v", survivorID, packets)
				}
				rest := assertRuntimePartyMembershipReduction(
					t, survivor.conn.(*bufferConn).write.Bytes(), 2, test.newLeaderSlot, 2)
				if test.hostTransfer {
					host, after := splitGameServerUpperPacket(t, rest)
					rest = after
					if host.Header.MsgID != 26 || len(host.Body) != 1 || host.Body[0] != test.newLeaderSlot {
						t.Fatalf("survivor %d replacement dungeon host projection=%+v", survivorID, host)
					}
				}
				leave, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
				if leave.Header.MsgID != currentTownUserLeaveMsgID ||
					!bytes.Equal(leave.Body, buildTownUserLeaveBody(test.returningID)) || len(rest) != 0 {
					t.Fatalf("survivor %d leave projection=%+v trailing=%d", survivorID, leave, len(rest))
				}
				if survivor.dungeon.runtime == nil || survivor.dungeon.runtime.partyDungeonOwner != owner {
					t.Fatalf("survivor %d dungeon runtime was detached", survivorID)
				}
			}

			// The returning member's own frame is reduced to the party it left.
			returnPackets := splitAllCurrentGameServerUpperPackets(t, returning.conn.(*bufferConn).write.Bytes())
			if len(returnPackets) != 2 ||
				returnPackets[0].Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
				returnPackets[1].Header.MsgID != 0x0099 {
				t.Fatalf("returning projections=%+v", returnPackets)
			}
		})
	}
}

// TestRuntimePartyDungeonTownReturnDisbandsATwoMemberParty pins the
// single-member disband rule (operator, 2026-09-22: 「一个人不成队」) at the
// dungeon town-return boundary: the member left inside is detached as well, so
// neither client keeps a party window, while the shared dungeon roster still
// hands the dungeon host to the member that stayed and retires the returning
// member's actor from that member's scene.
func TestRuntimePartyDungeonTownReturnDisbandsATwoMemberParty(t *testing.T) {
	service := &Service{
		options:      options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameSessions: make(map[uint16]*gameSession),
	}
	leader := &gameSession{conn: &bufferConn{}, connID: "party-return-leader", selectedCharacterID: 99}
	follower := &gameSession{conn: &bufferConn{}, connID: "party-return-follower", selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	state, result, joined := service.createOrJoinManagedRuntimeParty(follower, leader)
	if !joined || !result.OK || len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("create party joined=%t result=%+v state=%+v", joined, result, state)
	}
	snapshot, found := service.runtimePartyManagerForService().SnapshotByID(uint16(state.PartyID))
	if !found {
		t.Fatal("managed party snapshot unavailable")
	}
	owner, err := newRuntimePartyDungeonOwner(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	leader.dungeon.runtime = &runtimeDungeonState{partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: 0}
	follower.dungeon.runtime = &runtimeDungeonState{partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: 1}

	if err := service.detachRuntimePartyDungeonMemberForTownReturn(
		leader, owner, runtimePartyStateSnapshot(leader), "test_solo_completed_town_return",
	); err != nil {
		t.Fatal(err)
	}

	for _, session := range []*gameSession{leader, follower} {
		if got := runtimePartyStateSnapshot(session); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
			t.Fatalf("character %d kept a party after the dissolve: %+v", session.selectedCharacterID, got)
		}
		if _, found := service.runtimePartyManagerForService().SnapshotByUser(
			session.selectedCharacterID, session.characterGeneration,
		); found {
			t.Fatalf("character %d remained in the managed party", session.selectedCharacterID)
		}
	}
	roster := owner.roster()
	if len(roster) != 1 || roster[0].userID != 100 || owner.leaderID != 100 || owner.leaderSlot != 1 {
		t.Fatalf("shared dungeon roster=%+v leader=%d/%d", roster, owner.leaderID, owner.leaderSlot)
	}

	followerRest := assertRuntimePartySoloRowThenCleared(
		t, follower.conn.(*bufferConn).write.Bytes(), 1, uint16(state.PartyID))
	host, followerRest := splitGameServerUpperPacket(t, followerRest)
	if host.Header.MsgID != 26 || len(host.Body) != 1 || host.Body[0] != 1 {
		t.Fatalf("survivor replacement dungeon host projection=%+v", host)
	}
	leave, followerRest := splitCurrentGameServerUpperPacketAuto(t, followerRest)
	if leave.Header.MsgID != currentTownUserLeaveMsgID ||
		!bytes.Equal(leave.Body, buildTownUserLeaveBody(99)) || len(followerRest) != 0 {
		t.Fatalf("survivor leave projection=%+v trailing=%d", leave, len(followerRest))
	}
	leaderRest := assertRuntimePartyMembershipCleared(
		t, leader.conn.(*bufferConn).write.Bytes(), uint16(state.PartyID))
	if len(leaderRest) != 0 {
		t.Fatalf("returning member trailing bytes=%d", len(leaderRest))
	}
}

func TestRuntimePartyDungeonConcurrentTownReturnsDoNotRepublishStaleRoster(t *testing.T) {
	service := &Service{
		options:      options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameSessions: make(map[uint16]*gameSession),
	}
	leader := &gameSession{conn: &bufferConn{}, connID: "party-return-race-leader", selectedCharacterID: 99}
	follower := &gameSession{conn: &bufferConn{}, connID: "party-return-race-follower", selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	state, result, joined := service.createOrJoinManagedRuntimeParty(follower, leader)
	if !joined || !result.OK || len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("create party joined=%t result=%+v state=%+v", joined, result, state)
	}
	snapshot, found := service.runtimePartyManagerForService().SnapshotByID(uint16(state.PartyID))
	if !found {
		t.Fatal("managed party snapshot unavailable")
	}
	owner, err := newRuntimePartyDungeonOwner(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	leader.dungeon.runtime = &runtimeDungeonState{partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: 0}
	follower.dungeon.runtime = &runtimeDungeonState{partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: 1}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for _, session := range []*gameSession{leader, follower} {
		workers.Add(1)
		go func(session *gameSession) {
			defer workers.Done()
			<-start
			errs <- service.detachRuntimePartyDungeonMemberForTownReturn(
				session,
				owner,
				runtimePartyStateSnapshot(session),
				"test_concurrent_midrun_town_return",
			)
		}(session)
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if roster := owner.roster(); len(roster) != 0 {
		t.Fatalf("concurrent returns retained shared roster=%+v", roster)
	}
	for _, session := range []*gameSession{leader, follower} {
		if got := runtimePartyStateSnapshot(session); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
			t.Fatalf("concurrent return retained local party state=%+v", got)
		}
		if _, found := service.runtimePartyManagerForService().SnapshotByUser(
			session.selectedCharacterID,
			session.characterGeneration,
		); found {
			t.Fatalf("character %d remained in managed party", session.selectedCharacterID)
		}
	}
}

func TestRuntimePartyDungeonConcurrentCommandsDoNotCrossWaitOnMemberMailboxes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{gameSessions: make(map[uint16]*gameSession)}
	leader := &gameSession{connID: "party-command-leader", ctx: ctx}
	member := &gameSession{connID: "party-command-member", ctx: ctx}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	owner := &runtimePartyDungeonOwner{
		partyID: 7, leaderID: 1001, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 1001, generation: leader.characterGeneration},
			{slot: 1, userID: 1002, generation: member.characterGeneration},
		},
	}
	leader.dungeon.runtime = &runtimeDungeonState{partyDungeonOwner: owner}
	member.dungeon.runtime = &runtimeDungeonState{partyDungeonOwner: owner}
	if err := service.startGameSessionEvents(ctx, leader); err != nil {
		t.Fatal(err)
	}
	defer stopTestGameSessionEvents(t, service, leader)
	if err := service.startGameSessionEvents(ctx, member); err != nil {
		t.Fatal(err)
	}
	defer stopTestGameSessionEvents(t, service, member)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	var leaderRuns atomic.Int32
	var memberRuns atomic.Int32
	run := func(source *gameSession) {
		results <- service.callGameSession(ctx, source, "simultaneous-monster-death", func() error {
			started <- struct{}{}
			<-release
			return service.executeRuntimePartyDungeonCommand(
				source,
				"simultaneous-monster-death",
				func(target *gameSession) error {
					if target == leader {
						leaderRuns.Add(1)
					} else if target == member {
						memberRuns.Add(1)
					}
					return nil
				},
			)
		})
	}
	go run(leader)
	go run(member)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("both member mailboxes did not enter their inbound commands")
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent party commands cross-waited on member mailboxes")
		}
	}

	barrierCtx, barrierCancel := context.WithTimeout(ctx, time.Second)
	defer barrierCancel()
	if err := service.callGameSession(barrierCtx, leader, "leader-projection-barrier", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := service.callGameSession(barrierCtx, member, "member-projection-barrier", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := leaderRuns.Load(); got != 2 {
		t.Fatalf("leader command applications=%d want=2", got)
	}
	if got := memberRuns.Load(); got != 2 {
		t.Fatalf("member command applications=%d want=2", got)
	}
}

func TestRuntimePartyDungeonDropRNGIsStableAndPrivatePerMember(t *testing.T) {
	owner := &runtimePartyDungeonOwner{
		partyID: 7,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 1001},
			{slot: 1, userID: 1002},
		},
	}
	leader := &runtimeDungeonState{
		partyDungeonOwner:  owner,
		partyMemberIndexed: true,
		partyMemberIndex:   0,
	}
	member := &runtimeDungeonState{
		partyDungeonOwner:  owner,
		partyMemberIndexed: true,
		partyMemberIndex:   1,
	}
	const sharedRoomSeed = uint32(0x12345678)
	leaderSeed := currentPartyDungeonPrivateDropRNG(leader, sharedRoomSeed)
	memberSeed := currentPartyDungeonPrivateDropRNG(member, sharedRoomSeed)
	if leaderSeed == 0 || memberSeed == 0 || leaderSeed == memberSeed {
		t.Fatalf("private drop seeds leader=%08x member=%08x", leaderSeed, memberSeed)
	}
	if replay := currentPartyDungeonPrivateDropRNG(leader, sharedRoomSeed); replay != leaderSeed {
		t.Fatalf("leader private drop seed changed: %08x -> %08x", leaderSeed, replay)
	}
	if solo := currentPartyDungeonPrivateDropRNG(&runtimeDungeonState{}, sharedRoomSeed); solo != sharedRoomSeed {
		t.Fatalf("solo drop seed=%08x want shared room seed=%08x", solo, sharedRoomSeed)
	}
}

func TestRuntimePartyDungeonDropProjectionBuildsPrivateOp38RowsPerMember(t *testing.T) {
	service, prepared, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	source, ok := service.dungeonMonsterTable.source.(bridgePVFSource)
	if !ok {
		t.Fatalf("test source type=%T", service.dungeonMonsterTable.source)
	}
	// Make the gold branch deterministic (always selected) while retaining a
	// wide RNG-backed amount variance, so this test observes the real planner
	// result carried by op38 rather than merely comparing derived seed words.
	source[dungeonCardDropRulePath] = currentDungeonGoldDropTestRuleText()
	source[dungeonCardGoldReferencePath] = "[gold drop ref table]\n20 100 50\n"
	currentDungeonMonsterDropRulesByCatalog.Delete(service.dungeonMonsterTable)
	currentDungeonGoldReferencesByCatalog.Delete(service.dungeonMonsterTable)

	owner := &runtimePartyDungeonOwner{
		partyID: 7,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 1001},
			{slot: 1, userID: 1002},
		},
	}
	cloneForMember := func(slot byte) *runtimeDungeonState {
		clone := *prepared
		clone.partyDungeonOwner = owner
		clone.partyMemberIndexed = true
		clone.partyMemberIndex = slot
		clone.DropOwner = nil
		clone.Rooms = make(map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit, len(prepared.Rooms))
		for key, visit := range prepared.Rooms {
			visitClone := *visit
			visitClone.DropRNG = currentPartyDungeonPrivateDropRNG(&clone, visit.Seed)
			clone.Rooms[key] = &visitClone
		}
		return &clone
	}
	leader := cloneForMember(0)
	member := cloneForMember(1)
	monster := prepared.Room.Snapshot().Monsters[0]
	leaderDrops, err := service.planCurrentDungeonMonsterDrops(leader, monster, 1001)
	if err != nil {
		t.Fatal(err)
	}
	memberDrops, err := service.planCurrentDungeonMonsterDrops(member, monster, 1002)
	if err != nil {
		t.Fatal(err)
	}
	goldAmount := func(drops []currentDungeonDeathDropWire, ownerKey uint16) uint32 {
		t.Helper()
		for _, drop := range drops {
			if drop.OwnerActorObjectKey != ownerKey {
				t.Fatalf("private drop owner=%d want=%d", drop.OwnerActorObjectKey, ownerKey)
			}
			if binary.LittleEndian.Uint32(drop.Item.data[0x02:0x06]) == 0 {
				return binary.LittleEndian.Uint32(drop.Item.data[0x06:0x0A])
			}
		}
		t.Fatalf("member %d received no selected gold row: %+v", ownerKey, drops)
		return 0
	}
	leaderGold := goldAmount(leaderDrops, 1001)
	memberGold := goldAmount(memberDrops, 1002)
	if leaderGold == memberGold {
		t.Fatalf("private op38 gold rolls were shared: leader=%d member=%d", leaderGold, memberGold)
	}
	leaderBody, err := buildCurrentDungeonDeathNotificationBodyWithDrops(monster.ObjectKey, currentDungeonDeathResponseMonster, leaderDrops)
	if err != nil {
		t.Fatal(err)
	}
	memberBody, err := buildCurrentDungeonDeathNotificationBodyWithDrops(monster.ObjectKey, currentDungeonDeathResponseMonster, memberDrops)
	if err != nil {
		t.Fatal(err)
	}
	if string(leaderBody) == string(memberBody) {
		t.Fatalf("private op38 bodies are identical: %x", leaderBody)
	}
}

func TestRuntimePartyDungeonConcurrentMonsterDeathCommitsOneSharedWorldFact(t *testing.T) {
	service, leaderRuntime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "100",
		AccountID:   "account-1",
		Level:       20,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "100",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	memberRuntime, _, err := service.prepareDungeonRuntime(
		context.Background(),
		&gameSession{selectedCharacterID: 100, accountID: "account-1"},
		dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memberRuntime.Room.AnnounceAllActors(memberRuntime.Session); err != nil {
		t.Fatal(err)
	}

	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	leader := &gameSession{
		conn: leaderConn, connID: "party-shared-world-leader", accountID: "account-1",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: leaderRuntime},
	}
	member := &gameSession{
		conn: memberConn, connID: "party-shared-world-member", accountID: "account-1",
		selectedCharacterID: 100, dungeon: dungeonSessionState{runtime: memberRuntime},
	}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(member, 100)
	owner := &runtimePartyDungeonOwner{
		partyID: 7, leaderID: 99, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 99, generation: leader.characterGeneration},
			{slot: 1, userID: 100, generation: member.characterGeneration},
		},
		worldRooms:  make(map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom),
		worldDeaths: make(map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact),
	}
	leaderRuntime.partyDungeonOwner, leaderRuntime.partyMemberIndexed, leaderRuntime.partyMemberIndex = owner, true, 0
	memberRuntime.partyDungeonOwner, memberRuntime.partyMemberIndexed, memberRuntime.partyMemberIndex = owner, true, 1
	owner.worldMu.Lock()
	leaderRoomKey, err := owner.observeRuntimePartyDungeonWorldLocked(leaderRuntime)
	if err == nil {
		_, err = owner.observeRuntimePartyDungeonWorldLocked(memberRuntime)
	}
	owner.worldMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	objectKey := leaderRuntime.Room.Snapshot().Monsters[0].ObjectKey
	deathBody := func(ownerObjectKey uint16) []byte {
		body := make([]byte, dungeoncmd.DieMonsterRequestSize)
		binary.LittleEndian.PutUint32(body[0:4], objectKey)
		binary.LittleEndian.PutUint16(body[4:6], ownerObjectKey)
		return body
	}
	results := make(chan error, 2)
	go func() { results <- service.handleDungeonMonsterDeath(leader, deathBody(99)) }()
	go func() { results <- service.handleDungeonMonsterDeath(member, deathBody(100)) }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if !runtimePartyDungeonActorDefeated(leaderRuntime, objectKey) ||
		!runtimePartyDungeonActorDefeated(memberRuntime, objectKey) {
		t.Fatalf(
			"shared death diverged: leader=%t member=%t",
			runtimePartyDungeonActorDefeated(leaderRuntime, objectKey),
			runtimePartyDungeonActorDefeated(memberRuntime, objectKey),
		)
	}
	owner.worldMu.Lock()
	fact := owner.worldDeaths[runtimePartyDungeonDeathKey{room: leaderRoomKey, objectKey: objectKey}]
	sharedRoom := owner.worldRooms[leaderRoomKey]
	owner.worldMu.Unlock()
	if fact == nil || fact.consumers != 0b11 {
		t.Fatalf("shared death fact=%+v want both member consumers", fact)
	}
	if sharedRoom == nil {
		t.Fatal("shared room state is missing")
	}
	if actor := sharedRoom.actors[objectKey]; actor.state != runtimeDungeonMonsterDefeated || !sharedRoom.cleared {
		t.Fatalf("shared room state=%+v actor=%+v", sharedRoom, actor)
	}
	if leaderConn.write.Len() == 0 || memberConn.write.Len() == 0 {
		t.Fatalf("death projection writes leader=%d member=%d", leaderConn.write.Len(), memberConn.write.Len())
	}
}
