package inventory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcargo "longheng.io/server/internal/modules/dnf/cargo"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 扩容券（物品栏 / 金库 / 帐号金库）的服务端接线。
//
// 现场 2026-10-03（服主报，角色 测试x = char 11）：
//
//	490701690 物品栏扩展券       [action type] `[upgrade inventory]`        背包槽 66
//	490701691 金库扩展券 (1阶段)  [action type] `[upgrade cargo]`            背包槽 68
//	490701692 帐号金库升级工具.   [action type] `[upgrade account cargo]`    背包槽 67
//
// 三件"使用后没效果"。加密包日志（conn game-000124，2026-10-03）证明客户端**有发包**：
//
//	09:56:57  RECV type=430 ENUM_CMDPACKET_UPGRADE_INVENTORY body=`4200`  ×11
//	09:58:42  RECV type=408 ENUM_CMDPACKET_UPGRADE_CARGO     body=`4400`
//	09:58:44  RECV type=515 ENUM_CMDPACKET_USE_STACKABLE_ACTION
//	          body=`4300000000000092…`（槽 0x43=67 + action index 0x92=146）
//	          服务端当场的真实回包是 op515 错误 17：
//	          "use stackable action index 146 unsupported"
//
// 前两个 opcode 在服务端**完全没有路由**（不在 aligned 表里、没有 handler，
// DispatchUpper 对未注册 opcode 直接 return false ⇒ 静默），第三个被 action 白名单拒绝。
//
// 官方服务端（同目录 df_game_r(1)，带 .symtab）给出了三种券的完整业务判据，
// 见 _investigation_顶号_official/upgrade_dis.txt：
//
//	Dispatcher_UpgradeInventory::dispatch_sig  读 PacketBuf::get_short 槽位 →
//	  校验槽上物品数量>0 → 校验物品（官方硬编码 2660298/8251 两个 id）→
//	  get_inventory_capacity()>15 则拒绝(0x13) → delete_item(1,slot,1,reason3) →
//	  发 class0/op14 空行清槽 → capacity+=8 → op66{u16 11,u16 capacity}
//	Dispatcher_UpgradeCargo::dispatch_sig      同样读槽位 → 数量>0 →
//	  verifyStackableItemActionPlace(StackableAction::Type 0x2b, itemID)（**PVF 判定**）→
//	  CCargo::isUpgradable(capacity+16) → delete_item → op14 空行 →
//	  CCargo::UpgradeCargo(capacity+16) → op66{u16 1,u16 capacity}
//
// 我们**不照抄官方的硬编码物品 id**：那是官服自己的白名单，本服的道具是自制的
// （490701690/691/692），所以判据取 PVF 自己的 `[action type]` —— 与官方金库那条
// `verifyStackableItemActionPlace` 同一个思路，而且覆盖全库 100 件同类道具
// （40 / 47 / 13 件），换一件券也照样能用。
//
// 容量推进口径（三处都与既有代码同一张表）：
//
//	物品栏 main_list_param16  0..24 步长 8（repository.currentMainInventoryExpansion）
//	  —— 券自己的说明："物品栏的空间增加8格。物品栏最多可以增加到56格。"（32+24）
//	金库   personal_cargo_list_param16  8..200 步长 16（currentPersonalCargoSlotCount）
//	帐号金库 account_cargo_level 走 dnfcargo.NextAccountCargoTier 的 16 档阶梯
//
// 帐号金库这一档**不收**金币/无色小晶块：券本身就是代价（金库窗口那条 op306 才收费）。

const (
	// accountCargoUpgradeActionIndex 是 `[action type] [upgrade account cargo]`
	// 在 op515 里的 action index。现场实测 0x92=146（见文件头日志），与
	// [rent point]=105、[add fatigue]=58、任务完成券=167 同一张编号表。
	accountCargoUpgradeActionIndex uint32 = 146

	// containerUpgradeMainStep / containerUpgradeMaxMainExpansion 见文件头。
	containerUpgradeMainStep         = 8
	containerUpgradeMaxMainExpansion = 24
	// containerUpgradeCargoStep / containerUpgradeMaxCargoSlots 见文件头。
	containerUpgradeCargoStep     = 16
	containerUpgradeMaxCargoSlots = 200
)

