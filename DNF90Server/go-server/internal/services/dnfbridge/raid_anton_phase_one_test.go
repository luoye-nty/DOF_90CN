package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// antonPhaseOneTestConfig mirrors the real etc/raid/anton.etc values asserted
// by TestRealScriptPVFAntonConfigMatchesCurrentTwoPhaseCatalog.
func antonPhaseOneTestConfig() *raid.AntonConfig {
	return &raid.AntonConfig{
		PhaseMax:            2,
		MemberMax:           20,
		StartMinimumMembers: 10,
		StartDelaySeconds:   3,
		PhaseTimeoutSeconds: []int{2400, 2400},
		PhaseBreakSeconds:   600,
		MovieEndSeconds:     []int{20, 20, 20, 20},
		EntryCost:           raid.AntonEntryCost{ItemID: 10096296, Count: 1},
		Phases: []raid.AntonPhase{
			{
				Name: "phase one", MinimalDungeonClearCount: 1,
				Dungeons: []raid.AntonDungeonInfo{
					{Index: 210, InitState: raid.AntonDungeonOpen, HP: 4, PartyMax: 5},
					{Index: 211, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
					{Index: 212, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
					{Index: 213, InitState: raid.AntonDungeonHide, HP: 2, PartyMax: 5},
					{Index: 214, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
					{Index: 215, InitState: raid.AntonDungeonHide, HP: 2, PartyMax: 5},
					{Index: 216, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 1},
				},
			},
			{
				Name: "phase two", MinimalDungeonClearCount: 1,
				Dungeons: []raid.AntonDungeonInfo{
					{Index: 218, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 5},
					{Index: 219, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 5},
					{Index: 220, InitState: raid.AntonDungeonHide, HP: 5, PartyMax: 5},
					{Index: 221, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
					{Index: 222, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
					{Index: 223, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
					{Index: 224, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 5},
				},
			},
		},
	}
}

type antonPhaseOneFixture struct {
	service *Service
	queue   *fakeCurrentDungeonDeathTimerQueue
	raidKey uint32
}

func newAntonPhaseOneFixture(t *testing.T) antonPhaseOneFixture {
	t.Helper()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		gameplayTimers: queue,
	}
	config := antonPhaseOneTestConfig()
	lifecycle := newRuntimeRaidLifecycleState(queue.Now())
	if err := initializeRuntimeRaidPhaseObjectives(&lifecycle, config, runtimeRaidDungeonPhaseOne); err != nil {
		t.Fatalf("initialize phase one: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseOne
	lifecycle.PhaseDeadline = queue.Now().Add(40 * time.Minute)
	members := []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}}
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
	return antonPhaseOneFixture{service: service, queue: queue, raidKey: raidKey}
}

func (f antonPhaseOneFixture) clear(t *testing.T, mapID int) {
	t.Helper()
	f.service.raidMu.Lock()
	err := advanceRuntimeRaidObjectiveLocked(f.service.raids[f.raidKey], runtimeRaidDungeonPhaseOne, mapID)
	f.service.raidMu.Unlock()
	if err != nil {
		t.Fatalf("clear %d: %v", mapID, err)
	}
	if err := f.service.afterRuntimeRaidObjectiveAdvance(f.raidKey, runtimeRaidDungeonPhaseOne, mapID); err != nil {
		t.Fatalf("mechanisms after clearing %d: %v", mapID, err)
	}
}

func (f antonPhaseOneFixture) state(t *testing.T) runtimeRaidState {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	return cloneRuntimeRaidState(*f.service.raids[f.raidKey])
}

func (f antonPhaseOneFixture) objective(t *testing.T, mapID int) runtimeRaidDungeonObjective {
	t.Helper()
	objective, ok := f.state(t).Lifecycle.Objectives[mapID]
	if !ok {
		t.Fatalf("objective %d missing", mapID)
	}
	return objective
}

func (f antonPhaseOneFixture) timer(t *testing.T, kind byte, mapID int) runtimeRaidMechanismTimer {
	t.Helper()
	timer, ok := f.state(t).Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: mapID}]
	if !ok {
		t.Fatalf("mechanism timer kind=%d map=%d is not armed: %+v", kind, mapID, f.state(t).Lifecycle.Mechanisms)
	}
	return timer
}

func (f antonPhaseOneFixture) task(t *testing.T, kind byte, mapID int) *fakeCurrentDungeonDeathTimerTask {
	t.Helper()
	timer := f.timer(t, kind, mapID)
	name := runtimeRaidMechanismTimerName(f.raidKey, timer)
	f.queue.mu.Lock()
	defer f.queue.mu.Unlock()
	task := f.queue.active[name]
	if task == nil {
		t.Fatalf("queue has no active task %s", name)
	}
	return task
}

func (f antonPhaseOneFixture) spendBlackFogSource(t *testing.T) {
	t.Helper()
	for i := 0; i < 4; i++ {
		f.clear(t, antonBlackFogSourceMapID)
	}
}

func TestAntonPhaseOneFourthBlackFogClearOpensGatesAndArmsMechanisms(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	for i := 0; i < 3; i++ {
		fixture.clear(t, antonBlackFogSourceMapID)
		if len(fixture.state(t).Lifecycle.Mechanisms) != 0 {
			t.Fatalf("clear %d of the black-fog source armed mechanisms early", i+1)
		}
		if fixture.objective(t, antonQuakeAMapID).State != raid.AntonDungeonHide {
			t.Fatalf("quake opened after only %d black-fog clears", i+1)
		}
	}
	fixture.clear(t, antonBlackFogSourceMapID)

	if source := fixture.objective(t, antonBlackFogSourceMapID); source.RemainingHP != 0 || source.State != raid.AntonDungeonHide {
		t.Fatalf("spent black-fog source=%+v", source)
	}
	for _, mapID := range []int{antonQuakeAMapID, antonLegAMapID, antonQuakeBMapID, antonLegBMapID} {
		if fixture.objective(t, mapID).State != raid.AntonDungeonOpen {
			t.Fatalf("gate %d did not open with the spent black-fog source", mapID)
		}
	}
	// The regenerated fog (211) carries the respawn countdown, and the client only
	// draws a type-2 gate countdown for a gate whose state is 1 or 3: its renderer
	// (0x00F142C9) hands state 0 to the type-1 key, state 1/3 to the type-2 key and
	// skips every other value. The PVF initialises 211 as hide, so it has to be
	// marked cleared here or the armed 300s timer is never rendered.
	if fog := fixture.objective(t, antonBlackFogMapID); fog.State != raid.AntonDungeonCleared {
		t.Fatalf("regenerated fog did not take the cleared marker: %+v", fog)
	}
	if cannon := fixture.objective(t, antonNavalCannonMapID); cannon.State != raid.AntonDungeonHide {
		t.Fatalf("timed gate %d opened immediately", antonNavalCannonMapID)
	}
	wantTimers := []struct {
		kind   byte
		mapID  int
		window time.Duration
	}{
		{runtimeRaidMechanismActive, antonQuakeAMapID, antonQuakeActive},
		{runtimeRaidMechanismActive, antonQuakeBMapID, antonQuakeActive},
		{runtimeRaidMechanismRecovery, antonBlackFogMapID, antonBlackFogRecovery},
		{runtimeRaidMechanismNavalOpen, antonNavalCannonMapID, antonNavalOpenDelay},
	}
	if got := len(fixture.state(t).Lifecycle.Mechanisms); got != len(wantTimers) {
		t.Fatalf("armed %d mechanism timers, want %d: %+v", got, len(wantTimers), fixture.state(t).Lifecycle.Mechanisms)
	}
	for _, want := range wantTimers {
		timer := fixture.timer(t, want.kind, want.mapID)
		if timer.Deadline.Sub(timer.StartedAt) != want.window {
			t.Fatalf("timer kind=%d map=%d window=%s want=%s", want.kind, want.mapID, timer.Deadline.Sub(timer.StartedAt), want.window)
		}
		if task := fixture.task(t, want.kind, want.mapID); task.delay != want.window {
			t.Fatalf("queued task kind=%d map=%d delay=%s want=%s", want.kind, want.mapID, task.delay, want.window)
		}
	}
	// A late clear from a group that was still inside 210 counts for nothing
	// and is not an error.
	fixture.clear(t, antonBlackFogSourceMapID)
	if fixture.objective(t, antonBlackFogSourceMapID).ClearCount != 4 {
		t.Fatal("stale black-fog clear changed the committed clear count")
	}
}

func TestAntonPhaseOneNavalCannonOpensAfterDelayAndBlackFogRecovers(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)

	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismNavalOpen, antonNavalCannonMapID), false)
	if naval := fixture.objective(t, antonNavalCannonMapID); naval.State != raid.AntonDungeonOpen || naval.RemainingHP != 1 {
		t.Fatalf("naval cannon after open delay=%+v", naval)
	}
	if timer := fixture.timer(t, runtimeRaidMechanismActive, antonNavalCannonMapID); timer.Deadline.Sub(timer.StartedAt) != antonNavalActive {
		t.Fatalf("naval active window=%s want=%s", timer.Deadline.Sub(timer.StartedAt), antonNavalActive)
	}

	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismRecovery, antonBlackFogMapID), false)
	if fog := fixture.objective(t, antonBlackFogMapID); fog.State != raid.AntonDungeonOpen || fog.RemainingHP != 1 || fog.ClearCount != 0 {
		t.Fatalf("regenerated black fog after recovery=%+v", fog)
	}
	if timer := fixture.timer(t, runtimeRaidMechanismActive, antonBlackFogMapID); timer.Deadline.Sub(timer.StartedAt) != antonBlackFogActive {
		t.Fatalf("black fog active window=%s want=%s", timer.Deadline.Sub(timer.StartedAt), antonBlackFogActive)
	}
}

