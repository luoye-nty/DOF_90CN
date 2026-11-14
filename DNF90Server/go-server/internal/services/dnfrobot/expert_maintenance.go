package dnfrobot

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	expertMachineCheckInterval     = 5 * time.Second
	expertMachineRetryInterval     = 15 * time.Second
	expertMachineRecoveryWaitLimit = 30 * time.Second
	defaultExpertStoreCost         = uint32(500)
)

// maintainExpertMachine checks server-owned expert-machine endurance for a
// running expert stall. Recovery runs asynchronously so the one-second
// automation loop cannot be blocked by a stop/relogin/open sequence.
func (m *Manager) maintainExpertMachine(now time.Time, runtime *profileRuntime) {
	if m == nil || runtime == nil {
		return
	}
	provider, ok := m.endpoint.(ExpertMachineMaintenance)
	if !ok {
		return
	}

	runtime.mu.Lock()
	client := runtime.client
	profile := runtime.profile
	if client == nil || profile.ExpertStoreKind == "" {
		runtime.nextExpertCheck = time.Time{}
		runtime.mu.Unlock()
		return
	}
	snapshot := client.Snapshot()
	if !snapshot.ExpertStoreActive {
		runtime.nextExpertCheck = time.Time{}
		runtime.mu.Unlock()
		return
	}
	if runtime.expertRecovering {
		runtime.mu.Unlock()
		return
	}
	if runtime.nextExpertCheck.IsZero() {
		runtime.nextExpertCheck = now.Add(expertMachineCheckInterval)
		runtime.mu.Unlock()
		return
	}
	if now.Before(runtime.nextExpertCheck) {
		runtime.mu.Unlock()
		return
	}
	runtime.expertRecovering = true
	runtime.nextExpertCheck = now.Add(expertMachineCheckInterval)
	runtime.mu.Unlock()

	go func() {
		queryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		state, err := provider.RobotExpertMachineState(queryCtx, profile.AccountID, profile.CharacterID)
		cancel()
		if err == nil && expertMachineBelowTenPercent(state) {
			err = m.recoverExpertMachineRuntime(runtime, profile.ID, provider)
		}
		runtime.mu.Lock()
		runtime.expertRecovering = false
		if err != nil {
			runtime.nextExpertCheck = time.Now().UTC().Add(expertMachineRetryInterval)
		}
		runtime.mu.Unlock()
		if err != nil {
			m.recordAutomationError(runtime, fmt.Errorf("expert machine maintenance: %w", err))
		}
	}()
}

func expertMachineBelowTenPercent(state ExpertMachineState) bool {
	return state.MaxEndurance > 0 && state.Endurance >= 0 &&
		state.Endurance*100 < state.MaxEndurance*10
}

func (m *Manager) recoverExpertMachineRuntime(runtime *profileRuntime, id string, provider ExpertMachineMaintenance) error {
	if runtime == nil {
		return errors.New("robot expert-machine recovery runtime is unavailable")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	operation := m.BeginOperation("expert-machine-repair", id)
	err := m.recoverExpertMachineLocked(runtime, id, provider)
	m.CompleteOperation(operation.ID, fmt.Sprintf("robot=%s", id), err)
	return err
}

func (m *Manager) recoverExpertMachineLocked(runtime *profileRuntime, id string, provider ExpertMachineMaintenance) error {
	runtime.mu.RLock()
	profile := runtime.profile
	client := runtime.client
	cost := runtime.expertStoreCost
	runtime.mu.RUnlock()
	if profile.ExpertStoreCost != nil {
		cost = *profile.ExpertStoreCost
	}
	if client == nil {
		return ErrNotReady
	}
	if cost == 0 {
		cost = defaultExpertStoreCost
	}
	if client.Snapshot().ExpertStoreActive {
		if err := client.CloseExpertJobStore(context.Background()); err != nil {
			return fmt.Errorf("close low-endurance expert store: %w", err)
		}
	}
	if err := m.stopProfileUnlocked(id); err != nil {
		return fmt.Errorf("stop robot for expert-machine repair: %w", err)
	}

	repairCtx, repairCancel := context.WithTimeout(context.Background(), 15*time.Second)
	state, err := provider.RestoreRobotExpertMachine(repairCtx, profile.AccountID, profile.CharacterID)
	repairCancel()
	if err != nil {
		return fmt.Errorf("restore expert-machine endurance offline: %w", err)
	}
	if state.MaxEndurance <= 0 || state.Endurance < state.MaxEndurance {
		return fmt.Errorf("expert-machine repair incomplete: endurance=%d max=%d", state.Endurance, state.MaxEndurance)
	}

	if _, err := m.StartProfile(id); err != nil {
		return fmt.Errorf("restart robot after expert-machine repair: %w", err)
	}
	if _, err := m.waitForProfileState(id, StateTownReady, expertMachineRecoveryWaitLimit); err != nil {
		return fmt.Errorf("wait for repaired robot town-ready state: %w", err)
	}
	m.mu.RLock()
	replacement := m.running[profileKey(id)]
	m.mu.RUnlock()
	if replacement == nil {
		return ErrNotReady
	}
	replacement.mu.Lock()
	replacement.expertStoreCost = cost
	replacement.expertAutoOpenAttempted = true
	replacementClient := replacement.client
	replacement.mu.Unlock()
	if replacementClient == nil {
		return ErrNotReady
	}
	openCtx, openCancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = replacementClient.OpenExpertJobStore(openCtx, cost)
	openCancel()
	if err != nil {
		return fmt.Errorf("reopen expert store after repair: %w", err)
	}
	m.logger.Info("robot expert-machine automatically repaired",
		"robot_id", id,
		"character_id", profile.CharacterID,
		"grade", state.Grade,
		"endurance", state.Endurance,
		"max_endurance", state.MaxEndurance)
	return nil
}

func (m *Manager) waitForProfileState(id string, wanted State, timeout time.Duration) (RuntimeSnapshot, error) {
	deadline := time.Now().Add(timeout)
	for {
		snapshot, err := m.Snapshot(id)
		if err == nil && snapshot.State == wanted {
			return snapshot, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return RuntimeSnapshot{}, err
			}
			return snapshot, fmt.Errorf("robot did not reach %s within %s", wanted, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
