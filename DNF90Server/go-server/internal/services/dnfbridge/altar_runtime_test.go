package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfmonster "longheng.io/server/internal/modules/dnf/monster"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonAltarEntryUsesOp211AndWaitsForReady(t *testing.T) {
	_, _, runtime, scene, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatal(err)
	}
	if currentDungeonStartMapNotificationForRuntime(runtime) != currentDungeonAltarStartMapMsgID {
		t.Fatalf("altar start msg=%d", currentDungeonStartMapNotificationForRuntime(runtime))
	}
	if len(packets.StartMap) != 15 || packets.StartMap[7] != 1 || binary.LittleEndian.Uint32(packets.StartMap[8:12]) != 17030 {
		t.Fatalf("altar initial op211=%x", packets.StartMap)
	}
	if runtime.altar.progress != currentDungeonAltarProgressAwaitingReady || runtime.altar.readyAccepted || runtime.altar.schedule != nil {
		t.Fatalf("altar did not wait for op258: %+v", runtime.altar)
	}
	ordinary := *runtime
	ordinary.altar = nil
	if currentDungeonStartMapNotificationForRuntime(&ordinary) != currentDungeonStartNotification {
		t.Fatal("ordinary dungeon start notification changed")
	}
	target := scene
	target.Coordinate = worldmap.RoomCoordinate{X: 1, Y: 0}
	target.Map.Map.ID = 17031
	plan := currentDungeonMovePlan{Target: target, TargetRoom: &runtimeDungeonRoom{}, Seed: 0x87654321}
	moveBody, err := buildCurrentDungeonMoveStartMapBody(runtime, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(moveBody) != 15 || moveBody[7] != 1 || binary.LittleEndian.Uint32(moveBody[8:12]) != 17031 {
		t.Fatalf("altar new-room op211=%x", moveBody)
	}
	plan.Revisit = true
	revisitBody, err := buildCurrentDungeonMoveStartMapBody(runtime, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisitBody) != 8 || revisitBody[7] != 0 {
		t.Fatalf("altar revisit op211=%x", revisitBody)
	}
}

func TestCurrentDungeonEndlessAltarAnnouncesCrazyMonsterAfterRoundInterval(t *testing.T) {
	service, session, runtime := prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	connection, ok := session.conn.(*bufferConn)
	if !ok {
		t.Fatalf("connection=%T", session.conn)
	}
	if runtime.altar.crazyMonster == nil {
		t.Fatal("Endless Altar crazy monster was not selected")
	}
	runtime.altar.crazyMonster.RoundIndex = 0
	runtime.altar.crazyMonster.PhaseIndex = 0
	runtime.altar.crazyMonster.MonsterCode = 61010
	runtime.altar.crazyMonster.Variant = 18

	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	interval, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if interval.Header.Classification != 0 || interval.Header.MsgID != currentDungeonAltarRoundIntervalMsgID {
		t.Fatalf("first packet=%+v body=%x", interval.Header, interval.Body)
	}
	crazy, rest := splitGameServerUpperPacket(t, rest)
	if crazy.Header.Classification != 0 || crazy.Header.MsgID != currentDungeonAltarCrazyMonsterMsgID ||
		!bytes.Equal(crazy.Body, []byte{0x52, 0xee, 0, 0, 18}) || len(rest) != 0 {
		t.Fatalf("crazy packet=%+v body=%x rest=%x", crazy.Header, crazy.Body, rest)
	}
	if scheduled, _, active := queue.counts(); scheduled != 1 || active != 1 {
		t.Fatalf("wave timers scheduled=%d active=%d", scheduled, active)
	}
}

