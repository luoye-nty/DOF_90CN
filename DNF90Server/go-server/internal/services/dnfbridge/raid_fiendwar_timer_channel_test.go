package dnfbridge

import (
	"encoding/binary"
	"os"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 超时空的计时显示必须随"人在不在副本里"切换，而客户端把它分成**两个互不相通的
// 存储**（证据：tools/攻坚队两个倒计时_根因与改法_20260916.md:12-25、:80-97）：
//
//	A  0x0254(596) → map<pair<timerType, value0>, {start,end}>，value0 = **地牢 ID**
//	   （就是 0x0249/585 那一行里的 DungeonID）。客户端的门渲染器按门状态查它：
//	   状态 0 → 查 {timerType 1, dungeonID}；状态 1|3 → 查 {timerType 2, dungeonID}；
//	   状态 2 什么都不画。
//	B  0x0255(597) → map<timerType, endMs>，**只看 timerType**，是攻坚剩余时间。
//
// 另有一条按**区域**给战况表格子画的钟：op1458 的 kind 12 行，
// `{u8 area, u8 kind=12, u32 deadline, u32 limit}`（客户端 0x9B19E0 把
// `start = A - B` 落到 AreaRecord+0x4C/0x50/0x54，见
// tools/_tmp_logs/re_badge/FINDINGS2.md:40-59）。
//
// 线上实测（packet_log-20261002-018，raid 12058625，本地 15:33–15:40）：
//
//	597  每个 tick 都发，body `00 7d0d0000` = timerType 0 / 3453 秒 —— 攻坚倒计时**在发**；
//	1458 每个 tick 都发 901 字节 = 90 行（30 区域 × {0,2,12}）—— 但**只有被占的那一格**
//	     kind 12 带真 deadline，其余 29 格全是 `{area,12,0,0}`；
//	596  **一条都没发**（全场 msg_id=596 只有 26 条，全在别的路径）——
//	     通用快照每条路径都是 0x0254 紧跟 0x0255 成对发，超时空的三条"战况表替换"
//	     分支都直接 return 了，把 0x0254 那一半整个漏掉。
//
// 于是：进副本后客户端查存储 A 查不到这一格 → **该副本的倒计时根本不画**；而空闲格
// 每秒被 30 条 `{area,12,0,0}` 断言"这一格的钟是 0"。
//
// 本用例按**客户端视角**判定，三者都要成立：
//
//	进副本前 —— 没有任何区域的 kind 12 钟，也没有任何门倒计时包；597 带攻坚剩余时间；
//	进副本后 —— 被占那一格 kind 12 带真 deadline，且恰好一条 0x0254 指向它的地牢 ID；
//	通关之后 —— kind 12 补一条 `{area,12,0,0}` 把钟撤掉，之后不再发；597 继续在。
func TestFiendWarBoardSwitchesBetweenTheRaidClockAndTheAreaCountdown(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	config, err := raid.LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	// 区域 7 / 副本 4955 是 [DUNGEON LIMIT TIME] 210 的普通区域（PVF 实证）。
	area, ok := battle.AreaForDungeon(fiendWarTimerTestDungeon)
	if !ok || area.Index != fiendWarTimerTestArea {
		t.Fatalf("夹具前提不成立：副本 %d 落在区域 %d (found=%t), want 区域 %d",
			fiendWarTimerTestDungeon, area.Index, ok, fiendWarTimerTestArea)
	}
	if area.LimitSeconds != fixtureFreeAreaLimitSeconds {
		t.Fatalf("区域 %d 的 [DUNGEON LIMIT TIME] = %d, want %d",
			area.Index, area.LimitSeconds, fixtureFreeAreaLimitSeconds)
	}
	// 真机上"这一格能不能进"看它有没有怪：先放一只，再把 1 队走进去。
	battle.Areas[area.Index] = raid.FiendWarAreaState{Enemy: raid.FiendWarUnit{ID: 1, Type: 1}}
	battle.GroupAreas[1] = area.Index

	const (
		raidKey         = uint32(0x00c80451)
		raidPhaseLength = 3600 * time.Second
	)
	now := time.Now().UTC().Truncate(time.Second)
	state := runtimeRaidState{
		RaidKey:       raidKey,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Started:       true,
		EntryCostPaid: true,
		Members:       []runtimeRaidMemberState{{CharID: 1, GroupIndex: 1, SlotOrder: 0}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1, GroupIndex: 1, SlotOrder: 0}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:         runtimeRaidLifecyclePhaseActive,
			Phase:          runtimeRaidDungeonPhaseOne,
			PhaseStartedAt: now,
			PhaseDeadline:  now.Add(raidPhaseLength),
			Objectives:     map[int]runtimeRaidDungeonObjective{},
			FiendWar:       battle,
		},
	}
	for id := range battle.Config.Areas {
		if dungeon := battle.Config.Areas[id].Dungeon; dungeon > 0 {
			state.Lifecycle.Objectives[dungeon] = runtimeRaidDungeonObjective{
				Phase: runtimeRaidDungeonPhaseOne, MapID: dungeon,
				State: raid.AntonDungeonHide, MaxHP: 1,
			}
		}
	}
	syncRuntimeFiendWarObjectives(&state)

	// --- 进副本前 -------------------------------------------------------------
	before, err := runtimeFiendWarBoardPackets(state, runtimeRaidState{}, fiendWarTimerTestViewer, now)
	if err != nil {
		t.Fatalf("board packets (before entering): %v", err)
	}
	if rows := fixtureKind12Rows(t, before); len(rows) != 0 {
		t.Fatalf("还没进图就有 %d 条 kind 12 钟（%+v）：空闲格不得断言「这一格的副本倒计时是 0」，"+
			"否则客户端按 0 画，0x0255 的攻坚倒计时切不回来", len(rows), rows)
	}
	if got := fixtureRaidTimerBodies(t, before); len(got) != 0 {
		t.Fatalf("还没进图就发了 %d 条门倒计时 0x0254：%+v", len(got), got)
	}
	if remaining, ok := fixtureRaidRemainSeconds(t, before); !ok || remaining != uint32(raidPhaseLength/time.Second) {
		t.Fatalf("进副本前的 0x0255 = %d 秒 (found=%t), want %d：攻坚倒计时必须一直在",
			remaining, ok, raidPhaseLength/time.Second)
	}
	// 进图前的快照 = "客户端上一帧看到的东西"。它必须在 Enter **之前**取，否则
	// deadline 的变化被自己吃掉，0x0254 与 kind 12 都不会发。
	previous := cloneRuntimeRaidState(state)

	// --- 进副本 ---------------------------------------------------------------
	if err := battle.Enter(1, fiendWarTimerTestDungeon, now); err != nil {
		t.Fatalf("enter dungeon %d: %v", fiendWarTimerTestDungeon, err)
	}
	syncRuntimeFiendWarObjectives(&state)
	// 真机进图时逐人会写一笔账本（game-fiendwar-member-area-marked）；Enter 自己不写，
	// 所以这里按生产形状补上。
	battle.MemberAreas[fiendWarTimerTestViewer] = area.Index
	deadline := battle.Areas[area.Index].Deadline
	if deadline.IsZero() {
		t.Fatal("Enter 之后这一格没有 deadline：后面的断言全无意义")
	}

	inside, err := runtimeFiendWarBoardPackets(state, previous, fiendWarTimerTestViewer, now)
	if err != nil {
		t.Fatalf("board packets (inside): %v", err)
	}
	rows := fixtureKind12Rows(t, inside)
	if len(rows) != 1 || rows[0].area != fiendWarTimerTestArea {
		t.Fatalf("进图后的 kind 12 行 = %+v, want 恰好一条、区域 %d", rows, fiendWarTimerTestArea)
	}
	if rows[0].deadline != uint32(deadline.Unix()) || rows[0].duration != fixtureFreeAreaLimitSeconds {
		t.Fatalf("进图后区域 %d 的 kind 12 = {deadline %d, duration %d}, want {%d, %d}",
			fiendWarTimerTestArea, rows[0].deadline, rows[0].duration,
			uint32(deadline.Unix()), fixtureFreeAreaLimitSeconds)
	}
	timers := fixtureRaidTimerBodies(t, inside)
	if len(timers) != 1 {
		t.Fatalf("进图后的门倒计时 0x0254 = %+v, want 恰好一条（通用路径每一条都是 0x0254 紧跟 0x0255）",
			timers)
	}
	if timers[0].timerType != 1 || timers[0].value0 != fiendWarTimerTestDungeon ||
		timers[0].value1 != uint32(deadline.Unix()) || timers[0].value2 != fixtureFreeAreaLimitSeconds {
		t.Fatalf("门倒计时 = %+v, want {type 1, value0 %d(地牢ID), value1 %d, value2 %d}",
			timers[0], fiendWarTimerTestDungeon, uint32(deadline.Unix()), fixtureFreeAreaLimitSeconds)
	}
	// 进图后 HUD 上那个「剩余时间」必须是**这一格**的倒计时，不是整个攻坚的。
	// 真机 2026-10-02 17:35 的截图就是这条没生效：进图后仍然写着 00:59:39。
	if remaining, ok := fixtureRaidRemainSeconds(t, inside); !ok || remaining != fixtureFreeAreaLimitSeconds {
		t.Fatalf("进图后 0x0255 = %d 秒 (found=%t), want %d（这一格的 [DUNGEON LIMIT TIME]）—— "+
			"HUD 的「剩余时间」读的是 0x0255 的存储 B，存储 A(0x0254) 画的是战况表上的按钮，"+
			"玩家在副本里时那张表是关着的，所以补 0x0254 照不到这个显示",
			remaining, ok, fixtureFreeAreaLimitSeconds)
	}
	// 同一时刻、**没进图**的那一位必须还是攻坚倒计时：切换是按收件人算的。
	otherParty, err := runtimeFiendWarBoardPackets(state, previous, fiendWarTimerTestOtherParty, now)
	if err != nil {
		t.Fatalf("board packets (other party): %v", err)
	}
	if remaining, ok := fixtureRaidRemainSeconds(t, otherParty); !ok || remaining != uint32(raidPhaseLength/time.Second) {
		t.Fatalf("没进图的队友 0x0255 = %d 秒 (found=%t), want %d：切换必须是按收件人的，不能全队一起改",
			remaining, ok, raidPhaseLength/time.Second)
	}

	// 账本区域滞后时（"走不动"那轮的病根）也必须能切：拿掉 MemberAreas 之后要靠
	// 小队的区域兜住，否则这种局面下 HUD 又回到攻坚倒计时。
	delete(battle.MemberAreas, fiendWarTimerTestViewer)
	fallback, err := runtimeFiendWarBoardPackets(state, previous, fiendWarTimerTestViewer, now)
	if err != nil {
		t.Fatalf("board packets (ledger cleared): %v", err)
	}
	if remaining, ok := fixtureRaidRemainSeconds(t, fallback); !ok || remaining != fixtureFreeAreaLimitSeconds {
		t.Fatalf("账本区域缺失时 0x0255 = %d 秒 (found=%t), want %d：还得靠小队的区域兜住",
			remaining, ok, fixtureFreeAreaLimitSeconds)
	}
	battle.MemberAreas[fiendWarTimerTestViewer] = area.Index

	// --- 下一个 tick（状态没变） ----------------------------------------------
	previousTicks := cloneRuntimeRaidState(state)
	ticks, err := runtimeFiendWarBoardPackets(state, previousTicks, fiendWarTimerTestViewer, now.Add(time.Second))
	if err != nil {
		t.Fatalf("board packets (tick): %v", err)
	}
	if rows := fixtureKind12Rows(t, ticks); len(rows) != 1 {
		t.Fatalf("心跳里区域 %d 的 kind 12 行 = %+v, want 仍然一条（钟要每个 tick 重申）",
			fiendWarTimerTestArea, rows)
	}
	if got := fixtureRaidTimerBodies(t, ticks); len(got) != 0 {
		t.Fatalf("心跳里重复发了 %d 条 0x0254：%+v —— 若本频道的分派走「相对」分支会把倒计时钉在满值",
			len(got), got)
	}
	// 心跳里副本钟要跟着走：deadline 没变、now 前进 1 秒 → 少 1 秒。
	if remaining, ok := fixtureRaidRemainSeconds(t, ticks); !ok || remaining != fixtureFreeAreaLimitSeconds-1 {
		t.Fatalf("心跳里 0x0255 = %d 秒 (found=%t), want %d（副本钟要一秒一秒地走）",
			remaining, ok, fixtureFreeAreaLimitSeconds-1)
	}

	// --- 通关 -----------------------------------------------------------------
	previousCleared := cloneRuntimeRaidState(state)
	if err := battle.Clear(fiendWarTimerTestDungeon, now.Add(time.Minute)); err != nil {
		t.Fatalf("clear dungeon %d: %v", fiendWarTimerTestDungeon, err)
	}
	syncRuntimeFiendWarObjectives(&state)

	clearedAt := now.Add(time.Minute)
	cleared, err := runtimeFiendWarBoardPackets(state, previousCleared, fiendWarTimerTestViewer, clearedAt)
	if err != nil {
		t.Fatalf("board packets (cleared): %v", err)
	}
	rows = fixtureKind12Rows(t, cleared)
	if len(rows) != 1 || rows[0].area != fiendWarTimerTestArea ||
		rows[0].deadline != 0 || rows[0].duration != 0 {
		t.Fatalf("通关后的 kind 12 行 = %+v, want 恰好一条区域 %d 的 {0,0} 撤钟行", rows, fiendWarTimerTestArea)
	}
	if got := fixtureRaidTimerBodies(t, cleared); len(got) != 0 {
		t.Fatalf("通关后还在发门倒计时 0x0254：%+v", got)
	}
	wantCleared := uint32((raidPhaseLength - time.Minute) / time.Second)
	if remaining, ok := fixtureRaidRemainSeconds(t, cleared); !ok || remaining != wantCleared {
		t.Fatalf("通关后 0x0255 = %d 秒 (found=%t), want %d —— 必须切回（还在走的）攻坚倒计时",
			remaining, ok, wantCleared)
	}

	// --- 再下一个 tick：撤钟行不重复 -------------------------------------------
	previousAfterCancel := cloneRuntimeRaidState(state)
	after, err := runtimeFiendWarBoardPackets(state, previousAfterCancel, fiendWarTimerTestViewer, clearedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("board packets (after cancel): %v", err)
	}
	if rows := fixtureKind12Rows(t, after); len(rows) != 0 {
		t.Fatalf("撤钟行之后还在发 kind 12：%+v —— 客户端会每秒被重新断言一次", rows)
	}
	if remaining, ok := fixtureRaidRemainSeconds(t, after); !ok || remaining != wantCleared-1 {
		t.Fatalf("撤钟后的 0x0255 = %d 秒 (found=%t), want %d", remaining, ok, wantCleared-1)
	}
}

