package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDungeonCardResponseClass byte = 1
	currentDungeonCardAutoFlipDelay      = 4 * time.Second
)

func currentDungeonCardLayoutValues(state ...*dungeonCardState) [dungeonCardWireSlotCount]uint16 {
	values := [dungeonCardWireSlotCount]uint16{0x0001, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff}
	if len(state) == 0 || state[0] == nil {
		return values
	}
	state[0].mu.Lock()
	defer state[0].mu.Unlock()
	if bundle := state[0].plan.Sides[dungeonCardSidePaid]; bundle.Gold > 0 || len(bundle.Items) > 0 {
		values[dungeonCardSlotsPerSide] = 0x0001
	}
	return values
}

func (s *Service) sendCurrentDungeonCardScrollStateLocked(session *gameSession, runtime *runtimeDungeonState, source string) error {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime {
		return nil
	}
	if currentDungeonRuntimeIsPractice(runtime) || currentDungeonTowerRewardMsgID(runtime) != 0 {
		return nil
	}
	if !runtime.settlementClearRewardSent || runtime.settlementPhase < currentDungeonSettlementPhaseResultShown {
		s.logGameEvent(session, "game-dungeon-card-scroll-state-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"source", source,
			"settlement_phase", runtime.settlementPhase.String(),
			"reason", "result_phase_not_committed")
		return nil
	}
	replay := runtime.settlementCardScrollStateSent
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketScoreScrollState),
		buildCurrentDungeonOp69SuccessBody(),
		currentDungeonCardResponseClass,
	); err != nil {
		return err
	}
	runtime.settlementCardScrollStateSent = true
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseCardScrollStarted)
	s.logGameEvent(session, "game-dungeon-card-scroll-state-ack-sent",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"replay", replay,
		"op69_body_len", len(buildCurrentDungeonOp69SuccessBody()),
		"classification", currentDungeonCardResponseClass,
		"next_stage", "await_current_exe_c2s_op70",
		"body_source", "current_exe_op69_proved_reader")
	return nil
}

func (s *Service) sendCurrentDungeonCardLayoutLocked(session *gameSession, runtime *runtimeDungeonState, source string) error {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime {
		return nil
	}
	if currentDungeonRuntimeIsPractice(runtime) || currentDungeonTowerRewardMsgID(runtime) != 0 {
		return nil
	}
	if !runtime.settlementCardScrollStateSent || runtime.settlementPhase < currentDungeonSettlementPhaseCardScrollStarted {
		s.logGameEvent(session, "game-dungeon-card-layout-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"source", source,
			"settlement_phase", runtime.settlementPhase.String(),
			"reason", "op69_scroll_state_not_acknowledged")
		return nil
	}
	if runtime.settlementCardRewardState == nil {
		if err := s.freezeCurrentDungeonCardRewardStateLocked(session, runtime); err != nil {
			return err
		}
	}
	replay := runtime.settlementCardLayoutSent
	body := buildCurrentDungeonOp70EightValueSuccessBody(currentDungeonCardLayoutValues(runtime.settlementCardRewardState))
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketCardSelectRightState),
		body,
		currentDungeonCardResponseClass,
	); err != nil {
		return err
	}
	runtime.settlementCardRightStateSent = true
	runtime.settlementCardLayoutSent = true
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseCardsRevealed)
	if err := s.flushRuntimePartyDungeonCardSelectionsLocked(session, runtime); err != nil {
		return err
	}
	if !replay {
		s.armCurrentDungeonCardAutoFlipLocked(session, runtime)
	}
	s.logGameEvent(session, "game-dungeon-card-layout-sent",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"replay", replay,
		"op70_body_len", len(body),
		"classification", currentDungeonCardResponseClass,
		"next_stage", "await_current_exe_c2s_op71",
		"body_source", "current_exe_op70_proved_reader")
	return nil
}

