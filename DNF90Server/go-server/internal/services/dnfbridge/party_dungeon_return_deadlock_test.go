package dnfbridge

import (
	"testing"
	"time"
)

// The live 2026-09-25 freeze, reproduced.
//
// A member who had finished a run clicked 返回城镇 while a squadmate was still
// inside the same instance. handleDungeonEplpCommand holds
// runtime.partyDungeonOwner.worldMu for the whole settlement commit, and that
// commit reaches detachRuntimePartyDungeonMemberForTownReturn, which took the
// same lock a second time. sync.Mutex is not reentrant, so the returning
// member's goroutine parked in sync.Mutex.Lock forever and KEPT worldMu: every
// later party, settlement and consumable command on that squad blocked behind a
// dead goroutine until the client was restarted.
//
// The goroutine dump that proves it (runtime/logs/goroutine_dump.txt,
// 2026-09-25T16:14:30 local, 161 goroutines) is unambiguous:
//
//	goroutine 499 [sync.Mutex.Lock, 2 minutes]:
//	  sync.(*Mutex).Lock
//	  dnfbridge.(*Service).detachRuntimePartyDungeonMemberForTownReturn      dungeon_return_town.go:1998
//	  dnfbridge.(*Service).sendCurrentFullDungeonReturnToTown               dungeon_return_town.go:1228
//	  dnfbridge.(*Service).sendCurrentCompletedDungeonReturnToTown          dungeon_return_town.go:1009
//	  dnfbridge.(*Service).completeCurrentSettlementReturnToTown            dungeon_card_flow.go:1430
//	  dnfbridge.(*Service).handleDungeonEplpCommandLocal                    dungeon_card_flow.go:1004
//	  dnfbridge.(*Service).handleDungeonEplpCommand                         dungeon_card_flow.go:590
//
// "2 minutes" is exactly the gap between the 返回城镇 click that ACKed at
// 08:12:38.570Z and the dump, which is why the member's client saw the ACK, not
// one further packet, and a character that could still walk around.
//
// The test drives the detach the way the op72 leader's goroutine does: the world
// lock is ALREADY held by this goroutine when the call is made. A goroutine plus
// a deadline is used rather than a plain call so a regression fails the test
// instead of hanging the whole test binary.
func TestPartyDungeonTownReturnDetachReusesTheCallersWorldLock(t *testing.T) {
	service, sessions, owner, _ := newPartyDungeonTownReturnFixture(t)
	returning := sessions[99]
	previous := runtimePartyStateSnapshot(returning)

	done := make(chan error, 1)
	go func() {
		owner.worldMu.Lock()
		defer owner.worldMu.Unlock()
		done <- service.detachRuntimePartyDungeonMemberForTownReturnWhileWorldLocked(
			returning,
			owner,
			previous,
			"test_op72_leader_already_owns_world_lock",
		)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("detach under the caller's world lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the town-return detach deadlocked on a world lock its caller already held; " +
			"live this parked the returning member's goroutine forever, kept worldMu owned by a " +
			"dead goroutine, and left 返回城镇, item use and the next party entry hanging until " +
			"the client was restarted")
	}
}

// The other half of the contract: every caller that does NOT already hold the
// world lock (op132, op42, the death return, the 战况表 return, party_directory)
// still gets it taken for them. This is the pre-existing behaviour, kept exact.
func TestPartyDungeonTownReturnDetachTakesTheWorldLockWhenTheCallerDoesNot(t *testing.T) {
	service, sessions, owner, _ := newPartyDungeonTownReturnFixture(t)
	returning := sessions[100]
	previous := runtimePartyStateSnapshot(returning)

	unlocked := make(chan struct{})
	if err := service.detachRuntimePartyDungeonMemberForTownReturn(
		returning,
		owner,
		previous,
		"test_op42_caller_holds_no_world_lock",
	); err != nil {
		t.Fatal(err)
	}
	// The wrapper must have released it again: a lock left held by this call
	// would freeze the squad exactly like the deadlock above.
	go func() {
		owner.worldMu.Lock()
		owner.worldMu.Unlock()
		close(unlocked)
	}()
	select {
	case <-unlocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the self-locking detach wrapper did not release worldMu")
	}
}
