package dnfbridge

import (
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

var runtimeRaidChinaCalendar = time.FixedZone("Asia/Shanghai", 8*60*60)

// antonRaidAttackSeconds is the raid's TOTAL attack window: 4800 seconds, i.e. the
// two 2400-second phases the operator runs (etc/raid/anton.etc). It is not a per-phase
// value -- see runtimeRaidPhaseConfig for the 86JP constant that is.
const antonRaidAttackSeconds = 4800

// runtimeRaidSingleTypeForTime implements the official single-Anton weekly
// route using the China service calendar. The route is frozen when the raid
// starts, so crossing midnight cannot change an already-running lifecycle.
func runtimeRaidSingleTypeForTime(at time.Time) byte {
	switch at.In(runtimeRaidChinaCalendar).Weekday() {
	case time.Sunday, time.Monday:
		return 0
	case time.Tuesday, time.Wednesday, time.Thursday:
		return 3
	default:
		return 6
	}
}

// runtimeRaidAntonConfig loads only etc/raid/anton.etc from the already
// configured runtime archive. The immutable result is shared by all online
// attack parties for this process.
func (s *Service) runtimeRaidAntonConfig() (*raid.AntonConfig, error) {
	if s == nil {
		return nil, raid.ErrAntonPVFSourceRequired
	}
	s.raidAntonMu.Lock()
	defer s.raidAntonMu.Unlock()
	if s.raidAnton != nil {
		return s.raidAnton, nil
	}
	if s.raidAntonErr != nil {
		return nil, s.raidAntonErr
	}

	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.raidAntonErr = fmt.Errorf("load anton runtime archive: %w", err)
		return nil, s.raidAntonErr
	}
	config, err := raid.LoadAntonConfig(archive)
	if err != nil {
		s.raidAntonErr = fmt.Errorf("load current anton runtime config: %w", err)
		return nil, s.raidAntonErr
	}
	s.raidAnton = config
	return config, nil
}

func runtimeRaidAntonPhase(config *raid.AntonConfig, phase byte) (raid.AntonPhase, bool) {
	if config == nil || phase == 0 || int(phase) > len(config.Phases) {
		return raid.AntonPhase{}, false
	}
	return config.Phases[int(phase)-1], true
}

func runtimeRaidPhaseConfig(state runtimeRaidState, phase byte) (raid.AntonPhase, int, bool) {
	config := runtimeRaidBaseConfig(state)
	if config == nil || phase == 0 {
		return raid.AntonPhase{}, 0, false
	}
	if state.TailFlag == 1 {
		if phase != runtimeRaidDungeonPhaseOne || config.SingleMode == nil {
			return raid.AntonPhase{}, 0, false
		}
		return config.SingleMode.Phase, config.SingleMode.TimeLimitSeconds, true
	}
	phaseConfig, ok := runtimeRaidAntonPhase(config, phase)
	if !ok || int(phase) > len(config.PhaseTimeoutSeconds) {
		return raid.AntonPhase{}, 0, false
	}
	// ==========================================================================
	// 临时调试开关（操作员 2026-10-02 要求）：把**超时空**整个攻坚倒计时压到 1 分钟，
	// 方便在真机上验证「攻坚失败横幅」。安东/卢克一律不受影响。
	//
	// 改回正式值：把 currentFiendWarDebugPhaseSeconds 改成 0 即可（0 = 不干预，
	// 走 PVF fiendwar.etc 的 [PHASE TIME OVER] 3600）。只有这一处引用它。
	// ==========================================================================
	if state.RaidMode == runtimeRaidOwnerFiendWar && currentFiendWarDebugPhaseSeconds > 0 {
		return phaseConfig, currentFiendWarDebugPhaseSeconds, true
	}
	// Per phase, not per raid: the operator's rule is 2400 seconds for each of the
	// two phases, i.e. 4800 in total, which is what etc/raid/anton.etc carries and
	// what this server ships in the 0x0254/0x0255 rows.
	//
	// 86JP's own constant is 4800 *per phase* (RaidHandler.AntonDefinitions.cs:14
	// AttackSeconds = 4800u, pinned by RaidProtocolSelfTest.cs:91 as 80u * 60u), so
	// that tree would run this raid for twice as long. Operator decision
	// 2026-09-19: follow the 2400-per-phase model, never 86's per-phase 4800.
	return phaseConfig, config.PhaseTimeoutSeconds[int(phase)-1], true
}

// currentFiendWarDebugPhaseSeconds 见 runtimeRaidPhaseConfig 里的说明。
//
// 0    = 关闭（正式行为：超时空用 PVF 的 [PHASE TIME OVER]，当前是 3600 秒）
// >0   = 临时把超时空的阶段时限压到这个秒数，**只用于真机测试攻坚失败横幅**
//
// 操作员 2026-10-02：「先临时把整个攻坚倒计时缩短至1分钟 方便测试 后面再改回去」。
// 改回去只需要把下面这个数字改成 0。
//
// 2026-10-02 第 11 轮收尾：已按操作员要求改回 0（正式 3600 秒）。第 11 轮的
// 横幅实验不再依赖它 —— 手动灌包只需要客户端真的存在 raidObj，与阶段时限无关。
const currentFiendWarDebugPhaseSeconds = 0

// runtimeRaidRewardPhaseIndex keeps the single-mode reward pool separate from
// team phase one, whose wire phase number is also one. The catalog uses the
// zero-based team phase indexes, so the single pool is appended after them.
func runtimeRaidRewardPhaseIndex(state runtimeRaidState, phase byte) (int, bool) {
	config := runtimeRaidBaseConfig(state)
	if config == nil || phase == 0 {
		return 0, false
	}
	if state.TailFlag == 1 {
		if phase != runtimeRaidDungeonPhaseOne || config.SingleMode == nil {
			return 0, false
		}
		return len(config.Phases), true
	}
	if int(phase) > len(config.Phases) {
		return 0, false
	}
	return int(phase) - 1, true
}
