package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdrops "longheng.io/server/internal/modules/dnf/drop"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var currentDungeonCardGoldDifficultyBonus = [...]float64{1.02, 1.38, 1.60, 1.90, 2.0}

const currentDungeonGoldCardItemMultiplier int64 = 2

func (s *Service) freezeCurrentDungeonCardRewardStateLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if s == nil || session == nil || runtime == nil || session.dungeon.runtime != runtime {
		return errDungeonCardAssetOwnerUnavailable
	}
	if currentDungeonTowerRewardMsgID(runtime) != 0 || runtime.settlementCardRewardState != nil {
		return nil
	}
	monsterCatalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return err
	}
	goldReferences, err := currentDungeonGoldReferences(monsterCatalog)
	if err != nil {
		return err
	}
	if !runtime.Dungeon.Metadata.BasisLevel.Set || runtime.Dungeon.Metadata.BasisLevel.Value <= 0 {
		return errCurrentDungeonSettlementPVFInvalid
	}
	dungeonLevel := int(runtime.Dungeon.Metadata.BasisLevel.Value)
	goldReference, found := goldReferences[int64(dungeonLevel)]
	if !found {
		return fmt.Errorf("%w: dungeon_level=%d", errCurrentDungeonGoldReferenceInvalid, runtime.Dungeon.Metadata.BasisLevel.Value)
	}
	itemCatalog, err := monsterCatalog.DropCatalog()
	if err != nil {
		return err
	}
	clearRewardRules, err := currentDungeonClearRewardRules(monsterCatalog)
	if err != nil {
		return err
	}
	partyMembers := 1
	if runtime.partyDungeonOwner != nil {
		partyMembers = len(runtime.partyDungeonOwner.roster())
	}
	visitedRooms := len(runtime.Rooms)
	clearRewardRule, err := clearRewardRules.Resolve(
		int64(dungeonLevel),
		int(runtime.Request.Difficulty),
		partyMembers,
		visitedRooms,
	)
	if err != nil {
		return err
	}
	// Freeze the commission beside the plan it prices. Resolve already proves
	// [gold card cost table] covers this level, so the value is the same one the
	// 86JP reference hands the client in op35's PAID-CARD COMMISSION field.
	runtime.settlementPaidCardCost = clearRewardRule.PaidCardCost
	goldTerms := currentDungeonCardGoldTerms{
		Difficulty:    int(runtime.Request.Difficulty),
		PartyMembers:  partyMembers,
		PartyRate:     clearRewardRules.PartyMemberRate(partyMembers),
		ClearRankRate: s.currentDungeonClearRankGoldRate(runtime),
		WeightedKills: runtime.currentDungeonClearWeightedKills(),
	}
	dimensionCatalog, err := currentDungeonDimensionDrops(monsterCatalog)
	if err != nil {
		return err
	}
	dimensionChronicleActive := false
	characterJob := byte(0)
	characterGrowType := byte(runtime.Character.Stats["grow_type"]) & 0x0f
	if _, dimensionMatched := currentDungeonDimensionTier(
		dimensionCatalog,
		runtime.Dungeon.ID,
		runtime.Character.Level,
	); dimensionMatched && len(dimensionCatalog.ChronicleByCareer) > 0 {
		jobValue, parseErr := strconv.ParseUint(strings.TrimSpace(runtime.Character.Job), 10, 8)
		if parseErr != nil {
			return fmt.Errorf("%w: dimension character job=%q", errCurrentDungeonDimensionDropInvalid, runtime.Character.Job)
		}
		characterJob = byte(jobValue)
		dimensionChronicleActive = true
	}

	rng := newCurrentDungeonDropLCG(runtime.Seed)
	var visit *runtimeDungeonRoomVisit
	if runtime.Session != nil {
		if scene, ok := runtime.Session.Scene(); ok {
			if ownedVisit, visitErr := runtime.currentDungeonRoomVisit(scene); visitErr == nil {
				visit = ownedVisit
				rng = newCurrentDungeonDropLCG(ownedVisit.DropRNG)
			}
		}
	}
	now := s.gameplayNow().UTC()
	randomReward := func() (dungeonCardRewardBundle, error) {
		if dimensionChronicleActive {
			return currentDungeonRandomDimensionCardRewardBundle(
				rng,
				goldReference,
				goldTerms,
				itemCatalog,
				dimensionCatalog,
				runtime.Dungeon.ID,
				runtime.Character.Level,
				characterJob,
				characterGrowType,
				now,
			)
		}
		return currentDungeonRandomCardRewardBundle(
			rng,
			goldReference,
			goldTerms,
			itemCatalog,
			dungeonLevel,
			clearRewardRule,
			now,
		)
	}
	free, err := randomReward()
	if err != nil {
		return err
	}
	paid, err := randomReward()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	_, account, character, premiumErr := s.currentPremiumServiceRecords(ctx, session)
	cancel()
	var blackDiamond [dungeonCardSlotsPerSide]dungeonCardRewardBundle
	blackDiamondActive := s.currentBlackDiamondActive(session)
	if blackDiamondActive {
		for member := range blackDiamond {
			blackDiamond[member], err = randomReward()
			if err != nil {
				return err
			}
		}
	}
	waivePaidCardCost := false
	if premiumErr == nil &&
		premium.Active(account, premium.DevilSlotType(premium.DevilSlotGoldCard), now) &&
		premium.DailyUsage(character, premium.DevilSlotGoldCard, now) < premium.DailyLimit(premium.DevilSlotGoldCard) {
		paid.ConsumePremiumDaily = true
		paid.PremiumDailySlot = premium.DevilSlotGoldCard
		// The purchased 魔王契约 黄金卡牌免费翻 service pays the commission for
		// this flip. 86JP waives the same charge through
		// DevilContractBenefitPolicy.GoldCardSlot, whose daily limit of 10
		// matches DailyLimit(DevilSlotGoldCard) above.
		waivePaidCardCost = true
		if err := applyCurrentDungeonGoldCardItemMultiplier(&paid); err != nil {
			return err
		}
	}
	if visit != nil {
		visit.DropRNG = rng.Seed()
	}
	plan, err := newDungeonCardRewardPlanWithBlackDiamond(
		dungeonCardPlanIdentity{
			CharacterID: strconv.Itoa(int(session.selectedCharacterID)),
			DungeonID:   runtime.Dungeon.ID,
			MazeIndex:   runtime.MazeIndex,
			RunSeed:     runtime.Seed,
		},
		"current_pvf_clear_reward_generator_free_and_paid_rows",
		free,
		paid,
		blackDiamond,
	)
	if err != nil {
		return err
	}
	// The commission printed under 黄金卡牌 N is the same number spent when that
	// card is flipped, so it is frozen beside the plan: the client is told the
	// list price whether or not the contract pays it, and a replay cannot charge
	// a different amount than it displayed.
	plan.PaidCardCost = clearRewardRule.PaidCardCost
	if waivePaidCardCost {
		plan.PaidCardCost = 0
	}
	state, err := newDungeonCardState(plan)
	if err != nil {
		return err
	}
	runtime.settlementCardRewardState = state
	s.logGameEvent(session, "game-dungeon-card-reward-plan-frozen",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"plan_id", plan.ID,
		"free_gold", free.Gold,
		"free_item_count", len(free.Items),
		"paid_gold", paid.Gold,
		"paid_item_count", len(paid.Items),
		"black_diamond_active", blackDiamondActive,
		"black_diamond_reward_seats", currentDungeonCardNonEmptyBundleCount(blackDiamond[:]),
		"premium_gold_card_available", paid.ConsumePremiumDaily,
		"source", plan.Source,
		"dimension_chronicle_active", dimensionChronicleActive,
		"dimension_chronicle_job", characterJob,
		"dimension_chronicle_grow_type", characterGrowType,
		"clear_reward_equipment_rate", clearRewardRule.EquipmentRate,
		"clear_reward_map_count", clearRewardRule.MapCount,
		"clear_reward_party_members", partyMembers,
		"domain_source", "current_pvf_clear_reward_equipment_rule")
	return nil
}

