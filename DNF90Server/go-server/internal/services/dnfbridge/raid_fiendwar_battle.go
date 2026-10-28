package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 超时空符号分两路走，分界线不是"121"，而是 op1460 自己认的区间。
//
// op1460 的 per-row 处理器 `0x009AF190` 第一件事就是
//
//	lea ecx, [eax-0x51]   ; key - 81
//	cmp ecx, 0x2c         ; 81..125
//	ja  default           ; 区间外的行被静默丢弃
//
// 而 `etc/raid/fiendwar/fiendwar.etc.symbol` 给这些 id 的正是：
// 81 雷达 / 82 炮塔 / 83-88 前哨与兵站本营 / 92-93 BOSS 状态 / 94 BOSS 机制位 /
// 95-96 BOSS HP / 102-103 KILL ALL、KICK OUT / 105 FIENDWAR INIT /
// 110-111 BOSS 显示 HP / 121-125 演出开关。
//
// 客户端把 81..88 写进战况表的 board+0x1C、92..96 与 102..103 写进 board+0x34
// 的字符串覆盖、110/111 直接调 UI 0x784150（BOSS 显示 HP）。
//
// 改动前只有 `key >= 121` 的行走 1460，也就是 81..111 全被发到了 op583 ——
// 客户端在那几个结构里读不到它们，于是雷达/炮塔状态、BOSS 状态与 BOSS 血条全空。
// 581..583 那条路（op583 = RaidSymbolMsgID）保持原样，只把 1460 的区间放宽。
const (
	// currentFiendWarNativeSymbolKeyMin/Max 是 op1460 认的 key 闭区间。
	currentFiendWarNativeSymbolKeyMin = 81
	currentFiendWarNativeSymbolKeyMax = 125
	// currentFiendWarRaidSymbolKeyMax 是 op583 那条路的上界（原行为：key < 121）。
	currentFiendWarRaidSymbolKeyMax = 121
)

// countRuntimeRaidGroupedMembers counts the LIVE roster rows that carry a real squad
// label. It exists for the failure log, which has to prove the 攻坚队 came through the
// failure with its 编制 intact.
func countRuntimeRaidGroupedMembers(state runtimeRaidState) int {
	grouped := 0
	for _, member := range state.Members {
		if runtimeRaidMemberSquad(member.GroupIndex) != 0 {
			grouped++
		}
	}
	return grouped
}

func syncRuntimeFiendWarObjectives(state *runtimeRaidState) {
	b := state.Lifecycle.FiendWar
	if b == nil {
		return
	}
	for id, o := range state.Lifecycle.Objectives {
		a, ok := b.AreaForDungeon(id)
		if !ok {
			continue
		}
		o.State = raid.AntonDungeonHide
		if b.AreaOpen(a.Index) || b.Areas[a.Index].Group != 0 {
			o.State = raid.AntonDungeonOpen
			o.RemainingHP = 1
		} else {
			o.RemainingHP = 0
		}
		state.Lifecycle.Objectives[id] = o
	}
}

func runtimeFiendWarRewardEligible(state runtimeRaidState, characterID uint16) bool {
	if state.RaidMode != runtimeRaidOwnerFiendWar {
		return true
	}
	config := runtimeRaidBaseConfig(state)
	if config == nil || len(config.Phases) != 1 {
		return false
	}
	minimum := config.Phases[0].MinimalDungeonClearCount
	if state.TailFlag == 1 {
		if config.SingleMode == nil || state.Lifecycle.FiendWar == nil || !state.Lifecycle.FiendWar.Complete {
			return false
		}
		minimum = config.SingleMode.Phase.MinimalDungeonClearCount
	}
	return state.Lifecycle.FiendWarClearCounts[characterID] >= minimum
}

// Called once by the dungeon-return owner before freezing the town transition.
func (s *Service) prepareRuntimeFiendWarRetreat(session *gameSession, transition currentDungeonTownTransition, died bool) (currentDungeonTownTransition, error) {
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || state.Lifecycle.FiendWar == nil || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return transition, nil
	}
	config := state.Lifecycle.FiendWar.Config
	waiting, found := s.townArea(int64(config.WaitingTown), int64(config.WaitingArea))
	if !found {
		return transition, fmt.Errorf("fiendwar waiting town absent from PVF")
	}
	var row currentSceneTransitionRow
	for _, npc := range waiting.NPCs {
		if slices.Contains(config.WaitingNPCs, int(npc.NPCID)) && npc.X >= 0 && npc.X <= 32767 && npc.Y >= 0 && npc.Y <= 32767 {
			row = currentSceneTransitionRow{ObjectOrResourceKey: currentSceneActorObjectKey(session.selectedCharacterID), Value1: uint16(npc.X), Value2: uint16(npc.Y), Value3: currentAntonRaidDirection, Value4: currentAntonRaidAreaState}
			break
		}
	}
	if row.ObjectOrResourceKey == 0 {
		return transition, fmt.Errorf("fiendwar recovery NPC position absent from PVF")
	}
	town, area := byte(config.WaitingTown), byte(config.WaitingArea)
	transition.TownID, transition.AreaID = town, area
	transition.PositionX, transition.PositionY = int16(row.Value1), int16(row.Value2)
	transition.Direction, transition.AreaState = row.Value3, row.Value4
	transition.PositionSource = "fiendwar_retreat_waiting_room"
	if died {
		transition.PositionSource = "fiendwar_death_recovery"
	}
	body, err := buildCurrentSceneTransitionBody(town, area, []currentSceneTransitionRow{row})
	if err != nil {
		return transition, err
	}
	transition.Body = body
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	current := s.raids[state.RaidKey]
	if current == nil || current.Lifecycle.FiendWar == nil || current.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return transition, errRuntimeRaidNotStarted
	}
	battle := current.Lifecycle.FiendWar.Clone()
	member, found := runtimeRaidFrozenMember(*current, session.selectedCharacterID)
	if !found {
		return transition, errRuntimeRaidNotMember
	}
	if battle.MemberAreas == nil {
		battle.MemberAreas = make(map[uint16]int)
	}
	// Interrupted runs return to the PVF camp; leaving a member in an uncleared
	// area both blocks movement and immediately starts another encounter.
	battle.MemberAreas[member.CharID] = config.WaitingArea
	if died {
		battle.ImprisonMember(member.CharID, s.gameplayNow())
	}
	group := runtimeRaidNormalizedGroup(member.GroupIndex)
	if !slices.ContainsFunc(current.FrozenMembers, func(peer runtimeRaidMemberState) bool {
		return peer.CharID != member.CharID && runtimeRaidNormalizedGroup(peer.GroupIndex) == group && battle.MemberArea(peer.CharID, group) != config.WaitingArea
	}) {
		battle.GroupAreas[group] = config.WaitingArea
	}
	current.Lifecycle.FiendWar = battle
	current.Lifecycle.Revision++
	return transition, nil
}

func (s *Service) selectRuntimeFiendWarMaze(characterID uint16, dungeon worldmap.Dungeon) (dungeonMazeSelection, error) {
	state, ok := s.runtimeRaidForMember(characterID)
	if !ok || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.FiendWar == nil {
		return dungeonMazeSelection{}, errRuntimeRaidNotStarted
	}
	b := state.Lifecycle.FiendWar
	a, ok := b.AreaForDungeon(int(dungeon.ID))
	member, present := runtimeRaidFrozenMember(state, characterID)
	if !ok || !present || !b.SingleAreaAllowed(a.Index) ||
		b.MemberArea(characterID, runtimeRaidNormalizedGroup(member.GroupIndex)) != a.Index ||
		!b.AreaOpen(a.Index) {
		return dungeonMazeSelection{}, errRuntimeRaidDungeonUnavailable
	}
	index, reason := 0, "fiendwar_area_unit_pvf_maze"
	if a.Kind == "" {
		unit := b.Areas[a.Index]
		// Actual PVF mazes: succubus/fusion/buster/alloy, then two orc variants.
		if unit.Enemy.ID != 0 {
			index = int(unit.Enemy.Type) - 1
		} else if unit.Monster.ID != 0 {
			index = int(unit.Monster.Type) + 3
		} else {
			return dungeonMazeSelection{}, errRuntimeRaidDungeonUnavailable
		}
	} else if rolled, ok := b.MazeForArea(a.Index); ok {
		// 设施区域（前哨基地/兵站本营）：这一格用哪张图是**固定的**，由它的位置决定
		// （上 → maze 0、中 → 1、下 → 2，见 raid.fiendWarFixtureMazeForKind）。
		// 它与战况表上画的那只同源，所以"表上看到的"与"进图打到的"必然是同一只。
		index = rolled
		reason = "fiendwar_fixture_maze_fixed"
	} else {
		// 没登记的战场（旧存档、或测试直接构造的 battle）保持原行为：当场取一张。
		// 单图副本（雷达/炮塔/传送站）只有一张，这里恒为 0。
		var err error
		index, err = s.chooseDungeonIndex(len(dungeon.Mazes))
		if err != nil {
			return dungeonMazeSelection{}, err
		}
		reason = "fiendwar_fixture_maze_default"
	}
	if index < 0 || index >= len(dungeon.Mazes) {
		return dungeonMazeSelection{}, worldmap.ErrMazeNotFound
	}
	// 记一条：这一场到底按哪条规则进的图。真机上报过"战况表画的是人类背叛者、
	// 进去打的却是宇宙粉碎机"，而两条路都读同一张 FixtureMazes —— 所以要么这里落到了
	// random 兜底分支（说明抽签没生效），要么两者其实一致、是别的环节对不上。
	// board_boss 是战况表对同一格会画的那只，直接放在一行里对照。
	if session, online := s.onlineGameSession(characterID); online && session != nil {
		rolledMaze, rolled := b.MazeForArea(a.Index)
		boardBoss, _ := raid.FiendWarFixtureBossID(a.Dungeon, rolledMaze)
		s.logGameEvent(session, "game-fiendwar-maze-selected",
			"char_id", characterID,
			"dungeon_id", dungeon.ID,
			"area", a.Index,
			"kind", a.Kind,
			"maze_index", index,
			"maze_count", len(dungeon.Mazes),
			"reason", reason,
			"rolled", rolled,
			"rolled_maze", rolledMaze,
			"board_boss", boardBoss,
			"source", "select_runtime_fiendwar_maze")
	}
	return dungeonMazeSelection{Index: index, Reason: reason}, nil
}

// Native enemy types 5..13 are the fixed PVF objectives, not roaming units.
//
// 返回值是这一格的**底账**（哪个单位站在这里），不带"给不给玩家看"这一层 ——
// 揭示与否由 runtimeFiendWarRevealEnemy 决定，因为客户端是靠单位 ID 画头像的，
// 而揭示开关（雷达）与单位本身是两条独立的线。
func runtimeFiendWarBoardEnemy(b *raid.FiendWarBattle, id int) raid.FiendWarUnit {
	if b == nil {
		return raid.FiendWarUnit{}
	}
	a := b.Config.Areas[id]
	kind := runtimeFiendWarFixtureKind(a.Kind)
	if kind == 0 {
		return b.Areas[id].Enemy
	}
	if b.AreaOpen(id) || b.Areas[id].Group != 0 {
		return raid.FiendWarUnit{ID: uint32(a.Dungeon), Type: kind}
	}
	return raid.FiendWarUnit{}
}

// runtimeFiendWarFixtureKind 把 PVF 的 [AREA INFO] 里的设施种类映射成客户端认的
// type：5 雷达 / 6 炮塔 / 7..9 前哨基地 / 10..12 兵站本营 / 13 量子时空传送站。
//
// 这个 type 同时就是客户端那张 kind→脸 表的下标，而**那张表是按格子的位置排的**：
//
//	7 人类背叛者:宾柏 / 8 引力恶魔:薛丁恶 / 9 宇宙粉碎机:克鲁瑟   （上/中/下 前哨基地）
//	10 光之恶魔:欧格里 / 11 暗之恶魔:艾森 / 12 重力恶魔:白斯      （上/中/下 兵站本营）
//
// 操作员复核历史截图确认：**每个格子画出来的怪是固定的**（不论哪一局都是这几只）。
// 所以真正要固定的是"实际打到的那只"，规则由 raid.fiendWarFixtureMazeForKind 给出
// （上格 → maze 0、中格 → 1、下格 → 2），两边就此对齐，不再靠抽签去碰。
func runtimeFiendWarFixtureKind(kind string) byte {
	return map[string]byte{"radar": 5, "battery": 6, "top outpost": 7, "mid outpost": 8, "bot outpost": 9, "top command center": 10, "mid command center": 11, "bot command center": 12, "top boss": 13, "bot boss": 13}[kind]
}

// runtimeFiendWarRadarRevealed 报告"地图中怪物头像"是否已经对全团解锁。
//
// 权威出处（百度百科「超时空之战」· 地图构造）：
//
//	雷达：该副本通关后增加所有玩家10%技能伤害，并显示地图中怪物头像。
//	      （反之怪物头像显示问号）
//
// 符号 81 RADAR STATE：PVF 在 [FIENDWAR INIT] 里置 0，雷达房被打通时由 battle.Clear
// 置 1。所以 0 = 未通关 = 问号，1 = 已通关 = 显示头像。
func runtimeFiendWarRadarRevealed(b *raid.FiendWarBattle) bool {
	if b == nil {
		return false
	}
	return b.Value("radar state") == 1
}

// runtimeFiendWarRevealEnemy 把这一格的底账换算成**发给客户端的单位行**。
//
// fiendWarFaceUnknown 是"雷达未通关"时发给这一格的 type —— 也就是那张**问号**。
//
// 它是实测出来的，不是猜的。2026-10-01 一次进团把候选值铺到不同格子上（红框一律
// 照画，所以"空框 / 头像 / 问号"一眼可分），结果：
//
//	type 0        → 红框里一个红问号   ← 就是它（雷达15、炮塔17、上哨19、上兵营27
//	                                    以及所有普通作战区域的数字格子同时命中）
//	type 1..13    → 怪物头像（13 是 BOSS 格，一直是头像）
//	type 14/15/16 → 红框照画、**里面是空的**（非法值）
//	type 17       → 头像（高值里也有合法的）
//	type 18/20    → 空框
//
// 为什么以前一直发不出 0：服务端拿 Type==0 当"这一格没有单位"的哨兵，带 0 的行
// 在发出去之前就被自己的守卫吃掉了。现在"有没有单位"由 runtimeFiendWarRevealEnemy
// 的第二个返回值（present）表示，与 Type 彻底分开，0 才能真的上线。
//
// 另一个已经证实的事实：**红框本身只由 unitID 决定** —— ID 认得出就画框，脸画不画
// 得出来是 type 的事。所以不能靠"ID=0"来表示未侦察（那样整格空白，既没框也没问号），
// 必须 **ID 照发 + type 换成 0**。
const fiendWarFaceUnknown byte = 0

// fiendWarUnknownFace 允许运维用环境变量临时换一张"未侦察"脸做 A/B：
//
//	set FIENDWAR_UNKNOWN_FACE=13
//
// 变量名不带 DNF 前缀是刻意的 —— 启动器的 runtimeEnvironment 会清掉继承的
// DNFBRIDGE_/DNF_/DFO_*，只有别的名字能透传到服务端进程。
func fiendWarUnknownFace() byte {
	raw := strings.TrimSpace(os.Getenv("FIENDWAR_UNKNOWN_FACE"))
	if raw == "" {
		return fiendWarFaceUnknown
	}
	value, err := strconv.ParseUint(raw, 10, 8)
	if err != nil {
		return fiendWarFaceUnknown
	}
	return byte(value)
}

// runtimeFiendWarRevealEnemy 把这一格的底账换算成**发给客户端的单位行**。
// 第二个返回值是"这一格到底有没有单位"，它与 Type 必须分开：
//
//	Type == 0 在底账里表示"没有单位"，而 0 又正好是客户端那张"问号脸"的下标，
//	所以问号脸必须靠这个 bool 才能发得出去（以前用 Type != 0 当守卫，问号永远发不出去）。
//
// 红框只由 unitID 决定：ID 认得出就画框，脸由 type 决定。因此雷达没通关时
// **ID 照发、只换 type**：
//
//	雷达未通关 → ID = 这一格实际会打到的那只怪，Type = 问号
//	雷达已通关 → 同一个 ID，Type = 这一格自己的头像
func runtimeFiendWarRevealEnemy(b *raid.FiendWarBattle, id int, unit raid.FiendWarUnit) (raid.FiendWarUnit, bool) {
	if unit.Type == 0 {
		return raid.FiendWarUnit{}, false
	}
	scouted := runtimeFiendWarScoutedEnemy(b, id, unit)
	if !runtimeFiendWarRadarRevealed(b) {
		scouted.Type = fiendWarUnknownFace()
	}
	return scouted, true
}

// runtimeFiendWarScoutedEnemy 是"雷达已通关"时应有的那一行：
//   - 普通作战区域的敌军军团 → 该兵种会走进的那张 maze 的领主；
//   - 原生怪（中立兽人）→ 同上，走 maze 4/5；
//   - 设施区域 → 该副本【开团时抽定】的那张图的领主（单图副本恒定）。
func runtimeFiendWarScoutedEnemy(b *raid.FiendWarBattle, id int, unit raid.FiendWarUnit) raid.FiendWarUnit {
	a := b.Config.Areas[id]
	if runtimeFiendWarFixtureKind(a.Kind) != 0 {
		maze, _ := b.MazeForArea(id)
		if boss, ok := raid.FiendWarFixtureBossID(a.Dungeon, maze); ok {
			return raid.FiendWarUnit{ID: boss, Type: unit.Type}
		}
		return raid.FiendWarUnit{Type: unit.Type}
	}
	// 普通作战区域：这里的 type 是兵种 1..4，它决定这只军团会走进哪张 maze
	// （maze = type-1），头像就用那张 maze 的领主。
	if boss, ok := raid.FiendWarArmyBossID(unit.Type); ok {
		return raid.FiendWarUnit{ID: boss, Type: unit.Type}
	}
	return raid.FiendWarUnit{Type: unit.Type}
}

// runtimeFiendWarRevealMonster 是原生怪/中立怪（op1456）的揭示：monster type 1..2
// 决定 maze 4/5，头像用那张 maze 的领主；雷达未通关时同样只把脸换成问号。
func runtimeFiendWarRevealMonster(b *raid.FiendWarBattle, id int, unit raid.FiendWarUnit) (raid.FiendWarUnit, bool) {
	if unit.Type == 0 {
		return raid.FiendWarUnit{}, false
	}
	scouted := raid.FiendWarUnit{Type: unit.Type}
	if boss, ok := raid.FiendWarOrcBossID(unit.Type); ok {
		scouted.ID = boss
	}
	if !runtimeFiendWarRadarRevealed(b) {
		scouted.Type = fiendWarUnknownFace()
	}
	return scouted, true
}

