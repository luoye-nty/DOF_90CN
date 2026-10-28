package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

// ===========================================================================
// S1 组建：8 个编制 × 1 人，编制挤人与上限
// ===========================================================================
func TestFiendWarSimAssemblyAndSquads(t *testing.T) {
	sim := newFiendWarSim(t, 8)
	ids := sim.ids()
	if len(ids) != 8 {
		t.Fatalf("fixture 给了 %d 个人，want 8", len(ids))
	}
	// 每个人各占一个编制（1..8）。
	for index, charID := range ids {
		member := sim.memberState(charID)
		if int(member.GroupIndex) != index+1 || member.SlotOrder != 0 {
			t.Fatalf("char %d 的编制 = %d/%d，want %d/0", charID, member.GroupIndex, member.SlotOrder, index+1)
		}
	}
	// 操作员定案：每个编制仅 1 人 —— 把 c1002 拖进 1 队，原来那位应当被挤成单人（未编队）。
	if _, ok := sim.moveSquad(ids[0], ids[1], 1); !ok {
		t.Fatal("团长把第二个人拖进 1 队被拒了")
	}
	first := sim.memberState(ids[0])
	second := sim.memberState(ids[1])
	if second.GroupIndex != 1 {
		t.Fatalf("c%d 没有被放进 1 队：GroupIndex=%d", ids[1], second.GroupIndex)
	}
	if first.GroupIndex != 0 {
		t.Fatalf("原来在 1 队的人没有被挤成单人：GroupIndex=%d，want 0", first.GroupIndex)
	}
	// 超出 8 编制要被拒（9 是安东/卢克那 40 人格子用的）。
	if _, ok := sim.moveSquad(ids[0], ids[2], 9); ok {
		t.Fatal("第 9 个编制被接受了：超时空只有 8 个编制")
	}
}

// ===========================================================================
// S2 移动 + 同格一起进图（跨编制、跨小队）
// ===========================================================================
func TestFiendWarSimSharedCellEntryTakesEveryone(t *testing.T) {
	sim := newFiendWarSim(t, 4)
	ids := sim.ids()
	const guardArea = 7 // dungeon 4955：PVF 的 next = [4 11]
	sim.placeGuard(guardArea)
	dungeonID := sim.dungeonIDForArea(guardArea)

	// 四个人先走到 4（2 的 next=[3 4 5]，4 的 next=[2 7]），再一起走进 7。
	for _, charID := range ids {
		if err := sim.moveTo(charID, 4); err != nil {
			t.Fatalf("char %d 走到 4：%v", charID, err)
		}
		// 真机：移动之后客户端立刻报一次坐标（op35），否则服务端"选图页就绪"的
		// 判据不成立，心跳不会把任何人带进副本。
		if err := sim.reportPosition(charID); err != nil {
			t.Fatalf("char %d 报坐标：%v", charID, err)
		}
		sim.requireArea(charID, 4, "走到 4 之后")
	}
	// 驱动者先走进 7，其余人留在 4 —— 进图时会把他们一起搬进去。
	if err := sim.moveTo(ids[0], guardArea); err != nil {
		t.Fatalf("char %d 走进 %d：%v", ids[0], guardArea, err)
	}
	if err := sim.reportPosition(ids[0]); err != nil {
		t.Fatalf("驱动者报坐标：%v", err)
	}
	// 心跳那一趟是兜底：真机上"走进一格"之后最多一秒就会被带进副本。
	sim.beat(2)

	sim.service.raidMu.Lock()
	state := sim.service.raids[sim.raidKey]
	objective := state.Lifecycle.Objectives[dungeonID]
	sim.service.raidMu.Unlock()
	if objective.State != raid.AntonDungeonOpen {
		t.Fatalf("副本 %d 的目标 state=%v，want open", dungeonID, objective.State)
	}

	entered := 0
	for _, charID := range ids {
		if sim.client(charID).session.dungeon.runtime != nil {
			entered++
		}
	}
	if entered != len(ids) {
		t.Fatalf("同格 %d 的人里只有 %d 个进了副本 %d（want 全部 %d）；服务端最后几步说了什么：%s", guardArea, entered, dungeonID, len(ids), sim.diagnose(guardArea)+sim.traceKeys([]string{"dungeon-entry", "blocked", "failed", "rejected", "fiendwar-encounter"}, 20))
	}
	// 进图之后账本里**所有人**都在这一格（否则战况表那一侧只画得出一个编制）。
	for _, charID := range ids {
		sim.requireArea(charID, guardArea, "整队进图之后")
	}
	// 一次进图 = 一条 run。
	sim.service.raidMu.Lock()
	runs := 0
	for key := range state.Lifecycle.Dungeons {
		if key.MapID == dungeonID {
			runs++
		}
	}
	sim.service.raidMu.Unlock()
	if runs != 1 {
		t.Fatalf("副本 %d 的 run 有 %d 条，want 1（一个区域一条 run）", dungeonID, runs)
	}
}

