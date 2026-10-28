package dnfbridge

import (
	"errors"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/raid"
)

var errRuntimeRaidRejoinSessionChanged = errors.New("runtime raid rejoin session character changed")

func normalizeRuntimeRaidRejoinError(err error) error {
	if errors.Is(err, errRuntimeRaidRejoinSessionChanged) {
		return nil
	}
	return err
}

func (s *Service) runtimeRaidRejoinNow() time.Time {
	return runtimeRaidNormalizeTime(s.gameplayNow())
}

// sendRuntimeRaidSceneRejoinOnce restores the server-owned attack-party state
// after the current EXE has finished its character-select handler. The current
// client does not issue C2S RejoinRaid when it reconnects directly into an
// Anton channel, so waiting for that request leaves the native manager empty
// even though the raid and its progress are still alive in the bridge.
//
// This is a projection only: it never admits a member, replaces a raid, resets
// lifecycle progress, charges entry cost, or replays the phase-start title.
func (s *Service) sendRuntimeRaidSceneRejoinOnce(session *gameSession, source string) error {
	if s == nil || session == nil || !isCurrentRaidDirectoryChannel(session) {
		return nil
	}
	identity, ok := s.boundGameSessionCharacterSnapshot(session)
	if !ok {
		return nil
	}
	state, ok := s.runtimeRaidForMember(identity.character)
	if !ok {
		clearRuntimeRaidRejoinProjection(session)
		return nil
	}
	// The character is back on a live session: release any disconnect
	// reservation and show the row as online again before restoring state.
	s.restoreRuntimeRaidPresence(session, source)
	if runtimeRaidRejoinProjectionMatches(session, state.RaidKey, state.Lifecycle.Revision) {
		return nil
	}
	if err := normalizeRuntimeRaidRejoinError(s.sendRuntimeRaidRejoinRecovery(
		identity,
		state,
		s.runtimeRaidRejoinNow(),
	)); err != nil {
		// This is a UI restore, not a session prerequisite: the player is already
		// back in the raid channel, and a stale raid window is recoverable (the
		// client re-queries, or the next roster change refreshes it), while a
		// closed socket is not. Returning the error would travel up through
		// handleGameUpper into the connection read loop, which treats it as fatal
		// and closes the connection -- the client shows "网络连接中断". Report it
		// and carry on.
		s.logGameEvent(session, "game-raid-scene-rejoin-failed",
			"source", source,
			"raid_key", state.RaidKey,
			"char_id", identity.character,
			"lifecycle_status", byte(state.Lifecycle.Status),
			"error", err)
		return nil
	}
	if !s.boundGameSessionCharacterCurrent(identity) {
		return nil
	}
	storeRuntimeRaidRejoinProjection(session, state.RaidKey, state.Lifecycle.Revision)
	s.logGameEvent(session, "game-raid-scene-rejoin-recovery-sent",
		"source", source,
		"raid_key", state.RaidKey,
		"leader_id", state.LeaderID,
		"member_count", len(state.Members),
		"frozen_member_count", len(state.FrozenMembers),
		"tail_flag", state.TailFlag,
		"single_raid_type", state.SingleRaidType,
		"started", state.Started,
		"entry_cost_paid", state.EntryCostPaid,
		"lifecycle_status", byte(state.Lifecycle.Status),
		"lifecycle_phase", state.Lifecycle.Phase,
		"lifecycle_revision", state.Lifecycle.Revision,
		"reason", "current_exe_does_not_emit_rejoin_raid_after_direct_anton_channel_login")
	return nil
}

func (s *Service) sendRuntimeRaidRejoinPacket(
	identity boundGameSessionCharacter,
	msgID uint16,
	body []byte,
	classification byte,
) error {
	if !s.boundGameSessionCharacterCurrent(identity) {
		s.logPacketEvent("game-raid-rejoin-projection-stopped",
			"char_id", identity.character,
			"msg_id", msgID,
			"reason", "session_character_or_generation_changed")
		return errRuntimeRaidRejoinSessionChanged
	}
	return s.sendGameUpperRawClassCodec(identity.session, msgID, body, classification, true)
}

