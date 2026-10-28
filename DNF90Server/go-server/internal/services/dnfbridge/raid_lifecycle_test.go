package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRuntimeRaidStartFreezesOnlineRosterAndOrdersAckBeforeCostSync(t *testing.T) {
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
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		raidAnton:          runtimeRaidBridgeTestAntonConfig(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leader := &gameSession{conn: leaderConn, channel: channel}
	member := &gameSession{conn: memberConn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	registerPartyPeerEndpointForTest(leader, 3, 5063)
	registerPartyPeerEndpointForTest(member, 3, 60359)
	raidKey := uint32(200)<<16 | 1001
	// Both members stand in the raid waiting area: the start command refuses a
	// roster that is not all there (see raid_start_waiting_area.go).
	enterRuntimeRaidWaitingAreaPresenceForTest(t, service, leader, testAntonWaitingAreaSpec(t), raidKey)
	enterRuntimeRaidWaitingAreaPresenceForTest(t, service, member, testAntonWaitingAreaSpec(t), raidKey)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1001,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketStartRaid),
		nil,
	)
	if err != nil || !handled {
		t.Fatalf("start handled=%t err=%v", handled, err)
	}
	state := service.raids[raidKey]
	if state == nil || !state.Started || !state.EntryCostPaid || state.StartPending {
		t.Fatalf("started state=%+v", state)
	}
	if got := runtimeRaidMemberIDs(runtimeRaidState{Members: state.FrozenMembers}); !sameRuntimeRaidMembers(state.FrozenMembers, []uint16{1001, 1002}) || len(got) != 2 {
		t.Fatalf("frozen members=%v", got)
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePreparing || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("lifecycle status=%d phase=%d", state.Lifecycle.Status, state.Lifecycle.Phase)
	}

	ack, rest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketStartRaid) ||
		!bytes.Equal(ack.Body, raid.BuildRaidCommandSuccessBody()) {
		t.Fatalf("first leader packet class=%d msg=%d body=% X, want start ACK", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	costSync, rest := splitGameServerUpperPacket(t, rest)
	if costSync.Header.Classification != 0 || costSync.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("second leader packet class=%d msg=%d, want op14 cost sync", costSync.Header.Classification, costSync.Header.MsgID)
	}
	_, rest = splitRaidRefreshPacket(t, rest, raidKey)
	rest = assertRuntimeRaidBuffRegistrationPrefix(t, rest, *state)
	rest = assertRuntimePartySnapshot(t, rest, 2)
	// A roster that is really standing in the waiting area receives one extra op9
	// aggregate before the ready timer: the town co-presence frame for the mate
	// that shares the area. It is not part of the start handshake and both the
	// pre-gate and post-gate server send it identically (A/B on 2026-09-20, same
	// fixture with presence rows); it appears here only because this fixture now
	// models a roster that can actually start on the live server.
	rest = assertCurrentPartyFrameProjection(t, rest, 2, true)
	// That peer projection is a kind-0 op9, so the publish closes the frame with the
	// op153 rows again: op153 is the only carrier of the per-member HP/MP values, and
	// a frame left on the op9 shows empty health bars and blocks gate entry.
	rest = assertRuntimePartyRealtimeRows(t, rest, 2)
	rest = assertRuntimeRaidReadyTimer(t, rest, raidKey)
	// Past the pinned handshake the start appends the entry-counter refresh: the
	// charge that just happened is what 帐号日入场数 / 帐号周入场场数 / 角色周入场次数
	// render, and the client never queries those numbers, so they have to be
	// re-pushed here or every member keeps reading the pre-charge values.
	rest = assertRuntimeRaidEntryCounterRefresh(t, rest)
	if len(rest) != 0 {
		t.Fatalf("leader trailing bytes=%d", len(rest))
	}
	memberCostSync, memberRest := splitGameServerUpperPacket(t, memberConn.write.Bytes())
	if memberCostSync.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("first member packet msg=%d, want op14", memberCostSync.Header.MsgID)
	}
	_, memberRest = splitRaidRefreshPacket(t, memberRest, raidKey)
	memberRest = assertRuntimeRaidBuffRegistrationPrefix(t, memberRest, *state)
	memberRest = assertRuntimePartySnapshot(t, memberRest, 2)
	memberRest = assertCurrentPartyFrameProjection(t, memberRest, 2, true)
	memberRest = assertRuntimePartyRealtimeRows(t, memberRest, 2)
	memberRest = assertRuntimeRaidReadyTimer(t, memberRest, raidKey)
	// Every frozen member is charged, so every member gets the refresh -- not just
	// the one who issued the start command.
	memberRest = assertRuntimeRaidEntryCounterRefresh(t, memberRest)
	if len(memberRest) != 0 {
		t.Fatalf("member trailing bytes=%d", len(memberRest))
	}
	if len(state.PendingCostSync) != 0 {
		t.Fatalf("pending cost sync=%d after all online writes", len(state.PendingCostSync))
	}

	if _, ok := service.modifyRuntimeRaidInfo(1001, raid.InfoRequest{RouteOrRaidType: 2}); ok {
		t.Fatalf("frozen raid accepted metadata modification")
	}
	// A frozen roster still refuses metadata and regrouping, but leaving is not a
	// roster mutation the freeze can hold: the 86JP reference removes a leaver
	// without consulting the raid state at all (RaidManager.LeaveLocked), and
	// refusing it left the player stuck inside a raid they could not leave.
	if _, removed, _ := service.removeRuntimeRaidMember(1002); !removed {
		t.Fatalf("frozen raid refused leave")
	}
	if _, ok := service.moveRuntimeRaidMember(1001, 1002, 2); ok {
		t.Fatalf("frozen raid accepted regroup")
	}
	if _, _, ok := service.setRuntimeRaidWaiting(1002, 1); ok {
		t.Fatalf("frozen raid accepted waiting mutation")
	}

	leaderConn.write.Reset()
	memberConn.write.Reset()
	handled, err = service.handleAlignedGameCommand(
		leader,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketStartRaid),
		nil,
	)
	if err != nil || !handled {
		t.Fatalf("idempotent start handled=%t err=%v", handled, err)
	}
	replayACK, replayRest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if replayACK.Header.MsgID != uint16(dnfenum.CmdPacketStartRaid) || !bytes.Equal(replayACK.Body, []byte{1}) {
		t.Fatalf("replay first packet msg=%d body=% X", replayACK.Header.MsgID, replayACK.Body)
	}
	next, _ := splitGameServerUpperPacket(t, replayRest)
	if next.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("idempotent start replayed a completed cost sync")
	}
	for _, characterID := range []string{"1001", "1002"} {
		inventory, found, loadErr := repositories.Inventory.Load(context.Background(), characterID)
		if loadErr != nil || !found || inventory.Slots["0:0"].Count != 1 {
			t.Fatalf("inventory %s found=%t err=%v record=%+v", characterID, found, loadErr, inventory)
		}
	}
}

