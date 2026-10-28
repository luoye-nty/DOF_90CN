package dnfbridge

import (
	"fmt"
	"hash/fnv"
	"sort"
	"time"
)

// Luke phase-one sanctum mazes.
//
// Each sanctum is cleared three times ([5054/5055/5056 DGN HP] 3) and its .dgn
// declares fifteen [maze info] blocks. They form five groups of three
// consecutive mazes -- luke.etc [CARD SET MAP INFO] 0 0 0 3 3 3 6 6 6 9 9 9 12 12
// 12 maps every maze to its group's first maze -- and luke.etc.buff
// [RAID MONSTER LIST] makes the third maze of every group the sanctum's own boss
// (诞生 罗莎乌拉 56673, 湮灭 哈布 56067, 扩散 56066) while the first two carry the
// group's mid bosses and named monsters. The sanctum is meant to be walked as
// one group: the day's group, then its first, second and third maze on the
// first, second and third clear. The group is the day's tarot: it turns over at
// 06:00 server time (runtimeRaidLukeTarotDay), and a raid keeps the group it
// drew even when it runs past 06:00.
//
// 当天那一套牌是**全服一整套**，不是每圣所各抽一根。依据是官方系统说明
// （2017-06-22 国服开放卢克团队模式的更新公告原文）：
//
//	「塔罗卡牌以卢克团队模式进行的当日6点为基准进行变更，所有频道的塔罗卡牌
//	  统一相同。」
//
// 而 [CARD SET MAP INFO] 正是这一整套牌的表示：**5 条记录、每条 3 个分量**
// （出货数据 `0 0 0 / 3 3 3 / 6 6 6 / 9 9 9 / 12 12 12`；客户端也是每 3 个数字
// 一条地 push 进 3 字节元素的向量，见规格 §8.1.2）。这张表只能表达「三个圣所
// 取同一条记录」，表达不出「诞生走第 0 组 + 湮灭走第 4 组」。所以这里改成：
// **一天抽一条记录（只按 tarot day 哈希，抽法不带圣所）、三个圣所共用该记录、
// 各读自己的分量**（runtimeRaidLukeTarotCardSetRecord +
// runtimeRaidLukeSanctumMazeRecordGroup）。出货数据里每条记录三个分量恒等，
// 所以三个圣所拿到的组起点相同。
//
// ⚠ **未证实（工程约定）**：「第几个分量属于哪个圣所」**没有任何客户端证据**。
// 客户端从不读这 15 个数字（规格 §8.1.2 的指令级扫描只找到 `_Mylast - _Myfirst`
// 这一次计数读，没有任何按 3 字节步长的遍历），所以哪一格属于谁只能靠数据形状
// 推断。这里按 luke.etc [DEFAULT OBJECT COUNT] 的书写顺序取最自然的读法：
// **BIRTH 5054 → 分量 0，EXTINCTION 5055 → 分量 1，SPREAD 5056 → 分量 2**
// （luke.etc.symbol 的 117 [BIRTH SANCTUM OBJECT] / 118 / 119 是同一顺序）。
//
// 记录里的分量若对这个圣所的迷宫数不成立（`start < 0` 或
// `start + 3 > mazeCount`；各圣所的 .dgn 迷宫数可以不同），就退回旧的「可用
// 候选组」抽法（runtimeRaidLukeSanctumMazeGroups），绝不发出越界的 maze。
//
// Picking each entry uniformly from all fifteen mazes let the finale come up on
// the first clear or three times in a row: live 2026-09-24T16:24-16:26Z on
// production, raid 1835030's three 湮灭之圣所 runs drew mazes 8, 11 and 5 --
// all third-of-group -- and fought 哈布 three times.

// runtimeRaidLukeSanctumMazeGroups lists the distinct group starts of [CARD SET
// MAP INFO] that fit a sanctum with mazeCount mazes.
//
// 自 2026-09-29 起它只作**回退**用：正常路径是「当天共用一条记录、各读分量」
// （runtimeRaidLukeSanctumMazeRecordGroup）。这里把 15 个值全部去重，得到的正是
// 每条记录的第一个分量（0/3/6/9/12），也就是旧「各圣所独立抽一根」时代的候选表。
func runtimeRaidLukeSanctumMazeGroups(cardSetMapInfo []int, mazeCount int) []int {
	seen := make(map[int]bool, len(cardSetMapInfo))
	groups := make([]int, 0, 5)
	for _, start := range cardSetMapInfo {
		if start < 0 || start+lukeSanctumClearsPerGroup > mazeCount || seen[start] {
			continue
		}
		seen[start] = true
		groups = append(groups, start)
	}
	sort.Ints(groups)
	return groups
}

