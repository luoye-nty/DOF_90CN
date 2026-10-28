package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

func TestRuntimeRaidPhaseSettlementPublishesResultBeforeState3(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	now := time.Date(2026, 8, 6, 4, 0, 0, 0, time.UTC)
	result := raid.RaidSettlementResult{
		ResultType:   2,
		PhaseIndex:   0,
		ClearSeconds: 321,
		// DeathCount is owned by the phase ledger, not by the caller: publishing
		// overwrites it with the accepted-op40 count for this raid and phase
		// (raid_phase_death_count.go). This fixture recorded no deaths, so the
		// body must carry 0 whatever the caller passes.
		DeathCount: 4,
		Score:      5,
		Flags:      6,
	}
	if err := service.publishRuntimeRaidPhaseSettlement(raidKey, runtimeRaidDungeonPhaseOne, result, now); err != nil {
		t.Fatalf("publish phase settlement: %v", err)
	}
	if got := service.runtimeRaidPhaseDeathCount(raidKey, runtimeRaidDungeonPhaseOne); got != 0 {
		t.Fatalf("fixture unexpectedly recorded %d deaths", got)
	}
	result.DeathCount = 0
	wantResult, err := raid.BuildRaidSettlementResultBody(result)
	if err != nil {
		t.Fatalf("build expected result: %v", err)
	}
	for _, test := range []struct {
		name    string
		session *gameSession
	}{
		{name: "leader", session: leader},
		{name: "member", session: member},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := test.session.conn.(*bufferConn)
			resultPacket, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
			if resultPacket.Header.Classification != 0 || resultPacket.Header.MsgID != raid.RaidResultMsgID || !bytes.Equal(resultPacket.Body, wantResult) {
				t.Fatalf("result packet class=%d msg=%#04x body=% X", resultPacket.Header.Classification, resultPacket.Header.MsgID, resultPacket.Body)
			}
			statePacket, rest := splitGameServerUpperPacket(t, rest)
			if statePacket.Header.Classification != 0 || statePacket.Header.MsgID != raid.RaidStateMsgID || !bytes.Equal(statePacket.Body, raid.BuildRaidStateBody(raid.RaidState{State: 3})) {
				t.Fatalf("state packet class=%d msg=%#04x body=% X", statePacket.Header.Classification, statePacket.Header.MsgID, statePacket.Body)
			}
			if len(rest) != 0 {
				t.Fatalf("trailing bytes=%d", len(rest))
			}
		})
	}
	state := service.raids[raidKey]
	if state == nil || state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("lifecycle after settlement=%+v", state)
	}
}

func TestRuntimeRaidPhaseSettlementRejectsWirePhaseMismatchBeforeMutation(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 1},
		time.Now().UTC(),
	)
	if !errors.Is(err, errRuntimeRaidSettlementPhaseResult) {
		t.Fatalf("phase mismatch error=%v", err)
	}
	if leader.conn.(*bufferConn).write.Len() != 0 || member.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("phase mismatch wrote packets")
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecyclePhaseActive {
		t.Fatalf("phase mismatch mutated lifecycle status=%d", got)
	}
}

func TestRuntimeRaidPhaseSettlementRejectsInvalidResultBeforeMutation(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{ResultType: 4, PhaseIndex: 0},
		time.Now().UTC(),
	)
	if err == nil {
		t.Fatal("invalid settlement result was accepted")
	}
	if leader.conn.(*bufferConn).write.Len() != 0 || member.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("invalid settlement result wrote packets")
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecyclePhaseActive {
		t.Fatalf("invalid settlement result mutated lifecycle status=%d", got)
	}
}

