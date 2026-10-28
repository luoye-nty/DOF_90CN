package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFRiftDropSources keeps the two rift dungeons on the same
// real-PVF audit path as ordinary dungeon drops.  In particular, Magic Rift
// (316) is not a Dimension Crack dungeon, so it must not borrow the latter's
// special material table merely to make a drop appear.
func TestRealScriptPVFRiftDropSources(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to audit real rift drops")
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
	drops, err := monsters.DropCatalog()
	if err != nil {
		t.Fatal(err)
	}
	rules, err := currentDungeonMonsterDropRules(monsters)
	if err != nil {
		t.Fatal(err)
	}
	gold, err := currentDungeonGoldReferences(monsters)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		id   int64
		name string
	}{
		{id: 316, name: "Magic Rift"},
		{id: 7527, name: "Time Rift (Time Gate)"},
		{id: 7539, name: "Time Rift (Metro Center)"},
	} {
		dungeon, found := table.FindDungeon(want.id)
		if !found {
			t.Fatalf("%s dungeon=%d missing", want.name, want.id)
		}
		if want.id == 316 {
			rows := dungeon.Metadata.SpecialPassiveObjectItems
			if len(rows) != 1 || rows[0].Index != 0 || rows[0].ItemID != 3276 || rows[0].DropRate != 5000 {
				t.Fatalf("Magic Rift dungeon-wide passive drops=%+v", rows)
			}
			for mazeIndex, maze := range dungeon.Mazes {
				if len(maze.SpecialPassiveObjectItems) != 0 {
					t.Fatalf("Magic Rift maze=%d incorrectly owns dungeon-wide passive drops=%+v", mazeIndex, maze.SpecialPassiveObjectItems)
				}
			}
		}
		var rooms, monstersSeen, randomAttempts, fixedAttempts, explicitPools, probabilityRows, itemReferenceRows, goldRows int
		passiveRewardRooms := 0
		var representativeNormal, representativeBoss *worldmap.MonsterSpawn
		for mazeIndex := range dungeon.Mazes {
			layout, layoutErr := worldmap.BuildDungeonLayout(resolver, dungeon.ID, mazeIndex, func(choice worldmap.DungeonMapChoice) (int64, error) {
				return choice.Candidates[0].ID, nil
			})
			if layoutErr != nil {
				t.Fatalf("%s maze=%d layout: %v", want.name, mazeIndex, layoutErr)
			}
			for _, room := range layout.Rooms() {
				if room.Map == nil {
					continue
				}
				rooms++
				scene := worldmap.DungeonRoomScene{
					Coordinate:            room.Coordinate,
					Map:                   *room.Map,
					PassiveObjects:        room.Map.Map.PassiveObjects,
					SpecialPassiveObjects: room.Map.Map.SpecialPassiveObjects,
				}
				owners, ownersErr := currentDungeonPassiveObjectGroupOwners(scene, monsters)
				if ownersErr != nil {
					t.Fatalf("%s maze=%d map=%d passive reward groups: %v", want.name, mazeIndex, room.Map.Map.ID, ownersErr)
				}
				if len(owners[0]) > 0 {
					passiveRewardRooms++
				}
				if want.id == 316 && (room.Boss || len(scene.SpecialPassiveObjects) > 0) {
					t.Logf("Magic Rift maze=%d map=%d path=%q boss=%t passive=%d special=%d event_positions=%d owners0=%v", mazeIndex, room.Map.Map.ID, room.Map.Map.Path, room.Boss, len(scene.PassiveObjects), len(scene.SpecialPassiveObjects), len(room.Map.Map.EventMonsterPositions), owners[0])
				}
				for _, spawn := range room.Map.Map.Monsters {
					spawnCopy := spawn
					if representativeNormal == nil && spawn.Rank == "[normal]" {
						representativeNormal = &spawnCopy
					}
					if representativeBoss == nil && spawn.Rank == "[boss]" {
						representativeBoss = &spawnCopy
					}
					monstersSeen++
					level, levelErr := currentDungeonMonsterLevel(spawn, dungeon.Metadata.BasisLevel)
					if levelErr != nil {
						t.Fatalf("%s maze=%d map=%d monster=%d level: %v", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, levelErr)
					}
					if _, typeErr := currentDungeonMonsterType(spawn.Rank); typeErr != nil {
						t.Fatalf("%s maze=%d map=%d monster=%d rank: %v", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, typeErr)
					}
					if _, exists, findErr := monsters.Find(spawn.MonsterID); findErr != nil || !exists {
						t.Fatalf("%s maze=%d map=%d monster=%d definition exists=%t err=%v", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, exists, findErr)
					}
					pool, poolErr := drops.MonsterPool(spawn.MonsterID)
					if poolErr != nil {
						t.Fatalf("%s maze=%d map=%d monster=%d pool: %v", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, poolErr)
					}
					if len(pool) > 0 {
						explicitPools++
						for _, item := range pool {
							if _, resolveErr := drops.ResolveItem(item.ItemID); resolveErr != nil {
								t.Fatalf("%s maze=%d map=%d monster=%d explicit item=%d: %v", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, item.ItemID, resolveErr)
							}
						}
					}
					random, randomErr := currentDungeonMapDropAttempts(spawn.RandomDropCount, "random")
					fixed, fixedErr := currentDungeonMapDropAttempts(spawn.FixedDropCount, "specified")
					if randomErr != nil || fixedErr != nil {
						t.Fatalf("%s maze=%d map=%d monster=%d attempts random=%v fixed=%v", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, randomErr, fixedErr)
					}
					randomAttempts += random
					fixedAttempts += fixed
					if _, probabilityFound := rules.Probability(int64(level)); probabilityFound {
						probabilityRows++
						if _, goldFound := gold[int64(level)]; !goldFound {
							t.Fatalf("%s maze=%d map=%d monster=%d level=%d has item drop probability but no gold reference", want.name, mazeIndex, room.Map.Map.ID, spawn.MonsterID, level)
						}
						goldRows++
					}
					if _, referenceFound := rules.ItemReference(int64(level)); referenceFound {
						itemReferenceRows++
					}
				}
			}
		}
		if rooms == 0 || monstersSeen == 0 {
			t.Fatalf("%s has no resolved monster rooms: rooms=%d monsters=%d", want.name, rooms, monstersSeen)
		}
		for rank, spawn := range map[string]*worldmap.MonsterSpawn{
			"normal": representativeNormal,
			"boss":   representativeBoss,
		} {
			if spawn == nil {
				t.Fatalf("%s has no representative %s monster", want.name, rank)
			}
			assertRealRiftMonsterDropPlanner(t, monsters, dungeon, *spawn, want.name+" "+rank)
		}
		assertRealRiftBossDropPlanner(t, archive, monsters, dungeon, *representativeNormal, *representativeBoss, want.name)
		t.Logf("%s dungeon=%d rooms=%d monster_rows=%d random_attempts=%d fixed_attempts=%d explicit_pools=%d probability_rows=%d item_reference_rows=%d gold_rows=%d passive_reward_rooms=%d", want.name, want.id, rooms, monstersSeen, randomAttempts, fixedAttempts, explicitPools, probabilityRows, itemReferenceRows, goldRows, passiveRewardRooms)
	}
}

