package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

type antonPhaseTwoFixture struct {
	service  *Service
	queue    *fakeCurrentDungeonDeathTimerQueue
	raidKey  uint32
	sessions map[uint16]*gameSession
}

func newAntonPhaseTwoFixture(t *testing.T) antonPhaseTwoFixture {
	t.Helper()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:          options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:            make(map[uint32]*runtimeRaidState),
		gameplayTimers:   queue,
		raidRewardRandom: func() uint64 { return 0 },
	}
	config := antonPhaseOneTestConfig()
	lifecycle := newRuntimeRaidLifecycleState(queue.Now())
	if err := initializeRuntimeRaidPhaseObjectives(&lifecycle, config, runtimeRaidDungeonPhaseTwo); err != nil {
		t.Fatalf("initialize phase two: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	lifecycle.PhaseDeadline = queue.Now().Add(40 * time.Minute)
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10200}
	memberIDs := []uint16{1001, 1002, 1003}
	members := make([]runtimeRaidMemberState, 0, len(memberIDs))
	sessions := make(map[uint16]*gameSession, len(memberIDs))
	for i, id := range memberIDs {
		session := &gameSession{conn: &bufferConn{}, channel: channel}
		service.bindGameSessionCharacter(session, id)
		sessions[id] = session
		members = append(members, runtimeRaidMemberState{CharID: id, GroupIndex: byte(i) + 1, SlotOrder: 0, UserState: 1})
	}
	const raidKey = uint32(0x00c80800)
	state := &runtimeRaidState{
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
	initRuntimeRaidPhaseTwoLocked(state, queue.Now())
	service.raids[raidKey] = state
	return antonPhaseTwoFixture{service: service, queue: queue, raidKey: raidKey, sessions: sessions}
}

func (f antonPhaseTwoFixture) state(t *testing.T) runtimeRaidState {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	return cloneRuntimeRaidState(*f.service.raids[f.raidKey])
}

func (f antonPhaseTwoFixture) objective(t *testing.T, mapID int) runtimeRaidDungeonObjective {
	t.Helper()
	objective, ok := f.state(t).Lifecycle.Objectives[mapID]
	if !ok {
		t.Fatalf("objective %d missing", mapID)
	}
	return objective
}

func (f antonPhaseTwoFixture) clear(t *testing.T, mapID int) {
	t.Helper()
	f.service.raidMu.Lock()
	err := advanceRuntimeRaidObjectiveLocked(f.service.raids[f.raidKey], runtimeRaidDungeonPhaseTwo, mapID)
	f.service.raidMu.Unlock()
	if err != nil {
		t.Fatalf("clear %d: %v", mapID, err)
	}
	if err := f.service.afterRuntimeRaidObjectiveAdvance(f.raidKey, runtimeRaidDungeonPhaseTwo, mapID); err != nil {
		t.Fatalf("mechanisms after clearing %d: %v", mapID, err)
	}
}

// enterRun puts the member inside a raid dungeon run so its op662 reports are
// authorized against that run.
func (f antonPhaseTwoFixture) enterRun(t *testing.T, id uint16, mapID int, token uint64) {
	t.Helper()
	session := f.sessions[id]
	session.dungeon.mu.Lock()
	session.dungeon.runtime = &runtimeDungeonState{
		Character:      dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(id))},
		Dungeon:        worldmap.Dungeon{ID: int64(mapID)},
		lifecycleToken: token,
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey:        f.raidKey,
			Phase:          runtimeRaidDungeonPhaseTwo,
			MapID:          mapID,
			LifecycleToken: token,
		},
	}
	session.dungeon.mu.Unlock()
}

func (f antonPhaseTwoFixture) task(t *testing.T, kind byte, mapID int) *fakeCurrentDungeonDeathTimerTask {
	t.Helper()
	timer, ok := f.state(t).Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: mapID}]
	if !ok {
		t.Fatalf("mechanism timer kind=%d map=%d is not armed: %+v", kind, mapID, f.state(t).Lifecycle.Mechanisms)
	}
	name := runtimeRaidMechanismTimerName(f.raidKey, timer)
	f.queue.mu.Lock()
	defer f.queue.mu.Unlock()
	task := f.queue.active[name]
	if task == nil {
		// Timers armed under the raid lock are queued by the caller; arm them
		// through the same path the runtime uses.
		f.queue.mu.Unlock()
		if err := f.service.scheduleRuntimeRaidMechanismTimers(f.raidKey, []runtimeRaidMechanismTimer{timer}); err != nil {
			t.Fatal(err)
		}
		f.queue.mu.Lock()
		task = f.queue.active[name]
	}
	if task == nil {
		t.Fatalf("queue has no active task %s", name)
	}
	return task
}

