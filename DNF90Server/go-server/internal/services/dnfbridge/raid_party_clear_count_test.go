package dnfbridge

import (
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 攻坚队的「地下城通关次数」是**队伍（攻坚队分组）通关次数**，不是「有几个队员打完了」。
// 黑雾之源（210）要 4 次通关才把关卡打掉、推进下一阶段；一次组队进图（一个 run）无论进去
// 几个人，都只能算 1 次。
//
// 现场（2026-09-19 组队实测）：两人组队通关两把就进下一阶段了；按这个算法四人组队一把就能
// 直接过。原因见 applyRuntimeRaidJoinedSquadClear 的注释：本服给每个队员各自一份房间实例
// （各自的 seed），于是同一次进图里每个队员的击杀都被当成一次独立的通关，各自扣一点关卡 HP。
//
// 真机口径（86JP RaidManager.TryClearDungeon / RaidDungeonParticipation）：一个
// (dungeon, party) 只有一条参与记录，通关计数是 (raid, dungeon) 上的标量，所以一个 run 只记
// 一次；队员个人列显示的是**本队**的计数（TryClearDungeon 给全队每个队员都加 1）。
func TestRuntimeRaidPartyRunSpendsOneClearHoweverManyMembersCleared(t *testing.T) {
	fixture := newRuntimeRaidPartyClearTestFixture(t, 4)
	owner := fixture.enter(t, 1001, 11)
	second := fixture.enter(t, 1002, 22)
	third := fixture.enter(t, 1003, 33)
	fourth := fixture.enter(t, 1004, 44)
	// 同一队的后进者沿用开团者的 binding：一个 run，一条记录。
	for _, runtime := range []*runtimeDungeonState{second, third, fourth} {
		if runtime.raidDungeon.LifecycleToken != owner.raidDungeon.LifecycleToken {
			t.Fatalf("a squadmate opened its own run: %+v want token %d",
				runtime.raidDungeon, owner.raidDungeon.LifecycleToken)
		}
	}

	for _, characterID := range []uint16{1001, 1002, 1003, 1004} {
		fixture.complete(t, characterID)
	}
	objective := fixture.objective(t, antonBlackFogSourceMapID)
	if objective.RemainingHP != 3 || objective.ClearCount != 1 {
		t.Fatalf("four members clearing one run spent %d gate points: %+v want exactly one",
			objective.ClearCount, objective)
	}
	if objective.State != raid.AntonDungeonOpen {
		t.Fatalf("the gate left the open state after a single party run: %+v", objective)
	}
	// 队员个人列读到的是本队的计数，不能因为四个人打同一把就翻四倍。
	if counts := fixture.clearCounts(t); counts[1001] != 1 || counts[1004] != 1 {
		t.Fatalf("member clear counts=%+v want the squad's single clear", counts)
	}

	// 第二把（同一个队、新的 run token）才扣第二点。
	fixture.enter(t, 1001, 12)
	fixture.complete(t, 1001)
	if got := fixture.objective(t, antonBlackFogSourceMapID); got.RemainingHP != 2 || got.ClearCount != 2 {
		t.Fatalf("the squad's second run=%+v want the second gate point", got)
	}
}

// 一队四个人打一把 = 1 次；所以四人队也要打满四把才推阶段，不能一把过。
func TestRuntimeRaidFourMemberPartyNeedsFourRunsToClearTheGate(t *testing.T) {
	fixture := newRuntimeRaidPartyClearTestFixture(t, 4)
	gate := antonBlackFogSourceMapID
	for run := 1; run <= 4; run++ {
		for _, characterID := range []uint16{1001, 1002, 1003, 1004} {
			fixture.enter(t, characterID, uint64(run)*10+uint64(characterID-1000))
		}
		for _, characterID := range []uint16{1001, 1002, 1003, 1004} {
			fixture.complete(t, characterID)
		}
	}
	objective := fixture.objective(t, gate)
	if objective.RemainingHP != 0 || objective.ClearCount != 4 {
		t.Fatalf("four four-member runs=%+v want the gate cleared after four party runs", objective)
	}
	// 打空的 210 由一阶段机制藏起来（黑雾散开），和 raid_anton_phase_one_test.go 里
	// 「四次通关后 hide」的既有口径一致。
	if objective.State != raid.AntonDungeonHide {
		t.Fatalf("a finished black-fog source must be hidden by the phase mechanism: %+v", objective)
	}
}

// 顺序反过来也一样：队员先在自己那份实例里打完，队长后打完，整把仍然只算 1 次。
func TestRuntimeRaidSquadmateClearBeforeTheOwnerStillSpendsOnePoint(t *testing.T) {
	fixture := newRuntimeRaidPartyClearTestFixture(t, 2)
	ownerRuntime := fixture.enter(t, 1001, 11)
	squadmateRuntime := fixture.enter(t, 1002, 22)
	if squadmateRuntime.raidDungeon.LifecycleToken != ownerRuntime.raidDungeon.LifecycleToken {
		t.Fatalf("fixture: the squadmate opened its own run: %+v", squadmateRuntime.raidDungeon)
	}
	fixture.joinedClear(t, 1002)
	if got := fixture.objective(t, antonBlackFogSourceMapID); got.RemainingHP != 3 || got.ClearCount != 1 {
		t.Fatalf("the squadmate's own instance=%+v want the run's single point", got)
	}

	fixture.completeOwner(t, 1001)
	got := fixture.objective(t, antonBlackFogSourceMapID)
	if got.RemainingHP != 3 || got.ClearCount != 1 {
		t.Fatalf("the owner's completion of the same run spent a second point: %+v", got)
	}
	if runs := fixture.openRunCount(t); runs != 0 {
		t.Fatalf("the owner's completion did not close the run: %d open", runs)
	}
}

// 守卫：不同攻坚队分组各打各的 run，各算一次通关。两个未分组（GroupIndex 0）的成员各自开一
// 个 run，所以两次通关必须各扣一点——「一个 run 一次」不能被读成「一个攻坚队一次」。
func TestRuntimeRaidSeparatePartiesStillSpendTheirOwnClear(t *testing.T) {
	fixture := newRuntimeRaidPartyClearTestFixture(t, 0)
	fixture.enter(t, 1001, 11)
	fixture.complete(t, 1001)
	if got := fixture.objective(t, antonBlackFogSourceMapID); got.RemainingHP != 3 || got.ClearCount != 1 {
		t.Fatalf("the first unassigned member's own run=%+v want one point", got)
	}
	fixture.enter(t, 1002, 22)
	fixture.complete(t, 1002)
	if got := fixture.objective(t, antonBlackFogSourceMapID); got.RemainingHP != 2 || got.ClearCount != 2 {
		t.Fatalf("the second unassigned member's own run=%+v want its own point", got)
	}
}

// 同一把里的重放报告（op39/op117 重发、队员重复报完成）一分不扣。
func TestRuntimeRaidReplayedPartyRunClearEarnsNothing(t *testing.T) {
	fixture := newRuntimeRaidPartyClearTestFixture(t, 2)
	fixture.enter(t, 1001, 11)
	fixture.enter(t, 1002, 22)
	fixture.complete(t, 1001)
	fixture.complete(t, 1002)
	fixture.complete(t, 1002)
	fixture.complete(t, 1001)
	if got := fixture.objective(t, antonBlackFogSourceMapID); got.RemainingHP != 3 || got.ClearCount != 1 {
		t.Fatalf("a replayed report spent another point: %+v", got)
	}
}

// runtimeRaidPartyClearTestFixture drives real raid gate runs: the first member into
// the gate opens the run, every later member of the same squad adopts that binding,
// and each one reports its own instance's completion from its own session.
//
// Every member gets its own live dungeon session on purpose. The raid's stale-gate
// sweep (releaseRuntimeRaidStaleGateOccupancy) releases any run whose owning session
// no longer holds an active dungeon, so a fixture without real sessions would have
// its open runs swept the moment one member's clear publishes objective progress --
// which is exactly the state the joined-clear ordering has to be judged in.
type runtimeRaidPartyClearTestFixture struct {
	t        *testing.T
	service  *Service
	raidKey  uint32
	config   *raid.AntonConfig
	topology *worldmap.DungeonTopology
	runs     map[uint16]*runtimeDungeonState
}

func newRuntimeRaidPartyClearTestFixture(t *testing.T, groupIndex byte) runtimeRaidPartyClearTestFixture {
	t.Helper()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		gameSessions:   make(map[uint16]*gameSession),
		gameplayTimers: queue,
	}
	_, resolver, _ := loadBridgeDungeonStaticData(t, bridgeDungeonPVF(false))
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		bridgeRaidPartyClearDungeonID,
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
	config := antonPhaseOneTestConfig()
	lifecycle := newRuntimeRaidLifecycleState(queue.Now())
	if err := initializeRuntimeRaidPhaseObjectives(&lifecycle, config, runtimeRaidDungeonPhaseOne); err != nil {
		t.Fatalf("initialize phase one: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseOne
	lifecycle.PhaseDeadline = queue.Now().Add(40 * time.Minute)
	members := make([]runtimeRaidMemberState, 0, 4)
	for index, characterID := range []uint16{1001, 1002, 1003, 1004} {
		members = append(members, runtimeRaidMemberState{
			CharID: characterID, GroupIndex: groupIndex, SlotOrder: byte(index),
		})
	}
	const raidKey = uint32(0x00c80700)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      1001,
		TailFlag:      5,
		AntonConfig:   config,
		Started:       true,
		EntryCostPaid: true,
		Members:       members,
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle:     lifecycle,
	}
	return runtimeRaidPartyClearTestFixture{
		t: t, service: service, raidKey: raidKey, config: config, topology: topology,
		runs: make(map[uint16]*runtimeDungeonState),
	}
}

// bridgeRaidPartyClearDungeonID is the bridge-PVF dungeon whose layout gives every
// member a real active run to hold. The raid gate identity comes from the request
// (210), which is what beginRuntimeRaidDungeon maps.
const bridgeRaidPartyClearDungeonID int64 = 700

// enter freezes one member's gate selection in its own session, exactly like a real
// op16 into the raid gate does: its own dungeon instance of the gate, its own
// lifecycle token, and the squad's shared run binding.
func (f runtimeRaidPartyClearTestFixture) enter(
	t *testing.T,
	characterID uint16,
	token uint64,
) *runtimeDungeonState {
	t.Helper()
	run, err := worldmap.NewDungeonRun(f.topology)
	if err != nil {
		t.Fatalf("char %d dungeon run: %v", characterID, err)
	}
	instance, err := worldmap.NewDungeonSession(run)
	if err != nil {
		t.Fatalf("char %d dungeon session: %v", characterID, err)
	}
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: antonBlackFogSourceMapID},
		Character:      dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(characterID))},
		lifecycleToken: token,
		Session:        instance,
	}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "raid-party-clear-" + strconv.Itoa(int(characterID)),
		selectedCharacterID: characterID,
		characterGeneration: 1,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := f.service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatalf("char %d entry: %v", characterID, err)
	}
	if err := f.service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatalf("char %d loaded: %v", characterID, err)
	}
	f.service.mu.Lock()
	f.service.gameSessions[characterID] = session
	f.service.mu.Unlock()
	f.runs[characterID] = runtime
	return runtime
}

