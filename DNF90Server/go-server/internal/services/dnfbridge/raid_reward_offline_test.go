package dnfbridge

import (
	"fmt"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// One crashed client must not swallow everybody else's flip.
//
// Live 2026-09-17: character 1's client faulted at 08:52:12Z, character 22
// cleared the last phase-two objective at 08:54:07Z and confirmed the result
// movie at 08:54:16Z, and the raid then stayed on AwaitingMovie until the log
// ended -- the movie gate required a confirmation from a socket that no longer
// existed. The surviving member's confirmation has to close the phase instead.
func TestRuntimeRaidPhaseTwoMovieSkipsADisconnectedMember(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	reserveRuntimeRaidDisconnectForTest(fixture, 1002)

	state := fixture.service.raids[fixture.raidKey]
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	clear(state.Lifecycle.RewardSelections)
	clear(state.Lifecycle.MovieConfirmed)
	state.Lifecycle.RewardRosterFrozen = false
	state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)

	settled, all, err := fixture.service.confirmRuntimeRaidSettlementMovie(
		1001,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("surviving member's movie confirmation: %v", err)
	}
	if !all {
		t.Fatal("a disconnected member still held the phase-two movie gate open")
	}
	if settled.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards {
		t.Fatalf("lifecycle status=%d want awaiting-rewards", settled.Lifecycle.Status)
	}
	if !settled.Lifecycle.RewardSkippedMembers[1002] {
		t.Fatalf("the dropped member was not recorded as skipped: %+v", settled.Lifecycle.RewardSkippedMembers)
	}
	if settled.Lifecycle.RewardSkippedMembers[1001] {
		t.Fatal("the member that confirmed the movie was skipped")
	}
}

// The member the settlement gave up on is not paid: its row is absent from the
// committed reward list and its inventory is untouched.
func TestRuntimeRaidSettlementDoesNotPayASkippedMember(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	reserveRuntimeRaidDisconnectForTest(fixture, 1002)
	before := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)

	state := fixture.service.raids[fixture.raidKey]
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	clear(state.Lifecycle.RewardSelections)
	state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	state.Lifecycle.RewardRosterFrozen = false
	state.Lifecycle.RewardSelections[1001] = runtimeRaidRewardSelection{Mode: 0, Index: 2, ConfirmedAt: fixture.now}

	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("commit with one member skipped: %v", err)
	}
	if len(delivery.Rewards.Members) != 1 || delivery.Rewards.Members[0].MemberKey != 1001 {
		t.Fatalf("committed rows=%+v want only the member that confirmed", delivery.Rewards.Members)
	}
	after := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	skippedKey := runtimeRaidRewardTestCharacterKey(1002)
	if len(after[skippedKey].Slots) != len(before[skippedKey].Slots) {
		t.Fatalf("the skipped member was paid: before=%d slots after=%d slots",
			len(before[skippedKey].Slots), len(after[skippedKey].Slots))
	}
	paidKey := runtimeRaidRewardTestCharacterKey(1001)
	if len(after[paidKey].Slots) == len(before[paidKey].Slots) {
		t.Fatalf("the member that confirmed was not paid: %+v", after[paidKey].Slots)
	}
}

