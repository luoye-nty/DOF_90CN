package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentDungeonAltarMaximumConcurrentPhases = 10

var (
	errCurrentDungeonAltarDefinitionInvalid = errors.New("current dungeon altar definition is invalid")
	errCurrentDungeonAltarRoomMismatch      = errors.New("current dungeon altar room does not own the request")
	errCurrentDungeonAltarRoomComplete      = errors.New("current dungeon altar room is already complete")
	errCurrentDungeonAltarSequenceUnknown   = errors.New("current dungeon altar monster sequence is unknown")
	errCurrentDungeonAltarSequenceCrossRoom = errors.New("current dungeon altar monster sequence belongs to another room")
	errCurrentDungeonAltarSequenceExhausted = errors.New("current dungeon altar monster sequence space is exhausted")
)

type currentDungeonAltarProgress uint8

const (
	currentDungeonAltarProgressNone currentDungeonAltarProgress = iota
	currentDungeonAltarProgressAwaitingReady
	currentDungeonAltarProgressRoundActive
	currentDungeonAltarProgressAwaitingDifficulty
	currentDungeonAltarProgressFinalRoundDelay
	currentDungeonAltarProgressRoomComplete
	currentDungeonAltarProgressDungeonComplete
)

type currentDungeonAltarRoomIdentity struct {
	Key        runtimeDungeonRoomKey
	Generation uint64
}

type currentDungeonAltarRound struct {
	PVFRound int64
	Phases   []worldmap.BloodAltarPhase
}

type currentDungeonAltarScheduledWave struct {
	Delay            time.Duration
	ExpireDelay      time.Duration
	RoundIndex       int
	GlobalRound      int
	PhaseIndex       int
	BatchIndex       int
	SpawnCount       int
	FinalRoomWave    bool
	FinalDungeonWave bool
	Phase            worldmap.BloodAltarPhase
	Monster          worldmap.BloodAltarMonster
}

type currentDungeonAltarRoundSchedule struct {
	RoomGeneration  uint64
	Generation      uint64
	RoundIndex      int
	GlobalRound     int
	Difficulty      worldmap.BloodAltarDifficulty
	InitialInterval time.Duration
	StartedAt       time.Time
	Waves           []currentDungeonAltarScheduledWave
}

type currentDungeonAltarCrazyMonster struct {
	RoundIndex  int
	PhaseIndex  int
	MonsterCode uint32
	Variant     byte
}

type currentDungeonAltarSpawn struct {
	Variant         byte
	Sequence        uint16
	MonsterCode     uint32
	MonsterType     byte
	Level           byte
	Scale           uint16
	X               uint16
	Y               uint16
	Z               uint16
	RoomGeneration  uint64
	RoundGeneration uint64
	FinalRoomWave   bool
}

type currentDungeonAltarWave struct {
	Spawns      []currentDungeonAltarSpawn
	Tail        uint16
	ScheduleGen uint64
	WaveIndex   int
}

type currentDungeonAltarSequenceOwner struct {
	RoomGeneration  uint64
	RoundGeneration uint64
	RoomKey         runtimeDungeonRoomKey
	MonsterCode     int64
	QuestDropSeed   uint32
	FinalRoomWave   bool
}

type currentDungeonAltarDeathSequence struct {
	Sequence uint16
	Owner    currentDungeonAltarSequenceOwner
}

type currentDungeonAltarDeathPreview struct {
	Active    []currentDungeonAltarDeathSequence
	Duplicate int
}

type currentDungeonAltarQuestDropResult struct {
	Body      []byte
	Delivered bool
}

type currentDungeonAltarQuestDropRetry struct {
	Room                     currentDungeonAltarRoomIdentity
	Sequences                []uint16
	Deaths                   []currentDungeonAltarDeathSequence
	DynamicDeathNotification bool
	Version                  uint64
}

type currentDungeonAltarDeathResult struct {
	Accepted     int
	Duplicate    int
	AutoReleased int
	Progress     currentDungeonAltarProgress
}

type currentDungeonAltarRuntime struct {
	Kind      worldmap.BloodAltarDungeonKind
	MaxRounds int
	Level     byte
	// entryDailyCount is the post-admission Endless Altar count used by the
	// authored experience-weight vector. Ultimate Altar does not use it.
	entryDailyCount uint32
	runSeed         uint32
	runSeedSet      bool
	boundMapType    string
	crazyMonster    *currentDungeonAltarCrazyMonster

	Room                     currentDungeonAltarRoomIdentity
	Rounds                   []currentDungeonAltarRound
	boundMonsters            []worldmap.BloodAltarMonster
	nextRoundIndex           int
	completedRounds          int
	completedRooms           map[runtimeDungeonRoomKey]struct{}
	progress                 currentDungeonAltarProgress
	readyAccepted            bool
	nextSequence             uint32
	generation               uint64
	difficulty               worldmap.BloodAltarDifficulty
	completedDifficulties    []worldmap.BloodAltarDifficulty
	difficultyVersion        uint64
	schedule                 *currentDungeonAltarRoundSchedule
	nextWaveIndex            int
	activeSequences          map[uint16]currentDungeonAltarSequenceOwner
	retiredSequences         map[uint16]currentDungeonAltarSequenceOwner
	questDropResults         map[uint16]currentDungeonAltarQuestDropResult
	questDropRetry           *currentDungeonAltarQuestDropRetry
	questDropRetryQueue      []*currentDungeonAltarQuestDropRetry
	questDropRetryProgress   currentDungeonAltarProgress
	waveTimerName            string
	expiryTimerNames         map[string]struct{}
	difficultyTimerName      string
	finalTimerName           string
	questDropRetryTimerName  string
	questDropRetryVersion    uint64
	roomTransitionTimerName  string
	roomTransitionVersion    uint64
	settlementRetryTimerName string
	settlementRetryVersion   uint64
	settlement               *currentDungeonAltarSettlementRuntime
}

