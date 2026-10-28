package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	dnfavatarcompound "longheng.io/server/internal/modules/dnf/avatarcompound"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// commitCurrentAvatarBindPlus 落地一次 op1119（梦想/绝对绑定装扮合成器）：
//
//	扣 8 件材料装扮（请求体里那 8 个 (槽, 道具)）+ 扣 1 个该套装的绝对绑定合成器
//	→ 发出玩家点名的输出装扮（[absolute bind plus rare][职业][部位]）
//
// 全程走 op99 同一套 dnfavatarcompound.Owner 事务：任何一步校验失败都整体回滚，
// 不会出现"扣了材料没发奖"。
//
// 合成器本身**不在请求体里**（两次现场取证的 144 字节里都找不到合成器字段），
// 所以这里用唯一自洽的口径反查：请求点名的输出 → chn_absolute_bind_cube.etc 里的
// 套装序号 → 背包里 [action type] [absolute bind cube] <同一套装> 的道具。这样
// 既不需要新协议字段，也不可能出现"没带合成器白嫖"。
func (s *Service) commitCurrentAvatarBindPlus(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	request currentAvatarBindPlusRequest,
) (currentAvatarBindPlusOutcome, error) {
	if catalog == nil || catalog.source == nil {
		return currentAvatarBindPlusOutcome{}, errCurrentAvatarBindPlusUnavailable
	}
	accountID, characterID, repositories, owner, err := s.currentAvatarCompoundOwner(session)
	if err != nil {
		return currentAvatarBindPlusOutcome{}, err
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || strings.TrimSpace(character.AccountID) != accountID {
		return currentAvatarBindPlusOutcome{}, errors.Join(errCurrentAvatarBindPlusUnavailable, err)
	}
	jobValue := numericCharacterStat(character.Job)
	if jobValue < 0 || jobValue > math.MaxUint8 {
		return currentAvatarBindPlusOutcome{}, fmt.Errorf("%w: character job=%q", errCurrentAvatarBindPlusUnavailable, character.Job)
	}
	config, err := loadCurrentAvatarBindPlusConfig(catalog.source)
	if err != nil {
		return currentAvatarBindPlusOutcome{}, err
	}
	jobKey, err := currentAvatarBindPlusJobKey(catalog.source, byte(jobValue))
	if err != nil {
		return currentAvatarBindPlusOutcome{}, err
	}
	output, err := currentAvatarBindPlusResolveOutput(config, jobKey, request.RequestedItemID)
	if err != nil {
		return currentAvatarBindPlusOutcome{}, err
	}
	// 输出道具必须是真实装扮（PVF 只读解析，放在事务外）。
	outputDefinition, _, err := currentAvatarCompoundDefinition(catalog, request.RequestedItemID)
	if err != nil {
		return currentAvatarBindPlusOutcome{}, err
	}

	var outcome currentAvatarBindPlusOutcome
	err = owner.Compound(ctx, dnfavatarcompound.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		Project: func(assets *dnfavatarcompound.Assets) error {
			if assets == nil || assets.Character == nil || assets.Inventory == nil || assets.Inventory.Slots == nil ||
				numericCharacterStat(assets.Character.Job) != jobValue {
				return errCurrentAvatarBindPlusUnavailable
			}
			inventory := assets.Inventory

			// 1. 8 件材料：槽位、道具 ID、数量、锁、PVF 装扮身份，逐条校验。
			materialKeys := make([]string, 0, len(request.Materials))
			for index, material := range request.Materials {
				key := currentCeraShopInventorySlotKey(currentAvatarInventoryListType, material.Slot)
				stack, exists := inventory.Slots[key]
				if !exists || stack.ItemID != int64(material.ItemID) || stack.Count != 1 || currentNPCShopItemLocked(stack) {
					return fmt.Errorf("%w: index=%d slot=%d item=%d found=%t stack=%+v",
						errCurrentAvatarBindPlusMaterial, index, material.Slot, material.ItemID, exists, stack)
				}
				if _, _, resolveErr := currentAvatarCompoundDefinition(catalog, material.ItemID); resolveErr != nil {
					return fmt.Errorf("%w: index=%d item=%d: %v",
						errCurrentAvatarBindPlusMaterial, index, material.ItemID, resolveErr)
				}
				materialKeys = append(materialKeys, key)
			}

			// 2. 合成器：按套装序号反查。
			cubeKey, cubeStack, cubeErr := currentAvatarBindPlusFindCube(catalog, inventory, output.SetIndex)
			if cubeErr != nil {
				return cubeErr
			}
			cubeListType, cubeSlot, _ := dnfrepo.ParseInventorySlotKey(cubeKey)
			cubeItemID := uint32(cubeStack.ItemID)

			// 3. 扣材料（8 件各 1，装扮不堆叠，直接删槽）。
			for _, key := range materialKeys {
				delete(inventory.Slots, key)
			}

			// 4. 扣合成器 1 个。
			cubeStack.Count--
			cubeRemain := cubeStack.Count
			if cubeStack.Count <= 0 {
				delete(inventory.Slots, cubeKey)
			} else {
				entry := currentItemListEntryFromStack(cubeListType, cubeSlot, cubeStack)
				cubeStack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[cubeKey] = cubeStack
			}

			// 5. 发输出装扮：落在第一个材料槽（刚空出来、必然是合法的装扮栏槽位）。
			outputSlot := request.Materials[0].Slot
			stack := currentAvatarCompoundOutputStack(outputDefinition, request.AbilityNo)
			entry := currentItemListEntryFromStack(currentAvatarInventoryListType, outputSlot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[currentCeraShopInventorySlotKey(currentAvatarInventoryListType, outputSlot)] = stack

			consumed := make([]int16, 0, currentAvatarBindPlusMaterialCount)
			for _, material := range request.Materials {
				consumed = append(consumed, material.Slot)
			}
			outcome = currentAvatarBindPlusOutcome{
				OutputItemID:  request.RequestedItemID,
				OutputSlot:    outputSlot,
				AbilityNo:     request.AbilityNo,
				ConsumedSlots: consumed,
				SetIndex:      output.SetIndex,
				Part:          output.Part,
				CubeItemID:    cubeItemID,
				CubeListType:  cubeListType,
				CubeSlot:      cubeSlot,
				CubeRemain:    cubeRemain,
			}
			return nil
		},
	})
	if err != nil {
		return currentAvatarBindPlusOutcome{}, currentAvatarCompoundOwnerError(err)
	}
	return outcome, nil
}

// currentAvatarBindPlusFindCube 在背包里找该套装序号的绝对绑定合成器。
// 多个候选时优先主背包（list 0），保证"帐号金库里的合成器"不会被优先消耗。
func currentAvatarBindPlusFindCube(
	catalog *pvfDungeonDropCatalog,
	inventory *dnfrepo.InventoryRecord,
	setIndex int64,
) (string, dnfrepo.ItemStack, error) {
	if catalog == nil || inventory == nil {
		return "", dnfrepo.ItemStack{}, errCurrentAvatarBindPlusCube
	}
	bestKey := ""
	var bestStack dnfrepo.ItemStack
	bestListType := byte(0xFF)
	for key, stack := range inventory.Slots {
		if stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count <= 0 || currentNPCShopItemLocked(stack) {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(uint32(stack.ItemID))
		if resolveErr != nil || !currentAvatarBindPlusDeclaresCube(definition) {
			continue
		}
		cubeSet, ok := currentAvatarBindPlusCubeSet(definition)
		if !ok || cubeSet != setIndex {
			continue
		}
		listType, _, parsed := dnfrepo.ParseInventorySlotKey(key)
		if !parsed {
			continue
		}
		if bestKey != "" && listType >= bestListType {
			continue
		}
		bestKey, bestStack, bestListType = key, stack, listType
	}
	if bestKey == "" {
		return "", dnfrepo.ItemStack{}, fmt.Errorf("%w: set=%d 背包里没有对应的绝对绑定合成器", errCurrentAvatarBindPlusCube, setIndex)
	}
	return bestKey, bestStack, nil
}
