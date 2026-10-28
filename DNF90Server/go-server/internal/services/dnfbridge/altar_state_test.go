package dnfbridge

import (
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonEndlessAltarOwnsFiveRoundsPerRoomAndFifteenGlobally(t *testing.T) {
	dungeon := currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15)
	runtime, err := newCurrentDungeonAltarRuntime(dungeon, currentDungeonEndlessAltarScene(0, 17020))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint16]struct{})
	var firstRoomSequence uint16
	for room := 0; room < 3; room++ {
		if room != 0 {
			if err := runtime.bindRoom(currentDungeonEndlessAltarScene(room, int64(17020+room))); err != nil {
				t.Fatalf("bind room %d: %v", room, err)
			}
		}
		schedule, accepted, err := runtime.acceptReady(runtime.Room.Key, time.Unix(int64(room+1), 0))
		if err != nil || !accepted || schedule == nil {
			t.Fatalf("room %d ready accepted=%t schedule=%+v err=%v", room, accepted, schedule, err)
		}
		if duplicate, accepted, err := runtime.acceptReady(runtime.Room.Key, time.Now()); err != nil || accepted || duplicate != nil {
			t.Fatalf("room %d duplicate ready accepted=%t schedule=%+v err=%v", room, accepted, duplicate, err)
		}
		for localRound := 0; localRound < 5; localRound++ {
			if schedule.GlobalRound != room*5+localRound || schedule.RoundIndex != localRound {
				t.Fatalf("room=%d local=%d schedule global=%d local=%d", room, localRound, schedule.GlobalRound, schedule.RoundIndex)
			}
			progress, sequence := completeCurrentDungeonAltarTestRound(t, runtime, schedule)
			if sequence == 0 {
				t.Fatal("server allocated sequence zero")
			}
			if _, duplicate := seen[sequence]; duplicate {
				t.Fatalf("sequence %d was reused", sequence)
			}
			seen[sequence] = struct{}{}
			if firstRoomSequence == 0 {
				firstRoomSequence = sequence
			}
			lastLocal := localRound == 4
			lastDungeon := room == 2 && lastLocal
			switch {
			case lastDungeon && progress != currentDungeonAltarProgressDungeonComplete:
				t.Fatalf("final progress=%d", progress)
			case lastLocal && !lastDungeon && progress != currentDungeonAltarProgressRoomComplete:
				t.Fatalf("room %d final progress=%d", room, progress)
			case !lastLocal && progress != currentDungeonAltarProgressRoundActive:
				t.Fatalf("room=%d local=%d progress=%d", room, localRound, progress)
			}
			if !lastLocal {
				schedule, err = runtime.beginRound(time.Unix(int64(room*5+localRound+2), 0))
				if err != nil || schedule == nil {
					t.Fatalf("begin next room=%d local=%d schedule=%+v err=%v", room, localRound, schedule, err)
				}
			}
		}
		if room == 0 {
			nextScene := currentDungeonEndlessAltarScene(1, 17021)
			if err := runtime.previewBindRoom(nextScene); err != nil {
				t.Fatalf("preview next room: %v", err)
			}
			if _, err := runtime.applyDeaths(runtimeDungeonRoomKeyFromScene(nextScene), []uint16{firstRoomSequence}); !errors.Is(err, errCurrentDungeonAltarRoomMismatch) {
				t.Fatalf("unbound cross-room death error=%v", err)
			}
		}
		if room == 1 {
			completedRounds := runtime.completedRounds
			currentRoom := runtime.Room
			completedScene := currentDungeonEndlessAltarScene(0, 17020)
			if err := runtime.previewBindRoom(completedScene); !errors.Is(err, errCurrentDungeonAltarRoomComplete) {
				t.Fatalf("completed-room preview error=%v", err)
			}
			if err := runtime.bindRoom(completedScene); !errors.Is(err, errCurrentDungeonAltarRoomComplete) {
				t.Fatalf("completed-room bind error=%v", err)
			}
			if runtime.completedRounds != completedRounds || runtime.Room != currentRoom {
				t.Fatalf("completed-room replay mutated state: completed=%d room=%+v", runtime.completedRounds, runtime.Room)
			}
		}
	}
	if runtime.completedRounds != 15 || len(runtime.completedRooms) != 3 || runtime.progress != currentDungeonAltarProgressDungeonComplete {
		t.Fatalf("completed=%d rooms=%d progress=%d", runtime.completedRounds, len(runtime.completedRooms), runtime.progress)
	}
	if _, err := runtime.applyDeaths(runtime.Room.Key, []uint16{firstRoomSequence}); !errors.Is(err, errCurrentDungeonAltarSequenceCrossRoom) {
		t.Fatalf("old-room sequence error=%v", err)
	}
}

