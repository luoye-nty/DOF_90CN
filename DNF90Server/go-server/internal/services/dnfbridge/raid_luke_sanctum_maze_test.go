package dnfbridge

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// lukeCardSetMapInfo is luke.etc's [CARD SET MAP INFO], verbatim.
var lukeCardSetMapInfo = []int{0, 0, 0, 3, 3, 3, 6, 6, 6, 9, 9, 9, 12, 12, 12}

func lukeSanctumMazeTestState() *runtimeRaidState {
	state := lukePhaseOneGeneratorState()
	state.LukeConfig.CardSetMapInfo = append([]int(nil), lukeCardSetMapInfo...)
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{}
	for _, mapID := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
		state.Lifecycle.Objectives[mapID] = runtimeRaidDungeonObjective{
			Phase: runtimeRaidDungeonPhaseOne, MapID: mapID, State: raid.AntonDungeonOpen, MaxHP: 3, RemainingHP: 3,
		}
	}
	return state
}

// 这是**回退**候选表：正常路径已改成「当天共用一条记录、各读分量」，只有记录的分量
// 放不下该圣所的迷宫数时才回到这里。它对 "旧行为" 的钉法照旧有效。
func TestLukeSanctumMazeGroupsComeFromCardSetMapInfo(t *testing.T) {
	if got := runtimeRaidLukeSanctumMazeGroups(lukeCardSetMapInfo, 15); !reflect.DeepEqual(got, []int{0, 3, 6, 9, 12}) {
		t.Fatalf("groups=%v want [0 3 6 9 12]", got)
	}
	// A group whose third maze does not exist is not a group.
	if got := runtimeRaidLukeSanctumMazeGroups(lukeCardSetMapInfo, 13); !reflect.DeepEqual(got, []int{0, 3, 6, 9}) {
		t.Fatalf("groups for 13 mazes=%v want [0 3 6 9]", got)
	}
}

// Live 2026-09-24 production: three uniform picks of 湮灭之圣所 drew mazes 8, 11
// and 5, all third-of-group, and the squad fought 哈布 three times. The sanctum
// must instead walk one drawn group's first, second and third maze.
//
// ⚠ 语义变更（2026-09-29）：这个测试里的 choose 以前是「在候选组列表里抽一个下标、
// 各圣所各抽各的」，现在是「**当天抽哪一条 [CARD SET MAP INFO] 记录**」——官方
// 「所有频道的塔罗卡牌统一相同」⇒ 三圣所共用同一条记录。所以下面那条
// "Every sanctum draws its own group" 已按新语义换成"三所共用当天那条记录"。
// 其余部分（一次抽完不再抽、给放弃不重复抽、新一阶段重新抽）逐字保留。
func TestLukeSanctumWalksItsDrawnGroupOneMazePerClear(t *testing.T) {
	state := lukeSanctumMazeTestState()
	rolls := 0
	choose := func(limit int) (int, error) {
		rolls++
		if limit != 5 {
			t.Fatalf("card set record roll limit=%d want 5 records", limit)
		}
		return 3, nil // 记录 3 = `9 9 9`
	}
	for clear, wantMaze := range []int{9, 10, 11} {
		objective := state.Lifecycle.Objectives[lukeExtinctionSanctumMapID]
		objective.ClearCount = clear
		state.Lifecycle.Objectives[lukeExtinctionSanctumMapID] = objective
		maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, lukeExtinctionSanctumMapID, 15, choose)
		if err != nil || !ok {
			t.Fatalf("clear %d: ok=%t err=%v", clear, ok, err)
		}
		if maze.Maze != wantMaze || maze.GroupStart != 9 || maze.Chosen != (clear == 0) {
			t.Fatalf("clear %d: maze=%+v want maze %d of group 9", clear, maze, wantMaze)
		}
	}
	// A give-up does not clear: the retry re-enters the same maze.
	if maze, _, _ := runtimeRaidLukeSanctumMazeLocked(state, lukeExtinctionSanctumMapID, 15, choose); maze.Maze != 11 {
		t.Fatalf("retry after the third clear's give-up=%d want 11", maze.Maze)
	}
	if rolls != 1 {
		t.Fatalf("group rolled %d times want once per sanctum", rolls)
	}

	// 当天那条记录由三个圣所共用（旧断言是"每圣所各抽自己的组"，已随 2026-09-29
	// 的口径改动替换）：同一个记录下标下，另外两个圣所读到的正是同一条记录的分量。
	// 出货数据每条记录三个分量恒等，所以组起点相同。
	for _, sanctum := range []int{lukeBirthSanctumMapID, lukeSpreadSanctumMapID} {
		maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, sanctum, 15, choose)
		if err != nil || !ok {
			t.Fatalf("sanctum %d: ok=%t err=%v", sanctum, ok, err)
		}
		if maze.Record != 3 || maze.GroupStart != 9 || maze.Maze != 9 {
			t.Fatalf("sanctum %d maze=%+v want the day's record 3 (= group 9)", sanctum, maze)
		}
	}
	if got := state.Lifecycle.LukeSanctumMazeGroups[lukeExtinctionSanctumMapID]; got != 9 {
		t.Fatalf("another sanctum's draw moved the extinction group to %d", got)
	}

	// 换一天（换一条记录）就换一组：记录 0 = `0 0 0`。用一个还没抽过的团验证。
	fresh := lukeSanctumMazeTestState()
	other := func(int) (int, error) { return 0, nil }
	if maze, ok, _ := runtimeRaidLukeSanctumMazeLocked(fresh, lukeBirthSanctumMapID, 15, other); !ok ||
		maze.Record != 0 || maze.GroupStart != 0 || maze.Maze != 0 {
		t.Fatalf("birth sanctum maze=%+v ok=%t want record 0 / group 0", maze, ok)
	}

	// A new phase one draws again.
	initRuntimeRaidLukePhaseOneLocked(state)
	if len(state.Lifecycle.LukeSanctumMazeGroups) != 0 {
		t.Fatalf("phase-one init kept groups %v", state.Lifecycle.LukeSanctumMazeGroups)
	}
}

