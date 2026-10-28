package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentTournamentRewardAutoSelectDelay = 3 * time.Second

// sendCurrentTournamentClearRewardLocked freezes the PVF-selected reward
// faces and projects op388 once. Visible cards are committed through op458;
// an all-empty PVF plan has no client selection and crosses the gate here.
func (s *Service) sendCurrentTournamentClearRewardLocked(session *gameSession, runtime *runtimeDungeonState) error {
	if s == nil || session == nil || runtime == nil || runtime.tournament == nil {
		return nil
	}
	if runtime.tournamentClearRewardSent {
		plan, ok := runtime.tournament.RewardPlanSnapshot()
		if ok && plan.Cardless() && runtime.tournament.RewardSelectionComplete() {
			return s.completeCurrentTournamentRewardSelectionLocked(session, runtime)
		}
		return nil
	}
	_, completed, terminal := runtime.tournament.Snapshot()
	if terminal == worldmap.TournamentActive {
		return nil
	}
	experience, ok := runtime.tournament.Definition.Experience[completed]
	if !ok {
		return fmt.Errorf("tournament reward experience missing for completed_rounds=%d", completed)
	}
	experienceSource := "current_pvf_tournament_clear_reward_experience"
	if experience == 0 {
		catalog, catalogErr := s.dungeonMonsterCatalog()
		if catalogErr == nil {
			resources, resourceErr := currentDungeonMonsterExperienceResourcesForCatalog(catalog)
			if resourceErr == nil {
				fallback, fallbackErr := currentDungeonClearExperience(runtime, currentDungeonSettlementRewardResources{
					Progression:       resources.Progression,
					ExperienceSources: resources.Sources,
				})
				if fallbackErr == nil {
					experience = fallback
					experienceSource = "ordinary_clear_base_experience_for_zero_tournament_pvf"
				} else {
					s.logGameEvent(session, "game-tournament-display-experience-fallback-blocked",
						"dungeon_id", runtime.Dungeon.ID,
						"completed_rounds", completed,
						"reason", "clear_experience_invalid",
						"error", fallbackErr)
				}
			} else {
				s.logGameEvent(session, "game-tournament-display-experience-fallback-blocked",
					"dungeon_id", runtime.Dungeon.ID,
					"completed_rounds", completed,
					"reason", "progression_resources_unavailable",
					"error", resourceErr)
			}
		} else {
			s.logGameEvent(session, "game-tournament-display-experience-fallback-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"completed_rounds", completed,
				"reason", "monster_catalog_unavailable",
				"error", catalogErr)
		}
	}
	gold, err := s.currentTournamentRewardGold(runtime)
	if err != nil {
		return err
	}
	plan, _, err := runtime.tournament.PrepareRewardPlan(experience, gold, s.chooseDungeonIndex)
	if err != nil {
		return err
	}
	packet := currentTournamentClearReward{
		CompletedRounds: plan.CompletedRounds,
		Experience:      plan.Experience,
	}
	cardless := plan.Cardless()
	if !cardless {
		for side := byte(0); side < 2; side++ {
			entries := make([]currentTournamentClearRewardEntry, 0, plan.CardCount(side))
			for index := byte(0); index < plan.CardCount(side); index++ {
				reward, _ := plan.Card(side, index)
				entries = append(entries, currentTournamentRewardEntry(reward))
			}
			if side == 0 {
				packet.GroupA = entries
			} else {
				packet.GroupB = entries
			}
		}
	}
	body, err := packet.Build()
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentTournamentClearRewardNotification, body, 0); err != nil {
		return err
	}
	runtime.tournamentClearRewardSent = true
	runtime.tournamentRewardExperience = plan.Experience
	runtime.tournamentRewardGold = gold
	s.logGameEvent(session, "game-tournament-clear-reward-sent",
		"dungeon_id", runtime.Dungeon.ID,
		"completed_rounds", plan.CompletedRounds,
		"experience", plan.Experience,
		"experience_source", experienceSource,
		"gold", gold,
		"champion", plan.Champion,
		"cardless", cardless,
		"group_a_count", len(packet.GroupA),
		"group_b_count", len(packet.GroupB),
		"body_len", len(body),
		"source", "current_exe_op388_verified_reader")
	if cardless {
		if !runtime.tournament.CommitCardlessRewardPlan() {
			return fmt.Errorf("commit cardless tournament reward plan")
		}
		return s.completeCurrentTournamentRewardSelectionLocked(session, runtime)
	}
	return nil
}

