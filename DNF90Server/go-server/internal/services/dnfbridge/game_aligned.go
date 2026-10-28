// 本文件把已对齐的 DNF 旧客户端命令转交给模块化 handler。
// bridge 只负责分流和发包，不在这里补业务状态或伪造成功结果。
package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/avatartitle"
	"longheng.io/server/internal/modules/dnf/cargo"
	"longheng.io/server/internal/modules/dnf/character"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	"longheng.io/server/internal/modules/dnf/inventory"
	"longheng.io/server/internal/modules/dnf/itemlock"
	"longheng.io/server/internal/modules/dnf/mail"
	"longheng.io/server/internal/modules/dnf/packageitem"
	"longheng.io/server/internal/modules/dnf/party"
	"longheng.io/server/internal/modules/dnf/pet"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	"longheng.io/server/internal/modules/dnf/quest"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

var errAlignedPostCommitProjectionDeferred = errors.New("aligned post-commit projection deferred")

var gameAlignedRegistry = alignedcmd.NewRegistry(
	avatartitle.NewHandler(),
	cargo.NewHandler(),
	character.NewHandler(),
	dungeoncmd.NewHandler(),
	inventory.NewHandler(),
	itemlock.NewHandler(),
	mail.NewHandler(),
	packageitem.NewHandler(),
	party.NewHandler(),
	pet.NewHandler(),
	quest.NewHandler(),
	raid.NewHandler(),
	skillcmd.NewHandler(),
)

