package dnfbridge

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentMonsterCardSynthesisPVFPath   = "event/chn_event/chn_composecard.evt"
	currentMonsterCardSynthesisOddsScale = 100000
)

var errCurrentMonsterCardSynthesisUnavailable = errors.New("dnf monster card synthesis PVF is unavailable")

type currentMonsterCardSynthesisWeightedItem struct {
	ItemID uint32
	Weight int64
}

type currentMonsterCardSynthesisConfig struct {
	MixProbability map[int]int64
	MixFee         map[int]int64
	BinderRates    map[int]int64
	InputRarity    map[uint32]int
	Results        map[int][]currentMonsterCardSynthesisWeightedItem
}

func (s *Service) alignedMonsterCardSynthesisPlannerForCommand(
	opcode dnfenum.CmdPacket,
) (alignedcmd.MonsterCardSynthesisPlanner, error) {
	if opcode != dnfenum.CmdPacketMonstercardBind {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, errDungeonDropSourceRequired
	}
	s.initialEquipmentMu.Lock()
	source, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	config, err := loadCurrentMonsterCardSynthesisConfig(source)
	if err != nil {
		return nil, err
	}
	return func(
		binderType int,
		binder dnfrepo.ItemStack,
		cards []dnfrepo.ItemStack,
	) (alignedcmd.MonsterCardSynthesisPlan, error) {
		if currentNPCShopItemLocked(binder) {
			return alignedcmd.MonsterCardSynthesisPlan{}, fmt.Errorf(
				"%w: one or more synthesis source items are locked",
				errCurrentMonsterCardSynthesisUnavailable,
			)
		}
		for _, card := range cards {
			if currentNPCShopItemLocked(card) {
				return alignedcmd.MonsterCardSynthesisPlan{}, fmt.Errorf(
					"%w: one or more synthesis source items are locked",
					errCurrentMonsterCardSynthesisUnavailable,
				)
			}
		}
		return planCurrentMonsterCardSynthesis(
			catalog,
			source,
			config,
			binderType,
			binder,
			cards,
			rand.Intn,
		)
	}, nil
}