func currentTournamentRewardEntry(reward worldmap.TournamentReward) currentTournamentClearRewardEntry {
	if reward.Gold {
		return currentTournamentClearRewardEntry{CountOrGold: reward.Count}
	}
	if reward.ItemID != 0 {
		return currentTournamentClearRewardEntry{ItemOrSentinel: reward.ItemID, CountOrGold: reward.Count}
	}
	return currentTournamentClearRewardEntry{ItemOrSentinel: currentTournamentEmptyRewardSentinel}
}

func (s *Service) currentTournamentRewardGold(runtime *runtimeDungeonState) (uint32, error) {
	if s == nil || runtime == nil || runtime.tournament == nil {
		return 0, fmt.Errorf("tournament runtime is required for gold")
	}
	if runtime.Dungeon.Metadata.BasisLevel.Value <= 0 || runtime.Dungeon.Metadata.TournamentGoldRate <= 0 {
		return 0, fmt.Errorf("tournament gold metadata is invalid")
	}
	catalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return 0, err
	}
	references, err := currentDungeonGoldReferences(catalog)
	if err != nil {
		return 0, err
	}
	reference, ok := references[runtime.Dungeon.Metadata.BasisLevel.Value]
	if !ok {
		return 0, fmt.Errorf("tournament gold reference missing for level=%d", runtime.Dungeon.Metadata.BasisLevel.Value)
	}
	rng := newCurrentDungeonDropLCG(runtime.Seed)
	base := currentDungeonCardGoldAmount(rng, reference, currentDungeonCardGoldTerms{
		Difficulty:    int(runtime.Request.Difficulty),
		PartyMembers:  1,
		PartyRate:     1,
		WeightedKills: runtime.currentDungeonClearWeightedKills(),
	})
	if base <= 0 || base > math.MaxInt64/runtime.Dungeon.Metadata.TournamentGoldRate {
		return 0, fmt.Errorf("tournament gold overflows: base=%d rate=%d", base, runtime.Dungeon.Metadata.TournamentGoldRate)
	}
	amount := base * runtime.Dungeon.Metadata.TournamentGoldRate
	if amount > math.MaxUint32 {
		return 0, fmt.Errorf("tournament gold exceeds wire range: %d", amount)
	}
	return uint32(amount), nil
}

func decodeCurrentTournamentRewardStateRequest(body []byte) error {
	if len(body) != 0 {
		return fmt.Errorf("%w: op457 body_len=%d want=0", errTournamentProtocolRewardRequest, len(body))
	}
	return nil
}