// handleAlignedGameCommand 把协议号已对齐、但尚未落业务实现的命令归入模块边界。
// 当前只记录分流证据，不伪造成功 ACK；具体回包必须等对应模块按 EXE/日志补齐。
func (s *Service) handleAlignedGameCommand(session *gameSession, cmd byte, typ uint16, body []byte) (bool, error) {
	// 宠物蛋判据是进程级的、由运行时 PVF 证明的，装配一次即可；未装配时
	// creaturestate 保持改动前的行为。
	s.installPetEggProbeOnce()
	// 「累计在线得好礼」（原生 228 面板，本服 nativePCRoom 生效）的领取按钮走
	// cmd=1/type=632，动作体固定 2 字节：body[0] 是当日档位掩码 1/2/4/8，
	// 或 16 表示"领累计档"、此时 body[1] 是档位下标（0..3）；{0,255} 是纯刷新。
	//
	// 这个请求以前没有任何路由，handleCurrentPCRoomTimePoint 只被定义、从未被调用，
	// 所以客户端点按钮时服务端只记一条 "unhandled latest game command"，游戏里
	// 表现就是"点了完全没反应"。处理器的回执不是 632 成功体（该 EXE 没有 632 的
	// success handler），而是通知 561 的状态刷新加邮件告警，见 pcroom_timepoint.go。
	if typ == uint16(dnfenum.CmdPacketGetPcroomTimePointItem) {
		s.logInfo("dnfbridge pcroom timepoint claim received",
			"conn_id", session.connID,
			"type", typ,
			"body_len", len(body))
		return true, s.handleCurrentPCRoomTimePoint(session, body)
	}
	// 「充满爱慕的信」这类 [use favor point] 道具的 op515 是两段式：第一段（右键使用，
	// 槽位里还有券）由 inventory 的 ArmFavorLetter 扣券并登记；第二段（弹窗里选完 NPC
	// 按按钮，槽位已空但有登记）要在这里把点数落到那个 NPC 上并回执，否则客户端一直
	// 等不到"好感度已加"的回应，弹窗不关。见 favor_letter_action.go。
	if handled, err := s.tryCurrentFavorLetterAction(session, typ, body); handled || err != nil {
		return handled, err
	}
	if handled, err := s.handleCurrentAssaultCommand(session, typ, body); handled || err != nil {
		return handled, err
	}
	if handled, err := s.handleCurrentChatEmoticonCommand(session, typ, body); handled || err != nil {
		return handled, err
	}
	if typ == uint16(dnfenum.CmdPacketDeleteItem) &&
		(currentSessionDungeonIsPractice(session) || currentSessionDungeonIsTrainingRoom(session)) {
		// Practice and the training room both send the cube-delete command for
		// skills that cost 无色小晶块. Acknowledge its empty protobuf result
		// without mutating inventory.
		return true, s.sendGameUpperRaw(session, typ, []byte{1, 4, 0, 0, 0, 0x08, 0, 0x18, 0})
	}
	// 无色小晶块消耗埋点（纯观测）：放在练习场/修炼道场提前返回**之后**，
	// 所以只统计真实副本里的技能消耗；这里只累加计数，不改回包、不碰背包。
	if typ == uint16(dnfenum.CmdPacketDeleteItem) {
		s.noteCurrentColorlessSpendFromCommand(session, body)
	}
	if typ == uint16(dnfenum.CmdPacketUseStackable) &&
		(currentSessionDungeonIsPractice(session) ||
			(currentSessionDungeonIsTrainingRoom(session) && currentDungeonTrainingRoomRefusesConsumables())) {
		return true, s.sendGameUpperFailure(session, typ, 1)
	}
	if typ == uint16(dnfenum.CmdPacketCompoundItem) {
		return true, s.handleCurrentCompoundRecipe(session, body)
	}
	if typ == uint16(dnfenum.CmdPacketUseStackable) {
		if handled, err := s.handleCurrentExpertJobRecipeLearning(session, body); handled || err != nil {
			return handled, err
		}
	}
	if handled, err := s.handleOnlineItemTradeCommand(session, typ, body); handled || err != nil {
		return handled, err
	}
	if handled, err := s.handleOnlineRaidCommand(session, typ, body); handled || err != nil {
		return handled, err
	}
	if handled, err := s.handleOnlineSocialCommand(session, typ, body); handled || err != nil {
		return handled, err
	}
	if handled, err := s.handleOnlinePartyCommand(session, typ, body); handled || err != nil {
		return handled, err
	}
	repos, _ := s.repositoryGroup()
	accountID := s.accountIDForSession(session)
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketMailboxOpen {
		s.reconcileCurrentMailboxAttachmentExpirationsBeforeOpen(session)
	}
	var skillCatalog *dnfskill.Table
	var initialSkillLevels map[uint16]int
	var skillPointBaseline *dnfrepo.SkillPointState
	var equipmentPlacement alignedcmd.EquipmentPlacementValidator
	var guardianShieldResolver alignedcmd.GuardianShieldResolver
	var petHatchResolver alignedcmd.PetHatchResolver
	// dojoSkills 决定技能命令是否写进修炼道场自己的两页（内部页 2/3）。
	var dojoSkills bool
	if alignedSkillOwnerRulesRequired(dnfenum.CmdPacket(typ)) {
		// Resolve the PVF-backed skill owner inputs under the same bounded
		// lifetime as the mutation route. A previous op29 can be holding a
		// repository write gate; using Background here made the next learn
		// request wait forever before the handler had a chance to return the
		// native failure ACK that releases the client's pending operation.
		rulesBaseContext := context.Background()
		if session != nil && session.ctx != nil {
			rulesBaseContext = session.ctx
		}
		rulesContext, rulesCancel := context.WithTimeout(rulesBaseContext, createWriteTimeout)
		skillCatalog, initialSkillLevels, skillPointBaseline = s.skillOwnerRules(rulesContext, repos, session.selectedCharacterID)
		rulesCancel()
		var dojoErr error
		dojoSkills, dojoErr = s.currentPVPDojoSkillsActive(session)
		if dojoErr != nil {
			return true, dojoErr
		}
	}
	petHatchResolver, petCatalogErr := s.alignedPetHatchResolverForCommand(dnfenum.CmdPacket(typ))
	if petCatalogErr != nil {
		s.logPacketEvent("game-aligned-pet-hatch-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", petCatalogErr)
		s.logInfo("dnfbridge pet hatch resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", petCatalogErr)
	}
	enchantBeadResolver, enchantCatalogErr := s.alignedEnchantBeadResolverForCommand(dnfenum.CmdPacket(typ))
	if enchantCatalogErr != nil {
		s.logPacketEvent("game-aligned-enchant-bead-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", enchantCatalogErr)
		s.logInfo("dnfbridge enchant bead resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", enchantCatalogErr)
	}
	monsterCardUpgradeResolver, monsterCardUpgradeCatalogErr := s.alignedMonsterCardUpgradeResolverForCommand(dnfenum.CmdPacket(typ))
	if monsterCardUpgradeCatalogErr != nil {
		s.logPacketEvent("game-aligned-monster-card-upgrade-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", monsterCardUpgradeCatalogErr)
		s.logInfo("dnfbridge monster card upgrade resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", monsterCardUpgradeCatalogErr)
	}
	amplifyItemResolver, amplifyItemCatalogErr := s.alignedAmplifyItemResolverForCommand(dnfenum.CmdPacket(typ))
	if amplifyItemCatalogErr != nil {
		s.logPacketEvent("game-aligned-amplify-item-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", amplifyItemCatalogErr)
		s.logInfo("dnfbridge amplify item resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", amplifyItemCatalogErr)
	}
	randomOptionResolver, randomOptionCatalogErr := s.alignedRandomOptionResolverForCommand(dnfenum.CmdPacket(typ))
	if randomOptionCatalogErr != nil {
		s.logPacketEvent("game-aligned-random-option-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", randomOptionCatalogErr)
		s.logInfo("dnfbridge random option resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", randomOptionCatalogErr)
	}
	equipmentRegenerationResolver, equipmentRegenerationCatalogErr := s.alignedEquipmentRegenerationResolverForCommand(dnfenum.CmdPacket(typ))
	if equipmentRegenerationCatalogErr != nil {
		s.logPacketEvent("game-aligned-equipment-regeneration-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", equipmentRegenerationCatalogErr)
		s.logInfo("dnfbridge equipment regeneration resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", equipmentRegenerationCatalogErr)
	}
	upgradeTicketResolver, upgradeTicketCatalogErr := s.alignedUpgradeTicketResolverForCommand(dnfenum.CmdPacket(typ))
	if upgradeTicketCatalogErr != nil {
		s.logPacketEvent("game-aligned-upgrade-ticket-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", upgradeTicketCatalogErr)
		s.logInfo("dnfbridge upgrade ticket resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", upgradeTicketCatalogErr)
	}
	upgradeProtectionResolver, upgradeProtectionCatalogErr := s.alignedUpgradeProtectionResolverForCommand(dnfenum.CmdPacket(typ))
	if upgradeProtectionCatalogErr != nil {
		s.logPacketEvent("game-aligned-upgrade-protection-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", upgradeProtectionCatalogErr)
		s.logInfo("dnfbridge upgrade protection resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", upgradeProtectionCatalogErr)
	}
	upgradePolicyResolver := s.withCurrentWornUpgradeAssist(
		s.alignedUpgradePolicyResolver(), session, repos)
	// A 公会徽章 (guild flag, list-38) upgrade takes its policy from the native
	// flag_upgrade_table rather than the equipment upgrade table, so it replaces the
	// resolver wholesale -- upstream swaps it in at exactly this point.
	//
	// The worn upgrade assist is deliberately NOT wrapped around it: that assist
	// keys off equipment the character is wearing, and a flag sitting in the
	// guild-medal list is not worn equipment.
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketUpgradeItem {
		if medalRequest, decodeErr := inventory.DecodeUpgradeItemRequest(body); decodeErr == nil &&
			medalRequest.TargetListType == currentGuildMedalInventoryListType {
			upgradePolicyResolver = s.alignedGuildMedalUpgradePolicyResolver(uint32(medalRequest.TargetItemTemplateID))
		}
	}
	upgradeRefundMailer := s.currentUpgradeRefundMailer()
	forgingResolver, forgingCatalogErr := s.alignedForgingResolverForCommand(dnfenum.CmdPacket(typ))
	if forgingCatalogErr != nil {
		s.logPacketEvent("game-aligned-forging-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", forgingCatalogErr)
		s.logInfo("dnfbridge forging resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", forgingCatalogErr)
	}
	chronicleRefineResolver, chronicleRefineCatalogErr := s.alignedChronicleRefineResolverForCommand(dnfenum.CmdPacket(typ))
	if chronicleRefineCatalogErr != nil {
		s.logPacketEvent("game-aligned-chronicle-refine-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", chronicleRefineCatalogErr)
		s.logInfo("dnfbridge chronicle refine resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", chronicleRefineCatalogErr)
	}
	magicBoxResolver, magicBoxRewardResolver, magicBoxCatalogErr := s.alignedMagicBoxResolversForCommand(dnfenum.CmdPacket(typ))
	magicBoxContractActivator := func(account *dnfrepo.AccountRecord, units map[int64]int64, now time.Time) error {
		activation, activationErr := s.currentPremiumContractActivation(account, units, now)
		if activationErr != nil {
			// Returning the error makes the box owner place these units as
			// ordinary rewards, so the reward stays recoverable as an item row.
			s.logWarn("dnfbridge magic box contract activation fell back to ordinary placement",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"units", fmt.Sprint(units),
				"error", activationErr)
			return activationErr
		}
		if activation.Changed() {
			// The entitlement is committed with the box transaction; the client
			// only needs the ordinary premium activation projection.
			if err := s.sendCurrentPremiumInventoryActivations(session, activation, "aligned_magic_box_contract_direct_activation"); err != nil {
				s.logWarn("dnfbridge magic box contract activation projection deferred",
					"conn_id", session.connID,
					"char_id", session.selectedCharacterID,
					"error", err)
			}
			s.logGameEvent(session, "game-premium-contract-direct-activated",
				"source", "aligned_magic_box_contract_direct_activation",
				"char_id", session.selectedCharacterID,
				"units", fmt.Sprint(units),
				"activation_count", len(activation.Activations),
				"activations", fmt.Sprint(activation.Activations))
		}
		return nil
	}
	if magicBoxCatalogErr != nil {
		s.logPacketEvent("game-aligned-magic-box-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", magicBoxCatalogErr)
		s.logInfo("dnfbridge magic box resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", magicBoxCatalogErr)
	}
	premiumContractResolver, premiumCatalogErr := s.alignedPremiumContractResolverForCommand(dnfenum.CmdPacket(typ))
	if premiumCatalogErr != nil {
		s.logPacketEvent("game-aligned-premium-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", premiumCatalogErr)
		s.logInfo("dnfbridge premium contract resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", premiumCatalogErr)
	}
	experienceConsumableResolver, experienceCatalogErr := s.alignedExperienceConsumableResolverForCommand(dnfenum.CmdPacket(typ))
	if experienceCatalogErr != nil {
		s.logPacketEvent("game-aligned-experience-consumable-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", experienceCatalogErr)
		s.logInfo("dnfbridge experience-consumable resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", experienceCatalogErr)
	}
	chronicleGrowthResolver, chronicleCatalogErr := s.alignedChronicleGrowthResolverForCommand(dnfenum.CmdPacket(typ))
	if chronicleCatalogErr != nil {
		s.logPacketEvent("game-aligned-chronicle-growth-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", chronicleCatalogErr)
		s.logInfo("dnfbridge chronicle growth resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", chronicleCatalogErr)
	}
	damageFontResolver, damageFontCatalogErr := s.alignedDamageFontResolverForCommand(dnfenum.CmdPacket(typ))
	if damageFontCatalogErr != nil {
		s.logPacketEvent("game-aligned-damage-font-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", damageFontCatalogErr)
		s.logInfo("dnfbridge damage-font resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", damageFontCatalogErr)
	}
	randomRewardItemResolver, randomRewardCatalogErr := s.alignedRandomRewardItemResolverForCommand(dnfenum.CmdPacket(typ))
	if randomRewardCatalogErr != nil {
		s.logPacketEvent("game-aligned-random-reward-item-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", randomRewardCatalogErr)
		s.logInfo("dnfbridge random-reward-item resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", randomRewardCatalogErr)
	}
	repairCostResolver, repairCostCatalogErr := s.alignedRepairCostResolverForCommand(dnfenum.CmdPacket(typ))
	if repairCostCatalogErr != nil {
		s.logPacketEvent("game-aligned-repair-cost-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", repairCostCatalogErr)
		s.logInfo("dnfbridge repair cost resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", repairCostCatalogErr)
	}
	mailboxItemResolver, mailboxCatalogErr := s.alignedMailboxItemResolverForCommand(dnfenum.CmdPacket(typ))
	if mailboxCatalogErr != nil {
		s.logPacketEvent("game-aligned-mailbox-item-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", mailboxCatalogErr)
		s.logInfo("dnfbridge mailbox item resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", mailboxCatalogErr)
	}
	stackMergeResolver, stackMergeCatalogErr := s.alignedStackMergeResolverForCommand(dnfenum.CmdPacket(typ))
	if stackMergeCatalogErr != nil {
		s.logPacketEvent("game-aligned-stack-merge-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", stackMergeCatalogErr)
		s.logInfo("dnfbridge stack-merge resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", stackMergeCatalogErr)
	}
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketMoveItemspace {
		if currentPetMoveTouchesGrowthState(body) {
			if settleErr := s.settleCurrentPetGrowthClock(session, s.gameplayNow(), "current_exe_op19_before_pet_or_artifact_mutation"); settleErr != nil {
				s.logPacketEvent("game-pet-satiety-before-op19-deferred",
					"conn_id", session.connID,
					"selected_character_id", session.selectedCharacterID,
					"type", typ,
					"reason", "growth_clock_settlement_failed",
					"error", settleErr)
			}
		}
		placementContext, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		validator, placementErr := s.newCurrentEquipmentPlacementValidator(placementContext, accountID, session.selectedCharacterID, repos)
		cancel()
		if placementErr != nil {
			s.logPacketEvent("game-aligned-equipment-placement-validator-unavailable",
				"conn_id", session.connID,
				"account_id", accountID,
				"selected_character_id", session.selectedCharacterID,
				"type", typ,
				"error", placementErr)
		} else {
			equipmentPlacement = func(ctx context.Context, placement alignedcmd.EquipmentPlacementRequest) error {
				return validator.ValidateEquipmentPlacement(ctx, dnfequip.Placement{
					CharacterID:     placement.CharacterID,
					ItemID:          placement.ItemID,
					SourceListType:  placement.SourceListType,
					SourceSlotIndex: placement.SourceSlotIndex,
					TargetSlotIndex: placement.TargetSlotIndex,
				})
			}
		}
	}
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketChangeDeckInfo ||
		(dnfenum.CmdPacket(typ) == dnfenum.CmdPacketMoveItemspace && currentMoveTouchesGuardianShieldSpace(body)) {
		resolverContext, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		resolver, resolverErr := s.newCurrentGuardianShieldResolver(resolverContext)
		cancel()
		if resolverErr != nil {
			s.logPacketEvent("game-aligned-guardian-shield-resolver-unavailable",
				"conn_id", session.connID,
				"account_id", accountID,
				"selected_character_id", session.selectedCharacterID,
				"type", typ,
				"error", resolverErr)
		} else {
			guardianShieldResolver = resolver
		}
	}
	var petFeedSettlementErr error
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketUseStackable && currentPetConsumableUseTouchesGrowthState(body) {
		petFeedSettlementErr = s.settleCurrentPetGrowthClock(
			session,
			s.gameplayNow(),
			"current_exe_op44_before_pet_feed",
		)
		if petFeedSettlementErr != nil {
			s.logPacketEvent("game-pet-feed-before-op44-deferred",
				"conn_id", session.connID,
				"selected_character_id", session.selectedCharacterID,
				"type", typ,
				"reason", "growth_clock_settlement_failed",
				"error", petFeedSettlementErr)
		}
	}
	stackableUseResolver, stackableUseCatalogErr := s.alignedStackableUseResolverForCommand(dnfenum.CmdPacket(typ))
	if stackableUseCatalogErr != nil {
		s.logPacketEvent("game-aligned-stackable-use-resolver-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", stackableUseCatalogErr)
		s.logInfo("dnfbridge stackable-use resolver unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", stackableUseCatalogErr)
	}
	directExperiencePlanner, directExperienceCatalogErr := s.alignedDirectExperienceConsumablePlannerForCommand(dnfenum.CmdPacket(typ))
	if directExperienceCatalogErr != nil {
		s.logPacketEvent("game-aligned-direct-experience-planner-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", directExperienceCatalogErr)
		s.logInfo("dnfbridge direct-experience planner unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", directExperienceCatalogErr)
	}
	questCompletionTicketPlanner, questCompletionTicketCatalogErr := s.alignedQuestCompletionTicketPlannerForCommand(dnfenum.CmdPacket(typ))
	if questCompletionTicketCatalogErr != nil {
		s.logPacketEvent("game-aligned-quest-completion-ticket-planner-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", questCompletionTicketCatalogErr)
		s.logInfo("dnfbridge quest-completion-ticket planner unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", questCompletionTicketCatalogErr)
	}
	storyBookPlanner, storyBookCatalogErr := s.alignedStoryBookPlannerForCommand(dnfenum.CmdPacket(typ))
	if storyBookCatalogErr != nil {
		s.logPacketEvent("game-aligned-story-book-planner-unavailable",
			"conn_id", session.connID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", storyBookCatalogErr)
		s.logInfo("dnfbridge story-book planner unavailable",
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"error", storyBookCatalogErr)
	}
	if petFeedSettlementErr != nil && stackableUseResolver != nil {
		baseResolver := stackableUseResolver
		stackableUseResolver = func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			resolution, err := baseResolver(itemID)
			if err != nil {
				return alignedcmd.StackableUseResolution{}, err
			}
			if normalizeDungeonDropStackableType(resolution.StackableType) == "[feed]" {
				return alignedcmd.StackableUseResolution{}, fmt.Errorf("settle pet growth before feed: %w", petFeedSettlementErr)
			}
			return resolution, nil
		}
	}
	session.dungeon.mu.Lock()
	dungeonRuntime := session.dungeon.runtime
	inDungeon := dungeonRuntime != nil
	var dungeonID int64
	if dungeonRuntime != nil {
		dungeonID = dungeonRuntime.Dungeon.ID
	}
	session.dungeon.mu.Unlock()
	session.party.mu.Lock()
	routeBaseContext := context.Background()
	if session.ctx != nil {
		routeBaseContext = session.ctx
	}
	routeTimeout := createWriteTimeout
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketUseStackable {
		routeTimeout = currentConsumableWriteTimeout
	}
	routeContext, routeCancel := context.WithTimeout(routeBaseContext, routeTimeout)
	result, ok, err := gameAlignedRegistry.Route(routeContext, alignedcmd.Request{
		Command:                       cmd,
		CommandKnown:                  true,
		Opcode:                        typ,
		Body:                          body,
		AccountID:                     accountID,
		SelectedCharacterID:           session.selectedCharacterID,
		DojoSkills:                    dojoSkills,
		Repositories:                  repos,
		EquipmentPlacement:            equipmentPlacement,
		GuardianShield:                guardianShieldResolver,
		AvatarClassifier:              func(itemID uint32) (bool, bool) { return s.currentClassifyAvatarItem(itemID) },
		AuroraLookReplaceClassifier:   func(itemID uint32) bool { return s.currentIsAuroraLookReplaceAvatar(itemID) },
		StackMergeResolver:            stackMergeResolver,
		StackMergeResolverError:       stackMergeCatalogErr,
		AccountWarehouseSlotResolver:  currentAccountWarehouseFixedSlot,
		ItemCooltimeGroupResolver:     s.currentPVFItemCooltimeGroup,
		NameTagChecker:                func(itemID uint32) bool { return s.currentIsNameTagItem(itemID) },
		PetHatchResolver:              petHatchResolver,
		EnchantBeadResolver:           enchantBeadResolver,
		MonsterCardUpgradeResolver:    monsterCardUpgradeResolver,
		MonsterCardUpgradePolicy:      s.currentMonsterCardUpgradePolicy(routeContext, repos, session),
		AmplifyItemResolver:           amplifyItemResolver,
		RandomOptionResolver:          randomOptionResolver,
		EquipmentRegenerationResolver: equipmentRegenerationResolver,
		UpgradeTicketResolver:         upgradeTicketResolver,
		UpgradeProtectionResolver:     upgradeProtectionResolver,
		UpgradePolicyResolver:         upgradePolicyResolver,
		UpgradeRefundMailer:           upgradeRefundMailer,
		ForgingResolver:               forgingResolver,
		ChronicleRefineResolver:       chronicleRefineResolver,
		MagicBoxResolver:              magicBoxResolver,
		MagicBoxRewardItemResolver:    magicBoxRewardResolver,
		MagicBoxContractActivator:     magicBoxContractActivator,
		RandomRewardItemResolver:      randomRewardItemResolver,
		PremiumContractResolver:       premiumContractResolver,
		ExperienceConsumableResolver:  experienceConsumableResolver,
		StackableUseResolver:          stackableUseResolver,
		FatiguePotionNow:              s.gameplayNow(),
		RentalPointNow:                s.gameplayNow(),
		DirectExperiencePlanner:       directExperiencePlanner,
		QuestCompletionTicketPlanner:  questCompletionTicketPlanner,
		StoryBookPlanner:              storyBookPlanner,
		ChronicleGrowthResolver:       chronicleGrowthResolver,
		InDungeon:                     inDungeon,
		DungeonID:                     dungeonID,
		ConsumableFree:                currentSessionDungeonIgnoresConsumables(session),
		DamageFontResolver:            damageFontResolver,
		DamageFontNow:                 s.gameplayNow(),
		RepairCostResolver:            repairCostResolver,
		MailboxItemResolver:           mailboxItemResolver,
		SkillCatalog:                  skillCatalog,
		InitialSkillLevels:            initialSkillLevels,
		SkillPointBaseline:            skillPointBaseline,
		Party:                         &session.party.state,
	})
	routeCancel()
	session.party.mu.Unlock()
	if err != nil || !ok {
		return ok, err
	}
	decision := result.Decision
	if !result.Handled {
		return false, nil
	}
	switch decision.Action {
	case alignedcmd.ActionPendingModule:
		if typ == uint16(dnfenum.CmdPacketDeleteItem) && result.ResponseAllowed && len(result.UpperResponses) > 0 {
			// A dungeon potion is spent as op18 delete + op38 cast. Record the
			// committed delete so the cast arriving next can identify it.
			s.noteCurrentConsumableDeleteFromCommand(session, body)
		}
		if result.ResponseAllowed && len(result.UpperResponses) > 0 {
			s.logPacketEvent("game-aligned-command-real-response",
				"conn_id", session.connID,
				"channel_id", session.channel.ID,
				"account_id", accountID,
				"selected_character_id", session.selectedCharacterID,
				"cmd", cmd,
				"type", typ,
				"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
				"domain", string(decision.Domain),
				"support", string(decision.Support),
				"evidence", decision.Evidence,
				"note", decision.Note,
				"operation", result.Operation,
				"response_count", len(result.UpperResponses),
				"reason", result.Reason,
				"body_len", len(body))
			if err := s.sendAlignedUpperResponses(session, result); err != nil {
				return true, err
			}
			return true, nil
		}
		// A parsed command can still be interactive even when its domain owner is
		// incomplete or rejects the request before it can build a command-specific
		// response. Returning "handled" without a packet leaves the current
		// client waiting forever. The compatibility DLL publishes the exact
		// class-1 response-handler bitmap for this EXE, so use that independent
		// evidence to close only requests whose client really expects a response.
		// The generic outer failure is deliberately not a fabricated business
		// success: command-specific payload readers are skipped on failure.
		if rejected, rejectErr := s.rejectUnsupportedInteractiveCommand(
			session,
			typ,
			len(body),
			"aligned_pending",
		); rejected || rejectErr != nil {
			return true, rejectErr
		}
		s.logPacketEvent("game-aligned-command-pending-module",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"cmd", cmd,
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"domain", string(decision.Domain),
			"support", string(decision.Support),
			"evidence", decision.Evidence,
			"note", decision.Note,
			"operation", result.Operation,
			"response_allowed", result.ResponseAllowed,
			"reason", result.Reason,
			"body_len", len(body))
		s.logInfo("dnfbridge aligned game command pending module implementation",
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"domain", string(decision.Domain),
			"support", string(decision.Support),
			"operation", result.Operation,
			"response_allowed", result.ResponseAllowed,
			"reason", result.Reason,
			"body_len", len(body))
		return true, nil
	case alignedcmd.ActionBlocked:
		// Blocked migrations have the same client wait-state risk as an
		// unimplemented module. Capability-gate the failure so passive reports
		// remain silent and only proven interactive requests are released.
		if rejected, rejectErr := s.rejectUnsupportedInteractiveCommand(
			session,
			typ,
			len(body),
			"aligned_blocked",
		); rejected || rejectErr != nil {
			return true, rejectErr
		}
		s.logPacketEvent("game-legacy-migration-blocked-by-evidence",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"cmd", cmd,
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"domain", string(decision.Domain),
			"evidence", decision.Evidence,
			"operation", result.Operation,
			"reason", decision.Reason,
			"body_len", len(body))
		s.logInfo("dnfbridge legacy migration blocked by current exe evidence",
			"account_id", accountID,
			"selected_character_id", session.selectedCharacterID,
			"type", typ,
			"runtime_cmd_name", runtimeCmdPacketName(cmd, typ),
			"domain", string(decision.Domain),
			"operation", result.Operation,
			"body_len", len(body))
		return true, nil
	default:
		return false, nil
	}
}