// fixtureFreeAreaLimitSeconds 是 [DUNGEON LIMIT TIME] 里普通区域的时限（PVF 实证 210）。
const fixtureFreeAreaLimitSeconds = 210

const (
	fiendWarTimerTestArea    = 7
	fiendWarTimerTestDungeon = 4955
	// fiendWarTimerTestViewer 是 HUD 上那个「剩余时间」的收件人。0 = 不看任何人。
	fiendWarTimerTestViewer     = 1
	fiendWarTimerTestOtherParty = 2
)

// fixtureKind12Row 是一条解码后的 op1458 kind 12 行。
type fixtureKind12Row struct {
	area     int
	deadline uint32
	duration uint32
}

// fixtureKind12Rows 按 op1458 的线上格式（u8 count + rows{u8 area,u8 kind,u32,u32}，
// 每 255 行分片）挑出 kind 12 的行。空行（0,0）也返回 —— 它正是"撤钟"信号。
func fixtureKind12Rows(t *testing.T, packets []runtimeRaidWireNotification) []fixtureKind12Row {
	t.Helper()
	var out []fixtureKind12Row
	for _, packet := range packets {
		if packet.msgID != 1458 || len(packet.body) == 0 {
			continue
		}
		body := packet.body
		count := int(body[0])
		if len(body) != 1+count*10 {
			t.Fatalf("op1458 body 长度 = %d, count = %d（每行 10 字节）", len(body), count)
		}
		for row := 0; row < count; row++ {
			raw := body[1+row*10 : 1+(row+1)*10]
			if raw[1] != 12 {
				continue
			}
			out = append(out, fixtureKind12Row{
				area:     int(raw[0]),
				deadline: binary.LittleEndian.Uint32(raw[2:6]),
				duration: binary.LittleEndian.Uint32(raw[6:10]),
			})
		}
	}
	return out
}

