package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonAreaMaterialMapsOnlySelectableMetroCenterRange(t *testing.T) {
	for dungeonID := int64(310); dungeonID <= 315; dungeonID++ {
		entry, matched := currentDungeonAreaMaterialPoolEntry(dungeonID)
		if !matched || entry.ItemID != currentDungeonBirthBudItemID || entry.Weight != currentDungeonAreaMaterialWeight {
			t.Fatalf("dungeon=%d matched=%t entry=%+v", dungeonID, matched, entry)
		}
	}
	for _, dungeonID := range []int64{0, 144, 309, 316, 317, 5097, 7155} {
		if entry, matched := currentDungeonAreaMaterialPoolEntry(dungeonID); matched {
			t.Fatalf("dungeon=%d unexpectedly mapped to %+v", dungeonID, entry)
		}
	}
}

func TestCurrentDungeonAreaMaterialUsesSharedExplicitPoolRoll(t *testing.T) {
	monsterEntry := dungeonMonsterDropPoolEntry{ItemID: 9001, Weight: 10}
	areaEntry, matched := currentDungeonAreaMaterialPoolEntry(310)
	if !matched {
		t.Fatal("time plaza area material is not mapped")
	}
	pool := []dungeonMonsterDropPoolEntry{monsterEntry, areaEntry}

	var seed uint32
	selected := dungeonMonsterDropPoolEntry{}
	for candidate := uint32(1); candidate < 100000; candidate++ {
		rng := newCurrentDungeonDropLCG(candidate)
		if currentDungeonExplicitPoolRate <= rng.Next(currentDungeonDropDenominator) {
			continue
		}
		entry, ok := selectDungeonMonsterDrop(pool, uint64(rng.NextUint32()))
		if ok && entry.ItemID == currentDungeonBirthBudItemID {
			seed = candidate
			selected = entry
			break
		}
	}
	if seed == 0 || selected != areaEntry {
		t.Fatalf("shared explicit-pool path did not select birth bud: seed=%d selected=%+v", seed, selected)
	}

	// The regional item is not rolled independently. A failed shared outer
	// roll must suppress the entire weighted pool, including the area entry.
	failedOuterRollFound := false
	for candidate := uint32(1); candidate < 100000; candidate++ {
		rng := newCurrentDungeonDropLCG(candidate)
		if currentDungeonExplicitPoolRate <= rng.Next(currentDungeonDropDenominator) {
			failedOuterRollFound = true
			break
		}
	}
	if !failedOuterRollFound {
		t.Fatal("could not exercise failed shared explicit-pool outer roll")
	}
}

func TestCurrentDungeonMonsterPlanIntegratesBirthBudAreaMaterialBoundaries(t *testing.T) {
	const sharedExplicitPoolSeed uint32 = 39
	tests := []struct {
		name          string
		dungeonID     int64
		practice      bool
		wantBirthBud  bool
		wantAnyDrops  bool
		wantUnchanged bool
	}{
		{name: "time plaza", dungeonID: 310, wantBirthBud: true, wantAnyDrops: true},
		{name: "particular hell excluded", dungeonID: 316, wantAnyDrops: true},
		{name: "practice suppresses the regional pool", dungeonID: 310, practice: true, wantUnchanged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, runtime, _ := prepareCurrentDungeonDropTest(t, 1, 3227)
			source, ok := service.dungeonMonsterTable.source.(bridgePVFSource)
			if !ok {
				t.Fatalf("test source type=%T", service.dungeonMonsterTable.source)
			}
			// Keep the ordinary PVF formula valid but outside the synthetic
			// monster's level. This isolates the explicit monster/template pool,
			// whose fixed_drop_count=1 is the source used by area materials.
			rules := strings.Replace(
				currentDungeonGoldDropTestRuleText(),
				"1 200 10000 0 0 0 0",
				"1 1 0 0 0 0 0",
				1,
			)
			source[dungeonCardDropRulePath] = rules
			source[dungeonDropStackableList] = "3227 `material/test_drop.stk`\n" +
				"10161610 `material/test_birth_bud.stk`\n"
			source["stackable/material/test_birth_bud.stk"] = "[name]\n`Birth Bud`\n" +
				"[stackable type]\n`[material]`\n[stack limit]\n999\n[attach type]\n`[free]`\n"
			currentDungeonMonsterDropRulesByCatalog.Delete(service.dungeonMonsterTable)

			runtime.Dungeon.ID = test.dungeonID
			if test.practice {
				runtime.ProtocolMode = currentDungeonProtocolModePractice
				runtime.Request.RuntimeState = byte(currentDungeonProtocolModePractice)
			}
			scene, found := runtime.Session.Scene()
			if !found {
				t.Fatal("missing dungeon scene")
			}
			visit, err := runtime.currentDungeonRoomVisit(scene)
			if err != nil {
				t.Fatal(err)
			}
			visit.DropRNG = sharedExplicitPoolSeed
			startNextObjectKey := runtime.NextObjectKey
			startDropOwner := runtime.DropOwner

			monster := runtime.Room.Snapshot().Monsters[0]
			wires, err := service.planCurrentDungeonMonsterDrops(runtime, monster, currentSceneActorObjectKey(99))
			if err != nil {
				t.Fatal(err)
			}
			birthBudCount := 0
			for _, wire := range wires {
				if binary.LittleEndian.Uint32(wire.Item.data[0x02:0x06]) == currentDungeonBirthBudItemID {
					birthBudCount++
				}
			}
			if (birthBudCount != 0) != test.wantBirthBud {
				t.Fatalf("dungeon=%d practice=%t birth_bud_count=%d wires=%d", test.dungeonID, test.practice, birthBudCount, len(wires))
			}
			if (len(wires) != 0) != test.wantAnyDrops {
				t.Fatalf("dungeon=%d practice=%t wires=%d want_any=%t", test.dungeonID, test.practice, len(wires), test.wantAnyDrops)
			}
			if test.wantUnchanged && (visit.DropRNG != sharedExplicitPoolSeed || runtime.NextObjectKey != startNextObjectKey || runtime.DropOwner != startDropOwner) {
				t.Fatalf("practice mutated drop state: rng=%08x next=%d owner=%p", visit.DropRNG, runtime.NextObjectKey, runtime.DropOwner)
			}
		})
	}
}

