package dnfbridge

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// A raid squad must exist as a real ordinary party, not only as a raid roster
// row: the client draws its own party HUD from the ordinary party table, so a
// squad that only exists in the roster is invisible to its own members. These
// tests pin the projection at the boundaries where a squad's membership can
// change.

type runtimeRaidSquadProjectionFixture struct {
	service  *Service
	raidKey  uint32
	sessions map[uint16]*gameSession
}

func newRuntimeRaidSquadProjectionFixture(
	t *testing.T,
	members []runtimeRaidMemberState,
	started bool,
) runtimeRaidSquadProjectionFixture {
	t.Helper()
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:         make(map[uint32]*runtimeRaidState),
		onlinePlayers: newOnlinePlayerManager(),
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10238}
	sessions := make(map[uint16]*gameSession, len(members))
	for _, member := range members {
		session := &gameSession{conn: &bufferConn{}, channel: channel}
		service.bindGameSessionCharacter(session, member.CharID)
		// A squad party is published only once every remote member has a native
		// probe endpoint; without one the two-phase publish defers silently.
		registerPartyPeerEndpointForTest(session, byte(member.CharID), uint16(50000+member.CharID))
		sessions[member.CharID] = session
	}
	const raidKey = uint32(0x00c80900)
	state := &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       members[0].CharID,
		NameBytes:      []byte("anton"),
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		Started:        started,
		EntryCostPaid:  started,
		StartAttempt:   1,
		ChargedMembers: runtimeRaidMemberIDs(runtimeRaidState{Members: members}),
	}
	if started {
		state.Lifecycle = newRuntimeRaidLifecycleState(time.Now().UTC())
	}
	service.raids[raidKey] = state
	service.gameSessions = sessions
	return runtimeRaidSquadProjectionFixture{service: service, raidKey: raidKey, sessions: sessions}
}

func (f runtimeRaidSquadProjectionFixture) state(t *testing.T) runtimeRaidState {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	state := f.service.raids[f.raidKey]
	if state == nil {
		return runtimeRaidState{}
	}
	return cloneRuntimeRaidState(*state)
}

func runtimeRaidSquadProjectionMember(charID uint16, group byte, slot byte) runtimeRaidMemberState {
	return runtimeRaidMemberState{
		CharID:     charID,
		Name:       "member",
		GroupIndex: group,
		SlotOrder:  slot,
		UserState:  1,
		HPPercent:  100,
		MPPercent:  100,
	}
}

// One non-zero squad must end up as exactly one real ordinary party, and a second
// pass over an unchanged roster must refresh that same party instead of forming a
// new one. op11 is the formation handshake: it belongs to the first pass only, and
// replaying it during a plain refresh tears down a healthy peer link.
func TestRuntimeRaidSquadPartiesAreMaterializedAndRefreshedInPlace(t *testing.T) {
	fixture := newRuntimeRaidSquadProjectionFixture(t, []runtimeRaidMemberState{
		runtimeRaidSquadProjectionMember(1001, 1, 0),
		runtimeRaidSquadProjectionMember(1002, 1, 1),
	}, true)

	state := fixture.state(t)
	if err := fixture.service.applyRuntimeRaidParties(state); err != nil {
		t.Fatalf("first projection: %v", err)
	}
	projected := fixture.state(t)
	if len(projected.SquadParties) != 1 || projected.SquadParties[1] == 0 {
		t.Fatalf("squad 1 owns no ordinary party: %+v", projected.SquadParties)
	}
	manager := fixture.service.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByID(projected.SquadParties[1])
	if !found || snapshot.Leader != 1001 || len(snapshot.Members) != 2 {
		t.Fatalf("projected party=%+v found=%t", snapshot, found)
	}
	for _, session := range []*gameSession{fixture.sessions[1001], fixture.sessions[1002]} {
		got := runtimePartyStateSnapshot(session)
		if got.PartyID != int(projected.SquadParties[1]) || len(got.Members) != 2 {
			t.Fatalf("char %d cached party=%+v", session.selectedCharacterID, got)
		}
		if got.Members[0].UserID != 1001 || got.Members[0].Slot != 0 ||
			got.Members[1].UserID != 1002 || got.Members[1].Slot != 1 {
			t.Fatalf("char %d cached squad table=%+v", session.selectedCharacterID, got.Members)
		}
	}
	leaderPackets := runtimeRaidPeerTake(t, fixture.sessions[1001])
	if _, ok := runtimeRaidPeerFind(leaderPackets, 0x000b); !ok {
		t.Fatal("a newly formed squad party did not install its op11 peer table")
	}
	// Drain the member's first-pass stream too, so the refresh assertions below
	// only see what the refresh itself sent.
	runtimeRaidPeerTake(t, fixture.sessions[1002])

	// Second pass over an unchanged roster: refresh the same party, never reform it.
	state = fixture.state(t)
	if err := fixture.service.applyRuntimeRaidParties(state); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	refreshed := fixture.state(t)
	if refreshed.SquadParties[1] != projected.SquadParties[1] {
		t.Fatalf("a refresh reformed the squad party: %d -> %d",
			projected.SquadParties[1], refreshed.SquadParties[1])
	}
	for _, charID := range []uint16{1001, 1002} {
		packets := runtimeRaidPeerTake(t, fixture.sessions[charID])
		if _, ok := runtimeRaidPeerFind(packets, 0x000b); ok {
			t.Fatalf("char %d had op11 replayed during a squad refresh", charID)
		}
		if _, ok := runtimeRaidPeerFind(packets, 0x0099); !ok {
			t.Fatalf("char %d received no op153 during a squad refresh", charID)
		}
	}
}

