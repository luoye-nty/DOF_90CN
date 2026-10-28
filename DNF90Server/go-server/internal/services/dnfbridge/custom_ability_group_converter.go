package dnfbridge

import (
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

const currentCustomAbilityGroupConverterWindow = 90 * time.Second

type currentCustomAbilityGroupConverterPending struct {
	projection alignedcmd.CustomAbilityGroupConverterProjection
	expiresAt  time.Time
}

// currentCustomAbilityGroupConverterSessionState disambiguates the shared
// op1340 wire: an ordinary NPC modification spends PVF modify materials,
// whereas op44 [custom ability set group] opens the same window and makes the
// next op1340 spend exactly that device. The state is runtime-only and always
// bound to one selected character.
type currentCustomAbilityGroupConverterSessionState struct {
	mu       sync.Mutex
	pending  *currentCustomAbilityGroupConverterPending
	inFlight bool
}

func (state *currentCustomAbilityGroupConverterSessionState) arm(
	projection alignedcmd.CustomAbilityGroupConverterProjection,
	now time.Time,
) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.pending = &currentCustomAbilityGroupConverterPending{
		projection: projection,
		expiresAt:  now.Add(currentCustomAbilityGroupConverterWindow),
	}
	state.inFlight = false
}

func (state *currentCustomAbilityGroupConverterSessionState) begin(
	characterID uint16,
	now time.Time,
) (alignedcmd.CustomAbilityGroupConverterProjection, bool, bool) {
	if state == nil || characterID == 0 {
		return alignedcmd.CustomAbilityGroupConverterProjection{}, false, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.pending == nil {
		return alignedcmd.CustomAbilityGroupConverterProjection{}, false, false
	}
	if !now.Before(state.pending.expiresAt) || state.pending.projection.CharacterID != characterID {
		state.pending = nil
		state.inFlight = false
		return alignedcmd.CustomAbilityGroupConverterProjection{}, false, false
	}
	if state.inFlight {
		return alignedcmd.CustomAbilityGroupConverterProjection{}, true, true
	}
	state.inFlight = true
	return state.pending.projection, true, false
}

func (state *currentCustomAbilityGroupConverterSessionState) finish(committed bool) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if committed {
		state.pending = nil
	}
	state.inFlight = false
}

func (state *currentCustomAbilityGroupConverterSessionState) reset() {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.pending = nil
	state.inFlight = false
	state.mu.Unlock()
}
