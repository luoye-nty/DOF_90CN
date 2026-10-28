package dnfbridge

import (
	"fmt"
	"testing"
	"time"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

// The card window used to wait for EVERY eligible member's result-movie
// confirmation, so one client stuck in the movie held the whole raid's flip. The
// operator's report is "第二台1阶段过了2分钟左右才出来翻牌", and the same day's log
// shows the window only ever opening through the auto-flip (raid 1507329:
// flipped=6 in phase one, flipped=3 in phase two).
func TestRaidMovieConfirmFallbackOpensTheCardWindowWithoutTheSlowestClient(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	queue := newPartyPublishTestQueue()
	service.gameplayTimers = queue
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)

	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0},
		now,
	); err != nil {
		t.Fatalf("open phase movie: %v", err)
	}
	name := fmt.Sprintf("raid-movie-confirm-fallback:%d:%d", raidKey, runtimeRaidDungeonPhaseOne)
	if delay, armed := queue.delays[name]; !armed || delay != runtimeRaidMovieConfirmFallbackDelay {
		t.Fatalf("movie fallback armed=%t delay=%s want=%s", armed, delay, runtimeRaidMovieConfirmFallbackDelay)
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("status after settlement=%d want AwaitingMovie", got)
	}
	// One member confirms the movie; the other never does. Before the bounded wait
	// existed the raid stayed in AwaitingMovie for as long as that client took.
	leaderConn := leader.conn.(*bufferConn)
	memberConn := member.conn.(*bufferConn)
	if err := service.handleRuntimeRaidResultStage(leader, []byte{1}); err != nil {
		t.Fatalf("leader stage1: %v", err)
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("one confirmation opened the window: status=%d", got)
	}
	leaderBefore := leaderConn.write.Len()
	memberBefore := memberConn.write.Len()

	queue.fire(t, name)

	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecycleAwaitingRewards {
		t.Fatalf("fallback status=%d want AwaitingRewards", got)
	}
	state := service.raids[raidKey]
	if !state.Lifecycle.MovieConfirmed[1002] {
		t.Fatalf("fallback left member 1002 unconfirmed: %v", state.Lifecycle.MovieConfirmed)
	}
	if state.Lifecycle.RewardOpenedAt.IsZero() {
		t.Fatal("fallback did not open the reward window")
	}
	// Every member, including the one that never confirmed, receives the countdown
	// and then the cards-open state.
	assertRuntimeRaidMovieCountdownPackets(t, leaderConn.write.Bytes()[leaderBefore:])
	assertRuntimeRaidMovieCountdownPackets(t, memberConn.write.Bytes()[memberBefore:])
	cardsName := fmt.Sprintf("raid-cards-open:%d:%d:%d", raidKey, runtimeRaidDungeonPhaseOne, state.StartAttempt)
	if _, armed := queue.callbacks[cardsName]; !armed {
		t.Fatalf("cards-open was not re-armed after the fallback: %v", queue.order)
	}
	afterCountdown := memberConn.write.Len()
	queue.fire(t, cardsName)
	assertRuntimeRaidCardsOpenedPacket(t, memberConn.write.Bytes()[afterCountdown:])
}

// A client that finishes its movie late must still leave the movie: the window is
// already open, so its stage1 is answered and it joins the open card window
// instead of waiting for an ACK the server would otherwise never send.
func TestRaidMovieConfirmFallbackAcceptsALateStageOne(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	queue := newPartyPublishTestQueue()
	service.gameplayTimers = queue
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0},
		now,
	); err != nil {
		t.Fatalf("open phase movie: %v", err)
	}
	if err := service.handleRuntimeRaidResultStage(leader, []byte{1}); err != nil {
		t.Fatalf("leader stage1: %v", err)
	}
	queue.fire(t, fmt.Sprintf("raid-movie-confirm-fallback:%d:%d", raidKey, runtimeRaidDungeonPhaseOne))

	memberConn := member.conn.(*bufferConn)
	memberConn.write.Reset()
	if err := service.handleRuntimeRaidResultStage(member, []byte{1}); err != nil {
		t.Fatalf("late member stage1: %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, memberConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != raid.RaidResultStageRequestMsgID {
		t.Fatalf("late stage1 ACK class=%d msg=%#04x rest=%d",
			ack.Header.Classification, ack.Header.MsgID, len(rest))
	}
	if !service.raids[raidKey].Lifecycle.MovieConfirmed[1002] {
		t.Fatal("late stage1 was not recorded as a movie confirmation")
	}
}

// When every member confirms in time the bounded wait must stay a no-op: it may
// never re-open or re-broadcast a window the members already own.
func TestRaidMovieConfirmFallbackLeavesACompleteRosterAlone(t *testing.T) {
	service, leader, member, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	queue := newPartyPublishTestQueue()
	service.gameplayTimers = queue
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0},
		now,
	); err != nil {
		t.Fatalf("open phase movie: %v", err)
	}
	if err := service.handleRuntimeRaidResultStage(leader, []byte{1}); err != nil {
		t.Fatalf("leader stage1: %v", err)
	}
	if err := service.handleRuntimeRaidResultStage(member, []byte{1}); err != nil {
		t.Fatalf("member stage1: %v", err)
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecycleAwaitingRewards {
		t.Fatalf("complete roster status=%d want AwaitingRewards", got)
	}
	openedAt := service.raids[raidKey].Lifecycle.RewardOpenedAt
	leaderConn := leader.conn.(*bufferConn)
	memberConn := member.conn.(*bufferConn)
	leaderBefore := leaderConn.write.Len()
	memberBefore := memberConn.write.Len()

	queue.fire(t, fmt.Sprintf("raid-movie-confirm-fallback:%d:%d", raidKey, runtimeRaidDungeonPhaseOne))

	if got := service.raids[raidKey].Lifecycle.RewardOpenedAt; !got.Equal(openedAt) {
		t.Fatalf("fallback moved the reward window opened_at %v -> %v", openedAt, got)
	}
	if leaderConn.write.Len() != leaderBefore || memberConn.write.Len() != memberBefore {
		t.Fatalf("fallback rewrote a settled window: leader %d->%d member %d->%d",
			leaderBefore, leaderConn.write.Len(), memberBefore, memberConn.write.Len())
	}
}

func TestRaidMovieConfirmFallbackRefusesAfterThePhaseMovedOn(t *testing.T) {
	service, _, _, raidKey := newRuntimeRaidResultFlowBridgeService(t)
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	state := cloneRuntimeRaidState(*service.raids[raidKey])
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne

	// Wrong movie epoch: a fallback armed for a previous movie must never open the
	// window for a newer one.
	if _, pending, ok := service.forceRuntimeRaidMovieRosterConfirmed(
		raidKey, runtimeRaidDungeonPhaseOne, now.Add(-time.Minute), now,
	); ok || len(pending) != 0 {
		t.Fatalf("stale movie epoch opened the window: pending=%v", pending)
	}
	// Wrong phase.
	if _, _, ok := service.forceRuntimeRaidMovieRosterConfirmed(
		raidKey, runtimeRaidDungeonPhaseTwo, state.Lifecycle.MovieOpenedAt, now,
	); ok {
		t.Fatal("fallback acted on another phase")
	}
	// Wrong lifecycle status.
	service.raids[raidKey].Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	if _, _, ok := service.forceRuntimeRaidMovieRosterConfirmed(
		raidKey, runtimeRaidDungeonPhaseOne, service.raids[raidKey].Lifecycle.MovieOpenedAt, now,
	); ok {
		t.Fatal("fallback acted while the phase was still active")
	}
}
