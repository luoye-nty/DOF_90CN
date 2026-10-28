package dnfbridge

import (
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 攻坚二阶段的两处现场 bug（2026-09-19 生产服 raid_key=1507338）：
//
//  1. 火山盾只偶尔扣：同一把里只有「开团那个人」的 op662 上报被认，其余队员
//     （沿用开团者 binding 的人）整份被拒 ——
//     game-raid-barrier-request-rejected symbol=110 operand=100|3000 in_run=false run_map_id=0。
//     一把能量阻截战应当固定扣 30%（大传送）+ 每个能量传送装置 1%（最多 5 个，现场最少 33%）。
//
//  2. 孵化中心只能进一个人：在 218 里点墙上的传送点（op16 world=221..224）被当成
//     「重新组队进图」，队伍共存预检要求全队都在城镇，于是报
//     game-party-dungeon-entry-copresence-refused / party_entry_preflight_failed_before_any_member_commit，
//     请求者随后 op132 被强制回城，只有最后点的那个人进得去。
func TestRuntimeRaidSquadmateShieldReportCountsOncePerRun(t *testing.T) {
	fixture := newRuntimeRaidPhaseTwoBarrierFixture(t)
	opener := fixture.enterGate(t, 1001, 21)
	squadmate := fixture.adoptGateRun(t, 1002, 22, opener)

	// 开团者三个装置 + 通关 30%：现场也是这么扣的。
	for index := 0; index < 3; index++ {
		fixture.reportShield(t, 1001, antonEnergySmallTransmission)
	}
	fixture.reportShield(t, 1001, antonEnergyLargeTransmission)
	want := antonBarrierMaximum - 3*antonEnergySmallTransmission - antonEnergyLargeTransmission
	if got := fixture.barrier(t); got != want {
		t.Fatalf("the opener's own run=%d want %d", got, want)
	}

	// 队员在自己那份实例里打掉的装置：必须记账（改前 26 次上报全部 in_run=false 被丢）。
	before := fixture.barrier(t)
	fixture.reportShield(t, 1002, antonEnergySmallTransmission)
	if got := fixture.barrier(t); got != before-antonEnergySmallTransmission {
		t.Fatalf("a squadmate's transmission device was dropped: barrier=%d want %d",
			got, before-antonEnergySmallTransmission)
	}
	// 一把的传送预算由全队共享（5 个）：队友还能再记 1 个（第 5 个），第 6 个起不再扣。
	before = fixture.barrier(t)
	fixture.reportShield(t, 1002, antonEnergySmallTransmission)
	if got := fixture.barrier(t); got != before-antonEnergySmallTransmission {
		t.Fatalf("the fifth device of the run was dropped: barrier=%d want %d",
			got, before-antonEnergySmallTransmission)
	}
	before = fixture.barrier(t)
	for index := uint32(0); index < 3; index++ {
		fixture.reportShield(t, 1002, antonEnergySmallTransmission)
	}
	if got := fixture.barrier(t); got != before {
		t.Fatalf("the run's transmission budget is not shared: barrier=%d want %d", got, before)
	}
	if squadmate.raidDungeon.LifecycleToken != opener.raidDungeon.LifecycleToken {
		t.Fatalf("fixture: the squadmate opened its own run: %+v", squadmate.raidDungeon)
	}
}

// 守卫：不在 218/孵化 里的上报仍然必须被拒（城镇里点不出盾）。
func TestRuntimeRaidShieldReportOutsideTheRunIsStillRefused(t *testing.T) {
	fixture := newRuntimeRaidPhaseTwoBarrierFixture(t)
	fixture.enterGate(t, 1001, 21)
	before := fixture.barrier(t)
	fixture.reportShieldFrom(t, fixture.townSession(t, 1002), antonEnergySmallTransmission)
	if got := fixture.barrier(t); got != before {
		t.Fatalf("a town report moved the shield: %d want %d", got, before)
	}
}

// 守卫：站在别的图里（binding 与当前副本不符）也不能扣盾。
func TestRuntimeRaidShieldReportFromAnotherMapIsStillRefused(t *testing.T) {
	fixture := newRuntimeRaidPhaseTwoBarrierFixture(t)
	opener := fixture.enterGate(t, 1001, 21)
	// 同一个 binding，但这个人实际在孵化中心里：不是 218 的传送上报。
	fixture.runOnMap(t, 1002, 22, opener, antonFirstHatcheryMapID)
	before := fixture.barrier(t)
	fixture.reportShield(t, 1002, antonEnergySmallTransmission)
	if got := fixture.barrier(t); got != before {
		t.Fatalf("a hatchery device report moved the shield: %d want %d", got, before)
	}
}

// 218 里点传送点去孵化中心：这是「一个人在自己那份实例里换图」，不是重新组队进图，
// 所以队伍共存预检不能参与，否则请求者被强制回城、只有别人进得去。
func TestRuntimeRaidInRunHatcheryWarpIsNotAPartyEntry(t *testing.T) {
	fixture := newRuntimeRaidPhaseTwoBarrierFixture(t)
	leaderRun := fixture.enterGate(t, 1001, 21)
	fixture.adoptGateRun(t, 1002, 22, leaderRun)
	fixture.partyState(t, 1001, 1002)

	request := dungeoncmd.SelectDungeonRequest{DungeonID: uint32(antonFirstHatcheryMapID), Difficulty: 2}
	if !fixture.service.currentRaidInRunTransferEntry(fixture.session(t, 1001), request) {
		t.Fatal("the hatchery warp inside the gate must be an in-run transfer, not a party entry")
	}
	// 队员同样成立：每个人都在自己那份实例里走同一个传送点。
	if !fixture.service.currentRaidInRunTransferEntry(fixture.session(t, 1002), request) {
		t.Fatal("a squadmate warping into the hatchery must be an in-run transfer too")
	}
	// 已经回城、手上没有 run 的人不是转移：普通组队进图必须保持原样。
	if fixture.service.currentRaidInRunTransferEntry(fixture.townSession(t, 1003), request) {
		t.Fatal("a member with no committed run must still go through the party entry path")
	}
	// 另一阶段的图也不是转移（阶段不匹配）。
	if fixture.service.currentRaidInRunTransferEntry(
		fixture.session(t, 1001),
		dungeoncmd.SelectDungeonRequest{DungeonID: uint32(antonBlackFogSourceMapID), Difficulty: 2},
	) {
		t.Fatal("a phase-one map is not a transfer of the active phase-two run")
	}
}

// runtimeRaidPhaseTwoBarrierFixture drives one active Anton phase two with a real
// 218 run per member, the way the live raid does.
type runtimeRaidPhaseTwoBarrierFixture struct {
	t       *testing.T
	service *Service
	raidKey uint32
}

func newRuntimeRaidPhaseTwoBarrierFixture(t *testing.T) runtimeRaidPhaseTwoBarrierFixture {
	t.Helper()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		gameSessions:   make(map[uint16]*gameSession),
		gameplayTimers: queue,
	}
	config := antonPhaseOneTestConfig()
	lifecycle := newRuntimeRaidLifecycleState(queue.Now())
	if err := initializeRuntimeRaidPhaseObjectives(&lifecycle, config, runtimeRaidDungeonPhaseTwo); err != nil {
		t.Fatalf("initialize phase two: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	lifecycle.PhaseDeadline = queue.Now().Add(40 * time.Minute)
	lifecycle.Barrier = antonBarrierMaximum
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "opener", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "squadmate", GroupIndex: 1, SlotOrder: 1},
		{CharID: 1003, Name: "town-member", GroupIndex: 1, SlotOrder: 2},
	}
	const raidKey = uint32(0x00c80900)
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
	return runtimeRaidPhaseTwoBarrierFixture{t: t, service: service, raidKey: raidKey}
}

