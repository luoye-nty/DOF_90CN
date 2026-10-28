package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDungeonDieCharacterBodySize = 4
	currentDungeonUseCoinBodySize      = 2
	currentDungeonDeathStateMsgID      = uint16(32)
	currentDungeonDeathStateBodySize   = 4
	// The client starts its ten-second revive window when op32 arrives. Keep
	// the server return as a two-second watchdog so a late op41 can be handled.
	currentDungeonDeathReturnDelay = 12 * time.Second
	// Same-version party runtime owns one shared all-dead countdown. The
	// reference server starts it only after every immutable roster slot is
	// dead; a living member therefore keeps an earlier dead member admitted.
	currentPartyDungeonAllDeadReturnDelay = 10 * time.Second
	// Current EXE class-1/op41 success body carries one additional u8 after the
	// success discriminator. 0 is the private-server accepted-revive flag.
	currentDungeonUseCoinSuccessFlag = byte(0)
	// Private-server playability: if the character is dead but has no coin
	// stack, still allow revive so friends are not soft-locked by the death
	// return watchdog.
	currentDungeonUseCoinAllowFreeRevive = true
)

var errGameplayTimeQueueUnavailable = errors.New("gameplay time queue is unavailable")

// handleDungeonDieCharacter accepts only the normal current-EXE writer shape:
// two u16 actor coordinates. The request does not identify the actor; the
// selected runtime owner supplies the real scene object key used by op32.
func (s *Service) handleDungeonDieCharacter(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	var owner *runtimePartyDungeonOwner
	if runtime != nil {
		owner = runtime.partyDungeonOwner
	}
	session.dungeon.mu.Unlock()
	if owner == nil {
		return s.handleDungeonDieCharacterLocal(session, body)
	}

	owner.worldMu.Lock()
	if _, current := owner.member(session.selectedCharacterID, session.characterGeneration); !current {
		owner.worldMu.Unlock()
		return errors.New("party dungeon death source is not in the immutable roster")
	}
	if err := s.handleDungeonDieCharacterLocal(session, body); err != nil {
		owner.worldMu.Unlock()
		return err
	}
	session.dungeon.mu.Lock()
	accepted := session.dungeon.runtime == runtime && runtime.deathReturnWaiting
	session.dungeon.mu.Unlock()
	if !accepted {
		owner.worldMu.Unlock()
		return nil
	}
	changed, allDead := owner.setMemberDead(
		session.selectedCharacterID,
		session.characterGeneration,
		true,
	)
	if !changed {
		owner.worldMu.Unlock()
		return nil
	}
	deathBody := buildCurrentDungeonDeathStateBody(
		currentSceneActorObjectKey(session.selectedCharacterID),
		0,
		0,
	)
	owner.worldMu.Unlock()
	if err := s.publishRuntimePartyDungeonCharacterState(
		owner,
		session.selectedCharacterID,
		session.characterGeneration,
		deathBody,
		"party-dungeon-member-death-op32",
	); err != nil {
		return err
	}
	if allDead {
		return s.armRuntimePartyDungeonAllDeadReturns(owner, session)
	}
	return nil
}

func (s *Service) handleDungeonDieCharacterLocal(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	if len(body) != currentDungeonDieCharacterBodySize {
		s.logGameEvent(session, "game-dungeon-character-death-blocked",
			"body_len", len(body),
			"expected_body_len", currentDungeonDieCharacterBodySize,
			"reason", "current_exe_normal_op40_two_u16_coordinates_boundary_mismatch")
		return nil
	}

	positionX := binary.LittleEndian.Uint16(body[0:2])
	positionY := binary.LittleEndian.Uint16(body[2:4])

	// Do not hold the runtime owner while loading the persisted town target.
	// Revalidate the exact run pointer after the repository read.
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	characterID, readyReason := currentDungeonDeathRuntimeOwner(runtime, session)
	session.dungeon.mu.Unlock()
	if readyReason != "" {
		s.logGameEvent(session, "game-dungeon-character-death-blocked",
			"body_len", len(body),
			"position_x_raw", positionX,
			"position_y_raw", positionY,
			"reason", readyReason)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, characterID)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-character-death-blocked",
			"char_id", characterID,
			"body_len", len(body),
			"reason", "persisted_town_transition_unavailable",
			"error", err)
		return nil
	}

	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime {
		s.logGameEvent(session, "game-dungeon-character-death-blocked",
			"char_id", characterID,
			"body_len", len(body),
			"reason", "dungeon_runtime_changed_during_town_transition_load")
		return nil
	}
	if _, readyReason = currentDungeonDeathRuntimeOwner(runtime, session); readyReason != "" {
		s.logGameEvent(session, "game-dungeon-character-death-blocked",
			"char_id", characterID,
			"body_len", len(body),
			"reason", readyReason)
		return nil
	}

	if err := s.armCurrentDungeonDeathReturnLocked(session, runtime, transition); err != nil {
		s.logGameEvent(session, "game-dungeon-character-death-blocked",
			"char_id", characterID,
			"body_len", len(body),
			"reason", "death_return_timer_schedule_failed",
			"error", err)
		return nil
	}

	actorObjectKey := currentSceneActorObjectKey(characterID)
	deathBody := buildCurrentDungeonDeathStateBody(actorObjectKey, 0, 0)
	// 这一次死亡被服务端接受了（守卫全过、op32 即将发出），所以它是本阶段的真实
	// 一次死亡：计入 0x0259 结算条的 deaths。见 raid_phase_death_count.go。
	s.recordRuntimeRaidPhaseDeath(session, runtime)
	s.logGameEvent(session, "game-dungeon-character-death-op32-send",
		"char_id", characterID,
		"actor_object_key", actorObjectKey,
		"position_x_raw", positionX,
		"position_y_raw", positionY,
		"position_x_signed", int16(positionX),
		"position_y_signed", int16(positionY),
		"request_msg_id", uint16(dnfenum.CmdPacketDieCharacter),
		"response_msg_id", currentDungeonDeathStateMsgID,
		"classification", 0,
		"body_len", len(deathBody),
		"death_type", 0,
		"flag", 0,
		"timer_delay_ms", currentDungeonDeathReturnDelay.Milliseconds(),
		"body_source", "current_exe_sub_1D407C0_u16_u8_u8_reader")
	if err := s.sendGameUpperRawClass(session, currentDungeonDeathStateMsgID, deathBody, 0); err != nil {
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "op32_socket_write_failed")
		return err
	}
	return nil
}

