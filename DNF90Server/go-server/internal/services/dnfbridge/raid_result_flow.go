package dnfbridge

import (
	"errors"
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	runtimeRaidSettlementStateResult      byte = 3
	runtimeRaidSettlementStateMovieDone   byte = 4
	runtimeRaidSettlementStateCardsOpened byte = 5
	runtimeRaidSettlementStateFinished    byte = 6
	// runtimeRaidFailedState is the attack-window timeout state of the raid object,
	// with its 86JP argument: RaidManager.TryFailPhase sets State = 4 and
	// StateArgument = 1 "to distinguish timeout failure from the successful final
	// reward state" (RaidManager.cs:1975-1977).
	runtimeRaidFailedState         byte = 4
	runtimeRaidFailedStateArgument      = 1
	// runtimeRaidFailResultType is the 0x0259 result type of a failed phase; 1 is
	// the value 86JP ships with the timeout result
	// (RaidPacketBuilder.BuildRaidResult(1u, phaseIndex, ...)).
	runtimeRaidFailResultType          byte = 1
	runtimeRaidRewardOpenCountdown          = 3 * time.Second
	runtimeRaidSingleSquadRewardDelay       = 2 * time.Second
	runtimeRaidSingleRewardFinishDelay      = 5 * time.Second
	// runtimeRaidTeamRewardResultType is the 0x0258 result type of the whole-raid
	// reward row 86JP broadcasts after every member has picked (see
	// runtimeRaidTeamSquadRewardBody). It is deliberately not
	// runtimeRaidRewardResultType: that value selects the per-squad reveal board,
	// this one the attack-party board.
	runtimeRaidTeamRewardResultType byte = 3
	// runtimeRaidRewardAutoFlipDelay is how long the open card window waits for a
	// member that is online but never picks.
	//
	// 86JP does the same thing from the same place: AutoSelectPendingPhaseOneCardsAsync
	// (RaidHandler.Rewards.cs:383-416) awaits Task.Delay(10000), walks
	// flow.GetPendingUserIds() and gives each pending member the lowest free card
	// index, recording operation 1 then 0, revealing and granting. It is armed
	// once, when the movie ends (:355-358), and TryRecordCardOperation (:196-226)
	// makes a repeat a no-op, so a real click that lands first simply wins.
	//
	// The client's own card widget bounds the window from above: an un-picked card
	// runs 2000ms in its first state plus at most 30 x 500ms in its second before
	// it force-reveals (DNF.exe 0x01661180 / 0x01660C90; the state-2 tick
	// 0x0165F930 calls the C2S 660 sender 0x0165F390 over card ordinals 0..3), so
	// nothing may still be waiting after roughly 17s. Ten seconds leaves room for
	// the reveal and the 0x0258 fan-out inside that bound.
	runtimeRaidRewardAutoFlipDelay = 10 * time.Second
)

var (
	errRuntimeRaidSettlementPhase       = errors.New("runtime raid settlement phase is invalid")
	errRuntimeRaidSettlementPhaseResult = errors.New("runtime raid settlement result phase does not match the active phase")
	errRuntimeRaidSettlementRoster      = errors.New("runtime raid settlement frozen roster is empty")
)

// runtimeRaidSettlementMovieOwner is the state-machine hand-off used after
// C2S 659/stage1. It must keep the current phase in its card-selection state;
// phase one does not advance to phase two until its durable reward delivery
// has completed and the standby transition has run.
type runtimeRaidSettlementMovieOwner interface {
	confirmRuntimeRaidSettlementMovie(characterID uint16, phase byte, now time.Time) (runtimeRaidState, bool, error)
}

// runtimeRaidSettlementCardOwner owns the native mode1 -> mode0, same-card
// ordering. The bool is true only after every frozen member confirmed mode0.
type runtimeRaidSettlementCardOwner interface {
	selectRuntimeRaidSettlementCard(characterID uint16, phase byte, request raid.RaidRewardCardRequest, now time.Time) (runtimeRaidState, bool, error)
}

// runtimeRaidSettlementRewardDelivery is produced only after the reward unit
// of work has committed every frozen member's phase reward durably.
type runtimeRaidSettlementRewardDelivery struct {
	Raid               runtimeRaidState
	Token              uint64
	Rewards            raid.RaidSettlementRewards
	RewardBody         []byte
	PersonalRewardBody []byte
	TeamRewardBody     []byte
	InventoryRefreshes []runtimeRaidRewardInventoryRefresh
	WalletRefreshes    []runtimeRaidRewardWalletRefresh
}

// runtimeRaidSettlementRewardCompletion describes the post-0x0258 lifecycle
// transition. Phase one returns the native 600-second standby timer; phase two
// returns no timer and may finalize the global runtime object.
type runtimeRaidSettlementRewardCompletion struct {
	Raid         runtimeRaidState
	StandbyTimer *raid.RaidTimer
}

// runtimeRaidSettlementRewardOwner is deliberately split around the wire
// publication boundary: commit first, publish 0x0258, then enter standby or
// final cleanup. Both methods must be idempotent by Token.
type runtimeRaidSettlementRewardOwner interface {
	commitRuntimeRaidSettlementRewards(raidKey uint32, phase byte, now time.Time) (runtimeRaidSettlementRewardDelivery, error)
	completeRuntimeRaidSettlementRewards(raidKey uint32, phase byte, token uint64, now time.Time) (runtimeRaidSettlementRewardCompletion, error)
}

type runtimeRaidWireNotification struct {
	msgID uint16
	body  []byte
}

type runtimeRaidRewardPublication struct {
	RewardListMembers         map[uint16]bool
	SingleSquadRewardMembers  map[uint16]bool
	SingleFinishedStateMember map[uint16]bool
	StandbyTimerMembers       map[uint16]bool
	// TeamSquadRewardMembers is the fan-out ledger of the whole-raid team row, so
	// a retry of the same settlement cannot deliver the board twice.
	TeamSquadRewardMembers map[uint16]bool
}

// publishRuntimeRaidPhaseSettlement is the objective-owner entry point. The
// caller proves the current phase objective before entering it. The lifecycle
// transition is committed before the exact native result -> state3 sequence is
// broadcast, so a retry can safely replay the same presentation.
func (s *Service) publishRuntimeRaidPhaseSettlement(
	raidKey uint32,
	phase byte,
	result raid.RaidSettlementResult,
	now time.Time,
) error {
	wirePhase, err := runtimeRaidSettlementWirePhase(phase)
	if err != nil {
		return err
	}
	if result.PhaseIndex != wirePhase {
		return fmt.Errorf("%w: runtime=%d wire=%d", errRuntimeRaidSettlementPhaseResult, phase, result.PhaseIndex)
	}
	// 0x0259 的 deaths：这个 u16 是"本阶段全团死亡次数"，**所有团本都填真实值**
	// （服主 2026-09-29 拍板；卢克结算条画的就是它，实测确实死了并用了复活币、
	// 结算条却写「死亡次数 0」）。依据：86JP 的
	// RAID_RESULT(0, phase, clearTime, deaths, rank+2, rewardOption)
	// （86raid_flow_report.md:100），以及 luke.etc [CONDITION FOR RANK] 按
	// [DEAD COUNT] 分档。调用方一律不自己填这个字段（raid_phase_runtime.go 的
	// 生产调用点留空），唯一来源是接受 op40 那条边沿记下的账。
	// 见 raid_phase_death_count.go。
	result.DeathCount = s.runtimeRaidPhaseDeathCount(raidKey, phase)
	resultBody, err := raid.BuildRaidSettlementResultBody(result)
	if err != nil {
		return err
	}
	state, err := s.openRuntimeRaidPhaseMovie(raidKey, phase, now)
	if err != nil {
		return err
	}
	// The card window must not wait for the slowest client's result-movie
	// confirmation: see raid_movie_fallback.go. Armed before the broadcast so a
	// client that confirms immediately still wins the race (the fallback refuses
	// to act once the status has left AwaitingMovie).
	if err := s.scheduleRuntimeRaidMovieConfirmFallback(state); err != nil {
		s.logPacketEvent("game-raid-movie-confirm-fallback-schedule-failed",
			"raid_key", raidKey,
			"raid_phase", phase,
			"error", err)
	}
	notifications := []runtimeRaidWireNotification{
		runtimeRaidWireNotification{msgID: raid.RaidResultMsgID, body: resultBody},
		runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidSettlementStateResult,
				Argument: int8(wirePhase),
			}),
		},
	}
	// 超时空：结果面板要先收到 1453{3,0}(战况表切"结算中")，再收结果体。
	// 安东/卢克没有这一帧。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		notifications = append([]runtimeRaidWireNotification{{msgID: 1453, body: []byte{3, 0}}}, notifications...)
	}
	return s.broadcastRuntimeRaidWireSequence(state, notifications...)
}

