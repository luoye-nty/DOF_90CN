package dnfbridge

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfitemcooldown "longheng.io/server/internal/modules/dnf/itemcooldown"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

var fatiguePotionMaximumRemainingPattern = regexp.MustCompile(`(?i)(?:低于|below)\s*([0-9]+)`)

// currentPVFItemCooltimeGroup 返回某件道具的 PVF `[cooltime group]`（0 = 无组）。
//
// 它必须能解析**已经在角色冷却表里的**item id：冷却检查要判断"上一次用的是不是同组
// 的另一件道具"（PVF [cooltime group]，例如 66 组里的首日/次日/三日礼盒共用一天冷却）。
// 解析不到、或那件道具本身没有 [cooltime maintenance] 时返回 0 = 不参与共享。
func (s *Service) currentPVFItemCooltimeGroup(itemID int64) int64 {
	if s == nil || itemID <= 0 || itemID > int64(^uint32(0)) {
		return 0
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return 0
	}
	definition, err := catalog.ResolveItem(uint32(itemID))
	if err != nil || !definition.HasCooltimeMaintenance {
		return 0
	}
	if definition.CooltimeGroup <= 0 {
		return 0
	}
	return definition.CooltimeGroup
}

// alignedStackableUseResolverForCommand authorizes ordinary type-44 use and
// action-based quest tickets from the active PVF. Inventory rows from older
// grants may carry the real PVF path
// but lack stackable_type; the owner combines this resolver with that durable
// path. Main-inventory use remains restricted to [waste], while list-7 pet
// consumable use additionally requires a complete [feed] int-data contract.
func (s *Service) alignedStackableUseResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.StackableUseResolver, error) {
	if opcode != dnfenum.CmdPacketUseStackable && opcode != dnfenum.CmdPacketUseStackableAction &&
		opcode != dnfenum.CmdPacketRequestEventServerLevelUp && opcode != dnfenum.CmdPacketIncreaseStatus &&
		opcode != dnfenum.CmdPacketUpgradeInventory && opcode != dnfenum.CmdPacketUpgradeCargo {
		// op30 carries no item identity, so its owner resolves the durable row
		// through this same resolver before anything is consumed.
		//
		// op430/op408（扩容券）同样没有 item id，只有一个槽位；owner 必须靠这个
		// 解析器把"槽上那件东西到底是不是扩容券、是哪一种"从运行期 PVF 判出来。
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	return func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		if itemID <= 0 || itemID > int64(^uint32(0)) {
			return alignedcmd.StackableUseResolution{}, nil
		}
		definition, resolveErr := catalog.ResolveItem(uint32(itemID))
		if resolveErr != nil {
			return alignedcmd.StackableUseResolution{}, fmt.Errorf("resolve stackable-use item=%d: %w", itemID, resolveErr)
		}
		if definition.Kind != dungeonDropItemStackable {
			return alignedcmd.StackableUseResolution{}, nil
		}
		resolution := alignedcmd.StackableUseResolution{
			ItemID:        itemID,
			PVFPath:       definition.PVFPath,
			StackableType: definition.StackableType,
		}
		// PVF [total usable count] is the item's own 每天可用次数 cap and is not
		// fatigue-specific: the parser already records it for every stackable,
		// so every consumable that declares it gets the shared per-character
		// daily counter (inventory.Owner.ReserveDailyItemUse) plus its op541
		// projection. A missing field leaves the limit at zero = no cap and no
		// extra work.
		if definition.TotalUsableCount > 0 {
			resolution.DailyUseLimit = definition.TotalUsableCount
		}
		// PVF 冷却：只有声明了 `[cooltime maintenance]`（值非零）的道具才由服务端**持久**
		// 保留冷却窗口并据此拒绝再次使用。客户端的冷却门只在进程内内存里，小退重上就没了
		// （现场 2026-09-17：1 天冷却的消耗品重上后又能用）。[cool time] 是毫秒，
		// [cooltime group] 让同组道具共享一个窗口（0 = 只锁自己）。
		if definition.HasCooltimeMaintenance && definition.CooltimeMS > 0 {
			if seconds := dnfitemcooldown.MillisecondsToSeconds(definition.CooltimeMS); seconds > 0 {
				resolution.MaintainsCooltime = true
				resolution.CooltimeSeconds = seconds
				resolution.CooltimeGroup = definition.CooltimeGroup
			}
		}
		admissionTicket, admissionErr := s.currentPVFDungeonAdmissionTicket(definition)
		if admissionErr != nil {
			return alignedcmd.StackableUseResolution{}, fmt.Errorf("resolve dungeon-admission ticket item=%d path=%s: %w", itemID, definition.PVFPath, admissionErr)
		}
		if admissionTicket != nil {
			resolution.DungeonAdmissionTicket = admissionTicket
			return resolution, nil
		}
		// 塔罗牌（`[monster tarot card]`）：金币花费 + 加权奖池。分支就挂在这条既有
		// 的 op44 分派链上，与别的 [stackable type] 同一写法，没有新通道。
		if normalizeDungeonDropStackableType(definition.StackableType) == "[monster tarot card]" {
			lottery, lotteryErr := s.resolveCurrentTarotLotteryCard(definition)
			if lotteryErr != nil {
				return alignedcmd.StackableUseResolution{}, lotteryErr
			}
			resolution.TarotLottery = lottery
			return resolution, nil
		}
		if strings.EqualFold(normalizeDungeonDropStackableType(definition.StackableType), "[expert town potion]") &&
			normalizeDungeonDropStackableType(definition.ActionType) == "[add fatigue]" {
			if definition.ActionValues == nil || len(definition.ActionValues.Values) != 1 || definition.ActionValues.Values[0] <= 0 {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf("resolve fatigue potion item=%d path=%s: invalid add-fatigue values=%v", itemID, definition.PVFPath, definition.ActionValues)
			}
			maximumRemaining := int64(0)
			if matches := fatiguePotionMaximumRemainingPattern.FindStringSubmatch(definition.Explain); len(matches) == 2 {
				fmt.Sscan(matches[1], &maximumRemaining)
			}
			resolution.FatigueRecovery = definition.ActionValues.Values[0]
			resolution.FatigueMaximumRemaining = maximumRemaining
			return resolution, nil
		}
		// The current client routes [action type] `[rent point]` through
		// op515 action index 105 (see currentRentalPointActionIndex). The
		// grant comes from the item's own action value; the ceiling is the
		// rental system's [limit point], so both travel with the resolution
		// and the wallet stays clamped inside the commit transaction.
		if normalizeDungeonDropStackableType(definition.ActionType) == "[rent point]" {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[waste]" ||
				definition.ActionValues == nil || len(definition.ActionValues.Values) != 1 ||
				definition.ActionValues.Values[0] <= 0 || definition.ActionValues.Values[0] > int64(^uint32(0)) {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve rental point item=%d path=%s: incomplete [rent point] contract type=%q values=%v",
					itemID,
					definition.PVFPath,
					definition.StackableType,
					definition.ActionValues,
				)
			}
			catalog, catalogErr := s.currentRentalCatalogOnly(context.Background())
			if catalogErr != nil {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf("resolve rental point item=%d limit: %w", itemID, catalogErr)
			}
			if catalog == nil || catalog.Limit == 0 {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf("resolve rental point item=%d: rental [limit point] unavailable", itemID)
			}
			resolution.RentalPoint = &alignedcmd.RentalPointGrantResolution{
				Grant: uint32(definition.ActionValues.Values[0]),
				Limit: catalog.Limit,
			}
			return resolution, nil
		}
		// 扩容券三兄弟：`[action type]` 为 [upgrade inventory] / [upgrade cargo] /
		// [upgrade account cargo]。现场 2026-10-03（测试x）：使用后没效果 —— 服务端对
		// op430/op408 完全没有接线，解析器里也没有这三个 action type 的分支。
		//
		// 全库共 100 件道具声明这三种 action（40 / 47 / 13），**全部**是
		// `[stackable type] [etc]` + `[action usable place] [village]`，所以按家族判定，
		// 不按 id 硬编码。
		if upgradeKind, upgradeKindOK := currentContainerUpgradeKind(definition.ActionType); upgradeKindOK {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve container-upgrade voucher item=%d path=%s: action=%q requires a bare [etc] stackable, got type=%q",
					itemID,
					definition.PVFPath,
					definition.ActionType,
					definition.StackableType,
				)
			}
			if !definition.ActionUsableVillage {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve container-upgrade voucher item=%d path=%s: action=%q requires [action usable place] [village]",
					itemID,
					definition.PVFPath,
					definition.ActionType,
				)
			}
			resolution.ContainerUpgrade = &alignedcmd.ContainerUpgradeResolution{
				Kind:   upgradeKind,
				ItemID: itemID,
			}
			return resolution, nil
		}
		if normalizeCustomAbilityGroupConverterAction(definition.ActionType) {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" || definition.MinimumLevel <= 0 {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve custom-ability group converter item=%d path=%s: incomplete [etc] contract type=%q minimum_level=%d",
					itemID,
					definition.PVFPath,
					definition.StackableType,
					definition.MinimumLevel,
				)
			}
			resolution.CustomAbilityGroupConverter = &alignedcmd.CustomAbilityGroupConverterResolution{
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
			}
			return resolution, nil
		}
		// 城镇自施放增益药剂（史诗Buff药剂一族）：`[stackable type] [expert town potion]`
		// + **没有** `[action type]` + `[stat change duration]`。
		//
		// 现场 2026-10-03（服主报，角色 测试x = char 11）：史诗Buff药剂 490002458
		// 连点 15 次，op44 每一次都被 useStackableWithResolvers 的
		// "requires PVF-proven [waste] provenance" 那道门静默拒掉（error 17）：
		// 既没消耗、也没有效果，玩家看到的就是"没反应"。
		//
		// 这一族的效果是**服务端量**（深渊派对史诗掉落率），所以服务端必须自己记住
		// 增益窗口，见 internal/modules/dnf/statbuff + dungeon_hell_party_drop.go。
		//
		// 这里**不**要求 `[action usable place]`：那是"使用地点的限制"，不是"这是增益
		// 药剂"的证据。全库有 9 件同族根本没写这个字段（赛丽亚的幸运*药剂、强化秘药、
		// 和平的呐喊…），客户端自己也按同一份 PVF 判地点，服务端照做即可。
		if currentStatChangePotionDefinition(definition) {
			potion := &alignedcmd.StatChangePotionResolution{
				EffectDurationSeconds: currentStatChangePotionEffectDurationSeconds(definition),
			}
			// 权威来源：两张 buff 表（道具→buff+概率、buff→效果数值）。
			// 同一张概率表里可能混着不同效果的 buff，按 effect name 分流：
			//   掉落率 / 强化成功率 / 通关经验 —— 服务端要自己算的三种；
			//   其余（城镇移速 village_move_speed、进图属性 move_speed …）——
			//   客户端认得这些 buff、自己按 buff 表施加，服务端只要把掷到的 buff 注册过去。
			if tables, tablesOK := catalog.currentVariousBuffTables(); tablesOK {
				for _, roll := range tables.rollsForItem(itemID, currentEpicRarityDropRateEffect) {
					effect := tables.ByBuff[roll.BuffID]
					bonus, bonusOK := effect.dropRateBonusPercent()
					if !bonusOK {
						continue
					}
					potion.BuffRolls = append(potion.BuffRolls, alignedcmd.StatChangePotionBuffRoll{
						BuffID:       roll.BuffID,
						Weight:       roll.Weight,
						BonusPercent: bonus,
					})
				}
				for _, roll := range tables.rollsForItem(itemID, currentUpgradeSuccessRateEffect) {
					effect := tables.ByBuff[roll.BuffID]
					bonus, bonusOK := effect.upgradeSuccessBonusPercent()
					if !bonusOK {
						continue
					}
					potion.UpgradeKind = currentUpgradeRatePotionReinforce
					potion.UpgradeRolls = append(potion.UpgradeRolls, alignedcmd.StatChangePotionWeightedRoll{
						BuffID:       roll.BuffID,
						Weight:       roll.Weight,
						BonusPercent: bonus,
					})
				}
				for _, roll := range tables.rollsForItem(itemID, currentDungeonClearExpRateEffect) {
					effect := tables.ByBuff[roll.BuffID]
					bonus, bonusOK := effect.dungeonClearExpBonusPercent()
					if !bonusOK {
						continue
					}
					potion.DungeonClearExpRolls = append(potion.DungeonClearExpRolls,
						alignedcmd.StatChangePotionWeightedRoll{
							BuffID:       roll.BuffID,
							Weight:       roll.Weight,
							BonusPercent: bonus,
						})
				}
				// 剩下的族：客户端自施放（服务端只负责注册 buff）。
				family, familyRolls := tables.clientBuffRollsForItem(itemID)
				if len(familyRolls) > 0 {
					potion.ClientBuffFamily = family
					for _, roll := range familyRolls {
						potion.ClientBuffRolls = append(potion.ClientBuffRolls, alignedcmd.StatChangePotionWeightedRoll{
							BuffID:       roll.BuffID,
							Weight:       roll.Weight,
							BonusPercent: roll.BonusPercent,
						})
					}
				}
			}
			// 回退：表里没有条目（490000413 这类 2016 老活动券）才看说明文字。
			if len(potion.BuffRolls) == 0 {
				potion.EpicDropRatePercent = currentEpicDropRatePotionPercent(definition.Explain)
			}
			// 强化秘药这类"表里查不到、说明文字只说能提升强化成功率"的道具：给固定加成。
			if potion.UpgradeKind == "" && len(potion.UpgradeRolls) == 0 &&
				currentUpgradeRatePotionExplainKind(definition.Explain) == currentUpgradeRatePotionReinforce {
				potion.UpgradeKind = currentUpgradeRatePotionReinforce
				potion.UpgradeBonusPercent = currentUpgradeRatePotionDefaultBonusPercent
			}
			resolution.StatChangePotion = potion
			return resolution, nil
		}
		// 强化/增幅成功率药剂：`[stackable type] [item upgrad buff usual upgrade]` /
		// `[item upgrad buff amplify upgrade]`（强化的奥秘 / 永恒的增幅的秘密）。
		// 它们不是 [expert town potion] 户籍，所以走单独一条判据；这两类在 PVF 里同样
		// **没有数字字段**，加成取同一个常量。
		if kind, kindOK := currentUpgradeRatePotionStackableKind(definition.StackableType); kindOK &&
			definition.EffectDurationMS > 0 {
			resolution.StatChangePotion = &alignedcmd.StatChangePotionResolution{
				EffectDurationSeconds: currentStatChangePotionEffectDurationSeconds(definition),
				UpgradeKind:           kind,
				UpgradeBonusPercent:   currentUpgradeRatePotionDefaultBonusPercent,
			}
			return resolution, nil
		}
		avatarFittingTicket, fittingErr := currentPVFAvatarFittingRoomTicket(catalog.source, definition)
		if fittingErr != nil {
			return alignedcmd.StackableUseResolution{}, fittingErr
		}
		if avatarFittingTicket {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" ||
				definition.MinimumLevel <= 0 ||
				(!definition.ActionUsableVillage && !definition.ActionUsableSeriaRoom) {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve avatar-fitting ticket item=%d path=%s: incomplete [etc] contract type=%q minimum_level=%d village=%t seria=%t",
					itemID,
					definition.PVFPath,
					definition.StackableType,
					definition.MinimumLevel,
					definition.ActionUsableVillage,
					definition.ActionUsableSeriaRoom,
				)
			}
			resolution.AvatarFittingRoomTicket = &alignedcmd.AvatarFittingRoomTicketResolution{
				MinimumLevel:   definition.MinimumLevel,
				MaximumLevel:   definition.MaximumLevel,
				VillageAllowed: definition.ActionUsableVillage,
				SeriaAllowed:   definition.ActionUsableSeriaRoom,
			}
			return resolution, nil
		}
		if book, ok := currentPVFSkillPointBook(definition.PVFPath); ok {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" ||
				definition.MinimumLevel <= 0 ||
				(book.Kind != alignedcmd.SkillPointBookResetTP && book.Delta <= 0) {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve SP/TP book item=%d path=%s: incomplete [etc] contract type=%q minimum_level=%d kind=%d delta=%d",
					itemID,
					definition.PVFPath,
					definition.StackableType,
					definition.MinimumLevel,
					book.Kind,
					book.Delta,
				)
			}
			book.MinimumLevel = definition.MinimumLevel
			book.MaximumLevel = definition.MaximumLevel
			resolution.TPBook = &alignedcmd.TPBookResolution{
				Kind:         book.Kind,
				Delta:        book.Delta,
				MinimumLevel: book.MinimumLevel,
				MaximumLevel: book.MaximumLevel,
			}
			return resolution, nil
		}
		if actionType := normalizeStoryBookAction(definition.ActionType); actionType != "" {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[waste]" ||
				definition.ActionValues == nil || len(definition.ActionValues.Values) != 1 ||
				definition.ActionValues.Values[0] <= 0 || definition.ActionValues.Values[0] > int64(^uint16(0)) {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve story-book item=%d path=%s: invalid contract type=%q values=%v",
					itemID,
					definition.PVFPath,
					definition.StackableType,
					definition.ActionValues,
				)
			}
			resolution.StoryBook = &alignedcmd.StoryBookResolution{
				ActionType:   actionType,
				QuestID:      definition.ActionValues.Values[0],
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
			}
			return resolution, nil
		}
		actionType := normalizeQuestCompletionTicketAction(definition.ActionType)
		if actionType != "" {
			var actionValues []int64
			if definition.ActionValues != nil {
				actionValues = append([]int64(nil), definition.ActionValues.Values...)
			}
			resolution.QuestCompletionTicket = &alignedcmd.QuestCompletionTicketResolution{
				ActionType:   actionType,
				ActionValues: actionValues,
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
				VillageOnly:  definition.ActionVillageOnly,
			}
			return resolution, nil
		}
		if normalizeLevelUpTicketAction(definition.ActionType) != "" {
			if definition.LevelUpRange <= 0 || definition.LevelUpRange > int64(currentAdventureCharacterLevelCap) {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve level-up ticket item=%d path=%s: invalid levelup range %d",
					itemID,
					definition.PVFPath,
					definition.LevelUpRange,
				)
			}
			levelUpRange, rangeErr := rollLevelUpTicketRange(definition)
			if rangeErr != nil {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve level-up ticket item=%d path=%s outcome: %w",
					itemID,
					definition.PVFPath,
					rangeErr,
				)
			}
			// The current PVF models level-up tickets as an action rather than an
			// [increase status type]. Keep both mutations explicit: the direct
			// progression plan raises the character by [levelup range], while the
			// quest plan completes the eligible pre-upgrade story/ordinary chain
			// without ordinary rewards. The inventory owner recognizes the pair
			// and commits them with the source decrement in one settlement.
			resolution.DirectExperience = &alignedcmd.DirectExperienceConsumableResolution{
				ItemID:       itemID,
				PVFPath:      definition.PVFPath,
				EffectType:   "[level up ticket]",
				EffectValues: []int64{levelUpRange},
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
			}
			resolution.QuestCompletionTicket = &alignedcmd.QuestCompletionTicketResolution{
				ActionType:   "[quest clear]",
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
				VillageOnly:  definition.ActionVillageOnly,
			}
			return resolution, nil
		}
		directExperienceType := normalizeDirectExperienceType(definition.DirectExperienceType)
		if directExperienceType != "" {
			if validateErr := validateDirectExperienceValues(directExperienceType, definition.DirectExperienceValues); validateErr != nil {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve direct-experience item=%d path=%s: %w",
					itemID,
					definition.PVFPath,
					validateErr,
				)
			}
			resolution.DirectExperience = &alignedcmd.DirectExperienceConsumableResolution{
				ItemID:       itemID,
				PVFPath:      definition.PVFPath,
				EffectType:   directExperienceType,
				EffectValues: append([]int64(nil), definition.DirectExperienceValues...),
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
			}
			return resolution, nil
		}
		if amount, ok := currentPVFCharacterExperienceBook(definition.PVFPath); ok {
			if auditErr := auditCurrentPVFCharacterExperienceBook(definition, amount); auditErr != nil {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve character experience book item=%d path=%s: %w",
					itemID,
					definition.PVFPath,
					auditErr,
				)
			}
			resolution.DirectExperience = &alignedcmd.DirectExperienceConsumableResolution{
				ItemID:       itemID,
				PVFPath:      definition.PVFPath,
				EffectType:   "[expup]",
				EffectValues: []int64{amount},
				MinimumLevel: definition.MinimumLevel,
				MaximumLevel: definition.MaximumLevel,
			}
			return resolution, nil
		}
		transformation, transformErr := currentPVFTransformationUseResolution(definition)
		if transformErr != nil {
			return alignedcmd.StackableUseResolution{}, fmt.Errorf(
				"resolve transformation item=%d path=%s: %w",
				itemID,
				definition.PVFPath,
				transformErr,
			)
		}
		if transformation != nil {
			resolution.Transformation = transformation
			return resolution, nil
		}
		projectile, projectileErr := currentPVFProjectileUseResolution(definition)
		if projectileErr != nil {
			return alignedcmd.StackableUseResolution{}, fmt.Errorf(
				"resolve projectile item=%d path=%s: %w",
				itemID,
				definition.PVFPath,
				projectileErr,
			)
		}
		if projectile != nil {
			resolution.Projectile = projectile
			return resolution, nil
		}
		switch normalized := normalizeDungeonDropStackableType(definition.StackableType); {
		case strings.HasPrefix(normalized, "[waste]"):
			if definition.ActionUsableVillage || definition.ActionUsableDungeon || definition.ActionUsableSeriaRoom ||
				len(definition.ActionUsableDungeonIDs) > 0 || len(definition.LimitDungeonIDs) > 0 {
				resolution.UsePlace = &alignedcmd.StackableUsePlaceResolution{
					VillageAllowed:         definition.ActionUsableVillage,
					DungeonAllowed:         definition.ActionUsableDungeon || len(definition.ActionUsableDungeonIDs) > 0 || len(definition.LimitDungeonIDs) > 0,
					SeriaRoomAllowed:       definition.ActionUsableSeriaRoom,
					ActionUsableDungeonIDs: append([]int64(nil), definition.ActionUsableDungeonIDs...),
					LimitDungeonIDs:        append([]int64(nil), definition.LimitDungeonIDs...),
				}
			}
			return resolution, nil
		case normalized == "[feed]":
			if definition.PetFeedEffectDurationSeconds <= 0 ||
				definition.PetFeedSkillChargePercent >= 0 ||
				definition.PetFeedSatietyDelta <= 0 || definition.PetFeedSatietyDelta > 100 {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf(
					"resolve pet-feed item=%d path=%s: invalid int data duration=%d skill_charge=%d satiety=%d",
					itemID,
					definition.PVFPath,
					definition.PetFeedEffectDurationSeconds,
					definition.PetFeedSkillChargePercent,
					definition.PetFeedSatietyDelta,
				)
			}
			resolution.PetFeedEffectDurationSeconds = definition.PetFeedEffectDurationSeconds
			resolution.PetFeedSkillChargePercent = definition.PetFeedSkillChargePercent
			resolution.PetFeedSatietyDelta = definition.PetFeedSatietyDelta
			return resolution, nil
		default:
			return alignedcmd.StackableUseResolution{}, nil
		}
	}, nil
}