func TestRuntimeRaidStage0AckPrecedesMovieNotification(t *testing.T) {
	service, leader, _, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0},
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("open movie: %v", err)
	}
	conn := leader.conn.(*bufferConn)
	conn.write.Reset()

	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(1),
		raid.RaidResultStageRequestMsgID,
		[]byte{0},
	)
	if err != nil || !handled {
		t.Fatalf("stage0 handled=%t err=%v", handled, err)
	}
	ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != raid.RaidResultStageRequestMsgID || len(ack.Body) != 0 {
		t.Fatalf("stage0 ACK class=%d msg=%#04x body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	movie, rest := splitGameServerUpperPacket(t, rest)
	if movie.Header.Classification != 0 || movie.Header.MsgID != raid.RaidMovieMsgID || len(movie.Body) != 0 {
		t.Fatalf("movie class=%d msg=%#04x body=% X", movie.Header.Classification, movie.Header.MsgID, movie.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("stage0 trailing bytes=%d", len(rest))
	}
}

func TestRuntimeRaidMovieCompletionPublishesState4CountdownThenState5(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	queue := &runtimeRaidPhaseTestQueue{now: now}
	service.gameplayTimers = queue
	service.raids[raidKey].Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	service.raids[raidKey].Lifecycle.RewardOpenedAt = now
	state := cloneRuntimeRaidState(*service.raids[raidKey])
	if err := service.broadcastRuntimeRaidMovieComplete(state); err != nil {
		t.Fatalf("broadcast movie complete: %v", err)
	}
	for _, session := range []*gameSession{leader, member} {
		conn := session.conn.(*bufferConn)
		assertRuntimeRaidMovieCountdownPackets(t, conn.write.Bytes())
	}
	if queue.scheduledDelay != runtimeRaidRewardOpenCountdown || queue.callback == nil {
		t.Fatalf("cards-open schedule delay=%s callback_nil=%t", queue.scheduledDelay, queue.callback == nil)
	}
	leaderBefore := leader.conn.(*bufferConn).write.Len()
	memberBefore := member.conn.(*bufferConn).write.Len()
	queue.callback(now.Add(runtimeRaidRewardOpenCountdown))
	assertRuntimeRaidCardsOpenedPacket(t, leader.conn.(*bufferConn).write.Bytes()[leaderBefore:])
	assertRuntimeRaidCardsOpenedPacket(t, member.conn.(*bufferConn).write.Bytes()[memberBefore:])
	// Opening the cards is also the moment the window's one automatic settlement
	// is armed: a member that never picks must not hold the whole squad.
	if got := queue.scheduledName; got != "raid-reward-auto-flip:13108201:1:1" {
		t.Fatalf("auto-flip schedule name=%q", got)
	}
	if queue.scheduledDelay != runtimeRaidRewardAutoFlipDelay || queue.callback == nil {
		t.Fatalf("auto-flip schedule delay=%s callback_nil=%t", queue.scheduledDelay, queue.callback == nil)
	}
}

func TestRuntimeRaidStage1AckPrecedesRosterState4Countdown(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	state := cloneRuntimeRaidState(*service.raids[raidKey])
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	service.raids[raidKey].Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	owner := runtimeRaidSettlementMovieOwnerFunc(func(characterID uint16, phase byte, now time.Time) (runtimeRaidState, bool, error) {
		if characterID != 1001 || phase != runtimeRaidDungeonPhaseOne || now.IsZero() {
			t.Fatalf("movie owner character=%d phase=%d now=%v", characterID, phase, now)
		}
		return state, true, nil
	})

	if err := service.handleRuntimeRaidResultStageWithOwner(leader, []byte{1}, owner); err != nil {
		t.Fatalf("stage1: %v", err)
	}
	leaderConn := leader.conn.(*bufferConn)
	ack, rest := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != raid.RaidResultStageRequestMsgID || len(ack.Body) != 0 {
		t.Fatalf("stage1 ACK class=%d msg=%#04x body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	assertRuntimeRaidMovieCountdownPackets(t, rest)
	assertRuntimeRaidMovieCountdownPackets(t, member.conn.(*bufferConn).write.Bytes())
}

func TestRuntimeRaidStage1ProductionOwnerWaitsForFrozenRosterOnSamePhase(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	service.raids[raidKey].Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie

	if err := service.handleRuntimeRaidResultStage(leader, []byte{1}); err != nil {
		t.Fatalf("leader stage1: %v", err)
	}
	leaderAck, rest := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if leaderAck.Header.MsgID != raid.RaidResultStageRequestMsgID || len(leaderAck.Body) != 0 || len(rest) != 0 {
		t.Fatalf("leader stage1 ACK msg=%#04x body=% X trailing=%d", leaderAck.Header.MsgID, leaderAck.Body, len(rest))
	}
	if member.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("first stage1 confirmation broadcast before frozen roster completed")
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("first confirmation status=%d want awaiting movie", got)
	}
	leader.conn.(*bufferConn).write.Reset()

	if err := service.handleRuntimeRaidResultStage(member, []byte{1}); err != nil {
		t.Fatalf("member stage1: %v", err)
	}
	memberAck, rest := splitGameServerUpperPacket(t, member.conn.(*bufferConn).write.Bytes())
	if memberAck.Header.MsgID != raid.RaidResultStageRequestMsgID || len(memberAck.Body) != 0 {
		t.Fatalf("member stage1 ACK msg=%#04x body=% X", memberAck.Header.MsgID, memberAck.Body)
	}
	assertRuntimeRaidMovieCountdownPackets(t, rest)
	assertRuntimeRaidMovieCountdownPackets(t, leader.conn.(*bufferConn).write.Bytes())
	state := service.raids[raidKey]
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("movie roster completion status=%d phase=%d", state.Lifecycle.Status, state.Lifecycle.Phase)
	}
}

func TestRuntimeRaidStage1AckWriteFailureRemainsRetryableAfterRosterConfirmation(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	service.raids[raidKey].Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie

	if err := service.handleRuntimeRaidResultStage(leader, []byte{1}); err != nil {
		t.Fatalf("leader stage1: %v", err)
	}
	leader.conn.(*bufferConn).write.Reset()

	wantWriteErr := errors.New("stage1 ACK write failed")
	failing := &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}
	member.conn = failing
	if err := service.handleRuntimeRaidResultStage(member, []byte{1}); !errors.Is(err, wantWriteErr) {
		t.Fatalf("last member stage1 write error=%v want=%v", err, wantWriteErr)
	}
	if failing.write.Len() != 0 {
		t.Fatalf("failed stage1 ACK partially wrote=%d bytes", failing.write.Len())
	}
	stateAfterFailure := service.raids[raidKey]
	if stateAfterFailure.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		stateAfterFailure.Lifecycle.Phase != runtimeRaidDungeonPhaseOne ||
		!stateAfterFailure.Lifecycle.MovieConfirmed[1001] ||
		!stateAfterFailure.Lifecycle.MovieConfirmed[1002] {
		t.Fatalf("state after failed ACK=%+v", stateAfterFailure.Lifecycle)
	}
	revisionAfterFailure := stateAfterFailure.Lifecycle.Revision

	retryConn := &bufferConn{}
	member.conn = retryConn
	if err := service.handleRuntimeRaidResultStage(member, []byte{1}); err != nil {
		t.Fatalf("last member stage1 retry: %v", err)
	}
	if got := service.raids[raidKey].Lifecycle.Revision; got != revisionAfterFailure {
		t.Fatalf("stage1 retry changed lifecycle revision=%d want=%d", got, revisionAfterFailure)
	}
	retryAck, rest := splitGameServerUpperPacket(t, retryConn.write.Bytes())
	if retryAck.Header.Classification != dnfproto.DefaultChannelClassification ||
		retryAck.Header.MsgID != raid.RaidResultStageRequestMsgID || len(retryAck.Body) != 0 {
		t.Fatalf("retry ACK class=%d msg=%#04x body=% X", retryAck.Header.Classification, retryAck.Header.MsgID, retryAck.Body)
	}
	assertRuntimeRaidMovieCountdownPackets(t, rest)
	assertRuntimeRaidMovieCountdownPackets(t, leader.conn.(*bufferConn).write.Bytes())
}

func TestRuntimeRaidRewardCardPublishesAfterDurableCommitThenStandbyTimer(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	state := cloneRuntimeRaidState(*service.raids[raidKey])
	cardOwner := &runtimeRaidSettlementCardOwnerStub{state: state}
	endUnix := uint32(0x11223344)
	timer := raid.RaidTimer{TimerType: 1, Value0: raidKey, Value1: endUnix, Value2: 600}
	rewards := raid.RaidSettlementRewards{
		ResultType: 1,
		Members: []raid.RaidSettlementMemberRewards{
			{
				MemberKey: 1001,
				CardIndex: 7,
				Rewards: []raid.RaidSettlementReward{
					{Kind: 0, ItemIDOrValue: 10093974, CountOrAux: 5},
				},
			},
			{MemberKey: 1002, CardIndex: 8},
		},
	}
	rewardOwner := &runtimeRaidSettlementRewardOwnerStub{
		state:   state,
		token:   0x55667788,
		rewards: rewards,
		timer:   &timer,
	}

	if err := service.handleRuntimeRaidRewardCardWithOwners(
		leader,
		[]byte{1, 7},
		cardOwner,
		rewardOwner,
	); err != nil {
		t.Fatalf("mode1: %v", err)
	}
	mode1Ack, rest := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if mode1Ack.Header.MsgID != raid.RaidRewardCardRequestMsgID || !bytes.Equal(mode1Ack.Body, []byte{1}) || len(rest) != 0 {
		t.Fatalf("mode1 ACK msg=%#04x body=% X trailing=%d", mode1Ack.Header.MsgID, mode1Ack.Body, len(rest))
	}
	if rewardOwner.commits != 0 || rewardOwner.completions != 0 {
		t.Fatalf("mode1 invoked reward delivery commit=%d complete=%d", rewardOwner.commits, rewardOwner.completions)
	}
	leader.conn.(*bufferConn).write.Reset()

	if err := service.handleRuntimeRaidRewardCardWithOwners(
		leader,
		[]byte{0, 7},
		cardOwner,
		rewardOwner,
	); err != nil {
		t.Fatalf("mode0: %v", err)
	}
	if rewardOwner.commits != 1 || rewardOwner.completions != 1 || rewardOwner.completedToken != rewardOwner.token {
		t.Fatalf("reward owner commit=%d complete=%d token=%#x", rewardOwner.commits, rewardOwner.completions, rewardOwner.completedToken)
	}
	// The reveal board is scoped to the recipient's own squad: 1001 is squad 1 and
	// 1002 is squad 2 in this fixture, so each receives a body holding only its own
	// row of the same settlement and never the other squad's card.
	wantLeaderRewards, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: rewards.ResultType,
		Members:    []raid.RaidSettlementMemberRewards{rewards.Members[0]},
	})
	if err != nil {
		t.Fatalf("build expected leader rewards: %v", err)
	}
	wantMemberRewards, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: rewards.ResultType,
		Members:    []raid.RaidSettlementMemberRewards{rewards.Members[1]},
	})
	if err != nil {
		t.Fatalf("build expected member rewards: %v", err)
	}
	if bytes.Equal(wantLeaderRewards, wantMemberRewards) {
		t.Fatal("the fixture's two squads produced one shared reward body")
	}
	// The whole-raid team row follows both reveals unchanged: one resultType-3 row
	// per settled member (a member with no resolved row contributes none), sent to
	// every member. Both squads must receive the identical body, which is what makes
	// the attack-party board complete again (see runtimeRaidTeamSquadRewardBody).
	teamRows := make([]raid.RaidSettlementMemberRewards, 0, len(rewards.Members))
	for _, row := range rewards.Members {
		if len(row.Rewards) == 0 {
			continue
		}
		teamRows = append(teamRows, raid.RaidSettlementMemberRewards{
			MemberKey: row.MemberKey,
			CardIndex: 2,
			Rewards:   []raid.RaidSettlementReward{row.Rewards[len(row.Rewards)-1]},
		})
	}
	wantTeamRewards, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: runtimeRaidTeamRewardResultType,
		Members:    teamRows,
	})
	if err != nil {
		t.Fatalf("build expected team rewards: %v", err)
	}
	wantTimer := raid.BuildRaidTimerBody(timer)

	mode0Ack, rest := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if mode0Ack.Header.Classification != dnfproto.DefaultChannelClassification || mode0Ack.Header.MsgID != raid.RaidRewardCardRequestMsgID || !bytes.Equal(mode0Ack.Body, []byte{1}) {
		t.Fatalf("mode0 ACK class=%d msg=%#04x body=% X", mode0Ack.Header.Classification, mode0Ack.Header.MsgID, mode0Ack.Body)
	}
	assertRuntimeRaidRewardAndTimerPackets(t, rest, wantLeaderRewards, wantTeamRewards, wantTimer)
	assertRuntimeRaidRewardAndTimerPackets(t, member.conn.(*bufferConn).write.Bytes(), wantMemberRewards, wantTeamRewards, wantTimer)
}

