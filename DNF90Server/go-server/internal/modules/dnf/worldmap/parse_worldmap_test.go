package worldmap

import "testing"

func TestParseAreaKnownRepeatedAndUnknownSections(t *testing.T) {
	doc := parseTestDocument(t, "worldmap/test.wdm", `
[map image]
`+"`WorldMap/Test.img`"+` 3
[ui path]
`+"`WorldMap/UI/Test.xui`"+`
[name]
`+"`Test Area`"+`
[dungeon]
1 -1 2 20
[/dungeon]
[dungeon]
`+"`[in progress]`"+` 900 3
[/dungeon]
[in progress]
901 4 902 5
[hell dungeon]
1
[hell quest]
100 101
[/hell quest]
[hell quest]
102
[/hell quest]
[hell freepass item]
500 1 501 2
[/hell freepass item]
[inferno freepass item]
600 1 601 2
[/inferno freepass item]
[particularhell freepass item]
700 1 701 2
[/particularhell freepass item]
[item condition]
800
[/item condition]
[future area rule]
`+"`preserve me`"+`
`)

	area := ParseArea(8, "worldmap/test.wdm", doc)
	if area.Name != "Test Area" || area.MapImage.Path != "WorldMap/Test.img" || len(area.MapImage.Params) != 1 || area.UIPath == "" {
		t.Fatalf("unexpected area metadata: %+v", area)
	}
	if len(area.Dungeons) != 5 || area.Dungeons[1].QuestID != 20 || !area.Dungeons[2].InProgressOnly || area.Dungeons[2].DungeonID != 3 || area.Dungeons[2].QuestID != 900 || area.Dungeons[4].DungeonID != 5 || area.Dungeons[4].QuestID != 902 {
		t.Fatalf("unexpected dungeon graph: %+v", area.Dungeons)
	}
	if !area.HellDungeon.Set || area.HellDungeon.Value != 1 || len(area.HellQuestIDs) != 3 || len(area.HellFreePassItems) != 2 ||
		len(area.InfernoFreePassItems) != 2 || len(area.ParticularHellFreePassItems) != 2 ||
		area.InfernoFreePassItems[0] != (TicketItem{ItemID: 600, Count: 1}) ||
		area.ParticularHellFreePassItems[1] != (TicketItem{ItemID: 701, Count: 2}) ||
		len(area.ItemConditions) != 1 || area.ItemConditions[0] != 800 {
		t.Fatalf("unexpected hell data: %+v", area)
	}
	if len(area.UnknownSections) != 1 || len(area.Extensions) != 1 || area.UnknownSections[0].Tokens[0].Value != "preserve me" {
		t.Fatalf("unknown area section was lost: %+v", area.UnknownSections)
	}
}

func TestParseAreaMalformedPairsReportDiagnostics(t *testing.T) {
	doc := parseTestDocument(t, "worldmap/bad.wdm", `
[dungeon]
`+"`[in progress]`"+` 99
[hell freepass item]
1 2 3
[inferno freepass item]
4 5 6
[particularhell freepass item]
7 8 9
`)
	area := ParseArea(9, "worldmap/bad.wdm", doc)
	if len(area.Dungeons) != 0 || len(area.HellFreePassItems) != 1 ||
		len(area.InfernoFreePassItems) != 1 || len(area.ParticularHellFreePassItems) != 1 || len(area.Diagnostics) < 4 {
		t.Fatalf("malformed area parse = %+v", area)
	}
}
