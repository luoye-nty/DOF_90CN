package town

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFElvengardSeriaRoom(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the current town catalog")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	table, err := Load(context.Background(), archive, Options{})
	if err != nil {
		t.Fatalf("load real town catalog: %v", err)
	}
	town, ok := table.Find(38)
	if !ok || town.Name != "\u827e\u5c14\u6587\u9632\u7ebf" {
		t.Fatalf("town 38=(%+v,%t)", town, ok)
	}
	area, ok := table.FindArea(38, 1)
	if !ok || area.MapPath != "Cataclysm/Town/Elvengard/new_seria_room.map" ||
		area.Gate == nil || area.Gate.X != 450 || area.Gate.Y != 234 {
		t.Fatalf("town 38 area 1=(%+v,%t)", area, ok)
	}
	gateArea, ok := table.FindGateArea(38)
	if !ok || gateArea.ID != 1 || gateArea.MapPath != area.MapPath ||
		gateArea.Gate == nil || gateArea.Gate.X != 450 || gateArea.Gate.Y != 234 {
		t.Fatalf("town 38 gate area=(%+v,%t)", gateArea, ok)
	}
	if !table.AllowsNPCEnterTown(107, 0, 20276) {
		t.Fatal("current PVF did not index town 107 area 0 NPC-enter target 20276")
	}
	skyTower, ok := table.FindArea(40, 3)
	if !ok || skyTower.MapPath != "Cataclysm/Town/west_coast/WestCoast_D.map" ||
		skyTower.MinLevel != 17 || len(skyTower.NeedQuests) != 1 || skyTower.NeedQuests[0] != 3161 {
		t.Fatalf("town 40 area 3 Sky Tower gate=(%+v,%t)", skyTower, ok)
	}
}