func (s *Service) handleDungeonScoreScrollState(session *gameSession, body []byte) error {
	// The semantic request is bodyless, but the legacy game transport can pad the
	// body (like op71/op72). A padded body carries no extra meaning, so proceed
	// and only note the padding instead of rejecting the whole request.
	if len(body) != 0 {
		s.logGameEvent(session, "game-dungeon-card-layout-request-padding",
			"body_len", len(body),
			"reason", "current_exe_op69_semantic_request_bodyless_legacy_padding_ignored")
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return s.sendCurrentDungeonCardScrollStateLocked(session, session.dungeon.runtime, "current_exe_op69_request")
}

func (s *Service) handleDungeonCardSelectRightState(session *gameSession, body []byte) error {
	if len(body) != 0 {
		s.logGameEvent(session, "game-dungeon-card-layout-request-padding",
			"body_len", len(body),
			"reason", "current_exe_op70_semantic_request_bodyless_legacy_padding_ignored")
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return s.sendCurrentDungeonCardLayoutLocked(session, session.dungeon.runtime, "current_exe_op70_request")
}

func (s *Service) handleDungeonSelectCard(session *gameSession, body []byte) error {
	request, err := decodeCurrentDungeonOp71Request(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-card-select-blocked",
			"body_len", len(body),
			"reason", "current_exe_op71_request_shape_mismatch",
			"error", err)
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if currentDungeonRuntimeIsPractice(runtime) || currentDungeonTowerRewardMsgID(runtime) != 0 {
		return nil
	}
	if runtime == nil || !runtime.settlementCardLayoutSent ||
		runtime.settlementPhase < currentDungeonSettlementPhaseCardsRevealed {
		s.logGameEvent(session, "game-dungeon-card-select-blocked",
			"char_id", session.selectedCharacterID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"request_value_count", request.ValueCount,
			"reason", "card_layout_not_committed_after_op70")
		return nil
	}
	side, ok := currentDungeonCardRequestSide(request.ValueA)
	if !ok || request.ValueB >= dungeonCardSlotsPerSide {
		s.logGameEvent(session, "game-dungeon-card-select-blocked",
			"char_id", session.selectedCharacterID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"reason", "current_exe_op71_row_or_member_index_invalid")
		return nil
	}
	if request.HasScreenSlot {
		return s.selectCurrentDungeonCardLocked(session, runtime, side, request.ValueB,
			"current_exe_op71_screen_select_request", request.ScreenSlot)
	}
	return s.selectCurrentDungeonCardLocked(
		session,
		runtime,
		side,
		request.ValueB,
		"current_exe_op71_select_request",
	)
}

func currentDungeonSelectedCardSlots(
	runtime *runtimeDungeonState,
) ([dungeonCardWireSlotCount]currentDungeonOp71Slot, int) {
	var slots [dungeonCardWireSlotCount]currentDungeonOp71Slot
	for index := range slots {
		slots[index] = currentDungeonOp71Slot{StateA: 0xff, StateB: 0xff}
	}
	if runtime == nil || runtime.settlementCardRewardState == nil {
		return slots, 0
	}
	runtime.settlementCardRewardState.mu.Lock()
	plan := cloneDungeonCardRewardPlan(runtime.settlementCardRewardState.plan)
	runtime.settlementCardRewardState.mu.Unlock()

	rewardTupleCount := 0
	for side := dungeonCardSide(0); side < dungeonCardSideCount; side++ {
		if !runtime.settlementCardSideSelectionKnown[side] {
			continue
		}
		memberIndex := runtime.settlementCardSideMember[side]
		if memberIndex >= dungeonCardSlotsPerSide {
			continue
		}
		wireIndex := int(memberIndex)
		if runtime.settlementCardSidePositionKnown[side] {
			wireIndex = int(runtime.settlementCardSidePosition[side])
		}
		slot := slots[wireIndex]
		if side == dungeonCardSideFree {
			slot.StateA = memberIndex
		} else {
			slot.StateB = memberIndex
		}
		// sub_1CF52B0 inserts tuples into reward[StateB][3], and skips them
		// when StateB is FF. Free rewards must already be in op35's member
		// table; StateA only announces the selecting member's stable slot.
		if side == dungeonCardSidePaid {
			slot.Rewards = currentDungeonOp71RewardTuplesFromCardBundle(plan.Sides[side])
		}
		rewardTupleCount += len(slot.Rewards)
		slots[wireIndex] = slot
	}
	return slots, rewardTupleCount
}

func currentDungeonCardRequestSide(value byte) (dungeonCardSide, bool) {
	switch value {
	case 0:
		return dungeonCardSideFree, true
	case 1, 2:
		return dungeonCardSidePaid, true
	default:
		return 0, false
	}
}

func currentDungeonOp71RewardTuplesFromCardBundle(bundle dungeonCardRewardBundle) []currentDungeonOp71RewardTuple {
	displayItems := currentDungeonCardDisplayItems(bundle)
	tuples := make([]currentDungeonOp71RewardTuple, 0, 1+len(displayItems))
	if bundle.Gold > 0 {
		tuples = append(tuples, currentDungeonOp71RewardTuple{ValueA: 0, ValueB: uint32(bundle.Gold)})
	}
	for _, item := range displayItems {
		if item.ItemID <= 0 || item.Count <= 0 {
			continue
		}
		count := item.Count
		if count > math.MaxUint32 {
			count = math.MaxUint32
		}
		tuples = append(tuples, currentDungeonOp71RewardTuple{
			ValueA: uint32(item.ItemID),
			ValueB: uint32(count),
		})
	}
	return tuples
}

func (s *Service) handleDungeonEplpCommand(session *gameSession, body []byte) error {
	request, err := decodeCurrentDungeonOp72Request(body)
	if err != nil || session == nil || request.ValueA != 1 || request.ValueB > 2 {
		return s.handleDungeonEplpCommandLocal(session, body, true, false)
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	var owner *runtimePartyDungeonOwner
	if runtime != nil {
		owner = runtime.partyDungeonOwner
	}
	session.dungeon.mu.Unlock()
	if owner == nil {
		return s.handleDungeonEplpCommandLocal(session, body, true, false)
	}
	member, current := owner.member(session.selectedCharacterID, session.characterGeneration)
	if !current {
		return errors.New("party dungeon settlement source is not in the immutable roster")
	}
	if member.slot != owner.leaderSlot {
		s.logGameEvent(session, "game-party-dungeon-settlement-non-leader-consumed",
			"char_id", session.selectedCharacterID,
			"party_id", owner.partyID,
			"option", request.ValueB,
			"reason", "same_version_party_settlement_is_leader_owned")
		return nil
	}

	peers := make([]runtimePartyDungeonMember, 0, len(owner.roster())-1)
	for _, peer := range owner.roster() {
		if peer.userID == session.selectedCharacterID && peer.generation == session.characterGeneration {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(peer.userID)
		if !online || identity.session == nil || identity.generation != peer.generation {
			// The peer left the run -- a dropped socket, a relog or a member that
			// quit. Its settlement cannot be delivered and is not this session's
			// problem: the leader's own result still has to commit. Returning an
			// error here travelled up through handleGameUpper into the connection
			// read loop, which closed the LEADER's socket, so one teammate's
			// disconnect turned into "网络连接中断" for the survivor (live incident
			// 2026-09-16 18:56, error="party dungeon settlement member 2 session
			// generation changed" three seconds after the leader's op35).
			s.logGameEvent(session, "game-party-dungeon-settlement-peer-skipped",
				"char_id", session.selectedCharacterID,
				"peer_char_id", peer.userID,
				"party_id", owner.partyID,
				"reason", "peer_session_generation_changed",
				"online", online)
			continue
		}
		peers = append(peers, peer)
	}

	// Commit the leader's own route while the shared world is serialized, then
	// release the world lock before posting immutable work to peer mailboxes.
	// Synchronously waiting for B while A owns worldMu can deadlock against a
	// simultaneous B command that owns B's mailbox and waits for the same lock.
	//
	// The commit below must therefore NOT take worldMu a second time from this
	// goroutine: sync.Mutex is not reentrant, and the party-dungeon roster
	// detach it reaches (detachRuntimePartyDungeonMemberForTownReturn) used to.
	// Measured live 2026-09-25: the 返回城镇 click ACKed at 08:12:38.570Z and then
	// sent nothing at all, and the goroutine dump at 16:14:30 local (161
	// goroutines) showed "[sync.Mutex.Lock, 2 minutes]" at
	// dungeon_return_town.go:1998 -- reached from this very call through
	// completeCurrentSettlementReturnToTown ->
	// sendCurrentCompletedDungeonReturnToTown -> sendCurrentFullDungeonReturnToTown
	// (:1228). Two minutes: the click and the dump. The dead goroutine kept
	// worldMu forever, so every later party/settlement/consumable command on that
	// squad blocked until the client was restarted. The lock is still taken here
	// and still released before the peer fan-out below; only the second
	// acquisition from this same goroutine is gone.
	owner.worldMu.Lock()
	localErr := s.handleDungeonEplpCommandLocal(session, body, true, true)
	owner.worldMu.Unlock()
	if localErr != nil {
		return localErr
	}

	for _, peer := range peers {
		identity, online := s.onlineGameSessionCharacterSnapshot(peer.userID)
		if !online || identity.session == nil || identity.generation != peer.generation {
			// Same reasoning as the collection above: the peer went away between
			// the leader's commit and this delivery. Skip it; the leader's result
			// stands and the connection stays up.
			s.logGameEvent(session, "game-party-dungeon-settlement-peer-skipped",
				"char_id", session.selectedCharacterID,
				"peer_char_id", peer.userID,
				"party_id", owner.partyID,
				"reason", "peer_session_generation_changed_after_leader_commit",
				"online", online)
			continue
		}
		peerSession := identity.session
		peerBody := append([]byte(nil), body...)
		if err := s.postGameSessionCharacterEvent(
			peerSession,
			"party-dungeon-settlement-exit",
			identity.character,
			identity.generation,
			func() error {
				// This task deliberately does NOT hold owner.worldMu across the exit
				// any more, and the measurement is why.
				//
				// Live 2026-09-29 (raid 1507329, 16 members / 4 squads, own build):
				// the per-peer task measured up to 2903 ms on that session's own loop,
				// and the split was
				//
				//	33 tasks: total 30075 ms = wait 11427 ms (38%) + hold 18649 ms (62%)
				//	worst:    task 2903 ms = wait 1575 ms + hold 1329 ms
				//
				// while the squad's other three members queued behind the same lock.
				// The lock itself is needed by ONE call in that chain -- the shared
				// party-dungeon roster detach (dungeon_return_town.go:1309-1315 selects
				// the locking variant from this exact flag) -- so the task now passes
				// worldLocked=false and lets the chain take the lock around the detach
				// alone. Everything else in here (the op72 ACK, the run retirement, the
				// follower selector sequence) is session-private work that the serialized
				// hold was only delaying.
				started := time.Now()
				peerSession.dungeon.mu.Lock()
				peerRuntime := peerSession.dungeon.runtime
				peerSession.dungeon.mu.Unlock()
				if peerRuntime == nil || peerRuntime.partyDungeonOwner != owner {
					return nil
				}
				var err error
				if request.ValueB == 2 {
					err = s.handleDungeonEplpCommandLocal(peerSession, peerBody, true, false)
				} else {
					err = s.handleDungeonEplpCommandLocal(peerSession, peerBody, false, false)
					if err == nil {
						var retired bool
						retired, err = s.retireCompletedDungeonForTownSelect(peerSession)
						if err == nil && !retired {
							err = errors.New("party dungeon settlement peer runtime did not retire")
						}
						if err == nil {
							// 选择其他地下城 ({1,1}) moves the whole party to the selector page,
							// not just the leader: the member's client mirrors the request and
							// expects the same page its leader is looking at. Stopping at the
							// retire above left the member holding the settlement page while
							// still standing inside the instance, and every later exit was
							// refused -- measured 2026-09-19 in an ordinary party and again in an
							// Anton gate, where the member's own op15 answered
							// raid_member_not_in_private_waiting_instance and 返回城镇 answered
							// "Anton selector origin is not bound", so only a client restart got
							// it out (and until then the leader's next gate was refused with
							// 队员不在附近). Send the member the follower selector sequence the
							// leader's own op15 fan-out uses: fatigue + context, no fabricated
							// op15 ACK, and the shared selector page stays the leader's.
							if _, selectErr := s.prepareRuntimePartyFollowerEnterSelect(
								peerSession,
								session.selectedCharacterID,
								int(owner.partyID),
								"party_dungeon_settlement_peer_follows_other_selector",
							); selectErr != nil {
								err = selectErr
							}
						}
					}
				}
				s.logPacketEvent("game-raid-settlement-exit-segments",
					"conn_id", peerSession.connID,
					"char_id", peerSession.selectedCharacterID,
					"party_id", owner.partyID,
					"task_ms", raidMillis(time.Since(started)),
					"lock_scope", "detach_only_inside_the_chain",
					"reason", "peer_exit_no_longer_holds_the_squad_world_lock_for_its_whole_body")
				if raidKey := s.runtimeRaidKeyForMember(peerSession.selectedCharacterID); raidKey != 0 {
					s.raidCostMember(raidKey, peerSession.selectedCharacterID)
				}
				return err
			},
		); err != nil {
			return fmt.Errorf("post party dungeon settlement member %d: %w", peer.userID, err)
		}
	}
	return nil
}

func (s *Service) handleDungeonEplpCommandLocal(
	session *gameSession,
	body []byte,
	runAction bool,
	// worldLocked reports whether the caller already holds
	// runtime.partyDungeonOwner.worldMu. The settlement commit below reaches
	// detachRuntimePartyDungeonMemberForTownReturn, which mutates the shared
	// party-dungeon roster under that lock; taking it a second time from the
	// same goroutine is a permanent self-deadlock, because sync.Mutex is not
	// reentrant. The two callers that hold it (the leader's own commit and each
	// peer's fan-out delivery) pass true so the detach reuses their hold; every
	// other caller passes false and the detach takes the lock itself.
	worldLocked bool,
) error {
	request, err := decodeCurrentDungeonOp72Request(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-card-exit-blocked",
			"body_len", len(body),
			"reason", "current_exe_op72_request_shape_mismatch",
			"error", err)
		return nil
	}
	if session == nil {
		return nil
	}

	// Current-EXE EPLP writers prove {u8 state, u8 option} semantics, and live
	// plaintext clicks on the patched client pinned the option indices: hover
	// sends {2, index}; clicks send {1, 0} for 再次挑战, {1, 1} for
	// 选择其它地下城, and {1, 2} for 返回城镇 (the sub_32BF720
	// settlement-countdown expiry also sends {1,2}). Only {1,2} owns the
	// completed-dungeon town route. After their exact ACK echo, {1,0} is
	// answered by a server-driven same-dungeon re-entry (the client waits for
	// the entry sequence instead of sending op16) and {1,1} by the current
	// op15/op27 selector push (the client waits instead of sending op15).
	// Ordinary dungeons cannot cross the op69 -> op70 -> op71 card/reward
	// barriers. Single-floor towers use their dedicated result owner and send
	// op72 directly. The throttled {2,x} focus notification is ACK-only and must not consume
	// the one-shot exit gate, or the following real click would lose its ACK.
	isFocusNotification := request.ValueA == 2
	shouldReturnToTown := request.ValueA == 1 && request.ValueB == 2
	shouldRetrySameDungeon := request.ValueA == 1 && request.ValueB == 0
	shouldOpenOtherSelector := request.ValueA == 1 && request.ValueB == 1

	// First breadcrumb of the op72 trail. A failing 返回城镇 leaves a trail of
	// return-outcome rows in runtime/logs/records/dungeon-*.jsonl and the LAST one
	// names the exit; without this entry row a request that never reached the ack
	// would leave nothing at all.
	s.recordCompletedReturnOutcome(session, "op72-enter",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"request_value_a", request.ValueA,
		"request_value_b", request.ValueB,
		"run_action", runAction)

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if pendingCurrentDungeonAltarSettlementCommitRetry(runtime, session.selectedCharacterID) {
		if retryErr := s.startCurrentDungeonAltarSettlementLocked(session, runtime); retryErr != nil {
			s.logGameEvent(session, "game-dungeon-altar-settlement-op72-retry-deferred",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"phase", runtime.altar.settlement.phase,
				"error", retryErr)
		}
	}
	nativeRaidExitReady := completedRuntimeRaidDungeonExitReady(runtime, session.selectedCharacterID)
	// A raid gate owns no ordinary card row at all, so a cleared gate must stay
	// leavable for every member once its result is up -- including a squadmate
	// that only adopted the gate's binding and a session whose raid completion
	// publish failed without being disconnected for it.
	raidGateCardlessExit := false
	if !nativeRaidExitReady && s.currentRaidGateCardlessExitReady(runtime, session.selectedCharacterID) {
		nativeRaidExitReady = true
		raidGateCardlessExit = true
	}
	altarExitReady := completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID)
	// 超时空：攻坚已经打完（罗什已死）之后，"返回城镇"必须放行 —— 不管这一场的结算
	// 是服务端发的还是客户端自己放的（native 路线一个结算包都没有）。真机
	// 2026-10-01 20:35：9999 连按十几次全被 settlement_result_not_ready 挡掉。
	fiendWarExitReady := s.fiendWarCompletedDungeonExitReady(session, runtime)
	if runtime == nil || (!runtime.settlementClearRewardSent && !nativeRaidExitReady && !altarExitReady && !fiendWarExitReady) {
		// 战况表回城 before the result exists is the squad leader's mid-run
		// whole-party exit, not an unfinished settlement. Narrow on purpose: a raid
		// gate only (the 战况表 exists nowhere else) and only before the client's
		// boss-die handshake, so a group standing on a finished boss still goes
		// through the settlement page and keeps its clear. See
		// dungeon_war_table_return.go for the operator's rule and the live log.
		if runtime != nil && shouldReturnToTown && !nativeRaidExitReady && !altarExitReady &&
			!runtime.settlementClearRewardSent && !runtime.bossDieCheckAccepted &&
			s.currentDungeonRuntimeIsRaidGate(runtime) {
			session.dungeon.mu.Unlock()
			if s.leadRuntimePartyHomeFromActiveRun(session, "current_exe_op72_war_table_return_to_town") {
				return nil
			}
			session.dungeon.mu.Lock()
		}
		// This branch is reachable with runtime == nil (see the guard above), so
		// every runtime field is read defensively.
		blockedFields := []any{
			"reason", "settlement_result_not_ready",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"native_raid_exit_ready", nativeRaidExitReady,
			"altar_exit_ready", altarExitReady,
			"runtime_present", runtime != nil,
		}
		if runtime != nil {
			blockedFields = append(blockedFields,
				"dungeon_id", runtime.Dungeon.ID,
				"settlement_clear_reward_sent", runtime.settlementClearRewardSent,
				"boss_die_check_accepted", runtime.bossDieCheckAccepted)
			if runtime.Session != nil {
				blockedFields = append(blockedFields,
					"run_status", string(runtime.Session.Snapshot().Run.Status))
			}
		}
		s.recordCompletedReturnOutcome(session, "blocked-before-ack", blockedFields...)
		s.logGameEvent(session, "game-dungeon-card-exit-blocked",
			"char_id", session.selectedCharacterID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"reason", "settlement_result_not_ready")
		session.dungeon.mu.Unlock()
		return nil
	}
	if isFocusNotification {
		response := buildCurrentDungeonOp72SuccessBody(request.ValueA, request.ValueB)
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketEplpCommand), response, currentDungeonCardResponseClass); err != nil {
			session.dungeon.mu.Unlock()
			return err
		}
		s.logGameEvent(session, "game-dungeon-card-focus-ack-sent",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"classification", currentDungeonCardResponseClass)
		session.dungeon.mu.Unlock()
		return nil
	}
	tournamentExitReady := completedTournamentSettlementExitReady(runtime, session.selectedCharacterID)
	raidExitReady := nativeRaidExitReady
	practiceExitReady := currentDungeonRuntimeIsPractice(runtime) && completedDungeonSettlementResultReady(runtime, session.selectedCharacterID)
	// A tower's clear window is its own notification (op348/op1263) and its
	// exit readiness is owned by that flag, not by the ordinary card chain.
	towerExitReady := completedCurrentSingleFloorTowerSettlementExitReady(runtime, session.selectedCharacterID)
	cardlessExitReady := tournamentExitReady || raidExitReady || practiceExitReady || altarExitReady || towerExitReady || fiendWarExitReady
	if !cardlessExitReady && runtime.settlementCardLayoutSent &&
		!runtime.settlementCardSideSelectionSent[dungeonCardSideFree] {
		if err := s.selectCurrentDungeonCardLocked(
			session,
			runtime,
			dungeonCardSideFree,
			runtimePartyDungeonCardMemberIndex(runtime),
			"current_exe_op72_auto_flip_free_row_before_action",
		); err != nil {
			s.logGameEvent(session, "game-dungeon-card-exit-auto-flip-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"request_value_a", request.ValueA,
				"request_value_b", request.ValueB,
				"error", err)
		}
	}
	if !cardlessExitReady && runtime.settlementCardSelectionSent && !runtime.settlementCardRewardCommitted {
		if err := s.commitCurrentDungeonCardRewardLocked(session, runtime, "current_exe_op72_reward_retry_before_exit"); err != nil {
			s.logGameEvent(session, "game-dungeon-card-exit-reward-retry-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"request_value_a", request.ValueA,
				"request_value_b", request.ValueB,
				"error", err)
		}
	}
	cardlessExitReady = cardlessExitReady || towerExitReady
	if !cardlessExitReady && (!runtime.settlementCardLayoutSent || !runtime.settlementCardSelectionSent ||
		!runtime.settlementCardRewardCommitted ||
		runtime.settlementPhase < currentDungeonSettlementPhaseRewardCommitted) {
		s.recordCompletedReturnOutcome(session, "blocked-before-ack",
			"reason", "op72_cannot_cross_unfinished_card_or_reward_phase",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"layout_sent", runtime.settlementCardLayoutSent,
			"selection_sent", runtime.settlementCardSelectionSent,
			"reward_committed", runtime.settlementCardRewardCommitted,
			"settlement_phase", runtime.settlementPhase.String())
		s.logGameEvent(session, "game-dungeon-card-exit-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"layout_sent", runtime.settlementCardLayoutSent,
			"selection_sent", runtime.settlementCardSelectionSent,
			"reward_committed", runtime.settlementCardRewardCommitted,
			"settlement_phase", runtime.settlementPhase.String(),
			"reason", "op72_cannot_cross_unfinished_card_or_reward_phase")
		session.dungeon.mu.Unlock()
		return nil
	}
	if tournamentExitReady {
		s.logGameEvent(session, "game-tournament-settlement-exit-ready",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"ordinary_card_flow_skipped", true,
			"reason", "current_exe_tournament_reward_ledger_already_committed")
	}
	if raidExitReady {
		s.logGameEvent(session, "game-raid-dungeon-settlement-exit-ready",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"raid_key", runtime.raidDungeon.RaidKey,
			"raid_phase", runtime.raidDungeon.Phase,
			"raid_map_id", runtime.raidDungeon.MapID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"ordinary_card_flow_skipped", true,
			"binding_completed_notified", runtime.raidDungeon.completedNotified,
			"cardless_gate_fallback", raidGateCardlessExit,
			"reason", "completed_raid_objective_returns_to_private_waiting_selector")
	}
	if altarExitReady {
		s.logGameEvent(session, "game-dungeon-altar-settlement-exit-ready",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"ordinary_card_flow_skipped", true,
			"unregistered_op260_skipped", true,
			"reason", "dedicated_op208_op209_reward_projection_committed")
	}
	if towerExitReady {
		s.logGameEvent(session, "game-dungeon-single-floor-tower-settlement-exit-ready",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"ordinary_card_flow_skipped", true,
			"reason", "dedicated_tower_reward_floor_and_result_committed")
	}

	// A leader's 返回城镇 on a completed party settlement is a WHOLE-PARTY exit:
	// the fan-out in handleDungeonEplpCommand drives every peer through this very
	// same return, so nobody is leaving alone and the squad has to survive the
	// trip. Without this mark the completed route classified the click as a solo
	// return ("只要是自己单独回城了 视作直接退出队伍") and detached the member --
	// handing leadership to a survivor while the fan-out was still walking the
	// others home -- so the roster was reduced once per member and the party
	// dissolved. Reported live 2026-09-25 as "回城后队伍自动解散了 ... 有点像是
	// 各自回城", the first time this code ran at all: every earlier attempt
	// deadlocked at detachRuntimePartyDungeonMemberForTownReturn.
	//
	// runtime.townReturnKeepParty is the existing whole-party marker that the
	// raid phase return and the 战况表 leader-led return already use
	// ("a whole-party exit must keep the squad together"). Reusing it keeps one
	// exit class instead of adding another.
	if shouldReturnToTown && runAction && runtime.partyDungeonOwner != nil {
		runtime.townReturnKeepParty = true
	}

	runtimeForReturn := runtime
	if runtime.settlementCardExitAckSent {
		s.logGameEvent(session, "game-dungeon-card-exit-replay",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"reason", "exit_ack_already_sent")
		session.dungeon.mu.Unlock()
		if !runAction {
			return nil
		}
		if shouldReturnToTown {
			return s.completeCurrentSettlementReturnToTown(
				session,
				runtimeForReturn,
				"current_exe_op72_card_exit_replay_return_to_town",
				worldLocked,
			)
		}
		return nil
	}
	response := buildCurrentDungeonOp72SuccessBody(request.ValueA, request.ValueB)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketEplpCommand), response, currentDungeonCardResponseClass); err != nil {
		session.dungeon.mu.Unlock()
		return err
	}
	// A {2,x} focus notification is answered but must not consume the one-shot
	// exit gate; the following real click still needs its own ACK.
	if !isFocusNotification {
		runtime.settlementCardExitAckSent = true
		// The settlement no longer arms an auto-return timer (aligned with the
		// DNF_NEW reference), so an explicit exit click has no timeout to disarm.
		if shouldReturnToTown || shouldRetrySameDungeon || shouldOpenOtherSelector {
			runtime.advanceSettlementPhase(currentDungeonSettlementPhaseEnding)
		}
	}
	s.logGameEvent(session, "game-dungeon-card-exit-ack-sent",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"request_value_a", request.ValueA,
		"request_value_b", request.ValueB,
		"focus_notification", isFocusNotification,
		"response_body_len", len(response),
		"classification", currentDungeonCardResponseClass,
		"body_source", "current_exe_op72_proved_reader")
	session.dungeon.mu.Unlock()
	if !runAction {
		// The ack above has already gone out, so the client is now waiting for the
		// exit it asked for. Nothing else on this path logs, and the packet log is
		// lossy (measured 2026-09-25T07:00Z on conn game-003214: 99 of 855 adjacent
		// pkt_seq pairs discontinuous), so report this through the service logger.
		s.recordCompletedReturnOutcome(session, "ack-action-suppressed",
			"reason", "op72_run_action_false",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"should_return_to_town", shouldReturnToTown,
			"dungeon_id", runtime.Dungeon.ID)
		return nil
	}

	if shouldReturnToTown {
		// Half-split probe: the ack above already went out, so from here the only
		// question left is whether the town route was even attempted. Every exit
		// inside it records its own outcome; this one proves the attempt happened.
		s.recordCompletedReturnOutcome(session, "return-dispatch",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_action", runAction,
			"request_value_a", request.ValueA,
			"request_value_b", request.ValueB,
			"runtime_session_present", runtimeForReturn != nil && runtimeForReturn.Session != nil,
			"runtime_is_current", session.dungeon.runtime == runtimeForReturn,
			"source", "current_exe_op72_card_exit_return_to_town")
		return s.completeCurrentSettlementReturnToTown(
			session,
			runtimeForReturn,
			"current_exe_op72_card_exit_return_to_town",
			worldLocked,
		)
	}
	if shouldRetrySameDungeon {
		return s.retryCurrentCompletedDungeon(session, runtimeForReturn, "current_exe_op72_completed_retry_same_dungeon", worldLocked)
	}
	if shouldOpenOtherSelector {
		return s.sendEnterSelectDungeonState(session, "current_exe_op72_completed_select_other_dungeon", false, true)
	}
	// The ack is already on the wire and none of the three actions applied, so the
	// client is left waiting for an exit that will never come. Record it: this
	// fall-through was silent before.
	s.recordCompletedReturnOutcome(session, "no-action",
		"reason", "op72_acked_but_no_completed_exit_action_matched",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"request_value_a", request.ValueA,
		"request_value_b", request.ValueB,
		"should_return_to_town", shouldReturnToTown,
		"should_retry_same_dungeon", shouldRetrySameDungeon,
		"should_open_other_selector", shouldOpenOtherSelector,
		"run_action", runAction)
	return nil
}

