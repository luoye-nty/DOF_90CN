package worldmap

import (
	"slices"
	"testing"
)

func TestParseDungeonMetadataRepeatedMazesAndExtensions(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/test.dgn", `
[name]
`+"`Test Dungeon`"+`
[explain]
`+"`Structured from local PVF`"+`
[cutscene image]
`+"`Map/Test/loading.img`"+` 3
[minimum required level]
10
[basis level]
20
[experience increasing point]
1.25
[dungeon type]
`+"`[normal]`"+`
[tutorial dungeon]
1
[recommended level]
10 30
[schedule]
2418 2423
[fatigue]
8
[raid dungeon]
1
[maze info]
[size]
3 2
[greed]
`+"`BBFFEE`"+`
[map specification]
`+"`map`"+` 0 0 100 `+"`map`"+` 1 0 101 102 `+"`boss`"+` 2 1 103
[start map]
0 0 7
[boss map]
2 1
[quest connection]
0 900 -1
[boss map specification]
2 1 103
[layered map specification]
1 1 104 105
[maze minimap ani icon]
0 1 0 `+"`ui/maze.ani`"+` 1
[randomized object creation]
[select]
1
[regenerate]
0
[minimap icon]
2
[object]
[map]
1 0
[index]
64024
[team]
[monster]
[pos]
563 197
[/object]
[/randomized object creation]
[clear condition]
[destroy object]
80458 1
[/clear condition]
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 200
[start map]
0 0
[boss map]
0 0
[fatigue result]
4
`)

	dungeon := ParseDungeon(77, "dungeon/test.dgn", doc)
	if dungeon.Metadata.Name != "Test Dungeon" || !dungeon.Metadata.MinimumRequiredLevel.Set || dungeon.Metadata.MinimumRequiredLevel.Value != 10 {
		t.Fatalf("unexpected metadata: %+v", dungeon.Metadata)
	}
	if !dungeon.Metadata.ExperienceIncreasingPoint.Set || dungeon.Metadata.ExperienceIncreasingPoint.Value != 1.25 {
		t.Fatalf("unexpected numeric metadata: %+v", dungeon.Metadata.ExperienceIncreasingPoint)
	}
	if !dungeon.Metadata.TutorialDungeon.Set || dungeon.Metadata.TutorialDungeon.Value != 1 {
		t.Fatalf("tutorial dungeon metadata missing: %+v", dungeon.Metadata.TutorialDungeon)
	}
	if !slices.Equal(dungeon.Metadata.ScheduleIDs, []int64{2418, 2423}) {
		t.Fatalf("schedule ids = %+v", dungeon.Metadata.ScheduleIDs)
	}
	if dungeon.Metadata.IntegerValues["fatigue"] != 8 || dungeon.Metadata.IntegerValues["fatigue result"] != 4 {
		t.Fatalf("stable integer metadata missing: %+v", dungeon.Metadata.IntegerValues)
	}
	if len(dungeon.Mazes) != 2 || dungeon.Mazes[0].Index != 0 || dungeon.Mazes[1].Index != 1 {
		t.Fatalf("repeated mazes = %+v", dungeon.Mazes)
	}
	first := dungeon.Mazes[0]
	if first.Width.Value != 3 || first.Height.Value != 2 || first.Greed != "BBFFEE" || len(first.MapSpecifications) != 3 {
		t.Fatalf("unexpected first maze: %+v", first)
	}
	if got := first.MapSpecifications[1].MapIDs; len(got) != 2 || got[0] != 101 || got[1] != 102 {
		t.Fatalf("map candidates = %+v", got)
	}
	if first.Start == nil || first.Start.X != 0 || len(first.Start.Params) != 1 || first.Boss == nil || first.Boss.X != 2 {
		t.Fatalf("start/boss points = start=%+v boss=%+v", first.Start, first.Boss)
	}
	if len(dungeon.Extensions) != 1 || dungeon.Extensions[0].Name != "raid dungeon" || dungeon.Extensions[0].Kind != SectionIntegers || dungeon.Extensions[0].Integers[0] != 1 {
		t.Fatalf("local metadata extension = %+v", dungeon.Extensions)
	}
	if len(first.Extensions) != 1 || first.Extensions[0].Name != "maze minimap ani icon" || first.Extensions[0].Kind != SectionMixed || first.Extensions[0].Texts[0] != "ui/maze.ani" {
		t.Fatalf("local maze extension = %+v", first.Extensions)
	}
	if len(first.OpaqueSections) < 2 || len(first.SourceSections) == 0 || len(dungeon.SourceSections) != len(doc.Sections) {
		t.Fatalf("raw ownership missing: maze opaque=%+v source=%d dungeon_source=%d", first.OpaqueSections, len(first.SourceSections), len(dungeon.SourceSections))
	}
	if len(first.BossSpecifications) != 1 || first.BossSpecifications[0].MapIDs[0] != 103 || len(first.LayeredSpecifications) != 1 || len(first.LayeredSpecifications[0].MapIDs) != 2 {
		t.Fatalf("boss/layered specifications = boss=%+v layered=%+v", first.BossSpecifications, first.LayeredSpecifications)
	}
	if len(first.RandomizedObjects) != 1 || first.RandomizedObjects[0].MinimapIcon.Value != 2 || len(first.RandomizedObjects[0].Objects) != 1 {
		t.Fatalf("randomized object script = %+v", first.RandomizedObjects)
	}
	object := first.RandomizedObjects[0].Objects[0]
	if object.ObjectIndex.Value != 64024 || object.Team != "monster" || object.Position.X != 563 || object.MazeCoordinate.X != 1 {
		t.Fatalf("randomized object = %+v", object)
	}
	if len(first.ClearConditions) != 1 || first.ClearConditions[0].Type != "destroy object" || first.ClearConditions[0].TargetID != 80458 {
		t.Fatalf("clear conditions = %+v", first.ClearConditions)
	}
}

func TestParseDungeonEscapeHellBelongsToSelectedMaze(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/escape_hell.dgn", `
[hell dungeon]
1
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 100
[start map]
0 0
[boss map]
0 0
[maze info]
[escape hell]
1
[size]
1 1
[map specification]
`+"`map`"+` 0 0 200
[start map]
0 0
[boss map]
0 0
`)

	dungeon := ParseDungeon(7542, "dungeon/escape_hell.dgn", doc)
	if got := dungeon.Metadata.IntegerValues["hell dungeon"]; got != 1 {
		t.Fatalf("hell dungeon=%d want 1", got)
	}
	if len(dungeon.Mazes) != 2 {
		t.Fatalf("mazes=%d want 2", len(dungeon.Mazes))
	}
	if dungeon.Mazes[0].EscapeHell.Set {
		t.Fatalf("unmarked maze inherited escape-hell state: %+v", dungeon.Mazes[0].EscapeHell)
	}
	if got := dungeon.Mazes[1].EscapeHell; !got.Set || got.Value != 1 {
		t.Fatalf("selected maze escape-hell=%+v want {Value:1 Set:true}", got)
	}
	if _, leaked := dungeon.Metadata.IntegerValues["escape hell"]; leaked {
		t.Fatalf("maze escape-hell leaked into dungeon metadata: %+v", dungeon.Metadata.IntegerValues)
	}
}

func TestParseDungeonBossRoomEntranceConditionBelongsToCurrentMaze(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/boss_gate.dgn", `
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 100
[start map]
0 0
[boss map]
0 0
[boss room entrance condition]
`+"`[clear action]` `[time condition]` 1 0 `[hunt monster]` 2 69201 0 2 69210 0 2"+`
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 200
[start map]
0 0
[boss map]
0 0
[boss room entrance condition]
`+"`[clear action]` `[time condition]` 7 9 `[hunt monster]` 1 70001 4 3"+`
`)

	dungeon := ParseDungeon(2005, "dungeon/boss_gate.dgn", doc)
	if len(dungeon.Mazes) != 2 {
		t.Fatalf("mazes=%+v", dungeon.Mazes)
	}
	first := dungeon.Mazes[0].BossRoomEntranceCondition
	if first == nil || first.Action != "clear action" || len(first.TimeCondition) != 2 ||
		first.TimeCondition[0] != 1 || first.TimeCondition[1] != 0 || len(first.HuntMonsters) != 2 {
		t.Fatalf("first boss-room entrance condition=%+v diagnostics=%+v", first, dungeon.Mazes[0].Diagnostics)
	}
	if got := first.HuntMonsters[0]; got.TargetID != 69201 || got.Opaque != 0 || got.Count != 2 {
		t.Fatalf("first hunt target=%+v", got)
	}
	if got := first.HuntMonsters[1]; got.TargetID != 69210 || got.Opaque != 0 || got.Count != 2 {
		t.Fatalf("second hunt target=%+v", got)
	}
	second := dungeon.Mazes[1].BossRoomEntranceCondition
	if second == nil || second.TimeCondition[0] != 7 || second.TimeCondition[1] != 9 ||
		len(second.HuntMonsters) != 1 || second.HuntMonsters[0] != (BossRoomEntranceHuntMonster{TargetID: 70001, Opaque: 4, Count: 3}) {
		t.Fatalf("second boss-room entrance condition=%+v diagnostics=%+v", second, dungeon.Mazes[1].Diagnostics)
	}
	for _, section := range dungeon.OpaqueSections {
		if sectionKey(section.Name) == "boss room entrance condition" {
			t.Fatalf("per-maze boss-room gate leaked into dungeon opaque sections: %+v", section)
		}
	}

	cloned := cloneDungeon(dungeon)
	cloned.Mazes[0].BossRoomEntranceCondition.TimeCondition[0] = 99
	cloned.Mazes[0].BossRoomEntranceCondition.HuntMonsters[0].Count = 99
	if first.TimeCondition[0] != 1 || first.HuntMonsters[0].Count != 2 {
		t.Fatalf("clone aliases source condition: source=%+v clone=%+v", first, cloned.Mazes[0].BossRoomEntranceCondition)
	}
}

func TestParseDungeonEventMonsterMetadataRemainsDungeonWide(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/event_monster.dgn", `
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 58708
[start map]
0 0
[boss map]
0 0
[event monster random map]
0
[event monster]
1 1000 21100 1 1000 1 1 5
[event monster2]
2 2000 31100
`)

	dungeon := ParseDungeon(26, "dungeon/event_monster.dgn", doc)
	if !dungeon.Metadata.EventMonsterRandomMap.Set || dungeon.Metadata.EventMonsterRandomMap.Value != 0 {
		t.Fatalf("event-monster random-map selector=%+v", dungeon.Metadata.EventMonsterRandomMap)
	}
	if len(dungeon.Metadata.EventMonsters) != 2 ||
		dungeon.Metadata.EventMonsters[0].Kind != "event monster" ||
		dungeon.Metadata.EventMonsters[1].Kind != "event monster2" {
		t.Fatalf("event-monster definitions=%+v", dungeon.Metadata.EventMonsters)
	}
	want := []int64{1, 1000, 21100, 1, 1000, 1, 1, 5}
	if !slices.Equal(dungeon.Metadata.EventMonsters[0].Values, want) {
		t.Fatalf("event-monster values=%v want=%v", dungeon.Metadata.EventMonsters[0].Values, want)
	}

	cloned := cloneDungeon(dungeon)
	cloned.Metadata.EventMonsters[0].Values[0] = 99
	if dungeon.Metadata.EventMonsters[0].Values[0] != 1 {
		t.Fatalf("clone aliases source event-monster row: source=%v clone=%v",
			dungeon.Metadata.EventMonsters[0].Values, cloned.Metadata.EventMonsters[0].Values)
	}
	compactDungeonForRuntime(&cloned)
	if len(cloned.Metadata.EventMonsters) != 2 || !cloned.Metadata.EventMonsterRandomMap.Set {
		t.Fatalf("runtime compaction removed typed event-monster metadata: %+v", cloned.Metadata)
	}
}

func TestParseDungeonKeepsEventAndAddedAdmissionItemsSeparate(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/event_mixed.dgn", `
[required item]
1001 2 0
[/required item]
[event required item]
2001 1 0
[/event required item]
[added required item]
3001 3 0
[/added required item]
`)
	dungeon := ParseDungeon(9000, "dungeon/event_mixed.dgn", doc)
	if len(dungeon.Metadata.RequiredItems) != 1 || dungeon.Metadata.RequiredItems[0] != (TicketItem{ItemID: 1001, Count: 2}) {
		t.Fatalf("ordinary required items=%+v", dungeon.Metadata.RequiredItems)
	}
	if len(dungeon.Metadata.EventRequiredItems) != 1 || dungeon.Metadata.EventRequiredItems[0] != (TicketItem{ItemID: 2001, Count: 1}) {
		t.Fatalf("event required items=%+v", dungeon.Metadata.EventRequiredItems)
	}
	if len(dungeon.Metadata.AddedRequiredItems) != 1 || dungeon.Metadata.AddedRequiredItems[0] != (TicketItem{ItemID: 3001, Count: 3}) {
		t.Fatalf("added required items=%+v", dungeon.Metadata.AddedRequiredItems)
	}

	cloned := cloneDungeon(dungeon)
	cloned.Metadata.EventRequiredItems[0].Count = 99
	cloned.Metadata.AddedRequiredItems[0].Count = 99
	if dungeon.Metadata.EventRequiredItems[0].Count != 1 || dungeon.Metadata.AddedRequiredItems[0].Count != 3 {
		t.Fatalf("clone aliases admission evidence: source=%+v clone=%+v", dungeon.Metadata, cloned.Metadata)
	}
}

func TestParseDungeonBossRoomEntranceConditionRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "wrong action", payload: "`[open action]` `[time condition]` 1 0 `[hunt monster]` 1 69201 0 2"},
		{name: "missing time value", payload: "`[clear action]` `[time condition]` 1 `[hunt monster]` 1 69201 0 2"},
		{name: "wrong hunt label", payload: "`[clear action]` `[time condition]` 1 0 `[hunt apc]` 1 69201 0 2"},
		{name: "truncated row", payload: "`[clear action]` `[time condition]` 1 0 `[hunt monster]` 2 69201 0 2 69210 0"},
		{name: "trailing row", payload: "`[clear action]` `[time condition]` 1 0 `[hunt monster]` 1 69201 0 2 7"},
		{name: "non-positive target", payload: "`[clear action]` `[time condition]` 1 0 `[hunt monster]` 1 0 0 2"},
		{name: "non-positive count", payload: "`[clear action]` `[time condition]` 1 0 `[hunt monster]` 1 69201 0 0"},
		{name: "non-integer row", payload: "`[clear action]` `[time condition]` 1 0 `[hunt monster]` 1 69201 `opaque` 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseTestDocument(t, "dungeon/boss_gate_bad.dgn", `
[maze info]
[size]
1 1
[boss room entrance condition]
`+tt.payload+`
`)
			dungeon := ParseDungeon(2005, "dungeon/boss_gate_bad.dgn", doc)
			if len(dungeon.Mazes) != 1 || dungeon.Mazes[0].BossRoomEntranceCondition != nil || len(dungeon.Mazes[0].Diagnostics) == 0 {
				t.Fatalf("malformed condition accepted: maze=%+v", dungeon.Mazes)
			}
		})
	}
}

func TestParseDungeonBossRoomEntranceConditionRejectsDuplicates(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/boss_gate_duplicate.dgn", `
[maze info]
[size]
1 1
[boss room entrance condition]
`+"`[clear action]` `[time condition]` 1 0 `[hunt monster]` 1 69201 0 2"+`
[boss room entrance condition]
`+"`[clear action]` `[time condition]` 1 0 `[hunt monster]` 1 69210 0 2"+`
`)
	dungeon := ParseDungeon(2005, "dungeon/boss_gate_duplicate.dgn", doc)
	if len(dungeon.Mazes) != 1 || dungeon.Mazes[0].BossRoomEntranceCondition != nil || len(dungeon.Mazes[0].Diagnostics) == 0 {
		t.Fatalf("duplicate condition accepted: maze=%+v", dungeon.Mazes)
	}
}

func TestParseDungeonBloodAltarMetadata(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantKind  BloodAltarDungeonKind
		wantRound int64
		valid     bool
	}{
		{name: "endless", body: "[blood dungeon]\n1\n[blood max round]\n15", wantKind: BloodAltarDungeonKindEndless, wantRound: 15, valid: true},
		{name: "ultimate", body: "[blood dungeon]\n2\n[blood max round]\n7", wantKind: BloodAltarDungeonKindUltimate, wantRound: 7, valid: true},
		{name: "empty kind rejected", body: "[blood dungeon]\n[blood max round]\n15", valid: false},
		{name: "invalid kind", body: "[blood dungeon]\n3\n[blood max round]\n15"},
		{name: "invalid round", body: "[blood dungeon]\n1\n[blood max round]\n256"},
		{name: "extra round field", body: "[blood dungeon]\n1\n[blood max round]\n7 8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dungeon := ParseDungeon(11006, "dungeon/altar.dgn", parseTestDocument(t, "dungeon/altar.dgn", tt.body))
			if !dungeon.Metadata.Flags["blood dungeon"] {
				t.Fatalf("blood dungeon compatibility flag missing: %+v", dungeon.Metadata.Flags)
			}
			if tt.valid {
				if dungeon.BloodAltarKind != tt.wantKind || dungeon.BloodMaxRounds != tt.wantRound ||
					dungeon.Metadata.IntegerValues["blood max round"] != tt.wantRound {
					t.Fatalf("blood altar metadata = kind=%d rounds=%d integers=%+v diagnostics=%+v",
						dungeon.BloodAltarKind, dungeon.BloodMaxRounds, dungeon.Metadata.IntegerValues, dungeon.Diagnostics)
				}
				return
			}
			if len(dungeon.Diagnostics) == 0 || (dungeon.BloodAltarKind != 0 && dungeon.BloodMaxRounds != 0) {
				t.Fatalf("invalid blood altar metadata accepted: %+v", dungeon)
			}
		})
	}
}

func TestParseDungeonInfernoMetadataAfterMazeUsesClosedQuestList(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/inferno.dgn", `
[inferno dungeon]
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 80117
[start map]
0 0
[boss map]
0 0
[inferno quest]
10104
[/inferno quest]
`)
	dungeon := ParseDungeon(7527, "dungeon/inferno.dgn", doc)
	if !dungeon.Metadata.InfernoDungeon || len(dungeon.Metadata.InfernoQuestIDs) != 1 || dungeon.Metadata.InfernoQuestIDs[0] != 10104 {
		t.Fatalf("inferno metadata=%+v", dungeon.Metadata)
	}
}

func TestParseDungeonSpecialPassiveObjectItemGroups(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/passive_items.dgn", `
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 76512
[start map]
0 0
[boss map]
0 0
[special passive object item]
0 -1 3 1069 1500 1071 500 1072 3000 1 -1 0 2 85 1 10099777 9400
[/special passive object item]
`)
	dungeon := ParseDungeon(102, "dungeon/passive_items.dgn", doc)
	if len(dungeon.Mazes) != 1 {
		t.Fatalf("mazes=%+v", dungeon.Mazes)
	}
	// Record 1 is declared empty (`1 -1 0`), so the following record sits at
	// vector position 2 and carries its own level override 85. The table is
	// dungeon-wide even when the file places it under a maze.
	if len(dungeon.Mazes[0].SpecialPassiveObjectItems) != 0 {
		t.Fatalf("maze retained route-local rows=%+v", dungeon.Mazes[0].SpecialPassiveObjectItems)
	}
	rows := dungeon.Metadata.SpecialPassiveObjectItems
	if len(rows) != 4 || rows[0].Index != 0 || rows[1].Index != 0 || rows[2].Index != 0 || rows[3].Index != 2 ||
		rows[0].ItemID != 1069 || rows[0].DropRate != 1500 || rows[0].LevelOverride != -1 ||
		rows[3].LevelOverride != 85 || rows[3].ItemID != 10099777 || rows[3].DropRate != 9400 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestParseParticularHellPromotesTrailingPassiveItemsToEveryMaze(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/particular_hell.dgn", `
[dungeon type]
`+"`particular hell`"+`
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 100
[start map]
0 0
[boss map]
0 0
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 101
[start map]
0 0
[boss map]
0 0
[special passive object item]
0 -1 1 3276 5000
[/special passive object item]
`)
	dungeon := ParseDungeon(316, "dungeon/particular_hell.dgn", doc)
	rows := dungeon.Metadata.SpecialPassiveObjectItems
	if len(rows) != 1 || rows[0].ItemID != 3276 || rows[0].DropRate != 5000 {
		t.Fatalf("particular-hell rows=%+v", rows)
	}
	for index, maze := range dungeon.Mazes {
		if len(maze.SpecialPassiveObjectItems) != 0 {
			t.Fatalf("maze=%d retained promoted rows=%+v", index, maze.SpecialPassiveObjectItems)
		}
	}
}

func TestParseSpecialDungeonPromotesTrailingPassiveItemsToEveryMaze(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/special.dgn", `
[special dungeon]
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 100
[start map]
0 0
[boss map]
0 0
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 101
[start map]
0 0
[boss map]
0 0
[special passive object item]
0 -1 1 10088439 10000
[/special passive object item]
`)
	dungeon := ParseDungeon(2007, "dungeon/special.dgn", doc)
	rows := dungeon.Metadata.SpecialPassiveObjectItems
	if len(rows) != 1 || rows[0].Index != 0 || rows[0].ItemID != 10088439 || rows[0].DropRate != 10000 {
		t.Fatalf("special-dungeon rows=%+v", rows)
	}
	for index, maze := range dungeon.Mazes {
		if len(maze.SpecialPassiveObjectItems) != 0 {
			t.Fatalf("maze=%d retained promoted rows=%+v", index, maze.SpecialPassiveObjectItems)
		}
	}
}

func TestParseDungeonInfernoRouteMetadata(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/infernomode/time_rift.dgn", `
[hell dungeon]
1
[hell party required item count]
24
[inferno dungeon]
[select dungeon normal disable]
[maze info]
[size]
5 2
[maze chance rate]
11875
[greed]
`+"`AAAAAAAAII`"+`
[map specification]
`+"`map`"+` 0 1 80117 `+"`boss`"+` 4 0 80297 80298
[start map]
0 1
[boss map]
4 0
[maze info]
[size]
5 2
[maze chance rate]
2500
[inferno special]
[greed]
`+"`JJFFFFFFEE`"+`
[map specification]
`+"`map`"+` 0 1 80165 `+"`boss`"+` 4 0 80170
[start map]
0 1
[boss map]
4 0
[inferno quest]
10104
`)
	dungeon := ParseDungeon(7527, "dungeon/infernomode/time_rift.dgn", doc)
	if !dungeon.Metadata.InfernoDungeon || !dungeon.Metadata.SelectDungeonNormalDisabled ||
		!dungeon.Metadata.HellPartyRequiredItemCount.Set || dungeon.Metadata.HellPartyRequiredItemCount.Value != 24 {
		t.Fatalf("inferno metadata=%+v", dungeon.Metadata)
	}
	if got := dungeon.Metadata.InfernoQuestIDs; len(got) != 1 || got[0] != 10104 {
		t.Fatalf("inferno quests=%v", got)
	}
	if len(dungeon.Mazes) != 2 || !dungeon.Mazes[0].ChanceRate.Set || dungeon.Mazes[0].ChanceRate.Value != 11875 ||
		!dungeon.Mazes[1].InfernoSpecial || dungeon.Mazes[1].ChanceRate.Value != 2500 {
		t.Fatalf("inferno mazes=%+v", dungeon.Mazes)
	}
}

func TestParseDungeonWarpMapConditionsPreserveOrderedPairsAndAmbiguousSources(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/warp.dgn", `
[maze info]
[size]
3 3
[map specification]
`+"`map`"+` 0 0 100 `+"`map`"+` 2 2 101
[start map]
0 0
[boss map]
2 2
[warp map condition]
[source grid pos]
1 1
[dest grid pos]
2 2
[source grid pos]
1 1
[dest grid pos]
0 0
[source grid pos]
-1 -1
[dest grid pos]
0 0
[/warp map condition]
`)
	dungeon := ParseDungeon(4000, "dungeon/warp.dgn", doc)
	want := []WarpMapCondition{
		{Source: Point{X: 1, Y: 1}, Destination: Point{X: 2, Y: 2}},
		{Source: Point{X: 1, Y: 1}, Destination: Point{X: 0, Y: 0}},
		{Source: Point{X: -1, Y: -1}, Destination: Point{X: 0, Y: 0}},
	}
	if len(dungeon.WarpMapConditions) != len(want) {
		t.Fatalf("warp rules=%+v diagnostics=%+v", dungeon.WarpMapConditions, dungeon.Diagnostics)
	}
	for index := range want {
		if dungeon.WarpMapConditions[index] != want[index] {
			t.Fatalf("warp rule %d=%+v want=%+v", index, dungeon.WarpMapConditions[index], want[index])
		}
	}
	if len(dungeon.Mazes) != 1 || len(dungeon.Mazes[0].WarpMapConditions) != len(want) {
		t.Fatalf("per-maze warp rules=%+v", dungeon.Mazes)
	}
	for index := range want {
		if dungeon.Mazes[0].WarpMapConditions[index] != want[index] {
			t.Fatalf("maze warp rule %d=%+v want=%+v", index, dungeon.Mazes[0].WarpMapConditions[index], want[index])
		}
	}
}

func TestParseDungeonWarpMapConditionsBelongToPhysicalMaze(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/per_maze_warp.dgn", `
[maze info]
[size]
3 3
[map specification]
`+"`map`"+` 0 0 100 `+"`map`"+` 2 2 101
[start map]
0 0
[boss map]
2 2
[warp map condition]
[source grid pos]
2 1
[dest grid pos]
0 0
[/warp map condition]
[maze info]
[size]
3 3
[map specification]
`+"`map`"+` 0 0 200 `+"`map`"+` 2 2 201
[start map]
0 0
[boss map]
2 2
[warp map condition]
[source grid pos]
1 2
[dest grid pos]
2 2
[/warp map condition]
`)
	dungeon := ParseDungeon(4000, "dungeon/per_maze_warp.dgn", doc)
	if len(dungeon.WarpMapConditions) != 2 || len(dungeon.Mazes) != 2 {
		t.Fatalf("dungeon=%+v", dungeon)
	}
	want := []WarpMapCondition{
		{Source: Point{X: 2, Y: 1}, Destination: Point{X: 0, Y: 0}},
		{Source: Point{X: 1, Y: 2}, Destination: Point{X: 2, Y: 2}},
	}
	for mazeIndex := range dungeon.Mazes {
		got := dungeon.Mazes[mazeIndex].WarpMapConditions
		if len(got) != 1 || got[0] != want[mazeIndex] {
			t.Fatalf("maze %d warp rules=%+v want=%+v", mazeIndex, got, want[mazeIndex])
		}
	}
}

func TestParseDungeonLostLandDurations(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/lost_land.dgn", `
[dungeon type]
`+"`lost land`"+`
[lost land parameters]
120000 120000
[limit time]
600 1200
`)
	dungeon := ParseDungeon(4000, "dungeon/lost_land.dgn", doc)
	if got := dungeon.Metadata.LostLandParameters; !got.Set || got.DayMilliseconds != 120000 || got.NightMilliseconds != 120000 {
		t.Fatalf("lost land parameters=%+v", got)
	}
	if got := dungeon.Metadata.LimitTime; !got.Set || got.PrimarySeconds != 600 || got.SecondarySeconds != 1200 {
		t.Fatalf("limit time=%+v", got)
	}
	for _, section := range dungeon.OpaqueSections {
		if key := sectionKey(section.Name); key == "lost land parameters" || key == "limit time" {
			t.Fatalf("typed lost-land section retained as opaque: %s", key)
		}
	}
}

func TestParseDungeonLostLandDurationsRejectMalformedValues(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/lost_land_bad.dgn", `
[lost land parameters]
120000 0
[limit time]
600
`)
	dungeon := ParseDungeon(4000, "dungeon/lost_land_bad.dgn", doc)
	if dungeon.Metadata.LostLandParameters.Set || dungeon.Metadata.LimitTime.Set {
		t.Fatalf("malformed durations parsed: lost_land=%+v limit=%+v", dungeon.Metadata.LostLandParameters, dungeon.Metadata.LimitTime)
	}
	if len(dungeon.Diagnostics) != 2 {
		t.Fatalf("diagnostics=%+v, want two typed duration errors", dungeon.Diagnostics)
	}
}

func TestParseDungeonLostLandLimitTimeInsideClearCondition(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/lost_land_scoped.dgn", `
[maze info]
[size]
1 1
[map specification]
`+"`map`"+` 0 0 11200
[start map]
0 0
[boss map]
0 0
[clear condition]
[limit time]
600 1200
[/clear condition]
`)
	dungeon := ParseDungeon(4000, "dungeon/lost_land_scoped.dgn", doc)
	if got := dungeon.Metadata.LimitTime; !got.Set || got.PrimarySeconds != 600 || got.SecondarySeconds != 1200 {
		t.Fatalf("scoped limit time=%+v", got)
	}
}

func TestParseDungeonMalformedMazeKeepsRawAndDiagnostics(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/bad.dgn", `
[name]
`+"`Bad Dungeon`"+`
[maze info]
[size]
`+"`wide`"+` 2
[map specification]
`+"`map`"+` 0 `+"`bad-y`"+` 100
[start map]
1
[future maze node]
{ `+"`payload`"+` = 4 }
`)
	dungeon := ParseDungeon(78, "dungeon/bad.dgn", doc)
	if len(dungeon.Mazes) != 1 || len(dungeon.Mazes[0].Diagnostics) < 3 {
		t.Fatalf("missing malformed diagnostics: %+v", dungeon.Mazes)
	}
	maze := dungeon.Mazes[0]
	if len(maze.MapSpecifications) != 0 || maze.Start != nil || len(maze.UnknownSections) != 1 || len(maze.Extensions) != 1 {
		t.Fatalf("malformed values crossed boundaries: %+v", maze)
	}
	if maze.Extensions[0].Kind != SectionMixed || len(maze.Extensions[0].Tokens) != 5 {
		t.Fatalf("future extension tokens lost: %+v", maze.Extensions)
	}
}

func TestParseDungeonTutorialMetadataAfterMazeKeepsDungeonOwnership(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/tutorial_after_maze.dgn", `
[name]
`+"`Profession Tutorial`"+`
[maze info]
[size]
1 1
[greed]
`+"`A`"+`
[map specification]
`+"`map`"+` 0 0 100
[start map]
0 0
[boss map]
0 0
[tutorial dungeon]
1
[tutorial dungeon]
1
`)
	dungeon := ParseDungeon(7110, "dungeon/tutorial_after_maze.dgn", doc)
	if !dungeon.Metadata.TutorialDungeon.Set || dungeon.Metadata.TutorialDungeon.Value != 1 {
		t.Fatalf("post-maze tutorial metadata missing: %+v", dungeon.Metadata.TutorialDungeon)
	}
	if len(dungeon.Mazes) != 1 {
		t.Fatalf("tutorial mazes=%+v", dungeon.Mazes)
	}
	for _, section := range dungeon.Mazes[0].UnknownSections {
		if sectionKey(section.Name) == "tutorial dungeon" {
			t.Fatalf("tutorial metadata leaked into maze unknown sections: %+v", dungeon.Mazes[0].UnknownSections)
		}
	}
	for _, extension := range dungeon.Mazes[0].Extensions {
		if sectionKey(extension.Name) == "tutorial dungeon" {
			t.Fatalf("tutorial metadata leaked into maze extensions: %+v", dungeon.Mazes[0].Extensions)
		}
	}
}

func TestDungeonExtensionsClassifyLocalPVFValueShapes(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/extensions.dgn", `
[local flag]
[local integers]
1 -2 3
[local numbers]
1.5 2
[local texts]
`+"`alpha` beta"+`
[local mixed]
1 `+"`alpha`"+` { 2 }
`)
	dungeon := ParseDungeon(79, "dungeon/extensions.dgn", doc)
	if len(dungeon.Extensions) != 5 || len(dungeon.UnknownSections) != 5 {
		t.Fatalf("extensions/raw = %d/%d", len(dungeon.Extensions), len(dungeon.UnknownSections))
	}
	want := []SectionValueKind{SectionFlag, SectionIntegers, SectionNumbers, SectionTexts, SectionMixed}
	for i, kind := range want {
		if dungeon.Extensions[i].Kind != kind {
			t.Errorf("extension %d kind = %s, want %s: %+v", i, dungeon.Extensions[i].Kind, kind, dungeon.Extensions[i])
		}
	}
	if len(dungeon.Extensions[4].Tokens) != 5 || len(dungeon.Extensions[4].Symbols) != 2 {
		t.Fatalf("mixed extension did not retain structure: %+v", dungeon.Extensions[4])
	}
}

func TestDungeonNestedMetadataBlockAfterMazeKeepsDungeonOwnership(t *testing.T) {
	doc := parseTestDocument(t, "dungeon/scoped.dgn", `
[maze info]
[size]
1 1
[greed]
`+"`A`"+`
[start map]
0 0
[boss map]
0 0
[monsterapc diff table]
[normal]
[attack]
1.25
[future balance field]
7
[/normal]
[/monsterapc diff table]
`)
	dungeon := ParseDungeon(80, "dungeon/scoped.dgn", doc)
	if len(dungeon.Mazes) != 1 {
		t.Fatalf("mazes = %+v", dungeon.Mazes)
	}
	if len(dungeon.OpaqueSections) != 1 || sectionKey(dungeon.OpaqueSections[0].Name) != "monsterapc diff table" {
		t.Fatalf("dungeon metadata block = %+v", dungeon.OpaqueSections)
	}
	if len(dungeon.UnknownSections) != 3 || len(dungeon.Extensions) != 4 {
		t.Fatalf("dungeon scoped extensions = unknown=%+v extensions=%+v", dungeon.UnknownSections, dungeon.Extensions)
	}
	for _, section := range dungeon.UnknownSections {
		if len(section.Scope) == 0 || section.Scope[0] != "monsterapc diff table" {
			t.Fatalf("dungeon extension scope = %+v", section)
		}
	}
	if len(dungeon.Mazes[0].UnknownSections) != 0 {
		t.Fatalf("metadata children leaked into maze = %+v", dungeon.Mazes[0].UnknownSections)
	}
}
