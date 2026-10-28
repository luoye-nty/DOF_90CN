// Package inventory routes current-EXE inventory commands to repository-backed
// owners. Mutations return success only after the required transaction and
// authoritative ACK/refresh sequence have been constructed.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	dnfguardianshield "longheng.io/server/internal/modules/dnf/guardianshield"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

type handler struct{}

// NewHandler 创建背包协议处理器。
func NewHandler() alignedcmd.Handler {
	return handler{}
}

// Domain 返回背包业务域。
func (handler) Domain() dnfenum.AlignedDomain {
	return dnfenum.AlignedDomainInventory
}

// Handle 解析已经对齐协议号的背包请求，并在关键写路径闭合前阻止 bridge 伪造成功 ACK。
func (handler) Handle(ctx context.Context, req alignedcmd.Request) (alignedcmd.Result, error) {
	switch dnfenum.CmdPacket(req.Opcode) {
	case dnfenum.CmdPacketDeleteItem:
		parsed, err := DecodeDeleteRequest(req.Body)
		cmd := NewDeleteCommand(req, parsed)
		if err != nil {
			return ownerRejectedDeleteResult(req.Opcode, cmd, err), nil
		}
		if err := checkDeleteListType(cmd.SourceListType); err != nil {
			return ownerRejectedDeleteResult(req.Opcode, cmd, err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerRejectedDeleteResult(req.Opcode, cmd, err), nil
		}
		result, err := owner.Delete(ctx, cmd)
		if err != nil {
			return ownerRejectedDeleteResult(req.Opcode, cmd, err), nil
		}
		return ownerAppliedDeleteResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketMoveItemspace:
		parsed, err := DecodeMoveItemspaceRequest(req.Body)
		cmd := NewMoveCommand(req, parsed)
		if err != nil {
			return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
		}
		if dnfguardianshield.IsWireList(cmd.SourceListType) || dnfguardianshield.IsWireList(cmd.DestinationListType) {
			if cmd.MoveCount != 0 || cmd.SourceInstanceValue < 0 || cmd.DestinationInstance != 0 || cmd.DestinationStack != 0 ||
				(cmd.SourceListType == dnfguardianshield.WireListEquipped && cmd.SourceInstanceValue != 0) ||
				(cmd.SourceListType == dnfguardianshield.WireListCatalog && cmd.SourceInstanceValue == 0) {
				return ownerRejectedGuardianShieldMoveResult(req.Opcode, cmd, fmt.Errorf("invalid current guardian shield op19 scalar fields")), nil
			}
			if req.GuardianShield == nil {
				return ownerRejectedGuardianShieldMoveResult(req.Opcode, cmd, fmt.Errorf("guardian shield PVF resolver is unavailable")), nil
			}
			owner, ownerErr := dnfguardianshield.NewOwner(req.Repositories, req.GuardianShield)
			if ownerErr != nil {
				return ownerRejectedGuardianShieldMoveResult(req.Opcode, cmd, ownerErr), nil
			}
			result, ownerErr := owner.Move(ctx, dnfguardianshield.MoveCommand{
				CharacterID:          req.SelectedCharacterID,
				AccountID:            req.AccountID,
				SourceListType:       cmd.SourceListType,
				SourceSlotIndex:      cmd.SourceSlotIndex,
				SourceShieldItemID:   uint32(cmd.SourceInstanceValue),
				DestinationListType:  cmd.DestinationListType,
				DestinationSlotIndex: cmd.DestinationSlotIndex,
			})
			if ownerErr != nil {
				return ownerRejectedGuardianShieldMoveResult(req.Opcode, cmd, ownerErr), nil
			}
			return ownerAppliedGuardianShieldMoveResult(req.Opcode, cmd, result), nil
		}
		if req.StackMergeResolverError != nil && moveTouchesMainQuickSlot(cmd) {
			return ownerRejectedQuickSlotCharmMoveResult(
				req.Opcode,
				cmd,
				fmt.Errorf("%w: %v", ErrQuickSlotCharmValidationUnavailable, req.StackMergeResolverError),
			), nil
		}
		petEquipmentMove := isProvenPetEquipmentMove(cmd)
		if petEquipmentMove {
			if err := validateProvenPetEquipmentMove(cmd); err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			var owner *dnfequip.Owner
			if cmd.DestinationSlotIndex == 26 {
				owner, err = dnfequip.NewOwner(req.Repositories)
			} else if req.EquipmentPlacement == nil {
				return ownerBlockedResult(cmd, fmt.Errorf("pet artifact move requires the current PVF placement validator")), nil
			} else {
				owner, err = dnfequip.NewOwnerWithPlacementValidator(req.Repositories, dnfequip.PlacementValidatorFunc(func(ctx context.Context, placement dnfequip.Placement) error {
					return req.EquipmentPlacement(ctx, alignedcmd.EquipmentPlacementRequest{
						CharacterID:     placement.CharacterID,
						ItemID:          placement.ItemID,
						SourceListType:  placement.SourceListType,
						SourceSlotIndex: placement.SourceSlotIndex,
						TargetSlotIndex: placement.TargetSlotIndex,
					})
				}))
			}
			if err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			ownerCommand := normalizedEquipmentMoveCommand(cmd)
			// Current NoPack uses actor-worn endpoint 3 or 17 on the wire.
			// Both resolve to the same durable EquipmentRecord. Keep the raw
			// endpoint in cmd for request evidence while the ACK reports the
			// owner's committed list-3 endpoint.
			result, err := owner.Move(ctx, ownerCommand)
			if err != nil {
				return ownerRejectedMoveRequestResult(req.Opcode, cmd, err), nil
			}
			return ownerAppliedPetEquipmentMoveResult(req.Opcode, cmd, result), nil
		}
		if (cmd.SourceListType == listTypePet || cmd.DestinationListType == listTypePet) &&
			!(cmd.SourceListType == listTypePet && cmd.DestinationListType == listTypePet) {
			return blockedParsedResult(cmd.Operation, cmd.String(), fmt.Errorf("unsupported current-EXE pet move endpoints")), nil
		}
		if isActorWornEndpoint(cmd.SourceListType) || isActorWornEndpoint(cmd.DestinationListType) {
			var owner *dnfequip.Owner
			if req.EquipmentPlacement == nil {
				return ownerBlockedResult(cmd, fmt.Errorf("normal equipment move requires the current PVF placement validator")), nil
			} else {
				owner, err = dnfequip.NewOwnerWithPlacementValidator(req.Repositories, dnfequip.PlacementValidatorFunc(func(ctx context.Context, placement dnfequip.Placement) error {
					return req.EquipmentPlacement(ctx, alignedcmd.EquipmentPlacementRequest{
						CharacterID:     placement.CharacterID,
						ItemID:          placement.ItemID,
						SourceListType:  placement.SourceListType,
						SourceSlotIndex: placement.SourceSlotIndex,
						TargetSlotIndex: placement.TargetSlotIndex,
					})
				}))
			}
			if err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			owner.SetAvatarClassifier(req.AvatarClassifier)
			owner.SetAuroraLookReplaceClassifier(req.AuroraLookReplaceClassifier)
			owner.SetNameTagChecker(req.NameTagChecker)
			result, err := owner.Move(ctx, normalizedEquipmentMoveCommand(cmd))
			if err != nil {
				return ownerRejectedMoveRequestResult(req.Opcode, cmd, err), nil
			}
			return ownerAppliedEquipmentMoveResult(req.Opcode, cmd, result), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		owner.SetStackMergeResolver(req.StackMergeResolver)
		owner.SetAccountWarehouseSlotResolver(req.AccountWarehouseSlotResolver)
		result, err := owner.Move(ctx, cmd)
		if err != nil {
			if errors.Is(err, ErrQuickSlotCharmAlreadyEquipped) {
				return ownerRejectedQuickSlotCharmMoveResult(req.Opcode, cmd, err), nil
			}
			if errors.Is(err, ErrMoveDestinationFull) || errors.Is(err, ErrMoveCarryLimitExceeded) {
				return ownerRejectedMoveSpaceResult(req.Opcode, cmd, err), nil
			}
			// 2026-09-18 现场：把道具拖进账号金库**未解锁**的栏位（dst 12:28、容量 8）时
			// owner 以 ErrAccountCargoSlotOutOfRange 拒绝，旧代码落到 ownerBlockedResult
			// 不回帧，客户端 sub_1D2DE80 的 pending 拖拽对象永远不释放。已解析成 move 的
			// 请求被 owner 拒绝，必须回失败帧——见 ownerRejectedMoveRequestResult。
			return ownerRejectedMoveRequestResult(req.Opcode, cmd, err), nil
		}
		return ownerAppliedMoveResult(ctx, req.Repositories.Settings, req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketChangeDeckInfo:
		deck, err := decodeCurrentGuardianShieldDeckRequest(req.Body)
		if err != nil {
			return rejectedGuardianShieldDeckResult(req.Opcode, err), nil
		}
		if req.GuardianShield == nil {
			return rejectedGuardianShieldDeckResult(req.Opcode, fmt.Errorf("guardian shield PVF resolver is unavailable")), nil
		}
		owner, err := dnfguardianshield.NewOwner(req.Repositories, req.GuardianShield)
		if err != nil {
			return rejectedGuardianShieldDeckResult(req.Opcode, err), nil
		}
		result, err := owner.SaveDeck(ctx, req.SelectedCharacterID, req.AccountID, deck)
		if err != nil {
			return rejectedGuardianShieldDeckResult(req.Opcode, err), nil
		}
		return appliedGuardianShieldDeckResult(req.Opcode, result), nil
	case dnfenum.CmdPacketSortItem:
		parsed, err := DecodeSortItemRequest(req.Body)
		cmd := NewSortCommand(req, parsed)
		if err != nil {
			return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.Sort(ctx, cmd)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedSortResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketBuyItem:
		parsed, err := DecodeBuyItemRequest(req.Body)
		cmd := NewBuyCommand(req, parsed)
		return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
	case dnfenum.CmdPacketSellItem:
		parsed, err := DecodeDeleteOrSellRequest(req.Body)
		cmd := NewSellCommand(req, parsed)
		if err != nil {
			return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.Sell(ctx, cmd)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedSellResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketRepairEquipment:
		parsed, err := DecodeRepairEquipmentRequest(req.Body)
		cmd := NewRepairCommand(req, parsed)
		if err != nil {
			return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
		}
		if cmd.SourceSlotIndex == -1 {
			// 全部修理 (86JP TryRepairAll): 穿戴装备 + 主列表快捷栏 slot 3..8,
			// 合计费用一次扣除，ACK 槽位 0xFFFF，随后发布实际变更行。
			if cmd.SourceListType != listTypeEquipment && cmd.SourceListType != listTypeMain {
				return ownerBlockedResult(cmd, fmt.Errorf("%w: %d", ErrUnsupportedList, cmd.SourceListType)), nil
			}
			owner, err := dnfequip.NewOwner(req.Repositories)
			if err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			result, err := owner.RepairAll(ctx, dnfequip.RepairCommand{
				SelectedCharacterID: cmd.SelectedCharacterID,
				AccountID:           req.AccountID,
				SlotIndex:           cmd.SourceSlotIndex,
				QuickRepair:         cmd.QuickRepair,
				AutoRepair:          cmd.AutoRepair,
			}, req.RepairCostResolver)
			if err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			return ownerAppliedRepairAllResult(req.Opcode, cmd, result), nil
		}
		if cmd.SourceListType == listTypeEquipment {
			owner, err := dnfequip.NewOwner(req.Repositories)
			if err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			result, err := owner.Repair(ctx, dnfequip.RepairCommand{
				SelectedCharacterID: cmd.SelectedCharacterID,
				AccountID:           req.AccountID,
				SlotIndex:           cmd.SourceSlotIndex,
				QuickRepair:         cmd.QuickRepair,
				AutoRepair:          cmd.AutoRepair,
			}, req.RepairCostResolver)
			if err != nil {
				return ownerBlockedResult(cmd, err), nil
			}
			return ownerAppliedEquippedRepairResult(req.Opcode, cmd, result), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.Repair(ctx, cmd, req.RepairCostResolver)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedRepairResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketDisjointItem:
		parsed, err := DecodeDisjointItemRequest(req.Body)
		cmd := NewDisjointCommand(req, parsed)
		return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
	case dnfenum.CmdPacketRequestEventServerLevelUp:
		parsed, err := DecodeEventServerLevelUpRequest(req.Body)
		cmd := NewEventServerLevelUpCommand(req, parsed)
		if err != nil {
			return rejectedEventServerLevelUpResult(req.Opcode, cmd, err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return rejectedEventServerLevelUpResult(req.Opcode, cmd, err), nil
		}
		result, err := owner.UseEventServerLevelUp(
			ctx,
			cmd,
			req.StackableUseResolver,
			req.DirectExperiencePlanner,
			req.QuestCompletionTicketPlanner,
			req.InDungeon,
		)
		if err != nil {
			return rejectedEventServerLevelUpResult(req.Opcode, cmd, err), nil
		}
		return appliedEventServerLevelUpResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketIncreaseStatus:
		parsed, err := DecodeIncreaseStatusRequest(req.Body)
		cmd := NewIncreaseStatusCommand(req, parsed)
		if err != nil {
			return rejectedIncreaseStatusResult(req.Opcode, cmd, err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return rejectedIncreaseStatusResult(req.Opcode, cmd, err), nil
		}
		result, err := owner.UseIncreaseStatusBook(
			ctx,
			cmd,
			req.StackableUseResolver,
			req.DirectExperiencePlanner,
			req.SkillCatalog,
			req.InitialSkillLevels,
			req.SkillPointBaseline,
		)
		if err != nil {
			return rejectedIncreaseStatusResult(req.Opcode, cmd, err), nil
		}
		return appliedIncreaseStatusResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUseStackable:
		parsed, err := DecodeUseStackableRequest(req.Body)
		cmd := NewUseStackableCommand(req, parsed)
		if err != nil {
			return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
		}
		if err := checkUseStackableRequestListType(cmd.SourceListType); err != nil {
			return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
		}
		experienceResolver := req.ExperienceConsumableResolver
		if experienceResolver != nil {
			resolution, resolveErr := experienceResolver(int64(cmd.ItemTemplateID))
			if resolveErr != nil {
				return ownerRejectedUseStackableResult(req.Opcode, cmd, fmt.Errorf("%w: %v", ErrExperienceConsumableResolveFailed, resolveErr)), nil
			}
			if resolution.BonusPercent > 0 {
				if !req.InDungeon {
					return ownerRejectedUseStackableResult(req.Opcode, cmd, ErrExperienceConsumableDungeonRequired), nil
				}
				experienceResolver = func(int64) (alignedcmd.ExperienceConsumableResolution, error) {
					return resolution, nil
				}
			}
		}
		stackableResolver := req.StackableUseResolver
		dailyUseLimit := int64(0)
		cooltimeSeconds := int64(0)
		cooltimeGroup := int64(0)
		maintainsCooltime := false
		if stackableResolver != nil {
			resolution, resolveErr := stackableResolver(int64(cmd.ItemTemplateID))
			if resolveErr != nil {
				return ownerRejectedUseStackableResult(req.Opcode, cmd, fmt.Errorf("%w: %v", ErrQuestCompletionTicketResolveFailed, resolveErr)), nil
			}
			if resolution.QuestCompletionTicket != nil && resolution.QuestCompletionTicket.VillageOnly && req.InDungeon {
				return ownerRejectedUseStackableResult(req.Opcode, cmd, ErrQuestCompletionTicketContract), nil
			}
			// PVF [total usable count] of ANY stackable consumable: the number
			// the client itself prints as 每天可用次数. Zero means "no cap".
			dailyUseLimit = resolution.DailyUseLimit
			// PVF [cool time] + [cooltime maintenance]: only items whose
			// cooldown must survive a relog get a durable window (see
			// inventory.Owner.ReserveItemCooltime).
			maintainsCooltime = resolution.MaintainsCooltime
			cooltimeSeconds = resolution.CooltimeSeconds
			cooltimeGroup = resolution.CooltimeGroup
			stackableResolver = func(int64) (alignedcmd.StackableUseResolution, error) {
				return resolution, nil
			}
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
		}
		// Reserve the daily slot before the use commits: a rejection at the cap
		// must leave the source stack untouched, and a failed use must give the
		// slot back. 修炼道场 sandbox runs consume nothing, so they never reserve.
		characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
		reservation := DailyItemUseReservation{}
		if !cmd.ConsumableFree {
			reservation, err = owner.ReserveDailyItemUse(
				ctx, characterID, int64(cmd.ItemTemplateID), dailyUseLimit, time.Time{})
			if err != nil {
				return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
			}
		}
		// 冷却（PVF [cool time] + [cooltime maintenance]）：用过就在窗口内拒绝，并在同一
		// 事务里落一份到期时间，这样小退重上仍然被拦（客户端的冷却门只在内存里）。同
		// [cooltime group] 的道具共用一个窗口，所以检查要用能解析**已存在冷却行**的
		// group 解析器，而不是只看本次这一件。
		cooltime := ItemCooltimeReservation{}
		if !cmd.ConsumableFree && maintainsCooltime && cooltimeSeconds > 0 {
			cooltime, err = owner.ReserveItemCooltime(
				ctx, characterID, int64(cmd.ItemTemplateID), cooltimeSeconds, cooltimeGroup,
				req.ItemCooltimeGroupResolver, time.Time{})
			if err != nil {
				return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
			}
		}
		result, err := owner.UseStackableWithAllResolversAndSkillCatalog(
			ctx,
			cmd,
			req.PremiumContractResolver,
			experienceResolver,
			stackableResolver,
			req.QuestCompletionTicketPlanner,
			req.DirectExperiencePlanner,
			req.StoryBookPlanner,
			req.SkillCatalog,
			req.InitialSkillLevels,
			req.SkillPointBaseline,
		)
		if err != nil {
			if releaseErr := owner.ReleaseDailyItemUse(ctx, characterID, reservation, time.Time{}); releaseErr != nil {
				err = fmt.Errorf("%w (daily-use slot release failed: %v)", err, releaseErr)
			}
			if releaseErr := owner.ReleaseItemCooltime(ctx, characterID, cooltime, time.Time{}); releaseErr != nil {
				err = fmt.Errorf("%w (item cooltime release failed: %v)", err, releaseErr)
			}
			return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
		}
		result.DailyUseItemID = reservation.ItemID
		result.DailyUseUsed = reservation.Used
		result.DailyUseLimit = reservation.Limit
		result.CooltimeItemID = cooltime.ItemID
		result.CooltimeExpireAt = cooltime.ExpireAt
		// 防虚弱药剂: the client applies the buff itself and never reports it, so
		// the server records the immunity window from the item id alone. Applied
		// after the use, so the effect can never exist without the consumption.
		if _, err := owner.ApplyWeaknessPotion(ctx, characterID, int64(cmd.ItemTemplateID), time.Time{}); err != nil {
			return ownerRejectedUseStackableResult(req.Opcode, cmd, err), nil
		}
		return ownerAppliedUseStackableResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUseSharedEffectItem:
		parsed, err := DecodeUseSharedEffectItemRequest(req.Body)
		cmd := NewUseSharedEffectItemCommand(req, parsed)
		if err != nil {
			return blockedParsedResult(cmd.Operation, cmd.String(), err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.UseSharedEffectItem(ctx, cmd, req.RandomRewardItemResolver)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedSharedEffectItemResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUseStackableAction:
		parsed, err := DecodeUseStackableActionRequest(req.Body)
		cmd := NewUseStackableActionCommand(req, parsed)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), nil
		}
		// 每日可用次数（PVF [total usable count]）与 op44 同一条规则：动作落地之前
		// 先按道具**自己的**声明预留一次，动作没成功就还回去。上限属于道具、与走哪个
		// action 无关，所以预留放在分派之前（见 stackable_action_daily_use.go）。
		characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
		reservation, err := owner.reserveUseStackableActionDailyUse(
			ctx, cmd, req.StackableUseResolver, characterID)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), nil
		}
		applied, committed := owner.applyUseStackableAction(ctx, cmd, req)
		if !committed {
			if releaseErr := owner.ReleaseDailyItemUse(ctx, characterID, reservation, time.Time{}); releaseErr != nil {
				applied.Reason = fmt.Sprintf("%s (daily-use slot release failed: %v)", applied.Reason, releaseErr)
			}
		}
		// 只有真正预留过的道具才投影 op541。action 58（疲劳药）在自己的事务里计数并
		// 已经带上 DailyUseStates，这里绝不覆盖它。
		if states := dailyUseStatesFromReservation(reservation); len(states) > 0 {
			applied.DailyUseStates = states
		}
		return applied, nil
	case dnfenum.CmdPacketUpgradeInventory, dnfenum.CmdPacketUpgradeCargo:
		// 扩容券（op430 物品栏 / op408 金库）。请求体只有一个 i16 槽位，券的身份由
		// owner 从持久行 + 运行时 PVF `[action type]` 反查，客户端说什么不算数。
		parsed, err := DecodeContainerUpgradeRequest(req.Body)
		cmd := NewContainerUpgradeCommand(req, parsed)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), nil
		}
		containerKind := alignedcmd.ContainerUpgradeInventory
		if dnfenum.CmdPacket(req.Opcode) == dnfenum.CmdPacketUpgradeCargo {
			containerKind = alignedcmd.ContainerUpgradeCargo
		}
		result, err := owner.UpgradeContainerFromVoucher(
			ctx, cmd, req.StackableUseResolver, containerKind, time.Time{})
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), nil
		}
		return ownerAppliedContainerUpgradeResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketSelectDamageFontSkin:
		parsed, err := DecodeSelectDamageFontRequest(req.Body)
		cmd := NewSelectDamageFontCommand(req, parsed)
		if err != nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("select damage font parse failed: %v", err),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildSelectDamageFontErrorAck(damageFontSelectionErrorUnavailable)),
				},
			}, nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.SelectDamageFont(ctx, cmd, req.DamageFontNow)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedDamageFontSelectionResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUpgradeItem:
		parsed, err := DecodeUpgradeItemRequest(req.Body)
		cmd := NewUpgradeCommand(req, parsed)
		if err != nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("upgrade_item parse failed: %v; returned current-EXE op50 error ACK", err),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildUpgradeItemErrorAck(upgradeErrorInvalidTarget)),
				},
			}, nil
		}
		var portableTool *alignedcmd.UpgradeTicketResolution
		if upgradeTicketTargetListSupported(cmd) && req.UpgradeTicketResolver != nil && !upgradeMaterialSlotIsAccountShared(cmd) {
			if materialItemID := upgradeMaterialItemIDOfSlot(req.Repositories, ctx, cmd); materialItemID > 0 {
				resolution, resolveErr := req.UpgradeTicketResolver(materialItemID, int64(cmd.TargetItemTemplateID))
				if resolveErr != nil {
					return alignedcmd.Result{
						Handled: true, ResponseAllowed: true, Operation: cmd.Operation,
						Reason:         fmt.Sprintf("portable/ticket PVF resolution failed: %v", resolveErr),
						UpperResponses: []alignedcmd.UpperResponse{class1Response(req.Opcode, buildUpgradeItemErrorAck(upgradeErrorInvalidMaterial))},
					}, nil
				}
				if resolution.PortableAmplifier || resolution.PortableReinforcer {
					if !strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") || resolution.TargetUpgradeForbidden {
						return alignedcmd.Result{
							Handled: true, ResponseAllowed: true, Operation: cmd.Operation,
							Reason:         "portable amplifier rejected by target PVF",
							UpperResponses: []alignedcmd.UpperResponse{class1Response(req.Opcode, buildUpgradeItemErrorAck(upgradeErrorInvalidTarget))},
						}, nil
					}
					portableTool = &resolution
					cmd.UpgradePortableAmplifier = resolution.PortableAmplifier
					cmd.UpgradePortableReinforcer = resolution.PortableReinforcer
					cmd.UpgradePortableMinimum = resolution.PortableMinimumLevel
					cmd.UpgradePortableMaximum = resolution.PortableMaximumLevel
					cmd.UpgradeTargetMinimumLevel = resolution.TargetMinimumLevel
					cmd.UpgradeTargetLevelMinimum = resolution.TargetLevelMinimum
					cmd.UpgradeTargetLevelMaximum = resolution.TargetLevelMaximum
					cmd.UpgradeMaterialItemID = int(materialItemID)
					cmd.UpgradeMaterialCount = 1
				}
			}
		}
		if upgradeTicketTargetListSupported(cmd) && req.UpgradeTicketResolver != nil && portableTool == nil && !upgradeMaterialSlotIsAccountShared(cmd) {
			ticketOwner, ownerErr := NewOwner(req.Repositories)
			if ownerErr != nil {
				return ownerBlockedResult(cmd, ownerErr), nil
			}
			ticketResult, ticketErr := ticketOwner.UpgradeTicket(ctx, cmd, req.UpgradeTicketResolver)
			if ticketErr != nil {
				return ownerBlockedResult(cmd, ticketErr), nil
			}
			if ticketResult.TicketResolved {
				return ownerAppliedUpgradeTicketResult(ctx, req.Repositories.Settings, req.Opcode, cmd, ticketResult), nil
			}
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		owner.SetStackMergeResolver(req.StackMergeResolver)
		owner.SetUpgradeProtectionResolver(req.UpgradeProtectionResolver)
		owner.SetUpgradeRefundMailer(req.UpgradeRefundMailer)
		targetStack, _ := upgradeTargetStackOfSlot(req.Repositories, ctx, cmd)
		targetAmplifyType, _ := upgradeAmplifyState(targetStack)
		// PVF 强化策略解析失败必须拒绝这一次强化：成功率、失败惩罚、金币和材料口径
		// 全在这一份策略里。旧行为是把 UpgradeSuccessWeight 留成 0 继续往下走，owner
		// 侧把它回落成「必成」，而材料口径因为 requiredMaterialItemID==0 退化成
		// 「玩家点的那一格随便什么道具扣 1 个」（owner.go:2179 分支）—— 于是一次策略
		// 表缺行就等于全局永久 100% 强化成功 + 不扣对材料。
		//
		// 只拦「解析器在、但解析失败」这一种。解析器整个缺失（nil）在线上不会发生
		// （dnfbridge 的 alignedUpgradePolicyResolver 恒装配），而 op50 的既有单测
		// 依赖 nil == 走旧路径，所以这里刻意保持原样不动。
		if req.UpgradePolicyResolver != nil {
			policy, policyErr := req.UpgradePolicyResolver(cmd.Mode, int(upgradeLevelOf(targetStack)), int64(cmd.TargetItemTemplateID), int(targetAmplifyType))
			if policyErr != nil {
				cmd.UpgradePolicyError = policyErr.Error()
				return ownerAppliedUpgradeResult(ctx, req.Repositories.Settings, req.Opcode, cmd,
					UpgradeResult{ErrorCode: upgradeErrorInvalidTarget}), nil
			}
			cmd.UpgradeSuccessWeight = policy.SuccessWeight
			cmd.UpgradePenaltyType = policy.PenaltyType
			cmd.UpgradePenaltyValue = policy.PenaltyValue
			cmd.UpgradeGoldCost = policy.GoldCost
			cmd.UpgradeGoldCostResolved = true
			cmd.UpgradeMaterialItemID = policy.MaterialItemID
			cmd.UpgradeMaterialCount = policy.MaterialCount
			cmd.UpgradeDestroyBonusItemID = policy.DestroyBonusItemID
			cmd.UpgradeDestroyBonusCount = policy.DestroyBonusCount
			cmd.UpgradeDestroyExtraRewards = append([]alignedcmd.UpgradeDestroyReward(nil), policy.DestroyExtraRewards...)
			cmd.UpgradeNoticeLevel = policy.NoticeLevel
			// The two guild-medal discriminators: the resolver flags a list-38
			// 公会徽章 attempt and reports the flag level its table row came
			// from, so the owner can re-check both against the stored row.
			cmd.UpgradeGuildMedal = policy.GuildMedal
			cmd.UpgradePolicyLevel = policy.CurrentLevel
			applyUpgradeHighUpgradeTerms(&cmd)
		} else {
			cmd.UpgradePolicyError = "upgrade policy resolver missing"
		}
		if portableTool != nil {
			cmd.UpgradeMaterialItemID = int(upgradeMaterialItemIDOfSlot(req.Repositories, ctx, cmd))
			cmd.UpgradeMaterialCount = 1
		}
		result, err := owner.Upgrade(ctx, cmd)
		if err != nil {
			if cmd.TargetListType == listTypeGuildMedal {
				// The medal page waits on an op50 class1 ACK. ownerBlockedResult
				// answers with the generic blocked form, which the medal window
				// does not read, so a failed transaction would leave it spinning;
				// upstream answers the same list-38 failure with the ordinary
				// upgrade error ACK instead.
				return alignedcmd.Result{
					Handled: true, ResponseAllowed: true, Operation: cmd.Operation,
					Reason:         fmt.Sprintf("guild medal upgrade transaction failed: %v", err),
					UpperResponses: []alignedcmd.UpperResponse{class1Response(req.Opcode, buildUpgradeItemErrorAck(upgradeErrorInvalidTarget))},
				}, nil
			}
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedUpgradeResult(ctx, req.Repositories.Settings, req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUpgradeChronicle:
		parsed, err := DecodeChronicleGrowthRequest(req.Body)
		cmd := NewChronicleGrowthCommand(req, parsed)
		if err != nil {
			return alignedcmd.Result{
				Handled: true, ResponseAllowed: true, Operation: cmd.Operation,
				Reason:         fmt.Sprintf("upgrade_chronicle parse failed: %v", err),
				UpperResponses: []alignedcmd.UpperResponse{class1Response(req.Opcode, buildChronicleGrowthErrorAck(1))},
			}, nil
		}
		if req.ChronicleGrowthResolver == nil {
			return alignedcmd.Result{
				Handled: true, ResponseAllowed: true, Operation: cmd.Operation,
				Reason:         fmt.Sprintf("upgrade_chronicle blocked: %v", ErrChronicleResolverRequired),
				UpperResponses: []alignedcmd.UpperResponse{class1Response(req.Opcode, buildChronicleGrowthErrorAck(0x13))},
			}, nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		growth, err := owner.UpgradeChronicle(ctx, cmd, req.ChronicleGrowthResolver)
		if err != nil {
			return alignedcmd.Result{
				Handled: true, ResponseAllowed: true, Operation: cmd.Operation,
				Reason:         fmt.Sprintf("upgrade_chronicle rejected: %v", err),
				UpperResponses: []alignedcmd.UpperResponse{class1Response(req.Opcode, buildChronicleGrowthErrorAck(0x13))},
			}, nil
		}
		return ownerAppliedChronicleGrowthResult(req.Opcode, cmd, growth), nil
	case dnfenum.CmdPacketUpgradeItemSeparate:
		parsed, err := DecodeSeparateUpgradeRequest(req.Body)
		cmd := NewSeparateUpgradeCommand(req, parsed)
		if err != nil {
			return forgingRejectedResult(req.Opcode, cmd, fmt.Sprintf("request parse failed: %v", err)), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return forgingRejectedResult(req.Opcode, cmd, err.Error()), nil
		}
		result, err := owner.Forge(ctx, cmd, req.ForgingResolver)
		if err != nil {
			return forgingRejectedResult(req.Opcode, cmd, err.Error()), nil
		}
		return ownerAppliedForgingResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketEnchant3rdChronicleItem:
		parsed, err := DecodeChronicleRefineRequest(req.Body)
		cmd := NewChronicleRefineCommand(req, parsed)
		if err != nil {
			return chronicleRefineRejectedResult(req.Opcode, cmd, chronicleRefineErrorInvalidMaterial, fmt.Sprintf("request parse failed: %v", err)), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return chronicleRefineRejectedResult(req.Opcode, cmd, chronicleRefineErrorInvalidMaterial, err.Error()), nil
		}
		result, err := owner.RefineChronicle(ctx, cmd, req.ChronicleRefineResolver)
		if err != nil {
			return chronicleRefineRejectedResult(req.Opcode, cmd, result.ErrorCode, err.Error()), nil
		}
		return ownerAppliedChronicleRefineResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketEnchantByBead:
		parsed, err := DecodeEnchantByBeadRequest(req.Body)
		cmd := NewEnchantCommand(req, parsed)
		if err != nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("enchant_by_bead parse failed: %v; returned current-EXE 0x0110 error ACK", err),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildEnchantByBeadErrorAck(enchantErrorInvalidBead)),
				},
			}, nil
		}
		if req.EnchantBeadResolver == nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: false,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("enchant_by_bead blocked: runtime-PVF resolver unavailable; refusing to trust request metadata: %v", ErrEnchantResolverRequired),
			}, nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.Enchant(ctx, cmd, req.EnchantBeadResolver)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedEnchantResult(ctx, req.Repositories.Settings, req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUpgradeCard:
		parsed, err := DecodeMonsterCardUpgradeRequest(req.Body)
		cmd := NewMonsterCardUpgradeCommand(req, parsed)
		if err != nil {
			return rejectedMonsterCardUpgradeResult(req.Opcode, cmd, monsterCardUpgradeErrorInvalid, err), nil
		}
		if req.MonsterCardUpgradeResolver == nil {
			return rejectedMonsterCardUpgradeResult(req.Opcode, cmd, monsterCardUpgradeErrorInvalid, ErrMonsterCardUpgradeResolverRequired), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return rejectedMonsterCardUpgradeResult(req.Opcode, cmd, monsterCardUpgradeErrorInvalid, err), nil
		}
		result, err := owner.UpgradeMonsterCard(ctx, cmd, req.MonsterCardUpgradeResolver, req.MonsterCardUpgradePolicy)
		if err != nil {
			return rejectedMonsterCardUpgradeResult(req.Opcode, cmd, monsterCardUpgradeErrorInvalid, err), nil
		}
		return ownerAppliedMonsterCardUpgradeResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketPurifyItem:
		parsed, err := DecodePurifyItemRequest(req.Body)
		cmd := NewPurifyItemCommand(req, parsed)
		if err != nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("purify_item parse failed: %v; returned current-EXE op204 fixed failure ACK", err),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildPurifyItemErrorAck()),
				},
			}, nil
		}
		if req.AmplifyItemResolver == nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("purify_item blocked: runtime-PVF resolver unavailable: %v", ErrAmplifyItemResolverRequired),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildPurifyItemErrorAck()),
				},
			}, nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.PurifyAmplifyItem(ctx, cmd, req.AmplifyItemResolver)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedPurifyItemResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketInvestItemAmplifyOption:
		parsed, err := DecodeInvestItemAmplifyOptionRequest(req.Body)
		cmd := NewInvestItemAmplifyOptionCommand(req, parsed)
		if err != nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("invest_item_amplify_option parse failed: %v; returned current-EXE op205 error ACK", err),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildInvestItemAmplifyOptionErrorAck(investAmplifyErrorInvalid)),
				},
			}, nil
		}
		if req.AmplifyItemResolver == nil {
			return alignedcmd.Result{
				Handled:         true,
				ResponseAllowed: true,
				Operation:       cmd.Operation,
				Reason:          fmt.Sprintf("invest_item_amplify_option blocked: runtime-PVF resolver unavailable: %v", ErrAmplifyItemResolverRequired),
				UpperResponses: []alignedcmd.UpperResponse{
					class1Response(req.Opcode, buildInvestItemAmplifyOptionErrorAck(investAmplifyErrorInvalid)),
				},
			}, nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		result, err := owner.InvestAmplifyOption(ctx, cmd, req.AmplifyItemResolver)
		if err != nil {
			return ownerBlockedResult(cmd, err), nil
		}
		return ownerAppliedInvestAmplifyOptionResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketUnsealRandomOption:
		parsed, err := DecodeUnsealRandomOptionRequest(req.Body)
		cmd := NewUnsealRandomOptionCommand(req, parsed)
		if err != nil || req.RandomOptionResolver == nil {
			return randomOptionFailureResult(req.Opcode, cmd, fmt.Errorf("parse=%v resolver_available=%t", err, req.RandomOptionResolver != nil)), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return randomOptionFailureResult(req.Opcode, cmd, err), nil
		}
		result, err := owner.UnsealRandomOption(ctx, cmd, req.RandomOptionResolver)
		if err != nil {
			return randomOptionFailureResult(req.Opcode, cmd, err), nil
		}
		return ownerAppliedRandomOptionResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketRegenerationRandomOption:
		parsed, err := DecodeEquipmentRegenerationRequest(req.Body)
		if err != nil || req.EquipmentRegenerationResolver == nil {
			return equipmentRegenerationFailureResult(req.Opcode, fmt.Errorf("parse=%v resolver_available=%t", err, req.EquipmentRegenerationResolver != nil)), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return equipmentRegenerationFailureResult(req.Opcode, err), nil
		}
		result, err := owner.RegenerateEquipment(ctx, req.AccountID, req.SelectedCharacterID, parsed, req.EquipmentRegenerationResolver)
		if err != nil || !result.Success {
			if err == nil {
				err = fmt.Errorf("equipment regeneration rejected")
			}
			return equipmentRegenerationFailureResult(req.Opcode, err), nil
		}
		return equipmentRegenerationAppliedResult(req.Opcode, result), nil
	case dnfenum.CmdPacketChangeRandomOption:
		parsed, err := DecodeChangeRandomOptionRequest(req.Body)
		cmd := NewChangeRandomOptionCommand(req, parsed)
		if err != nil || req.RandomOptionResolver == nil {
			return randomOptionFailureResult(req.Opcode, cmd, fmt.Errorf("parse=%v resolver_available=%t", err, req.RandomOptionResolver != nil)), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return randomOptionFailureResult(req.Opcode, cmd, err), nil
		}
		result, err := owner.ChangeRandomOption(ctx, cmd, req.RandomOptionResolver)
		if err != nil {
			return randomOptionFailureResult(req.Opcode, cmd, err), nil
		}
		return ownerAppliedRandomOptionResult(req.Opcode, cmd, result), nil
	case dnfenum.CmdPacketResetRandomOption:
		parsed, err := DecodeResetRandomOptionRequest(req.Body)
		cmd := NewResetRandomOptionCommand(req, parsed)
		if err != nil || req.RandomOptionResolver == nil {
			return randomOptionResetFailureResult(req.Opcode, cmd, randomOptionResetErrorInvalidMaterial,
				fmt.Errorf("parse=%v resolver_available=%t", err, req.RandomOptionResolver != nil)), nil
		}
		owner, err := NewOwner(req.Repositories)
		if err != nil {
			return randomOptionResetFailureResult(req.Opcode, cmd, randomOptionResetErrorInvalidMaterial, err), nil
		}
		result, err := owner.ResetRandomOption(ctx, cmd, req.RandomOptionResolver)
		if err != nil {
			return randomOptionResetFailureResult(req.Opcode, cmd, randomOptionResetErrorInvalidMaterial, err), nil
		}
		return ownerAppliedRandomOptionResetResult(req.Opcode, cmd, result), nil
	default:
		return alignedcmd.Result{
			Handled:         false,
			ResponseAllowed: false,
			Operation:       "unregistered",
			Reason:          fmt.Sprintf("inventory 模块未登记 opcode %d", req.Opcode),
		}, nil
	}
}

