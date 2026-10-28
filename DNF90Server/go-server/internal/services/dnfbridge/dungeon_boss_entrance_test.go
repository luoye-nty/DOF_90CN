package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonBossEntranceConditionCountsDefeatedTargetsAcrossRoomsOnce(t *testing.T) {
	roomA := bossEntranceTestRoom(69201, runtimeDungeonMonsterDefeated)
	roomB := bossEntranceTestRoom(69201, runtimeDungeonMonsterDefeated)
	roomC := bossEntranceTestRoom(69210, runtimeDungeonMonsterDefeated)
	roomD := bossEntranceTestRoom(69210, runtimeDungeonMonsterAnnounced)
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{Mazes: []worldmap.Maze{{
			BossRoomEntranceCondition: &worldmap.BossRoomEntranceCondition{
				Action:        "clear action",
				TimeCondition: []int64{1, 0},
				HuntMonsters: []worldmap.BossRoomEntranceHuntMonster{
					{TargetID: 69201, Count: 2},
					{TargetID: 69210, Count: 2},
				},
			},
		}}},
		MazeIndex: 0,
		Room:      roomD,
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
			{X: 1, Y: 1, MapID: 17061}: {Room: roomA},
			{X: 3, Y: 1, MapID: 17063}: {Room: roomB},
			{X: 1, Y: 3, MapID: 17067}: {Room: roomC},
			{X: 3, Y: 3, MapID: 17065}: {Room: roomD},
			// A second cache key for the same room must not double-count it.
			{X: 9, Y: 9, MapID: 17065}: {Room: roomD},
		},
	}

	progress, found := currentDungeonBossEntranceConditionProgress(runtime)
	if !found || progress.Ready || len(progress.Targets) != 2 ||
		progress.Targets[0] != (currentDungeonBossEntranceHuntProgress{TargetID: 69201, Required: 2, Defeated: 2}) ||
		progress.Targets[1] != (currentDungeonBossEntranceHuntProgress{TargetID: 69210, Required: 2, Defeated: 1}) {
		t.Fatalf("before final tower progress=%+v found=%t", progress, found)
	}

	roomD.mu.Lock()
	roomD.monsters[0].State = runtimeDungeonMonsterDefeated
	roomD.mu.Unlock()
	progress, found = currentDungeonBossEntranceConditionProgress(runtime)
	if !found || !progress.Ready || progress.Targets[1].Defeated != 2 {
		t.Fatalf("after final tower progress=%+v found=%t", progress, found)
	}
}

func TestCurrentDungeonCompleteConditionPassGateMatchesCurrentEXEAndPublishesOnce(t *testing.T) {
	wantBody := []byte{0, 0, 0, 0, 0}
	if body := buildCurrentDungeonCompleteConditionPassGateBody(); !bytes.Equal(body, wantBody) {
		t.Fatalf("body=%x want=%x", body, wantBody)
	}

	roomA := bossEntranceTestRoom(69201, runtimeDungeonMonsterDefeated)
	roomB := bossEntranceTestRoom(69201, runtimeDungeonMonsterDefeated)
	roomC := bossEntranceTestRoom(69210, runtimeDungeonMonsterDefeated)
	roomD := bossEntranceTestRoom(69210, runtimeDungeonMonsterAnnounced)
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{ID: 2005, Mazes: []worldmap.Maze{{
			BossRoomEntranceCondition: &worldmap.BossRoomEntranceCondition{
				Action: "clear action",
				HuntMonsters: []worldmap.BossRoomEntranceHuntMonster{
					{TargetID: 69201, Count: 2},
					{TargetID: 69210, Count: 2},
				},
			},
		}}},
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
			{X: 1, Y: 1, MapID: 17061}: {Room: roomA},
			{X: 3, Y: 1, MapID: 17063}: {Room: roomB},
			{X: 1, Y: 3, MapID: 17067}: {Room: roomC},
			{X: 3, Y: 3, MapID: 17065}: {Room: roomD},
		},
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	service := &Service{}
	if err := service.publishCurrentDungeonBossEntranceGateAfterDeathLocked(session, runtime); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 || runtime.bossEntranceConditionComplete || runtime.bossEntranceGateNotificationSent {
		t.Fatalf("premature publish bytes=%x complete=%t sent=%t", connection.write.Bytes(), runtime.bossEntranceConditionComplete, runtime.bossEntranceGateNotificationSent)
	}

	roomD.mu.Lock()
	roomD.monsters[0].State = runtimeDungeonMonsterDefeated
	roomD.mu.Unlock()
	if err := service.publishCurrentDungeonBossEntranceGateAfterDeathLocked(session, runtime); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentDungeonCompleteConditionPassGateMsgID ||
		!bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("packet class=%d msg=%d body=%x rest=%x", packet.Header.Classification, packet.Header.MsgID, packet.Body, rest)
	}
	if !runtime.bossEntranceConditionComplete || !runtime.bossEntranceGateNotificationSent {
		t.Fatalf("complete=%t sent=%t", runtime.bossEntranceConditionComplete, runtime.bossEntranceGateNotificationSent)
	}
	written := connection.write.Len()
	if err := service.publishCurrentDungeonBossEntranceGateAfterDeathLocked(session, runtime); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != written {
		t.Fatalf("duplicate publish wrote=%x", connection.write.Bytes()[written:])
	}
}

func TestCurrentDungeonBossEntranceConditionRejectsMissingOrUnsupportedGate(t *testing.T) {
	for _, runtime := range []*runtimeDungeonState{
		nil,
		{},
		{Dungeon: worldmap.Dungeon{Mazes: []worldmap.Maze{{}}}},
		{Dungeon: worldmap.Dungeon{Mazes: []worldmap.Maze{{BossRoomEntranceCondition: &worldmap.BossRoomEntranceCondition{
			Action: "future action", HuntMonsters: []worldmap.BossRoomEntranceHuntMonster{{TargetID: 1, Count: 1}},
		}}}}},
	} {
		if progress, found := currentDungeonBossEntranceConditionProgress(runtime); found {
			t.Fatalf("unsupported runtime=%+v progress=%+v", runtime, progress)
		}
	}
}

func bossEntranceTestRoom(monsterID int64, state runtimeDungeonMonsterState) *runtimeDungeonRoom {
	return &runtimeDungeonRoom{monsters: []runtimeDungeonMonster{{
		Spawn: worldmap.MonsterSpawn{MonsterID: monsterID},
		State: state,
	}}}
}
