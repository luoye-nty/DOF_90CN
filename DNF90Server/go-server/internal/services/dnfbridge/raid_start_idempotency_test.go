package dnfbridge

import (
	"bytes"
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/platform/timequeue"
)

func TestRuntimeRaidCreate664ReusesIdenticalRequestAndRejectsMembershipConflicts(t *testing.T) {
	service := &Service{raids: make(map[uint32]*runtimeRaidState)}
	channel := channelcatalog.Channel{ID: 200, Type: 23}
	leader := &gameSession{channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	req := raid.InfoRequest{RouteOrRaidType: 1, NameBytes: []byte("anton"), TailFlag: 5}

	created, accepted := service.createRuntimeRaid(leader, 1001, req)
	if !accepted || created.RaidKey == 0 || len(service.raids) != 1 {
		t.Fatalf("first 664 accepted=%t state=%+v raid_count=%d", accepted, created, len(service.raids))
	}
	replayed, accepted := service.createRuntimeRaid(leader, 1001, req)
	if !accepted || replayed.RaidKey != created.RaidKey || len(service.raids) != 1 {
		t.Fatalf("identical 664 accepted=%t key=%#x want=%#x raid_count=%d", accepted, replayed.RaidKey, created.RaidKey, len(service.raids))
	}

	different := req
	different.NameBytes = []byte("other")
	if _, accepted = service.createRuntimeRaid(leader, 1001, different); accepted || len(service.raids) != 1 {
		t.Fatalf("different 664 accepted=%t raid_count=%d", accepted, len(service.raids))
	}

	service.raids[created.RaidKey].Members = append(service.raids[created.RaidKey].Members,
		runtimeRaidMemberState{CharID: 1002, GroupIndex: 1, SlotOrder: 1})
	member := &gameSession{channel: channel}
	service.bindGameSessionCharacter(member, 1002)
	if _, accepted = service.createRuntimeRaid(member, 1002, req); accepted || len(service.raids) != 1 {
		t.Fatalf("member-owned 664 accepted=%t raid_count=%d", accepted, len(service.raids))
	}

	otherLeader := &gameSession{channel: channel}
	service.bindGameSessionCharacter(otherLeader, 2001)
	storeRuntimePartyState(otherLeader, alignedcmd.PartyState{
		PartyID:    2001,
		UserID:     2001,
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 2001},
			{UserID: 1002},
		},
	})
	if _, accepted = service.createRuntimeRaid(otherLeader, 2001, req); accepted || len(service.raids) != 1 {
		t.Fatalf("party containing existing raid member accepted=%t raid_count=%d", accepted, len(service.raids))
	}
}

func TestRuntimeRaidCreate664PreservesUnfinishedRaidWhenModeDiffers(t *testing.T) {
	service := &Service{raids: make(map[uint32]*runtimeRaidState)}
	channel := channelcatalog.Channel{ID: 200, Type: 23}
	leader := &gameSession{channel: channel}
	service.bindGameSessionCharacter(leader, 1001)

	single := raid.InfoRequest{RouteOrRaidType: 1, NameBytes: []byte("anton"), TailFlag: 1}
	created, accepted := service.createRuntimeRaid(leader, 1001, single)
	if !accepted || created.TailFlag != 1 || len(created.Members) != 1 {
		t.Fatalf("single create accepted=%t state=%+v", accepted, created)
	}

	normal := single
	normal.TailFlag = 0
	if _, accepted = service.createRuntimeRaid(leader, 1001, normal); accepted {
		t.Fatal("normal switch replaced the unfinished single raid")
	}
	if len(service.raids) != 1 {
		t.Fatalf("mode mismatch raid count=%d want=1", len(service.raids))
	}
	if current := service.raids[created.RaidKey]; current == nil || current.TailFlag != 1 || current.RaidKey != created.RaidKey {
		t.Fatalf("unfinished raid was not preserved: current=%+v created=%+v", current, created)
	}
}

