package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// lukeGeneratorTestConfig carries the two luke.etc tables the generator logic
// reads, verbatim: [DEFAULT OBJECT COUNT] and [1PHASE OBJ DESTROY].
func lukeGeneratorTestConfig() *raid.LukeConfig {
	return &raid.LukeConfig{
		SanctumObjectCounts: []raid.LukeSanctumObjectCount{
			{Section: "birth sanctum", Count: 5, DungeonID: 5054},
			{Section: "extinction sanctum", Count: 5, DungeonID: 5055},
			{Section: "spread sanctum", Count: 5, DungeonID: 5056},
		},
		PhaseOneObjectDestroy: []raid.LukePhaseOneObjectDestroy{
			{Group: 0, Minimum: 0, Maximum: 5},
			{Group: 1, Minimum: 6, Maximum: 10},
			{Group: 2, Minimum: 11, Maximum: 15},
		},
	}
}

func lukePhaseOneGeneratorState() *runtimeRaidState {
	return &runtimeRaidState{
		RaidKey:    0x00300001,
		RaidMode:   runtimeRaidOwnerLuke,
		Started:    true,
		LukeConfig: lukeGeneratorTestConfig(),
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive,
			Phase:  runtimeRaidDungeonPhaseOne,
		},
	}
}

func TestLukeGeneratorCountsIntoItsSanctumAndSaturatesAtFive(t *testing.T) {
	state := lukePhaseOneGeneratorState()
	for i := uint32(1); i <= 5; i++ {
		count, counted := countRuntimeRaidLukeGeneratorLocked(state, lukeBirthSanctumMapID)
		if !counted || count.Symbol != lukeSymbolBirthSanctumObject || count.After != i || count.Cap != 5 {
			t.Fatalf("generator %d: count=%+v counted=%t", i, count, counted)
		}
	}
	if count, counted := countRuntimeRaidLukeGeneratorLocked(state, lukeBirthSanctumMapID); counted || count.After != 5 {
		t.Fatalf("6th generator: count=%+v counted=%t want saturated at 5", count, counted)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolExtinctionSanctumObject); got != 0 {
		t.Fatalf("birth generators leaked into extinction: %d", got)
	}
	// Tier of 5 destroyed is 0; five more elsewhere make it 1.
	if tier := runtimeRaidLukeEnergyControlTierLocked(state); tier != 0 {
		t.Fatalf("tier after 5 = %d want 0", tier)
	}
	countRuntimeRaidLukeGeneratorLocked(state, lukeExtinctionSanctumMapID)
	if tier := runtimeRaidLukeEnergyControlTierLocked(state); tier != 1 {
		t.Fatalf("tier after 6 = %d want 1", tier)
	}
}

func TestLukeGeneratorIgnoredOutsidePhaseOneAndOutsideSanctums(t *testing.T) {
	state := lukePhaseOneGeneratorState()
	if _, counted := countRuntimeRaidLukeGeneratorLocked(state, lukeLightAltarMapID); counted {
		t.Fatal("a phase-two map counted a generator")
	}
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	if _, counted := countRuntimeRaidLukeGeneratorLocked(state, lukeBirthSanctumMapID); counted {
		t.Fatal("phase two counted a sanctum generator")
	}
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.TailFlag = 1
	if _, counted := countRuntimeRaidLukeGeneratorLocked(state, lukeBirthSanctumMapID); counted {
		t.Fatal("a single-mode raid counted a generator")
	}
}

