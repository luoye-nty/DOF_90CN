package dnfbridge

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 史诗碎片池（来自史诗图鉴，按装备 [minimum level] 分档）在当前 PVF 里只有
// 65/70/75/80/85/90 六档，最高 90。这里锁住 currentHellPartyEpicLevels 的取值表。
//
// 回归点：改之前「怪物等级 > 90」直接返回 {90}，导致所有 91 级以上的深渊
// （例如红色魔女之森 [basis level] = 92）永远刷不出 85 级史诗，而 85 档池子里
// 其实有 138 件候选。现在统一按 90 处理，取 {85, 90}，与 90 级副本一致。
func TestCurrentHellPartyEpicLevelsTakesAdjacentTiersAndClampsAboveNinety(t *testing.T) {
	cases := []struct {
		monsterLevel int
		want         []int
	}{
		{1, []int{50}},  // 下限钳到 50
		{49, []int{50}}, // 下限钳到 50
		{50, []int{50}}, // upper=50 lower=45→50，两档重合
		{54, []int{50, 55}},
		{55, []int{50, 55}},
		{70, []int{65, 70}},
		{85, []int{80, 85}},
		{86, []int{85, 90}},
		{90, []int{85, 90}},
		{91, []int{85, 90}},  // 回归点：改前 = {90}
		{92, []int{85, 90}},  // 红色魔女之森 [basis level] = 92
		{95, []int{85, 90}},  // 回归点：改前 = {90}
		{200, []int{85, 90}}, // 池子上限就是 90
	}
	for _, test := range cases {
		got := currentHellPartyEpicLevels(test.monsterLevel)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("monster_level=%d got=%v want=%v", test.monsterLevel, got, test.want)
		}
	}
}

// 真机诊断：用运行时 PVF 打出史诗碎片池分档与实际取池结果。
//
//	DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge -run TestRealPVFHellEpicLevelPools -v
func TestRealPVFHellEpicLevelPools(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect hell party epic pools")
	}
	service := &Service{options: options{pvfPath: pvfPath}}
	_, pools, err := service.currentHellPartyDropAssets()
	if err != nil {
		t.Fatal(err)
	}
	levels := make([]int, 0, len(pools))
	for level := range pools {
		levels = append(levels, level)
	}
	sort.Ints(levels)
	t.Logf("史诗碎片池分档（共 %d 档）", len(levels))
	for _, level := range levels {
		t.Logf("  等级 %3d : %d 件候选", level, len(pools[level]))
	}
	for monsterLevel := 85; monsterLevel <= 95; monsterLevel++ {
		t.Logf("  怪物等级 %2d -> 取池 %v", monsterLevel, currentHellPartyEpicLevels(monsterLevel))
	}
}

// 真机诊断：红色魔女之森(314) 的 [basis level] 决定深渊怪等级，进而决定史诗取池。
func TestRealPVFRedWitchForestHellPartyBasisLevel(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect the red witch forest hell party")
	}
	service := &Service{options: options{pvfPath: pvfPath}}
	if err := service.preloadDungeonWorldMap(context.Background()); err != nil {
		t.Fatal(err)
	}
	table, resolver, err := service.dungeonWorldMap()
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(314)
	if !found {
		t.Fatal("dungeon 314 not found")
	}
	basis := dungeon.Metadata.BasisLevel
	if !basis.Set || basis.Value <= 0 {
		t.Fatalf("dungeon 314 has no basis level: %+v", basis)
	}
	// dungeon_hell_party.go 的 entity_type 0 分支：深渊怪等级 = byte(basis.Value)。
	t.Logf("副本 314 basis_level=%d -> 深渊怪等级=%d -> 史诗取池 %v",
		basis.Value, basis.Value, currentHellPartyEpicLevels(int(basis.Value)))
	topology, err := worldmap.BuildDungeonTopology(resolver, dungeon.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.prepareCurrentDungeonHellParty(table, topology, dungeon, 0, 1); err != nil {
		t.Fatal(err)
	}
}

// oldHellPartyEpicLevels 是修复前的实现，保留下来用于对比「哪些副本曾受影响」。
func oldHellPartyEpicLevels(monsterLevel int) []int {
	if monsterLevel < 50 {
		monsterLevel = 50
	}
	if monsterLevel > 90 {
		return []int{90}
	}
	upper := ((monsterLevel + 4) / 5) * 5
	if upper < 50 {
		upper = 50
	}
	lower := upper - 5
	if lower < 50 {
		lower = 50
	}
	if lower == upper {
		return []int{upper}
	}
	return []int{lower, upper}
}

// dungeonHasHellSection 报告地下城文件里有没有 [hell dungeon] 段
// —— 有些副本不在「深渊区域」下，但自己也带深渊。
func dungeonHasHellSection(dungeon worldmap.Dungeon) bool {
	for _, section := range dungeon.SourceSections {
		if strings.EqualFold(strings.TrimSpace(section.Name), "hell dungeon") {
			return true
		}
	}
	return false
}

// 真机普查：列出所有 [basis level] > 90 的副本 —— 这些副本修复前都被夹成只出 90 级史诗。
func TestRealPVFDungeonsWithBasisLevelAboveNinety(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to enumerate dungeons with basis level above 90")
	}
	service := &Service{options: options{pvfPath: pvfPath}}
	if err := service.preloadDungeonWorldMap(context.Background()); err != nil {
		t.Fatal(err)
	}
	table, _, err := service.dungeonWorldMap()
	if err != nil {
		t.Fatal(err)
	}

	all := table.Dungeons()
	above := make([]worldmap.Dungeon, 0)
	for _, dungeon := range all {
		basis := dungeon.Metadata.BasisLevel
		if basis.Set && basis.Value > 90 {
			above = append(above, dungeon)
		}
	}
	sort.Slice(above, func(i, j int) bool { return above[i].ID < above[j].ID })

	t.Logf("副本总数 %d，其中 [basis level] > 90 的共 %d 个：", len(all), len(above))
	hellCount := 0
	for _, dungeon := range above {
		basis := int(dungeon.Metadata.BasisLevel.Value)
		hell := "否"
		if dungeonHasHellSection(dungeon) {
			hell = "是(自带)"
			hellCount++
		} else if area, ok := table.FindAreaByDungeon(dungeon.ID); ok && area.HellDungeon.Set && area.HellDungeon.Value == 1 {
			hell = "是(深渊区域)"
			hellCount++
		}
		t.Logf("  id=%-5d %-28s basis=%-3d 深渊=%-14s 改前取池 %-4v -> 改后 %v",
			dungeon.ID, dungeon.Metadata.Name, basis, hell,
			oldHellPartyEpicLevels(basis), currentHellPartyEpicLevels(basis))
	}
	t.Logf("其中会开深渊的共 %d 个 —— 这些副本修复前都只能出 90 级史诗。", hellCount)

	// 顺带把 86~90 的副本列出来做对照（这些副本本来就能出 85 级史诗）。
	edge := make([]worldmap.Dungeon, 0)
	for _, dungeon := range all {
		basis := dungeon.Metadata.BasisLevel
		if basis.Set && basis.Value >= 86 && basis.Value <= 90 {
			edge = append(edge, dungeon)
		}
	}
	sort.Slice(edge, func(i, j int) bool { return edge[i].ID < edge[j].ID })
	t.Logf("对照：basis level 在 86~90 的副本共 %d 个（这些本来就能出 85 级史诗）", len(edge))
}
