package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 通关量子时空传送站（area 30/31）之后，落点必须是**待机区 98/2**。
//
// 真机 2026-10-01 16:06（raid 12058646，char_id=1 通关 4978）：
//
//	game-fiendwar-clear-auto-return    town_id=98 area_id=30          ← 目标是 BOSS 房区域
//	game-dungeon-full-town-route-send  town_id=98 area_id=30 position_x=0 position_y=0
//	  position_source=current_exe_op35_session_scoped_luke_runtime_origin_snapshot
//
// 通用"回城"路径回答的是"这个人开打时站在哪个作战区域"，而 BOSS 区域在原点快照里
// 没有坐标（0,0），客户端拿不到可用落点 → 结算都发了、人留在副本里不回城。
// 同一个快照对普通作战区域是好的（实测 area14 → 113,462），所以这条修复只针对
// BOSS 区域，普通区域一个字都不动。
func TestRealPVFFiendWarBossClearReturnsToWaitingRoom(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	f := newRuntimeRaidRewardTestFixture(t, true)
	s := f.service
	s.options.pvfPath = path
	if err := s.preloadTownCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, err := s.runtimeRaidFiendWarConfig()
	if err != nil {
		t.Fatal(err)
	}
	state := s.raids[f.raidKey]
	state.RaidMode, state.RouteOrRaidType = runtimeRaidOwnerFiendWar, 2
	state.AntonConfig, state.FiendWarConfig = nil, config
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.FiendWar, err = raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatal(err)
	}

	// 只有 BOSS 区域走改道；普通作战区域（Kind 为空）保持原样回原区域。
	boss, ok := state.Lifecycle.FiendWar.AreaForDungeon(4978)
	if !ok || !strings.HasSuffix(boss.Kind, "boss") {
		t.Fatalf("副本 4978 应落在 BOSS 区域，实测 kind=%q ok=%t", boss.Kind, ok)
	}
	for _, dungeon := range []int{4954, 4962, 4974} {
		area, found := state.Lifecycle.FiendWar.AreaForDungeon(dungeon)
		if !found || strings.HasSuffix(area.Kind, "boss") {
			t.Fatalf("普通作战区域 %d 不该被判成 BOSS 房：kind=%q", dungeon, area.Kind)
		}
	}

	// 输入就是真机那一笔：通用路径给出的 98/30 + (0,0)。
	generic := currentDungeonTownTransition{
		TownID:         98,
		AreaID:         30,
		PositionSource: "current_exe_op35_session_scoped_luke_runtime_origin_snapshot",
	}
	got, err := s.fiendWarWaitingRoomTransition(*state, 1, generic)
	if err != nil {
		t.Fatal(err)
	}
	if got.TownID != 98 || got.AreaID != 2 {
		t.Fatalf("落点 = %d/%d，want 98/2（待机区）", got.TownID, got.AreaID)
	}
	if got.PositionSource != "fiendwar_clear_waiting_room" {
		t.Fatalf("position_source = %q", got.PositionSource)
	}
	if len(got.Body) == 0 {
		t.Fatal("没有重建场景过渡体，客户端会留在副本里")
	}
	// 坐标必须与撤退/掉线那条路**同源**（PVF 等待室 NPC），不是另造一套。
	town, area, row, err := s.fiendWarWaitingAreaScene(*state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.TownID != town || got.AreaID != area {
		t.Fatalf("改道落点 %d/%d 与 fiendWarWaitingAreaScene 的 %d/%d 不一致", got.TownID, got.AreaID, town, area)
	}
	if got.PositionX != int16(row.Value1) || got.PositionY != int16(row.Value2) {
		t.Fatalf("落点坐标 %d,%d 与等待室 NPC 的 %d,%d 不一致",
			got.PositionX, got.PositionY, int16(row.Value1), int16(row.Value2))
	}
	if got.PositionX == 0 && got.PositionY == 0 {
		t.Fatal("落点仍是 0,0 —— 这正是客户端卡在副本里的原因")
	}

	// 单人翻牌结束时，online raid 可能已经从 s.raids 清理。返回城镇仍然必须
	// 根据当前 BOSS 副本和 PVF 等待室配置落到 98/2，而不是依赖活跃攻坚记录。
	delete(s.raids, f.raidKey)
	finished, applies, err := s.fiendWarCompletedBossReturnTransition(1, 4978, generic)
	if err != nil {
		t.Fatalf("finalized-raid BOSS return: %v", err)
	}
	if !applies || finished.TownID != 98 || finished.AreaID != 2 {
		t.Fatalf("finalized-raid BOSS return = %d/%d applies=%t, want 98/2", finished.TownID, finished.AreaID, applies)
	}
	if finished.PositionX == 0 && finished.PositionY == 0 {
		t.Fatal("finalized-raid BOSS return still has no waiting-room position")
	}
}
