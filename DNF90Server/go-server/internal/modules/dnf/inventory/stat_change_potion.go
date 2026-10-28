package inventory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 城镇自施放增益药剂（史诗Buff药剂一族）的落地。
//
// 现场 2026-10-03（服主报，角色 测试x = char 11）：史诗Buff药剂 490002458 连点 15 次，
// op44 每一次都被 useStackableWithResolvers 的 "[waste] provenance" 门静默拒绝
// （error 17），所以玩家看到的是"没反应"。
//
// 这一族（`[stackable type] [expert town potion]` + 无 `[action type]` +
// `[stat change duration]`）与普通 [waste] 消耗品的区别在于：**它的效果是服务端量**。
// 客户端自己会画 buff 图标，但"深渊派对史诗掉落率"只有服务端说了算，所以服务端要
// 自己落一个限时窗口（statbuff），出史诗前查它、把它翻倍。
//
// 为什么单独开一个 owner 而不是挂在通用 [waste] 路径上：
//   - 通用路径的写边界是 withinInventoryTransaction（只有背包 + 装备），
//     而这里必须和**角色记录**同笔（背包减 1 与增益窗口同时生效或同时不生效）。
//     这与 useExperienceConsumable 的选择一致，同样用 WithinRentalAssetsScoped
//     （账号 + 角色 + 背包 + 装备同一条事务）。
//   - 不放松原来那道 [waste] 门：普通消耗品通道的判据保持原样，本族走自己的分支。

var ErrStatChangePotionContract = errors.New("stat-change potion contract is invalid")