func forgingRejectedResult(opcode uint16, cmd Command, reason string) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("weapon forging rejected: %s reason=%s", cmd.String(), reason),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildForgingErrorAck(4)),
		},
	}
}

func chronicleRefineRejectedResult(opcode uint16, cmd Command, errorCode byte, reason string) alignedcmd.Result {
	if errorCode == 0 {
		errorCode = chronicleRefineErrorInvalidMaterial
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("chronicle refine rejected: %s reason=%s", cmd.String(), reason),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildChronicleRefineErrorAck(errorCode)),
		},
	}
}

func ownerAppliedChronicleRefineResult(opcode uint16, cmd Command, result ChronicleRefineResult) alignedcmd.Result {
	if !result.Success {
		return chronicleRefineRejectedResult(opcode, cmd, result.ErrorCode, "PVF or repository validation failed")
	}
	responses := []alignedcmd.UpperResponse{
		class1Response(opcode, buildChronicleRefineResultAck(result)),
	}
	mainRows := map[int16]dnfrepo.ItemStack{
		result.MaterialSlotIndex: result.MaterialUpdatedStack,
	}
	for _, reward := range result.FailureRewards {
		mainRows[reward.SlotIndex] = reward.UpdatedStack
	}
	if result.TargetListType == listTypeMain {
		mainRows[result.TargetSlotIndex] = result.TargetUpdatedStack
	} else {
		responses = append(responses, class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeEquipment, []commonItemListEntry{{slot: result.TargetSlotIndex, stack: result.TargetUpdatedStack}})))
	}
	entries := make([]commonItemListEntry, 0, len(mainRows))
	for slot, stack := range mainRows {
		entries = append(entries, commonItemListEntry{slot: slot, stack: stack})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].slot < entries[j].slot })
	responses = append(responses, class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, entries)))
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"chronicle refine applied: %s aura_type=%d canonical_id=%d option_count=%d probability=%d roll=%d refine_succeeded=%t target_destroyed=%t rewards=%d material_left=%d; sent current-EXE op370 result then op14 row updates",
			cmd.String(), result.AuraType, result.CanonicalAuraOptionID, result.OptionCount, result.Probability, result.ProbabilityRoll,
			result.RefineSucceeded, result.TargetDestroyed, len(result.FailureRewards), result.MaterialRemainingCount,
		),
		UpperResponses: responses,
	}
}