func TestCurrentDungeonUltimateAltarDifficultySelectionAndTimeoutRace(t *testing.T) {
	dungeon := currentDungeonAltarTestDungeon(11007, worldmap.BloodAltarDungeonKindUltimate, 7)
	runtime, err := newCurrentDungeonAltarRuntime(dungeon, currentDungeonUltimateAltarScene())
	if err != nil {
		t.Fatal(err)
	}
	schedule, accepted, err := runtime.acceptReady(runtime.Room.Key, time.Unix(1, 0))
	if err != nil || !accepted || schedule.Difficulty != worldmap.BloodAltarDifficultyA {
		t.Fatalf("first schedule=%+v accepted=%t err=%v", schedule, accepted, err)
	}
	for round := 0; round < 7; round++ {
		progress, _ := completeCurrentDungeonAltarTestRound(t, runtime, schedule)
		if round == 6 {
			if progress != currentDungeonAltarProgressDungeonComplete {
				t.Fatalf("final progress=%d", progress)
			}
			break
		}
		if round == 5 {
			if progress != currentDungeonAltarProgressFinalRoundDelay {
				t.Fatalf("penultimate progress=%d", progress)
			}
			runtime.progress = currentDungeonAltarProgressRoundActive
			schedule, err = runtime.beginRound(time.Unix(int64(round+2), 0))
			if err != nil || schedule == nil || schedule.Difficulty != worldmap.BloodAltarDifficultyA {
				t.Fatalf("final fixed-A schedule=%+v err=%v", schedule, err)
			}
			continue
		}
		if progress != currentDungeonAltarProgressAwaitingDifficulty {
			t.Fatalf("round=%d progress=%d", round, progress)
		}
		roomGeneration, promptVersion := runtime.Room.Generation, runtime.difficultyVersion
		chosen := worldmap.BloodAltarDifficultyB
		if round%2 != 0 {
			chosen = worldmap.BloodAltarDifficultyA
		}
		if !runtime.resolveDifficulty(roomGeneration, promptVersion, chosen) {
			t.Fatalf("round=%d first difficulty lost", round)
		}
		other := worldmap.BloodAltarDifficultyA
		if chosen == other {
			other = worldmap.BloodAltarDifficultyB
		}
		if runtime.resolveDifficulty(roomGeneration, promptVersion, other) {
			t.Fatalf("round=%d stale difficulty won race", round)
		}
		schedule, err = runtime.beginRound(time.Unix(int64(round+2), 0))
		if err != nil || schedule == nil || schedule.Difficulty != chosen {
			t.Fatalf("round=%d schedule=%+v chosen=%q err=%v", round, schedule, chosen, err)
		}
	}
	if runtime.completedRounds != 7 || runtime.progress != currentDungeonAltarProgressDungeonComplete {
		t.Fatalf("completed=%d progress=%d", runtime.completedRounds, runtime.progress)
	}
	if len(runtime.completedDifficulties) != 7 || runtime.completedDifficulties[0] != worldmap.BloodAltarDifficultyA ||
		runtime.completedDifficulties[6] != worldmap.BloodAltarDifficultyA {
		t.Fatalf("completed difficulties=%v", runtime.completedDifficulties)
	}
}