// TestAntonPhaseOneFogRegenerationPullsTheSquadsOutOfTheClosedGates is the operator's
// 2026-09-20 rule for the regenerated black fog:
//
//	"当黑雾重生倒计时完毕 战况表和选择副本界面就没有擎天 震颤 舰炮了 相当于副本就
//	 进不去了 … 所有还在擎天 震颤 舰炮的小队 全部强制退出副本（还是以组队的状态）"
//
// The reopening edge is the fog's own recovery timer; firing it must queue exactly one
// forced-return sweep over the gates the fog closes.
func TestAntonPhaseOneFogRegenerationPullsTheSquadsOutOfTheClosedGates(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)

	// The sweep is not armed while the gates are still open.
	if !runtimeRaidPhaseOneGateClosedByFogRegeneration(antonQuakeAMapID) ||
		!runtimeRaidPhaseOneGateClosedByFogRegeneration(antonQuakeBMapID) ||
		!runtimeRaidPhaseOneGateClosedByFogRegeneration(antonLegAMapID) ||
		!runtimeRaidPhaseOneGateClosedByFogRegeneration(antonLegBMapID) ||
		!runtimeRaidPhaseOneGateClosedByFogRegeneration(antonNavalCannonMapID) {
		t.Fatal("a gate the regenerated fog closes is not matched by the sweep")
	}
	for _, mapID := range []int{antonBlackFogSourceMapID, antonBlackFogMapID, antonEnergyInterceptionMapID, antonBlackVolcanoMapID} {
		if runtimeRaidPhaseOneGateClosedByFogRegeneration(mapID) {
			t.Fatalf("map %d is not closed by the regenerated black fog but is matched", mapID)
		}
	}

	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismRecovery, antonBlackFogMapID), false)
	// The fog itself is back and stays enterable.
	if fog := fixture.objective(t, antonBlackFogMapID); fog.State != raid.AntonDungeonOpen || fog.RemainingHP != 1 {
		t.Fatalf("regenerated black fog=%+v", fog)
	}
	sweepName := "raid-phase-return-groups:" +
		strconv.FormatUint(uint64(fixture.raidKey), 10) + ":1:raid_phase_one_fog_regenerated"
	fixture.queue.mu.Lock()
	sweep := fixture.queue.active[sweepName]
	fixture.queue.mu.Unlock()
	if sweep == nil {
		t.Fatalf("the regenerated black fog queued no forced-return sweep (%s)", sweepName)
	}
	// Running it against a roster that holds no gate is a no-op, not an error.
	fixture.queue.fire(sweep, false)
}