// enterGate gives one member its own committed 218 run and the squad's binding.
func (f runtimeRaidPhaseTwoBarrierFixture) enterGate(
	t *testing.T,
	characterID uint16,
	token uint64,
) *runtimeDungeonState {
	t.Helper()
	run := f.newRun(t, characterID, token, antonEnergyInterceptionMapID)
	binding, err := f.service.beginRuntimeRaidDungeon(characterID, antonEnergyInterceptionMapID, token)
	if err != nil {
		t.Fatalf("char %d gate entry: %v", characterID, err)
	}
	if binding.RaidKey == 0 {
		t.Fatalf("char %d got no raid binding", characterID)
	}
	run.raidDungeon = binding
	f.register(t, characterID, run)
	return run
}

// adoptGateRun is the squadmate's own instance of the run its opener already holds:
// it keeps the opener's binding, exactly like the live adoption branch.
func (f runtimeRaidPhaseTwoBarrierFixture) adoptGateRun(
	t *testing.T,
	characterID uint16,
	token uint64,
	owner *runtimeDungeonState,
) *runtimeDungeonState {
	t.Helper()
	run := f.newRun(t, characterID, token, owner.raidDungeon.MapID)
	run.raidDungeon = owner.raidDungeon
	f.register(t, characterID, run)
	return run
}

