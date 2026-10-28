package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfaltar "longheng.io/server/internal/modules/dnf/dungeonaltar"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

var errCurrentDungeonAltarSettlementInvalid = errors.New("current dungeon altar settlement is invalid")

type currentDungeonAltarSettlementPhase uint8

const (
	currentDungeonAltarSettlementPrepared currentDungeonAltarSettlementPhase = iota
	currentDungeonAltarSettlementRankingShown
	currentDungeonAltarSettlementRewardShown
	currentDungeonAltarSettlementRewardCommitted
	currentDungeonAltarSettlementExitReady
)

type currentDungeonAltarSettlementPlan struct {
	CurrentRound         uint32
	PlayTimeMilliseconds uint32
	BestRound            uint32
	BestTimeMilliseconds uint32
	MaxRound             uint32
	RewardExperience     uint32
	RewardGroups         [4][]currentDungeonAltarRewardEntry
	RewardBundle         dungeonCardRewardBundle
	CharacterStateBody   []byte
}

type currentDungeonAltarRewardPlanningResult struct {
	plan         currentDungeonAltarSettlementPlan
	state        *dungeonCardState
	visit        *runtimeDungeonRoomVisit
	nextDropSeed uint32
	definition   currentDungeonAltarRewardDefinition
}

// currentDungeonAltarSettlementRuntime freezes the complete op208/op209 view
// and gates its one durable asset grant. It is owned by session.dungeon.mu;
// dungeonCardState provides a second one-shot barrier around the repository
// transaction so a settlement retry can never grant the same bundle twice.
type currentDungeonAltarSettlementRuntime struct {
	phase              currentDungeonAltarSettlementPhase
	plan               currentDungeonAltarSettlementPlan
	rewardState        *dungeonCardState
	rewardCommit       dungeonCardAssetGrantResult
	rewardCommitted    bool
	characterStateSent bool
	goldStateSent      bool
	inventoryStateSent bool
	mailboxAlarmSent   bool
}

func (s *Service) startCurrentDungeonAltarSettlementLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if s == nil || session == nil || runtime == nil || runtime.altar == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime ||
		runtime.altar.completedRounds != runtime.altar.MaxRounds ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		return errCurrentDungeonAltarSettlementInvalid
	}
	settlement, err := s.prepareCurrentDungeonAltarSettlementLocked(session, runtime)
	if err != nil {
		return err
	}
	switch settlement.phase {
	case currentDungeonAltarSettlementPrepared:
		body := buildCurrentDungeonAltarRankingBody(
			settlement.plan.CurrentRound,
			settlement.plan.PlayTimeMilliseconds,
			settlement.plan.BestRound,
			settlement.plan.BestTimeMilliseconds,
			settlement.plan.MaxRound,
			settlement.plan.RewardExperience,
		)
		if err := s.sendGameUpperRawClass(session, currentDungeonAltarRankingMsgID, body, 0); err != nil {
			return err
		}
		settlement.phase = currentDungeonAltarSettlementRankingShown
		s.logGameEvent(session, "game-dungeon-altar-ranking-sent",
			"dungeon_id", runtime.Dungeon.ID,
			"rounds", settlement.plan.CurrentRound,
			"max_rounds", settlement.plan.MaxRound,
			"play_time_ms", settlement.plan.PlayTimeMilliseconds,
			"reward_exp", settlement.plan.RewardExperience,
			"body_source", "current_exe_sub_1D38CF0_six_u32_time_current_best_time_best_max_exp")
		fallthrough
	case currentDungeonAltarSettlementRankingShown:
		return s.showCurrentDungeonAltarRewardLocked(session, runtime, settlement)
	case currentDungeonAltarSettlementRewardShown:
		return s.commitCurrentDungeonAltarRewardLocked(session, runtime, settlement)
	case currentDungeonAltarSettlementRewardCommitted:
		return s.projectCurrentDungeonAltarCommittedSettlementLocked(session, runtime, settlement)
	case currentDungeonAltarSettlementExitReady:
		return nil
	default:
		return errCurrentDungeonAltarSettlementInvalid
	}
}