func currentDungeonDeathRuntimeOwner(runtime *runtimeDungeonState, session *gameSession) (uint16, string) {
	if runtime == nil || runtime.Session == nil || session == nil {
		return 0, "active_dungeon_runtime_missing"
	}
	characterID := session.selectedCharacterID
	if characterID == 0 || !dungeonRuntimeOwnsCharacter(runtime, characterID) {
		return 0, "active_dungeon_runtime_character_owner_mismatch"
	}
	snapshot := runtime.Session.Snapshot()
	// A clear does not end the character's death. In the "trade lives with the
	// boss" case the gate completes with the member already down and the drops
	// it rolled are still lying in the room, so the player must be able to spend
	// a revive coin and stand back up. Only a torn-down run loses the
	// death/revive fact.
	//
	// Live 2026-09-23 23:29 (黑雾之源, run_token=5): the death watchdog armed at
	// 23:29:29.001 and the clear committed 0.28s later, which cancelled the whole
	// death state. The client kept its revive prompt and sent op41 at 23:29:36,
	// and because the run was no longer Active this check refused it with
	// dungeon_run_not_active -- the only one of 12 op41 requests that day the
	// server never answered.
	if snapshot.Run.Status != worldmap.DungeonRunActive &&
		snapshot.Run.Status != worldmap.DungeonRunCompleted {
		return 0, "dungeon_run_not_active"
	}
	if runtime.townReturnPending {
		return 0, "town_transition_already_pending"
	}
	return characterID, ""
}

func buildCurrentDungeonDeathStateBody(actorObjectKey uint16, deathType, flag byte) []byte {
	body := make([]byte, currentDungeonDeathStateBodySize)
	binary.LittleEndian.PutUint16(body[0:2], actorObjectKey)
	body[2] = deathType
	body[3] = flag
	return body
}

func buildCurrentDungeonUseCoinSuccessBody(targetObjectKey uint16) []byte {
	body := make([]byte, 3)
	binary.LittleEndian.PutUint16(body[0:2], targetObjectKey)
	body[2] = currentDungeonUseCoinSuccessFlag
	return body
}

func (s *Service) armCurrentDungeonDeathReturnLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
) error {
	return s.armCurrentDungeonDeathReturnAfterLocked(
		session,
		runtime,
		transition,
		currentDungeonDeathReturnDelay,
	)
}