func (s *Service) handleTournamentRewardSelectState(session *gameSession, body []byte) error {
	if err := decodeCurrentTournamentRewardStateRequest(body); err != nil {
		s.logGameEvent(session, "game-tournament-reward-state-blocked", "body_len", len(body), "reason", "request_shape", "error", err)
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.tournament == nil || !runtime.tournamentClearRewardSent {
		s.logGameEvent(session, "game-tournament-reward-state-blocked", "reason", "clear_reward_not_sent")
		return nil
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentTournamentRewardSelectState,
		append([]byte(nil), currentTournamentRewardSelectOpenBody...),
		1,
	); err != nil {
		return err
	}
	s.armCurrentTournamentRewardAutoSelectLocked(session, runtime)
	return nil
}

func (s *Service) handleTournamentRewardSelect(session *gameSession, body []byte) error {
	request, err := decodeCurrentTournamentRewardSelectRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-tournament-reward-select-blocked", "body_len", len(body), "reason", "request_shape", "error", err)
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.tournament == nil || !runtime.tournamentClearRewardSent {
		s.logGameEvent(session, "game-tournament-reward-select-blocked", "side", request.Side, "card_index", request.CardIndex, "reason", "clear_reward_not_sent")
		return nil
	}
	return s.applyCurrentTournamentRewardSelectionLocked(session, runtime, request, "client")
}

func (s *Service) applyCurrentTournamentRewardSelectionLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	request currentTournamentRewardSelectRequest,
	source string,
) error {
	if session == nil || runtime == nil || runtime.tournament == nil ||
		session.dungeon.runtime != runtime || !runtime.tournamentClearRewardSent {
		return nil
	}
	reward, reserved, replay := runtime.tournament.ReserveRewardSelection(request.Side, request.CardIndex)
	if !reserved && !replay {
		s.logGameEvent(session, "game-tournament-reward-select-blocked", "side", request.Side, "card_index", request.CardIndex, "source", source, "reason", "selection_rejected")
		return nil
	}
	bundle, err := s.currentTournamentRewardBundle(reward)
	if err != nil {
		if reserved {
			runtime.tournament.RollbackRewardSelection(request.Side, request.CardIndex)
		}
		return err
	}
	if reserved {
		grant, err := s.grantCurrentTournamentRewardLocked(session, runtime, bundle)
		if err != nil {
			runtime.tournament.RollbackRewardSelection(request.Side, request.CardIndex)
			return err
		}
		if !runtime.tournament.CommitRewardSelection(request.Side, request.CardIndex) {
			// The database grant has already committed. Do not roll the
			// selection back here: doing so could grant the same reward again
			// on a retry if this invariant is ever violated.
			s.logGameEvent(session, "game-tournament-reward-select-commit-gate-failed",
				"side", request.Side, "card_index", request.CardIndex,
				"gold_after", grant.GoldAfter, "item_slots", len(grant.ItemSlots))
			return fmt.Errorf("tournament reward selection commit gate rejected side=%d index=%d", request.Side, request.CardIndex)
		}
		if err := s.projectCurrentTournamentRewardLocked(session, grant); err != nil {
			// Durable state is already committed; the caller may retry the
			// same request to replay the client projection.
			return err
		}
	}
	plan, ok := runtime.tournament.RewardPlanSnapshot()
	if !ok {
		return fmt.Errorf("tournament reward plan missing after selection commit")
	}
	selectionState, err := (currentTournamentRewardSelectionState{Plan: plan}).build()
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentTournamentRewardSelect, selectionState, 1); err != nil {
		return err
	}
	s.logGameEvent(session, "game-tournament-reward-select-sent",
		"side", request.Side, "card_index", request.CardIndex,
		"item_id", reward.ItemID, "count", reward.Count,
		"gold", reward.Gold, "empty", reward.Empty,
		"reserved", reserved, "replay", replay,
		"item_detail_response_sent", false,
		"selection_state_len", len(selectionState),
		"source", source)
	if runtime.tournament.RewardSelectionComplete() {
		return s.completeCurrentTournamentRewardSelectionLocked(session, runtime)
	}
	return nil
}