func (s *Service) prepareCurrentDungeonAltarSettlementLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) (*currentDungeonAltarSettlementRuntime, error) {
	if runtime.altar.settlement != nil {
		return runtime.altar.settlement, nil
	}
	now := s.gameplayNow().UTC()
	snapshot := runtime.Session.Snapshot()
	if err := ensureCurrentDungeonCompletionReceiptKey(
		session,
		runtime,
		snapshot,
		now,
		"blood_altar_dedicated_settlement",
	); err != nil {
		return nil, err
	}
	rewardPlanning, err := s.planCurrentDungeonAltarRewardsLocked(runtime, now)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	resources, err := s.currentDungeonSettlementRewardResources(ctx)
	if err != nil {
		return nil, err
	}
	rewardExperience, err := currentDungeonAltarExperience(runtime, resources, rewardPlanning.definition)
	if err != nil {
		return nil, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, dnfaltar.ErrOwnerUnavailable
	}
	recordOwner, err := dnfaltar.NewOwner(repositories)
	if err != nil {
		return nil, err
	}
	record, err := recordOwner.UpdateBestRecord(
		ctx,
		runtime.Character.CharacterID,
		uint32(runtime.Dungeon.ID),
		rewardPlanning.plan.CurrentRound,
		rewardPlanning.plan.PlayTimeMilliseconds,
		"altar-record:"+runtime.clearMapCompletionKey,
		now,
	)
	if err != nil {
		return nil, err
	}
	rewardPlanning.plan.BestRound = record.BestRound
	rewardPlanning.plan.BestTimeMilliseconds = record.BestTimeMillis
	progressionCommit, err := s.commitCurrentDungeonSettlement(
		ctx,
		session,
		runtime,
		resources.Progression,
		rewardExperience,
	)
	if err != nil {
		return nil, err
	}
	runtime.Character = dnfrepo.CloneCharacter(progressionCommit.Character)
	rewardPlanning.plan.RewardExperience = progressionCommit.ExperienceGain
	rewardPlanning.plan.CharacterStateBody = buildCurrentFinishLoadingCharacterStateBody(
		progressionCommit.Character,
		progressionCommit.Skill.Points,
	)
	if len(rewardPlanning.plan.CharacterStateBody) != currentFinishLoadingCharacterStateBodySize {
		return nil, fmt.Errorf("%w: op37 body=%d", errCurrentDungeonAltarSettlementInvalid, len(rewardPlanning.plan.CharacterStateBody))
	}
	settlement := &currentDungeonAltarSettlementRuntime{
		phase:       currentDungeonAltarSettlementPrepared,
		plan:        rewardPlanning.plan,
		rewardState: rewardPlanning.state,
	}
	runtime.altar.settlement = settlement
	// Commit the room RNG cursor only after the complete settlement snapshot is
	// installed. Any transient progression/repository failure before this point
	// therefore retries the exact same reward plan instead of rerolling it.
	if rewardPlanning.visit != nil {
		rewardPlanning.visit.DropRNG = rewardPlanning.nextDropSeed
	}
	s.logGameEvent(session, "game-dungeon-altar-settlement-frozen",
		"dungeon_id", runtime.Dungeon.ID,
		"completion_key", runtime.clearMapCompletionKey,
		"rounds", rewardPlanning.plan.CurrentRound,
		"max_rounds", rewardPlanning.plan.MaxRound,
		"reward_exp", rewardPlanning.plan.RewardExperience,
		"reward_cards", len(rewardPlanning.plan.RewardGroups[0]),
		"reward_gold", rewardPlanning.plan.RewardBundle.Gold,
		"reward_items", len(rewardPlanning.plan.RewardBundle.Items),
		"progression_replayed", progressionCommit.Replayed,
		"experience_weight_vectors_applied", true,
		"entry_daily_count", runtime.altar.entryDailyCount,
		"best_round", record.BestRound,
		"best_time_ms", record.BestTimeMillis,
		"record_replayed", record.Replayed)
	return settlement, nil
}