func TestLukeSanctumMazeLeavesEveryOtherRunToTheOrdinaryChoice(t *testing.T) {
	failing := func(int) (int, error) { return 0, errors.New("must not roll") }
	state := lukeSanctumMazeTestState()
	if _, ok, err := runtimeRaidLukeSanctumMazeLocked(state, lukeLightAltarMapID, 15, failing); ok || err != nil {
		t.Fatalf("a phase-two map took the sanctum path ok=%t err=%v", ok, err)
	}
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	if _, ok, err := runtimeRaidLukeSanctumMazeLocked(state, lukeBirthSanctumMapID, 15, failing); ok || err != nil {
		t.Fatalf("phase two took the sanctum path ok=%t err=%v", ok, err)
	}
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.TailFlag = 1
	if _, ok, err := runtimeRaidLukeSanctumMazeLocked(state, lukeBirthSanctumMapID, 15, failing); ok || err != nil {
		t.Fatalf("a single-mode raid took the sanctum path ok=%t err=%v", ok, err)
	}
	if _, ok, err := runtimeRaidLukeSanctumMazeLocked(nil, lukeBirthSanctumMapID, 15, failing); ok || err != nil {
		t.Fatalf("no raid took the sanctum path ok=%t err=%v", ok, err)
	}
}

// TestLukeSanctumMazeSharesTheDaysCardSetRecordAcrossSanctums is the state-level
// pin for the 2026-09-29 rule change. Official system text (2017-06-22 国服开放
// 卢克团队模式): 「塔罗卡牌以卢克团队模式进行的当日6点为基准进行变更，所有频道
// 的塔罗卡牌统一相同。」⇒ 一天抽**一条** [CARD SET MAP INFO] 记录，三个圣所共用
// 它、各读自己的分量；抽取器只按 tarot day，不带圣所。
//
// 用生产侧那一个抽取器跑一整个月：任一天里三个圣所必须报出同一个 Record，且
// 组起点就是该记录的分量；跨天至少出现 3 条不同记录（否则"每天变"是假的）。
func TestLukeSanctumMazeSharesTheDaysCardSetRecordAcrossSanctums(t *testing.T) {
	seen := map[int]bool{}
	for day := 1; day <= 30; day++ {
		raidDay := 20260900 + day
		// 生产侧 selectRuntimeRaidLukeSanctumMaze 给的就是这一个：只按 tarot day。
		draw := func(limit int) (int, error) {
			return runtimeRaidLukeTarotCardSetRecord(raidDay, limit), nil
		}
		record := runtimeRaidLukeTarotCardSetRecord(raidDay, 5)
		if record < 0 || record >= 5 {
			t.Fatalf("day %d record=%d outside [0,5)", raidDay, record)
		}
		seen[record] = true
		state := lukeSanctumMazeTestState()
		for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
			maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, sanctum, 15, draw)
			if err != nil || !ok {
				t.Fatalf("day %d sanctum %d ok=%t err=%v", raidDay, sanctum, ok, err)
			}
			if maze.Record != record {
				t.Fatalf("day %d sanctum %d read record %d, the day drew %d", raidDay, sanctum, maze.Record, record)
			}
			// 出货数据每条记录三个分量恒等 ⇒ 三所组起点都等于该记录的分量。
			want := lukeCardSetMapInfo[record*lukeSanctumClearsPerGroup]
			if maze.GroupStart != want || maze.Maze != want {
				t.Fatalf("day %d sanctum %d maze=%+v want group start %d on the first clear",
					raidDay, sanctum, maze, want)
			}
		}
	}
	if len(seen) < 3 {
		t.Fatalf("a month of tarot days drew only card set records %v", seen)
	}
}

