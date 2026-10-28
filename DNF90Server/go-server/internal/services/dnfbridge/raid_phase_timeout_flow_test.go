package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 攻坚阶段超时要按 86JP 走「失败」而不是直接删团：
// RaidHandler.Runtime.cs RunAttackTimeoutAsync(:592-728) 先 TryFailPhase
// (RaidManager.cs:1956-1982，state=4 + StateArgument=1)，把这个阶段所有关卡状态
// 置 2，发一阶段失败符号 104、失败结算(type=1)、state=4，让还在图里的队员能走
// (EnablePhaseOneDungeonReturnAsync :1993-2013)，最后才交给
// RemoveFailedRaidAfterResultDelayAsync(:730-762) 在 10 秒后真正删团。
//
// 本服以前是到点就 delete(s.raids)，所以客户端看不到失败结算、图里的队伍被留在
// 一个服务器已经不认的副本里。
func TestRuntimeRaidPhaseTimeoutFailsBeforeItReleasesTheRaid(t *testing.T) {
	now := time.Now().UTC()
	service, raidKey := newRuntimeRaidLifecycleService(now)
	state := service.raids[raidKey]
	config := runtimeRaidBridgeTestAntonConfig()
	state.AntonConfig = config
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.Lifecycle.PhaseStartedAt = now.Add(-time.Hour)
	state.Lifecycle.PhaseDeadline = now.Add(-time.Minute)
	if err := initializeRuntimeRaidPhaseObjectives(&state.Lifecycle, config, runtimeRaidDungeonPhaseOne); err != nil {
		t.Fatalf("initialize phase one: %v", err)
	}
	// One gate the members actually walked into, so the table has something to close.
	objective := state.Lifecycle.Objectives[antonBlackFogSourceMapID]
	objective.State = raid.AntonDungeonOpen
	objective.RemainingHP = objective.MaxHP
	state.Lifecycle.Objectives[antonBlackFogSourceMapID] = objective

	failed, err := service.expireRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, now)
	if err != nil {
		t.Fatalf("phase timeout: %v", err)
	}
	if failed.Lifecycle.Status != runtimeRaidLifecycleFailed {
		t.Fatalf("timeout status=%d want failed", failed.Lifecycle.Status)
	}
	if failed.Lifecycle.FailedAt.IsZero() {
		t.Fatal("the timeout did not record when the phase failed")
	}
	if _, alive := service.raids[raidKey]; !alive {
		t.Fatal("the failed raid was deleted before its ten-second result window")
	}
	if symbol := failed.Lifecycle.MovieSymbols[antonSymbolPhaseOneFail]; symbol != 1 {
		t.Fatalf("phase-one failure symbol %d=%d want 1", antonSymbolPhaseOneFail, symbol)
	}
	for mapID, gate := range failed.Lifecycle.Objectives {
		if gate.Phase != runtimeRaidDungeonPhaseOne {
			continue
		}
		if wire := runtimeRaidObjectiveWireState(gate.State); wire != 2 {
			t.Fatalf("gate %d closed as wire state %d want 2", mapID, wire)
		}
	}
	// The presentation itself must not fail on an online-less fixture: it is the
	// same call the timer makes before it arms the ten-second removal.
	if err := service.publishFailedRuntimeRaid(failed); err != nil {
		t.Fatalf("publish phase timeout: %v", err)
	}

	service.removeRuntimeRaidFailedPhase(raidKey, runtimeRaidDungeonPhaseOne)
	if _, alive := service.raids[raidKey]; alive {
		t.Fatal("the failed raid was never released after its result window")
	}
}

// 守卫：还没到点（或阶段已经不是活跃状态）的团不能被判失败。
func TestRuntimeRaidPhaseTimeoutRequiresAnExpiredActivePhase(t *testing.T) {
	now := time.Now().UTC()
	service, raidKey := newRuntimeRaidLifecycleService(now)
	state := service.raids[raidKey]
	state.AntonConfig = runtimeRaidBridgeTestAntonConfig()
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.Lifecycle.PhaseStartedAt = now
	state.Lifecycle.PhaseDeadline = now.Add(time.Hour)

	if _, err := service.expireRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, now); err == nil {
		t.Fatal("a phase inside its attack window was failed")
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		t.Fatalf("status=%d want active", state.Lifecycle.Status)
	}
}
