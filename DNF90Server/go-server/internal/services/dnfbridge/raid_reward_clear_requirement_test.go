package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

// The reward gate reads the phase's own [MINIMAL DUNGEON CLEAR COUNT]
// (runtimeRaidMemberClearedGateLocked -> runtimeRaidPhaseMinimalClearRequirement).
//
// The official reward condition is 「1阶段：无条件 / 2阶段：通关2个以上的地下城」,
// which is exactly the pair Luke declares, so the card window must withhold for a
// member whose 地下城通关 column has not reached the phase's requirement.
//
// lukeClearRequirementConfig is that shape: phase one 0, phase two 2, and the
// single-mode phase -- whose PVF block omits the section entirely -- left at the
// shared parser's default of 1 (anton_pvf.go:503), NOT inheriting team phase one's 0.
// All three values are pinned against the shipped PVF by
// raid.TestRealScriptPVFLukePhaseMechanismTables.
func lukeClearRequirementConfig() raid.AntonConfig {
	return raid.AntonConfig{
		Phases: []raid.AntonPhase{
			{Name: "进入源生宫殿", MinimalDungeonClearCount: 0},
			{Name: "进入卓越圣殿", MinimalDungeonClearCount: 2},
		},
		SingleMode: &raid.AntonSingleModeConfig{
			Name:  "进入源生宫殿",
			Phase: raid.AntonPhase{Name: "进入源生宫殿", MinimalDungeonClearCount: 1},
		},
	}
}

// antonClearRequirementConfig is the shipped etc/raid/anton.etc shape: both team
// phases declare 1 (anton_pvf_test.go:544/:565, read from the real PVF) and the
// single phase is defaulted to 1.
func antonClearRequirementConfig() raid.AntonConfig {
	return raid.AntonConfig{
		Phases: []raid.AntonPhase{
			{Name: "阻截安徒恩", MinimalDungeonClearCount: 1},
			{Name: "灭杀安徒恩", MinimalDungeonClearCount: 1},
		},
		SingleMode: &raid.AntonSingleModeConfig{
			Phase: raid.AntonPhase{Name: "阻截安徒恩", MinimalDungeonClearCount: 1},
		},
	}
}

func TestRuntimeRaidMemberClearedGateLockedUsesThePhaseClearRequirement(t *testing.T) {
	t.Parallel()
	lukeConfig := lukeClearRequirementConfig()
	antonConfig := antonClearRequirementConfig()
	luke := &raid.LukeConfig{Raid: &lukeConfig}

	lukeState := func(phase byte) runtimeRaidState {
		return runtimeRaidState{
			RaidMode:   runtimeRaidOwnerLuke,
			LukeConfig: luke,
			Lifecycle:  runtimeRaidLifecycleState{Phase: phase},
		}
	}
	antonState := func(phase byte) runtimeRaidState {
		return runtimeRaidState{
			RaidMode:    runtimeRaidOwnerAnton,
			AntonConfig: &antonConfig,
			Lifecycle:   runtimeRaidLifecycleState{Phase: phase},
		}
	}
	lukeSingle := lukeState(runtimeRaidDungeonPhaseOne)
	lukeSingle.TailFlag = 1
	antonSingle := antonState(runtimeRaidDungeonPhaseOne)
	antonSingle.TailFlag = 1

	tests := []struct {
		name        string
		state       runtimeRaidState
		characterID uint16
		cleared     uint32
		want        bool
	}{
		{"luke phase two withholds at zero clears", lukeState(runtimeRaidDungeonPhaseTwo), 19, 0, false},
		{"luke phase two withholds at one clear", lukeState(runtimeRaidDungeonPhaseTwo), 19, 1, false},
		{"luke phase two pays at two clears", lukeState(runtimeRaidDungeonPhaseTwo), 19, 2, true},
		{"luke phase two pays above the requirement", lukeState(runtimeRaidDungeonPhaseTwo), 19, 3, true},
		{"luke phase one is unconditional", lukeState(runtimeRaidDungeonPhaseOne), 19, 0, true},
		{"luke phase one pays a clearing member too", lukeState(runtimeRaidDungeonPhaseOne), 19, 3, true},
		{"luke single mode withholds at zero clears", lukeSingle, 19, 0, false},
		{"luke single mode pays at one clear", lukeSingle, 19, 1, true},

		{"anton phase one withholds at zero clears", antonState(runtimeRaidDungeonPhaseOne), 19, 0, false},
		{"anton phase one pays at one clear", antonState(runtimeRaidDungeonPhaseOne), 19, 1, true},
		{"anton phase two withholds at zero clears", antonState(runtimeRaidDungeonPhaseTwo), 19, 0, false},
		{"anton phase two pays at one clear", antonState(runtimeRaidDungeonPhaseTwo), 19, 1, true},
		{"anton single mode withholds at zero clears", antonSingle, 19, 0, false},
		{"anton single mode pays at one clear", antonSingle, 19, 1, true},

		{"character zero is never eligible", lukeState(runtimeRaidDungeonPhaseTwo), 0, 9, false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.state.Lifecycle.PhaseClearCounts = map[uint16]uint32{19: test.cleared}
			if got := runtimeRaidMemberClearedGateLocked(test.state, test.characterID); got != test.want {
				t.Fatalf("cleared=%d eligible=%t want %t", test.cleared, got, test.want)
			}
		})
	}
}