func runtimeFiendWarBoardPackets(state, previous runtimeRaidState, viewer uint16, now time.Time) ([]runtimeRaidWireNotification, error) {
	b := state.Lifecycle.FiendWar
	if b == nil {
		return nil, errRuntimeRaidLifecycleState
	}
	old := previous.Lifecycle.FiendWar
	if previous.RaidKey != state.RaidKey {
		old = nil
	}
	var packets []runtimeRaidWireNotification
	appendRows := func(op uint16, rows [][]byte) {
		for len(rows) > 0 {
			n := min(len(rows), 255)
			body := []byte{byte(n)}
			for _, r := range rows[:n] {
				body = append(body, r...)
			}
			packets = append(packets, runtimeRaidWireNotification{op, body})
			rows = rows[n:]
		}
	}
	var symbols []raid.RaidSymbolRow
	var nativeSymbols [][]byte
	hiddenAreaSymbols := make(map[uint32]bool)
	if b.Single != nil {
		for id := range b.Config.Areas {
			if !b.SingleAreaAllowed(id) {
				hiddenAreaSymbols[b.Config.Areas[id].Symbol] = true
			}
		}
	}
	keys := make([]int, 0, len(b.Symbols))
	for key := range b.Symbols {
		keys = append(keys, int(key))
	}
	sort.Ints(keys)
	for _, key := range keys {
		value := b.Symbols[uint32(key)]
		if hiddenAreaSymbols[uint32(key)] {
			value = 0
		}
		if key < currentFiendWarRaidSymbolKeyMax {
			symbols = append(symbols, raid.RaidSymbolRow{Key: uint32(key), Value: uint32(value)})
		}
		if old != nil && old.Symbols[uint32(key)] == value {
			continue
		}
		if key < currentFiendWarNativeSymbolKeyMin || key > currentFiendWarNativeSymbolKeyMax {
			continue
		}
		if key >= currentFiendWarRaidSymbolKeyMax && (old == nil || value == 0) {
			continue
		}
		row := binary.LittleEndian.AppendUint32(nil, uint32(key))
		row = binary.LittleEndian.AppendUint32(row, uint32(value))
		nativeSymbols = append(nativeSymbols, row)
	}
	body, err := raid.BuildRaidSymbolBody(symbols)
	if err != nil {
		return nil, err
	}
	packets = append(packets, runtimeRaidWireNotification{raid.RaidSymbolMsgID, body})
	appendRows(1460, nativeSymbols)
	ids := make([]int, 0, len(b.Areas))
	for id := range b.Areas {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var enemies, monsters, warnings, timers [][]byte
	// unitRow 的第四个字段 const：客户端自己把默认值预置成 2，我们一直跟着发 2
	// （逐像素 A/B 证明换成 0/1 画出来一模一样，见 tools/_tmp_logs/re_badge/cmp2_badge_zoom.png）。
	unitRow := func(area int, unit raid.FiendWarUnit, op uint32, enemy bool) []byte {
		r := []byte{byte(area)}
		r = binary.LittleEndian.AppendUint32(r, unit.ID)
		r = append(r, unit.Type)
		r = binary.LittleEndian.AppendUint32(r, op)
		if enemy {
			r = binary.LittleEndian.AppendUint32(r, 2)
		}
		return r
	}
	for _, id := range ids {
		a := b.Areas[id]
		prev := raid.FiendWarAreaState{}
		if old != nil {
			prev = old.Areas[id]
		}
		if !b.SingleAreaAllowed(id) {
			continue
		}
		// 底账（谁在这一格）与揭示（给不给看）分开比较：
		//   - 底账变了 → 老规矩，先 2(移除) 再 1(新增)；
		//   - 只有揭示变了（雷达刚通关）→ 只补一条 1(新增)，让客户端把问号换成头像。
		//     刻意不发 2：那一条要带"上一次公布过的 ID"，而问号那一行的 ID 是 0，
		//     拿 0 去删很可能删不掉，反而在同一格留下两行。
		enemy, previousEnemy := runtimeFiendWarBoardEnemy(b, id), runtimeFiendWarBoardEnemy(old, id)
		enemyRow, enemyPresent := runtimeFiendWarRevealEnemy(b, id, enemy)
		previousRow, previousPresent := runtimeFiendWarRevealEnemy(old, id, previousEnemy)
		switch {
		case previousEnemy != enemy, enemyPresent != previousPresent:
			if previousPresent {
				enemies = append(enemies, unitRow(id, previousRow, 2, true))
			}
			if enemyPresent {
				enemies = append(enemies, unitRow(id, enemyRow, 1, true))
			}
		case enemyPresent && enemyRow != previousRow:
			enemies = append(enemies, unitRow(id, enemyRow, 1, true))
		}
		monsterRow, monsterPresent := runtimeFiendWarRevealMonster(b, id, a.Monster)
		previousMonsterRow, previousMonsterPresent := runtimeFiendWarRevealMonster(old, id, prev.Monster)
		switch {
		case prev.Monster != a.Monster, monsterPresent != previousMonsterPresent:
			if previousMonsterPresent {
				monsters = append(monsters, unitRow(id, previousMonsterRow, 2, false))
			}
			if monsterPresent {
				monsters = append(monsters, unitRow(id, monsterRow, 1, false))
			}
		case monsterPresent && monsterRow != previousMonsterRow:
			monsters = append(monsters, unitRow(id, monsterRow, 1, false))
		}
		// sub_9B1380 ignores row[1]; every row starts a six-second alert.
		if a.Warning && !prev.Warning {
			warnings = append(warnings, []byte{byte(id), 1})
		}
		// sub_9B19E0 derives start=deadline-duration for bars; sub_A48D20
		// compares it with the Unix-second server clock (sub_2276FE0).
		// Kind 12 keeps the deadline directly for the dungeon countdown.
		//
		// Kind 12 是**按区域**表达的"这一次进攻还剩多久"，也就是玩家在副本里看到的
		// 那个倒计时。它必须"有在跑的进攻才有钟"：
		//
		//	有 deadline -> 发 {area,12,deadline,limit}，客户端 AreaRecord 上的钟跟着走；
		//	deadline 刚没 -> 补**一条** {area,12,0,0} 把钟撤掉（op1458 是逐行更新不是
		//	                全量快照 —— appendRows 会按 255 行分片，全量快照语义在分片
		//	                下自相矛盾；op1455/1456/1457 也用 op=2 撤行）；
		//	本来就空闲 -> **一行都不发**。
		//
		// 以前这里对全部 30 个区域无条件补 3 行，于是每个 tick 有 30 条
		// {area,12,0,0} 在断言"这一格的副本倒计时是 0"：玩家站在没开打的格子里时，
		// 客户端按 0 画，0x0255 的**攻坚倒计时**再也切不回来；通关/退出把 deadline
		// 清空后，同一行又把它钉死在 0 上 —— 操作员报的"进副本前/通关后看不到攻坚
		// 倒计时"就是这两段。空闲格现在不发行，客户端回落到 0x0255。
		for _, kind := range []byte{0, 2, 12} {
			var start, deadline time.Time
			if kind == 12 {
				if a.Deadline.IsZero() {
					if !prev.Deadline.IsZero() {
						timers = append(timers, []byte{byte(id), 12, 0, 0, 0, 0, 0, 0, 0, 0})
					}
					continue
				}
				deadline = a.Deadline
				start = deadline.Add(-time.Duration(b.Config.Areas[id].LimitSeconds) * time.Second)
			} else {
				for key, timer := range b.Timers {
					wire := raid.FiendWarTimerKinds[key.Kind]
					if key.Area == id && (kind == 0 && wire == 0 || kind == 2 && wire >= 2 && wire <= 9) {
						if deadline.IsZero() || timer.Deadline.Before(deadline) {
							start = timer.StartedAt
							deadline = timer.Deadline
						}
					}
				}
			}
			end, duration := uint32(0), uint32(0)
			if !start.IsZero() && !deadline.IsZero() && deadline.After(start) && deadline.After(now) && start.Unix() > 0 && deadline.Unix() <= 1<<31-1 {
				end = uint32(deadline.Unix())
				duration = uint32(deadline.Unix() - start.Unix())
			}
			r := []byte{byte(id), kind}
			r = binary.LittleEndian.AppendUint32(r, end)
			r = binary.LittleEndian.AppendUint32(r, duration)
			timers = append(timers, r)
		}
	}
	appendRows(1457, enemies)
	appendRows(1456, monsters)
	appendRows(1454, warnings)
	appendRows(1458, timers)
	var users [][]byte
	recoveryDeadline := func(battle *raid.FiendWarBattle, id uint16) uint32 {
		if battle == nil {
			return 0
		}
		if until := battle.RecoveryUntil[id]; !until.IsZero() {
			return uint32(until.Unix())
		}
		return 0
	}
	userRow := func(op uint32, id uint16, area int, deadline uint32) []byte {
		r := binary.LittleEndian.AppendUint32(nil, op)
		r = append(r, byte(area))
		r = binary.LittleEndian.AppendUint32(r, uint32(id))
		// sub_9ADDC0 subtracts the server clock from this recovery deadline.
		return binary.LittleEndian.AppendUint32(r, deadline)
	}
	for _, member := range previous.FrozenMembers {
		_, present := runtimeRaidFrozenMember(state, member.CharID)
		if old != nil && !present {
			users = append(users, userRow(2, member.CharID, old.MemberArea(member.CharID, runtimeRaidNormalizedGroup(member.GroupIndex)), recoveryDeadline(old, member.CharID)))
		}
	}
	for _, member := range state.FrozenMembers {
		group := runtimeRaidNormalizedGroup(member.GroupIndex)
		area := b.MemberArea(member.CharID, group)
		deadline := recoveryDeadline(b, member.CharID)
		previousMember, present := runtimeRaidFrozenMember(previous, member.CharID)
		if old == nil || !present {
			users = append(users, userRow(1, member.CharID, area, deadline))
			continue
		}
		previousArea := old.MemberArea(member.CharID, runtimeRaidNormalizedGroup(previousMember.GroupIndex))
		if previousArea != area {
			users = append(users, userRow(2, member.CharID, previousArea, recoveryDeadline(old, member.CharID)))
			users = append(users, userRow(1, member.CharID, area, deadline))
			continue
		}
		if recoveryDeadline(old, member.CharID) != deadline {
			users = append(users, userRow(1, member.CharID, area, deadline))
		}
	}
	appendRows(1455, users)
	packets = append(packets, runtimeRaidWireNotification{1459, []byte{byte(b.MerchantArea)}})
	var dungeons []raid.RaidDungeonStateRow
	// 区域倒计时（0x0254）——"这一格打起来还剩多久"。它和战况表上的条不是同一个通道：
	//
	//	0x0254(596) → 客户端存储 A：map<pair<timerType, value0>, {start, end}>，
	//	              value0 = **地牢 ID**（就是下面 585 行里那个 DungeonID）；
	//	0x0255(597) → 客户端存储 B：map<timerType, endMs>，只看 timerType。
	//	（证据 tools/攻坚队两个倒计时_根因与改法_20260916.md:12-25、:80-97。）
	//
	// 客户端的门渲染器按 585/586 给的门状态去查存储 A，9 个调用点守卫一致：
	// 状态 0 → 查 {timerType 1, dungeonID}，状态 1|3 → 查 {timerType 2, dungeonID}，
	// 状态 2（隐藏）什么都不画。超时空的战况表把整张通用快照替换掉了
	// （raid_phase_runtime.go:276 / raid_current_compat.go:303 / raid_rejoin_runtime.go:264
	// 三条都直接 return 到这张表），而通用路径每一条都是 **0x0254 紧跟 0x0255** 成对发的
	// （raid_phase_runtime.go:363-377、raid_current_compat.go:339-352、
	//   raid_result_flow.go:314-318、raid_rejoin_runtime.go:246-250、
	//   raid_online.go:2362-2371），安徒恩的门计时器也是同一形状
	// （raid_anton_phase_one.go:587-595）。于是超时空这一半从来没发过：玩家进图后
	// 客户端查存储 A 查不到这一格 → 倒计时根本不画，只剩下 0x0255 的攻坚剩余时间。
	//
	// 只在该区域的 deadline **发生变化**时发一条（首次 / 进图 / 重连补帧各一次），
	// 不跟着 1 Hz 心跳重复：形状与安徒恩一致（value1=绝对 deadline、value2=时长），
	// 万一本频道的分派表走的是"相对"分支（start=now、end=now+value2），重复发会
	// 把倒计时永远钉在满值 —— 只发一次则两种语义都正确。
	var areaTimers []runtimeRaidWireNotification
	for _, id := range ids {
		a := b.Config.Areas[id]
		if a.Dungeon <= 0 {
			continue
		}
		o := state.Lifecycle.Objectives[a.Dungeon]
		wire := runtimeRaidObjectiveWireState(o.State)
		dungeons = append(dungeons, raid.RaidDungeonStateRow{DungeonID: uint32(a.Dungeon), State: wire})
		area := b.Areas[id]
		var previousArea raid.FiendWarAreaState
		// 零值目标状态（AntonDungeonState("")）在 wire 上就是 hide(2)，
		// 所以首次帧 / 没有旧账时拿 2 当"上一帧"是对的。
		previousWire := uint32(2)
		if old != nil {
			previousArea = old.Areas[id]
			previousWire = runtimeRaidObjectiveWireState(previous.Lifecycle.Objectives[a.Dungeon].State)
		}
		// 触发条件是 **(deadline, 门状态) 这一对**变了，不是只有 deadline。
		// 只比 deadline 会漏掉"进图时状态还没同步"的那一帧：deadline 已经在
		// Enter 里设好了，可 Objectives 要等下一次 sync 才变 open —— 那一帧按
		// hide 被跳过之后，deadline 再也不会"变"，这条倒计时就永远发不出去了。
		if area.Deadline.IsZero() ||
			(area.Deadline.Equal(previousArea.Deadline) && wire == previousWire) {
			continue
		}
		timerType, drawable := runtimeRaidGateCountdownTimerType(wire)
		if !drawable {
			continue
		}
		areaTimers = append(areaTimers, runtimeRaidWireNotification{
			msgID: raid.RaidTimerMsgID,
			body: raid.BuildRaidTimerBody(raid.RaidTimer{
				TimerType: timerType,
				Value0:    uint32(a.Dungeon),
				Value1:    uint32(area.Deadline.Unix()),
				Value2:    runtimeRaidDurationSeconds(time.Duration(a.LimitSeconds) * time.Second),
			}),
		})
	}
	body, err = raid.BuildRaidDungeonStateBody(raid.RaidDungeonStateSnapshot{Rows: dungeons})
	if err != nil {
		return nil, err
	}
	packets = append(packets, runtimeRaidWireNotification{raid.RaidDungeonStateMsgID, body})
	packets = append(packets, areaTimers...)
	packets = append(packets, runtimeRaidWireNotification{raid.RaidRemainTimeMsgID, raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: runtimeFiendWarViewerClockSeconds(state, viewer, now)})})
	return packets, nil
}

// runtimeFiendWarViewerClockSeconds 是这一名**观看者**此刻 HUD 上"剩余时间"该显示的秒数。
//
// 操作员的要求是**一个显示、三种状态**：进副本前显示攻坚倒计时、进副本后显示该副本
// 倒计时、通关/退出后切回攻坚倒计时。
//
// 那个显示读的是客户端**存储 B**（`map<timerType,endMs>`，键只有 timerType，唯一写者
// 是 0x0255 —— tools/攻坚队两个倒计时_根因与改法_20260916.md:12-25、:101-120 的
// 全局渲染器 `cmp [raidObj+0x28],5` 决定读 type 1 还是 type 0）。所以"切换"这件事只能由
// 服务端按**收件人**决定这一次 0x0255 带哪个值：
//
//	这一位此刻站在一个**有在跑 deadline 的区域**里 -> 带那个区域还剩多久；
//	否则（在战况表上、在待机区、刚通关回城）  -> 带整个攻坚阶段的剩余。
//
// 2026-10-02 的教训：我先前把 0x0254（存储 A）当成这个显示的来源，补了整整一条包，
// 真机上截图仍然是 `00:59:39`。存储 A 的 9 个消费点画的是**战况表上的门/格子按钮**，
// 而玩家在副本里时那张表是关着的 —— 存储 A 填得再对，也照不到这个 HUD。真正要改的是
// 这一行 0x0255。
func runtimeFiendWarViewerClockSeconds(state runtimeRaidState, viewer uint16, now time.Time) uint32 {
	raidClock := runtimeRaidDurationSeconds(state.Lifecycle.PhaseDeadline.Sub(now))
	b := state.Lifecycle.FiendWar
	if b == nil || viewer == 0 {
		return raidClock
	}
	// 先看"这一位本人被记在哪一格"。MemberAreas 是进图/回城时逐人写的账本
	// （game-fiendwar-member-area-marked），它才是"这一个人在不在图里"的正解。
	if areaID, tracked := b.MemberAreas[viewer]; tracked {
		if seconds, live := runtimeFiendWarAreaClockSeconds(b, areaID, now); live {
			return seconds
		}
	}
	// 账本区域会滞后（2026-10-01「走不动」那轮的病根），所以再用**小队的区域**兜一次。
	// 两条都拿不到"有在跑的钟"就回落到攻坚倒计时 —— 战况表上、待机区、刚通关回城
	// 都是这一支。
	if member, ok := runtimeRaidFrozenMember(state, viewer); ok {
		group := runtimeRaidNormalizedGroup(member.GroupIndex)
		if areaID, tracked := b.GroupAreas[group]; tracked {
			if seconds, live := runtimeFiendWarAreaClockSeconds(b, areaID, now); live {
				return seconds
			}
		}
	}
	return raidClock
}

// runtimeFiendWarAreaClockSeconds 报告这一格是不是"真的被一个小队占着、而且钟还在跑"，
// 是的话返回还剩多少秒。通关那一刻 battle.Clear 会把 Group 与 Deadline 一起清掉
// （fiendwar_battle.go:556），所以这一条同时覆盖了"通关/退出后切回攻坚倒计时"。
func runtimeFiendWarAreaClockSeconds(b *raid.FiendWarBattle, areaID int, now time.Time) (uint32, bool) {
	if b == nil {
		return 0, false
	}
	area := b.Areas[areaID]
	if area.Group == 0 || area.Deadline.IsZero() || !area.Deadline.After(now) {
		return 0, false
	}
	return runtimeRaidDurationSeconds(area.Deadline.Sub(now)), true
}

// runtimeRaidGateCountdownTimerType 把 0x0249 的门状态映射成客户端**去存储 A 里查**
// 用的 timerType（tools/攻坚队两个倒计时_根因与改法_20260916.md:80-97）：
//
//	state 0     -> timerType 1（开门/生效窗倒计时）
//	state 1 | 3 -> timerType 2（关闭/已清的恢复倒计时）
//	其它(-1|2)  -> 客户端不画，所以也不必发
func runtimeRaidGateCountdownTimerType(wireState uint32) (byte, bool) {
	switch wireState {
	case 0:
		return 1, true
	case 1, 3:
		return 2, true
	default:
		return 0, false
	}
}

func (s *Service) sendRuntimeFiendWarBoard(session *gameSession, state runtimeRaidState, announce bool, now time.Time) error {
	session.fiendWarBoardMu.Lock()
	defer session.fiendWarBoardMu.Unlock()
	current, member := s.runtimeRaidForMember(session.selectedCharacterID)
	if !member || current.RaidKey != state.RaidKey {
		return nil
	}
	if current.Lifecycle.Revision > state.Lifecycle.Revision {
		state = current
	}
	prev := session.fiendWarBoard
	if prev.RaidKey == state.RaidKey && prev.Lifecycle.Revision > state.Lifecycle.Revision {
		return nil
	}
	packets, err := runtimeFiendWarBoardPackets(state, prev, session.selectedCharacterID, now)
	if err != nil {
		return err
	}
	// 战况表这一版准备给每个设施区域画哪只怪。和进图那条 `game-fiendwar-maze-selected`
	// 放在一起，就能证明"表上画的"与"进去打的"是不是同一只（真机上报过不一致）。
	// 只在变化时记一行，不会刷屏。
	if b := state.Lifecycle.FiendWar; b != nil {
		shown := make([]string, 0, len(b.Config.Areas))
		for id, areaCfg := range b.Config.Areas {
			if runtimeFiendWarFixtureKind(areaCfg.Kind) == 0 {
				continue
			}
			maze, _ := b.MazeForArea(id)
			boss, ok := raid.FiendWarFixtureBossID(areaCfg.Dungeon, maze)
			if !ok {
				continue
			}
			shown = append(shown, fmt.Sprintf("area%d:dungeon%d:maze%d:boss%d", id, areaCfg.Dungeon, maze, boss))
		}
		sort.Strings(shown)
		signature := strings.Join(shown, ",")
		if signature != session.fiendWarBoardFixtureSignature {
			session.fiendWarBoardFixtureSignature = signature
			s.logGameEvent(session, "game-fiendwar-board-fixtures",
				"raid_key", state.RaidKey,
				"char_id", session.selectedCharacterID,
				"entries", shown,
				"source", "send_runtime_fiendwar_board")
		}
	}
	if prev.RaidKey != state.RaidKey {
		packets = append([]runtimeRaidWireNotification{{1453, []byte{1, 0}}}, packets...)
	}
	if announce || prev.RaidKey != state.RaidKey {
		packets = append(packets, runtimeRaidWireNotification{raid.RaidStateMsgID, raid.BuildRaidStateBody(raid.RaidState{State: 2, Argument: 0})}, runtimeRaidWireNotification{1453, []byte{2, 0}}, runtimeRaidWireNotification{1452, nil})
	}
	for _, packet := range packets {
		if err := s.sendGameUpperRawClassCodec(session, packet.msgID, packet.body, 0, true); err != nil {
			return err
		}
	}
	session.fiendWarBoard = cloneRuntimeRaidState(state)
	return nil
}