// complete reports one member's own instance of the run as cleared.
func (f runtimeRaidPartyClearTestFixture) complete(t *testing.T, characterID uint16) {
	t.Helper()
	session, runtime := f.sessionFor(t, characterID)
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if err := f.service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime); err != nil {
		t.Fatalf("char %d completed: %v", characterID, err)
	}
}

// joinedClear reports the same completion through the adopting squadmate's own path:
// the run belongs to the opener, so the squadmate only books its own clear.
func (f runtimeRaidPartyClearTestFixture) joinedClear(t *testing.T, characterID uint16) {
	t.Helper()
	session, runtime := f.sessionFor(t, characterID)
	f.service.applyRuntimeRaidJoinedSquadClear(session, runtime.raidDungeon, runtime.lifecycleToken)
}

// completeOwner publishes the run's own completion event, the way its owner does.
func (f runtimeRaidPartyClearTestFixture) completeOwner(t *testing.T, characterID uint16) {
	t.Helper()
	_, runtime := f.sessionFor(t, characterID)
	if err := f.service.applyRuntimeRaidDungeonLifecycleEvent(
		runtimeRaidDungeonEventForTest(runtime.raidDungeon, characterID, runtimeRaidDungeonCompleted),
	); err != nil {
		t.Fatalf("char %d completed: %v", characterID, err)
	}
}