func (s *Service) planCurrentDungeonAltarRewardsLocked(
	runtime *runtimeDungeonState,
	now time.Time,
) (currentDungeonAltarRewardPlanningResult, error) {
	if runtime == nil || runtime.altar == nil || runtime.altar.completedRounds <= 0 || runtime.altar.MaxRounds <= 0 ||
		!runtime.Dungeon.Metadata.BasisLevel.Set || runtime.Dungeon.Metadata.BasisLevel.Value <= 0 {
		return currentDungeonAltarRewardPlanningResult{}, errCurrentDungeonAltarSettlementInvalid
	}
	catalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	definition, err := loadCurrentDungeonAltarRewardDefinition(catalog.source)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	goldReferences, err := currentDungeonGoldReferences(catalog)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	dungeonLevel := int(runtime.Dungeon.Metadata.BasisLevel.Value)
	goldReference, found := goldReferences[int64(dungeonLevel)]
	if !found {
		return currentDungeonAltarRewardPlanningResult{}, fmt.Errorf("%w: altar dungeon level=%d", errCurrentDungeonGoldReferenceInvalid, dungeonLevel)
	}
	itemCatalog, err := catalog.DropCatalog()
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	dropDocument, err := parseDungeonCardPVFDocument(catalog.source, dungeonCardDropRulePath)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	itemReferences, err := parseDungeonCardPVFItemDropReferences(dropDocument)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	itemReference, found := itemReferences[int64(dungeonLevel)]
	if !found {
		return currentDungeonAltarRewardPlanningResult{}, fmt.Errorf("%w: altar dungeon level=%d", errDungeonCardPVFSectionMissing, dungeonLevel)
	}

	rng := newCurrentDungeonDropLCG(runtime.Seed)
	var visit *runtimeDungeonRoomVisit
	if scene, ok := runtime.Session.Scene(); ok {
		if ownedVisit, visitErr := runtime.currentDungeonRoomVisit(scene); visitErr == nil {
			visit = ownedVisit
			rng = newCurrentDungeonDropLCG(ownedVisit.DropRNG)
		}
	}
	rewardGroups, bundle, err := currentDungeonAltarGenerateRewards(
		definition,
		runtime.altar,
		rng,
		goldReference,
		currentDungeonCardGoldTerms{
			Difficulty:    int(runtime.Request.Difficulty),
			PartyMembers:  1,
			PartyRate:     1,
			WeightedKills: runtime.currentDungeonClearWeightedKills(),
		},
		itemCatalog,
		dungeonLevel,
		itemReference,
		now,
	)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	cardPlan, err := newDungeonCardRewardPlan(
		dungeonCardPlanIdentity{
			CharacterID: runtime.Character.CharacterID,
			DungeonID:   runtime.Dungeon.ID,
			MazeIndex:   runtime.MazeIndex,
			RunSeed:     runtime.Seed,
		},
		"current_pvf_bloodclearreward_dedicated_settlement",
		bundle,
		dungeonCardRewardBundle{},
	)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	rewardState, err := newDungeonCardState(cardPlan)
	if err != nil {
		return currentDungeonAltarRewardPlanningResult{}, err
	}
	playTime := now.Sub(runtime.startedAt)
	if runtime.startedAt.IsZero() || playTime < 0 {
		playTime = 0
	}
	playTimeMilliseconds := uint32(min(int64(math.MaxUint32), playTime.Milliseconds()))
	completedRounds := uint32(runtime.altar.completedRounds)
	maximumRounds := uint32(runtime.altar.MaxRounds)
	return currentDungeonAltarRewardPlanningResult{
		plan: currentDungeonAltarSettlementPlan{
			CurrentRound:         completedRounds,
			PlayTimeMilliseconds: playTimeMilliseconds,
			BestRound:            completedRounds,
			BestTimeMilliseconds: playTimeMilliseconds,
			MaxRound:             maximumRounds,
			RewardGroups:         rewardGroups,
			RewardBundle:         bundle,
		},
		state:        rewardState,
		visit:        visit,
		nextDropSeed: rng.Seed(),
		definition:   definition,
	}, nil
}

func currentDungeonAltarExperience(
	runtime *runtimeDungeonState,
	resources currentDungeonSettlementRewardResources,
	definition currentDungeonAltarRewardDefinition,
) (uint32, error) {
	if runtime == nil || runtime.altar == nil || resources.ExperienceSources == nil ||
		runtime.Character.Level <= 0 || runtime.altar.completedRounds <= 0 ||
		runtime.altar.completedRounds >= len(definition.RewardExperienceWeights) {
		return 0, errCurrentDungeonAltarSettlementInvalid
	}
	base, found := resources.ExperienceSources.MonsterTableValue(runtime.Character.Level)
	if !found || base == 0 {
		return 0, fmt.Errorf("%w: monster reward level=%d", errCurrentDungeonAltarSettlementInvalid, runtime.Character.Level)
	}
	weight := definition.RewardExperienceWeights[runtime.altar.completedRounds]
	value := float64(base) * float64(runtime.altar.completedRounds) * weight
	switch runtime.altar.Kind {
	case worldmap.BloodAltarDungeonKindUltimate:
		value *= 10
	case worldmap.BloodAltarDungeonKindEndless:
		if len(definition.EntryCountExperienceWeight) == 0 {
			return 0, errCurrentDungeonAltarSettlementInvalid
		}
		entryIndex := int(runtime.altar.entryDailyCount)
		if entryIndex >= len(definition.EntryCountExperienceWeight) {
			entryIndex = len(definition.EntryCountExperienceWeight) - 1
		}
		value *= definition.EntryCountExperienceWeight[entryIndex]
	default:
		return 0, errCurrentDungeonAltarSettlementInvalid
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errCurrentDungeonAltarSettlementInvalid
	}
	if value > 1_000_000 {
		value = 1_000_000
	}
	return uint32(value), nil
}