func loadCurrentMonsterCardSynthesisConfig(source dnfpvf.Source) (currentMonsterCardSynthesisConfig, error) {
	config := currentMonsterCardSynthesisConfig{
		MixProbability: make(map[int]int64),
		MixFee:         make(map[int]int64),
		BinderRates:    make(map[int]int64),
		InputRarity:    make(map[uint32]int),
		Results:        make(map[int][]currentMonsterCardSynthesisWeightedItem),
	}
	if source == nil {
		return config, errDungeonDropSourceRequired
	}
	text, err := source.ReadText(currentMonsterCardSynthesisPVFPath)
	if err != nil {
		return config, fmt.Errorf("read %s: %w", currentMonsterCardSynthesisPVFPath, err)
	}
	document, err := dnfpvf.Parse(currentMonsterCardSynthesisPVFPath, text)
	if err != nil {
		return config, fmt.Errorf("parse %s: %w", currentMonsterCardSynthesisPVFPath, err)
	}

	probability := document.Ints("mix probability")
	if len(probability) == 0 || len(probability)%2 != 0 {
		return config, fmt.Errorf("%w: [mix probability] values=%d", errCurrentMonsterCardSynthesisUnavailable, len(probability))
	}
	for index := 0; index < len(probability); index += 2 {
		rarity, value := int(probability[index]), probability[index+1]
		if rarity < 0 || rarity > 6 || value < 0 || value > currentMonsterCardSynthesisOddsScale {
			return config, fmt.Errorf("%w: [mix probability] rarity=%d value=%d", errCurrentMonsterCardSynthesisUnavailable, rarity, value)
		}
		config.MixProbability[rarity] = value
	}

	fees := document.Ints("mix fee")
	if len(fees)%2 != 0 {
		return config, fmt.Errorf("%w: [mix fee] values=%d", errCurrentMonsterCardSynthesisUnavailable, len(fees))
	}
	for index := 0; index < len(fees); index += 2 {
		rarity, fee := int(fees[index]), fees[index+1]
		if rarity < 0 || rarity > 6 || fee < 0 {
			return config, fmt.Errorf("%w: [mix fee] rarity=%d fee=%d", errCurrentMonsterCardSynthesisUnavailable, rarity, fee)
		}
		config.MixFee[rarity] = fee
	}

	cards := document.Ints("card list")
	if len(cards) == 0 || len(cards)%2 != 0 {
		return config, fmt.Errorf("%w: [card list] values=%d", errCurrentMonsterCardSynthesisUnavailable, len(cards))
	}
	for index := 0; index < len(cards); index += 2 {
		rarity, itemID := int(cards[index]), cards[index+1]
		if rarity < 0 || rarity > 6 || itemID <= 0 || itemID > int64(^uint32(0)) {
			return config, fmt.Errorf("%w: [card list] rarity=%d item=%d", errCurrentMonsterCardSynthesisUnavailable, rarity, itemID)
		}
		config.InputRarity[uint32(itemID)] = rarity
	}

	results := document.Ints("mix result")
	if len(results) == 0 || len(results)%3 != 0 {
		return config, fmt.Errorf("%w: [mix result] values=%d", errCurrentMonsterCardSynthesisUnavailable, len(results))
	}
	for index := 0; index < len(results); index += 3 {
		rarity := int(results[index])
		itemID, weight := results[index+1], results[index+2]
		if rarity < 0 || rarity > 6 || itemID <= 0 || itemID > int64(^uint32(0)) || weight < 0 {
			return config, fmt.Errorf("%w: [mix result] row=%d", errCurrentMonsterCardSynthesisUnavailable, index/3)
		}
		if weight == 0 {
			continue
		}
		config.Results[rarity] = append(
			config.Results[rarity],
			currentMonsterCardSynthesisWeightedItem{ItemID: uint32(itemID), Weight: weight},
		)
	}
	for rarity := range config.MixProbability {
		if rarity > 0 && len(config.Results[rarity]) == 0 {
			return config, fmt.Errorf("%w: no [mix result] pool rarity=%d", errCurrentMonsterCardSynthesisUnavailable, rarity)
		}
	}

	enchanterText, err := source.ReadText("character/expertjob/enchanter.exj")
	if err != nil {
		return config, fmt.Errorf("read character/expertjob/enchanter.exj: %w", err)
	}
	enchanterDocument, err := dnfpvf.Parse("character/expertjob/enchanter.exj", enchanterText)
	if err != nil {
		return config, fmt.Errorf("parse character/expertjob/enchanter.exj: %w", err)
	}
	bindInfo := enchanterDocument.Ints("monstercard bind info")
	if len(bindInfo) == 0 || len(bindInfo)%2 != 0 {
		return config, fmt.Errorf("%w: [monstercard bind info] values=%d", errCurrentMonsterCardSynthesisUnavailable, len(bindInfo))
	}
	for index := 0; index < len(bindInfo); index += 2 {
		bindType, rate := int(bindInfo[index]), bindInfo[index+1]
		if bindType < 0 || bindType > 255 || rate < 0 {
			return config, fmt.Errorf("%w: [monstercard bind info] row=%d", errCurrentMonsterCardSynthesisUnavailable, index/2)
		}
		config.BinderRates[bindType] = rate
	}
	return config, nil
}