// fixtureRaidTimer 是一条解码后的 0x0254 门倒计时。
type fixtureRaidTimer struct {
	timerType byte
	value0    uint32
	value1    uint32
	value2    uint32
}

func fixtureRaidTimerBodies(t *testing.T, packets []runtimeRaidWireNotification) []fixtureRaidTimer {
	t.Helper()
	var out []fixtureRaidTimer
	for _, packet := range packets {
		if packet.msgID != raid.RaidTimerMsgID {
			continue
		}
		if len(packet.body) != 13 {
			t.Fatalf("0x0254 body 长度 = %d, want 13（u8 + 3×u32）", len(packet.body))
		}
		out = append(out, fixtureRaidTimer{
			timerType: packet.body[0],
			value0:    binary.LittleEndian.Uint32(packet.body[1:5]),
			value1:    binary.LittleEndian.Uint32(packet.body[5:9]),
			value2:    binary.LittleEndian.Uint32(packet.body[9:13]),
		})
	}
	return out
}

// fixtureRaidRemainSeconds 读最后一条 0x0255 的秒数。
func fixtureRaidRemainSeconds(t *testing.T, packets []runtimeRaidWireNotification) (uint32, bool) {
	t.Helper()
	seconds := uint32(0)
	found := false
	for _, packet := range packets {
		if packet.msgID != raid.RaidRemainTimeMsgID {
			continue
		}
		if len(packet.body) != 5 {
			t.Fatalf("0x0255 body 长度 = %d, want 5（u8 + u32）", len(packet.body))
		}
		seconds, found = binary.LittleEndian.Uint32(packet.body[1:5]), true
	}
	return seconds, found
}