// completeCurrentSettlementReturnToTown is the single implementation of
// "leave the completed settlement screen for town": the persisted town
// transition followed by the completed-dungeon return addressed to op72. The
// client's own 返回城镇 click, its replay, and the settlement timeout all go
// through it, so there is no second town route to keep in sync.
func (s *Service) completeCurrentSettlementReturnToTown(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
	// worldLocked is forwarded from handleDungeonEplpCommandLocal: true only for
	// the op72 leader/peer fan-out paths, which already own
	// runtime.partyDungeonOwner.worldMu. See
	// detachRuntimePartyDungeonMemberForTownReturn for why a second acquisition
	// from the same goroutine must never happen.
	worldLocked bool,
) error {
	if s == nil || session == nil || runtime == nil {
		// Last unprobed exit on the 返回城镇 path: the client has already been ACKed
		// by the time control reaches here, so returning early IS the hang.
		if session != nil {
			s.recordCompletedReturnOutcome(session, "no-town-route-inputs",
				"reason", "session_or_runtime_missing",
				"runtime_present", runtime != nil,
				"source", source)
		}
		return nil
	}
	s.recordCompletedReturnOutcome(session, "town-route-entered",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	transition, transitionErr := s.prepareCurrentDungeonTownTransitionForSession(
		ctx, session, session.selectedCharacterID)
	cancel()
	if transitionErr != nil {
		// The client has already been ACKed for its 返回城镇, so this error is the
		// player-visible hang. Report it on the service logger, not only in the
		// lossy packet log; the four live hangs were unattributable for exactly
		// that reason.
		s.recordCompletedReturnOutcome(session, "no-town-transition",
			"reason", "persisted_town_transition_unavailable",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"town_return_origin_character", runtime.townReturnOrigin.CharacterID,
			"town_return_origin_town", runtime.townReturnOrigin.TownID,
			"town_return_origin_area", runtime.townReturnOrigin.AreaID,
			"town_return_origin_valid", runtime.townReturnOrigin.PositionValid,
			"error", transitionErr)
		s.logGameEvent(session, "game-dungeon-card-exit-return-blocked",
			"char_id", session.selectedCharacterID,
			"source", source,
			"reason", "persisted_town_transition_unavailable",
			"error", transitionErr)
		return nil
	}
	err := s.sendCurrentCompletedDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketEplpCommand),
		source,
		worldLocked,
	)
	if err != nil {
		return err
	}
	return s.backOffCompletedSettlementReturnWhenNothingWasSent(session, runtime, source)
}