// A leader's regroup has to reach the ordinary parties too: moving a member into
// another squad retires the old party and forms the new one, so neither client
// keeps showing the squads they no longer fight in.
func TestRuntimeRaidSquadRegroupRebuildsOrdinaryParty(t *testing.T) {
	fixture := newRuntimeRaidSquadProjectionFixture(t, []runtimeRaidMemberState{
		runtimeRaidSquadProjectionMember(1001, 1, 0),
		runtimeRaidSquadProjectionMember(1002, 2, 0),
	}, true)
	if err := fixture.service.applyRuntimeRaidParties(fixture.state(t)); err != nil {
		t.Fatalf("initial projection: %v", err)
	}
	before := fixture.state(t)
	if before.SquadParties[1] == 0 || before.SquadParties[2] == 0 {
		t.Fatalf("initial squads are not both projected: %+v", before.SquadParties)
	}
	// The regroup is only offered during the phase-one standby break.
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Lifecycle.Status = runtimeRaidLifecycleStandby
	fixture.service.raidMu.Unlock()
	for _, session := range fixture.sessions {
		session.conn.(*bufferConn).write.Reset()
	}

	if err := fixture.service.handleOnlineRaidManagerWork(
		fixture.sessions[1001],
		uint16(dnfenum.CmdPacketRaidManagerWork),
		runtimeRaidManagerWorkBody(runtimeRaidManagerWorkAssignGroup, 1002, 1),
	); err != nil {
		t.Fatalf("regroup: %v", err)
	}
	after := fixture.state(t)
	if len(after.SquadParties) != 1 || after.SquadParties[1] == 0 {
		t.Fatalf("regroup left %d projected squads: %+v", len(after.SquadParties), after.SquadParties)
	}
	manager := fixture.service.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByID(after.SquadParties[1])
	if !found || snapshot.Leader != 1001 || len(snapshot.Members) != 2 {
		t.Fatalf("regrouped party=%+v found=%t", snapshot, found)
	}
	// The old squad-2 party must be gone with its only member.
	if _, stillThere := manager.SnapshotByID(before.SquadParties[2]); stillThere {
		t.Fatalf("the retired squad-2 party %d survived the regroup", before.SquadParties[2])
	}
	for _, charID := range []uint16{1001, 1002} {
		got := runtimePartyStateSnapshot(fixture.sessions[charID])
		if got.PartyID != int(after.SquadParties[1]) || len(got.Members) != 2 ||
			got.Members[0].UserID != 1001 || got.Members[1].UserID != 1002 {
			t.Fatalf("char %d party after regroup=%+v", charID, got)
		}
	}
}