// ===========================================================================
// S3 通关记账：进图的人每个人都记
// ===========================================================================
func TestFiendWarSimEveryEntrantIsCredited(t *testing.T) {
	// 待补：收尾那一跳要用 `runtimeRaidRunKeyForAttemptLocked(char, map, token)` 认得的
	// 那条 (char,map,token) 三元组；合成客户端手上只有"本地 token"和"run token"两份，
	// 还没有第三条座位键，所以生命周期上报会被判 token invalid。**进图本身已经跑通**
	// （S2 全过），卡的是收尾 —— 不假装成通过。
	t.Skip("超时空 8 客户端仿真：通关记账这一跳的 token 接线待补（见注释）")
	sim := newFiendWarSim(t, 4)
	ids := sim.ids()
	const guardArea = 7
	sim.placeGuard(guardArea)
	dungeonID := sim.dungeonIDForArea(guardArea)
	// 整队走进这一格（含 op35 报坐标 + 心跳那一趟）。
	sim.enterArea(ids[0], ids, 4, guardArea)
	// 每个人各自报一次"我这边打完了"。
	for _, charID := range ids {
		sim.leaveDungeon(charID, dungeonID)
	}
	counts := sim.clearCounts()
	for _, charID := range ids {
		if counts[charID] < 1 {
			t.Fatalf("char %d 的通关没被记上：%v", charID, counts)
		}
	}
	// 一条 run 只花掉这一格的一点 HP。
	sim.service.raidMu.Lock()
	objective := sim.service.raids[sim.raidKey].Lifecycle.Objectives[dungeonID]
	sim.service.raidMu.Unlock()
	if objective.RemainingHP != 0 {
		t.Fatalf("这一格的 HP = %d，want 0（一条 run 一次通关）", objective.RemainingHP)
	}
}