// lukeSanctumClearsPerGroup is how many mazes one group walks: the sanctum's
// [DGN HP].
const lukeSanctumClearsPerGroup = 3

// lukeSanctumCardSetComponents is which component of one [CARD SET MAP INFO]
// record each phase-one sanctum reads.
//
// ⚠ 未证实约定，理由见文件头：客户端从不读这 15 个数字，所以分量归属只能按
// luke.etc [DEFAULT OBJECT COUNT] 的书写顺序（BIRTH 5054 / EXTINCTION 5055 /
// SPREAD 5056 → 0/1/2）取最自然的读法。
var lukeSanctumCardSetComponents = map[int]int{
	lukeBirthSanctumMapID:      0,
	lukeExtinctionSanctumMapID: 1,
	lukeSpreadSanctumMapID:     2,
}

// runtimeRaidLukeTarotCardSetRecords is how many [CARD SET MAP INFO] records the
// day's set is drawn from. The block is a flat value list holding three
// components per record, which is exactly how the client groups it into its
// 3-byte element vector (spec §8.1.2), so the record count is the value count
// divided by three -- 5 for the shipped `0 0 0 3 3 3 6 6 6 9 9 9 12 12 12`.
func runtimeRaidLukeTarotCardSetRecords(cardSetMapInfo []int) int {
	return len(cardSetMapInfo) / lukeSanctumClearsPerGroup
}

// runtimeRaidLukeTarotCardSetRecord is the [CARD SET MAP INFO] record the whole
// server walks on one tarot day.
//
// 官方：「塔罗卡牌以卢克团队模式进行的当日6点为基准进行变更，所有频道的塔罗
// 卡牌统一相同。」⇒ 抽取只按 tarot day，**不带圣所、不带攻坚队**：同一天同一台
// 服务器的每一个团、每一个圣所都落在同一条记录上。换日即变。
func runtimeRaidLukeTarotCardSetRecord(raidDay int, records int) int {
	if records <= 1 {
		return 0
	}
	hash := fnv.New32a()
	_, _ = fmt.Fprintf(hash, "luke-tarot:%d", raidDay)
	return int(hash.Sum32() % uint32(records))
}

// runtimeRaidLukeSanctumMazeRecordGroup reads one sanctum's group start out of
// the day's shared [CARD SET MAP INFO] record. ok is false when that component
// does not address a whole group of this sanctum's maze count, in which case the
// caller falls back to the candidate list.
//
// 防御性（不 panic）：分量下标越出这个扁平列表时回退到**该记录的第一个分量**
// （未列入 lukeSanctumCardSetComponents 的地牢 ID 同样按第一个分量处理；调用方
// 只会就一阶段三个圣所提问，所以这条只在表被截断时才可能生效）。
func runtimeRaidLukeSanctumMazeRecordGroup(
	cardSetMapInfo []int,
	record int,
	sanctumMapID int,
	mazeCount int,
) (int, bool) {
	component, known := lukeSanctumCardSetComponents[sanctumMapID]
	if !known {
		component = 0
	}
	base := record * lukeSanctumClearsPerGroup
	index := base + component
	if base < 0 || index >= len(cardSetMapInfo) {
		component = 0
		index = base
	}
	if index < 0 || index >= len(cardSetMapInfo) {
		return 0, false
	}
	start := cardSetMapInfo[index]
	if start < 0 || start+lukeSanctumClearsPerGroup > mazeCount {
		return 0, false
	}
	return start, true
}