// Every member's client runs the machine's destroy action, so one machine
// arrives once per member in the room. A room is credited with the most reports
// any single member made there; rooms and runs are independent.
func TestLukeGeneratorReportsDeduplicateAcrossMembersPerRoom(t *testing.T) {
	state := lukePhaseOneGeneratorState()
	roomA := runtimeRaidLukeGeneratorReportKey{
		MapID: lukeExtinctionSanctumMapID, LifecycleToken: 7,
		Room: runtimeDungeonRoomKey{X: 1, Y: 0, MapID: 74139},
	}
	roomB := roomA
	roomB.Room = runtimeDungeonRoomKey{X: 2, Y: 0, MapID: 74140}
	nextRun := roomA
	nextRun.LifecycleToken = 8

	steps := []struct {
		key       runtimeRaidLukeGeneratorReportKey
		member    uint16
		wantFresh bool
	}{
		{roomA, 1, true},   // member 1 breaks the first machine
		{roomA, 42, false}, // member 42's client reports the same machine
		{roomA, 42, true},  // member 42 breaks a second machine first
		{roomA, 1, false},  // member 1's copy of that second machine
		{roomB, 1, true},   // another room is its own ledger
		{nextRun, 1, true}, // so is the next run of the sanctum
		{nextRun, 42, false},
	}
	for i, step := range steps {
		_, fresh := recordRuntimeRaidLukeGeneratorReportLocked(state, step.key, step.member)
		if fresh != step.wantFresh {
			t.Fatalf("step %d (%+v member %d): fresh=%t want %t", i, step.key, step.member, fresh, step.wantFresh)
		}
		if fresh {
			countRuntimeRaidLukeGeneratorLocked(state, step.key.MapID)
		}
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolExtinctionSanctumObject); got != 4 {
		t.Fatalf("extinction counter = %d want 4 (two in room A, one in room B, one next run)", got)
	}
	// The snapshot clone must not share the ledger with the live state.
	clone := cloneRuntimeRaidState(*state)
	clone.Lifecycle.LukeGeneratorReports[roomA].ByMember[1] = 99
	if got := state.Lifecycle.LukeGeneratorReports[roomA].ByMember[1]; got != 2 {
		t.Fatalf("clone aliased the live ledger: member 1 = %d", got)
	}
}

// lukeGeneratorRunInSanctum binds one session to a live run of a Luke phase-one
// sanctum. localToken is this member's OWN lifecycle token; bindingToken is the
// SQUAD's run token, which is what an adopting squadmate's binding carries (see
// the adoption branch of beginRuntimeRaidDungeon).
func lukeGeneratorRunInSanctum(
	session *gameSession,
	charID uint16,
	raidKey uint32,
	mapID int,
	localToken uint64,
	bindingToken uint64,
) *runtimeDungeonState {
	run := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(mapID)},
		Character:      dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(charID))},
		lifecycleToken: localToken,
	}
	run.raidDungeon = runtimeRaidDungeonBinding{
		RaidKey:        raidKey,
		Phase:          runtimeRaidDungeonPhaseOne,
		MapID:          mapID,
		LifecycleToken: bindingToken,
	}
	run.Room = &runtimeDungeonRoom{mapID: int64(mapID) + 68600}
	session.dungeon = dungeonSessionState{runtime: run}
	return run
}

func lukeGeneratorReportBody(symbol uint32) []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint32(body[0:4], symbol)
	binary.LittleEndian.PutUint32(body[4:8], 1)
	body[8] = runtimeRaidSymbolOperationAdd
	return body
}

// lukeGeneratorLiveRaid turns a fixture's roster into a started, active Luke
// phase-one raid.
func lukeGeneratorLiveRaid(t *testing.T, service *Service, raidKey uint32) {
	t.Helper()
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	state := service.raids[raidKey]
	if state == nil {
		t.Fatalf("raid %d is not registered", raidKey)
	}
	state.RaidKey = raidKey
	state.RaidMode = runtimeRaidOwnerLuke
	state.TailFlag = 0
	state.Started = true
	state.StartPending = false
	state.LukeConfig = lukeGeneratorTestConfig()
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
}