func TestCurrentDungeonAltarReadyDeathAndDifficultyRace(t *testing.T) {
	service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue

	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	interval, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if interval.Header.Classification != 0 || interval.Header.MsgID != currentDungeonAltarRoundIntervalMsgID ||
		!bytes.Equal(interval.Body, []byte{1, 0, 0, 0, 0}) || len(rest) != 0 {
		t.Fatalf("ready interval header=%+v body=%x rest=%x", interval.Header, interval.Body, rest)
	}
	if scheduled, _, active := queue.counts(); scheduled != 1 || active != 1 {
		t.Fatalf("ready timers scheduled=%d active=%d", scheduled, active)
	}
	bytesAfterFirstReady := connection.write.Len()
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	if scheduled, _, active := queue.counts(); scheduled != 1 || active != 1 || connection.write.Len() != bytesAfterFirstReady {
		t.Fatalf("duplicate ready timers scheduled=%d active=%d bytes_before=%d bytes_after=%d", scheduled, active, bytesAfterFirstReady, connection.write.Len())
	}

	waveTask := queue.task(t, 0)
	beforeWave := connection.write.Len()
	queue.fire(waveTask, false)
	stream := connection.write.Bytes()[beforeWave:]
	spawn, rest := splitGameServerUpperPacket(t, stream)
	if spawn.Header.Classification != 0 || spawn.Header.MsgID != currentDungeonAltarMonsterSpawnMsgID || len(rest) != 0 {
		t.Fatalf("spawn header=%+v body=%x rest=%x", spawn.Header, spawn.Body, rest)
	}
	if len(spawn.Body) != 21 || binary.LittleEndian.Uint16(spawn.Body[:2]) != 1 ||
		binary.LittleEndian.Uint16(spawn.Body[11:13]) != 1000 || binary.LittleEndian.Uint16(spawn.Body[19:21]) != 1 {
		t.Fatalf("spawn body=%x", spawn.Body)
	}
	sequence := binary.LittleEndian.Uint16(spawn.Body[3:5])
	if sequence == 0 {
		t.Fatal("spawn sequence is zero")
	}

	beforeDeath := connection.write.Len()
	deathBody := []byte{1, byte(sequence), byte(sequence >> 8)}
	if err := service.handleCurrentDungeonAltarDeaths(session, deathBody); err != nil {
		t.Fatal(err)
	}
	prompt, rest := splitGameServerUpperPacket(t, connection.write.Bytes()[beforeDeath:])
	if prompt.Header.Classification != 0 || prompt.Header.MsgID != currentDungeonAltarDifficultyUIMsgID ||
		!bytes.Equal(prompt.Body, []byte{0, 1, 10, 0}) || len(rest) != 0 {
		t.Fatalf("difficulty prompt header=%+v body=%x rest=%x", prompt.Header, prompt.Body, rest)
	}
	if runtime.altar.progress != currentDungeonAltarProgressAwaitingDifficulty || runtime.altar.completedRounds != 1 {
		t.Fatalf("after death progress=%d completed=%d", runtime.altar.progress, runtime.altar.completedRounds)
	}
	difficultyTask := queue.task(t, 1)

	beforeChoice := connection.write.Len()
	if err := service.handleCurrentDungeonAltarDifficulty(session, []byte{2}); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes()[beforeChoice:])
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketSelectUltimateDifficulty) ||
		!bytes.Equal(ack.Body, []byte{1, 2}) {
		t.Fatalf("op317 ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	secondInterval, rest := splitGameServerUpperPacket(t, rest)
	if secondInterval.Header.Classification != 0 || secondInterval.Header.MsgID != currentDungeonAltarRoundIntervalMsgID || len(rest) != 0 {
		t.Fatalf("second interval header=%+v body=%x rest=%x", secondInterval.Header, secondInterval.Body, rest)
	}
	if runtime.altar.schedule == nil || runtime.altar.schedule.Difficulty != worldmap.BloodAltarDifficultyB {
		t.Fatalf("manual difficulty did not own next round: %+v", runtime.altar.schedule)
	}
	bytesBeforeStale := connection.write.Len()
	queue.fire(difficultyTask, true)
	if connection.write.Len() != bytesBeforeStale || runtime.altar.schedule == nil || runtime.altar.schedule.Difficulty != worldmap.BloodAltarDifficultyB {
		t.Fatalf("stale timeout changed state/body: before=%d after=%d schedule=%+v", bytesBeforeStale, connection.write.Len(), runtime.altar.schedule)
	}
}

func TestCancelCurrentDungeonAltarTimersActivelyCancelsScheduledWave(t *testing.T) {
	service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	task := queue.task(t, 0)
	session.dungeon.mu.Lock()
	service.cancelCurrentDungeonAltarTimersLocked(runtime)
	session.dungeon.mu.Unlock()
	if !task.cancelled {
		t.Fatal("altar wave timer was not actively canceled")
	}
	if scheduled, cancelled, active := queue.counts(); scheduled != 1 || cancelled != 1 || active != 0 {
		t.Fatalf("timers scheduled=%d cancelled=%d active=%d", scheduled, cancelled, active)
	}
}

func TestCancelCurrentDungeonAltarTimersActivelyCancelsTimedMonsterExpiry(t *testing.T) {
	service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	runtime.altar.boundMonsters[0].DurationMilliseconds = 3000
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	queue.fire(queue.task(t, 0), false)
	sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
	if len(runtime.altar.expiryTimerNames) != 1 {
		t.Fatalf("expiry timers=%v", runtime.altar.expiryTimerNames)
	}
	var timerName string
	for name := range runtime.altar.expiryTimerNames {
		timerName = name
	}
	queue.mu.Lock()
	expiryTask := queue.active[timerName]
	queue.mu.Unlock()
	if expiryTask == nil {
		t.Fatal("timed monster expiry task is missing")
	}
	session.dungeon.mu.Lock()
	service.cancelCurrentDungeonAltarTimersLocked(runtime)
	session.dungeon.mu.Unlock()
	if !expiryTask.cancelled || len(runtime.altar.expiryTimerNames) != 0 {
		t.Fatalf("expiry task cancelled=%t timers=%v", expiryTask.cancelled, runtime.altar.expiryTimerNames)
	}
	queue.fire(expiryTask, true)
	if _, active := runtime.altar.activeSequences[sequence]; !active || runtime.altar.completedRounds != 0 {
		t.Fatalf("stale cancelled expiry mutated altar: active=%v completed=%d",
			runtime.altar.activeSequences, runtime.altar.completedRounds)
	}
}

func TestCurrentDungeonAltarPVFDurationExpiryAdvancesEndlessAndUltimate(t *testing.T) {
	tests := []struct {
		name         string
		prepare      func(*testing.T) (*Service, *gameSession, *runtimeDungeonState)
		wantProgress currentDungeonAltarProgress
		wantSchedule bool
	}{
		{
			name: "endless",
			prepare: func(t *testing.T) (*Service, *gameSession, *runtimeDungeonState) {
				return prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
			},
			wantProgress: currentDungeonAltarProgressRoundActive,
			wantSchedule: true,
		},
		{
			name: "ultimate",
			prepare: func(t *testing.T) (*Service, *gameSession, *runtimeDungeonState) {
				service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
				return service, session, runtime
			},
			wantProgress: currentDungeonAltarProgressAwaitingDifficulty,
			wantSchedule: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime := test.prepare(t)
			queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
			if runtime.altar == nil || len(runtime.altar.boundMonsters) == 0 {
				t.Fatal("altar monster table unavailable")
			}
			runtime.altar.boundMonsters[0].DurationMilliseconds = 3000
			runtime.altar.boundMonsters[0].SpawnIntervalMilliseconds = 0
			if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
				t.Fatal(err)
			}
			waveTask := queue.task(t, 0)
			queue.fire(waveTask, false)
			sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
			if len(runtime.altar.expiryTimerNames) != 1 {
				t.Fatalf("expiry timers=%v", runtime.altar.expiryTimerNames)
			}
			var expiryTimerName string
			for name := range runtime.altar.expiryTimerNames {
				expiryTimerName = name
			}
			queue.mu.Lock()
			expiryTask := queue.active[expiryTimerName]
			queue.mu.Unlock()
			if expiryTask == nil || expiryTask.delay != 3*time.Second {
				t.Fatalf("expiry task=%+v", expiryTask)
			}
			queue.fire(expiryTask, false)
			if runtime.altar.completedRounds != 1 || runtime.altar.progress != test.wantProgress ||
				(runtime.altar.schedule != nil) != test.wantSchedule || len(runtime.altar.activeSequences) != 0 {
				t.Fatalf("after expiry completed=%d progress=%d schedule=%+v active=%v",
					runtime.altar.completedRounds, runtime.altar.progress, runtime.altar.schedule, runtime.altar.activeSequences)
			}
			if _, retired := runtime.altar.retiredSequences[sequence]; !retired {
				t.Fatalf("expired sequence %d was not retired", sequence)
			}
			completedBeforeStale := runtime.altar.completedRounds
			queue.fire(expiryTask, true)
			if runtime.altar.completedRounds != completedBeforeStale {
				t.Fatalf("stale expiry advanced twice: before=%d after=%d", completedBeforeStale, runtime.altar.completedRounds)
			}
		})
	}
}