// backOffCompletedSettlementReturnWhenNothingWasSent is the safety net for the
// settlement page's 返回城镇.
//
// Every guard inside sendCurrentFullDungeonReturnToTown returns nil WITHOUT
// sending anything, and initial_town_route_packets.go:796-802 advances
// session.initialTownRouteStage to currentInitialTownRouteTransitionSent only
// once the typed op24 has actually crossed the wire. So "runtime.townReturnOp24Sent
// is still false" is exactly "the client received no scene transition", and a
// client in that state sits on the settlement page with 返回城镇 doing nothing --
// while still able to walk around and cast, because the character is still the
// live dungeon actor.
//
// Live: 2026-09-24T17:07:49Z (char 1) and 2026-09-25T05:25:22Z / 05:25:23Z
// (chars 1 and 2). All three logged game-raid-dungeon-settlement-exit-ready and
// game-dungeon-card-exit-ack-sent and then not one further packet until the player
// gave up and closed the client 13-17 minutes later. 96 of the 99 recorded
// 返回城镇 clicks delivered; these three did not.
//
// The fallback is the settlement page's own 选择其它地下城 recipe and adds no new
// route: sendEnterSelectDungeonState already owns it end to end -- it retires the
// completed run into the selector at game_enter_select_dungeon.go:91 and leaves
// the frozen pre-entry origin bound, so 返回城镇 from the page it lands on still
// resolves (town_enter_select.go:256-271). The {1,1} option reaches that same call
// from exactly this state: same op14/op72 ack, same advanceSettlementPhase(Ending).
//
// On every path where the town route did deliver the typed op24,
// runtime.townReturnOp24Sent is already true and this returns immediately, so no
// working return changes.
func (s *Service) backOffCompletedSettlementReturnWhenNothingWasSent(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if s == nil || session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	delivered := runtime.townReturnOp24Sent
	stillCurrent := session.dungeon.runtime == runtime
	session.dungeon.mu.Unlock()
	if delivered || !stillCurrent {
		// The town route crossed the wire, or a newer generation owns the session
		// and this return is stale. Either way, leave it alone -- but say which,
		// because this used to be silent and the two cases need opposite fixes.
		s.recordCompletedReturnOutcome(session, "backstop-skipped",
			"reason", "town_route_delivered_or_generation_replaced",
			"delivered", delivered,
			"still_current", stillCurrent,
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"source", source)
		return nil
	}
	s.recordCompletedReturnOutcome(session, "no-scene-transition",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"fallback", "town_dungeon_selector",
		"reason", "completed_town_route_sent_no_scene_transition")
	s.logGameEvent(session, "game-dungeon-completed-return-fell-back-to-selector",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"reason", "completed_town_route_sent_no_scene_transition")
	return s.sendEnterSelectDungeonState(
		session,
		source+"_fallback_after_empty_town_route",
		false,
		true,
	)
}

