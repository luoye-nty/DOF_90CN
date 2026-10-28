package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFYellowDragonTournamentDefinition(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNF_WORLDMAP_REAL_PVF_SMOKE is not set")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipAreas: true})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeonPath("dungeon/shonantournament/yellowdragon.dgn")
	if !ok {
		t.Fatal("yellow dragon dungeon is absent")
	}
	tournamentMap, ok := table.FindMapPath("map/tournament/yellowdragon_00.map")
	if !ok {
		t.Fatal("yellow dragon map is absent")
	}
	definition, err := table.TournamentDefinition(dungeon.ID, tournamentMap.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if definition.DungeonID != 120 || definition.MapID == 0 || definition.DifficultyLevel != 72 || definition.PartyCount != 1 {
		t.Fatalf("identity=%+v", definition)
	}
	if len(definition.Candidates) != 15 || len(definition.StartAreas) != 2 || len(definition.ResultCards) != 6 || len(definition.Experience) != 5 || len(definition.RewardItems) != 3 {
		t.Fatalf("definition counts: candidates=%d areas=%d cards=%d exp=%d items=%d", len(definition.Candidates), len(definition.StartAreas), len(definition.ResultCards), len(definition.Experience), len(definition.RewardItems))
	}
	if definition.Experience[4] != 200000 || definition.ResultCards[5].ItemWeight != 100 ||
		len(dungeon.Metadata.RequiredItems) != 1 || dungeon.Metadata.RequiredItems[0] != (TicketItem{ItemID: 4726, Count: 1}) {
		t.Fatalf("reward projection=%+v", definition)
	}
}

func TestRealScriptPVFStoryYellowDragonUsesCardlessRewardPlan(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNF_WORLDMAP_REAL_PVF_SMOKE is not set")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipAreas: true})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeonPath("dungeon/shonantournament/realizeyellowdragon.dgn")
	if !ok {
		t.Fatal("story yellow dragon dungeon is absent")
	}
	tournamentMap, ok := table.FindMap(95265)
	if !ok {
		t.Fatal("story yellow dragon map 95265 is absent")
	}
	definition, err := table.TournamentDefinition(dungeon.ID, tournamentMap.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if definition.DungeonID != 123 || definition.Experience[4] != 0 {
		t.Fatalf("story reward identity dungeon=%d experience=%d", definition.DungeonID, definition.Experience[4])
	}
	runtime := &TournamentRuntime{
		Definition:      definition,
		terminal:        TournamentChampion,
		completedRounds: tournamentRoundCount,
	}
	plan, _, err := runtime.PrepareRewardPlan(1, 1, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Cardless() {
		t.Fatalf("story yellow dragon reward plan is not cardless: upper=%+v lower=%+v", plan.Upper, plan.Lower)
	}
}
