package dnfbridge

import (
	"os"
	"testing"

	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealPVFHighLevelRarityPoolsAreNotEmpty 是通用装备池重建那次修复的回归钉。
//
// 症状：Lv90 深渊 / 时空裂缝 / 魔界裂缝 每局约三件白装、一件紫装都没有。而白装在
// etc/itemdropinfo_monster_hell.etc 的 [basis of rarity dicision] 里权重是 0，本来
// 不可能被摇出来 —— 它只能来自"请求的稀有度没有候选、降级成 rarity 0"。
//
// 根因是池子按 Etc/ItemDictionary*.etc 的 column1 建稀有度键：column1 是同一稀有度
// 的别名编码（regional 表用 9 表示稀有、5/10 表示神器、7/8 表示史诗/传说），代码只
// 接受 0..6，于是高等级稀有与神器整档没有候选。现在池子直接来自 .equ 的
// ([rarity], [minimum level]) 与 [creation rate]。
//
// 神器这一档尤其要盯住：它在稀有度表里权重是 0，但池子必须先有货，将来给权重时才会
// 真的掉落，而不是又一次降级成白装。
func TestRealPVFHighLevelRarityPoolsAreNotEmpty(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the high level rarity pools")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := dnfhellparty.LoadDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := items.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	audit := items.GenericPoolAudit()
	t.Logf("equipment pool scanned=%d buckets=%d items=%d fallbacks=%d by_rarity=%v lowest_level=%v highest_level=%v "+
		"rarity_missing=%d creation_rate_absent=%d level_out_of_range=%d excluded_type=%d read_failed=%d",
		audit.Scanned, audit.Buckets, audit.Items, audit.Fallbacks, audit.ByRarity, audit.LowestLevel, audit.HighestLevel,
		audit.RarityMissing, audit.CreationRateAbsent, audit.LevelOutOfRange, audit.ExcludedEquipmentType, audit.ReadFailed)
	if audit.Scanned < 1000 || audit.Items < 1000 {
		t.Fatalf("equipment pool looks unbuilt: %+v", audit)
	}
	for _, basisLevel := range []int{70, 86, 90} {
		window, found := rules.GradeWindow(basisLevel)
		if !found {
			t.Fatalf("basis=%d has no [item drop ref table] row", basisLevel)
		}
		counts := make([]int, 7)
		for rarity := range counts {
			counts[rarity] = len(items.genericEquipmentCandidates(basisLevel, window.Down, window.Up, int64(rarity)))
		}
		t.Logf("basis=%d window=%+v candidates_by_rarity=%v", basisLevel, window, counts)
		if counts[1] == 0 {
			t.Fatalf("basis=%d rarity 1 pool is empty: %v", basisLevel, counts)
		}
		if basisLevel < 86 {
			continue
		}
		if counts[2] == 0 {
			t.Fatalf("basis=%d rarity 2 pool is empty — 紫装会被静默降级成白装: %v", basisLevel, counts)
		}
		if counts[3] == 0 {
			t.Fatalf("basis=%d rarity 3 pool is empty — 神器拿到权重也只会降级成白装: %v", basisLevel, counts)
		}
	}
}