// useStatChangePotion 消耗一张城镇增益药剂，并在同一笔事务里落下增益窗口。
func (o *Owner) useStatChangePotion(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
) (UseStackableResult, error) {
	potion := resolution.StatChangePotion
	if o == nil || o.rentalAssets == nil || potion == nil ||
		cmd.SelectedCharacterID == 0 || cmd.SourceListType != listTypeMain ||
		cmd.SourceSlotIndex < 0 || cmd.ItemTemplateID <= 0 ||
		resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" ||
		potion.EffectDurationSeconds <= 0 {
		return UseStackableResult{}, ErrStatChangePotionContract
	}
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" {
		return UseStackableResult{}, ErrStatChangePotionContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	now := time.Now().UTC()
	if o.now != nil {
		now = o.now().UTC()
	}
	duration := time.Duration(potion.EffectDurationSeconds) * time.Second

	var result UseStackableResult
	err := dnfrepo.WithinRentalAssetsScoped(ctx, o.rentalAssets, accountID, characterID, func(
		_ dnfrepo.AccountRepository,
		characters dnfrepo.CharacterRepository,
		inventoryRepo dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return ErrCharacterRequired
		}
		if owner := strings.TrimSpace(character.AccountID); owner != "" && owner != accountID {
			return ErrStatChangePotionContract
		}
		record, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := record.Slots[key]
		if !found || stack.Count <= 0 || stack.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: list=%d slot=%d item=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID)
		}
		// 持久行自己的 PVF 路径必须与运行期解析一致：旧行可能带着别的道具的路径。
		if durablePath := strings.TrimSpace(stack.Extra["pvf_path"]); durablePath != "" &&
			!strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) {
			return fmt.Errorf("%w: item=%d durable_path=%q resolved=%q",
				ErrStatChangePotionContract, resolution.ItemID, durablePath, resolution.PVFPath)
		}

		// 增益窗口先算出来：ActivateEpicDropRate 会改 character，必须在保存之前调用。
		//
		// 数值来源有两条路（见 alignedcmd.StatChangePotionResolution）：
		//   1. 权威：buff 表掷点 —— etc/variousbuffstackablelist.etc 的 (buff id, 权重)
		//      配上 etc/variousbufflist.etc 的 `epic_rarity_item_drop_rate` param。
		//      赛丽亚的幸运掉落药剂就是三档 60/30/10，只能服务端掷。
		//   2. 回退：表里没有条目（490000413 这类 2016 老活动券）⇒ 用说明文字的 200。
		// 换档/换药剂时要把旧 buff 撤掉（参考服务端同一族只用一条 buff 槽）。
		previousBuffID := int64(0)
		if previous, previouslyActive := statbuff.ActiveEpicDropRate(character, now); previouslyActive {
			previousBuffID = previous.BuffID
		}
		appliedPercent, appliedBuffID := currentStatChangePotionRoll(potion, o.statChangeRandom)
		expireAt := int64(0)
		if appliedPercent > 0 {
			effect, activateErr := statbuff.ActivateEpicDropRate(
				&character, resolution.ItemID, appliedBuffID, appliedPercent, duration, now)
			if activateErr != nil {
				return fmt.Errorf("%w: item=%d: %v", ErrStatChangePotionContract, resolution.ItemID, activateErr)
			}
			appliedPercent = effect.MultiplierPercent
			appliedBuffID = effect.BuffID
			expireAt = effect.ExpireAt.Unix()
		}

		// 强化/增幅成功率窗口：同一族药剂、同一笔事务、同一套投影，只是作用对象不同。
		// 服主 2026-10-03："看看增幅的秘密 强化秘药这类增加强化增幅成功率的道具 也一并修复了"。
		// 加成来源：buff 表掷点（赛丽亚的幸运强化药剂 1097/1098/1099 = 10/20/30），
		// 或者 PVF 没有数字时的固定常量（强化秘药/强化的奥秘/永恒的增幅的秘密）。
		upgradeKind := ""
		upgradePercent := int64(0)
		upgradeBuffID := int64(0)
		upgradeExpireAt := int64(0)
		previousUpgradeBuffID := int64(0)
		if statKind, kindOK := statbuff.KindForUpgradeMode(potion.UpgradeKind); kindOK {
			if previous, previouslyActive := statbuff.ActiveRate(character, statKind, now); previouslyActive {
				previousUpgradeBuffID = previous.BuffID
			}
			bonus, buffID := currentUpgradeRatePotionRoll(potion, o.statChangeRandom)
			if bonus > 0 {
				window, activateErr := statbuff.ActivateRate(
					&character, statKind, resolution.ItemID, buffID, bonus, duration, now)
				if activateErr != nil {
					return fmt.Errorf("%w: item=%d upgrade: %v", ErrStatChangePotionContract, resolution.ItemID, activateErr)
				}
				upgradeKind = potion.UpgradeKind
				upgradePercent = window.Percent
				upgradeBuffID = window.BuffID
				upgradeExpireAt = window.ExpireAt.Unix()
			}
		}

		// 地下城通关经验窗口（赛丽亚的幸运经验药剂 10008222，三档 50/100/200）。
		// 与掉落率同构：经验是服务端发的，所以必须服务端记窗口。
		clearExpPercent := int64(0)
		clearExpBuffID := int64(0)
		clearExpExpireAt := int64(0)
		if len(potion.DungeonClearExpRolls) > 0 {
			bonus, buffID := currentWeightedStatChangeRoll(potion.DungeonClearExpRolls, o.statChangeRandom)
			if bonus > 0 {
				window, activateErr := statbuff.ActivateRate(&character, statbuff.KindDungeonClearExp,
					resolution.ItemID, buffID, bonus, duration, now)
				if activateErr != nil {
					return fmt.Errorf("%w: item=%d clear-exp: %v", ErrStatChangePotionContract, resolution.ItemID, activateErr)
				}
				clearExpPercent, clearExpBuffID, clearExpExpireAt = window.Percent, window.BuffID, window.ExpireAt.Unix()
			}
		}

		// 客户端自施放的那一族（城镇移速 / 进图属性…）：服务端只掷点 + 记住窗口，
		// 效果由客户端按同一份 buff 表自己施加，窗口按 family 分开存。
		clientBuffFamily := ""
		clientBuffID := int64(0)
		clientBuffPercent := int64(0)
		clientBuffExpireAt := int64(0)
		if strings.TrimSpace(potion.ClientBuffFamily) != "" && len(potion.ClientBuffRolls) > 0 {
			bonus, buffID := currentWeightedStatChangeRoll(potion.ClientBuffRolls, o.statChangeRandom)
			if buffID > 0 {
				window, activateErr := statbuff.ActivateClientBuff(&character, potion.ClientBuffFamily,
					resolution.ItemID, buffID, bonus, duration, now)
				if activateErr != nil {
					return fmt.Errorf("%w: item=%d client buff %q: %v",
						ErrStatChangePotionContract, resolution.ItemID, potion.ClientBuffFamily, activateErr)
				}
				clientBuffFamily = window.Family
				clientBuffID, clientBuffPercent = window.BuffID, window.Percent
				clientBuffExpireAt = window.ExpireAt.Unix()
			}
		}

		remaining := stack.Count
		if !cmd.ConsumableFree {
			remaining = stack.Count - 1
			if remaining == 0 {
				delete(record.Slots, key)
			} else {
				stack = cloneStack(stack)
				stack.Count = remaining
				updateStackRawAmount(&stack)
				record.Slots[key] = stack
			}
		}
		record.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, record, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if appliedPercent > 0 || upgradePercent > 0 || clearExpPercent > 0 || clientBuffID > 0 {
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		result = UseStackableResult{
			CharacterID:                characterID,
			AccountID:                  accountID,
			ListType:                   cmd.SourceListType,
			SlotIndex:                  cmd.SourceSlotIndex,
			InstanceValue:              cmd.SourceInstanceValue,
			ItemID:                     stack.ItemID,
			RemainingCount:             remaining,
			Changed:                    true,
			PVFPath:                    strings.TrimSpace(resolution.PVFPath),
			StackableType:              strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")),
			StatChangePotionApplied:    true,
			StatChangePotionSeconds:    potion.EffectDurationSeconds,
			EpicDropRatePercent:        appliedPercent,
			EpicDropRateBuffID:         appliedBuffID,
			PreviousEpicDropRateBuffID: previousBuffID,
			EpicDropRateExpireAt:       expireAt,
			UpgradeRateKind:            upgradeKind,
			UpgradeRatePercent:         upgradePercent,
			UpgradeRateBuffID:          upgradeBuffID,
			PreviousUpgradeRateBuffID:  previousUpgradeBuffID,
			UpgradeRateExpireAt:        upgradeExpireAt,
			DungeonClearExpPercent:     clearExpPercent,
			DungeonClearExpBuffID:      clearExpBuffID,
			DungeonClearExpExpireAt:    clearExpExpireAt,
			ClientBuffFamily:           clientBuffFamily,
			ClientBuffID:               clientBuffID,
			ClientBuffPercent:          clientBuffPercent,
			ClientBuffExpireAt:         clientBuffExpireAt,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

// currentStatChangePotionRoll 按权重从 buff 表里掷一个结果。
//
// 返回 (倍率百分比, buff id)：倍率 = 100 + buff 的 `epic_rarity_item_drop_rate` param
// （1136 param=100 ⇒ 200 = 2 倍；1239 param=10 ⇒ 110；1240 param=5 ⇒ 105）。
// 表里没有条目时回退到 resolution.EpicDropRatePercent（说明文字口径），buff id 记 0。
//
// random 传 nil（或表为空/权重非法）时按"第一条有效的"取：宁可确定性，也不要为了掷点
// 让一条已经证明可用的路径随机失败。
func currentStatChangePotionRoll(
	potion *alignedcmd.StatChangePotionResolution,
	random func(int) int,
) (int64, int64) {
	if potion == nil {
		return 0, 0
	}
	weighted := make([]alignedcmd.StatChangePotionBuffRoll, 0, len(potion.BuffRolls))
	totalWeight := 0
	for _, roll := range potion.BuffRolls {
		if roll.Weight <= 0 || roll.BonusPercent <= 0 {
			continue
		}
		weighted = append(weighted, roll)
		totalWeight += int(roll.Weight)
	}
	if len(weighted) > 0 {
		pick := 0
		if random != nil && totalWeight > 0 {
			pick = random(totalWeight)
			if pick < 0 || pick >= totalWeight {
				pick = 0
			}
		}
		accumulated := 0
		for _, roll := range weighted {
			accumulated += int(roll.Weight)
			if pick < accumulated {
				return 100 + roll.BonusPercent, roll.BuffID
			}
		}
		last := weighted[len(weighted)-1]
		return 100 + last.BonusPercent, last.BuffID
	}
	if potion.EpicDropRatePercent > 0 {
		return potion.EpicDropRatePercent, 0
	}
	return 0, 0
}

// applyCurrentUpgradeSuccessWindow 把"强化/增幅成功率药剂"的窗口作用到成功权重上。
//
// 服主 2026-10-03 问："具体逻辑应该是 在服务端决定是否成功前增加成功率？成功率应该提升
// 多少？" —— 对，就是在**掷点之前**放大成功权重，位置与既有"高级炉岩炭"完全一致
// （applyUpgradeHighUpgradeTerms，handler.go）。
//
// 算法：weight × (100 + N) / 100，N 取 PVF 的 param（赛丽亚三档 10/20/30）或常量 10。
// 为什么是**乘法**而不是"直接加 N 个百分点"：本仓库已有先例就是乘法 —— 高级炉岩炭的
// PVF 原文是"使用高级炉岩炭时的强化成功率为普通炉岩炭的110%"，实现即
// `weight × 110 / 100`。乘法也是两者中较保守的一侧（+30 在 40% 的成功率上得到 52%，
// 而不是 70%）。若服主要改成"直接加 N 个百分点"，把这里换成
// `weight + weight*0 + N*currentUpgradeSuccessBase/100` 即可，是一行的事。
//
// 权重是万分率（currentUpgradeSuccessBase = 100000），上限封在必成（= 100000）。
// 窗口过期/模式不匹配/字段缺失一律原样返回 —— 绝不 fail-open。
func applyCurrentUpgradeSuccessWindow(
	weight int,
	character dnfrepo.CharacterRecord,
	mode string,
	now time.Time,
) int {
	if weight <= 0 {
		return weight
	}
	kind, ok := statbuff.KindForUpgradeMode(mode)
	if !ok {
		return weight
	}
	window, active := statbuff.ActiveRate(character, kind, now)
	if !active || window.Percent <= 0 {
		return weight
	}
	scaled := weight * int(100+window.Percent) / 100
	if scaled > currentUpgradeSuccessBase {
		scaled = currentUpgradeSuccessBase
	}
	return scaled
}

// currentWeightedStatChangeRoll 按权重从任意一组"服务端效果候选"里掷一个结果。
//
// 强化成功率、通关经验、客户端 family 三族共用（形状完全一样）。返回 (加成值, buff id)：
// 表里有候选就掷点；random 为 nil（或权重全非法）时按第一条有效的取 —— 宁可确定性，
// 也不要为了掷点让一条已经证明可用的路径随机失败。
func currentWeightedStatChangeRoll(
	rolls []alignedcmd.StatChangePotionWeightedRoll,
	random func(int) int,
) (int64, int64) {
	weighted := make([]alignedcmd.StatChangePotionWeightedRoll, 0, len(rolls))
	totalWeight := 0
	for _, roll := range rolls {
		if roll.Weight <= 0 || roll.BuffID <= 0 {
			continue
		}
		weighted = append(weighted, roll)
		totalWeight += int(roll.Weight)
	}
	if len(weighted) == 0 {
		return 0, 0
	}
	pick := 0
	if random != nil && totalWeight > 0 {
		pick = random(totalWeight)
		if pick < 0 || pick >= totalWeight {
			pick = 0
		}
	}
	accumulated := 0
	for _, roll := range weighted {
		accumulated += int(roll.Weight)
		if pick < accumulated {
			return roll.BonusPercent, roll.BuffID
		}
	}
	last := weighted[len(weighted)-1]
	return last.BonusPercent, last.BuffID
}

// currentUpgradeRatePotionRoll 按权重从**强化/增幅成功率**候选里掷一个结果。
//
// 返回 (加成百分点, buff id)：与掉落率那条同构 —— 表里有候选就掷点，没有就用固定常量。
//   - 赛丽亚的幸运强化药剂：1097 权重60(+10) / 1098 权重30(+20) / 1099 权重10(+30)
//   - 强化秘药 / 强化的奥秘 / 永恒的增幅的秘密：PVF 无数值 ⇒ UpgradeBonusPercent（10）
func currentUpgradeRatePotionRoll(
	potion *alignedcmd.StatChangePotionResolution,
	random func(int) int,
) (int64, int64) {
	if potion == nil {
		return 0, 0
	}
	weighted := make([]alignedcmd.StatChangePotionWeightedRoll, 0, len(potion.UpgradeRolls))
	totalWeight := 0
	for _, roll := range potion.UpgradeRolls {
		if roll.Weight <= 0 || roll.BonusPercent <= 0 {
			continue
		}
		weighted = append(weighted, roll)
		totalWeight += int(roll.Weight)
	}
	if len(weighted) > 0 {
		pick := 0
		if random != nil && totalWeight > 0 {
			pick = random(totalWeight)
			if pick < 0 || pick >= totalWeight {
				pick = 0
			}
		}
		accumulated := 0
		for _, roll := range weighted {
			accumulated += int(roll.Weight)
			if pick < accumulated {
				return roll.BonusPercent, roll.BuffID
			}
		}
		last := weighted[len(weighted)-1]
		return last.BonusPercent, last.BuffID
	}
	if potion.UpgradeBonusPercent > 0 {
		return potion.UpgradeBonusPercent, 0
	}
	return 0, 0
}
