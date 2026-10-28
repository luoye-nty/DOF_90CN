package dnfbridge

import (
	"testing"
)

func TestRuntimeRaidTeamGraphOwnerResolvesUnsetModeToAnton(t *testing.T) {
	tests := []struct {
		name      string
		state     runtimeRaidState
		wantOwner byte
		wantAnton bool
	}{
		{
			// RaidMode is stamped at start, so a roster that has not started yet
			// reads 0. Anton is the only team graph such a state can belong to.
			name:      "unset mode is the anton team graph",
			state:     runtimeRaidState{},
			wantOwner: runtimeRaidOwnerAnton,
			wantAnton: true,
		},
		{
			name:      "anton team",
			state:     runtimeRaidState{RaidMode: runtimeRaidOwnerAnton},
			wantOwner: runtimeRaidOwnerAnton,
			wantAnton: true,
		},
		{
			name:      "luke team",
			state:     runtimeRaidState{RaidMode: runtimeRaidOwnerLuke},
			wantOwner: runtimeRaidOwnerLuke,
			wantAnton: false,
		},
		{
			// Single mode runs a flat route and arms no team mechanism, so it is
			// not the Anton team graph even though its owner resolves to Anton.
			name:      "anton single",
			state:     runtimeRaidState{RaidMode: runtimeRaidOwnerAnton, TailFlag: 1},
			wantOwner: runtimeRaidOwnerAnton,
			wantAnton: false,
		},
		{
			name:      "luke single",
			state:     runtimeRaidState{RaidMode: runtimeRaidOwnerLuke, TailFlag: 1},
			wantOwner: runtimeRaidOwnerLuke,
			wantAnton: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runtimeRaidTeamGraphOwner(test.state); got != test.wantOwner {
				t.Fatalf("owner=%d want=%d", got, test.wantOwner)
			}
			if got := runtimeRaidUsesAntonTeamGraph(test.state); got != test.wantAnton {
				t.Fatalf("uses anton team graph=%t want=%t", got, test.wantAnton)
			}
		})
	}
}

func TestLukeTeamPhaseNeverCompletesThroughAntonGates(t *testing.T) {
	// 213 and 215 are Anton's two legs. A Luke state that happens to carry them
	// must still not read as complete, because Luke's phase one ends on its own
	// three sanctums.
	state := runtimeRaidState{
		RaidMode: runtimeRaidOwnerLuke,
		Lifecycle: runtimeRaidLifecycleState{
			Phase: runtimeRaidDungeonPhaseOne,
			Objectives: map[int]runtimeRaidDungeonObjective{
				213: {Phase: runtimeRaidDungeonPhaseOne, MapID: 213, RemainingHP: 0},
				215: {Phase: runtimeRaidDungeonPhaseOne, MapID: 215, RemainingHP: 0},
			},
		},
	}
	if runtimeRaidPhaseObjectivesComplete(state, runtimeRaidDungeonPhaseOne) {
		t.Fatal("luke phase one completed through Anton's legs")
	}
	state.RaidMode = runtimeRaidOwnerAnton
	if !runtimeRaidPhaseObjectivesComplete(state, runtimeRaidDungeonPhaseOne) {
		t.Fatal("anton phase one did not complete on its own legs")
	}

	phaseTwo := runtimeRaidState{
		RaidMode: runtimeRaidOwnerLuke,
		Lifecycle: runtimeRaidLifecycleState{
			Phase: runtimeRaidDungeonPhaseTwo,
			Objectives: map[int]runtimeRaidDungeonObjective{
				220: {Phase: runtimeRaidDungeonPhaseTwo, MapID: 220, RemainingHP: 0},
			},
		},
	}
	if runtimeRaidPhaseObjectivesComplete(phaseTwo, runtimeRaidDungeonPhaseTwo) {
		t.Fatal("luke phase two completed through Anton's heart")
	}
	phaseTwo.RaidMode = runtimeRaidOwnerAnton
	if !runtimeRaidPhaseObjectivesComplete(phaseTwo, runtimeRaidDungeonPhaseTwo) {
		t.Fatal("anton phase two did not complete on its own heart")
	}
}

// TestLukeTeamPhaseTwoSnapshotCarriesNoAntonSymbols proves the snapshot no
// longer mixes namespaces: every key runtimeRaidPhaseTwoSymbolRows emits is an
// etc/raid/anton.etc.symbol id, and those ids mean different things in
// etc/raid/luke/luke.etc.symbol.
func TestLukeTeamPhaseTwoSnapshotCarriesNoAntonSymbols(t *testing.T) {
	state := runtimeRaidState{
		RaidMode: runtimeRaidOwnerLuke,
		Lifecycle: runtimeRaidLifecycleState{
			Phase:        runtimeRaidDungeonPhaseTwo,
			Barrier:      antonBarrierMaximum,
			MovieSymbols: map[uint32]uint32{},
		},
	}
	if rows := runtimeRaidPhaseTwoSymbolRows(state); len(rows) != 0 {
		t.Fatalf("luke phase two symbol rows=%+v want none", rows)
	}
	state.RaidMode = runtimeRaidOwnerAnton
	if rows := runtimeRaidPhaseTwoSymbolRows(state); len(rows) == 0 {
		t.Fatal("anton phase two symbol rows are empty")
	}
}