// A machine's destroy action runs on EVERY member's client, and a squadmate that
// walked into the opener's run carries the SQUAD's token in its binding while its
// own local token is different. Comparing the two here silently dropped that
// member's report -- the exact failure upstream had already removed from the Anton
// twin (runtimeRaidCurrentRaidDungeonRun, 2026-09-19 raid 1507338) and left in this
// Luke helper, which the port carried across:
//
//	live 2026-09-29 09:21-09:23Z, raid 1835009, 湮灭之圣所: char 22 opened the run
//	(binding token 7), char 1 followed it in (its own token 8). The machine's
//	[RAID SET SYMBOL] 118 [+] 1 arrived as op662 {118, 1, add} and was declined,
//	fell through to the Anton barrier owner and was rejected as
//	not_inside_energy_or_hatchery_run; 破坏目标 stayed 5/5 while the opener's own
//	reports had counted to 5 earlier the same day.
func TestLukeGeneratorReportCountsForAnAdoptedSquadmate(t *testing.T) {
	fixture := newStressRaidProjectionFixture(t, 2)
	lukeGeneratorLiveRaid(t, fixture.service, fixture.raidKey)
	opener, adopted := fixture.members[0], fixture.members[1]

	// The opener's own run: binding token == its local token.
	lukeGeneratorRunInSanctum(opener.session, opener.charID, fixture.raidKey, lukeExtinctionSanctumMapID, 7, 7)
	// The squadmate that walked into that same run.
	lukeGeneratorRunInSanctum(adopted.session, adopted.charID, fixture.raidKey, lukeExtinctionSanctumMapID, 8, 7)

	// One machine per member's client, reported from the adopted member first.
	for _, member := range []stressRaidProjectionMember{adopted, opener} {
		if err := fixture.service.handleOnlineRaidSetSymbol(
			member.session, 662, lukeGeneratorReportBody(lukeSymbolExtinctionSanctumObject),
		); err != nil {
			t.Fatalf("char %d generator report: %v", member.charID, err)
		}
	}

	fixture.service.raidMu.Lock()
	defer fixture.service.raidMu.Unlock()
	live := fixture.service.raids[fixture.raidKey]
	if got := runtimeRaidLukeSymbolLocked(live, lukeSymbolExtinctionSanctumObject); got != 1 {
		t.Fatalf("湮灭 generator counter = %d want 1: the adopted member's report was dropped "+
			"(both clients reported the same machine, so the room credits it once)", got)
	}
	// The ledger is keyed on the SQUAD's token, not on either member's local one,
	// so both members' reports land in the same room entry.
	key := runtimeRaidLukeGeneratorReportKey{
		MapID:          lukeExtinctionSanctumMapID,
		LifecycleToken: 7,
		Room:           runtimeDungeonRoomKey{MapID: int64(lukeExtinctionSanctumMapID) + 68600},
	}
	room, ok := live.Lifecycle.LukeGeneratorReports[key]
	if !ok {
		t.Fatalf("no generator ledger for the squad's run token; entries=%v", live.Lifecycle.LukeGeneratorReports)
	}
	if room.ByMember[adopted.charID] != 1 || room.ByMember[opener.charID] != 1 || room.Credited != 1 {
		t.Fatalf("ledger = %+v want one report from each member and one credited machine", room)
	}
}

// The Luke claim must keep declining everything that is not a Luke phase-one
// sanctum, so Anton's own op662 reaches applyRuntimeRaidBarrierRequest with the
// same request at the same moment it always did. Map and symbol both have to miss:
// Anton stands in 210/218/21x and reports symbol 110, Luke sanctums are 5054/5055/
// 5056 and report 117/118/119.
func TestLukeGeneratorClaimDeclinesEveryAntonReport(t *testing.T) {
	fixture := newStressRaidProjectionFixture(t, 2)
	lukeGeneratorLiveRaid(t, fixture.service, fixture.raidKey)
	member := fixture.members[0]

	antonRun := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: antonEnergyInterceptionMapID},
		Character:      dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(member.charID))},
		lifecycleToken: 11,
	}
	antonRun.raidDungeon = runtimeRaidDungeonBinding{
		RaidKey:        fixture.raidKey,
		Phase:          runtimeRaidDungeonPhaseTwo,
		MapID:          antonEnergyInterceptionMapID,
		LifecycleToken: 11,
	}
	antonRun.Room = &runtimeDungeonRoom{mapID: 999}
	member.session.dungeon = dungeonSessionState{runtime: antonRun}

	reports := []raid.SetSymbolRequest{
		{Symbol: runtimeRaidSymbolOperationSubtract, SourceValue: antonSymbolBarrier, TargetValue: 1},
		// Even a Luke symbol counts for nothing on an Anton map ...
		{Symbol: runtimeRaidSymbolOperationAdd, SourceValue: lukeSymbolExtinctionSanctumObject, TargetValue: 1},
	}
	for index, report := range reports {
		if fixture.service.applyRuntimeRaidLukeGeneratorReport(member.session, report) {
			t.Fatalf("report %d (%+v) was claimed by the Luke generator on an Anton run", index, report)
		}
	}

	// ... and an Anton symbol counts for nothing inside a Luke sanctum.
	lukeGeneratorRunInSanctum(member.session, member.charID, fixture.raidKey, lukeExtinctionSanctumMapID, 12, 12)
	if fixture.service.applyRuntimeRaidLukeGeneratorReport(member.session, raid.SetSymbolRequest{
		Symbol: runtimeRaidSymbolOperationSubtract, SourceValue: antonSymbolBarrier, TargetValue: 1,
	}) {
		t.Fatal("Anton's barrier report was claimed inside a Luke sanctum")
	}
	fixture.service.raidMu.Lock()
	defer fixture.service.raidMu.Unlock()
	if got := runtimeRaidLukeSymbolLocked(fixture.service.raids[fixture.raidKey], lukeSymbolExtinctionSanctumObject); got != 0 {
		t.Fatalf("a declined report moved the sanctum counter to %d", got)
	}
}
