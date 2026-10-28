package dnfbridge

import (
	"os"
	"strings"
)

// 修炼道场 (5000) consumable handling. Live 2026-09-12: the client sends the
// ordinary op44 use and the server deducted the stack; after the deduction was
// removed the player reported the count looking like it gained one, because the
// client adjusts its own stack and then applied the authoritative op14 row.
//
//	training_room_consumables.txt / DNF90_TRAINING_ROOM_CONSUMABLES
//	    keep    (default) run the use for its effect, deduct nothing and send no
//	            item row, so the stack never moves on the wire
//	    refuse  acknowledge the use as a failure: the count never moves, but the
//	            consumable's effect does not apply either
const (
	currentDungeonTrainingRoomConsumableKeep   = "keep"
	currentDungeonTrainingRoomConsumableRefuse = "refuse"
)

func currentDungeonTrainingRoomConsumableMode() string {
	const key = "DNF90_TRAINING_ROOM_CONSUMABLES"
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == currentDungeonTrainingRoomConsumableRefuse {
		return currentDungeonTrainingRoomConsumableRefuse
	}
	return currentDungeonTrainingRoomConsumableKeep
}

// currentDungeonTrainingRoomRefusesConsumables reports the diagnostic mode that
// answers every use with a failure instead of a silent no-deduction use.
func currentDungeonTrainingRoomRefusesConsumables() bool {
	return currentDungeonTrainingRoomConsumableMode() == currentDungeonTrainingRoomConsumableRefuse
}