func planCurrentMonsterCardSynthesis(
	catalog *pvfDungeonDropCatalog,
	source dnfpvf.Source,
	config currentMonsterCardSynthesisConfig,
	requestBinderType int,
	binder dnfrepo.ItemStack,
	cards []dnfrepo.ItemStack,
	random func(int) int,
) (alignedcmd.MonsterCardSynthesisPlan, error) {
	plan := alignedcmd.MonsterCardSynthesisPlan{}
	if catalog == nil || source == nil || random == nil || len(cards) < 2 || len(cards) > 5 {
		return plan, errCurrentMonsterCardSynthesisUnavailable
	}
	binderDefinition, err := catalog.ResolveItem(uint32(binder.ItemID))
	if err != nil || binderDefinition.Kind != dungeonDropItemStackable ||
		!binderDefinition.MonsterCardBindOK ||
		!strings.HasPrefix(normalizeDungeonDropStackableType(binderDefinition.StackableType), "[expert town potion]") {
		return plan, fmt.Errorf("%w: binder item=%d", errCurrentMonsterCardSynthesisUnavailable, binder.ItemID)
	}
	if _, ok := config.Results[0]; !ok {
		return plan, fmt.Errorf("%w: rarity-zero result pool", errCurrentMonsterCardSynthesisUnavailable)
	}
	documentBinderType := int(binderDefinition.MonsterCardBind)
	if requestBinderType >= 0 && requestBinderType != 0xff &&
		requestBinderType != documentBinderType {
		return plan, fmt.Errorf(
			"%w: binder type request=%d document=%d",
			errCurrentMonsterCardSynthesisUnavailable,
			requestBinderType,
			documentBinderType,
		)
	}

	rarities := make([]int, len(cards))
	cardItemIDs := make([]int64, len(cards))
	baseRarity := 0
	for index, card := range cards {
		rarity, rarityErr := currentMonsterCardSynthesisInputRarity(catalog, source, config, card.ItemID)
		if rarityErr != nil {
			return plan, rarityErr
		}
		rarities[index] = rarity
		cardItemIDs[index] = card.ItemID
		if rarity > baseRarity {
			baseRarity = rarity
		}
	}
	bindType := documentBinderType
	if bindType < 0 || bindType > 2 {
		return plan, fmt.Errorf("%w: binder bind type=%d", errCurrentMonsterCardSynthesisUnavailable, bindType)
	}

	chance := int64(0)
	if len(rarities) == 2 {
		chance, err = currentMonsterCardSynthesisChance(config, bindType, rarities[0], rarities[1])
		if err != nil {
			return plan, err
		}
	} else {
		perPair, chanceErr := currentMonsterCardSynthesisChance(config, bindType, baseRarity, baseRarity)
		if chanceErr != nil {
			return plan, chanceErr
		}
		failure := int64(currentMonsterCardSynthesisOddsScale)
		for index := 1; index < len(rarities); index++ {
			failure = failure * (currentMonsterCardSynthesisOddsScale - perPair) /
				currentMonsterCardSynthesisOddsScale
		}
		chance = currentMonsterCardSynthesisOddsScale - failure
	}
	roll := 0
	upgradeRoll := chance >= currentMonsterCardSynthesisOddsScale
	if chance > 0 && chance < currentMonsterCardSynthesisOddsScale {
		rolled := random(currentMonsterCardSynthesisOddsScale)
		roll = rolled
		upgradeRoll = int64(rolled) < chance
	}
	resultRarity := baseRarity
	if len(rarities) == 2 {
		resultRarity = currentMonsterCardSynthesisResultRarity(rarities[0], rarities[1], upgradeRoll)
	} else if upgradeRoll {
		resultRarity = min(3, baseRarity+1)
	}
	outputID, err := currentMonsterCardSynthesisSelectOutput(config, resultRarity, random)
	if err != nil {
		return plan, err
	}
	outputDefinition, err := catalog.ResolveItem(outputID)
	if err != nil || outputDefinition.Kind != dungeonDropItemStackable ||
		outputDefinition.SlotStart < 0 || outputDefinition.SlotEnd < outputDefinition.SlotStart ||
		outputDefinition.StackLimit < 0 {
		return plan, fmt.Errorf("%w: output item=%d", errCurrentMonsterCardSynthesisUnavailable, outputID)
	}

	fee := config.MixFee[baseRarity]
	plan = alignedcmd.MonsterCardSynthesisPlan{
		BinderItemID:  binder.ItemID,
		CardItemIDs:   cardItemIDs,
		BinderMinimum: binderDefinition.MinimumLevel,
		CardRarities:  rarities,
		ResultRarity:  resultRarity,
		Chance:        int(chance),
		Roll:          roll,
		UpgradeRoll:   upgradeRoll,
		GoldCost:      fee,
		Output: alignedcmd.MagicBoxRewardItem{
			ItemID:           int64(outputDefinition.ItemID),
			Kind:             string(outputDefinition.Kind),
			TargetListType:   dnfrepo.MainInventoryListType,
			StackLimit:       outputDefinition.StackLimit,
			SlotStart:        outputDefinition.SlotStart,
			SlotEnd:          outputDefinition.SlotEnd,
			Seal:             strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(outputDefinition.AttachType, "`", "")), "sealing"),
			PVFPath:          outputDefinition.PVFPath,
			ExpireAt:         outputDefinition.ExpirationDate,
			UsablePeriodDays: outputDefinition.UsablePeriodDays,
		},
		StackableType: outputDefinition.StackableType,
	}
	return plan, nil
}