func runtimeRaidSymbolRequestBody(symbol uint32, operand uint32, operation byte) []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint32(body[0:4], symbol)
	binary.LittleEndian.PutUint32(body[4:8], operand)
	body[8] = operation
	return body
}

func (f antonPhaseTwoFixture) report(t *testing.T, id uint16, symbol uint32, operand uint32, operation byte) {
	t.Helper()
	if err := f.service.handleOnlineRaidSetSymbol(f.sessions[id], uint16(dnfenum.CmdPacketRaidSetSymbol), runtimeRaidSymbolRequestBody(symbol, operand, operation)); err != nil {
		t.Fatalf("op662 symbol=%d operand=%d op=%d: %v", symbol, operand, operation, err)
	}
}

func TestAntonPhaseTwoStartsShieldedWithVolcanoClosed(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	state := fixture.state(t)
	if state.Lifecycle.Barrier != antonBarrierMaximum || state.Lifecycle.VolcanoOpen || state.Lifecycle.InfectionMapID != 0 {
		t.Fatalf("phase two init barrier=%d volcano=%t infection=%d", state.Lifecycle.Barrier, state.Lifecycle.VolcanoOpen, state.Lifecycle.InfectionMapID)
	}
	timer, ok := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: runtimeRaidMechanismHatcheryOpen, MapID: antonBlackVolcanoMapID}]
	if !ok || timer.Deadline.Sub(timer.StartedAt) != antonHatcheryOpenDelay {
		t.Fatalf("hatchery open timer=%+v ok=%t", timer, ok)
	}
	rows := runtimeRaidPhaseTwoSymbolRows(state)
	want := map[uint32]uint32{antonSymbolPhaseTwoInit: 1, antonSymbolBarrier: antonBarrierMaximum, antonSymbolVolcanoOpen: 0, antonSymbolInfectionFlag: 0, antonSymbolInfectionMapID: 0}
	for _, row := range rows {
		if value, expected := want[row.Key]; expected && value != row.Value {
			t.Fatalf("symbol %d=%d want %d", row.Key, row.Value, value)
		}
		delete(want, row.Key)
	}
	if len(want) != 0 {
		t.Fatalf("phase-two symbol rows missing %v", want)
	}
	// The volcano is open by PVF state but shut by the shield.
	if _, err := fixture.service.beginRuntimeRaidDungeon(1001, antonBlackVolcanoMapID, 9); err == nil {
		t.Fatal("volcano admitted a group while the shield held")
	}
	if _, err := fixture.service.beginRuntimeRaidDungeon(1001, antonEnergyInterceptionMapID, 9); err != nil {
		t.Fatalf("energy interception refused a group: %v", err)
	}
}

func TestAntonPhaseTwoShieldReportsRespectRunBudgets(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	fixture.enterRun(t, 1001, antonEnergyInterceptionMapID, 11)

	fixture.report(t, 1001, antonSymbolBarrier, antonEnergyLargeTransmission, runtimeRaidSymbolOperationSubtract)
	if got := fixture.state(t).Lifecycle.Barrier; got != antonBarrierMaximum-antonEnergyLargeTransmission {
		t.Fatalf("barrier after large transmission=%d", got)
	}
	// Second large transmission from the same run is over budget.
	fixture.report(t, 1001, antonSymbolBarrier, antonEnergyLargeTransmission, runtimeRaidSymbolOperationSubtract)
	if got := fixture.state(t).Lifecycle.Barrier; got != antonBarrierMaximum-antonEnergyLargeTransmission {
		t.Fatalf("second large transmission was accepted: barrier=%d", got)
	}
	for i := 0; i < int(antonEnergySmallTransmissionCount)+1; i++ {
		fixture.report(t, 1001, antonSymbolBarrier, antonEnergySmallTransmission, runtimeRaidSymbolOperationSubtract)
	}
	wantAfterSmall := antonBarrierMaximum - antonEnergyLargeTransmission - antonEnergySmallTransmission*antonEnergySmallTransmissionCount
	if got := fixture.state(t).Lifecycle.Barrier; got != wantAfterSmall {
		t.Fatalf("barrier after small transmissions=%d want=%d", got, wantAfterSmall)
	}
	// Wrong symbol, wrong operation, and a member outside any run change nothing.
	fixture.report(t, 1001, 50, antonEnergySmallTransmission, runtimeRaidSymbolOperationSubtract)
	fixture.report(t, 1001, antonSymbolBarrier, antonEnergySmallTransmission, runtimeRaidSymbolOperationAdd)
	fixture.report(t, 1002, antonSymbolBarrier, antonEnergyLargeTransmission, runtimeRaidSymbolOperationSubtract)
	if got := fixture.state(t).Lifecycle.Barrier; got != wantAfterSmall {
		t.Fatalf("invalid reports moved the barrier to %d", got)
	}
	// A hatchery reduction is not valid from inside energy interception.
	fixture.report(t, 1001, antonSymbolBarrier, antonHatcheryReduction, runtimeRaidSymbolOperationSubtract)
	if got := fixture.state(t).Lifecycle.Barrier; got != wantAfterSmall {
		t.Fatalf("hatchery reduction accepted from energy interception: %d", got)
	}
	// A fresh run of the same dungeon has its own budget.
	fixture.enterRun(t, 1001, antonEnergyInterceptionMapID, 12)
	fixture.report(t, 1001, antonSymbolBarrier, antonEnergyLargeTransmission, runtimeRaidSymbolOperationSubtract)
	if got := fixture.state(t).Lifecycle.Barrier; got != wantAfterSmall-antonEnergyLargeTransmission {
		t.Fatalf("new run budget rejected: barrier=%d", got)
	}
	// The reporter received one success ACK per accepted report and the
	// other members received the shield symbol.
	packets := runtimeRaidPeerTake(t, fixture.sessions[1002])
	symbol, ok := runtimeRaidPeerFind(packets, raid.RaidSymbolMsgID)
	if !ok {
		t.Fatal("other members received no shield symbol")
	}
	if symbol.Body[0] != 1 || binary.LittleEndian.Uint32(symbol.Body[1:5]) != antonSymbolBarrier {
		t.Fatalf("shield symbol body=%x", symbol.Body)
	}
}

