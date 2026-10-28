package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The map [item] child's level is the number of specific-group rolls: -1 (the
// live PVF's "none" marker) rolls nothing, 2 rolls twice. The first parameter
// is the unimplemented random-attempt count and must not trigger a roll.
func TestPassiveObjectDropPlanRollsSpecificAttemptsPerSpawnLevel(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{{
		Index: 0, ItemID: 3227, DropRate: 10000,
	}}
	scene, _ := runtime.Session.Scene()
	tests := []struct {
		name      string
		spawn     worldmap.SpecialObjectSpawn
		wantDrops int
		wantRolls int
	}{
		{"level -1 rolls nothing", worldmap.SpecialObjectSpawn{Kind: "[item]", Code: 0, Level: -1, Params: [3]int64{-1, -1, -1}}, 0, 0},
		{"random-only spawn rolls nothing", worldmap.SpecialObjectSpawn{Kind: "[item]", Code: 0, Level: -1, Params: [3]int64{3, -1, -1}}, 0, 0},
		{"level 1 rolls once", worldmap.SpecialObjectSpawn{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}, 1, 1},
		{"level 2 rolls twice", worldmap.SpecialObjectSpawn{Kind: "[item]", Code: 0, Level: 2, Params: [3]int64{-1, -1, -1}}, 2, 2},
		{"level 3 with random attempts rolls three times", worldmap.SpecialObjectSpawn{Kind: "[item]", Code: 0, Level: 3, Params: [3]int64{2, -1, -1}}, 3, 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
				PassiveObject: worldmap.PassiveObject{ObjectID: 10673},
				Spawns:        []worldmap.SpecialObjectSpawn{test.spawn},
			}}
			seed := uint32(77)
			plan, err := service.planCurrentDungeonPassiveObjectDrops(
				&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, seed,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.ExtraEntries) != test.wantDrops || len(plan.Drops) != test.wantDrops {
				t.Fatalf("entries=%d drops=%d want=%d", len(plan.ExtraEntries), len(plan.Drops), test.wantDrops)
			}
			wantRNG := newCurrentDungeonDropLCG(seed)
			for roll := 0; roll < test.wantRolls; roll++ {
				wantRNG.Next(currentDungeonDropDenominator)
			}
			if plan.NextDropRNG != wantRNG.Seed() {
				t.Fatalf("rng=%08x want=%08x (%d rolls)", plan.NextDropRNG, wantRNG.Seed(), test.wantRolls)
			}
			for _, entry := range plan.ExtraEntries {
				if entry.PassiveObjectIndex != 0 || entry.ItemID != 3227 {
					t.Fatalf("entry=%+v", entry)
				}
			}
		})
	}
}

// A multi-item group is one weighted table per roll: the roll is compared with
// the cumulative drop rates, so at most one item is produced per attempt and
// the remainder of the 10000-point space is "no drop".
func TestPassiveObjectDropPlanSelectsAtMostOneItemPerGroupRoll(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	source := service.dungeonMonsterTable.source.(bridgePVFSource)
	source[dungeonDropStackableList] = "1069 `material/a.stk`\n1071 `material/b.stk`\n1072 `material/c.stk`\n"
	for _, name := range []string{"a", "b", "c"} {
		source["stackable/material/"+name+".stk"] = "[name]\n`Test`\n[stackable type]\n`[material]`\n[stack limit]\n999\n[attach type]\n`[free]`\n"
	}
	// 1stBackbone-style group: 1069@1500, 1071@500, 1072@3000 (total 5000).
	runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems = []worldmap.SpecialPassiveObjectItem{
		{Index: 0, LevelOverride: -1, ItemID: 1069, DropRate: 1500},
		{Index: 0, LevelOverride: -1, ItemID: 1071, DropRate: 500},
		{Index: 0, LevelOverride: -1, ItemID: 1072, DropRate: 3000},
	}
	scene, _ := runtime.Session.Scene()
	scene.SpecialPassiveObjects = []worldmap.SpecialPassiveObject{{
		Spawns: []worldmap.SpecialObjectSpawn{{Kind: "[item]", Code: 0, Level: 1, Params: [3]int64{-1, -1, -1}}},
	}}
	wantByRoll := func(roll int) int64 {
		switch {
		case roll < 1500:
			return 1069
		case roll < 2000:
			return 1071
		case roll < 5000:
			return 1072
		default:
			return 0
		}
	}
	seen := make(map[int64]bool)
	for seed := uint32(1); seed <= 400; seed++ {
		roll := newCurrentDungeonDropLCG(seed).Next(currentDungeonDropDenominator)
		want := wantByRoll(roll)
		plan, err := service.planCurrentDungeonPassiveObjectDrops(
			&gameSession{selectedCharacterID: 99}, runtime, scene, runtime.NextObjectKey, seed,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.ExtraEntries) > 1 {
			t.Fatalf("seed=%d produced %d items from one roll", seed, len(plan.ExtraEntries))
		}
		got := int64(0)
		if len(plan.ExtraEntries) == 1 {
			got = int64(plan.ExtraEntries[0].ItemID)
		}
		if got != want {
			t.Fatalf("seed=%d roll=%d item=%d want=%d", seed, roll, got, want)
		}
		seen[got] = true
	}
	for _, itemID := range []int64{0, 1069, 1071, 1072} {
		if !seen[itemID] {
			t.Fatalf("400 seeds never produced outcome %d: %v", itemID, seen)
		}
	}
}

func TestPassiveObjectGroupSelectUsesCumulativeRates(t *testing.T) {
	group := []worldmap.SpecialPassiveObjectItem{
		{Index: 0, ItemID: 1, DropRate: 100},
		{Index: 0, ItemID: 2, DropRate: 0},
		{Index: 0, ItemID: 3, DropRate: 200},
	}
	for _, test := range []struct {
		roll int
		want int64
	}{{0, 1}, {99, 1}, {100, 3}, {299, 3}, {300, 0}, {9999, 0}} {
		if got := currentDungeonPassiveObjectGroupSelect(group, test.roll); got != test.want {
			t.Fatalf("roll=%d got=%d want=%d", test.roll, got, test.want)
		}
	}
	if got := currentDungeonPassiveObjectGroupSelect(nil, 0); got != 0 {
		t.Fatalf("empty group selected %d", got)
	}
	for _, test := range []struct {
		value int64
		want  int
	}{{-1, -1}, {0, 0}, {1, 1}, {10, 10}, {255, -1}} {
		if got := currentDungeonPassiveObjectSignedByte(test.value); got != test.want {
			t.Fatalf("signed byte of %d = %d want %d", test.value, got, test.want)
		}
	}
}