func TestCurrentDungeonAltarDifficultyTimeoutWinsAgainstLateSelection(t *testing.T) {
	service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	service.dungeonChoice = func(limit int) (int, error) {
		if limit != 2 {
			t.Fatalf("difficulty choice limit=%d", limit)
		}
		return 1, nil
	}
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	queue.fire(queue.task(t, 0), false)
	var sequence uint16
	for candidate := range runtime.altar.activeSequences {
		sequence = candidate
	}
	if sequence == 0 {
		t.Fatal("first round did not own a live sequence")
	}
	if err := service.handleCurrentDungeonAltarDeaths(session, []byte{1, byte(sequence), byte(sequence >> 8)}); err != nil {
		t.Fatal(err)
	}
	timeout := queue.task(t, 1)
	beforeTimeout := connection.write.Len()
	queue.fire(timeout, false)
	resolved, rest := splitGameServerUpperPacket(t, connection.write.Bytes()[beforeTimeout:])
	if resolved.Header.Classification != 0 || resolved.Header.MsgID != currentDungeonAltarDifficultyUIMsgID ||
		!bytes.Equal(resolved.Body, []byte{2}) {
		t.Fatalf("timeout resolution header=%+v body=%x", resolved.Header, resolved.Body)
	}
	interval, rest := splitGameServerUpperPacket(t, rest)
	if interval.Header.Classification != 0 || interval.Header.MsgID != currentDungeonAltarRoundIntervalMsgID || len(rest) != 0 {
		t.Fatalf("timeout interval header=%+v body=%x rest=%x", interval.Header, interval.Body, rest)
	}
	if runtime.altar.schedule == nil || runtime.altar.schedule.Difficulty != worldmap.BloodAltarDifficultyB {
		t.Fatalf("timeout did not select B: %+v", runtime.altar.schedule)
	}
	bytesBeforeLateSelection := connection.write.Len()
	if err := service.handleCurrentDungeonAltarDifficulty(session, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != bytesBeforeLateSelection || runtime.altar.schedule == nil ||
		runtime.altar.schedule.Difficulty != worldmap.BloodAltarDifficultyB {
		t.Fatalf("late manual selection changed timeout winner: bytes_before=%d bytes_after=%d schedule=%+v",
			bytesBeforeLateSelection, connection.write.Len(), runtime.altar.schedule)
	}
}

func TestCurrentDungeonEndlessAltarDynamicOp39FailsClosedWithoutUniqueNextRoom(t *testing.T) {
	service, session, runtime := prepareCurrentDungeonEndlessAltarCompletionTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	connection := session.conn.(*bufferConn)

	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	for localRound := 0; localRound < 5; localRound++ {
		queue.fire(queue.task(t, localRound), false)
		sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
		owner := runtime.altar.activeSequences[sequence]
		if got, want := owner.FinalRoomWave, localRound == 4; got != want {
			t.Fatalf("local round %d final-room-wave=%t want=%t", localRound, got, want)
		}

		beforeDeath := connection.write.Len()
		if err := service.handleDungeonMonsterDeathLocal(
			session,
			currentDungeonAltarDynamicOp39Body(sequence, currentSceneActorObjectKey(session.selectedCharacterID), 1),
		); err != nil {
			t.Fatalf("local round %d dynamic op39: %v", localRound, err)
		}
		rest := assertCurrentDungeonAltarDynamicOp38(t, connection.write.Bytes()[beforeDeath:], sequence)
		if localRound < 4 {
			interval, trailing := splitGameServerUpperPacket(t, rest)
			trailing = assertOptionalCurrentDungeonEndlessAltarCrazyPacket(t, runtime, localRound+1, trailing)
			if interval.Header.Classification != 0 || interval.Header.MsgID != currentDungeonAltarRoundIntervalMsgID ||
				len(interval.Body) != 5 || interval.Body[0] != byte(localRound+2) || len(trailing) != 0 {
				t.Fatalf("local round %d next interval header=%+v body=%x trailing=%x", localRound, interval.Header, interval.Body, trailing)
			}
		} else if len(rest) != 0 {
			t.Fatalf("fifth-round dynamic death emitted non-op38 packets: %x", rest)
		}
	}

	scene, ok := runtime.Session.Scene()
	if !ok || !scene.Cleared || runtime.altar.completedRounds != 5 ||
		runtime.altar.progress != currentDungeonAltarProgressRoomComplete || len(runtime.altar.activeSequences) != 0 ||
		runtime.altar.roomTransitionTimerName == "" {
		t.Fatalf("fifth round did not release room: scene=%+v completed=%d progress=%d active=%d",
			scene, runtime.altar.completedRounds, runtime.altar.progress, len(runtime.altar.activeSequences))
	}
	if _, err := currentDungeonEndlessAltarNextRoom(runtime); err == nil {
		t.Fatal("malformed one-room endless altar invented a next-room target")
	}
}

func TestCurrentDungeonEndlessAltarRoomCompleteActivelyMovesToNextRoom(t *testing.T) {
	tests := []struct {
		name    string
		dynamic bool
	}{
		{name: "op255", dynamic: false},
		{name: "op39", dynamic: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime := prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
			queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
			connection := session.conn.(*bufferConn)
			initialGeneration := runtime.altar.Room.Generation

			if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
				t.Fatal(err)
			}
			connection.write.Reset()
			for localRound := 0; localRound < 5; localRound++ {
				scheduled, _, _ := queue.counts()
				queue.fire(queue.task(t, scheduled-1), false)
				sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
				connection.write.Reset()
				var err error
				if test.dynamic {
					err = service.handleDungeonMonsterDeathLocal(
						session,
						currentDungeonAltarDynamicOp39Body(
							sequence,
							currentSceneActorObjectKey(session.selectedCharacterID),
							1,
						),
					)
				} else {
					err = service.handleCurrentDungeonAltarDeaths(
						session,
						[]byte{1, byte(sequence), byte(sequence >> 8)},
					)
				}
				if err != nil {
					t.Fatalf("local round %d death: %v", localRound, err)
				}

				stream := connection.write.Bytes()
				if test.dynamic {
					stream = assertCurrentDungeonAltarDynamicOp38(t, stream, sequence)
				}
				if len(stream) == 0 {
					scene, _ := runtime.Session.Scene()
					scheduled, cancelled, active := queue.counts()
					t.Fatalf("local round %d emitted no progress packet: room=%s/%d progress=%d transition_timer=%q timers=%d/%d/%d",
						localRound, scene.Coordinate, scene.Map.Map.ID, runtime.altar.progress,
						runtime.altar.roomTransitionTimerName, scheduled, cancelled, active)
				}
				packet, rest := splitGameServerUpperPacket(t, stream)
				if localRound < 4 {
					rest = assertOptionalCurrentDungeonEndlessAltarCrazyPacket(t, runtime, localRound+1, rest)
					if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonAltarRoundIntervalMsgID ||
						len(packet.Body) != 5 || packet.Body[0] != byte(localRound+2) || len(rest) != 0 {
						t.Fatalf("local round %d interval header=%+v body=%x rest=%x", localRound, packet.Header, packet.Body, rest)
					}
					continue
				}
				if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonAltarStartMapMsgID ||
					len(packet.Body) != 15 || packet.Body[0] != 1 || packet.Body[1] != 0 || packet.Body[7] != 1 ||
					binary.LittleEndian.Uint32(packet.Body[8:12]) != 17021 || len(rest) != 0 {
					t.Fatalf("fifth-round op211 header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
				}
			}

			scene, ok := runtime.Session.Scene()
			if !ok || scene.Coordinate != (worldmap.RoomCoordinate{X: 1, Y: 0}) || scene.Map.Map.ID != 17021 ||
				runtime.altar.Room.Key != runtimeDungeonRoomKeyFromScene(scene) ||
				runtime.altar.Room.Generation == initialGeneration ||
				runtime.altar.progress != currentDungeonAltarProgressAwaitingReady ||
				runtime.altar.completedRounds != 5 || len(runtime.altar.completedRooms) != 1 {
				t.Fatalf("next room scene=%+v altar=%+v", scene, runtime.altar)
			}

			connection.write.Reset()
			if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
				t.Fatal(err)
			}
			interval, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			rest = assertOptionalCurrentDungeonEndlessAltarCrazyPacket(t, runtime, 0, rest)
			if interval.Header.Classification != 0 || interval.Header.MsgID != currentDungeonAltarRoundIntervalMsgID ||
				len(interval.Body) != 5 || interval.Body[0] != 6 || len(rest) != 0 {
				t.Fatalf("second-room first interval header=%+v body=%x rest=%x", interval.Header, interval.Body, rest)
			}
		})
	}
}