// A member pulled back to standby (group 0) fights in no squad, so the party this
// projection gave their old squad must be taken away -- and only the survivors'
// rebuilt squad may remain.
func TestRuntimeRaidStandbyRegroupDetachesSquadProjection(t *testing.T) {
	fixture := newRuntimeRaidSquadProjectionFixture(t, []runtimeRaidMemberState{
		runtimeRaidSquadProjectionMember(1001, 1, 0),
		runtimeRaidSquadProjectionMember(1002, 1, 1),
	}, true)
	if err := fixture.service.applyRuntimeRaidParties(fixture.state(t)); err != nil {
		t.Fatalf("initial projection: %v", err)
	}
	projected := fixture.state(t)
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Lifecycle.Status = runtimeRaidLifecycleStandby
	fixture.service.raidMu.Unlock()

	if err := fixture.service.handleOnlineRaidManagerWork(
		fixture.sessions[1001],
		uint16(dnfenum.CmdPacketRaidManagerWork),
		runtimeRaidManagerWorkBody(runtimeRaidManagerWorkAssignGroup, 1002, 0),
	); err != nil {
		t.Fatalf("standby regroup: %v", err)
	}
	manager := fixture.service.runtimePartyManagerForService()
	if snapshot, found := manager.SnapshotByUser(1002, 0); found {
		t.Fatalf("standby member 1002 kept the squad party: %+v", snapshot)
	}
	if got := runtimePartyStateSnapshot(fixture.sessions[1002]); got.PartyID != 0 || len(got.Members) != 0 {
		t.Fatalf("standby member 1002 kept a client party table: %+v", got)
	}
	after := fixture.state(t)
	if len(after.SquadParties) != 1 || after.SquadParties[1] == projected.SquadParties[1] {
		t.Fatalf("squad 1 party was not rebuilt for its survivor: %+v", after.SquadParties)
	}
	remaining, found := manager.SnapshotByID(after.SquadParties[1])
	if !found || len(remaining.Members) != 1 || remaining.Members[0].UserID != 1001 {
		t.Fatalf("survivor party=%+v found=%t", remaining, found)
	}
}

// Leaving the raid detaches the leaver from the party this projection created for
// their squad and rebuilds what is left for the survivors.
func TestRuntimeRaidLeaveDetachesSquadProjection(t *testing.T) {
	fixture := newRuntimeRaidSquadProjectionFixture(t, []runtimeRaidMemberState{
		runtimeRaidSquadProjectionMember(1001, 1, 0),
		runtimeRaidSquadProjectionMember(1002, 1, 1),
	}, true)
	if err := fixture.service.applyRuntimeRaidParties(fixture.state(t)); err != nil {
		t.Fatalf("initial projection: %v", err)
	}
	leaveBody := make([]byte, 2)
	binary.LittleEndian.PutUint16(leaveBody, 1002)
	if err := fixture.service.handleOnlineLeaveRaid(
		fixture.sessions[1002],
		uint16(dnfenum.CmdPacketLeaveRaid),
		leaveBody,
	); err != nil {
		t.Fatalf("leave raid: %v", err)
	}
	manager := fixture.service.runtimePartyManagerForService()
	if snapshot, found := manager.SnapshotByUser(1002, 0); found {
		t.Fatalf("leaver kept the squad party: %+v", snapshot)
	}
	after := fixture.state(t)
	if len(after.SquadParties) != 1 || after.SquadParties[1] == 0 {
		t.Fatalf("squad 1 is no longer projected for its survivor: %+v", after.SquadParties)
	}
	remaining, found := manager.SnapshotByID(after.SquadParties[1])
	if !found || len(remaining.Members) != 1 || remaining.Members[0].UserID != 1001 {
		t.Fatalf("survivor party=%+v found=%t", remaining, found)
	}
}

// With one squad the reveal board is one body for everybody, and it has to stay
// byte-identical to the body the settlement committed: the per-squad split only
// exists to separate squads that are actually different.
func TestRuntimeRaidRewardListKeepsOneBodyForOneSquad(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	fixture.service.raidMu.Lock()
	for index := range fixture.service.raids[fixture.raidKey].FrozenMembers {
		fixture.service.raids[fixture.raidKey].FrozenMembers[index].GroupIndex = 1
	}
	for index := range fixture.service.raids[fixture.raidKey].Members {
		fixture.service.raids[fixture.raidKey].Members[index].GroupIndex = 1
	}
	fixture.service.raidMu.Unlock()

	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("commit rewards: %v", err)
	}
	bodies, err := runtimeRaidRewardSquadBodies(delivery, delivery.RewardBody)
	if err != nil {
		t.Fatalf("split reward bodies: %v", err)
	}
	if bodies != nil {
		t.Fatalf("one squad produced %d squad bodies: %+v", len(bodies), bodies)
	}
	publication := &runtimeRaidRewardPublication{RewardListMembers: make(map[uint16]bool)}
	if err := fixture.service.publishRuntimeRaidRewardListLocked(delivery, delivery.RewardBody, publication); err != nil {
		t.Fatalf("publish rewards: %v", err)
	}
	for _, session := range []*gameSession{fixture.leader, fixture.member} {
		packets := runtimeRaidPeerTake(t, session)
		reward, found := runtimeRaidPeerFind(packets, raid.RaidRewardListMsgID)
		if !found {
			t.Fatalf("char %d received no reward list: %d packets", session.selectedCharacterID, len(packets))
		}
		if string(reward.Body) != string(delivery.RewardBody) {
			t.Fatalf("char %d body=%x want the committed body %x",
				session.selectedCharacterID, reward.Body, delivery.RewardBody)
		}
	}
}

