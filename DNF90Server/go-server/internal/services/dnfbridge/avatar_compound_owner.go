package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfavatarcompound "longheng.io/server/internal/modules/dnf/avatarcompound"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) currentAvatarCompoundOwner(session *gameSession) (string, string, dnfrepo.Group, *dnfavatarcompound.Owner, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return "", "", dnfrepo.Group{}, nil, errCurrentAvatarCompoundUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.CharacterAssets == nil {
		return "", "", dnfrepo.Group{}, nil, errCurrentAvatarCompoundUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return "", "", dnfrepo.Group{}, nil, errCurrentAvatarCompoundUnavailable
	}
	owner, err := dnfavatarcompound.NewOwner(repositories)
	if err != nil {
		return "", "", dnfrepo.Group{}, nil, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	return accountID, strconv.FormatUint(uint64(session.selectedCharacterID), 10), repositories, owner, nil
}

// currentAvatarCompoundCubeBonus resolves the rare-rate bonus of the consumed
// 装扮合成器 (the "bind cube"). Three sources, in order:
//
//  1. the canonical three-tier tail of the job file — 21 装扮合成器 (ordinary),
//     899 黄金装扮合成器 (upper), 490702265 钻石装扮合成器 (master); their bonus
//     comes from etc/serverparameter.etc;
//  2. a [common bind cube] block's own [material] (升级的远古装扮合成器
//     10301984/10302329 and the other 90-level families). Those blocks carry no
//     bonus field, so they price at the ordinary tier and are told apart by the
//     block's own [output] pool;
//  3. the event re-issues of the same cubes (钻石装扮合成器 10007495/10007718/
//     10007901/10008013/10008512, 追忆之装扮合成器 2683630, …). These are NOT in
//     the job file at all, so the only PVF evidence left is the item name: it
//     must end with 装扮合成器 and must not be one of the 梦想/梦幻 absolute
//     cubes (those are the `[action type] [absolute bind cube]` items and use the
//     separate op1119 flow).
//
// Live 2026-10-04: only source 1 was implemented, so every 远古/追忆/钻石活动版
// cube was answered op99 {0,0x16} and the client reported the cube as unusable.
func currentAvatarCompoundCubeBonus(
	config currentAvatarCompoundConfig,
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
) (int64, bool) {
	if bonus, ok := config.MaterialBonuses[itemID]; ok {
		return bonus, true
	}
	if config.BlockMaterials[itemID] {
		return 0, true
	}
	if catalog == nil || itemID == 0 {
		return 0, false
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.Kind != dungeonDropItemStackable {
		return 0, false
	}
	name := strings.TrimSpace(definition.Name)
	if !strings.HasSuffix(name, "装扮合成器") || strings.Contains(name, "梦想") || strings.Contains(name, "梦幻") {
		return 0, false
	}
	switch {
	case strings.Contains(name, "钻石"):
		return config.MasterBonus, true
	case strings.Contains(name, "黄金"), strings.Contains(name, "追忆"):
		// 追忆之装扮合成器的说明写明"出现稀有装扮的几率比普通装扮合成器更高"，
		// 与黄金同档；钻石再高一档（其说明："比黄金装扮合成器更高"）。
		return config.UpperBonus, true
	default:
		return 0, true
	}
}

func currentAvatarCompoundOwnerError(err error) error {
	switch {
	case errors.Is(err, dnfavatarcompound.ErrOwnerUnavailable),
		errors.Is(err, dnfavatarcompound.ErrAccountRequired),
		errors.Is(err, dnfavatarcompound.ErrCharacterRequired),
		errors.Is(err, dnfavatarcompound.ErrCharacterNotFound),
		errors.Is(err, dnfavatarcompound.ErrAccountMismatch),
		errors.Is(err, dnfavatarcompound.ErrInventoryNotFound):
		return errors.Join(errCurrentAvatarCompoundUnavailable, err)
	default:
		return err
	}
}

func (s *Service) commitCurrentAvatarCompound(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	request currentAvatarCompoundRequest,
) (currentAvatarCompoundResult, error) {
	if catalog == nil || catalog.source == nil {
		return currentAvatarCompoundResult{}, errCurrentAvatarCompoundUnavailable
	}
	accountID, characterID, repositories, owner, err := s.currentAvatarCompoundOwner(session)
	if err != nil {
		return currentAvatarCompoundResult{}, err
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || strings.TrimSpace(character.AccountID) != accountID {
		return currentAvatarCompoundResult{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	jobValue := numericCharacterStat(character.Job)
	if jobValue < 0 || jobValue > math.MaxUint8 {
		return currentAvatarCompoundResult{}, fmt.Errorf("%w: character job=%q", errCurrentAvatarCompoundUnavailable, character.Job)
	}
	config, err := loadCurrentAvatarCompoundConfig(catalog.source, byte(jobValue))
	if err != nil {
		return currentAvatarCompoundResult{}, err
	}
	partName, found := currentAvatarCompoundRequestedPart(config, request.RequestedItemID)
	if !found {
		return currentAvatarCompoundResult{}, fmt.Errorf("%w: requested item=%d config=%s", errCurrentAvatarCompoundOutputInvalid, request.RequestedItemID, config.PVFPath)
	}

	requestedDefinition, requestedDocument, err := currentAvatarCompoundDefinition(catalog, request.RequestedItemID)
	if err != nil {
		return currentAvatarCompoundResult{}, err
	}
	requestedPart, ok := currentAvatarCompoundDocumentPart(requestedDocument)
	if !ok || requestedPart != partName {
		return currentAvatarCompoundResult{}, fmt.Errorf("%w: requested item=%d part=%q config_part=%q", errCurrentAvatarCompoundOutputInvalid, request.RequestedItemID, requestedPart, partName)
	}
	source1Definition, source1Document, err := currentAvatarCompoundDefinition(catalog, request.ExpectedItemID1)
	if err != nil {
		return currentAvatarCompoundResult{}, errCurrentAvatarCompoundSourceInvalid
	}
	source2Definition, source2Document, err := currentAvatarCompoundDefinition(catalog, request.ExpectedItemID2)
	if err != nil {
		return currentAvatarCompoundResult{}, errCurrentAvatarCompoundSourceInvalid
	}
	part1, source1PartOK := currentAvatarCompoundDocumentPart(source1Document)
	part2, source2PartOK := currentAvatarCompoundDocumentPart(source2Document)
	if !source1PartOK || !source2PartOK || part1 != partName || part2 != partName {
		return currentAvatarCompoundResult{}, fmt.Errorf("%w: requested_part=%s source_parts=%s,%s", errCurrentAvatarCompoundSourceInvalid, partName, part1, part2)
	}

	var result currentAvatarCompoundResult
	err = owner.Compound(ctx, dnfavatarcompound.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		Project: func(assets *dnfavatarcompound.Assets) error {
			if assets == nil || assets.Character == nil || assets.Inventory == nil || assets.Inventory.Slots == nil ||
				numericCharacterStat(assets.Character.Job) != jobValue {
				return errCurrentAvatarCompoundUnavailable
			}
			inventory := assets.Inventory
			sourceKey1 := currentCeraShopInventorySlotKey(currentAvatarInventoryListType, request.Slot1)
			sourceKey2 := currentCeraShopInventorySlotKey(currentAvatarInventoryListType, request.Slot2)
			sourceStack1, sourceFound1 := inventory.Slots[sourceKey1]
			sourceStack2, sourceFound2 := inventory.Slots[sourceKey2]
			if !currentAvatarCompoundSourceStackValid(sourceStack1, sourceFound1, request.ExpectedItemID1) ||
				!currentAvatarCompoundSourceStackValid(sourceStack2, sourceFound2, request.ExpectedItemID2) {
				return errCurrentAvatarCompoundSourceInvalid
			}

			materialKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.ConsumeSlot)
			materialStack, materialFound := inventory.Slots[materialKey]
			if !materialFound || materialStack.ItemID <= 0 || materialStack.ItemID > math.MaxUint32 || materialStack.Count <= 0 || currentNPCShopItemLocked(materialStack) {
				return errCurrentAvatarCompoundMaterial
			}
			materialItemID := uint32(materialStack.ItemID)
			bonus, accepted := currentAvatarCompoundCubeBonus(config, catalog, materialItemID)
			if !accepted {
				return fmt.Errorf("%w: item=%d", errCurrentAvatarCompoundMaterial, materialItemID)
			}
			config = config.withCubeBonus(materialItemID, bonus)
			materialDefinition, resolveErr := catalog.ResolveItem(materialItemID)
			if resolveErr != nil || materialDefinition.Kind != dungeonDropItemStackable {
				return errCurrentAvatarCompoundMaterial
			}

			rate, rateErr := currentAvatarCompoundRareRate(config, partName, source1Definition.Grade, source2Definition.Grade, materialItemID)
			if rateErr != nil {
				return rateErr
			}
			roll, rollErr := currentDisjointRandomUint64(10000)
			if rollErr != nil {
				return rollErr
			}
			newItemIDs := []uint32{request.RequestedItemID}
			rareHit := roll < uint64(rate)
			if rareHit {
				rareItemID, rareErr := rollCurrentAvatarCompoundRare(config.Parts[partName].Rare)
				if rareErr != nil {
					return rareErr
				}
				_, rareDocument, rareErr := currentAvatarCompoundDefinition(catalog, rareItemID)
				rarePart, rarePartOK := currentAvatarCompoundDocumentPart(rareDocument)
				if rareErr != nil || !rarePartOK || rarePart != partName {
					return fmt.Errorf("%w: rare item=%d part=%s", errCurrentAvatarCompoundOutputInvalid, rareItemID, rarePart)
				}
				newItemIDs = append(newItemIDs, rareItemID)
			}

			delete(inventory.Slots, sourceKey1)
			delete(inventory.Slots, sourceKey2)
			materialStack.Count--
			materialRemain := materialStack.Count
			if materialStack.Count == 0 {
				delete(inventory.Slots, materialKey)
			} else {
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.ConsumeSlot, materialStack)
				materialStack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[materialKey] = materialStack
			}

			newSlots := []int16{request.Slot1, request.Slot2}
			sort.Slice(newSlots, func(left, right int) bool { return newSlots[left] < newSlots[right] })
			newSlots = newSlots[:len(newItemIDs)]
			for index, itemID := range newItemIDs {
				definition := requestedDefinition
				if itemID != request.RequestedItemID {
					definition, _, resolveErr = currentAvatarCompoundDefinition(catalog, itemID)
					if resolveErr != nil {
						return resolveErr
					}
				}
				slot := newSlots[index]
				stack := currentAvatarCompoundOutputStack(definition, request.AbilityNo)
				entry := currentItemListEntryFromStack(currentAvatarInventoryListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[currentCeraShopInventorySlotKey(currentAvatarInventoryListType, slot)] = stack
			}
			result = currentAvatarCompoundResult{
				MaterialItemID: materialItemID,
				MaterialRemain: materialRemain,
				SourceItemIDs:  [2]uint32{request.ExpectedItemID1, request.ExpectedItemID2},
				NewSlots:       append([]int16(nil), newSlots...),
				NewItemIDs:     append([]uint32(nil), newItemIDs...),
				RareRate:       rate,
				RareRoll:       roll,
				RareHit:        rareHit,
			}
			return nil
		},
	})
	if err != nil {
		return currentAvatarCompoundResult{}, currentAvatarCompoundOwnerError(err)
	}
	return result, nil
}

func currentAvatarCompoundSourceStackValid(stack dnfrepo.ItemStack, found bool, expectedItemID uint32) bool {
	return found && stack.ItemID == int64(expectedItemID) && (stack.Count == 0 || stack.Count == 1) && !currentNPCShopItemLocked(stack)
}

func currentAvatarCompoundDefinition(catalog *pvfDungeonDropCatalog, itemID uint32) (dungeonDropItemDefinition, *dnfpvf.Document, error) {
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.Kind != dungeonDropItemEquipment ||
		(!definition.ExpirationDate.IsZero() && !time.Now().UTC().Before(definition.ExpirationDate)) {
		return dungeonDropItemDefinition{}, nil, errors.Join(errCurrentAvatarCompoundOutputInvalid, err)
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil || !currentDisjointDocumentIsAvatar(document) {
		return dungeonDropItemDefinition{}, nil, errors.Join(errCurrentAvatarCompoundOutputInvalid, err)
	}
	return definition, document, nil
}

func currentAvatarCompoundOutputStack(definition dungeonDropItemDefinition, abilityNo uint16) dnfrepo.ItemStack {
	extra := map[string]string{
		"source":            "avatar_compound",
		"item_kind":         "avatar",
		"pvf_path":          definition.PVFPath,
		"equipment_type":    definition.EquipmentType,
		"avatar_ability_no": strconv.FormatUint(uint64(abilityNo), 10),
		"durability":        strconv.FormatUint(uint64(abilityNo), 10),
		"amount_or_count":   "0",
	}
	stack := dnfrepo.ItemStack{ItemID: int64(definition.ItemID), Count: 1, Extra: extra}
	if !definition.ExpirationDate.IsZero() {
		stack, _ = applyCurrentPVFItemExpirationAt(stack, definition, time.Now().UTC())
	}
	return stack
}