func currentDungeonRandomCardRewardBundle(
	rng *currentDungeonDropLCG,
	goldReference currentDungeonGoldReference,
	goldTerms currentDungeonCardGoldTerms,
	itemCatalog *pvfDungeonDropCatalog,
	dungeonLevel int,
	clearRewardRule dnfdrops.ClearRewardRule,
	now time.Time,
) (dungeonCardRewardBundle, error) {
	bundle := dungeonCardRewardBundle{
		Gold: currentDungeonCardGoldAmount(rng, goldReference, goldTerms),
	}
	item, found, err := currentDungeonRandomClearCardEquipment(
		rng,
		itemCatalog,
		dungeonLevel,
		clearRewardRule,
		now,
	)
	if err != nil {
		return dungeonCardRewardBundle{}, err
	}
	if found {
		bundle.Items = []dungeonCardItemReward{item}
	}
	return bundle, nil
}

func currentDungeonRandomClearCardEquipment(
	rng *currentDungeonDropLCG,
	catalog *pvfDungeonDropCatalog,
	dungeonLevel int,
	rule dnfdrops.ClearRewardRule,
	now time.Time,
) (dungeonCardItemReward, bool, error) {
	if rng == nil || catalog == nil || dungeonLevel <= 0 || rule.RarityScale == 0 {
		return dungeonCardItemReward{}, false, errDungeonCardPVFSourceRequired
	}
	if uint32(rng.Next(int(dnfdrops.ClearRewardItemRateDenominator))) >= rule.EquipmentRate {
		return dungeonCardItemReward{}, false, nil
	}
	rarityRoll := uint32(rng.Next(int(rule.RarityScale)))
	rarity := currentDungeonClearCardEquipmentRarity(rule.RarityThresholds, rarityRoll)
	itemID, found, err := catalog.SelectGenericEquipment(
		rng,
		dungeonLevel,
		rule.GradeDown,
		rule.GradeUp,
		int64(rarity),
	)
	if err != nil {
		return dungeonCardItemReward{}, false, err
	}
	if !found || itemID == 0 {
		// The native clear-card pool is equipment-only. A missing equipment
		// candidate leaves this card gold-only; it must never fall through to
		// the generic stackable pool (bombs, discs, boomerangs, and materials).
		return dungeonCardItemReward{}, false, nil
	}
	reward, err := currentDungeonCardItemReward(rng, catalog, itemID, now)
	return reward, err == nil, err
}

