package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// 超时空：队友先把区域那一格的 run 开出来之后，驱动者带着**自己那份** token 来报
// lifecycle 事件，会被归属校验当成非法 token 拒掉 —— 而这一拒是致命的：整场进图
// 被拆，谁都进不去。
//
// 真机 2026-10-01 21:27:11（本工作区短命上线、21:29:13 已回退的那一版）：
//
//	13:27:11.010 game-party-dungeon-entry-member-prepared party_id=4
//	             leader_char_id=1 char_id=20 dungeon_id=4960
//	13:27:11.120 game-raid-dungeon-lifecycle char_id=20 event=selected
//	             dungeon_lifecycle_token=1
//	13:27:11.123 game-dungeon-select-blocked dungeon_id=4960
//	             reason=dungeon_runtime_commit_failed
//	             error=runtime raid dungeon lifecycle token is invalid:
//	                   raid=12058625 phase=1 map=4960 token=2
//	13:27:11.124 game-fiendwar-encounter-entry entered=false
//	13:27:11.124 game-single-raid-ordinary-party-cleared
//	             source=fiendwar_encounter_rejected char_id=1 party_id=4
//
// 之后心跳永远重试 game-fiendwar-encounter-peer-not-ready。
//
// 设计意图是"一场只报一次、由 owner 报"（beginRuntimeRaidDungeonForRuntime 里
// binding.LifecycleToken != runtime.lifecycleToken 那条 return nil），所以这里正确
// 的行为是**静默接受**，不是报错。这个用例钉的就是那条判定
// runtimeRaidDungeonLifecycleReportAccepted —— 生产代码与用例共用同一个谓词。
func TestFiendWarTeammatesRunAcceptsTheSecondMembersOwnTokenReport(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	// 一支小队：1001 是驱动者，1002 是队友（真机上队友先被 prepare）。
	state.Members[1].GroupIndex, state.FrozenMembers[1].GroupIndex = 1, 1

	const (
		areaID    = 12
		dungeonID = 4960
		openerID  = 1002
		driverID  = 1001
	)
	newFiendWarBattleForSeatTest(t, state, areaID, dungeonID)

	// 队友先把这一格的 run 开出来。真机上这一步发生在
	// prepareRuntimePartyDungeonEntry 准备队友的时候。
	opener, err := service.beginRuntimeRaidDungeon(openerID, dungeonID, 1)
	if err != nil {
		t.Fatalf("队友开这一格的 run: %v", err)
	}
	// 驱动者随后提交：必须采用队友那一份，而不是各开一份。
	adopter, err := service.beginRuntimeRaidDungeon(driverID, dungeonID, 2)
	if err != nil {
		t.Fatalf("驱动者没能采用队友的绑定: %v", err)
	}
	if adopter != opener {
		t.Fatalf("驱动者拿到 %+v，want 队友的 %+v", adopter, opener)
	}

	// 驱动者报自己的 selected，事件上带的是**他自己那份** token = 2。
	driverEvent := runtimeRaidDungeonLifecycleEvent{
		Kind:           runtimeRaidDungeonSelected,
		RaidKey:        adopter.RaidKey,
		Phase:          adopter.Phase,
		MapID:          adopter.MapID,
		LifecycleToken: 2,
		CharacterID:    driverID,
	}
	runKey, progress, exists := runtimeRaidRunKeyForAttemptLocked(state, driverID, dungeonID, driverEvent.LifecycleToken)
	if !runtimeRaidDungeonLifecycleReportAccepted(state, runKey, progress, exists, driverEvent) {
		t.Fatalf("队友开的那条 run 不认驱动者(tag=%d)带自己 token 的事件：runKey=%+v progress=%+v exists=%t —— "+
			"notifyRuntimeRaidDungeonLifecycleLocked 会以 errRuntimeRaidDungeonToken 拒掉，"+
			"整场进图被拆，这正是真机 token=2 那条报错",
			driverEvent.LifecycleToken, runKey, progress, exists)
	}

	// owner 本人带着自己的 token 照旧必须被接受（常规那半条不能被我改坏）。
	ownerEvent := runtimeRaidDungeonLifecycleEvent{
		Kind:           runtimeRaidDungeonSelected,
		RaidKey:        opener.RaidKey,
		Phase:          opener.Phase,
		MapID:          opener.MapID,
		LifecycleToken: opener.LifecycleToken,
		CharacterID:    openerID,
	}
	ownerKey, ownerProgress, ownerExists := runtimeRaidRunKeyForAttemptLocked(state, openerID, dungeonID, ownerEvent.LifecycleToken)
	if !runtimeRaidDungeonLifecycleReportAccepted(state, ownerKey, ownerProgress, ownerExists, ownerEvent) {
		t.Fatalf("owner 本人的事件被拒了：runKey=%+v progress=%+v", ownerKey, ownerProgress)
	}

	// owner 用**新** token 重开自己的局（racing duplicate）必须继续被拒：
	// 那条 run 的 owner 就是他本人。
	duplicate := ownerEvent
	duplicate.LifecycleToken = 99
	dupKey, dupProgress, dupExists := runtimeRaidRunKeyForAttemptLocked(state, openerID, dungeonID, duplicate.LifecycleToken)
	if runtimeRaidDungeonLifecycleReportAccepted(state, dupKey, dupProgress, dupExists, duplicate) {
		t.Fatalf("本人用新 token 重开自己的局被放行了：runKey=%+v progress=%+v", dupKey, dupProgress)
	}

	// 完全不存在的 run 依旧必须被拒（state 为空 / 查不到）。
	if runtimeRaidDungeonLifecycleReportAccepted(state, runKey, progress, false, driverEvent) {
		t.Fatal("查不到的 run 被放行了")
	}
}