func assertOptionalCurrentDungeonEndlessAltarCrazyPacket(
	t *testing.T,
	runtime *runtimeDungeonState,
	localRound int,
	stream []byte,
) []byte {
	t.Helper()
	if runtime == nil || runtime.altar == nil || runtime.altar.crazyMonster == nil ||
		runtime.altar.crazyMonster.RoundIndex != localRound {
		return stream
	}
	packet, rest := splitGameServerUpperPacket(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonAltarCrazyMonsterMsgID ||
		len(packet.Body) != 5 || binary.LittleEndian.Uint32(packet.Body[:4]) != runtime.altar.crazyMonster.MonsterCode ||
		packet.Body[4] != runtime.altar.crazyMonster.Variant {
		t.Fatalf("local round %d crazy packet header=%+v body=%x", localRound, packet.Header, packet.Body)
	}
	return rest
}

func TestCurrentDungeonEndlessAltarRoomMoveWriteFailureRetriesOnce(t *testing.T) {
	service, session, runtime := prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
	queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	for localRound := 0; localRound < 4; localRound++ {
		scheduled, _, _ := queue.counts()
		queue.fire(queue.task(t, scheduled-1), false)
		sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
		if err := service.handleCurrentDungeonAltarDeaths(
			session,
			[]byte{1, byte(sequence), byte(sequence >> 8)},
		); err != nil {
			t.Fatalf("local round %d death: %v", localRound, err)
		}
	}
	scheduled, _, _ := queue.counts()
	queue.fire(queue.task(t, scheduled-1), false)
	sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
	sourceRoom := runtime.altar.Room
	wantWriteErr := errors.New("altar op211 write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}
	if err := service.handleCurrentDungeonAltarDeaths(
		session,
		[]byte{1, byte(sequence), byte(sequence >> 8)},
	); err != nil {
		t.Fatal(err)
	}
	scene, ok := runtime.Session.Scene()
	if !ok || runtimeDungeonRoomKeyFromScene(scene) != sourceRoom.Key || runtime.altar.Room != sourceRoom ||
		runtime.altar.progress != currentDungeonAltarProgressRoomComplete ||
		runtime.altar.roomTransitionTimerName == "" {
		t.Fatalf("failed write committed or lost retry: scene=%+v altar=%+v", scene, runtime.altar)
	}
	scheduled, _, active := queue.counts()
	if active != 1 {
		t.Fatalf("room-move retry timers scheduled=%d active=%d", scheduled, active)
	}
	retryTask := queue.task(t, scheduled-1)
	timerName := runtime.altar.roomTransitionTimerName
	retryVersion := runtime.altar.roomTransitionVersion
	if err := service.fireCurrentDungeonAltarRoomMoveRetry(
		session,
		runtime,
		runtime.lifecycleToken+1,
		sourceRoom,
		retryVersion,
		timerName,
		queue.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if runtime.altar.Room != sourceRoom || runtime.altar.roomTransitionTimerName != timerName {
		t.Fatalf("stale lifecycle token changed retry owner: room=%+v timer=%q", runtime.altar.Room, runtime.altar.roomTransitionTimerName)
	}

	retryConnection := &bufferConn{}
	session.conn = retryConnection
	queue.fire(retryTask, false)
	if retryConnection.write.Len() == 0 {
		scene, _ = runtime.Session.Scene()
		scheduled, cancelled, active := queue.counts()
		t.Fatalf("room-move retry emitted no op211: scene=%+v altar=%+v timers=%d/%d/%d",
			scene, runtime.altar, scheduled, cancelled, active)
	}
	packet, rest := splitGameServerUpperPacket(t, retryConnection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonAltarStartMapMsgID ||
		len(packet.Body) != 15 || packet.Body[0] != 1 || packet.Body[1] != 0 ||
		binary.LittleEndian.Uint32(packet.Body[8:12]) != 17021 || len(rest) != 0 {
		t.Fatalf("retry op211 header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
	scene, ok = runtime.Session.Scene()
	if !ok || scene.Coordinate != (worldmap.RoomCoordinate{X: 1, Y: 0}) || scene.Map.Map.ID != 17021 ||
		runtime.altar.Room.Generation == sourceRoom.Generation ||
		runtime.altar.progress != currentDungeonAltarProgressAwaitingReady ||
		runtime.altar.roomTransitionTimerName != "" {
		t.Fatalf("retry did not commit exactly one next room: scene=%+v altar=%+v", scene, runtime.altar)
	}
	committedRoom := runtime.altar.Room
	bytesBeforeStale := retryConnection.write.Len()
	queue.fire(retryTask, true)
	if retryConnection.write.Len() != bytesBeforeStale || runtime.altar.Room != committedRoom {
		t.Fatalf("stale retry replayed op211/commit: bytes=%d->%d room=%+v->%+v",
			bytesBeforeStale, retryConnection.write.Len(), committedRoom, runtime.altar.Room)
	}
}

func TestCurrentDungeonEndlessAltarDynamicOp39RetriesOnlyOp211AfterSecondWriteFails(t *testing.T) {
	service, session, runtime := prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
	queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	for localRound := 0; localRound < 4; localRound++ {
		fireCurrentDungeonAltarNamedTimerTest(t, queue, runtime.altar.waveTimerName)
		sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
		if err := service.handleDungeonMonsterDeathLocal(
			session,
			currentDungeonAltarDynamicOp39Body(
				sequence,
				currentSceneActorObjectKey(session.selectedCharacterID),
				1,
			),
		); err != nil {
			t.Fatalf("local round %d dynamic death: %v", localRound, err)
		}
	}
	fireCurrentDungeonAltarNamedTimerTest(t, queue, runtime.altar.waveTimerName)
	sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
	sourceRoom := runtime.altar.Room
	wantWriteErr := errors.New("altar dynamic op211 second write failed")
	failing := &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}
	session.conn = failing
	if err := service.handleDungeonMonsterDeathLocal(
		session,
		currentDungeonAltarDynamicOp39Body(
			sequence,
			currentSceneActorObjectKey(session.selectedCharacterID),
			1,
		),
	); err != nil {
		t.Fatal(err)
	}
	if failing.writes != 2 {
		t.Fatalf("dynamic final writes=%d want op38 success then op211 failure", failing.writes)
	}
	if rest := assertCurrentDungeonAltarDynamicOp38(t, failing.write.Bytes(), sequence); len(rest) != 0 {
		t.Fatalf("failed op211 retained packets after op38: %x", rest)
	}
	scene, ok := runtime.Session.Scene()
	if !ok || runtimeDungeonRoomKeyFromScene(scene) != sourceRoom.Key || runtime.altar.Room != sourceRoom ||
		runtime.altar.progress != currentDungeonAltarProgressRoomComplete ||
		runtime.altar.roomTransitionTimerName == "" {
		t.Fatalf("second-write failure committed or lost retry: scene=%+v altar=%+v", scene, runtime.altar)
	}
	scheduled, _, active := queue.counts()
	if active != 1 {
		t.Fatalf("dynamic room-move retry timers scheduled=%d active=%d", scheduled, active)
	}
	retryTask := queue.task(t, scheduled-1)

	retryConnection := &bufferConn{}
	session.conn = retryConnection
	queue.fire(retryTask, false)
	packet, rest := splitGameServerUpperPacket(t, retryConnection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonAltarStartMapMsgID ||
		len(packet.Body) != 15 || packet.Body[0] != 1 || packet.Body[1] != 0 ||
		binary.LittleEndian.Uint32(packet.Body[8:12]) != 17021 || len(rest) != 0 {
		t.Fatalf("dynamic retry packet header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
	scene, ok = runtime.Session.Scene()
	if !ok || scene.Coordinate != (worldmap.RoomCoordinate{X: 1, Y: 0}) || scene.Map.Map.ID != 17021 ||
		runtime.altar.Room.Generation == sourceRoom.Generation ||
		runtime.altar.progress != currentDungeonAltarProgressAwaitingReady {
		t.Fatalf("dynamic retry did not commit once: scene=%+v altar=%+v", scene, runtime.altar)
	}
	committedRoom := runtime.altar.Room
	bytesBeforeStale := retryConnection.write.Len()
	queue.fire(retryTask, true)
	if retryConnection.write.Len() != bytesBeforeStale || runtime.altar.Room != committedRoom {
		t.Fatalf("dynamic stale retry replayed packet/commit: bytes=%d->%d room=%+v->%+v",
			bytesBeforeStale, retryConnection.write.Len(), committedRoom, runtime.altar.Room)
	}
}

func TestCurrentDungeonEndlessAltarCompletesFifteenRoundsAcrossThreeRooms(t *testing.T) {
	service, session, runtime := prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
	queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	connection := session.conn.(*bufferConn)
	prepareCurrentDungeonAltarEmptySettlementTestState(t, runtime)
	op211Count, rankingCount, rewardCount := 0, 0, 0

	for roomIndex := 0; roomIndex < 3; roomIndex++ {
		if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
			t.Fatalf("room %d ready: %v", roomIndex, err)
		}
		connection.write.Reset()
		for localRound := 0; localRound < 5; localRound++ {
			fireCurrentDungeonAltarNamedTimerTest(t, queue, runtime.altar.waveTimerName)
			sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
			connection.write.Reset()
			if err := service.handleCurrentDungeonAltarDeaths(
				session,
				[]byte{1, byte(sequence), byte(sequence >> 8)},
			); err != nil {
				t.Fatalf("room %d local round %d death: %v", roomIndex, localRound, err)
			}
			stream := connection.write.Bytes()
			for len(stream) != 0 {
				packet, rest := splitGameServerUpperPacket(t, stream)
				switch packet.Header.MsgID {
				case currentDungeonAltarStartMapMsgID:
					op211Count++
				case currentDungeonAltarRankingMsgID:
					rankingCount++
				case currentDungeonAltarRewardMsgID:
					rewardCount++
				}
				stream = rest
			}
		}
	}

	snapshot := runtime.Session.Snapshot()
	if runtime.altar.completedRounds != 15 || len(runtime.altar.completedRooms) != 3 ||
		runtime.altar.progress != currentDungeonAltarProgressDungeonComplete ||
		snapshot.Run.Status != worldmap.DungeonRunCompleted || op211Count != 2 ||
		rankingCount != 1 || rewardCount != 1 ||
		!completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("endless completion rounds=%d rooms=%d progress=%d status=%s op211=%d op208=%d op209=%d settlement=%+v",
			runtime.altar.completedRounds, len(runtime.altar.completedRooms), runtime.altar.progress,
			snapshot.Run.Status, op211Count, rankingCount, rewardCount, runtime.altar.settlement)
	}
}

func TestCurrentDungeonUltimateAltarCompletesSevenRoundsWithoutRoomMove(t *testing.T) {
	service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	prepareCurrentDungeonAltarEmptySettlementTestState(t, runtime)
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	op211Count, rankingCount, rewardCount := 0, 0, 0

	for runtime.altar.completedRounds < runtime.altar.MaxRounds {
		fireCurrentDungeonAltarNamedTimerTest(t, queue, runtime.altar.waveTimerName)
		sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
		connection.write.Reset()
		if err := service.handleCurrentDungeonAltarDeaths(
			session,
			[]byte{1, byte(sequence), byte(sequence >> 8)},
		); err != nil {
			t.Fatalf("round %d death: %v", runtime.altar.completedRounds+1, err)
		}
		stream := connection.write.Bytes()
		for len(stream) != 0 {
			packet, rest := splitGameServerUpperPacket(t, stream)
			switch packet.Header.MsgID {
			case currentDungeonAltarStartMapMsgID:
				op211Count++
			case currentDungeonAltarRankingMsgID:
				rankingCount++
			case currentDungeonAltarRewardMsgID:
				rewardCount++
			}
			stream = rest
		}
		if runtime.altar.completedRounds == runtime.altar.MaxRounds {
			break
		}
		switch runtime.altar.progress {
		case currentDungeonAltarProgressAwaitingDifficulty:
			connection.write.Reset()
			if err := service.handleCurrentDungeonAltarDifficulty(session, []byte{1}); err != nil {
				t.Fatal(err)
			}
		case currentDungeonAltarProgressFinalRoundDelay:
			connection.write.Reset()
			fireCurrentDungeonAltarNamedTimerTest(t, queue, runtime.altar.finalTimerName)
		default:
			t.Fatalf("unexpected ultimate progress after round %d: %d", runtime.altar.completedRounds, runtime.altar.progress)
		}
	}

	snapshot := runtime.Session.Snapshot()
	if runtime.altar.completedRounds != 7 || runtime.altar.progress != currentDungeonAltarProgressDungeonComplete ||
		snapshot.Run.Status != worldmap.DungeonRunCompleted || op211Count != 0 ||
		rankingCount != 1 || rewardCount != 1 ||
		!completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("ultimate completion rounds=%d progress=%d status=%s op211=%d op208=%d op209=%d settlement=%+v",
			runtime.altar.completedRounds, runtime.altar.progress, snapshot.Run.Status,
			op211Count, rankingCount, rewardCount, runtime.altar.settlement)
	}
}

func TestCurrentDungeonUltimateAltarDynamicOp39DuplicateResendsOnlyOp38(t *testing.T) {
	service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	queue.fire(queue.task(t, 0), false)
	sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
	body := currentDungeonAltarDynamicOp39Body(sequence, currentSceneActorObjectKey(session.selectedCharacterID), 1)

	beforeFirst := connection.write.Len()
	if err := service.handleDungeonMonsterDeathLocal(session, body); err != nil {
		t.Fatal(err)
	}
	rest := assertCurrentDungeonAltarDynamicOp38(t, connection.write.Bytes()[beforeFirst:], sequence)
	prompt, trailing := splitGameServerUpperPacket(t, rest)
	if prompt.Header.Classification != 0 || prompt.Header.MsgID != currentDungeonAltarDifficultyUIMsgID || len(trailing) != 0 {
		t.Fatalf("ultimate dynamic death prompt=%+v body=%x trailing=%x", prompt.Header, prompt.Body, trailing)
	}
	if _, active := runtime.altar.activeSequences[sequence]; active {
		t.Fatal("ultimate dynamic sequence remained active")
	}
	if _, retired := runtime.altar.retiredSequences[sequence]; !retired || runtime.altar.completedRounds != 1 {
		t.Fatalf("ultimate dynamic sequence retired=%t completed=%d", retired, runtime.altar.completedRounds)
	}

	beforeDuplicate := connection.write.Len()
	if err := service.handleDungeonMonsterDeathLocal(session, body); err != nil {
		t.Fatal(err)
	}
	duplicateRest := assertCurrentDungeonAltarDynamicOp38(t, connection.write.Bytes()[beforeDuplicate:], sequence)
	if len(duplicateRest) != 0 || runtime.altar.completedRounds != 1 ||
		runtime.altar.progress != currentDungeonAltarProgressAwaitingDifficulty {
		t.Fatalf("duplicate dynamic op39 rest=%x completed=%d progress=%d",
			duplicateRest, runtime.altar.completedRounds, runtime.altar.progress)
	}
}

func TestCurrentDungeonAltarDynamicOp39AcceptsNativeVariableShapes(t *testing.T) {
	tests := []struct {
		name string
		body func(sequence uint16, owner uint16) []byte
	}{
		{
			name: "count_zero",
			body: func(sequence uint16, owner uint16) []byte {
				return currentDungeonAltarDynamicOp39Body(sequence, owner, 0)
			},
		},
		{
			name: "count_greater_than_one",
			body: func(sequence uint16, owner uint16) []byte {
				return currentDungeonAltarDynamicOp39Body(sequence, owner, 2)
			},
		},
		{
			name: "non_player_owner",
			body: func(sequence uint16, owner uint16) []byte {
				return currentDungeonAltarDynamicOp39Body(sequence, owner+1, 1)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
			queue := newFakeCurrentDungeonDeathTimerQueue()
			service.gameplayTimers = queue
			if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
				t.Fatal(err)
			}
			queue.fire(queue.task(t, 0), false)
			sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
			before := connection.write.Len()
			if err := service.handleDungeonMonsterDeathLocal(
				session,
				test.body(sequence, currentSceneActorObjectKey(session.selectedCharacterID)),
			); err != nil {
				t.Fatal(err)
			}
			rest := assertCurrentDungeonAltarDynamicOp38(t, connection.write.Bytes()[before:], sequence)
			prompt, trailing := splitGameServerUpperPacket(t, rest)
			if prompt.Header.MsgID != currentDungeonAltarDifficultyUIMsgID || len(trailing) != 0 {
				t.Fatalf("native variable altar op39 prompt=%+v body=%x trailing=%x", prompt.Header, prompt.Body, trailing)
			}
			if _, active := runtime.altar.activeSequences[sequence]; active || runtime.altar.completedRounds != 1 {
				t.Fatalf("native variable altar op39 active=%t completed=%d", active, runtime.altar.completedRounds)
			}
		})
	}
}

func TestCurrentDungeonAltarDynamicOp39RejectsUnownedBoundaries(t *testing.T) {
	tests := []struct {
		name string
		body func(sequence uint16, owner uint16) []byte
	}{
		{
			name: "fixed62_layout",
			body: func(sequence uint16, owner uint16) []byte {
				body := make([]byte, dungeoncmd.DieMonsterRequestSize)
				binary.LittleEndian.PutUint32(body[0:4], uint32(sequence))
				binary.LittleEndian.PutUint16(body[4:6], owner)
				return body
			},
		},
		{
			name: "opaque_tail",
			body: func(sequence uint16, owner uint16) []byte {
				return append(currentDungeonAltarDynamicOp39Body(sequence, owner, 1), 0xaa)
			},
		},
		{
			name: "wrong_sequence",
			body: func(sequence uint16, owner uint16) []byte {
				return currentDungeonAltarDynamicOp39Body(sequence+1000, owner, 1)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
			queue := newFakeCurrentDungeonDeathTimerQueue()
			service.gameplayTimers = queue
			if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
				t.Fatal(err)
			}
			queue.fire(queue.task(t, 0), false)
			sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
			before := connection.write.Len()
			if err := service.handleDungeonMonsterDeathLocal(
				session,
				test.body(sequence, currentSceneActorObjectKey(session.selectedCharacterID)),
			); err != nil {
				t.Fatal(err)
			}
			if connection.write.Len() != before {
				t.Fatalf("unowned altar op39 emitted bytes: %x", connection.write.Bytes()[before:])
			}
			if _, active := runtime.altar.activeSequences[sequence]; !active || runtime.altar.completedRounds != 0 {
				t.Fatalf("unowned altar op39 active=%t completed=%d", active, runtime.altar.completedRounds)
			}
		})
	}
}

func TestCurrentDungeonAltarDynamicOp39WriteFailureRetriesBeforeRetirement(t *testing.T) {
	service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	queue.fire(queue.task(t, 0), false)
	sequence := onlyCurrentDungeonAltarActiveSequence(t, runtime)
	wantWriteErr := errors.New("altar dynamic op38 write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}
	if err := service.handleDungeonMonsterDeathLocal(
		session,
		currentDungeonAltarDynamicOp39Body(sequence, currentSceneActorObjectKey(session.selectedCharacterID), 1),
	); err != nil {
		t.Fatal(err)
	}
	if _, active := runtime.altar.activeSequences[sequence]; !active {
		t.Fatal("failed dynamic op38 retired altar sequence")
	}
	if runtime.altar.questDropRetry == nil || !runtime.altar.questDropRetry.DynamicDeathNotification ||
		runtime.altar.completedRounds != 0 {
		t.Fatalf("failed dynamic op38 retry=%+v completed=%d", runtime.altar.questDropRetry, runtime.altar.completedRounds)
	}

	retryConnection := &bufferConn{}
	session.conn = retryConnection
	queue.fire(queue.task(t, 1), false)
	rest := assertCurrentDungeonAltarDynamicOp38(t, retryConnection.write.Bytes(), sequence)
	prompt, trailing := splitGameServerUpperPacket(t, rest)
	if prompt.Header.MsgID != currentDungeonAltarDifficultyUIMsgID || len(trailing) != 0 {
		t.Fatalf("retry prompt=%+v body=%x trailing=%x", prompt.Header, prompt.Body, trailing)
	}
	if _, active := runtime.altar.activeSequences[sequence]; active || runtime.altar.questDropRetry != nil ||
		runtime.altar.completedRounds != 1 {
		t.Fatalf("retry active=%t pending=%+v completed=%d", active, runtime.altar.questDropRetry, runtime.altar.completedRounds)
	}
}

func TestCurrentDungeonAltarPendingRetryQueuesLaterDynamicDeathsWithoutLoss(t *testing.T) {
	service, session, runtime, _, connection := prepareCurrentDungeonAltarRuntimeTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	runtime.altar.boundMonsters[0].BaseSpawnCount = 3

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	counter := &countingCurrentDungeonAltarSettlement{base: repositories.CharacterSettlement}
	availableRepositories := repositories
	availableRepositories.CharacterSettlement = counter
	unavailableRepositories := availableRepositories
	unavailableRepositories.CharacterSettlement = nil
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return unavailableRepositories, true }

	if err := service.handleCurrentDungeonAltarReady(session, nil); err != nil {
		t.Fatal(err)
	}
	queue.fire(queue.task(t, 0), false)
	sequences := make([]uint16, 0, len(runtime.altar.activeSequences))
	for sequence := range runtime.altar.activeSequences {
		sequences = append(sequences, sequence)
	}
	if len(sequences) != 3 {
		t.Fatalf("active sequences=%v", sequences)
	}
	for left := 0; left < len(sequences); left++ {
		for right := left + 1; right < len(sequences); right++ {
			if sequences[left] > sequences[right] {
				sequences[left], sequences[right] = sequences[right], sequences[left]
			}
		}
	}
	beforeFailure := connection.write.Len()
	firstDynamicBody := currentDungeonAltarDynamicOp39Body(sequences[0], 700, 0)
	if err := service.handleDungeonMonsterDeathLocal(session, firstDynamicBody); err != nil {
		t.Fatalf("queue first dynamic sequence %d: %v", sequences[0], err)
	}
	if err := service.handleCurrentDungeonAltarDeaths(
		session,
		[]byte{1, byte(sequences[1]), byte(sequences[1] >> 8)},
	); err != nil {
		t.Fatalf("queue op255 sequence %d: %v", sequences[1], err)
	}
	lastDynamicBody := currentDungeonAltarDynamicOp39Body(sequences[2], 701, 2)
	if err := service.handleDungeonMonsterDeathLocal(session, lastDynamicBody); err != nil {
		t.Fatalf("queue last dynamic sequence %d: %v", sequences[2], err)
	}
	if err := service.handleDungeonMonsterDeathLocal(session, lastDynamicBody); err != nil {
		t.Fatalf("duplicate queued dynamic sequence %d: %v", sequences[2], err)
	}
	if connection.write.Len() != beforeFailure {
		t.Fatalf("pending deaths emitted packets before retry: %x", connection.write.Bytes()[beforeFailure:])
	}
	if runtime.altar.questDropRetry == nil || len(runtime.altar.questDropRetryQueue) != 2 ||
		len(runtime.altar.activeSequences) != 3 {
		t.Fatalf("pending head=%+v queue=%+v active=%d",
			runtime.altar.questDropRetry, runtime.altar.questDropRetryQueue, len(runtime.altar.activeSequences))
	}
	if !runtime.altar.questDropRetry.DynamicDeathNotification ||
		runtime.altar.questDropRetryQueue[0].DynamicDeathNotification ||
		!runtime.altar.questDropRetryQueue[1].DynamicDeathNotification {
		t.Fatalf("pending dynamic markers head=%t queue=%t,%t",
			runtime.altar.questDropRetry.DynamicDeathNotification,
			runtime.altar.questDropRetryQueue[0].DynamicDeathNotification,
			runtime.altar.questDropRetryQueue[1].DynamicDeathNotification)
	}
	pendingBatches := []*currentDungeonAltarQuestDropRetry{
		runtime.altar.questDropRetry,
		runtime.altar.questDropRetryQueue[0],
		runtime.altar.questDropRetryQueue[1],
	}
	for index, pendingBatch := range pendingBatches {
		owner := runtime.altar.activeSequences[sequences[index]]
		if len(pendingBatch.Deaths) != 1 || pendingBatch.Deaths[0].Sequence != sequences[index] ||
			pendingBatch.Deaths[0].Owner != owner {
			t.Fatalf("pending batch %d=%+v owner=%+v", index, pendingBatch, owner)
		}
	}
	retryTask := queue.task(t, 1)
	pending := runtime.altar.questDropRetry
	staleRoom := pending.Room
	staleRoom.Generation++
	if err := service.fireCurrentDungeonAltarQuestDropRetry(
		session,
		runtime,
		runtime.lifecycleToken,
		staleRoom,
		pending.Version,
		retryTask.name,
		queue.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if len(runtime.altar.activeSequences) != 3 || connection.write.Len() != beforeFailure {
		t.Fatalf("stale room generation mutated queue: active=%d bytes=%d",
			len(runtime.altar.activeSequences), connection.write.Len()-beforeFailure)
	}

	service.repositoryProvider = func() (dnfrepo.Group, bool) { return availableRepositories, true }
	queue.fire(retryTask, false)
	stream := connection.write.Bytes()[beforeFailure:]
	stream = assertCurrentDungeonAltarDynamicOp38(t, stream, sequences[0])
	stream = assertCurrentDungeonAltarDynamicOp38(t, stream, sequences[2])
	prompt, trailing := splitGameServerUpperPacket(t, stream)
	if prompt.Header.Classification != 0 || prompt.Header.MsgID != currentDungeonAltarDifficultyUIMsgID || len(trailing) != 0 {
		t.Fatalf("queued dynamic prompt=%+v body=%x trailing=%x", prompt.Header, prompt.Body, trailing)
	}
	if counter.calls != 3 || len(runtime.altar.questDropResults) != 3 {
		t.Fatalf("queued dynamic quest commits=%d cached=%d", counter.calls, len(runtime.altar.questDropResults))
	}
	if len(runtime.altar.activeSequences) != 0 || runtime.altar.completedRounds != 1 ||
		runtime.altar.questDropRetry != nil || len(runtime.altar.questDropRetryQueue) != 0 ||
		runtime.altar.questDropRetryProgress != currentDungeonAltarProgressNone {
		t.Fatalf("queued dynamic drain active=%d completed=%d head=%+v queue=%+v progress=%d",
			len(runtime.altar.activeSequences), runtime.altar.completedRounds, runtime.altar.questDropRetry,
			runtime.altar.questDropRetryQueue, runtime.altar.questDropRetryProgress)
	}
	beforeStale := connection.write.Len()
	queue.fire(retryTask, true)
	if connection.write.Len() != beforeStale || counter.calls != 3 {
		t.Fatalf("stale completed retry bytes=%d quest commits=%d", connection.write.Len()-beforeStale, counter.calls)
	}
}

func TestCurrentDungeonAltarDynamicOp39DoesNotChangeOrdinaryDeath(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonMonsterExperienceTest(t)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "altar-dynamic-op39-ordinary-control",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if runtime.altar != nil {
		t.Fatal("ordinary control unexpectedly owns altar runtime")
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	body := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(body[0:4], monster.ObjectKey)
	binary.LittleEndian.PutUint16(body[4:6], currentSceneActorObjectKey(session.selectedCharacterID))
	if err := service.handleDungeonMonsterDeathLocal(session, body); err != nil {
		t.Fatal(err)
	}
	death, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if death.Header.Classification != 0 || death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) {
		t.Fatalf("ordinary op39 response=%+v body=%x", death.Header, death.Body)
	}
	if got := runtime.Room.Snapshot().Monsters[0].State; got != runtimeDungeonMonsterDefeated {
		t.Fatalf("ordinary op39 monster state=%s", got)
	}
}

func currentDungeonAltarDynamicOp39Body(sequence uint16, owner uint16, combatEntryCount byte) []byte {
	body := make([]byte, dungeoncmd.DieMonsterVariableBaseSize+int(combatEntryCount)*dungeoncmd.DieMonsterVariableCombatEntrySize)
	binary.LittleEndian.PutUint32(body[0:4], uint32(sequence))
	binary.LittleEndian.PutUint16(body[4:6], owner)
	body[22] = combatEntryCount
	return body
}

func onlyCurrentDungeonAltarActiveSequence(t *testing.T, runtime *runtimeDungeonState) uint16 {
	t.Helper()
	if runtime == nil || runtime.altar == nil || len(runtime.altar.activeSequences) != 1 {
		t.Fatalf("altar active sequences=%v", runtime)
	}
	for sequence := range runtime.altar.activeSequences {
		return sequence
	}
	return 0
}

func assertCurrentDungeonAltarDynamicOp38(t *testing.T, stream []byte, sequence uint16) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	wantBody, err := buildCurrentDungeonDeathNotificationBody(uint32(sequence), currentDungeonDeathResponseMonster)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		!bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("altar dynamic op38 header=%+v body=%x want=%x", packet.Header, packet.Body, wantBody)
	}
	return rest
}

func fireCurrentDungeonAltarNamedTimerTest(
	t *testing.T,
	queue *fakeCurrentDungeonDeathTimerQueue,
	timerName string,
) {
	t.Helper()
	if queue == nil || timerName == "" {
		t.Fatalf("altar timer unavailable: queue=%p name=%q", queue, timerName)
	}
	queue.mu.Lock()
	task := queue.active[timerName]
	queue.mu.Unlock()
	if task == nil {
		t.Fatalf("altar timer %q is not active", timerName)
	}
	queue.fire(task, false)
}

func prepareCurrentDungeonAltarEmptySettlementTestState(t *testing.T, runtime *runtimeDungeonState) {
	t.Helper()
	if runtime == nil || runtime.altar == nil {
		t.Fatal("altar runtime unavailable")
	}
	plan, err := newDungeonCardRewardPlan(
		dungeonCardPlanIdentity{
			CharacterID: "99",
			DungeonID:   runtime.Dungeon.ID,
			MazeIndex:   runtime.MazeIndex,
			RunSeed:     runtime.Seed,
		},
		"altar-full-path-test",
		dungeonCardRewardBundle{},
		dungeonCardRewardBundle{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rewardState, err := newDungeonCardState(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime.altar.settlement = &currentDungeonAltarSettlementRuntime{
		phase: currentDungeonAltarSettlementPrepared,
		plan: currentDungeonAltarSettlementPlan{
			CurrentRound:         uint32(runtime.altar.MaxRounds),
			PlayTimeMilliseconds: uint32(runtime.altar.MaxRounds * 100),
			BestRound:            uint32(runtime.altar.MaxRounds),
			BestTimeMilliseconds: uint32(runtime.altar.MaxRounds * 100),
			MaxRound:             uint32(runtime.altar.MaxRounds),
			CharacterStateBody:   make([]byte, currentFinishLoadingCharacterStateBodySize),
		},
		rewardState: rewardState,
	}
}

func prepareCurrentDungeonAltarRuntimeTest(
	t *testing.T,
) (*Service, *gameSession, *runtimeDungeonState, worldmap.DungeonRoomScene, *bufferConn) {
	t.Helper()
	source := currentDungeonUltimateAltarPVFTestSource()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       70,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:      queue,
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "altar-runtime-test",
		selectedCharacterID: 99,
		characterGeneration: 1,
	}
	runtime, scene, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 11007, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.lifecycleToken = 1
	session.dungeon.runtime = runtime
	return service, session, runtime, scene, connection
}

func prepareCurrentDungeonEndlessAltarRoomMoveTest(
	t *testing.T,
) (*Service, *gameSession, *runtimeDungeonState) {
	t.Helper()
	source := currentDungeonEndlessAltarRoomMovePVFTestSource()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       70,
		Stats:       map[string]int64{"fatigue": 100, "gold": 100_000},
	}); err != nil {
		t.Fatal(err)
	}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:      queue,
	}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "endless-altar-room-move-test",
		selectedCharacterID: 99,
		characterGeneration: 1,
	}
	runtime, _, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 11006, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.lifecycleToken = 1
	session.dungeon.runtime = runtime
	return service, session, runtime
}