// retryCurrentCompletedDungeon answers the settlement dialog's 再次挑战
// request ({state=1, option=0}). Live plaintext evidence shows the client
// waits for a server-driven re-entry after the ACK instead of sending op16,
// so the owner retires the completed run and reuses the standard op16 entry
// flow with the client's own proven request (same dungeon, difficulty, and
// entry options) cloned from the retired runtime.

func (s *Service) armCurrentDungeonCardAutoFlipLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) {
	if s == nil || session == nil || runtime == nil ||
		session.dungeon.runtime != runtime ||
		!runtime.settlementCardLayoutSent ||
		runtime.settlementCardSideSelectionKnown[dungeonCardSideFree] {
		return
	}
	s.cancelCurrentDungeonCardAutoFlipLocked(session, runtime, "replace_auto_flip_timer")
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	runtime.settlementCardAutoFlipGeneration++
	generation := runtime.settlementCardAutoFlipGeneration
	timerName := fmt.Sprintf(
		"dnf-dungeon-card-auto-flip:%s:run:%d",
		session.connID,
		runtime.lifecycleToken,
	)
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(
		session,
		session.selectedCharacterID,
	)
	if err != nil {
		return
	}
	runtime.settlementCardAutoFlipTimerName = timerName
	if err := queue.ScheduleAfter(timerName, currentDungeonCardAutoFlipDelay, func(time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"dungeon-card-auto-flip",
			characterID,
			characterGeneration,
			func() error {
				session.dungeon.mu.Lock()
				defer session.dungeon.mu.Unlock()
				current := session.dungeon.runtime
				if current != runtime ||
					runtime.settlementCardAutoFlipGeneration != generation ||
					!runtime.settlementCardLayoutSent ||
					runtime.settlementCardSideSelectionKnown[dungeonCardSideFree] {
					return nil
				}
				runtime.settlementCardAutoFlipTimerName = ""
				return s.selectCurrentDungeonCardLocked(
					session,
					runtime,
					dungeonCardSideFree,
					runtimePartyDungeonCardMemberIndex(runtime),
					"timequeue_auto_flip_free_row",
				)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logPacketEvent("game-dungeon-card-auto-flip-submit-failed",
				"conn_id", session.connID,
				"char_id", characterID,
				"error", postErr)
		}
	}); err != nil {
		runtime.settlementCardAutoFlipTimerName = ""
		s.logGameEvent(session, "game-dungeon-card-auto-flip-schedule-failed",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"error", err)
	}
}