// TestRuntimeRaidTeamRewardBoardIsRaisedForEverySquadShape pins the whole-raid
// board to the reference's condition, which is none: 86JP broadcasts the
// resultType-3 attack-party row to every member of the eligible roster whatever
// the party layout is (RaidHandler.Rewards.cs:602-622). Live 2026-09-17T12:46Z,
// the phase-two settlement of raid 1507329 skipped it because its two members had
// merged onto the default squad, and the 团队展示 stage of that flip had no board
// behind it.
func TestRuntimeRaidTeamRewardBoardIsRaisedForEverySquadShape(t *testing.T) {
	delivery := func(groups ...byte) runtimeRaidSettlementRewardDelivery {
		state := runtimeRaidState{RaidKey: 0x00c803e9}
		rewards := raid.RaidSettlementRewards{ResultType: 1}
		for index, group := range groups {
			characterID := uint16(1001 + index)
			state.FrozenMembers = append(state.FrozenMembers, runtimeRaidMemberState{
				CharID:     characterID,
				GroupIndex: group,
			})
			rewards.Members = append(rewards.Members, raid.RaidSettlementMemberRewards{
				MemberKey: characterID,
				CardIndex: byte(index),
				Rewards: []raid.RaidSettlementReward{
					{Kind: 0, ItemIDOrValue: 10093974, CountOrAux: 5},
					{Kind: 0, ItemIDOrValue: 3330, CountOrAux: 150},
				},
			})
		}
		return runtimeRaidSettlementRewardDelivery{Raid: state, Token: 1, Rewards: rewards}
	}
	shapes := []struct {
		name   string
		groups []byte
	}{
		{name: "two squads", groups: []byte{1, 2}},
		{name: "one squad", groups: []byte{1, 1}},
		{name: "both unassigned", groups: []byte{0, 0}},
		{name: "unassigned beside one", groups: []byte{0, 1}},
	}
	for _, shape := range shapes {
		body, ok, err := runtimeRaidTeamSquadRewardBody(delivery(shape.groups...))
		if err != nil {
			t.Fatalf("%s: %v", shape.name, err)
		}
		if !ok {
			t.Fatalf("%s: the attack-party board was skipped", shape.name)
		}
		if len(body) < 2 || body[0] != runtimeRaidTeamRewardResultType || int(body[1]) != len(shape.groups) {
			t.Fatalf("%s: body=% X", shape.name, body)
		}
	}
	if _, ok, err := runtimeRaidTeamSquadRewardBody(runtimeRaidSettlementRewardDelivery{}); err != nil || ok {
		t.Fatalf("empty delivery ok=%v err=%v", ok, err)
	}
}

