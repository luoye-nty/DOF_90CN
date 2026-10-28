package dnfbridge

import (
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestPassiveObjectDropPlanUsesDungeonWideDefinitionForEveryMaze(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes = append(runtime.Dungeon.Mazes, worldmap.Maze{Index: 1})
	runtime.Dungeon.Metadata.SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 0, ItemID: 3227, DropRate: 10000,
	}}
	scene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatal("missing current scene")
	}
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
		Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}},
	}}
	for _, mazeIndex := range []int{0, 1} {
		runtime.MazeIndex = mazeIndex
		plan, err := service.planCurrentDungeonPassiveObjectDrops(
			&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, 7,
		)
		if err != nil {
			t.Fatalf("maze=%d: %v", mazeIndex, err)
		}
		if plan.ConfiguredRowCount != 1 || len(plan.Drops) != 1 {
			t.Fatalf("maze=%d rows=%d drops=%d", mazeIndex, plan.ConfiguredRowCount, len(plan.Drops))
		}
	}
}

func TestPassiveObjectDropPlanCommitsOnlyAfterStartMap(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{
		{Index: 0, ItemID: 3227, DropRate: 10000},
		{Index: 1, ItemID: 3227, DropRate: 10000},
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatal("missing current scene")
	}
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{
		{Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}}},
		{Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 1, Level: 1, Params: [3]int64{-1, -1, -1}}}},
	}
	projectionScene := scene
	runtime.Room.specialObjectCount = len(scene.SpecialPassiveObjects)
	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		t.Fatal(err)
	}
	startObjectKey, startRNG := runtime.NextObjectKey, visit.DropRNG
	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, startObjectKey, startRNG,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != 2 || len(plan.Drops) != 2 {
		t.Fatalf("plan entries=%d drops=%d", len(plan.ExtraEntries), len(plan.Drops))
	}
	if runtime.NextObjectKey != startObjectKey || runtime.DropOwner != nil || visit.DropRNG != startRNG || visit.PassiveObjectDropsPlanned {
		t.Fatalf("preflight mutated runtime: next=%d owner=%p rng=%08x planned=%t", runtime.NextObjectKey, runtime.DropOwner, visit.DropRNG, visit.PassiveObjectDropsPlanned)
	}
	packets, err := buildCurrentDungeonEntryPacketsWithPassiveDrops(runtime, projectionScene, plan.ExtraEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets.StartMap) == 0 {
		t.Fatal("start-map projection is empty")
	}
	actorCount := int(packets.StartMap[18])
	extraCountOffset := 19 + actorCount*21
	if extraCountOffset >= len(packets.StartMap) || packets.StartMap[extraCountOffset] != 2 {
		t.Fatalf("start-map extra count offset=%d body=%x", extraCountOffset, packets.StartMap)
	}
	if err := commitCurrentDungeonPassiveObjectDrops(runtime, scene, plan); err != nil {
		t.Fatal(err)
	}
	visit, err = runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.NextObjectKey != plan.NextObjectKey || runtime.DropOwner == nil ||
		visit.DropRNG != plan.NextDropRNG || !visit.PassiveObjectDropsPlanned || len(visit.PassiveObjectExtraEntries) != 2 {
		t.Fatalf("committed runtime mismatch: next=%d owner=%p visit=%+v", runtime.NextObjectKey, runtime.DropOwner, visit)
	}
	ownerKey := currentSceneActorObjectKey(99)
	for _, drop := range plan.Drops {
		owned, ownErr := runtime.DropOwner.owned(drop.ObjectKey, runtimeDungeonRoomKeyFromScene(scene), ownerKey)
		if ownErr != nil || owned != drop {
			t.Fatalf("pickup ownership object=%d owned=%p want=%p err=%v", drop.ObjectKey, owned, drop, ownErr)
		}
	}
	if err := commitCurrentDungeonPassiveObjectDrops(runtime, scene, plan); !errors.Is(err, errDungeonMoveRuntimeOwnerMismatch) {
		t.Fatalf("duplicate commit error=%v", err)
	}
}