func currentDungeonEndlessAltarRoomMovePVFTestSource() bridgePVFSource {
	phaseRows := ""
	for round := 0; round < 5; round++ {
		phaseRows += fmt.Sprintf("%d 0 0 1 0 0\n", round)
	}
	mapBody := func(name, mapType string) string {
		return "[map name]\n`" + name + "`\n" +
			"[dungeon]\n11006\n" +
			"[type]\n`[" + mapType + "]`\n" +
			"[blood monster]\n61010 0 440 222 0 0 0 1 0 1\n[/blood monster]\n" +
			"[blood phase time]\n" + phaseRows + "[/blood phase time]\n"
	}
	return bridgePVFSource{
		worldmap.DefaultMapList:                "17020 `dungeon/altar/endless_start.map`\n17021 `dungeon/altar/endless_middle.map`\n17022 `dungeon/altar/endless_boss.map`\n",
		"map/dungeon/altar/endless_start.map":  mapBody("endless start", "start"),
		"map/dungeon/altar/endless_middle.map": mapBody("endless middle", "normal"),
		"map/dungeon/altar/endless_boss.map":   mapBody("endless boss", "boss"),
		worldmap.DefaultDungeonList:            "11006 `altar/endless.dgn`\n",
		"dungeon/altar/endless.dgn": "[name]\n`Endless Altar`\n" +
			"[minimum required level]\n1\n[basis level]\n70\n[no fatigue]\n" +
			"[blood dungeon]\n1\n[blood max round]\n15\n" +
			"[limit inout count]\n3\n" +
			"[maze info]\n[size]\n3 1\n[greed]\n`BFE`\n" +
			"[map specification]\n`map` 0 0 17020 `map` 1 0 17021 `boss` 2 0 17022\n" +
			"[start map]\n0 0\n[boss map]\n2 0\n",
		worldmap.DefaultWorldMapList: "1 `altar.wdm`\n",
		"worldmap/altar.wdm":         "[name]\n`Altar`\n[dungeon]\n11006 -1\n[/dungeon]\n",
		currentDungeonAltarAdmissionDefinitionPath: "[price average]\n1 50 8000 51 55 10000 56 99 12000\n" +
			"[lotto cost]\n10000\n[admission increase]\n1 0 2 60 3 200 4 450 5 800 6 1350 7 1950 8 2650 9 3500 10 4550 11 4550 12 4550\n" +
			"[lotto point]\n1 1 2 3 3 3 4 3 5 4 6 4 7 5 8 5 9 7 10 7 11 8 12 8 13 10\n",
		dnfmonster.DefaultList: "1 `dummy.gob`\n",
		"monster/dummy.gob":    "[name]\n`dummy`\n[level]\n1\n[hp]\n1\n[exp]\n0\n",
	}
}