func TestRuntimeRaidRewardPublicationStopsWhenRecipientCharacterGenerationChanges(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("commit rewards: %v", err)
	}
	if len(delivery.InventoryRefreshes) == 0 {
		t.Fatal("committed delivery has no inventory refreshes")
	}

	connection := &runtimeRaidResultGenerationConn{}
	fixture.leader.conn = connection
	connection.afterFirstWrite = func() {
		fixture.service.bindGameSessionCharacter(fixture.leader, 1999)
	}
	publication := &runtimeRaidRewardPublication{
		RewardListMembers:   make(map[uint16]bool),
		StandbyTimerMembers: make(map[uint16]bool),
	}
	if err := fixture.service.publishRuntimeRaidRewardListLocked(
		delivery,
		delivery.RewardBody,
		publication,
	); err != nil {
		t.Fatalf("publish rewards across generation change: %v", err)
	}
	if publication.RewardListMembers[1001] {
		t.Fatal("stale leader generation was marked as published")
	}
	if !publication.RewardListMembers[1002] {
		t.Fatal("unchanged member generation was not published")
	}
	first, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID == raid.RaidRewardListMsgID || len(trailing) != 0 {
		t.Fatalf("stale generation received reward list: first=%#04x trailing=%x", first.Header.MsgID, trailing)
	}

	replacement := &gameSession{conn: &bufferConn{}, channel: fixture.leader.channel}
	fixture.service.bindGameSessionCharacter(replacement, 1001)
	if err := fixture.service.publishRuntimeRaidRewardListLocked(
		delivery,
		delivery.RewardBody,
		publication,
	); err != nil {
		t.Fatalf("retry reward publication on replacement session: %v", err)
	}
	if !publication.RewardListMembers[1001] {
		t.Fatal("replacement session reward publication was not recorded")
	}
	raw := replacement.conn.(*bufferConn).write.Bytes()
	packetIDs := make([]uint16, 0, 2)
	for len(raw) != 0 {
		var packet dnfproto.ChannelPacket
		packet, raw = splitGameServerUpperPacket(t, raw)
		packetIDs = append(packetIDs, packet.Header.MsgID)
	}
	if len(packetIDs) < 2 || packetIDs[len(packetIDs)-1] != raid.RaidRewardListMsgID {
		t.Fatalf("replacement session packets=%v, want inventory refresh then reward list", packetIDs)
	}
}