// currentContainerUpgradeKind 识别扩容券的三条 [action type]。
//
// 口径见 alignedcmd.ContainerUpgradeResolution 的注释：现场 2026-10-03 三种券使用后
// 没效果的根因就是这里没人认它们。
func currentContainerUpgradeKind(actionType string) (alignedcmd.ContainerUpgradeKind, bool) {
	switch normalizeDungeonDropStackableType(actionType) {
	case "[upgrade inventory]":
		return alignedcmd.ContainerUpgradeInventory, true
	case "[upgrade cargo]":
		return alignedcmd.ContainerUpgradeCargo, true
	case "[upgrade account cargo]":
		return alignedcmd.ContainerUpgradeAccountCargo, true
	default:
		return "", false
	}
}

// currentStatChangePotionDefinition 判定一件道具是不是"城镇自施放增益药剂"。
//
// 判据全部来自运行期 PVF，**不看物品 id、也不看名字**：
//
//	[stackable type] `[expert town potion]`   —— 户籍（本族全库 711 件）
//	没有 [action type]                        —— 有 action 的走各自通道
//	                                              （[add fatigue] 走 op515 action 58）
//	[stat change duration] > 0                —— 它自己声明了"我是限时增益"
//
// 三条同时成立才认。为什么这就是"证据"：`[stat change duration]` 是"给自己挂一个
// 限时 stat 变化"的专用字段，配上 [expert town potion] 户籍与"没有 action"，
// 形状只有这一种含义；反过来，有 action 的道具（疲劳药、摆件、竞猜币…）统统留给
// 它们自己的通道，一条也不抢。
//
// 名字里有没有"史诗"完全不参与判定：判据命中的 32 件里既有 15 个 id 的史诗Buff药剂，
// 也有"小灯笼的大瓶·小瓶史诗Buff药剂""欧皇药水""城镇移速药剂""有机甜瓜汁""强化秘药"
// "和平的呐喊"——它们都是同一族。是否**影响掉落率**另有一道判据（说明文字里有"掉落率"）。
func currentStatChangePotionDefinition(definition dungeonDropItemDefinition) bool {
	if normalizeDungeonDropStackableType(definition.StackableType) != "[expert town potion]" {
		return false
	}
	if strings.TrimSpace(definition.ActionType) != "" {
		return false
	}
	return definition.EffectDurationMS > 0
}