func newCurrentDungeonAltarRuntime(
	dungeon worldmap.Dungeon,
	scene worldmap.DungeonRoomScene,
) (*currentDungeonAltarRuntime, error) {
	if dungeon.BloodAltarKind == 0 {
		if dungeon.ID == 11006 || dungeon.ID == 11007 {
			return nil, fmt.Errorf("%w: dungeon=%d kind=%d max_rounds=%d", errCurrentDungeonAltarDefinitionInvalid, dungeon.ID, dungeon.BloodAltarKind, dungeon.BloodMaxRounds)
		}
		return nil, nil
	}
	if (dungeon.ID != 11006 || dungeon.BloodAltarKind != worldmap.BloodAltarDungeonKindEndless || dungeon.BloodMaxRounds != 15) &&
		(dungeon.ID != 11007 || dungeon.BloodAltarKind != worldmap.BloodAltarDungeonKindUltimate || dungeon.BloodMaxRounds != 7) {
		return nil, fmt.Errorf("%w: dungeon=%d kind=%d max_rounds=%d", errCurrentDungeonAltarDefinitionInvalid, dungeon.ID, dungeon.BloodAltarKind, dungeon.BloodMaxRounds)
	}
	if !dungeon.Metadata.BasisLevel.Set || dungeon.Metadata.BasisLevel.Value <= 0 || dungeon.Metadata.BasisLevel.Value > math.MaxUint8 {
		return nil, fmt.Errorf("%w: dungeon=%d basis_level=%d set=%t", errCurrentDungeonAltarDefinitionInvalid, dungeon.ID, dungeon.Metadata.BasisLevel.Value, dungeon.Metadata.BasisLevel.Set)
	}
	runtime := &currentDungeonAltarRuntime{
		Kind:             dungeon.BloodAltarKind,
		MaxRounds:        int(dungeon.BloodMaxRounds),
		Level:            byte(dungeon.Metadata.BasisLevel.Value),
		completedRooms:   make(map[runtimeDungeonRoomKey]struct{}),
		nextSequence:     1,
		activeSequences:  make(map[uint16]currentDungeonAltarSequenceOwner),
		retiredSequences: make(map[uint16]currentDungeonAltarSequenceOwner),
		questDropResults: make(map[uint16]currentDungeonAltarQuestDropResult),
		expiryTimerNames: make(map[string]struct{}),
	}
	if err := runtime.bindRoom(scene); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (runtime *currentDungeonAltarRuntime) bindRoom(scene worldmap.DungeonRoomScene) error {
	if runtime == nil {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	if len(scene.BlockingHostiles) != 0 {
		return fmt.Errorf("%w: room=%s map=%d ordinary_blocking_hostiles=%d", errCurrentDungeonAltarDefinitionInvalid, scene.Coordinate, scene.Map.Map.ID, len(scene.BlockingHostiles))
	}
	key := runtimeDungeonRoomKeyFromScene(scene)
	if runtime.Room.Generation != 0 && runtime.Room.Key == key {
		return nil
	}
	if _, complete := runtime.completedRooms[key]; complete {
		return fmt.Errorf("%w: target=%+v", errCurrentDungeonAltarRoomComplete, key)
	}
	if runtime.Room.Generation != 0 {
		if _, complete := runtime.completedRooms[runtime.Room.Key]; !complete {
			return fmt.Errorf("%w: current=%+v target=%+v reason=current_room_incomplete", errCurrentDungeonAltarRoomMismatch, runtime.Room.Key, key)
		}
	}
	rounds, monsters, err := currentDungeonAltarRounds(runtime.Kind, scene.Map.Map)
	if err != nil {
		return err
	}
	if runtime.completedRounds+len(rounds) > runtime.MaxRounds {
		return fmt.Errorf("%w: completed=%d room_rounds=%d max=%d", errCurrentDungeonAltarDefinitionInvalid, runtime.completedRounds, len(rounds), runtime.MaxRounds)
	}
	runtime.generation = nextCurrentDungeonAltarGeneration(runtime.generation)
	runtime.Room = currentDungeonAltarRoomIdentity{Key: key, Generation: runtime.generation}
	runtime.Rounds = rounds
	runtime.boundMonsters = monsters
	runtime.boundMapType = scene.Map.Map.Type
	runtime.nextRoundIndex = 0
	runtime.progress = currentDungeonAltarProgressAwaitingReady
	runtime.readyAccepted = false
	runtime.difficulty = ""
	runtime.schedule = nil
	runtime.nextWaveIndex = 0
	runtime.activeSequences = make(map[uint16]currentDungeonAltarSequenceOwner)
	runtime.waveTimerName = ""
	runtime.expiryTimerNames = make(map[string]struct{})
	runtime.difficultyTimerName = ""
	runtime.finalTimerName = ""
	runtime.questDropRetry = nil
	runtime.questDropRetryQueue = nil
	runtime.questDropRetryProgress = currentDungeonAltarProgressNone
	runtime.questDropRetryTimerName = ""
	runtime.roomTransitionTimerName = ""
	runtime.settlementRetryTimerName = ""
	if runtime.runSeedSet {
		if err := runtime.configureCrazyMonster(); err != nil {
			return err
		}
	} else {
		runtime.crazyMonster = nil
	}
	return nil
}

func (runtime *currentDungeonAltarRuntime) configureRunSeed(seed uint32) error {
	if runtime == nil {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	runtime.runSeed = seed
	runtime.runSeedSet = true
	return runtime.configureCrazyMonster()
}

// configureCrazyMonster reproduces the reference Endless Altar room rule:
// choose one major round, mark the first monster of its first phase with a
// weighted variant, and announce that pair before the round starts. Boss maps
// exclude their fifth/final round. Ultimate Altar explicitly disables this
// mechanic.
func (runtime *currentDungeonAltarRuntime) configureCrazyMonster() error {
	runtime.crazyMonster = nil
	if runtime.Kind != worldmap.BloodAltarDungeonKindEndless {
		return nil
	}
	if len(runtime.Rounds) != 5 || len(runtime.boundMonsters) == 0 || runtime.Room.Generation == 0 {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	roundChoices := len(runtime.Rounds)
	if strings.EqualFold(strings.TrimSpace(runtime.boundMapType), "[boss]") {
		roundChoices--
	}
	if roundChoices <= 0 {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	seed := runtime.runSeed ^ uint32(runtime.Room.Key.MapID) ^ uint32(runtime.Room.Key.MapID>>32) ^
		uint32(runtime.Room.Key.X*0x45d9f3b) ^ uint32(runtime.Room.Key.Y*0x119de1f3)
	rng := newCurrentDungeonDropLCG(seed)
	roundIndex := rng.Next(roundChoices)
	if roundIndex < 0 || roundIndex >= len(runtime.Rounds) || len(runtime.Rounds[roundIndex].Phases) == 0 {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	phase := runtime.Rounds[roundIndex].Phases[0]
	if phase.MonsterTemplateIndex < 0 || phase.MonsterTemplateIndex >= int64(len(runtime.boundMonsters)) {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	monster := runtime.boundMonsters[phase.MonsterTemplateIndex]
	if monster.MonsterCode <= 0 || monster.MonsterCode > math.MaxUint32 {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	runtime.crazyMonster = &currentDungeonAltarCrazyMonster{
		RoundIndex:  roundIndex,
		PhaseIndex:  0,
		MonsterCode: uint32(monster.MonsterCode),
		Variant:     currentDungeonAltarCrazyMonsterVariant(rng),
	}
	return nil
}

func currentDungeonAltarCrazyMonsterVariant(rng *currentDungeonDropLCG) byte {
	roll := rng.Next(100)
	switch {
	case roll < 40:
		return byte(rng.Next(5))
	case roll < 70:
		return byte(5 + rng.Next(6))
	case roll < 85:
		return byte(11 + rng.Next(4))
	case roll < 95:
		return byte(15 + rng.Next(3))
	default:
		return 18
	}
}

func (runtime *currentDungeonAltarRuntime) previewBindRoom(scene worldmap.DungeonRoomScene) error {
	if runtime == nil {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	if len(scene.BlockingHostiles) != 0 {
		return fmt.Errorf("%w: room=%s map=%d ordinary_blocking_hostiles=%d", errCurrentDungeonAltarDefinitionInvalid, scene.Coordinate, scene.Map.Map.ID, len(scene.BlockingHostiles))
	}
	key := runtimeDungeonRoomKeyFromScene(scene)
	if runtime.Room.Key == key {
		return nil
	}
	if _, complete := runtime.completedRooms[key]; complete {
		return fmt.Errorf("%w: target=%+v", errCurrentDungeonAltarRoomComplete, key)
	}
	if _, complete := runtime.completedRooms[runtime.Room.Key]; !complete {
		return fmt.Errorf("%w: current=%+v target=%+v", errCurrentDungeonAltarRoomMismatch, runtime.Room.Key, key)
	}
	rounds, _, err := currentDungeonAltarRounds(runtime.Kind, scene.Map.Map)
	if err != nil {
		return err
	}
	if runtime.completedRounds+len(rounds) > runtime.MaxRounds {
		return fmt.Errorf("%w: completed=%d target_rounds=%d max=%d", errCurrentDungeonAltarDefinitionInvalid, runtime.completedRounds, len(rounds), runtime.MaxRounds)
	}
	return nil
}

func currentDungeonAltarRounds(
	kind worldmap.BloodAltarDungeonKind,
	mapValue worldmap.Map,
) ([]currentDungeonAltarRound, []worldmap.BloodAltarMonster, error) {
	monsters := mapValue.BloodMonsters
	phases := mapValue.BloodPhases
	if kind == worldmap.BloodAltarDungeonKindUltimate {
		monsters = mapValue.UltimateMonsters
		phases = mapValue.UltimatePhases
	}
	if len(monsters) == 0 || len(phases) == 0 {
		return nil, nil, fmt.Errorf("%w: map=%d kind=%d monsters=%d phases=%d", errCurrentDungeonAltarDefinitionInvalid, mapValue.ID, kind, len(monsters), len(phases))
	}
	grouped := make(map[int64][]worldmap.BloodAltarPhase)
	keys := make([]int64, 0)
	for _, phase := range phases {
		if phase.MonsterTemplateIndex < 0 || phase.MonsterTemplateIndex >= int64(len(monsters)) {
			return nil, nil, fmt.Errorf("%w: map=%d phase_round=%d monster_index=%d", errCurrentDungeonAltarDefinitionInvalid, mapValue.ID, phase.Round, phase.MonsterTemplateIndex)
		}
		if _, exists := grouped[phase.Round]; !exists {
			keys = append(keys, phase.Round)
		}
		grouped[phase.Round] = append(grouped[phase.Round], phase)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	expectedRoundCount := 0
	switch kind {
	case worldmap.BloodAltarDungeonKindEndless:
		expectedRoundCount = 5
	case worldmap.BloodAltarDungeonKindUltimate:
		expectedRoundCount = 7
	default:
		return nil, nil, fmt.Errorf("%w: map=%d unsupported kind=%d", errCurrentDungeonAltarDefinitionInvalid, mapValue.ID, kind)
	}
	if len(keys) != expectedRoundCount {
		return nil, nil, fmt.Errorf("%w: map=%d kind=%d rounds=%d expected=%d", errCurrentDungeonAltarDefinitionInvalid, mapValue.ID, kind, len(keys), expectedRoundCount)
	}
	for index, key := range keys {
		if key != int64(index) {
			return nil, nil, fmt.Errorf("%w: map=%d kind=%d round_key=%d expected=%d", errCurrentDungeonAltarDefinitionInvalid, mapValue.ID, kind, key, index)
		}
	}
	rounds := make([]currentDungeonAltarRound, 0, len(keys))
	for _, key := range keys {
		rounds = append(rounds, currentDungeonAltarRound{PVFRound: key, Phases: append([]worldmap.BloodAltarPhase(nil), grouped[key]...)})
	}
	return rounds, append([]worldmap.BloodAltarMonster(nil), monsters...), nil
}

func (runtime *currentDungeonAltarRuntime) acceptReady(
	room runtimeDungeonRoomKey,
	startedAt time.Time,
) (*currentDungeonAltarRoundSchedule, bool, error) {
	if runtime == nil || runtime.Room.Key != room {
		return nil, false, errCurrentDungeonAltarRoomMismatch
	}
	if runtime.readyAccepted {
		return nil, false, nil
	}
	if runtime.progress != currentDungeonAltarProgressAwaitingReady {
		return nil, false, nil
	}
	runtime.readyAccepted = true
	schedule, err := runtime.beginRound(startedAt)
	return schedule, schedule != nil, err
}

func (runtime *currentDungeonAltarRuntime) beginRound(startedAt time.Time) (*currentDungeonAltarRoundSchedule, error) {
	if runtime == nil || runtime.nextRoundIndex < 0 || runtime.nextRoundIndex >= len(runtime.Rounds) || runtime.schedule != nil || len(runtime.activeSequences) != 0 {
		return nil, nil
	}
	difficulty := worldmap.BloodAltarDifficulty("")
	if runtime.Kind == worldmap.BloodAltarDungeonKindUltimate {
		fixed := runtime.nextRoundIndex == 0 || runtime.nextRoundIndex == len(runtime.Rounds)-1
		if fixed {
			difficulty = worldmap.BloodAltarDifficultyA
		} else {
			difficulty = runtime.difficulty
		}
		if difficulty != worldmap.BloodAltarDifficultyA && difficulty != worldmap.BloodAltarDifficultyB {
			return nil, nil
		}
	}
	round := runtime.Rounds[runtime.nextRoundIndex]
	phases := make([]worldmap.BloodAltarPhase, 0, len(round.Phases))
	for _, phase := range round.Phases {
		if runtime.Kind != worldmap.BloodAltarDungeonKindUltimate || phase.Difficulty == difficulty {
			phases = append(phases, phase)
		}
	}
	if len(phases) == 0 {
		return nil, fmt.Errorf("%w: room=%+v round=%d difficulty=%q has no phases", errCurrentDungeonAltarDefinitionInvalid, runtime.Room.Key, runtime.nextRoundIndex, difficulty)
	}
	waves, err := runtime.buildRoundSchedule(phases, runtime.nextRoundIndex, runtime.completedRounds)
	if err != nil {
		return nil, err
	}
	if len(waves) == 0 {
		return nil, fmt.Errorf("%w: room=%+v round=%d has no waves", errCurrentDungeonAltarDefinitionInvalid, runtime.Room.Key, runtime.nextRoundIndex)
	}
	runtime.generation = nextCurrentDungeonAltarGeneration(runtime.generation)
	schedule := &currentDungeonAltarRoundSchedule{
		RoomGeneration:  runtime.Room.Generation,
		Generation:      runtime.generation,
		RoundIndex:      runtime.nextRoundIndex,
		GlobalRound:     runtime.completedRounds,
		Difficulty:      difficulty,
		InitialInterval: time.Duration(maxInt64(0, phases[0].DelayMilliseconds)) * time.Millisecond,
		StartedAt:       startedAt,
		Waves:           waves,
	}
	runtime.difficulty = ""
	runtime.schedule = schedule
	runtime.nextWaveIndex = 0
	runtime.progress = currentDungeonAltarProgressRoundActive
	return schedule, nil
}

func (runtime *currentDungeonAltarRuntime) buildRoundSchedule(
	phases []worldmap.BloodAltarPhase,
	roundIndex int,
	globalRound int,
) ([]currentDungeonAltarScheduledWave, error) {
	var monsters []worldmap.BloodAltarMonster
	// The currently-bound map tables are recovered from phase indices through
	// the frozen room map by the caller-provided runtime copy below.
	// Store them transiently from the room rounds' source is not possible, so
	// bindRoom fills altarMonsters alongside Rounds.
	monsters = runtime.monsters()
	if len(monsters) == 0 {
		return nil, errCurrentDungeonAltarDefinitionInvalid
	}
	waves := make([]currentDungeonAltarScheduledWave, 0)
	elapsed := int64(0)
	for index := 0; index < len(phases); {
		groupWaveStart := len(waves)
		concurrent := int(phases[index].ConcurrentPhaseCount)
		if concurrent < 1 {
			concurrent = 1
		}
		if concurrent > currentDungeonAltarMaximumConcurrentPhases {
			concurrent = currentDungeonAltarMaximumConcurrentPhases
		}
		if remaining := len(phases) - index; concurrent > remaining {
			concurrent = remaining
		}
		groupEnd := elapsed
		var groupTail *worldmap.BloodAltarMonster
		for concurrentIndex := 0; concurrentIndex < concurrent; concurrentIndex++ {
			phaseIndex := index + concurrentIndex
			phase := phases[phaseIndex]
			monster := monsters[int(phase.MonsterTemplateIndex)]
			batches := int(monster.BatchCount)
			if batches < 1 {
				batches = 1
			}
			interval := maxInt64(0, monster.SpawnIntervalMilliseconds)
			for batch := 0; batch < batches; batch++ {
				count64 := monster.BaseSpawnCount + monster.SpawnCountIncrement*int64(batch)
				if monster.MonsterCode <= 0 || count64 <= 0 {
					continue
				}
				if count64 > math.MaxUint16 {
					count64 = math.MaxUint16
				}
				delayMs := elapsed + maxInt64(0, phase.DelayMilliseconds) + interval*int64(batch)
				waves = append(waves, currentDungeonAltarScheduledWave{
					Delay:       time.Duration(delayMs) * time.Millisecond,
					RoundIndex:  roundIndex,
					GlobalRound: globalRound,
					PhaseIndex:  phaseIndex,
					BatchIndex:  batch,
					SpawnCount:  int(count64),
					Phase:       phase,
					Monster:     monster,
				})
			}
			candidateEnd := elapsed + maxInt64(0, phase.DelayMilliseconds) + interval*int64(batches-1)
			if groupTail == nil || candidateEnd >= groupEnd {
				groupEnd = candidateEnd
				copyMonster := monster
				groupTail = &copyMonster
			}
		}
		if groupTail != nil {
			tailDelay := groupTail.DurationMilliseconds - maxInt64(0, groupTail.SpawnIntervalMilliseconds)*int64(maxInt(1, int(groupTail.BatchCount)))
			elapsed = groupEnd + maxInt64(0, tailDelay)
			expireDelay := time.Duration(elapsed) * time.Millisecond
			for waveIndex := groupWaveStart; waveIndex < len(waves); waveIndex++ {
				waves[waveIndex].ExpireDelay = expireDelay
			}
		}
		index += concurrent
	}
	sort.SliceStable(waves, func(i, j int) bool {
		if waves[i].Delay != waves[j].Delay {
			return waves[i].Delay < waves[j].Delay
		}
		if waves[i].PhaseIndex != waves[j].PhaseIndex {
			return waves[i].PhaseIndex < waves[j].PhaseIndex
		}
		return waves[i].BatchIndex < waves[j].BatchIndex
	})
	if roundIndex == len(runtime.Rounds)-1 && len(waves) != 0 {
		waves[len(waves)-1].FinalRoomWave = true
		waves[len(waves)-1].FinalDungeonWave = globalRound == runtime.MaxRounds-1
	}
	return waves, nil
}

// roomMonsters is frozen on bind separately from Rounds so scheduled phase
// indices cannot observe a later world-map cache mutation.
func (runtime *currentDungeonAltarRuntime) monsters() []worldmap.BloodAltarMonster {
	return runtime.boundMonsters
}

func (runtime *currentDungeonAltarRuntime) reserveWave(scheduleGeneration uint64, waveIndex int) (currentDungeonAltarWave, error) {
	if runtime == nil || runtime.schedule == nil || runtime.schedule.Generation != scheduleGeneration ||
		waveIndex != runtime.nextWaveIndex || waveIndex < 0 || waveIndex >= len(runtime.schedule.Waves) {
		return currentDungeonAltarWave{}, errCurrentDungeonAltarRoomMismatch
	}
	scheduled := runtime.schedule.Waves[waveIndex]
	if runtime.nextSequence == 0 || uint64(runtime.nextSequence)+uint64(scheduled.SpawnCount)-1 > math.MaxUint16 {
		return currentDungeonAltarWave{}, errCurrentDungeonAltarSequenceExhausted
	}
	spawns := make([]currentDungeonAltarSpawn, 0, scheduled.SpawnCount)
	for index := 0; index < scheduled.SpawnCount; index++ {
		sequence := uint16(runtime.nextSequence + uint32(index))
		variant := byte(19)
		if index == 0 && scheduled.BatchIndex == 0 && runtime.crazyMonster != nil &&
			scheduled.RoundIndex == runtime.crazyMonster.RoundIndex &&
			scheduled.PhaseIndex == runtime.crazyMonster.PhaseIndex &&
			uint32(scheduled.Monster.MonsterCode) == runtime.crazyMonster.MonsterCode {
			variant = runtime.crazyMonster.Variant
		}
		spawns = append(spawns, currentDungeonAltarSpawn{
			Variant:         variant,
			Sequence:        sequence,
			MonsterCode:     uint32(scheduled.Monster.MonsterCode),
			MonsterType:     boolByteValue(scheduled.FinalDungeonWave, 3, 0),
			Level:           runtime.Level,
			Scale:           clampCurrentDungeonAltarUint16(int64(math.Round(scheduled.Phase.Scale * 1000))),
			X:               clampCurrentDungeonAltarUint16(scheduled.Monster.Position.X),
			Y:               clampCurrentDungeonAltarUint16(scheduled.Monster.Position.Y),
			Z:               clampCurrentDungeonAltarUint16(scheduled.Monster.Position.Z),
			RoomGeneration:  runtime.Room.Generation,
			RoundGeneration: scheduleGeneration,
			FinalRoomWave:   scheduled.FinalRoomWave,
		})
	}
	tail := uint16(0)
	if !scheduled.FinalRoomWave {
		// The current EXE uses this field only as a continuation marker. A
		// legitimate non-terminal wave may have a zero spawn interval, so wire
		// timing must not leak into the terminal/non-terminal decision.
		tail = 1
	}
	return currentDungeonAltarWave{Spawns: spawns, Tail: tail, ScheduleGen: scheduleGeneration, WaveIndex: waveIndex}, nil
}

func (runtime *currentDungeonAltarRuntime) commitWave(wave currentDungeonAltarWave) (currentDungeonAltarProgress, error) {
	if runtime == nil || runtime.schedule == nil || runtime.schedule.Generation != wave.ScheduleGen || wave.WaveIndex != runtime.nextWaveIndex {
		return currentDungeonAltarProgressNone, errCurrentDungeonAltarRoomMismatch
	}
	scheduled := runtime.schedule.Waves[wave.WaveIndex]
	for index, spawn := range wave.Spawns {
		if spawn.Sequence == 0 || spawn.RoomGeneration != runtime.Room.Generation || spawn.RoundGeneration != wave.ScheduleGen {
			return currentDungeonAltarProgressNone, errCurrentDungeonAltarSequenceUnknown
		}
		if spawn.FinalRoomWave != scheduled.FinalRoomWave {
			return currentDungeonAltarProgressNone, errCurrentDungeonAltarSequenceUnknown
		}
		if uint32(spawn.Sequence) != runtime.nextSequence+uint32(index) {
			return currentDungeonAltarProgressNone, errCurrentDungeonAltarSequenceUnknown
		}
		owner := currentDungeonAltarSequenceOwner{
			RoomGeneration:  runtime.Room.Generation,
			RoundGeneration: wave.ScheduleGen,
			RoomKey:         runtime.Room.Key,
			MonsterCode:     int64(spawn.MonsterCode),
			QuestDropSeed: currentDungeonAltarQuestDropSeed(
				spawn.Sequence,
				spawn.MonsterCode,
				runtime.Room.Generation,
				wave.ScheduleGen,
			),
			FinalRoomWave: spawn.FinalRoomWave,
		}
		runtime.activeSequences[spawn.Sequence] = owner
	}
	runtime.nextSequence += uint32(len(wave.Spawns))
	runtime.nextWaveIndex++
	if runtime.nextWaveIndex < len(runtime.schedule.Waves) || len(runtime.activeSequences) != 0 {
		return currentDungeonAltarProgressNone, nil
	}
	return runtime.finishRound()
}

func (runtime *currentDungeonAltarRuntime) applyDeaths(
	room runtimeDungeonRoomKey,
	sequences []uint16,
) (currentDungeonAltarDeathResult, error) {
	result := currentDungeonAltarDeathResult{}
	preview, err := runtime.previewDeaths(room, sequences)
	if err != nil {
		return result, err
	}
	result.Duplicate = preview.Duplicate
	acceptedFinalRoomWave := false
	for _, death := range preview.Active {
		delete(runtime.activeSequences, death.Sequence)
		runtime.retiredSequences[death.Sequence] = death.Owner
		result.Accepted++
		acceptedFinalRoomWave = acceptedFinalRoomWave || death.Owner.FinalRoomWave
	}
	if runtime.shouldReleaseEndlessOldWaves(acceptedFinalRoomWave) {
		for sequence, owner := range runtime.activeSequences {
			if owner.RoomGeneration != runtime.Room.Generation || owner.RoundGeneration != runtime.schedule.Generation || owner.FinalRoomWave {
				continue
			}
			delete(runtime.activeSequences, sequence)
			runtime.retiredSequences[sequence] = owner
			result.AutoReleased++
		}
	}
	if runtime.schedule != nil && runtime.nextWaveIndex >= len(runtime.schedule.Waves) && len(runtime.activeSequences) == 0 {
		progress, err := runtime.finishRound()
		if err != nil {
			return currentDungeonAltarDeathResult{}, err
		}
		result.Progress = progress
	}
	return result, nil
}

// expireWave retires only the sequences owned by one scheduled PVF phase
// group. The current client removes those actors at the [blood monster] /
// [ultimate monster] duration boundary without emitting op255 or op39. Keep
// the owners in retiredSequences so a death report racing that boundary is
// still idempotent, but never award quest drops for a client-side expiry.
func (runtime *currentDungeonAltarRuntime) expireWave(
	room currentDungeonAltarRoomIdentity,
	roundGeneration uint64,
	sequences []uint16,
) (int, currentDungeonAltarProgress, error) {
	if runtime == nil || runtime.Room != room || runtime.schedule == nil ||
		runtime.schedule.Generation != roundGeneration {
		return 0, currentDungeonAltarProgressNone, nil
	}
	expired := 0
	for _, sequence := range sequences {
		owner, active := runtime.activeSequences[sequence]
		if !active || owner.RoomKey != room.Key || owner.RoomGeneration != room.Generation ||
			owner.RoundGeneration != roundGeneration {
			continue
		}
		delete(runtime.activeSequences, sequence)
		runtime.retiredSequences[sequence] = owner
		expired++
	}
	if runtime.nextWaveIndex < len(runtime.schedule.Waves) || len(runtime.activeSequences) != 0 {
		return expired, currentDungeonAltarProgressNone, nil
	}
	progress, err := runtime.finishRound()
	if err != nil {
		return 0, currentDungeonAltarProgressNone, err
	}
	return expired, progress, nil
}

// The reference Endless Altar lifecycle treats the final scheduled wave as
// the room boundary. Once every server-owned sequence from that wave is dead,
// survivors from earlier overlapping waves no longer block progression.
// Implicit releases deliberately bypass quest-drop processing: only sequences
// explicitly accepted from op255 are authoritative monster deaths.
func (runtime *currentDungeonAltarRuntime) shouldReleaseEndlessOldWaves(acceptedFinalRoomWave bool) bool {
	if runtime == nil || runtime.Kind != worldmap.BloodAltarDungeonKindEndless || !acceptedFinalRoomWave ||
		runtime.schedule == nil || runtime.nextWaveIndex < len(runtime.schedule.Waves) {
		return false
	}
	for _, owner := range runtime.activeSequences {
		if owner.RoomGeneration == runtime.Room.Generation && owner.RoundGeneration == runtime.schedule.Generation && owner.FinalRoomWave {
			return false
		}
	}
	return true
}

func (runtime *currentDungeonAltarRuntime) previewDeaths(
	room runtimeDungeonRoomKey,
	sequences []uint16,
) (currentDungeonAltarDeathPreview, error) {
	preview := currentDungeonAltarDeathPreview{Active: make([]currentDungeonAltarDeathSequence, 0, len(sequences))}
	if runtime == nil || runtime.Room.Key != room {
		return currentDungeonAltarDeathPreview{}, errCurrentDungeonAltarRoomMismatch
	}
	for _, sequence := range sequences {
		if owner, ok := runtime.activeSequences[sequence]; ok {
			if owner.RoomKey != room || owner.RoomGeneration != runtime.Room.Generation {
				return currentDungeonAltarDeathPreview{}, fmt.Errorf("%w: sequence=%d", errCurrentDungeonAltarSequenceCrossRoom, sequence)
			}
			if owner.MonsterCode <= 0 || owner.QuestDropSeed == 0 {
				return currentDungeonAltarDeathPreview{}, fmt.Errorf("%w: sequence=%d monster=%d drop_seed=%d", errCurrentDungeonAltarSequenceUnknown, sequence, owner.MonsterCode, owner.QuestDropSeed)
			}
			preview.Active = append(preview.Active, currentDungeonAltarDeathSequence{Sequence: sequence, Owner: owner})
			continue
		}
		if owner, ok := runtime.retiredSequences[sequence]; ok {
			if owner.RoomKey != room || owner.RoomGeneration != runtime.Room.Generation {
				return currentDungeonAltarDeathPreview{}, fmt.Errorf("%w: sequence=%d", errCurrentDungeonAltarSequenceCrossRoom, sequence)
			}
			preview.Duplicate++
			continue
		}
		return currentDungeonAltarDeathPreview{}, fmt.Errorf("%w: sequence=%d", errCurrentDungeonAltarSequenceUnknown, sequence)
	}
	return preview, nil
}

func (runtime *currentDungeonAltarRuntime) finishRound() (currentDungeonAltarProgress, error) {
	if runtime == nil || runtime.schedule == nil || runtime.nextWaveIndex < len(runtime.schedule.Waves) || len(runtime.activeSequences) != 0 {
		return currentDungeonAltarProgressNone, nil
	}
	if runtime.Kind == worldmap.BloodAltarDungeonKindUltimate &&
		(runtime.schedule.Difficulty == worldmap.BloodAltarDifficultyA || runtime.schedule.Difficulty == worldmap.BloodAltarDifficultyB) {
		runtime.completedDifficulties = append(runtime.completedDifficulties, runtime.schedule.Difficulty)
	}
	runtime.completedRounds++
	runtime.nextRoundIndex++
	runtime.schedule = nil
	runtime.nextWaveIndex = 0
	if runtime.completedRounds > runtime.MaxRounds {
		return currentDungeonAltarProgressNone, errCurrentDungeonAltarDefinitionInvalid
	}
	if runtime.nextRoundIndex >= len(runtime.Rounds) || runtime.completedRounds == runtime.MaxRounds {
		runtime.completedRooms[runtime.Room.Key] = struct{}{}
		if runtime.completedRounds == runtime.MaxRounds {
			runtime.progress = currentDungeonAltarProgressDungeonComplete
			return runtime.progress, nil
		}
		runtime.progress = currentDungeonAltarProgressRoomComplete
		return runtime.progress, nil
	}
	if runtime.Kind == worldmap.BloodAltarDungeonKindUltimate {
		if runtime.nextRoundIndex == len(runtime.Rounds)-1 {
			runtime.progress = currentDungeonAltarProgressFinalRoundDelay
			return runtime.progress, nil
		}
		runtime.progress = currentDungeonAltarProgressAwaitingDifficulty
		runtime.difficultyVersion = nextCurrentDungeonAltarGeneration(runtime.difficultyVersion)
		return runtime.progress, nil
	}
	runtime.progress = currentDungeonAltarProgressRoundActive
	return runtime.progress, nil
}

func (runtime *currentDungeonAltarRuntime) resolveDifficulty(
	roomGeneration uint64,
	promptVersion uint64,
	difficulty worldmap.BloodAltarDifficulty,
) bool {
	if runtime == nil || runtime.Kind != worldmap.BloodAltarDungeonKindUltimate ||
		runtime.progress != currentDungeonAltarProgressAwaitingDifficulty ||
		runtime.Room.Generation != roomGeneration || runtime.difficultyVersion != promptVersion ||
		(difficulty != worldmap.BloodAltarDifficultyA && difficulty != worldmap.BloodAltarDifficultyB) {
		return false
	}
	runtime.difficulty = difficulty
	runtime.progress = currentDungeonAltarProgressRoundActive
	return true
}

func (runtime *currentDungeonAltarRuntime) timerNames() []string {
	if runtime == nil {
		return nil
	}
	names := []string{
		runtime.waveTimerName,
		runtime.difficultyTimerName,
		runtime.finalTimerName,
		runtime.questDropRetryTimerName,
		runtime.roomTransitionTimerName,
		runtime.settlementRetryTimerName,
	}
	for name := range runtime.expiryTimerNames {
		names = append(names, name)
	}
	return names
}

func nextCurrentDungeonAltarGeneration(previous uint64) uint64 {
	if previous == math.MaxUint64 {
		return 1
	}
	return previous + 1
}

func currentDungeonAltarQuestDropSeed(
	sequence uint16,
	monsterCode uint32,
	roomGeneration uint64,
	roundGeneration uint64,
) uint32 {
	seed := uint32(sequence)<<16 ^ monsterCode ^ uint32(roomGeneration) ^ uint32(roomGeneration>>32) ^
		uint32(roundGeneration*0x9e3779b9) ^ uint32(roundGeneration>>32)
	if seed == 0 {
		return 1
	}
	return seed
}

func clampCurrentDungeonAltarUint16(value int64) uint16 {
	if value <= 0 {
		return 0
	}
	if value > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(value)
}

func boolByteValue(condition bool, yes byte, no byte) byte {
	if condition {
		return yes
	}
	return no
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