func TestRuntimeRaidRewardCardCommitFailureReturnsFailureAndRemainsRetryable(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	state := cloneRuntimeRaidState(*service.raids[raidKey])
	cardOwner := &runtimeRaidSettlementCardOwnerStub{state: state}
	wantCommitErr := errors.New("reward commit failed")
	rewardOwner := &runtimeRaidSettlementRewardOwnerStub{
		state:     state,
		token:     0x10203040,
		commitErr: wantCommitErr,
	}

	if err := service.handleRuntimeRaidRewardCardWithOwners(leader, []byte{1, 7}, cardOwner, rewardOwner); err != nil {
		t.Fatalf("mode1: %v", err)
	}
	leader.conn.(*bufferConn).write.Reset()
	if err := service.handleRuntimeRaidRewardCardWithOwners(leader, []byte{0, 7}, cardOwner, rewardOwner); err != nil {
		t.Fatalf("failed mode0 commit ACK: %v", err)
	}
	failure, rest := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if failure.Header.MsgID != raid.RaidRewardCardRequestMsgID ||
		!bytes.Equal(failure.Body, []byte{0, runtimeRaidFailureCode}) || len(rest) != 0 {
		t.Fatalf("commit failure packet msg=%#04x body=% X trailing=%d", failure.Header.MsgID, failure.Body, len(rest))
	}
	if rewardOwner.commits != 1 || rewardOwner.completions != 0 || member.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("failed commit calls=%d/%d member_bytes=%d", rewardOwner.commits, rewardOwner.completions, member.conn.(*bufferConn).write.Len())
	}

	rewardOwner.commitErr = nil
	leader.conn.(*bufferConn).write.Reset()
	if err := service.handleRuntimeRaidRewardCardWithOwners(leader, []byte{0, 7}, cardOwner, rewardOwner); err != nil {
		t.Fatalf("mode0 retry: %v", err)
	}
	success, rest := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if success.Header.MsgID != raid.RaidRewardCardRequestMsgID || !bytes.Equal(success.Body, []byte{1}) {
		t.Fatalf("retry ACK msg=%#04x body=% X", success.Header.MsgID, success.Body)
	}
	rewardList, rest := splitGameServerUpperPacket(t, rest)
	if rewardList.Header.MsgID != raid.RaidRewardListMsgID || len(rest) != 0 {
		t.Fatalf("retry reward msg=%#04x trailing=%d", rewardList.Header.MsgID, len(rest))
	}
	if rewardOwner.commits != 2 || rewardOwner.completions != 1 {
		t.Fatalf("retry commit calls=%d/%d", rewardOwner.commits, rewardOwner.completions)
	}
}