func runtimeRaidSettlementWirePhase(phase byte) (byte, error) {
	switch phase {
	case runtimeRaidDungeonPhaseOne:
		return 0, nil
	case runtimeRaidDungeonPhaseTwo:
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: %d", errRuntimeRaidSettlementPhase, phase)
	}
}

func (s *Service) handleRuntimeRaidResultStage(session *gameSession, body []byte) error {
	owner, _ := any(s).(runtimeRaidSettlementMovieOwner)
	return s.handleRuntimeRaidResultStageWithOwner(session, body, owner)
}

func (s *Service) handleRuntimeRaidResultStageWithOwner(
	session *gameSession,
	body []byte,
	owner runtimeRaidSettlementMovieOwner,
) error {
	request, err := raid.DecodeRaidResultStageRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-result-stage-body-invalid",
			"body_len", len(body),
			"error", err)
		return nil
	}
	characterID := selectedCharacterID(session)
	state, ok := s.runtimeRaidForMember(characterID)
	if !ok || !state.Started || !state.EntryCostPaid {
		s.logGameEvent(session, "game-raid-result-stage-rejected",
			"stage", request.Stage,
			"char_id", characterID,
			"reason", "started_paid_raid_not_found")
		return nil
	}
	if _, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase); err != nil {
		return err
	}

	switch request.Stage {
	case 0:
		if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie {
			s.logGameEvent(session, "game-raid-result-stage-rejected",
				"stage", request.Stage,
				"raid_key", state.RaidKey,
				"raid_phase", state.Lifecycle.Phase,
				"lifecycle_status", state.Lifecycle.Status,
				"reason", "movie_not_open")
			return nil
		}
		if err := s.sendGameUpperRawClassCodec(
			session,
			raid.RaidResultStageRequestMsgID,
			raid.BuildRaidResultStageAckBody(),
			dnfproto.DefaultChannelClassification,
			true,
		); err != nil {
			return err
		}
		return s.sendGameUpperRawClassCodec(
			session,
			raid.RaidMovieMsgID,
			raid.BuildRaidMovieSkipBody(),
			0,
			true,
		)
	case 1:
		if owner == nil {
			s.logGameEvent(session, "game-raid-result-stage-deferred",
				"stage", request.Stage,
				"raid_key", state.RaidKey,
				"raid_phase", state.Lifecycle.Phase,
				"reason", "phase_reward_lifecycle_owner_unavailable")
			return nil
		}
		state, rosterConfirmed, err := owner.confirmRuntimeRaidSettlementMovie(
			characterID,
			state.Lifecycle.Phase,
			time.Now().UTC(),
		)
		if err != nil {
			s.logGameEvent(session, "game-raid-result-stage-rejected",
				"stage", request.Stage,
				"raid_key", state.RaidKey,
				"error", err)
			return nil
		}
		if err := s.sendGameUpperRawClassCodec(
			session,
			raid.RaidResultStageRequestMsgID,
			raid.BuildRaidResultStageAckBody(),
			dnfproto.DefaultChannelClassification,
			true,
		); err != nil {
			return err
		}
		if !rosterConfirmed {
			return nil
		}
		return s.broadcastRuntimeRaidMovieComplete(state)
	default:
		return nil
	}
}

func (s *Service) broadcastRuntimeRaidMovieComplete(state runtimeRaidState) error {
	wirePhase, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase)
	if err != nil {
		return err
	}
	openedAt := state.Lifecycle.RewardOpenedAt
	if openedAt.IsZero() {
		openedAt = runtimeRaidNormalizeTime(s.gameplayNow())
		state.Lifecycle.RewardOpenedAt = openedAt
	}
	deadline := openedAt.Add(runtimeRaidRewardOpenCountdown)
	seconds := runtimeRaidDurationSeconds(runtimeRaidRewardOpenCountdown)
	timer := raid.RaidTimer{
		TimerType: 0,
		Value0:    runtimeRaidTopTimerKey,
		Value1:    uint32(deadline.Unix()),
		Value2:    seconds,
	}
	if err := s.broadcastRuntimeRaidWireSequence(state,
		runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidSettlementStateMovieDone,
				Argument: int8(wirePhase),
			}),
		},
		runtimeRaidWireNotification{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(timer)},
		runtimeRaidWireNotification{
			msgID: raid.RaidRemainTimeMsgID,
			body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: seconds}),
		},
	); err != nil {
		return err
	}
	return s.scheduleRuntimeRaidCardsOpened(state, deadline)
}

func (s *Service) scheduleRuntimeRaidCardsOpened(state runtimeRaidState, deadline time.Time) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	delay := deadline.Sub(queue.Now())
	if delay < 0 {
		delay = 0
	}
	name := fmt.Sprintf("raid-cards-open:%d:%d:%d", state.RaidKey, state.Lifecycle.Phase, state.StartAttempt)
	return queue.ScheduleAfter(name, delay, func(_ time.Time) {
		s.raidMu.Lock()
		current := s.raids[state.RaidKey]
		if current == nil || current.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
			current.Lifecycle.Phase != state.Lifecycle.Phase ||
			!current.Lifecycle.RewardOpenedAt.Equal(state.Lifecycle.RewardOpenedAt) {
			s.raidMu.Unlock()
			return
		}
		ready := cloneRuntimeRaidState(*current)
		s.raidMu.Unlock()
		if err := s.publishRuntimeRaidCardsOpened(ready); err != nil {
			s.logPacketEvent("game-raid-cards-open-publish-failed",
				"raid_key", ready.RaidKey,
				"raid_phase", ready.Lifecycle.Phase,
				"error", err)
			return
		}
		// The window is open and one member may never pick. This is exactly where
		// 86JP arms its own auto-flip: once, right after the cards are published.
		if err := s.scheduleRuntimeRaidRewardAutoFlip(ready); err != nil {
			s.logPacketEvent("game-raid-reward-auto-flip-schedule-failed",
				"raid_key", ready.RaidKey,
				"raid_phase", ready.Lifecycle.Phase,
				"error", err)
		}
	})
}

// scheduleRuntimeRaidRewardAutoFlip arms the one automatic settlement of the
// open card window. The timer name is keyed by the same triple the cards-open
// timer uses, and the queue replaces a task scheduled under a name it already
// holds, so re-entering the window cannot leave two flips pending.
func (s *Service) scheduleRuntimeRaidRewardAutoFlip(state runtimeRaidState) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	delay := state.Lifecycle.RewardOpenedAt.Add(runtimeRaidRewardAutoFlipDelay).Sub(queue.Now())
	if delay < 0 {
		delay = 0
	}
	name := fmt.Sprintf("raid-reward-auto-flip:%d:%d:%d", state.RaidKey, state.Lifecycle.Phase, state.StartAttempt)
	return queue.ScheduleAfter(name, delay, func(_ time.Time) {
		s.autoFlipRuntimeRaidPendingCards(state)
	})
}