func (s *Service) broadcastRuntimeFiendWarBoard(state runtimeRaidState, announce bool, now time.Time) error {
	for _, member := range state.FrozenMembers {
		if session, ok := s.onlineGameSession(member.CharID); ok {
			if !isCurrentFiendWarRaidChannel(session) {
				continue
			}
			if err := s.sendRuntimeFiendWarBoard(session, state, announce, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) scheduleRuntimeFiendWarTick(state runtimeRaidState) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	return queue.ScheduleAfter(fmt.Sprintf("fiendwar:%d:%d", state.RaidKey, state.StartAttempt), time.Second, func(now time.Time) {
		s.raidMu.Lock()
		current := s.raids[state.RaidKey]
		if current == nil || current.StartAttempt != state.StartAttempt || current.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || current.Lifecycle.FiendWar == nil {
			s.raidMu.Unlock()
			return
		}
		battle := current.Lifecycle.FiendWar.Clone()
		err := battle.Tick(now)
		if err == nil {
			captureRuntimeFiendWarRetreats(current, battle)
			current.Lifecycle.FiendWar = battle
			syncRuntimeFiendWarObjectives(current)
			current.Lifecycle.Revision++
		}
		next := cloneRuntimeRaidState(*current)
		s.raidMu.Unlock()
		if err != nil {
			s.logPacketEvent("game-fiendwar-tick-failed", "raid_key", state.RaidKey, "error", err)
			return
		}
		if battle.Failed {
			if err := s.grantRuntimeFiendWarRetry(next, now); err != nil {
				s.logPacketEvent("game-fiendwar-retry-ticket-grant-failed", "raid_key", next.RaidKey, "error", err)
				s.scheduleRuntimeFiendWarRetry(next, now)
			}
			// 攻坚失败：不解散攻坚队，只结束这一次"攻坚运行"。
			// failRuntimeFiendWarRaid 取代 abandonRuntimeRaid —— 后者会删掉攻坚队行，
			// 于是 town_move 之后拒绝一切 AreaID>=2 的 op36 (连等待区也要求成员资格)，
			// 站在城镇作战区的人自己走不回去。收尾见 publishFailedFiendWar。
			if failed, failErr := s.failRuntimeFiendWarRaid(next.RaidKey, now); failErr == nil {
				if err := s.publishFailedFiendWar(failed); err != nil {
					s.logPacketEvent("game-fiendwar-failure-publish-failed", "raid_key", next.RaidKey, "error", err)
				}
				// 战斗失败也是"攻坚失败"：与倒计时到点同一条返还（超时空特有，安东/卢克
				// 只有倒计时那一种）。放在演出之后，见 refundRuntimeRaidEntryOnFailure。
				s.refundFailedRuntimeRaidEntry(failed, now)
			} else {
				s.logPacketEvent("game-fiendwar-failure-transition-failed", "raid_key", next.RaidKey, "error", failErr)
			}
			return
		}
		if err := s.returnRuntimeFiendWarDungeons(next, next.Lifecycle.FiendWarRetreats, false); err != nil {
			s.logPacketEvent("game-fiendwar-retreat-failed", "raid_key", state.RaidKey, "error", err)
		}
		if err := s.broadcastRuntimeFiendWarBoard(next, false, now); err != nil {
			s.logPacketEvent("game-fiendwar-board-send-failed", "raid_key", state.RaidKey, "error", err)
		}
		for _, member := range next.FrozenMembers {
			identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
			if !online || identity.session == nil {
				continue
			}
			if err := s.postGameSessionCharacterEvent(identity.session, "fiendwar-encounter", identity.character, identity.generation, func() error {
				return s.tryRuntimeFiendWarEncounter(identity.session, now)
			}); err != nil {
				s.logPacketEvent("game-fiendwar-encounter-post-failed", "raid_key", next.RaidKey, "error", err)
			}
		}
		if err := s.scheduleRuntimeFiendWarTick(next); err != nil {
			s.logPacketEvent("game-fiendwar-tick-schedule-failed", "raid_key", state.RaidKey, "error", err)
		}
	})
}

// Consume each domain retreat under raidMu while its original attempt still
// owns the room. Area-only markers must not survive into another selection.
func captureRuntimeFiendWarRetreats(state *runtimeRaidState, battle *raid.FiendWarBattle) {
	if state.Lifecycle.FiendWarRetreats == nil {
		state.Lifecycle.FiendWarRetreats = make(map[int]runtimeRaidDungeonProgress)
	}
	for group, dungeon := range battle.Retreats {
		// 本服务端的一次区域尝试按 (副本, 座位) 记账，座位就是发起那次进攻的小队
		// (runtimeRaidDungeonSeat{Group: g})。超时空战场自己已经保证"一个区域只被
		// 一个小队占用"，所以 group 直接映射成座位即可；未编队成员 (group 0) 在
		// 超时空不允许入场，不会出现以角色为座位的记录。
		seat := runtimeRaidDungeonSeat{Group: group}
		key := runtimeRaidDungeonRunKey{MapID: dungeon, Seat: seat}
		if progress, ok := state.Lifecycle.Dungeons[key]; ok && progress.Seat == seat {
			state.Lifecycle.FiendWarRetreats[dungeon] = progress
			delete(state.Lifecycle.Dungeons, key)
			delete(state.Lifecycle.GroupOccupancy, seat)
			state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{progress.CharacterID, progress.LifecycleToken}] = runtimeRaidDungeonTerminal{Phase: progress.Phase, MapID: dungeon, Kind: runtimeRaidDungeonAbandoned}
		}
		delete(battle.Retreats, group)
	}
}

// The caller holds townMu and has already validated/committed the area move.
// fiendWarPlannedEncounterWindow 是"计划进图正在跑"的兜底时长：正常一趟（含
// prepareRuntimePartyDungeonEntry 把 4 个人逐个备好）实测都在 1 秒内，真机最长
// 的一次（23:07 驱动者卡住那次）也没超过 3 秒。5 秒只是"清标记那一行万一没跑到"
// 的安全网 —— 它不是节流，正常路径由 defer 立刻清掉。
const fiendWarPlannedEncounterWindow = 5 * time.Second

// beginRuntimeFiendWarPlannedEncounter 在把计划进图投进本人信箱**之前**立牌。
//
// 顺序是刻意的：牌必须在任何人读到"本人实时坐标已被推到目标区域"之前立好，
// 否则落在 [推坐标, runtime 落地] 这 ~300ms 窗口里的战况表心跳照样会单人开图。
func (s *Service) beginRuntimeFiendWarPlannedEncounter(session *gameSession, now time.Time) {
	if session == nil {
		return
	}
	session.dungeon.mu.Lock()
	session.fiendWarPlannedEncounterUntil = now.Add(fiendWarPlannedEncounterWindow)
	session.dungeon.mu.Unlock()
}

// endRuntimeFiendWarPlannedEncounter 在计划那一趟跑完（成功、失败、或者退化成
// 普通移动）之后立刻落牌，战况表心跳随即恢复。
func (s *Service) endRuntimeFiendWarPlannedEncounter(session *gameSession) {
	if session == nil {
		return
	}
	session.dungeon.mu.Lock()
	session.fiendWarPlannedEncounterUntil = time.Time{}
	session.dungeon.mu.Unlock()
}

// runtimeFiendWarPlannedEncounterPending 报告"这一刻是否有计划进图在飞"。只看
// 本人自己的信箱：牌是每个人自己立的，别人的进图不影响我的心跳。
func (s *Service) runtimeFiendWarPlannedEncounterPending(session *gameSession, now time.Time) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	until := session.fiendWarPlannedEncounterUntil
	session.dungeon.mu.Unlock()
	return !until.IsZero() && now.Before(until)
}

// fiendWarEncounterPeerRefusal reports why one area companion cannot be dragged
// into this encounter yet, or "" when they can.
//
// 区域一起进图的代价就在这条判定上：一个人没好（离线 / 还在切场景 / 坐标还没落），
// 整格就都不进。这是刻意的 —— 把一个正在切场景的客户端硬拖进副本会把它弄坏。
// 但它必须**可观测**：原来这里全是静默 `return false, nil`，真机上只能看到
// "走进了区域却没开打"，没有任何线索。所以每条拒绝都带 peer_id 与 reason 落盘。
func (s *Service) fiendWarEncounterPeerRefusal(peer runtimeRaidMemberState, townID byte, area int) string {
	other, online := s.onlineGameSession(peer.CharID)
	if !online || other == nil {
		return "peer_offline"
	}
	if ready, _ := s.currentTownEnterSelectReady(other); !ready {
		return "peer_scene_not_ready"
	}
	other.townMu.Lock()
	position := other.townPositionSnapshot
	other.townMu.Unlock()
	if position.TownID != townID || int(position.AreaID) != area {
		return "peer_not_in_area"
	}
	return ""
}

// Queue on the leader mailbox so a last-arriving follower never blocks its own
// mailbox while the leader's ordinary dungeon entry prepares that follower.
func (s *Service) queueRuntimeFiendWarMoveEncounterLocked(session *gameSession, request currentTownSetUserAreaRequest, row currentSceneTransitionRow, body []byte) (bool, error) {
	session.townMu.Unlock()
	defer session.townMu.Lock()
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || state.Lifecycle.FiendWar == nil {
		return false, nil
	}
	b := state.Lifecycle.FiendWar
	area := int(request.AreaID)
	member, found := runtimeRaidFrozenMember(state, session.selectedCharacterID)
	if !found || !b.AreaOpen(area) || b.Areas[area].Group != 0 {
		return false, nil
	}
	// 超时空的进图单位是**编制小队**（操作员 2026-10-01 的定案：「也不做附近自动组队的
	// 机制了，暂时就按小队编制走吧」）。所以名单就是发起者自己那支小队，且全队都得站在
	// **他按下进图时所在的那个格子**（originArea）—— 不是 request.AreaID：后者是隔壁
	// 那个要打的区域（真机 op36：area=6 / opaque=(98,3)）。
	//
	// 驱动者仍然按官方口径是**按下进图的人**（「第一个进入新副本的玩家为队长」）：
	// 若挂到名册序第一个队友的邮箱上，他没动过，selectRuntimeFiendWarMaze
	// 的"驱动者必须站在该区域"就会把整场拒掉 —— 真机看到的就是队伍刚组成又解散。
	//
	// 编制本身不动：进图时编出来的仍是一支临时普通队伍（tryRuntimeFiendWarEncounter →
	// replaceManagedRuntimeParty），攻坚队/小队的名册与座位一概不改。
	session.townMu.Lock()
	origin := session.townPositionSnapshot
	session.townMu.Unlock()
	originArea := int(origin.AreaID)
	if !origin.PositionValid || originArea < 2 || int(origin.TownID) != int(request.TownID) {
		// 实时坐标还没落下来时，用客户端自己报的"我从哪一格来"兜底。
		if from := int(request.OpaqueU16B); from >= 2 {
			originArea = from
		} else {
			originArea = int(b.MemberArea(member.CharID, runtimeRaidNormalizedGroup(member.GroupIndex)))
		}
	}
	plan := fiendWarEncounterPlan{Area: area, OriginArea: originArea, Requester: session.selectedCharacterID}
	leaderID := session.selectedCharacterID
	// 原来这里还有一段「队友没准备好就整脚 defer」的守卫：遍历发起者**小队**的成员，
	// 只要有一个人不在原格（离线 / 还在切场景 / 坐标没落），就直接 return false，
	// 让这一脚退化成普通移动。
	//
	// 2026-10-01 按客户端原生规格去掉（弹窗 sub_9AE390，dstr 75467）：
	//
	//   - 进图单位是**原格里站着的人**，不是小队 —— 一个不相关的小队成员没有资格
	//     挡住这一脚；
	//   - 名单由 tryRuntimeFiendWarEncounter 里的 runtimeRaidFiendWarAreaRoster
	//     现算，它会把"此刻进不去的人"剔掉并记一行日志，而不是把所有人一起拖住。
	//
	// 留着一个"谁都能把这一脚按停"的守卫，正是真机上"走进了区域却什么都没发生"
	// 的来源之一（13:27 那次的 game-fiendwar-squad-encounter-deferred 就是它）。
	leader, online := s.onlineGameSessionCharacterSnapshot(leaderID)
	if !online || leader.session == nil {
		return false, nil
	}
	session.townMu.Lock()
	previousPosition := session.townPositionSnapshot
	previousOrigin, previousBound := session.townSelectorOriginSnapshot, session.townSelectorOriginBound
	setCurrentTownPositionSceneFromTransitionLocked(session, session.selectedCharacterID, request.TownID, request.AreaID, row)
	characterID, generation := session.selectedCharacterID, session.characterGeneration
	moveBody := append([]byte(nil), body...)
	session.townMu.Unlock()
	// 立牌必须在投递之前：从这一行起，本人的实时坐标已经指向目标区域，落在
	// [这里, runtime 落地] 之间的战况表心跳会单人开图（真机 2026-10-01 00:03:53）。
	s.beginRuntimeFiendWarPlannedEncounter(session, s.gameplayNow())
	err := s.postGameSessionCharacterEvent(leader.session, "fiendwar-move-encounter", leader.character, leader.generation, func() error {
		defer s.endRuntimeFiendWarPlannedEncounter(session)
		if err := s.tryRuntimeFiendWarEncounterPlanned(leader.session, s.gameplayNow(), &plan); err != nil {
			return err
		}
		return s.postGameSessionCharacterEvent(session, "fiendwar-move-encounter-result", characterID, generation, func() error {
			session.dungeon.mu.Lock()
			entered := session.dungeon.runtime != nil
			session.dungeon.mu.Unlock()
			if entered {
				return nil
			}
			session.townMu.Lock()
			position := session.townPositionSnapshot
			session.townMu.Unlock()
			if position.TownID != request.TownID || position.AreaID != request.AreaID {
				return nil // A newer move owns the scene now.
			}
			// Admission can change while queued; complete the validated field
			// move once instead of leaving the client waiting or retrying it here.
			return s.handleTownSetUserAreaAfterEncounter(session, moveBody, true)
		})
	})
	session.townMu.Lock()
	if err != nil {
		session.dungeon.mu.Lock()
		entered := session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		if !entered {
			session.townPositionSnapshot = previousPosition
			session.townSelectorOriginSnapshot, session.townSelectorOriginBound = previousOrigin, previousBound
		}
	}
	session.townMu.Unlock()
	// 投递本身失败时闭包一次都没跑，defer 也就没机会落牌 —— 这里补一下，别让心跳
	// 白等满 5 秒的安全网。
	if err != nil {
		s.endRuntimeFiendWarPlannedEncounter(session)
	}
	return true, err
}

// currentFiendWarSquadlessNotice is what a Fiend War member standing in an
// uncleared area with no squad is told.
//
// It is deliberately phrased as an instruction rather than a refusal: the state
// itself is legitimate (the operator's ruling makes a member who walked home
// alone squadless), and the raid manager's regroup command is the gesture that
// ends it.
const currentFiendWarSquadlessNotice = "你不在任何小队里，请先在攻坚队管理器中重新编队。"

// runtimeRaidFiendWarSquadSource names which roster answered "which squad does this
// member fight in".
type runtimeRaidFiendWarSquadSource string

const (
	// runtimeRaidFiendWarSquadSourceLive: the member's LIVE roster row named a squad
	// that the live roster also has rows for. This is the ordinary case and the only
	// source an encounter is built from.
	runtimeRaidFiendWarSquadSourceLive runtimeRaidFiendWarSquadSource = "live"
	// runtimeRaidFiendWarSquadSourceFrozen: the member has no LIVE row at all, so the
	// frozen phase roster answered instead.
	runtimeRaidFiendWarSquadSourceFrozen runtimeRaidFiendWarSquadSource = "frozen"
)

// runtimeRaidFiendWarSquad is the resolved answer to "which squad does this member
// walk into a Fiend War area with".
//
// Refusal is empty exactly when Group/Members are usable; otherwise it names the
// fact that stopped the resolution, for the log and for the player-facing notice.
type runtimeRaidFiendWarSquad struct {
	Group   byte
	Members []runtimeRaidMemberState
	Source  runtimeRaidFiendWarSquadSource
	Refusal string
}

// runtimeRaidLiveMemberSquad reports the squad the member's LIVE roster row carries,
// or 0 when the row is unassigned or absent. It is the live-row counterpart of
// runtimeRaidMemberSquad(runtimeRaidFrozenMember(...).GroupIndex).
func runtimeRaidLiveMemberSquad(state runtimeRaidState, characterID uint16) byte {
	index := runtimeRaidMemberIndex(state, characterID)
	if index < 0 {
		return 0
	}
	return runtimeRaidMemberSquad(state.Members[index].GroupIndex)
}

// runtimeRaidLiveSquadMembers lists the LIVE roster rows of one squad in roster
// order, which is the order runtimeRaidPartyGroups produces and therefore the order
// runtimeRaidGroupPartyState projects onto a native party.
func runtimeRaidLiveSquadMembers(state runtimeRaidState, group byte) []runtimeRaidMemberState {
	if group == 0 {
		return nil
	}
	for _, members := range runtimeRaidPartyGroups(state) {
		if len(members) > 0 && runtimeRaidMemberSquad(members[0].GroupIndex) == group {
			return members
		}
	}
	return nil
}