func (s *Service) armCurrentDungeonDeathReturnAfterLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	delay time.Duration,
) error {
	if s == nil || session == nil || runtime == nil || session.dungeon.runtime != runtime || s.gameplayTimers == nil {
		return errGameplayTimeQueueUnavailable
	}
	if runtime.lifecycleToken == 0 {
		return fmt.Errorf("%w: runtime lifecycle token is zero", errGameplayTimeQueueUnavailable)
	}
	if delay <= 0 {
		return fmt.Errorf("%w: death return delay is invalid", errGameplayTimeQueueUnavailable)
	}
	// Every armed watchdog is recorded: this is the only timer that can remove a
	// member from a live dungeon without that member asking, so a report of
	// "突然把我踢出副本" has to be answerable by "who armed it, when, and what
	// happened to it" -- see the fired/suppressed/cancelled rows that follow.
	s.recordRaidEvent(session, "forced_return", map[string]any{
		"dungeon_id":       runtime.Dungeon.ID,
		"run_token":        runtime.lifecycleToken,
		"timer_generation": runtime.deathReturnGeneration,
		"timer_delay_ms":   delay.Milliseconds(),
		"result":           "watchdog_armed",
		"actor_char_id":    runtime.Character.CharacterID,
		"settlement_sent":  runtime.settlementEntrySent,
		"source":           "current_exe_op40_client_death_state",
		"client_visible":   "revive_prompt_with_a_12s_town_return_deadline",
	})

	s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "replace_or_initialize_death_timer")
	clearCurrentDungeonPendingReviveLocked(runtime)
	generation := runtime.deathReturnGeneration
	timerName := fmt.Sprintf("dnf-dungeon-death:%s:run:%d", session.connID, runtime.lifecycleToken)
	runtime.deathReturnWaiting = true
	runtime.deathReturnTimerName = timerName
	runtime.deathReturnDueAt = s.gameplayTimers.Now().Add(delay)
	runtime.deathReturnTransition = cloneCurrentDungeonTownTransition(transition)
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "death_timer_character_identity_changed")
		return err
	}
	dungeonID := runtime.Dungeon.ID
	runToken := runtime.lifecycleToken

	err = s.gameplayTimers.ScheduleAfter(
		timerName,
		delay,
		func(due time.Time) {
			postErr := s.postGameSessionCharacterEvent(
				session,
				"dungeon-death-return-timequeue",
				characterID,
				characterGeneration,
				func() error {
					s.fireCurrentDungeonDeathReturn(session, runtime, generation, due)
					return nil
				},
			)
			if postErr != nil && !isClosedGameSessionEventError(postErr) {
				s.logPacketEvent("game-session-event-submit-failed",
					"conn_id", session.connID,
					"source", "dungeon-death-return-timequeue",
					"char_id", characterID,
					"character_generation", characterGeneration,
					"dungeon_id", dungeonID,
					"run_token", runToken,
					"error", postErr)
			}
		},
	)
	if err != nil {
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "death_timer_schedule_failed")
		return err
	}
	s.logGameEvent(session, "game-dungeon-character-death-timer-scheduled",
		"char_id", runtime.Character.CharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"run_token", runtime.lifecycleToken,
		"timer_generation", generation,
		"timer_name", timerName,
		"due_at_utc", runtime.deathReturnDueAt.UTC().Format(time.RFC3339Nano),
		"delay_ms", delay.Milliseconds(),
		"source", "timequeue_one_shot")
	return nil
}

func (s *Service) fireCurrentDungeonDeathReturn(
	session *gameSession,
	runtime *runtimeDungeonState,
	generation uint64,
	due time.Time,
) {
	if s == nil || session == nil || runtime == nil {
		return
	}
	session.dungeon.mu.Lock()
	if session.dungeon.runtime != runtime || !runtime.deathReturnWaiting ||
		runtime.deathReturnGeneration != generation || runtime.lifecycleToken == 0 {
		s.logGameEvent(session, "game-dungeon-character-death-timer-stale",
			"timer_generation", generation,
			"current_generation", runtime.deathReturnGeneration,
			"run_token", runtime.lifecycleToken,
			"runtime_is_current", session.dungeon.runtime == runtime,
			"waiting", runtime.deathReturnWaiting,
			"reason", "run_identity_or_generation_changed")
		session.dungeon.mu.Unlock()
		return
	}
	if _, reason := currentDungeonDeathRuntimeOwner(runtime, session); reason != "" {
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "timer_fired_after_runtime_state_changed")
		s.logGameEvent(session, "game-dungeon-character-death-timer-stale",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"timer_generation", generation,
			"reason", reason)
		session.dungeon.mu.Unlock()
		return
	}
	if runtime.settlementEntrySent {
		// The clear handshake owns the exit from here on. A member who dies
		// *after* the gate was cleared still needs the revive prompt and the
		// revive coin, but a town return now would pull them out of the card
		// flow and cost them the flip reward. Consume the watchdog and keep the
		// death state, exactly like the party-alive case below.
		runtime.deathReturnTimerName = ""
		runtime.deathReturnDueAt = time.Time{}
		s.logGameEvent(session, "game-dungeon-character-death-timer-suppressed",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"timer_generation", generation,
			"death_state_kept", true,
			"reason", "settlement_flow_owns_exit")
		s.recordRaidEvent(session, "forced_return", map[string]any{
			"dungeon_id":       runtime.Dungeon.ID,
			"run_token":        runtime.lifecycleToken,
			"timer_generation": generation,
			"timer_delay_ms":   currentDungeonDeathReturnDelay.Milliseconds(),
			"result":           "suppressed",
			"actor_char_id":    runtime.Character.CharacterID,
			"death_state_kept": true,
			"source":           "current_exe_op40_death_state_12s_watchdog",
			"reason":           "settlement_flow_owns_exit",
			"client_visible":   "kept_in_dungeon_for_the_card_window",
		})
		session.dungeon.mu.Unlock()
		return
	}
	if owner := runtime.partyDungeonOwner; owner != nil && !owner.allMembersDead() {
		// The individual callback is only a socket watchdog. Party death is a
		// shared-world fact, so keep the revive state and frozen town transition
		// but consume this early timer while any admitted member is alive. If the
		// final member dies, the owner re-arms every dead member at one 10s edge.
		runtime.deathReturnTimerName = ""
		runtime.deathReturnDueAt = time.Time{}
		s.logGameEvent(session, "game-party-dungeon-death-timer-suppressed",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"timer_generation", generation,
			"reason", "party_member_still_alive")
		s.recordRaidEvent(session, "forced_return", map[string]any{
			"dungeon_id":       runtime.Dungeon.ID,
			"run_token":        runtime.lifecycleToken,
			"timer_generation": generation,
			"timer_delay_ms":   currentDungeonDeathReturnDelay.Milliseconds(),
			"result":           "suppressed",
			"actor_char_id":    runtime.Character.CharacterID,
			"party_dead_slots": int(runtime.partyDungeonOwner.deadSlotsSnapshot()),
			"party_size":       len(runtime.partyDungeonOwner.roster()),
			"source":           "current_exe_op40_death_state_12s_watchdog",
			"reason":           "party_member_still_alive",
			"client_visible":   "kept_in_dungeon_with_revive_prompt",
		})
		session.dungeon.mu.Unlock()
		return
	}

	s.logGameEvent(session, "game-dungeon-character-death-timer-fired",
		"char_id", runtime.Character.CharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"run_token", runtime.lifecycleToken,
		"timer_generation", generation,
		"scheduled_due_utc", due.UTC().Format(time.RFC3339Nano),
		"source", "timequeue_one_shot")
	transition := cloneCurrentDungeonTownTransition(runtime.deathReturnTransition)
	// The watchdog below is the one server-side path that removes a player from a
	// live dungeon without them asking, so it must always be attributable: the
	// operator's "有时候会突然把人踢出副本" is either this row or a raid phase
	// force-return, and nothing else on the server returns a living member.
	partySize, partyDeadSlots := 0, 0
	if runtime.partyDungeonOwner != nil {
		partySize = len(runtime.partyDungeonOwner.roster())
		partyDeadSlots = int(runtime.partyDungeonOwner.deadSlotsSnapshot())
	}
	s.recordRaidEvent(session, "forced_return", map[string]any{
		"dungeon_id":       runtime.Dungeon.ID,
		"run_token":        runtime.lifecycleToken,
		"timer_generation": generation,
		"timer_delay_ms":   currentDungeonDeathReturnDelay.Milliseconds(),
		"result":           "town_return_committed",
		"actor_char_id":    runtime.Character.CharacterID,
		"party_size":       partySize,
		"party_dead_slots": partyDeadSlots,
		"settlement_sent":  runtime.settlementEntrySent,
		"source":           "current_exe_op40_death_state_12s_watchdog",
		"reason":           "no_revive_request_inside_the_12s_watchdog",
		"client_visible":   "player_removed_from_the_dungeon_after_the_death_prompt",
	})
	session.dungeon.mu.Unlock()
	if err := s.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketDieCharacter),
		"current_exe_op40_death_state_12s_watchdog",
	); err != nil {
		s.logGameEvent(session, "game-dungeon-character-death-return-failed",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"timer_generation", generation,
			"error", err)
	}
}