func ownerAppliedForgingResult(opcode uint16, cmd Command, result ForgingResult) alignedcmd.Result {
	if !result.Success {
		return forgingRejectedResult(opcode, cmd, "PVF or repository validation failed")
	}
	responses := []alignedcmd.UpperResponse{
		// Current NoPack registers command-response op438 to the forging window
		// callback. The leading success marker is consumed by the common wrapper;
		// the remaining 12-byte result updates both item views and posts the native
		// success/failure dialog before the authoritative op14 refresh.
		class1Response(opcode, buildForgingResultAck(result)),
	}
	if result.TargetListType == listTypeMain {
		entries := []commonItemListEntry{
			{slot: result.TargetSlotIndex, stack: result.TargetUpdatedStack},
			{slot: result.MaterialSlotIndex, stack: result.MaterialUpdatedStack},
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].slot < entries[j].slot })
		responses = append(responses, class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, entries)))
	} else {
		responses = append(responses,
			class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeEquipment, []commonItemListEntry{{slot: result.TargetSlotIndex, stack: result.TargetUpdatedStack}})),
			class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{slot: result.MaterialSlotIndex, stack: result.MaterialUpdatedStack}})),
		)
	}
	var areaBroadcasts []alignedcmd.UpperResponse
	if forgingBroadcastRequired(result.NewLevel) {
		notice := class0Response(msgUpgradeNotice, buildForgingNoticeBody(
			result.UpgradeSucceeded,
			cmd.SelectedCharacterID,
			result.TargetItemTemplateID,
			result.ReinforceLevel,
			result.NewLevel,
		))
		responses = append(responses, notice)
		areaBroadcasts = append(areaBroadcasts, notice)
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"weapon forging applied: %s level=%d->%d succeeded=%t independentAttack=%d successWeight=%d material=(%d,%d,left=%d) changed=%t; sent current-EXE op438 native 13-byte success result then op14 row updates broadcast=%t",
			cmd.String(), result.OldLevel, result.NewLevel, result.UpgradeSucceeded, result.IndependentAttack, result.SuccessWeight,
			result.MaterialItemID, result.MaterialCost, result.MaterialRemainingCount, result.Changed, forgingBroadcastRequired(result.NewLevel),
		),
		UpperResponses:         responses,
		AreaBroadcastResponses: areaBroadcasts,
	}
}

func forgingBroadcastRequired(level byte) bool {
	return level >= 6
}

func randomOptionFailureResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("random-option request rejected: %s error=%v", cmd.String(), err),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildRandomOptionStatusAck(false)),
		},
	}
}

// randomOptionResetFailureResult always answers op455. The current 90 client
// blocks the whole inventory UI until the pending command answers, so every
// rejection path must still emit the class1 result body: {u8 0} followed by the
// error code the client's op455 handler paints.
func randomOptionResetFailureResult(opcode uint16, cmd Command, errorCode byte, err error) alignedcmd.Result {
	if errorCode == 0 {
		errorCode = randomOptionResetErrorInvalidMaterial
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"random-option seal-grade reset rejected: %s target_slot=%d material_slot=%d option_index=%d error_code=%d error=%v",
			cmd.String(), cmd.TargetSlotIndex, cmd.MaterialSlotIndex, cmd.RandomOptionIndex, errorCode, err,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildRandomOptionResetErrorAck(errorCode)),
		},
	}
}

// ownerAppliedRandomOptionResetResult answers a committed op455 with the
// incremental item rows for the re-graded equipment and the consumed adjustment
// box, then the class1 success byte.
func ownerAppliedRandomOptionResetResult(opcode uint16, cmd Command, result RandomOptionResetResult) alignedcmd.Result {
	if !result.Success {
		errorCode := result.ErrorCode
		if errorCode == 0 {
			errorCode = randomOptionResetErrorInvalidMaterial
		}
		failure := randomOptionResetFailureResult(opcode, cmd, errorCode, fmt.Errorf("failure=%s item=%d", result.Failure, result.TargetItemID))
		failure.Reason = fmt.Sprintf(
			"random-option seal-grade reset rejected: %s item=%d material=%d option_index=%d failure=%s error_code=%d",
			cmd.String(), result.TargetItemID, result.MaterialItemID, result.OptionIndex, result.Failure, errorCode,
		)
		return failure
	}
	entries := make([]commonItemListEntry, 0, 2)
	entries = append(entries, commonItemListEntry{slot: result.TargetSlotIndex, stack: result.UpdatedTarget})
	entries = append(entries, commonItemListEntry{slot: result.MaterialSlotIndex, stack: result.UpdatedMaterial})
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"random-option seal-grade reset applied: %s item=%d option_index=%d type=%d grade=%d percent=%d->%d material=%d consumed=%t",
			cmd.String(), result.TargetItemID, result.OptionIndex, result.OptionType,
			result.Grade, result.OldPercent, result.NewPercent, result.MaterialItemID, result.MaterialConsumed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, entries)),
			class1Response(opcode, buildRandomOptionStatusAck(true)),
		},
		// 客户端 op455 处理器只处理失败分支，成功提示由服务端补发（与 op81
		// 品级调整同款系统提示），详见 alignedcmd.PostActionNotifyRandomOptionResetSuccess。
		PostActions: []alignedcmd.PostAction{alignedcmd.PostActionNotifyRandomOptionResetSuccess},
	}
}

func ownerAppliedRandomOptionResult(opcode uint16, cmd Command, result RandomOptionMutationResult) alignedcmd.Result {
	if !result.Success {
		if result.Failure == RandomOptionFailureInsufficientGold {
			failure := randomOptionFailureResult(opcode, cmd, fmt.Errorf(
				"insufficient gold required=%d available=%d",
				result.GoldCost,
				result.UpdatedGold,
			))
			failure.PostActions = []alignedcmd.PostAction{alignedcmd.PostActionNotifyRandomOptionGoldInsufficient}
			return failure
		}
		return randomOptionFailureResult(opcode, cmd, fmt.Errorf("owner rejected target item=%d", result.TargetItemID))
	}
	// 【扣了金币就必须把余额推给客户端】
	//
	// 修理 / 出售把新余额塞在自己的 ACK 里（buildRepairEquipmentAck /
	// buildSellItemAck 的 updatedGold 字段），客户端因此实时更新。随机属性家族
	// （op401 解封 / op437 变更）的成功 ACK 只有一个 result 字节
	// （buildRandomOptionStatusAck -> {0x01}），没有任何放金币的位置，于是服务端
	// 扣了钱而客户端仍显示旧余额 —— 玩家要小退重登才能看到扣完的数字。
	//
	// 主列表 0 号槽就是金币钱包行（id=0，余额放在 count 字段，见
	// currentGoldWalletItemListEntry / writeCommonItemListEntry 的 goldWallet 分支），
	// 所以这里挂 PostActionRefreshSelectedMainWallets，由 bridge 在道具行和 ACK 之后
	// 重发一次钱包刷新（它会重新读盘后的余额，读到的正是刚提交的事务结果）。
	postActions := make([]alignedcmd.PostAction, 0, 1)
	if result.GoldCost > 0 {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedMainWallets)
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("random-option owner applied: %s item=%d options=%d gold_cost=%d updated_gold=%d percentile_fallback=%t", cmd.String(), result.TargetItemID, len(result.Options), result.GoldCost, result.UpdatedGold, result.PercentileFallback),
		UpperResponses: []alignedcmd.UpperResponse{
			class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{slot: result.TargetSlotIndex, stack: result.UpdatedStack}})),
			class1Response(opcode, buildRandomOptionStatusAck(true)),
		},
		PostActions: postActions,
	}
}

func equipmentRegenerationFailureResult(opcode uint16, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Decision:        alignedcmd.Decision{Action: alignedcmd.ActionPendingModule, Domain: dnfenum.AlignedDomainInventory, Support: dnfenum.AlignedSupportDirect},
		Handled:         true,
		ResponseAllowed: true,
		Operation:       "equipment_regeneration",
		Reason:          err.Error(),
		UpperResponses:  []alignedcmd.UpperResponse{class1Response(opcode, []byte{0, equipmentRegenerationErrorDefault})},
	}
}

