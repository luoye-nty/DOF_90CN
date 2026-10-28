package worldmap

import (
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func TestCompactMapForRuntimePreservesTypedState(t *testing.T) {
	value := Map{
		Monsters:         []MonsterSpawn{{MonsterID: 11}},
		BloodMonsters:    []BloodAltarMonster{{MonsterCode: 21}},
		BloodPhases:      []BloodAltarPhase{{Round: 2, Scale: 1.5}},
		UltimateMonsters: []BloodAltarMonster{{MonsterCode: 31}},
		UltimatePhases:   []BloodAltarPhase{{Round: 3, Scale: 2, Difficulty: BloodAltarDifficultyB}},
		SourceSections:   []RawSection{{Name: "monster", Tokens: []dnfpvf.Token{{Kind: dnfpvf.TokenInt, Int: 11}}}},
		OpaqueSections:   []RawSection{{Name: "tile"}},
		Extensions:       []ExtensionSection{{Name: "tile", Tokens: []dnfpvf.Token{{Kind: dnfpvf.TokenInt, Int: 1}}}},
		Summons: []MapSummon{{
			Key:             OptionalInt{Value: 7, Set: true},
			SourceSections:  []RawSection{{Name: "summon"}},
			UnknownSections: []RawSection{{Name: "future"}},
			Extensions:      []ExtensionSection{{Name: "future"}},
		}},
	}

	compactMapForRuntime(&value)

	if len(value.Monsters) != 1 || value.Monsters[0].MonsterID != 11 ||
		len(value.BloodMonsters) != 1 || value.BloodMonsters[0].MonsterCode != 21 ||
		len(value.BloodPhases) != 1 || value.BloodPhases[0].Scale != 1.5 ||
		len(value.UltimateMonsters) != 1 || value.UltimateMonsters[0].MonsterCode != 31 ||
		len(value.UltimatePhases) != 1 || value.UltimatePhases[0].Difficulty != BloodAltarDifficultyB ||
		len(value.Summons) != 1 || !value.Summons[0].Key.Set || value.Summons[0].Key.Value != 7 {
		t.Fatalf("typed runtime state changed: %+v", value)
	}
	if value.SourceSections != nil || value.OpaqueSections != nil || value.Extensions != nil ||
		value.Summons[0].SourceSections != nil || value.Summons[0].UnknownSections != nil ||
		value.Summons[0].Extensions != nil {
		t.Fatalf("raw parser evidence retained: %+v", value)
	}
}

func TestCompactDungeonForRuntimePreservesMechanisms(t *testing.T) {
	value := Dungeon{
		SourceSections: []RawSection{{Name: "maze"}},
		BloodAltarKind: BloodAltarDungeonKindUltimate,
		BloodMaxRounds: 7,
		Metadata:       DungeonMetadata{WorldMapInfo: []RawSection{{Name: "worldmap info"}}},
		Mazes: []Maze{{
			BossSpecifications:       []MapSpecification{{Type: "boss", MapIDs: []int64{101}}},
			LayeredSpecifications:    []MapSpecification{{Type: "layered", MapIDs: []int64{102}}},
			ClearConditions:          []ClearCondition{{Type: "hunt monster", TargetID: 9, Count: 1}},
			BossMapSpecifications:    []RawSection{{Name: "boss map specification"}},
			LayeredMapSpecifications: []RawSection{{Name: "layered map specification"}},
			RandomizedObjects: []RandomizedObjectScript{{
				SelectCount:    OptionalInt{Value: 1, Set: true},
				SourceSections: []RawSection{{Name: "randomized object creation"}},
				Objects: []RidableObject{{
					ObjectIndex:    OptionalInt{Value: 4, Set: true},
					SourceSections: []RawSection{{Name: "object"}},
				}},
			}},
		}},
	}

	compactDungeonForRuntime(&value)

	maze := value.Mazes[0]
	if value.BloodAltarKind != BloodAltarDungeonKindUltimate || value.BloodMaxRounds != 7 ||
		len(maze.BossSpecifications) != 1 || len(maze.LayeredSpecifications) != 1 ||
		len(maze.ClearConditions) != 1 || len(maze.RandomizedObjects) != 1 ||
		!maze.RandomizedObjects[0].SelectCount.Set || !maze.RandomizedObjects[0].Objects[0].ObjectIndex.Set {
		t.Fatalf("typed mechanisms changed: %+v", maze)
	}
	if value.SourceSections != nil || value.Metadata.WorldMapInfo != nil ||
		maze.BossMapSpecifications != nil || maze.LayeredMapSpecifications != nil ||
		maze.RandomizedObjects[0].SourceSections != nil ||
		maze.RandomizedObjects[0].Objects[0].SourceSections != nil {
		t.Fatalf("raw parser evidence retained: %+v", value)
	}
}

func TestCloneMapDeepCopiesBloodAltarTables(t *testing.T) {
	original := Map{
		BloodMonsters:    []BloodAltarMonster{{MonsterCode: 11, Position: Point3{X: 1}}},
		BloodPhases:      []BloodAltarPhase{{Round: 2, Scale: 1.5}},
		UltimateMonsters: []BloodAltarMonster{{MonsterCode: 21, Position: Point3{X: 3}}},
		UltimatePhases:   []BloodAltarPhase{{Round: 4, Difficulty: BloodAltarDifficultyA}},
	}
	cloned := cloneMap(original)
	cloned.BloodMonsters[0].Position.X = 99
	cloned.BloodPhases[0].Scale = 9
	cloned.UltimateMonsters[0].MonsterCode = 88
	cloned.UltimatePhases[0].Difficulty = BloodAltarDifficultyB
	if original.BloodMonsters[0].Position.X != 1 || original.BloodPhases[0].Scale != 1.5 ||
		original.UltimateMonsters[0].MonsterCode != 21 || original.UltimatePhases[0].Difficulty != BloodAltarDifficultyA {
		t.Fatalf("clone mutated original altar tables: original=%+v cloned=%+v", original, cloned)
	}
}