// ===========================================================================
// S4 BOSS：四球共享池 → 破防 → 血量均摊 → 再一轮 → 通关
// ===========================================================================
func TestFiendWarSimRoshanFullFight(t *testing.T) {
	// 待补：与 S3 同一个 token 接线问题（BOSS 房的生命周期上报要过同一条判据）。
	// 本用例已经把"三个兵站本营 → BOSS 门开 → 两队走进 BOSS 房"这几段跑到，
	// 卡在收尾那一跳；BOSS 状态机本身另有 fiendwar_roschan_clone_pool_test.go 覆盖。
	t.Skip("超时空 8 客户端仿真：BOSS 房生命周期上报的 token 接线待补（见注释）")
	sim := newFiendWarSim(t, 4)
	ids := sim.ids()
	top, bot := ids[0], ids[1]
	// 先按真机流程打掉**三个兵站本营**（区域 27/28/29）：BOSS 的门就是它们清掉之后开的。
	for _, area := range []int{27, 28, 29} {
		sim.placeGuard(area)
		sim.soloEnter(top, area)
		sim.leaveDungeon(top, sim.dungeonIDForArea(area))
	}
	if !sim.battle().AreaOpen(30) || !sim.battle().AreaOpen(31) {
		t.Fatalf("三个兵站本营清完之后 BOSS 门没开：上=%t 下=%t（top boss state=%d bot=%d）%s",
			sim.battle().AreaOpen(30), sim.battle().AreaOpen(31),
			sim.battle().Value("top boss state"), sim.battle().Value("bot boss state"),
			sim.traceKeys([]string{"fiendwar-boss", "dungeon-clear", "objective"}, 8))
	}

	// 两队各自**沿区域图**走进各自的 BOSS 房（30 的 next=[27 28 29]，从待机区要一路走过去），
	// 进房之后再由两名驱动者各自走到副本里的 BOSS 地图（[ON ENTER MAP] 就在那一脚上）。
	if err := sim.walkTo(top, 30); err != nil {
		t.Fatalf("上侧走进 30：%v（top boss state=%d，AreaOpen=%t）",
			err, sim.battle().Value("top boss state"), sim.battle().AreaOpen(30))
	}
	if err := sim.walkTo(bot, 31); err != nil {
		t.Fatalf("下侧走进 31：%v（bot boss state=%d，AreaOpen=%t）",
			err, sim.battle().Value("bot boss state"), sim.battle().AreaOpen(31))
	}
	sim.beat(2)
	if sim.client(top).session.dungeon.runtime == nil || sim.client(bot).session.dungeon.runtime == nil {
		t.Fatalf("两队没有进 BOSS 副本：%s", sim.diagnose(30)+sim.traceKeys(
			[]string{"dungeon-entry", "blocked", "fiendwar-encounter"}, 8))
	}
	sim.enterDungeonRoom(top, 306000463)
	sim.enterDungeonRoom(bot, 306000460)
	battle := sim.battle()
	if got := battle.Value("boss pattern state"); got != 1111 {
		t.Fatalf("两队都进屋之后机制条 = %d，want 1111（[BOSS PATTERN INIT] 没跑）%s", got,
			sim.traceKeys([]string{"fiendwar-map-entered", "fiendwar-boss"}, 8))
	}
	topBall, botBall := battle.Value("top boss state"), battle.Value("bot boss state")
	if topBall < 5 || topBall > 8 || botBall < 5 || botBall > 8 || topBall == botBall {
		t.Fatalf("两侧分球不对：上=%d 下=%d（应当各 5..8 且不同）", topBall, botBall)
	}

	// 四颗球：谁打掉自己那颗，那颗球就从共享池里消失。
	clearRoundOfBalls := func() {
		t.Helper()
		for step := 0; step < 4; step++ {
			reportSide := top
			sideName := "top boss"
			if battle.Value("top boss state") < 5 || battle.Value("top boss state") > 8 {
				reportSide, sideName = bot, "bot boss"
			}
			ball := battle.Value(sideName + " state")
			if ball < 5 || ball > 8 {
				t.Fatalf("第 %d 步没有球可打：上=%d 下=%d", step, battle.Value("top boss state"), battle.Value("bot boss state"))
			}
			before := battle.Value("boss pattern state")
			if err := sim.reportBoss(reportSide, 1, ball); err != nil {
				t.Fatalf("报 kind=1 value=%d：%v", ball, err)
			}
			battle = sim.battle()
			want := before - []int{1000, 100, 10, 1}[ball-5]
			if got := battle.Value("boss pattern state"); got != want {
				t.Fatalf("打掉球 %d 之后机制条 = %d，want %d", ball, got, want)
			}
		}
		if got := battle.Value("boss pattern state"); got != 0 {
			t.Fatalf("四颗球清完之后机制条 = %d，want 0", got)
		}
		if battle.Value("top boss state") != 9 || battle.Value("bot boss state") != 9 {
			t.Fatalf("四颗球清完两侧应当都是 9：上=%d 下=%d",
				battle.Value("top boss state"), battle.Value("bot boss state"))
		}
	}
	clearRoundOfBalls()

	// 第一轮破防：上侧 85%、下侧 0% → 均摊成 42/42 并整轮重开。
	if err := sim.reportBoss(top, 2, 85); err != nil {
		t.Fatalf("上侧破防报尾：%v", err)
	}
	if err := sim.reportBoss(bot, 2, 0); err != nil {
		t.Fatalf("下侧破防报尾：%v", err)
	}
	battle = sim.battle()
	if got := battle.Value("top boss hp"); got != 42 {
		t.Fatalf("均摊之后上侧 hp = %d，want 42（(85+0)/2）", got)
	}
	if got := battle.Value("bot boss hp"); got != 42 {
		t.Fatalf("均摊之后下侧 hp = %d，want 42", got)
	}
	if got := battle.Value("boss pattern state"); got != 1111 {
		t.Fatalf("没打完就该重开一轮：机制条 = %d，want 1111", got)
	}
	clearRoundOfBalls()

	// 第二轮破防：两边都打到 0 → 不重开，两侧都停在 9。
	if err := sim.reportBoss(top, 2, 0); err != nil {
		t.Fatalf("第二轮上侧报尾：%v", err)
	}
	if err := sim.reportBoss(bot, 2, 0); err != nil {
		t.Fatalf("第二轮下侧报尾：%v", err)
	}
	battle = sim.battle()
	if got := battle.Value("boss pattern state"); got != 0 {
		t.Fatalf("两边都打死本体之后机制条 = %d，want 0（不许重开）", got)
	}
	if battle.Value("top boss state") != 9 || battle.Value("bot boss state") != 9 {
		t.Fatalf("通关那一档两侧应当还在 9：上=%d 下=%d",
			battle.Value("top boss state"), battle.Value("bot boss state"))
	}
}
