package inventory

import "testing"

// 卡片升到最后一级时结果留在原槽（实测 242 -> 242），行里只有升级字节 03 -> 04
// 变了，item id 和槽位都没动。这个旧客户端对这种"原地改行"的增量 op14 不重绘，
// 背包会一直显示上一级——玩家实测满级后仍显示 3/4，重新选角色才正常。前几级之所以
// 没事，是因为每次都换了槽位，客户端看到的是"删一个 + 加一个"。
//
// 所以升级成功必须补一次 list-0 整表重建。
func TestMonsterCardUpgradeSuccessRequestsAListZeroRebuild(t *testing.T) {
	result := ownerAppliedMonsterCardUpgradeResult(603, Command{Operation: "upgrade_monster_card"},
		MonsterCardUpgradeResult{
			Success:          true,
			UpgradeSucceeded: true,
			TargetItemID:     3681,
			UpgradeCount:     4,
			ResultSlot:       242,
			ChangedSlots:     []int16{235, 242},
		})
	if len(result.ItemListRebuilds) != 1 || result.ItemListRebuilds[0] != listTypeMain {
		t.Fatalf("rebuilds = %v, want the main inventory list", result.ItemListRebuilds)
	}
	// 增量行仍然要发：整表重建是兜底，不是替代。
	if len(result.ItemSlotRefreshes) != 2 {
		t.Fatalf("slot refreshes = %v, want one per changed slot", result.ItemSlotRefreshes)
	}
}

// 掷骰失败时卡片和材料都没动，增量行足够，不该为此重发整张主背包。
func TestMonsterCardUpgradeFailedRollSkipsTheRebuild(t *testing.T) {
	result := ownerAppliedMonsterCardUpgradeResult(603, Command{Operation: "upgrade_monster_card"},
		MonsterCardUpgradeResult{
			Success:          true,
			UpgradeSucceeded: false,
			TargetItemID:     10159184,
			ResultSlot:       233,
			ChangedSlots:     []int16{233},
		})
	if len(result.ItemListRebuilds) != 0 {
		t.Fatalf("rebuilds = %v, want none on a failed roll", result.ItemListRebuilds)
	}
	if len(result.ItemSlotRefreshes) != 1 {
		t.Fatalf("slot refreshes = %v, want the single unchanged slot", result.ItemSlotRefreshes)
	}
}