// autoFlipRuntimeRaidPendingCards picks for every eligible member that still has
// no card, then settles the phase exactly as the last real click would have.
//
// It only ever touches the eligible roster: a member the settlement already
// wrote off keeps having no card, gets no grant, and cannot hold the roster --
// so this is complementary to refreshRuntimeRaidRewardSkipsLocked, never a
// second path around it. Nothing is sent to any client here: the members are
// asleep at the keyboard, and the publication below is what tells every client
// what it won.
func (s *Service) autoFlipRuntimeRaidPendingCards(expected runtimeRaidState) {
	s.raidMu.Lock()
	current := s.raids[expected.RaidKey]
	if current == nil ||
		current.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		current.Lifecycle.Phase != expected.Lifecycle.Phase ||
		!current.Lifecycle.RewardOpenedAt.Equal(expected.Lifecycle.RewardOpenedAt) ||
		current.Lifecycle.RewardDeliveryCommitted {
		s.raidMu.Unlock()
		return
	}
	pending := make([]uint16, 0, len(current.FrozenMembers))
	eligible := s.runtimeRaidRewardEligibleMembersLocked(current)
	for _, member := range eligible {
		if selection, ok := current.Lifecycle.RewardSelections[member.CharID]; ok && selection.Mode == 0 {
			continue
		}
		pending = append(pending, member.CharID)
	}
	raidKey := current.RaidKey
	phase := current.Lifecycle.Phase
	rosterSize := len(current.FrozenMembers)
	openedAt := current.Lifecycle.RewardOpenedAt
	s.raidMu.Unlock()
	if len(pending) == 0 {
		return
	}

	now := runtimeRaidNormalizeTime(s.gameplayNow())
	flipped := 0
	skipped := make([]map[string]any, 0, len(pending))
	for _, characterID := range pending {
		// mode1 then mode0 is the order the client itself sends and the only one
		// selectRuntimeRaidSettlementCard accepts; index 0 is a seat the wire
		// accepts and the first slot 86JP's GetOrCreateAvailableCardIndex tries.
		if _, _, err := s.selectRuntimeRaidSettlementCard(characterID, phase,
			raid.RaidRewardCardRequest{Mode: 1, CardIndex: 0}, now); err != nil {
			s.logPacketEvent("game-raid-reward-auto-flip-skipped",
				"raid_key", raidKey, "raid_phase", phase, "char_id", characterID,
				"stage", "reveal", "error", err)
			skipped = append(skipped, map[string]any{
				"member_id": characterID, "stage": "reveal", "error": errorText(err),
			})
			continue
		}
		if _, _, err := s.selectRuntimeRaidSettlementCard(characterID, phase,
			raid.RaidRewardCardRequest{Mode: 0, CardIndex: 0}, now); err != nil {
			s.logPacketEvent("game-raid-reward-auto-flip-skipped",
				"raid_key", raidKey, "raid_phase", phase, "char_id", characterID,
				"stage", "confirm", "error", err)
			skipped = append(skipped, map[string]any{
				"member_id": characterID, "stage": "confirm", "error": errorText(err),
			})
			continue
		}
		flipped++
	}
	if flipped == 0 && len(skipped) == 0 {
		return
	}

	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		state.Lifecycle.Phase != phase || state.Lifecycle.RewardDeliveryCommitted {
		s.raidMu.Unlock()
		return
	}
	settled := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	owner, _ := any(s).(runtimeRaidSettlementRewardOwner)
	if err := s.commitAndPublishRuntimeRaidSettlementRewardsWithRetry(settled, owner, now); err != nil {
		if !errors.Is(err, errRuntimeRaidRewardNotReady) {
			s.logPacketEvent("game-raid-reward-auto-flip-commit-failed",
				"raid_key", raidKey, "raid_phase", phase, "flipped", flipped, "error", err)
			s.recordRaidStateEventLight(nil, "reward_incomplete", &settled, map[string]any{
				"actor_char_id":  0,
				"raid_phase":     int(phase),
				"auto_flip":      true,
				"flipped":        flipped,
				"pending":        len(pending),
				"eligible":       len(eligible),
				"roster_size":    rosterSize,
				"flip_skipped":   skipped,
				"wait_seconds":   raidMillis(now.Sub(openedAt)) / 1000,
				"result":         "commit_failed",
				"reason":         errorText(err),
				"client_visible": "card_window_stays_open_and_the_flip_never_settles",
			})
			return
		}
		// Someone still has no card: the eligible roster is wider than the members
		// this timer picked for (a member rejoined, or one pick was rejected).
		// Nothing is committed, and that member's own click still settles the
		// phase exactly as before.
		s.logPacketEvent("game-raid-reward-auto-flip-incomplete",
			"raid_key", raidKey, "raid_phase", phase, "flipped", flipped)
		s.recordRaidStateEventLight(nil, "reward_incomplete", &settled, map[string]any{
			"raid_phase":     int(phase),
			"auto_flip":      true,
			"flipped":        flipped,
			"pending":        len(pending),
			"eligible":       len(eligible),
			"roster_size":    rosterSize,
			"flip_skipped":   skipped,
			"wait_seconds":   raidMillis(now.Sub(openedAt)) / 1000,
			"result":         "roster_not_covered",
			"reason":         "some_eligible_member_still_holds_no_card",
			"client_visible": "flip_waits_for_that_members_own_click",
		})
		return
	}
	s.logPacketEvent("game-raid-reward-auto-flipped",
		"raid_key", raidKey, "raid_phase", phase, "flipped", flipped,
		"reason", "no_manual_pick_inside_the_client_force_reveal_window")
	s.recordRaidStateEventLight(nil, "reward_auto_flipped", &settled, map[string]any{
		"raid_phase":        int(phase),
		"flipped":           flipped,
		"pending":           len(pending),
		"eligible":          len(eligible),
		"roster_size":       rosterSize,
		"flip_skipped":      skipped,
		"wait_seconds":      raidMillis(now.Sub(openedAt)) / 1000,
		"auto_flip_delay_s": int(runtimeRaidRewardAutoFlipDelay / time.Second),
		"result":            "settled",
		"reason":            "no_manual_pick_inside_the_client_force_reveal_window",
	})
}

func (s *Service) publishRuntimeRaidCardsOpened(state runtimeRaidState) error {
	wirePhase, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase)
	if err != nil {
		return err
	}
	if state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		// The client's raid-object state 5 means "between phases, standby": its
		// op 0x024B handler (sub_1D50040, 0x01D500B5 mov [ebx+0x28], edi) writes
		// exactly this byte, the 战况表 row keeper sub_00F176A0 keeps rows for
		// {2,3,5}, and the standby HUD then reads 0x0255 timerType 1. Phase two has
		// no successor to wait for -- PVF anton.etc turns the 220 heart clear into
		// [CLEAR PHASE], and 86JP's Rewards.cs:634-658 sends nothing but
		// CancelAllPhaseTwoTimers + ENABLE_CLEAR_DUNGEON + cleanup for phase 2 --
		// so a 5 here resurrects a standby countdown after the raid is already
		// over (live 2026-09-16T15:45:25Z: 587(5,1) then four rejected START_RAID
		// attempts, the last two after the stale timer had expired).
		s.logPacketEvent("game-raid-cards-open-skipped",
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"reason", "no_standby_state_after_final_phase")
		s.recordRaidStateEventLight(nil, "reward_cards_open_skipped", &state, map[string]any{
			"raid_phase": int(state.Lifecycle.Phase),
			"result":     "skipped",
			"reason":     "no_standby_state_after_final_phase",
		})
		return nil
	}
	if err := s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{
		msgID: raid.RaidStateMsgID,
		body: raid.BuildRaidStateBody(raid.RaidState{
			State:    runtimeRaidSettlementStateCardsOpened,
			Argument: int8(wirePhase),
		}),
	}); err != nil {
		return err
	}
	s.recordRaidStateEventLight(nil, "reward_cards_opened", &state, map[string]any{
		"raid_phase":        int(state.Lifecycle.Phase),
		"opened_at":         state.Lifecycle.RewardOpenedAt.UTC().Format(time.RFC3339Nano),
		"auto_flip_delay_s": int(runtimeRaidRewardAutoFlipDelay / time.Second),
		"open_countdown_s":  int(runtimeRaidRewardOpenCountdown / time.Second),
		"result":            "window_open",
	})
	return nil
}

func (s *Service) handleRuntimeRaidRewardCard(session *gameSession, body []byte) error {
	cardOwner, _ := any(s).(runtimeRaidSettlementCardOwner)
	rewardOwner, _ := any(s).(runtimeRaidSettlementRewardOwner)
	return s.handleRuntimeRaidRewardCardWithOwners(session, body, cardOwner, rewardOwner)
}