func currentDungeonClearCardEquipmentRarity(thresholds [7]uint32, roll uint32) int {
	for rarity, threshold := range thresholds {
		if roll < threshold {
			return rarity
		}
	}
	return len(thresholds) - 1
}

func currentDungeonRandomDimensionCardRewardBundle(
	rng *currentDungeonDropLCG,
	goldReference currentDungeonGoldReference,
	goldTerms currentDungeonCardGoldTerms,
	itemCatalog *pvfDungeonDropCatalog,
	dimensionCatalog *currentDungeonDimensionDropCatalog,
	dungeonID int64,
	characterLevel int,
	job byte,
	growType byte,
	now time.Time,
) (dungeonCardRewardBundle, error) {
	bundle := dungeonCardRewardBundle{
		Gold: currentDungeonCardGoldAmount(rng, goldReference, goldTerms),
	}
	_, itemID, _, matched, selected, err := currentDungeonDimensionChronicleItem(
		dimensionCatalog,
		dungeonID,
		characterLevel,
		job,
		growType,
		rng,
		true,
	)
	if err != nil {
		return dungeonCardRewardBundle{}, err
	}
	if !matched {
		return dungeonCardRewardBundle{}, fmt.Errorf(
			"%w: dimension clear reward dungeon_id=%d",
			errCurrentDungeonDimensionDropInvalid,
			dungeonID,
		)
	}
	if selected {
		item, resolveErr := currentDungeonCardItemReward(rng, itemCatalog, itemID, now)
		if resolveErr != nil {
			return dungeonCardRewardBundle{}, resolveErr
		}
		bundle.Items = []dungeonCardItemReward{item}
	} else {
		bundle.Gold += 100
	}
	return bundle, nil
}