// runtimeRaidLukeSanctumMazeDrawnGroup draws one sanctum's group for one tarot
// day. It reads the day's shared [CARD SET MAP INFO] record first and only
// falls back to the old per-sanctum candidate list when that record cannot
// address a whole group of this sanctum's maze count.
//
// The returned record is the [CARD SET MAP INFO] record the group came from, or
// -1 when the candidate fallback produced it. ok is false when neither path
// yields a usable group: the caller then keeps its ordinary maze choice rather
// than being handed an out-of-range maze. choose must not be nil, and is called
// with "how many choices the day has" -- records first, then, only on the
// fallback, the candidate count.
func runtimeRaidLukeSanctumMazeDrawnGroup(
	cardSetMapInfo []int,
	mapID int,
	mazeCount int,
	choose func(int) (int, error),
) (int, int, bool, error) {
	records := runtimeRaidLukeTarotCardSetRecords(cardSetMapInfo)
	if records > 0 {
		record, err := choose(records)
		if err != nil {
			return 0, -1, false, err
		}
		if record < 0 || record >= records {
			return 0, -1, false, fmt.Errorf("luke sanctum %d card set record %d outside [0,%d)", mapID, record, records)
		}
		if start, usable := runtimeRaidLukeSanctumMazeRecordGroup(cardSetMapInfo, record, mapID, mazeCount); usable {
			return start, record, true, nil
		}
	}
	groups := runtimeRaidLukeSanctumMazeGroups(cardSetMapInfo, mazeCount)
	if len(groups) == 0 {
		return 0, -1, false, nil
	}
	position, err := choose(len(groups))
	if err != nil {
		return 0, -1, false, err
	}
	if position < 0 || position >= len(groups) {
		return 0, -1, false, fmt.Errorf("luke sanctum %d group roll %d outside [0,%d)", mapID, position, len(groups))
	}
	return groups[position], -1, true, nil
}

type runtimeRaidLukeSanctumMaze struct {
	RaidKey    uint32
	GroupStart int
	Clears     int
	Maze       int
	Chosen     bool
	// Record is the [CARD SET MAP INFO] record GroupStart came from, or -1 when
	// the group came from the per-sanctum candidate fallback. Every sanctum of
	// one tarot day reports the same record.
	Record int
}

// runtimeRaidLukeSanctumMazeLocked returns the maze a Luke team raid's next run
// of a phase-one sanctum must use, drawing the sanctum's group on its first
// entry. ok is false for anything that is not such a run, and the caller keeps
// its ordinary maze choice. Caller holds raidMu.
func runtimeRaidLukeSanctumMazeLocked(
	state *runtimeRaidState,
	mapID int,
	mazeCount int,
	choose func(int) (int, error),
) (runtimeRaidLukeSanctumMaze, bool, error) {
	if state == nil || state.TailFlag == 1 || !state.Started || state.LukeConfig == nil ||
		runtimeRaidTeamGraphOwner(*state) != runtimeRaidOwnerLuke ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		return runtimeRaidLukeSanctumMaze{}, false, nil
	}
	if _, sanctum := runtimeRaidLukeSanctumObjectSymbol(mapID); !sanctum {
		return runtimeRaidLukeSanctumMaze{}, false, nil
	}
	groups := runtimeRaidLukeSanctumMazeGroups(state.LukeConfig.CardSetMapInfo, mazeCount)
	if len(groups) == 0 {
		return runtimeRaidLukeSanctumMaze{}, false, nil
	}
	result := runtimeRaidLukeSanctumMaze{RaidKey: state.RaidKey, Record: -1}
	start, chosen := state.Lifecycle.LukeSanctumMazeGroups[mapID]
	if !chosen || start < 0 || start+lukeSanctumClearsPerGroup > mazeCount {
		if choose == nil {
			return runtimeRaidLukeSanctumMaze{}, false, fmt.Errorf("%w: luke sanctum %d records=%d groups=%d",
				errDungeonChoiceRequired, mapID, runtimeRaidLukeTarotCardSetRecords(state.LukeConfig.CardSetMapInfo), len(groups))
		}
		drawnGroup, record, ok, err := runtimeRaidLukeSanctumMazeDrawnGroup(
			state.LukeConfig.CardSetMapInfo, mapID, mazeCount, choose)
		if err != nil {
			return runtimeRaidLukeSanctumMaze{}, false, err
		}
		if !ok {
			return runtimeRaidLukeSanctumMaze{}, false, nil
		}
		start = drawnGroup
		result.Record = record
		if state.Lifecycle.LukeSanctumMazeGroups == nil {
			state.Lifecycle.LukeSanctumMazeGroups = make(map[int]int, 3)
		}
		state.Lifecycle.LukeSanctumMazeGroups[mapID] = start
		state.Lifecycle.Revision++
		result.Chosen = true
	}
	clears := 0
	if objective, ok := state.Lifecycle.Objectives[mapID]; ok {
		clears = objective.ClearCount
	}
	slot := clears
	if slot < 0 {
		slot = 0
	}
	if slot >= lukeSanctumClearsPerGroup {
		slot = lukeSanctumClearsPerGroup - 1
	}
	result.GroupStart = start
	result.Clears = clears
	result.Maze = start + slot
	return result, true, nil
}