func currentMonsterCardSynthesisInputRarity(
	catalog *pvfDungeonDropCatalog,
	source dnfpvf.Source,
	config currentMonsterCardSynthesisConfig,
	itemID int64,
) (int, error) {
	if itemID <= 0 || itemID > int64(^uint32(0)) {
		return 0, errCurrentMonsterCardSynthesisUnavailable
	}
	rarity, listed := config.InputRarity[uint32(itemID)]
	definition, err := catalog.ResolveItem(uint32(itemID))
	if err != nil || definition.Kind != dungeonDropItemStackable {
		return 0, fmt.Errorf("%w: card item=%d", errCurrentMonsterCardSynthesisUnavailable, itemID)
	}
	document, err := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if err != nil {
		return 0, err
	}
	category, found := document.Text("item category")
	if !found || !strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(category, "`", "")), "monster card") {
		return 0, fmt.Errorf("%w: item=%d category=%q", errCurrentMonsterCardSynthesisUnavailable, itemID, category)
	}
	if listed {
		return rarity, nil
	}
	// Custom/imported 90-card templates can be valid monster cards whose IDs
	// were never appended to the legacy [card list]. Their own [rarity] field is
	// authoritative for synthesis, while the category check above still prevents
	// arbitrary stackables from entering the card pool.
	documentRarity := document.Ints("rarity")
	if len(documentRarity) == 1 && documentRarity[0] >= 0 && documentRarity[0] <= 6 {
		return int(documentRarity[0]), nil
	}
	return 0, fmt.Errorf(
		"%w: card item=%d is absent from [card list] and has no valid [rarity]",
		errCurrentMonsterCardSynthesisUnavailable,
		itemID,
	)
}

func currentMonsterCardSynthesisChance(
	config currentMonsterCardSynthesisConfig,
	binderType int,
	firstRarity int,
	secondRarity int,
) (int64, error) {
	binderRate, ok := config.BinderRates[binderType]
	if !ok {
		return 0, fmt.Errorf("%w: binder type=%d", errCurrentMonsterCardSynthesisUnavailable, binderType)
	}
	low, high := min(firstRarity, secondRarity), max(firstRarity, secondRarity)
	end := high
	if low == high {
		end = low + 1
	}
	value := int64(currentMonsterCardSynthesisOddsScale)
	for rarity := low; rarity < end; rarity++ {
		step, found := config.MixProbability[rarity]
		if !found {
			return 0, fmt.Errorf("%w: [mix probability] rarity=%d", errCurrentMonsterCardSynthesisUnavailable, rarity)
		}
		value = value * step / currentMonsterCardSynthesisOddsScale
	}
	value = value * binderRate / 100
	if value > currentMonsterCardSynthesisOddsScale {
		value = currentMonsterCardSynthesisOddsScale
	}
	return value, nil
}

func currentMonsterCardSynthesisResultRarity(first, second int, upgrade bool) int {
	high := max(first, second)
	if first == second {
		if upgrade {
			return min(3, first+1)
		}
		return first
	}
	if upgrade {
		return high
	}
	return max(0, high-1)
}

func currentMonsterCardSynthesisSelectOutput(
	config currentMonsterCardSynthesisConfig,
	rarity int,
	random func(int) int,
) (uint32, error) {
	pool := config.Results[rarity]
	total := int64(0)
	for _, entry := range pool {
		if entry.Weight > 0 {
			total += entry.Weight
		}
	}
	if total <= 0 || total > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w: result rarity=%d total=%d", errCurrentMonsterCardSynthesisUnavailable, rarity, total)
	}
	roll := int64(random(int(total)))
	for _, entry := range pool {
		if entry.Weight <= 0 {
			continue
		}
		if roll < entry.Weight {
			return entry.ItemID, nil
		}
		roll -= entry.Weight
	}
	return 0, fmt.Errorf("%w: result roll rarity=%d", errCurrentMonsterCardSynthesisUnavailable, rarity)
}