func currentDungeonCardNonEmptyBundleCount(bundles []dungeonCardRewardBundle) int {
	count := 0
	for _, bundle := range bundles {
		if bundle.Gold > 0 || len(bundle.Items) > 0 {
			count++
		}
	}
	return count
}

// applyCurrentDungeonGoldCardItemMultiplier turns the current EXE's visible
// x2 Gold Card result into two durable item units. Stackables stay one reward
// row with a doubled count. Equipment is cloned into independent count-one
// instances because equipment must never be stored as a stack.
func applyCurrentDungeonGoldCardItemMultiplier(bundle *dungeonCardRewardBundle) error {
	if bundle == nil || len(bundle.Items) == 0 {
		return nil
	}
	items := make([]dungeonCardItemReward, 0, len(bundle.Items)*int(currentDungeonGoldCardItemMultiplier))
	for _, item := range bundle.Items {
		if item.ItemID <= 0 || item.Count <= 0 ||
			item.Count > math.MaxInt64/currentDungeonGoldCardItemMultiplier {
			return fmt.Errorf(
				"%w: gold-card item=%d count=%d",
				errDungeonCardPlanInvalid,
				item.ItemID,
				item.Count,
			)
		}
		if item.Stackable {
			item.Count *= currentDungeonGoldCardItemMultiplier
			items = append(items, cloneDungeonCardItemReward(item))
			continue
		}
		if item.Count != 1 {
			return fmt.Errorf(
				"%w: gold-card equipment=%d count=%d",
				errDungeonCardPlanInvalid,
				item.ItemID,
				item.Count,
			)
		}
		for range currentDungeonGoldCardItemMultiplier {
			items = append(items, cloneDungeonCardItemReward(item))
		}
	}
	bundle.Items = items
	return nil
}

func currentDungeonRandomCardItem(
	rng *currentDungeonDropLCG,
	catalog *pvfDungeonDropCatalog,
	dungeonLevel int,
	reference dungeonCardPVFItemDropReference,
	now time.Time,
) (dungeonCardItemReward, bool, error) {
	if rng == nil || catalog == nil || dungeonLevel <= 0 {
		return dungeonCardItemReward{}, false, errDungeonCardPVFSourceRequired
	}
	rarityRoll := rng.Next(1_000_000) + 1
	rarity := int64(0)
	switch {
	case rarityRoll <= 500_000:
		rarity = 0
	case rarityRoll <= 799_900:
		rarity = 1
	case rarityRoll <= 999_900:
		rarity = 2
	default:
		rarity = 3
	}
	itemID, found, err := catalog.SelectGenericEquipment(
		rng,
		dungeonLevel,
		reference.ValueA,
		reference.ValueB,
		rarity,
	)
	if err != nil {
		return dungeonCardItemReward{}, false, err
	}
	if !found {
		itemID, found, err = catalog.SelectGenericStackable(
			rng,
			dungeonLevel,
			reference.ValueA,
			reference.ValueB,
			rarity,
		)
		if err != nil {
			return dungeonCardItemReward{}, false, err
		}
	}
	if !found || itemID == 0 {
		return dungeonCardItemReward{}, false, nil
	}
	reward, err := currentDungeonCardItemReward(rng, catalog, itemID, now)
	return reward, err == nil, err
}