func currentSessionDungeonIsPractice(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return dungeonRuntimeOwnsCharacter(session.dungeon.runtime, session.selectedCharacterID) &&
		currentDungeonRuntimeIsPractice(session.dungeon.runtime)
}

// currentSessionDungeonIsTrainingRoom reports whether the session's current run is
// 修炼道场 (5000). Live 2026-09-12: the client kept sending the skills' 无色 delete
// command (op18) and the consumable use command (op44) there and the server
// deducted both, while the room is supposed to consume nothing.
func currentSessionDungeonIsTrainingRoom(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return dungeonRuntimeOwnsCharacter(session.dungeon.runtime, session.selectedCharacterID) &&
		currentDungeonIsTrainingRoom(session.dungeon.runtime.Dungeon)
}

// currentSessionDungeonIgnoresConsumables marks the sandbox runs whose
// consumables must not be deducted. Skill materials are handled by the caller
// (ack only); ordinary stackable use still runs for its effect and the owner
// keeps the stack count unchanged because this flag travels in the request.
func currentSessionDungeonIgnoresConsumables(session *gameSession) bool {
	return currentSessionDungeonIsTrainingRoom(session)
}

func alignedSkillOwnerRulesRequired(opcode dnfenum.CmdPacket) bool {
	switch opcode {
	case dnfenum.CmdPacketChangeSkillslot, dnfenum.CmdPacketBuySkill, dnfenum.CmdPacketSkillInit,
		dnfenum.CmdPacketUseStackable, dnfenum.CmdPacketIncreaseStatus:
		// op30 SP/TP point books rebase every skill-tree ledger against the
		// active PVF baseline, so they need the same owner rules as op44.
		return true
	default:
		return false
	}
}

