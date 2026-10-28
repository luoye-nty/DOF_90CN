package dnfbridge

import (
	"context"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 一阶段：擎天 A/B（213/215）一通，这一阶段的所有关卡就算关闭 —— 86JP
// RaidHandler.Progression.cs 的 ReturnRaidDungeonGroupsClosedByObjectiveAsync(:1068-1177)
// 会把「被关闭关卡里的队伍」全部送回城，PublishCompletedPhaseOneAsync(:2387-2434)
// 一次性把一阶段所有关卡置为关闭。
//
// 本服的阶段过场又要求没有任何队伍占着关卡（openRuntimeRaidPhaseMovie 要求
// len(GroupOccupancy)==0），所以只要还有一队在别的图里，阶段就永远结束不了 ——
// 现场的「一阶段 有队伍在图里 就不结束」。这里钉住：阶段目标一完成，图里的队伍被送回城、
// 占用清零、阶段立刻进入过场。
func TestRuntimeRaidPhaseCompletionReturnsGroupsStillInside(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	// 另一个一阶段关卡里还站着一队（210 黑雾之源），擎天 A/B 已经打完。
	inside := fixture.enterGateRun(t, 99, 11, antonBlackFogSourceMapID)
	if runs := fixture.openRunCount(t); runs != 1 {
		t.Fatalf("fixture: open runs=%d want 1", runs)
	}

	if err := fixture.service.publishRuntimeRaidObjectiveProgress(
		fixture.raidKey, runtimeRaidDungeonPhaseOne); err != nil {
		t.Fatalf("publish phase progress: %v", err)
	}
	// 收尾被交给计时器线程：跑一次，关卡里的队伍必须被送回城、阶段进入过场。
	fixture.finishCompletedPhase(t)

	if runs := fixture.openRunCount(t); runs != 0 {
		t.Fatalf("the phase completed with %d gate runs still open", runs)
	}
	if occupied := len(fixture.lifecycle().GroupOccupancy); occupied != 0 {
		t.Fatalf("the phase completed with %d gate occupancies left", occupied)
	}
	if runtime := fixture.sessionRuntime(t, 99); runtime != nil {
		t.Fatalf("the member was left inside dungeon %d after the phase completed", runtime.Dungeon.ID)
	}
	if status := inside.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("the gate run was not abandoned: %s", status)
	}
	if status := fixture.lifecycle().Status; status != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("the phase movie did not open: status=%d", status)
	}
	if writes := fixture.sessionWrites(t, 99); writes == 0 {
		t.Fatal("the member received no town route")
	}
}

// 守卫：阶段没完成时，图里的队伍一动不动（只有目标全清才会被送回城）。
func TestRuntimeRaidPhaseForceReturnWaitsForTheObjective(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	// 擎天 A 还有一次没打完：一阶段目标没达成。
	fixture.reopenObjective(antonLegAMapID)
	fixture.enterGateRun(t, 99, 11, antonBlackFogSourceMapID)

	if err := fixture.service.publishRuntimeRaidObjectiveProgress(
		fixture.raidKey, runtimeRaidDungeonPhaseOne); err != nil {
		t.Fatalf("publish phase progress: %v", err)
	}
	if runs := fixture.openRunCount(t); runs != 1 {
		t.Fatalf("an unfinished phase released runs: open=%d want 1", runs)
	}
	if runtime := fixture.sessionRuntime(t, 99); runtime == nil {
		t.Fatal("an unfinished phase pulled the member out of its gate")
	}
	if status := fixture.lifecycle().Status; status != runtimeRaidLifecyclePhaseActive {
		t.Fatalf("an unfinished phase left active status: status=%d", status)
	}
}

// reopenObjective puts one gate back to an unfinished state (one clear left), which
// is what an incomplete phase looks like to runtimeRaidPhaseObjectivesComplete.
func (f runtimeRaidPhaseForceReturnFixture) reopenObjective(mapID int) {
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	lifecycle := &f.service.raids[f.raidKey].Lifecycle
	objective := lifecycle.Objectives[mapID]
	objective.State = raid.AntonDungeonOpen
	if objective.MaxHP == 0 {
		objective.MaxHP = 1
	}
	objective.RemainingHP = objective.MaxHP
	objective.ClearCount = 0
	lifecycle.Objectives[mapID] = objective
}