func TestRuntimeRaidStartRejectsOfflineRosterBeforeDurableDebit(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002"} {
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots: map[string]dnfrepo.ItemStack{
				"0:0": {ItemID: runtimeRaidEntryItemID, Count: 1},
			},
		}); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		raidAnton:          runtimeRaidBridgeTestAntonConfig(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	conn := &bufferConn{}
	leader := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 200, Type: 23}}
	service.bindGameSessionCharacter(leader, 1001)
	raidKey := uint32(200)<<16 | 1001
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:  raidKey,
		LeaderID: 1001,
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
			{CharID: 1002, GroupIndex: 1, SlotOrder: 1},
		},
	}

	handled, err := service.handleAlignedGameCommand(leader, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketStartRaid), nil)
	if err != nil || !handled {
		t.Fatalf("offline start handled=%t err=%v", handled, err)
	}
	state := service.raids[raidKey]
	if state.Started || state.StartPending || state.EntryCostPaid || len(state.FrozenMembers) != 0 {
		t.Fatalf("offline start state=%+v", state)
	}
	for _, characterID := range []string{"1001", "1002"} {
		inventory, found, loadErr := repositories.Inventory.Load(context.Background(), characterID)
		if loadErr != nil || !found || inventory.Slots["0:0"].Count != 1 {
			t.Fatalf("offline validation debited %s: found=%t err=%v record=%+v", characterID, found, loadErr, inventory)
		}
	}
}

