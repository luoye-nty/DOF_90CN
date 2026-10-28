package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFDungeonEntryAdmissionLists(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to audit dungeon admission lists")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{worldMapTable: table}

	magicRift, found := table.FindDungeon(316)
	if !found || magicRift.Metadata.DungeonType != "particular hell" ||
		len(magicRift.Metadata.RequiredItems) != 1 ||
		magicRift.Metadata.RequiredItems[0] != (worldmap.TicketItem{ItemID: 10158964, Count: 2}) {
		t.Fatalf("Magic Rift admission metadata=%+v found=%t", magicRift.Metadata, found)
	}
	magicPlan, err := service.resolveCurrentDungeonEntryAdmissionPlan(&runtimeDungeonState{Dungeon: magicRift})
	if err != nil {
		t.Fatal(err)
	}
	if !containsDungeonEntryItem(magicPlan.FreePasses, worldmap.TicketItem{ItemID: 490005479, Count: 1}) ||
		!containsDungeonEntryItem(magicPlan.FreePasses, worldmap.TicketItem{ItemID: 490702499, Count: 1}) ||
		magicPlan.Requirements[0] != (worldmap.TicketItem{ItemID: 10158964, Count: 2}) {
		t.Fatalf("Magic Rift admission plan=%+v", magicPlan)
	}

	timeRift, found := table.FindDungeon(7539)
	if !found || !timeRift.Metadata.InfernoDungeon || !timeRift.Metadata.HellPartyRequiredItemCount.Set ||
		timeRift.Metadata.HellPartyRequiredItemCount.Value != 30 {
		t.Fatalf("Time Rift admission metadata=%+v found=%t", timeRift.Metadata, found)
	}
	timePlan, err := service.resolveCurrentDungeonEntryAdmissionPlan(&runtimeDungeonState{Dungeon: timeRift})
	if err != nil {
		t.Fatal(err)
	}
	if !containsDungeonEntryItem(timePlan.FreePasses, worldmap.TicketItem{ItemID: 490701656, Count: 1}) ||
		len(timePlan.Requirements) != 1 || timePlan.Requirements[0] != (worldmap.TicketItem{ItemID: 3330, Count: 30}) {
		t.Fatalf("Time Rift admission plan=%+v", timePlan)
	}

	timePlaza, found := table.FindDungeon(310)
	if !found || !timePlaza.Metadata.HellPartyRequiredItemCount.Set || timePlaza.Metadata.HellPartyRequiredItemCount.Value != 28 {
		t.Fatalf("Time Plaza hell admission metadata=%+v found=%t", timePlaza.Metadata, found)
	}
	hellPlan, err := service.resolveCurrentDungeonEntryAdmissionPlan(&runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
		Dungeon: timePlaza,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsDungeonEntryItem(hellPlan.FreePasses, worldmap.TicketItem{ItemID: 490006398, Count: 1}) ||
		len(hellPlan.Requirements) != 1 || hellPlan.Requirements[0] != (worldmap.TicketItem{ItemID: 3330, Count: 28}) {
		t.Fatalf("Time Plaza hell admission plan=%+v", hellPlan)
	}
}

func containsDungeonEntryItem(items []worldmap.TicketItem, want worldmap.TicketItem) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