func TestCurrentDungeonEndlessAltarCrazyMonsterSelectionAndVariant(t *testing.T) {
	dungeon := currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15)
	scene := currentDungeonEndlessAltarScene(2, 17022)
	scene.Map.Map.Type = "[boss]"
	runtime, err := newCurrentDungeonAltarRuntime(dungeon, scene)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.configureRunSeed(0x12345678); err != nil {
		t.Fatal(err)
	}
	if runtime.crazyMonster == nil || runtime.crazyMonster.RoundIndex < 0 || runtime.crazyMonster.RoundIndex >= 4 ||
		runtime.crazyMonster.PhaseIndex != 0 || runtime.crazyMonster.MonsterCode != 61010 ||
		runtime.crazyMonster.Variant > 18 {
		t.Fatalf("boss-map crazy monster=%+v", runtime.crazyMonster)
	}

	runtime.nextRoundIndex = runtime.crazyMonster.RoundIndex
	runtime.completedRounds = runtime.crazyMonster.RoundIndex
	schedule, err := runtime.beginRound(time.Unix(1, 0))
	if err != nil || schedule == nil || len(schedule.Waves) == 0 {
		t.Fatalf("schedule=%+v err=%v", schedule, err)
	}
	wave, err := runtime.reserveWave(schedule.Generation, 0)
	if err != nil || len(wave.Spawns) == 0 || wave.Spawns[0].Variant != runtime.crazyMonster.Variant {
		t.Fatalf("crazy wave=%+v err=%v crazy=%+v", wave, err, runtime.crazyMonster)
	}

	ultimate, err := newCurrentDungeonAltarRuntime(
		currentDungeonAltarTestDungeon(11007, worldmap.BloodAltarDungeonKindUltimate, 7),
		currentDungeonUltimateAltarScene(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ultimate.configureRunSeed(0x12345678); err != nil || ultimate.crazyMonster != nil {
		t.Fatalf("ultimate crazy=%+v err=%v", ultimate.crazyMonster, err)
	}
}

func TestCurrentDungeonAltarRejectsNonContiguousOrWrongRoundCount(t *testing.T) {
	tests := []struct {
		name    string
		dungeon worldmap.Dungeon
		scene   worldmap.DungeonRoomScene
	}{
		{
			name:    "endless_missing_round",
			dungeon: currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
			scene: func() worldmap.DungeonRoomScene {
				scene := currentDungeonEndlessAltarScene(0, 17020)
				scene.Map.Map.BloodPhases = scene.Map.Map.BloodPhases[:4]
				return scene
			}(),
		},
		{
			name:    "endless_round_gap",
			dungeon: currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
			scene: func() worldmap.DungeonRoomScene {
				scene := currentDungeonEndlessAltarScene(0, 17020)
				scene.Map.Map.BloodPhases[4].Round = 5
				return scene
			}(),
		},
		{
			name:    "ultimate_missing_round",
			dungeon: currentDungeonAltarTestDungeon(11007, worldmap.BloodAltarDungeonKindUltimate, 7),
			scene: func() worldmap.DungeonRoomScene {
				scene := currentDungeonUltimateAltarScene()
				scene.Map.Map.UltimatePhases = scene.Map.Map.UltimatePhases[:12]
				return scene
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newCurrentDungeonAltarRuntime(test.dungeon, test.scene); !errors.Is(err, errCurrentDungeonAltarDefinitionInvalid) {
				t.Fatalf("definition error=%v", err)
			}
		})
	}
}

func TestCurrentDungeonAltarKnownIDsCannotDowngradeToOrdinaryDungeon(t *testing.T) {
	for _, dungeonID := range []int64{11006, 11007} {
		dungeon := currentDungeonAltarTestDungeon(dungeonID, 0, 0)
		runtime, err := newCurrentDungeonAltarRuntime(dungeon, worldmap.DungeonRoomScene{})
		if runtime != nil || !errors.Is(err, errCurrentDungeonAltarDefinitionInvalid) {
			t.Fatalf("dungeon=%d runtime=%+v error=%v", dungeonID, runtime, err)
		}
	}
	runtime, err := newCurrentDungeonAltarRuntime(worldmap.Dungeon{ID: 100}, worldmap.DungeonRoomScene{})
	if err != nil || runtime != nil {
		t.Fatalf("ordinary dungeon runtime=%+v error=%v", runtime, err)
	}
}

func TestCurrentDungeonAltarRejectsOrdinaryBlockingHostiles(t *testing.T) {
	scene := currentDungeonEndlessAltarScene(0, 17020)
	scene.BlockingHostiles = []worldmap.HostileReference{{Kind: worldmap.HostileMonster, Index: 0}}
	runtime, err := newCurrentDungeonAltarRuntime(
		currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
		scene,
	)
	if runtime != nil || !errors.Is(err, errCurrentDungeonAltarDefinitionInvalid) {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
}

func TestCurrentDungeonAltarEntryIsSoloOnlyWithoutChangingOrdinaryDungeonAdmission(t *testing.T) {
	character := dnfrepo.CharacterRecord{Level: 70, Stats: map[string]int64{"fatigue": 100}}
	party := &gameSession{}
	party.party.state = alignedcmd.PartyState{Members: []alignedcmd.PartyMemberState{
		{UserID: 99},
		{UserID: 100},
	}}
	altar := currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15)
	if err := validateDungeonEntry(character, altar, party, false); !errors.Is(err, dnfdungeon.ErrEntryPartyLimit) {
		t.Fatalf("party altar error=%v", err)
	} else if reason, reply := selectDungeonFailureReason(err); !reply || reason != currentSelectDungeonPreparationRejectedReason {
		t.Fatalf("party altar rejection reason=%d reply=%t", reason, reply)
	}

	solo := &gameSession{}
	solo.party.state = alignedcmd.PartyState{Members: []alignedcmd.PartyMemberState{{UserID: 99}}}
	if err := validateDungeonEntry(character, altar, solo, false); err != nil {
		t.Fatalf("solo altar rejected: %v", err)
	}

	ordinary := worldmap.Dungeon{ID: 700}
	if err := validateDungeonEntry(character, ordinary, party, false); err != nil {
		t.Fatalf("ordinary dungeon admission changed: %v", err)
	}
}

func TestCurrentDungeonAltarPracticeEntryIsRejectedWithoutChangingOrdinaryPractice(t *testing.T) {
	for _, dungeon := range []worldmap.Dungeon{
		currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
		currentDungeonAltarTestDungeon(11007, worldmap.BloodAltarDungeonKindUltimate, 7),
		{ID: 99999, BloodAltarKind: worldmap.BloodAltarDungeonKindEndless},
	} {
		err := validatePracticeDungeonRoute(dungeon)
		if !errors.Is(err, errDungeonEntryModeUnsupported) {
			t.Fatalf("altar dungeon=%d kind=%d practice error=%v", dungeon.ID, dungeon.BloodAltarKind, err)
		}
		if reason, reply := selectDungeonFailureReason(err); !reply || reason != currentSelectDungeonPreparationRejectedReason {
			t.Fatalf("altar dungeon=%d practice rejection reason=%d reply=%t", dungeon.ID, reason, reply)
		}
	}
	if err := validatePracticeDungeonRoute(worldmap.Dungeon{ID: 700}); err != nil {
		t.Fatalf("ordinary practice route changed: %v", err)
	}
}

func TestCurrentDungeonAltarDeathIsAtomicAndDuplicateReportIsIdempotent(t *testing.T) {
	runtime, err := newCurrentDungeonAltarRuntime(
		currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
		currentDungeonEndlessAltarScene(0, 17020),
	)
	if err != nil {
		t.Fatal(err)
	}
	schedule, _, err := runtime.acceptReady(runtime.Room.Key, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	wave, err := runtime.reserveWave(schedule.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.commitWave(wave); err != nil {
		t.Fatal(err)
	}
	sequence := wave.Spawns[0].Sequence
	if _, err := runtime.applyDeaths(runtime.Room.Key, []uint16{sequence, 65000}); !errors.Is(err, errCurrentDungeonAltarSequenceUnknown) {
		t.Fatalf("mixed death error=%v", err)
	}
	if _, active := runtime.activeSequences[sequence]; !active {
		t.Fatal("partially applied mixed valid/unknown death report")
	}
	result, err := runtime.applyDeaths(runtime.Room.Key, []uint16{sequence})
	if err != nil || result.Accepted != 1 {
		t.Fatalf("accepted result=%+v err=%v", result, err)
	}
	duplicate, err := runtime.applyDeaths(runtime.Room.Key, []uint16{sequence})
	if err != nil || duplicate.Accepted != 0 || duplicate.Duplicate != 1 || runtime.completedRounds != 1 {
		t.Fatalf("duplicate result=%+v completed=%d err=%v", duplicate, runtime.completedRounds, err)
	}
}

func TestCurrentDungeonAltarPVFDurationExpiresUnreportedActorsAtPhaseBoundary(t *testing.T) {
	scene := currentDungeonEndlessAltarScene(0, 17020)
	scene.Map.Map.BloodMonsters = []worldmap.BloodAltarMonster{
		{
			MonsterCode:               61171,
			Position:                  worldmap.Point3{X: 295, Y: 184},
			DurationMilliseconds:      3000,
			SpawnIntervalMilliseconds: 1000,
			BaseSpawnCount:            1,
			SpawnCountIncrement:       1,
			BatchCount:                2,
		},
		{
			MonsterCode:          61172,
			Position:             worldmap.Point3{X: 300, Y: 184},
			DurationMilliseconds: 1000,
			BaseSpawnCount:       1,
			BatchCount:           1,
		},
	}
	scene.Map.Map.BloodPhases = []worldmap.BloodAltarPhase{
		{Round: 0, MonsterTemplateIndex: 0, DelayMilliseconds: 1000, Scale: 1.2},
		{Round: 0, MonsterTemplateIndex: 1, DelayMilliseconds: 1000, Scale: 1.2},
		{Round: 1, MonsterTemplateIndex: 1, Scale: 1},
		{Round: 2, MonsterTemplateIndex: 1, Scale: 1},
		{Round: 3, MonsterTemplateIndex: 1, Scale: 1},
		{Round: 4, MonsterTemplateIndex: 1, Scale: 1},
	}
	runtime, err := newCurrentDungeonAltarRuntime(
		currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
		scene,
	)
	if err != nil {
		t.Fatal(err)
	}
	schedule, accepted, err := runtime.acceptReady(runtime.Room.Key, time.Unix(1, 0))
	if err != nil || !accepted || len(schedule.Waves) != 3 {
		t.Fatalf("schedule=%+v accepted=%t err=%v", schedule, accepted, err)
	}
	if schedule.Waves[0].Delay != time.Second || schedule.Waves[1].Delay != 2*time.Second ||
		schedule.Waves[0].ExpireDelay != 3*time.Second || schedule.Waves[1].ExpireDelay != 3*time.Second ||
		schedule.Waves[2].Delay != 4*time.Second || schedule.Waves[2].ExpireDelay != 5*time.Second {
		t.Fatalf("unexpected duration schedule=%+v", schedule.Waves)
	}

	first, err := runtime.reserveWave(schedule.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.commitWave(first); err != nil {
		t.Fatal(err)
	}
	second, err := runtime.reserveWave(schedule.Generation, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Spawns) != 1 || len(second.Spawns) != 2 {
		t.Fatalf("phase spawn counts=%d/%d", len(first.Spawns), len(second.Spawns))
	}
	if _, err := runtime.commitWave(second); err != nil {
		t.Fatal(err)
	}
	phaseSequences := []uint16{first.Spawns[0].Sequence, second.Spawns[0].Sequence, second.Spawns[1].Sequence}
	expired, progress, err := runtime.expireWave(runtime.Room, schedule.Generation, phaseSequences)
	if err != nil || expired != 3 || progress != currentDungeonAltarProgressNone || len(runtime.activeSequences) != 0 {
		t.Fatalf("first phase expiry expired=%d progress=%d active=%d err=%v", expired, progress, len(runtime.activeSequences), err)
	}
	duplicate, err := runtime.applyDeaths(runtime.Room.Key, []uint16{phaseSequences[0]})
	if err != nil || duplicate.Accepted != 0 || duplicate.Duplicate != 1 {
		t.Fatalf("late death after expiry result=%+v err=%v", duplicate, err)
	}

	third, err := runtime.reserveWave(schedule.Generation, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.commitWave(third); err != nil {
		t.Fatal(err)
	}
	expired, progress, err = runtime.expireWave(runtime.Room, schedule.Generation, []uint16{third.Spawns[0].Sequence})
	if err != nil || expired != 1 || progress != currentDungeonAltarProgressRoundActive || runtime.completedRounds != 1 {
		t.Fatalf("second phase expiry expired=%d progress=%d completed=%d err=%v", expired, progress, runtime.completedRounds, err)
	}
}

func TestCurrentDungeonEndlessAltarFinalRoomWaveReleasesEarlierSurvivors(t *testing.T) {
	scene := currentDungeonEndlessAltarScene(0, 17020)
	scene.Map.Map.BloodMonsters = append(scene.Map.Map.BloodMonsters, worldmap.BloodAltarMonster{
		MonsterCode:    61011,
		Position:       worldmap.Point3{X: 200, Y: 200},
		BaseSpawnCount: 2,
		BatchCount:     1,
	})
	scene.Map.Map.BloodPhases = append(scene.Map.Map.BloodPhases, worldmap.BloodAltarPhase{
		Round: 4, MonsterTemplateIndex: 1, Scale: 1,
	})
	runtime, err := newCurrentDungeonAltarRuntime(
		currentDungeonAltarTestDungeon(11006, worldmap.BloodAltarDungeonKindEndless, 15),
		scene,
	)
	if err != nil {
		t.Fatal(err)
	}
	schedule, _, err := runtime.acceptReady(runtime.Room.Key, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 4; round++ {
		progress, _ := completeCurrentDungeonAltarTestRound(t, runtime, schedule)
		if progress != currentDungeonAltarProgressRoundActive {
			t.Fatalf("round=%d progress=%d", round, progress)
		}
		schedule, err = runtime.beginRound(time.Unix(int64(round+2), 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(schedule.Waves) != 2 {
		t.Fatalf("final local round waves=%d", len(schedule.Waves))
	}
	oldWave, err := runtime.reserveWave(schedule.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.commitWave(oldWave); err != nil {
		t.Fatal(err)
	}
	finalWave, err := runtime.reserveWave(schedule.Generation, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalWave.Spawns) != 2 || !finalWave.Spawns[0].FinalRoomWave || !finalWave.Spawns[1].FinalRoomWave {
		t.Fatalf("final wave=%+v", finalWave)
	}
	if _, err := runtime.commitWave(finalWave); err != nil {
		t.Fatal(err)
	}

	firstFinal := finalWave.Spawns[0].Sequence
	partial, err := runtime.applyDeaths(runtime.Room.Key, []uint16{firstFinal})
	if err != nil || partial.Accepted != 1 || partial.AutoReleased != 0 || partial.Progress != currentDungeonAltarProgressNone {
		t.Fatalf("partial final-wave death result=%+v err=%v", partial, err)
	}
	if _, active := runtime.activeSequences[oldWave.Spawns[0].Sequence]; !active {
		t.Fatal("earlier wave released before every final-wave sequence died")
	}

	lastFinal := finalWave.Spawns[1].Sequence
	completed, err := runtime.applyDeaths(runtime.Room.Key, []uint16{lastFinal})
	if err != nil || completed.Accepted != 1 || completed.AutoReleased != 1 || completed.Progress != currentDungeonAltarProgressRoomComplete {
		t.Fatalf("last final-wave death result=%+v err=%v", completed, err)
	}
	oldOwner, retired := runtime.retiredSequences[oldWave.Spawns[0].Sequence]
	if !retired || oldOwner.FinalRoomWave {
		t.Fatalf("earlier survivor retirement owner=%+v retired=%t", oldOwner, retired)
	}
	if runtime.completedRounds != 5 || len(runtime.activeSequences) != 0 {
		t.Fatalf("completed rounds=%d active=%d", runtime.completedRounds, len(runtime.activeSequences))
	}
}

func completeCurrentDungeonAltarTestRound(
	t *testing.T,
	runtime *currentDungeonAltarRuntime,
	schedule *currentDungeonAltarRoundSchedule,
) (currentDungeonAltarProgress, uint16) {
	t.Helper()
	if len(schedule.Waves) != 1 {
		t.Fatalf("test schedule waves=%d", len(schedule.Waves))
	}
	wave, err := runtime.reserveWave(schedule.Generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(wave.Spawns) != 1 {
		t.Fatalf("test wave spawns=%d", len(wave.Spawns))
	}
	if progress, err := runtime.commitWave(wave); err != nil || progress != currentDungeonAltarProgressNone {
		t.Fatalf("commit progress=%d err=%v", progress, err)
	}
	result, err := runtime.applyDeaths(runtime.Room.Key, []uint16{wave.Spawns[0].Sequence})
	if err != nil {
		t.Fatal(err)
	}
	return result.Progress, wave.Spawns[0].Sequence
}

func currentDungeonAltarTestDungeon(
	id int64,
	kind worldmap.BloodAltarDungeonKind,
	maxRounds int64,
) worldmap.Dungeon {
	return worldmap.Dungeon{
		ID:             id,
		BloodAltarKind: kind,
		BloodMaxRounds: maxRounds,
		Metadata: worldmap.DungeonMetadata{
			BasisLevel: worldmap.OptionalInt{Value: 70, Set: true},
		},
	}
}

func currentDungeonEndlessAltarScene(room int, mapID int64) worldmap.DungeonRoomScene {
	phases := make([]worldmap.BloodAltarPhase, 5)
	for index := range phases {
		phases[index] = worldmap.BloodAltarPhase{Round: int64(index), MonsterTemplateIndex: 0, Scale: 1}
	}
	return worldmap.DungeonRoomScene{
		Coordinate: worldmap.RoomCoordinate{X: int64(room), Y: 0},
		Map: worldmap.ResolvedMap{Map: worldmap.Map{
			ID: mapID,
			BloodMonsters: []worldmap.BloodAltarMonster{{
				MonsterCode: 61010, Position: worldmap.Point3{X: 100, Y: 200}, BaseSpawnCount: 1, BatchCount: 1,
			}},
			BloodPhases: phases,
		}},
	}
}

func currentDungeonUltimateAltarScene() worldmap.DungeonRoomScene {
	phases := make([]worldmap.BloodAltarPhase, 0, 14)
	for round := 0; round < 7; round++ {
		phases = append(phases,
			worldmap.BloodAltarPhase{Round: int64(round), MonsterTemplateIndex: 0, Scale: 1, Difficulty: worldmap.BloodAltarDifficultyA},
			worldmap.BloodAltarPhase{Round: int64(round), MonsterTemplateIndex: 0, Scale: 1, Difficulty: worldmap.BloodAltarDifficultyB},
		)
	}
	return worldmap.DungeonRoomScene{
		Coordinate: worldmap.RoomCoordinate{X: 0, Y: 0},
		Map: worldmap.ResolvedMap{Map: worldmap.Map{
			ID: 17030,
			UltimateMonsters: []worldmap.BloodAltarMonster{{
				MonsterCode: 56017, Position: worldmap.Point3{X: 440, Y: 222}, BaseSpawnCount: 1, BatchCount: 1,
			}},
			UltimatePhases: phases,
		}},
	}
}
