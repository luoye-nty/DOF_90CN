package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/crystalcontract"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// Crystal Contract is a client-applied weapon attribute whose consumable cost
// belongs to the active dungeon lifecycle. Current NoPack sends op535 only
// when the selection changes and never sends a periodic cube-use command.
const currentCrystalContractConsumeInterval = 30 * time.Second

func (s *Service) armCurrentCrystalContractTimer(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if s == nil || session == nil || runtime == nil || currentDungeonRuntimeIsPractice(runtime) {
		return nil
	}
	owner, err := s.currentCrystalContractOwner()
	if err != nil {
		return fmt.Errorf("build crystal contract dungeon owner: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	state, err := owner.State(
		ctx,
		s.accountIDForSession(session),
		strconv.Itoa(int(session.selectedCharacterID)),
		s.gameplayNow().UTC(),
	)
	cancel()
	if err != nil {
		return fmt.Errorf("load crystal contract dungeon state: %w", err)
	}
	if !state.Active || state.Selection == crystalcontract.SelectionNone || state.CubeItemID <= 0 {
		return nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return s.scheduleCurrentCrystalContractTimerLocked(session, runtime, queue)
}

func (s *Service) scheduleCurrentCrystalContractTimerLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	queue gameplayTimeQueue,
) error {
	if session == nil || runtime == nil || queue == nil ||
		session.dungeon.runtime != runtime || runtime.lifecycleToken == 0 ||
		runtime.lifecycleState != currentDungeonRuntimeActive || runtime.Session == nil ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive ||
		currentDungeonRuntimeIsPractice(runtime) {
		return nil
	}
	if runtime.crystalContractTimerActive {
		return nil
	}
	runtime.crystalContractTimerGeneration = nextCurrentCrystalContractTimerGeneration(
		runtime.crystalContractTimerGeneration,
	)
	generation := runtime.crystalContractTimerGeneration
	timerName := fmt.Sprintf(
		"dnf-crystal-contract:%s:run:%d:tick:%d",
		session.connID,
		runtime.lifecycleToken,
		generation,
	)
	runtime.crystalContractTimerActive = true
	runtime.crystalContractTimerName = timerName
	runtime.crystalContractTimerDueAt = queue.Now().Add(currentCrystalContractConsumeInterval)
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(
		session,
		session.selectedCharacterID,
	)
	if err != nil {
		s.cancelCurrentCrystalContractTimerLocked(session, runtime, "timer_character_identity_changed")
		return err
	}
	dungeonID := runtime.Dungeon.ID
	runToken := runtime.lifecycleToken
	if err := queue.ScheduleAfter(timerName, currentCrystalContractConsumeInterval, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"crystal-contract-timequeue",
			characterID,
			characterGeneration,
			func() error {
				s.fireCurrentCrystalContractTimer(session, runtime, generation, due)
				return nil
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logPacketEvent("game-session-event-submit-failed",
				"conn_id", session.connID,
				"source", "crystal-contract-timequeue",
				"char_id", characterID,
				"character_generation", characterGeneration,
				"dungeon_id", dungeonID,
				"run_token", runToken,
				"error", postErr)
		}
	}); err != nil {
		s.cancelCurrentCrystalContractTimerLocked(session, runtime, "timer_schedule_failed")
		return err
	}
	s.logGameEvent(session, "game-crystal-contract-timer-scheduled",
		"char_id", characterID,
		"dungeon_id", dungeonID,
		"run_token", runToken,
		"timer_generation", generation,
		"timer_name", timerName,
		"due_at_utc", runtime.crystalContractTimerDueAt.UTC().Format(time.RFC3339Nano),
		"interval_seconds", int64(currentCrystalContractConsumeInterval/time.Second),
		"source", "current_exe_no_periodic_c2s_server_owned_dungeon_tick")
	return nil
}

