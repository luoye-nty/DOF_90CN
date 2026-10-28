package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// completedReturnFallbackFixture reaches the exact state the three live
// 返回城镇 hangs were in: a settlement-exit-ready completed run that is still the
// session's current runtime, with no scene transition ever sent.
//
// The steps mirror the proven exit fixture in dungeon_settlement_exit_test.go: a
// completed run (prepareOrdinaryCompletedSettlementRuntimeForExitTest), the card
// reward committed, then the settlement's op46 and op123 that make
// completedDungeonSettlementExitReady true.
func completedReturnFallbackFixture(t *testing.T) (*Service, *runtimeDungeonState, *gameSession, *bufferConn) {
	t.Helper()
	service, runtime, session, connection := prepareOrdinaryCompletedSettlementRuntimeForExitTest(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("completed-settlement repository group unavailable")
	}
	// The selector page installs the current scene's skill info, so the fixture
	// needs the same PVF-backed skill projection the tower exit test enables.
	enableTownMoveSkillProjection(t, service, repositories, "99")
	markCurrentDungeonCardRewardCommittedForTest(runtime)
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
	runtime.settlementResultPlan = &plan
	if err := service.handleDungeonSetPlayResult(session, make([]byte, currentDungeonPlayResultBaseSize)); err != nil {
		t.Fatalf("set play result: %v", err)
	}
	if err := service.handleDungeonCharacterStatistic(session, make([]byte, currentDungeonCharacterStatisticBodySize)); err != nil {
		t.Fatalf("character statistic: %v", err)
	}
	if !service.completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("fixture is not settlement-exit-ready: entry=%t play_result=%t reward=%t statistic=%t phase=%s",
			runtime.settlementEntrySent, runtime.settlementPlayResultReceived,
			runtime.settlementClearRewardSent, runtime.settlementStatisticReceived,
			runtime.settlementPhase.String())
	}
	if runtime.townReturnOp24Sent {
		t.Fatal("fixture already delivered the typed op24; the failure precondition is not reproduced")
	}
	connection.write.Reset()
	return service, runtime, session, connection
}

// TestCompletedSettlementReturnFallsBackToSelectorWhenTheTownRouteSendsNothing
// pins the fix for the three live 返回城镇 hangs.
//
// Live: 2026-09-24T17:07:49Z (char 1) and 2026-09-25T05:25:22Z / 05:25:23Z
// (chars 1 and 2). Each logged game-raid-dungeon-settlement-exit-ready and
// game-dungeon-card-exit-ack-sent and then not one further packet, while the
// character stayed a live dungeon actor (still able to walk and cast). The player
// gave up and closed the client 13-17 minutes later. 96 of the 99 recorded
// 返回城镇 clicks delivered; these three did not.
//
// The town route answers an unstartable return with a bare `return nil` from one
// of its five guards, so the settlement page's 返回城镇 must not be allowed to
// end with nothing on the wire.
func TestCompletedSettlementReturnFallsBackToSelectorWhenTheTownRouteSendsNothing(t *testing.T) {
	service, runtime, session, connection := completedReturnFallbackFixture(t)
	connection.write.Reset()

	if err := service.backOffCompletedSettlementReturnWhenNothingWasSent(
		session, runtime, "test_empty_town_route",
	); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if len(connection.write.Bytes()) == 0 {
		t.Fatal("the player was left with nothing: neither the town route nor the fallback sent a page")
	}
	// The run has to be retired into the selector: that is what leaves the client
	// on a page it can leave, and what keeps the frozen town origin bound so a
	// 返回城镇 from that page still resolves (town_enter_select.go:256-271).
	if session.dungeon.runtime != nil {
		t.Fatal("the completed run was not retired into the selector")
	}
	foundSelect := false
	for rest := connection.write.Bytes(); len(rest) > 0; {
		packet, tail := splitGameServerUpperPacket(t, rest)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketEnterSelectDungeon) {
			foundSelect = true
		}
		rest = tail
	}
	if !foundSelect {
		t.Fatalf("the fallback did not push the dungeon selector page: %x", connection.write.Bytes())
	}
}

// TestCompletedSettlementTownRouteDropsAReplacedRuntime documents the class of
// silent drop the fallback compensates for. When the session's runtime is no
// longer the one the request captured, sendCurrentFullDungeonReturnToTown answers
// with a bare nil and not one byte reaches the client -- which is what left the
// three live sessions with a dead 返回城镇 button. That guard now also logs
// game-dungeon-full-return-not-started, so the next occurrence names itself
// instead of leaving a 440MB log with no explanation.
func TestCompletedSettlementTownRouteDropsAReplacedRuntime(t *testing.T) {
	service, runtime, session, connection := completedReturnFallbackFixture(t)
	session.dungeon.runtime = nil
	connection.write.Reset()

	if err := service.sendCurrentFullDungeonReturnToTown(
		session,
		runtime,
		currentDungeonTownTransition{},
		uint16(dnfenum.CmdPacketEplpCommand),
		"test_runtime_replaced",
		false,
		false,
	); err != nil {
		t.Fatalf("town route: %v", err)
	}
	if len(connection.write.Bytes()) != 0 {
		t.Fatalf("a replaced-runtime return was still handed a town route: %x", connection.write.Bytes())
	}
	if runtime.townReturnOp24Sent {
		t.Fatal("a dropped town route still recorded the typed op24 as sent")
	}
}

// TestCompletedSettlementReturnFallbackIsInertAfterTheTownRouteDelivered is the
// no-regression half: every 返回城镇 click that worked reached the typed op24
// (see TestDungeonCardExitStateOneReturnsToTownAfterAck, which asserts exactly
// that), so the fallback must send nothing and retire nothing.
func TestCompletedSettlementReturnFallbackIsInertAfterTheTownRouteDelivered(t *testing.T) {
	service, runtime, session, connection := completedReturnFallbackFixture(t)
	runtime.townReturnOp24Sent = true
	connection.write.Reset()

	if err := service.backOffCompletedSettlementReturnWhenNothingWasSent(
		session, runtime, "test_delivered_town_route",
	); err != nil {
		t.Fatalf("fallback after a delivered route: %v", err)
	}
	if len(connection.write.Bytes()) != 0 {
		t.Fatalf("a delivered return was answered a second time: %x", connection.write.Bytes())
	}
	if session.dungeon.runtime != runtime {
		t.Fatal("a delivered return had its runtime retired by the fallback")
	}
}

// TestCompletedSettlementReturnFallbackIgnoresAReplacedGeneration keeps a stale
// request from acting on the newer run that took the session over.
func TestCompletedSettlementReturnFallbackIgnoresAReplacedGeneration(t *testing.T) {
	service, runtime, session, connection := completedReturnFallbackFixture(t)
	session.dungeon.runtime = nil
	connection.write.Reset()

	if err := service.backOffCompletedSettlementReturnWhenNothingWasSent(
		session, runtime, "test_stale_generation",
	); err != nil {
		t.Fatalf("stale generation fallback: %v", err)
	}
	if len(connection.write.Bytes()) != 0 {
		t.Fatalf("a stale return still wrote to the client: %x", connection.write.Bytes())
	}
}