func TestRuntimeRaidDungeonLifecycleUsesPhaseAndTokenWithoutAutomaticAdvance(t *testing.T) {
	start := time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC)
	service, raidKey := newRuntimeRaidLifecycleService(start)

	ordinary, err := service.beginRuntimeRaidDungeon(1001, 5000, 10)
	if err != nil || ordinary.RaidKey != 0 {
		t.Fatalf("ordinary binding=%+v err=%v", ordinary, err)
	}
	binding, err := service.beginRuntimeRaidDungeon(1001, 210, 11)
	if err != nil {
		t.Fatalf("begin phase-one map: %v", err)
	}
	if binding.RaidKey != raidKey || binding.Phase != runtimeRaidDungeonPhaseOne || binding.MapID != 210 || binding.LifecycleToken != 11 {
		t.Fatalf("binding=%+v", binding)
	}
	if _, err := service.beginRuntimeRaidDungeon(1001, 211, 12); !errors.Is(err, errRuntimeRaidGroupOccupied) {
		t.Fatalf("same group second map error=%v", err)
	}
	selected := runtimeRaidDungeonEventForTest(binding, 1001, runtimeRaidDungeonSelected)
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(selected); err != nil {
		t.Fatalf("selected event: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(binding, 1001, runtimeRaidDungeonCompleted)); !errors.Is(err, errRuntimeRaidDungeonEventOrder) {
		t.Fatalf("complete before load error=%v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(binding, 1001, runtimeRaidDungeonLoaded)); err != nil {
		t.Fatalf("loaded event: %v", err)
	}
	completed := runtimeRaidDungeonEventForTest(binding, 1001, runtimeRaidDungeonCompleted)
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(completed); err != nil {
		t.Fatalf("completed event: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(completed); err != nil {
		t.Fatalf("duplicate completed event: %v", err)
	}
	state := service.raids[raidKey]
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("map completion advanced phase automatically: status=%d phase=%d", state.Lifecycle.Status, state.Lifecycle.Phase)
	}
	if got := runtimeRaidCompletedMapIDs(*state, runtimeRaidDungeonPhaseOne); len(got) != 1 || got[0] != 210 {
		t.Fatalf("completed maps=%v", got)
	}
	if _, err := service.beginRuntimeRaidDungeon(1002, 210, 21); !errors.Is(err, errRuntimeRaidDungeonCompleted) {
		t.Fatalf("completed map begin error=%v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(binding, 1001, runtimeRaidDungeonAbandoned)); !errors.Is(err, errRuntimeRaidDungeonEventOrder) {
		t.Fatalf("abandon completed token error=%v", err)
	}

	retry, err := service.beginRuntimeRaidDungeon(1001, 211, 22)
	if err != nil {
		t.Fatalf("begin retryable map: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(retry, 1001, runtimeRaidDungeonAbandoned)); err != nil {
		t.Fatalf("abandon selected map: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(retry, 1001, runtimeRaidDungeonAbandoned)); err != nil {
		t.Fatalf("duplicate abandon: %v", err)
	}
	if _, err := service.beginRuntimeRaidDungeon(1001, 211, 22); !errors.Is(err, errRuntimeRaidDungeonToken) {
		t.Fatalf("reused closed token error=%v", err)
	}
	if _, err := service.beginRuntimeRaidDungeon(1001, 211, 23); err != nil {
		t.Fatalf("new token after abandon: %v", err)
	}
}

func TestRuntimeRaidExplicitTwoPhaseMovieTimerRewardAndFinalCleanup(t *testing.T) {
	start := time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC)
	service, raidKey := newRuntimeRaidLifecycleService(start)
	config := runtimeRaidBridgeTestAntonConfig()
	heart := config.Phases[1].Dungeons[2]
	heart.InitState = raid.AntonDungeonOpen
	heart.HP = 1
	config.Phases[1].Dungeons = []raid.AntonDungeonInfo{heart}
	service.raids[raidKey].AntonConfig = config
	// The phase pays the members its clears registered, so this fixture carries the
	// registration a reached settlement implies (runtimeRaidRewardEligibleMembersLocked).
	seedRuntimeRaidPhaseClearsForTest(&service.raids[raidKey].Lifecycle, 1001, 1002)
	deadline := start.Add(20 * time.Minute)
	state, err := service.setRuntimeRaidPhaseDeadline(raidKey, runtimeRaidDungeonPhaseOne, deadline)
	if err != nil {
		t.Fatalf("set phase-one deadline: %v", err)
	}
	if got := runtimeRaidPhaseElapsed(state, start.Add(3*time.Minute)); got != 3*time.Minute {
		t.Fatalf("elapsed=%s", got)
	}
	if got := runtimeRaidPhaseRemaining(state, start.Add(3*time.Minute)); got != 17*time.Minute {
		t.Fatalf("remaining=%s", got)
	}
	state, err = service.openRuntimeRaidPhaseMovie(raidKey, runtimeRaidDungeonPhaseOne, start.Add(4*time.Minute))
	if err != nil || state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("open phase-one movie state=%+v err=%v", state.Lifecycle, err)
	}
	if _, advanced, err := service.confirmRuntimeRaidPhaseMovie(1001, runtimeRaidDungeonPhaseOne, start.Add(5*time.Minute)); err != nil || advanced {
		t.Fatalf("first movie confirm advanced=%t err=%v", advanced, err)
	}
	if _, advanced, err := service.confirmRuntimeRaidPhaseMovie(1001, runtimeRaidDungeonPhaseOne, start.Add(5*time.Minute)); err != nil || advanced {
		t.Fatalf("duplicate movie confirm advanced=%t err=%v", advanced, err)
	}
	state, advanced, err := service.confirmRuntimeRaidPhaseMovie(1002, runtimeRaidDungeonPhaseOne, start.Add(6*time.Minute))
	if err != nil || !advanced || state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("phase-one reward transition advanced=%t state=%+v err=%v", advanced, state.Lifecycle, err)
	}
	if _, all, err := service.selectRuntimeRaidSettlementCard(1001, runtimeRaidDungeonPhaseOne, raid.RaidRewardCardRequest{Mode: 1, CardIndex: 2}, start.Add(7*time.Minute)); err != nil || all {
		t.Fatalf("phase-one first preview all=%t err=%v", all, err)
	}
	if _, all, err := service.selectRuntimeRaidSettlementCard(1001, runtimeRaidDungeonPhaseOne, raid.RaidRewardCardRequest{Mode: 0, CardIndex: 2}, start.Add(7*time.Minute)); err != nil || all {
		t.Fatalf("phase-one first confirm all=%t err=%v", all, err)
	}
	if _, all, err := service.selectRuntimeRaidSettlementCard(1002, runtimeRaidDungeonPhaseOne, raid.RaidRewardCardRequest{Mode: 1, CardIndex: 3}, start.Add(7*time.Minute)); err != nil || all {
		t.Fatalf("phase-one last preview all=%t err=%v", all, err)
	}
	if _, all, err := service.selectRuntimeRaidSettlementCard(1002, runtimeRaidDungeonPhaseOne, raid.RaidRewardCardRequest{Mode: 0, CardIndex: 3}, start.Add(7*time.Minute)); err != nil || !all {
		t.Fatalf("phase-one last confirm all=%t err=%v", all, err)
	}
	_, phaseOneToken, err := service.reserveRuntimeRaidRewardDelivery(raidKey, runtimeRaidDungeonPhaseOne, start.Add(7*time.Minute))
	if err != nil || phaseOneToken == 0 {
		t.Fatalf("phase-one reward reservation token=%#x err=%v", phaseOneToken, err)
	}
	if _, err := service.markRuntimeRaidRewardDeliveryCommitted(raidKey, phaseOneToken, start.Add(7*time.Minute)); err != nil {
		t.Fatalf("phase-one durable reward commit: %v", err)
	}
	state, timer, err := service.enterRuntimeRaidStandby(raidKey, runtimeRaidDungeonPhaseOne, start.Add(7*time.Minute))
	if err != nil || state.Lifecycle.Status != runtimeRaidLifecycleStandby || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne || timer.Value2 != 600 {
		t.Fatalf("phase-one standby state=%+v timer=%+v err=%v", state.Lifecycle, timer, err)
	}
	state, err = service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseTwo, start.Add(8*time.Minute), true)
	if err != nil || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo {
		t.Fatalf("phase-two activation state=%+v err=%v", state.Lifecycle, err)
	}
	// A phase pays exactly the members its clears registered, and activating it cleared
	// the phase-one counters: both members are in the phase-two roster this fixture
	// settles (1001 clears 220 below, 1002 confirms the movie).
	seedRuntimeRaidPhaseClearsForTest(&service.raids[raidKey].Lifecycle, 1001, 1002)
	if _, err := service.beginRuntimeRaidDungeon(1001, 210, 30); !errors.Is(err, errRuntimeRaidPhaseMismatch) {
		t.Fatalf("old phase map error=%v", err)
	}
	phaseTwoBinding, err := service.beginRuntimeRaidDungeon(1001, 220, 31)
	if err != nil {
		t.Fatalf("begin phase-two map: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(phaseTwoBinding, 1001, runtimeRaidDungeonLoaded)); err != nil {
		t.Fatalf("phase-two loaded: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(phaseTwoBinding, 1001, runtimeRaidDungeonCompleted)); err != nil {
		t.Fatalf("phase-two completed: %v", err)
	}
	if _, err := service.openRuntimeRaidPhaseMovie(raidKey, runtimeRaidDungeonPhaseTwo, start.Add(10*time.Minute)); err != nil {
		t.Fatalf("open phase-two movie: %v", err)
	}
	if _, _, err := service.confirmRuntimeRaidPhaseMovie(1001, runtimeRaidDungeonPhaseTwo, start.Add(11*time.Minute)); err != nil {
		t.Fatalf("phase-two first movie confirm: %v", err)
	}
	state, advanced, err = service.confirmRuntimeRaidPhaseMovie(1002, runtimeRaidDungeonPhaseTwo, start.Add(12*time.Minute))
	if err != nil || !advanced || state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards {
		t.Fatalf("reward transition advanced=%t state=%+v err=%v", advanced, state.Lifecycle, err)
	}

	if _, all, err := service.confirmRuntimeRaidRewardSelection(1001, 0, 1, 2, start.Add(14*time.Minute)); err != nil || all {
		t.Fatalf("first reward selection all=%t err=%v", all, err)
	}
	if _, all, err := service.confirmRuntimeRaidRewardSelection(1001, 0, 1, 2, start.Add(14*time.Minute)); err != nil || all {
		t.Fatalf("duplicate reward selection all=%t err=%v", all, err)
	}
	if _, all, err := service.confirmRuntimeRaidRewardSelection(1001, 0, 0, 2, start.Add(14*time.Minute)); err != nil || all {
		t.Fatalf("first reward confirmation all=%t err=%v", all, err)
	}
	if _, err := service.markRuntimeRaidRewardDeliveryCommitted(raidKey, 1, start.Add(15*time.Minute)); !errors.Is(err, errRuntimeRaidRewardToken) {
		t.Fatalf("early durable reward commit error=%v", err)
	}
	if _, all, err := service.confirmRuntimeRaidRewardSelection(1002, 0, 1, 3, start.Add(16*time.Minute)); err != nil || all {
		t.Fatalf("last reward preview all=%t err=%v", all, err)
	}
	if _, all, err := service.confirmRuntimeRaidRewardSelection(1002, 0, 0, 3, start.Add(16*time.Minute)); err != nil || !all {
		t.Fatalf("last reward selection all=%t err=%v", all, err)
	}
	_, rewardToken, err := service.reserveRuntimeRaidRewardDelivery(raidKey, runtimeRaidDungeonPhaseTwo, start.Add(16*time.Minute))
	if err != nil || rewardToken == 0 {
		t.Fatalf("phase-two reward reservation token=%#x err=%v", rewardToken, err)
	}
	if _, err := service.finalizeRuntimeRaid(raidKey, rewardToken, start.Add(17*time.Minute)); !errors.Is(err, errRuntimeRaidRewardNotReady) {
		t.Fatalf("cleanup before durable reward commit error=%v", err)
	}
	if _, err := service.markRuntimeRaidRewardDeliveryCommitted(raidKey, rewardToken, start.Add(18*time.Minute)); err != nil {
		t.Fatalf("durable reward commit: %v", err)
	}
	finished, err := service.finalizeRuntimeRaid(raidKey, rewardToken, start.Add(19*time.Minute))
	if err != nil {
		t.Fatalf("final cleanup: %v", err)
	}
	if finished.Lifecycle.Status != runtimeRaidLifecycleFinished || finished.Lifecycle.FinishedAt.IsZero() {
		t.Fatalf("finished lifecycle=%+v", finished.Lifecycle)
	}
	if _, exists := service.raids[raidKey]; exists {
		t.Fatalf("global raid retained after confirmed durable reward delivery")
	}
}

func TestRuntimeRaidSingleRouteAdvancesNativeMapGraph(t *testing.T) {
	start := time.Date(2026, 8, 6, 7, 0, 0, 0, time.UTC)
	routes := []struct {
		typ  byte
		maps []int
	}{
		{typ: 0, maps: []int{5314, 5317, 5322, 5319, 5320}},
		{typ: 3, maps: []int{5314, 5315, 5318, 5319, 5320}},
		{typ: 6, maps: []int{5314, 5316, 5321, 5319, 5320}},
	}
	for _, route := range routes {
		config := runtimeRaidBridgeTestAntonConfig()
		config.SingleMode = &raid.AntonSingleModeConfig{
			Name:             "single",
			TimeLimitSeconds: 2100,
			Phase: raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{
				{Index: 5314, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 0},
				{Index: 5315, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5316, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5317, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5318, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5321, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5322, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5319, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
				{Index: 5320, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
			}},
		}
		lifecycle, err := newRuntimeRaidLifecycleStateFromAntonModeAndType(config, start, true, route.typ)
		if err != nil {
			t.Fatalf("route %d lifecycle: %v", route.typ, err)
		}
		member := runtimeRaidMemberState{CharID: 1001, Name: "solo", GroupIndex: 1, SlotOrder: 0}
		const raidKey = uint32(0x00d00001)
		service := &Service{
			options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
			gameplayTimers: &runtimeRaidPhaseTestQueue{now: start},
			raids: map[uint32]*runtimeRaidState{raidKey: {
				RaidKey: raidKey, LeaderID: member.CharID, TailFlag: 1, SingleRaidType: route.typ,
				AntonConfig: config, Members: []runtimeRaidMemberState{member}, FrozenMembers: []runtimeRaidMemberState{member},
				Started: true, EntryCostPaid: true, StartAttempt: 1, Lifecycle: lifecycle,
			}},
		}
		active, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, lifecycle.ReadyDeadline, false)
		if err != nil {
			t.Fatalf("route %d activate: %v", route.typ, err)
		}
		if got := active.Lifecycle.PhaseDeadline.Sub(active.Lifecycle.PhaseStartedAt); got != 2100*time.Second {
			t.Fatalf("route %d timeout=%s", route.typ, got)
		}
		for index, mapID := range route.maps {
			binding, err := service.beginRuntimeRaidDungeon(member.CharID, mapID, uint64(index+1))
			if err != nil {
				t.Fatalf("route %d map %d begin: %v", route.typ, mapID, err)
			}
			for _, kind := range []runtimeRaidDungeonLifecycleKind{runtimeRaidDungeonSelected, runtimeRaidDungeonLoaded, runtimeRaidDungeonCompleted} {
				if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonLifecycleEvent{
					Kind: kind, RaidKey: binding.RaidKey, Phase: binding.Phase, MapID: binding.MapID,
					LifecycleToken: binding.LifecycleToken, CharacterID: member.CharID,
				}); err != nil {
					t.Fatalf("route %d map %d %s: %v", route.typ, mapID, kind, err)
				}
			}
		}
		state := service.raids[raidKey]
		if state == nil || !runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseOne) {
			t.Fatalf("route %d did not complete final map: %+v", route.typ, state)
		}
		if _, err := service.openRuntimeRaidPhaseMovie(raidKey, runtimeRaidDungeonPhaseOne, start.Add(time.Minute)); err != nil {
			t.Fatalf("route %d open movie: %v", route.typ, err)
		}
	}
}