func (s *Service) sendRuntimeRaidRejoinRecovery(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
	now time.Time,
) error {
	if identity.character == 0 || runtimeRaidMemberIndex(state, identity.character) < 0 {
		return errRuntimeRaidNotMember
	}
	// A single Raid never owns an ordinary party, before or after START_RAID.
	// Clear stale projections from older processes before restoring the
	// server-owned Raid object.
	//
	// 超时空同理，但多一个条件：只有人不在副本里时才清 —— 他要是正在区域里打，
	// 那份普通队伍投影正是他此刻战斗用的，清掉会把队友一起打散。
	identity.session.dungeon.mu.Lock()
	inDungeon := identity.session.dungeon.runtime != nil
	identity.session.dungeon.mu.Unlock()
	if state.TailFlag == 1 || (state.RaidMode == runtimeRaidOwnerFiendWar && !inDungeon) {
		if err := s.clearSingleRaidOrdinaryPartyProjection(identity.session, "single_raid_rejoin"); err != nil {
			return err
		}
	}
	if err := s.sendRuntimeRaidRejoinBaseSnapshot(identity, state); err != nil {
		return err
	}

	switch state.Lifecycle.Status {
	case runtimeRaidLifecyclePreparing:
		return s.sendRuntimeRaidRejoinPreparing(identity, state, now)
	case runtimeRaidLifecyclePhaseActive:
		return s.sendRuntimeRaidRejoinPhaseActive(identity, state, now)
	case runtimeRaidLifecycleAwaitingMovie:
		return s.sendRuntimeRaidRejoinAwaitingMovie(identity, state)
	case runtimeRaidLifecycleAwaitingRewards:
		return s.sendRuntimeRaidRejoinAwaitingRewards(identity, state, now)
	case runtimeRaidLifecycleStandby:
		return s.sendRuntimeRaidRejoinStandby(identity, state, now)
	default:
		return nil
	}
}

func (s *Service) sendRuntimeRaidRejoinBaseSnapshot(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
) error {
	objectBody, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(state))
	if err != nil {
		return err
	}
	refreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		return err
	}
	buffBody, err := s.runtimeRaidBuffStatusBody(state)
	if err != nil {
		return err
	}
	notifications := []runtimeRaidWireNotification{
		{msgID: raid.RaidObjectMsgID, body: objectBody},
		{msgID: raid.RaidMemberRefreshMsgID, body: refreshBody},
		{msgID: raid.RaidBuffMsgID, body: buffBody},
		{msgID: raid.RaidWaitingListMsgID, body: runtimeRaidWaitingListBody(state)},
		{msgID: raid.RaidEntryCostMsgID, body: s.runtimeRaidEntryCostSnapshotBody(state)},
	}
	if err := s.sendRuntimeRaidRejoinSequence(identity, notifications...); err != nil {
		return err
	}
	// 超时空不投影普通队伍的"重连快照"：它的小队是由 encounter 路径建的托管队伍，
	// 重连时交给战况表去表达，别用单人那条路再塑一次。
	if !state.Started || state.TailFlag == 1 || state.RaidMode == runtimeRaidOwnerFiendWar {
		return nil
	}
	return s.sendRuntimeRaidRejoinPartySnapshot(identity, state)
}

func (s *Service) sendRuntimeRaidRejoinPartySnapshot(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
) error {
	partyState, ok := runtimeRaidRejoinPartyState(state, identity.character)
	if !ok {
		// An unassigned member is a first-class state, not an error: a joiner
		// lands in the standby group (GroupIndex 0) and stays there until the
		// leader assigns a squad, and runtimeRaidPartyGroups deliberately skips
		// that group. Such a member has no party to restore -- the ordinary town
		// route already published the partyless op9 row (party_id=0) -- so the
		// party step is skipped. Returning errRuntimeRaidNotMember here used to
		// travel all the way up through handleGameUpper into the connection read
		// loop and close the socket, which is what made a rejoining standby
		// member see "网络连接中断" the moment they entered the raid channel.
		s.logGameEvent(identity.session, "game-raid-rejoin-party-skipped",
			"char_id", identity.character,
			"raid_key", state.RaidKey,
			"reason", "member_is_in_the_standby_group")
		return nil
	}
	if !s.boundGameSessionCharacterCurrent(identity) {
		return errRuntimeRaidRejoinSessionChanged
	}
	storeRuntimePartyState(identity.session, partyState)
	// Rebuild the central party graph as well as the per-session client cache.
	// Disconnecting retires the old session generation from RuntimePartyManager;
	// leaving only the cached op13 projection would make later dungeon/leave
	// mutations treat the reconnected raid member as partyless.
	if managed, restored := s.bootstrapManagedRuntimePartyForSession(identity.session); restored {
		partyState = managed.StateFor(identity.character)
	}
	return s.sendRuntimePartySnapshot(identity.session, partyState)
}