func (s *Service) fireCurrentCrystalContractTimer(
	session *gameSession,
	runtime *runtimeDungeonState,
	generation uint64,
	due time.Time,
) {
	if s == nil || session == nil || runtime == nil {
		return
	}
	session.dungeon.mu.Lock()
	if session.dungeon.runtime != runtime || !runtime.crystalContractTimerActive ||
		runtime.crystalContractTimerGeneration != generation || runtime.lifecycleToken == 0 ||
		runtime.lifecycleState != currentDungeonRuntimeActive || runtime.Session == nil ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive {
		session.dungeon.mu.Unlock()
		return
	}
	runtime.crystalContractTimerActive = false
	runtime.crystalContractTimerName = ""
	runtime.crystalContractTimerDueAt = time.Time{}
	runToken := runtime.lifecycleToken
	dungeonID := runtime.Dungeon.ID
	session.dungeon.mu.Unlock()

	owner, err := s.currentCrystalContractOwner()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		result, consumeErr := owner.ConsumeSelected(
			ctx,
			s.accountIDForSession(session),
			strconv.Itoa(int(session.selectedCharacterID)),
			due.UTC(),
		)
		cancel()
		err = consumeErr
		if err == nil {
			s.logGameEvent(session, "game-crystal-contract-dungeon-consume-committed",
				"char_id", session.selectedCharacterID,
				"dungeon_id", dungeonID,
				"run_token", runToken,
				"timer_generation", generation,
				"slot_index", result.SlotIndex,
				"item_id", result.ItemID,
				"consumed", result.Consumed,
				"remaining", result.Remaining,
				"selection_after", result.SelectionAfter,
				"state_source", "atomic_selected_account_crystal_warehouse_tick")
			if sendErr := s.sendCurrentCrystalContractCubeInventoryUpdate(session, result); sendErr != nil {
				s.logGameEvent(session, "game-crystal-contract-dungeon-consume-refresh-failed",
					"dungeon_id", dungeonID, "run_token", runToken, "error", sendErr)
				return
			}
			if result.SelectionAfter == crystalcontract.SelectionNone {
				if sendErr := s.sendCurrentCrystalContractState(session, "dungeon_tick_after_last_selected_cube"); sendErr != nil {
					s.logGameEvent(session, "game-crystal-contract-dungeon-state-refresh-failed",
						"dungeon_id", dungeonID, "run_token", runToken, "error", sendErr)
				}
				return
			}
			s.rearmCurrentCrystalContractTimer(session, runtime)
			return
		}
	}

	stop := errors.Is(err, crystalcontract.ErrContractInactive) ||
		errors.Is(err, crystalcontract.ErrSelectionInvalid) ||
		errors.Is(err, crystalcontract.ErrCubeUnavailable) ||
		errors.Is(err, crystalcontract.ErrCubeRequestMismatch)
	s.logGameEvent(session, "game-crystal-contract-dungeon-consume-failed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", dungeonID,
		"run_token", runToken,
		"timer_generation", generation,
		"terminal_for_current_selection", stop,
		"error", err)
	if stop {
		if sendErr := s.sendCurrentCrystalContractState(session, "dungeon_tick_after_selection_became_unavailable"); sendErr != nil {
			s.logGameEvent(session, "game-crystal-contract-dungeon-state-refresh-failed",
				"dungeon_id", dungeonID, "run_token", runToken, "error", sendErr)
		}
		return
	}
	// A transient repository failure must not permanently disable an otherwise
	// active contract for the rest of this dungeon attempt.
	s.rearmCurrentCrystalContractTimer(session, runtime)
}

func (s *Service) rearmCurrentCrystalContractTimer(
	session *gameSession,
	runtime *runtimeDungeonState,
) {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil || session == nil || runtime == nil {
		return
	}
	session.dungeon.mu.Lock()
	err := s.scheduleCurrentCrystalContractTimerLocked(session, runtime, queue)
	session.dungeon.mu.Unlock()
	if err != nil {
		s.logGameEvent(session, "game-crystal-contract-timer-rearm-failed", "error", err)
	}
}

func (s *Service) sendCurrentCrystalContractCubeInventoryUpdate(
	session *gameSession,
	result crystalcontract.ConsumeResult,
) error {
	stack := dnfrepo.ItemStack{
		ItemID: result.ItemID,
		Count:  result.Remaining,
		Extra:  map[string]string{"item_kind": "stackable"},
	}
	if result.Remaining <= 0 {
		stack.ItemID = 0
		stack.Count = 0
	}
	entry := currentItemListEntryFromStack(
		dnfrepo.MainInventoryListType,
		int16(result.SlotIndex),
		stack,
	)
	body := buildCurrentItemUpdateBody(
		dnfrepo.MainInventoryListType,
		[]currentItemListEntry{entry},
	)
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		body,
		0,
	)
}

// cancelCurrentCrystalContractTimerLocked invalidates a dequeued callback and
// removes the pending named task. The caller holds session.dungeon.mu.
func (s *Service) cancelCurrentCrystalContractTimerLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if runtime == nil {
		return
	}
	wasActive := runtime.crystalContractTimerActive
	timerName := runtime.crystalContractTimerName
	previousGeneration := runtime.crystalContractTimerGeneration
	runtime.crystalContractTimerGeneration = nextCurrentCrystalContractTimerGeneration(previousGeneration)
	runtime.crystalContractTimerActive = false
	runtime.crystalContractTimerName = ""
	runtime.crystalContractTimerDueAt = time.Time{}
	cancelled := false
	if timerName != "" && s != nil && s.gameplayTimers != nil {
		cancelled = s.gameplayTimers.Cancel(timerName)
	}
	if wasActive || timerName != "" {
		s.logGameEvent(session, "game-crystal-contract-timer-cancelled",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"previous_generation", previousGeneration,
			"current_generation", runtime.crystalContractTimerGeneration,
			"timer_name", timerName,
			"queue_item_cancelled", cancelled,
			"source", source)
	}
}

func nextCurrentCrystalContractTimerGeneration(current uint64) uint64 {
	next := current + 1
	if next == 0 {
		next++
	}
	return next
}