// 门状态比 deadline 晚一帧到位时，倒计时也必须补发。
//
// 真机 2026-10-02 16:28–16:36（raid 12058625）实测：每一次进图，4 个成员的 0x0254
// 都比进图晚 1.4–3.1 秒才发（08:29:56.61 进图 → 08:29:59.72 才发；08:35:55.49 → 08:35:57.07），
// 中间那段时间 HUD 上还是攻坚倒计时（操作员截图 00:58:28）。
//
// 原因就是这条竞态：`battle.Enter` 在 loaded 边界里把 deadline 设好，而 585/0x0254
// 共用的 Objectives 要等 syncRuntimeFiendWarObjectives 才变成 open。只比 deadline 的
// 实现会在那一帧按"状态 2 → 不画"跳过，之后 deadline 再也不变，这条倒计时就永远
// 发不出去了。修法两条：loaded 边界补一次 sync，比较条件改成 (deadline, 门状态) 这一对。
func TestFiendWarAreaCountdownIsSentWhenTheGateStateArrivesAfterTheDeadline(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	config, err := raid.LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	area, ok := battle.AreaForDungeon(fiendWarTimerTestDungeon)
	if !ok {
		t.Fatalf("区域里没有副本 %d", fiendWarTimerTestDungeon)
	}
	now := time.Now().UTC().Truncate(time.Second)
	deadline := now.Add(time.Duration(area.LimitSeconds) * time.Second)
	// Enter 已经跑完：deadline 与占用就位，Objectives 还没同步。
	battle.Areas[area.Index] = raid.FiendWarAreaState{Group: 1, Deadline: deadline}

	const raidKey = uint32(0x00c80453)
	state := runtimeRaidState{
		RaidKey:       raidKey,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Members:       []runtimeRaidMemberState{{CharID: 1, GroupIndex: 1}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1, GroupIndex: 1}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:         runtimeRaidLifecyclePhaseActive,
			Phase:          runtimeRaidDungeonPhaseOne,
			PhaseStartedAt: now,
			PhaseDeadline:  now.Add(time.Hour),
			Objectives: map[int]runtimeRaidDungeonObjective{
				fiendWarTimerTestDungeon: {
					Phase: runtimeRaidDungeonPhaseOne, MapID: fiendWarTimerTestDungeon,
					State: raid.AntonDungeonHide, MaxHP: 1,
				},
			},
			FiendWar: battle,
		},
	}

	// 第一帧：状态还是 hide(2)，客户端按规则不画，服务器也不该发。
	first, err := runtimeFiendWarBoardPackets(state, runtimeRaidState{}, 0, now)
	if err != nil {
		t.Fatalf("board packets (state still hidden): %v", err)
	}
	if rows := fixtureKind12Rows(t, first); len(rows) != 1 {
		t.Fatalf("第一帧的 kind 12 行 = %+v, want 那条带真 deadline 的行", rows)
	}
	if got := fixtureRaidTimerBodies(t, first); len(got) != 0 {
		t.Fatalf("门状态还是 hide 就发了 0x0254：%+v（客户端本来就不画）", got)
	}

	// 第二帧：Objectives 同步成 open，**deadline 一个字节都没动**。
	previous := cloneRuntimeRaidState(state)
	state.Lifecycle.Objectives[fiendWarTimerTestDungeon] = runtimeRaidDungeonObjective{
		Phase: runtimeRaidDungeonPhaseOne, MapID: fiendWarTimerTestDungeon,
		State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1,
	}
	second, err := runtimeFiendWarBoardPackets(state, previous, 0, now.Add(time.Second))
	if err != nil {
		t.Fatalf("board packets (gate state arrived): %v", err)
	}
	timers := fixtureRaidTimerBodies(t, second)
	if len(timers) != 1 {
		t.Fatalf("门状态到位、deadline 未变时 0x0254 = %+v, want 恰好一条 —— "+
			"只比 deadline 的实现会在这一帧静默跳过，那条副本倒计时就永远发不出去", timers)
	}
	if timers[0].timerType != 1 || timers[0].value0 != fiendWarTimerTestDungeon {
		t.Fatalf("门状态到位的 0x0254 = %+v, want {type 1, value0 %d}", timers[0], fiendWarTimerTestDungeon)
	}
}