func (s *Service) skillOwnerRules(ctx context.Context, repos dnfrepo.Group, characterID uint16) (*dnfskill.Table, map[uint16]int, *dnfrepo.SkillPointState) {
	if s == nil || repos.Character == nil || characterID == 0 {
		return nil, nil, nil
	}
	record, ok, err := repos.Character.Load(ctx, fmt.Sprintf("%d", characterID))
	if err != nil || !ok {
		return nil, nil, nil
	}
	job, ok := characterJobByte(record)
	if !ok {
		return nil, nil, nil
	}
	profiles, catalog, err := s.currentProfessionResources(ctx)
	if err != nil {
		s.logInfo("dnfbridge profession skill rules unavailable", "character_id", characterID, "job", job, "error", err)
		return nil, nil, nil
	}
	growTypeValue := numericCharacterStatValue(record, "grow_type")
	if growTypeValue < 0 || growTypeValue > 0xff {
		s.logInfo("dnfbridge profession grow type invalid", "character_id", characterID, "job", job, "grow_type", growTypeValue)
		return nil, nil, nil
	}
	levels, ok := protectedProfessionSkillLevels(profiles, catalog, job, byte(growTypeValue), record.Level)
	if !ok {
		s.logInfo("dnfbridge profession free skill rules unavailable", "character_id", characterID, "job", job, "grow_type", growTypeValue, "error", dnfprofession.ErrProfileMissing)
		return nil, nil, nil
	}
	// CONTRACT: this baseline is the ORDINARY PVF target (Etc/spTable.etc plus
	// Etc/TP table through the character's level) and must NOT include the
	// persisted bonus_sp/bonus_tp. The SP/TP book owner
	// (inventory.useTPBook -> skillPointBookTarget) receives this same value as
	// Request.SkillPointBaseline and adds the bonus itself, so folding the bonus
	// in here made every book count the already-accumulated bonus twice and
	// granted quadratic free SP.
	//
	// The skill-plan mutations that DO need the bonus (op29 buy_skill / op491
	// skill reset) add it themselves from the character record in
	// skillcmd.skillPointTargetForCharacter, which mirrors the bonus-inclusive
	// currentPVFSkillPointTarget used by the login/reselect repair.
	points, err := s.initialSkillPoints(ctx, record.Level)
	if err != nil {
		s.logInfo("dnfbridge skill point baseline unavailable", "character_id", characterID, "level", record.Level, "error", err)
		return nil, nil, nil
	}
	baseline := &dnfrepo.SkillPointState{
		TotalSP: points.TotalSP, RemainingSP: points.TotalSP,
		TotalTP: points.TotalTP, RemainingTP: points.TotalTP,
		SyncedLevel: points.SyncedLevel,
	}
	return catalog, levels, baseline
}

