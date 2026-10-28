package dnfbridge

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// runtimeRaidLifecycleStatus describes server-owned progression only. Wire
// values are deliberately kept in the protocol package and are not mirrored
// here.
type runtimeRaidLifecycleStatus byte

const (
	runtimeRaidLifecycleCreated runtimeRaidLifecycleStatus = iota
	runtimeRaidLifecyclePreparing
	runtimeRaidLifecyclePhaseActive
	runtimeRaidLifecycleAwaitingMovie
	runtimeRaidLifecycleAwaitingRewards
	runtimeRaidLifecycleStandby
	runtimeRaidLifecycleFinished
	runtimeRaidLifecycleAbandoned
	// runtimeRaidLifecycleFailed is the attack-window timeout: the raid is over
	// (86JP RaidManager.TryFailPhase sets state 4 with argument 1) but the object
	// stays alive for the ten-second result/reconnect window before the delayed
	// remover releases the roster (RunAttackTimeoutAsync +
	// RemoveFailedRaidAfterResultDelayAsync, RaidHandler.Runtime.cs:592-762).
	runtimeRaidLifecycleFailed
)

type runtimeRaidDungeonProgressStatus byte

const (
	runtimeRaidDungeonProgressSelected runtimeRaidDungeonProgressStatus = iota + 1
	runtimeRaidDungeonProgressLoaded
	runtimeRaidDungeonProgressCompleted
)

// runtimeRaidDungeonSeat names who holds a raid gate: the attack group of the member
// that opened the run, or -- when that member belongs to no attack group -- the member
// itself.
//
// A squad fights one gate at a time, so its members share one seat: the second one to
// walk in adopts the run the first one opened. A member nobody has assigned to a squad
// belongs to no group, so it gets its own seat instead of squatting on squad 1's:
// without that, two unassigned members could not be inside two different gates at the
// same time (the second entry was refused with errRuntimeRaidGroupOccupied), and an
// unassigned member standing in one gate blocked squad 1 from entering another.
// Live report 2026-09-17 21:5x, the same session in which an unassigned member's 战况表
// row turned out to be drawn as squad 1's.
type runtimeRaidDungeonSeat struct {
	Group       byte
	CharacterID uint16
}

// runtimeRaidSeatFor is the seat of one roster member.
func runtimeRaidSeatFor(member runtimeRaidMemberState) runtimeRaidDungeonSeat {
	if squad := runtimeRaidMemberSquad(member.GroupIndex); squad != 0 {
		return runtimeRaidDungeonSeat{Group: squad}
	}
	return runtimeRaidDungeonSeat{CharacterID: member.CharID}
}

// runtimeRaidDungeonSeatFor resolves the seat one run is keyed by.
//
// 安东/卢克：座位就是小队（或未编队时的个人），逐字沿用 runtimeRaidSeatFor。
//
// 超时空：区域是**共享战场**，不是小队的格子。客户端点进已有同伴的区域时自己弹的
// 是「移动后，会与处于当前城镇的同伴一同开始战斗。确认要移动吗？」；PVF 的
// [AREA INFO] 也只有 [MEMBER MAX]（每区域 4 人），没有"归属小队"。
//
// 所以超时空的座位按**区域**算：同一张图（一张图只属于一个区域）永远只有一条 run，
// 后到的人沿用先到者的座位，于是命中下面 beginRuntimeRaidDungeon 里现成的 adopt
// 分支（同座位 + 同 MapID → 沿用对方 binding 与 LifecycleToken，一场只报一次）。
//
// 注意**不能**用一个固定的零值座位：那张表的 key 只有座位，全团共用一个座位会让
// 分处两个区域的两个人互相顶掉对方的占位。
func runtimeRaidDungeonSeatFor(state runtimeRaidState, member runtimeRaidMemberState, mapID int) runtimeRaidDungeonSeat {
	seat := runtimeRaidSeatFor(member)
	if state.Lifecycle.FiendWar == nil {
		return seat
	}
	for occupiedSeat, occupied := range state.Lifecycle.GroupOccupancy {
		if occupied.MapID == mapID {
			return occupiedSeat
		}
	}
	return seat
}

// runtimeRaidDungeonRunKey identifies one gate run: one entry per (dungeon, seat), so
// two squads -- or two unassigned members -- may fight the same map at the same time
// (86JP RaidManager.cs:731-792 TryEnterDungeon refuses only a duplicate userId, and
// :1342-1349 keeps a list of participations).
type runtimeRaidDungeonRunKey struct {
	MapID int
	Seat  runtimeRaidDungeonSeat
}

type runtimeRaidDungeonAttemptKey struct {
	CharacterID  uint16
	LifecycleKey uint64
}

type runtimeRaidDungeonProgress struct {
	Phase          byte
	MapID          int
	Seat           runtimeRaidDungeonSeat
	CharacterID    uint16
	LifecycleToken uint64
	Status         runtimeRaidDungeonProgressStatus
	SelectedAt     time.Time
	LoadedAt       time.Time
	CompletedAt    time.Time
	// CoinsUsed counts the revive coins spent in this one entry, shared by the
	// squad that holds it. The record is dropped when the run is completed or
	// abandoned, so a retreat and re-entry starts again at zero -- which is the
	// dungeon's own [coin limit] rule (Luke phase one: 8 per sanctum entry).
	CoinsUsed int
}

type runtimeRaidDungeonTerminal struct {
	Phase byte
	MapID int
	Kind  runtimeRaidDungeonLifecycleKind
}

type runtimeRaidDungeonObjective struct {
	Phase       byte
	MapID       int
	State       raid.AntonDungeonState
	MaxHP       int
	RemainingHP int
	ClearCount  int
	PartyMax    int
}

type runtimeRaidRewardSelection struct {
	Mode        byte
	Index       byte
	ConfirmedAt time.Time
}

type runtimeRaidLifecycleState struct {
	Status           runtimeRaidLifecycleStatus
	Phase            byte
	Revision         uint64
	StartedAt        time.Time
	ReadyDeadline    time.Time
	PhaseStartedAt   time.Time
	PhaseDeadline    time.Time
	StandbyStartedAt time.Time
	StandbyDeadline  time.Time
	MovieOpenedAt    time.Time
	MovieConfirmed   map[uint16]bool
	Objectives       map[int]runtimeRaidDungeonObjective
	Dungeons         map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress
	// PhaseClearCounts is the per-member clear registration of the active phase.
	// The current client's 地下城通关 column renders the member record's clear byte
	// as "<cleared>/<required>", and the phase settlement only rewards members it
	// can prove cleared a gate, so this is the only place that fact lives. 86JP
	// (RaidManager.TryClearDungeon) increments every member of the clearing squad
	// and clears all counters when the next phase starts; the count therefore
	// survives a disconnect and a rejoin, which is the same lifetime as the raid
	// aggregate here.
	PhaseClearCounts map[uint16]uint32
	// RunClearCredits is the one-clear-per-run ledger of the active phase: it
	// remembers, per gate run, which run token already spent that gate's point.
	//
	// 黑雾之源 (210) needs four clears, and a clear is a PARTY clear: one entry
	// into the gate by one squad counts once, no matter how many of its members
	// walked in. This server gives every member its own instance of the room
	// (different seeds), so without this ledger a four-member squad took the whole
	// gate down in a single run -- four member clears, four points -- and a
	// two-member squad needed two runs instead of four (operator report
	// 2026-09-19). 86JP counts the same way: one RaidDungeonParticipation per
	// (dungeon, party) and one scalar clear count per (raid, dungeon)
	// (RaidManager.cs, TryClearDungeon).
	//
	// The run key is reused by the squad's next run, so opening a new run clears
	// its credit (see beginRuntimeRaidDungeon); a run whose record is already gone
	// (its owner finished first) keeps its credit until then, which is what makes
	// the late squadmate completions free.
	RunClearCredits map[runtimeRaidDungeonRunKey]uint64
	// DungeonStates is the last (gate -> wire state) table this raid broadcast for
	// DungeonStatePhase. A cleared gate keeps reading 3 for the rest of the phase, so
	// re-sending an unchanged row re-announces a finished dungeon to the client,
	// which answers each cleared row with the 「已通关[%s]地下城」 toast (string 646).
	// The live server was sending 618 whole tables in 23 minutes (2026-09-19
	// 14:23-14:46Z, raid 1507338), and every one of them re-announced 第一孵化中心
	// (221) 104 times, 第二孵化中心 (222) 52 times and 第四孵化中心 (224) 196 times.
	//
	// Only the changed rows travel after the phase's one full table (86JP:
	// Progression.cs:681-685 sends the table once at activation and every later
	// change goes out as RAID_CHANGE_DUNGEON_STATE, Runtime.cs:369-403). See
	// claimRuntimeRaidDungeonStateRows.
	DungeonStates     map[uint32]uint32
	DungeonStatePhase byte
	// DungeonStateInfection is the trailing dword of that same 0x0249: the infected
	// hatchery's map id, or 0 while none is coloured. It needs its own memory
	// because the client reads the infection from that trailer and from nowhere
	// else -- sub_1D39B50 hands it to sub_17B87B0, the only writer of
	// manager+0x150, which every 战况表 renderer compares each gate against
	// (sub_1CF26F0, and sub_17C29F0/sub_17C2B80 under the board init sub_F176A0).
	//
	// A row-only change travels as 0x024A, which has no trailer, so a gate moving
	// never carried the infection with it: the board kept the value it last saw
	// until the next WHOLE table happened to be drawn, which is why the operator
	// saw no infection until an op15/ENTER_SELECT_DUNGEON table arrived (live
	// 2026-09-20, raid 1507329: the tail only ever moved at 02:13:02Z and 02:19:13Z,
	// both op15-driven, never on the 02:13:02Z open / 02:14:35Z suppress /
	// 02:19:13Z recovery edges themselves). See claimRuntimeRaidDungeonStateRows.
	DungeonStateInfection     uint32
	GroupOccupancy            map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding
	ClosedDungeonAttempts     map[runtimeRaidDungeonAttemptKey]runtimeRaidDungeonTerminal
	RewardToken               uint64
	RewardOpenedAt            time.Time
	RewardSelections          map[uint16]runtimeRaidRewardSelection
	RewardDelivery            *runtimeRaidRewardDeliveryCache
	RewardDeliveryCommitted   bool
	RewardDeliveryCommittedAt time.Time
	FinishedAt                time.Time
	AbandonedAt               time.Time
	// FailedAt is when the attack window expired. The failed raid stays in the
	// raid table until the ten-second removal timer fires.
	FailedAt time.Time
	// Mechanisms holds the armed Anton mechanism timers of the active phase.
	// MechanismGeneration grows on every arm/clear so a dequeued callback can
	// prove it still owns its slot; PhaseCycle grows on every mechanism reset
	// so callbacks from a failed cycle cannot touch the next attempt.
	Mechanisms          map[runtimeRaidMechanismKey]runtimeRaidMechanismTimer
	MechanismGeneration uint64
	PhaseCycle          uint64
	// Phase-two barrier state. Barrier is the black-volcano shield (symbol
	// 110); VolcanoOpen mirrors symbol 111 and is the dungeon-entry authority
	// for 219; InfectionMapID is the infected hatchery (symbol 8, 0 = none) and
	// InfectionSuppressed mirrors the clearing of symbol 7: the infection marker is
	// on whenever a hatchery is infected and that hatchery is still up, and it is
	// switched off while the infected hatchery is down (86JP writes symbol 7 to 0
	// from ClearHatcheryAsync and back to 1 from TryRecoverHatcheryAsync).
	// BarrierRequests counts accepted client shield requests per dungeon run
	// so a run cannot report the same transmission twice.
	Barrier             uint32
	VolcanoOpen         bool
	InfectionMapID      int
	InfectionSuppressed bool
	BarrierRequests     map[runtimeRaidBarrierRequestKey]uint32
	// LukeGeneratorReports are the op662 generator reports of Luke's phase
	// one, per sanctum run and room (raid_luke_generator.go). Created lazily.
	LukeGeneratorReports map[runtimeRaidLukeGeneratorReportKey]runtimeRaidLukeGeneratorRoomReports
	// LukeSanctumMazeGroups is the [CARD SET MAP INFO] group each phase-one
	// sanctum walks, drawn on its first entry (raid_luke_sanctum_maze.go).
	LukeSanctumMazeGroups map[int]int
	// MovieSymbols are the one-shot 0x0247 markers the raid pushes so the client
	// plays its Anton cutscenes and barrier animations. etc/raid/anton.etc.symbol
	// owns the mapping (120 [DGN SMOKE OPEN], 122 [MOVIE SMOKE OUT], 126 [MOVIE
	// BARRIER BREAK], 127 [MOVIE BARRIER RECOVER], 128 [MOVIE HATCHERY OPEN]); a
	// value of 1 pulses the marker and it is replayed with every phase snapshot so
	// a reconnecting member keeps the same state. Live 2026-09-16: the server had
	// never sent a single 121..128 key in the whole log, which is why clearing
	// 榛戦浘涔嬫簮 four times played no video.
	MovieSymbols map[uint32]uint32
	// LukeSymbols holds the Luke team graph's counters and effect flags from
	// etc/raid/luke/luke.etc.symbol (the altar/temple counters, the two energy
	// room effects, the phase-two route marker). Unlike MovieSymbols these are
	// durable phase state rather than one-shot pulses, so the periodic snapshot
	// carries them and a reconnecting member sees the same progress. Luke's
	// symbol namespace is its own; nothing here is comparable to an Anton id.
	LukeSymbols map[uint32]uint32
	// LukeRouteLocks records which phase-two route each member is committed to,
	// keyed by character id and holding the gate's own [LOCKOUT TYPE] value
	// (4 light, 5 dark). A member is bound by the first route gate it enters and
	// cannot cross to the other side for the rest of the phase. The two shared
	// rooms declare 0 and never bind or block anyone.
	LukeRouteLocks map[uint16]byte
	// PhaseCoinsUsed counts the revive coins the whole roster has spent in the
	// active phase. It only matters for a phase that declares
	// [PHASE COIN LIMIT] (Luke phase two: 20, shared, not reset by re-entry);
	// activation starts it at zero. See raid_phase_coin.go.
	PhaseCoinsUsed int
	// ClearAnnounced is the one-shot guard of the channel-wide clear
	// announcement, keyed by the phase that cleared the raid. The announcement is
	// raised on the objective transition, so anything that keeps the lifecycle on
	// PhaseActive (a gate still occupied by a member whose client froze) re-runs
	// that transition on every further clear and would repeat the banner: live
	// 2026-09-17 07:26:44Z pushed msg 0x0272 to both members twice, 92ms apart.
	// Claiming the phase once makes the banner a property of the raid, not of the
	// number of times its completion was observed.
	ClearAnnounced map[byte]bool
	// RewardSkippedMembers is the set of frozen members this phase's settlement
	// has given up on. A member that drops before it confirms the phase movie or
	// picks its card can never satisfy either roster gate again, and the gate is
	// what stands between the members that ARE there and their cards: live
	// 2026-09-17, character 1's client crashed at 08:52:12Z, character 22
	// confirmed the phase-two movie at 08:54:16Z, and the raid then sat in
	// AwaitingMovie until the log ended -- one absent member swallowed the whole
	// flip. A dropped socket is exactly "offline and only a reconnect can bring it
	// back", which is the same fact raid_disconnect.go already records.
	//
	// The record is monotonic per phase: once a member is listed it never leaves
	// the set, so a late reconnect can neither be paid (its row is not in the
	// committed receipt) nor pull the settlement back into the waiting state (the
	// gates stay satisfied).
	RewardSkippedMembers map[uint16]bool
	// RewardRosterFrozen pins RewardSkippedMembers at the instant the settlement's
	// eligible roster is materialised, i.e. when the delivery token is minted.
	// Until then the set is recomputed from live presence, so a member that
	// reconnects inside its grace window is simply not skipped; from then on the
	// set and the receipt that was planned from it can no longer drift apart.
	RewardRosterFrozen bool
	// FiendWar is the 超时空攻坚战 persistent battle: the 30-area map, the
	// roaming enemy/monster units, the area timers, radar and battery, the
	// merchant, and the two bosses' four-mechanism cycle. Non-nil only while
	// RaidMode == runtimeRaidOwnerFiendWar.
	//
	// It coexists with Objectives rather than replacing it: the PVF rule pairs
	// evaluated here are the authority, and syncRuntimeFiendWarObjectives derives
	// the Objectives rows the client's battle-status table reads from them.
	FiendWar *raid.FiendWarBattle
	// FiendWarClearCounts is how many areas each member has cleared. Fiend War
	// declares [MINIMAL DUNGEON CLEAR COUNT] 1, and the settlement pays only the
	// members that reached it (runtimeFiendWarRewardEligible), so this counter
	// exists for reward eligibility and nothing else.
	FiendWarClearCounts map[uint16]int
	// FiendWarRetreats records the areas the battle decided must retreat: area ->
	// the dungeon attempt that was holding it. The battle writes it, the bridge
	// consumes it and clears it after each member has actually been moved back to
	// the waiting room (captureRuntimeFiendWarRetreats).
	FiendWarRetreats map[int]runtimeRaidDungeonProgress
}