// TestLukeSanctumCardSetComponentsMapEachSanctumToItsOwnCell uses a table whose
// records carry three DIFFERENT components, so "each sanctum reads its own cell"
// is distinguishable from "all three read the first cell". The mapping itself
// (BIRTH 0 / EXTINCTION 1 / SPREAD 2) is an unproven convention taken from
// luke.etc [DEFAULT OBJECT COUNT]'s written order -- see the file header.
func TestLukeSanctumCardSetComponentsMapEachSanctumToItsOwnCell(t *testing.T) {
	distinct := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	draw := func(int) (int, error) { return 2, nil } // 记录 2 = `6 7 8`
	want := map[int]int{
		lukeBirthSanctumMapID:      6,
		lukeExtinctionSanctumMapID: 7,
		lukeSpreadSanctumMapID:     8,
	}
	state := lukeSanctumMazeTestState()
	state.LukeConfig.CardSetMapInfo = distinct
	for sanctum, wantGroup := range want {
		// mazeCount 取 60：这 15 个分量都是合法组起点，所以走的是共用记录这条路径。
		maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, sanctum, 60, draw)
		if err != nil || !ok {
			t.Fatalf("sanctum %d ok=%t err=%v", sanctum, ok, err)
		}
		if maze.Record != 2 || maze.GroupStart != wantGroup || maze.Maze != wantGroup {
			t.Fatalf("sanctum %d maze=%+v want record 2 / group %d", sanctum, maze, wantGroup)
		}
	}
}

// TestLukeSanctumMazeFallsBackWhenTheDaysRecordDoesNotFit keeps the guard the
// change must not lose: the shared record's component still has to address a
// whole group of THIS sanctum's maze count (each .dgn declares its own), and a
// group that does not fit falls back to the candidate list instead of emitting
// an out-of-range maze. An unusable table takes over nothing at all.
func TestLukeSanctumMazeFallsBackWhenTheDaysRecordDoesNotFit(t *testing.T) {
	// 记录 1 = `14 14 14`；迷宫数 15 容不下 14（14+3 > 15）。候选表只剩 {12}。
	tight := []int{12, 12, 12, 14, 14, 14}
	// 抽取器对两次调用都要给出合法值：记录数 2 → 1；候选表大小 1 → 0。
	draw := func(limit int) (int, error) { return limit - 1, nil }
	state := lukeSanctumMazeTestState()
	state.LukeConfig.CardSetMapInfo = tight
	maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, lukeBirthSanctumMapID, 15, draw)
	if err != nil || !ok {
		t.Fatalf("ok=%t err=%v", ok, err)
	}
	if maze.Record != -1 {
		t.Fatalf("record=%d want -1 (the candidate fallback drew this one)", maze.Record)
	}
	if maze.GroupStart != 12 || maze.Maze != 12 {
		t.Fatalf("maze=%+v want the only candidate group 12", maze)
	}
	if maze.Maze+lukeSanctumClearsPerGroup > 15 {
		t.Fatalf("maze=%d runs past the sanctum's 15 mazes", maze.Maze)
	}

	// 候选表为空时宁可不接管这一次选图，也不发越界 maze。
	none := lukeSanctumMazeTestState()
	none.LukeConfig.CardSetMapInfo = []int{14, 14, 14}
	if maze, ok, err := runtimeRaidLukeSanctumMazeLocked(none, lukeBirthSanctumMapID, 15, draw); ok || err != nil {
		t.Fatalf("an unusable table took the sanctum path maze=%+v ok=%t err=%v", maze, ok, err)
	}

	// 越界的记录下标是错误，不是静默当成第 0 条。
	outOfRange := func(int) (int, error) { return 9, nil }
	if _, _, ok, err := runtimeRaidLukeSanctumMazeDrawnGroup(lukeCardSetMapInfo, lukeBirthSanctumMapID, 15, outOfRange); ok || err == nil {
		t.Fatalf("record roll 9 ok=%t err=%v want an error", ok, err)
	}
}

