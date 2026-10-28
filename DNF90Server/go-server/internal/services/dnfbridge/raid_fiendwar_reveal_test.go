package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 百科（地图构造）："雷达：该副本通关后增加所有玩家10%技能伤害，并显示地图中怪物
// 头像。（反之怪物头像显示问号）" —— 符号 81 RADAR STATE 就是那个开关。
func fiendWarRevealBattle() *raid.FiendWarBattle {
	return &raid.FiendWarBattle{
		Config: &raid.FiendWarBattleConfig{
			Symbols: map[string]uint32{"radar state": 81},
			Areas: map[int]raid.FiendWarArea{
				14: {Index: 14, Dungeon: 4962},                      // 普通作战区域
				15: {Index: 15, Dungeon: 4963, Kind: "radar"},       // 单图设施
				19: {Index: 19, Dungeon: 4967, Kind: "top outpost"}, // 三图设施（预抽 maze 2）
			},
		},
		Symbols:      map[uint32]int{81: 0},
		Areas:        map[int]raid.FiendWarAreaState{},
		FixtureMazes: map[int]int{19: 2},
	}
}

func TestFiendWarRadarGatesBoardMonster(t *testing.T) {
	b := fiendWarRevealBattle()
	// 雷达未通关：**ID 照发、只换脸**。
	//   ID —— 红框只由 unitID 决定，清零就整格空白（既没框也没问号），
	//         2026-10-01 的 packet log 与现场截图双向印证过；
	//   脸 —— type 换成 fiendWarFaceUnknown（= 0）。type 14/15/16 实测是"框在、里面空的"，
	//         只有 0 从来没发出去过：服务端过去拿 Type==0 当"这格没单位"的哨兵，
	//         带 0 的行在发出去之前就被自己的守卫吃掉了 —— 所以现在用第二个返回值
	//         （present）来表示"有没有单位"，与 Type 彻底分开。
	if got, present := runtimeFiendWarRevealEnemy(b, 19, raid.FiendWarUnit{ID: 4967, Type: 7}); !present || got.ID != 110708 || got.Type != fiendWarFaceUnknown {
		t.Fatalf("雷达未通关的前哨格 = %+v present=%v，want ID=110708（框要画）Type=%d（问号）", got, present, fiendWarFaceUnknown)
	}
	if got, present := runtimeFiendWarRevealEnemy(b, 15, raid.FiendWarUnit{ID: 4963, Type: 5}); !present || got.ID != 110973 || got.Type != fiendWarFaceUnknown {
		t.Fatalf("雷达未通关的雷达格 = %+v present=%v，want ID=110973 Type=%d（问号）", got, present, fiendWarFaceUnknown)
	}
	if got, present := runtimeFiendWarRevealEnemy(b, 14, raid.FiendWarUnit{ID: 3, Type: 2}); !present || got.ID != 110977 || got.Type != fiendWarFaceUnknown {
		t.Fatalf("雷达未通关的军团格 = %+v present=%v，want ID=110977 Type=%d（问号）", got, present, fiendWarFaceUnknown)
	}
	if got, present := runtimeFiendWarRevealMonster(b, 14, raid.FiendWarUnit{ID: 5, Type: 1}); !present || got.ID != 110714 || got.Type != fiendWarFaceUnknown {
		t.Fatalf("雷达未通关的中立怪格 = %+v present=%v，want ID=110714 Type=%d（问号）", got, present, fiendWarFaceUnknown)
	}
	// 问号那个 type 必须能真的发出去：它不再是"这格没单位"的哨兵值。
	if fiendWarFaceUnknown != 0 {
		t.Logf("问号 type = %d（若 0 被证伪，这里就是换上的新值）", fiendWarFaceUnknown)
	}
	// 真的没有单位的格子依然一行都不发。
	if _, present := runtimeFiendWarRevealEnemy(b, 14, raid.FiendWarUnit{}); present {
		t.Fatal("空区域不该发号")
	}

	b.Symbols[81] = 1 // 雷达通关

	// 设施格：露出这一格**固定**的那张图的领主。fixture 给 19 号格钉的是 maze 2，
	// 所以露出的是 110708（4967 maze 2 的领主）；脸仍然是这一格的位置 kind（7）——
	// 客户端那张 kind→脸 表按位置排，而实际打到的怪也钉在同一位置，两者必然一致
	// （见 TestFiendWarFixtureBoardAndEntryAreFixedTogether）。
	if got, present := runtimeFiendWarRevealEnemy(b, 19, raid.FiendWarUnit{ID: 4967, Type: 7}); !present || got.ID != 110708 || got.Type != 7 {
		t.Fatalf("雷达通关后的前哨格 = %+v，want ID=110708 Type=7", got)
	}
	// 单图设施：没有预抽记录时 maze=0，露出恒定的那只（雷达基地 = 磁力恶魔:马格内特）。
	if got, present := runtimeFiendWarRevealEnemy(b, 15, raid.FiendWarUnit{ID: 4963, Type: 5}); !present || got.ID != 110973 || got.Type != 5 {
		t.Fatalf("雷达通关后的雷达格 = %+v，want ID=110973 Type=5", got)
	}
	// 军团格：兵种 type 决定它走进哪张 maze，头像用那张图的领主。
	for enemyType, want := range map[byte]uint32{1: 110979, 2: 110977, 3: 110987, 4: 110982} {
		if got, _ := runtimeFiendWarRevealEnemy(b, 14, raid.FiendWarUnit{ID: uint32(enemyType), Type: enemyType}); got.ID != want {
			t.Fatalf("兵种 %d 的头像 = %d，want %d", enemyType, got.ID, want)
		}
	}
	// 中立怪：monster type 1..2 → maze 4/5 的领主。
	for monsterType, want := range map[byte]uint32{1: 110714, 2: 110975} {
		if got, _ := runtimeFiendWarRevealMonster(b, 14, raid.FiendWarUnit{ID: 7, Type: monsterType}); got.ID != want {
			t.Fatalf("原生怪 %d 的头像 = %d，want %d", monsterType, got.ID, want)
		}
	}
	// 空格子（没有单位）永远不发：雷达通关也不会凭空长出一只怪。
	if got, present := runtimeFiendWarRevealEnemy(b, 14, raid.FiendWarUnit{}); present || got != (raid.FiendWarUnit{}) {
		t.Fatalf("空区域的揭示 = %+v present=%v，want 零值且不发", got, present)
	}
}