func currentDungeonAltarGenerateRewards(
	definition currentDungeonAltarRewardDefinition,
	altar *currentDungeonAltarRuntime,
	rng *currentDungeonDropLCG,
	goldReference currentDungeonGoldReference,
	goldTerms currentDungeonCardGoldTerms,
	itemCatalog *pvfDungeonDropCatalog,
	dungeonLevel int,
	itemReference dungeonCardPVFItemDropReference,
	now time.Time,
) ([4][]currentDungeonAltarRewardEntry, dungeonCardRewardBundle, error) {
	var groups [4][]currentDungeonAltarRewardEntry
	if altar == nil || rng == nil || itemCatalog == nil || altar.completedRounds <= 0 || altar.MaxRounds <= 0 {
		return groups, dungeonCardRewardBundle{}, errCurrentDungeonAltarSettlementInvalid
	}
	count := definition.rewardCardCount(altar.completedRounds, altar.MaxRounds)
	scale := definition.rewardRollScale()
	if count <= 0 || scale <= 0 {
		return groups, dungeonCardRewardBundle{}, errCurrentDungeonAltarRewardDefinitionInvalid
	}
	groups[0] = make([]currentDungeonAltarRewardEntry, 0, count+1)
	bundle := dungeonCardRewardBundle{}
	for index := 0; index < count; index++ {
		candidate, err := definition.classifyReward(altar.completedRounds, rng.Next(scale))
		if err != nil {
			return [4][]currentDungeonAltarRewardEntry{}, dungeonCardRewardBundle{}, err
		}
		switch candidate {
		case currentDungeonAltarRewardCandidateGold:
			amount := definition.scaleGold(currentDungeonCardGoldAmount(rng, goldReference, goldTerms))
			if amount <= 0 || amount > math.MaxUint32 || bundle.Gold > math.MaxInt64-amount {
				groups[0] = append(groups[0], currentDungeonAltarRewardEntry{ItemID: math.MaxUint32})
				continue
			}
			bundle.Gold += amount
			groups[0] = append(groups[0], currentDungeonAltarRewardEntry{Amount: uint32(amount)})
		case currentDungeonAltarRewardCandidateItem:
			item, found, itemErr := currentDungeonRandomCardItem(rng, itemCatalog, dungeonLevel, itemReference, now)
			if itemErr != nil {
				return [4][]currentDungeonAltarRewardEntry{}, dungeonCardRewardBundle{}, itemErr
			}
			if !found || item.ItemID <= 0 || item.ItemID > math.MaxUint32 || item.Count <= 0 || item.Count > math.MaxUint32 {
				groups[0] = append(groups[0], currentDungeonAltarRewardEntry{ItemID: math.MaxUint32})
				continue
			}
			bundle.Items = append(bundle.Items, item)
			groups[0] = append(groups[0], currentDungeonAltarRewardEntry{ItemID: uint32(item.ItemID), Amount: uint32(item.Count)})
		default:
			groups[0] = append(groups[0], currentDungeonAltarRewardEntry{ItemID: math.MaxUint32})
		}
	}
	if altar.Kind == worldmap.BloodAltarDungeonKindUltimate {
		point := definition.ultimatePoint(altar.completedDifficulties)
		if itemID, found := definition.ultimateRewardItem(point, rng.Next(100)); found {
			item, itemErr := currentDungeonCardItemReward(rng, itemCatalog, itemID, now)
			if itemErr != nil {
				return [4][]currentDungeonAltarRewardEntry{}, dungeonCardRewardBundle{}, itemErr
			}
			bundle.Items = append(bundle.Items, item)
			groups[0] = append(groups[0], currentDungeonAltarRewardEntry{ItemID: itemID, Amount: uint32(item.Count)})
		}
	}
	return groups, bundle, nil
}

