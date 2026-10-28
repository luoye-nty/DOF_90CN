package town

import (
	"context"
	"testing"
)

// The couple-room doorway must land on the native arrival point: the portal's
// vertical center, half a portal width beyond its right edge. For the live row
// `49 224 34 52 101 0` that is (100,250). Landing inside the trigger rectangle
// would re-arm the client's overlap latch and bounce the character back.
func TestLoadDerivesWeddingRoomReturnPointFromPortal(t *testing.T) {
	table, err := Load(context.Background(), memorySource{
		"town/town.lst":  "38 `seria.twn`",
		"town/seria.twn": "[area]\n0 `Cataclysm/Town/Elvengard/new_Elvengard.map` `[normal]`\n[/area]\n[area]\n1 `Cataclysm/Town/Elvengard/new_seria_room.map` `[gate]` 450 234\n[/area]",
		"map/Cataclysm/Town/Elvengard/new_seria_room.map": "[town movable area]\n858 183 30 100 38 0 49 224 34 52 101 0\n[/town movable area]",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	area, ok := table.FindArea(38, 1)
	if !ok || area.WeddingRoomReturn == nil {
		t.Fatalf("wedding doorway missing: ok=%v area=%+v", ok, area)
	}
	if area.WeddingRoomReturn.X != 100 || area.WeddingRoomReturn.Y != 250 {
		t.Fatalf("wedding doorway = %+v, want (100,250)", *area.WeddingRoomReturn)
	}
	// The arrival must sit outside every authored trigger rectangle.
	for _, movable := range area.MovableAreas {
		if movable.Width <= 0 || movable.Height <= 0 {
			continue
		}
		if area.WeddingRoomReturn.X >= movable.X && area.WeddingRoomReturn.X <= movable.X+movable.Width &&
			area.WeddingRoomReturn.Y >= movable.Y && area.WeddingRoomReturn.Y <= movable.Y+movable.Height {
			t.Fatalf("arrival retriggers portal %+v", movable)
		}
	}
	// A map with no couple-room portal must not invent one.
	area, ok = table.FindArea(38, 0)
	if !ok || area.WeddingRoomReturn != nil {
		t.Fatalf("town area gained a couple-room return: ok=%v area=%+v", ok, area)
	}
	// The catalog must not hand out a mutable alias.
	first, _ := table.FindArea(38, 1)
	first.WeddingRoomReturn.X = 999
	again, _ := table.FindArea(38, 1)
	if again.WeddingRoomReturn == nil || again.WeddingRoomReturn.X != 100 {
		t.Fatalf("mutable catalog alias: %+v", again.WeddingRoomReturn)
	}
}

// A nested [movable area]/[move town] form must be understood too, because the
// flat six-int reader cannot see it.
func TestLoadDerivesWeddingRoomReturnFromNestedPortal(t *testing.T) {
	table, err := Load(context.Background(), memorySource{
		"town/town.lst":  "38 `seria.twn`",
		"town/seria.twn": "[area]\n1 `Cataclysm/Town/Elvengard/new_seria_room.map` `[gate]` 450 234\n[/area]",
		"map/Cataclysm/Town/Elvengard/new_seria_room.map": "[town movable area with animation]\n[movable area]\n[area] 60 210 40 60\n[move town] 101 0\n[/movable area]\n[/town movable area with animation]",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	area, ok := table.FindArea(38, 1)
	if !ok || area.WeddingRoomReturn == nil {
		t.Fatalf("nested wedding doorway missing: ok=%v area=%+v", ok, area)
	}
	if area.WeddingRoomReturn.X != 120 || area.WeddingRoomReturn.Y != 240 {
		t.Fatalf("nested wedding doorway = %+v, want (120,240)", *area.WeddingRoomReturn)
	}
}