// 守卫：阶段完成的收尾不能跑在「通关那个人的 session 锁里」。
//
// 现场调用链是 completeCurrentDungeonBossDieCheckLocked（持 session.dungeon.mu）
// → notifyRuntimeRaidDungeonCompletedLocked → publishRuntimeRaidObjectiveProgress。
// 强制回城要给每个队员的 session 上锁，包括调用者自己那把，而 Go 的 Mutex 不可重入：
// 内联执行会把通关那个人的收发线程永久卡在自己的锁上（现场 2026-09-19 16:37:45Z，
// raid 1507329：关卡表快照发出去之后什么都没有了 —— 没有 force-return、没有结算、
// 连错误日志都没有，玩家看到「一阶段打完了 不结束」）。
func TestRuntimeRaidPhaseCompletionRunsOutsideTheMembersSessionLock(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	inside := fixture.enterGateRun(t, 99, 11, antonBlackFogSourceMapID)

	done := make(chan error, 1)
	go func() {
		session := inside.Session
		_ = session
		leader, ok := fixture.service.onlineGameSession(99)
		if !ok {
			done <- errRuntimeRaidNotFound
			return
		}
		leader.dungeon.mu.Lock()
		defer leader.dungeon.mu.Unlock()
		done <- fixture.service.publishRuntimeRaidObjectiveProgress(
			fixture.raidKey, runtimeRaidDungeonPhaseOne)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("publish phase progress under the member's own lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the phase-completion tail deadlocked on the completing member's own session lock")
	}

	// 收尾被交给计时器线程：跑一次到期的任务，关卡里的队伍必须被送回城、阶段进入过场。
	fixture.finishCompletedPhase(t)
	if runs := fixture.openRunCount(t); runs != 0 {
		t.Fatalf("the completed phase left %d gate runs open", runs)
	}
	if status := fixture.lifecycle().Status; status != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("the phase movie did not open: status=%d", status)
	}
	if runtime := fixture.sessionRuntime(t, 99); runtime != nil {
		t.Fatalf("the member was left inside dungeon %d after the phase completed", runtime.Dungeon.ID)
	}
}

// finishCompletedPhase runs the completed phase's deferred tail once, the way the
// gameplay timer goroutine would (the queue only stores the task in tests).
func (f runtimeRaidPhaseForceReturnFixture) finishCompletedPhase(t *testing.T) {
	t.Helper()
	f.service.finishRuntimeRaidPhase(f.raidKey, runtimeRaidDungeonPhaseOne)
}

// runtimeRaidPhaseForceReturnFixture is one active phase-one raid whose completion
// condition (213/215) is already satisfied, with a real PVF-backed dungeon run.
type runtimeRaidPhaseForceReturnFixture struct {
	t        *testing.T
	service  *Service
	raidKey  uint32
	topology *worldmap.DungeonTopology
	conns    map[uint16]*bufferConn
}

func newRuntimeRaidPhaseForceReturnFixture(t *testing.T) runtimeRaidPhaseForceReturnFixture {
	t.Helper()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, bridgeRaidPhaseForceReturnPVF())
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		bridgeRaidPhaseForceReturnDungeonID,
		0,
		func(choice worldmap.DungeonMapChoice) (int64, error) {
			if len(choice.Candidates) == 0 {
				return 0, worldmap.ErrMapChoiceRequired
			}
			return choice.Candidates[0].ID, nil
		},
	)
	if err != nil {
		t.Fatalf("build a live dungeon layout for the fixture: %v", err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, id := range []string{"99", "100"} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "account-1",
			Level:       90,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 19, "area_id": 2,
				"pos_x": 1000, "pos_y": 300, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: id, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x44556677, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:      queue,
		raids:               make(map[uint32]*runtimeRaidState),
		gameSessions:        make(map[uint16]*gameSession),
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")

	config := antonPhaseOneTestConfig()
	lifecycle := newRuntimeRaidLifecycleState(queue.Now())
	if err := initializeRuntimeRaidPhaseObjectives(&lifecycle, config, runtimeRaidDungeonPhaseOne); err != nil {
		t.Fatalf("initialize phase one: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseOne
	lifecycle.PhaseDeadline = queue.Now().Add(40 * time.Minute)
	// 擎天 A/B 已经打完：一阶段目标达成。
	for _, mapID := range []int{antonLegAMapID, antonLegBMapID} {
		objective := lifecycle.Objectives[mapID]
		objective.State = raid.AntonDungeonCleared
		objective.RemainingHP = 0
		objective.ClearCount = objective.MaxHP
		lifecycle.Objectives[mapID] = objective
	}
	members := []runtimeRaidMemberState{
		{CharID: 99, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 100, Name: "member", GroupIndex: 2, SlotOrder: 0},
	}
	const raidKey = uint32(0x00c80a00)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      99,
		TailFlag:      5,
		AntonConfig:   config,
		Started:       true,
		EntryCostPaid: true,
		Members:       members,
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle:     lifecycle,
	}
	fixture := runtimeRaidPhaseForceReturnFixture{
		t: t, service: service, raidKey: raidKey, topology: topology, conns: make(map[uint16]*bufferConn),
	}
	fixture.bindSession(t, 99)
	fixture.bindSession(t, 100)
	return fixture
}

func (f runtimeRaidPhaseForceReturnFixture) bindSession(t *testing.T, characterID uint16) {
	t.Helper()
	channel := channelcatalog.Channel{ServerID: 1, ID: 23, Type: 23, Name: "raid", Port: 10023}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "raid-phase-return-" + strconv.Itoa(int(characterID)),
		channel:             channel,
		residentChannel:     channel,
		selectedCharacterID: characterID,
		characterGeneration: 1,
	}
	f.service.bindGameSessionCharacter(session, characterID)
	bindDungeonSelectorOriginForTestAt(t, f.service, session, 19, 2, 1000, 300)
	instanceKey := f.raidKey
	f.service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: characterID,
		ChannelID:   townPresenceChannelID(session),
		TownID:      19,
		AreaID:      2,
		InstanceKey: instanceKey,
		PositionX:   1000,
		PositionY:   300,
		Session:     session,
	})
	f.service.mu.Lock()
	f.service.gameSessions[characterID] = session
	f.service.mu.Unlock()
	f.conns[characterID] = conn
}