func TestAntonPhaseTwoBrokenShieldOpensVolcanoAndRecharges(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	fixture.service.raidMu.Lock()
	state := fixture.service.raids[fixture.raidKey]
	change, armed, ok := changeRuntimeRaidBarrierLocked(state, antonBarrierMaximum, runtimeRaidSymbolOperationSubtract, fixture.queue.Now())
	fixture.service.raidMu.Unlock()
	if !ok || !change.Broke || change.Next != 0 || len(armed) != 1 {
		t.Fatalf("breaking the shield change=%+v armed=%d ok=%t", change, len(armed), ok)
	}
	if !fixture.state(t).Lifecycle.VolcanoOpen {
		t.Fatal("broken shield did not open the volcano")
	}
	if _, err := fixture.service.beginRuntimeRaidDungeon(1001, antonBlackVolcanoMapID, 9); err != nil {
		t.Fatalf("volcano still refused a group after the shield broke: %v", err)
	}

	// Recharge ticks: 17 per second, 34 while a hatchery is infected.
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismBarrierRecovery, antonBlackVolcanoMapID), false)
	if got := fixture.state(t).Lifecycle.Barrier; got != antonBarrierChargePerSecond {
		t.Fatalf("barrier after one tick=%d want=%d", got, antonBarrierChargePerSecond)
	}
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Lifecycle.InfectionMapID = 222
	fixture.service.raidMu.Unlock()
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismBarrierRecovery, antonBlackVolcanoMapID), false)
	if got := fixture.state(t).Lifecycle.Barrier; got != antonBarrierChargePerSecond+antonBarrierInfectedChargePerSecond {
		t.Fatalf("barrier after infected tick=%d", got)
	}
	// Reaching the maximum closes the volcano and stops the ticks.
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Lifecycle.Barrier = antonBarrierMaximum - 1
	fixture.service.raidMu.Unlock()
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismBarrierRecovery, antonBlackVolcanoMapID), false)
	after := fixture.state(t)
	if after.Lifecycle.Barrier != antonBarrierMaximum || after.Lifecycle.VolcanoOpen {
		t.Fatalf("restored shield barrier=%d volcano=%t", after.Lifecycle.Barrier, after.Lifecycle.VolcanoOpen)
	}
	if _, armed := after.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: runtimeRaidMechanismBarrierRecovery, MapID: antonBlackVolcanoMapID}]; armed {
		t.Fatal("recharge kept ticking after the shield was restored")
	}
	if _, err := fixture.service.beginRuntimeRaidDungeon(1002, antonBlackVolcanoMapID, 10); err == nil {
		t.Fatal("volcano admitted a group after the shield was restored")
	}
	// The client plays its barrier animations from the symbol edges only
	// (etc/raid/anton.etc: [ON CHANGE SYMBOL][BLACK VOLCANO BARRIER] 0/10000 ->
	// [SET] [MOVIE BARRIER BREAK/RECOVER] 1), so both markers must travel with the
	// shield rows and the volcano authority row.
	markers := fixture.state(t).Lifecycle.MovieSymbols
	if markers[antonSymbolBarrierRecover] != 1 || markers[antonSymbolBarrierBreak] != 0 {
		t.Fatalf("restored-shield movie markers=%v", markers)
	}
}