func currentDungeonCardItemReward(
	rng *currentDungeonDropLCG,
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
	now time.Time,
) (dungeonCardItemReward, error) {
	if rng == nil || catalog == nil || itemID == 0 {
		return dungeonCardItemReward{}, errDungeonCardPVFSourceRequired
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return dungeonCardItemReward{}, err
	}
	definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
	if err != nil {
		return dungeonCardItemReward{}, err
	}
	extra := map[string]string{
		"source":    "dungeon_card_reward",
		"item_kind": string(definition.Kind),
		"pvf_path":  definition.PVFPath,
	}
	if definition.StackableType != "" {
		extra["stackable_type"] = definition.StackableType
	}
	if definition.StackLimit > 0 {
		extra["stack_limit"] = strconv.FormatInt(definition.StackLimit, 10)
	}
	if definition.EquipmentType != "" {
		extra["equipment_type"] = definition.EquipmentType
	}
	if definition.Durability > 0 {
		extra["durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
		extra["max_durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
	}
	if definition.Kind == dungeonDropItemEquipment {
		qualitySeed := rng.NextUint32()%currentEquipmentRandomQualitySeedCount + 1
		extra["quality_seed"] = strconv.FormatUint(uint64(qualitySeed), 10)
	}
	return dungeonCardItemReward{
		ItemID:     int64(itemID),
		Count:      1,
		Stackable:  definition.Kind == dungeonDropItemStackable,
		StackLimit: definition.StackLimit,
		SlotStart:  definition.SlotStart,
		SlotEnd:    definition.SlotEnd,
		ExpireAt:   definition.ExpirationDate,
		Extra:      extra,
	}, nil
}

func (s *Service) selectCurrentDungeonCardLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	side dungeonCardSide,
	memberIndex byte,
	source string,
	clickedSlot ...byte,
) error {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime ||
		!runtime.settlementCardLayoutSent ||
		runtime.settlementPhase < currentDungeonSettlementPhaseCardsRevealed ||
		side >= dungeonCardSideCount || memberIndex >= dungeonCardSlotsPerSide {
		return nil
	}
	if runtime.partyDungeonOwner != nil {
		member, current := runtime.partyDungeonOwner.member(
			session.selectedCharacterID,
			session.characterGeneration,
		)
		if !current || !runtime.partyMemberIndexed || runtime.partyMemberIndex != member.slot || memberIndex != member.slot {
			s.logGameEvent(session, "game-party-dungeon-card-select-blocked",
				"char_id", session.selectedCharacterID,
				"party_id", runtime.partyDungeonOwner.partyID,
				"reported_member_index", memberIndex,
				"stable_member_index", member.slot,
				"source", source,
				"reason", "same_version_card_result_must_use_native_party_slot")
			return nil
		}
	}
	if runtime.settlementCardSideSelectionKnown[side] &&
		runtime.settlementCardSideMember[side] != memberIndex {
		s.logGameEvent(session, "game-dungeon-card-select-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"side", side,
			"member_index", memberIndex,
			"accepted_member_index", runtime.settlementCardSideMember[side],
			"source", source,
			"reason", "row_selection_conflicts_with_first_accepted_member")
		return nil
	}
	if runtime.settlementCardRewardState == nil {
		if err := s.freezeCurrentDungeonCardRewardStateLocked(session, runtime); err != nil {
			s.logGameEvent(session, "game-dungeon-card-reward-plan-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"side", side,
				"member_index", memberIndex,
				"source", source,
				"error", err)
			return nil
		}
	}
	replay := runtime.settlementCardSideSelectionSent[side]
	if !runtime.settlementCardSidePositionKnown[side] {
		position := memberIndex
		if len(clickedSlot) != 0 {
			position = clickedSlot[0]
		}
		if position >= dungeonCardSlotsPerSide {
			return errDungeonCardSelectionInvalid
		}
		if owner := runtime.partyDungeonOwner; owner != nil {
			owner.mu.Lock()
			if owner.cardScreenOwners[side][position] != 0 && owner.cardScreenOwners[side][position] != session.selectedCharacterID {
				if len(clickedSlot) != 0 {
					owner.mu.Unlock()
					// The DLL restores only this pending choice; no reward is
					// committed for two members racing for the same card.
					return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSelectCard),
						[]byte{0, 254, byte(side), position}, currentDungeonCardResponseClass)
				}
				// Timed auto-flips and old two-byte clients take an empty card.
				position = dungeonCardSlotsPerSide
				for candidate, occupant := range owner.cardScreenOwners[side] {
					if occupant == 0 {
						position = byte(candidate)
						break
					}
				}
				if position >= dungeonCardSlotsPerSide {
					owner.mu.Unlock()
					return errDungeonCardSelectionInvalid
				}
			}
			owner.cardScreenOwners[side][position] = session.selectedCharacterID
			owner.mu.Unlock()
		}
		runtime.settlementCardSidePosition[side] = position
		runtime.settlementCardSidePositionKnown[side] = true
	}
	runtime.settlementCardSideSelectionKnown[side] = true
	runtime.settlementCardSideMember[side] = memberIndex
	runtime.settlementCardSelected = byte(side)
	runtime.refreshCurrentDungeonFreeCardReadiness()
	slots71, rewardTupleCount := currentDungeonSelectedCardSlots(runtime)
	body71, err := buildCurrentDungeonOp71SuccessBody(slots71)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSelectCard), body71, currentDungeonCardResponseClass); err != nil {
		return err
	}
	runtime.settlementCardSideSelectionSent[side] = true
	runtime.refreshCurrentDungeonFreeCardReadiness()
	if side == dungeonCardSideFree {
		s.cancelCurrentDungeonCardAutoFlipLocked(session, runtime, "free_row_selected")
	}
	s.logGameEvent(session, "game-dungeon-card-select-ack-sent",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"side", side,
		"member_index", memberIndex,
		"screen_slot", runtime.settlementCardSidePosition[side],
		"replay", replay,
		"response_body_len", len(body71),
		"classification", currentDungeonCardResponseClass,
		"source", source,
		"reward_tuple_count", rewardTupleCount,
		"body_source", "current_exe_op71_proved_reader_selection_state_with_frozen_card_reward_context")

	if err := s.commitCurrentDungeonCardSideRewardLocked(session, runtime, side, source); err != nil {
		s.logGameEvent(session, "game-dungeon-card-reward-delivery-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"side", side,
			"member_index", memberIndex,
			"source", source,
			"error", err)
	}
	if err := s.publishRuntimePartyDungeonCardSelection(
		session,
		runtime,
		side,
		memberIndex,
		body71,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) commitCurrentDungeonCardRewardLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if runtime == nil || runtime.settlementCardRewardCommitted {
		return nil
	}
	return s.commitCurrentDungeonCardSideRewardLocked(session, runtime, dungeonCardSideFree, source)
}

