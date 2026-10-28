package inventory

import (
	"context"
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
)

// 本文件是 op515（use stackable action）的统一入口，也是「每天可用次数」
// （PVF `[total usable count]`）在 action 家族里的唯一挂载点。
//
// 为什么要在**分派之前**预留：上限是道具自己的 PVF 声明，与它走哪个 action
// 无关。挂在各个 action 分支里意味着每加一个 action 就要记得再挂一次，而实测
// 真实 Script.pvf 里 630 件声明该字段的道具中，那些走 action 的（入场券、租赁
// 点、伤害字体、任务完成券）此前一件都没被计过。放在这里，分支只负责动作本身。
//
// 唯一的例外是 action 58（疲劳药）：它的次数必须和疲劳恢复落在同一个事务里，
// 所以由 fatigue_potion.go 自己扣（同一份 itemdailylimit 实现），这里为它让路，
// 否则同一件事会被计两次。

// applyUseStackableAction 跑 op515 的 5 个 action 分支。返回的 bool 表示动作
// 是否真的落地：false 时调用方必须把已经预留的每日次数还回去。
func (o *Owner) applyUseStackableAction(
	ctx context.Context,
	cmd Command,
	req alignedcmd.Request,
) (alignedcmd.Result, bool) {
	switch cmd.ActionIndex {
	case dungeonAdmissionTicketActionIndex:
		result, err := o.UseDungeonAdmissionTicketAction(
			ctx,
			cmd,
			req.StackableUseResolver,
			req.InDungeon,
		)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedDungeonAdmissionTicketActionResult(req.Opcode, cmd, result), true
	case fatiguePotionActionIndex:
		result, err := o.UseFatiguePotionAction(
			ctx,
			cmd,
			req.StackableUseResolver,
			req.FatiguePotionNow,
			req.InDungeon,
		)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedFatiguePotionActionResult(req.Opcode, cmd, result), true
	case damageFontActionIndex:
		result, err := o.UnlockDamageFont(ctx, cmd, req.DamageFontResolver, req.DamageFontNow)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedDamageFontUnlockResult(req.Opcode, cmd, result), true
	case rentalPointActionIndex:
		result, err := o.UseRentalPointAction(ctx, cmd, req.StackableUseResolver, req.RentalPointNow)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedRentalPointActionResult(req.Opcode, cmd, result), true
	case questCompletionTicketActionIndex:
		result, err := o.UseQuestCompletionTicketAction(
			ctx,
			cmd,
			req.StackableUseResolver,
			req.QuestCompletionTicketPlanner,
			req.InDungeon,
		)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedQuestCompletionTicketActionResult(req.Opcode, cmd, result), true
	case accountCargoUpgradeActionIndex:
		// `[action type] [upgrade account cargo]`（帐号金库升级工具）。客户端没有给
		// 它单独 opcode，走的是这条共用 action 通道；现场日志里服务端当时回的是
		// "use stackable action index 146 unsupported"。
		result, err := o.UpgradeContainerFromVoucher(
			ctx,
			cmd,
			req.StackableUseResolver,
			alignedcmd.ContainerUpgradeAccountCargo,
			time.Time{},
		)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedContainerUpgradeResult(req.Opcode, cmd, result), true
	case favorLetterActionIndex:
		// `[action type] [use favor point]`（充满爱慕的信等）：客户端只在这一步登记
		// "要用这张券"，选中 NPC 的确认是随后的 op812 —— 扣道具与加好感度都在那里落地
		// （见 dnfbridge/returnee_npc_gift.go、onlineevent/npc_favor_letter.go）。
		result, err := o.ArmFavorLetter(ctx, cmd)
		if err != nil {
			return ownerRejectedUseStackableActionResult(req.Opcode, cmd, err), false
		}
		return ownerAppliedFavorLetterArmResult(req.Opcode, cmd, result), true
	default:
		return ownerRejectedUseStackableActionResult(
			req.Opcode, cmd, fmt.Errorf("use stackable action index %d unsupported", cmd.ActionIndex)), false
	}
}

// reserveUseStackableActionDailyUse 在动作落地之前，按道具**自己的** PVF
// `[total usable count]` 预留一次每日次数。
//
// 它是纯增量的：只要道具没声明上限（或者解析不出它的身份），返回的就是空预留，
// 整条 op515 路径的行为与挂载之前逐字节相同 —— 只有声明了上限的道具才会多出
// 这一步判定与 op541 投影。已到上限时返回可识别的拒绝，动作不会执行。
func (o *Owner) reserveUseStackableActionDailyUse(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
	characterID string,
) (DailyItemUseReservation, error) {
	// action 58 的计数在 UseFatiguePotionAction 的事务里，见文件头注释。
	if cmd.ActionIndex == fatiguePotionActionIndex || o == nil || o.repo == nil || resolver == nil {
		return DailyItemUseReservation{}, nil
	}
	if cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 {
		return DailyItemUseReservation{}, nil
	}
	// 道具身份要从持久行上取（op515 请求体里没有 item id），但**只用于**取上限：
	// 读不到就让 action 分支去报它原本的错误，这里绝不抢先改变失败原因。
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil || !found {
		return DailyItemUseReservation{}, nil
	}
	stack, found := record.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.ItemID <= 0 {
		return DailyItemUseReservation{}, nil
	}
	resolution, err := resolver(stack.ItemID)
	if err != nil || resolution.ItemID != stack.ItemID {
		return DailyItemUseReservation{}, nil
	}
	if !dnfitemdailylimit.DeclaresLimit(resolution.DailyUseLimit) {
		return DailyItemUseReservation{}, nil
	}
	// 到这一步才可能出现"已达上限"：这个错误必须上报，动作不得执行。
	return o.ReserveDailyItemUse(ctx, characterID, stack.ItemID, resolution.DailyUseLimit, time.Time{})
}

// dailyUseStatesFromReservation 把一次预留投影成客户端 op541 的状态。
// 字节是**已用**次数（客户端按「上限 − 字节」渲染剩余），与 op44 那条路的
// dailyUseStates 同一套算术。空预留（道具没有上限）不产生任何包。
func dailyUseStatesFromReservation(reservation DailyItemUseReservation) []alignedcmd.DailyUseState {
	if !reservation.Reserved || reservation.ItemID <= 0 || reservation.ItemID > 0xFFFFFFFF {
		return nil
	}
	used := reservation.Used
	if used < 0 {
		used = 0
	}
	if used > 0xFF {
		used = 0xFF
	}
	return []alignedcmd.DailyUseState{{ItemID: uint32(reservation.ItemID), Used: byte(used)}}
}
