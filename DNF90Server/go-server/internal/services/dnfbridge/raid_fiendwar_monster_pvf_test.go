package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 战况表上要画的"怪物头像"必须与进图实际打到的那只是同一只，所以
// FiendWarFixtureBossIDs / fiendWarArmyBossIDs / fiendWarOrcBossIDs 这三张表
// 是从 PVF 抄下来的 —— 这个测试把它们逐条**回读 PVF 复核**：
//
//	dungeon/gcontents/180621_fiendwar/cosmo_*.dgn
//	  [maze info] → [map specification] `boss` <mapID>
//	map/.../(2,0)_Boss_<mapID>.map → [monster] <怪物ID> ... `[boss]`
//
// 一旦换 PVF、或者有人手改这三张表，这里先红，而不是等玩家在战况表上看见
// 一个进图打不到的怪。
//
//	$env:DNF_WORLDMAP_REAL_PVF_SMOKE='runtime\data\dnf\Script.pvf'
//	go test ./internal/services/dnfbridge/ -run FiendWarMonsterTable -count=1 -v
func TestFiendWarMonsterTableMatchesRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify the Fiend War monster table against the real PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// 1. 设施副本：表里的 maze 张数、以及每一张的领主，逐条对 PVF。
	for dungeon, want := range raid.FiendWarFixtureBossIDs {
		value, ok := table.FindDungeon(int64(dungeon))
		if !ok {
			t.Fatalf("PVF 里没有副本 %d", dungeon)
		}
		if len(value.Mazes) != len(want) {
			t.Fatalf("副本 %d：PVF 有 %d 张 maze，表里登记了 %d 只领主", dungeon, len(value.Mazes), len(want))
		}
		for maze, wantBoss := range want {
			got := fiendWarMazeBossMonster(t, table, dungeon, value.Mazes[maze])
			if got != int64(wantBoss) {
				t.Fatalf("副本 %d maze %d 的领主：PVF=%d 表=%d", dungeon, maze, got, wantBoss)
			}
		}
	}

	// 2. 普通作战区域：16 张，每张 6 张 maze —— maze 0..3 是四只军团领主
	//    （[ENEMY CREATE RATE] 的兵种 1..4），maze 4/5 是两只原生怪领主。
	armyWant := []int64{110979, 110977, 110987, 110982}
	orcWant := []int64{110714, 110975}
	normalDungeons := []int{4954, 4955, 4956, 4957, 4958, 4959, 4960, 4961, 4962, 4964, 4966, 4968, 4970, 4972, 4973, 4974}
	for _, dungeon := range normalDungeons {
		value, ok := table.FindDungeon(int64(dungeon))
		if !ok {
			t.Fatalf("PVF 里没有副本 %d", dungeon)
		}
		if len(value.Mazes) != 6 {
			t.Fatalf("副本 %d 的 maze 张数 = %d，want 6（兵种 1..4 + 原生怪 1..2）", dungeon, len(value.Mazes))
		}
		for maze := 0; maze < 6; maze++ {
			var want int64
			if maze < 4 {
				want = armyWant[maze]
			} else {
				want = orcWant[maze-4]
			}
			if got := fiendWarMazeBossMonster(t, table, dungeon, value.Mazes[maze]); got != want {
				t.Fatalf("副本 %d maze %d 的领主：PVF=%d 表=%d", dungeon, maze, got, want)
			}
		}
	}
}

// fiendWarMazeBossMonster 读出这一张 maze 的 BOSS 房里那只 [boss]。
//
// BOSS 房可能不止一只 [boss] 行：雷达基地的 306000219 就是 110973 Boss_Magnetic
// 与 110974 Boss_Polar 两行（磁力恶魔的两极身体，百科的领主表里只登记了"磁力恶魔"
// 一个）。规则取**第一行** —— 那是领主本体，其余是它的部件。
func fiendWarMazeBossMonster(t *testing.T, table *worldmap.Table, dungeon int, maze worldmap.Maze) int64 {
	t.Helper()
	var bossMaps []int64
	for _, spec := range maze.MapSpecifications {
		if fiendWarMapSpecificationKind(spec.Type) != "boss" {
			continue
		}
		bossMaps = append(bossMaps, spec.MapIDs...)
	}
	if len(bossMaps) == 0 {
		t.Fatalf("副本 %d maze %d 没有 `boss` 地图规格", dungeon, maze.Index)
	}
	var bosses []int64
	for _, mapID := range bossMaps {
		value, ok := table.FindMap(mapID)
		if !ok {
			t.Fatalf("副本 %d maze %d：地图 %d 不在世界地图表里", dungeon, maze.Index, mapID)
		}
		for _, spawn := range value.Monsters {
			if fiendWarMonsterSpawnIsBoss(spawn) {
				bosses = append(bosses, spawn.MonsterID)
			}
		}
	}
	if len(bosses) == 0 {
		t.Fatalf("副本 %d maze %d 的 BOSS 房里没有 [boss]", dungeon, maze.Index)
	}
	if len(bosses) > 1 {
		t.Logf("副本 %d maze %d 的 BOSS 房有 %d 只 [boss]，取第一只：%v", dungeon, maze.Index, len(bosses), bosses)
	}
	return bosses[0]
}

func fiendWarMapSpecificationKind(value string) string {
	return strings.ToLower(strings.Trim(value, "[]` \t\r\n"))
}

func fiendWarMonsterSpawnIsBoss(spawn worldmap.MonsterSpawn) bool {
	return fiendWarMapSpecificationKind(spawn.Rank) == "boss" ||
		fiendWarMapSpecificationKind(spawn.SuffixMarker) == "boss"
}