// Entering a raid gate changes the scene for that member, and both the member that
// moved and the ones that stayed need the other's actor record to draw the 战况表
// cell and the standby room. The projection must reach every pair, in both
// directions, without the same-area shortcut.
//
// The direction into the client that is inside the gate is deliberately excluded:
// the body carries the channel's scene context, and a client rendering a dungeon
// instance binds it to the locally controlled actor instead of creating a peer
// (see sendRuntimeRaidPeerActor). The install into the member that stayed is the
// one that carries the roster cell, and it is asserted here.
func TestRuntimeRaidDungeonLoadedInstallsMateActorsIntoTargetsOutsideTheGate(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002"} {
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: characterID,
			AccountID:   "dnf:1",
			Name:        "char" + characterID,
			Level:       90,
		}); err != nil {
			t.Fatalf("seed character %s: %v", characterID, err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "packet.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.packetLog.Store(logger)
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10238}
	leader := &gameSession{conn: &bufferConn{}, channel: channel}
	member := &gameSession{conn: &bufferConn{}, channel: channel}
	// The member that stayed is sitting in the raid waiting area, so its channel
	// scene actor is committed (game_bootstrap.go:54-55 and the town route set
	// townActorOwnerChannel; only a scene the channel does not own resets it).
	// Without this the fixture models a client that is inside a selector or an
	// instance, which gets the record-only install below.
	member.townActorOwnerChannel = byte(channel.ID)
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	const raidKey = uint32(0x00c80901)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		Members:        []runtimeRaidMemberState{runtimeRaidSquadProjectionMember(1001, 1, 0), runtimeRaidSquadProjectionMember(1002, 1, 1)},
		FrozenMembers:  []runtimeRaidMemberState{runtimeRaidSquadProjectionMember(1001, 1, 0), runtimeRaidSquadProjectionMember(1002, 1, 1)},
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002},
		Lifecycle:      newRuntimeRaidLifecycleState(time.Now().UTC()),
	}
	service.gameSessions = map[uint16]*gameSession{1001: leader, 1002: member}

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "1001"},
		lifecycleToken: 41,
	}
	leader.dungeon = dungeonSessionState{runtime: runtime}
	if err := service.freezeRuntimeRaidDungeonSelection(leader, runtime); err != nil {
		t.Fatalf("freeze selection: %v", err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(leader); err != nil {
		t.Fatalf("loaded: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	// 包日志自 2026-09-30 起落盘即密文；断言前先解密。
	text := packetLogPlaintext(t, logPath)
	// The member that stayed receives the record of the one that entered.
	if !strings.Contains(text, "kind=game-town-copresence-actor-state-send conn_id= channel_id=200 source=raid_dungeon_loaded_raid_mate_actors target_char_id=1002") ||
		!strings.Contains(text, "actor_char_id=1001") {
		t.Fatalf("raid dungeon loaded did not install the entering actor into the member that stayed:\n%s", text)
	}
	// The member inside the gate still gets the record -- it is the client whose
	// roster portraits were the default ones without it -- but never the mode=1
	// binding, whose skill pages the client applies to its own controlled actor.
	if !strings.Contains(text, "kind=game-raid-peer-actor-sent conn_id= channel_id=200 peer_char_id=1002 source=raid_dungeon_loaded_raid_mate_actors mode=mode0_record_only") {
		t.Fatalf("raid dungeon loaded did not give the client inside the gate the record-only install:\n%s", text)
	}
	// The mate that stayed in town is a channel-owned scene. It used to be the one
	// shape that kept the full (mode0+mode1) install, on the theory that the binding
	// is what paints the peer's equipment there. 2026-09-29: that theory cost a
	// full-second freeze for every client in the raid -- live raid 1507329 (8
	// members, own build) measured the SAME fan-out at 981ms..1.025s per loop while
	// the installs carried mode1 (waiting area) and 351..377ms on those same loops
	// five minutes later once they were record-only -- so the waiting area is now
	// record-only too, exactly like the town co-presence path has been since
	// 2026-09-26 ("mode1 现在没有任何消费者").
	if !strings.Contains(text, "kind=game-raid-peer-actor-sent conn_id= channel_id=200 peer_char_id=1001 source=raid_dungeon_loaded_raid_mate_actors mode=mode0_record_only") {
		t.Fatalf("the mate that stayed in town did not get the record-only install:\n%s", text)
	}
	if !strings.Contains(text, "kind=game-raid-peer-actor-sent") {
		t.Fatalf("the loaded boundary did not run its mate-actor install: %s", text)
	}
}

// The `selected` boundary runs while the entering client may still be between
// scenes: its channel-owned scene is gone (owner byte 0) and the next one does
// not exist yet. A peer record installed then lands in a scene the client is
// about to destroy, and the client then walks that half-built user-table entry
// from its own raid UI (live 2026-09-17: 11:05:52.017 selected(224)
// target_local_owner=0 -> client died 11:05:52.377 inside the raid UI with
// eip=0). The receiver in that state must be skipped, the receiver that still
// has a live scene must not be, and the loaded boundary must still install.
func TestRuntimeRaidDungeonSelectedDefersMateActorsBetweenScenes(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002"} {
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: characterID,
			AccountID:   "dnf:1",
			Name:        "char" + characterID,
			Level:       90,
		}); err != nil {
			t.Fatalf("seed character %s: %v", characterID, err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "packet.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.packetLog.Store(logger)
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10238}
	leader := &gameSession{conn: &bufferConn{}, channel: channel}
	member := &gameSession{conn: &bufferConn{}, channel: channel}
	// The member that stayed owns a channel scene; the entering leader does not
	// (that is exactly the between-scenes state the selected boundary sees).
	member.townActorOwnerChannel = byte(channel.ID)
	leader.townActorOwnerChannel = 0
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	const raidKey = uint32(0x00c80902)
	lifecycle := newRuntimeRaidLifecycleState(time.Now().UTC())
	// The gate lives in phase two, which is the phase whose entries produced the
	// live crash.
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		Members:        []runtimeRaidMemberState{runtimeRaidSquadProjectionMember(1001, 1, 0), runtimeRaidSquadProjectionMember(1002, 1, 1)},
		FrozenMembers:  []runtimeRaidMemberState{runtimeRaidSquadProjectionMember(1001, 1, 0), runtimeRaidSquadProjectionMember(1002, 1, 1)},
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002},
		Lifecycle:      lifecycle,
	}
	service.gameSessions = map[uint16]*gameSession{1001: leader, 1002: member}

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 218},
		Character:      dnfrepo.CharacterRecord{CharacterID: "1001"},
		lifecycleToken: 42,
	}
	leader.dungeon = dungeonSessionState{runtime: runtime}
	if err := service.freezeRuntimeRaidDungeonSelection(leader, runtime); err != nil {
		t.Fatalf("freeze selection: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	// 包日志自 2026-09-30 起落盘即密文；断言前先解密。
	text := packetLogPlaintext(t, logPath)
	if !strings.Contains(text, "kind=game-raid-peer-actor-deferred") ||
		!strings.Contains(text, "reason=scene_between_scenes") ||
		!strings.Contains(text, "char_id=1001") {
		t.Fatalf("the between-scenes receiver was not deferred:\n%s", text)
	}
	if strings.Contains(text, "source=raid_dungeon_selected_raid_mate_actors target_char_id=1001") {
		t.Fatalf("a peer record was installed into the between-scenes receiver:\n%s", text)
	}
	// The mate that owns a scene still gets its record from the same boundary.
	if !strings.Contains(text, "source=raid_dungeon_selected_raid_mate_actors target_char_id=1002") {
		t.Fatalf("the selected boundary stopped installing into the receiver that owns a scene:\n%s", text)
	}
	if strings.Contains(text, "source=raid_dungeon_selected_raid_mate_actors") &&
		!strings.Contains(text, "kind=game-raid-peer-actor-sent") {
		t.Fatalf("the selected boundary logged no install at all:\n%s", text)
	}
}