// enterGateRun gives one member a real dungeon run bound to one raid gate: its own
// dungeon instance of the gate, its own lifecycle token, and the squad's run binding,
// exactly like a real op16 into the gate does.
func (f runtimeRaidPhaseForceReturnFixture) enterGateRun(
	t *testing.T,
	characterID uint16,
	token uint64,
	mapID int,
) *runtimeDungeonState {
	t.Helper()
	session, ok := f.service.onlineGameSession(characterID)
	if !ok || session == nil {
		t.Fatalf("char %d has no session", characterID)
	}
	repositories, ok := f.service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), strconv.Itoa(int(characterID)))
	if err != nil || !found {
		t.Fatalf("char %d character record: found=%t err=%v", characterID, found, err)
	}
	run, err := worldmap.NewDungeonRun(f.topology)
	if err != nil {
		t.Fatalf("char %d dungeon run: %v", characterID, err)
	}
	instance, err := worldmap.NewDungeonSession(run)
	if err != nil {
		t.Fatalf("char %d dungeon session: %v", characterID, err)
	}
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(mapID), Difficulty: 2},
		Dungeon:        worldmap.Dungeon{ID: bridgeRaidPhaseForceReturnDungeonID},
		Character:      character,
		lifecycleToken: token,
		Session:        instance,
	}
	session.dungeon.mu.Lock()
	session.dungeon.runtime = runtime
	session.dungeon.mu.Unlock()
	// freezeRuntimeRaidDungeonSelection only freezes the run its own session holds.
	if err := f.service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatalf("char %d raid binding: %v", characterID, err)
	}
	if runtime.raidDungeon.RaidKey != f.raidKey {
		t.Fatalf("char %d binding=%+v want raid %d", characterID, runtime.raidDungeon, f.raidKey)
	}
	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("char %d freeze town origin: %v", characterID, err)
	}
	if err := f.service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatalf("char %d loaded: %v", characterID, err)
	}
	return runtime
}

// openObjective puts one gate back to full HP so the phase is not complete.
func (f runtimeRaidPhaseForceReturnFixture) openObjective(mapID int) {
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	lifecycle := &f.service.raids[f.raidKey].Lifecycle
	objective := lifecycle.Objectives[mapID]
	objective.State = raid.AntonDungeonOpen
	if objective.MaxHP == 0 {
		objective.MaxHP = 1
	}
	objective.RemainingHP = objective.MaxHP
	lifecycle.Objectives[mapID] = objective
}

func (f runtimeRaidPhaseForceReturnFixture) lifecycle() runtimeRaidLifecycleState {
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	return f.service.raids[f.raidKey].Lifecycle
}

// 黑火山（219）在被破盾期间是可进的；盾回满时 86JP 会把 219 里的参与记录全部
// 拆掉并把队伍送回城（RaidManager.cs:1664-1673、Progression.cs:1993-1998），
// 否则破盾期进去的队伍可以等盾满了再打死火山，整段攻坚就被跳过。
// 这个测试钉住「只退 219，不动别的图」。
func TestRuntimeRaidBarrierRestoreReturnsOnlyTheVolcano(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	fixture.switchToPhaseTwo(t)
	// 破盾状态：219 开放。
	fixture.setVolcanoOpen(t, true)
	fixture.enterGateRun(t, 99, 21, antonBlackVolcanoMapID)
	fixture.enterGateRun(t, 100, 22, antonEnergyInterceptionMapID)

	returned := fixture.service.forceReturnRuntimeRaidGroupsMatching(
		fixture.raidKey,
		runtimeRaidDungeonPhaseTwo,
		"raid_barrier_restored_test",
		func(mapID int) bool { return mapID == antonBlackVolcanoMapID },
	)
	if returned != 1 {
		t.Fatalf("barrier restore returned %d sessions want 1", returned)
	}
	if runtime := fixture.sessionRuntime(t, 99); runtime != nil {
		t.Fatalf("the volcano member stayed inside %d after the shield recovered", runtime.Dungeon.ID)
	}
	if runtime := fixture.sessionRuntime(t, 100); runtime == nil {
		t.Fatal("the energy-interception member was pulled out by the volcano seal")
	}
	if runs := fixture.openRunCount(t); runs != 1 {
		t.Fatalf("open runs=%d want only the untouched 218 run", runs)
	}
}

