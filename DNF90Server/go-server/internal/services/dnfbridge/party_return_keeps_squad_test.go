package dnfbridge

import "testing"

// The leader's 返回城镇 on a completed PARTY settlement is a whole-party exit:
// the op72 fan-out in handleDungeonEplpCommand walks every peer home through
// this same return, so nobody is leaving alone and the squad has to survive the
// trip.
//
// Live 2026-09-25, reported the moment the world-lock deadlock was fixed and
// this code ran for the first time: "现在确实能回城了 但是回城后队伍自动解散了
// 正确逻辑是 队长点击返回城镇 所有队员跟着返回 并且队伍不散 现在有点像各自回城了".
//
// The mechanism was the completed route classifying the click as a solo return
// -- "只要是自己单独回城了 视作直接退出队伍" -- and detaching the member from the
// shared roster while the fan-out was still walking the others home. The mark
// asserted here (runtime.townReturnKeepParty) is the existing whole-party marker
// the raid phase return and the 战况表 leader-led return already use.
func TestPartyDungeonLeaderReturnToTownKeepsTheSquad(t *testing.T) {
	service, sessions, _, partyState := newPartyDungeonTownReturnFixture(t)
	leader := sessions[99]

	leader.dungeon.mu.Lock()
	runtime := leader.dungeon.runtime
	if runtime == nil || runtime.partyDungeonOwner == nil {
		leader.dungeon.mu.Unlock()
		t.Fatal("fixture did not give the leader a shared party dungeon roster")
	}
	// The fully committed completed settlement is what the guards above the exit
	// dispatch need; the rest of the route bails out on the missing repositories,
	// which is fine here: the whole-party classification happens before any of
	// that.
	markCurrentDungeonCardRewardCommittedForTest(runtime)
	leader.dungeon.mu.Unlock()

	if err := service.handleDungeonEplpCommandLocal(leader, []byte{1, 2}, true, false); err != nil {
		t.Fatalf("leader 返回城镇: %v", err)
	}

	if !runtime.townReturnKeepParty {
		t.Fatal("the leader's completed 返回城镇 in a party dungeon was NOT marked as a " +
			"whole-party exit: the completed route will detach the member from the shared " +
			"roster and the party dissolves as each member comes home")
	}
	if _, found := service.runtimePartyManagerForService().SnapshotByID(uint16(partyState.PartyID)); !found {
		t.Fatal("the leader's 返回城镇 disbanded the party")
	}
}

// A member who leaves on their own still leaves the party -- the operator's
// long-standing rule 「只要是自己单独回城了 视作直接退出队伍」 must survive the
// whole-party marking above. The mark is therefore scoped to the op72 return
// ACTION in a party dungeon, not to "a completed run is being left".
func TestPartyDungeonSoloReturnStillLeavesTheParty(t *testing.T) {
	service, sessions, _, _ := newPartyDungeonTownReturnFixture(t)
	returning := sessions[100]

	returning.dungeon.mu.Lock()
	runtime := returning.dungeon.runtime
	// Same fully committed completed settlement as the whole-party test above, so
	// the only difference between the two is the op72 exit action itself.
	markCurrentDungeonCardRewardCommittedForTest(runtime)
	returning.dungeon.mu.Unlock()

	// The settlement auto-return timer's entry point: the same completed return,
	// without the op72 leader click that makes it a whole-party exit.
	if err := service.completeCurrentSettlementReturnToTown(
		returning, runtime, "current_exe_settlement_card_timeout", false,
	); err != nil {
		t.Fatalf("solo completed return: %v", err)
	}
	if runtime.townReturnKeepParty {
		t.Fatal("a return with no op72 exit action was marked as a whole-party exit; " +
			"a member who goes home alone must still leave the party")
	}
}