// 进图用的 maze 必须与雷达通关后战况表显示的那只是同一个：
// selectRuntimeFiendWarMaze 读的是同一张 FixtureMazes。
func TestFiendWarFixtureMazeAndBoardAgree(t *testing.T) {
	b := fiendWarRevealBattle()
	b.Symbols[81] = 1
	maze, ok := b.MazeForArea(19)
	if !ok {
		t.Fatal("前哨没有预抽记录")
	}
	board, present := runtimeFiendWarRevealEnemy(b, 19, raid.FiendWarUnit{ID: 4967, Type: 7})
	if !present {
		t.Fatal("前哨格应该发号")
	}
	fromEntry, ok := raid.FiendWarFixtureBossID(4967, maze)
	if !ok {
		t.Fatal("进图侧查不到这只领主")
	}
	if board.ID != fromEntry {
		t.Fatalf("表上 %d != 进图 %d", board.ID, fromEntry)
	}
}

// 战况表上画的必须就是进图会打到的那一只 —— 而且这是**固定**的，不靠抽签对齐。
//
// 操作员口径（2026-10-01 复核历史截图）：
//
//	"我所说的这些格子里的怪物是固定的……雷达打完以后客户端就是这么画的，服务端也
//	 理应按照客户端画的去让怪物真正百分百地出现 —— 就不再抽签了。"
//
// 客户端那张 kind→脸 的表按格子位置排（上哨=宾柏 / 中哨=薛丁恶 / 下哨=克鲁瑟，
// 兵站本营同理），而战况表发的 type 正是这个位置 kind。所以这里钉死：
// 表格子画谁，它固定用的那张 maze 的领主就必须是谁。
func TestFiendWarFixtureBoardAndEntryAreFixedTogether(t *testing.T) {
	cases := []struct {
		area    int
		kind    string
		maze    int
		face    byte
		boss    uint32
		dungeon int
	}{
		{19, "top outpost", 0, 7, 110981, 4967},         // 上哨 · 宾柏
		{21, "mid outpost", 1, 8, 110984, 4969},         // 中哨 · 薛丁恶
		{23, "bot outpost", 2, 9, 110708, 4971},         // 下哨 · 克鲁瑟
		{27, "top command center", 0, 10, 110709, 4975}, // 上兵营 · 光之恶魔
		{28, "mid command center", 1, 11, 110978, 4976}, // 中兵营 · 暗之恶魔
		{29, "bot command center", 2, 12, 110992, 4977}, // 下兵营 · 重力恶魔
	}
	areas := map[int]raid.FiendWarArea{}
	for _, tc := range cases {
		areas[tc.area] = raid.FiendWarArea{Index: tc.area, Dungeon: tc.dungeon, Kind: tc.kind}
	}
	b, err := raid.NewFiendWarBattle(&raid.FiendWarBattleConfig{
		Raid:    &raid.AntonConfig{PhaseMax: 1, MemberMax: 8},
		Rules:   []raid.FiendWarRule{{}},
		Areas:   areas,
		Symbols: map[string]uint32{"radar state": 81},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Symbols[81] = 1 // 雷达通关：这一格该露脸了
	for _, tc := range cases {
		maze, ok := b.MazeForArea(tc.area)
		if !ok {
			t.Fatalf("设施 %d 没有固定图", tc.area)
		}
		if maze != tc.maze {
			t.Fatalf("设施 %d 的固定图 = %d，want %d", tc.area, maze, tc.maze)
		}
		got, present := runtimeFiendWarRevealEnemy(b, tc.area, raid.FiendWarUnit{ID: uint32(tc.dungeon), Type: tc.face})
		if !present {
			t.Fatalf("设施 %d 应该发号", tc.area)
		}
		if got.ID != tc.boss || got.Type != tc.face {
			t.Fatalf("设施 %d 表上画的 = %+v，want ID=%d Type=%d", tc.area, got, tc.boss, tc.face)
		}
		// 进图侧读的是同一张图（selectRuntimeFiendWarMaze → MazeForArea），
		// 所以这里查出来的领主就是那一格真正会打到的领主。
		fromEntry, ok := raid.FiendWarFixtureBossID(tc.dungeon, maze)
		if !ok || fromEntry != tc.boss {
			t.Fatalf("设施 %d 进图侧查到的领主 = %d（ok=%t），want %d", tc.area, fromEntry, ok, tc.boss)
		}
	}
}
