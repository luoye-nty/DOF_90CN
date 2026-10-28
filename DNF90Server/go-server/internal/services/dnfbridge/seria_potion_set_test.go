package dnfbridge

import (
	"testing"
	"time"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 赛丽亚那五件是同一批（10008219 掉落率 / 10008220 强化成功率 / 10008221 城镇移速 /
// 10008222 通关经验 / 10008223 进图属性）。服主 2026-10-03：
// "10008222，10008223，10008219，10008220，10008221 这些道具 也一并做了吧 有几个好像已经做完了"。
//
// 前两件早已完成；这里钉住后三件的分类：
//
//	10008221 → 客户端自施放 family `village_move_speed`（15/30/50）
//	10008222 → 服务端效果 `dungeon_clear_exp_rate`（50/100/200）
//	10008223 → 客户端自施放 family `move_speed`（进图属性）
func TestAlignedStackableUseResolverCoversTheSeriaPotionSet(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":               "",
		"equipment/equipment.lst":           "",
		"stackable/stackable.lst":           "10008221 `cash/town_speed.stk`\n10008222 `cash/clear_exp.stk`\n10008223 `cash/dungeon_status.stk`\n",
		"stackable/cash/town_speed.stk":     "[name] `seria town`\n[explain] `使用后， 可以获得城镇移动速度提升Buff， 效果持续30分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 1800000 `myself`\n",
		"stackable/cash/clear_exp.stk":      "[name] `seria exp`\n[explain] `使用后， 可以获得地下城通关经验值增加Buff， 效果持续30分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 1800000 `myself`\n",
		"stackable/cash/dungeon_status.stk": "[name] `seria status`\n[explain] `使用后， 角色进入地下城后力量、 智力、 体力、 精神及攻速、 移动速度、 施放速度等属性增加。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 1800000 `myself`\n",
		"etc/variousbuffstackablelist.etc": "[index]\n10008221 \n[probability buff list]\n1100 60 1101 30 1102 10 \n[/index]\n" +
			"[index]\n10008222 \n[probability buff list]\n1103 30 1104 50 1105 20 \n[/index]\n" +
			"[index]\n10008223 \n[probability buff list]\n1106 30 1107 40 1108 30 \n[/index]\n",
		"etc/variousbufflist.etc": seriaBuffDefinition(1100, "village_move_speed", 15) +
			seriaBuffDefinition(1101, "village_move_speed", 30) +
			seriaBuffDefinition(1102, "village_move_speed", 50) +
			seriaBuffDefinition(1103, "dungeon_clear_exp_rate", 50) +
			seriaBuffDefinition(1104, "dungeon_clear_exp_rate", 100) +
			seriaBuffDefinition(1105, "dungeon_clear_exp_rate", 200) +
			seriaBuffDefinition(1106, "move_speed", 50) +
			seriaBuffDefinition(1107, "move_speed", 100) +
			seriaBuffDefinition(1108, "move_speed", 200),
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil || resolver == nil {
		t.Fatalf("op44 resolver=%v err=%v", resolver, err)
	}

	// 城镇移速 / 进图属性：客户端自施放那一族（服务端只注册 buff）。
	for _, tc := range []struct {
		itemID   int64
		family   string
		wantRoll int
	}{
		{itemID: 10008221, family: "village_move_speed", wantRoll: 3},
		{itemID: 10008223, family: "move_speed", wantRoll: 3},
	} {
		resolution, resolveErr := resolver(tc.itemID)
		potion := resolution.StatChangePotion
		if resolveErr != nil || potion == nil {
			t.Fatalf("item %d resolution=%+v err=%v", tc.itemID, resolution, resolveErr)
		}
		if potion.ClientBuffFamily != tc.family || len(potion.ClientBuffRolls) != tc.wantRoll {
			t.Fatalf("item %d family=%q rolls=%+v want family=%q rolls=%d",
				tc.itemID, potion.ClientBuffFamily, potion.ClientBuffRolls, tc.family, tc.wantRoll)
		}
		if len(potion.BuffRolls) != 0 || len(potion.UpgradeRolls) != 0 || len(potion.DungeonClearExpRolls) != 0 {
			t.Fatalf("item %d must only be a client-applied family: %+v", tc.itemID, potion)
		}
	}

	// 通关经验：服务端要自己算的那一族。
	resolution, err := resolver(10008222)
	if err != nil {
		t.Fatal(err)
	}
	potion := resolution.StatChangePotion
	if potion == nil || len(potion.DungeonClearExpRolls) != 3 || potion.ClientBuffFamily != "" {
		t.Fatalf("clear-exp potion=%+v", potion)
	}
	wantRolls := []struct {
		buffID int64
		weight int64
		bonus  int64
	}{{1103, 30, 50}, {1104, 50, 100}, {1105, 20, 200}}
	for i, want := range wantRolls {
		got := potion.DungeonClearExpRolls[i]
		if got.BuffID != want.buffID || got.Weight != want.weight || got.BonusPercent != want.bonus {
			t.Fatalf("clear-exp roll %d=%+v want %+v", i, got, want)
		}
	}
}

func seriaBuffDefinition(buffID int64, effectName string, param int64) string {
	return "[index]\n" + itoaForTest(buffID) + " \n[icon index]\n3000 \n[tooltip text]\n`seria` \n[effect]\n[name]\n`" +
		effectName + "` \n[param]\n" + itoaForTest(param) + " \n[/param]\n[/effect]\n[/index]\n"
}

// 通关经验加成：base × percent / 100，窗口过期就是 0。
func TestCurrentStatChangePotionClearExperienceBonus(t *testing.T) {
	// 生效中的窗口用真实 API 写（到期 = 现在 + 1 小时）。
	withWindow := func(percent int64) dnfrepo.CharacterRecord {
		character := dnfrepo.CharacterRecord{CharacterID: "11"}
		if _, err := statbuff.ActivateRate(&character, statbuff.KindDungeonClearExp,
			10008222, 1105, percent, time.Hour, time.Now().UTC()); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return character
	}
	// 已过期的窗口只能直接写 Stats：ActivateRate 的语义是"现在开始生效 N 秒"，
	// 不接受过去的到期时间（这正是 fail-closed 的一半）。
	expired := func(percent int64) dnfrepo.CharacterRecord {
		return dnfrepo.CharacterRecord{CharacterID: "11", Stats: map[string]int64{
			"dungeon_clear_exp_rate_buff_percent":   percent,
			"dungeon_clear_exp_rate_buff_expire_at": time.Now().Add(-time.Hour).UTC().Unix(),
			"dungeon_clear_exp_rate_buff_item_id":   10008222,
			"dungeon_clear_exp_rate_buff_id":        1105,
		}}
	}
	for _, tc := range []struct {
		name      string
		character dnfrepo.CharacterRecord
		base      uint32
		want      uint32
	}{
		{name: "no window adds nothing", character: dnfrepo.CharacterRecord{CharacterID: "11"}, base: 1000, want: 0},
		{name: "+50% of the base", character: withWindow(50), base: 1000, want: 500},
		{name: "+100% of the base", character: withWindow(100), base: 1000, want: 1000},
		{name: "+200% of the base", character: withWindow(200), base: 250, want: 500},
		{name: "expired window adds nothing", character: expired(200), base: 1000, want: 0},
		{name: "zero base stays zero", character: withWindow(200), base: 0, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{}
			if got := service.currentStatChangePotionClearExperienceBonus(tc.character, tc.base); got != tc.want {
				t.Fatalf("bonus=%d want=%d", got, tc.want)
			}
		})
	}
}