// 火山被打掉后，218/219/221..224 全部作废（86JP TryInvalidateDungeonRuns），
// 220（心脏）不在其中。
func TestRuntimeRaidPhaseTwoGateAfterVolcanoCoversEveryClosedGate(t *testing.T) {
	for _, mapID := range []int{
		antonEnergyInterceptionMapID,
		antonBlackVolcanoMapID,
		antonFirstHatcheryMapID,
		antonLastHatcheryMapID,
	} {
		if !runtimeRaidPhaseTwoGateAfterVolcano(mapID) {
			t.Fatalf("gate %d is not closed by the falling volcano", mapID)
		}
	}
	if runtimeRaidPhaseTwoGateAfterVolcano(antonHeartMapID) {
		t.Fatalf("the heart (%d) is the objective the volcano opens, not a gate it closes", antonHeartMapID)
	}
}

// switchToPhaseTwo turns the fixture's raid into an active phase two.
func (f runtimeRaidPhaseForceReturnFixture) switchToPhaseTwo(t *testing.T) {
	t.Helper()
	f.service.raidMu.Lock()
	state := f.service.raids[f.raidKey]
	if err := initializeRuntimeRaidPhaseObjectives(&state.Lifecycle, state.AntonConfig, runtimeRaidDungeonPhaseTwo); err != nil {
		f.service.raidMu.Unlock()
		t.Fatalf("initialize phase two: %v", err)
	}
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	f.service.raidMu.Unlock()
}

// joinSquadOne puts one frozen member into squad 1 so it shares 99's seat.
func (f runtimeRaidPhaseForceReturnFixture) joinSquadOne(t *testing.T, characterID uint16) {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	state := f.service.raids[f.raidKey]
	for index := range state.Members {
		if state.Members[index].CharID == characterID {
			state.Members[index].GroupIndex = 1
		}
	}
	for index := range state.FrozenMembers {
		if state.FrozenMembers[index].CharID == characterID {
			state.FrozenMembers[index].GroupIndex = 1
		}
	}
}

// setVolcanoOpen mirrors the shield edge: 219 is enterable only while the volcano is
// open.
func (f runtimeRaidPhaseForceReturnFixture) setVolcanoOpen(t *testing.T, open bool) {
	t.Helper()
	f.service.raidMu.Lock()
	f.service.raids[f.raidKey].Lifecycle.VolcanoOpen = open
	f.service.raidMu.Unlock()
}

func (f runtimeRaidPhaseForceReturnFixture) openRunCount(t *testing.T) int {
	t.Helper()
	return len(f.lifecycle().Dungeons)
}

func (f runtimeRaidPhaseForceReturnFixture) sessionRuntime(
	t *testing.T,
	characterID uint16,
) *runtimeDungeonState {
	t.Helper()
	session, ok := f.service.onlineGameSession(characterID)
	if !ok {
		t.Fatalf("char %d has no session", characterID)
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return session.dungeon.runtime
}

func (f runtimeRaidPhaseForceReturnFixture) sessionWrites(t *testing.T, characterID uint16) int {
	t.Helper()
	conn := f.conns[characterID]
	if conn == nil {
		return 0
	}
	return conn.write.Len()
}

// bridgeRaidPhaseForceReturnPVF indexes one ordinary bridge dungeon under the raid
// gate ids the fixture enters, so the run is a real PVF-backed dungeon.
func bridgeRaidPhaseForceReturnPVF() bridgePVFSource {
	source := bridgeDungeonPVF(false)
	source[worldmap.DefaultDungeonList] = "700 `test.dgn`\n210 `test.dgn`\n212 `test.dgn`\n"
	source["worldmap/test.wdm"] = "[name]\n`Synthetic Area`\n[dungeon]\n700 -1\n210 -1\n212 -1\n[/dungeon]\n"
	return source
}

// bridgeRaidPhaseForceReturnDungeonID is the bridge-PVF dungeon whose layout gives
// every member a real active run to hold. The raid gate identity comes from the
// request (210), which is what beginRuntimeRaidDungeon maps.
const bridgeRaidPhaseForceReturnDungeonID int64 = 700