func currentDungeonUltimateAltarPVFTestSource() bridgePVFSource {
	phaseRows := ""
	for round := 0; round < 7; round++ {
		phaseRows += string(rune('0'+round)) + " 0 0 1 0 0 `A`\n"
		phaseRows += string(rune('0'+round)) + " 0 0 1 0 0 `B`\n"
	}
	return bridgePVFSource{
		worldmap.DefaultMapList: "17030 `dungeon/altar/ultimate.map`\n",
		"map/dungeon/altar/ultimate.map": "[map name]\n`ultimate`\n" +
			"[dungeon]\n11007\n" +
			"[type]\n`[start]`\n" +
			"[ultimate monster]\n56017 0 440 222 0 0 0 1 0 1\n[/ultimate monster]\n" +
			"[ultimate phase time]\n" + phaseRows + "[/ultimate phase time]\n",
		worldmap.DefaultDungeonList: "11007 `altar/ultimate.dgn`\n",
		"dungeon/altar/ultimate.dgn": "[name]\n`Ultimate Altar`\n" +
			"[minimum required level]\n1\n[basis level]\n70\n[no fatigue]\n[limit party count]\n1\n" +
			"[blood dungeon]\n2\n[blood max round]\n7\n" +
			"[limit inout count]\n3\n" +
			"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
			"[map specification]\n`map` 0 0 17030\n[start map]\n0 0\n[boss map]\n0 0\n",
		worldmap.DefaultWorldMapList: "1 `altar.wdm`\n",
		"worldmap/altar.wdm":         "[name]\n`Altar`\n[dungeon]\n11007 -1\n[/dungeon]\n",
		currentDungeonAltarAdmissionDefinitionPath: "[price average]\n1 50 8000 51 55 10000 56 99 12000\n" +
			"[lotto cost]\n10000\n[admission increase]\n1 0 2 60 3 200\n" +
			"[lotto point]\n1 1 2 3 3 5\n",
		dnfmonster.DefaultList: "1 `dummy.gob`\n",
		"monster/dummy.gob":    "[name]\n`dummy`\n[level]\n1\n[hp]\n1\n[exp]\n0\n",
	}
}