func TestRuntimeRaidBehaviorAndMessageAreParsedWithoutAck(t *testing.T) {
	service, leader, _, _ := newRuntimeRaidResultFlowBridgeService(t)
	conn := leader.conn.(*bufferConn)

	// 661 is a behavior report: parsed and answered with nothing, as before.
	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(1),
		raid.RaidDoBehaviorRequestMsgID,
		[]byte{1, 0, 0, 0, 2, 0},
	)
	if err != nil || !handled {
		t.Fatalf("opcode=%d handled=%t err=%v", raid.RaidDoBehaviorRequestMsgID, handled, err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("661 wrote %d bytes despite its native no-ACK handler", conn.write.Len())
	}
	conn.write.Reset()

	// 663 is the raid message request. It used to be dropped. The banner turns out
	// to be a chat MODE rather than a dedicated opcode (86: IsRaidMessageMode(52|53)
	// with mode 53 gated on the raid leader), and this client answered the labelled
	// sweep with 56.
	handled, err = service.handleAlignedGameCommand(
		leader,
		byte(1),
		raid.RaidMessageRequestMsgID,
		[]byte{3, 2, 0, 0, 0, 'o', 'k', 4},
	)
	if err != nil || !handled {
		t.Fatalf("opcode=%d handled=%t err=%v", raid.RaidMessageRequestMsgID, handled, err)
	}
	if conn.write.Len() == 0 {
		t.Fatal("663 wrote nothing; the leader's announcement did not run")
	}
	first, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if first.Header.MsgID != currentTownChatNotificationMsgID {
		t.Fatalf("663 answer msg=%#04x want %#04x", first.Header.MsgID, currentTownChatNotificationMsgID)
	}
	want := buildCurrentTownChatNotification(currentRaidAnnouncementMode, leader.selectedCharacterID, []byte("ok"))
	if !bytes.Equal(first.Body, want) {
		t.Fatalf("663 answer body=% X want % X", first.Body, want)
	}
}

