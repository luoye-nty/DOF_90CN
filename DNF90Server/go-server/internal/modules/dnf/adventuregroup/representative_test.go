package adventuregroup

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRepresentativeOwnerRoundTripAndAccountIsolation(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	owner, err := NewRepresentativeOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	slot := RepresentativeSlot{Job: 11, BaseGrow: 4}
	if err := owner.Select(ctx, "dnf:1", slot, "267", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	state, err := owner.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Selections[slot]; got != (RepresentativeSelection{CharacterID: "267"}) {
		t.Fatalf("selection=%+v", got)
	}
	other, err := owner.Load(ctx, "dnf:2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Selections) != 0 {
		t.Fatalf("cross-account selections=%+v", other.Selections)
	}
	if err := owner.Remove(ctx, "dnf:1", slot, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	state, err = owner.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Selections[slot]; !got.Removed || got.CharacterID != "" {
		t.Fatalf("removed selection=%+v", got)
	}
}

func TestProjectRepresentativeCharactersUsesSelectionRemovalAndStaleFallback(t *testing.T) {
	characters := []dnfrepo.CharacterRecord{
		{CharacterID: "1", Job: "0", Level: 90, Stats: map[string]int64{"grow_type": 0x21}},
		{CharacterID: "2", Job: "0", Level: 90, Stats: map[string]int64{"grow_type": 0x31}},
		{CharacterID: "3", Job: "1", Level: 90, Stats: map[string]int64{"grow_type": 0x22}},
		{CharacterID: "4", Job: "1", Level: 89, Stats: map[string]int64{"grow_type": 0x22}},
		{CharacterID: "5", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 0x12}},
	}
	slotA := RepresentativeSlot{Job: 0, BaseGrow: 1}
	slotB := RepresentativeSlot{Job: 1, BaseGrow: 2}
	projected := ProjectRepresentativeCharacters(characters, RepresentativeState{
		Selections: map[RepresentativeSlot]RepresentativeSelection{
			slotA: {CharacterID: "2"},
			slotB: {Removed: true},
		},
	}, 90)
	if len(projected) != 1 || projected[0].CharacterID != "2" {
		t.Fatalf("selected/removal projection=%+v", projected)
	}
	projected = ProjectRepresentativeCharacters(characters, RepresentativeState{
		Selections: map[RepresentativeSlot]RepresentativeSelection{
			slotA: {CharacterID: "999"},
		},
	}, 90)
	if len(projected) != 2 || projected[0].CharacterID != "1" || projected[1].CharacterID != "3" {
		t.Fatalf("stale fallback projection=%+v", projected)
	}
}

func TestRepresentativeCharacterEligibilityUsesPackedGrow(t *testing.T) {
	slot := RepresentativeSlot{Job: 11, BaseGrow: 4}
	character := dnfrepo.CharacterRecord{
		Job: "11", Level: 90, Stats: map[string]int64{"grow_type": 0x24},
	}
	if !RepresentativeCharacterEligible(character, slot, 90) {
		t.Fatal("second-awakening packed grow was rejected")
	}
	character.Stats["grow_type"] = 0x14
	if RepresentativeCharacterEligible(character, slot, 90) {
		t.Fatal("first-awakening packed grow was accepted")
	}
	character.Stats["grow_type"] = 0x24
	character.Level = 89
	if RepresentativeCharacterEligible(character, slot, 90) {
		t.Fatal("below-cap character was accepted")
	}
}