// TestAntonPhaseOneCycleResetPullsTheSquadsOutOfTheClosedGates is the edge the operator
// actually hits: the fog's respawn countdown and the quakes' active windows share the
// same 300s deadline, so the quake timer expires first and the reset closes the gates --
// live 2026-09-20T13:50:09Z, raid 1507329, with four members left standing inside 213/215
// until they clicked 回城 themselves 47 seconds later. The reset must queue the same
// forced-return sweep.
func TestAntonPhaseOneCycleResetPullsTheSquadsOutOfTheClosedGates(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)

	// The reset closes every mechanism gate and reopens only the black-fog source.
	if runtimeRaidPhaseOneGateClosedByCycleReset(antonBlackFogSourceMapID) {
		t.Fatal("the black-fog source reopens with the reset and must not be swept")
	}
	for _, mapID := range []int{antonBlackFogMapID, antonQuakeAMapID, antonQuakeBMapID, antonLegAMapID, antonLegBMapID, antonNavalCannonMapID} {
		if !runtimeRaidPhaseOneGateClosedByCycleReset(mapID) {
			t.Fatalf("map %d closes with the cycle reset but is not swept", mapID)
		}
	}

	quake := fixture.task(t, runtimeRaidMechanismActive, antonQuakeAMapID)
	fixture.queue.fire(quake, false)
	if cycle := fixture.state(t).Lifecycle.PhaseCycle; cycle != 1 {
		t.Fatalf("phase cycle=%d after the active window expired, want 1 (the reset)", cycle)
	}
	sweepName := "raid-phase-return-groups:" +
		strconv.FormatUint(uint64(fixture.raidKey), 10) + ":1:raid_phase_one_cycle_reset"
	fixture.queue.mu.Lock()
	sweep := fixture.queue.active[sweepName]
	fixture.queue.mu.Unlock()
	if sweep == nil {
		t.Fatalf("the cycle reset queued no forced-return sweep (%s)", sweepName)
	}
	fixture.queue.fire(sweep, false)
}