func assertRealRiftBossDropPlanner(
	t *testing.T,
	archive *platformpvf.Archive,
	monsters *pvfDungeonMonsterCatalog,
	dungeon worldmap.Dungeon,
	normalSpawn worldmap.MonsterSpawn,
	bossSpawn worldmap.MonsterSpawn,
	label string,
) {
	t.Helper()
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 1, 3227)
	service.initialEquipmentArchive = archive
	service.dungeonMonsterTable = monsters
	runtime.Dungeon = dungeon
	runtime.Character.Level = 90
	runtime.Room.mu.Lock()
	runtime.Room.monsters[0].Spawn = normalSpawn
	runtime.Room.mu.Unlock()
	normal := runtime.Room.Snapshot().Monsters[0]
	startObjectKey := runtime.NextObjectKey
	if wires, selected, err := service.planCurrentDungeonStandaloneRiftBossDrops(
		runtime, normal, currentSceneActorObjectKey(99),
	); err != nil || len(wires) != 0 || len(selected) != 0 || runtime.NextObjectKey != startObjectKey {
		t.Fatalf("%s normal standalone-rift drops wires=%d selected=%+v next=%d err=%v", label, len(wires), selected, runtime.NextObjectKey, err)
	}

	runtime.Room.mu.Lock()
	runtime.Room.monsters[0].Spawn = bossSpawn
	runtime.Room.mu.Unlock()
	boss := runtime.Room.Snapshot().Monsters[0]
	visitScene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatalf("%s has no runtime scene", label)
	}
	visit, err := runtime.currentDungeonRoomVisit(visitScene)
	if err != nil {
		t.Fatalf("%s room visit: %v", label, err)
	}
	visit.DropRNG = 1
	wires, selected, err := service.planCurrentDungeonStandaloneRiftBossDrops(
		runtime, boss, currentSceneActorObjectKey(99),
	)
	if err != nil {
		t.Fatalf("%s standalone-rift boss planner: %v", label, err)
	}
	fragmentCount := 0
	for _, drop := range selected {
		if drop.Source == "epic_fragment" {
			fragmentCount++
		}
	}
	if fragmentCount == 0 || len(wires) != len(selected) || runtime.DropOwner == nil {
		t.Fatalf("%s standalone-rift boss wires=%d selected=%+v fragment_count=%d owner=%p", label, len(wires), selected, fragmentCount, runtime.DropOwner)
	}
}

func assertRealRiftMonsterDropPlanner(
	t *testing.T,
	monsters *pvfDungeonMonsterCatalog,
	dungeon worldmap.Dungeon,
	spawn worldmap.MonsterSpawn,
	label string,
) {
	t.Helper()
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 1, 3227)
	service.dungeonMonsterTable = monsters
	runtime.Dungeon = dungeon
	runtime.Character.Level = 90
	runtime.Room.mu.Lock()
	runtime.Room.monsters[0].Spawn = spawn
	runtime.Room.mu.Unlock()
	monster := runtime.Room.Snapshot().Monsters[0]
	scene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatalf("%s has no runtime scene", label)
	}
	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		t.Fatalf("%s room visit: %v", label, err)
	}
	selected := 0
	for seed := uint32(1); seed <= 256; seed++ {
		visit.DropRNG = seed
		runtime.DropOwner = nil
		runtime.NextObjectKey = 403
		wires, planErr := service.planCurrentDungeonMonsterDrops(runtime, monster, currentSceneActorObjectKey(99))
		if planErr != nil {
			t.Fatalf("%s seed=%d planner: %v", label, seed, planErr)
		}
		selected += len(wires)
	}
	if selected == 0 {
		t.Fatalf("%s selected no PVF drops across 256 independent room seeds", label)
	}
}