// TestRuntimeRaidClearedGateKeepsTheLegacyRuleWithoutACatalog covers the states a
// hand-built fixture or an unavailable config produces. runtimeRaidPhaseMinimalClearRequirement
// reports them as "no declaration", and a missing declaration must not be read as a
// declared 0 -- otherwise every member of a catalog-less raid would be handed a card.
func TestRuntimeRaidClearedGateKeepsTheLegacyRuleWithoutACatalog(t *testing.T) {
	t.Parallel()
	states := map[string]runtimeRaidState{
		"no config":  {Lifecycle: runtimeRaidLifecycleState{Phase: runtimeRaidDungeonPhaseOne}},
		"phase zero": {AntonConfig: &raid.AntonConfig{}, Lifecycle: runtimeRaidLifecycleState{Phase: 0}},
		"phase past the catalog": {
			AntonConfig: &raid.AntonConfig{Phases: []raid.AntonPhase{{MinimalDungeonClearCount: 1}}},
			Lifecycle:   runtimeRaidLifecycleState{Phase: runtimeRaidDungeonPhaseTwo},
		},
		"single mode without a single catalog": {
			TailFlag:    1,
			AntonConfig: &raid.AntonConfig{Phases: []raid.AntonPhase{{MinimalDungeonClearCount: 1}}},
			Lifecycle:   runtimeRaidLifecycleState{Phase: runtimeRaidDungeonPhaseOne},
		},
	}
	for name, state := range states {
		state := state
		t.Run(name, func(t *testing.T) {
			state.Lifecycle.PhaseClearCounts = map[uint16]uint32{19: 0}
			if runtimeRaidMemberClearedGateLocked(state, 19) {
				t.Fatal("a missing declaration was read as an unconditional phase")
			}
			state.Lifecycle.PhaseClearCounts[19] = 1
			if !runtimeRaidMemberClearedGateLocked(state, 19) {
				t.Fatal("the pre-PVF rule no longer pays a member that cleared a gate")
			}
		})
	}
}