func TestPassiveObjectDropRateUsesTenThousandDenominator(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 1, ItemID: 3227, DropRate: 9400,
	}}
	scene, _ := runtime.Session.Scene()
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
		Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 1, Level: 1, Params: [3]int64{-1, -1, -1}}},
	}}
	var selectedSeed, rejectedSeed uint32
	for seed := uint32(1); seed < 100000 && (selectedSeed == 0 || rejectedSeed == 0); seed++ {
		roll := newCurrentDungeonDropLCG(seed).Next(currentDungeonDropDenominator)
		if roll < 9400 && selectedSeed == 0 {
			selectedSeed = seed
		}
		if roll >= 9400 && rejectedSeed == 0 {
			rejectedSeed = seed
		}
	}
	if selectedSeed == 0 || rejectedSeed == 0 {
		t.Fatal("failed to find deterministic rate fixtures")
	}
	for _, test := range []struct {
		seed uint32
		want int
	}{{selectedSeed, 1}, {rejectedSeed, 0}} {
		plan, err := service.planCurrentDungeonPassiveObjectDrops(
			&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, test.seed,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.ExtraEntries) != test.want {
			t.Fatalf("seed=%d roll=%d selected=%d want=%d", test.seed, newCurrentDungeonDropLCG(test.seed).Next(10000), len(plan.ExtraEntries), test.want)
		}
	}
}

func TestPassiveObjectDropPlanRollsOncePerSpecialObjectInstance(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 0, ItemID: 3227, DropRate: 1500,
	}}
	scene, _ := runtime.Session.Scene()
	for index := 0; index < 4; index++ {
		scene.SpecialPassiveObjects = append(scene.SpecialPassiveObjects, worldmap.SpecialPassiveObject{
			PassiveObject: worldmap.PassiveObject{ObjectID: int64(16111 + index%2)},
			Spawns:        []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}},
		})
	}
	seed := uint32(1)
	wantSelected := 0
	wantRNG := newCurrentDungeonDropLCG(seed)
	for index := 0; index < 4; index++ {
		if wantRNG.Next(currentDungeonDropDenominator) < 1500 {
			wantSelected++
		}
	}
	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, seed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != wantSelected || len(plan.Drops) != wantSelected {
		t.Fatalf("selected=%d drops=%d want=%d", len(plan.ExtraEntries), len(plan.Drops), wantSelected)
	}
	if plan.NextDropRNG != wantRNG.Seed() {
		t.Fatalf("next RNG=%08x want=%08x", plan.NextDropRNG, wantRNG.Seed())
	}
}

func TestPassiveObjectDropPlanUsesOwningSpecialObjectIndexAcrossRewardGroups(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{
		{Index: 0, ItemID: 3227, DropRate: 10000},
		{Index: 1, ItemID: 3227, DropRate: 10000},
		{Index: 2, ItemID: 3227, DropRate: 10000},
	}
	scene, _ := runtime.Session.Scene()
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
		PassiveObject: worldmap.PassiveObject{ObjectID: 37224},
		Spawns: []worldmap.SpecialObjectSpawn{
			{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}},
			{Kind: "[item]", Code: 1, Level: 1, Params: [3]int64{-1, -1, -1}},
			{Kind: "[item]", Code: 2, Level: 1, Params: [3]int64{-1, -1, -1}},
		},
	}}

	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, 7,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != 3 {
		t.Fatalf("entries=%d want=3", len(plan.ExtraEntries))
	}
	for index, entry := range plan.ExtraEntries {
		if entry.PassiveObjectIndex != 0 {
			t.Fatalf("entry[%d] passive object index=%d want=0", index, entry.PassiveObjectIndex)
		}
	}
}