func (s *Service) cancelCurrentDungeonDeathReturn(session *gameSession, source string) {
	if session == nil {
		return
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	s.cancelCurrentDungeonDeathReturnLocked(session, session.dungeon.runtime, source)
}

// cancelCurrentDungeonDeathReturnLocked invalidates callbacks before removing
// the named queue item. This also defeats a callback that was already dequeued
// but is waiting for the dungeon owner lock.
func (s *Service) cancelCurrentDungeonDeathReturnLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if runtime == nil {
		return
	}
	wasWaiting := runtime.deathReturnWaiting
	timerName := runtime.deathReturnTimerName
	previousGeneration := runtime.deathReturnGeneration
	runtime.deathReturnGeneration = nextCurrentDungeonDeathGeneration(previousGeneration)
	runtime.deathReturnWaiting = false
	runtime.deathReturnTimerName = ""
	runtime.deathReturnDueAt = time.Time{}
	runtime.deathReturnTransition = currentDungeonTownTransition{}
	cancelled := false
	if timerName != "" && s != nil && s.gameplayTimers != nil {
		cancelled = s.gameplayTimers.Cancel(timerName)
	}
	if wasWaiting || timerName != "" {
		s.logGameEvent(session, "game-dungeon-character-death-timer-cancelled",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"previous_generation", previousGeneration,
			"current_generation", runtime.deathReturnGeneration,
			"timer_name", timerName,
			"queue_item_cancelled", cancelled,
			"source", source)
		s.recordRaidEvent(session, "forced_return", map[string]any{
			"dungeon_id":           runtime.Dungeon.ID,
			"run_token":            runtime.lifecycleToken,
			"timer_generation":     previousGeneration,
			"timer_delay_ms":       currentDungeonDeathReturnDelay.Milliseconds(),
			"result":               "cancelled",
			"actor_char_id":        runtime.Character.CharacterID,
			"queue_item_cancelled": cancelled,
			"source":               source,
			"client_visible":       "kept_in_dungeon",
		})
	}
}