func TestRuntimeRaidLukeSingleRoutesAdvanceOneNativeObjectiveAtATime(t *testing.T) {
	start := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	luke := runtimeRaidBridgeTestLukeConfig()
	routes := []struct {
		typ  byte
		maps []int
	}{
		{typ: runtimeRaidLukeLightRoute, maps: []int{5323, 5324, 5325, 5326, 5327, 5328}},
		{typ: runtimeRaidLukeDarkRoute, maps: []int{5329, 5330, 5331, 5332, 5333, 5334}},
	}
	for _, route := range routes {
		lifecycle, err := newRuntimeRaidLifecycleStateFromModeAndType(luke.Raid, runtimeRaidOwnerLuke, start, true, route.typ)
		if err != nil {
			t.Fatalf("route %d lifecycle: %v", route.typ, err)
		}
		if got := lifecycle.Objectives[route.maps[0]].State; got != raid.AntonDungeonOpen {
			t.Fatalf("route %d first state=%v want open", route.typ, got)
		}
		if got := lifecycle.Objectives[route.maps[1]].State; got != raid.AntonDungeonClosed {
			t.Fatalf("route %d second state=%v want closed", route.typ, got)
		}
		member := runtimeRaidMemberState{CharID: 1001, Name: "solo", GroupIndex: 1, SlotOrder: 0}
		raidKey := uint32(0x00200000) | uint32(route.typ)
		service := &Service{
			options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
			gameplayTimers: &runtimeRaidPhaseTestQueue{now: lifecycle.ReadyDeadline},
			raids: map[uint32]*runtimeRaidState{raidKey: {
				RaidKey: raidKey, LeaderID: member.CharID, RaidMode: runtimeRaidOwnerLuke,
				TailFlag: 1, SingleRaidType: route.typ, LukeConfig: luke,
				Members: []runtimeRaidMemberState{member}, FrozenMembers: []runtimeRaidMemberState{member},
				Started: true, EntryCostPaid: true, StartAttempt: 1, Lifecycle: lifecycle,
			}},
		}
		active, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, lifecycle.ReadyDeadline, false)
		if err != nil {
			t.Fatalf("route %d activate: %v", route.typ, err)
		}
		if got := active.Lifecycle.PhaseDeadline.Sub(active.Lifecycle.PhaseStartedAt); got != 1800*time.Second {
			t.Fatalf("route %d timeout=%s want=30m", route.typ, got)
		}
		for index, mapID := range route.maps {
			binding, beginErr := service.beginRuntimeRaidDungeon(member.CharID, mapID, uint64(index+1))
			if beginErr != nil {
				t.Fatalf("route %d map %d begin: %v", route.typ, mapID, beginErr)
			}
			for _, kind := range []runtimeRaidDungeonLifecycleKind{runtimeRaidDungeonSelected, runtimeRaidDungeonLoaded, runtimeRaidDungeonCompleted} {
				if eventErr := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonLifecycleEvent{
					Kind: kind, RaidKey: binding.RaidKey, Phase: binding.Phase, MapID: binding.MapID,
					LifecycleToken: binding.LifecycleToken, CharacterID: member.CharID,
				}); eventErr != nil {
					t.Fatalf("route %d map %d %s: %v", route.typ, mapID, kind, eventErr)
				}
			}
			state := service.raids[raidKey]
			if got := state.Lifecycle.Objectives[mapID].State; got != raid.AntonDungeonHide {
				t.Fatalf("route %d completed map %d state=%v want hidden", route.typ, mapID, got)
			}
			if index+1 < len(route.maps) {
				if got := state.Lifecycle.Objectives[route.maps[index+1]].State; got != raid.AntonDungeonOpen {
					t.Fatalf("route %d next map %d state=%v want open", route.typ, route.maps[index+1], got)
				}
			}
			if index+2 < len(route.maps) {
				if got := state.Lifecycle.Objectives[route.maps[index+2]].State; got != raid.AntonDungeonClosed {
					t.Fatalf("route %d preview map %d state=%v want closed", route.typ, route.maps[index+2], got)
				}
			}
		}
		if state := service.raids[raidKey]; state == nil || !runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseOne) {
			t.Fatalf("route %d did not complete terminal objective: %+v", route.typ, state)
		}
	}
}