// sendAlignedUpperResponses 发送模块返回的真实 upper 响应。
// 模块给出的 Body 已经是完整业务包体，不能再统一套成功字节。
func (s *Service) sendAlignedUpperResponses(session *gameSession, result alignedcmd.Result) error {
	s.recordAlignedCeraSpend(session, result)
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, result.Operation, result.PreResponseItemSlotRefreshes); err != nil {
		return err
	}
	for _, response := range result.UpperResponses {
		if err := s.sendGameUpperRawClassCodec(session, response.MsgID, response.Body, response.Classification, response.AllowCodec); err != nil {
			return err
		}
	}
	s.sendAlignedDailyUseStates(session, result)
	s.logAlignedItemCooltimeStates(session, result)
	if err := s.sendSelectedEmptyItemSlotRefreshes(session, result.Operation, result.PostResponseItemSlotClears); err != nil {
		return err
	}
	if result.CustomAbilityGroupConverter != nil {
		session.customAbilityConverter.arm(*result.CustomAbilityGroupConverter, s.gameplayNow())
		s.logGameEvent(session, "game-custom-ability-group-converter-opened",
			"character_id", result.CustomAbilityGroupConverter.CharacterID,
			"source_list", result.CustomAbilityGroupConverter.SourceListType,
			"source_slot", result.CustomAbilityGroupConverter.SourceSlotIndex,
			"source_item_id", result.CustomAbilityGroupConverter.SourceItemID,
			"sequence", "op44_success_written_then_arm_op1340_continuation")
	}
	s.broadcastAlignedAreaResponses(session, result.Operation, result.AreaBroadcastResponses)
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, result.Operation, result.ItemSlotRefreshes); err != nil {
		return err
	}
	// 原地改行的兜底：增量 op14 对"同槽同 item id、只有行内字段变化"的更新在这个
	// 客户端上不重绘，只能整表重建。放在增量行之后，让权威快照最后落地。
	for _, listType := range result.ItemListRebuilds {
		if err := s.sendSelectedCurrentContainerListRefresh(
			session, result.Operation+"_item_list_rebuild", listType); err != nil {
			return err
		}
	}
	if err := s.sendSelectedIncrementalEquipmentSlotRefreshes(session, result.Operation, result.EquipmentSlotRefreshes); err != nil {
		return err
	}
	// 城镇增益药剂（史诗Buff药剂一族）：把 effect-item 记录补发给客户端，否则它的
	// buff 图标不会出现（现场 2026-10-03 服主截图）。这一族的 buff 由 PVF 的
	// [probability buff list] 决定、必须服务端掷点，客户端自己算不出来。
	if result.EffectItemInfo != nil {
		body := buildCurrentEffectItemInfoBody(result.EffectItemInfo.ItemID, result.EffectItemInfo.RemainingSeconds)
		s.logPacketEvent("game-aligned-effect-item-info-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"char_id", session.selectedCharacterID,
			"operation", result.Operation,
			"item_id", result.EffectItemInfo.ItemID,
			"remaining_seconds", result.EffectItemInfo.RemainingSeconds,
			"msg_id", currentLoadEffectItemInfoMsgID,
			"classification", 0,
			"body_len", len(body),
			"sequence", "durable_owner_then_ack_then_item_row_then_class0_op174_load_effect_item_info")
		if err := s.sendGameUpperRawClass(session, currentLoadEffectItemInfoMsgID, body, 0); err != nil {
			return err
		}
	}
	// buff 图标靠这一发：class0/op487 CHARACTER_ADD_BUFF（同族换档时先 op488 撤旧的，
	// 再 op489 置激活）。现场 2026-10-03：只发 op174 时客户端只弹
	// "[史诗Buff药剂]的效果时间还剩10分钟"，buff 栏仍然空着 —— 因为通知号之前按参考
	// 服务端 A21 的 486 基址推，在本客户端（基址 485）ADD_BUFF 实际是 487，等于一直
	// 在发 DEL_BUFF。定标依据见 stat_change_potion_projection.go 顶部注释。
	if result.CharacterAddBuff != nil && result.CharacterAddBuff.BuffID > 0 {
		if removeID := result.CharacterAddBuff.RemoveBuffID; removeID > 0 && removeID != result.CharacterAddBuff.BuffID {
			removeBody := buildCurrentCharacterRemoveBuffBody([]int32{removeID})
			s.logPacketEvent("game-character-remove-buff-send",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"operation", result.Operation,
				"buff_id", removeID,
				"msg_id", currentCharacterRemoveBuffMsgID,
				"classification", 0,
				"body_len", len(removeBody),
				"sequence", "same_group_buff_replaced_before_add")
			if err := s.sendGameUpperRawClass(session, currentCharacterRemoveBuffMsgID, removeBody, 0); err != nil {
				return err
			}
		}
		if err := s.sendCurrentCharacterAddBuff(session, result.CharacterAddBuff.BuffID,
			"aligned_"+result.Operation+"_after_ack"); err != nil {
			return err
		}
		// op487 不带时长，客户端不会自己摘图标 —— 排一个到期定时器，
		// 到点补发 CHARACTER_DEL_BUFF + BUFF_DUNGEON(空)。见 stat_change_potion_expiry.go。
		if expireAtUnix := result.CharacterAddBuff.ExpireAtUnix; expireAtUnix > 0 {
			s.armCurrentStatChangePotionTimer(
				session,
				session.selectedCharacterID,
				time.Unix(expireAtUnix, 0).UTC(),
				"buff_registered_after_use",
			)
		}
	}
	if result.CharacterGoldAfter != nil {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentGoldStateBody(*result.CharacterGoldAfter),
			0,
		); err != nil {
			return err
		}
	}
	if len(result.EpicBookBalances) > 0 {
		rows := make([]currentEpicBookBalanceRow, 0, len(result.EpicBookBalances))
		for _, balance := range result.EpicBookBalances {
			rows = append(rows, currentEpicBookBalanceRow{
				FragmentID: balance.FragmentID,
				Balance:    balance.Balance,
			})
		}
		body, err := buildCurrentEpicBookInfoBody(1, rows)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, body, 0); err != nil {
			return err
		}
	}
	for _, response := range result.PostItemSlotRefreshResponses {
		if err := s.sendGameUpperRawClassCodec(session, response.MsgID, response.Body, response.Classification, response.AllowCodec); err != nil {
			return err
		}
	}
	if result.CeraWallet != nil {
		if err := s.sendGameUpperRawClass(
			session,
			currentCeraShopUpdateMsgID,
			buildCurrentCeraShopWalletBody(
				result.CeraWallet.Cera,
				result.CeraWallet.TokenCera,
				result.CeraWallet.HappyTokenCera,
			),
			0,
		); err != nil {
			return err
		}
	}
	if result.SeriaLuck != nil {
		if err := s.sendCurrentSeriaLuckGauge(session, result.SeriaLuck.Before, result.SeriaLuck.After, "aligned_"+result.Operation+"_after_ack"); err != nil {
			return err
		}
	}
	refreshCombatPowerAffixes := false
	for _, action := range result.PostActions {
		if alignedPostActionTouchesCombatPower(action) {
			refreshCombatPowerAffixes = true
		}
		// 2026-10-08：副本内的宠物外观发布改走 publishDungeonCreatureAppearanceChange
		// （见 creature_appearance_publication.go）。发什么由开关文件决定，缺省不发。
		//
		// 现场证据：房内宠物 equip/swap/unequip 4/4 之后客户端再也发不出 op45（换房），
		// 同局的武器 3/3、时装 3/3、无动作清房 1/1 全部正常 —— 包序列里唯一的差别就是
		// 宠物这条分支补发的 op2/mode0 角色行。而
		// 「初心-DNF轮回最终版/client-patch/README.md」写明 90CN 的设计原则是
		// "does not destroy/recreate the local paper-doll or replay mode0/mode1"，
		// 所以候选换成客户端原生的「既有 actor 装备」通道 op342 + op357。
		//
		// 这个动作本身两种模式下都不跳过：用 continue 而不是让它落到下面去 break。
		if action == alignedcmd.PostActionRefreshSelectedActorAppearance &&
			currentDungeonSceneActive(session) &&
			slices.Contains(result.PostActions, alignedcmd.PostActionRefreshSelectedCreatureState) {
			if err := s.publishDungeonCreatureAppearanceChange(session, result.Operation); err != nil {
				s.logWarn("dnfbridge dungeon creature appearance publication failed",
					"conn_id", session.connID,
					"operation", result.Operation,
					"char_id", session.selectedCharacterID,
					"error", err)
			}
			continue
		}
		if err := s.sendAlignedPostAction(session, result.Operation, action); err != nil {
			if errors.Is(err, errAlignedPostCommitProjectionDeferred) {
				s.logWarn("dnfbridge deferred remaining aligned post-commit projections",
					"conn_id", session.connID,
					"operation", result.Operation,
					"post_action", action,
					"char_id", session.selectedCharacterID,
					"error", err)
				break
			}
			return err
		}
	}
	if refreshCombatPowerAffixes {
		if err := s.sendSelectedCurrentCombatPowerAffixes(
			session,
			"aligned_"+result.Operation+"_after_ack_and_worn_state_commit",
		); err != nil {
			// The business mutation and ACK already succeeded. A private Lua
			// projection failure is retryable on the next panel/town refresh and
			// must not turn the committed item move into a false client failure.
			s.logWarn("dnfbridge combat-power affix post-action deferred",
				"conn_id", session.connID,
				"operation", result.Operation,
				"char_id", session.selectedCharacterID,
				"error", err)
		}
	}
	if result.Operation == "move_itemspace" || result.Operation == "sort_item" {
		if err := s.sendSkillSwitchInventoryLocationRefresh(session, "aligned_"+result.Operation+"_after_ack_and_item_move"); err != nil {
			// The durable item move and native ACK are already committed. The next
			// scene/panel refresh can rebuild the same dynamic position table, so a
			// transient projection failure must not turn this into a false op19
			// failure or disconnect the selected character.
			s.logWarn("dnfbridge skill-switch item-location refresh deferred",
				"conn_id", session.connID,
				"operation", result.Operation,
				"char_id", session.selectedCharacterID,
				"error", err)
		}
	}
	if result.MailboxAlarmRecipientID != 0 {
		if err := s.sendMailboxAlarmToOnlineRecipient(result.MailboxAlarmRecipientID); err != nil {
			// The durable send and sender ACK have already succeeded. A stale
			// recipient connection must not turn that committed operation into a
			// false failure; their next op96 projects the same mailbox state.
			s.logWarn("dnfbridge mailbox online alarm deferred to next mailbox open",
				"recipient_character_id", result.MailboxAlarmRecipientID,
				"operation", result.Operation,
				"error", err)
		}
	}
	if result.Operation == "enchant_by_bead" {
		// Must run AFTER the ACK: the client zeroes the account crystal/soul cells
		// synchronously while it handles that ACK (probe + disassembly 2026-09-13:
		// slots 354..359, caller 0x01E80130 in sub_1E7FFD0) and the same handling
		// rebuilds its container, so a pre-ACK snapshot is discarded (verified
		// live). Replaying the login-shaped container snapshot afterwards restores
		// the panel without any acquire toast; the transient blank is the client's
		// own refresh and cannot be removed from the server side.
		if err := s.sendSelectedCurrentContainerListsRefresh(session, "bead_enchant_after_ack"); err != nil {
			s.logWarn("dnfbridge bead enchant container snapshot deferred",
				"conn_id", session.connID,
				"operation", result.Operation,
				"char_id", session.selectedCharacterID,
				"error", err)
		}
	}
	return nil
}