// runOnMap gives one member the squad's binding while standing in another map of the
// same run (the hatchery a squad warped into).
func (f runtimeRaidPhaseTwoBarrierFixture) runOnMap(
	t *testing.T,
	characterID uint16,
	token uint64,
	owner *runtimeDungeonState,
	mapID int,
) *runtimeDungeonState {
	t.Helper()
	run := f.newRun(t, characterID, token, mapID)
	run.raidDungeon = owner.raidDungeon
	f.register(t, characterID, run)
	return run
}

func (f runtimeRaidPhaseTwoBarrierFixture) newRun(
	t *testing.T,
	characterID uint16,
	token uint64,
	mapID int,
) *runtimeDungeonState {
	t.Helper()
	return &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(mapID)},
		Character:      dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(characterID))},
		Dungeon:        worldmap.Dungeon{ID: int64(mapID)},
		lifecycleToken: token,
	}
}

func (f runtimeRaidPhaseTwoBarrierFixture) register(
	t *testing.T,
	characterID uint16,
	run *runtimeDungeonState,
) {
	t.Helper()
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "raid-shield-" + strconv.Itoa(int(characterID)),
		selectedCharacterID: characterID,
		characterGeneration: 1,
		dungeon:             dungeonSessionState{runtime: run},
	}
	f.service.mu.Lock()
	f.service.gameSessions[characterID] = session
	f.service.mu.Unlock()
}

func (f runtimeRaidPhaseTwoBarrierFixture) session(t *testing.T, characterID uint16) *gameSession {
	t.Helper()
	session, ok := f.service.onlineGameSession(characterID)
	if !ok {
		t.Fatalf("char %d has no session", characterID)
	}
	return session
}

// townSession is a raid member standing in town: no committed run at all.
func (f runtimeRaidPhaseTwoBarrierFixture) townSession(t *testing.T, characterID uint16) *gameSession {
	t.Helper()
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "raid-town-" + strconv.Itoa(int(characterID)),
		selectedCharacterID: characterID,
		characterGeneration: 1,
	}
	f.service.mu.Lock()
	f.service.gameSessions[characterID] = session
	f.service.mu.Unlock()
	return session
}

func (f runtimeRaidPhaseTwoBarrierFixture) partyState(t *testing.T, leader uint16, members ...uint16) {
	t.Helper()
	state := alignedcmd.PartyState{PartyID: 9, UserID: leader}
	all := append([]uint16{leader}, members...)
	for index, characterID := range all {
		state.Members = append(state.Members, alignedcmd.PartyMemberState{
			UserID: characterID, Slot: byte(index), UserState: 1, HPPercent: 100, MPPercent: 100,
		})
	}
	for _, characterID := range all {
		storeRuntimePartyState(f.session(t, characterID), state)
	}
}

func (f runtimeRaidPhaseTwoBarrierFixture) reportShield(
	t *testing.T,
	characterID uint16,
	operand uint32,
) {
	t.Helper()
	f.reportShieldFrom(t, f.session(t, characterID), operand)
}

func (f runtimeRaidPhaseTwoBarrierFixture) reportShieldFrom(
	t *testing.T,
	session *gameSession,
	operand uint32,
) {
	t.Helper()
	handled, err := f.service.applyRuntimeRaidBarrierRequest(session, raid.SetSymbolRequest{
		Symbol:      runtimeRaidSymbolOperationSubtract,
		SourceValue: antonSymbolBarrier,
		TargetValue: operand,
	})
	if err != nil {
		t.Fatalf("char %d shield report: %v", session.selectedCharacterID, err)
	}
	if !handled {
		t.Fatalf("char %d shield report was not handled by the raid owner", session.selectedCharacterID)
	}
}

func (f runtimeRaidPhaseTwoBarrierFixture) barrier(t *testing.T) uint32 {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	return f.service.raids[f.raidKey].Lifecycle.Barrier
}