func equipmentRegenerationAppliedResult(opcode uint16, result EquipmentRegenerationResult) alignedcmd.Result {
	entries := make([]commonItemListEntry, 0, len(result.ChangedRows))
	for _, row := range result.ChangedRows {
		entries = append(entries, commonItemListEntry{slot: row.SlotIndex, stack: row.Stack})
	}
	return alignedcmd.Result{
		Decision:        alignedcmd.Decision{Action: alignedcmd.ActionPendingModule, Domain: dnfenum.AlignedDomainInventory, Support: dnfenum.AlignedSupportDirect},
		Handled:         true,
		ResponseAllowed: true,
		Operation:       "equipment_regeneration",
		Reason:          fmt.Sprintf("op407 committed source=%d result=%d slot=%d candidates=%d", result.SourceItemID, result.ResultItemID, result.SourceSlotIndex, result.CandidateCount),
		UpperResponses: []alignedcmd.UpperResponse{
			class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{slot: result.SourceSlotIndex}})),
			class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, entries)),
			class1Response(opcode, []byte{1, byte(result.SourceSlotIndex), byte(result.SourceSlotIndex >> 8)}),
		},
	}
}

func ownerAppliedPurifyItemResult(opcode uint16, cmd Command, result AmplifyMutationResult) alignedcmd.Result {
	if !result.Success {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason:          fmt.Sprintf("inventory owner rejected: %s internal_error=%d; returned current-EXE op204 fixed failure ACK", cmd.String(), result.ErrorCode),
			UpperResponses: []alignedcmd.UpperResponse{
				class1Response(opcode, buildPurifyItemErrorAck()),
			},
		}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s mode=%s amplify=(%d,%d) materialLeft=%d; returned self-contained current-EXE op204 ACK without unproven item-list replay",
			cmd.String(), result.Mode, result.AmplifyType, result.AmplifyValue, result.MaterialRemainingCount,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildPurifyItemSuccessAck(result)),
		},
	}
}

func ownerAppliedInvestAmplifyOptionResult(opcode uint16, cmd Command, result AmplifyMutationResult) alignedcmd.Result {
	if !result.Success {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason:          fmt.Sprintf("inventory owner rejected: %s error=%d; returned current-EXE op205 error ACK", cmd.String(), result.ErrorCode),
			UpperResponses: []alignedcmd.UpperResponse{
				class1Response(opcode, buildInvestItemAmplifyOptionErrorAck(result.ErrorCode)),
			},
		}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s mode=%s amplify=(%d,%d) level=%d materialLeft=%d; returned self-contained current-EXE op205 ACK without item-list replay",
			cmd.String(), result.Mode, result.AmplifyType, result.AmplifyValue, result.AmplifyLevel, result.MaterialRemainingCount,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildInvestItemAmplifyOptionSuccessAck(result)),
		},
	}
}

// upgradeTargetStackOfSlot reads the target the upgrade command names, from the
// backpack (list 0) or the equipment record (list 3), so the PVF policy
// resolver can pick the correct table row before the atomic transaction begins.
// Both the current level and the 红字 attribute come off this one load: the
// destruction refund needs the latter, because the disjointer's otherverse
// bonus is keyed off it.
//
// The backpack lookup is keyed by the command's **own** target list, not by
// list 0: the list-38 公会徽章 page stores its rows in the same backpack map
// under `38:<slot>` (upstream's upgradeLevelOfSlot does exactly this). Reading
// `0:<slot>` for a medal handed the policy resolver the level of whatever
// unrelated stack shares that index — and the owner's list-38 gate
// (`int(oldLevel) != cmd.UpgradePolicyLevel`) then rejected the attempt with
// error 4, i.e. the same "勋章不给强化" symptom from a second direction.
func upgradeTargetStackOfSlot(repos dnfrepo.Group, ctx context.Context, cmd Command) (dnfrepo.ItemStack, bool) {
	if cmd.SelectedCharacterID == 0 {
		return dnfrepo.ItemStack{}, false
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if cmd.TargetListType == listTypeEquipment {
		if repos.Equipment == nil {
			return dnfrepo.ItemStack{}, false
		}
		record, found, err := repos.Equipment.Load(ctx, characterID)
		if err != nil || !found || len(record.Entries) == 0 {
			return dnfrepo.ItemStack{}, false
		}
		entry, ok := record.Entries[strconv.Itoa(int(cmd.TargetSlotIndex))]
		if !ok || entry.ItemID != int64(cmd.TargetItemTemplateID) {
			return dnfrepo.ItemStack{}, false
		}
		return forgingStackFromEquipmentEntry(entry), true
	}
	if repos.Inventory == nil {
		return dnfrepo.ItemStack{}, false
	}
	record, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found || record.Slots == nil {
		return dnfrepo.ItemStack{}, false
	}
	stack, ok := record.Slots[slotKey(cmd.TargetListType, cmd.TargetSlotIndex)]
	if !ok || stack.ItemID != int64(cmd.TargetItemTemplateID) {
		return dnfrepo.ItemStack{}, false
	}
	return stack, true
}

func upgradeMaterialItemIDOfSlot(repos dnfrepo.Group, ctx context.Context, cmd Command) int64 {
	if repos.Inventory == nil || cmd.SelectedCharacterID == 0 || cmd.MaterialSlotIndex < 0 {
		return 0
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	record, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found || record.Slots == nil {
		return 0
	}
	stack, found := record.Slots[slotKey(listTypeMain, cmd.MaterialSlotIndex)]
	if !found || stack.Count <= 0 {
		return 0
	}
	return stack.ItemID
}

// highUpgradeFuelItemID is 高级炉岩炭 (stackable/cash/chn_high_upgrade), the
// fuel the 高级装备强化机 burns in place of 无色小晶块.
const highUpgradeFuelItemID = 2681644

// highUpgradeSuccessNumerator/Denominator scale the table's success weight for
// the advanced machine, and highUpgradeFuelDivisor is how much less fuel it
// burns than the ordinary machine's 无色小晶块. The item's own [explain] reads
// 「使用高级炉岩炭时的强化成功率为普通炉岩炭的110%」; etc/upgrade.etc has no
// high-upgrade table at all, so both the multiplier and the 1/10 fuel ratio
// are the operator's terms, not PVF data.
const (
	highUpgradeSuccessNumerator   = 11
	highUpgradeSuccessDenominator = 10
	highUpgradeFuelDivisor        = 10
)

// applyUpgradeHighUpgradeTerms rewrites the resolved policy for op50 sub mode
// 2: the material requirement becomes 高级炉岩炭 at a tenth of the table's
// 无色小晶块 count, and the success weight is multiplied by 1.1 and capped at
// the guaranteed-success base. Everything else -- gold, penalty type,
// destruction compensation -- stays exactly as the table says.
//
// The fuel count rounds up and never reaches zero: a row asking for fewer than
// ten crystals must still cost one fuel, not become free.
func applyUpgradeHighUpgradeTerms(cmd *Command) {
	if cmd == nil || !cmd.UpgradeHighUpgrade {
		return
	}
	if cmd.UpgradeMaterialCount > 0 {
		cmd.UpgradeMaterialItemID = highUpgradeFuelItemID
		cmd.UpgradeMaterialCount = (cmd.UpgradeMaterialCount + highUpgradeFuelDivisor - 1) / highUpgradeFuelDivisor
	}
	if cmd.UpgradeSuccessWeight > 0 {
		scaled := cmd.UpgradeSuccessWeight * highUpgradeSuccessNumerator / highUpgradeSuccessDenominator
		if scaled > currentUpgradeSuccessBase {
			scaled = currentUpgradeSuccessBase
		}
		cmd.UpgradeSuccessWeight = scaled
	}
}

// upgradeTicketTargetListSupported 界定 op50「券场景」能服务的目标列表。
//
// 上游的两处券前置（便携式强化器与 direct-level 券）都写死
// `cmd.TargetListType == listTypeMain`；本树另外支持**穿戴 list-3** 的券，
// 所以放行 0/3。**list-38 公会徽章必须排除**：
//
//   - 勋章页面压根没有券场景：它的材料是 flag_upgrade_table 行自带的
//     公会贡献证书，不是强化券；
//   - 而 UpgradeTicket 的目标分支只认 list 0 与 list 3（装备记录），list 38 落到
//     default 分支 —— 那里带着 base 的 TicketResolved=true 直接返回，于是券前置
//     把这次请求当成「券已裁决」，回一个 error 4（无效目标）的 0x0050 ACK，
//     本树移植的勋章 owner 路径（owner.go 的 list-38 分支）**一次都跑不到**。
//
// 现场：2026-10-04 19:16:30 / 19:17:16 两次 op50 0x2600（完美的守护图腾勋章 100380058
// 在 38:2、古老的勋章 100380021 在 38:1，材料 公会贡献证书 10154439 在 0:179）都是
// `inventory owner rejected: ... goldPVF=0 ... error=4; returned 0x0050 error ACK`，
// 客户端表现为「勋章不给强化」。
func upgradeTicketTargetListSupported(cmd Command) bool {
	return cmd.TargetListType == listTypeMain || cmd.TargetListType == listTypeEquipment
}

func upgradeMaterialSlotIsAccountShared(cmd Command) bool {
	if cmd.MaterialSlotIndex < 0 {
		return false
	}
	return dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.MaterialSlotIndex)
}

func ownerAppliedUpgradeResult(ctx context.Context, settings dnfrepo.SettingsRepository, opcode uint16, cmd Command, result UpgradeResult) alignedcmd.Result {
	if !result.Success {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner rejected: %s error=%d; returned current-EXE op50 error ACK",
				cmd.String(),
				result.ErrorCode,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				class1Response(opcode, buildUpgradeItemErrorAck(result.ErrorCode)),
			},
		}
	}

	responses := []alignedcmd.UpperResponse{
		class1Response(opcode, buildUpgradeItemSuccessAck(result)),
	}
	// A worn target is not part of the list-0 refresh: its row is repainted by
	// the bridge through one single-slot list-3 op14 (EquipmentSlotRefreshes).
	// Never widen that to the whole worn table - a bulk list-3 op14 terminates
	// the current client while it owns live equipment objects.
	equippedTarget := result.TargetListType == listTypeEquipment
	if entries := upgradeItemUpdateEntries(result, equippedTarget); len(entries) > 0 {
		responses = append(responses, class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, entries)))
	}
	var equipmentRefreshes []int16
	if equippedTarget && result.ResultCode != upgradeResultCodeDestroy {
		equipmentRefreshes = []int16{result.TargetSlotIndex}
	}
	// NOTI 0x0056 upgrade announcement when level meets notice threshold.
	if cmd.UpgradeNoticeLevel >= 0 {
		noticeLevel := result.NewLevel
		if !result.UpgradeSucceeded {
			noticeLevel = result.OldLevel
		}
		if int(noticeLevel) >= cmd.UpgradeNoticeLevel {
			responses = append(responses, class0Response(msgUpgradeNotice, buildUpgradeNoticeBody(
				result.UpgradeSucceeded, cmd.SelectedCharacterID, result.TargetItemTemplateID, noticeLevel)))
		}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s mode=%s level=%d->%d materialLeft=%d protectionUsed=%t protectionLeft=%d protectionItem=%d goldCost=%d changed=%t resultCode=%d; opened 0x0050 ACK + %s in 86JP order",
			cmd.String(),
			result.Mode,
			result.OldLevel,
			result.NewLevel,
			result.MaterialRemainingStackCount,
			result.ProtectionTicketUsed,
			result.ProtectionTicketRemaining,
			result.ProtectionTicketItemID,
			result.GoldCost,
			result.Changed,
			result.ResultCode,
			upgradeRefreshLabel(equippedTarget),
		),
		UpperResponses:         responses,
		EquipmentSlotRefreshes: equipmentRefreshes,
	}
}

// upgradeRefreshLabel names the refresh the handler opens after the op50 ACK.
func upgradeRefreshLabel(equippedTarget bool) string {
	if equippedTarget {
		return "0x000D material-only refresh + single-slot list3 equipment refresh"
	}
	return "0x000D main-list refresh"
}

// upgradeItemUpdateEntries builds the list-0 row refresh. For a worn target
// the target row is excluded (it is repainted through EquipmentSlotRefreshes);
// only material, protection-ticket and destroy-bonus rows remain.
func upgradeItemUpdateEntries(result UpgradeResult, equippedTarget bool) []commonItemListEntry {
	updates := make(map[int16]dnfrepo.ItemStack, 4)
	// The wallet is main-list slot 0 (item id 0, balance in the count field),
	// and it is not one of the mutated item slots, so nothing else here would
	// carry it. Without this row the gold left the database but the client
	// went on painting the old balance until the next full list refresh, which
	// looks exactly like "强化没有扣金币". The NPC shop already refreshes the
	// wallet this way.
	if result.GoldCost > 0 {
		updates[0] = dnfrepo.ItemStack{Count: result.UpdatedGold}
	}
	if !equippedTarget && result.TargetSlotIndex >= 0 {
		updates[result.TargetSlotIndex] = result.TargetUpdatedStack
	}
	if result.MaterialUpdated && result.MaterialSlotIndex >= 0 {
		updates[result.MaterialSlotIndex] = result.MaterialUpdatedStack
	}
	if result.ProtectionTicketUsed {
		protectionSlot := result.ProtectionTicketSlotIndex
		// Slot 0 is not a legal protection-ticket endpoint. Treat the zero
		// value as unspecified so older/directly constructed results continue
		// to refresh the explicit request slot.
		if protectionSlot <= 0 {
			protectionSlot = result.OptionalTicketSlotIndex
		}
		if protectionSlot >= 0 {
			updates[protectionSlot] = result.ProtectionTicketUpdatedStack
		}
	}
	for _, destroySlot := range result.DestroyBonusSlots {
		if stack, ok := result.MainRefresh[slotKey(listTypeMain, destroySlot)]; ok {
			updates[destroySlot] = stack
		}
	}
	if len(result.DestroyBonusSlots) == 0 && result.DestroyBonusItemID > 0 && result.DestroyBonusSlot >= 0 {
		if stack, ok := result.MainRefresh[slotKey(listTypeMain, result.DestroyBonusSlot)]; ok {
			updates[result.DestroyBonusSlot] = stack
		}
	}
	entries := make([]commonItemListEntry, 0, len(updates))
	for slot, stack := range updates {
		entries = append(entries, commonItemListEntry{slot: slot, stack: stack})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].slot < entries[j].slot })
	return entries
}

func ownerAppliedEnchantResult(ctx context.Context, settings dnfrepo.SettingsRepository, opcode uint16, cmd Command, result EnchantResult) alignedcmd.Result {
	if !result.Success {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner rejected: %s error=%d; returned current-EXE 0x0110 error ACK",
				cmd.String(),
				result.ErrorCode,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				class1Response(opcode, buildEnchantByBeadErrorAck(result.ErrorCode)),
			},
		}
	}

	state, found, err := dnfrepo.LoadCharacterContainerState(ctx, settings, result.CharacterID)
	if err != nil || !found {
		state = dnfrepo.CharacterContainerState{}
	}
	if result.TargetListType == listTypePet {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner applied pet enchant: %s card=%d upgrade=%d beadLeft=%d changed=%t; sent target list-7 and bead list-0 op14 rows before current-EXE 0x0110 ACK",
				cmd.String(),
				result.CardItemID,
				result.EnchantUpgradeCount,
				result.BeadRemainingCount,
				result.Changed,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				class0Response(msgItemListUpdate, buildPetItemListUpdateBody([]commonItemListEntry{{slot: result.TargetSlotIndex, stack: result.TargetUpdatedStack}})),
				class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{slot: result.BeadSlotIndex, stack: result.BeadUpdatedStack}})),
				class1Response(opcode, buildEnchantByBeadSuccessAck(result)),
			},
		}
	}
	if result.TargetListType == listTypeEquipment && result.TargetSlotIndex == 26 {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner applied equipped creature enchant: %s card=%d upgrade=%d beadLeft=%d changed=%t; sent bead row then current-EXE 0x0110 ACK and deferred worn creature op14 refresh",
				cmd.String(), result.CardItemID, result.EnchantUpgradeCount,
				result.BeadRemainingCount, result.Changed,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{slot: result.BeadSlotIndex, stack: result.BeadUpdatedStack}})),
				class1Response(opcode, buildEnchantByBeadSuccessAck(result)),
			},
			PostActions: []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedEquippedCreatureItem},
		}
	}
	if result.TargetListType == listTypeEquipment {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner applied worn equipment enchant: %s card=%d upgrade=%d beadLeft=%d changed=%t; sent target list-3 and bead list-0 op14 rows before current-EXE 0x0110 ACK so the ACK resolves the refreshed worn item",
				cmd.String(), result.CardItemID, result.EnchantUpgradeCount,
				result.BeadRemainingCount, result.Changed,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				// 附魔身上穿着的装备时，被附魔的那一行必须在 ACK 之前发：ACK 的
				// handler 会把目标槽解析成物品指针并交给结果弹窗，行先到则弹窗拿到
				// 的是已带新附魔属性的对象。活体 2026-09-15：只发魔石行时，客户端
				// 直到小退重建装备才显示附魔属性。
				class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeEquipment, []commonItemListEntry{{slot: result.TargetSlotIndex, stack: result.TargetUpdatedStack}})),
				class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{slot: result.BeadSlotIndex, stack: result.BeadUpdatedStack}})),
				class1Response(opcode, buildEnchantByBeadSuccessAck(result)),
			},
		}
	}
	// The current EXE's 0x0110 success handler resolves the target slot to an
	// item pointer and publishes that pointer to the result popup. A generic
	// selected-container refresh after the ACK destroys and recreates the
	// item objects while the popup still owns the pointer, so the popup can
	// render an unrelated item that later reuses the same allocation.
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s card=%d upgrade=%d beadLeft=%d changed=%t; opened one 0x000D main-list refresh then 0x0110 ACK without a pointer-invalidating duplicate refresh",
			cmd.String(),
			result.CardItemID,
			result.EnchantUpgradeCount,
			result.BeadRemainingCount,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class0Response(msgItemListRefresh, buildCommonItemListRefreshBodyWithState(listTypeMain, result.MainRefresh, state)),
			class1Response(opcode, buildEnchantByBeadSuccessAck(result)),
		},
	}
}