// suspendCurrentDungeonDeathReturnForSettlementLocked keeps the death fact but
// silences the town-return watchdog once the clear handshake owns the exit.
//
// The clear used to cancel the entire death state
// (cancelCurrentDungeonDeathReturnLocked, source "normal_settlement_entry"),
// which is the same defect the room-transition path already fixed below: a
// member who dies as the boss dies clears the gate with the revive prompt still
// on screen, and cancelling here left op41 refused while the character was still
// down.
//
// The settlement's real requirement is not that the death fact disappear but
// that the watchdog stay quiet: while the card flow runs, a town return would
// drag the member out mid-flip and cost them the flip reward. So consume the
// queue item and bump the generation (which is what makes an already-dequeued
// callback stale in fireCurrentDungeonDeathReturn), keep deathReturnWaiting and
// the frozen town transition, and let the revive coin still work.
//
// A death that arrives *after* the clear re-arms normally and is consumed by the
// settlement suppression in fireCurrentDungeonDeathReturn instead.
func (s *Service) suspendCurrentDungeonDeathReturnForSettlementLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if runtime == nil {
		return
	}
	if !runtime.deathReturnWaiting {
		// Nothing to preserve: fall back to the plain cancel so a stray timer
		// name cannot survive into the settlement.
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, source)
		return
	}
	timerName := runtime.deathReturnTimerName
	previousGeneration := runtime.deathReturnGeneration
	runtime.deathReturnGeneration = nextCurrentDungeonDeathGeneration(previousGeneration)
	runtime.deathReturnTimerName = ""
	runtime.deathReturnDueAt = time.Time{}
	cancelled := false
	if timerName != "" && s != nil && s.gameplayTimers != nil {
		cancelled = s.gameplayTimers.Cancel(timerName)
	}
	s.logGameEvent(session, "game-dungeon-character-death-return-suspended",
		"char_id", runtime.Character.CharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"run_token", runtime.lifecycleToken,
		"previous_generation", previousGeneration,
		"current_generation", runtime.deathReturnGeneration,
		"timer_name", timerName,
		"queue_item_cancelled", cancelled,
		"death_state_kept", true,
		"source", source)
}

// keepCurrentDungeonDeathReturnAcrossRoomTransitionLocked preserves the death
// fact of a ghost that walks into the next room. A dead member stays dead for
// its own client across the room load (the revive prompt remains), so
// cancelling here left the run in a state where op41 was refused with
// death_timer_not_waiting while the character was still down. The frozen town
// transition describes the persisted town target rather than the room, so it
// stays valid, and an already-armed watchdog keeps its own schedule.
func (s *Service) keepCurrentDungeonDeathReturnAcrossRoomTransitionLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) {
	if runtime == nil {
		return
	}
	if !runtime.deathReturnWaiting {
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "dungeon_room_transition_committed")
		return
	}
	s.logGameEvent(session, "game-dungeon-character-death-return-preserved",
		"char_id", runtime.Character.CharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"run_token", runtime.lifecycleToken,
		"timer_generation", runtime.deathReturnGeneration,
		"timer_armed", runtime.deathReturnTimerName != "",
		"source", "dungeon_room_transition_committed")
}

func nextCurrentDungeonDeathGeneration(current uint64) uint64 {
	next := current + 1
	if next == 0 {
		next++
	}
	return next
}

// completeCurrentDungeonReviveLocked is the cancellation boundary for a real
// revive transaction. Callers must first commit the authoritative coin
// consumption and deliver both the op32 live-state packet and the op41 ACK;
// a request alone must not cancel the death timer.
func (s *Service) completeCurrentDungeonReviveLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime {
		return
	}
	s.cancelCurrentDungeonDeathReturnLocked(session, runtime, source)
}

func clearCurrentDungeonPendingReviveLocked(runtime *runtimeDungeonState) {
	if runtime == nil {
		return
	}
	runtime.deathReviveCommitted = false
	runtime.deathReviveGeneration = 0
	runtime.deathReviveTargetObjectKey = 0
	runtime.deathReviveConsume = currentDungeonReviveCoinConsumeResult{}
}

type currentDungeonReviveCoinConsumeResult struct {
	Consumed      bool
	FreeRevive    bool
	SlotKey       string
	Slot          int16
	ItemID        int64
	CountAfter    int64
	HasItemUpdate bool
	ItemUpdate    currentItemListEntry
}

// handleDungeonUseCoin accepts current-EXE op41 (body = target actor object
// key u16) while the death-return timer is armed. It consumes one revive coin
// stack (item id 1) when available, then sends the current-EXE revive sequence
// (class0/op32 live state followed by class1/op41 ACK). The death watchdog is
// cancelled only after both packets are written successfully.
func (s *Service) handleDungeonUseCoin(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	var owner *runtimePartyDungeonOwner
	if runtime != nil {
		owner = runtime.partyDungeonOwner
	}
	session.dungeon.mu.Unlock()
	if owner == nil {
		return s.handleDungeonUseCoinLocal(session, body)
	}

	owner.worldMu.Lock()
	if _, current := owner.member(session.selectedCharacterID, session.characterGeneration); !current {
		owner.worldMu.Unlock()
		return errors.New("party dungeon revive source is not in the immutable roster")
	}
	if err := s.handleDungeonUseCoinLocal(session, body); err != nil {
		owner.worldMu.Unlock()
		return err
	}
	session.dungeon.mu.Lock()
	revived := session.dungeon.runtime == runtime && !runtime.deathReturnWaiting
	session.dungeon.mu.Unlock()
	if !revived {
		owner.worldMu.Unlock()
		return nil
	}
	changed, _ := owner.setMemberDead(
		session.selectedCharacterID,
		session.characterGeneration,
		false,
	)
	if !changed {
		owner.worldMu.Unlock()
		return nil
	}
	reviveBody := buildCurrentDungeonDeathStateBody(
		currentSceneActorObjectKey(session.selectedCharacterID),
		1,
		0,
	)
	owner.worldMu.Unlock()
	return s.publishRuntimePartyDungeonCharacterState(
		owner,
		session.selectedCharacterID,
		session.characterGeneration,
		reviveBody,
		"party-dungeon-member-revive-op32",
	)
}