func TestAntonPhaseTwoShieldEdgesCarryMovieMarkers(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	fixture.service.raidMu.Lock()
	state := fixture.service.raids[fixture.raidKey]
	broken, _, ok := changeRuntimeRaidBarrierLocked(state, antonBarrierMaximum, runtimeRaidSymbolOperationSubtract, fixture.queue.Now())
	snapshot := cloneRuntimeRaidState(*state)
	fixture.service.raidMu.Unlock()
	if !ok || !broken.Broke {
		t.Fatalf("break change=%+v ok=%t", broken, ok)
	}
	if got := snapshot.Lifecycle.MovieSymbols[antonSymbolBarrierBreak]; got != 1 {
		t.Fatalf("break marker=%d want 1", got)
	}
	if got := snapshot.Lifecycle.MovieSymbols[antonSymbolBarrierRecover]; got != 0 {
		t.Fatalf("recover marker=%d want 0 alongside the break", got)
	}
	edge := runtimeRaidMovieSymbolChanges(map[uint32]uint32{antonSymbolBarrierRecover: 1}, snapshot.Lifecycle.MovieSymbols)
	keys := make([]uint32, 0, len(edge))
	for _, row := range edge {
		keys = append(keys, row.Key)
	}
	if len(keys) != 2 || keys[0] != antonSymbolBarrierBreak || keys[1] != antonSymbolBarrierRecover {
		t.Fatalf("break edge rows=%v want both markers", edge)
	}
	if filtered := filterRuntimeRaidMovieSymbolRows(edge, antonSymbolBarrierBreak, antonSymbolBarrierRecover); len(filtered) != 0 {
		t.Fatalf("barrier edge markers leaked into the generic pulse: %v", filtered)
	}
}

func TestAntonPhaseTwoHatcheriesOpenInfectAndRecover(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)

	state := fixture.state(t)
	open := make([]int, 0, 4)
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		if state.Lifecycle.Objectives[mapID].State == raid.AntonDungeonOpen {
			open = append(open, mapID)
		}
	}
	if len(open) != antonHatcheryOpenCount {
		t.Fatalf("open hatcheries=%v want %d", open, antonHatcheryOpenCount)
	}
	// raidRewardRandom returns 0: 221 is omitted, 222 is infected.
	if state.Lifecycle.Objectives[221].State == raid.AntonDungeonOpen || state.Lifecycle.InfectionMapID != 222 {
		t.Fatalf("omitted/infected selection open=%v infection=%d", open, state.Lifecycle.InfectionMapID)
	}

	// Hatchery reductions: 1000 anywhere open, the extra 500 only in the infected one.
	fixture.enterRun(t, 1001, 222, 21)
	fixture.enterRun(t, 1002, 223, 22)
	fixture.report(t, 1001, antonSymbolBarrier, antonHatcheryReduction, runtimeRaidSymbolOperationSubtract)
	fixture.report(t, 1001, antonSymbolBarrier, antonInfectedHatcheryExtraReduction, runtimeRaidSymbolOperationSubtract)
	fixture.report(t, 1002, antonSymbolBarrier, antonHatcheryReduction, runtimeRaidSymbolOperationSubtract)
	fixture.report(t, 1002, antonSymbolBarrier, antonInfectedHatcheryExtraReduction, runtimeRaidSymbolOperationSubtract)
	want := antonBarrierMaximum - 2*antonHatcheryReduction - antonInfectedHatcheryExtraReduction
	if got := fixture.state(t).Lifecycle.Barrier; got != want {
		t.Fatalf("barrier after hatchery reports=%d want=%d", got, want)
	}

	// Clearing a hatchery marks it cleared and schedules its recovery.
	fixture.clear(t, 223)
	if fixture.objective(t, 223).State != raid.AntonDungeonCleared {
		t.Fatalf("cleared hatchery=%+v", fixture.objective(t, 223))
	}
	recovery := fixture.task(t, runtimeRaidMechanismRecovery, 223)
	if recovery.delay != antonHatcheryRecovery {
		t.Fatalf("hatchery recovery delay=%s want=%s", recovery.delay, antonHatcheryRecovery)
	}
	fixture.queue.fire(recovery, false)
	if recovered := fixture.objective(t, 223); recovered.State != raid.AntonDungeonOpen || recovered.RemainingHP != 1 || recovered.ClearCount != 0 {
		t.Fatalf("recovered hatchery=%+v", recovered)
	}
}