// runtimeRaidFiendWarEncounterSquad resolves the squad a Fiend War encounter is
// started under. It exists because the LIVE roster and the FROZEN phase roster can
// disagree about one member's squad, and that disagreement used to be a silent
// dead end.
//
// The measured failure (live 2026-10-01, raid 12058625, char 1):
//
//	11:34:24 game-raid-squad-unassigned-on-solo-town-return char_id=1 previous_group=1
//	         group=0 slot=0 frozen_row=kept_for_phase_credit
//	         source=current_exe_bodyless_op42_active_run_giveup
//	11:35:55 game-town-set-user-area-blocked area_id=10
//	         reason=fiendwar source area requires combat
//
// unassignRuntimeRaidSquadOnSoloTownReturn rewrites the live row only
// (runtimeRaidSquadLabelLiveRowOnly) because the frozen row still credits the phase.
// This function read the group from the FROZEN row (group 1), searched the LIVE
// groups for it, found none, and returned nil without a word: the encounter never
// fired, so the area's monster could never be cleared, and FiendWarBattle.Move
// refuses to let anyone walk past an uncleared area. The member was locked in.
//
// The precedence is the operator's ruling (「如果是队内成员单独自己回城 应该取消这个
// 人的编制」 plus 「在战况表里撤退 应该保持编制」): the LIVE row is the roster the
// 攻击队 window draws and the squad -> ordinary-party projection is built from, so
// it decides. The frozen row answers only when the live roster has no row for the
// member at all, which is the one divergence a live row cannot explain.
//
// Group 0 with a live row is NOT a fallback case: it is the legitimate squadless
// state the solo town return produces, and the member is expected to re-assign
// himself in the raid manager (game-raid-manager-regrouped / type=669) before the
// encounter can fire.
func runtimeRaidFiendWarEncounterSquad(state runtimeRaidState, characterID uint16) runtimeRaidFiendWarSquad {
	frozen, ok := runtimeRaidFrozenMember(state, characterID)
	if !ok {
		return runtimeRaidFiendWarSquad{Refusal: "member_not_in_frozen_roster"}
	}
	hasLiveRow := runtimeRaidMemberIndex(state, characterID) >= 0
	squad, source := runtimeRaidLiveMemberSquad(state, characterID), runtimeRaidFiendWarSquadSourceLive
	members := []runtimeRaidMemberState(nil)
	switch {
	case squad != 0:
		members = runtimeRaidLiveSquadMembers(state, squad)
	case hasLiveRow:
		// The live row exists and is unassigned: the legitimate squadless state the
		// solo town return produces. Deliberately NOT answered from the frozen row --
		// that is the ghost squad the live bug dragged the member into.
		return runtimeRaidFiendWarSquad{Refusal: "member_not_in_any_squad"}
	default:
		// The live roster has no row for this member at all, which only the frozen
		// phase roster can explain. Its squad still exists if the live roster has its
		// peers, and the member joins them: the fused list is the frozen row plus the
		// squad's live rows, put back in roster order (the same order
		// runtimeRaidPartyGroups produces).
		squad, source = runtimeRaidMemberSquad(frozen.GroupIndex), runtimeRaidFiendWarSquadSourceFrozen
		members = append(members, frozen)
		members = append(members, runtimeRaidLiveSquadMembers(state, squad)...)
		runtimeRaidSortMembers(members)
	}
	if squad == 0 {
		return runtimeRaidFiendWarSquad{Refusal: "member_not_in_any_squad"}
	}
	if len(members) == 0 {
		// Nobody in the live roster is in the squad this row names, so there is no
		// party to walk in with.
		return runtimeRaidFiendWarSquad{Group: squad, Source: source, Refusal: "squad_has_no_live_members"}
	}
	if !slices.ContainsFunc(members, func(member runtimeRaidMemberState) bool {
		return member.CharID == characterID
	}) {
		return runtimeRaidFiendWarSquad{Group: squad, Source: source, Refusal: "member_missing_from_its_own_squad"}
	}
	return runtimeRaidFiendWarSquad{Group: squad, Members: members, Source: source}
}

// fiendWarEncounterPlan 是"由一次移动触发的遭遇战"的既定事实，由**发起移动的那一次
// 调用**算好，交给（很可能挂在别人邮箱上的）遭遇战执行体。
//
// 为什么必须传，而不能在执行时重算：真机 op36 的语义是
//
//	RECV op36  char=22  town=98  area=6  x=177 y=597 dir=5  opaque=(98, 3)
//	                     ^^^^^^ 要打的那个区域（带副本 4954）
//	                                          ^^^^^^^^^^^^^ 他**按下进图时站的格子**
//
// 也就是说界面上"作战区域 03"是格子 3，而 request.AreaID 是隔壁那个有怪的区域 6。
// 执行体跑起来时发起者已经被挪进 6 了，这时任何"按实时坐标重算队伍"的做法都会得出
// "这一格里只有他一个人" —— 真机就是这么变成"一个人进图"的。
type fiendWarEncounterPlan struct {
	// Area 是要打的那个区域（决定进哪张副本）。
	Area int
	// OriginArea 是发起者按下进图时所在的格子 —— **同伴必须站在这儿**，
	// 客户端自己的提示就是这句：「移动后，会与处于当前城镇的同伴一同开始战斗」。
	OriginArea int
	// Requester 是按下进图的人：他随后会站到 Area，队友留在 OriginArea。
	// 他同时也是这一场的驱动者 —— 官方口径「第一个进入新副本的玩家为队长」。
	Requester uint16
}

// runtimeRaidRosterLedBy 把 leader 挪到名册序第一位，其余顺序逐字保留。
//
// runtimeRaidGroupPartyState 拿 members[0] 当这支普通队伍的队长（UserID）与 0 号
// 座位，而 prepareRuntimePartyDungeonEntry 只在"驱动进图的 session 就是队伍队长"
// 时才会把队友搬进同一个副本（dungeon_navigation_runtime.go:753）。所以"谁驱动
// 这一场"必须排在最前面，否则队友一个都带不走。
//
// leader 不在 members 里时原样返回：调用方（超时空遭遇战）已经保证它在，
// 静默不动比在这里造一个不存在的成员安全。
func runtimeRaidRosterLedBy(members []runtimeRaidMemberState, leader uint16) []runtimeRaidMemberState {
	roster := append([]runtimeRaidMemberState(nil), members...)
	if leader == 0 || len(roster) < 2 || roster[0].CharID == leader {
		return roster
	}
	for index, member := range roster {
		if member.CharID != leader {
			continue
		}
		led := make([]runtimeRaidMemberState, 0, len(roster))
		led = append(led, member)
		led = append(led, roster[:index]...)
		led = append(led, roster[index+1:]...)
		return led
	}
	return roster
}

// runtimeRaidFiendWarAreaRoster 是超时空这一场进图的名单：**触发者 + 原格子里
// 实时站着的所有人**，按区域的 [MEMBER MAX] 封顶，**与小队编制无关**。
//
// 依据是客户端原生机制（2026-10-01 逆向确认，见
// 超时空_同区域进图_客户端逆向规格_20261001.md）：弹窗 sub_9AE390 只在频道类型 42
// 下出现，文案 dstr 75467「移动后，会与处于当前城镇的同伴一同开始战斗。确认要
// 移动吗？」，而弹窗分支只是暂停并返回 —— 客户端不组队、不收集成员，把这一格的
// 人一起带进副本完全是服务端的事。
//
// 三条取舍，都是刻意的：
//
//  1. **触发者永远第一个**：官方口径「第一个进入新副本的玩家为队长」，
//     runtimeRaidRosterLedBy 之后他就是临时队伍的队长（prepareRuntimePartyDungeonEntry
//     的守卫要求驱动者就是队伍队长，见那一段的注释）。
//  2. **只收"能进图"的人**：队友搬运有个硬约定 —— 一个人准备不好，整场进图会被
//     整个拒掉（"A party that cannot be prepared is REFUSED, never split"）。
//     2026-10-01 21:24 那次事故就是这条把所有人挡在门外。所以这里先把不满足条件的
//     人剔除并记一行日志，绝不让他们把触发者一起拖住。
//  3. **不按小队筛**：同格里站着的人不问编制，这正是"同格里有人却只有一个人进副本"
//     的根因（原先的名单是 squad.Members）。
//
// 位置一律以**实时坐标**为准（onlinePlayers 的 area 实例表），不用战场账本 ——
// 账本滞后是今天反复出事故的那一类判断。
func (s *Service) runtimeRaidFiendWarAreaRoster(
	session *gameSession,
	state runtimeRaidState,
	townID byte,
	area int,
	now time.Time,
) []runtimeRaidMemberState {
	if s == nil || session == nil || area < 2 || state.Lifecycle.FiendWar == nil {
		return nil
	}
	limit := 0
	if a, ok := state.Lifecycle.FiendWar.Config.Areas[area]; ok {
		limit = a.MemberMax
	}
	roster := make([]runtimeRaidMemberState, 0, raid.MaxAttackPartyMembers)
	seen := make(map[uint16]bool, raid.MaxAttackPartyMembers)
	// 触发者：他刚被 town_move.go 挪进目标区域，所以位置校验交给调用方的
	// origin/area 判定；这里只做"是不是本团成员 + 复活禁锢 + 名额"。
	add := func(charID uint16) bool {
		if charID == 0 || seen[charID] || (limit > 0 && len(roster) >= limit) {
			return false
		}
		member, ok := runtimeRaidFrozenMember(state, charID)
		if !ok {
			return false // 不是本团成员：不进图
		}
		if now.Before(state.Lifecycle.FiendWar.RecoveryUntil[charID]) {
			return false // 复活禁锢中
		}
		seen[charID] = true
		roster = append(roster, member)
		return true
	}
	add(session.selectedCharacterID)
	if s.onlinePlayers == nil {
		return roster
	}
	// 原格里**实时站着**的人。「当前城镇」在超时空就等于"当前区域"：每一格是独立
	// 的一张 map（Fiendwar_TownNN.map），而 area 实例表同时按 channel 与 RaidKey
	// 分区，所以别的频道/别的团的人不会串进来。
	peers := s.onlinePlayers.GetAreaInstancePlayers(
		townPresenceChannelID(session), townID, byte(area), state.RaidKey)
	extra := make([]runtimeRaidMemberState, 0, len(peers))
	for _, peer := range peers {
		if peer.CharacterID == 0 || peer.CharacterID == session.selectedCharacterID {
			continue
		}
		member, ok := runtimeRaidFrozenMember(state, peer.CharacterID)
		if !ok {
			continue
		}
		if reason := s.fiendWarEncounterPeerRefusal(member, townID, area); reason != "" {
			// 让开，但必须说出来：一个卡在切场景的人不该把整格一起拖住。
			s.logGameEvent(session, "game-fiendwar-area-peer-skipped",
				"raid_key", state.RaidKey,
				"char_id", peer.CharacterID,
				"area", area,
				"reason", reason,
				"source", "runtime_raid_fiendwar_area_roster")
			continue
		}
		extra = append(extra, member)
	}
	runtimeRaidSortMembers(extra)
	for _, member := range extra {
		if limit > 0 && len(roster) >= limit {
			s.logGameEvent(session, "game-fiendwar-area-peer-over-limit",
				"raid_key", state.RaidKey,
				"char_id", member.CharID,
				"area", area,
				"limit", limit,
				"roster", len(roster),
				"source", "runtime_raid_fiendwar_area_roster")
			break
		}
		add(member.CharID)
	}
	return roster
}