func (s *Service) cancelCurrentDungeonCardAutoFlipLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if runtime == nil {
		return
	}
	timerName := runtime.settlementCardAutoFlipTimerName
	runtime.settlementCardAutoFlipTimerName = ""
	runtime.settlementCardAutoFlipGeneration++
	if timerName == "" || s == nil {
		return
	}
	cancelled := false
	if queue := s.ensureGameplayTimeQueue(); queue != nil {
		cancelled = queue.Cancel(timerName)
	}
	s.logGameEvent(session, "game-dungeon-card-auto-flip-cancelled",
		"char_id", runtime.Character.CharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"cancelled", cancelled)
}

// retryCurrentCompletedDungeon answers the settlement dialog's 再次挑战
// request ({state=1, option=0}). Live plaintext evidence shows the client
// waits for a server-driven re-entry after the ACK instead of sending op16,
// so the owner retires the completed run and reuses the standard op16 entry
// flow with the client's own proven request (same dungeon, difficulty, and
// entry options) cloned from the retired runtime.
func (s *Service) retryCurrentCompletedDungeon(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
	// worldLocked is forwarded from handleDungeonEplpCommandLocal; the retry can
	// land on the completed town route below, which detaches the member from the
	// shared roster under worldMu.
	worldLocked bool,
) error {
	if session == nil || session.selectedCharacterID == 0 || runtime == nil || runtime.Session == nil {
		return nil
	}
	characterID := session.selectedCharacterID
	if !s.completedDungeonSettlementExitReady(runtime, characterID) {
		s.logGameEvent(session, "game-dungeon-card-exit-retry-blocked",
			"char_id", characterID,
			"reason", "completed_settlement_exit_not_ready",
			"source", source)
		return nil
	}
	request := normalCurrentDungeonRetryRequest(runtime.Request)
	dungeonID := runtime.Dungeon.ID
	nextStory := s.resolveNextStoryQuestDungeon(session, runtime)
	if nextStory.ChainKnown && !nextStory.HasDungeon {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		transition, transitionErr := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, characterID)
		cancel()
		if transitionErr != nil {
			s.logGameEvent(session, "game-dungeon-card-exit-story-town-blocked",
				"char_id", characterID,
				"dungeon_id", dungeonID,
				"current_quest_id", nextStory.CurrentQuestID,
				"next_quest_id", nextStory.NextQuestID,
				"resolution", nextStory.Reason,
				"source", source,
				"error", transitionErr)
			return nil
		}
		s.logGameEvent(session, "game-dungeon-card-exit-story-town-route",
			"char_id", characterID,
			"dungeon_id", dungeonID,
			"current_quest_id", nextStory.CurrentQuestID,
			"next_quest_id", nextStory.NextQuestID,
			"resolution", nextStory.Reason,
			"source", source,
			"reason", "direct_successor_cannot_enter_a_dungeon_without_normal_acceptance_flow")
		return s.sendCurrentCompletedDungeonReturnToTown(
			session,
			runtime,
			transition,
			uint16(dnfenum.CmdPacketEplpCommand),
			"current_exe_op72_story_successor_requires_town_flow",
			worldLocked,
		)
	}
	preflightRequest := request
	preflightEnterDungeonID := dungeonID
	if nextStory.HasDungeon && nextStory.DungeonID != dungeonID {
		preflightEnterDungeonID = nextStory.DungeonID
	}
	preflightRequest.DungeonID = uint32(preflightEnterDungeonID)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	preflightErr := s.validateCurrentDungeonRetryAdmission(ctx, session, preflightRequest, runtime.MazeIndex)
	cancel()
	// 入场材料不足仍然是"先检测、后退役"——2026-09-14 22:22 的教训（先退役、再在真正
	// 入场时才发现材料不足，玩家被困在结算页，连返回城镇都没反应）继续保留。
	//
	// 检测出不足之后做两件事：
	//   1) 发 op16 失败回执（reason 17）—— 客户端 sub_1CFC410 → sub_1CFA790 的 254 路
	//      switch case 17 弹出官方那条 "因[%s]缺少入场材料，无法进入『%s』。"（现场
	//      2026-09-24 11:2x/13:08 都验证过：这条提示是客户端自己画的，不是我们发的）；
	//   2) 退役已完成的运行 + op5/op27，把玩家放到"选择副本"界面。
	//
	// 唯一的坑：客户端收到"入场失败"之后有一套自己的状态迁移，收尾是它自己发 bodyless
	// op132(返回城镇) —— 现场 2026-09-24 12:44:46 与 13:08:23 都是回执发出后 6~70ms
	// 就收到这条 op132，玩家看到"直接回城"。这不是玩家点的，所以由
	// suppressCurrentDungeonShortageEscape 在 handleDungeonBackToVillage 里拦一次
	// （第一次当反射吞掉并补选择副本界面，第二次才是玩家真的要回城）。
	var preflightShortage currentDungeonEntryItemShortage
	if preflightErr != nil && errors.As(preflightErr, &preflightShortage) {
		s.logGameEvent(session, "game-dungeon-card-exit-retry-rejected",
			"char_id", characterID,
			"dungeon_id", dungeonID,
			"enter_dungeon_id", preflightEnterDungeonID,
			"source", source,
			"reason", "retry_admission_preflight_failed",
			"error", preflightErr,
			"fallback", "op16_failure_notice_then_op5_op27_selector",
			"escape_guard", "suppress_first_client_reflex_op132")
		if noticeErr := s.sendCurrentDungeonEntryItemMissingFailure(session, runtime, preflightErr); noticeErr != nil {
			s.logGameEvent(session, "game-dungeon-card-exit-retry-notice-blocked",
				"char_id", characterID,
				"dungeon_id", dungeonID,
				"reason", "entry_item_shortage_notice_failed",
				"source", source,
				"error", noticeErr)
		}
	}
	retired, err := s.retireCompletedDungeonForTownSelect(session)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-card-exit-retry-blocked",
			"char_id", characterID,
			"dungeon_id", dungeonID,
			"reason", "completed_runtime_retire_failed",
			"source", source,
			"error", err)
		return nil
	}
	if !retired {
		s.logGameEvent(session, "game-dungeon-card-exit-retry-blocked",
			"char_id", characterID,
			"dungeon_id", dungeonID,
			"reason", "completed_runtime_retire_declined",
			"source", source)
		return nil
	}
	if preflightErr != nil {
		s.logGameEvent(session, "game-dungeon-card-exit-retry-rejected",
			"char_id", characterID,
			"dungeon_id", dungeonID,
			"enter_dungeon_id", preflightEnterDungeonID,
			"source", source,
			"reason", "retry_admission_preflight_failed",
			"error", preflightErr,
			"fallback", "op5_op27_selector")
		return s.restoreCurrentDungeonSelectorAfterEntryFailure(
			session,
			"current_exe_op72_retry_admission_rejected",
		)
	}
	// The ordinary-dungeon entry flow freezes the new run's town-return origin
	// from the bound selector origin. Re-bind it from the retirement-restored
	// town snapshot, or ordinary re-entry is rejected with
	// ordinary_dungeon_town_return_origin_unavailable (live proof).
	origin, originBound := bindCurrentTownSelectorOrigin(session)
	if !originBound {
		s.logGameEvent(session, "game-dungeon-card-exit-retry-blocked",
			"char_id", characterID,
			"dungeon_id", dungeonID,
			"reason", "town_selector_origin_unavailable",
			"source", source)
		return nil
	}
	enterDungeonID := dungeonID
	enterReason := "retired_runtime_cloned_op16_request"
	if nextStory.HasDungeon && nextStory.DungeonID != dungeonID {
		// Story context (the dialog's 开始下个任务): the completed maze is
		// quest-connected, so the client's own chain leads to the next quest's
		// dungeon instead of a same-dungeon retry. Difficulty/entry options
		// still come from the finished run.
		enterDungeonID = nextStory.DungeonID
		enterReason = "pvf_quest_chain_next_dungeon"
	}
	request.DungeonID = uint32(enterDungeonID)
	// NOT WIRED, deliberately. Retiring the teammates' completed runs from this
	// request (retireCompletedRunPartyIntoSelctor below) was tried on 2026-09-20 and
	// froze the leader's client on 再次挑战 (live 2026-09-20 01:03, build 9530E59C: the
	// goroutine dump has no server-side lock wait, so the stall is the client waiting
	// for a packet this path never sends). The party-retry defect it was meant to fix
	// -- "party member N selector is not ready: town_scene_player_state_not_finalized"
	// after the leader's retry -- is understood and reproducible; it is NOT fixed here
	// until that freeze is diagnosed. Do not re-enable without a live trace.
	_ = dungeonID
	s.logGameEvent(session, "game-dungeon-card-exit-retry-enter",
		"char_id", characterID,
		"dungeon_id", dungeonID,
		"enter_dungeon_id", enterDungeonID,
		"current_quest_id", nextStory.CurrentQuestID,
		"next_quest_id", nextStory.NextQuestID,
		"story_chain_known", nextStory.ChainKnown,
		"has_next_story", nextStory.HasDungeon,
		"story_resolution", nextStory.Reason,
		"difficulty", request.Difficulty,
		"entry_option", request.EntryOption,
		"selection_mode", request.SelectionMode,
		"runtime_state", request.RuntimeState,
		"runtime_token", request.RuntimeToken,
		"special_mode", request.SpecialMode,
		"town_id", origin.TownID,
		"area_id", origin.AreaID,
		"source", source,
		"body_source", enterReason)
	// Only the leader owns 再次挑战, so this request carries the whole team. Two things
	// the members cannot do for themselves are done here, both packet-free: leave the
	// town anchor the co-presence gate needs, and accept a member that is still on its
	// own completed run's settlement page (see
	// handleDungeonSelectUpperForSettlementRetry). Nothing about the members' runs is
	// retired and no packet is sent to them: the entry request below drives them, the
	// way 86JP's PARTY_LOADING_GATE does.
	if partyState := runtimePartyStateSnapshot(session); partyState.PartyID > 0 &&
		partyState.UserID == session.selectedCharacterID {
		if bound := s.bindSettlementRetryMemberPresence(session, dungeonID, source); bound > 0 {
			s.logGameEvent(session, "game-dungeon-card-exit-retry-party-presence-bound",
				"char_id", characterID,
				"dungeon_id", dungeonID,
				"bound_members", bound,
				"source", source)
		}
	}
	if err := s.handleDungeonSelectUpperForSettlementRetry(session, encodeCurrentDungeonSelectRequest(request)); err != nil {
		return err
	}
	// handleDungeonSelectUpper reports its own admission failures through the op16
	// ack, so the only local signal left is whether a runtime was committed. When
	// the re-entry failed after the retirement (a party member's missing item, a
	// race with another session), the client is on the settlement page with
	// nothing left to replay: hand it the town selector instead of stranding it.
	session.dungeon.mu.Lock()
	committed := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if !committed {
		return s.restoreCurrentDungeonSelectorAfterEntryFailure(
			session,
			"current_exe_op72_retry_entry_not_committed",
		)
	}
	return nil
}