// TestLukeSanctumMazeRecordGroupReadsTheSharedRecordAndIsDefensive pins the
// record→group reader on its own: the shipped table, a distinct-component table,
// an unknown dungeon id, and the defensive paths (out-of-range record, truncated
// table) that must fall back to the record's first component instead of
// panicking.
func TestLukeSanctumMazeRecordGroupReadsTheSharedRecordAndIsDefensive(t *testing.T) {
	for record := 0; record < 5; record++ {
		for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
			start, ok := runtimeRaidLukeSanctumMazeRecordGroup(lukeCardSetMapInfo, record, sanctum, 15)
			if !ok || start != record*lukeSanctumClearsPerGroup {
				t.Fatalf("record %d sanctum %d start=%d ok=%t want %d",
					record, sanctum, start, ok, record*lukeSanctumClearsPerGroup)
			}
		}
	}
	distinct := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	for component, sanctum := range map[int]int{
		0: lukeBirthSanctumMapID,
		1: lukeExtinctionSanctumMapID,
		2: lukeSpreadSanctumMapID,
	} {
		start, ok := runtimeRaidLukeSanctumMazeRecordGroup(distinct, 2, sanctum, 60)
		if !ok || start != 6+component {
			t.Fatalf("sanctum %d component %d start=%d ok=%t want %d", sanctum, component, start, ok, 6+component)
		}
	}
	// 未列入映射的地牢 ID 按该记录的第一个分量处理（防御性；调用方只会问三个圣所）。
	if start, ok := runtimeRaidLukeSanctumMazeRecordGroup(distinct, 2, 12345, 60); !ok || start != 6 {
		t.Fatalf("unknown sanctum start=%d ok=%t want 6", start, ok)
	}
	// 越界的记录下标不 panic，只是不成立。
	if start, ok := runtimeRaidLukeSanctumMazeRecordGroup(lukeCardSetMapInfo, 99, lukeBirthSanctumMapID, 15); ok {
		t.Fatalf("record 99 start=%d ok=%t want not ok", start, ok)
	}
	if _, ok := runtimeRaidLukeSanctumMazeRecordGroup(lukeCardSetMapInfo, -1, lukeBirthSanctumMapID, 15); ok {
		t.Fatal("a negative record was accepted")
	}
	// 表被截断（16 个值 = 5 条记录 + 1）：分量 2 越出列表 ⇒ 回退到该记录的第一个
	// 分量，不 panic。
	truncated := append(append([]int(nil), lukeCardSetMapInfo...), 15)
	if start, ok := runtimeRaidLukeSanctumMazeRecordGroup(truncated, 5, lukeSpreadSanctumMapID, 18); !ok || start != 15 {
		t.Fatalf("truncated table start=%d ok=%t want the record's first component 15", start, ok)
	}
	// 组起点放不下这个圣所的迷宫数 ⇒ 不成立，交给回退。
	if start, ok := runtimeRaidLukeSanctumMazeRecordGroup(lukeCardSetMapInfo, 4, lukeBirthSanctumMapID, 13); ok {
		t.Fatalf("group start %d was accepted for a 13-maze sanctum", start)
	}

	// 出货数据下「共用记录、各读分量」与旧「每圣所各取同一分量」在**记录→组起点**
	// 这一层逐值等价：记录 i 的三个分量恒等，且正是旧候选表里的第 i 项。行为差异因此
	// 只剩"三所抽的是不是同一条记录"这一点，也就是本次要改的那一点。
	groups := runtimeRaidLukeSanctumMazeGroups(lukeCardSetMapInfo, 15)
	if len(groups) != 5 {
		t.Fatalf("candidate groups=%v want 5 records", groups)
	}
	for record := range groups {
		for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
			start, ok := runtimeRaidLukeSanctumMazeRecordGroup(lukeCardSetMapInfo, record, sanctum, 15)
			if !ok || start != groups[record] {
				t.Fatalf("record %d sanctum %d start=%d ok=%t want the old candidate %d",
					record, sanctum, start, ok, groups[record])
			}
		}
	}
}