func (s *Service) armCurrentTournamentRewardAutoSelectLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) {
	if s == nil || session == nil || runtime == nil || runtime.tournament == nil ||
		session.dungeon.runtime != runtime || runtime.lifecycleToken == 0 ||
		runtime.tournament.RewardSelectionComplete() || runtime.tournamentRewardAutoSelectTimerName != "" {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	runtime.tournamentRewardAutoSelectGeneration++
	generation := runtime.tournamentRewardAutoSelectGeneration
	runToken := runtime.lifecycleToken
	timerName := fmt.Sprintf("dnf-tournament-reward-auto-select:%s:run:%d", session.connID, runToken)
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		return
	}
	runtime.tournamentRewardAutoSelectTimerName = timerName
	if err := queue.ScheduleAfter(timerName, currentTournamentRewardAutoSelectDelay, func(time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"tournament-reward-auto-select",
			characterID,
			characterGeneration,
			func() error {
				session.dungeon.mu.Lock()
				defer session.dungeon.mu.Unlock()
				if session.dungeon.runtime != runtime || runtime.lifecycleToken != runToken ||
					runtime.tournamentRewardAutoSelectGeneration != generation ||
					runtime.tournamentRewardAutoSelectTimerName != timerName {
					return nil
				}
				runtime.tournamentRewardAutoSelectTimerName = ""
				return s.autoSelectCurrentTournamentRewardsLocked(session, runtime)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logPacketEvent("game-tournament-reward-auto-select-submit-failed",
				"conn_id", session.connID, "char_id", characterID, "error", postErr)
		}
	}); err != nil {
		runtime.tournamentRewardAutoSelectTimerName = ""
		s.logGameEvent(session, "game-tournament-reward-auto-select-schedule-failed",
			"dungeon_id", runtime.Dungeon.ID, "error", err)
		return
	}
	s.logGameEvent(session, "game-tournament-reward-auto-select-scheduled",
		"dungeon_id", runtime.Dungeon.ID, "run_token", runToken,
		"timer_generation", generation, "delay_ms", currentTournamentRewardAutoSelectDelay.Milliseconds())
}

func (s *Service) autoSelectCurrentTournamentRewardsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if session == nil || runtime == nil || runtime.tournament == nil ||
		session.dungeon.runtime != runtime || runtime.tournament.RewardSelectionComplete() {
		return nil
	}
	plan, ok := runtime.tournament.RewardPlanSnapshot()
	if !ok {
		return nil
	}
	for side := byte(0); side < 2; side++ {
		if plan.CardCount(side) == 0 || runtime.tournament.RewardSideSelectionComplete(side) {
			continue
		}
		if err := s.applyCurrentTournamentRewardSelectionLocked(
			session,
			runtime,
			currentTournamentRewardSelectRequest{Side: side, CardIndex: 0},
			"timequeue_auto_select",
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cancelCurrentTournamentRewardAutoSelectLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if runtime == nil {
		return
	}
	timerName := runtime.tournamentRewardAutoSelectTimerName
	runtime.tournamentRewardAutoSelectTimerName = ""
	runtime.tournamentRewardAutoSelectGeneration++
	if timerName == "" || s == nil {
		return
	}
	cancelled := false
	if queue := s.ensureGameplayTimeQueue(); queue != nil {
		cancelled = queue.Cancel(timerName)
	}
	s.logGameEvent(session, "game-tournament-reward-auto-select-cancelled",
		"dungeon_id", runtime.Dungeon.ID, "source", source, "cancelled", cancelled)
}

// completeCurrentTournamentRewardSelectionLocked is the tournament equivalent
// of the ordinary final-room clear coordinator. The current EXE only proceeds
// to the result request after both op458 selections have been committed; an
// an all-empty PVF plan crosses the same gate without exposing a card.
func (s *Service) completeCurrentTournamentRewardSelectionLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if session == nil || runtime == nil || runtime.tournament == nil ||
		runtime.Session == nil || session.dungeon.runtime != runtime ||
		!runtime.tournament.RewardSelectionComplete() {
		return nil
	}
	s.cancelCurrentTournamentRewardAutoSelectLocked(session, runtime, "reward_selection_complete")
	snapshot := runtime.Session.Snapshot()
	if snapshot.Run.Status == worldmap.DungeonRunCompleted {
		return s.sendCurrentDungeonSettlementEntryLocked(
			session, runtime, "tournament_reward_selection_replay")
	}
	if snapshot.Run.Status != worldmap.DungeonRunActive {
		return nil
	}
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		return fmt.Errorf("complete tournament reward room: %w", err)
	}
	completed := runtime.Session.Snapshot()
	s.logGameEvent(session, "game-tournament-reward-selection-complete",
		"dungeon_id", runtime.Dungeon.ID,
		"run_status", completed.Run.Status,
		"source", "current_exe_op458_two_phase_selection")
	return s.sendCurrentDungeonSettlementEntryLocked(
		session, runtime, "tournament_reward_selection_complete")
}