func TestAntonPhaseTwoEnergyRepeatsAndVolcanoEndsTheSiege(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)

	fixture.clear(t, antonEnergyInterceptionMapID)
	if energy := fixture.objective(t, antonEnergyInterceptionMapID); energy.State != raid.AntonDungeonOpen || energy.RemainingHP != 1 || energy.ClearCount != 0 {
		t.Fatalf("energy interception after a clear=%+v (must be repeatable)", energy)
	}

	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Lifecycle.VolcanoOpen = true
	fixture.service.raidMu.Unlock()
	fixture.clear(t, antonBlackVolcanoMapID)
	state := fixture.state(t)
	for _, mapID := range []int{antonEnergyInterceptionMapID, antonBlackVolcanoMapID, 221, 222, 223, 224} {
		if state.Lifecycle.Objectives[mapID].State != raid.AntonDungeonHide {
			t.Fatalf("gate %d stayed %s after the volcano fell", mapID, state.Lifecycle.Objectives[mapID].State)
		}
	}
	if state.Lifecycle.Objectives[antonHeartMapID].State != raid.AntonDungeonOpen || state.Lifecycle.Objectives[antonHeartMapID].RemainingHP != 5 {
		t.Fatalf("heart after the volcano fell=%+v", state.Lifecycle.Objectives[antonHeartMapID])
	}
	if state.Lifecycle.VolcanoOpen || state.Lifecycle.InfectionMapID != 0 || len(state.Lifecycle.Mechanisms) != 0 {
		t.Fatalf("siege bookkeeping survived the volcano: volcano=%t infection=%d timers=%d", state.Lifecycle.VolcanoOpen, state.Lifecycle.InfectionMapID, len(state.Lifecycle.Mechanisms))
	}
	for i := 0; i < 5; i++ {
		fixture.clear(t, antonHeartMapID)
	}
	if !runtimeRaidPhaseObjectivesComplete(fixture.state(t), runtimeRaidDungeonPhaseTwo) {
		t.Fatal("five heart clears did not complete phase two")
	}
}