// runtimeFiendWarSquadPartyLeader 读这一支小队当前的队长标志（SquadPartyLeaders），
// 读不到就报 false。读也要持 raidMu：那张 map 是活的，战况表心跳、掉线移交和别的
// Party publish 都会写它。
func (s *Service) runtimeFiendWarSquadPartyLeader(raidKey uint32, squad byte) (uint16, bool) {
	if s == nil || squad == 0 {
		return 0, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil || state.SquadPartyLeaders == nil {
		return 0, false
	}
	leader, found := state.SquadPartyLeaders[squad]
	return leader, found && leader != 0
}

// restoreRuntimeFiendWarSquadPartyLeader 把小队队长标志放回操作员排的那个人，并且
// **只放回被这一场进图换掉的那一个**：值已经不是 driver，就说明期间有人正经改过队长
// （掉线移交、在攻坚队管理器里重新编队），那就不该被这条覆盖。
//
// 返回这次是否真的动了它，调用方只在真的发生时写日志 —— 这条例外永远不静默。
func (s *Service) restoreRuntimeFiendWarSquadPartyLeader(raidKey uint32, squad byte, driver, leader uint16) bool {
	if s == nil || squad == 0 || leader == 0 || driver == leader {
		return false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil || state.SquadPartyLeaders == nil || state.SquadPartyLeaders[squad] != driver {
		return false
	}
	state.SquadPartyLeaders[squad] = leader
	return true
}

// Called on the character mailbox. The board tick also covers enemies arriving
// at a stationary squad; packet delivery alone is not town-load confirmation.
func (s *Service) tryRuntimeFiendWarEncounter(session *gameSession, now time.Time) error {
	return s.tryRuntimeFiendWarEncounterPlanned(session, now, nil)
}

// tryRuntimeFiendWarEncounterPlanned 是遭遇战执行体。plan 为 nil 时是战况表 1 Hz 心跳
// 那一趟（"我站在一个有怪的区域里" → 就地开打）；非 nil 时是**一次移动触发的**那一趟，
// 目标区域与同伴名单都已由发起移动的那次调用算好（见 fiendWarEncounterPlan：
// op36 的 area 是要打的区域，发起者站的格子才是同伴的锚点）。
func (s *Service) tryRuntimeFiendWarEncounterPlanned(session *gameSession, now time.Time, plan *fiendWarEncounterPlan) error {
	if !isCurrentFiendWarRaidChannel(session) {
		return nil
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.FiendWar == nil {
		return nil
	}
	b := state.Lifecycle.FiendWar
	member, ok := runtimeRaidFrozenMember(state, session.selectedCharacterID)
	if !ok {
		return nil
	}
	// The native Fiend War scene does not expose the ordinary result exit.
	// Its existing board tick retries the full return on the character mailbox.
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime != nil {
		// 清完自动回城 —— **只看这一局对本人有没有结束，不看是谁**。
		//
		// 操作员 2026-10-01 定案：「不管是谁 谁是驱动者 谁收尾 这都不重要
		// 通关直接回城就是了；当然了 这些逻辑只适用于超时空」。
		//
		// 这里原来写的是 `runtime.raidDungeon.LifecycleToken == runtime.lifecycleToken`，
		// 那只对**开局的人**成立：超时空一个区域共用一条 run，而
		// prepareRuntimePartyDungeonEntry **先备队友** ⇒ 常常是队友开局、驱动者 adopt，
		// 驱动者手上那份 binding 带的是开局人的 token，永远不等于自己的本地 token。
		// 我第 4 轮试过用"攻坚账本里还留着这条 run"兜底 —— 那也是错的：通关处理本身
		// 就会把那条 run 从账本里删掉，等该回城时账本已经空了。
		//
		// 真机把两种兜底各打脸一次：23:38 开局人收尾失败（账本还在）⇒ 驱动者回了城
		// （"队长自动回城了 队员没回去"）；23:54 开局人收尾成功（账本被删）⇒ 同一个
		// 驱动者又回不去（"队员回了 队长没回去"）。凡是"谁开的、谁报的、账本还剩什么"
		// 这类判断都会再翻一次，所以全部拿掉。
		//
		// 剩下的三条都不是"谁"：我这份绑定指向的确实是此刻在打的这局攻坚
		//（RaidKey/MapID 对齐，见 runtimeFiendWarClearReturnBindingAccepts），
		// 以及"这一局对我已经结束并且结算做完了"（completedRuntimeRaidDungeonExitReady）。
		// 安东/卢克走不到这里（上面 isCurrentFiendWarRaidChannel + RaidMode 已经挡过）。
		ready := state.Started && state.EntryCostPaid && state.RaidMode == runtimeRaidOwnerFiendWar &&
			runtime.lifecycleToken != 0 && runtime.raidDungeon.RaidKey == state.RaidKey &&
			runtime.raidDungeon.Phase == state.Lifecycle.Phase &&
			runtimeFiendWarClearReturnBindingAccepts(runtime) &&
			completedRuntimeRaidDungeonExitReady(runtime, session.selectedCharacterID)
		if ready {
			// 清完一个区域后的自动回城由服务器驱动，玩家没有按任何东西：它不是
			// 「队内成员单独自己回城」，所以编制跟着小队留下。
			runtime.townReturnKeepsSquadLabel = true
		}
		session.dungeon.mu.Unlock()
		if !ready {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, session.selectedCharacterID)
		if err != nil {
			return err
		}
		// 超时空：BOSS 房（量子时空传送站）通关是**整场攻坚的收尾**，落点必须是待机区。
		//
		// 通用路径给的是"回到你刚才所在的作战区域"（原点快照，见 position_source=
		// current_exe_op35_session_scoped_luke_runtime_origin_snapshot）。普通作战区域
		// 有真实坐标（实测 area14 → 113,462），所以那条路一直是对的；但 BOSS 区域
		// 30/31 在快照里**没有坐标** —— 2026-10-01 16:06 真机 char_id=1 通关 4978 后
		// 服务端发的是 `town_id=98 area_id=30 position_x=0 position_y=0`，客户端拿不到
		// 可用落点，于是通关结算都发了、人也留在副本里不回城。
		//
		// 待机区坐标只有一个来源（PVF 等待室的 NPC），也就是撤退/掉线那条路用的
		// fiendWarWaitingAreaScene —— 这里复用它，不另造一套坐标。
		if area, found := b.AreaForDungeon(int(runtime.Dungeon.ID)); found && strings.HasSuffix(area.Kind, "boss") {
			transition, err = s.fiendWarWaitingRoomTransition(state, session.selectedCharacterID, transition)
			if err != nil {
				return err
			}
		}
		s.logGameEvent(session, "game-fiendwar-clear-auto-return", "raid_key", state.RaidKey,
			"dungeon_id", runtime.Dungeon.ID, "lifecycle_token", runtime.lifecycleToken,
			"town_id", transition.TownID, "area_id", transition.AreaID, "position_source", transition.PositionSource)
		// 账本里本人的区域要跟着落到**目的地那一格**：这一脚之后他站在原区域/待机区，
		// 而不是他刚被带进去那张副本所在的格子。不落就会被下一脚的相邻判定挡死
		// （真机 2026-10-02 14:20 礼白666，见 markRuntimeFiendWarMemberArea 的注释）。
		if markErr := s.markRuntimeFiendWarMemberArea(session.selectedCharacterID, int(transition.AreaID), "fiendwar_clear_auto_return"); markErr != nil {
			s.logGameEvent(session, "game-fiendwar-member-area-mark-failed",
				"char_id", session.selectedCharacterID, "area", transition.AreaID, "error", markErr)
		}
		// op132 是它自己的处理器，不持有 world lock，与既有 op132 调用点一致
		// (dungeon_return_town.go:352)。
		return s.sendCurrentCompletedDungeonReturnToTown(session, runtime, transition, 132, "fiendwar_clear_auto_return", false)
	}
	session.dungeon.mu.Unlock()
	// Which squad this member walks in with is resolved LIVE-first, with a defined
	// answer for the live/frozen divergence instead of the silent return nil this
	// used to be; see runtimeRaidFiendWarEncounterSquad for the measured deadlock.
	squad := runtimeRaidFiendWarEncounterSquad(state, session.selectedCharacterID)
	groupID := runtimeRaidNormalizedGroup(member.GroupIndex)
	if squad.Group != 0 {
		groupID = squad.Group
	}
	session.townMu.Lock()
	position := session.townPositionSnapshot
	session.townMu.Unlock()
	// 区域以**实时坐标**为准（客户端眼里的"我在哪一格"），战场账本只兜底。
	// 账本滞后时（真机 2026-10-01：人在 14、账本记着 10）按账本算会去开错副本，
	// 甚至因为 position.AreaID != area 直接静默放弃整场遭遇战。
	area := b.MemberArea(member.CharID, groupID)
	originArea := area
	switch {
	case plan != nil:
		// 移动触发的那一趟：目标是"要打的区域"，锚点是发起者按下进图时站的格子。
		area, originArea = plan.Area, plan.OriginArea
	case int(position.TownID) == b.Config.WaitingTown && int(position.AreaID) >= 2:
		if liveArea := int(position.AreaID); liveArea != area {
			s.logGameEvent(session, "game-fiendwar-encounter-area-from-live-position",
				"raid_key", state.RaidKey, "char_id", session.selectedCharacterID,
				"live_area", liveArea, "battle_area", area,
				"reason", "battle_member_area_lags_the_client_position",
				"source", "try_runtime_fiendwar_encounter")
			area = liveArea
		}
		originArea = area
	}
	// 战况表心跳(plan==nil)与计划进图的竞态收口 —— 见
	// fiendWarPlannedEncounterUntil 的注释。
	//
	// 只挡 plan==nil 这一路：计划那一趟(plan!=nil)正是被保护的对象，不能挡。
	// 挡在这里而不是更早：本人副本里"清完自动回城"那段在上面已经跑完，心跳即使
	// 让位也不会漏掉收尾。
	//
	// 真机 2026-10-01 本地 00:03:53（op36 53.743 / 心跳刻度 53.73 / 单人进图 53.790）：
	// 这一趟算出来的 originArea 是**目标区域 7**（计划那一脚刚把本人坐标推过去），
	// 格 4 里的 char 22 于是完全不在名单里，日志上一行拒绝都没有 —— 只留下
	// game-current-party-frame-op9-send party_id=4 slot_count=1 这条"临时队伍只有
	// 一个人"的铁证。现在这一趟会明着让位并记一行，不再静默单人开图。
	if plan == nil && s.runtimeFiendWarPlannedEncounterPending(session, now) {
		s.logGameEvent(session, "game-fiendwar-encounter-heartbeat-deferred",
			"raid_key", state.RaidKey, "char_id", session.selectedCharacterID,
			"area", area, "origin_area", originArea,
			"reason", "a_planned_move_encounter_is_in_flight_on_this_mailbox",
			"source", "try_runtime_fiendwar_encounter")
		return nil
	}
	if now.Before(b.RecoveryUntil[member.CharID]) {
		return nil
	}
	if !b.AreaOpen(area) || b.Areas[area].Group != 0 {
		session.fiendWarEncounterAt = time.Time{}
		return nil
	}
	if ready, _ := s.currentTownEnterSelectReady(session); !ready {
		return nil
	}
	if plan == nil && (int(position.TownID) != b.Config.WaitingTown || int(position.AreaID) != originArea) {
		return nil
	}
	// 进图名单 = 发起者的**编制小队**（squad 已经由 runtimeRaidFiendWarEncounterSquad
	// 解出）。这里不再按"附近的人"重算：操作员 2026-10-01 明确定案不做附近自动组队，
	// 暂时就按小队编制走。
	newEncounter := session.fiendWarEncounterRaidKey != state.RaidKey || session.fiendWarEncounterArea != position.AreaID || session.fiendWarEncounterAt.IsZero()
	if newEncounter {
		session.fiendWarEncounterRaidKey, session.fiendWarEncounterArea = state.RaidKey, position.AreaID
		session.fiendWarEncounterAt = now
	}
	if squad.Source == runtimeRaidFiendWarSquadSourceFrozen {
		// The live roster has no row for this member at all, so the frozen phase
		// roster answered instead. Kept loud. 注意：它只影响 battle 的 group 记法，
		// **不再**影响进图资格 —— 见下面名单那一段。
		s.logGameEvent(session, "game-fiendwar-encounter-squad-from-frozen-row",
			"raid_key", state.RaidKey,
			"char_id", session.selectedCharacterID,
			"area", area,
			"group", squad.Group,
			"reason", "member_absent_from_the_live_roster",
			"source", "try_runtime_fiendwar_encounter")
	}
	// 进图名单 = **触发者 + 原格子里实时站着的所有人**，按区域 [MEMBER MAX] 封顶，
	// **与小队编制无关**。
	//
	// 这是客户端原生机制，已逆向确认（2026-10-01）：
	//
	//	弹窗函数 sub_9AE390（VA 0x9AE390）第一件事就是 `cmp eax,0x2a` —— 频道类型
	//	42（超时空）才弹窗；文案 dstr 75467「移动后，会与处于当前城镇的同伴一同
	//	开始战斗。确认要移动吗？」。调用点 sub_1195060 里，弹窗分支
	//	`test al,al / jne 0x11951a8` 只是**暂停并返回**，不组任何包；玩家点"确认"
	//	之后走的是普通移动 sub_11936E0。
	//
	// 也就是说：客户端不组队、不做成员收集，**把这一格的人一起带进副本完全是
	// 服务端的事**。「当前城镇」在超时空就等于"当前区域"—— 每一格是独立的一张
	// map（map/GContents/180621_Fiendwar/Town/Fiendwar_TownNN.map）。
	//
	// 所以名单不再取 squad.Members：那正是"同格里站着别人、却只有一个人进副本"
	// 的根因。小队概念只剩两个用途 —— battle 的 group 记法，以及下面小队队长
	// 标志的保护。
	members := s.runtimeRaidFiendWarAreaRoster(session, state, position.TownID, originArea, now)
	if len(members) == 0 {
		return nil
	}
	// 每个人都必须**确实站在该站的地方**并且准备好进图：
	//   - 发起者已经挪进目标区域（他的 op36 已经被应用），所以查 Area；
	//   - 其余人留在发起者按下进图的那个格子里，所以查 OriginArea。
	// 这里同样以实时坐标为准：上面那句 `b.MemberArea(...) != area` 用的是战场账本，
	// 账本一滞后就会把整场遭遇战静默掐掉（2026-10-01 真机：人在 14、账本记着 10）。
	for _, member := range members {
		if now.Before(b.RecoveryUntil[member.CharID]) {
			return nil
		}
		want := originArea
		if plan != nil && member.CharID == plan.Requester {
			want = area
		}
		if reason := s.fiendWarEncounterPeerRefusal(member, position.TownID, want); reason != "" {
			s.logGameEvent(session, "game-fiendwar-encounter-peer-not-ready",
				"raid_key", state.RaidKey,
				"char_id", session.selectedCharacterID,
				"peer_id", member.CharID,
				"area", area,
				"origin_area", originArea,
				"want_area", want,
				"reason", reason,
				"source", "try_runtime_fiendwar_encounter")
			return nil
		}
		peer, online := s.onlineGameSession(member.CharID)
		if !online {
			return nil
		}
		if ready, _ := s.currentTownEnterSelectReady(peer); !ready {
			return nil
		}
		if _, bound := bindCurrentTownSelectorOrigin(peer); !bound {
			return nil
		}
	}
	// 这一场进图的**队长必须是把它驱动起来的那个人**（移动那一趟就是
	// plan.Requester；战况表心跳那一趟上面已经保证就是当前 session）。
	// 两件事同时依赖它：
	//
	//  1) 客户端侧：选中页归"选择的所有者"（队长）所有，队友是被队长带着走的；
	//  2) 服务端侧：prepareRuntimePartyDungeonEntry 的第一道守卫就是
	//     `state.UserID != leader.selectedCharacterID → return nil, nil`，
	//     而 RuntimePartySnapshot.StateFor 给**每个**成员的 UserID 都是
	//     snapshot.Leader（runtime_manager.go:652）。
	//
	// runtimeRaidGroupPartyState 原来把 UserID 定成 members[0]，也就是名册序第一个
	// （runtimeRaidSortMembers 按 SlotOrder 排）。只要按下进图的不是名册序第一，
	// 这支临时队伍就会被建出来、op11/op9/op153 也发出去，然后在上面那条守卫上
	// **静默**丢掉：队友一个都不搬，驱动者一个人进图。真机 2026-10-01 20:56:48
	// 正是如此 —— party_id=4 slot_count=2 发给了 char 1 与 char 20，紧接着只有
	// char 1 拿到 op29；日志里连 game-party-dungeon-entry-blocked 都没有，
	// 因为那趟 fan-out 根本没跑。20:56:11~13 那三条
	// `game-fiendwar-encounter-deferred reason=the_squad_leader_owns_the_encounter`
	// 是同一个"队长 ≠ 按进图的人"的另一面。
	//
	// 编制不动：这里重排的只是**这一支临时普通队伍**的名册序（它同时决定这支队伍的
	// 0 号座位），攻坚队/小队的名册与座位一个字节都不改。
	driver := session.selectedCharacterID
	if plan != nil && plan.Requester != 0 {
		driver = plan.Requester
	}
	roster := runtimeRaidRosterLedBy(members, driver)
	// 临时队伍的队长换了，但**攻坚队里那一支小队的队长标志不跟着换**：
	// replaceManagedRuntimeParty → publishRuntimePartySnapshot 会把刚发布的普通队伍的
	// 队长无条件镜像进 SquadPartyLeaders，而客户端每个成员的 队长标志 与战况表的
	// 队长按钮读的就是它（runtimeRaidMemberLeadsSquad）。进图这一场为了把队友带走，
	// 队长必须让给驱动者，可"谁是这支小队的队长"是操作员在攻坚队管理器里排的编制
	// 事实，不该因为"这次是谁按的进图"而改手。所以记下发布前的值，发布完立刻放回去。
	squadLeader, hadSquadLeader := s.runtimeFiendWarSquadPartyLeader(state.RaidKey, squad.Group)
	managed, ok := s.replaceManagedRuntimeParty(runtimeRaidGroupPartyState(state, roster))
	if !ok {
		return fmt.Errorf("fiendwar encounter party unavailable")
	}
	if hadSquadLeader && squadLeader != driver &&
		s.restoreRuntimeFiendWarSquadPartyLeader(state.RaidKey, squad.Group, driver, squadLeader) {
		s.logGameEvent(session, "game-fiendwar-squad-leader-restored",
			"raid_key", state.RaidKey,
			"squad", squad.Group,
			"driver", driver,
			"leader", squadLeader,
			"reason", "the_entry_party_is_led_by_the_driver_while_the_squad_keeps_its_own_leader")
	}
	request := dungeoncmd.SelectDungeonRequest{DungeonID: uint32(b.Config.Areas[area].Dungeon)}
	err := s.sendManagedRuntimePartySnapshots(managed)
	if err == nil {
		err = s.handleDungeonSelectUpper(session, encodeCurrentDungeonSelectRequest(request))
	}
	session.dungeon.mu.Lock()
	entered := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if !entered {
		for _, member := range members {
			if peer, online := s.onlineGameSession(member.CharID); online {
				if cleanupErr := s.clearSingleRaidOrdinaryPartyProjection(peer, "fiendwar_encounter_rejected"); cleanupErr != nil && err == nil {
					err = cleanupErr
				}
			}
		}
		// 兜底：队伍搬不动时，**至少让触发者自己进去**。
		//
		// 2026-10-01 21:24 那次事故就是 fan-out 失败把整场进图一起拒了，于是
		// "谁都进不去"——比不改还糟（不改时单人至少能进）。临时队伍刚在上面被
		// 清干净，所以这一趟走的是**无队伍的 select**，也就是改动前的老行为。
		// 最坏情况因此收敛回"一个人进"，而不是"一个人都进不去"。
		//
		// 只重试一次；成功就把 err 清掉，失败则保留原始错误。
		// 注意：handleDungeonSelectUpper 自己会取 session.dungeon.mu，
		// 所以这里**不能**持锁调用。
		retryErr := s.handleDungeonSelectUpper(session, encodeCurrentDungeonSelectRequest(request))
		session.dungeon.mu.Lock()
		entered = session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		if entered {
			err = nil
		} else if retryErr != nil {
			err = retryErr
		}
		s.logGameEvent(session, "game-fiendwar-encounter-solo-fallback",
			"raid_key", state.RaidKey, "area", area,
			"dungeon_id", request.DungeonID,
			"entered", entered, "error", retryErr,
			"reason", "the_party_fanout_failed_so_the_trigger_member_enters_alone")
		session.fiendWarEncounterAt = now
	}
	s.logGameEvent(session, "game-fiendwar-encounter-entry", "raid_key", state.RaidKey, "area", area, "dungeon_id", request.DungeonID, "entered", entered, "error", err)
	if entered {
		// 整支队伍确实进去了 —— 把**每个人**在战场账本里的区域补成这一格。
		//
		// 队友是被 prepareRuntimePartyDungeonEntry 备好、由驱动者按进图那一脚一起
		// 搬进去的，**他们没有走过 op36 那一步**，所以 battle.MemberAreas 里他们
		// 仍然停在"进图之前站的那一格"。战况表的 op1455 成员行取的就是 MemberArea，
		// 账本一滞后，客户端眼里这一格里就只剩驱动者一个人 ——「简易战况表」上该侧
		// 于是只画得出一个编制徽章。
		//
		// 真机 2026-10-02 13:50（raid 12058625）：
		//   上侧 c1 走进 30、c20 被带进 4978，账本里 c20 仍停在 28；
		//   下侧 c22 走进 31、c21 被带进 4953，账本里 c21 仍停在 29。
		//   操作员看到的正是「黑暗有 1、2 队进了只显示 1 队（队长编制 1）；
		//   纯白 3、4 队进了只显示 4 队（队长编制 4）」。
		//
		// 官方资料图里那一行徽章是**这一侧房间里的所有编制**（1/3/4 与 7/5/6/8），
		// 所以这一笔必须按"所有参与者"补，而不是只补驱动者。
		//
		// **但"参与者"必须是真的进去了的人**：生产路径上 entered 也可以由"兜底单人进图"
		// 成立（队友那一趟 fan-out 失败时只有 trigger 者进去），那时候把整队都记进这一格
		// 就是**反向**的账本漂移 —— 人在原格、账本写新格，下一脚移动又会被"旧格子不相邻"
		// 挡死（8 客户端仿真 TestFiendWarSimSharedCellEntryTakesEveryone 抓到的就是这个）。
		// 判据：他手上确实握着这一局这条 run。
		entrants := make([]runtimeRaidMemberState, 0, len(members))
		for _, member := range members {
			peer, online := s.onlineGameSession(member.CharID)
			if !online || peer == nil {
				continue
			}
			peer.dungeon.mu.Lock()
			bound := peer.dungeon.runtime != nil && peer.dungeon.runtime.raidDungeon.RaidKey == state.RaidKey
			peer.dungeon.mu.Unlock()
			if bound {
				entrants = append(entrants, member)
			}
		}
		if markErr := s.markRuntimeFiendWarPartyInArea(state.RaidKey, entrants, area, "fiendwar_encounter_entry"); markErr != nil {
			s.logGameEvent(session, "game-fiendwar-encounter-area-mark-failed",
				"raid_key", state.RaidKey, "area", area, "error", markErr)
		}
	}
	return err
}

// markRuntimeFiendWarMemberArea 把某人战场账本里的区域改成 area。
//
// 它和 markRuntimeFiendWarPartyInArea 是一对"进/出"：
//   - **进**：整队被带进某一格（key 在 encounter entry）；进的那一格是**副本所在**的格子；
//   - **出**：他打完/撤退/被送回去时，落点是原区域或待机区 —— 账本必须跟着落到**那一格**。
//
// 不跟着落就会卡死，而且卡的是"走不动"：
// `MoveMember` 的相邻判定用的是账本里的个人区域（module 层
// `b.GroupAreas[group] = b.MemberArea(character, group)` → `Move`），账本一滞后，
// 玩家点隔壁那一格会被按"旧格子与目标不相邻"拒掉。
//
// 真机 2026-10-02 14:20（礼白666，raid 12058625）：
//
//	06:18:58 game-fiendwar-party-area-marked area=16 char_ids=1   ← 他被队友带进 area16 的副本
//	06:19:05 game-dungeon-full-town-route-send ... area_id=11     ← 打完回到 11（他的原区域）
//	06:20:59 game-fiendwar-move-refused requested_area_id=7 keep_area_id=11
//	         reason=fiendwar areas are not adjacent
//	而 area 11 的 next = [7 10 12 16] **明明含 7** —— 因为判定用的是账本里的 16。
func (s *Service) markRuntimeFiendWarMemberArea(characterID uint16, area int, source string) error {
	if s == nil || characterID == 0 || area < 2 {
		return nil
	}
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.Lifecycle.FiendWar == nil {
		s.raidMu.Unlock()
		return nil
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		s.raidMu.Unlock()
		return nil
	}
	group := runtimeRaidNormalizedGroup(member.GroupIndex)
	if state.Lifecycle.FiendWar.MemberArea(characterID, group) == area {
		s.raidMu.Unlock()
		return nil
	}
	battle := state.Lifecycle.FiendWar.Clone()
	if battle.MemberAreas == nil {
		battle.MemberAreas = make(map[uint16]int)
	}
	battle.MemberAreas[characterID] = area
	peerElsewhere := slices.ContainsFunc(state.FrozenMembers, func(peer runtimeRaidMemberState) bool {
		return peer.CharID != characterID && runtimeRaidNormalizedGroup(peer.GroupIndex) == group &&
			battle.MemberArea(peer.CharID, group) != area
	})
	if !peerElsewhere {
		battle.GroupAreas[group] = area
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Revision++
	raidKeyCopy := state.RaidKey
	raidPhase := state.Lifecycle.Phase
	s.raidMu.Unlock()
	s.logPacketEvent("game-fiendwar-member-area-marked",
		"raid_key", raidKeyCopy,
		"raid_phase", raidPhase,
		"char_id", characterID,
		"area", area,
		"source", source)
	return nil
}

// syncRuntimeFiendWarMemberAreaFromLivePosition 把"这个人现在实际站在哪一格"拉回战场账本，
// 并返回更新后的 state（没变就原样返回）。
//
// 这是移动路径上的**兜底**：账本永远可能有滞后（被带进副本、重连、服务端搬运……），
// 而相邻判定读的就是账本。与遭遇战那条路同口径（live-first，见
// game-fiendwar-encounter-area-from-live-position）。
func (s *Service) syncRuntimeFiendWarMemberAreaFromLivePosition(characterID uint16, liveArea int, source string) runtimeRaidState {
	if s == nil || characterID == 0 || liveArea < 2 {
		return runtimeRaidState{}
	}
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.Lifecycle.FiendWar == nil {
		s.raidMu.Unlock()
		return runtimeRaidState{}
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		s.raidMu.Unlock()
		return runtimeRaidState{}
	}
	group := runtimeRaidNormalizedGroup(member.GroupIndex)
	ledger := state.Lifecycle.FiendWar.MemberArea(characterID, group)
	if ledger == liveArea {
		ready := cloneRuntimeRaidState(*state)
		s.raidMu.Unlock()
		return ready
	}
	battle := state.Lifecycle.FiendWar.Clone()
	if battle.MemberAreas == nil {
		battle.MemberAreas = make(map[uint16]int)
	}
	battle.MemberAreas[characterID] = liveArea
	peerElsewhere := slices.ContainsFunc(state.FrozenMembers, func(peer runtimeRaidMemberState) bool {
		return peer.CharID != characterID && runtimeRaidNormalizedGroup(peer.GroupIndex) == group &&
			battle.MemberArea(peer.CharID, group) != liveArea
	})
	if !peerElsewhere {
		battle.GroupAreas[group] = liveArea
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Revision++
	ready := cloneRuntimeRaidState(*state)
	raidKeyCopy := state.RaidKey
	s.raidMu.Unlock()
	s.logGameEvent(nil, "game-fiendwar-member-area-from-live-position",
		"raid_key", raidKeyCopy,
		"char_id", characterID,
		"live_area", liveArea,
		"battle_area", ledger,
		"reason", "battle_member_area_lags_the_client_position",
		"source", source)
	return ready
}

// markRuntimeFiendWarPartyInArea 把"这支临时队伍刚被整体带进 area 这一格"记进战场账本。
//
// 与 runtimeFiendWarRejoinLocation（重连回到待机区那一笔）同形：本人一格、
// 他所在编制也指向这一格（只有当他那一队没有别人还留在原格时才动编制）。
func (s *Service) markRuntimeFiendWarPartyInArea(raidKey uint32, members []runtimeRaidMemberState, area int, source string) error {
	if s == nil || raidKey == 0 || area <= 0 || len(members) == 0 {
		return nil
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.Lifecycle.FiendWar == nil ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		s.raidMu.Unlock()
		return nil
	}
	battle := state.Lifecycle.FiendWar.Clone()
	if battle.MemberAreas == nil {
		battle.MemberAreas = make(map[uint16]int)
	}
	group := func(member runtimeRaidMemberState) byte {
		return runtimeRaidNormalizedGroup(member.GroupIndex)
	}
	moved := make([]uint16, 0, len(members))
	for _, member := range members {
		if _, ok := runtimeRaidFrozenMember(*state, member.CharID); !ok {
			continue
		}
		if battle.MemberArea(member.CharID, group(member)) == area {
			continue
		}
		battle.MemberAreas[member.CharID] = area
		moved = append(moved, member.CharID)
	}
	if len(moved) == 0 {
		s.raidMu.Unlock()
		return nil
	}
	// 编制指针只在"这一队没有别人还留在别处"时跟着走（同重连那一笔）。
	for _, member := range members {
		if !slices.Contains(moved, member.CharID) {
			continue
		}
		groupID := group(member)
		peersElsewhere := slices.ContainsFunc(state.FrozenMembers, func(peer runtimeRaidMemberState) bool {
			return peer.CharID != member.CharID && group(peer) == groupID && battle.MemberArea(peer.CharID, groupID) != area
		})
		if !peersElsewhere {
			battle.GroupAreas[groupID] = area
		}
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Revision++
	raidKeyCopy := state.RaidKey
	raidPhase := state.Lifecycle.Phase
	s.raidMu.Unlock()
	marked := make([]string, 0, len(moved))
	for _, charID := range moved {
		marked = append(marked, strconv.Itoa(int(charID)))
	}
	s.logPacketEvent("game-fiendwar-party-area-marked",
		"raid_key", raidKeyCopy,
		"raid_phase", raidPhase,
		"area", area,
		"char_ids", strings.Join(marked, ","),
		"source", source,
		"reason", "the_party_was_carried_into_this_area_without_walking_in")
	return nil
}

// fiendWarMoveRefusalNotice 把一次被拒的移动翻译成能给玩家看的一句话。
func fiendWarMoveRefusalNotice(reason string) string {
	switch {
	case strings.Contains(reason, "member limit"):
		return "这个区域已经满员了（每个区域最多 4 人）。"
	case strings.Contains(reason, "recovering"):
		return "你还在复活禁锢中，暂时无法移动。"
	case strings.Contains(reason, "boss gate"):
		return "量子时空传送站尚未激活：先打掉前哨基地与兵站本营。"
	case strings.Contains(reason, "not adjacent"):
		return "这个区域与你现在的位置不相邻，过不去。"
	case strings.Contains(reason, "group inactive"):
		return currentFiendWarSquadlessNotice
	default:
		return "现在无法移动到该区域。"
	}
}

// refuseRuntimeFiendWarMoveLocked 是"拒绝也要回话"的收口。
//
// 血泪教训（2026-10-01 真机，礼白666）：超时空里客户端**自己**允许玩家走到任何相邻
// 格子，服务端一旦拒绝而**一个包都不发**，客户端就永远停在"正在进入"上 —— 玩家看到
// 的就是被卡死。所以任何一条拒绝路径都必须：
//
//  1. 用 op0x16(坐标)+op0x17(区域) 把客户端拽回服务端认定的位置（宁可按老位置弹回，
//     也不能让客户端等一个永远不来的场景切换）；
//  2. 给一句人话提示，说明为什么过不去。
//
// 调用者持 session.townMu（与 town_move.go 的两处拒绝点一致）。
func (s *Service) refuseRuntimeFiendWarMoveLocked(session *gameSession, request currentTownSetUserAreaRequest, reason string) {
	if session == nil {
		return
	}
	position := session.townPositionSnapshot
	s.logGameEvent(session, "game-fiendwar-move-refused",
		"char_id", session.selectedCharacterID,
		"requested_town_id", request.TownID, "requested_area_id", request.AreaID,
		"keep_town_id", position.TownID, "keep_area_id", position.AreaID,
		"position_valid", position.PositionValid,
		"reason", reason, "source", "fiendwar_move_refusal_answered")
	if position.TownID == 0 || !position.PositionValid {
		// 连"他站在哪"都不知道就没法弹回去；日志留痕，至少能查。
		return
	}
	row := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(session.selectedCharacterID),
		Value1:              position.PositionX,
		Value2:              position.PositionY,
		Value3:              position.MovementCode,
	}
	if err := s.sendCurrentTownLocationNotifications(
		session, session.selectedCharacterID, position.TownID, position.AreaID,
		row, "fiendwar_move_refused"); err != nil {
		s.logGameEvent(session, "game-fiendwar-move-refused-answer-failed",
			"char_id", session.selectedCharacterID, "error", err)
		return
	}
	_ = s.sendNotice(session, noticeRequest{
		Surface: noticeSurfaceSystemMagenta,
		Text:    fiendWarMoveRefusalNotice(reason),
		Source:  "fiendwar_move_refused",
	})
}

// fiendWarCompletedDungeonExitReady 报告"这个人现在就在超时空副本里，而且这一场
// 攻坚已经打完" —— 这时"返回城镇"必须放行，结算由服务端发还是客户端自己放都行。
//
// 真机 2026-10-01 20:35（9999）：他那一场走的是 dungeon_boss_die_settlement.go 里
// 的 native 路线（game-dungeon-native-raid-settlement-owned，服务端一个结算包都没发），
// 于是 settlementClearRewardSent 永远是 false → 连按十几次返回城镇全部被
// game-dungeon-card-exit-blocked reason=settlement_result_not_ready 挡掉 —— 死按钮。
func (s *Service) fiendWarCompletedDungeonExitReady(session *gameSession, runtime *runtimeDungeonState) bool {
	if session == nil || runtime == nil || runtime.raidDungeon.RaidKey == 0 {
		return false
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || state.RaidMode != runtimeRaidOwnerFiendWar || state.Lifecycle.FiendWar == nil {
		return false
	}
	if state.Lifecycle.Status == runtimeRaidLifecycleAwaitingMovie ||
		state.Lifecycle.Status == runtimeRaidLifecycleAwaitingRewards ||
		state.Lifecycle.Status == runtimeRaidLifecycleStandby ||
		state.Lifecycle.Status == runtimeRaidLifecycleFinished {
		return true
	}
	// 阶段目标完成 = 罗什已死 = 整场收尾（[PHASE MAX] 1）。
	return runtimeRaidPhaseObjectivesComplete(state, state.Lifecycle.Phase)
}

func (s *Service) moveRuntimeFiendWarGroup(characterID uint16, target int) (runtimeRaidState, error) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.Lifecycle.FiendWar == nil || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return runtimeRaidState{}, errRuntimeRaidNotStarted
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		return runtimeRaidState{}, errRuntimeRaidNotMember
	}
	if state.Lifecycle.FiendWar == nil {
		// 超时空：区域是共享战场，一个人物理上只站在一个区域里，"一支小队一次只打
		// 一个门"这条占位规则不适用于它（后到的人要能走进正在打的区域一起打）。
		// 安东/卢克逐字保留。
		if _, occupied := state.Lifecycle.GroupOccupancy[runtimeRaidSeatFor(member)]; occupied {
			return runtimeRaidState{}, errRuntimeRaidGroupOccupied
		}
	}
	battle := state.Lifecycle.FiendWar.Clone()
	if battle.MemberAreas == nil {
		battle.MemberAreas = make(map[uint16]int)
	}
	for _, peer := range state.FrozenMembers {
		if _, exists := battle.MemberAreas[peer.CharID]; !exists {
			battle.MemberAreas[peer.CharID] = battle.MemberArea(peer.CharID, runtimeRaidNormalizedGroup(peer.GroupIndex))
		}
	}
	group := runtimeRaidNormalizedGroup(member.GroupIndex)
	if err := battle.MoveMember(characterID, group, target, s.gameplayNow()); err != nil {
		return runtimeRaidState{}, err
	}
	battle.GroupAreas[group] = target
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}

func (s *Service) rollbackRuntimeFiendWarMove(before runtimeRaidState, characterID uint16, target int) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[before.RaidKey]
	if state == nil || state.StartAttempt != before.StartAttempt || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || before.Lifecycle.FiendWar == nil || state.Lifecycle.FiendWar == nil {
		return
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		return
	}
	group := runtimeRaidNormalizedGroup(member.GroupIndex)
	battle := state.Lifecycle.FiendWar.Clone()
	if battle.MemberArea(characterID, group) != target || battle.RecoveryUntil[characterID] != before.Lifecycle.FiendWar.RecoveryUntil[characterID] {
		return
	}
	battle.MemberAreas[characterID] = before.Lifecycle.FiendWar.MemberArea(characterID, group)
	// A failed write must not roll back another member's movement or a board tick.
	if battle.GroupAreas[group] == target {
		peerAtTarget := false
		for _, peer := range state.FrozenMembers {
			if peer.CharID != characterID && runtimeRaidNormalizedGroup(peer.GroupIndex) == group && battle.MemberArea(peer.CharID, group) == target {
				peerAtTarget = true
			}
		}
		if !peerAtTarget {
			battle.GroupAreas[group] = before.Lifecycle.FiendWar.GroupAreas[group]
		}
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Revision++
}

func (s *Service) handleRuntimeFiendWarBoss(session *gameSession, body []byte) error {
	if len(body) != 8 {
		return nil
	}
	kind, value := int(binary.LittleEndian.Uint32(body[:4])), int(binary.LittleEndian.Uint32(body[4:]))
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	// **不能要求 runtime.raidDungeon.loadedNotified。**
	//
	// 一个区域共用一条 run，而 run 上的 token 是**开局人**的；adopt 进来的成员
	// （往往就是**队长** —— prepareRuntimePartyDungeonEntry 先备队友）自己那份绑定
	// 带着开局人的 token、本地 token 是另一个，于是
	// notifyRuntimeRaidDungeonLifecycleLocked 走"加入者"分支：**`loaded` 直接跳过**，
	// 只有 `completed` 才在本地留一笔。也就是说**他的 loadedNotified 永远是 false**。
	//
	// 真机 2026-10-02 12:34~12:38（raid 12058625，分片 012，两两进罗什）：
	//   4978 的 run 由 char 20 开（token=11）→ 队长 char 1 adopt
	//   4953 的 run 由 char 21 开（token=13）→ 队长 char 22 adopt
	//   lifecycle 行里只有这两个**队员**的 selected/loaded，两个队长一条都没有；
	//   而 kind=1（机制）上报**只有队长在发**（队员只发 kind=0 血条心跳）——
	//   于是每一条机制上报都在这里被静默丢掉：机制条停在 1111、两侧 state 停在 5，
	//   客户端反复刷同一颗球。
	//   排查时"rejected 0 / dropped 0"正是因为这个出口既不记日志也不说话。
	//
	// 真正该问的是"**这一场对他还在跑吗**"：completed/abandoned 两个标记足够了 ——
	// 加上下面账本里那条 run 的存在性（runtimeRaidDungeonLifecycleReportAccepted），
	// 已经能保证他确实在这场攻坚的这个副本里。
	if runtime == nil || runtime.raidDungeon.RaidKey == 0 || runtime.raidDungeon.completedNotified || runtime.raidDungeon.abandonedNotified {
		if runtime != nil {
			s.logGameEvent(session, "game-fiendwar-boss-report-dropped",
				"raid_key", runtime.raidDungeon.RaidKey,
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Request.DungeonID,
				"kind", kind,
				"value", value,
				"loaded_notified", runtime.raidDungeon.loadedNotified,
				"completed_notified", runtime.raidDungeon.completedNotified,
				"abandoned_notified", runtime.raidDungeon.abandonedNotified,
				"reason", "no_live_raid_dungeon_binding")
		}
		return nil
	}
	if owner := runtime.partyDungeonOwner; owner != nil {
		owner.mu.Lock()
		host := !owner.retired && owner.leaderID == session.selectedCharacterID
		owner.mu.Unlock()
		_, current := owner.member(session.selectedCharacterID, session.characterGeneration)
		if !current {
			// 报告者必须**此刻确实在这支临时队伍里**（掉线/换人之后的残留会话不算）。
			// 原来这里是静默 return —— "谁被允许报机制"在日志上完全不可见。
			s.logGameEvent(session, "game-fiendwar-boss-report-dropped",
				"char_id", session.selectedCharacterID,
				"reason", "reporter_is_not_a_current_party_member",
				"party_host", host,
				"member_current", current)
			return nil
		}
		if !host {
			// **队员也能报。** 旧代码在这里要求"只有队长能报"，而机制上报天然幂等
			// （kind=1 必须 value == 这一侧当前的 boss state、且那颗球还在共享池里，
			//   打掉之后这一侧立刻被分到下一颗，同一条重复报必被 stale 拒掉），
			// 所以按人区分没有任何好处 —— 只会留下"队员打掉那颗球、上报被丢掉、
			// 球永远不切换"这种坑。操作员 2026-10-02 的顾虑原话：
			// 「只有队长能报是什么意思？队员走机制就要卡住？」
			//
			// 真机那一局（11:49~11:58）恰好是队长在报（char 1 / char 20），队员
			// （char 21 / char 22）只发 HP 心跳，所以没暴露出来 —— 那是运气不是保证。
			// 现在照样收下，只记一行，便于回查是谁报的。
			s.logGameEvent(session, "game-fiendwar-boss-report-from-a-non-host",
				"char_id", session.selectedCharacterID,
				"reason", "party_members_may_report_the_mechanic_too",
				"kind", kind,
				"value", value)
		}
	}
	s.raidMu.Lock()
	state := s.raids[runtime.raidDungeon.RaidKey]
	if state == nil || state.Lifecycle.FiendWar == nil || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		// 同样是**静默**出口之一，而且是最可疑的那一个：机制上报到得了这里，
		// 却因为"这一局不是超时空的进行中阶段"被丢掉，日志上什么都不留。
		lifecycleStatus := runtimeRaidLifecycleStandby
		fiendWarPresent := false
		raidKey := runtime.raidDungeon.RaidKey
		if state != nil {
			lifecycleStatus = state.Lifecycle.Status
			fiendWarPresent = state.Lifecycle.FiendWar != nil
		}
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-fiendwar-boss-report-dropped",
			"raid_key", raidKey,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Request.DungeonID,
			"kind", kind,
			"value", value,
			"raid_state_present", state != nil,
			"fiendwar_present", fiendWarPresent,
			"lifecycle_status", int(lifecycleStatus),
			"reason", "raid_is_not_in_an_active_fiendwar_phase")
		return nil
	}
	// 本服务端的一次区域尝试按 (副本, 座位) 记账。**报告者既不必是开局那个人，也不必是队长**
	// —— 只要他此刻确实在这支临时队伍里（上面那句），并且这次报告对得上账本里的那条 run。
	//
	// **判据不能要求「本人本地 token == run 的 token」**：一个区域共用一条 run，
	// run 上的 token 是**开局人**的，而队长手上那份绑定带的正是开局人的 token、
	// 他自己的本地 token 是另一个 —— 因为 prepareRuntimePartyDungeonEntry **先备队友**，
	// 开局人常常就是队友。于是"队长 == 唯一能报的人"与"token 必须相等"一叠加，
	// **每一条 BOSS 机制上报都被静默丢掉**：
	//
	//	真机 2026-10-02 11:49:36~11:58:21（raid 12058625，分片 010，两两进罗什）
	//	  4978（上）：run 由 char 21 开（token=11），队长 char 1 adopt；
	//	  4953（下）：run 由 char 22 开（token=13），队长 char 20 adopt；
	//	  两侧客户端**都在**报 kind=1 value=5（char 1 六条、char 20 六条），
	//	  服务端一条 rejected 都没有 —— 因为这里是静默 return。
	//	  ⇒ 机制条停在 1111、两侧 state 停在 5 ⇒ 客户端（state=5）**反复刷同一颗球**
	//	    —— 操作员看到的「一直循环第一个球 不切换」。
	//
	// 现在与生命周期那条路同口径（runtimeRaidDungeonLifecycleReportAccepted）：
	// run 在账本里、属于本阶段、属于这一张地图就接受，谁报的都算。
	member, ok := runtimeRaidFrozenMember(*state, session.selectedCharacterID)
	if !ok {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-fiendwar-boss-report-dropped",
			"raid_key", state.RaidKey,
			"char_id", session.selectedCharacterID,
			"reason", "reporter_is_not_a_frozen_raid_member")
		return nil
	}
	key := runtimeRaidDungeonRunKey{MapID: int(runtime.Request.DungeonID), Seat: runtimeRaidDungeonSeatFor(*state, member, int(runtime.Request.DungeonID))}
	progress, exists := state.Lifecycle.Dungeons[key]
	raidKey := state.RaidKey
	phase := state.Lifecycle.Phase
	if !runtimeRaidDungeonLifecycleReportAccepted(state, key, progress, exists,
		runtimeRaidDungeonLifecycleEvent{
			Kind:           runtimeRaidDungeonLoaded,
			RaidKey:        raidKey,
			Phase:          phase,
			MapID:          key.MapID,
			LifecycleToken: runtime.lifecycleToken,
			CharacterID:    session.selectedCharacterID,
		}) {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-fiendwar-boss-report-dropped",
			"raid_key", raidKey,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Request.DungeonID,
			"raid_map_id", key.MapID,
			"run_seat_group", key.Seat.Group,
			"run_exists", exists,
			"run_lifecycle_token", progress.LifecycleToken,
			"local_lifecycle_token", runtime.lifecycleToken,
			"kind", kind,
			"value", value,
			"reason", "no_run_in_the_ledger_for_this_map_and_phase")
		return nil
	}
	battle := state.Lifecycle.FiendWar.Clone()
	// 观测：报之前这一侧的 BOSS 状态、以及报完之后两侧状态/机制条/两侧标志位。
	// 真机排查"球不切换"时缺的就是这一组数：rejected 0、dropped 0，但机制没动，
	// 只能靠猜。现在每一条上报都留一行。
	area, areaFound := state.Lifecycle.FiendWar.AreaForDungeon(int(runtime.Request.DungeonID))
	side, stateBefore := -1, -1
	if areaFound {
		stateBefore = state.Lifecycle.FiendWar.Value(area.Kind + " state")
		if area.Kind == "bot boss" {
			side = 1
		} else {
			side = 0
		}
	}
	err := battle.BossReport(int(runtime.Request.DungeonID), kind, value, time.Now().UTC())
	if err != nil {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-fiendwar-boss-report-rejected",
			"kind", kind, "value", value,
			"raid_key", raidKey,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Request.DungeonID,
			"area", area.Index, "area_kind", area.Kind, "side", side,
			"boss_state_before", stateBefore,
			"pattern_state", state.Lifecycle.FiendWar.Value("boss pattern state"),
			"error", err)
		return nil
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Revision++
	snapshot := cloneRuntimeRaidState(*state)
	topAfter := battle.Value("top boss state")
	botAfter := battle.Value("bot boss state")
	patternAfter := battle.Value("boss pattern state")
	windowTop, windowBot := battle.BossWindowDone[0], battle.BossWindowDone[1]
	s.raidMu.Unlock()
	s.logGameEvent(session, "game-fiendwar-boss-report-accepted",
		"raid_key", raidKey,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Request.DungeonID,
		"kind", kind,
		"value", value,
		"area", area.Index,
		"area_kind", area.Kind,
		"side", side,
		"boss_state_before", stateBefore,
		"top_boss_state", topAfter,
		"bot_boss_state", botAfter,
		"pattern_state", patternAfter,
		"window_done_top", windowTop,
		"window_done_bot", windowBot)
	return s.broadcastRuntimeFiendWarBoard(snapshot, false, time.Now().UTC())
}