// currentStatChangePotionEffectDurationSeconds 返回这次使用要落的窗口长度（秒）。
//
// 口径就是 PVF 的 `[stat change duration]`（本族 600000ms = 10 分钟）。2026-10-03 服主
// 做过一次"硬编码 1 分钟"的过期验证（确认图标会被正确摘掉），验证完已按服主要求改回
// PVF 口径 —— 这里**不留**任何测试旋钮。
func currentStatChangePotionEffectDurationSeconds(definition dungeonDropItemDefinition) int64 {
	return definition.EffectDurationMS / 1000
}

// 强化/增幅成功率药剂（服主 2026-10-03："看看增幅的秘密 强化秘药这类增加强化增幅成功率的
// 道具 也一并修复了"）。判定只认 PVF 的 `[stackable type]`，不看 id、不看名字。
const (
	// currentUpgradeRatePotionReinforce 对应 `[stackable type] [item upgrad buff usual upgrade]`
	// （强化成功率，例：强化的奥秘 490007878）。
	currentUpgradeRatePotionReinforce = "reinforce"
	// currentUpgradeRatePotionAmplify 对应 `[stackable type] [item upgrad buff amplify upgrade]`
	// （增幅成功率，例：永恒的增幅的秘密 490001534）。
	currentUpgradeRatePotionAmplify = "amplify"

	// currentUpgradeRatePotionDefaultBonusPercent 是"PVF 里没有数字"那几件的固定加成
	// （百分点）。取值依据（同一份 PVF 内的证据，不是拍脑袋）：
	//
	//	crystallight_10007343（SAO 联动）: "使用后，强化成功率增加10%%，效果持续30秒"
	//	fortuneaura_success:            "在该频道中，增加10%%的强化成功率，效果持续60分钟"
	//	赛丽亚的幸运强化药剂的三档:      10（下级）/ 20（中级）/ 30（上级）
	//
	// 强化秘药、强化的奥秘、永恒的增幅的秘密都只写"可以提升强化/增幅成功率"，没有数字，
	// 所以取这三处共同的下限值 10。要调整就改这一个常量（档位表里的 10/20/30 不受影响）。
	currentUpgradeRatePotionDefaultBonusPercent int64 = 10
)

