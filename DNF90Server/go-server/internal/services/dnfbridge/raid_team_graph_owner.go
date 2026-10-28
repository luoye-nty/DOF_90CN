package dnfbridge

// The team dungeon graph — which gate opens which, which mechanism timers are
// armed, which symbols ride along and what counts as the phase being finished —
// is raid-specific. Anton's graph used to be the only one, so its rules were
// written straight into the shared lifecycle helpers and keyed on Anton's own
// map ids (210..216 in phase one, 218..224 in phase two). Those helpers ask this
// owner first, so Luke's graph lives beside Anton's (raid_luke_phase_runtime.go)
// instead of inside it.
//
// Nothing here changes what Anton does.

// runtimeRaidTeamGraphOwner names the raid whose team dungeon graph governs this
// state.
//
// RaidMode is stamped when the raid starts (startRuntimeRaid writes the channel
// owner's mode into a state that was created with zero), so a roster that has
// not started yet, and every state a test builds by hand, reads 0. Zero resolves
// to Anton on purpose: the Luke owner is only reachable through a HiddenRaid
// channel, and that channel stamps mode 2 before any phase can activate, so no
// Luke raid ever reaches a phase with the mode still unset.
func runtimeRaidTeamGraphOwner(state runtimeRaidState) byte {
	if state.RaidMode == runtimeRaidOwnerLuke {
		return runtimeRaidOwnerLuke
	}
	return runtimeRaidOwnerAnton
}

// runtimeRaidUsesAntonTeamGraph reports whether the Anton team graph owns this
// state. Single-mode raids are excluded: they run a flat route through
// advanceRuntimeRaidSingleObjectiveLocked and arm no team mechanism at all.
func runtimeRaidUsesAntonTeamGraph(state runtimeRaidState) bool {
	return state.TailFlag != 1 && runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerAnton
}