// A member that comes back after the settlement froze its eligible roster
// neither receives a second grant nor pulls the flow back into the waiting
// state: the record of who was skipped is monotonic for the phase.
func TestRuntimeRaidSettlementSurvivesTheSkippedMembersReconnect(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	reserveRuntimeRaidDisconnectForTest(fixture, 1002)

	state := fixture.service.raids[fixture.raidKey]
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	clear(state.Lifecycle.RewardSelections)
	clear(state.Lifecycle.MovieConfirmed)
	state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	state.Lifecycle.RewardRosterFrozen = false

	if _, all, err := fixture.service.confirmRuntimeRaidSettlementMovie(
		1001, runtimeRaidDungeonPhaseOne, fixture.now,
	); err != nil || !all {
		t.Fatalf("movie gate all=%t err=%v", all, err)
	}
	if _, _, err := fixture.service.selectRuntimeRaidSettlementCard(
		1001, runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 1, CardIndex: 2}, fixture.now,
	); err != nil {
		t.Fatalf("surviving member mode1: %v", err)
	}
	if _, all, err := fixture.service.selectRuntimeRaidSettlementCard(
		1001, runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 0, CardIndex: 2}, fixture.now,
	); err != nil || !all {
		t.Fatalf("surviving member mode0 all=%t err=%v", all, err)
	}
	first, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey, runtimeRaidDungeonPhaseOne, fixture.now,
	)
	if err != nil {
		t.Fatalf("commit before the reconnect: %v", err)
	}
	afterCommit := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)

	// The dropped member reconnects inside its grace window.
	fixture.service.raidMu.Lock()
	delete(fixture.service.raidDisconnects, 1002)
	fixture.service.raidMu.Unlock()
	fixture.service.restoreRuntimeRaidPresence(fixture.member, "test_reconnect_after_settlement")

	fixture.service.raidMu.Lock()
	if !fixture.service.runtimeRaidRewardRosterConfirmedLocked(fixture.service.raids[fixture.raidKey]) {
		fixture.service.raidMu.Unlock()
		t.Fatal("the reconnect pulled the settled roster back into the waiting state")
	}
	fixture.service.raidMu.Unlock()
	if err := fixture.service.validateRuntimeRaidRewardDeliveryCommitted(
		fixture.raidKey, runtimeRaidDungeonPhaseOne, first.Token,
	); err != nil {
		t.Fatalf("a committed delivery stopped validating after the reconnect: %v", err)
	}

	// The reconnected client's card click is acknowledged and ignored.
	lateState, all, err := fixture.service.selectRuntimeRaidSettlementCard(
		1002, runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 0, CardIndex: 2}, fixture.now,
	)
	if err != nil {
		t.Fatalf("the skipped member's late card click was rejected: %v", err)
	}
	if !all {
		t.Fatal("the skipped member's late card click was not reported as settled")
	}
	if _, exists := lateState.Lifecycle.RewardSelections[1002]; exists {
		t.Fatal("the skipped member's late click was recorded as a reward selection")
	}
	replayed, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey, runtimeRaidDungeonPhaseOne, fixture.now,
	)
	if err != nil {
		t.Fatalf("commit after the reconnect: %v", err)
	}
	if replayed.Token != first.Token || len(replayed.Rewards.Members) != 1 ||
		replayed.Rewards.Members[0].MemberKey != 1001 {
		t.Fatalf("replayed delivery=%+v first=%+v", replayed.Rewards.Members, first.Rewards.Members)
	}
	if afterReplay := loadRuntimeRaidRewardTestInventories(t, fixture.repositories); !runtimeRaidRewardInventoriesEqual(afterCommit, afterReplay) {
		t.Fatal("the reconnect granted the skipped member a second reward")
	}
}

// An online member that simply never picks a card must not hold the squad.
//
// The card window has no countdown of its own: the only timer on the reward path
// is the 3s one that opens it (runtimeRaidRewardOpenCountdown). 86JP settles the
// window itself instead -- AutoSelectPendingPhaseOneCardsAsync
// (RaidHandler.Rewards.cs:383-416) waits ten seconds, gives every still-pending
// member the lowest free card index as operation 1 then 0, and settles the phase
// once the eligible roster is complete. This is the same timer, armed where
// 86JP arms it: once, right after the cards are published.
func TestRuntimeRaidAutoFlipSettlesAMemberThatNeverPicks(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = fixture.now
	fixture.service.gameplayTimers = queue

	state := fixture.service.raids[fixture.raidKey]
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.Lifecycle.RewardOpenedAt = fixture.now
	state.Lifecycle.RewardRosterFrozen = false
	state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	clear(state.Lifecycle.RewardSelections)

	// 1001 picks. 1002 is online and never will.
	if _, _, err := fixture.service.selectRuntimeRaidSettlementCard(
		1001, runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 1, CardIndex: 2}, fixture.now,
	); err != nil {
		t.Fatalf("leader mode1: %v", err)
	}
	if _, all, err := fixture.service.selectRuntimeRaidSettlementCard(
		1001, runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 0, CardIndex: 2}, fixture.now,
	); err != nil || all {
		t.Fatalf("leader mode0 all=%t err=%v", all, err)
	}
	snapshot, ok := fixture.service.runtimeRaidForMember(1001)
	if !ok {
		t.Fatal("the raid was not found for the leader")
	}
	if snapshot.Lifecycle.RewardOpenedAt.IsZero() {
		t.Fatal("the card window has no open time to measure the auto flip from")
	}
	if snapshot.Lifecycle.RewardSkippedMembers[1002] {
		t.Fatal("the online member that has not clicked yet was written off")
	}

	if err := fixture.service.scheduleRuntimeRaidRewardAutoFlip(snapshot); err != nil {
		t.Fatalf("arm the auto flip: %v", err)
	}
	name := fmt.Sprintf("raid-reward-auto-flip:%d:%d:%d",
		fixture.raidKey, runtimeRaidDungeonPhaseOne, snapshot.StartAttempt)
	queue.mu.Lock()
	task := queue.active[name]
	queue.mu.Unlock()
	if task == nil {
		t.Fatal("the card window was opened without arming its auto flip")
	}
	if task.delay != runtimeRaidRewardAutoFlipDelay {
		t.Fatalf("auto flip delay=%s want=%s", task.delay, runtimeRaidRewardAutoFlipDelay)
	}
	queue.fire(task, false)

	settled := fixture.service.raids[fixture.raidKey]
	selection, exists := settled.Lifecycle.RewardSelections[1002]
	if !exists || selection.Mode != 0 {
		t.Fatalf("the idle member's card was not picked for it: %+v", settled.Lifecycle.RewardSelections)
	}
	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey, runtimeRaidDungeonPhaseOne, fixture.now,
	)
	if err != nil {
		t.Fatalf("commit after the auto flip: %v", err)
	}
	if len(delivery.Rewards.Members) != 2 {
		t.Fatalf("committed rows=%+v want both members", delivery.Rewards.Members)
	}
	after := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	idleKey := runtimeRaidRewardTestCharacterKey(1002)
	if len(after[idleKey].Slots) == 0 {
		t.Fatal("the member the server picked for was not paid")
	}

	// Idempotent: the same task fired again, and a duplicate commit, grant
	// nothing a second time.
	queue.fire(task, true)
	if _, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey, runtimeRaidDungeonPhaseOne, fixture.now,
	); err != nil {
		t.Fatalf("duplicate commit: %v", err)
	}
	if replayed := loadRuntimeRaidRewardTestInventories(t, fixture.repositories); !runtimeRaidRewardInventoriesEqual(after, replayed) {
		t.Fatal("the auto flip paid a member twice")
	}
}