// currentUpgradeRatePotionStackableKind 识别"专用 stackable type"的强化/增幅药剂。
func currentUpgradeRatePotionStackableKind(stackableType string) (string, bool) {
	switch normalizeDungeonDropStackableType(stackableType) {
	case "[item upgrad buff usual upgrade]":
		return currentUpgradeRatePotionReinforce, true
	case "[item upgrad buff amplify upgrade]":
		return currentUpgradeRatePotionAmplify, true
	}
	return "", false
}

// currentUpgradeRatePotionExplainKind 是最后一道回退：`[expert town potion]` 户籍、两张 buff
// 表里都没有条目时看说明文字。目前命中这一条的就是"强化秘药"（"可以提升强化成功率"）。
func currentUpgradeRatePotionExplainKind(explain string) string {
	if !strings.Contains(explain, "成功率") {
		return ""
	}
	switch {
	case strings.Contains(explain, "强化成功率"):
		return currentUpgradeRatePotionReinforce
	case strings.Contains(explain, "增幅成功率"):
		return currentUpgradeRatePotionAmplify
	}
	return ""
}

// currentEpicDropRatePotionMultiplierPercent 是本族药剂对"深渊派对史诗掉落率"的倍率。
//
// 200 = 2 倍。为什么是常量而不是读 PVF：这一族在 PVF 里**根本没有数字字段**
// （`[stat change duration]` 只是窗口长度；`[icon mark]` 只是图标），"2 倍"只写在
// `[explain]` 的说明文字里，而官方服务端二进制里也查不到这些 id
// （_investigation_顶号_official/epicbuff_scan.py 扫过 490000413/490002458/…/10008219，
// 全部 0 命中）。所以口径取家族同名道具自己的说明："史诗装备掉落率变为2倍"。
//
// 说明文字里只写"增加/小幅增加"而没有倍数的几件（小灯笼的大瓶·小瓶、欧皇药水）
// 同样按 200 处理：它们共享同一个 10 分钟窗口与 `[cooltime group] 251`，
// 所以不存在叠加放大；这一点在文档里显式记下来，运营若要区分可以再补表。
const currentEpicDropRatePotionMultiplierPercent int64 = 200