func TestAntonPhaseOneClearingMechanismCancelsActiveAndStartsRecovery(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)
	staleActive := fixture.task(t, runtimeRaidMechanismActive, antonQuakeAMapID)

	fixture.clear(t, antonQuakeAMapID)

	if quake := fixture.objective(t, antonQuakeAMapID); quake.State != raid.AntonDungeonCleared || quake.RemainingHP != 0 {
		t.Fatalf("cleared quake=%+v", quake)
	}
	if _, armed := fixture.state(t).Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: runtimeRaidMechanismActive, MapID: antonQuakeAMapID}]; armed {
		t.Fatal("clearing the quake left its active window armed")
	}
	if timer := fixture.timer(t, runtimeRaidMechanismRecovery, antonQuakeAMapID); timer.Deadline.Sub(timer.StartedAt) != antonQuakeRecovery || timer.ActiveAfter != antonQuakeActive {
		t.Fatalf("quake recovery timer=%+v", timer)
	}
	// The dequeued active callback must not fail the cycle any more.
	cycleBefore := fixture.state(t).Lifecycle.PhaseCycle
	fixture.queue.fire(staleActive, true)
	if fixture.state(t).Lifecycle.PhaseCycle != cycleBefore || fixture.objective(t, antonBlackFogSourceMapID).RemainingHP != 0 {
		t.Fatal("stale active callback reset phase one")
	}
	// Recovery reopens the quake with full HP and a fresh active window.
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismRecovery, antonQuakeAMapID), false)
	if quake := fixture.objective(t, antonQuakeAMapID); quake.State != raid.AntonDungeonOpen || quake.RemainingHP != 1 || quake.ClearCount != 0 {
		t.Fatalf("recovered quake=%+v", quake)
	}
	fixture.timer(t, runtimeRaidMechanismActive, antonQuakeAMapID)
}

