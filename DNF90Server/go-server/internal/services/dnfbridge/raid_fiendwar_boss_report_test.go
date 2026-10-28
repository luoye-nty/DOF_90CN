package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
)

// 罗什的机制上报必须**收得下队长的**（他手上是开局人的 token）。
//
// 真机 2026-10-02 11:49:36~11:58:21（raid 12058625，分片 010，两两进罗什）：
//
//	4978（上）：run 由 char 21 开（dungeon_lifecycle_token=11），队长 char 1 adopt；
//	4953（下）：run 由 char 22 开（dungeon_lifecycle_token=13），队长 char 20 adopt；
//	两侧客户端**都在**报 op1511 kind=1 value=5（char 1 六条、char 20 六条），
//	服务端一条 rejected 都没有 —— 因为 handleRuntimeFiendWarBoss 里那条
//	`progress.LifecycleToken != runtime.lifecycleToken` 是**静默 return**。
//
// ⇒ 两侧的球永远清不掉、boss pattern state 停在 1111、两侧 state 停在 5
//
//	⇒ 客户端（state=5）反复刷同一颗球：操作员看到的「一直循环第一个球 不切换」。
//
// 本用例把两侧各自的 adopt 形状摆出来，要求：**两队各打掉自己手上那颗球**之后，
//
//	机制条按颗数掉位（1111 → 111 → 11），而且**谁报的球只影响谁自己**
//	（两侧拿的是不同的球，不是"同一颗球各报一次"）。
func TestFiendWarBossMechanismFromTheAdoptingLeaderAdvancesThePattern(t *testing.T) {
	const (
		raidKey     = uint32(0x00c80430)
		topDungeon  = 4978
		botDungeon  = 4953
		topRunToken = uint64(11) // 开局人（队友）的 token
		botRunToken = uint64(13)
		topLocal    = uint64(9) // 队长自己那份本地 token —— 与 run 的**不同**
		botLocal    = uint64(12)
		// 客户端 0x9D80A0 那张表：5=宝石/虚无 6=火 7=冰 8=风。
		topClone      = 5
		botClone      = 6
		patternStart  = 1111
		patternAfter1 = 111 // 打掉 1000 那一颗
		patternAfter2 = 11  // 再打掉 100 那一颗
	)
	config := &raid.FiendWarBattleConfig{
		Raid:        runtimeRaidBridgeTestAntonConfig(),
		WaitingTown: 98,
		WaitingArea: 2,
		Areas: map[int]raid.FiendWarArea{
			2:  {Index: 2},
			30: {Index: 30, Dungeon: topDungeon, Kind: "top boss", Symbol: 200},
			31: {Index: 31, Dungeon: botDungeon, Kind: "bot boss", Symbol: 201},
		},
		Symbols: map[string]uint32{
			"top boss state": 200, "bot boss state": 201,
			"top boss hp": 202, "bot boss hp": 203,
			"top boss view hp": 204, "bot boss view hp": 205,
			"boss pattern state": 206,
		},
		// NewFiendWarBattle 要求规则非空；这份 fixture 只走 BossReport，规则用不上。
		Rules: []raid.FiendWarRule{{}},
	}
	battle, err := raid.NewFiendWarBattle(config)
	if err != nil {
		t.Fatalf("fixture 战场: %v", err)
	}
	battle.Symbols[200], battle.Symbols[201] = topClone, botClone
	battle.Symbols[206] = patternStart
	battle.Areas[30] = raid.FiendWarAreaState{Group: 1}
	battle.Areas[31] = raid.FiendWarAreaState{Group: 2}

	topSeat := runtimeRaidDungeonSeat{Group: 2} // 开局人 char 21 所在编制
	botSeat := runtimeRaidDungeonSeat{Group: 4} // 开局人 char 22 所在编制
	members := []runtimeRaidMemberState{
		{CharID: 1, Name: "top-leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 21, Name: "top-opener", GroupIndex: 2, SlotOrder: 0},
		{CharID: 20, Name: "bot-leader", GroupIndex: 3, SlotOrder: 0},
		{CharID: 22, Name: "bot-opener", GroupIndex: 4, SlotOrder: 0},
	}
	topProgress := runtimeRaidDungeonProgress{
		Phase: 1, MapID: topDungeon, Seat: topSeat, CharacterID: 21,
		LifecycleToken: topRunToken, Status: runtimeRaidDungeonProgressLoaded,
	}
	botProgress := runtimeRaidDungeonProgress{
		Phase: 1, MapID: botDungeon, Seat: botSeat, CharacterID: 22,
		LifecycleToken: botRunToken, Status: runtimeRaidDungeonProgressLoaded,
	}
	state := &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      1,
		Started:       true,
		EntryCostPaid: true,
		Members:       append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecyclePhaseActive,
			Phase:    1,
			FiendWar: battle,
			// 一个区域一条 run：座位是**开局人**的，队长靠它反查到同一条 run。
			GroupOccupancy: map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding{
				topSeat: {RaidKey: raidKey, Phase: 1, MapID: topDungeon, LifecycleToken: topRunToken},
				botSeat: {RaidKey: raidKey, Phase: 1, MapID: botDungeon, LifecycleToken: botRunToken},
			},
			Dungeons: map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{
				{MapID: topDungeon, Seat: topSeat}: topProgress,
				{MapID: botDungeon, Seat: botSeat}: botProgress,
			},
		},
	}
	service := &Service{
		raids:        map[uint32]*runtimeRaidState{raidKey: state},
		gameSessions: map[uint16]*gameSession{},
	}
	newPartySession := func(charID, openerID uint16, dungeonID int, localToken, runToken uint64) *gameSession {
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "fiendwar-boss-report-" + strconv.Itoa(int(charID)),
			channel:             channelcatalog.Channel{ID: 184, Type: 42},
			selectedCharacterID: charID,
			characterGeneration: 1,
		}
		owner := &runtimePartyDungeonOwner{
			partyID:  uint16(20 + charID),
			leaderID: charID,
			members: []runtimePartyDungeonMember{
				{slot: 0, userID: charID, generation: 1},
				{slot: 1, userID: openerID, generation: 1},
			},
		}
		runtime := &runtimeDungeonState{
			Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(dungeonID)},
			lifecycleToken: localToken,
			raidDungeon: runtimeRaidDungeonBinding{
				RaidKey: raidKey, Phase: 1, MapID: dungeonID,
				LifecycleToken: runToken, loadedNotified: true,
			},
			partyDungeonOwner: owner,
		}
		session.dungeon.runtime = runtime
		service.gameSessions[charID] = session
		return session
	}

	topSession := newPartySession(1, 21, topDungeon, topLocal, topRunToken)
	botSession := newPartySession(20, 22, botDungeon, botLocal, botRunToken)

	body := func(kind, value int) []byte {
		payload := make([]byte, 8)
		binary.LittleEndian.PutUint32(payload[:4], uint32(kind))
		binary.LittleEndian.PutUint32(payload[4:], uint32(value))
		return payload
	}

	// 上半侧（队长 adopt 开局人的 run）：这一条必须被收下，并且只清掉**他自己**
	// 那颗球（1000 位）、只给他自己补一颗新的，下半侧一点不动。
	if err := service.handleRuntimeFiendWarBoss(topSession, body(1, topClone)); err != nil {
		t.Fatalf("上半侧机制上报: %v", err)
	}
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != patternAfter1 {
		t.Fatalf("上半侧打掉自己那颗球之后机制条 = %d，want %d", got, patternAfter1)
	}
	if got := state.Lifecycle.FiendWar.Value("top boss state"); got < 7 || got > 8 {
		t.Fatalf("上半侧没有补到新球：top boss state=%d，want 7/8（池里只剩冰、风）", got)
	}
	if got := state.Lifecycle.FiendWar.Value("bot boss state"); got != botClone {
		t.Fatalf("上半侧打自己的球却动了对面：bot boss state=%d，want %d", got, botClone)
	}
	// 下半侧（同样是队长 adopt）：打掉它那颗火球，池子只剩上一侧没拿的那一颗。
	if err := service.handleRuntimeFiendWarBoss(botSession, body(1, botClone)); err != nil {
		t.Fatalf("下半侧机制上报: %v", err)
	}
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != patternAfter2 {
		t.Fatalf("下半侧打掉自己那颗球之后机制条 = %d，want %d", got, patternAfter2)
	}
	topAfter := state.Lifecycle.FiendWar.Value("top boss state")
	botAfter := state.Lifecycle.FiendWar.Value("bot boss state")
	if botAfter < 7 || botAfter > 8 || botAfter == topAfter {
		t.Fatalf("下半侧没有拿到剩下那颗不同的球：top=%d bot=%d", topAfter, botAfter)
	}
	// 同一侧把**上一颗**再报一遍（客户端重发/迟到）不能重复扣球：那一颗早就不在池子里了。
	if err := service.handleRuntimeFiendWarBoss(topSession, body(1, topClone)); err != nil {
		t.Fatalf("重复上报: %v", err)
	}
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != patternAfter2 {
		t.Fatalf("重复上报又扣了一次机制条：%d，want %d", got, patternAfter2)
	}
	_ = time.Second
}

