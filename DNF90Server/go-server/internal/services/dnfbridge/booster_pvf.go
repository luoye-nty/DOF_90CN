package dnfbridge

import (
	cryptorand "crypto/rand"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func resolveCurrentBoosterDefinition(
	catalog *pvfDungeonDropCatalog,
	sourceItemID uint32,
	requestKind currentBoosterRequestKind,
) (currentBoosterDefinition, error) {
	if catalog == nil || catalog.source == nil || sourceItemID == 0 {
		return currentBoosterDefinition{}, errCurrentBoosterPVFInvalid
	}
	sourceDefinition, err := catalog.ResolveItem(sourceItemID)
	if err != nil {
		return currentBoosterDefinition{}, fmt.Errorf("%w: resolve source item=%d: %v", errCurrentBoosterPVFInvalid, sourceItemID, err)
	}
	if sourceDefinition.Kind != dungeonDropItemStackable {
		return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d is not stackable", errCurrentBoosterPVFInvalid, sourceItemID)
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, sourceDefinition.PVFPath)
	if err != nil {
		return currentBoosterDefinition{}, fmt.Errorf("%w: read source item=%d: %v", errCurrentBoosterPVFInvalid, sourceItemID, err)
	}
	stackableType := normalizeDungeonDropStackableType(magicBoxDocumentText(document, "stackable type"))
	giftAcquisitionOnly := currentMagicBoxHasImpossibleContent(document, "gift")
	definition := currentBoosterDefinition{Source: sourceDefinition, StackableType: stackableType}
	switch requestKind {
	case currentBoosterRequestSelection:
		if stackableType != "[booster selection]" {
			return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d type=%q is not selectable", errCurrentBoosterPVFInvalid, sourceItemID, stackableType)
		}
		requiredValues := document.Ints("booster selection num")
		if len(requiredValues) == 0 || requiredValues[0] < 0 || requiredValues[0] > currentBoosterMaxRewardRows {
			return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d selection_num=%v", errCurrentBoosterPVFInvalid, sourceItemID, requiredValues)
		}
		definition.SelectionRequired = int(requiredValues[0])
		definition.Selection, definition.SelectionCategory, err = parseCurrentBoosterSelectionCatalog(document)
		if err != nil {
			return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d: %v", errCurrentBoosterPVFInvalid, sourceItemID, err)
		}
		if len(definition.Selection) == 0 || len(definition.SelectionCategory) == 0 {
			return currentBoosterDefinition{}, fmt.Errorf(
				"%w: source item=%d selection_rows=%d selection_categories=%d",
				errCurrentBoosterPVFInvalid,
				sourceItemID,
				len(definition.Selection),
				len(definition.SelectionCategory),
			)
		}
	case currentBoosterRequestRandom, currentBoosterRequestLottery:
		if stackableType != "[booster]" && stackableType != "[cera booster]" && stackableType != "[booster random]" && stackableType != "[random upgradable legacy]" {
			if requestKind != currentBoosterRequestLottery || stackableType != "[upgradable legacy]" {
				return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d type=%q is not random", errCurrentBoosterPVFInvalid, sourceItemID, stackableType)
			}
		}
		definition.Random, err = resolveCurrentMagicBox(catalog, catalog.source, int64(sourceItemID))
		if err != nil {
			return currentBoosterDefinition{}, err
		}
		// Top-level [need material] on an owned box is acquisition/shop
		// metadata, not a second opening cost. Preserve only a true
		// opening key. Legacy op27 jars declare it as
		// [lottery use need item]; other random-upgradable boxes use
		// [sealing removal item] (for example a magic hammer).
		// This applies to both op160 booster boxes and op27 lottery jars.
		if requestKind == currentBoosterRequestRandom || requestKind == currentBoosterRequestLottery {
			if requestKind == currentBoosterRequestLottery {
				definition.Random.MaterialItemID, definition.Random.MaterialCountPerUse =
					resolveMagicBoxLotteryOpeningMaterial(document, int64(sourceItemID))
			} else {
				definition.Random.MaterialItemID, definition.Random.MaterialCountPerUse =
					resolveMagicBoxSealingRemovalMaterial(document, int64(sourceItemID))
			}
		}
		if definition.Random.Kind != "random" || len(definition.Random.Groups) == 0 {
			return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d has no random reward groups", errCurrentBoosterPVFInvalid, sourceItemID)
		}
	default:
		return currentBoosterDefinition{}, errCurrentBoosterRequestMalformed
	}
	if materialValues := document.Ints("need material"); len(materialValues) > 0 && !giftAcquisitionOnly &&
		requestKind == currentBoosterRequestSelection {
		if len(materialValues) < 2 ||
			materialValues[0] <= 0 ||
			materialValues[1] <= 0 ||
			materialValues[1] > currentBoosterMaxRewardUnits {
			return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d need_material=%v", errCurrentBoosterPVFInvalid, sourceItemID, materialValues)
		}
		if _, resolveErr := catalog.ResolveItem(uint32(materialValues[0])); resolveErr != nil {
			return currentBoosterDefinition{}, fmt.Errorf("%w: source item=%d need_material=%v: %v", errCurrentBoosterPVFInvalid, sourceItemID, materialValues, resolveErr)
		}
		definition.MaterialItemID = materialValues[0]
		definition.MaterialCount = materialValues[1]
	}
	return definition, nil
}

func parseCurrentBoosterSelectionCandidates(document *dnfpvf.Document) ([]currentBoosterSelectionCandidate, error) {
	candidates, _, err := parseCurrentBoosterSelectionCatalog(document)
	return candidates, err
}

func parseCurrentBoosterSelectionCatalog(
	document *dnfpvf.Document,
) ([]currentBoosterSelectionCandidate, map[uint16][]currentBoosterSelectionCandidate, error) {
	if document == nil {
		return nil, nil, errCurrentBoosterPVFInvalid
	}
	inCategory := false
	var categoryID uint16
	var upgradeLevel byte
	result := make([]currentBoosterSelectionCandidate, 0, 8)
	seen := make(map[uint32]currentBoosterSelectionCandidate)
	categorySeen := make(map[uint16]map[uint32]currentBoosterSelectionCandidate)
	categories := make(map[uint16][]currentBoosterSelectionCandidate)
	for _, section := range document.Sections {
		name := normalizeMagicBoxSectionName(section.Name)
		switch name {
		case "booster select category":
			// Repeating the header implicitly ends the preceding category in
			// historical files which omit each individual closing tag.
			header := magicBoxSectionInts(document, section)
			if len(header) == 0 ||
				header[0] < 0 ||
				header[0] > math.MaxUint8 ||
				(len(header) > 1 && (header[1] < 0 || header[1] > math.MaxUint8)) {
				return nil, nil, fmt.Errorf("invalid booster selection category header=%v", header)
			}
			// Current NoPack writes the two category header bytes separately
			// immediately after source_slot. Keep the same little-endian key
			// instead of discarding the grow-type/subcategory byte.
			categoryID = uint16(header[0])
			if len(header) > 1 {
				categoryID |= uint16(header[1]) << 8
			}
			inCategory = true
			upgradeLevel = 0
			if _, exists := categories[categoryID]; !exists {
				categories[categoryID] = make([]currentBoosterSelectionCandidate, 0, 8)
				categorySeen[categoryID] = make(map[uint32]currentBoosterSelectionCandidate)
			}
			continue
		case "/booster select category":
			if !inCategory {
				// A small set of historical files contains a redundant category
				// close after a sequence of individually closed categories.
				// It carries no rewards and can be ignored without widening the
				// accepted item catalog.
				continue
			}
			inCategory = false
			continue
		}
		if !inCategory || strings.HasPrefix(name, "/") {
			continue
		}
		values := magicBoxSectionInts(document, section)
		// [booster equipment upgrade] 不是奖励行，而是**该分类内装备奖励的强化等级**：
		// 它写在分类头之后、equipment 段之前，作用域是"本分类"。升级预约活动
		// （onlineevent level_aiming，活动 222）发的武器就靠这个值决定 +11 之类，
		// 所以不能像参考实现以外的那样当"已知但忽略"。计数为 0 的分类不受影响。
		if name == "booster equipment upgrade" {
			if len(values) != 1 || values[0] < 0 || values[0] > math.MaxUint8 {
				return nil, nil, fmt.Errorf("invalid booster equipment upgrade=%v", values)
			}
			upgradeLevel = byte(values[0])
			continue
		}
		if len(values) == 0 {
			continue
		}
		stride := 0
		switch name {
		case "booster category num", "booster selection num", "default select", "booster equipment grade", "booster equipment separate", "random probability":
			// Historical selection boxes keep client-side defaults and a
			// display-defaults hint inside each category. Neither section is
			// a reward row: the following equipment/stackable/avatar sections
			// remain the authoritative selectable catalog.
			continue
		case "avatar":
			// Avatar choices carry item/count plus the client attribute
			// category and option written in the 10-byte op160 request.
			stride = 4
		case "cera", "stackable", "stackble", "creature", "equipment", "etc":
			// Ordinary items, pets and enchant orbs are simple item/count
			// pairs. Treating two adjacent pairs as one avatar quad made the
			// second item look like an option byte and rejected valid choices.
			stride = 2
		default:
			return nil, nil, fmt.Errorf("selection section=%q with %d integer values has unsupported runtime-PVF shape", section.Name, len(values))
		}
		if len(values)%stride != 0 {
			return nil, nil, fmt.Errorf("selection section=%q values=%d is not divisible by stride=%d", section.Name, len(values), stride)
		}
		for offset := 0; offset < len(values); offset += stride {
			itemID, count := values[offset], values[offset+1]
			categoryKind, option := int64(0), int64(0)
			if stride == 4 {
				categoryKind, option = values[offset+2], values[offset+3]
			}
			if itemID <= 0 || itemID > math.MaxUint32 || count <= 0 || count > currentBoosterMaxRewardUnits || option < -1 || option > math.MaxUint8 {
				return nil, nil, fmt.Errorf("selection section=%q invalid row=%v", section.Name, values[offset:offset+stride])
			}
			if option == -1 {
				// -1 means no PVF default. Avatar choices receive their actual
				// option from the current op160 request after category validation.
				option = 0
			}
			candidate := currentBoosterSelectionCandidate{
				ItemID:       uint32(itemID),
				Count:        uint32(count),
				CategoryKind: categoryKind,
				Option:       byte(option),
			}
			// 强化等级只作用于**装备**行：stackable/avatar/creature 拿到它没有意义，
			// 而且分类之间不能互相泄漏（升级预约的分类 != 普通选择的分类）。
			if name == "equipment" {
				candidate.UpgradeLevel = upgradeLevel
			}
			if _, duplicate := seen[candidate.ItemID]; !duplicate {
				seen[candidate.ItemID] = candidate
				result = append(result, candidate)
			}
			if previous, duplicate := categorySeen[categoryID][candidate.ItemID]; duplicate {
				if previous != candidate {
					return nil, nil, fmt.Errorf("selection category=%d item=%d has conflicting rows", categoryID, candidate.ItemID)
				}
				// Some historical event files repeat an identical job category or
				// list the same weapon twice. Keep one authoritative choice.
				continue
			}
			categorySeen[categoryID][candidate.ItemID] = candidate
			categories[categoryID] = append(categories[categoryID], candidate)
		}
	}
	// Some historical event definitions terminate the last selection category
	// at end-of-file rather than writing a closing tag. A category header was
	// still required before every accepted reward row, so EOF is a safe implicit
	// close and does not admit unscoped rewards.
	for id, candidates := range categories {
		// Job/grow-type selection documents retain empty category placeholders
		// for combinations the client does not offer (for example a job without
		// a fifth advancement).  They do not invalidate populated categories.
		// currentBoosterSelectedCandidates still rejects an empty category if a
		// forged request explicitly selects one.
		if len(candidates) > currentBoosterMaxRewardRows {
			return nil, nil, fmt.Errorf("booster selection category=%d rows=%d", id, len(candidates))
		}
	}
	return result, categories, nil
}

func currentBoosterSelectedCandidates(
	definition currentBoosterDefinition,
	request currentBoosterOpenRequest,
) ([]currentBoosterSelectionCandidate, error) {
	if len(request.Selections) == 0 {
		return nil, errCurrentBoosterSelectionInvalid
	}
	category, found := definition.SelectionCategory[request.SelectionContext]
	if !found || len(category) == 0 {
		return nil, fmt.Errorf("%w: category=%d is not in source PVF", errCurrentBoosterSelectionInvalid, request.SelectionContext)
	}
	required := definition.SelectionRequired
	if required == 0 {
		required = len(category)
	}
	if required <= 0 || len(request.Selections) != required {
		return nil, fmt.Errorf(
			"%w: category=%d selected=%d want=%d",
			errCurrentBoosterSelectionInvalid,
			request.SelectionContext,
			len(request.Selections),
			required,
		)
	}
	byItem := make(map[uint32]currentBoosterSelectionCandidate, len(category))
	for _, candidate := range category {
		byItem[candidate.ItemID] = candidate
	}
	selected := make([]currentBoosterSelectionCandidate, 0, len(request.Selections))
	seen := make(map[uint32]struct{}, len(request.Selections))
	for _, choice := range request.Selections {
		if choice.ItemID == 0 {
			return nil, errCurrentBoosterSelectionInvalid
		}
		if _, duplicate := seen[choice.ItemID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate item=%d", errCurrentBoosterSelectionInvalid, choice.ItemID)
		}
		seen[choice.ItemID] = struct{}{}
		candidate, found := byItem[choice.ItemID]
		if !found {
			return nil, fmt.Errorf(
				"%w: item=%d is not in source PVF category=%d",
				errCurrentBoosterSelectionInvalid,
				choice.ItemID,
				request.SelectionContext,
			)
		}
		if candidate.CategoryKind != 0 {
			// Avatar category rows carry the client-selected attribute byte.
			// The fourth PVF integer is not that runtime choice: the live
			// eight-piece writer sends different values for hat/hair/top/etc.
			candidate.Option = choice.Option
		} else if candidate.Option != choice.Option {
			return nil, fmt.Errorf("%w: item=%d option=%d want=%d", errCurrentBoosterSelectionInvalid, choice.ItemID, choice.Option, candidate.Option)
		}
		selected = append(selected, candidate)
	}
	return selected, nil
}

func validateCurrentBoosterSourceExpiration(stack dnfrepo.ItemStack, definition dungeonDropItemDefinition, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if expire := currentItemListStackExpire(stack); expire != 0 {
		if uint64(expire) <= uint64(now.Unix()) {
			return errCurrentBoosterExpired
		}
		return nil
	}
	if definition.UsablePeriodDays > 0 && currentItemListStackExpire(stack) == 0 {
		return errCurrentBoosterExpired
	}
	if !definition.ExpirationDate.IsZero() && !now.Before(definition.ExpirationDate) {
		return errCurrentBoosterExpired
	}
	return nil
}
func resolveCurrentBoosterRewardAmounts(
	definition currentBoosterDefinition,
	request currentBoosterOpenRequest,
	roll func(int64) (int64, error),
) (map[uint32]uint32, map[uint32]byte, uint32, error) {
	amounts := make(map[uint32]uint32)
	options := make(map[uint32]byte)
	gold := uint64(0)
	switch request.Kind {
	case currentBoosterRequestSelection:
		candidates, err := currentBoosterSelectedCandidates(definition, request)
		if err != nil {
			return nil, nil, 0, err
		}
		for _, candidate := range candidates {
			current := uint64(amounts[candidate.ItemID])
			if current+uint64(candidate.Count) > currentBoosterMaxRewardUnits {
				return nil, nil, 0, errCurrentBoosterPVFInvalid
			}
			amounts[candidate.ItemID] = uint32(current + uint64(candidate.Count))
			options[candidate.ItemID] = candidate.Option
		}
	case currentBoosterRequestRandom, currentBoosterRequestLottery:
		if roll == nil {
			return nil, nil, 0, errCurrentBoosterPVFInvalid
		}
		for _, group := range definition.Random.Groups {
			totalWeight := int64(0)
			for _, entry := range group.Entries {
				validItem := !entry.Gold && entry.ItemID > 0 && entry.ItemID <= math.MaxUint32
				validGold := entry.Gold && entry.ItemID == 0
				if (!validItem && !validGold) || entry.Weight < 0 || entry.Count <= 0 || entry.Count > currentBoosterMaxRewardUnits {
					return nil, nil, 0, errCurrentBoosterPVFInvalid
				}
				if entry.Weight > math.MaxInt64-totalWeight {
					return nil, nil, 0, errCurrentBoosterPVFInvalid
				}
				totalWeight += entry.Weight
			}
			if totalWeight <= 0 {
				return nil, nil, 0, errCurrentBoosterPVFInvalid
			}
			drawCount := group.DrawCount
			if drawCount < 1 {
				drawCount = 1
			}
			if drawCount > currentBoosterMaxRewardRows {
				return nil, nil, 0, errCurrentBoosterPVFInvalid
			}
			for draw := int64(0); draw < drawCount; draw++ {
				value, err := roll(totalWeight)
				if err != nil || value < 0 || value >= totalWeight {
					return nil, nil, 0, fmt.Errorf("%w: random roll: %v", errCurrentBoosterPVFInvalid, err)
				}
				cumulative := int64(0)
				for _, entry := range group.Entries {
					cumulative += entry.Weight
					if value >= cumulative {
						continue
					}
					if entry.Gold {
						if gold+uint64(entry.Count) > math.MaxInt32 {
							return nil, nil, 0, errCurrentBoosterPVFInvalid
						}
						gold += uint64(entry.Count)
						break
					}
					current := uint64(amounts[uint32(entry.ItemID)])
					if current+uint64(entry.Count) > currentBoosterMaxRewardUnits {
						return nil, nil, 0, errCurrentBoosterPVFInvalid
					}
					amounts[uint32(entry.ItemID)] = uint32(current + uint64(entry.Count))
					options[uint32(entry.ItemID)] = entry.Option
					break
				}
			}
		}
	default:
		return nil, nil, 0, errCurrentBoosterRequestMalformed
	}
	rewardRows := len(amounts)
	if gold != 0 {
		rewardRows++
	}
	if rewardRows == 0 || rewardRows > currentBoosterMaxRewardRows {
		return nil, nil, 0, errCurrentBoosterPVFInvalid
	}
	return amounts, options, uint32(gold), nil
}

func secureCurrentBoosterRoll(limit int64) (int64, error) {
	if limit <= 0 {
		return 0, errCurrentBoosterPVFInvalid
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(limit))
	if err != nil {
		return 0, err
	}
	return value.Int64(), nil
}

func resolveCurrentBoosterRewards(
	catalog *pvfDungeonDropCatalog,
	amounts map[uint32]uint32,
	options map[uint32]byte,
	sourceStack dnfrepo.ItemStack,
	now time.Time,
) ([]currentBoosterReward, error) {
	itemIDs := make([]uint32, 0, len(amounts))
	for itemID := range amounts {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(i, j int) bool { return itemIDs[i] < itemIDs[j] })
	rewards := make([]currentBoosterReward, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		count := amounts[itemID]
		definition, err := catalog.ResolveItem(itemID)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve reward item=%d: %v", errCurrentBoosterPVFInvalid, itemID, err)
		}
		definition, err = currentPVFItemDefinitionForNestedRewardGrantAt(definition, sourceStack, now)
		if err != nil {
			return nil, fmt.Errorf("%w: reward item=%d expiration: %v", errCurrentBoosterPVFInvalid, itemID, err)
		}
		document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
		if err != nil {
			return nil, fmt.Errorf("%w: reward item=%d: %v", errCurrentBoosterPVFInvalid, itemID, err)
		}
		attachType := normalizeMagicBoxPVFType(magicBoxDocumentText(document, "attach type"))
		placement, err := currentBoosterPlacement(definition)
		if err != nil {
			return nil, err
		}
		rewards = append(rewards, currentBoosterReward{
			Definition: definition,
			Count:      count,
			Option:     options[itemID],
			Seal:       attachType == "sealing",
			Placement:  placement,
		})
	}
	return rewards, nil
}

func currentBoosterPlacement(definition dungeonDropItemDefinition) (currentBoosterRewardPlacement, error) {
	if definition.Kind == dungeonDropItemStackable {
		if isCurrentCeraShopPetConsumable(definition) {
			return currentBoosterRewardPetConsumable, nil
		}
		return currentBoosterRewardMain, nil
	}
	if definition.Kind != dungeonDropItemEquipment {
		return currentBoosterRewardMain, errCurrentBoosterPVFInvalid
	}
	if isCurrentCeraShopCreatureItem(definition) {
		return currentBoosterRewardPetBody, nil
	}
	if rule, supported := currentEquipmentPlacementRuleForPVFType(definition.EquipmentType); supported && rule.class == currentEquipmentPlacementClassAvatar {
		return currentBoosterRewardAvatar, nil
	}
	normalizedType := normalizeMagicBoxPVFType(definition.EquipmentType)
	if strings.HasPrefix(normalizedType, "artifact ") {
		return currentBoosterRewardPetArtifact, nil
	}
	return currentBoosterRewardMain, nil
}