func runtimeRaidRejoinPartyState(state runtimeRaidState, characterID uint16) (alignedcmd.PartyState, bool) {
	for _, group := range runtimeRaidPartyGroups(state) {
		for _, member := range group {
			if member.CharID == characterID {
				return runtimeRaidGroupPartyState(state, group), true
			}
		}
	}
	return alignedcmd.PartyState{}, false
}

func (s *Service) sendRuntimeRaidRejoinPreparing(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
	now time.Time,
) error {
	if state.Lifecycle.ReadyDeadline.IsZero() {
		return errRuntimeRaidLifecycleState
	}
	seconds := runtimeRaidRejoinRemainingSeconds(state.Lifecycle.ReadyDeadline, now)
	timer := raid.RaidTimer{
		TimerType: 0,
		Value0:    runtimeRaidTopTimerKey,
		Value1:    uint32(state.Lifecycle.ReadyDeadline.Unix()),
		Value2:    runtimeRaidDurationSeconds(state.Lifecycle.ReadyDeadline.Sub(state.Lifecycle.StartedAt)),
	}
	if err := s.sendRuntimeRaidRejoinSequence(identity,
		runtimeRaidWireNotification{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(timer)},
		runtimeRaidWireNotification{
			msgID: raid.RaidRemainTimeMsgID,
			body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: seconds}),
		},
	); err != nil {
		return err
	}
	return s.scheduleRuntimeRaidReady(state)
}

func (s *Service) sendRuntimeRaidRejoinPhaseActive(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
	now time.Time,
) error {
	// 超时空重连恢复就是"按当前战场状态重发一遍战况表"；0x0247/0x0249 那一套是
	// 安徒恩/卢克的格式，这里整条绕开。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return s.sendRuntimeFiendWarBoard(identity.session, state, false, now)
	}
	symbolBody, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(state)
	if err != nil {
		return err
	}
	seconds := runtimeRaidDurationSeconds(runtimeRaidPhaseSnapshotRemaining(state, now))
	phaseDuration := state.Lifecycle.PhaseDeadline.Sub(state.Lifecycle.PhaseStartedAt)
	if phaseDuration < 0 {
		phaseDuration = 0
	}
	// Current EXE sub_F176A0 initializes the selector on op587 state=2 and
	// sub_F163C0 reads the dungeon manager during that initialization. Rejoin
	// recovery must therefore preload op583/op585 before materializing the view.
	notifications := []runtimeRaidWireNotification{
		{msgID: raid.RaidSymbolMsgID, body: symbolBody},
		{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody},
		{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidPhaseStartState,
				Argument: int8(state.Lifecycle.Phase - 1),
			}),
		},
	}
	notifications = append(notifications,
		runtimeRaidWireNotification{
			msgID: raid.RaidTimerMsgID,
			body: raid.BuildRaidTimerBody(raid.RaidTimer{
				TimerType: 0,
				Value0:    runtimeRaidTopTimerKey,
				Value1:    uint32(state.Lifecycle.PhaseDeadline.Unix()),
				Value2:    runtimeRaidDurationSeconds(phaseDuration),
			}),
		},
		runtimeRaidWireNotification{
			msgID: raid.RaidRemainTimeMsgID,
			body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: seconds}),
		},
	)
	notifications = append(notifications, runtimeRaidMechanismTimerNotifications(state, now)...)
	if err := s.sendRuntimeRaidRejoinSequence(identity, notifications...); err != nil {
		return err
	}
	return s.scheduleRuntimeRaidPhaseExpiry(state)
}

func (s *Service) sendRuntimeRaidRejoinAwaitingMovie(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
) error {
	wirePhase, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase)
	if err != nil {
		return err
	}
	clearDuration := state.Lifecycle.MovieOpenedAt.Sub(state.Lifecycle.PhaseStartedAt)
	if state.Lifecycle.MovieOpenedAt.IsZero() || state.Lifecycle.PhaseStartedAt.IsZero() || clearDuration < 0 {
		clearDuration = 0
	}
	resultBody, err := raid.BuildRaidSettlementResultBody(raid.RaidSettlementResult{
		ResultType:   0,
		PhaseIndex:   wirePhase,
		ClearSeconds: runtimeRaidDurationSeconds(clearDuration),
	})
	if err != nil {
		return err
	}
	return s.sendRuntimeRaidRejoinSequence(identity,
		runtimeRaidWireNotification{msgID: raid.RaidResultMsgID, body: resultBody},
		runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidSettlementStateResult,
				Argument: int8(wirePhase),
			}),
		},
	)
}