// currentEpicDropRatePotionPercent 从道具说明判断它是否影响掉落率。
//
// 只有说明里明确写"掉落率"的才给倍率：经验药剂、强化秘药、和平的呐喊、城镇移动速度
// 药剂这些同族道具说明里没有"掉落率"，返回 0 = 服务端不改掉落（药剂照样能正常使用）。
func currentEpicDropRatePotionPercent(explain string) int64 {
	if !strings.Contains(strings.TrimSpace(explain), "掉落率") {
		return 0
	}
	return currentEpicDropRatePotionMultiplierPercent
}

func (s *Service) currentPVFDungeonAdmissionTicket(definition dungeonDropItemDefinition) (*alignedcmd.DungeonAdmissionTicketResolution, error) {
	actionType := normalizeDungeonDropStackableType(definition.ActionType)
	if actionType != "[add dimension dungeon enter count]" && actionType != "[up layer despair dungeon]" {
		return nil, nil
	}
	if normalizeDungeonDropStackableType(definition.StackableType) != "[waste]" ||
		definition.ActionValues == nil || len(definition.ActionValues.Values) != 1 ||
		definition.ActionValues.Values[0] <= 0 || definition.MinimumLevel <= 0 ||
		(!definition.ActionUsableVillage && !definition.ActionUsableSeriaRoom) {
		return nil, fmt.Errorf(
			"incomplete contract type=%q values=%v minimum_level=%d village=%t seria=%t",
			definition.StackableType,
			definition.ActionValues,
			definition.MinimumLevel,
			definition.ActionUsableVillage,
			definition.ActionUsableSeriaRoom,
		)
	}
	s.worldMapMu.RLock()
	catalog := s.dungeonAdmissionCatalog
	s.worldMapMu.RUnlock()
	if catalog == nil {
		return nil, dnfdungeonadmission.ErrOwnerUnavailable
	}
	if definition.UseActionPacket < 0 || definition.UseActionPacket > int64(^uint32(0)) {
		return nil, fmt.Errorf("invalid use action packet=%d", definition.UseActionPacket)
	}
	ticket := &alignedcmd.DungeonAdmissionTicketResolution{
		ActionType:       actionType,
		ActionPacket:     uint32(definition.UseActionPacket),
		MinimumLevel:     definition.MinimumLevel,
		MaximumLevel:     definition.MaximumLevel,
		VillageAllowed:   definition.ActionUsableVillage,
		SeriaRoomAllowed: definition.ActionUsableSeriaRoom,
	}
	switch actionType {
	case "[add dimension dungeon enter count]":
		amount := definition.ActionValues.Values[0]
		if amount > 5 {
			return nil, fmt.Errorf("dimension-gate allowance %d exceeds the current-PVF daily additional cap 5", amount)
		}
		policy, found := catalog.Policy("dimension_gate.character.daily")
		if !found || policy.Scope != dnfdungeonadmission.ScopeCharacter || policy.Period != dnfdungeonadmission.PeriodDaily {
			return nil, fmt.Errorf("dimension-gate admission policy is unavailable")
		}
		ticket.Grants = []dnfdungeonadmission.AllowanceGrant{{Policy: policy, Amount: uint32(amount), MaximumBonus: 5}}
	case "[up layer despair dungeon]":
		target := definition.ActionValues.Values[0]
		var prefix string
		switch target {
		case 11008:
			prefix = "tower_of_despair"
		case 5115:
			prefix = "tower_of_grief"
		default:
			// Preserve the ordinary native-waste path for an unowned tower
			// family instead of consuming it against a guessed counter.
			return nil, nil
		}
		ticket.TargetDungeonID = uint32(target)
		for _, key := range []string{prefix + ".character.daily", prefix + ".account.daily"} {
			policy, found := catalog.Policy(key)
			if !found || policy.Period != dnfdungeonadmission.PeriodDaily || !dungeonAdmissionPolicyContains(policy, ticket.TargetDungeonID) {
				return nil, fmt.Errorf("tower admission policy %s is unavailable", key)
			}
			ticket.Grants = append(ticket.Grants, dnfdungeonadmission.AllowanceGrant{Policy: policy, Amount: 1, MaximumBonus: 2})
		}
		useLimit := definition.TotalUsableCount
		if useLimit < 0 || useLimit > 2 {
			return nil, fmt.Errorf("tower recommendation daily use count=%d exceeds supported maximum 2", useLimit)
		}
		if useLimit > 0 {
			ticket.UseLimit = dnfdungeonadmission.AllowanceUseLimit{
				Key: fmt.Sprintf("%s.recommendation.item.%d.daily", prefix, definition.ItemID), Limit: uint32(useLimit),
			}
		}
	}
	return ticket, nil
}