// The hatchery presentation window is opened by the server, never by the client.
// etc/raid/anton.etc only self-perpetuates [SET TIMER] 1 <hatchery> 45 and
// [SET TIMER] 3 <hatchery> 50 *after* [CHECK TIMER END] 1/3 <hatchery> has fired
// once, so the first 120s window has to arrive over 0x0254 as timer type 1 and
// timer type 3 keyed by the hatchery dungeon id. 86JP arms exactly that pair in
// StartHatcheryEffectTimersAsync (RaidHandler.Progression.cs:588-600) and clears
// it in ClearHatcheryAsync (:393-397); live traffic shows this server never sent
// type 1 or type 3 for 221..224, which left [SET] [<id> HATCHERY NAMED CREATE]
// unreachable on every client.
func TestAntonPhaseTwoHatcheryEffectTimersSeedTheClientScript(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)

	state := fixture.state(t)
	open := make([]int, 0, antonHatcheryOpenCount)
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		if state.Lifecycle.Objectives[mapID].State == raid.AntonDungeonOpen {
			open = append(open, mapID)
		}
	}
	if len(open) != antonHatcheryOpenCount {
		t.Fatalf("open hatcheries=%v want %d", open, antonHatcheryOpenCount)
	}

	// Every open hatchery carries both windows, seeded with the 86JP 120s.
	for _, mapID := range open {
		for _, kind := range []byte{runtimeRaidMechanismActive, runtimeRaidMechanismHatcheryOpen} {
			timer, ok := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: mapID}]
			if !ok {
				t.Fatalf("hatchery %d has no type-%d effect window: %+v", mapID, kind, state.Lifecycle.Mechanisms)
			}
			if window := timer.Deadline.Sub(timer.StartedAt); window != antonHatcheryEffectWindow {
				t.Fatalf("hatchery %d type %d window=%s want=%s", mapID, kind, window, antonHatcheryEffectWindow)
			}
		}
	}
	// The hatchery the raid left closed stays quiet.
	for _, kind := range []byte{runtimeRaidMechanismActive, runtimeRaidMechanismHatcheryOpen} {
		if _, ok := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: 221}]; ok {
			t.Fatalf("the unopened hatchery 221 armed a type-%d window", kind)
		}
	}

	// The windows reach the client as 0x0254 rows keyed by the hatchery id.
	sent := make(map[[2]uint32]uint32)
	for _, notification := range runtimeRaidMechanismTimerNotifications(state, fixture.queue.Now()) {
		if notification.msgID != raid.RaidTimerMsgID || len(notification.body) != 13 {
			t.Fatalf("mechanism timer notification=%+v", notification)
		}
		key := [2]uint32{uint32(notification.body[0]), binary.LittleEndian.Uint32(notification.body[1:5])}
		sent[key] = binary.LittleEndian.Uint32(notification.body[9:13])
	}
	for _, mapID := range open {
		for _, kind := range []byte{runtimeRaidMechanismActive, runtimeRaidMechanismHatcheryOpen} {
			got, ok := sent[[2]uint32{uint32(kind), uint32(mapID)}]
			if !ok {
				t.Fatalf("0x0254 never carried timer type %d for hatchery %d: %v", kind, mapID, sent)
			}
			if want := uint32(antonHatcheryEffectWindow / time.Second); got != want {
				t.Fatalf("0x0254 type %d hatchery %d seconds=%d want=%d", kind, mapID, got, want)
			}
		}
	}

	// The type-3 window pulses [<id> HATCHERY NAMED CREATE] and repeats at 50s.
	namedMapID := open[0]
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, namedMapID), false)
	_, namedSymbol, _ := antonHatcherySymbolPlan(namedMapID)
	if got := fixture.state(t).Lifecycle.MovieSymbols[namedSymbol]; got != 1 {
		t.Fatalf("hatchery %d named symbol %d=%d want 1", namedMapID, namedSymbol, got)
	}
	namedRepeat := fixture.state(t).Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: runtimeRaidMechanismHatcheryOpen, MapID: namedMapID}]
	if window := namedRepeat.Deadline.Sub(namedRepeat.StartedAt); window != antonHatcheryNamedRepeat {
		t.Fatalf("named repeat=%s want=%s", window, antonHatcheryNamedRepeat)
	}

	// The type-1 window pulses [<id> HATCHERY FAIL] and repeats at 45s.
	failMapID := open[1]
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismActive, failMapID), false)
	failSymbol, _, _ := antonHatcherySymbolPlan(failMapID)
	if got := fixture.state(t).Lifecycle.MovieSymbols[failSymbol]; got != 1 {
		t.Fatalf("hatchery %d fail symbol %d=%d want 1", failMapID, failSymbol, got)
	}
	failRepeat := fixture.state(t).Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: runtimeRaidMechanismActive, MapID: failMapID}]
	if window := failRepeat.Deadline.Sub(failRepeat.StartedAt); window != antonHatcheryFailRepeat {
		t.Fatalf("fail repeat=%s want=%s", window, antonHatcheryFailRepeat)
	}

	// Clearing the hatchery drops both windows and drives both symbols back to 0,
	// exactly like 86JP's ClearHatcheryAsync.
	fixture.clear(t, failMapID)
	cleared := fixture.state(t)
	for _, kind := range []byte{runtimeRaidMechanismActive, runtimeRaidMechanismHatcheryOpen} {
		if _, ok := cleared.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: failMapID}]; ok {
			t.Fatalf("cleared hatchery %d kept its type-%d window", failMapID, kind)
		}
	}
	if got := cleared.Lifecycle.MovieSymbols[failSymbol]; got != 0 {
		t.Fatalf("cleared hatchery %d kept fail symbol %d=%d", failMapID, failSymbol, got)
	}

	// The recovery timer re-arms the pair, like RunHatcheryRecoveryTimerAsync ->
	// StartHatcheryEffectTimersAsync.
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismRecovery, failMapID), false)
	if recovered := fixture.objective(t, failMapID); recovered.State != raid.AntonDungeonOpen {
		t.Fatalf("recovered hatchery=%+v", recovered)
	}
	for _, kind := range []byte{runtimeRaidMechanismActive, runtimeRaidMechanismHatcheryOpen} {
		if _, ok := fixture.state(t).Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: kind, MapID: failMapID}]; !ok {
			t.Fatalf("recovered hatchery %d did not re-arm its type-%d window", failMapID, kind)
		}
	}
}