func TestRuntimeRaidDungeonReservationIsConcurrentAndSingleOwner(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	const contenders = 32
	var successes atomic.Int32
	var wait sync.WaitGroup
	wait.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func(token uint64) {
			defer wait.Done()
			if binding, err := service.beginRuntimeRaidDungeon(1001, 212, token); err == nil && binding.RaidKey == raidKey {
				successes.Add(1)
			}
		}(uint64(i + 1))
	}
	wait.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful concurrent reservations=%d, want 1", got)
	}
	state := service.raids[raidKey]
	if len(state.Lifecycle.GroupOccupancy) != 1 || len(state.Lifecycle.Dungeons) != 1 {
		t.Fatalf("occupancy=%+v dungeons=%+v", state.Lifecycle.GroupOccupancy, state.Lifecycle.Dungeons)
	}
}

// The attack window expiring does not tear the raid down here: 86JP marks it failed
// (state 4 / argument 1) and hands it to a ten-second remover, so the members still
// get their result screen (RaidHandler.Runtime.cs:592-762). The release itself is
// removeRuntimeRaidFailedPhase.
func TestRuntimeRaidPhaseExpiryRequiresDeadlineAndFailsBeforeRelease(t *testing.T) {
	start := time.Date(2026, 8, 6, 3, 0, 0, 0, time.UTC)
	service, raidKey := newRuntimeRaidLifecycleService(start)
	if _, err := service.expireRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, start.Add(time.Minute)); !errors.Is(err, errRuntimeRaidLifecycleState) {
		t.Fatalf("expiry without deadline error=%v", err)
	}
	deadline := start.Add(10 * time.Minute)
	if _, err := service.setRuntimeRaidPhaseDeadline(raidKey, runtimeRaidDungeonPhaseOne, deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := service.expireRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, deadline.Add(-time.Second)); !errors.Is(err, errRuntimeRaidLifecycleState) {
		t.Fatalf("early expiry error=%v", err)
	}
	failed, err := service.expireRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, deadline)
	if err != nil {
		t.Fatalf("expire at deadline: %v", err)
	}
	if failed.Lifecycle.Status != runtimeRaidLifecycleFailed || !failed.Lifecycle.FailedAt.Equal(deadline) {
		t.Fatalf("failed lifecycle=%+v", failed.Lifecycle)
	}
	if _, exists := service.raids[raidKey]; !exists {
		t.Fatal("the failed raid was released before its result window")
	}
	service.removeRuntimeRaidFailedPhase(raidKey, runtimeRaidDungeonPhaseOne)
	if _, exists := service.raids[raidKey]; exists {
		t.Fatal("the failed raid was not cleaned up")
	}
}