func (s *Service) runtimeFiendWarPartyBinding(session *gameSession, runtime *runtimeDungeonState) (runtimeRaidDungeonBinding, bool) {
	if !isCurrentFiendWarRaidChannel(session) || runtime.partyDungeonOwner == nil {
		return runtimeRaidDungeonBinding{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(session.selectedCharacterID)
	if state == nil || !state.Started || !state.EntryCostPaid || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return runtimeRaidDungeonBinding{}, false
	}
	member, ok := runtimeRaidFrozenMember(*state, session.selectedCharacterID)
	if !ok {
		return runtimeRaidDungeonBinding{}, false
	}
	seat := runtimeRaidSeatFor(member)
	key := runtimeRaidDungeonRunKey{MapID: int(runtime.Request.DungeonID), Seat: seat}
	progress, ok := state.Lifecycle.Dungeons[key]
	if !ok || progress.Seat != seat {
		return runtimeRaidDungeonBinding{}, false
	}
	return runtimeRaidDungeonBinding{RaidKey: state.RaidKey, Phase: progress.Phase, MapID: progress.MapID, LifecycleToken: runtime.lifecycleToken}, true
}

func (s *Service) notifyRuntimeFiendWarMapLoaded(session *gameSession) error {
	if !isCurrentFiendWarRaidChannel(session) {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Room == nil || !runtime.raidDungeon.loadedNotified {
		return nil
	}
	mapID := int(runtime.Room.mapID)
	s.raidMu.Lock()
	state := s.raids[runtime.raidDungeon.RaidKey]
	if state == nil || state.Lifecycle.FiendWar == nil || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		s.raidMu.Unlock()
		return nil
	}
	a, ok := state.Lifecycle.FiendWar.AreaForDungeon(int(runtime.Request.DungeonID))
	if !ok || state.Lifecycle.FiendWar.Value(a.Kind+" state") != 2 {
		s.raidMu.Unlock()
		return nil
	}
	battle := state.Lifecycle.FiendWar.Clone()
	err := battle.Apply(raid.FiendWarEvent{Kind: "on enter map", ID: mapID}, time.Now().UTC())
	if err != nil {
		s.raidMu.Unlock()
		return err
	}
	if battle.Single != nil && a.Index == battle.Single.BossArea && battle.Value(a.Kind+" state") == 4 {
		battle.StartSingleBossPattern(a.Index)
	}
	state.Lifecycle.FiendWar = battle
	syncRuntimeFiendWarObjectives(state)
	state.Lifecycle.Revision++
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()
	return s.broadcastRuntimeFiendWarBoard(snapshot, false, time.Now().UTC())
}

// notifyRuntimeFiendWarMapEnteredLocked 是超时空的 [ON ENTER MAP] 的**房间切换**那一半。
//
// 为什么必须有它（2026-10-02 真机，两两进罗什）：
//
// 罗什那一侧的四颗星球**不是服务端刷出来的怪**，是罗什自己的动作动画
// (`Attack_Summon_Crystal/Fire/Water/Wind_Planet` → `cosmofiend_planet_*.ani`)，
// 客户端放不放它们，看的是这一侧 BOSS 的机制状态；而 PVF 的状态机是：
//
//	[ON ENTER DUNGEON] 4978      → top boss state = 2   （还没进机制）
//	[ON ENTER MAP]     306000463 → top boss state = 5   （四颗星球出场）
//	客户端每打掉一颗报一条 kind=1 → 5→6→7→8 → 9（伤害窗口打开）→ BOSS 死 → 10
//
// 上面那个 notifyRuntimeFiendWarMapLoaded 只挂在**副本 finish-loading** 上，而那一刻
// 人还在 0:0 起始房（map 306000461 / 306000458）——`on enter map` 带的是**起始地图**，
// PVF 那条规则是**按 BOSS 那张地图**（306000463 / 306000460）认的，于是什么都没发生；
// 玩家随后用 op45 走进 BOSS 房时，**没有任何代码再触发 `on enter map`**：
// state 永远停在 2，客户端看到 2 就不放球、也不开伤害窗口。
//
// 真机 2026-10-02 10:27:51~10:29:10 两次罗什的形状正是如此：四个球一个都不出现，
// 罗什不给打，62 条 op1511 **全是** kind=0 value=100（客户端压根没进机制阶段）。
// 旁证：同一天早些时候那次（10-01 04:53:47 / 04:54:25）确实对 306000463 / 306000460
// 触发过 `on enter map`（所以那一次 state 到了 5）——差别就在这个钩子，
// 它是**确定性缺陷，不是竞态**。
//
// 位置与卢克逐字对应（dungeon_navigation_runtime.go 的
// observeRuntimeRaidLukeEnterMapLocked：走到王座的 boss 房才开能源储藏室），
// 理由也一样：[ON ENTER MAP] 是**房间**边，不是副本边。
//
// 调用者持 session.dungeon.mu（dungeon.mu -> raidMu 是本服务端允许的顺序）。
// 战况表**不在这里下发**：它由 1 Hz 的心跳连同新状态一起带出去，避免在持有本会话
// dungeon 锁的时候去戳别的成员的会话状态。
func (s *Service) notifyRuntimeFiendWarMapEnteredLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	roomMapID int64,
	source string,
) {
	if s == nil || session == nil || runtime == nil || roomMapID == 0 {
		return
	}
	if !isCurrentFiendWarRaidChannel(session) {
		return
	}
	if runtime.raidDungeon.RaidKey == 0 || runtime.Request.DungeonID == 0 || !runtime.raidDungeon.loadedNotified {
		return
	}
	s.raidMu.Lock()
	state := s.raids[runtime.raidDungeon.RaidKey]
	if state == nil || state.Lifecycle.FiendWar == nil || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		s.raidMu.Unlock()
		return
	}
	a, ok := state.Lifecycle.FiendWar.AreaForDungeon(int(runtime.Request.DungeonID))
	if !ok || state.Lifecycle.FiendWar.Value(a.Kind+" state") != 2 {
		// 不是 BOSS 侧那次（或者已经推进过了）：一个字都不改。state != 2 这条守卫
		// 同时保证"进第二次房"不会重复推进。
		s.raidMu.Unlock()
		return
	}
	battle := state.Lifecycle.FiendWar.Clone()
	if err := battle.Apply(raid.FiendWarEvent{Kind: "on enter map", ID: int(roomMapID)}, time.Now().UTC()); err != nil {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-fiendwar-map-entered-failed",
			"raid_key", runtime.raidDungeon.RaidKey,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Request.DungeonID,
			"room_map_id", roomMapID,
			"area", a.Index,
			"error", err,
			"source", source)
		return
	}
	if battle.Single != nil && a.Index == battle.Single.BossArea && battle.Value(a.Kind+" state") == 4 {
		battle.StartSingleBossPattern(a.Index)
	}
	state.Lifecycle.FiendWar = battle
	syncRuntimeFiendWarObjectives(state)
	state.Lifecycle.Revision++
	raidKey := state.RaidKey
	stateAfter := battle.Value(a.Kind + " state")
	s.raidMu.Unlock()
	s.logGameEvent(session, "game-fiendwar-map-entered",
		"raid_key", raidKey,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Request.DungeonID,
		"area", a.Index,
		"area_kind", a.Kind,
		"room_map_id", roomMapID,
		"state_before", 2,
		"state_after", stateAfter,
		"pattern_state", battle.Value("boss pattern state"),
		"reason", "on_enter_map_room_edge_opens_the_boss_pattern",
		"source", source)
}