// The phase-two symbol table has to carry the hatchery presentation pair, exactly
// like 86JP's AntonSecondPhaseInitialSymbols (20 rows, RaidHandler.AntonDefinitions.cs:116-138).
//
// Those two ids are what the client's raid UI resolves for every hatchery row and
// what its own script sets when the timer windows above expire. This service used
// to omit them, so the first time either side touched one of them the client
// walked a raid object that had never been created: live 2026-09-17 that single
// walk is the entry crash (eip=0 at 0x025BD2FF/0x0263A2ED) and the
// return-to-town crash (access_address=0xB98 at 0x01F95C63/0x01F95611).
//
// The value is this service's own current one, not a forced 0, so a boundary
// snapshot repeats what the server believes instead of undoing a pulse the
// client's script already applied.
func TestAntonPhaseTwoSymbolTableSeedsHatcheryPresentationIds(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)

	// Fresh phase two: every hatchery fail/named id exists with 0.
	rows := runtimeRaidPhaseTwoSymbolRows(fixture.state(t))
	seen := make(map[uint32]uint32, len(rows))
	for _, row := range rows {
		seen[row.Key] = row.Value
	}
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		fail, named, ok := antonHatcherySymbolPlan(mapID)
		if !ok {
			t.Fatalf("hatchery %d has no symbol plan", mapID)
		}
		for _, key := range []uint32{fail, named} {
			value, present := seen[key]
			if !present {
				t.Fatalf("phase-two symbol table omits hatchery symbol %d (map %d): %v", key, mapID, rows)
			}
			if value != 0 {
				t.Fatalf("fresh phase two symbol %d=%d want 0", key, value)
			}
		}
	}

	// The wire body the boundary snapshot sends carries all 20 rows: the seven
	// 57..63 objective rows plus this service's five mechanism rows plus the eight
	// hatchery presentation ids.
	symbolBody, _, _, err := runtimeRaidPhaseSnapshotBodies(fixture.state(t))
	if err != nil {
		t.Fatalf("phase snapshot: %v", err)
	}
	if len(symbolBody) == 0 || len(symbolBody) != 1+int(symbolBody[0])*8 {
		t.Fatalf("symbol body len=%d does not match its count byte", len(symbolBody))
	}
	wire := make(map[uint32]uint32)
	for index := 0; index < int(symbolBody[0]); index++ {
		offset := 1 + index*8
		wire[binary.LittleEndian.Uint32(symbolBody[offset:offset+4])] =
			binary.LittleEndian.Uint32(symbolBody[offset+4 : offset+8])
	}
	if len(wire) != int(symbolBody[0]) {
		t.Fatalf("symbol body repeats a key: count=%d unique=%d", symbolBody[0], len(wire))
	}
	for _, key := range []uint32{3, 4, 5, 6, 9, 10, 11, 12} {
		if _, present := wire[key]; !present {
			t.Fatalf("boundary symbol body omits hatchery id %d: %v", key, wire)
		}
	}
	if len(wire) != 20 {
		t.Fatalf("boundary symbol body has %d rows want 20 (7 objectives + 5 mechanism + 8 hatchery): %v", len(wire), wire)
	}

	// After the fail window fires, the same table carries the pulse value, so a
	// boundary snapshot repeats it rather than resetting the row.
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)
	state := fixture.state(t)
	open := 0
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		if state.Lifecycle.Objectives[mapID].State == raid.AntonDungeonOpen {
			open = mapID
			break
		}
	}
	if open == 0 {
		t.Fatal("no hatchery opened")
	}
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismActive, open), false)
	failSymbol, _, _ := antonHatcherySymbolPlan(open)
	pulsed := runtimeRaidPhaseTwoSymbolRows(fixture.state(t))
	found := false
	for _, row := range pulsed {
		if row.Key != failSymbol {
			continue
		}
		found = true
		if row.Value != 1 {
			t.Fatalf("fired fail window symbol %d=%d want 1 in the table", row.Key, row.Value)
		}
	}
	if !found {
		t.Fatalf("after the fail window fired, symbol %d left the table: %v", failSymbol, pulsed)
	}
}

// parseRuntimeRaidDungeonStateBody splits the team-mode 0x0249 body the way the
// current EXE's handler sub_1D39B50 does: u8 reserved, u8 count, then count
// (dungeonID u32, state u8) rows, then the trailing u32. It deliberately returns
// the leftover so a layout drift is a test failure rather than a silent misread.
func parseRuntimeRaidDungeonStateBody(t *testing.T, body []byte) (rows map[uint32]byte, tail uint32) {
	t.Helper()
	if len(body) < 2+4 {
		t.Fatalf("0x0249 body too short: len=%d", len(body))
	}
	count := int(body[1])
	if len(body) != 2+count*5+4 {
		t.Fatalf("0x0249 body len=%d does not match count=%d (want %d)", len(body), count, 2+count*5+4)
	}
	rows = make(map[uint32]byte, count)
	offset := 2
	for index := 0; index < count; index++ {
		dungeonID := binary.LittleEndian.Uint32(body[offset : offset+4])
		rows[dungeonID] = body[offset+4]
		offset += 5
	}
	tail = binary.LittleEndian.Uint32(body[offset : offset+4])
	return rows, tail
}