func newRuntimeRaidLifecycleService(start time.Time) (*Service, uint32) {
	const raidKey = uint32(0x00c803e9)
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "member", GroupIndex: 2, SlotOrder: 0},
	}
	state := &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002},
		StartAttempt:   1,
		Lifecycle:      newRuntimeRaidLifecycleState(start),
	}
	return &Service{raids: map[uint32]*runtimeRaidState{raidKey: state}}, raidKey
}

func runtimeRaidDungeonEventForTest(binding runtimeRaidDungeonBinding, characterID uint16, kind runtimeRaidDungeonLifecycleKind) runtimeRaidDungeonLifecycleEvent {
	return runtimeRaidDungeonLifecycleEvent{
		Kind:           kind,
		RaidKey:        binding.RaidKey,
		Phase:          binding.Phase,
		MapID:          binding.MapID,
		LifecycleToken: binding.LifecycleToken,
		CharacterID:    characterID,
	}
}

func runtimeRaidBridgeTestAntonConfig() *raid.AntonConfig {
	return &raid.AntonConfig{
		PhaseMax:                  2,
		MemberMax:                 20,
		StartMinimumMembers:       1,
		SingleStartMinimumMembers: 1,
		StartDelaySeconds:         3,
		PhaseTimeoutSeconds:       []int{2400, 2400},
		PhaseBreakSeconds:         600,
		MovieEndSeconds:           []int{20, 20, 20, 20},
		EntryCost:                 raid.AntonEntryCost{ItemID: runtimeRaidEntryItemID, Count: runtimeRaidEntryCount},
		Phases: []raid.AntonPhase{
			{Dungeons: []raid.AntonDungeonInfo{
				{Index: 210, InitState: raid.AntonDungeonOpen, HP: 4, PartyMax: 5},
				{Index: 211, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 212, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 213, InitState: raid.AntonDungeonHide, HP: 2, PartyMax: 5},
				{Index: 214, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 215, InitState: raid.AntonDungeonHide, HP: 2, PartyMax: 5},
				{Index: 216, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 1},
			}},
			{Dungeons: []raid.AntonDungeonInfo{
				{Index: 218, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 5},
				{Index: 219, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 5},
				{Index: 220, InitState: raid.AntonDungeonHide, HP: 5, PartyMax: 5},
				{Index: 221, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 222, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 223, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 224, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
			}},
		},
		SingleMode: &raid.AntonSingleModeConfig{
			Name:             "single",
			TimeLimitSeconds: 1800,
			Phase: raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{
				{Index: 5320, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 0},
			}},
		},
	}
}