func (s *Service) markRuntimeFiendWarSettlementFinished(state runtimeRaidState) error {
	for _, member := range state.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		if runtime != nil && runtime.raidDungeon.RaidKey == state.RaidKey && runtime.raidDungeon.completedNotified {
			runtime.raidNativeSettlementFinished = true
		}
		session.dungeon.mu.Unlock()
	}
	return s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{1453, []byte{4, 0}})
}

// Forced retreats reuse the normal town-return owner on each character mailbox.
// The frozen attempt prevents a delayed timeout from retiring a newer run.
// Keep the latest attempt per map so every member can retry a failed return.
//
// Two gestures share this entry point and they are NOT the same exit:
//
//   - ended == false is a retreat (区域超时 / PVF 的整队撤退 / 放弃双 BOSS 一边).
//     The whole squad pulls out together, so it takes the same WHOLE-party exit the
//     战况表 leader-led return already uses and the squad label survives -- the
//     operator's ruling 「在战况表里撤退 应该保持编制」. Before 2026-10-01 this used
//     the plain interrupted-run exit, which detached the member from the ordinary
//     party and cleared his 编制, so a retreat silently dissolved the squad.
//   - ended == true is the failure tail: EVERY member ends in the waiting room
//     (town 98 / area 2) and the 攻坚队 survives; see publishFailedFiendWar.
func (s *Service) returnRuntimeFiendWarDungeons(state runtimeRaidState, attempts map[int]runtimeRaidDungeonProgress, ended bool) error {
	source := "fiendwar_retreat"
	if ended {
		source = "fiendwar_failed_waiting_area"
	}
	for _, member := range state.FrozenMembers {
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online || identity.session == nil {
			continue
		}
		session := identity.session
		if err := s.postGameSessionCharacterEvent(session, "fiendwar-retreat", identity.character, identity.generation, func() error {
			if current, ok := s.runtimeRaidForMember(identity.character); ok && current.RaidKey != state.RaidKey {
				return nil
			}
			session.dungeon.mu.Lock()
			runtime := session.dungeon.runtime
			if runtime == nil || runtime.raidDungeon.RaidKey != state.RaidKey {
				session.dungeon.mu.Unlock()
				if !ended {
					return nil
				}
				// Failure (Fix D): this member owns no dungeon of this raid, so the
				// town-return owner has nothing to rebuild for him. He is standing
				// in a town operation area -- and once the raid row lost its party,
				// town_move refused every op36 with AreaID >= 2
				// (fiendwar_membership_required), including the waiting area itself,
				// so the client hung and the player could not walk back. Push the
				// waiting-area scene directly instead; it needs no inbound op36.
				return s.pushRuntimeFiendWarWaitingArea(session, state, source)
			}
			attempt, found := attempts[int(runtime.Request.DungeonID)]
			// 本服务端的一次尝试按 (副本, 座位) 记账，没有独立的 owner token。
			// 这条守卫的意图是"迟到的超时不能退掉更新的一场"，所以比对的必须是
			// 当前这一次尝试的座位与副本 token，而不是谁先开的局。
			seat := runtimeRaidSeatFor(member)
			if !ended && (!found || attempt.Seat != seat || attempt.LifecycleToken != runtime.lifecycleToken) {
				session.dungeon.mu.Unlock()
				return nil
			}
			runtime.raidDungeon.abandonedNotified = true
			if ended {
				runtime.raidDungeon = runtimeRaidDungeonBinding{}
				// 失败收尾必须保住全员编制（「攻坚队不应该被解散」）：这是服务器把
				// 所有人送回待机区，不是「队内成员单独自己回城」。
				runtime.townReturnKeepsSquadLabel = true
			}
			status := runtime.Session.Snapshot().Run.Status
			session.dungeon.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			defer cancel()
			transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, identity.character)
			if err != nil {
				return err
			}
			if ended {
				// 失败落点必须是待机区 town 98 / area 2，不是频道门口 (98/0)。
				// 98/2 的坐标只能从等待区 NPC 取 —— currentRaidTownTransition 只返回
				// spec.areaID，且硬要求目标区域有 Gate。
				town, area, row, rowErr := s.fiendWarWaitingAreaScene(state, identity.character)
				if rowErr != nil {
					return rowErr
				}
				transition.TownID, transition.AreaID = town, area
				transition.PositionX, transition.PositionY = int16(row.Value1), int16(row.Value2)
				transition.Direction, transition.AreaState = row.Value3, row.Value4
				transition.PositionSource = source
				transition.Body, err = buildCurrentSceneTransitionBody(town, area, []currentSceneTransitionRow{row})
				if err != nil {
					return err
				}
			}
			switch {
			case status == worldmap.DungeonRunCompleted:
				// A cleared run keeps its own exit; the failure tail only re-targets
				// where it lands.
				err = s.sendCurrentCompletedDungeonReturnToTown(session, runtime, transition, 132, source, false)
			case ended:
				err = s.sendCurrentActiveDungeonReturnToTown(session, runtime, transition, 132, source)
			default:
				err = s.sendCurrentActiveDungeonReturnToTownKeepingParty(session, runtime, transition, 132, source)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// fiendWarWaitingAreaScene builds the waiting-room (town 98 / area 2) transition row
// for one member from the PVF waiting-area NPCs.
//
// This is the same source prepareRuntimeFiendWarRetreat and
// runtimeFiendWarRejoinLocation use, and it is the ONLY source that has a coordinate
// for 98/2: currentRaidTownTransition returns spec.areaID alone (the channel's gate
// area, 98/0) and hard-requires the target area to own a non-nil Gate.
func (s *Service) fiendWarWaitingAreaScene(state runtimeRaidState, characterID uint16) (byte, byte, currentSceneTransitionRow, error) {
	config := (*raid.FiendWarBattleConfig)(nil)
	if state.FiendWarConfig != nil {
		config = state.FiendWarConfig.Battle
	}
	return s.fiendWarWaitingAreaSceneForConfig(config, characterID)
}

func (s *Service) fiendWarWaitingAreaSceneForConfig(
	config *raid.FiendWarBattleConfig,
	characterID uint16,
) (byte, byte, currentSceneTransitionRow, error) {
	if config == nil {
		return 0, 0, currentSceneTransitionRow{}, errRuntimeRaidLifecycleState
	}
	waiting, found := s.townArea(int64(config.WaitingTown), int64(config.WaitingArea))
	if !found {
		return 0, 0, currentSceneTransitionRow{}, fmt.Errorf("fiendwar waiting town absent from PVF")
	}
	for _, npc := range waiting.NPCs {
		if slices.Contains(config.WaitingNPCs, int(npc.NPCID)) && npc.X >= 0 && npc.X <= 32767 && npc.Y >= 0 && npc.Y <= 32767 {
			row := currentSceneTransitionRow{
				ObjectOrResourceKey: currentSceneActorObjectKey(characterID),
				Value1:              uint16(npc.X),
				Value2:              uint16(npc.Y),
				Value3:              currentAntonRaidDirection,
				Value4:              currentAntonRaidAreaState,
			}
			return byte(config.WaitingTown), byte(config.WaitingArea), row, nil
		}
	}
	return 0, 0, currentSceneTransitionRow{}, fmt.Errorf("fiendwar waiting NPC position absent from PVF")
}

// fiendWarWaitingRoomTransition rewrites a town transition so it lands in the
// Fiend War waiting room (town 98 / area 2) at a waiting NPC.
//
// It exists because the generic "completed dungeon" transition answers "where was
// this member standing when the run started", and that answer only has coordinates
// for the ordinary operation areas. The two 量子时空传送站 areas (30/31) have none:
// live 2026-10-01 16:06, clearing 4978 produced `town_id=98 area_id=30
// position_x=0 position_y=0`, the client could not place the character, and the
// member stayed inside the dungeon after a fully settled clear.
//
// The field set mirrors prepareRuntimeFiendWarRetreat (the retreat path, which is
// the one that already works) so the two routes cannot drift apart.
func (s *Service) fiendWarWaitingRoomTransition(
	state runtimeRaidState,
	characterID uint16,
	transition currentDungeonTownTransition,
) (currentDungeonTownTransition, error) {
	config := (*raid.FiendWarBattleConfig)(nil)
	if state.FiendWarConfig != nil {
		config = state.FiendWarConfig.Battle
	}
	return s.fiendWarWaitingRoomTransitionForConfig(config, characterID, transition)
}

func (s *Service) fiendWarWaitingRoomTransitionForConfig(
	config *raid.FiendWarBattleConfig,
	characterID uint16,
	transition currentDungeonTownTransition,
) (currentDungeonTownTransition, error) {
	town, area, row, err := s.fiendWarWaitingAreaSceneForConfig(config, characterID)
	if err != nil {
		return transition, err
	}
	body, err := buildCurrentSceneTransitionBody(town, area, []currentSceneTransitionRow{row})
	if err != nil {
		return transition, err
	}
	transition.TownID, transition.AreaID = town, area
	transition.PositionX, transition.PositionY = int16(row.Value1), int16(row.Value2)
	transition.Direction, transition.AreaState = row.Value3, row.Value4
	transition.PositionSource = "fiendwar_clear_waiting_room"
	transition.Body = body
	return transition, nil
}

// fiendWarCompletedBossReturnTransition applies the waiting-room landing even
// after the online raid object has been finalized and removed. Single-mode card
// completion can finalize the raid before the player clicks 返回城镇; looking up
// the member in s.raids at that point fails and used to fall through to the BOSS
// area's synthetic (0,0) origin, leaving the client in the first BOSS room.
func (s *Service) fiendWarCompletedBossReturnTransition(
	characterID uint16,
	dungeonID int64,
	transition currentDungeonTownTransition,
) (currentDungeonTownTransition, bool, error) {
	config, err := s.runtimeRaidFiendWarConfig()
	if err != nil {
		return transition, false, err
	}
	if config == nil || config.Battle == nil {
		return transition, false, errRuntimeRaidLifecycleState
	}
	isBoss := false
	for _, area := range config.Battle.Areas {
		if int64(area.Dungeon) == dungeonID && strings.HasSuffix(area.Kind, "boss") {
			isBoss = true
			break
		}
	}
	if !isBoss {
		return transition, false, nil
	}
	waited, err := s.fiendWarWaitingRoomTransitionForConfig(config.Battle, characterID, transition)
	return waited, true, err
}

// pushRuntimeFiendWarWaitingArea is the server-driven town push for a Fiend War
// member whose screen has to move to the waiting room with NO inbound op36.
//
// It follows the shape the sibling route proved
// (finishRuntimeFiendWarLeave, raid_fiendwar_rejoin_leave.go:178-225) with the three
// differences that route needs and this one must not inherit:
//
//   - no cleanupOnlinePlayer: that broadcasts a spurious offline to friends and the
//     guild (online_player_manager.go:2072) while the player is still online;
//     publishCurrentTownPresence publishes the town/social state itself.
//   - the ready flag is written AFTER resetDungeonEntrySceneGates, which zeroes
//     townSceneReadyCharacterID (dungeon_return_town.go:2511).
//   - the pending dungeon-select context and selector origin are retired first, the
//     same two calls the interrupted-run town route makes.
//
// The caller must hold no session lock: with no event loop this runs inline on the
// posting goroutine (game_session_events.go:272-274).
func (s *Service) pushRuntimeFiendWarWaitingArea(session *gameSession, state runtimeRaidState, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || session.conn == nil {
		return nil
	}
	characterID := session.selectedCharacterID
	town, area, row, err := s.fiendWarWaitingAreaScene(state, characterID)
	if err != nil {
		return err
	}
	body, err := buildCurrentSceneTransitionBody(town, area, []currentSceneTransitionRow{row})
	if err != nil {
		return err
	}
	clearCurrentDungeonSelectContext(session)
	clearCurrentTownSelectorOrigin(session)
	resetDungeonEntrySceneGates(session)
	if err := s.sendCurrentTownLocationNotifications(session, characterID, town, area, row, source); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, body, 0); err != nil {
		return err
	}
	session.townMu.Lock()
	setCurrentTownPositionSceneFromTransitionLocked(session, characterID, town, area, row)
	session.townSceneReadyCharacterID = characterID
	session.townMu.Unlock()
	// 离开城镇等于放弃"走进区域即开战"的去重记录，下次抵达要能重新开。
	session.fiendWarEncounterRaidKey, session.fiendWarEncounterArea = 0, 0
	session.fiendWarEncounterAt = time.Time{}
	character := dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(characterID))}
	if repositories, ok := s.repositoryGroup(); ok && repositories.Character != nil {
		if loaded, found, loadErr := repositories.Character.Load(context.Background(), character.CharacterID); loadErr == nil && found {
			character = loaded
		}
	}
	mapPath := ""
	if loaded, found := s.townArea(int64(town), int64(area)); found {
		mapPath = loaded.MapPath
	}
	s.publishCurrentTownPresence(session, character, town, area, row, state.RaidKey, mapPath, source)
	s.logGameEvent(session, "game-fiendwar-force-waiting-area",
		"raid_key", state.RaidKey,
		"char_id", characterID,
		"town_id", town,
		"area_id", area,
		"position_x", row.Value1,
		"position_y", row.Value2,
		"reason", "the_run_ended_and_the_whole_raid_returns_to_the_waiting_room",
		"source", source)
	return nil
}

// publishFailedFiendWar is the Fiend War failure tail. It replaces the old
// publishExpiredFiendWar, whose two halves were both wrong for the operator's rule:
//
//  1. It returned only members who were physically inside a dungeon
//     (raid_fiendwar_battle.go:905-906 returned early when session.dungeon.runtime
//     was nil) and answered everybody else with op665
//     (sendRuntimeSingleRaidEnded(session, 665, "fiendwar_expired")). op665 is
//     exactly what makes the client print 46791「已经强制退出攻坚队。」
//     (raid_online.go:1501-1503) -- the popup the operator reported as "攻坚队被解散".
//  2. It landed members in the CHANNEL GATE area 98/0 (the
//     fiendwar_expired_public_town destination), not in the waiting room 98/2.
//
// Fix D's ruling: 「攻坚失败后 所有人应被传送回待机区 并且攻坚队不应该被解散」.
// Fix C's banner is published here too, as the 587/601 pair below.
func (s *Service) publishFailedFiendWar(state runtimeRaidState) error {
	// 过期退场要把"此刻还占着区域的所有尝试"都退掉。本服务端的 Dungeons 按
	// (副本, 座位) 记账，而 returnRuntimeFiendWarDungeons 按副本查 —— 在入口处
	// 拍平一次，键取 MapID。超时空一个区域只允许一个小队占用，所以不会撞键。
	attempts := make(map[int]runtimeRaidDungeonProgress, len(state.Lifecycle.Dungeons))
	for key, progress := range state.Lifecycle.Dungeons {
		attempts[key.MapID] = progress
	}
	if err := s.returnRuntimeFiendWarDungeons(state, attempts, true); err != nil {
		return err
	}
	return s.publishFiendWarFailurePresentation(state)
}

// The Fiend War failure presentation. Both constants are named so the in-game test
// is a one-line change and the packet log alone is enough to diagnose it.
const (
	// runtimeRaidFiendWarFailureState is the 587 state byte the presentation holds.
	//
	// The client's banner gate is the conjunction raidObj+0x28 == 6 (this byte) and
	// raidObj+0x2C == 1 (the 601 ResultType below), so state 6 is what makes the
	// 30210「作战失败，无法再继续进行攻坚。」 banner reachable at all. It is ALSO
	// what blocks C2S 666: the only START_RAID sender (FUN_017B2820) allows a start
	// for state 0 and state 5 and denies everything else, which is why the
	// presentation releases to runtimeRaidFiendWarFailureReleaseState afterwards.
	// Flip this to 4 if the banner is gated out in game; 4 is the value Anton/Luke
	// ship for a failed phase.
	runtimeRaidFiendWarFailureState byte = runtimeRaidSettlementStateFinished
	// runtimeRaidFiendWarFailureStateArgument is the 587 argument byte.
	runtimeRaidFiendWarFailureStateArgument int8 = 1
	// runtimeRaidFiendWarFailureHold is how long state 6 is held before the release.
	// The client's HUD predicate is a level test evaluated every frame, so this is
	// presentation time only; it is the parameter most likely to need tuning in game.
	runtimeRaidFiendWarFailureHold = 5 * time.Second
	// runtimeRaidFiendWarFailureBannerReassert is when the banner pair is repeated,
	// measured from the first publication.
	//
	// 「开始/成功都有横幅，失败没有」的那一半原因不在包上，在**顺序**上：失败一收尾
	// 就把全员传送回待机区，真机实测两者只差几十到三百毫秒 ——
	//
	//	16:00:36.619  1453 / 585 / 597 / 587{6,1} / 601{ResultType 1}   ← 横幅门点亮
	//	16:00:36.666  game-dungeon-full-town-route-send source=fiendwar_failed_waiting_area
	//	16:00:36.927  game-dungeon-full-town-route-committed
	//	16:00:41.620  587{0} 释放
	//
	// 客户端那个 30210 横幅是**逐帧**判断 raidObj+0x28 == 6 && raidObj+0x2C == 1 的
	// （FUN_01f67bc0，见 tools/_fw3_fixes_spec.md:240-256），场景一切换就被新的
	// 城镇场景盖住，于是那 5 秒的"演出时间"根本没人在看。这里在待机区落地之后再补
	// 一次同样的 587{6,1} + 601{1,…}：两个字节是电平判断不是边沿触发，重复发同值
	// 只是把门重新点亮，不改任何状态。
	runtimeRaidFiendWarFailureBannerReassert = 2 * time.Second
	// runtimeRaidFiendWarFailureReleaseState re-opens the client's start gate: 0 is
	// in FUN_017B2820's allow set, and the state-0 branch of the 587 handler does not
	// touch raidObj+0x2C, so the banner's result-type byte survives the release.
	runtimeRaidFiendWarFailureReleaseState byte = 0
	// runtimeRaidFiendWarFailResultType 是超时空**失败**要发的 601 结果类型。
	//
	// 它必须是 3，不是安东/卢克那个 1 —— 客户端反汇编（DNF.exe sha256 8A7AF614…AA4D）：
	//
	//   0x009B2910(byte* body)   ← 1453{u8 kind, u8 arg} 的分派器
	//       mov  eax,[ebp+8] / movsx eax,[eax] / dec eax / cmp eax,5 / ja -> return
	//       jmp  dword ptr [eax*4 + 0x9B2B7C]        ; 6 个 case，kind 1..6
	//         case 1 -> 0x9B29BD   case 2 -> 0x9B2A04   case 3 -> 0x9B2A8D
	//         case 4 -> 0x9B2B0B   case 5 -> 0x9B2B18   case 6 -> 0x9B2B18
	//
	//   0x009B2B18（case 5/6 的实体）：
	//       mov  ecx,[0x51b2748] / call 0x17B22C0    ; raidObj
	//       mov  eax,[eax+0x2C]                      ; ★ 结果类型
	//       sub  eax,2 / je 0x9B2B40                 ; == 2 -> 75518「量子时空传送站破坏成功」
	//       dec  eax / jne 0x9B2B60                  ; != 3 -> 什么都不发
	//       push 0x126EF (75503「未能破坏量子传送站。」) / jmp 0x9B2B45
	//
	// 而 raidObj+0x2C 全客户端只有 601 的处理器会写（sub_1D6B840 尾部
	// 0x01D6C3E5 mov ecx,[ebp-0x64] / 0x01D6C3E8 mov [eax+0x2C],ecx），
	// 写的就是 body 的第一个字节。
	//
	// ⇒ 所以"攻坚失败没提示"的完整根因是一条**顺序 + 取值**的双重错误：
	//   ① 我们发的 1453{6,0} 落在 case 6，那一刻读 +0x2C；
	//   ② 可是 601 是**在 1453 之后**才发的，而且带的是 1 —— 读到的既不是 2 也不是 3。
	//   修法：先发 601{ResultType 3}，再发 1453{6,0}。
	//
	// 代价说明：30210「作战失败，无法再继续进行攻坚。」那条通用横幅（HUD 逐帧函数
	// FUN_01f67bc0 0x01F67E7D）要的是 +0x2C == 1，与 3 互斥；实测它在真机上从来没亮过
	// （还差一个 UI 谓词 global[0x60746d8]->vslot5(0x11,0)），所以选客户端自己那条
	// 超时空失败文案。要让三条（开始/成功/失败）都用原生文案，成功那一条对应的是
	// +0x2C == 2（75518），但那会动到已经验收过的成功路径，先不碰。
	runtimeRaidFiendWarFailResultType byte = 3
)

// publishFiendWarFailurePresentation writes the failure sequence.
//
// ORDER IS THE FIX (see runtimeRaidFiendWarFailResultType): the 601 must land
// BEFORE 1453{6,0}, because the 1453 case-6 body reads raidObj+0x2C at that
// instant and only prints 75503 when it reads 3.
//
//	601{ResultType 3}  writes raidObj+0x2C = 3 (the only packet that writes it)
//	1453{6,0}          case 6 -> reads +0x2C == 3 -> 75503「未能破坏量子传送站。」
//	585                every gate of the phase closed
//	597{0,0}           remaining time zeroed
//	587{6,1}           lights the banner gate's state half
//
// and then hands the release (587{0}) to scheduleRuntimeFiendWarFailureRelease,
// which also re-asserts the pair once the waiting-room teleport has landed.
//
// NOT sent, deliberately: op665. See publishFailedFiendWar.
func (s *Service) publishFiendWarFailurePresentation(state runtimeRaidState) error {
	if state.RaidKey == 0 || len(state.FrozenMembers) == 0 {
		return errRuntimeRaidSettlementRoster
	}
	_, dungeonBody, dungeonRows, err := runtimeRaidPhaseSnapshotBodies(state)
	if err != nil {
		return err
	}
	wirePhase, err := runtimeRaidSettlementWirePhase(state.Lifecycle.Phase)
	if err != nil {
		return err
	}
	resultBody, err := raid.BuildRaidSettlementResultBody(raid.RaidSettlementResult{
		ResultType:   runtimeRaidFiendWarFailResultType,
		PhaseIndex:   wirePhase,
		ClearSeconds: runtimeRaidDurationSeconds(runtimeRaidPhaseElapsed(state, s.gameplayNow())),
		DeathCount:   s.runtimeRaidPhaseDeathCount(state.RaidKey, state.Lifecycle.Phase),
		Score:        0,
		Flags:        1,
	})
	if err != nil {
		return err
	}
	// The 585 rows travel once so every client redraws the selector from the same
	// closed state; the local table is recorded for the same reason
	// publishFailedRuntimeRaid records it.
	s.recordRuntimeRaidDungeonStateRows(state.RaidKey, state.Lifecycle.Phase, dungeonRows, runtimeRaidInfectionMapIDForWire(state))
	// 601 FIRST: it is what writes raidObj+0x2C, and the 1453 that follows reads it.
	var firstErr error
	for _, member := range state.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidResultMsgID, resultBody, 0, true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.logGameEvent(session, "game-fiendwar-failure-result-failed",
				"char_id", member.CharID, "raid_key", state.RaidKey, "error", err)
			continue
		}
		s.logGameEvent(session, "game-fiendwar-failure-published",
			"char_id", member.CharID,
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"result_type", runtimeRaidFiendWarFailResultType,
			"death_count", s.runtimeRaidPhaseDeathCount(state.RaidKey, state.Lifecycle.Phase),
			"failure_state", runtimeRaidFiendWarFailureState,
			"failure_state_argument", runtimeRaidFiendWarFailureStateArgument,
			"release_state", runtimeRaidFiendWarFailureReleaseState,
			"hold_seconds", int(runtimeRaidFiendWarFailureHold/time.Second),
			"disband_sent", false,
			"source", "fiendwar_failure_presentation")
	}
	notifications := []runtimeRaidWireNotification{
		{msgID: 1453, body: []byte{6, 0}},
		{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody},
		{msgID: raid.RaidRemainTimeMsgID, body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: 0})},
		{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{
			State:    runtimeRaidFiendWarFailureState,
			Argument: runtimeRaidFiendWarFailureStateArgument,
		})},
	}
	if err := s.broadcastRuntimeRaidWireSequence(state, notifications...); err != nil {
		return err
	}
	s.scheduleRuntimeFiendWarFailureRelease(state, resultBody)
	return firstErr
}