var (
	errRuntimeRaidNotFound              = errors.New("runtime raid not found")
	errRuntimeRaidNotMember             = errors.New("character is not a frozen raid member")
	errRuntimeRaidNotStarted            = errors.New("runtime raid has not started")
	errRuntimeRaidPhaseMismatch         = errors.New("runtime raid phase mismatch")
	errRuntimeRaidLifecycleState        = errors.New("runtime raid lifecycle state rejects transition")
	errRuntimeRaidDungeonToken          = errors.New("runtime raid dungeon lifecycle token is invalid")
	errRuntimeRaidDungeonOccupied       = errors.New("runtime raid dungeon is occupied")
	errRuntimeRaidGroupOccupied         = errors.New("runtime raid group already occupies a dungeon")
	errRuntimeRaidDungeonCompleted      = errors.New("runtime raid dungeon is already completed")
	errRuntimeRaidDungeonUnavailable    = errors.New("runtime raid dungeon objective is not open")
	errRuntimeRaidDungeonEventOrder     = errors.New("runtime raid dungeon event is out of order")
	errRuntimeRaidRewardToken           = errors.New("runtime raid reward token is invalid")
	errRuntimeRaidRewardSelectionFrozen = errors.New("runtime raid reward selection is already frozen")
	errRuntimeRaidRewardSelectionOrder  = errors.New("runtime raid reward card must confirm mode1 before mode0")
	errRuntimeRaidRewardRosterPending   = errors.New("runtime raid reward selection is not confirmed by the frozen roster")
	errRuntimeRaidRewardNotReady        = errors.New("runtime raid reward is not ready for final cleanup")
)

func newRuntimeRaidLifecycleState(now time.Time) runtimeRaidLifecycleState {
	now = runtimeRaidNormalizeTime(now)
	return runtimeRaidLifecycleState{
		Status:                runtimeRaidLifecyclePhaseActive,
		Phase:                 runtimeRaidDungeonPhaseOne,
		Revision:              1,
		StartedAt:             now,
		PhaseStartedAt:        now,
		MovieConfirmed:        make(map[uint16]bool),
		Objectives:            make(map[int]runtimeRaidDungeonObjective),
		Dungeons:              make(map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress),
		GroupOccupancy:        make(map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding),
		RunClearCredits:       make(map[runtimeRaidDungeonRunKey]uint64),
		ClosedDungeonAttempts: make(map[runtimeRaidDungeonAttemptKey]runtimeRaidDungeonTerminal),
		RewardSelections:      make(map[uint16]runtimeRaidRewardSelection),
		RewardSkippedMembers:  make(map[uint16]bool),
		Mechanisms:            make(map[runtimeRaidMechanismKey]runtimeRaidMechanismTimer),
		BarrierRequests:       make(map[runtimeRaidBarrierRequestKey]uint32),
		ClearAnnounced:        make(map[byte]bool),
	}
}

func newRuntimeRaidLifecycleStateFromAnton(config *raid.AntonConfig, now time.Time) (runtimeRaidLifecycleState, error) {
	return newRuntimeRaidLifecycleStateFromAntonMode(config, now, false)
}

func newRuntimeRaidLifecycleStateFromAntonMode(config *raid.AntonConfig, now time.Time, single bool) (runtimeRaidLifecycleState, error) {
	return newRuntimeRaidLifecycleStateFromAntonModeAndType(config, now, single, 0)
}

func newRuntimeRaidLifecycleStateFromAntonModeAndType(config *raid.AntonConfig, now time.Time, single bool, singleRaidType byte) (runtimeRaidLifecycleState, error) {
	return newRuntimeRaidLifecycleStateFromModeAndType(config, runtimeRaidOwnerAnton, now, single, singleRaidType)
}

// runtimeRaidPhaseCountMatchesOwner states each raid owner's phase count
// exactly. Anton and Luke are two-phase raids and a document that declares
// anything else is still rejected; Fiend War declares [PHASE MAX] 1 and
// parseFiendWarBattle synthesises exactly one phase from it. Keeping the check
// per owner -- rather than relaxing it to a range -- is what guarantees an
// Anton or Luke document cannot start a raid with the wrong shape.
func runtimeRaidPhaseCountMatchesOwner(config *raid.AntonConfig, raidMode byte) bool {
	if config == nil {
		return false
	}
	if raidMode == runtimeRaidOwnerFiendWar {
		return len(config.Phases) == 1
	}
	return len(config.Phases) == 2
}

func newRuntimeRaidLifecycleStateFromModeAndType(config *raid.AntonConfig, raidMode byte, now time.Time, single bool, singleRaidType byte) (runtimeRaidLifecycleState, error) {
	if config == nil || config.StartDelaySeconds <= 0 {
		return runtimeRaidLifecycleState{}, errRuntimeRaidLifecycleState
	}
	if single {
		if config.SingleMode == nil || config.SingleMode.TimeLimitSeconds <= 0 || len(config.SingleMode.Phase.Dungeons) == 0 {
			return runtimeRaidLifecycleState{}, errRuntimeRaidLifecycleState
		}
	} else if !runtimeRaidPhaseCountMatchesOwner(config, raidMode) {
		return runtimeRaidLifecycleState{}, errRuntimeRaidLifecycleState
	}
	now = runtimeRaidNormalizeTime(now)
	state := newRuntimeRaidLifecycleState(now)
	state.Status = runtimeRaidLifecyclePreparing
	state.PhaseStartedAt = time.Time{}
	state.ReadyDeadline = now.Add(time.Duration(config.StartDelaySeconds) * time.Second)
	var err error
	if single {
		err = initializeRuntimeRaidSinglePhaseObjectivesForOwner(&state, config, raidMode, singleRaidType)
	} else {
		err = initializeRuntimeRaidPhaseObjectives(&state, config, runtimeRaidDungeonPhaseOne)
	}
	if err != nil {
		return runtimeRaidLifecycleState{}, err
	}
	return state, nil
}

func initializeRuntimeRaidSinglePhaseObjectives(state *runtimeRaidLifecycleState, config *raid.AntonConfig) error {
	return initializeRuntimeRaidSinglePhaseObjectivesWithType(state, config, 0)
}

func initializeRuntimeRaidSinglePhaseObjectivesWithType(state *runtimeRaidLifecycleState, config *raid.AntonConfig, singleRaidType byte) error {
	return initializeRuntimeRaidSinglePhaseObjectivesForOwner(state, config, runtimeRaidOwnerAnton, singleRaidType)
}

func initializeRuntimeRaidSinglePhaseObjectivesForOwner(state *runtimeRaidLifecycleState, config *raid.AntonConfig, raidMode byte, singleRaidType byte) error {
	if state == nil || config == nil || config.SingleMode == nil || len(config.SingleMode.Phase.Dungeons) == 0 {
		return errRuntimeRaidPhaseMismatch
	}
	state.Objectives = make(map[int]runtimeRaidDungeonObjective, len(config.SingleMode.Phase.Dungeons))
	state.Dungeons = make(map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress)
	state.GroupOccupancy = make(map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding)
	state.RunClearCredits = make(map[runtimeRaidDungeonRunKey]uint64)
	for _, dungeon := range config.SingleMode.Phase.Dungeons {
		mapID := int(dungeon.Index)
		state.Objectives[mapID] = runtimeRaidDungeonObjective{
			Phase:       runtimeRaidDungeonPhaseOne,
			MapID:       mapID,
			State:       dungeon.InitState,
			MaxHP:       dungeon.HP,
			RemainingHP: dungeon.HP,
			PartyMax:    dungeon.PartyMax,
		}
	}
	switch raidMode {
	case runtimeRaidOwnerAnton:
		if singleRaidType != 0 && singleRaidType != 3 && singleRaidType != 6 {
			return errRuntimeRaidPhaseMismatch
		}
	case runtimeRaidOwnerLuke:
		if singleRaidType != runtimeRaidLukeLightRoute && singleRaidType != runtimeRaidLukeDarkRoute {
			return errRuntimeRaidPhaseMismatch
		}
		firstID := 5323
		secondID := 5324
		if singleRaidType == runtimeRaidLukeDarkRoute {
			firstID = 5329
			secondID = 5330
		}
		for mapID, objective := range state.Objectives {
			objective.State = raid.AntonDungeonHide
			switch mapID {
			case firstID:
				objective.State = raid.AntonDungeonOpen
			case secondID:
				objective.State = raid.AntonDungeonClosed
			}
			state.Objectives[mapID] = objective
		}
	case runtimeRaidOwnerFiendWar:
		if singleRaidType != 0 {
			return errRuntimeRaidPhaseMismatch
		}
	default:
		return errRuntimeRaidPhaseMismatch
	}
	return nil
}

func initializeRuntimeRaidPhaseObjectives(state *runtimeRaidLifecycleState, config *raid.AntonConfig, phase byte) error {
	phaseConfig, ok := runtimeRaidAntonPhase(config, phase)
	if state == nil || !ok || len(phaseConfig.Dungeons) == 0 {
		return errRuntimeRaidPhaseMismatch
	}
	state.Objectives = make(map[int]runtimeRaidDungeonObjective, len(phaseConfig.Dungeons))
	state.Dungeons = make(map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress)
	state.GroupOccupancy = make(map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding)
	state.RunClearCredits = make(map[runtimeRaidDungeonRunKey]uint64)
	// A new phase starts with no mechanism timer; anything still queued from
	// the previous phase must find a foreign generation and stop.
	state.Mechanisms = make(map[runtimeRaidMechanismKey]runtimeRaidMechanismTimer)
	state.MechanismGeneration++
	for _, dungeon := range phaseConfig.Dungeons {
		mapID := int(dungeon.Index)
		state.Objectives[mapID] = runtimeRaidDungeonObjective{
			Phase:       phase,
			MapID:       mapID,
			State:       dungeon.InitState,
			MaxHP:       dungeon.HP,
			RemainingHP: dungeon.HP,
			PartyMax:    dungeon.PartyMax,
		}
	}
	return nil
}

