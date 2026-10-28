package dnfbridge

import (
	"fmt"
	"time"
)

// raid_movie_fallback.go —— 过场确认的**有界等待**。
//
// 症状
//
//	"第二台1阶段过了2分钟左右才出来翻牌"（玩家原话）。翻牌窗口不是被阶段结算拖住
//	的，而是被**某一个客户端**拖住的：阶段目标达成后服务器只广播结果与过场状态
//	（openRuntimeRaidPhaseMovie -> AwaitingMovie），窗口要等**每一个**有资格成员都
//	回 stage1 之后才开（confirmRuntimeRaidSettlementMovie 里的
//	runtimeRaidMovieRosterConfirmedLocked）。一个卡在读条/掉帧/挂机的客户端不回，
//	全团就一直等它——实测同一天的日志：raid 1507329 一阶段 flipped=6、二阶段
//	flipped=3（也就是说多数人根本没点，最后靠自动翻牌兜底），而阶段结果到翻牌之间
//	的空档量到过 137 秒。
//
// 参考实现同样不等：86JP 在过场结束处一次性武装 AutoSelectPendingPhaseOneCardsAsync
// （RaidHandler.Rewards.cs:355-416），10 秒后把还没点的人直接按最低空位翻掉。本文件
// 把"过场确认"这一层也补上有界等待，与已有的"自动翻牌"兜底衔接：
//
//	结果显示 -> 过场状态广播 -> [有界等待 30s] -> 窗口打开 -> [10s] -> 自动翻牌结算
//
// 为什么是 30 秒
//
//	正常过一次场只需要几秒，客户端自身的翻牌控件在未点选时会在第一态 2000ms + 最多
//	30×500ms 内强制揭示（DNF.exe 0x01661180 / 0x01660C90），而实测的病态空档是
//	137 秒。30 秒既远大于正常过场，也把最坏情况从"等最后一个客户端"压到"等 30 秒"。
//
// 迟到的 stage1 不会被丢掉
//
//	窗口已经打开之后才回过场确认的客户端，由 confirmRuntimeRaidSettlementMovie 的
//	追赶分支接住（记下确认并回 true），它会收到和其他人一样的 state3 + 翻牌状态，
//	而不是一个永远不来的 ACK。
const runtimeRaidMovieConfirmFallbackDelay = 30 * time.Second

// scheduleRuntimeRaidMovieConfirmFallback arms the bounded wait for one phase's
// movie confirmation. The timer name carries the raid and phase, so a re-entry
// into the same phase replaces the pending wait instead of adding a second one.
func (s *Service) scheduleRuntimeRaidMovieConfirmFallback(state runtimeRaidState) error {
	if s == nil || state.RaidKey == 0 || state.Lifecycle.MovieOpenedAt.IsZero() {
		return nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	name := fmt.Sprintf("raid-movie-confirm-fallback:%d:%d", state.RaidKey, state.Lifecycle.Phase)
	raidKey := state.RaidKey
	phase := state.Lifecycle.Phase
	openedAt := state.Lifecycle.MovieOpenedAt
	return queue.ScheduleAfter(name, runtimeRaidMovieConfirmFallbackDelay, func(time.Time) {
		s.fireRuntimeRaidMovieConfirmFallback(raidKey, phase, openedAt)
	})
}

func (s *Service) fireRuntimeRaidMovieConfirmFallback(raidKey uint32, phase byte, movieOpenedAt time.Time) {
	if s == nil || raidKey == 0 {
		return
	}
	now := runtimeRaidNormalizeTime(s.gameplayNow())
	ready, pending, ok := s.forceRuntimeRaidMovieRosterConfirmed(raidKey, phase, movieOpenedAt, now)
	if !ok {
		return
	}
	elapsed := time.Duration(0)
	if !movieOpenedAt.IsZero() && now.After(movieOpenedAt) {
		elapsed = now.Sub(movieOpenedAt)
	}
	s.logGameEvent(nil, "game-raid-movie-confirm-fallback-opened",
		"raid_key", raidKey,
		"raid_phase", phase,
		"pending_members", pending,
		"pending_count", len(pending),
		"movie_open_wait_ms", raidMillis(elapsed),
		"fallback_delay_ms", runtimeRaidMovieConfirmFallbackDelay.Milliseconds(),
		"reason", "not_every_eligible_member_confirmed_the_phase_result_movie")
	s.recordRaidStateEventLight(nil, "movie_confirm_fallback", &ready, map[string]any{
		"raid_phase":         int(phase),
		"pending_members":    pending,
		"pending_count":      len(pending),
		"movie_open_wait_ms": raidMillis(elapsed),
		"fallback_delay_ms":  runtimeRaidMovieConfirmFallbackDelay.Milliseconds(),
		"result":             "card_window_opened_without_every_movie_confirmation",
		"reason":             "not_every_eligible_member_confirmed_the_phase_result_movie",
		"client_visible":     "the_flip_appears_without_waiting_for_the_slowest_client",
	})
	if err := s.broadcastRuntimeRaidMovieComplete(ready); err != nil {
		s.logPacketEvent("game-raid-movie-confirm-fallback-publish-failed",
			"raid_key", raidKey,
			"raid_phase", phase,
			"pending_members", pending,
			"error", err)
	}
}

// forceRuntimeRaidMovieRosterConfirmed completes the movie barrier for every
// eligible member that has not confirmed it, and performs the same lifecycle
// transition confirmRuntimeRaidSettlementMovie performs when the last member
// confirms. It refuses to act unless the raid is still waiting on that exact
// movie (same phase, same MovieOpenedAt), so a member who confirmed everything
// on their own always wins the race.
func (s *Service) forceRuntimeRaidMovieRosterConfirmed(
	raidKey uint32,
	phase byte,
	movieOpenedAt time.Time,
	now time.Time,
) (runtimeRaidState, []uint16, bool) {
	if s == nil || raidKey == 0 {
		return runtimeRaidState{}, nil, false
	}
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil ||
		state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie ||
		state.Lifecycle.Phase != phase ||
		!state.Lifecycle.MovieOpenedAt.Equal(movieOpenedAt) {
		return runtimeRaidState{}, nil, false
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	pending := make([]uint16, 0, len(state.FrozenMembers))
	for _, member := range s.runtimeRaidRewardEligibleMembersLocked(state) {
		if state.Lifecycle.MovieConfirmed[member.CharID] {
			continue
		}
		state.Lifecycle.MovieConfirmed[member.CharID] = true
		pending = append(pending, member.CharID)
	}
	if len(pending) == 0 {
		return runtimeRaidState{}, nil, false
	}
	if phase != runtimeRaidDungeonPhaseOne && phase != runtimeRaidDungeonPhaseTwo {
		return runtimeRaidState{}, nil, false
	}
	// Mirrors confirmRuntimeRaidSettlementMovie's transition exactly: the phase
	// advances to its cards, the previous phase's reward state is cleared.
	state.Lifecycle.MovieOpenedAt = time.Time{}
	state.Lifecycle.PhaseDeadline = time.Time{}
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	state.Lifecycle.RewardToken = 0
	state.Lifecycle.RewardOpenedAt = now
	clear(state.Lifecycle.RewardSelections)
	state.Lifecycle.RewardDelivery = nil
	state.Lifecycle.RewardDeliveryCommitted = false
	state.Lifecycle.RewardDeliveryCommittedAt = time.Time{}
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), pending, true
}