// The automatic pick fills in for a member that is present. It never fills in
// for one the settlement already wrote off: that member has no card, is absent
// from the committed receipt, and is not granted anything.
func TestRuntimeRaidAutoFlipLeavesASkippedMemberAlone(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	reserveRuntimeRaidDisconnectForTest(fixture, 1002)

	state := fixture.service.raids[fixture.raidKey]
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.Lifecycle.RewardOpenedAt = fixture.now
	state.Lifecycle.RewardRosterFrozen = false
	state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	clear(state.Lifecycle.RewardSelections)

	snapshot, ok := fixture.service.runtimeRaidForMember(1001)
	if !ok {
		t.Fatal("the raid was not found for the leader")
	}
	fixture.service.autoFlipRuntimeRaidPendingCards(snapshot)

	settled, ok := fixture.service.runtimeRaidForMember(1001)
	if !ok {
		t.Fatal("the raid disappeared during the auto flip")
	}
	if _, exists := settled.Lifecycle.RewardSelections[1002]; exists {
		t.Fatal("the auto flip picked a card for a member the settlement gave up on")
	}
	if selection, exists := settled.Lifecycle.RewardSelections[1001]; !exists || selection.Mode != 0 {
		t.Fatalf("the auto flip did not settle the only eligible member: %+v", settled.Lifecycle.RewardSelections)
	}
}

// reserveRuntimeRaidDisconnectForTest parks exactly what a dropped transport
// leaves behind: the roster row stays, and raid_disconnect.go holds the seat
// until the grace window expires.
func reserveRuntimeRaidDisconnectForTest(fixture runtimeRaidRewardTestFixture, characterID uint16) {
	fixture.service.raidMu.Lock()
	defer fixture.service.raidMu.Unlock()
	if fixture.service.raidDisconnects == nil {
		fixture.service.raidDisconnects = make(map[uint16]runtimeRaidDisconnectReservation)
	}
	fixture.service.raidDisconnects[characterID] = runtimeRaidDisconnectReservation{
		RaidKey:        fixture.raidKey,
		CharacterID:    characterID,
		Generation:     1,
		DisconnectedAt: fixture.now,
		ExpiresAt:      fixture.now.Add(runtimeRaidDisconnectGrace),
	}
	if state := fixture.service.raids[fixture.raidKey]; state != nil {
		if index := runtimeRaidMemberIndex(*state, characterID); index >= 0 {
			state.Members[index].UserState = runtimeRaidMemberUserStateOffline
		}
	}
}

func runtimeRaidRewardInventoriesEqual(
	left map[string]dnfrepo.InventoryRecord,
	right map[string]dnfrepo.InventoryRecord,
) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftInventory := range left {
		rightInventory, ok := right[key]
		if !ok || len(leftInventory.Slots) != len(rightInventory.Slots) {
			return false
		}
		for slot, stack := range leftInventory.Slots {
			other, ok := rightInventory.Slots[slot]
			if !ok || !reflect.DeepEqual(other, stack) {
				return false
			}
		}
	}
	return true
}