func TestApplyRuntimeRaidPartiesDoesNotCreateOrdinaryPartyForSingleRaid(t *testing.T) {
	service := &Service{raids: make(map[uint32]*runtimeRaidState)}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		channel: channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType},
	}
	service.bindGameSessionCharacter(session, 1001)
	member := runtimeRaidMemberState{CharID: 1001, GroupIndex: 1, SlotOrder: 0}
	state := runtimeRaidState{
		RaidKey:       0x00170001,
		LeaderID:      1001,
		TailFlag:      1,
		Started:       true,
		EntryCostPaid: true,
		Members:       []runtimeRaidMemberState{member},
		FrozenMembers: []runtimeRaidMemberState{member},
	}

	// Seed the stale one-member projection produced by the former start path.
	stale := runtimeRaidGroupPartyState(state, state.Members)
	storeRuntimePartyState(session, stale)
	if managed, restored := service.bootstrapManagedRuntimePartyForSession(session); !restored || len(managed.Members) != 1 {
		t.Fatalf("seed stale single raid party: restored=%t party=%+v", restored, managed)
	}
	connection.write.Reset()

	if err := service.applyRuntimeRaidParties(state); err != nil {
		t.Fatalf("apply single raid parties: %v", err)
	}
	if partyState := runtimePartyStateSnapshot(session); partyState.PartyID > 0 || len(partyState.Members) != 0 {
		t.Fatalf("single raid retained ordinary party projection: %+v", partyState)
	}
	if managed, found := service.runtimePartyManagerForService().SnapshotByUser(1001, session.characterGeneration); found {
		t.Fatalf("single raid retained central ordinary party: %+v", managed)
	}
	if connection.write.Len() == 0 {
		t.Fatal("stale single raid party was not cleared from the current client")
	}
}