func (s *Service) showCurrentDungeonAltarRewardLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	settlement *currentDungeonAltarSettlementRuntime,
) error {
	if settlement.phase != currentDungeonAltarSettlementRankingShown {
		return nil
	}
	body, err := buildCurrentDungeonAltarRewardBody(
		byte(min(uint32(math.MaxUint8), settlement.plan.CurrentRound)),
		byte(min(uint32(math.MaxUint8), settlement.plan.MaxRound)),
		settlement.plan.RewardGroups,
	)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonAltarRewardMsgID, body, 0); err != nil {
		return err
	}
	settlement.phase = currentDungeonAltarSettlementRewardShown
	s.logGameEvent(session, "game-dungeon-altar-reward-sent",
		"dungeon_id", runtime.Dungeon.ID,
		"cards", len(settlement.plan.RewardGroups[0]),
		"gold", settlement.plan.RewardBundle.Gold,
		"items", len(settlement.plan.RewardBundle.Items),
		"body_source", "current_exe_sub_1D3A770_four_independent_counted_groups")
	return s.commitCurrentDungeonAltarRewardLocked(session, runtime, settlement)
}

func (s *Service) commitCurrentDungeonAltarRewardLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	settlement *currentDungeonAltarSettlementRuntime,
) error {
	if settlement.phase != currentDungeonAltarSettlementRewardShown &&
		settlement.phase != currentDungeonAltarSettlementRewardCommitted {
		return nil
	}
	if !settlement.rewardCommitted {
		reservation, _, err := settlement.rewardState.reserveSelection(dungeonCardSideFree, 0)
		if err != nil {
			return err
		}
		if reservation.grant {
			completionKey, keyErr := currentDungeonAltarCardRewardCompletionKey(runtime, settlement.rewardState)
			if keyErr != nil {
				return keyErr
			}
			grant, grantErr := s.deliverCurrentDungeonAltarRewardReservationWithCompletionKey(
				session,
				settlement.rewardState,
				reservation,
				completionKey,
			)
			if grantErr != nil {
				return grantErr
			}
			settlement.rewardCommit = grant
		}
		settlement.rewardCommitted = true
		settlement.phase = currentDungeonAltarSettlementRewardCommitted
	}
	return s.projectCurrentDungeonAltarCommittedSettlementLocked(session, runtime, settlement)
}

// deliverCurrentDungeonAltarRewardReservation deliberately obtains the bag
// capacity from the selected character's persisted op13 container state. A
// zero literal here would make rewards without a PVF slot range invalid (or
// route them away from the real main bag) and violates the card asset owner
// contract.
func (s *Service) deliverCurrentDungeonAltarRewardReservation(
	session *gameSession,
	state *dungeonCardState,
	reservation dungeonCardDeliveryReservation,
) (dungeonCardAssetGrantResult, error) {
	return s.deliverCurrentDungeonAltarRewardReservationWithCompletionKey(session, state, reservation, "")
}

