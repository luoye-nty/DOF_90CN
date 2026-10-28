package dnfbridge

import (
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// Luke phase-one generators ("破坏目标 x/5").
//
// Each phase-one sanctum names its own counter in [DESTROY OBJECT INFO]
// ([BIRTH SANCTUM OBJECT] = symbol 117, extinction 118, spread 119) and caps it
// at [DEFAULT OBJECT COUNT] 5. luke.etc only zeroes them ([1PHASE INIT]) and
// resets one on giving up its sanctum ([RESET SANCTUM OBJECT]); [2PHASE INIT]
// leaves them alone. What reads them is [1PHASE OBJ DESTROY]: their total picks
// the tier stored in [ENERGY CONTROL OPEN] (133), and the war board draws each
// sanctum's five target icons lit for cap - symbol (DNF.exe sub_10D84D0: cap =
// sub_10D8450 clamped to 5, value = the raid symbol sub_17C2CF0 names). A fresh
// sanctum therefore shows 5/5 and loses one icon per destroyed generator.
//
// The generator is the sanctum's main machine, not the batteries or dry cells
// around it (湮灭之圣所炼金炉 = sanctum_of_extinction/mainmchine_raid.obj). Its
// PVF is [hp destroy] 0: at HP <= 1 its basic action switches to the destroy
// action, whose only behaviour is [RAID SET SYMBOL] <sanctum symbol> [+] 1:
//
//	birth       luke_mainmchine_destroy_raid.act / _mistral_raid.act   117 + 1
//	extinction  mainmchine_destroy_raid.act                            118 + 1
//	spread      mainmchine_destroy.act / censor_machine_broken_front_raid.act 119 + 1
//
// The client turns that into C2S op662 {u32 symbol, u32 operand, u8 operation}
// (sub_17B1600). No op39 is sent for the machine. The script runs on every
// member's client, so one machine arrives once per member standing in the
// room; a room is credited with the most reports any single member made there.

type runtimeRaidLukeGeneratorReportKey struct {
	MapID          int
	LifecycleToken uint64
	Room           runtimeDungeonRoomKey
}

type runtimeRaidLukeGeneratorRoomReports struct {
	ByMember map[uint16]uint32
	Credited uint32
}

func cloneRuntimeRaidLukeGeneratorReports(
	reports map[runtimeRaidLukeGeneratorReportKey]runtimeRaidLukeGeneratorRoomReports,
) map[runtimeRaidLukeGeneratorReportKey]runtimeRaidLukeGeneratorRoomReports {
	if reports == nil {
		return nil
	}
	out := make(map[runtimeRaidLukeGeneratorReportKey]runtimeRaidLukeGeneratorRoomReports, len(reports))
	for key, room := range reports {
		byMember := make(map[uint16]uint32, len(room.ByMember))
		for member, count := range room.ByMember {
			byMember[member] = count
		}
		out[key] = runtimeRaidLukeGeneratorRoomReports{ByMember: byMember, Credited: room.Credited}
	}
	return out
}

func runtimeRaidLukeSanctumObjectSymbol(mapID int) (uint32, bool) {
	switch mapID {
	case lukeBirthSanctumMapID:
		return lukeSymbolBirthSanctumObject, true
	case lukeExtinctionSanctumMapID:
		return lukeSymbolExtinctionSanctumObject, true
	case lukeSpreadSanctumMapID:
		return lukeSymbolSpreadSanctumObject, true
	default:
		return 0, false
	}
}

// runtimeRaidLukeEnergyControlTierLocked is the [1PHASE OBJ DESTROY] tier of
// the three sanctum counters. A raid without its Luke config falls back to tier
// 0, the shortest respawn. Caller holds raidMu.
func runtimeRaidLukeEnergyControlTierLocked(state *runtimeRaidState) uint32 {
	if state == nil || state.LukeConfig == nil {
		return 0
	}
	total := runtimeRaidLukeSymbolLocked(state, lukeSymbolBirthSanctumObject) +
		runtimeRaidLukeSymbolLocked(state, lukeSymbolExtinctionSanctumObject) +
		runtimeRaidLukeSymbolLocked(state, lukeSymbolSpreadSanctumObject)
	tier, ok := state.LukeConfig.EnergyControlTier(int(total))
	if !ok || tier < 0 {
		return 0
	}
	return uint32(tier)
}

type runtimeRaidLukeGeneratorCount struct {
	RaidKey uint32
	Phase   byte
	Symbol  uint32
	Before  uint32
	After   uint32
	Cap     int
}

// countRuntimeRaidLukeGeneratorLocked credits one destroyed generator to the
// member's sanctum counter, saturating at [DEFAULT OBJECT COUNT]. Caller holds
// raidMu.
func countRuntimeRaidLukeGeneratorLocked(state *runtimeRaidState, mapID int) (runtimeRaidLukeGeneratorCount, bool) {
	if state == nil || state.TailFlag == 1 || !state.Started ||
		runtimeRaidTeamGraphOwner(*state) != runtimeRaidOwnerLuke ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne || state.LukeConfig == nil {
		return runtimeRaidLukeGeneratorCount{}, false
	}
	symbol, ok := runtimeRaidLukeSanctumObjectSymbol(mapID)
	if !ok {
		return runtimeRaidLukeGeneratorCount{}, false
	}
	capacity, ok := state.LukeConfig.SanctumObjectCount(uint32(mapID))
	if !ok || capacity <= 0 {
		return runtimeRaidLukeGeneratorCount{}, false
	}
	result := runtimeRaidLukeGeneratorCount{
		RaidKey: state.RaidKey,
		Phase:   state.Lifecycle.Phase,
		Symbol:  symbol,
		Before:  runtimeRaidLukeSymbolLocked(state, symbol),
		Cap:     capacity,
	}
	result.After = result.Before
	if int(result.Before) >= capacity {
		return result, false
	}
	result.After = result.Before + 1
	setRuntimeRaidLukeSymbolLocked(state, symbol, result.After)
	return result, true
}

// recordRuntimeRaidLukeGeneratorReportLocked books one member's op662 for a
// room and reports whether it is a machine no other member has reported yet.
// Caller holds raidMu.
func recordRuntimeRaidLukeGeneratorReportLocked(
	state *runtimeRaidState,
	key runtimeRaidLukeGeneratorReportKey,
	characterID uint16,
) (runtimeRaidLukeGeneratorRoomReports, bool) {
	if state.Lifecycle.LukeGeneratorReports == nil {
		state.Lifecycle.LukeGeneratorReports = make(map[runtimeRaidLukeGeneratorReportKey]runtimeRaidLukeGeneratorRoomReports)
	}
	room := state.Lifecycle.LukeGeneratorReports[key]
	if room.ByMember == nil {
		room.ByMember = make(map[uint16]uint32)
	}
	room.ByMember[characterID]++
	fresh := room.ByMember[characterID] > room.Credited
	if fresh {
		room.Credited = room.ByMember[characterID]
	}
	state.Lifecycle.LukeGeneratorReports[key] = room
	return room, fresh
}

// runtimeRaidCurrentRaidDungeonRoom is runtimeRaidCurrentRaidDungeonRun plus the
// room the session stands in.
//
// The binding's token is the SQUAD's run token, never the adopting member's own
// (see the adoption branch of beginRuntimeRaidDungeon), so it must NOT be compared
// with the session's local token here -- exactly the trap the Anton twin
// (runtimeRaidCurrentRaidDungeonRun) documents. Upstream's Anton helper had that
// comparison removed on 2026-09-19 (raid 1507338: every shield report but the
// opener's was silently dropped) while this Luke helper kept it, and the port
// brought the bug across:
//
//	live 2026-09-29 09:21-09:23Z, raid 1835009: char 22 opened the 湮灭之圣所 run
//	(binding token 7), char 1 walked in behind it (its own token 8, per
//	game-raid-dungeon-lifecycle-joined-clear). The machine's hand-written
//	[RAID SET SYMBOL] 118 [+] 1 arrived as op662 {118, 1, add} and was declined
//	here, fell through to the Anton barrier owner and was logged as
//	game-raid-barrier-request-rejected reason=not_inside_energy_or_hatchery_run.
//	破坏目标 stayed 5/5 for the whole run while the same reports from the run's
//	opener had counted fine earlier that day.
//
// What a report has to prove is that this member stands inside the gate its
// binding names; the binding's token stays the caller's per-run dedupe key.
func runtimeRaidCurrentRaidDungeonRoom(session *gameSession) (runtimeRaidDungeonBinding, runtimeDungeonRoomKey, bool) {
	if session == nil {
		return runtimeRaidDungeonBinding{}, runtimeDungeonRoomKey{}, false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Room == nil || runtime.raidDungeon.RaidKey == 0 ||
		runtime.raidDungeon.LifecycleToken == 0 ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return runtimeRaidDungeonBinding{}, runtimeDungeonRoomKey{}, false
	}
	runtime.Room.mu.RLock()
	room := runtimeDungeonRoomKey{X: runtime.Room.coordinate.X, Y: runtime.Room.coordinate.Y, MapID: runtime.Room.mapID}
	runtime.Room.mu.RUnlock()
	return runtime.raidDungeon, room, true
}

// applyRuntimeRaidLukeGeneratorReport owns an op662 sent from inside a Luke
// phase-one sanctum for that sanctum's own counter. It returns false for any
// other op662 so the phase-two shield owner still sees it.
func (s *Service) applyRuntimeRaidLukeGeneratorReport(session *gameSession, req raid.SetSymbolRequest) bool {
	characterID := selectedCharacterID(session)
	if s == nil || characterID == 0 {
		return false
	}
	run, room, inRun := runtimeRaidCurrentRaidDungeonRoom(session)
	if !inRun {
		return false
	}
	symbol, sanctum := runtimeRaidLukeSanctumObjectSymbol(run.MapID)
	if !sanctum || req.SourceValue != symbol {
		return false
	}
	if req.Symbol != runtimeRaidSymbolOperationAdd || req.TargetValue != 1 || run.Phase != runtimeRaidDungeonPhaseOne {
		s.logGameEvent(session, "game-raid-luke-generator-report-rejected",
			"char_id", characterID, "sanctum", run.MapID, "symbol", req.SourceValue,
			"operand", req.TargetValue, "operation", req.Symbol, "run_phase", run.Phase,
			"reason", "not_a_single_generator_increment")
		return true
	}

	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.RaidKey != run.RaidKey {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-raid-luke-generator-report-rejected",
			"char_id", characterID, "sanctum", run.MapID, "reason", "raid_not_found")
		return true
	}
	key := runtimeRaidLukeGeneratorReportKey{MapID: run.MapID, LifecycleToken: run.LifecycleToken, Room: room}
	reports, fresh := recordRuntimeRaidLukeGeneratorReportLocked(state, key, characterID)
	var count runtimeRaidLukeGeneratorCount
	counted := false
	if fresh {
		count, counted = countRuntimeRaidLukeGeneratorLocked(state, run.MapID)
	}
	s.raidMu.Unlock()

	s.logGameEvent(session, "game-raid-luke-generator-reported",
		"char_id", characterID,
		"raid_key", run.RaidKey,
		"sanctum", run.MapID,
		"room", fmt.Sprintf("%d:%d", room.X, room.Y),
		"map_id", room.MapID,
		"member_reports", reports.ByMember[characterID],
		"room_credited", reports.Credited,
		"fresh", fresh,
		"symbol", symbol,
		"count_before", count.Before,
		"count_after", count.After,
		"cap", count.Cap,
		"counted", counted)
	if counted {
		s.scheduleRuntimeRaidLukeProgressPublish(count.RaidKey, count.Phase, fmt.Sprintf("generator:%d:%d", run.MapID, count.After))
	}
	return true
}

// scheduleRuntimeRaidLukeProgressPublish publishes the phase snapshot once the
// caller's locks are gone (see scheduleRuntimeRaidLukeEntryProgressPublish).
func (s *Service) scheduleRuntimeRaidLukeProgressPublish(raidKey uint32, phase byte, reason string) {
	if s == nil || raidKey == 0 {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	name := fmt.Sprintf("raid-luke-progress:%d:%s", raidKey, reason)
	if err := queue.ScheduleAfter(name, 0, func(time.Time) {
		if err := s.publishRuntimeRaidObjectiveProgress(raidKey, phase); err != nil {
			s.logPacketEvent("game-raid-luke-progress-publish-failed",
				"raid_key", raidKey, "reason", reason, "error", err)
		}
	}); err != nil {
		s.logPacketEvent("game-raid-luke-progress-schedule-failed",
			"raid_key", raidKey, "reason", reason, "error", err)
	}
}