func dungeonAdmissionPolicyContains(policy dnfdungeonadmission.Policy, dungeonID uint32) bool {
	for _, candidate := range policy.DungeonIDs {
		if candidate == dungeonID {
			return true
		}
	}
	return false
}

func normalizeCustomAbilityGroupConverterAction(value string) bool {
	return strings.EqualFold(
		strings.TrimSpace(strings.ReplaceAll(value, "`", "")),
		"[custom ability set group]",
	)
}

func normalizeAvatarFittingRoomTicketAction(value string) bool {
	return strings.EqualFold(
		strings.TrimSpace(strings.ReplaceAll(value, "`", "")),
		"[avatar fitting room ticket]",
	)
}

func currentPVFAvatarFittingRoomTicket(source dnfpvf.Source, definition dungeonDropItemDefinition) (bool, error) {
	if normalizeAvatarFittingRoomTicketAction(definition.ActionType) {
		return true, nil
	}
	if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" {
		return false, nil
	}
	if source == nil || strings.TrimSpace(definition.PVFPath) == "" {
		return false, nil
	}
	text, err := source.ReadText(definition.PVFPath)
	if err != nil {
		return false, fmt.Errorf("read avatar-fitting ticket %s: %w", definition.PVFPath, err)
	}
	document, err := dnfpvf.Parse(definition.PVFPath, text)
	if err != nil {
		return false, fmt.Errorf("parse avatar-fitting ticket %s: %w", definition.PVFPath, err)
	}
	_, found := document.Section("avatar fitting room ticket")
	return found, nil
}

