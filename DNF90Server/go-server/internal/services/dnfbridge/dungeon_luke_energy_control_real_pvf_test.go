package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Luke phase two energy control (dungeon 5063) is the live shape that the
// u16 exclusion used to strand: dungeon/Luke/Phase2/Energy_control.dgn declares
// [clear condition][destroy object] 64062 1 and its boss map
// map/Luke_Raid/Phase2/Energy_control/74232.map declares 64062 only under
// [passive object]. The room installs no actor for that code, so the client
// kills it locally and reports an op39 nothing owned. This test proves the
// whole chain against the real Script.pvf and drives it to settlement.
func TestRealScriptPVFLukeEnergyControlDestroyObjectSettlement(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real Luke 5063 energy-control settlement smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	monsters, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	aiCharacters, err := newPVFDungeonAICharacterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: the real maze really does wait on that exact object.
	const energyControlDungeonID = 5063
	const energyControlBossMapID = 74232
	const energyControlBossObjectID = uint32(64062)
	dungeon, ok := table.FindDungeon(energyControlDungeonID)
	if !ok {
		t.Fatalf("real Script.pvf has no dungeon %d", energyControlDungeonID)
	}
	mazesWithCondition := 0
	for _, maze := range dungeon.Mazes {
		for _, condition := range maze.ClearConditions {
			if normalizeDungeonPVFSymbol(condition.Type) == "destroy object" &&
				condition.TargetID == int64(energyControlBossObjectID) && condition.Count == 1 {
				mazesWithCondition++
			}
		}
	}
	if mazesWithCondition == 0 {
		t.Fatalf("dungeon %d declares no [clear condition][destroy object] %d 1: %+v",
			energyControlDungeonID, energyControlBossObjectID, dungeon.Mazes)
	}

	// Step 2: the real boss map declares that code as a passive object and
	// never as a monster, which is why no runtime actor is ever installed.
	bossMap, ok := table.FindMap(energyControlBossMapID)
	if !ok {
		t.Fatalf("real Script.pvf has no map %d", energyControlBossMapID)
	}
	passivePlacements := 0
	for _, object := range bossMap.PassiveObjects {
		if object.ObjectID == int64(energyControlBossObjectID) {
			passivePlacements++
		}
	}
	if passivePlacements != 1 {
		t.Fatalf("boss map %d passive placements of %d = %d, want 1: %+v",
			energyControlBossMapID, energyControlBossObjectID, passivePlacements, bossMap.PassiveObjects)
	}
	for _, monster := range bossMap.Monsters {
		if monster.MonsterID == int64(energyControlBossObjectID) {
			t.Fatalf("boss map %d also declares %d as [monster]: %+v",
				energyControlBossMapID, energyControlBossObjectID, monster)
		}
	}

	// Step 3: drive the real run into that boss room and report the op39 the
	// live client sent.
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "63",
		AccountID:   "account-1",
		Name:        "EnergyControlSmoke",
		Job:         "2",
		Level:       90,
		Stats:       map[string]int64{"fatigue": 200},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:                 options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		initialEquipmentArchive: archive,
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCharacters,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 1, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "real-luke-energy-control-test",
		selectedCharacterID: 63,
	}
	runtime, startScene, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: energyControlDungeonID, Difficulty: 0},
	)
	if err != nil {
		t.Fatalf("prepare real dungeon %d runtime: %v", energyControlDungeonID, err)
	}
	session.dungeon.runtime = runtime
	if runtime.BossCoordinate != (worldmap.RoomCoordinate{X: 1, Y: 1}) {
		t.Fatalf("real dungeon %d boss coordinate=%s want 1:1",
			energyControlDungeonID, runtime.BossCoordinate)
	}
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		runtime.Dungeon.ID,
		runtime.MazeIndex,
		func(choice worldmap.DungeonMapChoice) (int64, error) { return choice.Candidates[0].ID, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	queue := []worldmap.RoomCoordinate{startScene.Coordinate}
	visited := map[worldmap.RoomCoordinate]bool{startScene.Coordinate: true}
	previous := make(map[worldmap.RoomCoordinate]worldmap.RoomCoordinate)
	for len(queue) != 0 && !visited[runtime.BossCoordinate] {
		current := queue[0]
		queue = queue[1:]
		room, ok := topology.Room(current)
		if !ok {
			t.Fatalf("real dungeon %d path room missing: %s", energyControlDungeonID, current)
		}
		for _, neighbor := range room.Neighbors {
			if visited[neighbor.Coordinate] {
				continue
			}
			visited[neighbor.Coordinate] = true
			previous[neighbor.Coordinate] = current
			queue = append(queue, neighbor.Coordinate)
		}
	}
	if !visited[runtime.BossCoordinate] {
		t.Fatalf("real dungeon %d boss room %s unreachable from %s",
			energyControlDungeonID, runtime.BossCoordinate, startScene.Coordinate)
	}
	path := []worldmap.RoomCoordinate{runtime.BossCoordinate}
	for path[len(path)-1] != startScene.Coordinate {
		path = append(path, previous[path[len(path)-1]])
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}

	clearCurrentRoom := func() {
		t.Helper()
		room := runtime.Room.Snapshot()
		actorsPlanned := false
		for _, monster := range room.Monsters {
			actorsPlanned = actorsPlanned || monster.State == runtimeDungeonMonsterPlanned
		}
		for _, actor := range room.ExtendedActors {
			actorsPlanned = actorsPlanned || actor.State == runtimeDungeonMonsterPlanned
		}
		if actorsPlanned {
			if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
				t.Fatalf("announce real dungeon %d room: %v", energyControlDungeonID, err)
			}
		}
		scene := runtime.Session.Snapshot().Scene
		for _, reference := range scene.BlockingHostiles {
			var objectKey uint32
			for candidate, bound := range scene.RuntimeObjects {
				if bound == reference {
					objectKey = candidate
					break
				}
			}
			if objectKey == 0 {
				t.Fatalf("real dungeon %d blocker has no runtime object: %+v", energyControlDungeonID, reference)
			}
			if _, _, defeated := currentDungeonDefeatedActor(runtime, scene, objectKey); defeated {
				continue
			}
			if _, _, err := runtime.Room.CommitActorDeathReport(objectKey, runtime.Session); err != nil {
				t.Fatalf("defeat real dungeon %d blocker key=%d reference=%+v: %v",
					energyControlDungeonID, objectKey, reference, err)
			}
			scene = runtime.Session.Snapshot().Scene
		}
		if !runtime.Session.Snapshot().Scene.Cleared {
			t.Fatalf("real dungeon %d room did not clear: %+v",
				energyControlDungeonID, runtime.Session.Snapshot().Scene)
		}
	}

	for _, target := range path[1:] {
		clearCurrentRoom()
		body := make([]byte, dungeoncmd.MoveMapRequestSize)
		body[0] = byte(target.X)
		body[1] = byte(target.Y)
		session.conn.(*bufferConn).write.Reset()
		if err := service.handleDungeonMoveMap(session, body); err != nil {
			t.Fatalf("move real dungeon %d to %s: %v", energyControlDungeonID, target, err)
		}
		if scene := runtime.Session.Snapshot().Scene; scene.Coordinate != target {
			t.Fatalf("real dungeon %d move committed %s want %s", energyControlDungeonID, scene.Coordinate, target)
		}
	}
	scene := runtime.Session.Snapshot().Scene
	if !scene.Boss || scene.Map.Map.ID != energyControlBossMapID || !scene.Cleared {
		t.Fatalf("real energy-control boss scene=%+v", scene)
	}
	if len(scene.PassiveObjects) != 1 || scene.PassiveObjects[0].ObjectID != int64(energyControlBossObjectID) {
		t.Fatalf("real energy-control boss passive objects=%+v", scene.PassiveObjects)
	}
	if runtime.Room.HostsRuntimeActor(energyControlBossObjectID) {
		t.Fatalf("energy-control object %d was installed as a runtime actor", energyControlBossObjectID)
	}
	if source := currentDungeonDestroyObjectClearConditionSource(runtime, scene, energyControlBossObjectID); source == "" {
		t.Fatal("real energy-control clear condition was not recognized")
	}

	conn := session.conn.(*bufferConn)
	conn.write.Reset()
	if err := service.handleDungeonMonsterDeath(
		session,
		tutorialScopeVariableZeroCombatDeathBody(energyControlBossObjectID),
	); err != nil {
		t.Fatal(err)
	}
	if !runtime.ordinaryFinalRoomClearAccepted || !runtime.settlementEntrySent {
		t.Fatalf("energy-control clear did not enter settlement: runtime=%+v", runtime)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunCompleted {
		t.Fatalf("energy-control run status=%s want completed", status)
	}
	packets := splitAllFinishBridgePackets(t, conn.write.Bytes())
	if len(packets) < 2 ||
		packets[len(packets)-2].Header.MsgID != uint16(dnfenum.CmdPacketNotifyBossDieCheck) ||
		packets[len(packets)-1].Header.MsgID != currentDungeonSettlementEntryMsgID {
		t.Fatalf("final packet order=%+v", packets)
	}
	for _, packet := range packets {
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketNotifyDieMonster) {
			t.Fatalf("in-range passive object emitted an op38 body=%x", packet.Body)
		}
	}
}
