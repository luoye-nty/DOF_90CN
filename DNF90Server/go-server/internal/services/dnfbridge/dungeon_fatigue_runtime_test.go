package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestFatigueBatteryRoomConsumptionAndMonsterExperience(t *testing.T) {
	for _, test := range []struct {
		name                 string
		battery              int64
		exempt               bool
		wantBattery, wantEXP int64
		wantCounter          uint16
	}{
		{"charged", 2, false, 1, 260, 2},
		{"last paid room", 1, false, 0, 260, 1},
		{"empty", 0, false, 0, 200, 0},
		{"exempt", 1, true, 1, 200, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
			runtime.Character.Stats["fatigue_battery"] = test.battery
			runtime.Dungeon.Metadata.NoFatigue = test.exempt
			ctx := context.Background()
			if err := repositories.Character.Save(ctx, runtime.Character); err != nil {
				t.Fatal(err)
			}
			connection := &bufferConn{}
			session := &gameSession{conn: connection, selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime}}
			if err := service.consumeCurrentDungeonRoomFatigue(session, runtime, "test_battery"); err != nil {
				t.Fatal(err)
			}
			if !test.exempt {
				packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
				if packet.Header.MsgID != currentFatigueMsgID || len(rest) != 0 || binary.LittleEndian.Uint16(packet.Body[6:8]) != test.wantCounter {
					t.Fatalf("fatigue packet=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
				}
			}
			connection.write.Reset()
			if err := service.consumeCurrentDungeonRoomFatigue(session, runtime, "test_replay"); err != nil || connection.write.Len() != 0 {
				t.Fatalf("replayed consumption err=%v bytes=%x", err, connection.write.Bytes())
			}
			monster := runtime.Room.Snapshot().Monsters[0]
			request := make([]byte, dungeoncmd.DieMonsterRequestSize)
			binary.LittleEndian.PutUint32(request[0:4], monster.ObjectKey)
			binary.LittleEndian.PutUint16(request[4:6], currentSceneActorObjectKey(99))
			if err := service.handleDungeonMonsterDeath(session, request); err != nil {
				t.Fatal(err)
			}
			_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			state, _ := splitGameServerUpperPacket(t, rest)
			wantBonus := uint32(test.wantEXP - 200)
			if state.Header.MsgID != currentDungeonCharacterStateMsgID || len(state.Body) != currentFinishLoadingCharacterStateBodySize ||
				binary.LittleEndian.Uint32(state.Body[21:25]) != wantBonus || binary.LittleEndian.Uint32(state.Body[1:5]) != uint32(test.wantEXP) {
				t.Fatalf("kill experience packet=%+v body=%x want bonus=%d total=%d", state.Header, state.Body, wantBonus, test.wantEXP)
			}
			if runtime.settlementMonsterFatigueBatteryBonus != wantBonus {
				t.Fatalf("settlement battery accumulator=%d want=%d", runtime.settlementMonsterFatigueBatteryBonus, wantBonus)
			}
			connection.write.Reset()
			if err := service.handleDungeonMonsterDeath(session, request); err != nil || connection.write.Len() != 0 || runtime.settlementMonsterFatigueBatteryBonus != wantBonus {
				t.Fatalf("duplicate kill changed presentation/bonus: err=%v bytes=%x bonus=%d", err, connection.write.Bytes(), runtime.settlementMonsterFatigueBatteryBonus)
			}
			stored, found, err := repositories.Character.Load(ctx, "99")
			if err != nil || !found || stored.Stats["fatigue_battery"] != test.wantBattery || stored.Stats["exp"] != test.wantEXP {
				t.Fatalf("stored=%+v found=%t err=%v", stored, found, err)
			}
			// Reselecting after the last paid room must clear the native counter.
			if test.battery == 1 && !test.exempt && binary.LittleEndian.Uint16(buildCurrentFatigueBody(stored, true)[6:8]) != 0 {
				t.Fatal("exhausted blessing survived character reload")
			}
		})
	}
}

func consumeOptionalCurrentDungeonFatigueRefreshForTest(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) == 0 {
		return body
	}
	packet, trailing := splitGameServerUpperPacket(t, body)
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentFatigueMsgID {
		return body
	}
	if len(packet.Body) != 10 {
		t.Fatalf("fatigue refresh body=%x", packet.Body)
	}
	return trailing
}