func (s *Service) sendRuntimeRaidRejoinAwaitingRewards(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
	now time.Time,
) error {
	wirePhase, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase)
	if err != nil {
		return err
	}
	deadline := state.Lifecycle.RewardOpenedAt.Add(runtimeRaidRewardOpenCountdown)
	if !state.Lifecycle.RewardOpenedAt.IsZero() && now.Before(deadline) {
		seconds := runtimeRaidRejoinRemainingSeconds(deadline, now)
		if err := s.sendRuntimeRaidRejoinSequence(identity,
			runtimeRaidWireNotification{
				msgID: raid.RaidStateMsgID,
				body: raid.BuildRaidStateBody(raid.RaidState{
					State:    runtimeRaidSettlementStateMovieDone,
					Argument: int8(wirePhase),
				}),
			},
			runtimeRaidWireNotification{
				msgID: raid.RaidTimerMsgID,
				body: raid.BuildRaidTimerBody(raid.RaidTimer{
					TimerType: 0,
					Value0:    runtimeRaidTopTimerKey,
					Value1:    uint32(deadline.Unix()),
					Value2:    runtimeRaidDurationSeconds(runtimeRaidRewardOpenCountdown),
				}),
			},
			runtimeRaidWireNotification{
				msgID: raid.RaidRemainTimeMsgID,
				body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: seconds}),
			},
		); err != nil {
			return err
		}
		return s.scheduleRuntimeRaidCardsOpened(state, deadline)
	}
	if err := s.sendRuntimeRaidRejoinPacket(
		identity,
		raid.RaidStateMsgID,
		raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateCardsOpened, Argument: int8(wirePhase)}),
		0,
	); err != nil {
		return err
	}
	return s.sendRuntimeRaidRejoinCachedReward(identity, state)
}

func (s *Service) sendRuntimeRaidRejoinStandby(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
	now time.Time,
) error {
	wirePhase, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase)
	if err != nil {
		return err
	}
	if err := s.sendRuntimeRaidRejoinPacket(
		identity,
		raid.RaidStateMsgID,
		raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateCardsOpened, Argument: int8(wirePhase)}),
		0,
	); err != nil {
		return err
	}
	if err := s.sendRuntimeRaidRejoinCachedReward(identity, state); err != nil {
		return err
	}
	if state.Lifecycle.StandbyDeadline.IsZero() {
		return errRuntimeRaidLifecycleState
	}
	seconds := runtimeRaidRejoinRemainingSeconds(state.Lifecycle.StandbyDeadline, now)
	timer := raid.RaidTimer{
		TimerType: 1,
		Value0:    state.RaidKey,
		Value1:    uint32(state.Lifecycle.StandbyDeadline.Unix()),
		Value2:    seconds,
	}
	if err := s.sendRuntimeRaidRejoinSequence(identity,
		runtimeRaidWireNotification{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(timer)},
		runtimeRaidWireNotification{
			msgID: raid.RaidRemainTimeMsgID,
			body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 1, Seconds: seconds}),
		},
	); err != nil {
		return err
	}
	return s.scheduleRuntimeRaidStandby(state)
}

func (s *Service) sendRuntimeRaidRejoinCachedReward(
	identity boundGameSessionCharacter,
	state runtimeRaidState,
) error {
	cache := state.Lifecycle.RewardDelivery
	if cache == nil {
		return nil
	}
	body := append([]byte(nil), cache.RewardBody...)
	if len(body) == 0 {
		var err error
		body, err = raid.BuildRaidSettlementRewardsBody(cache.Rewards)
		if err != nil {
			return err
		}
	}
	return s.sendRuntimeRaidRejoinPacket(identity, raid.RaidRewardListMsgID, body, 0)
}

func (s *Service) sendRuntimeRaidRejoinSequence(
	identity boundGameSessionCharacter,
	notifications ...runtimeRaidWireNotification,
) error {
	for _, notification := range notifications {
		if err := s.sendRuntimeRaidRejoinPacket(identity, notification.msgID, notification.body, 0); err != nil {
			return err
		}
	}
	return nil
}

func runtimeRaidRejoinRemainingSeconds(deadline time.Time, now time.Time) uint32 {
	now = runtimeRaidNormalizeTime(now)
	if deadline.IsZero() || !deadline.After(now) {
		return 0
	}
	return runtimeRaidDurationSeconds(deadline.Sub(now))
}