func assertRuntimeRaidReadyTimer(t *testing.T, stream []byte, _ uint32) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidStateMsgID ||
		!bytes.Equal(packet.Body, raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidPrepareState, Argument: 0})) {
		t.Fatalf("ready prepare state header=%+v body=%x", packet.Header, packet.Body)
	}
	packet, rest = splitGameServerUpperPacket(t, rest)
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidTimerMsgID || len(packet.Body) != 13 {
		t.Fatalf("ready timer header=%+v body=%x", packet.Header, packet.Body)
	}
	if packet.Body[0] != 0 || binary.LittleEndian.Uint32(packet.Body[1:5]) != runtimeRaidTopTimerKey ||
		binary.LittleEndian.Uint32(packet.Body[5:9]) == 0 || binary.LittleEndian.Uint32(packet.Body[9:13]) != 3 {
		t.Fatalf("ready timer body=%x", packet.Body)
	}
	remain, rest := splitGameServerUpperPacket(t, rest)
	if remain.Header.Classification != 0 || remain.Header.MsgID != raid.RaidRemainTimeMsgID || len(remain.Body) != 5 ||
		remain.Body[0] != 0 || binary.LittleEndian.Uint32(remain.Body[1:]) != 3 {
		t.Fatalf("ready remaining header=%+v body=%x", remain.Header, remain.Body)
	}
	prepare, rest := splitGameServerUpperPacket(t, rest)
	if prepare.Header.Classification != 0 || prepare.Header.MsgID != raid.RaidPrepareStartMsgID || len(prepare.Body) != 0 {
		t.Fatalf("ready prepare header=%+v body=%x", prepare.Header, prepare.Body)
	}
	return rest
}

