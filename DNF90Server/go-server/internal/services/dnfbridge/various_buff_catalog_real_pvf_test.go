package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 真 Script.pvf 验收：两张 buff 表的解析结果必须和 PVF 里写的一模一样。
//
//	DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge -run VariousBuffTables -v
//
// 数值来源（现场 2026-10-03 抄录，可用 cmd/scratch/pvfmember 复核）：
//
//	etc/variousbuffstackablelist.etc  490002458 [probability buff list] 1136 100
//	                                  10008219  [probability buff list] 1090 60 1091 30 1092 10
//	                                  490005358 [probability buff list] 1239 100
//	                                  490005359 [probability buff list] 1240 100
//	etc/variousbufflist.etc           1136 `epic_rarity_item_drop_rate` param 100  icon 183
//	                                  1090 param 100 / 1091 param 150 / 1092 param 200
//	                                  1239 param 10  / 1240 param 5
func TestCurrentVariousBuffTablesRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the various-buff tables")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	tables, err := loadCurrentVariousBuffTables(archive)
	if err != nil {
		t.Fatalf("load various-buff tables: %v", err)
	}
	if len(tables.ByItem) == 0 || len(tables.ByBuff) == 0 {
		t.Fatalf("tables are empty: items=%d buffs=%d", len(tables.ByItem), len(tables.ByBuff))
	}

	for _, tc := range []struct {
		itemID     int64
		wantBuff   int64
		wantWeight int64
		wantBonus  int64
		wantIcon   int64
	}{
		{itemID: 490002458, wantBuff: 1136, wantWeight: 100, wantBonus: 100, wantIcon: 183},
		{itemID: 490003686, wantBuff: 1136, wantWeight: 100, wantBonus: 100, wantIcon: 183},
		{itemID: 490006963, wantBuff: 1136, wantWeight: 100, wantBonus: 100, wantIcon: 183},
		{itemID: 490005358, wantBuff: 1239, wantWeight: 100, wantBonus: 10, wantIcon: 183},
		{itemID: 490005359, wantBuff: 1240, wantWeight: 100, wantBonus: 5, wantIcon: 183},
	} {
		rolls := tables.ByItem[uint32(tc.itemID)]
		if len(rolls) != 1 || rolls[0].BuffID != tc.wantBuff || rolls[0].Weight != tc.wantWeight {
			t.Fatalf("item %d rolls=%+v want buff=%d weight=%d", tc.itemID, rolls, tc.wantBuff, tc.wantWeight)
		}
		effect, found := tables.ByBuff[tc.wantBuff]
		if !found {
			t.Fatalf("buff %d is missing", tc.wantBuff)
		}
		bonus, ok := effect.dropRateBonusPercent()
		if !ok || bonus != tc.wantBonus {
			t.Fatalf("buff %d bonus=%d ok=%t want=%d (%+v)", tc.wantBuff, bonus, ok, tc.wantBonus, effect)
		}
		if effect.IconIndex != tc.wantIcon {
			t.Fatalf("buff %d icon=%d want=%d", tc.wantBuff, effect.IconIndex, tc.wantIcon)
		}
		if effect.SystemGroup != "random_box_buff" {
			t.Fatalf("buff %d system group=%q", tc.wantBuff, effect.SystemGroup)
		}
	}

	// 赛丽亚的幸运掉落药剂：三档 60/30/10，数值 100/150/200 —— 这正是"随机一档"，
	// 所以只能服务端掷点，客户端自己算不出来。
	rolls := tables.ByItem[10008219]
	wantRolls := []currentVariousBuffRoll{
		{BuffID: 1090, Weight: 60},
		{BuffID: 1091, Weight: 30},
		{BuffID: 1092, Weight: 10},
	}
	if len(rolls) != len(wantRolls) {
		t.Fatalf("10008219 rolls=%+v want=%+v", rolls, wantRolls)
	}
	for i, want := range wantRolls {
		if rolls[i] != want {
			t.Fatalf("10008219 roll[%d]=%+v want=%+v", i, rolls[i], want)
		}
	}
	for buffID, wantBonus := range map[int64]int64{1090: 100, 1091: 150, 1092: 200} {
		bonus, ok := tables.ByBuff[buffID].dropRateBonusPercent()
		if !ok || bonus != wantBonus {
			t.Fatalf("buff %d bonus=%d ok=%t want=%d", buffID, bonus, ok, wantBonus)
		}
	}

	// 史诗Buff药剂一族必须都能从表里查到掉率候选。
	for _, itemID := range []int64{490002458, 490003686, 490004407, 490004506, 490005308,
		490005491, 490005778, 490006699, 490007961, 490008359, 490008470, 490008903,
		490701639, 490005358, 490005359, 10008219} {
		if got := tables.epicDropRateRollsForItem(itemID); len(got) == 0 {
			t.Fatalf("item %d has no epic drop-rate roll in the table", itemID)
		}
	}
	// 490000413（2016 活动券）表里没有条目 —— 它必须走 `[explain]` 文字回退，
	// 而不是被当成"没有效果"。
	if got := tables.epicDropRateRollsForItem(490000413); len(got) != 0 {
		t.Fatalf("490000413 unexpectedly present in the table: %+v", got)
	}
}