// **队员**报机制：必须当真（旧代码要求"只有队长能报"，操作员 2026-10-02 质疑过这一点）。
func TestFiendWarBossMechanismFromANonHostStillCounts(t *testing.T) {
	const raidKey = uint32(0x00c80431)
	config := &raid.FiendWarBattleConfig{
		Raid:        runtimeRaidBridgeTestAntonConfig(),
		WaitingTown: 98,
		WaitingArea: 2,
		Areas: map[int]raid.FiendWarArea{
			2:  {Index: 2},
			30: {Index: 30, Dungeon: 4978, Kind: "top boss", Symbol: 200},
			31: {Index: 31, Dungeon: 4953, Kind: "bot boss", Symbol: 201},
		},
		Symbols: map[string]uint32{
			"top boss state": 200, "bot boss state": 201,
			"boss pattern state": 206,
		},
		Rules: []raid.FiendWarRule{{}},
	}
	battle, err := raid.NewFiendWarBattle(config)
	if err != nil {
		t.Fatalf("fixture 战场: %v", err)
	}
	battle.Symbols[200], battle.Symbols[201], battle.Symbols[206] = 5, 6, 1111
	battle.Areas[30] = raid.FiendWarAreaState{Group: 1}
	seat := runtimeRaidDungeonSeat{Group: 2}
	progress := runtimeRaidDungeonProgress{
		Phase: 1, MapID: 4978, Seat: seat, CharacterID: 21,
		LifecycleToken: 11, Status: runtimeRaidDungeonProgressLoaded,
	}
	state := &runtimeRaidState{
		RaidKey: raidKey, LeaderID: 1, Started: true, EntryCostPaid: true,
		Members: []runtimeRaidMemberState{
			{CharID: 1, GroupIndex: 1}, {CharID: 21, GroupIndex: 2},
		},
		FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1, GroupIndex: 1}, {CharID: 21, GroupIndex: 2},
		},
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive, Phase: 1, FiendWar: battle,
			GroupOccupancy: map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding{
				seat: {RaidKey: raidKey, Phase: 1, MapID: 4978, LifecycleToken: 11},
			},
			Dungeons: map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{
				{MapID: 4978, Seat: seat}: progress,
			},
		},
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: state}, gameSessions: map[uint16]*gameSession{}}
	// char 21 是这条 run 的开局人，但不是这支临时队伍的队长。
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "fiendwar-boss-report-nonhost",
		channel:             channelcatalog.Channel{ID: 184, Type: 42},
		selectedCharacterID: 21,
		characterGeneration: 1,
	}
	session.dungeon.runtime = &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 4978},
		lifecycleToken: 11,
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey: raidKey, Phase: 1, MapID: 4978, LifecycleToken: 11, loadedNotified: true,
		},
		partyDungeonOwner: &runtimePartyDungeonOwner{
			partyID: 21, leaderID: 1,
			members: []runtimePartyDungeonMember{{slot: 0, userID: 1, generation: 1}, {slot: 1, userID: 21, generation: 1}},
		},
	}
	service.gameSessions[21] = session
	payload := make([]byte, 8)
	binary.LittleEndian.PutUint32(payload[:4], 1)
	binary.LittleEndian.PutUint32(payload[4:], 5)
	if err := service.handleRuntimeFiendWarBoss(session, payload); err != nil {
		t.Fatalf("队员上报: %v", err)
	}
	// **队员上报必须被当真。** kind=1 必须 value == 这一侧当前的 boss state、
	// 而且那颗球还在共享池里，所以重复报天然被 stale 挡掉 —— 按人区分没有好处，
	// 只会留下"队员打掉那颗球、上报被丢掉、球永远不切换"的坑。
	// 操作员 2026-10-02 的顾虑原话：「只有队长能报是什么意思？队员走机制就要卡住？」
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != 111 {
		t.Fatalf("队员打掉自己那颗球（5）之后机制条 = %d，want 111", got)
	}
	if got := state.Lifecycle.FiendWar.Value("top boss state"); got < 7 || got > 8 {
		t.Fatalf("队员没有补到新球：top boss state=%d，want 7/8", got)
	}
	// 同一侧把**已经打掉的**那颗再报一遍（客户端重发）不能重复扣球。
	if err := service.handleRuntimeFiendWarBoss(session, payload); err != nil {
		t.Fatalf("队员重发: %v", err)
	}
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != 111 {
		t.Fatalf("同一侧重复上报又扣了一次机制条：%d，want 111", got)
	}
}