func (s *Service) deliverCurrentDungeonAltarRewardReservationWithCompletionKey(
	session *gameSession,
	state *dungeonCardState,
	reservation dungeonCardDeliveryReservation,
	completionKey string,
) (dungeonCardAssetGrantResult, error) {
	if s == nil || session == nil || state == nil || !reservation.grant {
		return dungeonCardAssetGrantResult{}, nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	if state.plan.CharacterID != characterID {
		state.finishDelivery(reservation, false)
		return dungeonCardAssetGrantResult{}, errCurrentDungeonAltarSettlementInvalid
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil || repositories.Settings == nil {
		state.finishDelivery(reservation, false)
		return dungeonCardAssetGrantResult{}, errDungeonCardAssetOwnerUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	containerState := s.loadCurrentItemListContainerState(
		ctx,
		repositories.Settings,
		characterID,
		dnfrepo.MainInventoryListType,
	)
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), createWriteTimeout)
	grant, err := deliverDungeonCardReservationWithCompletionKey(
		ctx,
		state,
		reservation,
		repositories,
		containerState.MainSlotCount,
		s.gameplayNow().UTC(),
		completionKey,
	)
	cancel()
	return grant, err
}

func currentDungeonAltarCardRewardCompletionKey(
	runtime *runtimeDungeonState,
	state *dungeonCardState,
) (string, error) {
	if runtime == nil || runtime.altar == nil || state == nil ||
		strings.TrimSpace(runtime.clearMapCompletionKey) == "" || strings.TrimSpace(state.plan.ID) == "" {
		return "", errCurrentDungeonAltarSettlementInvalid
	}
	return runtime.clearMapCompletionKey + ":altar-card-plan:" + state.plan.ID, nil
}

func (s *Service) projectCurrentDungeonAltarCommittedSettlementLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	settlement *currentDungeonAltarSettlementRuntime,
) error {
	if settlement == nil || !settlement.rewardCommitted || settlement.phase < currentDungeonAltarSettlementRewardCommitted {
		return errCurrentDungeonAltarSettlementInvalid
	}
	if !settlement.characterStateSent {
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketFinishLoading), settlement.plan.CharacterStateBody, 0); err != nil {
			return err
		}
		settlement.characterStateSent = true
	}
	if !settlement.goldStateSent {
		if settlement.rewardCommit.GoldAfter != settlement.rewardCommit.GoldBefore {
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(settlement.rewardCommit.GoldAfter), 0); err != nil {
				return err
			}
		}
		settlement.goldStateSent = true
	}
	if !settlement.inventoryStateSent {
		refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(settlement.rewardCommit.ItemSlots))
		for _, slot := range settlement.rewardCommit.ItemSlots {
			if slot >= 0 {
				refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: slot})
			}
		}
		if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "blood-altar-reward", refreshes); err != nil {
			return err
		}
		settlement.inventoryStateSent = true
	}
	if !settlement.mailboxAlarmSent {
		if settlement.rewardCommit.OverflowMailID != "" {
			if err := s.sendMailboxAlarmToOnlineRecipient(session.selectedCharacterID); err != nil {
				s.logWarn("blood altar reward mailbox alarm failed", "character_id", session.selectedCharacterID, "error", err)
			}
		}
		// Mailbox alarms are a best-effort projection. Mark the attempt even on a
		// transport failure so an op37/inventory retry cannot spam the recipient.
		settlement.mailboxAlarmSent = true
	}
	if settlement.phase == currentDungeonAltarSettlementRewardCommitted {
		// IDA proves the current dungeon handler table has no altar op260. The
		// client returns through its already verified op72 {state,option}
		// boundary after the dedicated op208/op209 reward projection is complete.
		settlement.phase = currentDungeonAltarSettlementExitReady
	}
	s.logGameEvent(session, "game-dungeon-altar-settlement-committed",
		"dungeon_id", runtime.Dungeon.ID,
		"reward_exp", settlement.plan.RewardExperience,
		"gold_before", settlement.rewardCommit.GoldBefore,
		"gold_after", settlement.rewardCommit.GoldAfter,
		"item_slots", settlement.rewardCommit.ItemSlots,
		"reward_receipt_replayed", settlement.rewardCommit.Replayed,
		"return_boundary", "current_exe_op72_cardless_after_altar_settlement",
		"unregistered_op260_skipped", true,
		"ordinary_op31_op46_op35_skipped", true)
	return nil
}

func completedCurrentDungeonAltarSettlementExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	return runtime != nil && runtime.altar != nil && runtime.altar.settlement != nil &&
		runtime.altar.settlement.phase == currentDungeonAltarSettlementExitReady &&
		runtime.altar.settlement.rewardCommitted &&
		runtime.altar.settlement.characterStateSent &&
		runtime.altar.settlement.goldStateSent &&
		runtime.altar.settlement.inventoryStateSent &&
		runtime.altar.settlement.mailboxAlarmSent && runtime.Session != nil &&
		dungeonRuntimeOwnsCharacter(runtime, characterID) &&
		runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunCompleted
}

func pendingCurrentDungeonAltarSettlementCommitRetry(runtime *runtimeDungeonState, characterID uint16) bool {
	if runtime == nil || runtime.altar == nil || runtime.altar.settlement == nil || runtime.Session == nil ||
		!dungeonRuntimeOwnsCharacter(runtime, characterID) ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		return false
	}
	phase := runtime.altar.settlement.phase
	return phase == currentDungeonAltarSettlementRewardShown ||
		phase == currentDungeonAltarSettlementRewardCommitted
}