// 安东/卢克逐字不变：同一形状（队友先开 run、第二个人带自己的 token 来报）在
// **没有超时空战场**时必须继续被拒。这是"超时空另走一套、安东/卢克原样"这条边界的
// 守门用例 —— 它红了就说明那条放松漏到安东/卢克去了。
func TestAntonAndLukeTeammatesRunStillRejectsTheSecondMembersOwnTokenReport(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	// 同一支小队，但 Lifecycle.FiendWar == nil，也就是安东/卢克那条路。
	state.Members[1].GroupIndex, state.FrozenMembers[1].GroupIndex = 1, 1
	if state.Lifecycle.FiendWar != nil {
		t.Fatal("fixture 本意是安东/卢克，却带着超时空战场")
	}

	const (
		gateMapID = 212
		openerID  = 1002
		driverID  = 1001
	)
	opener, err := service.beginRuntimeRaidDungeon(openerID, gateMapID, 1)
	if err != nil {
		t.Fatalf("队友开 run: %v", err)
	}
	adopter, err := service.beginRuntimeRaidDungeon(driverID, gateMapID, 2)
	if err != nil {
		t.Fatalf("第二个人 adopt: %v", err)
	}
	if adopter != opener {
		t.Fatalf("安东/卢克的 adopt 语义变了：拿到 %+v，want %+v", adopter, opener)
	}

	driverEvent := runtimeRaidDungeonLifecycleEvent{
		Kind:           runtimeRaidDungeonSelected,
		RaidKey:        adopter.RaidKey,
		Phase:          adopter.Phase,
		MapID:          adopter.MapID,
		LifecycleToken: 2,
		CharacterID:    driverID,
	}
	runKey, progress, exists := runtimeRaidRunKeyForAttemptLocked(state, driverID, gateMapID, driverEvent.LifecycleToken)
	if runtimeRaidDungeonLifecycleReportAccepted(state, runKey, progress, exists, driverEvent) {
		t.Fatalf("超时空的放松漏到安东/卢克了：runKey=%+v progress=%+v", runKey, progress)
	}
}