func assertCurrentDungeonFatiguePacket(t *testing.T, runtime *runtimeDungeonState, stream []byte) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentFatigueMsgID ||
		!bytes.Equal(packet.Body, buildCurrentFatigueBody(runtime.Character, true)) {
		t.Fatalf("fatigue packet header=%+v body=%x want=%x rest=%x", packet.Header, packet.Body, buildCurrentFatigueBody(runtime.Character, true), rest)
	}
	return rest
}

func assertOptionalCurrentDungeonFatiguePacket(t *testing.T, runtime *runtimeDungeonState, stream []byte) {
	t.Helper()
	if len(stream) == 0 {
		return
	}
	if rest := assertCurrentDungeonFatiguePacket(t, runtime, stream); len(rest) != 0 {
		t.Fatalf("fatigue packet trailing bytes=%x", rest)
	}
}

func TestConsumeCurrentDungeonRoomFatiguePersistsAndSendsOnce(t *testing.T) {
	service, runtime := prepareSyntheticMoveRuntime(t, false)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	repositories, _ := service.repositoryGroup()
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character = dnfrepo.CloneCharacter(character)
	character.Stats["fatigue_limit"] = 156
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	runtime.Character = dnfrepo.CloneCharacter(character)
	// 推荐副本判据读进图等级快照，测试直接替换 runtime.Character 时必须同步刷新。
	runtime.entryCharacterLevel = character.Level
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime}}

	if err := service.consumeCurrentDungeonRoomFatigue(session, runtime, "test_initial_room"); err != nil {
		t.Fatal(err)
	}
	if rest := assertCurrentDungeonFatiguePacket(t, runtime, connection.write.Bytes()); len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes=%x", rest)
	}
	stored, _, err := repositories.Character.Load(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Stats["fatigue"] != 99 || stored.Stats["fatigue_display_update"] != 57 ||
		runtime.Character.Stats["fatigue"] != 99 {
		t.Fatalf("stored=%+v runtime=%+v", stored.Stats, runtime.Character.Stats)
	}
	visit, err := runtime.currentDungeonRoomVisit(runtime.Session.Snapshot().Scene)
	if err != nil || !visit.FatigueConsumed || visit.FatigueConsumedAmount != 1 {
		t.Fatalf("visit=%+v err=%v", visit, err)
	}
	beforeBytes := connection.write.Len()
	if err := service.consumeCurrentDungeonRoomFatigue(session, runtime, "test_replay"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != beforeBytes {
		t.Fatalf("replay emitted another fatigue packet: before=%d after=%d", beforeBytes, connection.write.Len())
	}
}

func TestConsumeCurrentDungeonRoomFatigueHonorsExemptions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimeDungeonState)
	}{
		{name: "practice", mutate: func(runtime *runtimeDungeonState) { runtime.ProtocolMode = currentDungeonProtocolModePractice }},
		{name: "no fatigue", mutate: func(runtime *runtimeDungeonState) { runtime.Dungeon.Metadata.NoFatigue = true }},
		{name: "enter without fatigue", mutate: func(runtime *runtimeDungeonState) {
			if runtime.Dungeon.Metadata.Flags == nil {
				runtime.Dungeon.Metadata.Flags = make(map[string]bool)
			}
			runtime.Dungeon.Metadata.Flags["enter without fatigue"] = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, runtime := prepareSyntheticMoveRuntime(t, false)
			service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
			test.mutate(runtime)
			connection := &bufferConn{}
			session := &gameSession{conn: connection, selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime}}
			if err := service.consumeCurrentDungeonRoomFatigue(session, runtime, "test_exempt"); err != nil {
				t.Fatal(err)
			}
			if connection.write.Len() != 0 {
				t.Fatalf("exempt room emitted bytes=%x", connection.write.Bytes())
			}
			repositories, _ := service.repositoryGroup()
			stored, _, err := repositories.Character.Load(context.Background(), "99")
			if err != nil || stored.Stats["fatigue"] != 100 {
				t.Fatalf("stored=%+v err=%v", stored.Stats, err)
			}
		})
	}
}