func TestRuntimeRaidStartUsesNormalMinimumAndReplaysWithoutSecondDebitOrPhaseAdvance(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002"} {
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots: map[string]dnfrepo.ItemStack{
				"0:0": {ItemID: runtimeRaidEntryItemID, Count: 2},
			},
		}); err != nil {
			t.Fatalf("seed ticket for %s: %v", characterID, err)
		}
	}
	config := runtimeRaidBridgeTestAntonConfig()
	config.StartMinimumMembers = 2
	config.SingleStartMinimumMembers = 1
	config.StartDelaySeconds = 3600
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	queue := &raidStartRecordingTimeQueue{now: now}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		raidAnton:          config,
		gameplayTimers:     queue,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	leader := &gameSession{conn: leaderConn, channel: channel}
	member := &gameSession{conn: memberConn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	raidKey := uint32(200)<<16 | 1001
	// Both members are in the raid waiting area: the start command refuses a roster
	// that is not all there (see raid_start_waiting_area.go). Only the leader is in
	// the roster at this point; the member is appended below from the same area.
	enterRuntimeRaidWaitingAreaPresenceForTest(t, service, leader, testAntonWaitingAreaSpec(t), raidKey)
	enterRuntimeRaidWaitingAreaPresenceForTest(t, service, member, testAntonWaitingAreaSpec(t), raidKey)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:  raidKey,
		LeaderID: 1001,
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
		},
	}

	if err := service.handleOnlineStartRaid(leader, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatalf("start below normal minimum: %v", err)
	}
	failure, rest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if !bytes.Equal(failure.Body, []byte{0, runtimeRaidStartFailureMinimumRoster}) || len(rest) != 0 {
		t.Fatalf("below-minimum response=% X trailing=%d", failure.Body, len(rest))
	}
	if state := service.raids[raidKey]; state.Started || state.StartPending || state.EntryCostPaid {
		t.Fatalf("single member bypassed normal minimum: %+v", state)
	}
	assertRuntimeRaidTicketCount(t, repositories, "1001", 2)

	service.raids[raidKey].Members = append(service.raids[raidKey].Members,
		runtimeRaidMemberState{CharID: 1002, GroupIndex: 1, SlotOrder: 1})
	leaderConn.write.Reset()
	if err := service.handleOnlineStartRaid(leader, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatalf("start at normal minimum: %v", err)
	}
	ack, _ := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketStartRaid) || !bytes.Equal(ack.Body, raid.BuildRaidCommandSuccessBody()) {
		t.Fatalf("successful start ACK msg=%d body=% X", ack.Header.MsgID, ack.Body)
	}
	started := cloneRuntimeRaidState(*service.raids[raidKey])
	if !started.Started || !started.EntryCostPaid || started.Lifecycle.Status != runtimeRaidLifecyclePreparing {
		t.Fatalf("successful start state=%+v", started)
	}
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
	assertRuntimeRaidTicketCount(t, repositories, "1002", 1)

	service.unbindGameSession(member)
	leaderConn.write.Reset()
	memberConn.write.Reset()
	if err := service.handleOnlineStartRaid(leader, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatalf("retry after member disconnect: %v", err)
	}
	retryACK, _ := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if !bytes.Equal(retryACK.Body, raid.BuildRaidCommandSuccessBody()) {
		t.Fatalf("retry ACK body=% X", retryACK.Body)
	}
	replayed := service.raids[raidKey]
	if replayed.StartAttempt != started.StartAttempt || !replayed.Lifecycle.StartedAt.Equal(started.Lifecycle.StartedAt) ||
		!replayed.Lifecycle.ReadyDeadline.Equal(started.Lifecycle.ReadyDeadline) {
		t.Fatalf("retry changed committed start: before=%+v after=%+v", started, replayed)
	}
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
	assertRuntimeRaidTicketCount(t, repositories, "1002", 1)

	// While the raid sits in the between-phase standby, the same empty START_RAID
	// is the client's "begin the next phase" command: live 2026-09-16 20:30Z the
	// leader clicked it three times during standby and every answer was a standby
	// replay, so phase two never began. The click must now activate phase two
	// without debiting the entry ticket again, and a second click must not advance
	// anything on top of it.
	standbyStarted := now.Add(10 * time.Minute)
	standbyDeadline := standbyStarted.Add(10 * time.Minute)
	service.raidMu.Lock()
	standby := service.raids[raidKey]
	standby.Lifecycle.Status = runtimeRaidLifecycleStandby
	standby.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	standby.Lifecycle.ReadyDeadline = time.Time{}
	standby.Lifecycle.StandbyStartedAt = standbyStarted
	standby.Lifecycle.StandbyDeadline = standbyDeadline
	standby.Lifecycle.Revision++
	service.raidMu.Unlock()

	leaderConn.write.Reset()
	if err := service.handleOnlineStartRaid(leader, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatalf("start during phase standby: %v", err)
	}
	// The next-phase activation broadcasts the raid object and the phase snapshot
	// around the command ACK, so look for the ACK itself instead of the first frame.
	standbyACK := false
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, leaderConn.write.Bytes()) {
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketStartRaid) &&
			bytes.Equal(packet.Body, raid.BuildRaidCommandSuccessBody()) {
			standbyACK = true
		}
	}
	if !standbyACK {
		t.Fatalf("standby start emitted no command ACK stream=%x", leaderConn.write.Bytes())
	}
	afterNextPhase := service.raids[raidKey]
	if afterNextPhase.Lifecycle.Status != runtimeRaidLifecyclePhaseActive ||
		afterNextPhase.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo {
		t.Fatalf("standby start did not begin phase two: %+v", afterNextPhase.Lifecycle)
	}
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
	assertRuntimeRaidTicketCount(t, repositories, "1002", 1)

	// A second click inside the now-active phase two only replays the state.
	activatedPhase := afterNextPhase.Lifecycle.Phase
	activatedStartedAt := afterNextPhase.Lifecycle.PhaseStartedAt
	leaderConn.write.Reset()
	if err := service.handleOnlineStartRaid(leader, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatalf("retry during active phase two: %v", err)
	}
	replayedPhaseTwo := service.raids[raidKey]
	if replayedPhaseTwo.Lifecycle.Phase != activatedPhase ||
		!replayedPhaseTwo.Lifecycle.PhaseStartedAt.Equal(activatedStartedAt) {
		t.Fatalf("second start advanced the active phase: %+v", replayedPhaseTwo.Lifecycle)
	}
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
	assertRuntimeRaidTicketCount(t, repositories, "1002", 1)
}