func currentPVFTransformationUseResolution(definition dungeonDropItemDefinition) (*alignedcmd.TransformationUseResolution, error) {
	stackableType := normalizeDungeonDropStackableType(definition.StackableType)
	actionType := normalizeDungeonDropStackableType(definition.ActionType)
	transformation := &alignedcmd.TransformationUseResolution{
		VillageAllowed:         definition.ActionUsableVillage,
		DungeonAllowed:         definition.ActionUsableDungeon,
		SeriaRoomAllowed:       definition.ActionUsableSeriaRoom,
		DisallowAssault:        definition.ActionDisallowAssault,
		ActionUsableDungeonIDs: append([]int64(nil), definition.ActionUsableDungeonIDs...),
		LimitDungeonIDs:        append([]int64(nil), definition.LimitDungeonIDs...),
	}
	hasExplicitPlace := transformation.VillageAllowed || transformation.DungeonAllowed || transformation.SeriaRoomAllowed

	switch {
	case stackableType == "[disguise]" || stackableType == "[random disguise]":
		transformation.ActionType = stackableType
		transformation.FormKinds = append([]string(nil), definition.ActionStringData...)
		if len(transformation.FormKinds) == 0 || len(definition.ActionIntData) < 2 || len(definition.ActionIntData)%2 != 0 {
			return nil, fmt.Errorf("%s requires form kind(s) and (form,duration) pairs: strings=%v ints=%v", stackableType, definition.ActionStringData, definition.ActionIntData)
		}
		for index := 0; index < len(definition.ActionIntData); index += 2 {
			formID := definition.ActionIntData[index]
			durationSeconds := definition.ActionIntData[index+1]
			if formID <= 0 || durationSeconds <= 0 || durationSeconds > int64(^uint32(0)) {
				return nil, fmt.Errorf("%s invalid form/duration pair (%d,%d)", stackableType, formID, durationSeconds)
			}
			transformation.FormIDs = append(transformation.FormIDs, formID)
			if transformation.DurationSeconds == 0 {
				transformation.DurationSeconds = durationSeconds
			}
		}
		if !hasExplicitPlace {
			transformation.VillageAllowed = true
			transformation.SeriaRoomAllowed = true
		}
	case actionType == "[riding]":
		if stackableType != "[waste]" || definition.ActionValues == nil || len(definition.ActionValues.Values) != 2 ||
			definition.ActionValues.Values[0] <= 0 || definition.ActionValues.Values[1] <= 0 ||
			definition.ActionValues.Values[1] > int64(^uint32(0)) {
			return nil, fmt.Errorf("[riding] requires [waste] and positive (form,duration_ms): type=%q values=%v", definition.StackableType, definition.ActionValues)
		}
		transformation.ActionType = actionType
		transformation.FormIDs = []int64{definition.ActionValues.Values[0]}
		transformation.DurationSeconds = (definition.ActionValues.Values[1] + 999) / 1000
		if !hasExplicitPlace {
			transformation.DungeonAllowed = true
		}
	case actionType == "[ridable item]":
		if stackableType != "[waste]" || definition.ActionValues == nil || len(definition.ActionValues.Values) == 0 || definition.ActionValues.Values[0] <= 0 {
			return nil, fmt.Errorf("[ridable item] requires [waste] and a positive form: type=%q values=%v", definition.StackableType, definition.ActionValues)
		}
		transformation.ActionType = actionType
		transformation.FormIDs = []int64{definition.ActionValues.Values[0]}
		if !hasExplicitPlace {
			transformation.DungeonAllowed = true
		}
	case actionType == "[celebrate big transition]":
		if stackableType != "[etc]" || definition.ActionValues == nil || len(definition.ActionValues.Values) == 0 || len(definition.ActionIntData) == 0 || definition.ActionIntData[0] <= 0 {
			return nil, fmt.Errorf("[celebrate big transition] requires [etc], form list and duration: type=%q values=%v ints=%v", definition.StackableType, definition.ActionValues, definition.ActionIntData)
		}
		for _, formID := range definition.ActionValues.Values {
			if formID <= 0 {
				return nil, fmt.Errorf("[celebrate big transition] invalid form id %d", formID)
			}
		}
		transformation.ActionType = actionType
		transformation.FormIDs = append([]int64(nil), definition.ActionValues.Values...)
		transformation.DurationSeconds = definition.ActionIntData[0]
		if !hasExplicitPlace {
			transformation.VillageAllowed = true
			transformation.SeriaRoomAllowed = true
		}
	default:
		return nil, nil
	}
	return transformation, nil
}

func currentPVFProjectileUseResolution(definition dungeonDropItemDefinition) (*alignedcmd.ProjectileUseResolution, error) {
	stackableType := normalizeDungeonDropStackableType(definition.StackableType)
	if stackableType != "[throw]" && stackableType != "[set]" {
		return nil, nil
	}
	if len(definition.ActionIntData) == 0 {
		return nil, fmt.Errorf("%s requires a passive-object/effect id: %v", stackableType, definition.ActionIntData)
	}
	if definition.ActionIntData[0] <= 0 {
		// The active PVF contains one APC-only dummy throw whose documented
		// effect id is zero. It intentionally plays only a motion and must not
		// become a player-consumable server action.
		return nil, nil
	}
	for _, value := range definition.ActionIntData {
		if value < 0 {
			return nil, fmt.Errorf("%s has a negative int-data operand: %v", stackableType, definition.ActionIntData)
		}
	}
	projectile := &alignedcmd.ProjectileUseResolution{
		StackableType:          stackableType,
		EffectID:               definition.ActionIntData[0],
		IntData:                append([]int64(nil), definition.ActionIntData...),
		ParticlePaths:          append([]string(nil), definition.ActionStringData...),
		DungeonAllowed:         definition.ActionUsableDungeon,
		ActionUsableDungeonIDs: append([]int64(nil), definition.ActionUsableDungeonIDs...),
		LimitDungeonIDs:        append([]int64(nil), definition.LimitDungeonIDs...),
	}
	if !definition.ActionUsableVillage && !definition.ActionUsableDungeon && !definition.ActionUsableSeriaRoom {
		projectile.DungeonAllowed = true
	}
	return projectile, nil
}

func currentPVFSkillPointBook(pvfPath string) (alignedcmd.TPBookResolution, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(pvfPath, "\\", "/")))
	switch normalized {
	case "stackable/book_skill1.stk", "stackable/extention/test_book_skill1.stk":
		return alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddSP, Delta: 5}, true
	case "stackable/book_skill2.stk":
		return alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddSP, Delta: 20}, true
	case "stackable/book_fskill1.stk":
		return alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddTP, Delta: 1}, true
	case "stackable/book_fskill2.stk":
		return alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddTP, Delta: 5}, true
	case "stackable/book_fskillreset.stk", "stackable/book_fskillreset_event.stk":
		return alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookResetTP}, true
	default:
		return alignedcmd.TPBookResolution{}, false
	}
}

