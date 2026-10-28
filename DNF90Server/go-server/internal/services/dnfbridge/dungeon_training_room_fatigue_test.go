package dnfbridge

import (
	"errors"
	"testing"

	alignedcmd "longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 修炼道场 (5000) is the free training room. Live 2026-09-12 13:25:33: a character
// with fatigue=0 was refused entry with
//
//	game-dungeon-select-blocked dungeon_id=5000 reason=dungeon_owner_validation_failed
//	  error=dnf dungeon character fatigue is exhausted: fatigue=0
//
// because this data set's 5000 metadata has neither "no fatigue" nor
// "enter without fatigue". These tests pin the explicit exemption.
func TestCurrentDungeonTrainingRoomEntryIgnoresFatigue(t *testing.T) {
	character := dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{"fatigue": 0, "fatigue_limit": 156}}
	session := &gameSession{}
	session.party.state = alignedcmd.PartyState{Members: []alignedcmd.PartyMemberState{{UserID: 1}}}

	training := worldmap.Dungeon{ID: currentDungeonTrainingRoomID}
	if err := validateDungeonEntry(character, training, session, false); err != nil {
		t.Fatalf("training room rejected at fatigue=0: %v", err)
	}

	ordinary := worldmap.Dungeon{ID: 700}
	if err := validateDungeonEntry(character, ordinary, session, false); !errors.Is(err, dnfdungeon.ErrEntryFatigueExhausted) {
		t.Fatalf("ordinary dungeon error=%v want ErrEntryFatigueExhausted", err)
	}
}

// TestCurrentSessionDungeonTrainingRoomGatesFreeConsumables pins the session-level
// decision the aligned dispatch uses: only a run that this session owns counts,
// and an ordinary run must keep consuming.
func TestCurrentSessionDungeonTrainingRoomGatesFreeConsumables(t *testing.T) {
	session := &gameSession{selectedCharacterID: 1}
	if currentSessionDungeonIsTrainingRoom(session) || currentSessionDungeonIgnoresConsumables(session) {
		t.Fatal("a session without a run must not look like the training room")
	}
	session.dungeon.runtime = &runtimeDungeonState{
		Dungeon:   worldmap.Dungeon{ID: currentDungeonTrainingRoomID},
		Character: dnfrepo.CharacterRecord{CharacterID: "1"},
	}
	if !currentSessionDungeonIsTrainingRoom(session) || !currentSessionDungeonIgnoresConsumables(session) {
		t.Fatal("training-room run not detected")
	}
	if currentSessionDungeonIsPractice(session) {
		t.Fatal("training room is not a practice route")
	}
	session.dungeon.runtime.Dungeon = worldmap.Dungeon{ID: 700}
	if currentSessionDungeonIsTrainingRoom(session) || currentSessionDungeonIgnoresConsumables(session) {
		t.Fatal("an ordinary run must keep consuming")
	}
}

func TestCurrentDungeonTrainingRoomDoesNotConsumeFatigue(t *testing.T) {
	runtime := &runtimeDungeonState{
		Dungeon:   worldmap.Dungeon{ID: currentDungeonTrainingRoomID},
		Character: dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{"fatigue": 0}},
	}
	if currentDungeonRuntimeConsumesFatigue(runtime) {
		t.Fatal("training room must not consume fatigue")
	}

	runtime.Dungeon = worldmap.Dungeon{ID: 700}
	if !currentDungeonRuntimeConsumesFatigue(runtime) {
		t.Fatal("ordinary dungeon must still consume fatigue")
	}

	// PVF-marked free dungeons keep working exactly as before.
	runtime.Dungeon = worldmap.Dungeon{ID: 701, Metadata: worldmap.DungeonMetadata{NoFatigue: true}}
	if currentDungeonRuntimeConsumesFatigue(runtime) {
		t.Fatal("PVF no-fatigue dungeon must not consume fatigue")
	}
	runtime.Dungeon = worldmap.Dungeon{
		ID:       702,
		Metadata: worldmap.DungeonMetadata{Flags: map[string]bool{"enter without fatigue": true}},
	}
	if currentDungeonRuntimeConsumesFatigue(runtime) {
		t.Fatal("PVF enter-without-fatigue dungeon must not consume fatigue")
	}
}