func rejectedMonsterCardUpgradeResult(opcode uint16, cmd Command, errorCode byte, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("monster card upgrade rejected: %s error=%d: %v", cmd.String(), errorCode, err),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMonsterCardUpgradeErrorAck(errorCode)),
		},
	}
}

func ownerAppliedMonsterCardUpgradeResult(opcode uint16, cmd Command, result MonsterCardUpgradeResult) alignedcmd.Result {
	if !result.Success {
		return rejectedMonsterCardUpgradeResult(
			opcode,
			cmd,
			result.ErrorCode,
			fmt.Errorf("repository state did not satisfy the op603 card-upgrade contract"),
		)
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(result.ChangedSlots))
	for _, slot := range result.ChangedSlots {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: slot})
	}
	// 升级成功后补一次 list-0 整表重建。
	//
	// 只发增量行是不够的：卡片升到**最后一级**时结果留在原槽（例如 242 -> 242），
	// 行里只有升级字节 03 -> 04 变了，item id 和槽位都没动 —— 这个旧客户端对这种
	// "原地改行"不重绘，背包会一直显示上一级（玩家实测满级后仍显示 3/4，重新选
	// 角色才正常）。前几级之所以没事，是因为每次都换了槽位，客户端看到的是
	// "删一个 + 加一个"。
	//
	// 只在**升级成功**时重建：失败那次卡片和材料都没动，增量行足够。
	var rebuilds []byte
	if result.UpgradeSucceeded {
		rebuilds = []byte{listTypeMain}
	}
	goldAfter := result.UpdatedGold
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"monster card upgrade committed: %s item=%d roll_success=%t upgrade=%d chance=%d/%d gold_cost=%d result_slot=%d changed_slots=%v",
			cmd.String(), result.TargetItemID, result.UpgradeSucceeded, result.UpgradeCount,
			result.Chance, monsterCardUpgradeProbabilityBase, result.GoldCost, result.ResultSlot, result.ChangedSlots,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMonsterCardUpgradeSuccessAck(result)),
		},
		ItemSlotRefreshes:  refreshes,
		ItemListRebuilds:   rebuilds,
		CharacterGoldAfter: &goldAfter,
	}
}

func ownerAppliedUpgradeTicketResult(ctx context.Context, settings dnfrepo.SettingsRepository, opcode uint16, cmd Command, result UpgradeTicketResult) alignedcmd.Result {
	if !result.Success {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner rejected: %s error=%d; returned 0x0050 error ACK",
				cmd.String(),
				result.ErrorCode,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				class1Response(opcode, buildUpgradeTicketErrorAck(result.ErrorCode)),
			},
		}
	}

	responses := []alignedcmd.UpperResponse{
		class1Response(opcode, buildUpgradeTicketSuccessAck(result)),
	}
	equippedTarget := result.TargetListType == listTypeEquipment
	if entries := upgradeTicketUpdateEntries(result, equippedTarget); len(entries) > 0 {
		responses = append(responses, class0Response(msgItemListUpdate, buildCommonItemListUpdateBody(listTypeMain, entries)))
	}
	var equipmentRefreshes []int16
	if equippedTarget {
		equipmentRefreshes = []int16{result.TargetSlotIndex}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s mode=%s level=%d->%d roll=%t ticketLeft=%d changed=%t; opened 0x0050 ACK + %s in 86JP order",
			cmd.String(),
			result.Mode,
			result.OldLevel,
			result.NewLevel,
			result.UpgradeSucceeded,
			result.MaterialRemainingStackCount,
			result.Changed,
			upgradeRefreshLabel(equippedTarget),
		),
		UpperResponses:         responses,
		EquipmentSlotRefreshes: equipmentRefreshes,
	}
}

// upgradeTicketUpdateEntries builds the list-0 row refresh. For a worn target
// the target row is excluded; only the ticket row changes in the backpack.
func upgradeTicketUpdateEntries(result UpgradeTicketResult, equippedTarget bool) []commonItemListEntry {
	updates := make(map[int16]dnfrepo.ItemStack, 2)
	if !equippedTarget && result.TargetSlotIndex >= 0 {
		updates[result.TargetSlotIndex] = result.TargetUpdatedStack
	}
	if result.MaterialUpdated && result.MaterialSlotIndex >= 0 {
		updates[result.MaterialSlotIndex] = result.MaterialUpdatedStack
	}
	entries := make([]commonItemListEntry, 0, len(updates))
	for slot, stack := range updates {
		entries = append(entries, commonItemListEntry{slot: slot, stack: stack})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].slot < entries[j].slot })
	return entries
}

func ownerAppliedMoveResult(ctx context.Context, settings dnfrepo.SettingsRepository, opcode uint16, cmd Command, result MoveResult) alignedcmd.Result {
	moveValue := clampInt32(result.MoveCount)
	if result.Mode == "noop" {
		moveValue = 0
	}
	ackCommand := cmd
	if result.RedirectedReservedMainSlot {
		ackCommand.SourceListType = result.SourceListType
		ackCommand.SourceSlotIndex = result.SourceSlotIndex
		ackCommand.DestinationListType = result.DestinationListType
		ackCommand.DestinationSlotIndex = result.DestinationSlotIndex
	}
	responses := []alignedcmd.UpperResponse{
		class1Response(opcode, buildMoveItemspaceSuccessAck(ackCommand, moveValue)),
	}
	itemSlotRefreshes := make([]alignedcmd.ItemSlotRefresh, 0, 2)
	// 2026-09-13 结论（客户端 sub_1D2DE80 + 86JP 参考一致）：op19 的 ACK 本身就带
	// 源/目标 list+槽与移动值，客户端据此自行重画，**不需要也不应收到额外的物品行**：
	// 补发的精确槽位行会被客户端当成额外变化套到它自己的 op19 对象上，现场表现为
	// "放进背包的那件叠加成了被换到仓库的那件"。因此这里保持只有跨容器"叠加"这一
	// 种情形走精确槽位（历史行为），swap 回到只发 ACK。
	crossContainerStack := result.Changed &&
		result.Mode == "stack" &&
		isSupportedCrossContainerStack(ackCommand.SourceListType, ackCommand.DestinationListType)
	if crossContainerStack {
		if ackCommand.SourceListType != listTypeAccountCargo {
			itemSlotRefreshes = append(itemSlotRefreshes,
				alignedcmd.ItemSlotRefresh{ListType: ackCommand.SourceListType, SlotIndex: ackCommand.SourceSlotIndex},
			)
		}
		if ackCommand.DestinationListType != listTypeAccountCargo {
			itemSlotRefreshes = append(itemSlotRefreshes,
				alignedcmd.ItemSlotRefresh{ListType: ackCommand.DestinationListType, SlotIndex: ackCommand.DestinationSlotIndex},
			)
		}
	} else {
		for _, listType := range result.RefreshListTypes {
			slots, ok := result.Refresh[listType]
			if !ok {
				continue
			}
			state, found, err := dnfrepo.LoadCharacterContainerState(ctx, settings, result.CharacterID)
			if err != nil || !found {
				state = dnfrepo.CharacterContainerState{}
			}
			responses = append(responses, class0Response(
				msgItemListRefresh,
				buildCommonItemListRefreshBodyWithState(listType, slots, state),
			))
		}
	}
	// 账号共享晶体/灵魂槽（list 0, 354..365）搬空时必须补一格精确删除行：op19 的
	// ACK 只带源/目标端点，客户端不据它重绘右侧晶体/灵魂面板，于是搬空的格子会留下
	// "幽灵行"（现场 2026-09-17：无色小晶块从晶体仓库拖到个人仓库后，面板仍显示原来
	// 的 40449，而且那一行点不动）。这里只补**空掉**的端点：补发物品行会被客户端当成
	// 额外变化套到它自己的 op19 对象上（2026-09-13 的重复显示），删除行没有这个问题。
	for _, slot := range result.EmptiedAccountSharedSlots {
		itemSlotRefreshes = append(itemSlotRefreshes,
			alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: slot},
		)
	}
	// 落点被改写到账号共享固定格时，客户端声明的那一格也要显式清掉：op19 是乐观移动，
	// 客户端发包前就已经把物品画在它自己声明的那一格上了（现场 2026-09-21：晶体从个人仓库
	// 拖回背包时声明 0:156，服务端落到 0:357，客户端从此留下 0:156 的幽灵行，那一格点不动、
	// 再往外拖连 op19 都不发）。owner 只在该格**持久化记录里确实为空**时才回报，所以这里补发
	// 的必然是背包空格的删除行 (0xFFFFFFFF)，不会盖掉真实物品。
	if slot := result.ClearedRedirectedMainSlot; slot > 0 && slot != result.DestinationSlotIndex {
		itemSlotRefreshes = append(itemSlotRefreshes,
			alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: slot},
		)
	}
	// 86JP SendMoveSortLockSignals：ACK 之后按仍然锁定的端点槽位补发 class1/op713，
	// 否则客户端的 padlock 会留在物品刚离开的旧槽位上。
	for _, signal := range result.SortLockSignals {
		responses = append(responses, class1Response(
			uint16(dnfenum.CmdPacketRequestItemSortLock),
			dnfitemlock.BuildSortLockStateBody(signal.ListType, signal.SlotIndex),
		))
	}
	postActions := make([]alignedcmd.PostAction, 0, 1)
	// 晶体/灵魂仓库（list 0, 354..365）参与的移动之后必须重绘那两排：op19 的 ACK 只声明
	// 源/目标端点，客户端不会据它重绘右侧晶体/灵魂面板（2026-09-17 现场：搬走之后面板
	// 还画着原来那一堆）。这一发按仓库当前占用行重放，同时带上金币钱包行。
	accountSharedTouched := dnfrepo.IsAccountSharedInventorySlot(result.SourceListType, result.SourceSlotIndex) ||
		dnfrepo.IsAccountSharedInventorySlot(result.DestinationListType, result.DestinationSlotIndex)
	if accountSharedTouched ||
		(result.Changed && isMainPersonalCargoCrossList(result.SourceListType, result.DestinationListType)) {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedMainWallets)
	}
	// 落点被改写到账号共享固定格的那一步，除增量行外还要补一次 list-0 **整表重建**。
	//
	// 为什么增量行治不了：搬空账号格时按晶体面板的语义发的是 (itemID=0, count=0) "空格"行，
	// 而这个旧客户端的 list-0 行读取器把 **itemID=0 理解成金币对象**。玩家把晶体从晶体仓库
	// 拖到个人仓库、再从个人仓库拖回背包（这一步落点被改写到固定格）之后，那一格在客户端
	// 就成了"金币"：右侧面板看着正常，但再往外拖时抓到的是金币（鼠标上的图标就是金币），
	// 客户端自己的规则不允许拖金币，于是**连 op19 都不发**，那一格等于废了。
	//
	// 现场 2026-09-21（玩家 1111，蓝色小晶块 3036）：晶体仓库 → 个人仓库 → 背包 走一遍之后
	// 就再也拖不动；**小退（整份 list-0 快照重建）立刻恢复**，再走一遍又坏 —— 正是
	// "整表重建能治、增量行治不了"的形态，与怪物卡片满级、宠物栏 list 7、list 38 同类。
	//
	// 只在"落点被改写"这一步重建（不是每次晶体移动）：那一步才让客户端的乐观落点与服务端
	// 的真实落点分叉，也正是幽灵/金币对象的现场。重建正文由
	// mergeCurrentAccountSharedItemListEntries 把 354..365 一并铺回去，并且排在增量行之后
	// 落地（见 game_aligned.go 的 ItemListRebuilds 循环），所以权威快照最后生效。
	var rebuilds []byte
	if result.Changed && accountSharedTouched && result.RedirectedReservedMainSlot {
		rebuilds = []byte{listTypeMain}
	}
	// 搬空晶体/灵魂槽不再额外重放容器列表快照：清空那一格由 op14 的"空格"行
	// (itemID=0, count=0) 负责，和拍卖行上架、摆摊售出同一条写法（现场 2026-09-17：
	// 那两条路径能正确减少晶体仓库数量，拖拽路径当时发的是 0xFFFFFFFF 所以不行）。
	if result.Changed && (result.SourceListType == listTypeAccountCargo || result.DestinationListType == listTypeAccountCargo) {
		// List 12 is account-owned.  The bridge rebuilds its current-EXE op13
		// snapshot from account metadata + AccountInventory after the op19 ACK;
		// it must not use the per-character common refresh path.
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedAccountCargo)
	}
	if result.Changed && (result.SourceListType == listTypePet || result.DestinationListType == listTypePet) {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedItemContainers)
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s moveMode=%s moveCount=%d changed=%t refreshLists=%v incrementalSlots=%v; opened 0x0013 ACK then authoritative refresh for changed counts",
			cmd.String(),
			result.Mode,
			result.MoveCount,
			result.Changed,
			result.RefreshListTypes,
			itemSlotRefreshes,
		),
		UpperResponses:    responses,
		ItemSlotRefreshes: itemSlotRefreshes,
		ItemListRebuilds:  rebuilds,
		PostActions:       postActions,
	}
}

func isSupportedCrossContainerStack(sourceListType, destinationListType byte) bool {
	if sourceListType == destinationListType {
		return false
	}
	supported := func(listType byte) bool {
		switch listType {
		case listTypeMain, listTypePersonalCargo, listTypeAccountCargo:
			return true
		default:
			return false
		}
	}
	return supported(sourceListType) && supported(destinationListType)
}

func ownerAppliedSellResult(opcode uint16, cmd Command, result SellResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s sold=%d goldDelta=%d updatedGold=%d changed=%t; opened 0x0016 response in C# order",
			cmd.String(),
			result.Sold.AppliedCount,
			result.GoldDelta,
			result.UpdatedGold,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildSellItemAck(result.Sold, result.UpdatedGold)),
		},
	}
}

func ownerAppliedSortResult(opcode uint16, cmd Command, result SortResult) alignedcmd.Result {
	postAction := alignedcmd.PostActionRefreshSelectedItemContainers
	if result.ListType == listTypePersonalCargo {
		postAction = alignedcmd.PostActionRefreshSelectedPersonalCargo
	}
	if result.ListType == listTypeAccountCargo {
		// list 12 是账号所有者容器：它的 op13 快照由账号 metadata + AccountInventory
		// 重建，不能用按角色的通用刷新体，否则金库表头和行会跟着当前角色变。
		postAction = alignedcmd.PostActionRefreshSelectedAccountCargo
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s sortMode=%s moved=%d range=%d-%d changed=%t; opened 0x0014 ACK then bridge-owned repository-backed sorted-container refresh",
			cmd.String(),
			result.Mode,
			result.MovedCount,
			result.StartSlot,
			result.EndSlot,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildSortItemAck(result.ListType)),
		},
		PostActions: []alignedcmd.PostAction{
			postAction,
		},
	}
}

