package repository

// ─────────────────────────────────────────────────────────────────────────────
// 账号共享仓库（晶体仓库 354..359 + 灵魂仓库 360..365）的**唯一权威表**
//
// 这 12 个道具在自己账号里只有一个家：list 0 的槽 354..365，由账号记录持有，
// 客户端照旧按普通 list-0 格子寻址（见 account_inventory_repository.go 的
// IsAccountSharedInventorySlot / MergeAccountSharedInventory）。
//
// 为什么集中到这里（2026-09-21 事故复盘）：
// 同一张表一度在三个互不相干的包里各写了一份 ——
//   internal/modules/dnf/inventory/owner.go            upgradeAccountSharedMaterialSlot
//   internal/services/dnfbridge/disjoint_compound_projection.go  currentDisjointWarehouseFixedSlots
//   internal/services/dnfbridge/equipment_regeneration_pvf.go    equipmentRegenerationAccountSharedItem
// 而"获取时查不查这张表"又是逐个功能各写一遍（已打补丁 8 处、未打补丁 30 余处），
// 于是 NPC 商店的**金币购买**那条漏了：1111 买了 20 个无色小晶块落在角色背包 0:154，
// 同时账号槽 0:358 另有 43890 个 —— 同一条 list-0 报文里出现两行同 itemID，
// 客户端把背包那一行显示成了仓库的数量，小退才恢复。
//
// 三处副本现已全部改为调用本表。今后新增道具或调整槽位**只改这里**。
//
// 运营口径（用户 2026-09-21 明确）：
//   * 获取（购买/掉落/奖励/兑换/邮件/交易…）必须落进 354..365，不允许落角色背包；
//   * 玩家手动把它们存进个人仓库(list 2)或账号金库(list 12)是**允许**的，保留该逻辑；
//   * 从这些仓库**取回**时仍要回到 354..365，不允许落进背包。
// ─────────────────────────────────────────────────────────────────────────────

// 六个晶块。ID 顺序对应 90 客户端晶体仓库的固定格序（354..359）。
const (
	AccountSharedCubeBlackItemID int64 = 3033 // 黑色小晶块 → 354
	AccountSharedCubeWhiteItemID int64 = 3034 // 白色小晶块 → 355
	AccountSharedCubeRedItemID   int64 = 3035 // 红色小晶块 → 356
	AccountSharedCubeBlueItemID  int64 = 3036 // 蓝色小晶块 → 357
	AccountSharedCubeClearItemID int64 = 3037 // 无色小晶块 → 358
	AccountSharedCubeGoldItemID  int64 = 3262 // 金色小晶块 → 359
)

// 六个灵魂。ID 顺序对应灵魂仓库的固定格序（360..365）。
const (
	AccountSharedSoulCommonItemID    int64 = 10100115 // 普通灵魂 → 360
	AccountSharedSoulAdvancedItemID  int64 = 10100116 // 高级灵魂 → 361
	AccountSharedSoulRareItemID      int64 = 10099773 // 稀有灵魂 → 362
	AccountSharedSoulUniqueItemID    int64 = 10099774 // 独有灵魂 → 363
	AccountSharedSoulLegendaryItemID int64 = 10099775 // 传说灵魂 → 364
	AccountSharedSoulEpicItemID      int64 = 10158124 // 史诗灵魂 → 365
)

// accountSharedWarehouseFixedSlots 是权威映射：一个道具 ID 对应唯一一个账号共享槽。
// 该映射的槽位序是 90 客户端布局，2026-07-15 实测并经运营确认。
var accountSharedWarehouseFixedSlots = map[int64]int16{
	AccountSharedCubeBlackItemID: CrystalWarehouseFirstSlot + 0,
	AccountSharedCubeWhiteItemID: CrystalWarehouseFirstSlot + 1,
	AccountSharedCubeRedItemID:   CrystalWarehouseFirstSlot + 2,
	AccountSharedCubeBlueItemID:  CrystalWarehouseFirstSlot + 3,
	AccountSharedCubeClearItemID: CrystalWarehouseFirstSlot + 4,
	AccountSharedCubeGoldItemID:  CrystalWarehouseFirstSlot + 5,

	AccountSharedSoulCommonItemID:    SoulWarehouseFirstSlot + 0,
	AccountSharedSoulAdvancedItemID:  SoulWarehouseFirstSlot + 1,
	AccountSharedSoulRareItemID:      SoulWarehouseFirstSlot + 2,
	AccountSharedSoulUniqueItemID:    SoulWarehouseFirstSlot + 3,
	AccountSharedSoulLegendaryItemID: SoulWarehouseFirstSlot + 4,
	AccountSharedSoulEpicItemID:      SoulWarehouseFirstSlot + 5,
}

// AccountSharedWarehouseFixedSlots 返回权威表的副本，供需要整体遍历的调用方使用
// （例如 bridge 侧构造自己的 uint32 视图）。返回副本，避免调用方改写权威表。
func AccountSharedWarehouseFixedSlots() map[int64]int16 {
	out := make(map[int64]int16, len(accountSharedWarehouseFixedSlots))
	for itemID, slot := range accountSharedWarehouseFixedSlots {
		out[itemID] = slot
	}
	return out
}

// AccountSharedWarehouseSlotForItem 给出该道具在账号共享仓库里的唯一固定槽。
// 非成员（含 <=0、超出物品 ID 位宽的入参）返回 (0, false)，不 panic、不猜。
func AccountSharedWarehouseSlotForItem(itemID int64) (int16, bool) {
	slot, ok := accountSharedWarehouseFixedSlots[itemID]
	return slot, ok
}

// IsAccountSharedWarehouseItem 判断该道具是否属于账号共享仓库（六个晶块 + 六个灵魂）。
func IsAccountSharedWarehouseItem(itemID int64) bool {
	_, ok := accountSharedWarehouseFixedSlots[itemID]
	return ok
}