// The tarot turns over at 06:00 server time and is the same for every raid that
// day ("每个卢克RAID攻坚日早上6点会刷新塔罗牌").
//
// 这段钉的是**每圣所的图案序号 B**（1181 里"该圣所今天启动哪套防御体系"），
// 2026-09-29 的改动没有动它的选法；"当天走哪组迷宫"改成了三所共用一条
// [CARD SET MAP INFO] 记录，由上面 TestLukeSanctumMazeSharesTheDaysCardSetRecord
// AcrossSanctums 钉住。
func TestLukeTarotDayTurnsOverAtSixAndIsSharedByTheServer(t *testing.T) {
	local := time.Local
	before := time.Date(2026, 9, 26, 5, 59, 0, 0, local)
	after := time.Date(2026, 9, 26, 6, 0, 0, 0, local)
	if got := runtimeRaidLukeTarotDay(before); got != 20260925 {
		t.Fatalf("05:59 tarot day=%d want the previous day 20260925", got)
	}
	if got := runtimeRaidLukeTarotDay(after); got != 20260926 {
		t.Fatalf("06:00 tarot day=%d want 20260926", got)
	}
	// Same day, same sanctum: every raid draws the same pattern index for it.
	for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
		first := runtimeRaidLukeTarotGroup(20260926, sanctum, 5)
		if first < 0 || first >= 5 {
			t.Fatalf("sanctum %d pattern index=%d outside [0,5)", sanctum, first)
		}
		if again := runtimeRaidLukeTarotGroup(20260926, sanctum, 5); again != first {
			t.Fatalf("sanctum %d drew %d then %d on one day", sanctum, first, again)
		}
	}
	// Across a month the draw actually changes.
	seen := map[int]bool{}
	for day := 1; day <= 30; day++ {
		seen[runtimeRaidLukeTarotGroup(20260900+day, lukeExtinctionSanctumMapID, 5)] = true
	}
	if len(seen) < 3 {
		t.Fatalf("a month of tarot days drew only pattern indices %v", seen)
	}

	// 当天的记录抽取器（走哪组迷宫）同样按天统一、换日即变，且不随圣所变。
	shared := map[int]int{}
	for day := 1; day <= 30; day++ {
		record := runtimeRaidLukeTarotCardSetRecord(20260900+day, 5)
		if record < 0 || record >= 5 {
			t.Fatalf("day %d card set record=%d outside [0,5)", day, record)
		}
		shared[record] = day
	}
	if len(shared) < 3 {
		t.Fatalf("a month of tarot days drew only card set records %v", shared)
	}
	if runtimeRaidLukeTarotCardSetRecord(20260926, 5) != runtimeRaidLukeTarotCardSetRecord(20260926, 5) {
		t.Fatal("the card set record draw is not reproducible on one day")
	}
	// 只有一条记录（或没有）时定格在 0，不 panic。
	for _, records := range []int{0, 1, -3} {
		if got := runtimeRaidLukeTarotCardSetRecord(20260926, records); got != 0 {
			t.Fatalf("records=%d record=%d want 0", records, got)
		}
	}
}