func TestRuntimeRaidRewardCardInvalidBodyUsesNativeFailureShape(t *testing.T) {
	service, leader, _, _ := newRuntimeRaidResultFlowBridgeService(t)
	handled, err := service.handleAlignedGameCommand(
		leader,
		byte(1),
		raid.RaidRewardCardRequestMsgID,
		[]byte{3, 0},
	)
	if err != nil || !handled {
		t.Fatalf("invalid card handled=%t err=%v", handled, err)
	}
	packet, rest := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != raid.RaidRewardCardRequestMsgID ||
		!bytes.Equal(packet.Body, []byte{0, runtimeRaidFailureCode}) {
		t.Fatalf("failure packet class=%d msg=%#04x body=% X", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("invalid card trailing bytes=%d", len(rest))
	}
}

func newRuntimeRaidResultFlowBridgeService(t *testing.T) (*Service, *gameSession, *gameSession, uint32) {
	t.Helper()
	const raidKey = uint32(0x00c803e9)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038}
	leader := &gameSession{conn: &bufferConn{}, channel: channel}
	member := &gameSession{conn: &bufferConn{}, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "member", GroupIndex: 2, SlotOrder: 0},
	}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002},
		StartAttempt:   1,
		Lifecycle:      newRuntimeRaidLifecycleState(time.Date(2026, 8, 6, 3, 0, 0, 0, time.UTC)),
	}
	// The settlement pays the members the phase registered a clear for; the fixture is
	// the state a reached settlement implies.
	seedRuntimeRaidPhaseClearsForTest(&service.raids[raidKey].Lifecycle, 1001, 1002)
	return service, leader, member, raidKey
}

type runtimeRaidSettlementMovieOwnerFunc func(uint16, byte, time.Time) (runtimeRaidState, bool, error)

type runtimeRaidResultGenerationConn struct {
	bufferConn
	writes          int
	afterFirstWrite func()
}

func (connection *runtimeRaidResultGenerationConn) Write(data []byte) (int, error) {
	written, err := connection.bufferConn.Write(data)
	connection.writes++
	if connection.writes == 1 && connection.afterFirstWrite != nil {
		connection.afterFirstWrite()
	}
	return written, err
}

func (owner runtimeRaidSettlementMovieOwnerFunc) confirmRuntimeRaidSettlementMovie(
	characterID uint16,
	phase byte,
	now time.Time,
) (runtimeRaidState, bool, error) {
	return owner(characterID, phase, now)
}

type runtimeRaidSettlementCardOwnerStub struct {
	state        runtimeRaidState
	previewIndex byte
	previewed    bool
}

func (owner *runtimeRaidSettlementCardOwnerStub) selectRuntimeRaidSettlementCard(
	_ uint16,
	_ byte,
	request raid.RaidRewardCardRequest,
	_ time.Time,
) (runtimeRaidState, bool, error) {
	switch request.Mode {
	case 1:
		owner.previewIndex = request.CardIndex
		owner.previewed = true
		return owner.state, false, nil
	case 0:
		if !owner.previewed || owner.previewIndex != request.CardIndex {
			return runtimeRaidState{}, false, errors.New("mode0 did not match mode1")
		}
		return owner.state, true, nil
	default:
		return runtimeRaidState{}, false, errors.New("unexpected mode")
	}
}

type runtimeRaidSettlementRewardOwnerStub struct {
	state          runtimeRaidState
	token          uint64
	rewards        raid.RaidSettlementRewards
	timer          *raid.RaidTimer
	commitErr      error
	commits        int
	completions    int
	completedToken uint64
}

func (owner *runtimeRaidSettlementRewardOwnerStub) commitRuntimeRaidSettlementRewards(
	_ uint32,
	_ byte,
	_ time.Time,
) (runtimeRaidSettlementRewardDelivery, error) {
	owner.commits++
	if owner.commitErr != nil {
		return runtimeRaidSettlementRewardDelivery{}, owner.commitErr
	}
	return runtimeRaidSettlementRewardDelivery{
		Raid:    owner.state,
		Token:   owner.token,
		Rewards: owner.rewards,
	}, nil
}

func (owner *runtimeRaidSettlementRewardOwnerStub) completeRuntimeRaidSettlementRewards(
	_ uint32,
	_ byte,
	token uint64,
	_ time.Time,
) (runtimeRaidSettlementRewardCompletion, error) {
	owner.completions++
	owner.completedToken = token
	return runtimeRaidSettlementRewardCompletion{Raid: owner.state, StandbyTimer: owner.timer}, nil
}

