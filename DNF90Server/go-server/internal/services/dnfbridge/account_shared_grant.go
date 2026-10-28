package dnfbridge

import (
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// accountSharedFirstGrant 是"先试账号共享仓"的结果。
type accountSharedFirstGrant struct {
	// Slot 是该道具的账号共享固定格（354..365）；不是账号共享道具时为 0。
	// 即使本次一个都没放进去也照常返回。
	Slot int16
	// Placed 是本次真正并进账号仓的数量。
	Placed int64
	// Remainder 是账号仓没能装下、需要调用方按自己的容器规则溢出的数量。
	Remainder uint32
	// Changed 表示账号记录被改动了（调用方据此决定要不要提交账号记录）。
	Changed bool
}

// grantCurrentAccountSharedFirst 是**所有获取途径的统一入口**：先把这个道具并进它在
// 账号共享仓库里的固定格，再把没能放进去的余量交回调用方，由调用方走自己原有的
// 角色侧发放（背包 / 勋章栏 list 38 / 复活币钱包 / 邮件…）。
//
// # 为什么要有它
//
// 口径只有一条：六个晶块(354..359)与六个灵魂(360..365)在同一个账号里只有那一个家。
// 可这套判定过去在 dnfbridge 里有 8 处各写一遍、另有 30 多处根本没写 ——
// 2026-09-21 的事故就是这么来的：NPC 商店**金币购买**那条自己写了一遍落点判定而漏了
// 固定槽这一支，20 个无色落进角色背包 0:154，同一条 list-0 报文里于是出现两行同 itemID，
// 客户端把背包那行显示成了仓库的数量。
//
// 新增功能只要调这一个函数：落点规则、容器额度规则、"固定格被别人占着"的处理，
// 以及新建行的溯源与 0x77 行盖章，都不必再抄一遍。
//
// # 与 dnfrepo.GrantAccountSharedWarehouseStack 的分工
//
// 那边是"并进固定格"的纯逻辑（任何包都能用，含额度与外来占用者判定）；
// 这一层补上 bridge 专属的两件事：给入库后的行盖 0x77 raw entry（行构造器只在这里），
// 以及按 source 写溯源 Extra。
//
// # 调用方必须做的一件事
//
// **Remainder > 0 时必须把余量发出去**（溢出到背包或邮件）。原语只负责不吞掉它，
// 不负责替调用方决定送到哪 —— 少发一次就是玩家丢件。
//
// 该道具没有固定格（元素结晶、普通材料…）时返回 {Changed:false, Remainder:count}，
// 调用方照常走原有发放路径，行为与改造前逐字一致。
func grantCurrentAccountSharedFirst(
	account *dnfrepo.AccountInventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	source string,
) accountSharedFirstGrant {
	if account == nil || count == 0 {
		return accountSharedFirstGrant{Remainder: count}
	}
	grant := dnfrepo.GrantAccountSharedWarehouseStack(
		account,
		int64(definition.ItemID),
		definition.StackLimit,
		int64(count),
		dnfrepo.AccountSharedGrantMeta{
			Source:        source,
			ItemKind:      string(definition.Kind),
			PVFPath:       definition.PVFPath,
			StackableType: definition.StackableType,
		},
	)
	if !grant.Changed {
		return accountSharedFirstGrant{Slot: grant.Slot, Remainder: count}
	}
	// 0x77 行由 bridge 这一侧盖章：原语刻意不碰 RawEntry，因为它不认识线格式。
	key := dnfrepo.AccountSharedInventorySlotKey(grant.Slot)
	stack := account.Slots[key]
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, grant.Slot, stack)
	stack.RawEntry = append([]byte(nil), entry.data[:]...)
	account.Slots[key] = stack

	remainder := uint32(0)
	if grant.Remainder > 0 {
		remainder = uint32(grant.Remainder)
	}
	return accountSharedFirstGrant{
		Slot:      grant.Slot,
		Placed:    grant.Placed,
		Remainder: remainder,
		Changed:   true,
	}
}