// The infected hatchery is identified to this client by ONE value: the trailing
// dword of 0x0249. The EXE's 0x0249 handler reads it after the rows and calls
// sub_17B87B0(manager, value), which is the only writer of manager+0x150 in the
// binary and which re-derives symbol 8 ([INFECTION DUNGEON INDEX]) and symbol 7
// ([IS EXIST INFECTION DUNGEON]) from it. The 战况表 renderers read that same
// field back, so a zero tail is not "no opinion" -- it actively clears symbol 8
// that the 0x0247 table set one packet earlier, which is why the board never
// showed the 感染孵化场 marker.
//
// This locks the value onto the wire and pins the suppression edge, where the
// client's own derived symbol 7 has to agree with our 0x0247 row.
func TestAntonPhaseTwoDungeonStateTableCarriesTheInfectedHatcheryID(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)

	// Before the hatcheries open there is no infection, so both projections are 0.
	_, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(fixture.state(t))
	if err != nil {
		t.Fatalf("phase snapshot: %v", err)
	}
	if _, tail := parseRuntimeRaidDungeonStateBody(t, dungeonBody); tail != 0 {
		t.Fatalf("pre-open 0x0249 tail=%d want 0", tail)
	}

	// The volcano's open timer is what opens the hatcheries and infects one of them.
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)
	state := fixture.state(t)
	infection := state.Lifecycle.InfectionMapID
	if !isAntonHatcheryMapID(infection) {
		t.Fatalf("no hatchery was infected: infection=%d", infection)
	}
	if state.Lifecycle.InfectionSuppressed {
		t.Fatal("a freshly infected hatchery must not start suppressed")
	}

	// The infected hatchery is one of the OPEN ones and keeps the same wire state
	// as its siblings: the marker rides the tail, never the row state.
	rows, tail := parseRuntimeRaidDungeonStateBody(t, mustRuntimeRaidPhaseDungeonBody(t, fixture))
	if tail != uint32(infection) {
		t.Fatalf("0x0249 tail=%d want the infected hatchery %d", tail, infection)
	}
	infectedState, present := rows[uint32(infection)]
	if !present {
		t.Fatalf("0x0249 rows omit the infected hatchery %d: %v", infection, rows)
	}
	if infectedState != 0 {
		t.Fatalf("infected hatchery %d state=%d want 0 (open), like every other open hatchery", infection, infectedState)
	}

	// Symbols 7/8 and the tail are one fact. They must never disagree.
	rows7and8 := make(map[uint32]uint32, 2)
	for _, row := range runtimeRaidPhaseTwoSymbolRows(state) {
		if row.Key == antonSymbolInfectionFlag || row.Key == antonSymbolInfectionMapID {
			rows7and8[row.Key] = row.Value
		}
	}
	if rows7and8[antonSymbolInfectionMapID] != uint32(infection) {
		t.Fatalf("symbol 8=%d want %d", rows7and8[antonSymbolInfectionMapID], infection)
	}
	if rows7and8[antonSymbolInfectionFlag] != 1 {
		t.Fatalf("symbol 7=%d want 1 while a hatchery is infected", rows7and8[antonSymbolInfectionFlag])
	}

	// Clearing the infected hatchery suppresses the marker: 86JP writes symbol 7
	// back to 0 on that edge (ClearHatcheryAsync), and this client derives symbol 7
	// from the tail, so the tail has to fall back to 0 with it.
	suppressed := state
	suppressed.Lifecycle.InfectionSuppressed = true
	if got := runtimeRaidInfectionMapIDForWire(suppressed); got != 0 {
		t.Fatalf("suppressed infection still projects %d onto the wire", got)
	}
	for _, row := range runtimeRaidPhaseTwoSymbolRows(suppressed) {
		if row.Key == antonSymbolInfectionFlag && row.Value != 0 {
			t.Fatalf("suppressed infection symbol 7=%d want 0", row.Value)
		}
	}

	// And a phase two with no infection at all keeps both at 0.
	cleared := state
	cleared.Lifecycle.InfectionMapID = 0
	cleared.Lifecycle.InfectionSuppressed = false
	if got := runtimeRaidInfectionMapIDForWire(cleared); got != 0 {
		t.Fatalf("infection-free phase two projects %d onto the wire", got)
	}
}

func mustRuntimeRaidPhaseDungeonBody(t *testing.T, fixture antonPhaseTwoFixture) []byte {
	t.Helper()
	_, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(fixture.state(t))
	if err != nil {
		t.Fatalf("phase snapshot: %v", err)
	}
	return dungeonBody
}