// newFiendWarBattleForSeatTest 给一份最小的超时空战场：等待区 2 + 一个带副本的区域，
// 小队已经走进那一格（真机上这是 town_move.go 的账本落地）。只用到座位解析，
// 所以不需要真实 Script.pvf —— 安东/卢克的用例仍然走它们原来的 fixture，一个字不改。
func newFiendWarBattleForSeatTest(t *testing.T, state *runtimeRaidState, areaID, dungeonID int) {
	t.Helper()
	phase, ok := runtimeRaidDungeonPhaseForMap(dungeonID)
	if !ok {
		t.Fatalf("副本 %d 不在超时空的 map 区间里", dungeonID)
	}
	battle, err := raid.NewFiendWarBattle(&raid.FiendWarBattleConfig{
		Raid:        runtimeRaidBridgeTestAntonConfig(),
		WaitingTown: 98,
		WaitingArea: 2,
		Areas: map[int]raid.FiendWarArea{
			2:      {Index: 2},
			areaID: {Index: areaID, Dungeon: dungeonID, Next: []int{2}},
		},
		Rules: []raid.FiendWarRule{{}},
	})
	if err != nil {
		t.Fatalf("超时空战场 fixture: %v", err)
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Phase = phase
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Objectives = nil
	battle.GroupAreas[1] = areaID
}

// 队友开的那条 run 报 loaded 时，战场必须真的把**这一格**标记成"被占"。
//
// 起因是 2026-10-01 23:22~23:23 真机（raid 12058625，分片 034）：组队进图之后能打、
// 能清完，但清完**没有任何反应** —— 单人时一直有的自动回城没了。两条日志：
//
//	15:22:40.369 game-raid-dungeon-loaded-publish-deferred char_id=22 party_id=4
//	             loading_cycle=1 raid_key=12058625 reason=party_loading_release_continues
//	             error=fiendwar dungeon admission rejected
//	15:23:05.986 game-raid-dungeon-completion-publish-failed char_id=22 dungeon_id=4956
//	             op115_sent=true error=fiendwar dungeon not occupied
//
// 链条：applyRuntimeRaidDungeonLifecycleEvent 的 loaded 分支原来用
// `battle.GroupAreas[seat.Group] = battle.MemberArea(报告者, seat.Group)`。
// 报告者**不一定是按下进图的人**：prepareRuntimePartyDungeonEntry 先备队友，
// 所以经常是**队友**把这条 run 开出来（真机 23:22:38 char_id=22 event=selected token=1，
// 驱动者 char 1 是 adopt 的、本地 token=2）。而队友按设计**留在原格** ——
// MemberAreas 钉在他按下进图时站的格子 —— 于是那一行把 GroupAreas[小队] 覆盖成**原格**，
// 紧接着 Enter 就以 "fiendwar dungeon admission rejected" 失败，
// 战场里这一格永远没被标记成被占；通关时 Clear 报 "fiendwar dungeon not occupied"，
// 结算推进不下去，自动回城的整段判定因此永远不成立。
//
// 区域本来就由**副本**决定（AreaForDungeon），不由报告者的实时格子决定。
func TestFiendWarLoadedFromThePinnedFollowerStillOccupiesTheArea(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	// helper 给的是 1001(1 队,队长) 与 1002(2 队)；这里让**队友 1002** 开局。
	const (
		areaID    = 12
		dungeonID = 4960
		openerID  = 1002
	)
	newFiendWarBattleForSeatTest(t, state, areaID, dungeonID)
	if state.Lifecycle.FiendWar == nil {
		t.Fatal("fixture 没有战场")
	}
	battle := state.Lifecycle.FiendWar
	// 区域 12 要"开着"：站着一只守军（与真机的"这一格有怪"一致）。
	battle.Areas[areaID] = raid.FiendWarAreaState{Monster: raid.FiendWarUnit{ID: 1, Type: 1}}
	// 队友按设计留在**原格**（等待区 2）；驱动者那一队已经走进目标区域。
	battle.MemberAreas[openerID] = 2
	battle.GroupAreas[1] = areaID

	binding, err := service.beginRuntimeRaidDungeon(openerID, dungeonID, 1)
	if err != nil {
		t.Fatalf("队友开这一格的 run：%v", err)
	}
	if binding.MapID != dungeonID {
		t.Fatalf("binding=%+v，want map=%d", binding, dungeonID)
	}

	// 队友报 loaded：run 就是他开的，所以他手上的 token 与 binding 一致。
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(
		runtimeRaidDungeonEventForTest(binding, openerID, runtimeRaidDungeonLoaded),
	); err != nil {
		t.Fatalf("队友报 loaded 被战场拒了：%v\n"+
			"（真机 2026-10-01 23:22:40 就是这句 fiendwar dungeon admission rejected；"+
			"战场里这一格没被标记成被占，通关时 Clear 会报 fiendwar dungeon not occupied，"+
			"自动回城整段不触发）", err)
	}

	after := state.Lifecycle.FiendWar
	area, found := after.AreaForDungeon(dungeonID)
	if !found {
		t.Fatalf("剧本里没有拥有副本 %d 的区域", dungeonID)
	}
	if after.Areas[area.Index].Group == 0 {
		t.Fatalf("区域 %d 没有被标记成被占（Areas[%d].Group=%d）：通关结算会失败、不会自动回城",
			area.Index, area.Index, after.Areas[area.Index].Group)
	}
	// 收尾那一步必须真的能过 —— 这就是真机卡住的那一步。
	if err := after.Clear(dungeonID, time.Now().UTC()); err != nil {
		t.Fatalf("清完这一格时战场结算失败：%v", err)
	}
}

// 一个区域共用一条 run：**队友先报通关**把这一格的那一点 HP 花掉之后，
// **开局人**再报通关仍然必须能过 —— 否则这条 run 永远关不掉、开局人永远不自动回城。
//
// 真机 2026-10-01 23:38（raid 12058625，分片 035）就是这条：
//
//	15:38:57.846 game-raid-joined-clear-booked char_id=1
//	             booked=true remaining_hp_before=1 remaining_hp_after=0
//	15:38:57.860 game-raid-dungeon-completion-publish-failed char_id=22 dungeon_id=4954
//	             op115_sent=true error=runtime raid dungeon objective is not open
//	15:38:58.751 game-fiendwar-clear-auto-return char_id=1 ...   ← 只有驱动者回城了
//	（char 22 再也没有回城）
//
// 原因：completed 分支里的超时空那一段**无条件**要求
// `RemainingHP > 0 && State == AntonDungeonOpen`，而队友那一笔已经走
// applyRuntimeRaidJoinedSquadClear 把 HP 花掉、把 RunClearCredits 记上了
// （"一条 run 一次通关"），于是开局人这一笔被判成 errRuntimeRaidDungeonUnavailable。
// 而**只有开局人那一笔**会把这条 run 标成 completed（队友那一笔只记本地）——
// 于是 run 关不掉、开局人也永远不自动回城。
func TestFiendWarOwnerCompletionSucceedsAfterASquadmateAlreadyBookedTheClear(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	const (
		areaID    = 12
		dungeonID = 4960
		openerID  = 1002 // 2 队、被钉在原格：这条 run 由他开
		joinedID  = 1001 // 1 队：adopt 进来、先报 completed 的那位
	)
	newFiendWarBattleForSeatTest(t, state, areaID, dungeonID)
	battle := state.Lifecycle.FiendWar
	battle.Areas[areaID] = raid.FiendWarAreaState{Monster: raid.FiendWarUnit{ID: 1, Type: 1}}
	battle.MemberAreas[openerID] = 2
	battle.GroupAreas[1] = areaID
	phase := state.Lifecycle.Phase
	// 这一格的目标还开着（1 点 HP）。
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		dungeonID: {Phase: phase, MapID: dungeonID, State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1},
	}
	state.Lifecycle.RunClearCredits = map[runtimeRaidDungeonRunKey]uint64{}

	binding, err := service.beginRuntimeRaidDungeon(openerID, dungeonID, 1)
	if err != nil {
		t.Fatalf("开局人开出这一格的 run：%v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(
		runtimeRaidDungeonEventForTest(binding, openerID, runtimeRaidDungeonLoaded),
	); err != nil {
		t.Fatalf("开局人报 loaded：%v", err)
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

	// 队友先报通关。这里**直接铺出它留下的账本状态**，而不是调用
	// applyRuntimeRaidJoinedSquadClear —— 后者会在 booked 之后调
	// publishRuntimeRaidObjectiveProgress → releaseRuntimeRaidStaleGateOccupancy，
	// 而单元 fixture 里没有"活着持有这条 run 的会话"，那条 run 会被当成过期回收
	// 成 abandoned（本用例第一版就是这么红的）。真机上持有 run 的会话是活的，
	// 不会被回收。
	//
	// applyRuntimeRaidJoinedSquadClear 留下的正是这三样（读代码核对过）：
	//   · objective 被 advanceRuntimeRaidObjectiveLocked 扣掉那一点 HP
	//   · runtimeRaidCreditRunClearLocked：这条 run 的 clear 已用
	//   · ClosedDungeonAttempts[{队友, 他自己的 localToken}] = completed
	state.Lifecycle.Objectives[dungeonID] = runtimeRaidDungeonObjective{
		Phase: phase, MapID: dungeonID, State: raid.AntonDungeonClosed,
		MaxHP: 1, RemainingHP: 0, ClearCount: 1,
	}
	state.Lifecycle.RunClearCredits[runKey] = binding.LifecycleToken
	state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
		CharacterID: joinedID, LifecycleKey: binding.LifecycleToken + 1,
	}] = runtimeRaidDungeonTerminal{Phase: phase, MapID: dungeonID, Kind: runtimeRaidDungeonCompleted}

	if objective := state.Lifecycle.Objectives[dungeonID]; objective.RemainingHP != 0 {
		t.Fatalf("fixture 前提没成立：这一格的 HP 没被花掉（remaining=%d）", objective.RemainingHP)
	}
	if credited, ok := state.Lifecycle.RunClearCredits[runKey]; !ok || credited != binding.LifecycleToken {
		t.Fatalf("fixture 前提没成立：这条 run 的 clear 没被记上（%v/%t）",
			state.Lifecycle.RunClearCredits, ok)
	}

	// 开局人再报通关：必须能过，并且把这条 run 标成 completed。
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(
		runtimeRaidDungeonEventForTest(binding, openerID, runtimeRaidDungeonCompleted),
	); err != nil {
		t.Fatalf("队友先记过账之后，开局人的通关被拒：%v\n"+
			"（真机 2026-10-01 23:38:57.860 就是这句 runtime raid dungeon objective is not open；"+
			"run 关不掉 ⇒ 开局人永远不自动回城 —— 现象「队长回城了、队员没回去」）\n"+
			"closed=%+v runs=%+v", err, state.Lifecycle.ClosedDungeonAttempts, state.Lifecycle.Dungeons)
	}
	if progress, ok := state.Lifecycle.Dungeons[runKey]; ok &&
		progress.Status != runtimeRaidDungeonProgressCompleted {
		t.Fatalf("这一条 run 没有被标成 completed：%+v", progress)
	}
	if attempt, closed := state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
		CharacterID: openerID, LifecycleKey: binding.LifecycleToken}]; !closed ||
		attempt.Kind != runtimeRaidDungeonCompleted {
		t.Fatalf("开局人那一笔没有被记成完成：%+v closed=%t", attempt, closed)
	}

	// **开局人自己的"地下城通关"也要记上。**
	//
	// 真机 2026-10-02 00:05:22（raid 12058625，分片 20261002-002）：
	//
	//	game-raid-joined-clear-booked        char_id=1  booked=true
	//	game-raid-run-clear-already-credited char_id=22 group=2
	//	game-raid-phase-clear-registration   char_id=22 member_clear_counts=1:1
	//	                                     ← 只有 char 1 记到了，char 22 是 0
	//
	// 原因是 runCredited 那一支原来只写日志、不记账；两人**同一小队**时看不出来
	// （队友记账那下已经把整队记上），但超时空的进图单位是区域名单、与编制无关，
	// 两个人常常不在同一小队。操作员原话：「通关以后 队友的通关次数居然没计数」。
	if got := state.Lifecycle.PhaseClearCounts[openerID]; got < 1 {
		t.Fatalf("开局人自己的通关数没被记上：PhaseClearCounts[%d]=%d（想要 >=1）；全表=%v",
			openerID, got, state.Lifecycle.PhaseClearCounts)
	}
	// 而且不许把队友那一队重复记 —— 超时空的上限是 1，两次也只停在 1。
	if got := state.Lifecycle.PhaseClearCounts[joinedID]; got > 1 {
		t.Fatalf("队友的通关数被重复记了：PhaseClearCounts[%d]=%d；全表=%v",
			joinedID, got, state.Lifecycle.PhaseClearCounts)
	}
}
