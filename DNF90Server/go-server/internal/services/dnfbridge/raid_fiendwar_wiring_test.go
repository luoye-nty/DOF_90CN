package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// TestRuntimeRaidOwnerModeCoversAllThreeRaidChannels pins the channel -> owner
// mapping.  A type-42 channel used to fall through to 0, and 0 is the value
// handleOnlineRaidCommand reads as "this session is in no raid channel", so every
// raid command from a Fiend War channel was dropped before it reached a handler.
func TestRuntimeRaidOwnerModeCoversAllThreeRaidChannels(t *testing.T) {
	cases := []struct {
		name        string
		channelType byte
		want        byte
	}{
		{name: "anton", channelType: dnfenum.RaidChannelType, want: runtimeRaidOwnerAnton},
		{name: "luke", channelType: dnfenum.HiddenRaidType, want: runtimeRaidOwnerLuke},
		{name: "fiendwar", channelType: dnfenum.FiendWarRaidChannelType, want: runtimeRaidOwnerFiendWar},
		{name: "ordinary town", channelType: 11, want: 0},
		{name: "pvp", channelType: 24, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := &gameSession{channel: channelcatalog.Channel{Type: tc.channelType}}
			if got := runtimeRaidModeForSession(session); got != tc.want {
				t.Fatalf("runtimeRaidModeForSession(channel type %d) = %d, want %d", tc.channelType, got, tc.want)
			}
		})
	}
	// The three owners must stay distinct: a mode collision would make one raid
	// channel start another raid's config.
	if runtimeRaidOwnerAnton == runtimeRaidOwnerFiendWar ||
		runtimeRaidOwnerLuke == runtimeRaidOwnerFiendWar ||
		runtimeRaidOwnerAnton == runtimeRaidOwnerLuke {
		t.Fatal("raid owner modes are not distinct")
	}
	if runtimeRaidOwnerFiendWar != currentRaidFiendWarMode {
		t.Fatalf("runtimeRaidOwnerFiendWar = %d, want the catalogued Fiend War mode %d",
			runtimeRaidOwnerFiendWar, currentRaidFiendWarMode)
	}
}

// TestRuntimeRaidPhaseCountIsPerOwner pins the phase-count gate a start walks
// through.  etc/raid/fiendwar/fiendwar.etc declares [PHASE MAX] 1 and
// parseFiendWarBattle synthesises exactly one phase, so the previous
// `len(config.Phases) != 2` refused every Fiend War start.  The rule is now per
// owner, and Anton/Luke must STILL require exactly two.
func TestRuntimeRaidPhaseCountIsPerOwner(t *testing.T) {
	one := &raid.AntonConfig{Phases: make([]raid.AntonPhase, 1)}
	two := &raid.AntonConfig{Phases: make([]raid.AntonPhase, 2)}
	three := &raid.AntonConfig{Phases: make([]raid.AntonPhase, 3)}
	cases := []struct {
		name   string
		config *raid.AntonConfig
		mode   byte
		want   bool
	}{
		{name: "fiendwar one phase", config: one, mode: runtimeRaidOwnerFiendWar, want: true},
		{name: "fiendwar two phases", config: two, mode: runtimeRaidOwnerFiendWar, want: false},
		{name: "fiendwar three phases", config: three, mode: runtimeRaidOwnerFiendWar, want: false},
		{name: "anton two phases", config: two, mode: runtimeRaidOwnerAnton, want: true},
		{name: "anton one phase", config: one, mode: runtimeRaidOwnerAnton, want: false},
		{name: "anton three phases", config: three, mode: runtimeRaidOwnerAnton, want: false},
		{name: "luke two phases", config: two, mode: runtimeRaidOwnerLuke, want: true},
		{name: "luke one phase", config: one, mode: runtimeRaidOwnerLuke, want: false},
		{name: "nil config", config: nil, mode: runtimeRaidOwnerAnton, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeRaidPhaseCountMatchesOwner(tc.config, tc.mode); got != tc.want {
				t.Fatalf("runtimeRaidPhaseCountMatchesOwner = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestRuntimeRaidDungeonPhaseForMapAddsOnlyTheFiendWarRange pins the closed
// 4953..4978 range, and -- the part that matters for blast radius -- asserts that
// every id which mapped before still maps to the same phase.
func TestRuntimeRaidDungeonPhaseForMapAddsOnlyTheFiendWarRange(t *testing.T) {
	unchanged := []struct {
		mapID int
		want  byte
	}{
		{210, runtimeRaidDungeonPhaseOne},
		{216, runtimeRaidDungeonPhaseOne},
		{217, 0},
		{218, runtimeRaidDungeonPhaseTwo},
		{224, runtimeRaidDungeonPhaseTwo},
		{225, 0},
		{5314, runtimeRaidDungeonPhaseOne},
		{5322, runtimeRaidDungeonPhaseOne},
		{5323, runtimeRaidDungeonPhaseOne},
		{5334, runtimeRaidDungeonPhaseOne},
		{5335, 0},
	}
	for _, tc := range unchanged {
		got, ok := runtimeRaidDungeonPhaseForMap(tc.mapID)
		if tc.want == 0 {
			if ok {
				t.Fatalf("map %d mapped to phase %d, want no raid phase", tc.mapID, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Fatalf("runtimeRaidDungeonPhaseForMap(%d) = (%d,%t), want (%d,true)", tc.mapID, got, ok, tc.want)
		}
	}

	// Fiend War's [DUNGEON INDEX] is 4953..4978 and dungeon/dungeon.lst has
	// neither 4952 nor 4979, so the new range must be exactly those two ends.
	for _, mapID := range []int{4953, 4978} {
		got, ok := runtimeRaidDungeonPhaseForMap(mapID)
		if !ok || got != runtimeRaidDungeonPhaseOne {
			t.Fatalf("fiendwar map %d = (%d,%t), want phase one", mapID, got, ok)
		}
	}
	for _, mapID := range []int{4952, 4979} {
		if got, ok := runtimeRaidDungeonPhaseForMap(mapID); ok {
			t.Fatalf("map %d mapped to phase %d but is outside the Fiend War range", mapID, got)
		}
	}
}
