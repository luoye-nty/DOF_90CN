package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFQuest3344StoryMazeDeclarations(t *testing.T) {
	source, table, resolver := loadRealStoryPVF(t)
	monsters, err := newPVFDungeonMonsterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeon(51)
	if !ok || len(dungeon.Mazes) <= 4 {
		t.Fatalf("real dungeon51 found=%t mazes=%d", ok, len(dungeon.Mazes))
	}
	maze := dungeon.Mazes[4]
	if !realMazeConnectsQuest(maze, 3344) {
		t.Fatalf("real dungeon51 maze4 quest connection=%v, want quest3344", maze.QuestConnection)
	}
	stages, err := resolver.StoryStages(51, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantCoordinates := []worldmap.RoomCoordinate{{X: 3, Y: 0}, {X: 4, Y: 0}, {X: 3, Y: 0}}
	wantMapIDs := []int64{76327, 76328, 76329}
	if len(stages) != len(wantMapIDs) {
		t.Fatalf("real quest3344 story stages=%+v", stages)
	}
	for index := range wantMapIDs {
		resolved, descriptor, resolveErr := resolver.ResolveStoryStage(51, 4, index)
		if resolveErr != nil || resolved.Map.ID != wantMapIDs[index] ||
			descriptor.MapID != wantMapIDs[index] || descriptor.Coordinate != wantCoordinates[index] {
			t.Fatalf("real quest3344 stage%d resolved=%+v descriptor=%+v err=%v", index, resolved, descriptor, resolveErr)
		}
	}
	// Nonora (75072) sits on [monster team] 0 in the final stage 76329: the
	// server must announce her to the client as a non-room-clearing actor.
	finalStage, _, err := resolver.ResolveStoryStage(51, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	nonoraIndex := -1
	for index, spawn := range finalStage.Map.Monsters {
		if spawn.MonsterID == 75072 {
			nonoraIndex = index
			break
		}
	}
	if nonoraIndex < 0 || nonoraIndex >= len(finalStage.Map.MonsterTeam) || finalStage.Map.MonsterTeam[nonoraIndex] != 0 {
		t.Fatalf("real quest3344 final-stage Nonora index=%d teams=%v monsters=%+v",
			nonoraIndex, finalStage.Map.MonsterTeam, finalStage.Map.Monsters)
	}
	definition, found, findErr := monsters.Find(75072)
	if findErr != nil || !found || definition.Name != "诺诺拉" {
		t.Fatalf("real quest3344 Nonora definition=%+v found=%t err=%v", definition, found, findErr)
	}
	finalScene := worldmap.DungeonRoomScene{
		Coordinate: stages[2].Coordinate,
		Map:        finalStage,
		Monsters:   append([]worldmap.MonsterSpawn(nil), finalStage.Map.Monsters...),
	}
	room, _, err := newRuntimeDungeonRoom(finalScene, monsters, firstDungeonMonsterObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := room.Snapshot()
	if snapshot.Monsters[nonoraIndex].RoomClearSkipByte != 1 {
		t.Fatalf("real quest3344 Nonora room-clear skip byte=%d want=1", snapshot.Monsters[nonoraIndex].RoomClearSkipByte)
	}
	baseAtFirstStage, err := resolver.Resolve(worldmap.ResolveRequest{DungeonID: 51, MazeIndex: 4, X: 3, Y: 0})
	if err != nil || baseAtFirstStage.Map.ID != 76327 {
		t.Fatalf("real quest3344 first stage coordinate base=%+v err=%v", baseAtFirstStage, err)
	}
	baseAtRepeatedStage, err := resolver.Resolve(worldmap.ResolveRequest{DungeonID: 51, MazeIndex: 4, X: 4, Y: 0})
	if err != nil || baseAtRepeatedStage.Map.ID != 76326 {
		t.Fatalf("real quest3344 repeated stage coordinate base=%+v err=%v", baseAtRepeatedStage, err)
	}
	candidates, err := resolver.ResolveCandidates(worldmap.ResolveRequest{DungeonID: 51, MazeIndex: 4, X: 4, Y: 0})
	if err != nil || len(candidates) != 1 || candidates[0].Map.ID != 76326 {
		t.Fatalf("real quest3344 ordinary candidates leaked story stages=%+v err=%v", candidates, err)
	}
}

func TestRealScriptPVFQuest3367RequiresHostileAIBossAndDummyBossDeaths(t *testing.T) {
	_, table, _ := loadRealStoryPVF(t)
	dungeon, ok := table.FindDungeon(7123)
	if !ok || len(dungeon.Mazes) <= 37 {
		t.Fatalf("real dungeon7123 found=%t mazes=%d", ok, len(dungeon.Mazes))
	}
	if !realMazeConnectsQuest(dungeon.Mazes[37], 3367) {
		t.Fatalf("real dungeon7123 maze37 quest connection=%v, want quest3367", dungeon.Mazes[37].QuestConnection)
	}
	mapValue, ok := table.FindMap(53546)
	if !ok {
		t.Fatal("real quest3367 final map53546 is missing")
	}
	dummyBosses := 0
	for index, spawn := range mapValue.Monsters {
		blocking := index >= len(mapValue.MonsterTeam) || mapValue.MonsterTeam[index] != 0
		if blocking && normalizeDungeonPVFSymbol(spawn.Rank) == "dummy" &&
			normalizeDungeonPVFSymbol(spawn.SuffixMarker) == "boss" {
			dummyBosses++
		}
	}
	hostileAIBosses := 0
	for _, actor := range mapValue.AICharacters {
		if normalizeDungeonPVFSymbol(actor.Faction) == "monster" && normalizeDungeonPVFSymbol(actor.AIType) == "boss" {
			hostileAIBosses++
		}
	}
	if dummyBosses != 1 || hostileAIBosses != 1 {
		t.Fatalf("real quest3367 map53546 dummy_bosses=%d hostile_AI_bosses=%d monsters=%+v teams=%v AI=%+v",
			dummyBosses, hostileAIBosses, mapValue.Monsters, mapValue.MonsterTeam, mapValue.AICharacters)
	}
}

func TestRealScriptPVFQuest3352BuildsOwnedMonsterWithoutDuplicatingMapObject(t *testing.T) {
	source, table, _ := loadRealStoryPVF(t)
	mapValue, ok := table.FindMap(76384)
	if !ok {
		t.Fatal("real quest3352 layered map76384 is missing")
	}
	var ownerDungeon worldmap.Dungeon
	foundQuestMaze := false
	for _, dungeon := range table.Dungeons() {
		for _, maze := range dungeon.Mazes {
			if realMazeConnectsQuest(maze, 3352) {
				ownerDungeon = dungeon
				foundQuestMaze = true
				break
			}
		}
		if foundQuestMaze {
			break
		}
	}
	if !foundQuestMaze {
		t.Fatal("real quest3352 is not connected to a maze")
	}
	if len(mapValue.Monsters) != 1 || mapValue.Monsters[0].MonsterID != 1 {
		t.Fatalf("real map76384 ordinary monsters=%+v, want off-screen monster1", mapValue.Monsters)
	}
	if len(mapValue.SpecialPassiveObjects) != 1 || mapValue.SpecialPassiveObjects[0].ObjectID != 1112 ||
		len(mapValue.SpecialPassiveObjects[0].Spawns) != 1 ||
		mapValue.SpecialPassiveObjects[0].Spawns[0].Code != 56716 ||
		normalizeDungeonPVFSymbol(mapValue.SpecialPassiveObjects[0].Spawns[0].Kind) != "monster" {
		t.Fatalf("real map76384 special passive objects=%+v", mapValue.SpecialPassiveObjects)
	}
	monsterCatalog, err := newPVFDungeonMonsterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRuntimeDungeonExtendedActors(worldmap.DungeonRoomScene{
		Coordinate:            worldmap.RoomCoordinate{X: 0, Y: 0},
		Map:                   worldmap.ResolvedMap{Map: mapValue},
		SpecialPassiveObjects: mapValue.SpecialPassiveObjects,
	}, monsterCatalog, nil, ownerDungeon.Metadata.BasisLevel, 403)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actors) != 1 || plan.NextObjectKey != 404 {
		t.Fatalf("real map76384 extended actor plan=%+v", plan)
	}
	child := plan.Actors[0]
	if child.ObjectKey != 403 || child.Packet.Code != 56716 || child.Packet.Type != 0 ||
		child.Packet.Flag0 != 1 || child.Packet.Flag1 != 0 {
		t.Fatalf("real map76384 child=%+v", child)
	}
	if len(plan.Diagnostics) != 1 || !strings.Contains(plan.Diagnostics[0], "MapObject/Obstacle/ElevatorSummon.obj") {
		t.Fatalf("real map76384 diagnostics=%v", plan.Diagnostics)
	}
}

func TestRealScriptPVFQuest3425RetainsDefinedActorsAndSkipsMissingFriendlyAI(t *testing.T) {
	source, table, _ := loadRealStoryPVF(t)
	dungeon, ok := table.FindDungeon(61)
	if !ok || len(dungeon.Mazes) <= 5 {
		t.Fatalf("real dungeon61 found=%t mazes=%d", ok, len(dungeon.Mazes))
	}
	if !realMazeConnectsQuest(dungeon.Mazes[5], 3425) {
		t.Fatalf("real dungeon61 maze5 quest connection=%v, want quest3425", dungeon.Mazes[5].QuestConnection)
	}
	mapValue, ok := table.FindMap(76331)
	if !ok {
		t.Fatal("real quest3425 final map76331 is missing")
	}
	wantMonsters := []int64{75067, 75072, 75099}
	if len(mapValue.Monsters) != len(wantMonsters) {
		t.Fatalf("real quest3425 map76331 monsters=%+v", mapValue.Monsters)
	}
	for index, wantID := range wantMonsters {
		if mapValue.Monsters[index].MonsterID != wantID {
			t.Fatalf("real quest3425 map76331 monster[%d]=%+v want_id=%d", index, mapValue.Monsters[index], wantID)
		}
	}
	if len(mapValue.NPCs) != 1 || mapValue.NPCs[0].NPCID != 1000 ||
		len(mapValue.AICharacters) != 1 || mapValue.AICharacters[0].Code != 6001 ||
		normalizeDungeonPVFSymbol(mapValue.AICharacters[0].Faction) != "character" {
		t.Fatalf("real quest3425 map76331 NPC=%+v AI=%+v", mapValue.NPCs, mapValue.AICharacters)
	}

	monsterCatalog, err := newPVFDungeonMonsterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	aiCatalog, err := newPVFDungeonAICharacterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRuntimeDungeonExtendedActors(worldmap.DungeonRoomScene{
		Coordinate:            worldmap.RoomCoordinate{X: 0, Y: 0},
		Map:                   worldmap.ResolvedMap{Map: mapValue},
		AICharacters:          mapValue.AICharacters,
		SpecialPassiveObjects: mapValue.SpecialPassiveObjects,
	}, monsterCatalog, aiCatalog, dungeon.Metadata.BasisLevel, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range plan.Actors {
		if actor.Kind == runtimeDungeonActorAICharacter && actor.Packet.Code == 6001 {
			t.Fatalf("missing friendly AI6001 was materialized: %+v", actor)
		}
	}
	foundDiagnostic := false
	for _, diagnostic := range plan.Diagnostics {
		if strings.Contains(diagnostic, "skip non-hostile AI character") && strings.Contains(diagnostic, "6001") {
			foundDiagnostic = true
			break
		}
	}
	if !foundDiagnostic {
		t.Fatalf("missing friendly AI6001 diagnostic=%v", plan.Diagnostics)
	}
}

func loadRealStoryPVF(t *testing.T) (dnfpvf.Source, *worldmap.Table, *worldmap.Resolver) {
	t.Helper()
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify current story dungeon declarations")
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
	return archive, table, resolver
}

func realMazeConnectsQuest(maze worldmap.Maze, questID int64) bool {
	for _, candidate := range maze.QuestConnection {
		if candidate == questID {
			return true
		}
	}
	return false
}