// selectRuntimeRaidLukeSanctumMaze is the dungeon-entry side of
// runtimeRaidLukeSanctumMazeLocked.
func (s *Service) selectRuntimeRaidLukeSanctumMaze(
	session *gameSession,
	mapID int64,
	mazeCount int,
) (dungeonMazeSelection, bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return dungeonMazeSelection{}, false, nil
	}
	raidDay := runtimeRaidLukeTarotDay(s.gameplayNow())
	// 官方「所有频道的塔罗卡牌统一相同」⇒ 当天那一套牌只由 tarot day 决定，抽取器
	// 不带圣所：三个圣所落在同一条 [CARD SET MAP INFO] 记录上、各读自己的分量。
	// 回退路径复用同一个「当天第 k 个」抽取器（各圣所迷宫数不同时候选表会不同，
	// 那是回退路径唯一可能的差异，也是它存在的理由）。
	daily := func(limit int) (int, error) {
		return runtimeRaidLukeTarotCardSetRecord(raidDay, limit), nil
	}
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(session.selectedCharacterID)
	// 记录条数在读锁内取：解锁后 state 可能被并发改写（例如攻坚释放时
	// LukeConfig 被置空），只有 maze 这个值副本可以带出锁。
	cardSetRecords := 0
	if state != nil && state.LukeConfig != nil {
		cardSetRecords = runtimeRaidLukeTarotCardSetRecords(state.LukeConfig.CardSetMapInfo)
	}
	maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, int(mapID), mazeCount, daily)
	s.raidMu.Unlock()
	if err != nil || !ok {
		return dungeonMazeSelection{}, false, err
	}
	s.logGameEvent(session, "game-raid-luke-sanctum-maze",
		"char_id", session.selectedCharacterID,
		"raid_key", maze.RaidKey,
		"sanctum", mapID,
		"tarot_day", raidDay,
		"card_set_record", maze.Record,
		"card_set_records", cardSetRecords,
		"group_start", maze.GroupStart,
		"group_chosen_now", maze.Chosen,
		"clears", maze.Clears,
		"maze_index", maze.Maze)
	return dungeonMazeSelection{Index: maze.Maze, Reason: "luke_sanctum_card_set_group"}, true, nil
}

// lukeTarotRefreshHour is when the tarot cards turn over: "每个卢克RAID攻坚日
// 早上6点会刷新塔罗牌，第一阶段的怪物分布和地图机制均按照塔罗牌显示安排"
// (2017-06 / 2018-05 CN guides). Server local time.
const lukeTarotRefreshHour = 6

// runtimeRaidLukeTarotDay is the tarot day a moment belongs to, as YYYYMMDD in
// server local time: 05:59 still reads as the previous day's cards.
func runtimeRaidLukeTarotDay(now time.Time) int {
	day := now.Local().Add(-lukeTarotRefreshHour * time.Hour)
	return day.Year()*10000 + int(day.Month())*100 + day.Day()
}

// runtimeRaidLukeTarotGroup picks one sanctum's index into a per-sanctum list of
// size `groups` for a tarot day: every raid on the server draws the same index
// on the same day, and the sanctums draw independently of each other.
//
// 它现在只服务**图案序号 B**（卢克塔罗牌 1181 记录里每个圣所"今天启动哪套
// 防御体系"，各圣所的 `[PATTERN]` 段数不同：5054/5056 两段、5055 四段）。
// 「走哪组迷宫」自 2026-09-29 起不再用它，改用只按天、三所共用的
// runtimeRaidLukeTarotCardSetRecord。
func runtimeRaidLukeTarotGroup(raidDay int, sanctumMapID int, groups int) int {
	if groups <= 1 {
		return 0
	}
	hash := fnv.New32a()
	_, _ = fmt.Fprintf(hash, "luke-tarot:%d:%d", raidDay, sanctumMapID)
	return int(hash.Sum32() % uint32(groups))
}