func TestPassiveObjectDropPlanKeepsDistinctSpecialObjectOwners(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 0, ItemID: 3227, DropRate: 10000,
	}}
	scene, _ := runtime.Session.Scene()
	for index := 0; index < 3; index++ {
		scene.SpecialPassiveObjects = append(scene.SpecialPassiveObjects, worldmap.SpecialPassiveObject{
			PassiveObject: worldmap.PassiveObject{ObjectID: int64(37224 + index)},
			Spawns:        []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}},
		})
	}

	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, 7,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != 3 {
		t.Fatalf("entries=%d want=3", len(plan.ExtraEntries))
	}
	for index, entry := range plan.ExtraEntries {
		if entry.PassiveObjectIndex != byte(index) {
			t.Fatalf("entry[%d] passive object index=%d want=%d", index, entry.PassiveObjectIndex, index)
		}
	}
}

func TestPassiveObjectDropPlanCountsOrdinaryQuestItemObjectsPerInstance(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	source := service.dungeonMonsterTable.source.(bridgePVFSource)
	source[currentDungeonPassiveObjectList] = "16111 `actionobject/test/magtonium1.obj`\n16112 `actionobject/test/magtonium2.obj`\n"
	source["passiveobject/actionobject/test/magtonium1.obj"] = "[reward type]\n`[quest item]`\n"
	source["passiveobject/actionobject/test/magtonium2.obj"] = "[reward type]\n`[quest item]`\n"
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 1, ItemID: 3227, DropRate: 10000,
	}}
	scene, _ := runtime.Session.Scene()
	for index := 0; index < 3; index++ {
		scene.PassiveObjects = append(scene.PassiveObjects, worldmap.PassiveObject{ObjectID: 16111})
	}
	for index := 0; index < 4; index++ {
		scene.PassiveObjects = append(scene.PassiveObjects, worldmap.PassiveObject{ObjectID: 16112})
	}
	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, 7,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != 7 || len(plan.Drops) != 7 {
		t.Fatalf("entries=%d drops=%d want=7", len(plan.ExtraEntries), len(plan.Drops))
	}
	for index, entry := range plan.ExtraEntries {
		if entry.PassiveObjectIndex != 1 {
			t.Fatalf("entry[%d] passive object index=%d want legacy group selector 1", index, entry.PassiveObjectIndex)
		}
	}
}

func TestPassiveObjectDropPlanSkipsMissingOrdinaryObjectDefinition(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	source := service.dungeonMonsterTable.source.(bridgePVFSource)
	source[currentDungeonPassiveObjectList] = "72228 `ActionObject/Map/MetroCenter/TimePlaza/building_broken_c.obj`\n"
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 1, ItemID: 3227, DropRate: 10000,
	}}
	scene, _ := runtime.Session.Scene()
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
		Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 1, Level: 1, Params: [3]int64{-1, -1, -1}}},
	}}
	scene.PassiveObjects = []worldmap.PassiveObject{{ObjectID: 72228}}

	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, 7,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != 1 || len(plan.Drops) != 1 {
		t.Fatalf("entries=%d drops=%d want=1", len(plan.ExtraEntries), len(plan.Drops))
	}

	group, known, err := service.dungeonMonsterTable.passiveObjectRewardGroup(72228)
	if err != nil || known || group != 0 {
		t.Fatalf("missing passive object group=%d known=%t err=%v", group, known, err)
	}
}

func TestPassiveObjectDropPlanDoesNotRollMissingGroup(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 1, ItemID: 3227, DropRate: 10000,
	}}
	scene, _ := runtime.Session.Scene()
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
		Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}},
	}}
	seed := uint32(0x12345678)
	plan, err := service.planCurrentDungeonPassiveObjectDrops(
		&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, seed,
	)
	if err != nil {
		t.Fatal(err)
	}
	// The reference consumes the attempt's roll even when the selected group
	// has no rows; only the drop is absent.
	wantRNG := newCurrentDungeonDropLCG(seed)
	wantRNG.Next(currentDungeonDropDenominator)
	if len(plan.ExtraEntries) != 0 || plan.NextDropRNG != wantRNG.Seed() {
		t.Fatalf("plan=%+v want no drop and exactly one consumed roll", plan)
	}
}