func (s *Service) broadcastAlignedAreaResponses(session *gameSession, operation string, responses []alignedcmd.UpperResponse) {
	if s == nil || session == nil || s.onlinePlayers == nil || len(responses) == 0 {
		return
	}
	source, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	if !ok || source.Session != session {
		s.logWarn("dnfbridge aligned town announcement skipped",
			"operation", operation,
			"char_id", session.selectedCharacterID,
			"reason", "requester_not_registered_in_current_town_area")
		return
	}
	for _, player := range s.onlinePlayers.GetAreaInstancePlayers(
		source.ChannelID, source.TownID, source.AreaID, source.InstanceKey) {
		if player.Session == nil || player.CharacterID == source.CharacterID {
			continue
		}
		for _, response := range responses {
			if err := s.sendGameUpperRawClassCodec(player.Session, response.MsgID, response.Body, response.Classification, response.AllowCodec); err != nil {
				s.logWarn("dnfbridge aligned town announcement send failed",
					"operation", operation,
					"source_char_id", source.CharacterID,
					"recipient_char_id", player.CharacterID,
					"msg_id", response.MsgID,
					"error", err)
			}
		}
	}
}

func alignedPostActionTouchesCombatPower(action alignedcmd.PostAction) bool {
	switch action {
	case alignedcmd.PostActionRefreshSelectedItemContainers,
		alignedcmd.PostActionRefreshSelectedActorAppearance,
		alignedcmd.PostActionRefreshSelectedActorExpandedEquipment,
		alignedcmd.PostActionRefreshSelectedEquipmentSlots,
		alignedcmd.PostActionRefreshSelectedWornCreatureInventory,
		alignedcmd.PostActionRefreshSelectedCreatureState:
		return true
	default:
		return false
	}
}