func ownerAppliedPetEquipmentMoveResult(opcode uint16, cmd Command, result dnfequip.MoveResult) alignedcmd.Result {
	moveValue := cmd.MoveCount
	if result.Mode == "noop" {
		moveValue = 0
	}
	postActions := []alignedcmd.PostAction(nil)
	// The current EXE keeps the worn creature in actor slot 26 as one dedicated
	// client-side object. Its only in-session destroy path is a list-3 op14 row
	// carrying itemID 0xFFFFFFFF: sub_1D73120's list-3 branch then releases the
	// worn object (vtbl+0xC), clears the slot (vtbl+0xB34) and removes the row
	// from the equipment UI ([...+0x334]) in one step. Without that row the
	// object survives an unequip until a full mode1 actor rebuild, which is
	// exactly the reported "pet icon stays in the worn slot until relog".
	// The owner deletes the worn entry only for a plain unequip; unequip_swap
	// writes the displaced item into slot 26, and equip/equip_swap leave the new
	// pet there, so all of those must keep the row untouched. List-3 rows for
	// any other slot are ignored by the same client branch and are never sent.
	// 一个宠物穿脱只改 list 7 与 act 穿戴槽 26，list 0/1/2/38 完全没动：只重建
	// 真正变化的 list 7（见 alignedcmd.PostActionRefreshSelectedWornCreatureInventory
	// 的实测数据），否则客户端会先花 1.7 秒重建那 4 张无关的表，宠物行与 mode0 只能
	// 排在后面，现场就是"图标/宠物要等一下才出现"。
	var itemSlotClears []alignedcmd.ItemSlotRefresh
	if result.Changed {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedWornCreatureInventory)
		if result.DestinationSlotIndex == 26 {
			postActions = append(postActions,
				alignedcmd.PostActionRefreshSelectedCreatureState,
				alignedcmd.PostActionRefreshSelectedActorAppearance,
			)
			if result.Mode == "unequip" {
				itemSlotClears = append(itemSlotClears, alignedcmd.ItemSlotRefresh{
					ListType:  listTypeEquipment,
					SlotIndex: 26,
				})
			}
		}
	}
	// Current NoPack op19 keeps positional endpoint semantics (A is the
	// inventory/pet side, B is the worn side), but the owner may redirect A to
	// the next real empty slot while resolving an unequip against stale client
	// inventory state. Report the committed slots while retaining the raw
	// actor-worn endpoint (3 or 17): sub_1D2DE80 has distinct native branches
	// for those endpoint values.
	ackCommand := committedEquipmentMoveAckCommand(cmd, result)
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"equipment owner applied current-EXE pet move: %s equipMove=%s durableSrc=(%d,%d) durableDst=(%d,%d) item=%d swap=%d changed=%t slot26_clear=%d; opened exact op19 ACK then authoritative list7 refresh, plus op105/appearance when target26 changed",
			cmd.String(),
			result.Mode,
			result.SourceListType,
			result.SourceSlotIndex,
			result.DestinationListType,
			result.DestinationSlotIndex,
			result.ItemID,
			result.SwappedItemID,
			result.Changed,
			len(itemSlotClears),
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMoveItemspaceSuccessAck(ackCommand, moveValue)),
		},
		PostActions:                postActions,
		PostResponseItemSlotClears: itemSlotClears,
	}
}

func ownerAppliedEquipmentMoveResult(opcode uint16, cmd Command, result dnfequip.MoveResult) alignedcmd.Result {
	moveValue := cmd.MoveCount
	if result.Mode == "noop" {
		moveValue = 0
	}
	postActions := []alignedcmd.PostAction(nil)
	if result.Changed {
		// Current NoPack applies a worn avatar/equipment change through the
		// selected actor's complete mode0 appearance table. It must contain all
		// fourteen base slots, including UINT32_MAX for an empty skin/avatar slot,
		// so the client removes the previous paper-doll layer and attaches the
		// committed one in the same town scene. The bridge suppresses only the
		// already-existing actor's initialization event; it does not replay any
		// profession, skill, container, or DLL-side visual action.
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedActorAppearance)
		// Slot 24 is outside the fourteen-slot mode0 base table. Current NoPack
		// reconciles these occupied rows through class0/op342 and then asks the
		// already-bound actor to redraw through op357. Keep that projection
		// narrowly scoped to moves which actually change the durable slot 24;
		// ordinary equipment continues to use the proven mode0-only path.
		if equipmentMoveTouchesActorSlot(result, 24) {
			postActions = append(postActions, alignedcmd.PostActionRefreshSelectedActorExpandedEquipment)
		}
	}
	// Current NoPack op19 keeps positional endpoint semantics (A is the
	// inventory/avatar/cargo side, B is the worn side). The equipment owner
	// preserves that order in MoveResult while replacing A with the actual
	// committed empty slot when an unequip destination was stale. Keep the
	// request's raw actor-worn endpoint because current NoPack distinguishes
	// list 3 from list 17 even though both share the durable equipment owner.
	ackCommand := committedEquipmentMoveAckCommand(cmd, result)
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"equipment owner applied endpoint move: %s equipMove=%s requestA=(%d,%d) requestB=(%d,%d) finalA=(%d,%d) finalB=(%d,%d) item=%d swap=%d changed=%t; opened current-EXE op19 ACK then event-suppressed complete mode0 paper-doll appearance projection without item-container replay",
			cmd.String(),
			result.Mode,
			cmd.SourceListType,
			cmd.SourceSlotIndex,
			cmd.DestinationListType,
			cmd.DestinationSlotIndex,
			result.SourceListType,
			result.SourceSlotIndex,
			result.DestinationListType,
			result.DestinationSlotIndex,
			result.ItemID,
			result.SwappedItemID,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMoveItemspaceSuccessAck(ackCommand, moveValue)),
		},
		PostActions: postActions,
	}
}

func equipmentMoveTouchesActorSlot(result dnfequip.MoveResult, slot int16) bool {
	return (result.SourceListType == listTypeEquipment && result.SourceSlotIndex == slot) ||
		(result.DestinationListType == listTypeEquipment && result.DestinationSlotIndex == slot)
}

func isProvenPetEquipmentMove(cmd Command) bool {
	if cmd.SourceListType != listTypePet || !isActorWornEndpoint(cmd.DestinationListType) {
		return false
	}
	if cmd.DestinationSlotIndex == 26 {
		return cmd.SourceSlotIndex >= 0 && cmd.SourceSlotIndex <= 139
	}
	return cmd.DestinationSlotIndex >= 27 && cmd.DestinationSlotIndex <= 29 &&
		cmd.SourceSlotIndex >= 140 && cmd.SourceSlotIndex <= 188
}

func isActorWornEndpoint(listType byte) bool {
	return listType == listTypeEquipment || listType == listTypeActorWornAlt
}

func petMoveTouchesWornCreatureSlot(cmd Command, result dnfequip.MoveResult) bool {
	return isWornCreatureSlot(cmd.SourceListType, cmd.SourceSlotIndex) ||
		isWornCreatureSlot(cmd.DestinationListType, cmd.DestinationSlotIndex) ||
		isWornCreatureSlot(result.SourceListType, result.SourceSlotIndex) ||
		isWornCreatureSlot(result.DestinationListType, result.DestinationSlotIndex)
}

func isWornCreatureSlot(listType byte, slotIndex int16) bool {
	return isActorWornEndpoint(listType) && slotIndex == 26
}

func committedEquipmentMoveAckCommand(cmd Command, result dnfequip.MoveResult) Command {
	ack := cmd
	ack.SourceListType = result.SourceListType
	ack.SourceSlotIndex = result.SourceSlotIndex
	ack.DestinationListType = result.DestinationListType
	ack.DestinationSlotIndex = result.DestinationSlotIndex
	if isActorWornEndpoint(cmd.SourceListType) && isActorWornEndpoint(result.SourceListType) {
		ack.SourceListType = cmd.SourceListType
	}
	if isActorWornEndpoint(cmd.DestinationListType) && isActorWornEndpoint(result.DestinationListType) {
		ack.DestinationListType = cmd.DestinationListType
	}
	return ack
}

func validateProvenPetEquipmentMove(cmd Command) error {
	if cmd.MoveCount != 1 || cmd.DestinationStack != 0 || cmd.TrailingState0 != 0 || cmd.TrailingState1 != 0 {
		return fmt.Errorf("invalid current-EXE pet op19 constants: count=%d destination_stack=%d tail=(%d,%d)", cmd.MoveCount, cmd.DestinationStack, cmd.TrailingState0, cmd.TrailingState1)
	}
	if cmd.ActorIndex < -1 || cmd.ActorIndex > 2 {
		return fmt.Errorf("invalid current-EXE pet op19 actor index: %d", cmd.ActorIndex)
	}
	return nil
}

func equipmentMoveCommand(cmd Command) dnfequip.MoveCommand {
	return dnfequip.MoveCommand{
		SelectedCharacterID:      cmd.SelectedCharacterID,
		SourceListType:           cmd.SourceListType,
		SourceSlotIndex:          cmd.SourceSlotIndex,
		SourceInstanceValue:      cmd.SourceInstanceValue,
		MoveCount:                cmd.MoveCount,
		DestinationListType:      cmd.DestinationListType,
		DestinationSlotIndex:     cmd.DestinationSlotIndex,
		DestinationInstanceValue: cmd.DestinationInstance,
	}
}

func normalizedEquipmentMoveCommand(cmd Command) dnfequip.MoveCommand {
	ownerCommand := equipmentMoveCommand(cmd)
	if isActorWornEndpoint(ownerCommand.SourceListType) {
		ownerCommand.SourceListType = listTypeEquipment
	}
	if isActorWornEndpoint(ownerCommand.DestinationListType) {
		ownerCommand.DestinationListType = listTypeEquipment
	}
	return ownerCommand
}

func ownerAppliedRepairResult(opcode uint16, cmd Command, result RepairResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s list=%d slot=%d durability=%d->%d cost=%d gold=%d changed=%t; opened 0x0017 response in C# order",
			cmd.String(),
			result.ListType,
			result.SlotIndex,
			result.OldDurability,
			result.NewDurability,
			result.Cost,
			result.UpdatedGold,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildRepairEquipmentAck(cmd.SourceListType, result.SlotIndex, result.UpdatedGold)),
		},
	}
}

func ownerAppliedRepairAllResult(opcode uint16, cmd Command, result dnfequip.RepairResult) alignedcmd.Result {
	itemRefreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(result.RepairedSlots))
	equipmentRefreshes := make([]int16, 0, len(result.RepairedSlots))
	for _, repaired := range result.RepairedSlots {
		if repaired.Equipped {
			equipmentRefreshes = append(equipmentRefreshes, repaired.SlotIndex)
			continue
		}
		itemRefreshes = append(itemRefreshes, alignedcmd.ItemSlotRefresh{
			ListType:  listTypeMain,
			SlotIndex: repaired.SlotIndex,
		})
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"equipment owner applied repair-all: %s repaired=%d cost=%d free_repair=%t gold=%d changed=%t; opened 0x0017 slot=0xFFFF response in 86JP order",
			cmd.String(),
			result.RepairedCount,
			result.Cost,
			result.FreeRepair,
			result.UpdatedGold,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildRepairEquipmentAck(cmd.SourceListType, -1, result.UpdatedGold)),
		},
		EquipmentSlotRefreshes: equipmentRefreshes,
		ItemSlotRefreshes:      itemRefreshes,
	}
}

func ownerAppliedEquippedRepairResult(opcode uint16, cmd Command, result dnfequip.RepairResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"equipment owner applied: %s equipSlot=%d item=%d durability=%d->%d cost=%d free_repair=%t gold=%d changed=%t; opened 0x0017 response in C# order",
			cmd.String(),
			result.SlotIndex,
			result.ItemID,
			result.OldDurability,
			result.NewDurability,
			result.Cost,
			result.FreeRepair,
			result.UpdatedGold,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildRepairEquipmentAck(cmd.SourceListType, result.SlotIndex, result.UpdatedGold)),
		},
	}
}

func ownerAppliedUseStackableResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	responses := []alignedcmd.UpperResponse{
		class1Response(opcode, buildUseStackableSuccessAck(result)),
	}
	postActions := make([]alignedcmd.PostAction, 0, 1)
	var itemSlotRefreshes []alignedcmd.ItemSlotRefresh
	if result.Changed {
		itemSlotRefreshes = append(itemSlotRefreshes, alignedcmd.ItemSlotRefresh{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		})
	}
	for _, slot := range result.RandomRewardSlots {
		itemSlotRefreshes = append(itemSlotRefreshes, alignedcmd.ItemSlotRefresh{
			ListType:  listTypeMain,
			SlotIndex: slot,
		})
	}
	reason := fmt.Sprintf(
		"inventory owner applied: %s item=%d remaining=%d pvf_path=%q stackable_type=%q changed=%t; opened exact current-EXE class1/op44 success response + one-row op14 refresh",
		cmd.String(),
		result.ItemID,
		result.RemainingCount,
		result.PVFPath,
		result.StackableType,
		result.Changed,
	)
	if result.TransformationApplied {
		reason = fmt.Sprintf(
			"inventory owner applied native transformation: %s item=%d remaining=%d action=%q pvf_path=%q changed=%t; exact current-EXE op44 success invokes the item's client PVF action, followed by one-row op14 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.TransformationAction,
			result.PVFPath,
			result.Changed,
		)
	}
	if result.ProjectileApplied {
		reason = fmt.Sprintf(
			"inventory owner applied native projectile: %s item=%d remaining=%d effect_id=%d pvf_path=%q changed=%t; exact current-EXE op44 success invokes the item's client PVF projectile action, followed by one-row op14 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.ProjectileEffectID,
			result.PVFPath,
			result.Changed,
		)
	}
	if result.AvatarFittingRoomOpened {
		reason = fmt.Sprintf(
			"inventory owner opened avatar fitting room: %s ticket=%d available=%d pvf_path=%q; op44 acknowledges the proven ticket without consuming it, op1488 owns the later per-part debit",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.PVFPath,
		)
	}
	// 城镇增益药剂（史诗Buff药剂一族）：把增益窗口写进日志，运营在游戏里点一次之后
	// 直接用 "epic_drop_rate_buff" 搜日志就能确认"药吃了、窗口落了、到期时间是多少"。
	if result.StatChangePotionApplied {
		reason = fmt.Sprintf(
			"inventory owner applied stat-change potion: %s item=%d remaining=%d duration=%ds epic_drop_rate_buff_percent=%d epic_drop_rate_buff_expire_at=%d pvf_path=%q changed=%t; atomic source decrement + character drop-rate window, then class1 op44 ACK and one-row op14 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.StatChangePotionSeconds,
			result.EpicDropRatePercent,
			result.EpicDropRateExpireAt,
			result.PVFPath,
			result.Changed,
		)
	}
	if result.CustomAbilityGroupConverterOpened {
		reason = fmt.Sprintf(
			"inventory owner opened custom-ability group converter: %s device=%d available=%d pvf_path=%q; op44 proves the source without consuming it and op1340 owns the later atomic option replacement plus one-device debit",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.PVFPath,
		)
	}
	if result.DungeonAdmissionTicketApplied {
		reason = fmt.Sprintf(
			"inventory/admission settlement owner applied: %s ticket=%d remaining=%d action=%q policy_count=%d bonus_each=%d remaining_daily_uses=%d; atomic source decrement + service-window allowance, then native op44 action and one-row op14 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.DungeonAdmissionAction,
			result.DungeonAdmissionPolicyCount,
			result.DungeonAdmissionBonus,
			result.DungeonAdmissionRemainingUses,
		)
	}
	if result.PremiumActivated {
		// Current EXE class0/op66 (sub_1D61460) reads u16 sub-op + u8 type +
		// raw[8] expiry; sub-op 2 is the activate/set path (86JP
		// PremiumService.ActivateAndNotify body matches it exactly). The 0x0312
		// service-data blob is old-client-only (current 786 = skill quick slot
		// sort) and is never pushed. Internal devil slot types 580..587 cannot
		// fit this body and are deliberately suppressed.
		activations := result.PremiumActivations
		if len(activations) == 0 {
			activations = []PremiumActivation{{
				PremiumType:      result.PremiumType,
				RemainingSeconds: result.PremiumRemainingSeconds,
			}}
		}
		for _, activation := range activations {
			if premium.CanNotifyActivation(activation.PremiumType) {
				responses = append(responses, class0Response(
					premiumActivatedNotifyMsgID,
					premium.BuildActivatedBody(activation.PremiumType, activation.RemainingSeconds),
				))
			}
		}
		reason = fmt.Sprintf(
			"inventory owner applied: %s contract item=%d remaining=%d premium_type=%d premium_remaining=%ds changed=%t; opened op44 ACK + NOTI 0x0042",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.PremiumType,
			result.PremiumRemainingSeconds,
			result.Changed,
		)
		if result.PremiumType == premium.TypeCrystal {
			postActions = append(postActions, alignedcmd.PostActionRefreshCrystalContractState)
		}
	}
	if result.ExperienceBonusActivated {
		reason = fmt.Sprintf(
			"inventory owner applied: %s experience consumable item=%d remaining=%d bonus_percent=%d effect_remaining=%ds changed=%t; atomic op44 decrement + account effect activation",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.ExperienceBonusPercent,
			result.ExperienceBonusRemainingSeconds,
			result.Changed,
		)
	}
	if result.DirectExperienceApplied {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedCharacterProgression)
		reason = fmt.Sprintf(
			"inventory/progression settlement owner applied: %s direct-experience item=%d remaining=%d gain=%d level=%d->%d total=%d->%d sp_gain=%d tp_gain=%d; atomic source decrement + character EXP/level + SP/TP ledger, then op44 ACK/source op14/native skill refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.DirectExperienceGain,
			result.DirectExperiencePreviousLevel,
			result.DirectExperienceNewLevel,
			result.DirectExperiencePreviousTotal,
			result.DirectExperienceNewTotal,
			result.DirectExperienceSPGain,
			result.DirectExperienceTPGain,
		)
	}
	if result.TPBookApplied {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedActorSkills)
		reason = fmt.Sprintf(
			"inventory/skill settlement owner applied: %s SP/TP book item=%d remaining=%d kind=%d sp_delta=%d tp_delta=%d tp_reset=%t available_sp=%d->%d available_tp=%d->%d; atomic source decrement + point bonus/reset + every skill-tree ledger, then op44 ACK/source op14/native op19 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.SkillPointBookKind,
			result.SkillPointBookSPDelta,
			result.SkillPointBookTPDelta,
			result.SkillPointBookTPReset,
			result.SkillPointBookPreviousSP,
			result.SkillPointBookNewSP,
			result.SkillPointBookPreviousTP,
			result.SkillPointBookNewTP,
		)
	}
	if result.LevelUpTicketApplied {
		reason = fmt.Sprintf(
			"inventory/progression/quest settlement owner applied: %s level-up ticket item=%d range=%d remaining=%d level=%d->%d total=%d->%d sp_gain=%d tp_gain=%d completed_pre_upgrade_quests=%d; atomic source decrement + character EXP/level + SP/TP + no-reward quest completion, then op44 ACK/source op14/native op37/op19/quest refresh",
			cmd.String(),
			result.ItemID,
			result.LevelUpTicketRange,
			result.RemainingCount,
			result.DirectExperiencePreviousLevel,
			result.DirectExperienceNewLevel,
			result.DirectExperiencePreviousTotal,
			result.DirectExperienceNewTotal,
			result.DirectExperienceSPGain,
			result.DirectExperienceTPGain,
			result.QuestCompletionCount,
		)
	}
	if result.ReviveCoinWalletUpdated {
		itemSlotRefreshes = append(itemSlotRefreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: dnfrevivecoin.WalletSlot,
		})
		reason = fmt.Sprintf(
			"inventory owner applied: %s revive-coin consumable item=%d remaining=%d wallet_total=%d changed=%t; atomic source decrement + slot1 wallet increment, then exact op44 ACK + two-row op14 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.ReviveCoinWalletTotal,
			result.Changed,
		)
	}
	if result.RandomRewardItemID > 0 {
		reason = fmt.Sprintf(
			"inventory owner applied: %s random reward item=%d remaining=%d reward=%d slots=%v; opened exact op44 ACK then refreshed source and reward rows",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.RandomRewardItemID,
			result.RandomRewardSlots,
		)
	}
	if result.PetFeedApplied {
		// The current client consumes the remaining stack count from the op44
		// acknowledgement. A list-7 op14 row immediately after that ACK can
		// invalidate the pet panel's native object, so publish the authoritative
		// op105/op102 growth state through the bridge instead.
		itemSlotRefreshes = nil
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedCreatureGrowthState)
		reason = fmt.Sprintf(
			"inventory owner applied: %s pet feed item=%d remaining=%d pet=%s satiety=%d->%d applied_delta=%d skill_charge=%d%% duration=%ds; atomic list7 decrement + pet-state update, then exact op44 ACK + immediate op105/op102 creature refresh (item rows suppressed)",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.PetFeedPetKey,
			result.PetFeedSatietyBefore,
			result.PetFeedSatietyAfter,
			result.PetFeedSatietyDelta,
			result.PetFeedSkillChargePercent,
			result.PetFeedEffectDurationSeconds,
		)
	}
	if result.QuestCompletionTicketApplied && !result.LevelUpTicketApplied {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedQuestState)
		if result.QuestCompletionProfessionChange {
			postActions = append(postActions,
				alignedcmd.PostActionRefreshSelectedActorAppearance,
				alignedcmd.PostActionRefreshSelectedActorSkills,
			)
		}
		reason = fmt.Sprintf(
			"inventory/quest settlement owner applied: %s quest-ticket item=%d action=%q completed=%d remaining=%d profession_changed=%t; atomic source decrement + no-reward quest completion, then op44 ACK/source op14/native quest snapshots",
			cmd.String(),
			result.ItemID,
			result.QuestCompletionAction,
			result.QuestCompletionCount,
			result.RemainingCount,
			result.QuestCompletionProfessionChange,
		)
	}
	if result.StoryBookApplied {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedQuestState)
		reason = fmt.Sprintf(
			"inventory/quest settlement owner applied: %s story-book item=%d quest=%d trigger=%d remaining=%d; atomic source decrement + PVF quest acceptance, then op44 ACK/source op14/native quest refresh",
			cmd.String(),
			result.ItemID,
			result.StoryBookQuestID,
			result.StoryBookInitTrigger,
			result.RemainingCount,
		)
	}
	alignedResult := alignedcmd.Result{
		Handled:           true,
		ResponseAllowed:   true,
		Operation:         cmd.Operation,
		Reason:            reason,
		UpperResponses:    responses,
		ItemSlotRefreshes: itemSlotRefreshes,
		PostActions:       postActions,
		// A [total usable count] item must publish its used count here: the
		// client only ever learns 每天可用次数 from op541.
		DailyUseStates: dailyUseStates(result),
		// 冷却窗口只上报给 bridge 记账/打日志：客户端自己按同一份 PVF 起本地冷却，
		// 重上之后的恢复走类 0 通知 172（item_cooltime_projection.go）。
		ItemCooltimeStates: cooltimeStates(result),
	}
	// 城镇增益药剂：使用这一发**只补发 buff 注册**（class0/op487 CHARACTER_ADD_BUFF）——
	// buff 栏图标就是它画出来的。协议形状与参考服务端 A21 的 EpicBuffPotion 一致，
	// 协议号按**本客户端**的通知名表定标（基址 485，见 stat_change_potion_projection.go）。
	//
	// 这里**故意不发** class0/op174（load effect item info）：服主 2026-10-03 反馈
	// "药剂刚能用那一版（服务端什么包都没发）提示只有一行，后来变成两行"。原因就是
	// 客户端自己按本地使用（op44）登记了效果道具并打了一行，op174 再登记一次就成了
	// 第二行 —— 这一点与既有经验药水路径的注释一致：
	//
	//	sendCurrentExperienceConsumableBuffProjection: "First use is handled locally
	//	by op44; op174 recreates the same item-owned effect with its durable
	//	remaining duration."（只在登录/重选恢复时发）
	//
	// 所以 op174 走"恢复"路径（bridge 的 sendCurrentStatChangePotionBuffProjection），
	// 使用路径不再发。
	// 使用路径只发 buff 注册（图标），数量=1；op174 只在恢复路径发（见上面的注释）。
	//
	// 强化/增幅成功率药剂走**同一条管道**（服主："套路都是一样的"）：它们同样有 buff id
	// （赛丽亚的幸运强化药剂掷到 1097/1098/1099）与到期时刻，客户端同样按 buff id 画图标，
	// 所以投影结构一模一样，只是数据来自 UpgradeRate* 而不是 EpicDropRate*。
	if result.StatChangePotionApplied {
		buffID, expireAtUnix, removeBuffID := int64(0), int64(0), int64(0)
		switch {
		case result.EpicDropRateExpireAt > 0 && result.EpicDropRateBuffID > 0:
			buffID, expireAtUnix = result.EpicDropRateBuffID, result.EpicDropRateExpireAt
			removeBuffID = result.PreviousEpicDropRateBuffID
		case result.UpgradeRateExpireAt > 0 && result.UpgradeRateBuffID > 0:
			buffID, expireAtUnix = result.UpgradeRateBuffID, result.UpgradeRateExpireAt
			removeBuffID = result.PreviousUpgradeRateBuffID
		case result.DungeonClearExpExpireAt > 0 && result.DungeonClearExpBuffID > 0:
			buffID, expireAtUnix = result.DungeonClearExpBuffID, result.DungeonClearExpExpireAt
		case result.ClientBuffExpireAt > 0 && result.ClientBuffID > 0:
			// 客户端自施放的那一族（城镇移速 / 进图属性）：同样要把 buff 注册过去，
			// 客户端才会画图标并按自己的 buff 表施加效果。
			buffID, expireAtUnix = result.ClientBuffID, result.ClientBuffExpireAt
		}
		if buffID > 0 && buffID <= math.MaxInt32 {
			projection := &alignedcmd.CharacterAddBuffProjection{
				BuffID:       int32(buffID),
				ExpireAtUnix: expireAtUnix,
			}
			if removeBuffID > 0 && removeBuffID != buffID && removeBuffID <= math.MaxInt32 {
				projection.RemoveBuffID = int32(removeBuffID)
			}
			alignedResult.CharacterAddBuff = projection
		}
	}
	if result.CustomAbilityGroupConverterOpened {
		alignedResult.CustomAbilityGroupConverter = &alignedcmd.CustomAbilityGroupConverterProjection{
			CharacterID:     cmd.SelectedCharacterID,
			SourceListType:  result.ListType,
			SourceSlotIndex: result.SlotIndex,
			SourceInstance:  result.InstanceValue,
			SourceItemID:    result.ItemID,
		}
	}
	return alignedResult
}

func appliedEventServerLevelUpResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory/progression/quest settlement owner applied current NoPack op418: %s item=%d range=%d remaining=%d gain=%d level=%d->%d sp_gain=%d tp_gain=%d completed_pre_upgrade_quests=%d; exact native delta ACK then source op14 and absolute progression projections",
			cmd.String(),
			result.ItemID,
			result.LevelUpTicketRange,
			result.RemainingCount,
			result.DirectExperienceGain,
			result.DirectExperiencePreviousLevel,
			result.DirectExperienceNewLevel,
			result.DirectExperienceSPGain,
			result.DirectExperienceTPGain,
			result.QuestCompletionCount,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildEventServerLevelUpSuccessAck(result)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedCharacterProgression,
		},
	}
}

const increaseStatusFailureCode byte = 17

// increaseStatusAck picks the op30 branch from the committed result. SP and TP
// point gains carry their amount in the u32 and select the matching HUD gauge
// and message string; every other outcome the client has no branch for — a TP
// reset and every experience book — acknowledges silently so only the following
// op19/op37 projections speak.
//
// sourceSlot is the main-inventory slot the book was consumed from. It is not
// cosmetic: the client looks it up in its main-inventory array and consumes that
// row for every kind (see buildIncreaseStatusSuccessAck), so sending anything
// else — the character id, as this used to — consumes whatever item happens to
// sit at that index.
func increaseStatusAck(sourceSlot uint16, result UseStackableResult) []byte {
	switch {
	case result.TPBookApplied && result.SkillPointBookSPDelta > 0:
		return buildIncreaseStatusSuccessAck(sourceSlot, increaseStatusKindSP, uint32(result.SkillPointBookSPDelta))
	case result.TPBookApplied && result.SkillPointBookTPDelta > 0:
		return buildIncreaseStatusSuccessAck(sourceSlot, increaseStatusKindTP, uint32(result.SkillPointBookTPDelta))
	default:
		return buildIncreaseStatusSuccessAck(sourceSlot, increaseStatusKindSilent, 0)
	}
}

func appliedIncreaseStatusResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	postActions := make([]alignedcmd.PostAction, 0, 1)
	reason := ""
	if result.TPBookApplied {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedActorSkills)
		reason = fmt.Sprintf(
			"inventory/skill settlement owner applied current NoPack op30: %s SP/TP book item=%d remaining=%d kind=%d sp_delta=%d tp_delta=%d tp_reset=%t available_sp=%d->%d available_tp=%d->%d; class1 op30 gain ACK then source op14 and native op19 refresh",
			cmd.String(), result.ItemID, result.RemainingCount, result.SkillPointBookKind,
			result.SkillPointBookSPDelta, result.SkillPointBookTPDelta, result.SkillPointBookTPReset,
			result.SkillPointBookPreviousSP, result.SkillPointBookNewSP,
			result.SkillPointBookPreviousTP, result.SkillPointBookNewTP,
		)
	}
	if result.DirectExperienceApplied {
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedCharacterProgression)
		reason = fmt.Sprintf(
			"inventory/progression settlement owner applied current NoPack op30: %s experience book item=%d remaining=%d gain=%d level=%d->%d total=%d->%d sp_gain=%d tp_gain=%d; silent class1 op30 ACK then source op14 and the absolute progression projections that own the experience bar and level-up presentation",
			cmd.String(), result.ItemID, result.RemainingCount, result.DirectExperienceGain,
			result.DirectExperiencePreviousLevel, result.DirectExperienceNewLevel,
			result.DirectExperiencePreviousTotal, result.DirectExperienceNewTotal,
			result.DirectExperienceSPGain, result.DirectExperienceTPGain,
		)
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          reason,
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, increaseStatusAck(uint16(cmd.SourceSlotIndex), result)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
		PostActions: postActions,
	}
}

// rejectedIncreaseStatusResult must always answer. An unanswered op30 leaves
// the client parked in its item wait state, after which every later action
// reports "请关闭正在使用的其他窗口后再尝试" until the character returns to select.
func rejectedIncreaseStatusResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"parsed %s but the increase-status book owner rejected it: %v; returned current NoPack op30 error 17 to release the pending item-use state",
			cmd.String(),
			err,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildIncreaseStatusFailureAck(increaseStatusFailureCode)),
		},
	}
}

const useStackableSilentFailureCode byte = 17

const useStackableActionFailureCode byte = 17

const eventServerLevelUpFailureCode byte = 17

func rejectedEventServerLevelUpResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"parsed %s but event-server level-up owner rejected it: %v; returned current NoPack op418 error 17 to release the pending coupon state",
			cmd.String(),
			err,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildEventServerLevelUpFailureAck(eventServerLevelUpFailureCode)),
		},
	}
}

func ownerRejectedUseStackableResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"parsed %s but inventory owner rejected it: %v; returned current-EXE op44 silent error 17 to release pending item-use state",
			cmd.String(),
			err,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableFailureAck(cmd, useStackableSilentFailureCode)),
		},
	}
}

func ownerRejectedUseStackableActionResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"parsed %s but stackable-action owner rejected it: %v; returned current-EXE op515 error 17 to release the shared pending item-operation state",
			cmd.String(),
			err,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionFailureAck(useStackableActionFailureCode)),
		},
	}
}

func ownerAppliedDungeonAdmissionTicketActionResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory/admission settlement owner applied: %s ticket=%d remaining=%d action=%q policy_count=%d bonus_each=%d remaining_daily_uses=%d; atomic source decrement + service-window allowance, then native op515 action-1 ACK and source op14 refresh",
			cmd.String(), result.ItemID, result.RemainingCount, result.DungeonAdmissionAction,
			result.DungeonAdmissionPolicyCount, result.DungeonAdmissionBonus, result.DungeonAdmissionRemainingUses,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{ListType: result.ListType, SlotIndex: result.SlotIndex}},
	}
}

func ownerAppliedSharedEffectItemResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	itemSlotRefreshes := []alignedcmd.ItemSlotRefresh{{
		ListType:  result.ListType,
		SlotIndex: result.SlotIndex,
	}}
	for _, slot := range result.RandomRewardSlots {
		itemSlotRefreshes = append(itemSlotRefreshes, alignedcmd.ItemSlotRefresh{
			ListType:  listTypeMain,
			SlotIndex: slot,
		})
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s item=%d remaining=%d reward=%d reward_slots=%v pvf_path=%q changed=%t; opened exact current-EXE class1/type251 result-byte ACK then source/reward op14 refresh",
			cmd.String(),
			result.ItemID,
			result.RemainingCount,
			result.RandomRewardItemID,
			result.RandomRewardSlots,
			result.PVFPath,
			result.Changed,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseSharedEffectItemSuccessAck()),
		},
		ItemSlotRefreshes: itemSlotRefreshes,
	}
}