func runtimeRaidNormalizeTime(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

// runtimeRaidGatePartyMaxReachedLocked 报告这个门是不是已经容下了它的 [PARTY MAX]
// 允许的小队数（只数除调用者自己座位以外的不同座位）。调用者持有 raidMu。
//
// 上限为 0 或缺失时一律放行 —— 没声明上限的门行为与改动前完全一致。
//
// 移植自上游 raid_lifecycle.go（ede0bf5）。
func runtimeRaidGatePartyMaxReachedLocked(
	state *runtimeRaidState,
	seat runtimeRaidDungeonSeat,
	mapID int,
) (int, int, bool) {
	if state == nil {
		return 0, 0, false
	}
	objective, exists := state.Lifecycle.Objectives[mapID]
	if !exists || objective.PartyMax <= 0 {
		return 0, 0, false
	}
	holders := make(map[runtimeRaidDungeonSeat]struct{})
	for otherKey, other := range state.Lifecycle.Dungeons {
		if otherKey.MapID != mapID || otherKey.Seat == seat ||
			other.Status == runtimeRaidDungeonProgressCompleted {
			continue
		}
		holders[otherKey.Seat] = struct{}{}
	}
	return len(holders), objective.PartyMax, len(holders) >= objective.PartyMax
}

func (state *runtimeRaidState) ensureRuntimeRaidLifecycleMapsLocked() {
	if state.Lifecycle.MovieConfirmed == nil {
		state.Lifecycle.MovieConfirmed = make(map[uint16]bool)
	}
	if state.Lifecycle.MovieSymbols == nil {
		state.Lifecycle.MovieSymbols = make(map[uint32]uint32)
	}
	// 卢克的持久符号表与路线锁在这里初始化（与上游一致）；LukeGeneratorReports 与
	// LukeSanctumMazeGroups 由各自的 setter 懒建。
	if state.Lifecycle.LukeSymbols == nil {
		state.Lifecycle.LukeSymbols = make(map[uint32]uint32)
	}
	if state.Lifecycle.LukeRouteLocks == nil {
		state.Lifecycle.LukeRouteLocks = make(map[uint16]byte)
	}
	if state.Lifecycle.Objectives == nil {
		state.Lifecycle.Objectives = make(map[int]runtimeRaidDungeonObjective)
	}
	if state.Lifecycle.Dungeons == nil {
		state.Lifecycle.Dungeons = make(map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress)
	}
	if state.Lifecycle.GroupOccupancy == nil {
		state.Lifecycle.GroupOccupancy = make(map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding)
	}
	if state.Lifecycle.RunClearCredits == nil {
		state.Lifecycle.RunClearCredits = make(map[runtimeRaidDungeonRunKey]uint64)
	}
	if state.Lifecycle.ClosedDungeonAttempts == nil {
		state.Lifecycle.ClosedDungeonAttempts = make(map[runtimeRaidDungeonAttemptKey]runtimeRaidDungeonTerminal)
	}
	if state.Lifecycle.RewardSelections == nil {
		state.Lifecycle.RewardSelections = make(map[uint16]runtimeRaidRewardSelection)
	}
	if state.Lifecycle.RewardSkippedMembers == nil {
		state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	}
	if state.Lifecycle.Mechanisms == nil {
		state.Lifecycle.Mechanisms = make(map[runtimeRaidMechanismKey]runtimeRaidMechanismTimer)
	}
	if state.Lifecycle.BarrierRequests == nil {
		state.Lifecycle.BarrierRequests = make(map[runtimeRaidBarrierRequestKey]uint32)
	}
	if state.Lifecycle.ClearAnnounced == nil {
		state.Lifecycle.ClearAnnounced = make(map[byte]bool)
	}
	// 超时空的两张表懒建。FiendWar 本身是 *raid.FiendWarBattle，由
	// newRuntimeRaidLifecycleStateFromModeAndType 之后的 resolve 路径按需挂上，
	// 不在这里造 —— 一个 nil 战场对安东/卢克是正常状态。
	if state.Lifecycle.FiendWarClearCounts == nil {
		state.Lifecycle.FiendWarClearCounts = make(map[uint16]int)
	}
	if state.Lifecycle.FiendWarRetreats == nil {
		state.Lifecycle.FiendWarRetreats = make(map[int]runtimeRaidDungeonProgress)
	}
}

// claimRuntimeRaidClearAnnouncement returns true exactly once per raid and phase,
// for the caller that is about to broadcast the clear banner.
func (s *Service) claimRuntimeRaidClearAnnouncement(raidKey uint32, phase byte) bool {
	if s == nil || raidKey == 0 {
		return true
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		// The raid is no longer registered: the status guard already decided, and
		// there is no record left to hold the claim.
		return true
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if state.Lifecycle.ClearAnnounced[phase] {
		return false
	}
	state.Lifecycle.ClearAnnounced[phase] = true
	state.Lifecycle.Revision++
	return true
}

// beginRuntimeRaidDungeon atomically reserves one current-phase map for the
// frozen member's attack-party group. Ordinary maps intentionally return a
// zero binding so their established dungeon lifecycle stays untouched.
func (s *Service) beginRuntimeRaidDungeon(characterID uint16, mapID int, lifecycleToken uint64) (runtimeRaidDungeonBinding, error) {
	phase, raidMap := runtimeRaidDungeonPhaseForMap(mapID)
	if !raidMap {
		return runtimeRaidDungeonBinding{}, nil
	}
	if lifecycleToken == 0 {
		return runtimeRaidDungeonBinding{}, errRuntimeRaidDungeonToken
	}

	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil {
		return runtimeRaidDungeonBinding{}, errRuntimeRaidNotFound
	}
	if !state.Started || !state.EntryCostPaid || len(state.FrozenMembers) == 0 {
		return runtimeRaidDungeonBinding{}, errRuntimeRaidNotStarted
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || state.Lifecycle.Phase != phase {
		return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: active=%d requested=%d status=%d", errRuntimeRaidPhaseMismatch, state.Lifecycle.Phase, phase, state.Lifecycle.Status)
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		return runtimeRaidDungeonBinding{}, errRuntimeRaidNotMember
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if len(state.Lifecycle.Objectives) != 0 {
		objective, exists := state.Lifecycle.Objectives[mapID]
		if !exists || objective.Phase != phase || objective.RemainingHP <= 0 || objective.State != raid.AntonDungeonOpen {
			return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: phase=%d map=%d", errRuntimeRaidDungeonUnavailable, phase, mapID)
		}
		if !runtimeRaidAntonEntryAllowedByBarrier(*state, mapID) {
			return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: map=%d barrier=%d", errRuntimeRaidDungeonUnavailable, mapID, state.Lifecycle.Barrier)
		}
	}
	attemptKey := runtimeRaidDungeonAttemptKey{CharacterID: characterID, LifecycleKey: lifecycleToken}
	if terminal, exists := state.Lifecycle.ClosedDungeonAttempts[attemptKey]; exists {
		return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: token already closed as %s phase=%d map=%d", errRuntimeRaidDungeonToken, terminal.Kind, terminal.Phase, terminal.MapID)
	}
	binding := runtimeRaidDungeonBinding{
		RaidKey:        state.RaidKey,
		Phase:          phase,
		MapID:          mapID,
		LifecycleToken: lifecycleToken,
	}
	seat := runtimeRaidDungeonSeatFor(*state, member, mapID)
	runKey := runtimeRaidDungeonRunKey{MapID: mapID, Seat: seat}
	// A warp out of the energy-interception gate (218) into a hatchery the phase
	// has opened is an in-run transfer, not a second gate: the member leaves its own
	// 218 run and enters the hatchery, so the squad keeps exactly one gate and its
	// occupancy moves with the member. Without this the transfer was refused with
	// "runtime raid group already occupies a dungeon: group=1 map=218" -- live
	// 2026-09-17 07:25:12Z, both op16 attempts of the 222 warp -- which is what left
	// the hatcheries unreachable and the member's session without a run at all.
	if runtimeRaidHatcheryWarpApplies(state, seat, characterID, phase, mapID) {
		s.releaseRuntimeRaidRunAtSeatLocked(state, seat)
	}
	if occupied, exists := state.Lifecycle.GroupOccupancy[seat]; exists {
		if runtimeRaidDungeonBindingMatches(occupied, binding) {
			progress, progressExists := state.Lifecycle.Dungeons[runKey]
			if progressExists && runtimeRaidDungeonProgressMatches(progress, characterID, binding) {
				return occupied, nil
			}
		}
		if occupied.Phase == phase && occupied.MapID == mapID {
			// The seat is already inside this gate. One squad fights one run, so a
			// SQUADMATE arriving second adopts that binding instead of opening a
			// rival one -- refusing here broke every party entry into a raid gate,
			// because the roster's members share a squad and each of their runtimes
			// carries its own lifecycle token, so the second freeze could never
			// match the first binding.
			//
			// The adopter keeps the owner's token, which is what keeps the run
			// single-counted: notifyRuntimeRaidDungeonLifecycleLocked only publishes
			// events whose token equals the session's own, so the run is reported
			// once by the opener -- and the adopter's own clear still spends one
			// point of the gate (applyRuntimeRaidJoinedSquadClear).
			//
			// The same character re-beginning with a new token is NOT an adoption:
			// that is a racing duplicate of its own request and stays refused.
			if progress, exists := state.Lifecycle.Dungeons[runKey]; exists && progress.CharacterID != characterID {
				return occupied, nil
			}
		}
		return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: group=%d char=%d map=%d", errRuntimeRaidGroupOccupied, seat.Group, seat.CharacterID, occupied.MapID)
	}
	// [PARTY MAX] 是「一个门同时能容几支小队」，以前一处都没执行过 —— 这个字段被
	// PVF 解析、存到 objective 上，然后只被读出来看过。上面那段占位检查是**反过来的**
	// 规则（一支小队一次只打一个门），不是这一条（一个门只容一支小队），所以第二支
	// 小队会直接走进别人正在打的同一个门。
	//
	// 卢克每个门都声明 1（同一圣所同时只容一支队伍）；安徒恩的门声明 5，而安徒恩
	// 最多 4 支小队，所以对安徒恩这条上限永远触发不到 —— 行为不变。
	if state.Lifecycle.FiendWar == nil {
		// 超时空的区域是共享战场：一个区域同时站着几个人是正常的，这一格不按小队计数。
		// 安东/卢克逐字保留原来的 [PARTY MAX] 判定。
		if holders, partyMax, full := runtimeRaidGatePartyMaxReachedLocked(state, seat, mapID); full {
			return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: map=%d squads=%d party_max=%d",
				errRuntimeRaidDungeonOccupied, mapID, holders, partyMax)
		}
	}
	// A cleared gate stays cleared for every squad until the phase resets it.
	// Without an objective table the completed record survives its own run (see
	// the completed branch of applyRuntimeRaidDungeonLifecycleEvent), so the same
	// map must still be refused here even though the run key belongs to another
	// squad.
	for otherKey, other := range state.Lifecycle.Dungeons {
		if otherKey.MapID == mapID && other.Status == runtimeRaidDungeonProgressCompleted {
			return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: map=%d", errRuntimeRaidDungeonCompleted, mapID)
		}
	}
	if progress, exists := state.Lifecycle.Dungeons[runKey]; exists {
		if runtimeRaidDungeonProgressMatches(progress, characterID, binding) {
			state.Lifecycle.GroupOccupancy[seat] = binding
			return binding, nil
		}
		// Only this seat is blocked by its own unclaimed run. A run owned by
		// another seat is a different record and must not stop this one: that
		// cross-squad refusal is exactly what kept a second squad out of a gate.
		return runtimeRaidDungeonBinding{}, fmt.Errorf("%w: map=%d group=%d char=%d", errRuntimeRaidDungeonOccupied, mapID, progress.Seat.Group, progress.Seat.CharacterID)
	}

	now := time.Now().UTC()
	state.Lifecycle.Dungeons[runKey] = runtimeRaidDungeonProgress{
		Phase:          phase,
		MapID:          mapID,
		Seat:           seat,
		CharacterID:    characterID,
		LifecycleToken: lifecycleToken,
		Status:         runtimeRaidDungeonProgressSelected,
		SelectedAt:     now,
	}
	// A new run of this gate starts with its clear unspent. The run key (gate +
	// seat) is reused by the squad's next run, so the previous run's credit -- which
	// has to outlive that run's record, because a squadmate may report its own
	// completion after the opener closed the run -- is dropped here rather than
	// when the run ends. Adopting squadmates never reach this line: they keep the
	// opener's binding, which is what keeps one run one clear.
	delete(state.Lifecycle.RunClearCredits, runKey)
	state.Lifecycle.GroupOccupancy[seat] = binding
	state.Lifecycle.Revision++
	return binding, nil
}

// releaseRuntimeRaidRunsOwnedByLocked drops every dungeon run a departing
// character owns.
//
// A run lives inside that character's session: once the session is gone the run
// can neither be completed nor abandoned, so its gate would stay occupied and the
// whole squad would be locked out of it until the process restarts. Live evidence
// 2026-09-16: the first entry (09:33:19 loaded map 210) ended in a client crash
// with no abandoned event, and every later select of that gate was answered with
// "runtime raid group already occupies a dungeon: group=1 map=210". Caller holds
// raidMu.
func (s *Service) releaseRuntimeRaidRunsOwnedByLocked(state *runtimeRaidState, characterID uint16) {
	if state == nil || characterID == 0 || len(state.Lifecycle.Dungeons) == 0 {
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	for runKey, progress := range state.Lifecycle.Dungeons {
		if progress.CharacterID != characterID || progress.Status == runtimeRaidDungeonProgressCompleted {
			continue
		}
		delete(state.Lifecycle.Dungeons, runKey)
		// Only this run's own squad is released: another squad may be holding the
		// same gate under a different run key and must keep its occupancy.
		delete(state.Lifecycle.GroupOccupancy, progress.Seat)
		state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
			CharacterID:  characterID,
			LifecycleKey: progress.LifecycleToken,
		}] = runtimeRaidDungeonTerminal{
			Phase: progress.Phase,
			MapID: runKey.MapID,
			Kind:  runtimeRaidDungeonAbandoned,
		}
		state.Lifecycle.Revision++
	}
}

// releaseRuntimeRaidRunAtSeatLocked drops the run a squad holds under one seat.
//
// This is the in-run transfer half of releaseRuntimeRaidRunsOwnedByLocked: a warp
// that moves the squad's single gate from 218 into a hatchery has to retire the run
// the squad held, and that run may have been opened by a squadmate rather than by
// the member doing the warping, so it cannot be found by owner. Caller holds raidMu.
func (s *Service) releaseRuntimeRaidRunAtSeatLocked(state *runtimeRaidState, seat runtimeRaidDungeonSeat) {
	if state == nil || len(state.Lifecycle.Dungeons) == 0 {
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	for runKey, progress := range state.Lifecycle.Dungeons {
		if runKey.Seat != seat || progress.Status == runtimeRaidDungeonProgressCompleted {
			continue
		}
		delete(state.Lifecycle.Dungeons, runKey)
		delete(state.Lifecycle.GroupOccupancy, progress.Seat)
		state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
			CharacterID:  progress.CharacterID,
			LifecycleKey: progress.LifecycleToken,
		}] = runtimeRaidDungeonTerminal{
			Phase: progress.Phase,
			MapID: runKey.MapID,
			Kind:  runtimeRaidDungeonAbandoned,
		}
		state.Lifecycle.Revision++
	}
}

// releaseRuntimeRaidStaleGateOccupancy drops every open gate whose owning session
// no longer holds the run it was opened for.
//
// A run lives inside its owner's session: only that session can report its loaded,
// completed or abandoned event. A client that froze inside a gate -- a live socket
// that stopped answering -- never reports any of them, so its record and its
// squad's GroupOccupancy stay forever and every later completion of the phase runs
// into openRuntimeRaidPhaseMovie's "len(GroupOccupancy) == 0" gate. Live
// 2026-09-17: character 1 loaded 218 at 07:24:57Z, its client froze at 07:25:12Z
// while warping to hatchery 222 (dungeon_runtime_commit_failed), and its session
// was then left with no runtime at all (game-dungeon-move-blocked
// reason=dungeon_runtime_missing). Character 22 cleared the heart (220) five times
// between 07:26:23Z and 07:26:44Z and the raid never ended, because that one
// orphaned occupancy survived all five.
//
// This never touches a live squad: a record is released only when its owner has no
// bound session, no committed runtime, a runtime that owns another character, a
// runtime bound to a different raid/map/token, or a run that is no longer active.
// It returns the number of released records.
func (s *Service) releaseRuntimeRaidStaleGateOccupancy(raidKey uint32) int {
	if s == nil || raidKey == 0 {
		return 0
	}
	type runtimeRaidGateRecord struct {
		key      runtimeRaidDungeonRunKey
		progress runtimeRaidDungeonProgress
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || len(state.Lifecycle.Dungeons) == 0 {
		s.raidMu.Unlock()
		return 0
	}
	open := make([]runtimeRaidGateRecord, 0, len(state.Lifecycle.Dungeons))
	for key, progress := range state.Lifecycle.Dungeons {
		if progress.Status == runtimeRaidDungeonProgressCompleted {
			continue
		}
		open = append(open, runtimeRaidGateRecord{key: key, progress: progress})
	}
	s.raidMu.Unlock()
	if len(open) == 0 {
		return 0
	}
	stale := make([]runtimeRaidGateRecord, 0, len(open))
	for _, record := range open {
		if s.runtimeRaidDungeonRunLive(raidKey, record.key, record.progress) {
			continue
		}
		stale = append(stale, record)
	}
	if len(stale) == 0 {
		return 0
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state = s.raids[raidKey]
	if state == nil {
		return 0
	}
	released := 0
	for _, record := range stale {
		current, exists := state.Lifecycle.Dungeons[record.key]
		if !exists ||
			current.LifecycleToken != record.progress.LifecycleToken ||
			current.Status == runtimeRaidDungeonProgressCompleted {
			continue
		}
		delete(state.Lifecycle.Dungeons, record.key)
		// Only this run's own seat is released, and only while its occupancy still
		// points at this exact run: a squad that has meanwhile entered another gate
		// keeps that one.
		if occupied, ok := state.Lifecycle.GroupOccupancy[record.key.Seat]; ok &&
			occupied.MapID == record.key.MapID &&
			occupied.LifecycleToken == current.LifecycleToken {
			delete(state.Lifecycle.GroupOccupancy, record.key.Seat)
		}
		state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
			CharacterID:  current.CharacterID,
			LifecycleKey: current.LifecycleToken,
		}] = runtimeRaidDungeonTerminal{
			Phase: current.Phase,
			MapID: record.key.MapID,
			Kind:  runtimeRaidDungeonAbandoned,
		}
		state.Lifecycle.Revision++
		released++
		s.logPacketEvent("game-raid-stale-gate-occupancy-released",
			"raid_key", raidKey,
			"char_id", current.CharacterID,
			"raid_phase", current.Phase,
			"raid_map_id", record.key.MapID,
			"group", record.key.Seat.Group,
			"seat_char_id", record.key.Seat.CharacterID,
			"lifecycle_token", current.LifecycleToken,
			"reason", "owning_session_no_longer_holds_the_run")
	}
	return released
}

// runtimeRaidDungeonRunLive reports whether the record's owner still holds exactly
// this run. It reads the bound session and that session's own dungeon runtime, so
// it must be called without raidMu held.
func (s *Service) runtimeRaidDungeonRunLive(raidKey uint32, key runtimeRaidDungeonRunKey, progress runtimeRaidDungeonProgress) bool {
	if s == nil || progress.CharacterID == 0 {
		return false
	}
	identity, online := s.onlineGameSessionCharacterSnapshot(progress.CharacterID)
	if !online || identity.session == nil {
		return false
	}
	session := identity.session
	// TryLock, never Lock: this runs from publishRuntimeRaidObjectiveProgress, which
	// the dungeon-completion path reaches already holding the completing member's own
	// session.dungeon.mu. Go mutexes are not reentrant, so a plain Lock on that
	// member's own session parked its packet goroutine forever the moment it had any
	// open gate at all (the completion of a gate whose record is still open is
	// exactly when this is asked). A session whose lock is busy is reported as live,
	// which is the conservative answer: it is never released on a guess.
	if !session.dungeon.mu.TryLock() {
		return true
	}
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil ||
		!dungeonRuntimeOwnsCharacter(runtime, progress.CharacterID) {
		return false
	}
	if runtime.raidDungeon.RaidKey != raidKey ||
		runtime.raidDungeon.Phase != progress.Phase ||
		runtime.raidDungeon.MapID != key.MapID ||
		runtime.raidDungeon.LifecycleToken != progress.LifecycleToken {
		return false
	}
	return runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunActive
}

// applyRuntimeRaidDungeonLifecycleEvent accepts only the binding returned by
// beginRuntimeRaidDungeon. It never performs network I/O and never acquires a
// dungeon/session lock; callers already own that lifecycle boundary.
func (s *Service) applyRuntimeRaidDungeonLifecycleEvent(event runtimeRaidDungeonLifecycleEvent) error {
	if event.RaidKey == 0 || event.CharacterID == 0 || event.LifecycleToken == 0 {
		return errRuntimeRaidDungeonToken
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[event.RaidKey]
	if state == nil {
		return errRuntimeRaidNotFound
	}
	if !state.Started || !state.EntryCostPaid {
		return errRuntimeRaidNotStarted
	}
	if _, ok := runtimeRaidFrozenMember(*state, event.CharacterID); !ok {
		return errRuntimeRaidNotMember
	}
	// The run this event belongs to is the one its own seat OPENED. That seat is
	// frozen into the run record and must never be recomputed from the live roster
	// here: a squad relabel during the run (a party change while the member stands
	// inside the gate, see unassignRuntimeRaidSquadOnPartyLeave) would move the key,
	// the lookup below would miss, and the member's completion would be dropped as
	// errRuntimeRaidDungeonToken while the old seat's GroupOccupancy stayed behind as
	// an orphan that blocks every later entry of that squad. Live 2026-09-19, raid
	// 1507338: 14:24:07 and 14:24:16 unassigned two members who were inside 218.
	runKey, progress, exists := runtimeRaidRunKeyForAttemptLocked(
		state, event.CharacterID, event.MapID, event.LifecycleToken)
	state.ensureRuntimeRaidLifecycleMapsLocked()
	attemptKey := runtimeRaidDungeonAttemptKey{CharacterID: event.CharacterID, LifecycleKey: event.LifecycleToken}
	if terminal, closed := state.Lifecycle.ClosedDungeonAttempts[attemptKey]; closed {
		if terminal.Phase == event.Phase && terminal.MapID == event.MapID && terminal.Kind == event.Kind {
			return nil
		}
		return fmt.Errorf("%w: closed event=%s received=%s", errRuntimeRaidDungeonEventOrder, terminal.Kind, event.Kind)
	}
	if !runtimeRaidDungeonLifecycleReportAccepted(state, runKey, progress, exists, event) {
		return fmt.Errorf("%w: raid=%d phase=%d map=%d token=%d", errRuntimeRaidDungeonToken, event.RaidKey, event.Phase, event.MapID, event.LifecycleToken)
	}
	now := time.Now().UTC()
	switch event.Kind {
	case runtimeRaidDungeonSelected:
		if progress.Status < runtimeRaidDungeonProgressSelected {
			return errRuntimeRaidDungeonEventOrder
		}
		return nil
	case runtimeRaidDungeonLoaded:
		if progress.Status >= runtimeRaidDungeonProgressLoaded {
			return nil
		}
		if progress.Status != runtimeRaidDungeonProgressSelected {
			return errRuntimeRaidDungeonEventOrder
		}
		progress.Status = runtimeRaidDungeonProgressLoaded
		progress.LoadedAt = now
		state.Lifecycle.Dungeons[runKey] = progress
		// 超时空：进图要告诉战场"这个小队占住了这个区域"，它会启动该区域的战斗
		// 计时器与 BOSS 机制。未编队成员 (Seat.Group == 0) 在这里被 battle.Enter
		// 拒绝 —— 这是刻意的：超时空的战场模型只有"小队占区域"，没有个人占区域。
		//
		// 区域由**副本**决定，不由报告者的实时格子决定。
		//
		// 原来这里写 `battle.GroupAreas[seat.Group] = battle.MemberArea(报告者, seat.Group)`，
		// 在"报告者就是按下进图的那个人"时是对的（他刚被 town_move.go 挪进目标区域，
		// MemberAreas 就是目标区域）—— 单人进图一直是这条形状，所以从来没暴露。
		//
		// 但**报告者不一定是按下进图的人**：超时空一个区域共用一条 run，
		// 而 prepareRuntimePartyDungeonEntry 先备队友，所以经常是**队友**把这条 run 开出来。
		// 真机 2026-10-01 23:22:38：game-raid-dungeon-lifecycle char_id=22 event=selected
		// dungeon_lifecycle_token=1 —— 队友 char 22 是开局人，驱动者 char 1 是 adopt 的
		// （binding token=1，本地 token=2，所以他一条都不发布）。而队友按设计**留在原格**：
		// MemberAreas 钉在他按下进图时站的格子，于是那一行把 GroupAreas[小队] 覆盖成**原格**，
		// 紧接着 Enter 就失败 ——
		//
		//	15:22:40.369 game-raid-dungeon-loaded-publish-deferred char_id=22 party_id=4
		//	             loading_cycle=1 raid_key=12058625
		//	             reason=party_loading_release_continues
		//	             error=fiendwar dungeon admission rejected
		//
		// 战场里这一格因此永远没被标记成"被占"，玩家能把图清完，但通关时 Clear 报
		//
		//	15:23:05.986 game-raid-dungeon-completion-publish-failed char_id=22
		//	             dungeon_id=4956 op115_sent=true
		//	             error=fiendwar dungeon not occupied
		//
		// 结算推进不下去，于是"清完自动回城"整段判定永远不成立 —— 现象就是
		// 「刷完没反应」。单人时一直正常，因为那时 MemberArea 恰好等于目标区域。
		if state.Lifecycle.FiendWar != nil {
			battle := state.Lifecycle.FiendWar.Clone()
			if area, ok := battle.AreaForDungeon(event.MapID); ok {
				battle.GroupAreas[progress.Seat.Group] = area.Index
			} else {
				// 副本不在任何区域名下（不该发生）：退回原来的写法，至少不改变行为。
				battle.GroupAreas[progress.Seat.Group] = battle.MemberArea(event.CharacterID, progress.Seat.Group)
			}
			if err := battle.Enter(progress.Seat.Group, event.MapID, now); err != nil {
				return err
			}
			state.Lifecycle.FiendWar = battle
			// 战况表的目标状态必须**在这一次边界广播之前**就位。
			//
			// publishRuntimeRaidDungeonParticipation 的超时空分支会把整张战况表
			// 立刻重发一遍（raid_dungeon_lifecycle.go 的 broadcastRuntimeFiendWarBoard），
			// 而 585 的门状态与 0x0254 的副本倒计时都是从 Objectives 上取的。
			// 以前这里不同步，只有 1 Hz 心跳里的 syncRuntimeFiendWarObjectives 会跑，
			// 于是**进图那一帧**广播出去的是：
			//   585 -> 这一格还是 hide(2)
			//   0x0254 -> 状态不是 0/1/3，按客户端规则"不画"，于是整条不发
			// 真机 2026-10-02 16:28–16:36（raid 12058625）实测：每一次进图，
			// 4 个成员的 0x0254 都要等**下一个心跳**才发，比进图晚 1.4–3.1 秒 ——
			// 操作员截图里的"进副本后显示的还是攻坚倒计时 00:58:28"就是这段窗口。
			// 同步一次只把心跳本来就会做的那次计算提前到这个边界，行为等价。
			syncRuntimeFiendWarObjectives(state)
		}
		if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke {
			// Luke's [ON ENTER DUNGEON] behaviors fire once the scene is real,
			// which is this boundary and not `selected`. [ON ENTER MAP] is a room
			// edge and runs from the room move (observeRuntimeRaidLukeEnterMapLocked).
			runtimeRaidLukeEnterDungeonLocked(state, event.MapID)
			if route, bound := bindRuntimeRaidLukeRouteLocked(state, event.CharacterID, event.MapID); bound {
				s.logPacketEvent("game-raid-luke-route-bound",
					"raid_key", event.RaidKey, "char_id", event.CharacterID,
					"raid_map_id", event.MapID, "route", route)
			}
		}
		state.Lifecycle.Revision++
		return nil
	case runtimeRaidDungeonCompleted:
		if progress.Status == runtimeRaidDungeonProgressCompleted {
			return nil
		}
		if progress.Status != runtimeRaidDungeonProgressLoaded {
			return errRuntimeRaidDungeonEventOrder
		}
		progress.Status = runtimeRaidDungeonProgressCompleted
		progress.CompletedAt = now
		// 超时空：清区域先让战场结算（BOSS 机制必须已完成，否则 Clear 返回错误，
		// 这一笔通关就不会被承认）。随后记清数 —— 超时空的
		// [MINIMAL DUNGEON CLEAR COUNT] 是 1，结算只给达到它的成员发奖。
		//
		// 记账口径与本服务端其它地方一致：一次副本 = 一个小队的一次通关，所以整队
		// 一起记，而不是只记报告的那个人。未编队 (Seat.Group == 0) 才记到个人头上。
		// One run is one party clear, and **an adopting squadmate may already have
		// booked it** (applyRuntimeRaidJoinedSquadClear credits the run and spends the
		// gate's point). Computed before the Fiend War block below because that block
		// needs it, and reused further down by the generic bookkeeping.
		runCredited := runtimeRaidRunClearCreditedLocked(state, runKey)
		if state.Lifecycle.FiendWar != nil {
			battle := state.Lifecycle.FiendWar.Clone()
			// BOSS 房用 ClearClientBossRoom：本客户端不发 op1511 的 kind=1 机制上报，
			// 机制状态机走不到 9，用 Clear 会把这一笔通关整个丢掉（真机现象：
			// 「超时空通关后没反应」，见 raid.FiendWarBattle.ClearClientBossRoom 的注释）。
			if err := battle.ClearClientBossRoom(event.MapID, now); err != nil {
				return err
			}
			// 这一格的目标必须**还开着** —— 除非这条 run 的通关已经被队友记过账了。
			//
			// 一个区域共用一条 run：adopt 进来的队友先报 completed 时走的是
			// applyRuntimeRaidJoinedSquadClear，它会把这一格的那一点 HP 花掉，
			// 并调用 runtimeRaidCreditRunClearLocked 把这条 run 记成"已记账"
			// （"一条 run 一次通关"）。于是随后**开局人**再报 completed 时
			// objective.RemainingHP 已经是 0。
			//
			// 原来这里无条件要求 RemainingHP > 0 && State == AntonDungeonOpen，
			// 开局人这一笔就被判成 errRuntimeRaidDungeonUnavailable —— 真机
			// 2026-10-01 23:38（raid 12058625，分片 035）：
			//
			//	15:38:57.846 game-raid-joined-clear-booked char_id=1
			//	             booked=true remaining_hp_before=1 remaining_hp_after=0
			//	15:38:57.860 game-raid-dungeon-completion-publish-failed char_id=22
			//	             dungeon_id=4954 error=runtime raid dungeon objective is not open
			//	15:38:58.751 game-fiendwar-clear-auto-return char_id=1 ...
			//	（char 22 是开局人，此后再也没有回城）
			//
			// 注意**只有开局人那一笔**会把这条 run 标成 completed（队友那一笔只记
			// 本地、ClosedDungeonAttempts 也挂在他自己名下），所以这一拒等于这条 run
			// 永远关不掉、开局人永远不自动回城 —— 现象就是「队长回城了、队员没回去」。
			//
			// 已经被记过账的 run 只关 run、不再动目标；清数（FiendWarClearCounts）
			// 也**不再加一次**：一次副本 = 一个小队的一次通关。
			if !runCredited {
				objective, ok := state.Lifecycle.Objectives[event.MapID]
				if !ok || objective.Phase != event.Phase || objective.RemainingHP <= 0 || objective.State != raid.AntonDungeonOpen {
					return errRuntimeRaidDungeonUnavailable
				}
				state.Lifecycle.FiendWar = battle
				if state.Lifecycle.FiendWarClearCounts == nil {
					state.Lifecycle.FiendWarClearCounts = make(map[uint16]int)
				}
				if squad := runtimeRaidMembersInSquad(*state, progress.Seat.Group); len(squad) > 0 {
					for _, squadMember := range squad {
						state.Lifecycle.FiendWarClearCounts[squadMember]++
					}
				} else {
					state.Lifecycle.FiendWarClearCounts[progress.CharacterID]++
				}
			} else {
				// 战场状态照样要落地（ClearClientBossRoom 已经把这一格放开了），
				// 只是不再动目标、不再加清数。
				state.Lifecycle.FiendWar = battle
			}
		}
		delete(state.Lifecycle.GroupOccupancy, progress.Seat)
		state.Lifecycle.ClosedDungeonAttempts[attemptKey] = runtimeRaidDungeonTerminal{
			Phase: event.Phase,
			MapID: event.MapID,
			Kind:  runtimeRaidDungeonCompleted,
		}
		clearCountBefore := 0
		if objective, ok := state.Lifecycle.Objectives[event.MapID]; ok {
			clearCountBefore = objective.ClearCount
		}
		if len(state.Lifecycle.Objectives) == 0 {
			state.Lifecycle.Dungeons[runKey] = progress
		} else {
			delete(state.Lifecycle.Dungeons, runKey)
			// One run is one party clear. An adopting squadmate may already have
			// reported its own instance of this very run, in which case the gate's
			// point is spent and the owner's completion only closes the run.
			if !runCredited {
				if err := advanceRuntimeRaidObjectiveLocked(state, event.Phase, event.MapID); err != nil {
					return err
				}
			}
		}
		// Only a clear that actually advanced the gate registers for its squad: a
		// completion reported after the gate was closed or reset earns nothing, and
		// neither does a second member's completion of an already credited run.
		objective, ok := state.Lifecycle.Objectives[event.MapID]
		if ok && objective.ClearCount > clearCountBefore {
			runtimeRaidCreditRunClearLocked(state, runKey, progress.LifecycleToken)
			runtimeRaidNotePhaseClearLocked(state, event.CharacterID, event.Phase)
		} else if !ok {
			runtimeRaidNotePhaseClearLocked(state, event.CharacterID, event.Phase)
		} else if runCredited {
			// 这一条 run 的那一点 HP 已经被队友花掉了（"一条 run 一次通关"），
			// 所以**不再动目标、不再花第二点**；但**本人这一笔通关要记在自己头上**。
			//
			// 原来这里只写一行日志、什么都不记。两人**同一小队**时看不出问题 ——
			// 队友记账那一下 runtimeRaidNotePhaseClearLocked 已经把整队都记上了。
			// 但超时空的进图单位是**区域名单（规格 D：与小队编制无关）**，
			// 两个人常常不在同一小队，于是记账那一方只记了自己那一队：
			//
			//	真机 2026-10-02 00:05:22（raid 12058625，分片 20261002-002）
			//	  game-raid-joined-clear-booked          char_id=1  booked=true
			//	  game-raid-run-clear-already-credited   char_id=22 group=2
			//	      reason=one_run_is_one_party_clear
			//	  game-raid-phase-clear-registration     char_id=22
			//	      member_clear_counts=1:1   ← 只有 char 1 记到了
			//
			// 于是**开局人（队员）的"地下城通关"一直是 0** —— 操作员 2026-10-02：
			// 「通关以后 队友的通关次数居然没计数」。
			//
			// 作用域刻意只加**超时空**：它的 [MINIMAL DUNGEON CLEAR COUNT] 是 1，
			// runtimeRaidNotePhaseClearLocked 自己会按 limit 封顶，所以即使同一小队
			// 被记两次也只停在 1；安东/卢克的门可能要求多次通关，重复记会提前把
			// 那一列填满，所以它们一个字不改。
			if state.Lifecycle.FiendWar != nil {
				// 两本账一起记：战况表那一列（PhaseClearCounts）+ 发奖门槛
				// （FiendWarClearCounts）。只记前者会出现"显示 1/1 却拿不到奖励"。
				runtimeRaidNoteFiendWarMemberClearLocked(state, event.CharacterID, event.Phase)
			}
			s.logPacketEvent("game-raid-run-clear-already-credited",
				"raid_key", event.RaidKey,
				"raid_phase", event.Phase,
				"raid_map_id", event.MapID,
				"char_id", event.CharacterID,
				"group", progress.Seat.Group,
				"seat_char_id", progress.Seat.CharacterID,
				"run_lifecycle_token", progress.LifecycleToken,
				"reason", "one_run_is_one_party_clear")
		}
		state.Lifecycle.Revision++
		return nil
	case runtimeRaidDungeonAbandoned:
		if progress.Status == runtimeRaidDungeonProgressCompleted {
			return errRuntimeRaidDungeonEventOrder
		}
		delete(state.Lifecycle.Dungeons, runKey)
		delete(state.Lifecycle.GroupOccupancy, progress.Seat)
		state.Lifecycle.ClosedDungeonAttempts[attemptKey] = runtimeRaidDungeonTerminal{
			Phase: event.Phase,
			MapID: event.MapID,
			Kind:  runtimeRaidDungeonAbandoned,
		}
		// 超时空：放弃区域要让战场把小队从区域里放出来、并按 PVF 规则决定要不要
		// 整队撤退 (双 BOSS 任一边放弃时两边一起退)。captureRuntimeFiendWarRetreats
		// 把战场写下的撤退标记转成桥上可消费的副本尝试；syncRuntimeFiendWarObjectives
		// 立刻把战况表的行更新成战场的判断。
		if state.Lifecycle.FiendWar != nil {
			battle := state.Lifecycle.FiendWar.Clone()
			if err := battle.Abandon(event.MapID, now); err != nil {
				return err
			}
			captureRuntimeFiendWarRetreats(state, battle)
			state.Lifecycle.FiendWar = battle
			syncRuntimeFiendWarObjectives(state)
		}
		if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke {
			// [ON GIVEUP DUNGEON]: a sanctum's object counter resets, a throne
			// closes the energy save room again.
			runtimeRaidLukeGiveUpDungeonLocked(state, event.MapID)
		}
		state.Lifecycle.Revision++
		return nil
	default:
		return errRuntimeRaidDungeonEventOrder
	}
}

// applyRuntimeRaidJoinedSquadClear books one adopting squadmate's own completion of
// the gate its squad already holds.
//
// A squad fights one raid gate at a time, so the second member to walk in keeps
// the binding the first one opened (see the adoption branch of
// beginRuntimeRaidDungeon) and its lifecycle events are not published as a run --
// that is what keeps the squad's gate *bookkeeping* single: one run record, one
// occupancy, one 战况表 row.
//
// The gate's HP is spent once per RUN, not once per member. This server gives every
// member its own dungeon runtime and its own copy of the room -- the two sessions
// move into the same gate with different `seed`s (live 2026-09-17T12:45:46Z, raid
// 1507329: character 1 seed=2820159178, character 22 seed=3502334619) -- so every
// member kills its own boss and reports its own clear, but those reports describe
// ONE party clear of the gate and the gate must lose exactly one point for them.
//
// Live 2026-09-17T12:45-12:46Z is what the run ledger preserves: characters 1 and 22
// cleared the heart together twice (12:45:47 and 12:45:54) and character 1 cleared
// it three more times alone. That is five party runs, and the gate booked five --
// two together-runs plus three solo runs. Counting each member's own instance
// instead (what this path used to do) would have charged the pair four points for
// those two together-runs and taken a four-clear gate down in two runs, or in ONE
// run for a four-member squad (operator report 2026-09-19: 两人组队通关两把就进
// 下一阶段；四人组队一把就过). One run, one point, however many members walked it.
//
// localToken is the joined session's own lifecycle token, never the owner's, so a
// replayed op39/op117 cannot book the same run twice.
// runtimeRaidNoteFiendWarMemberClearLocked 记"这个人清过这一格"，**两本账一起记**。
//
// 为什么要两本：
//
//   - `PhaseClearCounts`（经 runtimeRaidPhaseClearCount 进 mode=3 名册）是**战况表上
//     「地下城通关」那一列**：客户端刷新名册时从成员记录里读，操作员就是照这一列
//     说「后两个不给算通关」的；
//   - `FiendWarClearCounts` 是**发奖门槛**（runtimeFiendWarRewardEligible ≥
//     [MINIMAL DUNGEON CLEAR COUNT]，超时空是 1）。
//
// 只记前者会出现"战况表 1/1 但拿不到奖励"，只记后者则相反。两本都记才自洽。
//
// 幂等与上限：调用点由 ClosedDungeonAttempts 保证"每人每本地 token 只报一次"；
// 超时空的这一格是 1 点 HP、门槛也是 1，多记一次不改变任何判定（安东/卢克不走这里）。
//
// 真机 2026-10-02 11:32:16（raid 12058646，分片 009，四人一起进 4955）就是这一列：
//
//	joined-clear-booked char_id=22 booked=true    hp 1 -> 0
//	run-clear-already-credited char_id=21
//	phase-clear-registration char_id=21 member_clear_counts=21:1,22:1
//	joined-clear-booked char_id=1  booked=false   hp 0 -> 0   ← 一条不记
//	joined-clear-booked char_id=20 booked=false   hp 0 -> 0   ← 一条不记
//
// 操作员截图：编制 1、2 是 1/1，3、4 还是 0/1。调用者持 raidMu。
func runtimeRaidNoteFiendWarMemberClearLocked(state *runtimeRaidState, characterID uint16, phase byte) {
	if state == nil || characterID == 0 {
		return
	}
	runtimeRaidNotePhaseClearLocked(state, characterID, phase)
	if state.Lifecycle.FiendWarClearCounts == nil {
		state.Lifecycle.FiendWarClearCounts = make(map[uint16]int)
	}
	state.Lifecycle.FiendWarClearCounts[characterID]++
}

// runtimeRaidJoinedSquadClearIsAlreadySpentLocked 报告"这一格对本阶段已经不可打了"：
// 目标属于本阶段、而且那一点 HP 已经被花掉（谁花的都算）。
//
// 这是 applyRuntimeRaidJoinedSquadClear 判定"本人这一笔要不要记"的依据。
// **不能只看 RunClearCredits**：那张表是按 (map, seat) 键的，而座位是从**还在账本里的
// 那条 run** 反查出来的 —— 生命周期那条路处理完成时会把 run 从账本里删掉，于是后报的人
// 反查不到 owner token，runKey 退化成"他自己小队的座位"，runCredited 就成了 false，
// 本人这一笔就被整脚丢掉（真机 11:32:16 的 char 1 / char 20 正是如此）。
func runtimeRaidJoinedSquadClearIsAlreadySpentLocked(state *runtimeRaidState, mapID int, phase byte) bool {
	if state == nil {
		return false
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.Phase != phase {
		return false
	}
	return objective.RemainingHP <= 0
}

func (s *Service) applyRuntimeRaidJoinedSquadClear(
	session *gameSession,
	binding runtimeRaidDungeonBinding,
	localToken uint64,
) {
	if s == nil || session == nil || binding.RaidKey == 0 || binding.MapID == 0 || localToken == 0 {
		return
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		return
	}
	s.raidMu.Lock()
	state := s.raids[binding.RaidKey]
	if state == nil || !state.Started || !state.EntryCostPaid ||
		state.Lifecycle.Phase != binding.Phase ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		s.raidMu.Unlock()
		return
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		s.raidMu.Unlock()
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	// This member's own run of the gate shares its squad's seat and its opener's
	// token, which is the same key the owner's own completion uses: whichever of
	// them reports first spends the gate's point for that run. The seat is read back
	// from the run record itself, so a squad re-label during the run (an ordinary
	// party change, a leader re-group) cannot move the key out from under the credit
	// and leave the run's occupancy orphaned.
	runKey := runtimeRaidDungeonRunKey{
		MapID: binding.MapID,
		Seat: runtimeRaidRunSeatForTokenLocked(
			state, binding.MapID, binding.LifecycleToken, runtimeRaidSeatFor(member)),
	}
	attemptKey := runtimeRaidDungeonAttemptKey{CharacterID: characterID, LifecycleKey: localToken}
	if _, closed := state.Lifecycle.ClosedDungeonAttempts[attemptKey]; closed {
		s.raidMu.Unlock()
		return
	}
	// The clear is booked whether or not the gate had a point left to spend: a
	// gate that closed or reset while this member was inside earns nothing, which
	// is the same rule the owning path applies in advanceRuntimeRaidObjectiveLocked.
	// Recording it keeps a retried report a no-op either way.
	state.Lifecycle.ClosedDungeonAttempts[attemptKey] = runtimeRaidDungeonTerminal{
		Phase: binding.Phase,
		MapID: binding.MapID,
		Kind:  runtimeRaidDungeonCompleted,
	}
	runCredited := runtimeRaidRunClearCreditedLocked(state, runKey)
	booked := false
	remainingBefore := 0
	if objective, ok := state.Lifecycle.Objectives[binding.MapID]; ok &&
		objective.Phase == binding.Phase && objective.RemainingHP > 0 &&
		objective.State == raid.AntonDungeonOpen && !runCredited {
		remainingBefore = objective.RemainingHP
		if err := advanceRuntimeRaidObjectiveLocked(state, binding.Phase, binding.MapID); err != nil {
			delete(state.Lifecycle.ClosedDungeonAttempts, attemptKey)
		} else {
			booked = true
			runtimeRaidCreditRunClearLocked(state, runKey, binding.LifecycleToken)
			// 两本账一起记（战况表那一列 + 发奖门槛），见函数注释。
			runtimeRaidNoteFiendWarMemberClearLocked(state, characterID, binding.Phase)
		}
	}
	// 这一条 run 的那一点 HP 已经被别人花掉了（"一条 run 一次通关"）：**不再动目标**，
	// 但**本人这一笔通关要记在自己头上**。
	//
	// 这是 applyRuntimeRaidDungeonLifecycleEvent 里同一处修复的**另一条路径** ——
	// 那次只补了生命周期的 completed 分支，而队友（adopting squadmate）的通关报告
	// 走的是这一条（applyRuntimeRaidJoinedSquadClear），于是同一个洞在四个人进图时
	// 一次又一次地露出来：
	//
	//	真机 2026-10-02 10:20:33（raid 12058625，分片 006，四人一起进 4955）
	//	  joined-clear-booked char_id=1  booked=true   hp 1 -> 0
	//	  joined-clear-booked char_id=21 booked=false  hp 0 -> 0   ← 一条不记
	//	  joined-clear-booked char_id=20 booked=false  hp 0 -> 0   ← 一条不记
	//	  run-clear-already-credited char_id=22        （走生命周期那条）
	//	  phase-clear-registration char_id=22 member_clear_counts=1:1,22:1
	//
	//	真机 2026-10-02 11:32:16（raid 12058646，分片 009，**修过一版之后**的四人局）
	//	  joined-clear-booked char_id=22 booked=true   hp 1 -> 0
	//	  run-clear-already-credited char_id=21
	//	  phase-clear-registration char_id=21 member_clear_counts=21:1,22:1
	//	  joined-clear-booked char_id=1  booked=false  hp 0 -> 0   ← 还是不记
	//	  joined-clear-booked char_id=20 booked=false  hp 0 -> 0   ← 还是不记
	//	  操作员截图：编制 1、2 是 1/1，3、4 是 0/1。
	//
	// 上一版只判 `runCredited`，那一版漏在**账本已经被清掉**的情形上：生命周期那条路
	// 处理完成时会把 run 从 Lifecycle.Dungeons 里删掉，后报的人于是反查不到 owner
	// token，runKey 退化成"他自己小队的座位"，runCredited 就成了 false —— 判据失效。
	// 现在改成看**这一格的目标**：属于本阶段、HP 已经没了（谁花的都算）。
	//
	// 为什么两人时看不出来、四人时露馅：runtimeRaidNotePhaseClearLocked 记的是
	// **记账人所在小队**。超时空的进图单位是区域名单（规格 D：与编制无关），而且
	// 操作员 2026-10-02 定案「超时空每个小队编制仅可放 1 人」—— 于是"记整队"在
	// 超时空就等于"只记他自己"，同格另外三个人一个都不会被记到。
	//
	// 作用域刻意只加**超时空**：它的 [MINIMAL DUNGEON CLEAR COUNT] 是 1，
	// runtimeRaidNotePhaseClearLocked 自己按 limit 封顶，重复记也停在 1；
	// 安东/卢克的门可能要求多次通关（而且 limit 可能为 0 即不封顶），重复记会把
	// 那一列提前填满，所以它们一个字不改。
	//
	// 幂等：同一个 (角色, 本地 token) 的报告在函数开头就写进 ClosedDungeonAttempts，
	// 重放会直接返回，所以这里不会给同一个人记第二次。
	countedWithoutBooking := false
	if !booked && state.Lifecycle.FiendWar != nil &&
		(runCredited || runtimeRaidJoinedSquadClearIsAlreadySpentLocked(state, binding.MapID, binding.Phase)) {
		runtimeRaidNoteFiendWarMemberClearLocked(state, characterID, binding.Phase)
		countedWithoutBooking = true
	}
	state.Lifecycle.Revision++
	raidKey := state.RaidKey
	phase := state.Lifecycle.Phase
	remainingAfter := 0
	if next, ok := state.Lifecycle.Objectives[binding.MapID]; ok {
		remainingAfter = next.RemainingHP
	}
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	s.logGameEvent(session, "game-raid-joined-clear-booked",
		"raid_key", raidKey,
		"raid_phase", phase,
		"raid_map_id", binding.MapID,
		"char_id", characterID,
		"dungeon_lifecycle_token", localToken,
		"owner_lifecycle_token", binding.LifecycleToken,
		"booked", booked,
		"counted", booked || countedWithoutBooking,
		"remaining_hp_before", remainingBefore,
		"remaining_hp_after", remainingAfter)
	if !booked {
		if !countedWithoutBooking {
			return
		}
		// 记上了就必须**让客户端看见**：战况表那一列读的是 mode=3 名册里的成员记录。
		// 这一笔没有动目标，所以只补名册刷新，机制/进度那两步不归它。
		if err := s.broadcastRuntimeRaidPhaseClearRegistration(snapshot); err != nil {
			s.logPacketEvent("game-raid-joined-clear-registration-failed",
				"raid_key", raidKey, "raid_map_id", binding.MapID, "error", err)
		}
		s.logPacketEvent("game-raid-joined-clear-counted-without-booking",
			"raid_key", raidKey,
			"raid_phase", phase,
			"raid_map_id", binding.MapID,
			"char_id", characterID,
			"reason", "the_gate_point_was_already_spent_but_this_members_clear_still_counts")
		return
	}
	// The owning path's follow-ups, in the same order: mechanisms first, then the
	// progress snapshot every member's client redraws from, then the roster refresh
	// that carries the clear registration.
	if err := s.afterRuntimeRaidObjectiveAdvance(raidKey, phase, binding.MapID); err != nil {
		s.logGameEvent(session, "game-raid-anton-mechanism-arm-failed",
			"raid_key", raidKey, "raid_map_id", binding.MapID, "error", err)
	}
	if err := s.publishRuntimeRaidObjectiveProgress(raidKey, phase); err != nil {
		s.logPacketEvent("game-raid-joined-clear-progress-failed",
			"raid_key", raidKey, "raid_map_id", binding.MapID, "error", err)
	}
	if err := s.broadcastRuntimeRaidPhaseClearRegistration(snapshot); err != nil {
		s.logPacketEvent("game-raid-joined-clear-registration-failed",
			"raid_key", raidKey, "raid_map_id", binding.MapID, "error", err)
	}
}

func runtimeRaidDungeonBindingMatches(left runtimeRaidDungeonBinding, right runtimeRaidDungeonBinding) bool {
	return left.RaidKey == right.RaidKey &&
		left.Phase == right.Phase &&
		left.MapID == right.MapID &&
		left.LifecycleToken == right.LifecycleToken
}

// runtimeRaidRunKeyForAttemptLocked resolves the run record one lifecycle event
// belongs to, from the event's own identity (character + map + the run's opener
// token) instead of from the live roster.
//
// The run key carries the seat the run was OPENED under, and that seat is frozen
// into the record. Recomputing it from the current roster (runtimeRaidSeatFor)
// would move the key whenever a member's squad label changed during the run -- a
// party change or a leader re-group -- which silently dropped that member's
// completion and left the old seat's GroupOccupancy orphaned. Fall back to the
// roster seat only while no record exists yet (the "selected" event that creates
// it). Callers hold raidMu.
func runtimeRaidRunKeyForAttemptLocked(
	state *runtimeRaidState,
	characterID uint16,
	mapID int,
	token uint64,
) (runtimeRaidDungeonRunKey, runtimeRaidDungeonProgress, bool) {
	if state == nil || characterID == 0 || mapID == 0 || token == 0 {
		return runtimeRaidDungeonRunKey{}, runtimeRaidDungeonProgress{}, false
	}
	for runKey, progress := range state.Lifecycle.Dungeons {
		if runKey.MapID != mapID {
			continue
		}
		if progress.CharacterID == characterID && progress.LifecycleToken == token {
			return runKey, progress, true
		}
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		return runtimeRaidDungeonRunKey{}, runtimeRaidDungeonProgress{}, false
	}
	runKey := runtimeRaidDungeonRunKey{MapID: mapID, Seat: runtimeRaidSeatFor(member)}
	progress, exists := state.Lifecycle.Dungeons[runKey]
	return runKey, progress, exists
}

// runtimeRaidRunSeatForTokenLocked resolves the seat a run was opened under, by
// its opener token: a joined squadmate's report must be credited to the run's own
// seat, not to the seat its roster label happens to name now. Callers hold raidMu.
func runtimeRaidRunSeatForTokenLocked(
	state *runtimeRaidState,
	mapID int,
	token uint64,
	fallback runtimeRaidDungeonSeat,
) runtimeRaidDungeonSeat {
	if state == nil || mapID == 0 || token == 0 {
		return fallback
	}
	for runKey, progress := range state.Lifecycle.Dungeons {
		if runKey.MapID == mapID && progress.LifecycleToken == token {
			return runKey.Seat
		}
	}
	return fallback
}

func runtimeRaidDungeonProgressMatches(progress runtimeRaidDungeonProgress, characterID uint16, binding runtimeRaidDungeonBinding) bool {
	return progress.CharacterID == characterID &&
		progress.Phase == binding.Phase &&
		progress.MapID == binding.MapID &&
		progress.LifecycleToken == binding.LifecycleToken
}

// runtimeFiendWarClearReturnBindingAccepts 报告"本人手上这份 raidDungeon 绑定还能不能用"，
// 也就是"清完自动回城"里除了"这一局的结算已经做完"之外的那半条。
//
// 操作员 2026-10-01 定案（原话）：
//
//	「不管是谁 谁是驱动者 谁收尾 这都不重要 通关直接回城就是了
//	  当然了 这些逻辑只适用于超时空」
//
// 所以这里**只**回答"这份绑定描述的是不是我此刻真的在打的那张图"，
// 一切与"谁"有关的判断都不许出现在这条路上：
//
//   - 不看 binding token 等不等于本人本地 token —— 那只有**开局的人**成立；
//     超时空一个区域共用一条 run，而 prepareRuntimePartyDungeonEntry **先备队友**，
//     所以常常是队友开局、驱动者 adopt，驱动者手上那份 binding 带的是开局人的 token。
//   - 不看攻坚账本里还留不留着这条 run —— 通关处理本身就会
//     `delete(state.Lifecycle.Dungeons, runKey)`，等该回城时账本已经空了。
//     2026-10-01 真机把这两条各演示了一遍：23:38 开局人收尾**失败**（账本还在）
//     ⇒ 驱动者回了城（"队长自动回城了 队员没回去"）；23:54 开局人收尾**成功**
//     （账本被删）⇒ 同一个驱动者又回不去（"队员回了 队长没回去"）。
//
// 剩下这条是"我与我自己"的关系，不是"谁"：一个会话不可能拿着 A 图的绑定站在 B 图里
// （runtimeRaidCurrentRaidDungeonRun 用的是同一条不变式）。
//
// 作用域：本函数只被超时空那条清完回城的判定调用（tryRuntimeFiendWarEncounterPlanned，
// 入口已经用 isCurrentFiendWarRaidChannel + RaidMode==runtimeRaidOwnerFiendWar 挡过），
// 安东/卢克一个字都不改。
func runtimeFiendWarClearReturnBindingAccepts(runtime *runtimeDungeonState) bool {
	if runtime == nil || runtime.raidDungeon.RaidKey == 0 || runtime.lifecycleToken == 0 {
		return false
	}
	return runtime.raidDungeon.MapID == int(runtime.Dungeon.ID)
}

// runtimeRaidDungeonLifecycleReportAccepted 是 notifyRuntimeRaidDungeonLifecycleLocked
// 的准入判定：这一条 lifecycle 事件能不能落到 run 上。
//
// 常规那半条是逐字保留的「本人 + 同 token」。
//
// 超时空那半条是 2026-10-01 真机事故的修复：一个区域共用一条 run，**先开的人持有
// 它的 token**。后到的那位手里是自己那份 token（他这一趟从没开过 run），但这一格
// 确实已经有本小队的 run 在打 —— 按设计"一场只报一次、由 owner 报"
// （见 beginRuntimeRaidDungeonForRuntime 里 binding.LifecycleToken !=
// runtime.lifecycleToken 那条 return nil），这里必须是**静默接受**。
//
// 真机 21:27:11 把它当成了非法 token：队友先被 prepare 开出了这一格的 run
// （token=1，owner=char 20），驱动者随后带着自己那份 token=2 来报，于是
//
//	game-dungeon-select-blocked reason=dungeon_runtime_commit_failed
//	  error=runtime raid dungeon lifecycle token is invalid:
//	        raid=12058625 phase=1 map=4960 token=2
//
// 整场进图被拆（"A party that cannot be prepared is REFUSED, never split"），
// 之后心跳永远重试 game-fiendwar-encounter-peer-not-ready —— 谁都进不去。
//
// 命中条件刻意收得很紧：
//   - exists 为真：说明**本成员座位下**这一格的 run 确实在
//     （runtimeRaidRunKeyForAttemptLocked 的兜底用 runKey{MapID, runtimeRaidSeatFor}）；
//   - runKey.MapID / progress.MapID / progress.Phase 与事件一致；
//   - owner 非零且不是本人 —— 也就是"这是我队友开的、我们一起打的那一场"。
//
// 本人用新 token 重开自己的局（racing duplicate）owner 就是自己，仍然被拒。
//
// 安东/卢克逐字不变：它们的座位恒等于 runtimeRaidSeatFor，后到者在
// beginRuntimeRaidDungeon 就 adopt 了 owner 的 token，走不到这里；而且这半条被
// state.Lifecycle.FiendWar != nil 挡住。
func runtimeRaidDungeonLifecycleReportAccepted(
	state *runtimeRaidState,
	runKey runtimeRaidDungeonRunKey,
	progress runtimeRaidDungeonProgress,
	exists bool,
	event runtimeRaidDungeonLifecycleEvent,
) bool {
	if !exists {
		return false
	}
	if runtimeRaidDungeonProgressMatches(progress, event.CharacterID, runtimeRaidDungeonBinding{
		RaidKey:        event.RaidKey,
		Phase:          event.Phase,
		MapID:          event.MapID,
		LifecycleToken: event.LifecycleToken,
	}) {
		return true
	}
	return state != nil && state.Lifecycle.FiendWar != nil &&
		runKey.MapID == event.MapID &&
		progress.MapID == event.MapID &&
		progress.Phase == event.Phase &&
		progress.CharacterID != 0 &&
		progress.CharacterID != event.CharacterID
}

// runtimeRaidRunClearCreditedLocked reports whether this gate run already spent its
// one clear. One run is one party clear: the opener's completion and every adopting
// squadmate's own completion all name the same run, so only the first of them may
// take a point off the gate. Callers hold raidMu.
func runtimeRaidRunClearCreditedLocked(state *runtimeRaidState, runKey runtimeRaidDungeonRunKey) bool {
	if state == nil || state.Lifecycle.RunClearCredits == nil {
		return false
	}
	_, credited := state.Lifecycle.RunClearCredits[runKey]
	return credited
}

// runtimeRaidCreditRunClearLocked records that this run has spent its one clear.
// Callers hold raidMu and have just advanced the objective.
func runtimeRaidCreditRunClearLocked(state *runtimeRaidState, runKey runtimeRaidDungeonRunKey, token uint64) {
	if state == nil {
		return
	}
	if state.Lifecycle.RunClearCredits == nil {
		state.Lifecycle.RunClearCredits = make(map[runtimeRaidDungeonRunKey]uint64)
	}
	state.Lifecycle.RunClearCredits[runKey] = token
}

// runtimeRaidNotePhaseClearLocked registers one cleared gate for every member of
// the clearing squad, and only for the clearing member itself when that member
// belongs to no squad. The current client reads the result from the member record
// it already holds (see runtimeRaidMemberRecord), so the counter has to live on
// the raid state and be capped at the phase's own requirement: 86JP caps the
// same value at the gate's maximum clear count, and the 地下城通关 column must
// never read higher than the requirement it is measured against.
//
// An unassigned member stands alone: 86JP's BuildSituationGroups gives PartyIndex 0
// its own singleton group (RaidManager.cs:339-344), so its clear must not fill the
// 地下城通关 column of squad 1, which is what folding it onto the normalized group
// used to do.
//
// Callers hold raidMu and have just advanced the objective.
func runtimeRaidNotePhaseClearLocked(state *runtimeRaidState, characterID uint16, phase byte) {
	if state == nil || characterID == 0 {
		return
	}
	member, ok := runtimeRaidFrozenMember(*state, characterID)
	if !ok {
		// A session that reports a clear but is not (or no longer) a frozen
		// member registered nothing: the run itself still settles normally.
		return
	}
	group := runtimeRaidMemberSquad(member.GroupIndex)
	limit := uint32(runtimeRaidPhaseMinimalClearCount(*state, phase))
	if state.Lifecycle.PhaseClearCounts == nil {
		state.Lifecycle.PhaseClearCounts = make(map[uint16]uint32)
	}
	register := func(candidate uint16) {
		next := state.Lifecycle.PhaseClearCounts[candidate] + 1
		if limit > 0 && next > limit {
			next = limit
		}
		state.Lifecycle.PhaseClearCounts[candidate] = next
	}
	if group == 0 {
		register(characterID)
		return
	}
	for _, squadmate := range state.FrozenMembers {
		if runtimeRaidMemberSquad(squadmate.GroupIndex) != group {
			continue
		}
		register(squadmate.CharID)
	}
}

// runtimeRaidPhaseMinimalClearCount is the active phase's
// [minimal dungeon clear count] from the native PVF, or 0 when the config does
// not carry one (the callers then leave the registration uncapped).
//
// It is the *display* cap for the 地下城通关 column and keeps its historical
// shape on purpose: it indexes config.Phases for every raid, so single-mode
// Luke (whose team phase one declares 0) stays uncapped. The *reward*
// requirement is a different question with a different answer for that case --
// use runtimeRaidPhaseMinimalClearRequirement for it.
func runtimeRaidPhaseMinimalClearCount(state runtimeRaidState, phase byte) int {
	if phase == 0 {
		return 0
	}
	config := state.AntonConfig
	if state.RaidMode == runtimeRaidOwnerLuke && state.LukeConfig != nil {
		config = state.LukeConfig.Raid
	}
	if config == nil {
		return 0
	}
	index := int(phase) - 1
	if index < 0 || index >= len(config.Phases) {
		return 0
	}
	return config.Phases[index].MinimalDungeonClearCount
}

// runtimeRaidPhaseClearCount projects one member's phase clear registration onto
// the single byte the current client's member record carries.
func runtimeRaidPhaseClearCount(state runtimeRaidState, characterID uint16) byte {
	value := state.Lifecycle.PhaseClearCounts[characterID]
	if value > 0xff {
		value = 0xff
	}
	return byte(value)
}

func advanceRuntimeRaidObjectiveLocked(state *runtimeRaidState, phase byte, mapID int) error {
	if state == nil || state.Lifecycle.Phase != phase {
		return errRuntimeRaidPhaseMismatch
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.Phase != phase {
		return errRuntimeRaidDungeonUnavailable
	}
	if objective.RemainingHP <= 0 || objective.State != raid.AntonDungeonOpen {
		// The gate closed while this run was still inside: a fourth black-fog
		// clear hides 210 for the groups that had not finished, and a mechanism
		// reset reinitializes every gate. Such a clear earns nothing, but the run
		// itself must still settle normally, so this is not an error.
		return nil
	}
	objective.RemainingHP--
	objective.ClearCount++
	if objective.RemainingHP == 0 {
		// A finished gate reads as cleared (wire 3), not as merely hidden: the
		// client keeps drawing it while the phase runs, which is how the legs and
		// the hatcheries stay visible as "done" until the phase ends.
		objective.State = raid.AntonDungeonCleared
	}
	state.Lifecycle.Objectives[mapID] = objective
	if state.TailFlag == 1 {
		if phase != runtimeRaidDungeonPhaseOne {
			return errRuntimeRaidPhaseMismatch
		}
		advanceRuntimeRaidSingleObjectiveLocked(state, mapID, objective.RemainingHP == 0)
		if runtimeRaidPhaseObjectivesComplete(*state, phase) {
			for id, next := range state.Lifecycle.Objectives {
				if next.Phase == phase {
					next.State = raid.AntonDungeonHide
					state.Lifecycle.Objectives[id] = next
				}
			}
		}
		return nil
	}

	// 队伍卢克：它的副本图是自己那套。一阶段的门图在这里走；二阶段的图由
	// afterRuntimeRaidObjectiveAdvance 的已清关钩子驱动，因为那边要上弦机制计时器，
	// 而这个函数不能调度（上游 0c79ba6 / bb52082）。
	// 安徒恩状态求值为 false，下面那一段与改动前逐字相同。
	if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke {
		if phase == runtimeRaidDungeonPhaseOne {
			applyRuntimeRaidLukePhaseOneClearLocked(state, mapID)
		}
		return nil
	}

	switch phase {
	case runtimeRaidDungeonPhaseOne:
		if mapID == antonBlackFogSourceMapID && objective.RemainingHP == 0 {
			// The black-fog source is spent: the two quakes and the two legs open
			// now. The regenerated fog (211) and the naval cannon (216) stay
			// hidden until their own mechanism timers reopen them.
			objective.State = raid.AntonDungeonHide
			state.Lifecycle.Objectives[mapID] = objective
			// The regenerated fog carries the respawn countdown the client draws.
			// Its gate timer already exists (0x0254 timerType 2, map 211, 300s, armed
			// by applyRuntimeRaidAntonPhaseOneClearLocked), but the client only draws
			// a type-2 countdown for a gate whose state is 1 or 3 and skips state 2
			// entirely: 0x00F142C9 reads the state, sends state 0 to the type-1 key
			// and state 1/3 to the type-2 key, and every other value falls through to
			// the do-not-draw path. The PVF leaves 211 at hide, so nothing was ever
			// rendered and the reported "黑雾之源重生倒计时没�? followed. 86JP's
			// AntonFirstPhaseSmokeClearedStates writes 211 -> 3 here for the same
			// reason.
			if fog, exists := state.Lifecycle.Objectives[antonBlackFogMapID]; exists && fog.State == raid.AntonDungeonHide {
				fog.State = raid.AntonDungeonCleared
				state.Lifecycle.Objectives[antonBlackFogMapID] = fog
			}
			for id, next := range state.Lifecycle.Objectives {
				if next.Phase == phase && next.RemainingHP > 0 && antonPhaseOneOpensWithBlackFogSource(id) {
					next.State = raid.AntonDungeonOpen
					state.Lifecycle.Objectives[id] = next
				}
			}
		}
	case runtimeRaidDungeonPhaseTwo:
		if mapID == antonBlackVolcanoMapID && objective.RemainingHP == 0 {
			// The volcano falls: energy interception, the volcano itself and every
			// hatchery close (state 2, not the cleared marker -- the 86JP reference
			// overrides its own "cleared" write the same way in
			// ClearBlackVolcanoAsync), and only the heart remains open.
			for _, id := range []int{antonEnergyInterceptionMapID, antonBlackVolcanoMapID, 221, 222, 223, 224} {
				next, exists := state.Lifecycle.Objectives[id]
				if exists {
					next.State = raid.AntonDungeonHide
					state.Lifecycle.Objectives[id] = next
				}
			}
			if heart, exists := state.Lifecycle.Objectives[antonHeartMapID]; exists && heart.RemainingHP > 0 {
				heart.State = raid.AntonDungeonOpen
				state.Lifecycle.Objectives[antonHeartMapID] = heart
			}
		}
	default:
		return errRuntimeRaidPhaseMismatch
	}

	if runtimeRaidPhaseObjectivesComplete(*state, phase) {
		for id, next := range state.Lifecycle.Objectives {
			if next.Phase == phase {
				next.State = raid.AntonDungeonHide
				state.Lifecycle.Objectives[id] = next
			}
		}
	}
	return nil
}

func advanceRuntimeRaidSingleObjectiveLocked(state *runtimeRaidState, mapID int, completed bool) {
	if state == nil || !completed {
		return
	}
	if state.RaidMode == runtimeRaidOwnerLuke {
		route := runtimeRaidLukeSingleRoute(state.SingleRaidType)
		completedIndex := -1
		for index, routeMapID := range route {
			if routeMapID == mapID {
				completedIndex = index
				break
			}
		}
		if completedIndex < 0 {
			return
		}
		for index, routeMapID := range route {
			objective, exists := state.Lifecycle.Objectives[routeMapID]
			if !exists || objective.Phase != runtimeRaidDungeonPhaseOne {
				continue
			}
			switch {
			case index <= completedIndex:
				objective.State = raid.AntonDungeonHide
			case index == completedIndex+1:
				objective.State = raid.AntonDungeonOpen
			case index == completedIndex+2:
				objective.State = raid.AntonDungeonClosed
			default:
				objective.State = raid.AntonDungeonHide
			}
			state.Lifecycle.Objectives[routeMapID] = objective
		}
		return
	}
	nextIDs := runtimeRaidSingleNextMapIDs(state.RaidMode, state.SingleRaidType, mapID)
	for _, nextID := range nextIDs {
		next, exists := state.Lifecycle.Objectives[nextID]
		if exists && next.Phase == runtimeRaidDungeonPhaseOne && next.RemainingHP > 0 {
			next.State = raid.AntonDungeonOpen
			state.Lifecycle.Objectives[nextID] = next
		}
	}
}

func runtimeRaidLukeSingleRoute(singleRaidType byte) []int {
	switch singleRaidType {
	case runtimeRaidLukeLightRoute:
		return []int{5323, 5324, 5325, 5326, 5327, 5328}
	case runtimeRaidLukeDarkRoute:
		return []int{5329, 5330, 5331, 5332, 5333, 5334}
	default:
		return nil
	}
}

func runtimeRaidSingleNextMapIDs(raidMode, singleRaidType byte, mapID int) []int {
	if raidMode == runtimeRaidOwnerLuke {
		switch mapID {
		case 5323:
			return []int{5324}
		case 5324:
			return []int{5325}
		case 5325:
			return []int{5326}
		case 5326:
			return []int{5327}
		case 5327:
			return []int{5328}
		case 5329:
			return []int{5330}
		case 5330:
			return []int{5331}
		case 5331:
			return []int{5332}
		case 5332:
			return []int{5333}
		case 5333:
			return []int{5334}
		default:
			return nil
		}
	}
	switch mapID {
	case 5314:
		switch singleRaidType {
		case 3:
			return []int{5315}
		case 6:
			return []int{5316}
		default:
			return []int{5317}
		}
	case 5315:
		return []int{5318}
	case 5316:
		return []int{5321}
	case 5317:
		return []int{5322}
	case 5318, 5321, 5322:
		return []int{5319}
	case 5319:
		return []int{5320}
	default:
		return nil
	}
}

func runtimeRaidPhaseObjectivesComplete(state runtimeRaidState, phase byte) bool {
	// 超时空的"阶段完成"不是数副本 HP，而是战场自己判的：PVF 的 [CLEAR PHASE] 规则
	// 一旦求值到就把 battle.Complete 立起来。它只有一阶段，所以 phase 必须是 1。
	// 这条不接的话整团永远进不了结算 —— 战场会把 Complete 置位，但没人问它。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return phase == runtimeRaidDungeonPhaseOne && state.Lifecycle.FiendWar != nil && state.Lifecycle.FiendWar.Complete
	}
	remaining := func(mapID int) int {
		objective, ok := state.Lifecycle.Objectives[mapID]
		if !ok || objective.Phase != phase {
			return -1
		}
		return objective.RemainingHP
	}
	switch phase {
	case runtimeRaidDungeonPhaseOne:
		if state.TailFlag == 1 {
			if state.RaidMode == runtimeRaidOwnerLuke {
				if state.SingleRaidType == runtimeRaidLukeLightRoute {
					return remaining(5328) == 0
				}
				if state.SingleRaidType == runtimeRaidLukeDarkRoute {
					return remaining(5334) == 0
				}
				return false
			}
			return remaining(5320) == 0
		}
		// 卢克一阶段：三个圣所全部 clear 才算完（etc/raid/luke/luke.etc [1PHASE SUCCESS]）。
		// 安徒恩（owner != Anton 时才会进这里，即 owner == Luke）以外的路径逐字不变。
		if runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerLuke {
			return runtimeRaidLukePhaseObjectivesComplete(state, phase)
		}
		return remaining(213) == 0 && remaining(215) == 0
	case runtimeRaidDungeonPhaseTwo:
		if runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerLuke {
			return runtimeRaidLukePhaseObjectivesComplete(state, phase)
		}
		return remaining(220) == 0
	default:
		return false
	}
}

// runtimeRaidNormalizedGroup folds an unassigned member (group 0) onto the default
// squad. It is the rule for one thing only: the ordinary-party projection, which has
// to name a party that exists and puts an unassigned party into squad 1
// (runtimeRaidSquadForOrdinaryParty).
//
// Everywhere the group is a KEY or is PRESENTED, use what the member actually is:
// runtimeRaidSeatFor for the dungeon run key and its occupancy (a squad shares one
// seat; an unassigned member gets its own), and runtimeRaidMemberSquad for the 584
// squad row, the phase-clear registration and the reward reveal split. Folding an
// unassigned member onto squad 1 put it in squad 1's 战况表 cell, in squad 1's clear
// registration, on squad 1's reward board, and -- through the run key -- let it hold
// squad 1's gate against the squad that owns it (live report 2026-09-17 21:5x: an
// unassigned member entered a gate and the 战况表 drew it in squad 1).
func runtimeRaidNormalizedGroup(group byte) byte {
	if group == 0 {
		return runtimeRaidDefaultGroup
	}
	return group
}

// runtimeRaidMemberSquad reports the attack group a member actually belongs to, or 0
// when it belongs to none.
//
// 0 is what the client itself reads: the roster record carries the squad byte
// straight from the wire (sub_17B6EF0 stores it at +0x2D with no arithmetic) and
// every squad lookup compares it against 1..N, so an unassigned member matches no
// squad. The reference server draws the same line -- RaidManager.Create assigns no
// PartyIndex and BuildSituationGroups gives PartyIndex 0 its own singleton group
// (ServerS4A21-master, RaidManager.cs:339-344, RaidAggregate.cs:65
// `IsSolo => PartyIndex == 0`).
func runtimeRaidMemberSquad(group byte) byte {
	if group < 1 || group > runtimeRaidMaxGroups {
		return 0
	}
	return group
}

// runtimeRaidMembersInSquad lists the roster members that actually belong to one
// attack group, in ascending character order. An unassigned member is in no group,
// so it never appears in another squad's list.
func runtimeRaidMembersInSquad(state runtimeRaidState, group byte) []uint16 {
	if group == 0 {
		return nil
	}
	members := make([]uint16, 0, runtimeRaidGroupSize)
	for _, candidate := range state.FrozenMembers {
		if runtimeRaidMemberSquad(candidate.GroupIndex) == group {
			members = append(members, candidate.CharID)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	return members
}

// runtimeRaidMembersInSquadLiveRows lists the members whose CURRENT (live) squad
// label is this squad.
//
// The 584 participation row and the 0x027A battle-status row are classified by the
// CLIENT from each listed member's own 0x024F roster byte -- the client reads that
// byte straight and renders 0 as 单人 (see runtimeRaidMemberSquad) -- and the
// 0x024F roster is built from the live rows (runtimeRaidMemberRefresh). Listing a
// member from the frozen roster while its live label is 0 therefore produces a row
// the client cannot classify at all.
//
// Live 2026-09-29 00:52:45 (raid 1507329, 8 members, own build):
//
//	game-raid-squad-unassigned-on-session-leave char_id=2 previous_group=1
//	    group=0 frozen_row=kept_for_phase_credit source=game_session_unbound_or_disconnected
//	game-raid-squad-sync-skipped-members-inside-dungeon members=[1 9 35]
//
// char 2 (its client had been reset off the wire 10 minutes earlier) lost its LIVE
// label to 0 while the frozen row kept squad 1, so every following 0x024F carried
// squad byte 0 for it while the 584/0x027A rows still named it inside squad 1 --
// the squad's own row then drew as 单人 with an unresolved portrait, and stayed
// that way because the members were inside a gate and no party gesture could
// re-label them. The operator's report is exactly that: "1队进图了 2队在外面看
// 看不到是哪队进图 就显示个单 队长头像也是黑的".
//
// The frozen roster keeps its own job (phase clear credit and the reward board);
// this helper is for the two rows the client classifies by roster byte.
func runtimeRaidMembersInSquadLiveRows(state runtimeRaidState, group byte) []uint16 {
	if group == 0 {
		return nil
	}
	members := make([]uint16, 0, runtimeRaidGroupSize)
	for _, candidate := range state.Members {
		if runtimeRaidMemberSquad(candidate.GroupIndex) == group {
			members = append(members, candidate.CharID)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	return members
}

func runtimeRaidFrozenMember(state runtimeRaidState, characterID uint16) (runtimeRaidMemberState, bool) {
	for _, member := range state.FrozenMembers {
		if member.CharID == characterID {
			return member, true
		}
	}
	return runtimeRaidMemberState{}, false
}

// openRuntimeRaidPhaseMovie is called only after the current-version raid
// objective owner has proved that the active phase is complete. Map IDs alone
// do not imply phase completion.
func (s *Service) openRuntimeRaidPhaseMovie(raidKey uint32, phase byte, now time.Time) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if state.Lifecycle.Phase != phase {
		return runtimeRaidState{}, errRuntimeRaidPhaseMismatch
	}
	if state.Lifecycle.Status == runtimeRaidLifecycleAwaitingMovie {
		return cloneRuntimeRaidState(*state), nil
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || len(state.Lifecycle.GroupOccupancy) != 0 {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	if len(state.Lifecycle.Objectives) != 0 && !runtimeRaidPhaseObjectivesComplete(*state, phase) {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
	state.Lifecycle.MovieOpenedAt = now
	clear(state.Lifecycle.MovieConfirmed)
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}

// confirmRuntimeRaidPhaseMovie is kept as the narrow lifecycle helper used by
// older callers. Every phase now remains on that phase's reward cards after
// the movie; phase one advances only after its durable reward is published.
func (s *Service) confirmRuntimeRaidPhaseMovie(characterID uint16, phase byte, now time.Time) (runtimeRaidState, bool, error) {
	return s.confirmRuntimeRaidSettlementMovie(characterID, phase, now)
}

func (s *Service) confirmRuntimeRaidSettlementMovie(characterID uint16, phase byte, now time.Time) (runtimeRaidState, bool, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil {
		return runtimeRaidState{}, false, errRuntimeRaidNotFound
	}
	if _, ok := runtimeRaidFrozenMember(*state, characterID); !ok {
		return runtimeRaidState{}, false, errRuntimeRaidNotMember
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if state.Lifecycle.Status == runtimeRaidLifecycleAwaitingRewards && state.Lifecycle.Phase == phase &&
		state.Lifecycle.MovieConfirmed[characterID] && s.runtimeRaidMovieRosterConfirmedLocked(state) {
		return cloneRuntimeRaidState(*state), true, nil
	}
	// The card window is already open and THIS member's confirmation arrives late:
	// either the bounded wait opened it for the raid (see raid_movie_fallback.go)
	// or the member is re-sending after a reconnect. Record the confirmation and
	// report the roster as confirmed so the caller runs the same movie-complete
	// broadcast -- the client leaves its result movie and joins the open card
	// window instead of waiting for an ACK that would otherwise never come.
	if state.Lifecycle.Status == runtimeRaidLifecycleAwaitingRewards && state.Lifecycle.Phase == phase {
		if !state.Lifecycle.MovieConfirmed[characterID] {
			state.Lifecycle.MovieConfirmed[characterID] = true
			state.Lifecycle.Revision++
		}
		return cloneRuntimeRaidState(*state), true, nil
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie || state.Lifecycle.Phase != phase {
		return runtimeRaidState{}, false, errRuntimeRaidLifecycleState
	}
	if state.Lifecycle.MovieConfirmed[characterID] {
		return cloneRuntimeRaidState(*state), false, nil
	}
	state.Lifecycle.MovieConfirmed[characterID] = true
	state.Lifecycle.Revision++
	if !s.runtimeRaidMovieRosterConfirmedLocked(state) {
		return cloneRuntimeRaidState(*state), false, nil
	}
	state.Lifecycle.MovieOpenedAt = time.Time{}
	state.Lifecycle.PhaseDeadline = time.Time{}
	if phase != runtimeRaidDungeonPhaseOne && phase != runtimeRaidDungeonPhaseTwo {
		return runtimeRaidState{}, false, errRuntimeRaidPhaseMismatch
	}
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	state.Lifecycle.RewardToken = 0
	state.Lifecycle.RewardOpenedAt = now
	clear(state.Lifecycle.RewardSelections)
	// The members the movie gate skipped stay skipped: this transition opens the
	// cards for the members that are still here, and the record made above is
	// what lets the rest of the settlement agree on who those are. It is cleared
	// with the rest of the reward state when the next phase starts.
	state.Lifecycle.RewardDelivery = nil
	state.Lifecycle.RewardDeliveryCommitted = false
	state.Lifecycle.RewardDeliveryCommittedAt = time.Time{}
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), true, nil
}

func (s *Service) setRuntimeRaidPhaseDeadline(raidKey uint32, phase byte, deadline time.Time) (runtimeRaidState, error) {
	if deadline.IsZero() {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	deadline = deadline.UTC()
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.Lifecycle.Phase != phase || (state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive && state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie) {
		return runtimeRaidState{}, errRuntimeRaidPhaseMismatch
	}
	if !state.Lifecycle.PhaseStartedAt.IsZero() && !deadline.After(state.Lifecycle.PhaseStartedAt) {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	if state.Lifecycle.PhaseDeadline.Equal(deadline) {
		return cloneRuntimeRaidState(*state), nil
	}
	state.Lifecycle.PhaseDeadline = deadline
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}

func runtimeRaidPhaseElapsed(state runtimeRaidState, now time.Time) time.Duration {
	now = runtimeRaidNormalizeTime(now)
	if state.Lifecycle.PhaseStartedAt.IsZero() || now.Before(state.Lifecycle.PhaseStartedAt) {
		return 0
	}
	return now.Sub(state.Lifecycle.PhaseStartedAt)
}

func runtimeRaidPhaseRemaining(state runtimeRaidState, now time.Time) time.Duration {
	now = runtimeRaidNormalizeTime(now)
	if state.Lifecycle.PhaseDeadline.IsZero() || !now.Before(state.Lifecycle.PhaseDeadline) {
		return 0
	}
	return state.Lifecycle.PhaseDeadline.Sub(now)
}

// expireRuntimeRaidPhase is an explicit timer callback. Merely reading a zero
// remaining duration never mutates the raid, so packet retries and clock reads
// cannot race final cleanup.
// expireRuntimeRaidPhase fails the phase whose attack window just ran out.
//
// 86JP's timeout is not a teardown: RaidHandler.Runtime.cs RunAttackTimeoutAsync
// (:592-728) calls RaidManager.TryFailPhase, which turns the raid into state 4 with
// argument 1 (RaidManager.cs:1956-1982), closes every gate of that phase (state 2),
// sets the phase-one failure symbol, sends the fail result and then hands the raid
// to a ten-second remover. This server used to delete the raid on the spot, so the
// members never saw a result screen and a client that was still inside a gate kept a
// run the raid no longer owned.
//
// The raid therefore stays in the raid table in runtimeRaidLifecycleFailed; the
// caller publishes the failure presentation and arms the delayed removal
// (removeRuntimeRaidFailedPhase). Every other path already requires
// runtimeRaidLifecyclePhaseActive, so a failed raid accepts no further entry,
// completion or reward gesture while it waits.
func (s *Service) expireRuntimeRaidPhase(raidKey uint32, phase byte, now time.Time) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.StartPending || state.Lifecycle.Phase != phase ||
		(state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive && state.Lifecycle.Status != runtimeRaidLifecycleAwaitingMovie) ||
		state.Lifecycle.PhaseDeadline.IsZero() || now.Before(state.Lifecycle.PhaseDeadline) {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	// Every gate of the expired phase is closed, exactly as 86JP closes the phase's
	// dungeon states before it announces the failure. The gate keeps its clear
	// count and its remaining HP: the state row is what tells the client the gate
	// is gone, and a later re-read must not look like a completed objective.
	for mapID, objective := range state.Lifecycle.Objectives {
		if objective.Phase != phase {
			continue
		}
		objective.State = raid.AntonDungeonHide
		state.Lifecycle.Objectives[mapID] = objective
	}
	if phase == runtimeRaidDungeonPhaseOne {
		// 86JP AntonPhaseOneFailSymbolId = 104 (RaidHandler.AntonDefinitions.cs:55)
		// is the one-shot marker that plays the failure presentation of the first
		// phase; the second phase has no equivalent symbol.
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolPhaseOneFail, 1)
	}
	state.clearRuntimeRaidMechanismsLocked()
	state.Lifecycle.Status = runtimeRaidLifecycleFailed
	state.Lifecycle.FailedAt = now
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}

// removeRuntimeRaidFailedPhase releases a failed raid after its result window.
//
// It is 86JP's RemoveFailedRaidAfterResultDelayAsync (RaidHandler.Runtime.cs:730-762):
// the terminal object is dropped, the runtime state is cleaned up, and the removal is
// announced to the exact sessions that were in it. On this server the members still
// standing inside a gate of the failed phase are also sent back to town, because a
// run whose raid no longer exists can never be finished or abandoned by its own
// client: without that, the operator is left standing in a dungeon nobody owns.
func (s *Service) removeRuntimeRaidFailedPhase(raidKey uint32, phase byte) {
	if s == nil || raidKey == 0 {
		return
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.Lifecycle.Status != runtimeRaidLifecycleFailed || state.Lifecycle.Phase != phase {
		s.raidMu.Unlock()
		return
	}
	failed := cloneRuntimeRaidState(*state)
	delete(s.raids, raidKey)
	s.raidMu.Unlock()

	if returned := s.forceReturnRuntimeRaidPhaseGroups(raidKey, phase, "raid_phase_attack_timeout"); returned > 0 {
		s.logPacketEvent("game-raid-phase-groups-force-returned",
			"raid_key", raidKey,
			"raid_phase", phase,
			"returned_sessions", returned,
			"reason", "the_attack_window_expired_and_the_failed_raid_no_longer_owns_its_gates")
	}
	if err := s.publishExpiredRuntimeRaid(failed); err != nil {
		s.logPacketEvent("game-raid-failed-raid-release-failed",
			"raid_key", raidKey, "raid_phase", phase, "error", err)
	}
}

// openRuntimeRaidRewards freezes an opaque reward lifecycle token after the
// second movie. The reward resolver owns every PVF pool and durable grant.
func (s *Service) openRuntimeRaidRewards(raidKey uint32, rewardToken uint64, now time.Time) (runtimeRaidState, error) {
	if rewardToken == 0 {
		return runtimeRaidState{}, errRuntimeRaidRewardToken
	}
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		(state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne && state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo) {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	if state.Lifecycle.RewardToken != 0 {
		if state.Lifecycle.RewardToken != rewardToken {
			return runtimeRaidState{}, errRuntimeRaidRewardToken
		}
		return cloneRuntimeRaidState(*state), nil
	}
	// An externally supplied token means this settlement is already keyed, so the
	// eligible roster it was keyed on must stop moving here, exactly as it does
	// when this server mints the token itself.
	s.freezeRuntimeRaidRewardSkipsLocked(state)
	state.Lifecycle.RewardToken = rewardToken
	state.Lifecycle.RewardOpenedAt = now
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}

func (s *Service) confirmRuntimeRaidRewardSelection(characterID uint16, rewardToken uint64, mode byte, index byte, now time.Time) (runtimeRaidState, bool, error) {
	state, all, err := s.selectRuntimeRaidSettlementCard(characterID, 0, raid.RaidRewardCardRequest{Mode: mode, CardIndex: index}, now)
	if err != nil {
		return runtimeRaidState{}, false, err
	}
	if rewardToken != 0 && state.Lifecycle.RewardToken != 0 && state.Lifecycle.RewardToken != rewardToken {
		return runtimeRaidState{}, false, errRuntimeRaidRewardToken
	}
	return state, all, nil
}

func (s *Service) selectRuntimeRaidSettlementCard(characterID uint16, phase byte, request raid.RaidRewardCardRequest, now time.Time) (runtimeRaidState, bool, error) {
	now = runtimeRaidNormalizeTime(now)
	// Normalise the clicked slot before anything stores or compares it: the
	// client's resultType==3 branch only recognises card ordinals 0 and 2 and pops
	// "因未满足条件�?无法获得奖励�? (string 60456) for anything else, so a click on
	// slot 1 or 3 must be recorded as the slot it is drawn as. Normalising here
	// keeps the stored selection, its idempotency comparison, the reward plan and
	// the durable receipt consistent with the wire rows.
	request.CardIndex = currentRaidRewardWireCardIndex(request.CardIndex)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil {
		return runtimeRaidState{}, false, errRuntimeRaidNotFound
	}
	if _, ok := runtimeRaidFrozenMember(*state, characterID); !ok {
		return runtimeRaidState{}, false, errRuntimeRaidNotMember
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if phase == 0 {
		phase = state.Lifecycle.Phase
	}
	if state.Lifecycle.Status == runtimeRaidLifecycleStandby &&
		phase == runtimeRaidDungeonPhaseOne && state.Lifecycle.Phase == phase &&
		state.Lifecycle.RewardDeliveryCommitted && state.Lifecycle.RewardDelivery != nil {
		selection, exists := state.Lifecycle.RewardSelections[characterID]
		if request.Mode == 0 && exists && selection.Mode == 0 && selection.Index == request.CardIndex {
			return cloneRuntimeRaidState(*state), true, nil
		}
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || state.Lifecycle.Phase != phase {
		return runtimeRaidState{}, false, errRuntimeRaidLifecycleState
	}
	// A member the settlement already wrote off (it dropped before confirming,
	// and the eligible roster is now pinned) may come back and find the card
	// screen waiting. Acknowledging the click keeps its client from looping on a
	// failure without granting anything: its row is not in the committed receipt,
	// and the roster it is being told about already excludes it.
	if state.Lifecycle.RewardRosterFrozen && state.Lifecycle.RewardSkippedMembers[characterID] {
		return cloneRuntimeRaidState(*state), s.runtimeRaidRewardRosterConfirmedLocked(state), nil
	}
	// A member that cleared no gate this phase is not part of the settlement either:
	// the client may still put it in front of the cards (its own 地下城通关 cell reads
	// 0/1 and its resultType==3 branch pops 60456 for an unrecognised ordinal), and
	// acknowledging the click keeps it from looping on a failure without granting
	// anything. Its row is not in the plan, the receipt or the boards -- see
	// runtimeRaidRewardEligibleMembersLocked.
	if !runtimeRaidMemberClearedGateLocked(*state, characterID) {
		return cloneRuntimeRaidState(*state), s.runtimeRaidRewardRosterConfirmedLocked(state), nil
	}
	if selection, exists := state.Lifecycle.RewardSelections[characterID]; exists {
		if selection.Index != request.CardIndex {
			return runtimeRaidState{}, false, errRuntimeRaidRewardSelectionFrozen
		}
		if selection.Mode == request.Mode {
			return cloneRuntimeRaidState(*state), s.runtimeRaidRewardRosterConfirmedLocked(state), nil
		}
		if selection.Mode != 1 || request.Mode != 0 {
			return runtimeRaidState{}, false, errRuntimeRaidRewardSelectionOrder
		}
		selection.Mode = 0
		selection.ConfirmedAt = now
		state.Lifecycle.RewardSelections[characterID] = selection
		state.Lifecycle.Revision++
		return cloneRuntimeRaidState(*state), s.runtimeRaidRewardRosterConfirmedLocked(state), nil
	}
	if request.Mode != 1 {
		return runtimeRaidState{}, false, errRuntimeRaidRewardSelectionOrder
	}
	state.Lifecycle.RewardSelections[characterID] = runtimeRaidRewardSelection{Mode: 1, Index: request.CardIndex}
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), false, nil
}

// runtimeRaidRewardRosterConfirmed reports whether every member the settlement
// still owes a card to has confirmed one. A member the server has given up on is
// not owed anything and therefore cannot hold the roster open -- that is the
// whole point: one crashed client used to freeze the flip for everyone else.
//
// Caller holds raidMu.
func (s *Service) runtimeRaidRewardRosterConfirmedLocked(state *runtimeRaidState) bool {
	eligible := s.runtimeRaidRewardEligibleMembersLocked(state)
	if len(eligible) == 0 {
		return false
	}
	for _, member := range eligible {
		selection, ok := state.Lifecycle.RewardSelections[member.CharID]
		if !ok || selection.Mode != 0 {
			return false
		}
	}
	return true
}

// runtimeRaidMovieRosterConfirmed is the movie half of the same rule: the phase
// advances to its cards once every member that is still here confirmed the
// result movie, and a member whose socket is gone is skipped instead of waited
// for.
//
// Caller holds raidMu.
func (s *Service) runtimeRaidMovieRosterConfirmedLocked(state *runtimeRaidState) bool {
	eligible := s.runtimeRaidRewardEligibleMembersLocked(state)
	if len(eligible) == 0 {
		return false
	}
	for _, member := range eligible {
		if !state.Lifecycle.MovieConfirmed[member.CharID] {
			return false
		}
	}
	return true
}

// runtimeRaidRewardEligibleMembersLocked is the frozen roster minus the members the
// settlement pays nothing to, in roster order: a member that dropped, and a member
// that cleared no gate in the active phase.
//
// The clear requirement is the reference's own rule: 86JP builds the reward flow's
// eligible set from `HasClearedDungeon` (RaidHandler.Progression.cs:477-480, and the
// same shape for the phase-two waiting list at :766), and every card operation checks
// that set -- the reveal (:132-135), the card request (:399) and the squad reward
// (:557). A member with 0/1 in the 地下城通关 column never cleared a gate, so the flip
// window owes it nothing.
//
// Caller holds raidMu.
func (s *Service) runtimeRaidRewardEligibleMembersLocked(state *runtimeRaidState) []runtimeRaidMemberState {
	if state == nil || len(state.FrozenMembers) == 0 {
		return nil
	}
	s.refreshRuntimeRaidRewardSkipsLocked(state)
	eligible := make([]runtimeRaidMemberState, 0, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		if state.Lifecycle.RewardSkippedMembers[member.CharID] {
			continue
		}
		if !runtimeRaidMemberClearedGateLocked(*state, member.CharID) {
			continue
		}
		eligible = append(eligible, member)
	}
	return eligible
}

// runtimeRaidMemberClearedGateLocked reports whether the active phase's clear
// registration entitles this member to a reward.
//
// The counter is the same one the 地下城通关 column is drawn from
// (runtimeRaidMemberRecord's Field56), and the requirement is the phase's own
// [MINIMAL DUNGEON CLEAR COUNT]: the official reward condition is
// 「1阶段：无条件 / 2阶段：通关2个以上的地下城」, which is exactly the pair Luke
// declares (0 for phase one, 2 for phase two). The window therefore reads
// "1/2" and withholds the card, instead of paying a member whose column is not
// full.
//
// Anton is bit-for-bit unchanged: all of its phases declare 1, and
// `count >= 1` is the same predicate as `count > 0` on the uint32 counter. Its
// team phases cannot even declare 0 -- antonParsePhaseWithPartyMax demands a
// positive [MINIMAL DUNGEON CLEAR COUNT] (anton_pvf.go:586), and its single
// phase, whose PVF block omits the section entirely, is defaulted to 1
// (anton_pvf.go:503).
func runtimeRaidMemberClearedGateLocked(state runtimeRaidState, characterID uint16) bool {
	if characterID == 0 {
		return false
	}
	cleared := state.Lifecycle.PhaseClearCounts[characterID]
	minimum, known := runtimeRaidPhaseMinimalClearRequirement(state, state.Lifecycle.Phase)
	if !known {
		// No phase catalog covers this phase (a hand-built state, an unavailable
		// config). Keep the pre-PVF rule instead of reading a missing
		// declaration as "no requirement" and handing everybody a card.
		return cleared > 0
	}
	if minimum <= 0 {
		// [MINIMAL DUNGEON CLEAR COUNT] 0 is the official 「1阶段：无条件」
		// (Luke phase one): the phase pays its whole roster.
		return true
	}
	return cleared >= uint32(minimum)
}

// runtimeRaidPhaseMinimalClearRequirement is the active phase's
// [MINIMAL DUNGEON CLEAR COUNT] together with whether a phase catalog actually
// declares one.
//
// It mirrors runtimeRaidPhaseConfig's branch structure, which
// runtimeRaidPhaseMinimalClearCount deliberately does not: a single-mode raid
// reads config.SingleMode.Phase, not config.Phases[0]. The two disagree for
// Luke -- team phase one declares 0, while the single-mode phase (whose PVF
// block omits the section) parses to the shared parser's default of 1 -- so
// deriving the requirement from runtimeRaidPhaseMinimalClearCount would make
// single-mode Luke unconditional.
//
// known is false whenever no catalog covers the phase; callers must then keep
// their own rule, because a missing 0 is not a declared 0.
func runtimeRaidPhaseMinimalClearRequirement(state runtimeRaidState, phase byte) (int, bool) {
	if phase == 0 {
		return 0, false
	}
	config := runtimeRaidBaseConfig(state)
	if config == nil {
		return 0, false
	}
	if state.TailFlag == 1 {
		if phase != runtimeRaidDungeonPhaseOne || config.SingleMode == nil {
			return 0, false
		}
		return config.SingleMode.Phase.MinimalDungeonClearCount, true
	}
	index := int(phase) - 1
	if index < 0 || index >= len(config.Phases) {
		return 0, false
	}
	return config.Phases[index].MinimalDungeonClearCount, true
}

// refreshRuntimeRaidRewardSkipsLocked keeps RewardSkippedMembers in step with
// live presence while the settlement is still open.
//
// A member is skipped exactly when it has dropped (a disconnect reservation for
// this raid, i.e. its socket is gone and only a reconnect restores the seat) or
// the grace window already removed it from the roster, AND it has not confirmed
// a card yet -- a member that picked its card before the crash earned its
// reward and must be paid even if it never comes back.
//
// The set only ever grows, and only until RewardRosterFrozen pins it: once the
// delivery token is minted the eligible roster is part of the receipt, so a
// later reconnect can neither add itself back nor invalidate the settlement.
//
// Caller holds raidMu.
func (s *Service) refreshRuntimeRaidRewardSkipsLocked(state *runtimeRaidState) {
	if state == nil || state.Lifecycle.RewardRosterFrozen {
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	for _, member := range state.FrozenMembers {
		if state.Lifecycle.RewardSkippedMembers[member.CharID] {
			continue
		}
		if selection, picked := state.Lifecycle.RewardSelections[member.CharID]; picked && selection.Mode == 0 {
			continue
		}
		if s.runtimeRaidMemberSkippedForRewardsLocked(*state, member.CharID) {
			state.Lifecycle.RewardSkippedMembers[member.CharID] = true
		}
	}
}

// freezeRuntimeRaidRewardSkipsLocked pins the eligible roster. It is called at
// the exact moment the settlement stops being an open negotiation and becomes a
// durable receipt. Caller holds raidMu.
func (s *Service) freezeRuntimeRaidRewardSkipsLocked(state *runtimeRaidState) {
	if state == nil || state.Lifecycle.RewardRosterFrozen {
		return
	}
	s.refreshRuntimeRaidRewardSkipsLocked(state)
	state.Lifecycle.RewardRosterFrozen = true
}

// runtimeRaidMemberSkippedForRewardsLocked reports whether this member can no
// longer take part in the raid's settlement: its socket is gone (a disconnect
// reservation for this raid is held, see raid_disconnect.go) or the grace window
// already removed it. A member with no reservation and a live seat is never
// skipped, which is what makes a reconnect inside the grace window restore it.
//
// Caller holds raidMu.
func (s *Service) runtimeRaidMemberSkippedForRewardsLocked(state runtimeRaidState, characterID uint16) bool {
	if characterID == 0 || state.RaidKey == 0 {
		return false
	}
	if s.runtimeRaidMemberDisconnectedLocked(state, characterID) {
		return true
	}
	return runtimeRaidMemberIndex(state, characterID) < 0
}

// markRuntimeRaidRewardDeliveryCommitted records only the durable boundary;
// it does not resolve or grant any reward itself.
func (s *Service) markRuntimeRaidRewardDeliveryCommitted(raidKey uint32, rewardToken uint64, now time.Time) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || state.Lifecycle.RewardToken != rewardToken || rewardToken == 0 {
		return runtimeRaidState{}, errRuntimeRaidRewardToken
	}
	if !s.runtimeRaidRewardRosterConfirmedLocked(state) {
		return runtimeRaidState{}, errRuntimeRaidRewardRosterPending
	}
	if !state.Lifecycle.RewardDeliveryCommitted {
		state.Lifecycle.RewardDeliveryCommitted = true
		state.Lifecycle.RewardDeliveryCommittedAt = now
		state.Lifecycle.Revision++
	}
	return cloneRuntimeRaidState(*state), nil
}

// finalizeRuntimeRaid removes the global runtime object only after every
// frozen member confirmed a card and the reward owner committed delivery.
// finalizeRuntimeRaid is the terminal cleanup. It emits the per-raid cost summary
// AFTER releasing the raid lock: the summary reads the same lock (and the live
// session loops), so emitting it inside the locked body would self-deadlock.
func (s *Service) finalizeRuntimeRaid(raidKey uint32, rewardToken uint64, now time.Time) (runtimeRaidState, error) {
	finished, err := s.finalizeRuntimeRaidLocked(raidKey, rewardToken, now)
	if err == nil {
		s.publishRuntimeRaidCostSummary(raidKey, "raid_finalized")
	}
	return finished, err
}

func (s *Service) finalizeRuntimeRaidLocked(raidKey uint32, rewardToken uint64, now time.Time) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		state.Lifecycle.RewardToken != rewardToken || rewardToken == 0 ||
		!state.Lifecycle.RewardDeliveryCommitted || !s.runtimeRaidRewardRosterConfirmedLocked(state) {
		return runtimeRaidState{}, errRuntimeRaidRewardNotReady
	}
	state.Lifecycle.Status = runtimeRaidLifecycleFinished
	state.Lifecycle.FinishedAt = now
	state.Lifecycle.Revision++
	finished := cloneRuntimeRaidState(*state)
	delete(s.raids, raidKey)
	return finished, nil
}

func (s *Service) runtimeRaidIsSingle(raidKey uint32) bool {
	if s == nil {
		return false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	return state != nil && state.TailFlag == 1
}

// abandonRuntimeRaid is the explicit whole-raid abort path. A dungeon abandon
// event only releases that map attempt and never reaches this cleanup path.
func (s *Service) abandonRuntimeRaid(raidKey uint32, requestedBy uint16, now time.Time) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	if state.StartPending {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	if requestedBy != 0 && state.LeaderID != requestedBy {
		return runtimeRaidState{}, errRuntimeRaidNotMember
	}
	if state.Lifecycle.Status == runtimeRaidLifecycleFinished {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	state.Lifecycle.Status = runtimeRaidLifecycleAbandoned
	state.Lifecycle.AbandonedAt = now
	state.Lifecycle.Revision++
	abandoned := cloneRuntimeRaidState(*state)
	delete(s.raids, raidKey)
	return abandoned, nil
}

func runtimeRaidCompletedMapIDs(state runtimeRaidState, phase byte) []int {
	mapIDs := make([]int, 0)
	if len(state.Lifecycle.Objectives) != 0 {
		for mapID, objective := range state.Lifecycle.Objectives {
			if objective.Phase == phase && objective.RemainingHP == 0 {
				mapIDs = append(mapIDs, mapID)
			}
		}
		sort.Ints(mapIDs)
		return mapIDs
	}
	for runKey, progress := range state.Lifecycle.Dungeons {
		if progress.Phase == phase && progress.Status == runtimeRaidDungeonProgressCompleted {
			mapIDs = append(mapIDs, runKey.MapID)
		}
	}
	sort.Ints(mapIDs)
	return mapIDs
}

func cloneRuntimeRaidLifecycleState(state runtimeRaidLifecycleState) runtimeRaidLifecycleState {
	state.MovieConfirmed = cloneRuntimeRaidBoolMap(state.MovieConfirmed)
	objectives := state.Objectives
	state.Objectives = make(map[int]runtimeRaidDungeonObjective, len(objectives))
	for key, objective := range objectives {
		state.Objectives[key] = objective
	}
	dungeons := state.Dungeons
	state.Dungeons = make(map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress, len(dungeons))
	for key, progress := range dungeons {
		state.Dungeons[key] = progress
	}
	groupOccupancy := state.GroupOccupancy
	state.GroupOccupancy = make(map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding, len(groupOccupancy))
	for key, binding := range groupOccupancy {
		state.GroupOccupancy[key] = binding
	}
	runClearCredits := state.RunClearCredits
	state.RunClearCredits = make(map[runtimeRaidDungeonRunKey]uint64, len(runClearCredits))
	for key, token := range runClearCredits {
		state.RunClearCredits[key] = token
	}
	dungeonStates := state.DungeonStates
	state.DungeonStates = make(map[uint32]uint32, len(dungeonStates))
	for key, value := range dungeonStates {
		state.DungeonStates[key] = value
	}
	closedDungeonAttempts := state.ClosedDungeonAttempts
	state.ClosedDungeonAttempts = make(map[runtimeRaidDungeonAttemptKey]runtimeRaidDungeonTerminal, len(closedDungeonAttempts))
	for key, terminal := range closedDungeonAttempts {
		state.ClosedDungeonAttempts[key] = terminal
	}
	rewardSelections := state.RewardSelections
	state.RewardSelections = make(map[uint16]runtimeRaidRewardSelection, len(rewardSelections))
	for key, selection := range rewardSelections {
		state.RewardSelections[key] = selection
	}
	state.RewardSkippedMembers = cloneRuntimeRaidBoolMap(state.RewardSkippedMembers)
	state.RewardDelivery = cloneRuntimeRaidRewardDeliveryCache(state.RewardDelivery)
	mechanisms := state.Mechanisms
	state.Mechanisms = make(map[runtimeRaidMechanismKey]runtimeRaidMechanismTimer, len(mechanisms))
	for key, timer := range mechanisms {
		state.Mechanisms[key] = timer
	}
	barrierRequests := state.BarrierRequests
	state.BarrierRequests = make(map[runtimeRaidBarrierRequestKey]uint32, len(barrierRequests))
	for key, count := range barrierRequests {
		state.BarrierRequests[key] = count
	}
	// 卢克的四张表同样要深拷，否则「快照」会与活账本共享同一张 map ——
	// 上游的 TestLukeGeneratorReportsDeduplicateAcrossMembersPerRoom 钉的就是这条。
	state.LukeGeneratorReports = cloneRuntimeRaidLukeGeneratorReports(state.LukeGeneratorReports)
	if groups := state.LukeSanctumMazeGroups; groups != nil {
		state.LukeSanctumMazeGroups = make(map[int]int, len(groups))
		for mapID, start := range groups {
			state.LukeSanctumMazeGroups[mapID] = start
		}
	}
	lukeSymbols := state.LukeSymbols
	state.LukeSymbols = make(map[uint32]uint32, len(lukeSymbols))
	for key, value := range lukeSymbols {
		state.LukeSymbols[key] = value
	}
	lukeRouteLocks := state.LukeRouteLocks
	state.LukeRouteLocks = make(map[uint16]byte, len(lukeRouteLocks))
	for characterID, route := range lukeRouteLocks {
		state.LukeRouteLocks[characterID] = route
	}
	// 超时空战场同样必须深拷。Clone() 自己处理 nil，所以安东/卢克这条路径
	// (FiendWar 为 nil) 拿到的仍是 nil，行为不变。
	state.FiendWar = state.FiendWar.Clone()
	if counts := state.FiendWarClearCounts; counts != nil {
		state.FiendWarClearCounts = make(map[uint16]int, len(counts))
		for characterID, count := range counts {
			state.FiendWarClearCounts[characterID] = count
		}
	}
	if retreats := state.FiendWarRetreats; retreats != nil {
		state.FiendWarRetreats = make(map[int]runtimeRaidDungeonProgress, len(retreats))
		for dungeon, progress := range retreats {
			state.FiendWarRetreats[dungeon] = progress
		}
	}
	return state
}

func cloneRuntimeRaidBoolMap(source map[uint16]bool) map[uint16]bool {
	out := make(map[uint16]bool, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