// 不在册的会话（掉线/换人后的残留）上报机制：必须被挡下，而且**要留一行日志**。
func TestFiendWarBossMechanismFromAStaleSessionIsDropped(t *testing.T) {
	const raidKey = uint32(0x00c80432)
	config := &raid.FiendWarBattleConfig{
		Raid:        runtimeRaidBridgeTestAntonConfig(),
		WaitingTown: 98,
		WaitingArea: 2,
		Areas: map[int]raid.FiendWarArea{
			2:  {Index: 2},
			30: {Index: 30, Dungeon: 4978, Kind: "top boss", Symbol: 200},
			31: {Index: 31, Dungeon: 4953, Kind: "bot boss", Symbol: 201},
		},
		Symbols: map[string]uint32{
			"top boss state": 200, "bot boss state": 201,
			"boss pattern state": 206,
		},
		Rules: []raid.FiendWarRule{{}},
	}
	battle, err := raid.NewFiendWarBattle(config)
	if err != nil {
		t.Fatalf("fixture 战场: %v", err)
	}
	battle.Symbols[200], battle.Symbols[201], battle.Symbols[206] = 5, 6, 1111
	battle.Areas[30] = raid.FiendWarAreaState{Group: 1}
	seat := runtimeRaidDungeonSeat{Group: 2}
	progress := runtimeRaidDungeonProgress{
		Phase: 1, MapID: 4978, Seat: seat, CharacterID: 21,
		LifecycleToken: 11, Status: runtimeRaidDungeonProgressLoaded,
	}
	state := &runtimeRaidState{
		RaidKey: raidKey, LeaderID: 1, Started: true, EntryCostPaid: true,
		Members: []runtimeRaidMemberState{
			{CharID: 1, GroupIndex: 1}, {CharID: 21, GroupIndex: 2},
		},
		FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1, GroupIndex: 1}, {CharID: 21, GroupIndex: 2},
		},
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive, Phase: 1, FiendWar: battle,
			GroupOccupancy: map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding{
				seat: {RaidKey: raidKey, Phase: 1, MapID: 4978, LifecycleToken: 11},
			},
			Dungeons: map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{
				{MapID: 4978, Seat: seat}: progress,
			},
		},
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: state}, gameSessions: map[uint16]*gameSession{}}
	// char 21 不在队伍成员表里（generation 对不上 = 换人之前的残留会话）。
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "fiendwar-boss-report-stale",
		channel:             channelcatalog.Channel{ID: 184, Type: 42},
		selectedCharacterID: 21,
		characterGeneration: 7,
	}
	session.dungeon.runtime = &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 4978},
		lifecycleToken: 11,
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey: raidKey, Phase: 1, MapID: 4978, LifecycleToken: 11, loadedNotified: true,
		},
		partyDungeonOwner: &runtimePartyDungeonOwner{
			partyID: 21, leaderID: 1,
			members: []runtimePartyDungeonMember{{slot: 0, userID: 1, generation: 1}, {slot: 1, userID: 21, generation: 1}},
		},
	}
	service.gameSessions[21] = session
	payload := make([]byte, 8)
	binary.LittleEndian.PutUint32(payload[:4], 1)
	binary.LittleEndian.PutUint32(payload[4:], 5)
	if err := service.handleRuntimeFiendWarBoss(session, payload); err != nil {
		t.Fatalf("残留会话上报: %v", err)
	}
	if got := state.Lifecycle.FiendWar.Value("top boss state"); got != 5 {
		t.Fatalf("残留会话的机制上报推进了状态：top boss state=%d，want 5", got)
	}
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != 1111 {
		t.Fatalf("残留会话的机制上报动了机制条：boss pattern state=%d，want 1111", got)
	}
}