var (
	ErrContainerUpgradeContract       = errors.New("inventory container-upgrade contract is invalid")
	ErrContainerUpgradeItemMismatch   = errors.New("container-upgrade voucher's PVF action type does not match the requested container")
	ErrContainerUpgradeMaxStage       = errors.New("container is already at its maximum upgrade stage")
	ErrContainerUpgradeStateMissing   = errors.New("character container state is missing")
	ErrContainerUpgradeAccountMissing = errors.New("account cargo has not been created")
	ErrContainerUpgradeAccountBroken  = errors.New("account cargo metadata is invalid")
)

// UpgradeContainerFromVoucher 用一张扩容券推进一个容器档位。
//
// cmd.SourceSlotIndex 是客户端声明的券槽位（op430/op408 的 i16 body，或 op515 的槽位）；
// 券的身份一律从**持久行 + 运行时 PVF** 反查：请求体里没有 item id，客户端说什么不算数。
// wantKind 由调用方按 opcode（或 action index）给出，PVF 解析出的 kind 必须与它一致，
// 所以"拿金库券去点物品栏"这类错位组合会被当场拒掉。
func (o *Owner) UpgradeContainerFromVoucher(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
	wantKind alignedcmd.ContainerUpgradeKind,
	now time.Time,
) (UseStackableResult, error) {
	if o == nil || o.settlements == nil || o.settings == nil || resolver == nil {
		return UseStackableResult{}, ErrContainerUpgradeContract
	}
	if cmd.SelectedCharacterID == 0 || cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 {
		return UseStackableResult{}, ErrContainerUpgradeContract
	}
	switch wantKind {
	case alignedcmd.ContainerUpgradeInventory, alignedcmd.ContainerUpgradeCargo, alignedcmd.ContainerUpgradeAccountCargo:
	default:
		return UseStackableResult{}, ErrContainerUpgradeContract
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result UseStackableResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Inventory == nil || tx.Character == nil {
			return ErrContainerUpgradeContract
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return ErrCharacterRequired
		}
		if cmd.AccountID != "" && strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID) {
			return fmt.Errorf("%w: character=%s account=%q want=%q",
				ErrContainerUpgradeContract, characterID, character.AccountID, cmd.AccountID)
		}
		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 || stack.ItemID <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d is empty", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		resolution, err := resolver(stack.ItemID)
		if err != nil {
			return fmt.Errorf("%w: item=%d: %v", ErrContainerUpgradeContract, stack.ItemID, err)
		}
		upgrade := resolution.ContainerUpgrade
		if resolution.ItemID != stack.ItemID || upgrade == nil || upgrade.ItemID != stack.ItemID || upgrade.Kind != wantKind {
			return fmt.Errorf("%w: item=%d want=%s resolution=%+v pvf_path=%q",
				ErrContainerUpgradeItemMismatch, stack.ItemID, wantKind, resolution.ContainerUpgrade, resolution.PVFPath)
		}

		before, after := int64(0), int64(0)
		listType := listTypeMain
		cargoPad := false
		switch wantKind {
		case alignedcmd.ContainerUpgradeInventory, alignedcmd.ContainerUpgradeCargo:
			if tx.Settings == nil {
				return ErrContainerUpgradeContract
			}
			record, found, err := tx.Settings.Load(ctx, dnfrepo.CharacterContainerStateScope(characterID))
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: character=%s", ErrContainerUpgradeStateMissing, characterID)
			}
			state, err := dnfrepo.CharacterContainerStateFromSettings(record, characterID)
			if err != nil {
				return err
			}
			record = dnfrepo.CloneSettings(record)
			if record.Values == nil {
				record.Values = make(map[string]string)
			}
			switch wantKind {
			case alignedcmd.ContainerUpgradeInventory:
				before = int64(state.MainSlotCount)
				if before+containerUpgradeMainStep > containerUpgradeMaxMainExpansion {
					return fmt.Errorf("%w: main_list_param16=%d", ErrContainerUpgradeMaxStage, before)
				}
				after = before + containerUpgradeMainStep
				record.Values["main_list_param16"] = strconv.FormatInt(after, 10)
				record.Values["main_inventory_last_upgrade_commodity"] = strconv.FormatInt(stack.ItemID, 10)
				record.Values["main_inventory_last_upgrade_from"] = strconv.FormatInt(before, 10)
				record.Values["main_inventory_last_upgrade_to"] = strconv.FormatInt(after, 10)
				record.Values["source"] = "current_exe_container_upgrade_voucher"
				listType = listTypeMain
			default:
				before = int64(state.PersonalCargoSlotCount)
				if before+containerUpgradeCargoStep > containerUpgradeMaxCargoSlots {
					return fmt.Errorf("%w: personal_cargo_list_param16=%d", ErrContainerUpgradeMaxStage, before)
				}
				after = before + containerUpgradeCargoStep
				record.Values["personal_cargo_list_param16"] = strconv.FormatInt(after, 10)
				record.Values["personal_cargo_last_upgrade_commodity"] = strconv.FormatInt(stack.ItemID, 10)
				record.Values["personal_cargo_last_upgrade_from"] = strconv.FormatInt(before, 10)
				record.Values["personal_cargo_last_upgrade_to"] = strconv.FormatInt(after, 10)
				record.Values["source"] = "current_exe_container_upgrade_voucher"
				listType = listTypePersonalCargo
				cargoPad = true
			}
			record.UpdatedAt = now
			// 落库之前先用仓储自己的校验器审一遍：仓储是这几张合法表的唯一权威
			// （背包 0..24 步长 8、金库 8..200 步长 16），写坏了这里就会先炸。
			if _, err := dnfrepo.CharacterContainerStateFromSettings(record, characterID); err != nil {
				return err
			}
			if err := tx.Settings.Save(ctx, record); err != nil {
				return err
			}
		default:
			if tx.Account == nil {
				return ErrContainerUpgradeContract
			}
			accountID := strings.TrimSpace(character.AccountID)
			if accountID == "" {
				return fmt.Errorf("%w: character=%s", ErrContainerUpgradeContract, characterID)
			}
			account, found, err := tx.Account.Load(ctx, accountID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(account.AccountID) != accountID {
				return fmt.Errorf("%w: account=%q", ErrContainerUpgradeContract, accountID)
			}
			account = dnfrepo.CloneAccount(account)
			if account.Metadata == nil {
				account.Metadata = make(map[string]string, 4)
			}
			raw := strings.TrimSpace(account.Metadata["account_cargo_level"])
			if raw == "" {
				return fmt.Errorf("%w: account=%q", ErrContainerUpgradeAccountMissing, accountID)
			}
			level, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil {
				return fmt.Errorf("%w: account=%q account_cargo_level=%q", ErrContainerUpgradeAccountBroken, accountID, raw)
			}
			if level <= 0 {
				return fmt.Errorf("%w: account=%q level=%d", ErrContainerUpgradeAccountMissing, accountID, level)
			}
			next, ok := dnfcargo.NextAccountCargoTier(level)
			if !ok || next <= level {
				return fmt.Errorf("%w: account_cargo_level=%d", ErrContainerUpgradeMaxStage, level)
			}
			// 券付账：这里刻意**不调** applyCargoUpgradeCost 的那三档收费
			// （1->8 金币、16/24 无色小晶块、其余点券），否则等于让玩家付两次。
			account.Metadata["account_cargo_created"] = "true"
			account.Metadata["account_cargo_level"] = strconv.FormatInt(next, 10)
			if strings.TrimSpace(account.Metadata["account_cargo_gold"]) == "" {
				account.Metadata["account_cargo_gold"] = "0"
			}
			account.UpdatedAt = now
			if err := tx.Account.Save(ctx, account); err != nil {
				return err
			}
			before, after = level, next
			listType = listTypeAccountCargo
			cargoPad = true
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		result = UseStackableResult{
			CharacterID:                   characterID,
			AccountID:                     strings.TrimSpace(character.AccountID),
			ListType:                      cmd.SourceListType,
			SlotIndex:                     cmd.SourceSlotIndex,
			ItemID:                        stack.ItemID,
			RemainingCount:                remaining,
			Changed:                       true,
			PVFPath:                       strings.TrimSpace(resolution.PVFPath),
			StackableType:                 resolution.StackableType,
			ContainerUpgradeApplied:       true,
			ContainerUpgradeKind:          wantKind,
			ContainerUpgradeBefore:        before,
			ContainerUpgradeAfter:         after,
			ContainerUpgradeListType:      listType,
			ContainerUpgradeNeedsCargoPad: cargoPad,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}