func (s *Service) handleDungeonUseCoinLocal(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	if len(body) != currentDungeonUseCoinBodySize {
		s.logGameEvent(session, "game-dungeon-use-coin-blocked",
			"body_len", len(body),
			"expected_body_len", currentDungeonUseCoinBodySize,
			"reason", "current_exe_normal_op41_single_u16_target_boundary_mismatch")
		return nil
	}
	targetObjectKey := binary.LittleEndian.Uint16(body)

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	characterID := session.selectedCharacterID
	ownerID, readyReason := currentDungeonDeathRuntimeOwner(runtime, session)
	waiting := runtime != nil && runtime.deathReturnWaiting
	// A duplicate op41 that arrives after a committed revive already has its
	// success ACK; answering that one with a failure would turn a working revive
	// into what looks like a rejected one. armCurrentDungeonDeathReturnAfterLocked
	// clears this flag on the next death, so it means "the latest death was
	// already revived" rather than "some old revive happened".
	reviveAlreadyCommitted := runtime != nil && runtime.deathReviveCommitted
	runToken := uint64(0)
	generation := uint64(0)
	dungeonID := int64(0)
	if runtime != nil {
		runToken = runtime.lifecycleToken
		generation = runtime.deathReturnGeneration
		dungeonID = int64(runtime.Dungeon.ID)
	}
	expectedObjectKey := currentSceneActorObjectKey(characterID)
	session.dungeon.mu.Unlock()

	if readyReason != "" || !waiting {
		reason := readyReason
		if reason == "" {
			reason = "death_timer_not_waiting"
		}
		s.logGameEvent(session, "game-dungeon-use-coin-blocked",
			"char_id", characterID,
			"target_object_key", targetObjectKey,
			"expected_object_key", expectedObjectKey,
			"run_token", runToken,
			"death_timer_waiting", waiting,
			"reason", reason)
		if !reviveAlreadyCommitted {
			// Never refuse silently. A dropped op41 left the client's revive
			// confirm sitting on screen with no answer at all -- no op32 revive
			// state, no op41 ACK and not even a failure -- so the player could
			// only click a dead button (live 2026-09-23 23:29).
			_ = s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseCoin), 1)
		}
		return nil
	}
	if targetObjectKey != expectedObjectKey || targetObjectKey != currentSceneActorObjectKey(ownerID) {
		s.logGameEvent(session, "game-dungeon-use-coin-blocked",
			"char_id", characterID,
			"target_object_key", targetObjectKey,
			"expected_object_key", expectedObjectKey,
			"run_token", runToken,
			"reason", "target_object_key_mismatch")
		return nil
	}

	session.dungeon.mu.Lock()
	if session.dungeon.runtime != runtime || runtime == nil || !runtime.deathReturnWaiting ||
		runtime.deathReturnGeneration != generation || runtime.lifecycleToken != runToken {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-use-coin-blocked",
			"char_id", characterID,
			"target_object_key", targetObjectKey,
			"run_token", runToken,
			"timer_generation", generation,
			"reason", "death_timer_state_changed_before_commit")
		return nil
	}
	if _, reason := currentDungeonDeathRuntimeOwner(runtime, session); reason != "" {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-use-coin-blocked",
			"char_id", characterID,
			"target_object_key", targetObjectKey,
			"run_token", runToken,
			"reason", reason)
		return nil
	}

	consume := currentDungeonReviveCoinConsumeResult{}
	responseReplayed := false
	// 卢克的两层复活币上限（[PHASE COIN LIMIT] 全队 / [coin limit] 单次进图）。
	// ⚠️ 只对「队伍卢克」接：真 PVF 实测（2026-09-29, runtime/data/dnf/Script.pvf）
	// 显示安徒恩的 210..229 也声明了 [coin limit] 12，若对安徒恩也生效，会在
	// 那条已验收的复活币路径上多出一层「每次进图 12 次」的封顶。
	phaseCoin := runtimeRaidPhaseCoinStatus{}
	phaseCoinCharged := false
	if s.runtimeRaidLukePhaseCoinApplies(characterID) &&
		!(runtime.deathReviveCommitted && runtime.deathReviveGeneration == generation) {
		// A raid caps coins twice: [PHASE COIN LIMIT] roster-wide and the
		// dungeon's own [coin limit] per entry. Charge before touching the wallet
		// so an exhausted cap costs nothing.
		status, capped, allowed := s.commitRuntimeRaidPhaseCoin(
			characterID,
			int(runtime.Dungeon.ID),
			int(runtime.Dungeon.Metadata.CoinLimit.Value),
		)
		if capped && !allowed {
			session.dungeon.mu.Unlock()
			reason := "raid_phase_coin_limit_reached"
			if status.Refused == "map" {
				reason = "raid_dungeon_coin_limit_reached"
			}
			s.logGameEvent(session, "game-dungeon-use-coin-blocked",
				"char_id", characterID,
				"target_object_key", targetObjectKey,
				"run_token", runToken,
				"raid_key", status.RaidKey,
				"dungeon_id", dungeonID,
				"phase_coins_used", status.Used,
				"phase_coin_limit", status.Limit,
				"dungeon_coins_used", status.MapUsed,
				"dungeon_coin_limit", status.MapLimit,
				"reason", reason)
			_ = s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseCoin), 1)
			// op41 的失败分支（sub_1CF6330 -> 0x1CF6716）只会静默关掉复活窗，说清原因。
			_ = s.sendNotice(session, noticeRequest{
				Surface: noticeSurfaceSystemMagenta,
				Text:    "已超出复活币的使用上限",
				Source:  "raid_coin_limit_reached",
			})
			return nil
		}
		phaseCoin, phaseCoinCharged = status, capped
	}
	// raidCoinSpent is set once the committed revive has been billed to the raid's
	// own gate ledger, and raidCoinKey names that gate. Both stay zero on a replay
	// and on a free revive, so the board is republished exactly once per coin.
	raidCoinSpent := false
	var raidCoinKey runtimeRaidReviveCoinKey
	var raidCoinTotal uint32
	if runtime.deathReviveCommitted && runtime.deathReviveGeneration == generation {
		if runtime.deathReviveTargetObjectKey != targetObjectKey {
			session.dungeon.mu.Unlock()
			s.logGameEvent(session, "game-dungeon-use-coin-blocked",
				"char_id", characterID,
				"target_object_key", targetObjectKey,
				"run_token", runToken,
				"reason", "pending_revive_target_object_key_mismatch")
			return nil
		}
		consume = runtime.deathReviveConsume
		responseReplayed = true
	} else {
		// 超时空：复活币受两重额外约束 —— 地图自身是否禁用复活币，以及该区域的
		// [coin limit]。后者是【整队共享】的额度，所以取 party owner 上的计数，
		// 而不是本会话自己的；一个小队一起打一个区域，配额是一起花的。
		// 安东/卢克不进这个分支。
		if isCurrentFiendWarRaidChannel(session) {
			limit, exists := runtime.Dungeon.Metadata.IntegerValues["coin limit"]
			used := runtime.fiendWarReviveCount
			if runtime.partyDungeonOwner != nil {
				used = runtime.partyDungeonOwner.fiendWarReviveCount
			}
			if !exists || limit < 0 || used >= limit {
				session.dungeon.mu.Unlock()
				s.logGameEvent(session, "game-fiendwar-revive-limit", "used", used, "limit", limit)
				_ = s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseCoin), 1)
				return nil
			}
		}
		consumeResult := currentDungeonReviveCoinConsumeResult{}
		var consumeErr error
		if currentDungeonRuntimeIsPractice(runtime) {
			consumeResult.FreeRevive = true
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			consumeResult, consumeErr = s.consumeCurrentDungeonReviveCoin(ctx, characterID)
			cancel()
		}
		if consumeErr != nil {
			session.dungeon.mu.Unlock()
			if phaseCoinCharged {
				s.refundRuntimeRaidPhaseCoin(phaseCoin)
			}
			s.logGameEvent(session, "game-dungeon-use-coin-blocked",
				"char_id", characterID,
				"target_object_key", targetObjectKey,
				"run_token", runToken,
				"reason", "revive_coin_asset_transaction_failed",
				"error", consumeErr)
			_ = s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseCoin), 1)
			return nil
		}
		if !consumeResult.Consumed && !consumeResult.FreeRevive {
			session.dungeon.mu.Unlock()
			if phaseCoinCharged {
				s.refundRuntimeRaidPhaseCoin(phaseCoin)
			}
			s.logGameEvent(session, "game-dungeon-use-coin-blocked",
				"char_id", characterID,
				"target_object_key", targetObjectKey,
				"run_token", runToken,
				"reason", "revive_coin_insufficient")
			_ = s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseCoin), 1)
			return nil
		}
		consume = consumeResult
		runtime.deathReviveCommitted = true
		runtime.deathReviveGeneration = generation
		runtime.deathReviveTargetObjectKey = targetObjectKey
		runtime.deathReviveConsume = consumeResult
		// 超时空：记账点在"已提交的付费复活"这一支里，与上面那条 replay 分支分开，
		// 所以重复的 op41 不会把额度重复扣掉。队长的 party owner 在就记到它上面
		// (整队共享)，否则记在本会话的副本运行时上。
		if isCurrentFiendWarRaidChannel(session) {
			if runtime.partyDungeonOwner != nil {
				runtime.partyDungeonOwner.fiendWarReviveCount++
			} else {
				runtime.fiendWarReviveCount++
			}
		}
		// Bill the gate's revive-coin ledger once per committed PAID revive. This
		// sits in the commit branch, not in the replay branch above, so a duplicated
		// op41 cannot double count; a free revive (practice / private-server) leaves
		// Consumed false and is not a coin.
		//
		// The ledger lives on the RAID, keyed by (raid, phase, row, gate), not on
		// runtimeDungeonState as it used to: the runtime is allocated per run, so the
		// old per-run counter silently reset every re-entry and the board went back to
		// 12/12 in the same gate (live 2026-09-23). 86JP keeps the same two counters
		// on RaidDungeonParticipation, which is per (dungeon, party)
		// (RaidHandler.Progression.cs:1338). It is bookkeeping only: no branch below
		// reads it.
		if consumeResult.Consumed {
			raidCoinKey, raidCoinTotal, raidCoinSpent = s.recordRuntimeRaidReviveCoinUseLocked(session, runtime)
		}
	}

	source := "current_exe_op41_revive_coin_consumed"
	if consume.FreeRevive {
		if currentDungeonRuntimeIsPractice(runtime) {
			source = "current_exe_op41_practice_free_revive"
		} else {
			source = "current_exe_op41_private_server_free_revive"
		}
	}

	reviveStateBody := buildCurrentDungeonDeathStateBody(targetObjectKey, 1, 0)
	if err := s.sendGameUpperRawClass(session, currentDungeonDeathStateMsgID, reviveStateBody, 0); err != nil {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-use-coin-revive-state-failed",
			"char_id", characterID,
			"target_object_key", targetObjectKey,
			"run_token", runToken,
			"response_replayed", responseReplayed,
			"error", err)
		return err
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketUseCoin),
		buildCurrentDungeonUseCoinSuccessBody(targetObjectKey),
	); err != nil {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-use-coin-ack-failed",
			"char_id", characterID,
			"target_object_key", targetObjectKey,
			"run_token", runToken,
			"response_replayed", responseReplayed,
			"error", err)
		return err
	}
	s.completeCurrentDungeonReviveLocked(session, runtime, source)
	session.dungeon.mu.Unlock()

	if raidCoinSpent {
		// 86JP broadcasts the 战况表 row on a coin revive
		// (RaidHandler.Progression.cs:1338), and without it the client keeps showing
		// the number from the last packet -- live 2026-09-23, six coins spent and the
		// panel still read 10/12. Scheduled, never sent from here: this goroutine
		// still is the packet path and the broadcast reads every member's session
		// lock (see scheduleRuntimeRaidMonsterStateBroadcast).
		s.scheduleRuntimeRaidMonsterStateBroadcast(raidCoinKey.RaidKey, "revive_coin_spent")
	}

	if consume.HasItemUpdate {
		updateBody := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{consume.ItemUpdate})
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), updateBody, 0); err != nil {
			s.logGameEvent(session, "game-dungeon-use-coin-inventory-update-failed",
				"char_id", characterID,
				"slot_key", consume.SlotKey,
				"count_after", consume.CountAfter,
				"error", err)
			return err
		}
	}

	if phaseCoinCharged && phaseCoin.Limit > 0 {
		// 每个成员的客户端各自存一份阶段计数：刚复活的那个和其余队员都需要新总数。
		// 只有单图上限（卢克一阶段）是在服务端强制、客户端没有副本，所以这里按 Limit>0 过滤。
		s.broadcastRuntimeRaidPhaseCoinUsage(phaseCoin.RaidKey, phaseCoin.Used, "use_coin_accepted")
	}

	s.logGameEvent(session, "game-dungeon-use-coin-success",
		"char_id", characterID,
		"target_object_key", targetObjectKey,
		"dungeon_id", dungeonID,
		"run_token", runToken,
		"timer_generation", generation,
		"coin_consumed", consume.Consumed,
		"free_revive", consume.FreeRevive,
		"slot_key", consume.SlotKey,
		"item_id", consume.ItemID,
		"count_after", consume.CountAfter,
		"inventory_update_sent", consume.HasItemUpdate,
		"success_flag", currentDungeonUseCoinSuccessFlag,
		"revive_state_sent", true,
		"response_replayed", responseReplayed,
		"response_sent", true,
		"timer_cancelled", true,
		"raid_revive_coin_counted", raidCoinSpent,
		"raid_revive_coin_gate", raidCoinKey.DungeonID,
		"raid_revive_coin_total", raidCoinTotal,
		"source", source)
	return nil
}