func (s *Service) commitCurrentDungeonCardSideRewardLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	side dungeonCardSide,
	source string,
) error {
	if runtime == nil || side >= dungeonCardSideCount ||
		runtime.settlementCardSideRewardCommitted[side] {
		return nil
	}
	if !runtime.settlementCardSideSelectionKnown[side] ||
		!runtime.settlementCardSideSelectionSent[side] {
		return errDungeonCardSelectionInvalid
	}
	if err := s.deliverCurrentDungeonSelectedCardRewardLocked(
		session,
		runtime,
		side,
		runtime.settlementCardSideMember[side],
		source,
	); err != nil {
		return err
	}
	runtime.settlementCardSideRewardCommitted[side] = true
	runtime.refreshCurrentDungeonFreeCardReadiness()
	if side == dungeonCardSideFree {
		runtime.advanceSettlementPhase(currentDungeonSettlementPhaseRewardCommitted)
	}
	s.logGameEvent(session, "game-dungeon-card-reward-phase-committed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"side", side,
		"member_index", runtime.settlementCardSideMember[side],
		"source", source,
		"next_stage", "await_current_exe_c2s_op72_or_completed_op42")
	return nil
}

func (s *Service) deliverCurrentDungeonSelectedCardRewardLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	side dungeonCardSide,
	memberIndex byte,
	source string,
) error {
	if s == nil || session == nil || runtime == nil || runtime.settlementCardRewardState == nil {
		return nil
	}
	reservation, result, err := runtime.settlementCardRewardState.reserveSelection(side, memberIndex)
	if err != nil {
		return err
	}
	if !reservation.grant {
		s.logGameEvent(session, "game-dungeon-card-reward-delivery-skipped",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"selection_result", result,
			"side", side,
			"member_index", memberIndex,
			"source", source,
			"reason", "no_undelivered_reward_for_selection")
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		runtime.settlementCardRewardState.finishDelivery(reservation, false)
		return errDungeonCardAssetOwnerUnavailable
	}
	// 翻牌落点必须用该角色**真实已购**的背包扩展行数。这里过去写死 0，等于
	// "只认默认 4 行"：`currentMainInventoryClampedSlotEnd` 会把每个 56 格分类的
	// SlotEnd 夹到 `SlotStart+31`，于是买过扩展券的角色在前 4 行占满、扩展行空着
	// 的时候也会被判成"背包已满"，整份翻牌奖励转投邮件（现场：装备 9-64 只用
	// 9-40、材料 65-120 只用 65-96）。取不到容器状态时安全退回 0，与
	// 祭坛/赛丽亚商店同一条口径（见 altar_settlement.go 的 owner 契约注释）。
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	loadCtx, loadCancel := context.WithTimeout(context.Background(), createWriteTimeout)
	mainSlotCount := s.currentInventoryPlacementMainSlotCount(loadCtx, nil, characterID)
	loadCancel()
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	grant, err := deliverDungeonCardReservation(
		ctx,
		runtime.settlementCardRewardState,
		reservation,
		repositories,
		mainSlotCount,
		s.gameplayNow().UTC(),
	)
	if err != nil {
		return err
	}
	if grant.GoldAfter != grant.GoldBefore {
		body := buildCurrentGoldStateBody(grant.GoldAfter)
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(grant.ItemSlots))
	for _, slot := range grant.ItemSlots {
		if slot < 0 {
			continue
		}
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: slot,
		})
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(
		session,
		"dungeon-card-reward",
		refreshes,
	); err != nil {
		return err
	}
	if grant.OverflowMailID != "" {
		if err := s.sendMailboxAlarmToOnlineRecipient(session.selectedCharacterID); err != nil {
			// Reward and its mailbox fallback have already committed. A packet
			// delivery failure must not turn that durable result into a retryable
			// card selection; op96 will project it on the next mailbox open.
			s.logWarn("dungeon reward mailbox alarm deferred to next mailbox open",
				"character_id", session.selectedCharacterID,
				"mail_id", grant.OverflowMailID,
				"error", err)
		}
	}
	s.logGameEvent(session, "game-dungeon-card-reward-delivered",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"selection_result", result,
		"side", side,
		"member_index", memberIndex,
		"gold_before", grant.GoldBefore,
		"gold_after", grant.GoldAfter,
		"item_slots", grant.ItemSlots,
		"overflow_mail_id", grant.OverflowMailID,
		"source", source,
		"domain_source", "86jp_auto_flip_reward_delivery")
	return nil
}

func (runtime *runtimeDungeonState) refreshCurrentDungeonFreeCardReadiness() {
	if runtime == nil {
		return
	}
	runtime.settlementCardSelectionKnown = runtime.settlementCardSideSelectionKnown[dungeonCardSideFree]
	runtime.settlementCardSelectionSent = runtime.settlementCardSideSelectionSent[dungeonCardSideFree]
	runtime.settlementCardRewardCommitted = runtime.settlementCardSideRewardCommitted[dungeonCardSideFree]
}