func (f runtimeRaidPartyClearTestFixture) sessionFor(
	t *testing.T,
	characterID uint16,
) (*gameSession, *runtimeDungeonState) {
	t.Helper()
	runtime := f.runs[characterID]
	if runtime == nil {
		t.Fatalf("char %d never entered the gate", characterID)
	}
	session := &gameSession{
		selectedCharacterID: characterID,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	return session, runtime
}

func (f runtimeRaidPartyClearTestFixture) objective(t *testing.T, mapID int) runtimeRaidDungeonObjective {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	objective, ok := f.service.raids[f.raidKey].Lifecycle.Objectives[mapID]
	if !ok {
		t.Fatalf("objective %d missing", mapID)
	}
	return objective
}

func (f runtimeRaidPartyClearTestFixture) clearCounts(t *testing.T) map[uint16]uint32 {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	counts := make(map[uint16]uint32, len(f.service.raids[f.raidKey].Lifecycle.PhaseClearCounts))
	for characterID, count := range f.service.raids[f.raidKey].Lifecycle.PhaseClearCounts {
		counts[characterID] = count
	}
	return counts
}

func (f runtimeRaidPartyClearTestFixture) openRunCount(t *testing.T) int {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	return len(f.service.raids[f.raidKey].Lifecycle.Dungeons)
}