// currentPVFCharacterExperienceBook covers the six numbered character
// experience books. Unlike every growth capsule they declare no
// [increase status type], no action type and no operand at all: the active PVF
// entry is a bare [etc] stackable whose only numeric statement is its
// localized name. The amount therefore cannot be read out of the document and
// has to be declared here, keyed by PVF path like currentPVFSkillPointBook.
//
// Deliberately absent, and refused by the ordinary bare-[etc] fallthrough: the
// duel experience books (1121-1123) and the guild experience books
// (1200-1202), which spend separate experience currencies this server keeps no
// ledger for. The TP reset book is covered by currentPVFSkillPointBook.
func currentPVFCharacterExperienceBook(pvfPath string) (int64, bool) {
	switch strings.ToLower(strings.TrimSpace(strings.ReplaceAll(pvfPath, "\\", "/"))) {
	case "stackable/book_exp1.stk":
		return 100, true
	case "stackable/book_exp2.stk":
		return 1000, true
	case "stackable/book_exp3.stk":
		return 10000, true
	case "stackable/book_exp4.stk":
		return 100000, true
	case "stackable/book_exp5.stk":
		return 1000000, true
	case "stackable/book_exp6.stk":
		return 10000000, true
	default:
		return 0, false
	}
}

// currentPVFCharacterExperienceBookName is the name statement the declared
// amount has to reproduce. The books carry their own amount in their localized
// name, so this is what keeps the table above honest: a wrong number stops
// matching the runtime document and the resolution fails instead of granting
// experience nobody wrote down.
func currentPVFCharacterExperienceBookName(amount int64) string {
	return fmt.Sprintf("增加%d经验值", amount)
}

// auditCurrentPVFCharacterExperienceBook validates one table row against the
// runtime document. The table is only a fallback for a document that states
// nothing, so any declared effect on the entry is an error rather than
// something to shadow.
func auditCurrentPVFCharacterExperienceBook(definition dungeonDropItemDefinition, amount int64) error {
	if stackableType := normalizeDungeonDropStackableType(definition.StackableType); stackableType != "[etc]" {
		return fmt.Errorf("expected a bare [etc] stackable, got type=%q", definition.StackableType)
	}
	if actionType := strings.TrimSpace(strings.ReplaceAll(definition.ActionType, "`", "")); actionType != "" {
		return fmt.Errorf("entry declares action type %q, so the declared amount would shadow it", actionType)
	}
	if effectType := strings.TrimSpace(strings.ReplaceAll(definition.DirectExperienceType, "`", "")); effectType != "" {
		return fmt.Errorf("entry declares increase status type %q, so the declared amount would shadow it", effectType)
	}
	if amount <= 0 || amount > int64(^uint32(0)) {
		return fmt.Errorf("declared amount %d is out of range", amount)
	}
	want := currentPVFCharacterExperienceBookName(amount)
	if !strings.HasPrefix(strings.TrimSpace(definition.Name), want) {
		return fmt.Errorf("name %q does not begin with %q for the declared amount %d", definition.Name, want, amount)
	}
	return nil
}

func rollLevelUpTicketRange(definition dungeonDropItemDefinition) (int64, error) {
	if definition.LevelUpRange <= 0 {
		return 0, fmt.Errorf("levelup range %d is invalid", definition.LevelUpRange)
	}
	if len(definition.LevelUpOutcomes) == 0 {
		return definition.LevelUpRange, nil
	}
	var total int64
	for _, outcome := range definition.LevelUpOutcomes {
		if outcome.Range <= 0 || outcome.Range > definition.LevelUpRange || outcome.Weight <= 0 ||
			total > int64(^uint64(0)>>1)-outcome.Weight {
			return 0, fmt.Errorf("invalid weighted outcome %+v", outcome)
		}
		total += outcome.Weight
	}
	roll, err := secureCurrentBoosterRoll(total)
	if err != nil {
		return 0, err
	}
	for _, outcome := range definition.LevelUpOutcomes {
		if roll < outcome.Weight {
			return outcome.Range, nil
		}
		roll -= outcome.Weight
	}
	return 0, fmt.Errorf("weighted outcome total %d did not resolve", total)
}

func normalizeLevelUpTicketAction(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
	if value == "[level up ticket]" {
		return value
	}
	return ""
}

func normalizeStoryBookAction(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
	if value == "[story book]" {
		return value
	}
	return ""
}

func normalizeDirectExperienceType(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
	switch value {
	case "[expup]", "[exppercentup]", "[expupbypercent]", "[expupbytable]", "[levelupbytable]", "[level up ticket]":
		// The active PVF uses both spellings for the same one-operand,
		// percentage-of-current-level effect.  Keep one canonical type so the
		// transaction and its validation remain identical for every capsule.
		if value == "[expupbypercent]" {
			return "[exppercentup]"
		}
		return value
	default:
		return ""
	}
}

func validateDirectExperienceValues(effectType string, values []int64) error {
	switch effectType {
	case "[expup]":
		if len(values) != 1 || values[0] <= 0 || values[0] > int64(^uint32(0)) {
			return fmt.Errorf("invalid expUp operands %v", values)
		}
	case "[exppercentup]":
		if len(values) != 1 || values[0] <= 0 || values[0] > 100 {
			return fmt.Errorf("invalid expPercentUp operands %v", values)
		}
	case "[expupbytable]", "[levelupbytable]":
		if len(values) == 0 || len(values)%3 != 0 {
			return fmt.Errorf("invalid table operands %v", values)
		}
		for index := 0; index < len(values); index += 3 {
			minimum, maximum, result := values[index], values[index+1], values[index+2]
			if minimum <= 0 || maximum < minimum || result <= 0 ||
				(effectType == "[expupbytable]" && result > int64(^uint32(0))) {
				return fmt.Errorf("invalid table row %v", values[index:index+3])
			}
		}
	default:
		return fmt.Errorf("unsupported direct-experience type %q", effectType)
	}
	return nil
}

func normalizeQuestCompletionTicketAction(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
	switch value {
	case "[any quest clear]",
		"[achievement quest clear]",
		"[first awaken clear]",
		"[second awaken clear]",
		"[quest clear]",
		"[quest clear coupon]",
		"[lv clear]",
		"[gm quest clear]",
		"[side quest clear]":
		return value
	default:
		return ""
	}
}