func (s *Service) currentTournamentRewardBundle(reward worldmap.TournamentReward) (dungeonCardRewardBundle, error) {
	if reward.Gold {
		return dungeonCardRewardBundle{Gold: int64(reward.Count)}, nil
	}
	if reward.Empty || reward.ItemID == 0 {
		return dungeonCardRewardBundle{}, nil
	}
	catalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return dungeonCardRewardBundle{}, err
	}
	definition, err := catalog.DropCatalog()
	if err != nil {
		return dungeonCardRewardBundle{}, err
	}
	item, err := definition.ResolveItem(reward.ItemID)
	if err != nil {
		return dungeonCardRewardBundle{}, err
	}
	extra := map[string]string{
		"source":    "tournament_reward",
		"item_kind": string(item.Kind),
		"pvf_path":  item.PVFPath,
	}
	if item.StackableType != "" {
		extra["stackable_type"] = item.StackableType
	}
	if item.StackLimit > 0 {
		extra["stack_limit"] = strconv.FormatInt(item.StackLimit, 10)
	}
	if item.EquipmentType != "" {
		extra["equipment_type"] = item.EquipmentType
	}
	if item.Durability > 0 {
		extra["durability"] = strconv.FormatUint(uint64(item.Durability), 10)
		extra["max_durability"] = strconv.FormatUint(uint64(item.Durability), 10)
	}
	stack := dnfrepo.ItemStack{ItemID: int64(reward.ItemID), Count: int64(reward.Count), Extra: extra}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, stack)
	raw := append([]byte(nil), entry.data[:]...)
	return dungeonCardRewardBundle{Items: []dungeonCardItemReward{{
		ItemID: int64(reward.ItemID), Count: int64(reward.Count),
		Stackable: item.Kind == dungeonDropItemStackable, StackLimit: item.StackLimit,
		SlotStart: item.SlotStart, SlotEnd: item.SlotEnd,
		ExpireAt: item.ExpirationDate, RawEntry: append([]byte(nil), raw...), Extra: extra,
	}}}, nil
}

func (s *Service) grantCurrentTournamentRewardLocked(session *gameSession, runtime *runtimeDungeonState, bundle dungeonCardRewardBundle) (dungeonCardAssetGrantResult, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil || repositories.Settings == nil {
		return dungeonCardAssetGrantResult{}, errDungeonCardAssetOwnerUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	state := s.loadCurrentItemListContainerState(ctx, repositories.Settings, characterID, dnfrepo.MainInventoryListType)
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), createWriteTimeout)
	grant, err := grantDungeonCardRewardBundle(ctx, repositories, characterID, state.MainSlotCount, bundle, s.gameplayNow().UTC())
	cancel()
	if err != nil {
		return dungeonCardAssetGrantResult{}, err
	}
	return grant, nil
}

func (s *Service) projectCurrentTournamentRewardLocked(session *gameSession, grant dungeonCardAssetGrantResult) error {
	if grant.GoldAfter != grant.GoldBefore {
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(grant.GoldAfter), 0); err != nil {
			return err
		}
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(grant.ItemSlots))
	for _, slot := range grant.ItemSlots {
		if slot >= 0 {
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: slot})
		}
	}
	return s.sendSelectedIncrementalItemSlotRefreshes(session, "tournament-reward", refreshes)
}
