package repository

import "strconv"

// ─────────────────────────────────────────────────────────────────────────────
// 账号共享仓库的唯一入库原语
//
// 「获取途径必须把六个晶块 / 六个灵魂送进 354..365」这件事，过去在 dnfbridge 里
// 有 8 处各写一遍、另有 30 多处根本没写（2026-09-21 的事故就是这么来的：NPC 商店
// 金币购买漏查固定槽表，20 个无色落进角色背包 0:154）。
//
// 本文件提供**一个**原语，任何包（dnfbridge / inventory / mail / packageitem …）
// 都可以调用它把数量并进账号共享仓的固定格。把数量并进去、以及"装不下/被别人占着"
// 的判定都只在这里实现一次，新增功能不再需要抄一遍落点逻辑。
//
// 与 dnfbridge 侧旧实现的等价性：旧 grantCurrentDisjointWarehouseStack 的行为被逐条
// 搬到这里（外来占用者绝不驱赶、额度按容器内同 id 总量算、超限部分原样退回），
// 唯一的差别是**行内 raw entry 不再由这里盖章** —— 0x77 行构造器在 bridge / 各模块
// 自己手里，由调用方在拿到结果后补盖（最终落库结果完全一致）。
// ─────────────────────────────────────────────────────────────────────────────

// AccountSharedGrantMeta 描述新建一行账号共享道具时要写的溯源信息。
// 值一律原样写进 ItemStack.Extra；Source / ItemKind / PVFPath 三个键**始终**会被写入
// （与旧实现一致，即使取值为空），便于按来源排查"这件东西是谁放进来的"。
type AccountSharedGrantMeta struct {
	Source        string // 来源标记，例如 "dungeon_pvf_disjoint" / "npc_shop" / "mail"
	ItemKind      string // "stackable" / "equipment" …
	PVFPath       string
	StackableType string // 非空才写
}

// AccountSharedGrant 是一次账号共享仓入库的结果。
type AccountSharedGrant struct {
	// Slot 是该道具的固定格（354..365）。即使本次一个都没放进去也照常返回，
	// 调用方据此把余量溢出到角色容器。
	Slot int16
	// Changed 表示账号记录被改动了（调用方据此决定要不要提交账号记录）。
	Changed bool
	// Placed 是本次真正并进账号仓的数量。
	Placed int64
	// Remainder 是账号仓装不下、需要调用方溢出到角色容器的数量。
	// 未放进任何数量时 Remainder == 传进来的 count。
	Remainder int64
	// CountAfter 是该行入库之后的总量（Changed 为 false 时无意义）。
	CountAfter int64
}

// GrantAccountSharedWarehouseStack 把 count 个 itemID 并进它在账号共享仓库里的固定格。
//
// 规则（与旧的 bridge 私有实现逐条一致）：
//   - 固定格被**别的**道具占着（历史脏数据）时绝不驱赶、绝不合并，整笔退回；
//   - stackLimit > 0 时额度按"账号仓这一个容器里该 id 的总量"算（容器里已有的
//     其它同 id 行，包括历史拆行，都要计入），不是一个固定格的余量；
//     stackLimit <= 0（PVF 未声明 [stack limit]，本 PVF 的 12 个道具都是这样）
//     表示不受限；
//   - 放不下的部分原样退回，绝不吞掉，由调用方溢出到角色背包或邮件。
//
// itemID 不是账号共享道具时返回 {Changed:false, Remainder:count}，不 panic、不猜。
func GrantAccountSharedWarehouseStack(
	account *AccountInventoryRecord,
	itemID int64,
	stackLimit int64,
	count int64,
	meta AccountSharedGrantMeta,
) AccountSharedGrant {
	slot, member := AccountSharedWarehouseSlotForItem(itemID)
	if !member {
		return AccountSharedGrant{Remainder: count}
	}
	result := AccountSharedGrant{Slot: slot, Remainder: count}
	if account == nil || count <= 0 {
		return result
	}
	if account.Slots == nil {
		account.Slots = make(map[string]ItemStack)
	}
	key := AccountSharedInventorySlotKey(slot)
	stack, exists := account.Slots[key]
	if exists && stack.ItemID > 0 && stack.ItemID != itemID {
		// 固定格被别人占着：绝不动它，整笔退回。
		return result
	}
	room := count
	if stackLimit > 0 {
		space := ContainerItemSpace(account.Slots, MainInventoryListType, itemID, stackLimit)
		if space < room {
			room = space
		}
	}
	if room <= 0 {
		return result
	}
	if !exists || stack.ItemID <= 0 {
		stack = ItemStack{
			ItemID: itemID,
			Extra: map[string]string{
				"source":    meta.Source,
				"item_kind": meta.ItemKind,
				"pvf_path":  meta.PVFPath,
			},
		}
		if meta.StackableType != "" {
			stack.Extra["stackable_type"] = meta.StackableType
		}
		if stackLimit > 0 {
			stack.Extra["stack_limit"] = strconv.FormatInt(stackLimit, 10)
		}
	}
	stack.Count += room
	account.Slots[key] = stack
	result.Changed = true
	result.Placed = room
	result.Remainder = count - room
	result.CountAfter = stack.Count
	return result
}