func ownerAppliedDamageFontUnlockResult(opcode uint16, cmd Command, result DamageFontUnlockResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"damage-font owner applied: slot=%d item=%d font=%d expires_at=%d remaining=%d pvf_path=%q; op515 ACK then op14 slot refresh then NOTI 1239",
			result.SourceSlotIndex, result.ItemID, result.FontIndex, result.ExpiresAt, result.RemainingCount, result.PVFPath,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SourceSlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  listTypeMain,
			SlotIndex: result.SourceSlotIndex,
		}},
		PostActions: []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedDamageFontState},
	}
}

// dailyUseStates is the client-facing 每日可用次数 state one capped item use
// produces, for ANY item whose PVF definition carries [total usable count].
//
// The byte is the **used** count. Live 2026-09-15 settled it: with one potion
// already spent (durable counter 1) the server sent 2, and the item tooltip
// rendered "1/3" -- i.e. 上限 - 该字节, exactly the arithmetic the client's own
// accessor sub_01F17F70 performs ([item+0x470] - [item+0x46C]). The earlier
// "inverted" reading came from the packet being ignored entirely while it was
// sent as class 1.
func dailyUseStates(result UseStackableResult) []alignedcmd.DailyUseState {
	itemID := result.DailyUseItemID
	if itemID <= 0 {
		itemID = result.ItemID
	}
	if result.DailyUseLimit <= 0 || result.DailyUseUsed < 0 || itemID <= 0 || itemID > 0xFFFFFFFF {
		return nil
	}
	used := result.DailyUseUsed
	if used > 0xFF {
		used = 0xFF
	}
	return []alignedcmd.DailyUseState{{ItemID: uint32(itemID), Used: byte(used)}}
}

// cooltimeStates is the durable cooldown window one use started, for ANY item
// whose PVF declares `[cool time]` together with `[cooltime maintenance]`.
//
// The client's own gate lives in process memory only (cmd/scratch/potionpatch:
// "no packet is sent while the client blocks the action"), so a 小退/重上 wipes
// it. The window therefore has to be reported by the server, and the bridge logs
// it as the evidence that the durable half of the contract exists.
func cooltimeStates(result UseStackableResult) []alignedcmd.ItemCooltimeState {
	if result.CooltimeItemID <= 0 || result.CooltimeExpireAt <= 0 {
		return nil
	}
	return []alignedcmd.ItemCooltimeState{{
		ItemID:   result.CooltimeItemID,
		ExpireAt: result.CooltimeExpireAt,
	}}
}

func ownerAppliedFatiguePotionActionResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"fatigue-potion owner applied: %s item=%d recovery=%d fatigue=%d->%d remaining=%d; op515 ACK/source op14 then absolute fatigue refresh",
			cmd.String(), result.ItemID, result.FatigueRecovery, result.FatigueBefore, result.FatigueAfter, result.RemainingCount,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
		PostActions:    []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedFatigue},
		DailyUseStates: dailyUseStates(result),
	}
}

func ownerAppliedRentalPointActionResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"rental-point owner applied: %s item=%d granted=%d wallet=%d->%d limit=%d at_limit=%t remaining=%d; op515 action-105 ACK/source op14 then absolute op985 wallet refresh",
			cmd.String(), result.ItemID, result.RentalPointGranted, result.RentalPointBefore,
			result.RentalPointAfter, result.RentalPointLimit, result.RentalPointAtLimit, result.RemainingCount,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
		PostActions: []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedRentalWallet},
	}
}

func ownerAppliedQuestCompletionTicketActionResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	postActions := []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedQuestState}
	if result.QuestCompletionProfessionChange {
		postActions = append(postActions,
			alignedcmd.PostActionRefreshSelectedActorAppearance,
			alignedcmd.PostActionRefreshSelectedActorSkills,
		)
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory/quest settlement owner applied: %s quest-ticket item=%d action=%q completed=%d remaining=%d profession_changed=%t; atomic source decrement + no-reward quest completion, then op515 ACK/source op14/native quest snapshots",
			cmd.String(),
			result.ItemID,
			result.QuestCompletionAction,
			result.QuestCompletionCount,
			result.RemainingCount,
			result.QuestCompletionProfessionChange,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
		PostActions: postActions,
	}
}

// ownerAppliedContainerUpgradeResult 是扩容券落地后的回包与投影计划。
//
// 顺序沿用 cera_shop_purchase.go 那条已在游戏里验过的容量类路径：
//
//	背包（list 0）   → 券槽位 op14 增量清行 + list 0 整表重建
//	金库（list 2）   → 券槽位 op14 + op391 重建仓库底板 + list 2 整表（先底板后列表）
//	帐号金库（list12）→ 券槽位 op14 + op391 重建仓库底板 + list 12 整表
//
// PostActions 是**按切片顺序**执行的，所以"底板 → 列表"这两发必须挨着且顺序不能反。
func ownerAppliedContainerUpgradeResult(opcode uint16, cmd Command, result UseStackableResult) alignedcmd.Result {
	postActions := make([]alignedcmd.PostAction, 0, 2)
	if result.ContainerUpgradeNeedsCargoPad {
		postActions = append(postActions, alignedcmd.PostActionResetCargoPadBeforeItemLists)
	}
	switch result.ContainerUpgradeListType {
	case listTypePersonalCargo:
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedPersonalCargo)
	case listTypeAccountCargo:
		postActions = append(postActions, alignedcmd.PostActionRefreshSelectedAccountCargo)
	}
	var itemListRebuilds []byte
	if result.ContainerUpgradeListType == listTypeMain {
		itemListRebuilds = []byte{listTypeMain}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory container-upgrade owner applied: %s kind=%s voucher=%d remaining=%d %d->%d list=%d cargo_pad=%t pvf_path=%q; atomic voucher decrement + container tier advance, then class1 ACK + source op14 + container rebuild",
			cmd.String(),
			result.ContainerUpgradeKind,
			result.ItemID,
			result.RemainingCount,
			result.ContainerUpgradeBefore,
			result.ContainerUpgradeAfter,
			result.ContainerUpgradeListType,
			result.ContainerUpgradeNeedsCargoPad,
			result.PVFPath,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildUseStackableActionSuccessAck(result.SlotIndex, result.RemainingCount)),
		},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{
			ListType:  result.ListType,
			SlotIndex: result.SlotIndex,
		}},
		ItemListRebuilds: itemListRebuilds,
		PostActions:      postActions,
	}
}

func ownerAppliedDamageFontSelectionResult(opcode uint16, cmd Command, result DamageFontSelectionResult) alignedcmd.Result {
	body := buildSelectDamageFontErrorAck(damageFontSelectionErrorUnavailable)
	reason := fmt.Sprintf("damage font %d is not owned or has expired; returned current-EXE error 17", result.FontIndex)
	if result.Success {
		body = buildSelectDamageFontSuccessAck(result.FontIndex)
		reason = fmt.Sprintf("selected damage font %d persisted; returned current-EXE op1288 success ACK", result.FontIndex)
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason:          reason,
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, body),
		},
	}
}

func ownerBlockedResult(cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: false,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("已解析 %s：%s；inventory owner 未落库：%v；禁止回成功包", cmd.Operation, cmd.String(), err),
	}
}

const moveItemspaceRejectedErrorCode byte = 17

// moveItemspaceDestinationFullErrorCode 是"目标容器放不下这一堆"时回给客户端的 op19
// 失败码。现网证据：同一客户端把失败码 4 显示成"物品栏空间不足"（modules/dnf/cargo
// 里帐号金库 64->72 档被拒那次实测），86JP InventoryMoveService 也把 DestinationRejected
// 这类插入失败映射成 0x04，所以这里取 4。若现场文案不对，改这一个常量即可。
const moveItemspaceDestinationFullErrorCode byte = 4

// ownerRejectedMoveSpaceResult 是和 ownerRejectedQuickSlotCharmMoveResult 同一形状的
// 失败帧：sub_1D2DE80 消费 result/error 两个字节后还要读源/目标 list owner 才能释放
// 拖拽状态。不回帧，客户端就只是"没反应"（现场就是仓库满了没提示也放不进去）。
func ownerRejectedMoveSpaceResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"rejected move without mutation: %s: %v; returned current-EXE op19 error %d (destination container has no room)",
			cmd.String(),
			err,
			moveItemspaceDestinationFullErrorCode,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMoveItemspaceFailureAck(cmd, moveItemspaceDestinationFullErrorCode)),
		},
	}
}

// moveItemspaceRejectionErrorCode 把 owner 对一次已解析 move 的拒绝原因映射成 op19 的失败码。
//
// 容量/栏位一类（目标放不下、超过负重、账号金库栏位未解锁、金库未创建）统一回
// moveItemspaceDestinationFullErrorCode：现场这台客户端把 4 显示成"物品栏空间不足"，
// 语义上就是"这边放不了"；若要改文案，动这一个常量即可。其余拒绝回通用拒绝码。
func moveItemspaceRejectionErrorCode(err error) byte {
	switch {
	case errors.Is(err, ErrMoveDestinationFull),
		errors.Is(err, ErrMoveCarryLimitExceeded),
		errors.Is(err, ErrAccountCargoSlotOutOfRange),
		errors.Is(err, ErrAccountCargoNotCreated):
		return moveItemspaceDestinationFullErrorCode
	case errors.Is(err, ErrEmblemWarehouseForbidden), errors.Is(err, ErrAccountCargoItemNotAllowed):
		// 徽章禁存仓库与"这件道具不许进金库"是同一类：**不允许**，不是"放不下"。
		// 必须避开失败码 4 —— 那在客户端会显示成「物品栏空间不足」，而仓库明明空着，
		// 玩家会以为是格子问题。这里回通用拒绝码（17），与既有不可交易道具进金库同口径。
		return moveItemspaceRejectedErrorCode
	default:
		return moveItemspaceRejectedErrorCode
	}
}

// ownerRejectedMoveRequestResult 是"请求已经解析成一次 move、但 durable owner 拒绝"的统一
// 失败帧。
//
// 现场 2026-09-18：把道具拖进账号金库**未解锁**的栏位（dst=(12,28)，容量 8）时 owner 以
// ErrAccountCargoSlotOutOfRange 拒绝，旧代码落到 ownerBlockedResult 不回帧。客户端
// sub_1D2DE80 在收到 class1/op19 之前一直保持 pending 拖拽对象，于是金库/装备/消耗品窗口
// 全部点不动，而且它此后一个包都不再发（packet_log 里 RECV 从此静默，只剩服务端心跳）。
// 所以：只要请求已解析成 move，被 owner 拒绝就必须回这一帧——绝不静默。
func ownerRejectedMoveRequestResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	code := moveItemspaceRejectionErrorCode(err)
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"rejected move without mutation: %s: %v; returned current-EXE op19 error %d with source/destination list owners to release pending drag state",
			cmd.String(),
			err,
			code,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMoveItemspaceFailureAck(cmd, code)),
		},
	}
}

// ownerRejectedQuickSlotCharmMoveResult is deliberately narrower than the
// generic ownerBlockedResult.  Current NoPack sub_1D2DE80 consumes the common
// result/error bytes on a failed class1/op19, then reads the source and
// destination list owners before releasing its pending drag state.  Returning
// no frame leaves that state armed; returning a success frame enters the native
// move path even though the transaction rolled back.  This exact failure frame
// rejects only the second PVF [charm] while keeping both durable slots intact.
func ownerRejectedQuickSlotCharmMoveResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"rejected second quick-slot charm without mutation: %s: %v; returned current-EXE op19 error %d with source/destination list owners to release pending drag state",
			cmd.String(),
			err,
			moveItemspaceRejectedErrorCode,
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildMoveItemspaceFailureAck(cmd, moveItemspaceRejectedErrorCode)),
		},
	}
}

func ownerAppliedResult(cmd Command, detail string) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: false,
		Operation:       cmd.Operation,
		Reason:          fmt.Sprintf("已写入 inventory owner：%s %s；缺少 mutation id 与当前 EXE 的 %s/背包刷新/USERINFO 顺序证据，暂不回成功包", cmd.String(), detail, ackEvidenceLabel(cmd.Operation)),
	}
}

func ownerAppliedDeleteResult(opcode uint16, cmd Command, result DeleteResult) alignedcmd.Result {
	postActions := make([]alignedcmd.PostAction, 0, 1)
	if result.Changed && result.DeletedAccountMaterial {
		// 副本内删除账号共享晶体/灵魂材料后，bridge 需补空的 S0/0012 完成帧；
		// 是否在副本由 bridge 按 session.dungeon.runtime 判定并在城镇 no-op。
		postActions = append(postActions, alignedcmd.PostActionSendDeleteItemAccountMaterialCompletion)
	}
	if cmd.DeleteStateTrailer {
		refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(result.Removed))
		for _, item := range result.Removed {
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
				ListType:  item.ListType,
				SlotIndex: item.SlotIndex,
			})
		}
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf(
				"inventory owner applied skill-material delete: %s removed=%d changed=%t accountMaterial=%t; returned empty op18 RETURN then authoritative op14 rows",
				cmd.String(),
				len(result.Removed),
				result.Changed,
				result.DeletedAccountMaterial,
			),
			UpperResponses: []alignedcmd.UpperResponse{
				class1Response(opcode, buildDungeonSkillMaterialDeleteAck(true)),
			},
			ItemSlotRefreshes: refreshes,
			PostActions:       postActions,
		}
	}
	responses := make([]alignedcmd.UpperResponse, 0, 1)
	if len(result.Removed) > 0 {
		responses = append(responses, class1Response(opcode, buildDeleteItemAck(result.Removed)))
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: len(responses) > 0,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"inventory owner applied: %s removed=%d changed=%t accountMaterial=%t; opened 0x0012 response in C# order",
			cmd.String(),
			len(result.Removed),
			result.Changed,
			result.DeletedAccountMaterial,
		),
		UpperResponses: responses,
		PostActions:    postActions,
	}
}

// deleteItemLockedErrorCode 是"物品处于锁定状态"在 op18 失败表里的码。
//
// 反查结果（客户端通用映射器 0x032DDC70）：`cmp eax, 0xd6 / jne` → `push 0;
// push 0xa4f8` → GetText(0x0367BE00) → show(0x02FDD1A0)，即 214 → dstr 42232
// 「在物品已锁定状态下，您无法进行当前操作。」op18 的失败分支 0x01D21E8B 对表外
// 的码落到 0x01D21F3B 再 `call 0x32DDC70`，所以 214 在 op18 上会渲染 42232。
//
// 之前这里对所有拒绝一律回 4，而 4 在 op18 表里是"要丢弃的物品不存在或数量不足"
// 之外的通用拒绝 —— 玩家把锁定物品拖去删除时看到的是一句与锁定无关的话（现场
// 2026-09-15：锁定时点删除/丢弃完全没有提示）。
const (
	deleteItemFailureErrorCode byte = 4
	deleteItemLockedErrorCode  byte = 214
)

func deleteItemRejectionErrorCode(err error) byte {
	if errors.Is(err, ErrItemLocked) {
		return deleteItemLockedErrorCode
	}
	return deleteItemFailureErrorCode
}

func ownerRejectedDeleteResult(opcode uint16, cmd Command, err error) alignedcmd.Result {
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf(
			"parsed %s but delete owner rejected it: %v; returned empty op18 failure RETURN with code %d to release the shared item-operation pending state",
			cmd.String(),
			err,
			deleteItemRejectionErrorCode(err),
		),
		UpperResponses: []alignedcmd.UpperResponse{
			class1Response(opcode, buildDungeonSkillMaterialDeleteAckWithCode(deleteItemRejectionErrorCode(err))),
		},
	}
}

func class1Response(opcode uint16, body []byte) alignedcmd.UpperResponse {
	return alignedcmd.UpperResponse{
		MsgID:          opcode,
		Body:           body,
		Classification: dnfproto.DefaultChannelClassification,
		AllowCodec:     true,
	}
}

func class0Response(msgID uint16, body []byte) alignedcmd.UpperResponse {
	return alignedcmd.UpperResponse{
		MsgID:          msgID,
		Body:           body,
		Classification: 0,
		AllowCodec:     true,
	}
}

func ackEvidenceLabel(operation string) string {
	switch operation {
	case "delete_item":
		return "0x0012"
	case "sell_item":
		return "0x0016"
	case "move_itemspace":
		return "0x0013"
	case "sort_item":
		return "0x0014"
	case "repair_equipment":
		return "0x0017"
	default:
		return "对应 ACK"
	}
}

func blockedParsedResult(operation string, summary string, err error) alignedcmd.Result {
	if err != nil {
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: false,
			Operation:       operation,
			Reason:          fmt.Sprintf("%s 请求体解析失败：%v；禁止回成功包", operation, err),
		}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: false,
		Operation:       operation,
		Reason:          fmt.Sprintf("已解析 %s：%s；资产写路径需接入 durable owner、事务、幂等和当前 EXE S2C 刷新顺序后才能回包", operation, summary),
	}
}