// validateCurrentDungeonRetryAdmission is read-only: the completed runtime is
// retained until admission succeeds, so a rejected retry cannot consume
// fatigue, tickets, quest state, or settlement rewards.
func (s *Service) validateCurrentDungeonRetryAdmission(
	ctx context.Context,
	session *gameSession,
	request dungeoncmd.SelectDungeonRequest,
	mazeIndex int,
) error {
	if err := validateCurrentDungeonEntryRequest(request); err != nil {
		return err
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return err
	}
	dungeon, found := table.FindDungeon(int64(request.DungeonID))
	if !found {
		return fmt.Errorf("%w: dungeon=%d", worldmap.ErrDungeonNotIndexed, request.DungeonID)
	}
	character, err := s.dungeonCharacter(ctx, session.selectedCharacterID, session)
	if err != nil {
		return err
	}
	mode, modeSupported := currentDungeonProtocolModeForRoom(
		request.RuntimeState, request.RuntimeToken, dungeon.ID,
	)
	if !modeSupported {
		return fmt.Errorf("%w: runtime_state=%d runtime_token=%d", errDungeonEntryModeUnsupported, request.RuntimeState, request.RuntimeToken)
	}
	if mode == currentDungeonProtocolModePractice {
		if err := validatePracticeDungeonRoute(dungeon); err != nil {
			return err
		}
	}
	if err := validateDungeonEntry(character, dungeon, session, mode == currentDungeonProtocolModePractice); err != nil {
		return err
	}
	if mode == currentDungeonProtocolModePractice {
		return nil
	}
	return s.validateCurrentDungeonRetryEntryItems(ctx, session, request, dungeon, mazeIndex)
}