func (s *Service) handleRuntimeRaidRewardCardWithOwners(
	session *gameSession,
	body []byte,
	cardOwner runtimeRaidSettlementCardOwner,
	rewardOwner runtimeRaidSettlementRewardOwner,
) error {
	request, err := raid.DecodeRaidRewardCardRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-reward-card-body-invalid",
			"body_len", len(body),
			"error", err)
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	if request.Mode != 0 && request.Mode != 1 {
		s.logGameEvent(session, "game-raid-reward-card-rejected",
			"mode", request.Mode,
			"card_index", request.CardIndex,
			"reason", "current_anton_flow_requires_mode1_then_mode0")
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	characterID := selectedCharacterID(session)
	state, ok := s.runtimeRaidForMember(characterID)
	if !ok || !state.Started || !state.EntryCostPaid {
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	if cardOwner == nil {
		s.logGameEvent(session, "game-raid-reward-card-deferred",
			"mode", request.Mode,
			"card_index", request.CardIndex,
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"reason", "phase_reward_lifecycle_owner_unavailable")
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	state, rosterConfirmed, err := cardOwner.selectRuntimeRaidSettlementCard(
		characterID,
		state.Lifecycle.Phase,
		request,
		time.Now().UTC(),
	)
	if err != nil {
		s.logGameEvent(session, "game-raid-reward-card-rejected",
			"mode", request.Mode,
			"card_index", request.CardIndex,
			"raid_key", state.RaidKey,
			"error", err)
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	if request.Mode != 0 || !rosterConfirmed {
		return s.sendRuntimeRaidRewardCardSuccess(session)
	}

	// Serialize the durable commit, ACK, reward publication, and lifecycle
	// completion. A duplicate mode0 request can arrive as soon as the client's
	// first ACK is decoded; allowing two publication flows to interleave would
	// replay 0x0258 or advance standby while another flow is still writing.
	s.raidRewardFlowMu.Lock()
	defer s.raidRewardFlowMu.Unlock()
	delivery, err := rewardOwner.commitRuntimeRaidSettlementRewards(state.RaidKey, state.Lifecycle.Phase, time.Now().UTC())
	if err != nil {
		s.logGameEvent(session, "game-raid-reward-card-commit-failed",
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"error", err)
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	if err := validateRuntimeRaidSettlementRewardDelivery(state, delivery); err != nil {
		s.logGameEvent(session, "game-raid-reward-card-commit-invalid",
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"error", err)
		return s.sendRuntimeRaidRewardCardFailure(session)
	}
	if err := s.sendRuntimeRaidRewardCardSuccess(session); err != nil {
		return err
	}
	return s.publishCommittedRuntimeRaidSettlementRewardsWithRetry(delivery, rewardOwner, 1)
}

const (
	runtimeRaidRewardPublishRetryDelay  = 3 * time.Second
	runtimeRaidRewardPublishMaxAttempts = 20
)

func runtimeRaidRewardPublishRetryName(delivery runtimeRaidSettlementRewardDelivery) string {
	return fmt.Sprintf("raid-reward-publish:%d:%d:%d", delivery.Raid.RaidKey, delivery.Raid.Lifecycle.Phase, delivery.Token)
}

// publishCommittedRuntimeRaidSettlementRewardsWithRetry makes reward delivery
// server-owned. The last mode0 confirmation is the only production trigger of
// the publication, and the per-member fan-out stops at the first dead socket;
// without a retry the members after that socket never receive 0x0258 and the
// raid stays in AwaitingRewards with phase two never opening. Each retry skips
// members already in the publication ledger, so nothing is delivered twice, and
// a socket that has meanwhile been unbound is skipped as offline. After the
// last attempt the authoritative completion runs anyway; the durable grant is
// already committed and a reconnecting member replays its cached list.
// Caller holds raidRewardFlowMu.
func (s *Service) publishCommittedRuntimeRaidSettlementRewardsWithRetry(
	delivery runtimeRaidSettlementRewardDelivery,
	owner runtimeRaidSettlementRewardOwner,
	attempt int,
) error {
	err := s.publishCommittedRuntimeRaidSettlementRewards(delivery, owner)
	if err == nil {
		return nil
	}
	if attempt >= runtimeRaidRewardPublishMaxAttempts {
		s.logPacketEvent("game-raid-reward-publish-giving-up",
			"raid_key", delivery.Raid.RaidKey,
			"raid_phase", delivery.Raid.Lifecycle.Phase,
			"token", delivery.Token,
			"attempts", attempt,
			"error", err)
		return s.forceRuntimeRaidSettlementCompletionLocked(delivery, owner)
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return err
	}
	next := attempt + 1
	if scheduleErr := queue.ScheduleAfter(runtimeRaidRewardPublishRetryName(delivery), runtimeRaidRewardPublishRetryDelay, func(time.Time) {
		s.raidRewardFlowMu.Lock()
		defer s.raidRewardFlowMu.Unlock()
		if retryErr := s.publishCommittedRuntimeRaidSettlementRewardsWithRetry(delivery, owner, next); retryErr != nil {
			s.logPacketEvent("game-raid-reward-publish-retry-failed",
				"raid_key", delivery.Raid.RaidKey, "attempt", next, "error", retryErr)
		}
	}); scheduleErr != nil {
		return errors.Join(err, scheduleErr)
	}
	s.logPacketEvent("game-raid-reward-publish-retry-scheduled",
		"raid_key", delivery.Raid.RaidKey,
		"raid_phase", delivery.Raid.Lifecycle.Phase,
		"token", delivery.Token,
		"attempt", attempt,
		"next_attempt", next,
		"delay_seconds", int(runtimeRaidRewardPublishRetryDelay/time.Second),
		"error", err)
	return nil
}

// forceRuntimeRaidSettlementCompletionLocked advances the lifecycle when the
// presentation could not be delivered to every member. Caller holds
// raidRewardFlowMu.
func (s *Service) forceRuntimeRaidSettlementCompletionLocked(
	delivery runtimeRaidSettlementRewardDelivery,
	owner runtimeRaidSettlementRewardOwner,
) error {
	if delivery.Raid.TailFlag == 1 {
		// The single flow schedules its own finish; leave it to that path.
		return nil
	}
	completion, err := owner.completeRuntimeRaidSettlementRewards(
		delivery.Raid.RaidKey,
		delivery.Raid.Lifecycle.Phase,
		delivery.Token,
		time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	if completion.StandbyTimer == nil {
		return nil
	}
	key := runtimeRaidRewardCompletionKey{
		RaidKey: delivery.Raid.RaidKey,
		Phase:   delivery.Raid.Lifecycle.Phase,
		Token:   delivery.Token,
	}
	return s.publishRuntimeRaidStandbyTimerLocked(completion, s.runtimeRaidRewardPublicationLocked(key))
}

func (s *Service) sendRuntimeRaidRewardCardSuccess(session *gameSession) error {
	return s.sendGameUpperRawClassCodec(
		session,
		raid.RaidRewardCardRequestMsgID,
		raid.BuildRaidRewardCardAckBody(true, 0),
		dnfproto.DefaultChannelClassification,
		true,
	)
}

func (s *Service) sendRuntimeRaidRewardCardFailure(session *gameSession) error {
	return s.sendGameUpperRawClassCodec(
		session,
		raid.RaidRewardCardRequestMsgID,
		raid.BuildRaidRewardCardAckBody(false, runtimeRaidFailureCode),
		dnfproto.DefaultChannelClassification,
		true,
	)
}

func (s *Service) commitAndPublishRuntimeRaidSettlementRewards(
	state runtimeRaidState,
	owner runtimeRaidSettlementRewardOwner,
	now time.Time,
) error {
	if owner == nil {
		s.logPacketEvent("game-raid-reward-delivery-deferred",
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"reason", "durable_phase_reward_owner_unavailable")
		return nil
	}
	s.raidRewardFlowMu.Lock()
	defer s.raidRewardFlowMu.Unlock()
	delivery, err := owner.commitRuntimeRaidSettlementRewards(state.RaidKey, state.Lifecycle.Phase, now)
	if err != nil {
		return err
	}
	if err := validateRuntimeRaidSettlementRewardDelivery(state, delivery); err != nil {
		return err
	}
	return s.publishCommittedRuntimeRaidSettlementRewards(delivery, owner)
}

// commitAndPublishRuntimeRaidSettlementRewardsWithRetry is the same commit with
// the click path's per-member retry instead of a single attempt. The automatic
// flip uses it: nobody is at the keyboard to trigger the retry, so a member whose
// socket is momentarily unwritable would otherwise leave the phase in
// AwaitingRewards forever.
func (s *Service) commitAndPublishRuntimeRaidSettlementRewardsWithRetry(
	state runtimeRaidState,
	owner runtimeRaidSettlementRewardOwner,
	now time.Time,
) error {
	if owner == nil {
		s.logPacketEvent("game-raid-reward-delivery-deferred",
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"reason", "durable_phase_reward_owner_unavailable")
		return nil
	}
	s.raidRewardFlowMu.Lock()
	defer s.raidRewardFlowMu.Unlock()
	delivery, err := owner.commitRuntimeRaidSettlementRewards(state.RaidKey, state.Lifecycle.Phase, now)
	if err != nil {
		return err
	}
	if err := validateRuntimeRaidSettlementRewardDelivery(state, delivery); err != nil {
		return err
	}
	return s.publishCommittedRuntimeRaidSettlementRewardsWithRetry(delivery, owner, 1)
}

func validateRuntimeRaidSettlementRewardDelivery(state runtimeRaidState, delivery runtimeRaidSettlementRewardDelivery) error {
	if delivery.Token == 0 || delivery.Raid.RaidKey != state.RaidKey || delivery.Raid.Lifecycle.Phase != state.Lifecycle.Phase {
		return fmt.Errorf("runtime raid reward owner returned mismatched delivery")
	}
	return nil
}

func (s *Service) publishCommittedRuntimeRaidSettlementRewards(
	delivery runtimeRaidSettlementRewardDelivery,
	owner runtimeRaidSettlementRewardOwner,
) error {
	key := runtimeRaidRewardCompletionKey{
		RaidKey: delivery.Raid.RaidKey,
		Phase:   delivery.Raid.Lifecycle.Phase,
		Token:   delivery.Token,
	}
	publication := s.runtimeRaidRewardPublicationLocked(key)
	if err := s.sendRuntimeRaidSettlementWalletRefreshes(delivery.WalletRefreshes); err != nil {
		return err
	}
	if delivery.Raid.TailFlag == 1 {
		personalBody, squadBody, err := runtimeRaidSinglePresentationBodies(delivery)
		if err != nil {
			return err
		}
		if err := s.sendRuntimeRaidSettlementInventoryRefreshes(delivery.InventoryRefreshes); err != nil {
			return err
		}
		if len(personalBody) > 0 {
			if err := s.publishRuntimeRaidRewardBodyLocked(
				delivery,
				personalBody,
				publication.RewardListMembers,
				false,
			); err != nil {
				return err
			}
		}
		if len(squadBody) == 0 {
			return s.scheduleRuntimeRaidSingleRewardFinishLocked(delivery, publication, owner)
		}
		return s.scheduleRuntimeRaidSingleSquadRewardLocked(delivery, squadBody, publication, owner)
	}

	rewardBody := append([]byte(nil), delivery.RewardBody...)
	var err error
	if len(rewardBody) == 0 {
		rewardBody, err = raid.BuildRaidSettlementRewardsBody(delivery.Rewards)
		if err != nil {
			return err
		}
	}
	if err := s.publishRuntimeRaidRewardListLocked(delivery, rewardBody, publication); err != nil {
		return err
	}
	// Every squad has now received its own reveal rows. The whole-raid team row
	// follows in the same pass, before the standby transition below: it is the
	// attack-party board, and the standby state is what ends the presentation.
	if teamBody, ok, err := runtimeRaidTeamSquadRewardBody(delivery); err != nil {
		return err
	} else if ok {
		// The 0x0258 body leads with its result type and its row count, so the
		// published board is readable straight off the wire bytes.
		rows := 0
		if len(teamBody) >= 2 {
			rows = int(teamBody[1])
		}
		s.logPacketEvent("game-raid-team-reward-body-published",
			"raid_key", delivery.Raid.RaidKey,
			"raid_phase", delivery.Raid.Lifecycle.Phase,
			"team_rows", rows,
			"body_len", len(teamBody))
		if err := s.publishRuntimeRaidRewardBodyLocked(
			delivery,
			teamBody,
			publication.TeamSquadRewardMembers,
			false,
		); err != nil {
			return err
		}
	}
	// 超时空的收尾和单人攻坚同形：先把 state=6(已结束) 推给每个成员，全推到了
	// 才通知各客户端"本地原生结算已结束"，最后才落账。走不到这一步的话，客户端
	// 会一直停在结算面板上，F12/op72 退不出来。
	if delivery.Raid.RaidMode == runtimeRaidOwnerFiendWar {
		if err := s.publishRuntimeRaidMemberBodyLocked(
			delivery.Raid,
			raid.RaidStateMsgID,
			raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateFinished}),
			publication.SingleFinishedStateMember,
		); err != nil {
			return err
		}
		if !runtimeRaidRewardMembersPublished(delivery.Raid, publication.SingleFinishedStateMember) {
			return nil
		}
		if err := s.markRuntimeFiendWarSettlementFinished(delivery.Raid); err != nil {
			return err
		}
	}
	completion, err := owner.completeRuntimeRaidSettlementRewards(
		delivery.Raid.RaidKey,
		delivery.Raid.Lifecycle.Phase,
		delivery.Token,
		time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	if completion.StandbyTimer == nil {
		return nil
	}
	return s.publishRuntimeRaidStandbyTimerLocked(completion, publication)
}

func (s *Service) runtimeRaidRewardPublicationLocked(key runtimeRaidRewardCompletionKey) *runtimeRaidRewardPublication {
	// 读 nil map 是安全的；创建走有上限的写入，避免这张表随已结算攻坚次数无界增长。
	publication := s.raidRewardPublished[key]
	if publication == nil {
		publication = &runtimeRaidRewardPublication{
			RewardListMembers:         make(map[uint16]bool),
			SingleSquadRewardMembers:  make(map[uint16]bool),
			SingleFinishedStateMember: make(map[uint16]bool),
			StandbyTimerMembers:       make(map[uint16]bool),
			TeamSquadRewardMembers:    make(map[uint16]bool),
		}
		s.storeRaidRewardPublicationLocked(key, publication)
	}
	if publication.RewardListMembers == nil {
		publication.RewardListMembers = make(map[uint16]bool)
	}
	if publication.SingleSquadRewardMembers == nil {
		publication.SingleSquadRewardMembers = make(map[uint16]bool)
	}
	if publication.SingleFinishedStateMember == nil {
		publication.SingleFinishedStateMember = make(map[uint16]bool)
	}
	if publication.StandbyTimerMembers == nil {
		publication.StandbyTimerMembers = make(map[uint16]bool)
	}
	if publication.TeamSquadRewardMembers == nil {
		publication.TeamSquadRewardMembers = make(map[uint16]bool)
	}
	return publication
}

func (s *Service) publishRuntimeRaidRewardListLocked(
	delivery runtimeRaidSettlementRewardDelivery,
	rewardBody []byte,
	publication *runtimeRaidRewardPublication,
) error {
	return s.publishRuntimeRaidRewardBodyLocked(
		delivery,
		rewardBody,
		publication.RewardListMembers,
		true,
	)
}

// runtimeRaidTeamSettlementRewardListBodies splits one team phase settlement
// into the two 0x0258 presentations the current client flips through, in the
// order 86JP sends them: result type 0 first (the gold row), result type 1
// second (the party card row). Each list keeps one row per member, so both flips
// reveal the whole raid party instead of a single folded row per member.
func runtimeRaidTeamSettlementRewardListBodies(
	rewards raid.RaidSettlementRewards,
) ([][]byte, error) {
	first := raid.RaidSettlementRewards{Members: make([]raid.RaidSettlementMemberRewards, 0, len(rewards.Members))}
	second := raid.RaidSettlementRewards{ResultType: 1, Members: make([]raid.RaidSettlementMemberRewards, 0, len(rewards.Members))}
	for _, member := range rewards.Members {
		if len(member.Rewards) == 0 {
			continue
		}
		first.Members = append(first.Members, raid.RaidSettlementMemberRewards{
			MemberKey: member.MemberKey,
			CardIndex: member.CardIndex,
			Rewards:   []raid.RaidSettlementReward{member.Rewards[0]},
		})
		if len(member.Rewards) == 1 {
			continue
		}
		second.Members = append(second.Members, raid.RaidSettlementMemberRewards{
			MemberKey: member.MemberKey,
			CardIndex: member.CardIndex,
			Rewards:   append([]raid.RaidSettlementReward(nil), member.Rewards[1:]...),
		})
	}
	bodies := make([][]byte, 0, 2)
	for _, list := range []raid.RaidSettlementRewards{first, second} {
		if len(list.Members) == 0 {
			continue
		}
		body, err := raid.BuildRaidSettlementRewardsBody(list)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, body)
	}
	return bodies, nil
}

// publishRuntimeRaidRewardListsLocked delivers every revealed list of one phase
// settlement to each member in a single pass, so the publication map that drives
// the phase completion still advances exactly once per member.
func (s *Service) publishRuntimeRaidRewardListsLocked(
	delivery runtimeRaidSettlementRewardDelivery,
	bodies [][]byte,
	published map[uint16]bool,
) error {
	members := append([]runtimeRaidMemberState(nil), delivery.Raid.FrozenMembers...)
	if len(members) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	runtimeRaidSortMembers(members)
	refreshes := make(map[uint16]runtimeRaidRewardInventoryRefresh, len(delivery.InventoryRefreshes))
	for _, refresh := range delivery.InventoryRefreshes {
		refreshes[refresh.CharacterID] = refresh
	}
	for _, member := range members {
		if published[member.CharID] {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online {
			continue
		}
		if refresh, ok := refreshes[member.CharID]; ok {
			if !s.boundGameSessionCharacterCurrent(identity) {
				continue
			}
			if err := s.sendRuntimeRaidSettlementInventoryRefresh(identity.session, refresh); err != nil {
				return err
			}
		}
		if !s.boundGameSessionCharacterCurrent(identity) {
			continue
		}
		for _, body := range bodies {
			if err := s.sendGameUpperRawClassCodec(
				identity.session,
				raid.RaidRewardListMsgID,
				body,
				0,
				true,
			); err != nil {
				return err
			}
		}
		published[member.CharID] = true
	}
	return nil
}

// runtimeRaidRewardSquadBodies splits one settlement's reveal rows by squad, so
// each recipient's 0x0258 carries only its own squad's rows.
//
// The client's resultType==1 board reveals one card per row of the body it
// receives, and 86JP scopes a phase settlement to the member's own PartyIndex on
// both sides: the group list is built from the members of that PartyIndex
// (RaidHandler.Rewards.cs:346-361) and the per-card reveal broadcasts only rows
// whose entry.PartyIndex == member.PartyIndex (:513-528). Our durable receipt is
// deliberately left whole -- validateRuntimeRaidRewardReceipt compares it against
// the phase's whole roster -- so the split happens here, at the send boundary.
//
// Three cases return nil, meaning "send the caller's body unchanged":
//   - the team reward row (resultType 3). 86JP builds that one from every eligible
//     member of the whole raid and broadcasts it unchanged
//     (RaidHandler.Rewards.cs:599-622: one entry per member, no PartyIndex filter),
//     and it is the row the 20-cell attack-party board is drawn from. Splitting it
//     is what removed every other squad's cells from the board. It is recognised on
//     the body itself, because the delivery's own result type describes the reveal
//     list, not the presentation being published.
//   - one distinct squad (the default shape), where the original body already is
//     that squad's body. Reusing it keeps the single-squad presentation
//     byte-for-byte identical to before.
//   - a delivery that carries no structured rows (recovered from a receipt whose
//     body is the only surviving form), where splitting would produce an empty
//     board.
func runtimeRaidRewardSquadBodies(delivery runtimeRaidSettlementRewardDelivery, rewardBody []byte) (map[byte][]byte, error) {
	if len(delivery.Rewards.Members) == 0 {
		return nil, nil
	}
	if delivery.Rewards.ResultType == runtimeRaidTeamRewardResultType ||
		(len(rewardBody) != 0 && rewardBody[0] == runtimeRaidTeamRewardResultType) {
		return nil, nil
	}
	groups := make(map[uint16]byte, len(delivery.Raid.FrozenMembers))
	distinct := make(map[byte]bool, 2)
	for _, member := range delivery.Raid.FrozenMembers {
		// The board is a SQUAD's board: an unassigned member is in no squad, so its
		// row must not travel on squad 1's reveal (runtimeRaidMemberSquad, not the
		// normalized group -- see its comment).
		group := runtimeRaidMemberSquad(member.GroupIndex)
		groups[member.CharID] = group
		distinct[group] = true
	}
	if len(distinct) <= 1 {
		return nil, nil
	}
	lists := make(map[byte]*raid.RaidSettlementRewards, len(distinct))
	for _, row := range delivery.Rewards.Members {
		group, ok := groups[row.MemberKey]
		if !ok {
			// A row outside the frozen roster belongs to no squad's board.
			continue
		}
		list := lists[group]
		if list == nil {
			list = &raid.RaidSettlementRewards{ResultType: delivery.Rewards.ResultType}
			lists[group] = list
		}
		list.Members = append(list.Members, row)
	}
	bodies := make(map[byte][]byte, len(lists))
	for group, list := range lists {
		body, err := raid.BuildRaidSettlementRewardsBody(*list)
		if err != nil {
			return nil, err
		}
		bodies[group] = body
	}
	return bodies, nil
}

func (s *Service) publishRuntimeRaidRewardBodyLocked(
	delivery runtimeRaidSettlementRewardDelivery,
	rewardBody []byte,
	published map[uint16]bool,
	refreshInventory bool,
) error {
	members := append([]runtimeRaidMemberState(nil), delivery.Raid.FrozenMembers...)
	if len(members) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	runtimeRaidSortMembers(members)
	squadBodies, err := runtimeRaidRewardSquadBodies(delivery, rewardBody)
	if err != nil {
		return err
	}
	refreshes := make(map[uint16]runtimeRaidRewardInventoryRefresh)
	if refreshInventory {
		refreshes = make(map[uint16]runtimeRaidRewardInventoryRefresh, len(delivery.InventoryRefreshes))
		for _, refresh := range delivery.InventoryRefreshes {
			refreshes[refresh.CharacterID] = refresh
		}
	}
	for _, member := range members {
		if published[member.CharID] {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online {
			continue
		}
		if refresh, ok := refreshes[member.CharID]; ok {
			if !s.boundGameSessionCharacterCurrent(identity) {
				continue
			}
			if err := s.sendRuntimeRaidSettlementInventoryRefresh(identity.session, refresh); err != nil {
				return err
			}
		}
		if !s.boundGameSessionCharacterCurrent(identity) {
			continue
		}
		body := rewardBody
		if squadBody, ok := squadBodies[runtimeRaidMemberSquad(member.GroupIndex)]; ok {
			body = squadBody
		}
		if err := s.sendGameUpperRawClassCodec(
			identity.session,
			raid.RaidRewardListMsgID,
			body,
			0,
			true,
		); err != nil {
			return err
		}
		published[member.CharID] = true
	}
	return nil
}

// runtimeRaidSinglePresentationBodies prefers the source-pool bodies frozen by
// the reward plan. A party-card booster may contribute several rows to the
// personal flip, and a squad-item booster may contribute several rows to the
// team flip, so the single-mode flow must not infer meaning from row position.
// Older receipts have neither body and retain the historical exactly-two-row
// compatibility path below.
func runtimeRaidSinglePresentationBodies(
	delivery runtimeRaidSettlementRewardDelivery,
) ([]byte, []byte, error) {
	if len(delivery.PersonalRewardBody) > 0 || len(delivery.TeamRewardBody) > 0 {
		return append([]byte(nil), delivery.PersonalRewardBody...),
			append([]byte(nil), delivery.TeamRewardBody...),
			nil
	}
	return runtimeRaidSingleSettlementRewardBodies(delivery.Rewards)
}

func runtimeRaidSingleSettlementRewardBodies(
	rewards raid.RaidSettlementRewards,
) ([]byte, []byte, error) {
	personal := raid.RaidSettlementRewards{ResultType: 1, Members: make([]raid.RaidSettlementMemberRewards, 0, len(rewards.Members))}
	squad := raid.RaidSettlementRewards{ResultType: 3, Members: make([]raid.RaidSettlementMemberRewards, 0, len(rewards.Members))}
	for _, member := range rewards.Members {
		if len(member.Rewards) != 2 {
			return nil, nil, fmt.Errorf("single Anton reward member %d has %d rows, want personal+squad", member.MemberKey, len(member.Rewards))
		}
		personal.Members = append(personal.Members, raid.RaidSettlementMemberRewards{
			MemberKey: member.MemberKey,
			CardIndex: member.CardIndex,
			Rewards:   []raid.RaidSettlementReward{member.Rewards[0]},
		})
		// The squad row's ordinal is the other value the client's resultType==3
		// branch recognises (see currentRaidRewardWireCardIndex): ordinal 1 falls
		// through to the "因未满足条件， 无法获得奖励。" popup, so the independent
		// squad presentation uses 2.
		squad.Members = append(squad.Members, raid.RaidSettlementMemberRewards{
			MemberKey: member.MemberKey,
			CardIndex: 2,
			Rewards:   []raid.RaidSettlementReward{member.Rewards[1]},
		})
	}
	personalBody, err := raid.BuildRaidSettlementRewardsBody(personal)
	if err != nil {
		return nil, nil, err
	}
	squadBody, err := raid.BuildRaidSettlementRewardsBody(squad)
	if err != nil {
		return nil, nil, err
	}
	return personalBody, squadBody, nil
}

func (s *Service) scheduleRuntimeRaidSingleSquadRewardLocked(
	delivery runtimeRaidSettlementRewardDelivery,
	squadBody []byte,
	publication *runtimeRaidRewardPublication,
	owner runtimeRaidSettlementRewardOwner,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	name := fmt.Sprintf("raid-single-squad-reward:%d:%d", delivery.Raid.RaidKey, delivery.Token)
	return queue.ScheduleAfter(name, runtimeRaidSingleSquadRewardDelay, func(_ time.Time) {
		s.raidRewardFlowMu.Lock()
		defer s.raidRewardFlowMu.Unlock()
		if err := s.publishRuntimeRaidRewardBodyLocked(
			delivery,
			squadBody,
			publication.SingleSquadRewardMembers,
			false,
		); err != nil {
			s.logPacketEvent("game-raid-single-squad-reward-publish-failed", "raid_key", delivery.Raid.RaidKey, "error", err)
			return
		}
		if !runtimeRaidRewardMembersPublished(delivery.Raid, publication.SingleSquadRewardMembers) {
			return
		}
		if err := s.scheduleRuntimeRaidSingleRewardFinishLocked(delivery, publication, owner); err != nil {
			s.logPacketEvent("game-raid-single-reward-finish-schedule-failed", "raid_key", delivery.Raid.RaidKey, "error", err)
		}
	})
}

func (s *Service) scheduleRuntimeRaidSingleRewardFinishLocked(
	delivery runtimeRaidSettlementRewardDelivery,
	publication *runtimeRaidRewardPublication,
	owner runtimeRaidSettlementRewardOwner,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	name := fmt.Sprintf("raid-single-reward-finish:%d:%d", delivery.Raid.RaidKey, delivery.Token)
	return queue.ScheduleAfter(name, runtimeRaidSingleRewardFinishDelay, func(_ time.Time) {
		s.raidRewardFlowMu.Lock()
		defer s.raidRewardFlowMu.Unlock()
		wirePhase, err := runtimeRaidSettlementWirePhase(delivery.Raid.Lifecycle.Phase)
		if err != nil {
			s.logPacketEvent("game-raid-single-reward-finish-failed", "raid_key", delivery.Raid.RaidKey, "error", err)
			return
		}
		finishedBody := raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateFinished, Argument: int8(wirePhase)})
		if err := s.publishRuntimeRaidMemberBodyLocked(
			delivery.Raid,
			raid.RaidStateMsgID,
			finishedBody,
			publication.SingleFinishedStateMember,
		); err != nil {
			s.logPacketEvent("game-raid-single-reward-finish-publish-failed", "raid_key", delivery.Raid.RaidKey, "error", err)
			return
		}
		if !runtimeRaidRewardMembersPublished(delivery.Raid, publication.SingleFinishedStateMember) {
			return
		}
		if err := s.markRuntimeRaidNativeSettlementFinished(delivery.Raid); err != nil {
			s.logPacketEvent("game-raid-single-native-settlement-finish-failed", "raid_key", delivery.Raid.RaidKey, "error", err)
			return
		}
		if _, err := owner.completeRuntimeRaidSettlementRewards(
			delivery.Raid.RaidKey,
			delivery.Raid.Lifecycle.Phase,
			delivery.Token,
			time.Now().UTC(),
		); err != nil {
			s.logPacketEvent("game-raid-single-reward-completion-failed", "raid_key", delivery.Raid.RaidKey, "error", err)
		}
	})
}

// markRuntimeRaidNativeSettlementFinished bridges the final Raid state=6 to
// the dungeon owner's exit gate.  It must run before the global Raid object is
// finalized: after that removal there is intentionally no mutable Raid state
// left from which F12/op72 could prove that the two native reward cards ended.
func (s *Service) markRuntimeRaidNativeSettlementFinished(state runtimeRaidState) error {
	if state.TailFlag != 1 || len(state.FrozenMembers) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	for _, member := range state.FrozenMembers {
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online || !s.boundGameSessionCharacterCurrent(identity) {
			return fmt.Errorf("single Raid member %d is not on the current game session", member.CharID)
		}
		identity.session.dungeon.mu.Lock()
		runtime := identity.session.dungeon.runtime
		valid := runtime != nil && dungeonRuntimeOwnsCharacter(runtime, member.CharID) &&
			runtime.Session != nil &&
			runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunCompleted &&
			runtime.raidDungeon.RaidKey == state.RaidKey &&
			runtime.raidDungeon.Phase == state.Lifecycle.Phase &&
			runtime.raidDungeon.completedNotified &&
			!runtime.raidDungeon.abandonedNotified
		if valid {
			runtime.raidNativeSettlementFinished = true
		}
		identity.session.dungeon.mu.Unlock()
		if !valid {
			return fmt.Errorf("single Raid member %d has no matching completed native dungeon", member.CharID)
		}
		s.logGameEvent(identity.session, "game-raid-single-native-settlement-finished",
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"char_id", member.CharID)
	}
	return nil
}

func (s *Service) publishRuntimeRaidMemberBodyLocked(
	state runtimeRaidState,
	msgID uint16,
	body []byte,
	published map[uint16]bool,
) error {
	members := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	if len(members) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	runtimeRaidSortMembers(members)
	for _, member := range members {
		if published[member.CharID] {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online || !s.boundGameSessionCharacterCurrent(identity) {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(identity.session, msgID, body, 0, true); err != nil {
			return err
		}
		published[member.CharID] = true
	}
	return nil
}

func runtimeRaidRewardMembersPublished(state runtimeRaidState, published map[uint16]bool) bool {
	if len(state.FrozenMembers) == 0 {
		return false
	}
	for _, member := range state.FrozenMembers {
		if !published[member.CharID] {
			return false
		}
	}
	return true
}

func (s *Service) sendRuntimeRaidSettlementInventoryRefresh(
	session *gameSession,
	refresh runtimeRaidRewardInventoryRefresh,
) error {
	if len(refresh.Slots) == 0 {
		return nil
	}
	itemRefreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(refresh.Slots))
	for _, slot := range refresh.Slots {
		itemRefreshes = append(itemRefreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: slot,
		})
	}
	return s.sendSelectedIncrementalItemSlotRefreshes(session, "runtime_anton_raid_reward", itemRefreshes)
}

// runtimeRaidTeamSquadRewardBody builds the whole-raid 0x0258 row of one team
// phase settlement: resultType 3, one row per member, no squad filter.
//
// 86JP raises exactly this after every member has picked a card:
// ShowPhaseOneSquadRewardsAsync rolls the "squad_item" container for every
// eligible member of the whole raid (RaidHandler.Rewards.cs:602) and broadcasts
// the resulting list with RaidRewardList(3u, rewards) to the entire raid (:622),
// where each entry carries its own UserId (:614-621). It is the attack-party
// board -- one cell per member -- and the phase settlement never contained it
// here: the durable delivery keeps both of a member's resolved rows in one
// resultType-1 body, so the board had nothing to reveal once that body became
// per-squad.
//
// The board is raised for every team settlement, whatever the squad layout is.
// 86JP conditions it on nothing: ShowPhaseOneSquadRewardsAsync walks the whole
// eligible roster and broadcasts one list, so a raid that fights in a single
// squad receives it exactly like a five-squad one. Skipping it for the
// single-squad shape (2026-09-17T12:46Z, the phase-two settlement of raid
// 1507329) left the 团队展示 stage of that settlement's card flip with no board to
// draw: both members had merged into the same normalized squad by then, because
// character 22 left the ordinary party and an unassigned member (group 0)
// normalizes onto the default squad 1. The same raid's phase-one settlement
// (12:41Z) and the earlier phase-two settlement (12:14Z, raid 1507350) still had
// two squads and both carried the row.
func runtimeRaidTeamSquadRewardBody(
	delivery runtimeRaidSettlementRewardDelivery,
) ([]byte, bool, error) {
	// New settlements freeze the exact resultType-3 body at reward-plan time.
	// That body may contain several rows for one member when a squad reward
	// definition expands a [booster] into multiple items. Older receipts do not
	// have this field and fall back to the historical last-row projection below.
	if len(delivery.TeamRewardBody) > 0 {
		return append([]byte(nil), delivery.TeamRewardBody...), true, nil
	}
	if len(delivery.Rewards.Members) == 0 {
		return nil, false, nil
	}
	list := raid.RaidSettlementRewards{
		ResultType: runtimeRaidTeamRewardResultType,
		Members:    make([]raid.RaidSettlementMemberRewards, 0, len(delivery.Rewards.Members)),
	}
	for _, row := range delivery.Rewards.Members {
		if len(row.Rewards) == 0 {
			continue
		}
		// The last resolved row of a member is its squad/team item -- the same row
		// the single-mode presentation publishes as resultType 3, with the same
		// accepted ordinal (see runtimeRaidSingleSettlementRewardBodies).
		list.Members = append(list.Members, raid.RaidSettlementMemberRewards{
			MemberKey: row.MemberKey,
			CardIndex: 2,
			Rewards:   []raid.RaidSettlementReward{row.Rewards[len(row.Rewards)-1]},
		})
	}
	if len(list.Members) == 0 {
		return nil, false, nil
	}
	body, err := raid.BuildRaidSettlementRewardsBody(list)
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

func (s *Service) publishRuntimeRaidStandbyTimerLocked(
	completion runtimeRaidSettlementRewardCompletion,
	publication *runtimeRaidRewardPublication,
) error {
	members := append([]runtimeRaidMemberState(nil), completion.Raid.FrozenMembers...)
	if len(members) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	runtimeRaidSortMembers(members)
	body := raid.BuildRaidTimerBody(*completion.StandbyTimer)
	// The between-phase break is drawn from a different client store than a gate
	// timer: the raid object's state (which 0x024B carries) selects the timer type,
	// and while it reads 5 -- the phase-settlement/standby value -- the client asks
	// 0x0255 for timerType 1 (0x010D1494 cmp dword ptr [raidObj+0x28],5 / je ->
	// push 1 / call 0x17BA660). Sending only 0x0254 left the 600-second break with
	// no visible countdown, because that packet is keyed by map id and no gate
	// owns the break.
	remainBody := raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{
		TimerType: 1,
		Seconds:   runtimeRaidStandbyRemainingSeconds(*completion.StandbyTimer, s.gameplayNow()),
	})
	for _, member := range members {
		if publication.StandbyTimerMembers[member.CharID] {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online || !s.boundGameSessionCharacterCurrent(identity) {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(identity.session, raid.RaidTimerMsgID, body, 0, true); err != nil {
			return err
		}
		if err := s.sendGameUpperRawClassCodec(identity.session, raid.RaidRemainTimeMsgID, remainBody, 0, true); err != nil {
			return err
		}
		publication.StandbyTimerMembers[member.CharID] = true
	}
	return nil
}

// runtimeRaidStandbyRemainingSeconds is the seconds-left value 0x0255 carries.
// The client stores `end = now + seconds*1000` when it receives the packet, so a
// replayed or late send must carry what is left, never the whole break.
func runtimeRaidStandbyRemainingSeconds(timer raid.RaidTimer, now time.Time) uint32 {
	if timer.Value1 == 0 {
		return 0
	}
	deadline := time.Unix(int64(timer.Value1), 0).UTC()
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return runtimeRaidDurationSeconds(remaining)
}

func (s *Service) handleRuntimeRaidDoBehavior(session *gameSession, body []byte) error {
	request, err := raid.DecodeRaidDoBehaviorRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-behavior-body-invalid",
			"body_len", len(body),
			"error", err)
		return nil
	}
	s.logGameEvent(session, "game-raid-behavior-received",
		"value0", request.Value0,
		"value1", request.Value1,
		"ack", "none_current_exe")
	return nil
}

// handleRuntimeRaidMessage answers C2S 663, the client's dedicated raid message
// request, by raising the raid banner for every online member.
//
// The banner surface is opcode 119 (sub_1D88DE0), the twin of op12: same body
// layout, but the first byte is a flag and only flag zero formats string 640
// "           - 公告 - \n\n%s" and shows it. 0x24C, which this used to answer with,
// renders somewhere else entirely and raised nothing.
//
// The request only carries a mode and text, so the server supplies the two fields
// the notification needs: the speaker as actorKey and zero as the channel.
func (s *Service) handleRuntimeRaidMessage(session *gameSession, body []byte) error {
	request, err := raid.DecodeRaidMessageRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-message-body-invalid",
			"body_len", len(body),
			"error", err)
		return nil
	}
	sourceID := selectedCharacterID(session)
	state, inRaid := s.runtimeRaidForMember(sourceID)
	if !inRaid {
		s.logGameEvent(session, "game-raid-message-rejected",
			"mode", request.Mode,
			"message_len", len(request.MessageBytes),
			"tail", request.Tail,
			"char_id", sourceID,
			"reason", "sender_is_not_a_raid_member")
		return nil
	}
	// Only the raid leader (团长) raises the banner; a squad leader or an ordinary
	// member does not.
	if state.LeaderID != sourceID {
		s.logGameEvent(session, "game-raid-message-rejected",
			"raid_key", state.RaidKey,
			"mode", request.Mode,
			"message_len", len(request.MessageBytes),
			"char_id", sourceID,
			"leader_id", state.LeaderID,
			"reason", "sender_is_not_the_raid_leader")
		return nil
	}
	// The banner is a chat MODE (56 in this client), so the 663 request raises the
	// same notification the leader's own raid speech does.
	noticeBody := buildCurrentRaidAnnouncementNotification(sourceID, request.MessageBytes)
	recipients := 0
	for _, member := range state.Members {
		memberSession, online := s.onlineGameSession(member.CharID)
		if !online || memberSession == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(memberSession, currentTownChatNotificationMsgID, noticeBody, 0, true); err != nil {
			s.logGameEvent(session, "game-raid-message-send-failed",
				"raid_key", state.RaidKey,
				"member_char_id", member.CharID,
				"error", err)
			continue
		}
		recipients++
	}
	s.logGameEvent(session, "game-raid-message-broadcast",
		"raid_key", state.RaidKey,
		"char_id", sourceID,
		"is_leader", true,
		"mode", request.Mode,
		"message_len", len(request.MessageBytes),
		"tail", request.Tail,
		"member_count", len(state.Members),
		"delivered", recipients,
		"banner_mode", currentRaidAnnouncementMode,
		"source", "op663_raised_as_banner")
	return nil
}

// fanOutRuntimeRaidWireSequence delivers a packet sequence to every member of one
// raid, in roster order. It is the raw fan-out with no rule attached; callers that
// want the raid-state invariant use broadcastRuntimeRaidWireSequence.
func (s *Service) fanOutRuntimeRaidWireSequence(state runtimeRaidState, notifications ...runtimeRaidWireNotification) error {
	members := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	if len(members) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	runtimeRaidSortMembers(members)
	for _, member := range members {
		session, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		for _, notification := range notifications {
			if err := s.sendGameUpperRawClassCodec(
				session,
				notification.msgID,
				notification.body,
				0,
				true,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// broadcastRuntimeRaidWireSequence delivers a packet sequence to every member of
// one raid and then re-publishes the battle-situation board's rows if the sequence
// announced raid state.
//
// The re-publication is not an extra event: the client's 0x024B (raid state)
// handler WIPES its row table by applying an empty vector
// (0x01D504CC -> 0x235E750 default ctor -> 0x01D504F7 apply, see
// tools/_probe_634_report.md section 6c Q1). Without the rows in the same sequence
// a board that is already open when a phase changes stays blank until it is closed
// and reopened. It is inert outside an active phase, because a raid with nobody
// standing in a gate publishes nothing at all
// (see runtimeRaidMonsterStateRows: a group count of 0 would CLEAR the table, so
// "no rows" must mean "send nothing").
// broadcastRuntimeRaidWireSequencePerViewer 把同一串通知发给每一名成员，但允许其中
// 一条按接收者重建。replace 返回同一 msgID 的替代体；返回 false 就沿用共享体，
// 这样「按成员重建失败」会退化成共享视图，而不是把这个成员的整份快照丢掉。
//
// 移植自上游 raid_result_flow.go（bfb2098：路线锁按成员可见）。
func (s *Service) broadcastRuntimeRaidWireSequencePerViewer(
	state runtimeRaidState,
	notifications []runtimeRaidWireNotification,
	replace func(viewer uint16) (runtimeRaidWireNotification, bool),
) error {
	members := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	if len(members) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	runtimeRaidSortMembers(members)
	for _, member := range members {
		session, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		substitute, ok := replace(member.CharID)
		for _, notification := range notifications {
			if ok && notification.msgID == substitute.msgID {
				notification = substitute
			}
			if err := s.sendGameUpperRawClassCodec(
				session,
				notification.msgID,
				notification.body,
				0,
				true,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) broadcastRuntimeRaidWireSequence(state runtimeRaidState, notifications ...runtimeRaidWireNotification) error {
	if err := s.fanOutRuntimeRaidWireSequence(state, notifications...); err != nil {
		return err
	}
	announcesRaidState := false
	for _, notification := range notifications {
		if notification.msgID == raid.RaidStateMsgID {
			announcesRaidState = true
			break
		}
	}
	if !announcesRaidState {
		return nil
	}
	body, ok, err := s.runtimeRaidMonsterStateBody(state)
	if err != nil || !ok {
		return err
	}
	return s.fanOutRuntimeRaidWireSequence(state, runtimeRaidWireNotification{
		msgID: raid.RaidMonsterStateMsgID,
		body:  body,
	})
}