// TestRuntimeRaidAntonClearRequirementIsBitForBitUnchanged is the A/B that makes the
// Luke fix safe. Every Anton phase declares [MINIMAL DUNGEON CLEAR COUNT] 1:
//
//   - team phases cannot declare anything else, because antonParsePhaseWithPartyMax
//     demands a positive value (anton_pvf.go:586);
//   - the single phase's PVF block omits the section entirely and is defaulted to 1
//     (anton_pvf.go:503).
//
// The clear counter is a uint32, so `count >= 1` and the pre-PVF `count > 0` are the
// same predicate. This sweep proves the new requirement cannot diverge for any count
// a phase can register, including the counts the display cap can hold.
func TestRuntimeRaidAntonClearRequirementIsBitForBitUnchanged(t *testing.T) {
	t.Parallel()
	config := antonClearRequirementConfig()
	for _, phase := range []byte{runtimeRaidDungeonPhaseOne, runtimeRaidDungeonPhaseTwo} {
		for tail := byte(0); tail <= 1; tail++ {
			state := runtimeRaidState{
				RaidMode:    runtimeRaidOwnerAnton,
				TailFlag:    tail,
				AntonConfig: &config,
				Lifecycle:   runtimeRaidLifecycleState{Phase: phase},
			}
			for cleared := uint32(0); cleared <= 8; cleared++ {
				state.Lifecycle.PhaseClearCounts = map[uint16]uint32{19: cleared}
				want := cleared > 0
				if got := runtimeRaidMemberClearedGateLocked(state, 19); got != want {
					t.Fatalf("phase=%d tail=%d cleared=%d eligible=%t want %t",
						phase, tail, cleared, got, want)
				}
			}
		}
	}
}

// TestRuntimeRaidLukePhaseTwoRewardNeedsTwoRegisteredClears drives the requirement
// through the real registration path instead of a hand-written counter, and pins the
// two ends of the official rule: Luke phase two pays only once the 地下城通关 column
// reaches 2, while Luke phase one is unconditional.
func TestRuntimeRaidLukePhaseTwoRewardNeedsTwoRegisteredClears(t *testing.T) {
	t.Parallel()
	lukeConfig := lukeClearRequirementConfig()
	member := runtimeRaidMemberState{CharID: 19, Name: "leader", GroupIndex: 1, SlotOrder: 0}

	phaseTwo := &runtimeRaidState{
		RaidMode:      runtimeRaidOwnerLuke,
		LukeConfig:    &raid.LukeConfig{Raid: &lukeConfig},
		Members:       []runtimeRaidMemberState{member},
		FrozenMembers: []runtimeRaidMemberState{member},
		Lifecycle:     runtimeRaidLifecycleState{Phase: runtimeRaidDungeonPhaseTwo},
	}
	runtimeRaidNotePhaseClearLocked(phaseTwo, 19, runtimeRaidDungeonPhaseTwo)
	if counts := phaseTwo.Lifecycle.PhaseClearCounts[19]; counts != 1 {
		t.Fatalf("registered clears=%d want 1", counts)
	}
	if runtimeRaidMemberClearedGateLocked(*phaseTwo, 19) {
		t.Fatal("one registered clear already paid the card")
	}
	runtimeRaidNotePhaseClearLocked(phaseTwo, 19, runtimeRaidDungeonPhaseTwo)
	if counts := phaseTwo.Lifecycle.PhaseClearCounts[19]; counts != 2 {
		t.Fatalf("registered clears=%d want 2 (capped at the phase requirement)", counts)
	}
	if !runtimeRaidMemberClearedGateLocked(*phaseTwo, 19) {
		t.Fatal("two registered clears did not pay the card")
	}

	// Phase one declares 0, which is the official 「1阶段：无条件」: the roster is
	// paid without registering a single clear.
	phaseOne := &runtimeRaidState{
		RaidMode:      runtimeRaidOwnerLuke,
		LukeConfig:    &raid.LukeConfig{Raid: &lukeConfig},
		Members:       []runtimeRaidMemberState{member},
		FrozenMembers: []runtimeRaidMemberState{member},
		Lifecycle:     runtimeRaidLifecycleState{Phase: runtimeRaidDungeonPhaseOne},
	}
	if !runtimeRaidMemberClearedGateLocked(*phaseOne, 19) {
		t.Fatal("Luke phase one is unconditional and must pay a member with no clear")
	}
}