func (s *Service) sendAlignedPostAction(session *gameSession, operation string, action alignedcmd.PostAction) error {
	switch action {
	case alignedcmd.PostActionRefreshSelectedItemContainers:
		s.logPacketEvent("game-aligned-command-post-action-send",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
			"classification", 0,
			"list_types", currentSelectedItemListTypes,
			"sequence", "durable_owner_then_class1_op19_ack_then_repository_backed_op13_list0_list1_list2")
		if err := s.sendSelectedCurrentContainerListsWithRefresh(
			session,
			"aligned_"+operation+"_after_ack",
			true,
		); err != nil {
			return fmt.Errorf("%w: refresh selected item containers: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedWornCreatureInventory:
		// 宠物/宠物装备穿脱：只重建真正变化的 list 7，不重发 list 0/1/2/38
		// （理由与实测见 alignedcmd 里该常量的注释）。收尾动作与五表刷新保持一致
		// ——op13 重建会丢掉客户端进程内的锁标记，所以单表重建之后照旧补发排序锁
		// 与 op251 物品锁快照。
		s.logPacketEvent("game-aligned-command-post-action-send",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
			"classification", 0,
			"list_type", currentPetInventoryListType,
			"sequence", "durable_owner_then_class1_op19_ack_then_repository_backed_op13_list7_only_then_sort_lock_then_op251")
		if err := s.sendSelectedPetInventoryListRefresh(session, "aligned_"+operation+"_after_ack"); err != nil {
			return fmt.Errorf("%w: refresh selected pet inventory: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		if err := s.sendCurrentSortLockReplayForSession(session, "aligned_"+operation+"_after_pet_inventory_refresh"); err != nil {
			return err
		}
		return s.sendCurrentSelectedItemLockSnapshot(session, "aligned_"+operation+"_after_pet_inventory_refresh")
	case alignedcmd.PostActionResetCargoPadBeforeItemLists:
		// 容量类操作（开通/升级账号金库、扩容券）之后必须重建仓库底板：当前 EXE 的
		// 仓库/金库面板在创建时按容量铺格子，后续 op13 只更新行数据，不会把新解锁的
		// 格子解锁（现场 2026-09-18：扩容后仍显示锁，切页才正常）。这里照进城镇那套
		// 已验证顺序补一发——先 op391 重置 cargo pad，再由随后的列表重建面板。
		if err := s.sendCurrentCargoPadResetBeforeItemLists(
			session,
			"aligned_"+operation+"_after_ack_before_real_item_lists",
		); err != nil {
			return fmt.Errorf("%w: reset cargo pad: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedPersonalCargo:
		s.logPacketEvent("game-aligned-command-post-action-send",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
			"classification", 0,
			"list_type", currentEquipmentPlacementListPersonalCargo,
			"sequence", "durable_owner_then_class1_op20_ack_then_repository_backed_op13_list2_only")
		if err := s.sendSelectedCurrentContainerListRefresh(
			session,
			"aligned_"+operation+"_after_ack",
			currentEquipmentPlacementListPersonalCargo,
		); err != nil {
			return fmt.Errorf("%w: refresh selected personal cargo: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedMainWallets:
		if err := s.sendSelectedMainWalletsRefresh(
			session,
			"aligned_"+operation+"_after_ack",
		); err != nil {
			return fmt.Errorf("%w: refresh selected main wallets: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedEquipmentSlots:
		s.logPacketEvent("game-aligned-command-post-action-send",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"classification", 0,
			"list_type", currentSocketListEquipment,
			"sequence", "unsafe_op14_list3_equipment_rows_suppressed",
			"evidence", "86jp_move_itemspace_sends_ack_sortlock_pet_specific_refresh_then_noti2_no_generic_list3")
		return nil
	case alignedcmd.PostActionRefreshSelectedEquippedCreatureItem:
		if err := s.sendSelectedEquippedCreatureItemRefresh(session, "aligned_"+operation+"_after_ack"); err != nil {
			return fmt.Errorf("%w: refresh equipped creature item: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedAccountCargo:
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		body, source, count, ok := s.buildCurrentItemListBodyForSession(ctx, session, 12)
		if !ok {
			return fmt.Errorf("aligned post action %q could not build account cargo", action)
		}
		s.logPacketEvent("game-aligned-command-post-action-send",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
			"classification", 0,
			"list_type", 12,
			"entry_count", count,
			"body_len", len(body),
			"body_source", source,
			"sequence", "durable_account_owner_then_class1_ack_then_current_exe_op13_list12_then_sort_lock_replay")
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), body); err != nil {
			return err
		}
		// The list-12 rebuild destroys and recreates the vault's item rows, and the
		// Ctrl+left-click padlock lives on the row rather than in the op251 lock
		// list, so it has to be republished exactly like the character containers do.
		// The equipment-lock snapshot is deliberately not resent here: list 12 is
		// outside 86JP IsSupportedEquipmentLockListType and holds no equipment lock.
		return s.sendCurrentSortLockReplayForSession(session, "aligned_"+operation+"_after_account_cargo_refresh")
	case alignedcmd.PostActionRefreshSelectedActorAppearance:
		// Full mode1 is reserved for rebuilding a new post-op24 actor generation;
		// using it per click recreates the actor and blocks the next move.
		if !currentTownSelfActorAppearanceReady(session) {
			s.logWarn("dnfbridge deferred selected actor mode0 appearance",
				"conn_id", session.connID,
				"operation", operation,
				"char_id", session.selectedCharacterID,
				"reason", "selected_town_actor_generation_or_native_owner_not_ready")
			return fmt.Errorf(
				"%w: selected town actor generation or native owner is not ready",
				errAlignedPostCommitProjectionDeferred,
			)
		}
		if err := s.sendSelectedActorMode0AppearanceRefresh(
			session,
			operation,
			"durable_owner_then_class1_ack_then_repository_backed_native_owner_mode0",
		); err != nil {
			s.logWarn("dnfbridge deferred selected actor mode0 appearance",
				"conn_id", session.connID,
				"operation", operation,
				"char_id", session.selectedCharacterID,
				"error", err)
			return fmt.Errorf("%w: refresh selected actor mode0 appearance: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		// The native mode0 wear refresh rebuilds the avatar widget from defaults.
		// Reapply the repository-backed visibility bits so 转职/觉醒特效 and
		// 觉醒装扮 stay at the player's saved values after every wardrobe change.
		if err := s.sendSelectedActorUserStateRefreshIfNonDefault(
			session,
			"aligned_"+operation+"_op357_after_mode0_appearance",
		); err != nil {
			return fmt.Errorf("reapply selected actor visibility after mode0 appearance: %w", err)
		}
		// The owner's own repaint is not enough: every client looking at this
		// player keeps drawing the previous wardrobe until it is told.
		s.broadcastTownActorAppearanceRefresh(session, "aligned_"+operation+"_after_ack")
		return nil
	case alignedcmd.PostActionRefreshSelectedActorExpandedEquipment:
		if !currentTownSelfActorAppearanceReady(session) {
			return fmt.Errorf(
				"%w: selected town actor generation or native owner is not ready for expanded equipment",
				errAlignedPostCommitProjectionDeferred,
			)
		}
		source := "aligned_" + operation + "_after_ack"
		if err := s.sendSelectedActorExpandedEquipmentRefresh(session, source+"_op342"); err != nil {
			return fmt.Errorf("%w: refresh selected actor expanded equipment: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		if err := s.sendSelectedActorUserStateRefresh(session, source+"_op357_after_op342"); err != nil {
			return fmt.Errorf("%w: redraw selected actor after expanded equipment: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		s.broadcastTownActorAppearanceRefresh(session, source+"_peer_repaint")
		return nil
	case alignedcmd.PostActionRefreshSelectedCreatureState:
		if err := s.sendSelectedCreatureStateAfterMoveAck(session, "aligned_"+operation+"_after_ack"); err != nil {
			// Creature state is an absolute post-ACK projection. Preserve the
			// already-committed endpoint move and let the next scene bootstrap
			// converge it instead of disconnecting after a successful ACK.
			s.logWarn("dnfbridge deferred selected creature state after move",
				"conn_id", session.connID,
				"operation", operation,
				"char_id", session.selectedCharacterID,
				"error", err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedCreatureGrowthState:
		if err := s.sendSelectedCreatureGrowthStateAfterFeed(session, "aligned_"+operation+"_after_ack"); err != nil {
			return fmt.Errorf("%w: refresh selected creature growth: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedPetInventory:
		if err := s.sendSelectedPetInventoryListRefresh(session, "aligned_"+operation+"_after_ack"); err != nil {
			return fmt.Errorf("%w: refresh selected pet inventory: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshCrystalContractState:
		return s.sendCurrentCrystalContractState(session, "aligned_"+operation+"_after_type97_activation")
	case alignedcmd.PostActionRefreshSelectedDamageFontState:
		return s.sendSelectedDamageFontState(session, "aligned_"+operation+"_after_ack_and_item_refresh")
	case alignedcmd.PostActionRefreshSelectedPartyFrame:
		identity, identityOK := s.boundGameSessionCharacterSnapshot(session)
		var previous party.RuntimePartySnapshot
		previousFound := false
		if identityOK {
			previous, previousFound = s.runtimePartyManagerForService().SnapshotByUser(identity.character, identity.generation)
		}
		state, err := s.ensureManagedRuntimePartyForSession(session)
		if err != nil {
			return err
		}
		if previousFound && previous.ID == uint16(state.PartyID) && len(previous.Members) == len(runtimePartyMembers(state)) {
			return s.sendManagedRuntimePartyAggregateRefresh(state, "aligned_"+operation+"_party_settings")
		}
		if err := s.sendRuntimePartySnapshot(session, state); err != nil {
			return err
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedQuestState:
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		repositories, ok := s.repositoryGroup()
		if !ok || repositories.Quest == nil || session == nil || session.selectedCharacterID == 0 {
			return fmt.Errorf("%w: quest repository unavailable", errAlignedPostCommitProjectionDeferred)
		}
		characterID := fmt.Sprintf("%d", session.selectedCharacterID)
		record, found, err := repositories.Quest.Load(ctx, characterID)
		if err != nil || !found {
			return fmt.Errorf("%w: load committed quest record character=%s: %v", errAlignedPostCommitProjectionDeferred, characterID, err)
		}
		if err := s.sendCurrentClearQuestListFromCommittedQuest(session, record, "aligned_"+operation+"_after_op44_ack_and_source_refresh"); err != nil {
			return err
		}
		if err := s.sendCurrentActiveQuestSnapshotForSession(session, "aligned_"+operation+"_after_completed_quest_bitmap"); err != nil {
			return err
		}
		return s.sendCurrentAcceptableQuestListOnlyForSession(session, "aligned_"+operation+"_after_active_quest_snapshot")
	case alignedcmd.PostActionRefreshSelectedActorSkills:
		s.logPacketEvent("game-aligned-command-post-action-send",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", currentSkillInfoMsgID,
			"classification", 0,
			"sequence", "durable_skill_mutation_then_class1_ack_then_class0_op19_refresh")
		return s.sendSelectedActorCurrentSceneSkillInfo(session, "aligned_"+operation+"_after_ack")
	case alignedcmd.PostActionRefreshSelectedCharacterProgression:
		return s.sendSelectedCharacterProgressionPostCommitState(session, "aligned_"+operation)
	case alignedcmd.PostActionRefreshSelectedFatigue:
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		if session == nil || session.selectedCharacterID == 0 {
			return fmt.Errorf("%w: character repository unavailable", errAlignedPostCommitProjectionDeferred)
		}
		characterID, _, character, found := s.selectedCharacterForEnter(ctx, session)
		if !found || characterID != session.selectedCharacterID {
			return fmt.Errorf("%w: load committed fatigue character=%d", errAlignedPostCommitProjectionDeferred, session.selectedCharacterID)
		}
		if err := s.sendGameUpperRawClass(session, currentFatigueMsgID, buildCurrentFatigueBody(character, true), 0); err != nil {
			return fmt.Errorf("%w: send current fatigue: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		return nil
	case alignedcmd.PostActionRefreshSelectedRentalWallet:
		sent, err := s.sendSelectedCurrentRentalPointState(session, "aligned_"+operation+"_after_ack", true)
		if err != nil {
			return fmt.Errorf("%w: send current rental wallet: %v", errAlignedPostCommitProjectionDeferred, err)
		}
		if !sent {
			return fmt.Errorf("%w: rental wallet state deferred", errAlignedPostCommitProjectionDeferred)
		}
		return nil
	case alignedcmd.PostActionNotifyRandomOptionGoldInsufficient:
		return s.sendCurrentRandomOptionGoldInsufficientNotice(session, operation)
	case alignedcmd.PostActionNotifyRandomOptionResetSuccess:
		// op455 成功后补一条与品级调整（op81）同款的系统页提示：客户端自己的
		// op455 处理器没有成功分支，不补就没有任何反馈。
		return s.sendCurrentRandomOptionResetSuccessNotice(session)
	case alignedcmd.PostActionSendDeleteItemAccountMaterialCompletion:
		// 【不再发送 class-0/op18 空完成帧】
		//
		// class-0/op18 在本客户端的处理器是 sub_1D61130 —— 它就是【交易完成】
		// 处理器（读 u16 条数 + 条数×物品对、清空交易管理器、然后创建交易结果
		// 弹窗）。空体 {00 00} 会被读成「0 条结果的交易完成」，于是释放消耗无色
		// 小晶块这类账号共享材料的技能时，客户端会弹出「物品交易已完成」——
		// 与交易毫无关系的误提示。
		//
		// 该帧原本的假设（"us90 用它清理库存忙状态"）在本客户端不成立：账号共享
		// 材料删除的完成由同一路径里的 class-1/op18 DELETE_ITEM RETURN
		// （buildDungeonSkillMaterialDeleteAck）+ 随后的权威 op14 数量行负责。
		// 而且 {00 00} 无法"改造得安全"：交易完成处理器会把任何包体都当成条数读，
		// 写非零值反而会让它去读取不存在的行。
		//
		// 因此这里只记录事件、不发包。若日后发现副本内技能连放会卡住库存忙状态，
		// 应改为补发 class-1/op18 的合法空 RETURN，而不是恢复这一帧。
		session.dungeon.mu.Lock()
		inDungeon := session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		s.logPacketEvent("game-aligned-command-post-action-suppressed",
			"conn_id", session.connID,
			"operation", operation,
			"post_action", action,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketDeleteItem),
			"classification", 0,
			"in_dungeon", inDungeon,
			"reason", "class0_op18_is_the_trade_finish_handler_and_would_show_a_false_trade_toast")
		return nil
	default:
		return fmt.Errorf("aligned post action %q is not registered", action)
	}
}

// currentTownSelfActorAppearanceReady is stricter than the general scene-ready
// predicate. The in-place op357 redraw is allowed only for the already-finalized
// selected actor owned by the context committed for this town connection. This
// prevents an inventory action from targeting a remote/stale actor or crossing
// a town transition boundary.
func currentTownSelfActorAppearanceReady(session *gameSession) bool {
	if session == nil || session.selectedCharacterID == 0 {
		return false
	}
	session.townMu.Lock()
	actorOwner := session.townActorOwnerChannel
	connectionOwner := currentConnectionTownActorOwnerContext(session)
	ready := session.townSceneReadyCharacterID == session.selectedCharacterID &&
		(actorOwner == currentSceneObjectContext || actorOwner == connectionOwner)
	session.townMu.Unlock()
	if !ready {
		return false
	}
	session.townPostTransition.mu.Lock()
	stage := session.townPostTransition.stage
	session.townPostTransition.mu.Unlock()
	return stage == currentTownPostTransitionIdle ||
		stage == currentTownPostTransitionComplete
}