func TestRuntimeLukeSingleRaidStartDebitsLukeTicketAndFreezesNativeRoute(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	const characterID = "1001"
	luke := runtimeRaidBridgeTestLukeConfig()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: int64(luke.Raid.EntryCost.ItemID), Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	queue := &raidStartRecordingTimeQueue{now: time.Now().UTC()}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		raidLuke:           luke,
		gameplayTimers:     queue,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		channel: channelcatalog.Channel{ID: 241, Type: dnfenum.HiddenRaidType, Name: "luke"},
	}
	service.bindGameSessionCharacter(session, 1001)
	const raidKey = uint32(0x00f10001)
	// The Luke start carries the same waiting-area gate as the Anton one, against
	// the Luke town's own waiting area (see raid_start_waiting_area.go).
	enterRuntimeRaidWaitingAreaPresenceForTest(t, service, session, testLukeWaitingAreaSpec(t), raidKey)
	member := runtimeRaidMemberState{CharID: 1001, Name: "solo", GroupIndex: 1, SlotOrder: 0}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey: raidKey, LeaderID: member.CharID, RaidMode: runtimeRaidOwnerLuke,
		TailFlag: 1, LukeConfig: luke, Members: []runtimeRaidMemberState{member},
	}

	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatal(err)
	}
	state := service.raids[raidKey]
	if state == nil || !state.Started || !state.EntryCostPaid || state.RaidMode != runtimeRaidOwnerLuke ||
		state.LukeConfig != luke || state.AntonConfig != nil || state.Lifecycle.Status != runtimeRaidLifecyclePreparing {
		t.Fatalf("Luke start state=%+v", state)
	}
	wantRoute := runtimeRaidLukeSingleTypeForTime(state.Lifecycle.StartedAt)
	if state.SingleRaidType != wantRoute {
		t.Fatalf("Luke route=%d want=%d at %s", state.SingleRaidType, wantRoute, state.Lifecycle.StartedAt)
	}
	firstMap := runtimeRaidLukeSingleRoute(wantRoute)[0]
	if objective := state.Lifecycle.Objectives[firstMap]; objective.State != raid.AntonDungeonOpen {
		t.Fatalf("Luke first objective=%+v", objective)
	}
	assertRuntimeRaidTicketCount(t, repositories, characterID, 1)
	if len(queue.scheduled) != 1 || queue.scheduled[0] != "raid-ready:15794177:1" {
		t.Fatalf("Luke ready schedules=%v", queue.scheduled)
	}
}

func assertRuntimeRaidTicketCount(t *testing.T, repositories dnfrepo.Group, characterID string, want int64) {
	t.Helper()
	record, found, err := repositories.Inventory.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load ticket inventory %s found=%t err=%v", characterID, found, err)
	}
	got := int64(0)
	if stack, ok := record.Slots["0:0"]; ok {
		got = stack.Count
	}
	if got != want {
		t.Fatalf("ticket inventory %s count=%d want=%d", characterID, got, want)
	}
}

type raidStartRecordingTimeQueue struct {
	now       time.Time
	scheduled []string
}

func (q *raidStartRecordingTimeQueue) Now() time.Time { return q.now }

func (q *raidStartRecordingTimeQueue) ScheduleAfter(name string, _ time.Duration, _ timequeue.Callback) error {
	q.scheduled = append(q.scheduled, name)
	return nil
}

func (q *raidStartRecordingTimeQueue) RegisterDaily(
	name string,
	_, _ int,
	_ timequeue.Callback,
) error {
	q.scheduled = append(q.scheduled, name)
	return nil
}

func (q *raidStartRecordingTimeQueue) Cancel(string) bool { return false }

func (q *raidStartRecordingTimeQueue) Start(context.Context) {}