func TestRealPVFMetroCenterBirthBudAreaMaterial(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Metro Center area material")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	dropCatalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	birthBud, err := dropCatalog.ResolveItem(currentDungeonBirthBudItemID)
	if err != nil {
		t.Fatal(err)
	}
	if birthBud.Kind != dungeonDropItemStackable || birthBud.Name != "诞生之芽" ||
		normalizeDungeonDropStackableType(birthBud.StackableType) != "[material]" || birthBud.CreationRate != 0 {
		t.Fatalf("birth bud metadata=%+v", birthBud)
	}

	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	expectedPaths := map[int64]string{
		310: "dungeon/MetroCenter/Time_Plaza/Time_Plaza.dgn",
		311: "dungeon/MetroCenter/Canyon_of_Beast/Canyon_of_Beast.dgn",
		312: "dungeon/MetroCenter/Hideout_of_Fear/Hideout_of_Fear.dgn",
		313: "dungeon/MetroCenter/Gust_Zone/Gust_Zone.dgn",
		314: "dungeon/MetroCenter/Forest_of_Redwitch/Forest_of_Redwitch.dgn",
		315: "dungeon/MetroCenter/Special/Special.dgn",
	}
	for dungeonID, expectedPath := range expectedPaths {
		dungeon, found := table.FindDungeon(dungeonID)
		if !found {
			t.Fatalf("real PVF dungeon %d is missing", dungeonID)
		}
		gotPath := strings.ReplaceAll(dungeon.Path, "\\", "/")
		if !strings.EqualFold(gotPath, expectedPath) {
			t.Fatalf("dungeon=%d path=%q want=%q", dungeonID, gotPath, expectedPath)
		}
		entry, matched := currentDungeonAreaMaterialPoolEntry(dungeonID)
		if !matched || entry.ItemID != currentDungeonBirthBudItemID || entry.Weight != currentDungeonAreaMaterialWeight {
			t.Fatalf("dungeon=%d area entry matched=%t entry=%+v", dungeonID, matched, entry)
		}
	}
	for _, excludedDungeonID := range []int64{316, 317} {
		if _, found := table.FindDungeon(excludedDungeonID); !found {
			t.Fatalf("real PVF excluded boundary dungeon %d is missing", excludedDungeonID)
		}
		if entry, matched := currentDungeonAreaMaterialPoolEntry(excludedDungeonID); matched {
			t.Fatalf("excluded dungeon=%d unexpectedly mapped to %+v", excludedDungeonID, entry)
		}
	}

	t.Logf("birth bud area material: item=%d creation_rate=%d dungeons=310..315 weight=%d",
		birthBud.ItemID, birthBud.CreationRate, currentDungeonAreaMaterialWeight)
}
