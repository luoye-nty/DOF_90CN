package packageitem

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 现场 2026-09-27（线上 203.0.113.11）：
//
//	dnf_inventory_items 里 equipment/character/thief/weapon/twinsword/109010047.equ
//	（`传承 : 旭光精灵的碧岩双剑`）被写成了一行 count=2/4/5/6/8 的装备行，
//	extra 全部是魔盒发放写的那几个键（item_kind/pvf_path/quality_seed=999999998/
//	durability/max_durability/equipment_type/seal_flag），没有 source。
//	该武器 [price] 100800 / [value] 80640，NPC 出售每次付 16128 金币，而 0:37
//	这一格在 packet_log 里被连续卖了 29 次（game-000527，13:27:10~13:27:25）——
//	客户端装备行不显示数量，每卖一次服务端只扣 1（npc_shop_projection.go 里
//	equipment 恒 applied=1），行还在原处，玩家看到的就是“卖掉又回到背包”。
//
// 本用例钉住根因：魔盒发奖的“并入已有行”分支必须和新建行分支一样排除装备。
// 装备每件一行，绝不允许并进同 id 的已有行。
func TestMagicBoxEquipmentRewardNeverMergesIntoExistingEquipmentRow(t *testing.T) {
	const weapon = int64(109010047)
	items := map[string]dnfrepo.ItemStack{
		// 快捷栏 3..8 上的同一件装备（fill 分支优先扫这一段）。
		"0:7": {ItemID: weapon, Count: 1, Extra: map[string]string{"item_kind": "equipment"}},
	}
	reward := alignedcmd.MagicBoxRewardItem{
		ItemID:         weapon,
		Kind:           "equipment",
		TargetListType: listTypeMain,
		EquipmentType:  "[weapon]",
		// 真实武器 .equ 没有 [stack limit]，所以这里是 0（不限量）——
		// 旧实现的唯一护栏是 reward.StackLimit == 1，对 0 不生效。
		StackLimit: 0,
		SlotStart:  9,
		SlotEnd:    48,
		PVFPath:    "equipment/character/thief/weapon/twinsword/109010047.equ",
	}

	remaining, _, err := grantMagicBoxReward(items, reward, 3)
	if err != nil {
		t.Fatalf("grantMagicBoxReward error = %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0 (3 件都必须落地)", remaining)
	}
	if got := items["0:7"].Count; got != 1 {
		t.Fatalf("快捷栏已有装备行被并成 %d 件；装备必须每件一行（现场 bug：客户端不显示装备数量，卖一件行还在，等同“无限卖钱”）", got)
	}
	// 3 件装备 = 3 行：原有 1 行 + 新开的 3 行里的… 实际是 3 件各自一行。
	rows := 0
	for key, stack := range items {
		if stack.ItemID != weapon {
			t.Fatalf("意外道具 %s=%+v", key, stack)
		}
		if stack.Count != 1 {
			t.Fatalf("%s count=%d，装备行必须恒为 1", key, stack.Count)
		}
		rows++
	}
	if rows != 4 {
		t.Fatalf("装备行数 = %d, want 4（原有 1 + 新发 3）", rows)
	}
}

// 对照组：可叠加道具沿用旧行为（并进已有行），证明修复不会误伤 stackable。
func TestMagicBoxStackableRewardStillTopsUpExistingRow(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{
		"0:7": {ItemID: 2600014, Count: 5, Extra: map[string]string{"item_kind": "stackable"}},
	}
	reward := alignedcmd.MagicBoxRewardItem{
		ItemID:         2600014,
		Kind:           "stackable",
		TargetListType: listTypeMain,
		StackLimit:     0,
		SlotStart:      121,
		SlotEnd:        176,
		PVFPath:        "stackable/consumption/test.stk",
	}
	remaining, _, err := grantMagicBoxReward(items, reward, 3)
	if err != nil {
		t.Fatalf("grantMagicBoxReward error = %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
	if got := items["0:7"].Count; got != 8 {
		t.Fatalf("可叠加道具就没有并进已有行：count=%d, want 8", got)
	}
}