func assertRuntimeRaidMovieCountdownPackets(t *testing.T, raw []byte) {
	t.Helper()
	state4, rest := splitGameServerUpperPacket(t, raw)
	timer, rest := splitGameServerUpperPacket(t, rest)
	remain, rest := splitGameServerUpperPacket(t, rest)
	if state4.Header.Classification != 0 || state4.Header.MsgID != raid.RaidStateMsgID || !bytes.Equal(state4.Body, raid.BuildRaidStateBody(raid.RaidState{State: 4})) {
		t.Fatalf("state4 class=%d msg=%#04x body=% X", state4.Header.Classification, state4.Header.MsgID, state4.Body)
	}
	if timer.Header.Classification != 0 || timer.Header.MsgID != raid.RaidTimerMsgID || len(timer.Body) != 13 || timer.Body[0] != 0 || !bytes.Equal(timer.Body[9:], []byte{3, 0, 0, 0}) {
		t.Fatalf("countdown timer class=%d msg=%#04x body=% X", timer.Header.Classification, timer.Header.MsgID, timer.Body)
	}
	if remain.Header.Classification != 0 || remain.Header.MsgID != raid.RaidRemainTimeMsgID || !bytes.Equal(remain.Body, raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: 3})) {
		t.Fatalf("countdown remain class=%d msg=%#04x body=% X", remain.Header.Classification, remain.Header.MsgID, remain.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("movie countdown trailing bytes=%d", len(rest))
	}
}

func assertRuntimeRaidCardsOpenedPacket(t *testing.T, raw []byte) {
	t.Helper()
	state5, rest := splitGameServerUpperPacket(t, raw)
	if state5.Header.Classification != 0 || state5.Header.MsgID != raid.RaidStateMsgID || !bytes.Equal(state5.Body, raid.BuildRaidStateBody(raid.RaidState{State: 5})) {
		t.Fatalf("state5 class=%d msg=%#04x body=% X", state5.Header.Classification, state5.Header.MsgID, state5.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("cards-open trailing bytes=%d", len(rest))
	}
}

// assertRuntimeRaidRewardAndTimerPackets checks the presentation a member
// receives after a committed settlement: its own squad's reveal rows, the
// whole-raid team row when the settlement has more than one squad (wantTeam
// non-nil), and the standby timer with its remain-time twin.
func assertRuntimeRaidRewardAndTimerPackets(t *testing.T, raw []byte, wantRewards []byte, wantTeam []byte, wantTimer []byte) {
	t.Helper()
	rewardPacket, rest := splitGameServerUpperPacket(t, raw)
	if rewardPacket.Header.Classification != 0 || rewardPacket.Header.MsgID != raid.RaidRewardListMsgID || !bytes.Equal(rewardPacket.Body, wantRewards) {
		t.Fatalf("reward packet class=%d msg=%#04x body=% X", rewardPacket.Header.Classification, rewardPacket.Header.MsgID, rewardPacket.Body)
	}
	if wantTeam != nil {
		teamPacket, teamRest := splitGameServerUpperPacket(t, rest)
		if teamPacket.Header.Classification != 0 || teamPacket.Header.MsgID != raid.RaidRewardListMsgID || !bytes.Equal(teamPacket.Body, wantTeam) {
			t.Fatalf("team reward packet class=%d msg=%#04x body=% X want=% X", teamPacket.Header.Classification, teamPacket.Header.MsgID, teamPacket.Body, wantTeam)
		}
		rest = teamRest
	}
	timerPacket, rest := splitGameServerUpperPacket(t, rest)
	if timerPacket.Header.Classification != 0 || timerPacket.Header.MsgID != raid.RaidTimerMsgID || !bytes.Equal(timerPacket.Body, wantTimer) {
		t.Fatalf("timer packet class=%d msg=%#04x body=% X", timerPacket.Header.Classification, timerPacket.Header.MsgID, timerPacket.Body)
	}
	// The break countdown is read from 0x0255 timerType 1 while the raid object
	// state is the phase-settlement value 5, so every 0x0254 standby timer is
	// followed by its remain-time twin carrying whole seconds bounded by the break
	// duration the timer itself announces (0 when the fixture's clock is already
	// past the deadline, which the client draws as expired).
	remainPacket, rest := splitGameServerUpperPacket(t, rest)
	duration := binary.LittleEndian.Uint32(wantTimer[9:13])
	remaining := binary.LittleEndian.Uint32(remainPacket.Body[1:5])
	if remainPacket.Header.Classification != 0 || remainPacket.Header.MsgID != raid.RaidRemainTimeMsgID ||
		len(remainPacket.Body) != 5 || remainPacket.Body[0] != 1 || remaining > duration {
		t.Fatalf("standby remain packet class=%d msg=%#04x body=% X duration=%d",
			remainPacket.Header.Classification, remainPacket.Header.MsgID, remainPacket.Body, duration)
	}
	if len(rest) != 0 {
		t.Fatalf("reward/timer trailing bytes=%d", len(rest))
	}
}