// reassertFiendWarFailureBanner repeats the failure banner pair after the
// waiting-room teleport has replaced the player's scene.
//
// It re-reads the row under raidMu and does nothing once the release has already
// reset the raid to its lobby, so a late/duplicate callback is a no-op. Every
// packet is the same value the first publication sent: 587{6,1} writes
// raidObj+0x28 and 601{ResultType 1} writes raidObj+0x2C, and the HUD gate is the
// conjunction of the two, evaluated every frame -- re-sending only re-lights a
// level, it never advances anything.
func (s *Service) reassertFiendWarFailureBanner(raidKey uint32, resultBody []byte) {
	if len(resultBody) == 0 {
		return
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.Lifecycle.Status != runtimeRaidLifecycleFailed || len(state.FrozenMembers) == 0 {
		s.raidMu.Unlock()
		return
	}
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	if err := s.broadcastRuntimeRaidWireSequence(snapshot, runtimeRaidWireNotification{
		msgID: raid.RaidStateMsgID,
		body: raid.BuildRaidStateBody(raid.RaidState{
			State:    runtimeRaidFiendWarFailureState,
			Argument: runtimeRaidFiendWarFailureStateArgument,
		}),
	}); err != nil {
		s.logPacketEvent("game-fiendwar-failure-banner-reassert-send-failed",
			"raid_key", raidKey, "error", err)
		return
	}
	delivered := 0
	for _, member := range snapshot.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidResultMsgID, resultBody, 0, true); err != nil {
			s.logGameEvent(session, "game-fiendwar-failure-banner-reassert-failed",
				"char_id", member.CharID, "raid_key", raidKey, "error", err)
			continue
		}
		delivered++
	}
	s.logPacketEvent("game-fiendwar-failure-banner-reasserted",
		"raid_key", raidKey,
		"failure_state", runtimeRaidFiendWarFailureState,
		"result_type", runtimeRaidFailResultType,
		"recipients", delivered,
		"reason", "the_waiting_room_teleport_replaced_the_scene_the_banner_was_first_lit_in",
		"source", "fiendwar_failure_presentation")
}

// scheduleRuntimeFiendWarFailureRelease arms the two post-failure steps:
//
//	+2s  re-assert 587{6,1} + 601{1,…}  (reassertFiendWarFailureBanner)
//	+5s  release 587{0} + return the 攻坚队 to its lobby
//
// The re-assert is scheduled FIRST so the release stays the last schedule of the
// pair -- the one-shot assertions elsewhere read the most recent schedule.
//
// Releasing is what keeps 「攻坚队不应该被解散」 true in practice: the START_RAID gate
// allows only state 0 and state 5, so a party left holding state 6 could never begin
// another run. The party roster, its squad labels and the raid row itself are all
// untouched by the release -- what ends is this RUN, not the 攻坚队.
func (s *Service) scheduleRuntimeFiendWarFailureRelease(state runtimeRaidState, resultBody []byte) {
	release := func() {
		// Read the row back under the lock: a member may have left the raid during
		// the hold, and the release must describe the party as it is now.
		current, ok := s.runtimeRaidForMember(state.LeaderID)
		if !ok || current.RaidKey != state.RaidKey || len(current.Members) == 0 {
			return
		}
		if err := s.broadcastRuntimeRaidWireSequence(current, runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body:  raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidFiendWarFailureReleaseState}),
		}); err != nil {
			s.logPacketEvent("game-fiendwar-failure-release-failed", "raid_key", state.RaidKey, "error", err)
		}
		if err := s.resetRuntimeFiendWarRaidToLobby(state.RaidKey); err != nil {
			s.logPacketEvent("game-fiendwar-failure-lobby-reset-failed", "raid_key", state.RaidKey, "error", err)
		}
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		s.reassertFiendWarFailureBanner(state.RaidKey, resultBody)
		release()
		return
	}
	reassertName := fmt.Sprintf("fiendwar-failure-banner:%d:%d", state.RaidKey, state.StartAttempt)
	if err := queue.ScheduleAfter(reassertName, runtimeRaidFiendWarFailureBannerReassert, func(time.Time) {
		s.reassertFiendWarFailureBanner(state.RaidKey, resultBody)
	}); err != nil {
		s.logPacketEvent("game-fiendwar-failure-banner-reassert-schedule-failed",
			"raid_key", state.RaidKey, "error", err)
		s.reassertFiendWarFailureBanner(state.RaidKey, resultBody)
	}
	name := fmt.Sprintf("fiendwar-failure-release:%d:%d", state.RaidKey, state.StartAttempt)
	if err := queue.ScheduleAfter(name, runtimeRaidFiendWarFailureHold, func(time.Time) { release() }); err != nil {
		s.logPacketEvent("game-fiendwar-failure-release-schedule-failed", "raid_key", state.RaidKey, "error", err)
		release()
	}
}

// resetRuntimeFiendWarRaidToLobby puts a failed Fiend War 攻坚队 back into the state a
// created-but-not-started raid has, so the same party can press 开始攻坚 again.
//
// The raid row is NOT deleted and the roster is NOT touched: state.Members carries
// every 编制 label and survives verbatim. What is dropped is this run's frozen phase
// roster and its billing, both of which the next start re-creates
// (reserveRuntimeRaidStart + commitRuntimeRaidStartWithOwner, which builds a fresh
// FiendWar battle and re-places every member in the waiting area).
func (s *Service) resetRuntimeFiendWarRaidToLobby(raidKey uint32) error {
	// Read the clock before taking raidMu: gameplayNow reaches for the gameplay
	// timer queue under Service.mu, and this file never nests that inside raidMu.
	now := s.gameplayNow()
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.RaidMode != runtimeRaidOwnerFiendWar {
		s.raidMu.Unlock()
		return nil
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleFailed {
		// A member's own leave/abandon already took the row somewhere else.
		s.raidMu.Unlock()
		return nil
	}
	lobby := newRuntimeRaidLifecycleState(now)
	lobby.Status = runtimeRaidLifecycleCreated
	lobby.Phase = 0
	lobby.StartedAt = time.Time{}
	lobby.PhaseStartedAt = time.Time{}
	lobby.PhaseDeadline = time.Time{}
	lobby.ReadyDeadline = time.Time{}
	lobby.FailedAt = time.Time{}
	lobby.FiendWar = nil
	state.Lifecycle = lobby
	state.Started = false
	state.StartPending = false
	state.EntryCostPaid = false
	state.ChargedMembers = nil
	state.FrozenMembers = nil
	state.PendingCostSync = nil
	state.StartAttempt++
	if state.StartAttempt == 0 {
		state.StartAttempt++
	}
	state.Lifecycle.Revision++
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	// The client has to be handed back its lobby: the raid object (state 0, no
	// phase) is what the 攻击队 window is built from, and the roster refresh carries
	// the squad labels the failure left alone.
	if err := s.broadcastRuntimeRaidCreate(snapshot); err != nil {
		return err
	}
	if err := s.broadcastRuntimeRaidRefresh(snapshot); err != nil {
		return err
	}
	s.logPacketEvent("game-fiendwar-party-survived-failure",
		"raid_key", snapshot.RaidKey,
		"leader_id", snapshot.LeaderID,
		"member_count", len(snapshot.Members),
		"grouped_members", countRuntimeRaidGroupedMembers(snapshot),
		"reason", "the_run_failed_and_the_attack_party_stays_together",
		"source", "fiendwar_failure_release")
	return nil
}

// failRuntimeFiendWarRaid is abandonRuntimeRaid's Fiend War replacement: it marks the
// RUN failed and stops every later entry, completion or tick, but it KEEPS the raid
// row (and therefore the 攻坚队) in s.raids.
//
// abandonRuntimeRaid deletes the row, which is what made the operator's
// 「攻坚队不应该被解散」 impossible: town_move then refused every op36 with
// AreaID >= 2 (fiendwar_membership_required, town_move.go:135-145) -- including the
// waiting area at :120-129 -- so a member standing in a town operation area could
// not even walk back.
//
// It mirrors expireRuntimeRaidPhase's bookkeeping (gates closed, mechanisms cleared,
// Status = Failed) without its ten-second remover: resetRuntimeFiendWarRaidToLobby
// is what returns this row to the lobby, and removeRuntimeRaidFailedPhase must never
// run for a Fiend War raid.
func (s *Service) failRuntimeFiendWarRaid(raidKey uint32, now time.Time) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.RaidMode != runtimeRaidOwnerFiendWar {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	// Every gate of the failed phase closes, exactly as expireRuntimeRaidPhase closes
	// them before it announces the failure. The clear count and remaining HP stay:
	// the state row is what tells the client the gate is gone, and a later re-read
	// must not look like a completed objective.
	for mapID, objective := range state.Lifecycle.Objectives {
		objective.State = raid.AntonDungeonHide
		state.Lifecycle.Objectives[mapID] = objective
	}
	state.clearRuntimeRaidMechanismsLocked()
	state.Lifecycle.Status = runtimeRaidLifecycleFailed
	state.Lifecycle.FailedAt = now
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}
