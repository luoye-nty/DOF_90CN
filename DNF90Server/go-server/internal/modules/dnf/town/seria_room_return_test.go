package town

import (
	"context"
	"os"
	"strings"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Elvengard authors its Seria portal in the nested "with animation" form; the
// landing point must be the doorway centre pushed 32 units below the trigger
// and inside the virtual movable band. Mutating a lookup result must not leak
// back into the table.
func TestLoadDerivesSeriaRoomReturnPointFromNestedPortal(t *testing.T) {
	table, err := Load(context.Background(), questConditionTestSource{
		"town/town.lst":          "38 `new_Elvengard.twn`",
		"town/new_Elvengard.twn": "[area]\n0 `Cataclysm/Town/Elvengard/new_Elvengard.map` `[normal]`\n[/area]\n[area]\n1 `new_seria_room.map` `[gate]` 450 234\n[/area]\n[name]\n`Elvengard`",
		"map/Cataclysm/Town/Elvengard/new_Elvengard.map": strings.Join([]string{
			"[town movable area with animation]",
			"[movable area]",
			"[area]",
			"600 150 100 30",
			"[move town]",
			"38 1",
			"[/movable area]",
			"[/town movable area with animation]",
			"[virtual movable area]",
			"0 200 1000 200",
		}, "\n"),
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	area, ok := table.FindSeriaRoomReturnArea(38)
	if !ok || area.ID != 0 || area.SeriaRoomReturn == nil || area.SeriaRoomReturn.X != 650 || area.SeriaRoomReturn.Y != 212 {
		t.Fatalf("Seria return area=(%+v,%t), want area 0 at 650/212", area, ok)
	}
	area.SeriaRoomReturn.X = 999
	again, ok := table.FindSeriaRoomReturnArea(38)
	if !ok || again.SeriaRoomReturn == nil || again.SeriaRoomReturn.X != 650 {
		t.Fatalf("Seria return clone leaked mutation=(%+v,%t)", again, ok)
	}
	// The Seria room itself declares no portal back into itself.
	room, ok := table.FindArea(38, 1)
	if !ok || room.SeriaRoomReturn != nil {
		t.Fatalf("Seria room area=(%+v,%t), want no return point", room, ok)
	}
}

// The flat six-int [town movable area] form and the doorway-side fallback
// order: below the trigger first, then above, right, left; a doorway whose
// below-point falls outside every virtual band lands above it instead.
func TestTownPortalReturnPointPrefersBelowThenFallsBackInsideVirtualArea(t *testing.T) {
	trigger := townMovableArea{x: 600, y: 150, width: 100, height: 30, targetTown: 38, targetArea: 1}
	if point, ok := townPortalReturnPoint(trigger, []int64{0, 200, 1000, 200}); !ok || point != (Gate{X: 650, Y: 212}) {
		t.Fatalf("below point=%+v ok=%t", point, ok)
	}
	if point, ok := townPortalReturnPoint(trigger, []int64{0, 0, 1000, 140}); !ok || point != (Gate{X: 650, Y: 118}) {
		t.Fatalf("above point=%+v ok=%t", point, ok)
	}
	if point, ok := townPortalReturnPoint(trigger, nil); !ok || point != (Gate{X: 650, Y: 212}) {
		t.Fatalf("no-virtual fallback point=%+v ok=%t", point, ok)
	}
	if _, ok := townPortalReturnPoint(trigger, []int64{5000, 5000, 10, 10}); ok {
		t.Fatal("a virtual band that contains no candidate must yield no point")
	}
}

// Runtime PVF: CentralPark (town 31) area 1 portals into the Seria room and
// its derived landing point is 1891/234, next to the doorway on the walkable
// band rather than on the trigger itself.
func TestRealScriptPVFCentralParkSeriaRoomReturnPoint(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the Seria-room return point")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	table, err := Load(context.Background(), archive, Options{})
	if err != nil {
		t.Fatalf("load real town catalog: %v", err)
	}
	area, ok := table.FindSeriaRoomReturnArea(31)
	if !ok || area.ID != 1 || area.SeriaRoomReturn == nil ||
		area.SeriaRoomReturn.X != 1891 || area.SeriaRoomReturn.Y != 234 {
		t.Fatalf("town 31 Seria-room return=(%+v,%t)", area, ok)
	}
}
