package dnfbridge

import (
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

// TestRuntimeRaidRewardPublishRetriesServerSideAndCompletes covers the
// twenty-member failure mode: the last mode0 confirmation is the only trigger
// of 0x0258 publication, so a dead socket in the fan-out used to strand the
// whole attack party in AwaitingRewards. The retry is server-owned now.
func TestRuntimeRaidRewardPublishRetriesServerSideAndCompletes(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	fixture.service.gameplayTimers = queue
	wantWriteErr := errors.New("raid reward-list write failed")
	fixture.leader.conn = &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}
	memberConn := &bufferConn{}
	fixture.member.conn = memberConn

	fixture.service.raidRewardFlowMu.Lock()
	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(fixture.raidKey, runtimeRaidDungeonPhaseOne, fixture.now)
	if err != nil {
		fixture.service.raidRewardFlowMu.Unlock()
		t.Fatalf("commit: %v", err)
	}
	err = fixture.service.publishCommittedRuntimeRaidSettlementRewardsWithRetry(delivery, fixture.service, 1)
	fixture.service.raidRewardFlowMu.Unlock()
	if err != nil {
		t.Fatalf("first publish with retry returned %v; the failure must be absorbed into a scheduled retry", err)
	}
	if state := fixture.service.raids[fixture.raidKey]; state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards {
		t.Fatalf("status after failed publish=%v want AwaitingRewards", state.Lifecycle.Status)
	}
	scheduled, _, _ := queue.counts()
	if scheduled != 1 {
		t.Fatalf("scheduled %d timers, want the one publish retry", scheduled)
	}
	retry := queue.task(t, 0)
	if retry.name != runtimeRaidRewardPublishRetryName(delivery) || retry.delay != runtimeRaidRewardPublishRetryDelay {
		t.Fatalf("retry task=%s delay=%s", retry.name, retry.delay)
	}

	// The socket recovers before the retry fires.
	leaderRetry := &bufferConn{}
	fixture.leader.conn = leaderRetry
	queue.fire(retry, false)

	state := fixture.service.raids[fixture.raidKey]
	if state == nil || state.Lifecycle.Status != runtimeRaidLifecycleStandby {
		t.Fatalf("retry did not complete the settlement: %+v", state)
	}
	if !containsRaidMsg(t, leaderRetry.write.Bytes(), raid.RaidRewardListMsgID) {
		t.Fatal("leader never received the reward list on the retry")
	}
	// The member is fanned out after the leader, so the leader's failed write
	// aborted the pass before the member received anything: the retry owes it both
	// rows of the presentation -- its squad-scoped reveal and the whole-raid team
	// row -- and exactly one of each.
	memberLists := 0
	for _, packet := range runtimeRaidRejoinRaidPackets(t, memberConn.write.Bytes()) {
		if packet.msgID == raid.RaidRewardListMsgID {
			memberLists++
		}
	}
	if memberLists != 2 {
		t.Fatalf("member received %d reward lists, want exactly 2 (squad reveal + team row)", memberLists)
	}
}

func TestRuntimeRaidRewardPublishGivesUpIntoCompletion(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	fixture.service.gameplayTimers = queue
	fixture.leader.conn = &failNthDungeonWriteConn{failAt: 1, err: errors.New("dead socket")}
	fixture.member.conn = &bufferConn{}

	fixture.service.raidRewardFlowMu.Lock()
	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(fixture.raidKey, runtimeRaidDungeonPhaseOne, fixture.now)
	if err != nil {
		fixture.service.raidRewardFlowMu.Unlock()
		t.Fatalf("commit: %v", err)
	}
	// Enter at the last permitted attempt: no more retries, force completion.
	err = fixture.service.publishCommittedRuntimeRaidSettlementRewardsWithRetry(delivery, fixture.service, runtimeRaidRewardPublishMaxAttempts)
	fixture.service.raidRewardFlowMu.Unlock()
	if err != nil {
		t.Fatalf("give-up path returned %v", err)
	}
	if state := fixture.service.raids[fixture.raidKey]; state == nil || state.Lifecycle.Status != runtimeRaidLifecycleStandby {
		t.Fatalf("give-up did not advance to standby: %+v", state)
	}
	if scheduled, _, _ := queue.counts(); scheduled == 0 {
		t.Fatal("standby entry scheduled no standby timer")
	}
}

func containsRaidMsg(t *testing.T, raw []byte, msgID uint16) bool {
	t.Helper()
	for _, packet := range runtimeRaidRejoinRaidPackets(t, raw) {
		if packet.msgID == msgID {
			return true
		}
	}
	return false
}