func (s *Service) consumeCurrentDungeonReviveCoin(
	ctx context.Context,
	characterID uint16,
) (currentDungeonReviveCoinConsumeResult, error) {
	result := currentDungeonReviveCoinConsumeResult{}
	if characterID == 0 {
		if currentDungeonUseCoinAllowFreeRevive {
			result.FreeRevive = true
			return result, nil
		}
		return result, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterItems == nil {
		if currentDungeonUseCoinAllowFreeRevive {
			result.FreeRevive = true
			return result, nil
		}
		return result, errors.New("character item transaction unavailable")
	}
	characterKey := strconv.FormatUint(uint64(characterID), 10)
	owner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		return result, err
	}
	var update currentItemListEntry
	consume, err := owner.ConsumeReviveCoin(ctx, dnfdungeon.ReviveCoinCommand{
		CharacterID: characterKey,
		ItemID:      int64(csharpReviveCoinItemID),
		WalletSlot:  int16(csharpReviveCoinWalletSlot),
		AllowFree:   currentDungeonUseCoinAllowFreeRevive,
		UpdatedAt:   time.Now().UTC(),
		Project: func(slot int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			update = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), update.data[:]...)
			return stack, nil
		},
	})
	if err != nil {
		return result, err
	}
	result = currentDungeonReviveCoinConsumeResult{
		Consumed:      consume.Consumed,
		FreeRevive:    consume.FreeRevive,
		SlotKey:       consume.SlotKey,
		Slot:          consume.Slot,
		ItemID:        consume.ItemID,
		CountAfter:    consume.CountAfter,
		HasItemUpdate: consume.Consumed,
	}
	if consume.Removed {
		update.patchCore(consume.Slot, math.MaxUint32, 0)
	}
	result.ItemUpdate = update
	return result, nil
}
