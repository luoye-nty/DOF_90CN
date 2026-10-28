package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 超时空：**每个报通关的人都要记在自己头上**，包括那些"这一格的通关已经被别人记过账"的人。
//
// 真机 2026-10-02 10:20:33（raid 12058625，分片 20261002-006，四人一起进 4955）：
//
//	game-raid-joined-clear-booked char_id=1  booked=true   remaining_hp 1 -> 0
//	game-raid-joined-clear-booked char_id=21 booked=false  remaining_hp 0 -> 0   ← 一条不记
//	game-raid-joined-clear-booked char_id=20 booked=false  remaining_hp 0 -> 0   ← 一条不记
//	game-raid-run-clear-already-credited char_id=22        （走生命周期那一条路）
//	game-raid-phase-clear-registration char_id=22
//	    member_clear_counts=1:1,22:1      ← 4 个人进图，只有 2 个人记到了
//
// 操作员原话：「这次是4人进图 为什么有两个人没给算通关」。
//
// 这是 applyRuntimeRaidDungeonLifecycleEvent（completed 分支）里那处修复的**另一条路径**：
// 队友的通关报告走的是 applyRuntimeRaidJoinedSquadClear，而它的
// `if !booked { return }` 把"这条 run 已经有人记过账"的人也一起挡在记账之外。
//
// 两人时看不出来、四人时露馅的原因：runtimeRaidNotePhaseClearLocked 记的是
// **记账人所在小队**，而操作员 2026-10-02 的定案是「超时空每个小队编制仅可放 1 人」，
// 于是"记整队"在超时空就等于"只记他自己"，同格另外三个人一个都不会被记到。
func TestFiendWarJoinedClearCountsEveryReporterNotJustTheBooker(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	const (
		areaID    = 12
		dungeonID = 4960
		bookerID  = 1001 // 1 队：第一个报通关、把这一格那一点 HP 花掉的人
		lateID    = 1002 // 2 队：同格里后报通关的人（真机上的 char 20 / char 21）
	)
	newFiendWarBattleForSeatTest(t, state, areaID, dungeonID)
	state.Lifecycle.FiendWar.Areas[areaID] = raid.FiendWarAreaState{Monster: raid.FiendWarUnit{ID: 1, Type: 1}}
	phase := state.Lifecycle.Phase
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		dungeonID: {Phase: phase, MapID: dungeonID, State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1},
	}
	state.Lifecycle.RunClearCredits = map[runtimeRaidDungeonRunKey]uint64{}

	binding, err := service.beginRuntimeRaidDungeon(bookerID, dungeonID, 1)
	if err != nil {
		t.Fatalf("开出这一格的 run：%v", err)
	}
	runKey := runtimeRaidDungeonRunKey{}
	for key := range state.Lifecycle.Dungeons {
		if key.MapID == dungeonID {
			runKey = key
		}
	}
	if runKey.MapID != dungeonID {
		t.Fatalf("账本里没有副本 %d 的 run：%+v", dungeonID, state.Lifecycle.Dungeons)
	}

	// 第一个人已经报过通关。这里**直接铺出他留下的账本状态**，而不是调用
	// applyRuntimeRaidJoinedSquadClear —— 后者在 booked 之后会走
	// publishRuntimeRaidObjectiveProgress → releaseRuntimeRaidStaleGateOccupancy，
	// 而单元 fixture 里没有"活着持有这条 run 的会话"，那条 run 会被当成过期回收。
	// （同一取舍见 TestFiendWarOwnerCompletionSucceedsAfterASquadmateAlreadyBookedTheClear。）
	state.Lifecycle.Objectives[dungeonID] = runtimeRaidDungeonObjective{
		Phase: phase, MapID: dungeonID, State: raid.AntonDungeonClosed,
		MaxHP: 1, RemainingHP: 0, ClearCount: 1,
	}
	state.Lifecycle.RunClearCredits[runKey] = binding.LifecycleToken

	// 第二个人报通关。他的 localToken 与开局人**不同**（超时空每个成员各持自己的
	// token），但这条 run 是同一把钥匙 —— 正是"一条 run 一次通关"要保护的东西。
	lateSession := &gameSession{
		conn:                &bufferConn{},
		connID:              "fiendwar-joined-late",
		selectedCharacterID: lateID,
		characterGeneration: 1,
	}
	service.applyRuntimeRaidJoinedSquadClear(lateSession, binding, binding.LifecycleToken+1)

	// [本用例的核心] 他必须被记上。
	if got := state.Lifecycle.PhaseClearCounts[lateID]; got < 1 {
		t.Fatalf("同格同伴的通关数没被记上：PhaseClearCounts[%d]=%d（想要 >=1）；全表=%v\n"+
			"（真机 2026-10-02 10:20:33 就是这两行 booked=false —— 四人进图只有两个人记到）",
			lateID, got, state.Lifecycle.PhaseClearCounts)
	}
	// 发奖门槛那本账也要记：只记战况表那一列会出现"显示 1/1 却拿不到奖励"。
	if got := state.Lifecycle.FiendWarClearCounts[lateID]; got < 1 {
		t.Fatalf("同格同伴的发奖门槛没被记上：FiendWarClearCounts[%d]=%d（想要 >=1）；全表=%v",
			lateID, got, state.Lifecycle.FiendWarClearCounts)
	}
	// 「一条 run 一次通关」不许被破坏：不能再扣这一格第二点 HP。
	if remaining := state.Lifecycle.Objectives[dungeonID].RemainingHP; remaining != 0 {
		t.Fatalf("这一格的 HP 被重复扣了：RemainingHP=%d，want 0", remaining)
	}
	if credited, ok := state.Lifecycle.RunClearCredits[runKey]; !ok || credited != binding.LifecycleToken {
		t.Fatalf("这条 run 的 clear 记账被改动了：%v/%t（want token=%d）",
			state.Lifecycle.RunClearCredits, ok, binding.LifecycleToken)
	}
	// 重复报同一笔（同一个本地 token）必须是空操作 —— 记在 ClosedDungeonAttempts 上。
	service.applyRuntimeRaidJoinedSquadClear(lateSession, binding, binding.LifecycleToken+1)
	if got := state.Lifecycle.PhaseClearCounts[lateID]; got != 1 {
		t.Fatalf("同一个人的同一笔报告被记了两次：PhaseClearCounts[%d]=%d；全表=%v",
			lateID, got, state.Lifecycle.PhaseClearCounts)
	}

	// ── 真机 2026-10-02 11:32:16 的形状（raid 12058646，分片 009，四人局） ──────
	//
	// 那一次**修过一版之后**还是漏了后两个人，原因是判据只看 RunClearCredits：
	// 生命周期那条路处理完成时会把 run 从 Lifecycle.Dungeons 里删掉，后报的人于是
	// 反查不到 owner token，runKey 退化成"他自己小队的座位"，runCredited 就成了 false。
	// 这里把那一步补上：账本里的 run 消失 + 第三个人来报。
	const thirdID = 1003
	state.Members = append(state.Members,
		runtimeRaidMemberState{CharID: thirdID, Name: "third", GroupIndex: 3, SlotOrder: 0})
	state.FrozenMembers = append(state.FrozenMembers,
		runtimeRaidMemberState{CharID: thirdID, Name: "third", GroupIndex: 3, SlotOrder: 0})
	delete(state.Lifecycle.Dungeons, runKey)
	if _, still := state.Lifecycle.Dungeons[runKey]; still {
		t.Fatal("fixture 前提没成立：账本里的那条 run 没被删掉")
	}

	thirdSession := &gameSession{
		conn:                &bufferConn{},
		connID:              "fiendwar-joined-third",
		selectedCharacterID: thirdID,
		characterGeneration: 1,
	}
	service.applyRuntimeRaidJoinedSquadClear(thirdSession, binding, binding.LifecycleToken+2)
	if got := state.Lifecycle.PhaseClearCounts[thirdID]; got < 1 {
		t.Fatalf("账本里那条 run 已经消失之后，第三个人的通关又没被记上："+
			"PhaseClearCounts[%d]=%d（想要 >=1）；全表=%v\n"+
			"（真机 2026-10-02 11:32:16：编制 1、2 是 1/1，3、4 还是 0/1）",
			thirdID, got, state.Lifecycle.PhaseClearCounts)
	}
	if got := state.Lifecycle.FiendWarClearCounts[thirdID]; got < 1 {
		t.Fatalf("账本里那条 run 已经消失之后，第三个人的发奖门槛没被记上："+
			"FiendWarClearCounts[%d]=%d；全表=%v",
			thirdID, got, state.Lifecycle.FiendWarClearCounts)
	}
	// 目标依旧不许被再扣一次。
	if remaining := state.Lifecycle.Objectives[dungeonID].RemainingHP; remaining != 0 {
		t.Fatalf("第三个人的报告把这一格的 HP 又扣了一次：RemainingHP=%d，want 0", remaining)
	}
}
