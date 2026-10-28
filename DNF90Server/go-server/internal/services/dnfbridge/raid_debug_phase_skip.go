package dnfbridge

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TEST AID -- local testing only; delete the marker files to go back to
// normal. Nothing has to be rebuilt or restarted either way.
//
//   - runtime/state/raid-skip-to-phase-two: a started TEAM raid opens phase two
//     directly instead of phase one.
//   - runtime/state/raid-skip-to-luke-throne: the same, and a Luke raid then
//     fast-forwards phase two to its final stage -- both altar entrances, both
//     energy rooms, both altars and both temples read cleared, 机械王座:明 is
//     open, and every member holds the phase's minimum clear count so the
//     settlement still pays.
//
// They are files and not environment variables on purpose: the control tool
// strips every DNFBRIDGE_* variable out of the inherited environment
// (cleanRuntimeBaseEnvironment in cmd/server/control/config.go) and rebuilds the
// runtime environment from the instance config, so an exported variable would
// silently never arrive.
//
// Only the skipped runs are skipped. The roster, the entry ticket, the daily
// counter and phase-two activation take the ordinary path, and the throne
// fast-forward walks the real clear hooks (advanceRuntimeRaidObjectiveLocked +
// applyRuntimeRaidLukePhaseTwoClearLocked) in the order the gate graph opens
// them, so the gate states and symbols are the ones a real run would leave.
// Phase one's rewards are not granted -- do not judge phase-one settlement by a
// run that used this.
const (
	runtimeRaidSkipToPhaseTwoMarker = "raid-skip-to-phase-two"
	runtimeRaidSkipToLukeThroneMark = "raid-skip-to-luke-throne"
)

// runtimeRaidDebugStateDir 定位本安装的 runtime/state 目录。
//
// 上游用的是 internal/platform/installpath（本树没有那个包）。这里就地按我们自己的目录约定解析：
// DNF90_PROJECT_ROOT 优先；否则从工作目录与可执行文件目录逐级向上找含 runtime/state 的根。
// 找不到返回 ""，调试开关静默失效 —— 正常跑本来就没有标记文件，行为与不加这一层完全一致。
func runtimeRaidDebugStateDir() string {
	if root := strings.TrimSpace(os.Getenv("DNF90_PROJECT_ROOT")); root != "" {
		return filepath.Join(root, "runtime", "state")
	}
	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		current, err := filepath.Abs(start)
		if err != nil {
			continue
		}
		for depth := 0; depth < 8; depth++ {
			candidate := filepath.Join(current, "runtime", "state")
			if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
				return candidate
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	return ""
}

func runtimeRaidDebugMarkerPresent(name string) bool {
	dir := runtimeRaidDebugStateDir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// lukeThroneFastForwardGates is phase two's gate graph up to the thrones, in
// the order each clear opens the next (raid_luke_phase_runtime.go).
var lukeThroneFastForwardGates = []int{
	lukeLightEntranceMapID, lukeDarkEntranceMapID,
	lukeLightEnergyMapID, lukeDarkEnergyMapID,
	lukeLightAltarMapID, lukeDarkAltarMapID,
	lukeLightTempleMapID, lukeDarkTempleMapID,
}

// fastForwardRuntimeRaidLukeToThroneLocked clears every gate in front of the
// thrones through the real clear hooks. Caller holds raidMu.
func fastForwardRuntimeRaidLukeToThroneLocked(state *runtimeRaidState, now time.Time) []runtimeRaidMechanismTimer {
	if state == nil || state.TailFlag == 1 || runtimeRaidTeamGraphOwner(*state) != runtimeRaidOwnerLuke ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo {
		return nil
	}
	var armed []runtimeRaidMechanismTimer
	for _, mapID := range lukeThroneFastForwardGates {
		for guard := 0; guard < 16; guard++ {
			objective, ok := state.Lifecycle.Objectives[mapID]
			if !ok || objective.RemainingHP <= 0 {
				break
			}
			if err := advanceRuntimeRaidObjectiveLocked(state, runtimeRaidDungeonPhaseTwo, mapID); err != nil {
				break
			}
			if after := state.Lifecycle.Objectives[mapID]; after.RemainingHP == objective.RemainingHP {
				break // the gate was not open; the graph did not reach it
			}
		}
		armed = append(armed, applyRuntimeRaidLukePhaseTwoClearLocked(state, mapID, now)...)
	}
	// The settlement pays only members that registered the phase's minimum.
	minimum := runtimeRaidPhaseMinimalClearCount(*state, runtimeRaidDungeonPhaseTwo)
	for _, member := range state.FrozenMembers {
		for count := 0; count < minimum; count++ {
			runtimeRaidNotePhaseClearLocked(state, member.CharID, runtimeRaidDungeonPhaseTwo)
		}
	}
	state.Lifecycle.Revision++
	return armed
}

// activateRuntimeRaidReadyPhase is the ready-timer's activation edge. Without
// a marker it is exactly what the timer always did.
func (s *Service) activateRuntimeRaidReadyPhase(raidKey uint32, due time.Time) error {
	toThrone := runtimeRaidDebugMarkerPresent(runtimeRaidSkipToLukeThroneMark)
	if !toThrone && !runtimeRaidDebugMarkerPresent(runtimeRaidSkipToPhaseTwoMarker) {
		_, err := s.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, due, false)
		return err
	}

	// activateRuntimeRaidPhase admits phase two only from the post-reward
	// standby. Put the lifecycle in that state, then take the ordinary phase-two
	// path with allowEarly so the standby deadline is not waited out.
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.TailFlag == 1 ||
		state.Lifecycle.Status != runtimeRaidLifecyclePreparing ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		s.raidMu.Unlock()
		_, err := s.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, due, false)
		return err
	}
	state.Lifecycle.Status = runtimeRaidLifecycleStandby
	state.Lifecycle.StandbyStartedAt = due
	state.Lifecycle.StandbyDeadline = due
	state.Lifecycle.Revision++
	s.raidMu.Unlock()

	s.logPacketEvent("game-raid-debug-skipped-to-phase-two",
		"raid_key", raidKey, "to_luke_throne", toThrone)
	if _, err := s.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseTwo, due, true); err != nil || !toThrone {
		return err
	}

	s.raidMu.Lock()
	armed := fastForwardRuntimeRaidLukeToThroneLocked(s.raids[raidKey], due)
	s.raidMu.Unlock()
	if err := s.scheduleRuntimeRaidMechanismTimers(raidKey, armed); err != nil {
		s.logPacketEvent("game-raid-debug-throne-timer-failed", "raid_key", raidKey, "error", err)
	}
	s.logPacketEvent("game-raid-debug-fast-forwarded-to-luke-throne", "raid_key", raidKey, "timers", len(armed))
	return s.publishRuntimeRaidObjectiveProgress(raidKey, runtimeRaidDungeonPhaseTwo)
}