func assertRuntimeRaidBuffRegistrationPrefix(t *testing.T, stream []byte, state runtimeRaidState) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	wantBody := mustRuntimeRaidBuffRegistrationSnapshotBody(t, state)
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidBuffMsgID ||
		!bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("raid buff header=%+v body=%x want=%x", packet.Header, packet.Body, wantBody)
	}
	return rest
}

// assertRuntimeRaidEntryCounterRefresh consumes the tail a successful start
// appends AFTER the pinned handshake: the op1303 account rows, then one op1206
// record per raid mode (Anton 181, Luke 282, Fiend War 2449).
//
// The client never queries these counters -- its op1206 handler writes them onto
// the per-mode window element and the window only re-renders that stored state --
// so this tail is the only thing that makes 帐号日入场数 / 帐号周入场场数 /
// 角色周入场次数 move at the moment a start charges them, instead of at the next
// relog. Pinning the event ids here keeps that contract honest.
func assertRuntimeRaidEntryCounterRefresh(t *testing.T, stream []byte) []byte {
	t.Helper()
	daily, rest := splitGameServerUpperPacket(t, stream)
	if daily.Header.Classification != 0 || daily.Header.MsgID != currentRaidDailyAvailabilityMsgID {
		t.Fatalf("entry counter daily header=%+v body=%x", daily.Header, daily.Body)
	}
	for _, eventID := range []uint32{currentAntonRaidEventID, currentLukeRaidEventID, currentFiendWarEventID} {
		packet, next := splitGameServerUpperPacket(t, rest)
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentSpendTimeProgressMsgID {
			t.Fatalf("entry counter weekly event=%d header=%+v", eventID, packet.Header)
		}
		if len(packet.Body) < 5 || binary.LittleEndian.Uint32(packet.Body[:4]) != eventID || packet.Body[4] != 0 {
			t.Fatalf("entry counter weekly event=%d body=%x", eventID, packet.Body)
		}
		rest = next
	}
	return rest
}