// validateCurrentDungeonRetryEntryItems checks the op16 admission plan's entry
// items read-only, so a 再次挑战 the player cannot pay for is rejected while the
// completed settlement runtime is still alive.
//
// Live 2026-09-14 22:22 is the cost of not doing this: the retry preflight only
// checked permissions and fatigue, retired the run, and the real consume then
// failed with "dungeon entry item is missing: character=1 item=10158964 need=2
// available=0". The client stayed on the settlement page with no runtime left,
// so its own 返回城镇 (op72 {1,2}) had nothing to replay and the completed run
// could not be left at all.
func (s *Service) validateCurrentDungeonRetryEntryItems(
	ctx context.Context,
	session *gameSession,
	request dungeoncmd.SelectDungeonRequest,
	dungeon worldmap.Dungeon,
	mazeIndex int,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return nil
	}
	plan, err := s.resolveCurrentDungeonEntryAdmissionPlan(&runtimeDungeonState{
		Dungeon:   dungeon,
		Request:   request,
		MazeIndex: mazeIndex,
	})
	if err != nil {
		return err
	}
	if len(plan.FreePasses) == 0 && len(plan.Requirements) == 0 && len(plan.RequirementAlternatives) == 0 {
		return nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		inventory = dnfrepo.InventoryRecord{CharacterID: characterID}
	}
	if _, _, err = selectCurrentDungeonEntryItems(inventory.Slots, plan); err != nil {
		var shortage currentDungeonEntryItemShortage
		if errors.As(err, &shortage) {
			shortage.CharacterID = session.selectedCharacterID
			return shortage
		}
		return err
	}
	return nil
}

// restoreCurrentDungeonSelectorAfterEntryFailure keeps a rejected settlement
// retry on the already-restored town selector. Reapply the durable permission
// rows before binding op27 so the client does not retain the failed dungeon as
// an active selection or lose its repository-backed unlocks.
func (s *Service) restoreCurrentDungeonSelectorAfterEntryFailure(session *gameSession, source string) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if err := s.sendCurrentDungeonPermissionSnapshot(ctx, session, source+"_op5_permissions"); err != nil {
		return err
	}
	return s.sendCurrentTownEnterSelectContext(session, source+"_op27_context")
}

type currentNextStoryQuestResolution struct {
	CurrentQuestID int64
	NextQuestID    int64
	DungeonID      int64
	QuestConnected bool
	ChainKnown     bool
	HasDungeon     bool
	Reason         string
}

// resolveNextStoryQuestDungeon resolves only a direct, currently eligible PVF
// successor. Static successor discovery is retained solely to distinguish a
// story-chain end from an unmet or town-only next task; it never authorizes a
// dungeon entry by itself.
func (s *Service) resolveNextStoryQuestDungeon(session *gameSession, runtime *runtimeDungeonState) currentNextStoryQuestResolution {
	resolution := currentNextStoryQuestResolution{Reason: "ordinary_or_unresolved_dungeon"}
	if s == nil || session == nil || runtime == nil || runtime.MazeIndex < 0 || runtime.MazeIndex >= len(runtime.Dungeon.Mazes) {
		return resolution
	}
	connection := runtime.Dungeon.Mazes[runtime.MazeIndex].QuestConnection
	if len(connection) < 2 || connection[0] != 0 {
		return resolution
	}
	resolution.QuestConnected = true
	resolution.CurrentQuestID = connection[1]
	resolution.Reason = "quest_connected_catalog_unavailable"
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil || catalog == nil {
		return resolution
	}
	staticSuccessors := catalog.Successors(resolution.CurrentQuestID)
	if len(staticSuccessors) == 0 {
		resolution.Reason = "quest_chain_terminal"
		return resolution
	}
	resolution.ChainKnown = true
	resolution.Reason = "successor_character_or_quest_state_unavailable"
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return resolution
	}
	character := runtime.Character
	job, validJob := characterJobByte(character)
	if !validJob || character.Level <= 0 {
		return resolution
	}
	characterID := character.CharacterID
	if characterID == "" {
		characterID = strconv.Itoa(int(session.selectedCharacterID))
	}
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return resolution
	}
	if !found {
		record = dnfrepo.QuestRecord{CharacterID: characterID}
	}
	eligible := catalog.EligibleSuccessors(resolution.CurrentQuestID, dnfquest.CharacterEligibility{
		Level:    character.Level,
		Job:      int(job),
		GrowType: int(numericCharacterStatValue(character, "grow_type")),
	}, record)
	if len(eligible) == 0 {
		resolution.Reason = "direct_successor_not_currently_acceptable"
		return resolution
	}
	resolution.NextQuestID = eligible[0].ID
	resolution.Reason = "eligible_direct_successor_has_no_dungeon"
	table, _, err := s.dungeonWorldMap()
	if err != nil || table == nil {
		return resolution
	}
	for _, successor := range eligible {
		for _, dungeon := range table.Dungeons() {
			for _, maze := range dungeon.Mazes {
				candidate := maze.QuestConnection
				if len(candidate) >= 2 && candidate[0] == 0 && candidate[1] == successor.ID {
					resolution.NextQuestID = successor.ID
					resolution.DungeonID = dungeon.ID
					resolution.HasDungeon = true
					resolution.Reason = "eligible_direct_successor_dungeon"
					return resolution
				}
			}
		}
	}
	return resolution
}