func TestAntonPhaseOneActiveTimeoutResetsTheCycle(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)
	fixture.clear(t, antonLegAMapID)
	otherActive := fixture.task(t, runtimeRaidMechanismActive, antonQuakeAMapID)
	fogRecovery := fixture.task(t, runtimeRaidMechanismRecovery, antonBlackFogMapID)

	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismActive, antonQuakeBMapID), false)

	state := fixture.state(t)
	if state.Lifecycle.PhaseCycle != 1 {
		t.Fatalf("phase cycle=%d want 1 after a mechanism failure", state.Lifecycle.PhaseCycle)
	}
	if len(state.Lifecycle.Mechanisms) != 0 {
		t.Fatalf("reset left mechanism timers armed: %+v", state.Lifecycle.Mechanisms)
	}
	if source := state.Lifecycle.Objectives[antonBlackFogSourceMapID]; source.State != raid.AntonDungeonOpen || source.RemainingHP != 4 || source.ClearCount != 0 {
		t.Fatalf("black-fog source after reset=%+v", source)
	}
	if leg := state.Lifecycle.Objectives[antonLegAMapID]; leg.State != raid.AntonDungeonHide || leg.RemainingHP != 2 || leg.ClearCount != 0 {
		t.Fatalf("leg counters survived the reset: %+v", leg)
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("reset changed the phase lifecycle: %+v", state.Lifecycle.Status)
	}
	// Timers from the failed cycle are dead even if the queue still fires them.
	fixture.queue.fire(otherActive, true)
	fixture.queue.fire(fogRecovery, true)
	after := fixture.state(t)
	if after.Lifecycle.PhaseCycle != 1 || after.Lifecycle.Objectives[antonBlackFogMapID].State != raid.AntonDungeonHide {
		t.Fatal("callbacks from the failed cycle acted on the new cycle")
	}
	// The next attempt arms a fresh set once the source is spent again.
	fixture.spendBlackFogSource(t)
	if got := len(fixture.state(t).Lifecycle.Mechanisms); got != 4 {
		t.Fatalf("second cycle armed %d timers, want 4", got)
	}
}

func TestAntonPhaseOneCompletionDisarmsMechanisms(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)
	quakeActive := fixture.task(t, runtimeRaidMechanismActive, antonQuakeAMapID)
	fixture.clear(t, antonLegAMapID)
	fixture.clear(t, antonLegAMapID)
	fixture.clear(t, antonLegBMapID)
	fixture.clear(t, antonLegBMapID)

	state := fixture.state(t)
	if !runtimeRaidPhaseObjectivesComplete(state, runtimeRaidDungeonPhaseOne) {
		t.Fatal("both legs down did not complete phase one")
	}
	if len(state.Lifecycle.Mechanisms) != 0 {
		t.Fatalf("completed phase kept mechanism timers: %+v", state.Lifecycle.Mechanisms)
	}
	fixture.queue.fire(quakeActive, true)
	if fixture.state(t).Lifecycle.PhaseCycle != 0 {
		t.Fatal("a mechanism timeout reset a phase that was already won")
	}
}

func TestAntonPhaseOneSnapshotCarriesMechanismTimers(t *testing.T) {
	fixture := newAntonPhaseOneFixture(t)
	fixture.spendBlackFogSource(t)
	state := fixture.state(t)
	notifications := runtimeRaidMechanismTimerNotifications(state, fixture.queue.Now())
	if len(notifications) != 4 {
		t.Fatalf("timer notifications=%d want 4", len(notifications))
	}
	for _, notification := range notifications {
		if notification.msgID != raid.RaidTimerMsgID || len(notification.body) != 13 {
			t.Fatalf("timer notification msg=%d len=%d", notification.msgID, len(notification.body))
		}
		kind := notification.body[0]
		mapID := int(binary.LittleEndian.Uint32(notification.body[1:5]))
		timer, ok := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: mapID}]
		if !ok {
			t.Fatalf("notification kind=%d map=%d has no armed timer", kind, mapID)
		}
		if got := binary.LittleEndian.Uint32(notification.body[5:9]); got != uint32(timer.Deadline.Unix()) {
			t.Fatalf("notification deadline=%d want=%d", got, timer.Deadline.Unix())
		}
		if got := binary.LittleEndian.Uint32(notification.body[9:13]); got != runtimeRaidDurationSeconds(timer.Deadline.Sub(timer.StartedAt)) {
			t.Fatalf("notification duration=%d want=%d", got, runtimeRaidDurationSeconds(timer.Deadline.Sub(timer.StartedAt)))
		}
	}
	// Phase two initialization drops every phase-one timer.
	fixture.service.raidMu.Lock()
	err := initializeRuntimeRaidPhaseObjectives(&fixture.service.raids[fixture.raidKey].Lifecycle, fixture.service.raids[fixture.raidKey].AntonConfig, runtimeRaidDungeonPhaseTwo)
	fixture.service.raidMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.state(t).Lifecycle.Mechanisms) != 0 {
		t.Fatal("phase two initialization kept phase-one mechanism timers")
	}
}
