package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// TestCurrentDungeonRandomizedCreatureQueueKeepsNeutralTeamOnly pins the op479
// queue to NEUTRAL creatures.
//
// Dropping the team filter (tried live 2026-09-16T23:3xZ) did not make 218's
// hostile 托梅尔 appear: his association row already materializes him, and the
// queue produced a second, *friendly* 托梅尔 next to it -- the op479 descriptor has
// no faction field, so everything in the queue is team-neutral for the client.
// 58530 normalizes to team "monster", so it must stay out of this queue and keep
// riding the op29 association row (fifth value 0).
func TestCurrentDungeonRandomizedCreatureQueueKeepsNeutralTeamOnly(t *testing.T) {
	room := worldmap.RoomCoordinate{X: 1, Y: 4}
	groups := [][]currentDungeonRandomizedObjectSelection{{
		{
			Room: room, Family: currentDungeonRandomizedMonsterFamily, Team: "monster",
			DefinitionID: 58530, ObjectKey: 402, Level: 114,
			Position: worldmap.Point{X: 459, Y: 256},
		},
		{
			Room: room, Family: currentDungeonRandomizedMonsterFamily, Team: "neutral",
			DefinitionID: 65673, ObjectKey: 403, Level: 114,
			Position: worldmap.Point{X: 459, Y: 256},
		},
		{
			// Passive objects belong to the association rows only.
			Room: room, Family: currentDungeonRandomizedPassiveFamily, Team: "neutral",
			DefinitionID: 18865, ObjectKey: 404, Level: 114,
			Position: worldmap.Point{X: 459, Y: 256},
		},
	}}
	body, count, err := buildCurrentDungeonRandomizedCreatureQueue(room, groups)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(body) != 23 {
		t.Fatalf("queue count=%d body=%x", count, body)
	}
	if got := uint32(body[3]) | uint32(body[4])<<8 | uint32(body[5])<<16 | uint32(body[6])<<24; got != 65673 {
		t.Fatalf("queue definition=%d want 65673 (neutral row only) body=%x", got, body)
	}
	if got := uint16(body[7]) | uint16(body[8])<<8; got != 114 {
		t.Fatalf("queue level=%d want 114 body=%x", got, body)
	}
	if body[22] != currentDungeonExplicitPositionMode {
		t.Fatalf("queue position mode=%d want %d", body[22], currentDungeonExplicitPositionMode)
	}
	if other, otherCount, err := buildCurrentDungeonRandomizedCreatureQueue(worldmap.RoomCoordinate{X: 0, Y: 0}, groups); err != nil || otherCount != 0 || other != nil {
		t.Fatalf("foreign room queue=%x count=%d err=%v", other, otherCount, err)
	}
}
