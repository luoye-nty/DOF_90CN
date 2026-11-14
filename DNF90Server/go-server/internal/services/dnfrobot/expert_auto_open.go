package dnfrobot

import (
	"context"
	"fmt"
	"time"
)

const expertAutoOpenRetryInterval = 15 * time.Second

// maintainExpertAutoOpen opens a configured expert stall once per successful
// robot connection, after the character reaches its configured town position.
// A manual close stays closed until the next reconnect/start.
func (m *Manager) maintainExpertAutoOpen(now time.Time, runtime *profileRuntime) {
	if m == nil || runtime == nil {
		return
	}
	runtime.mu.Lock()
	client := runtime.client
	profile := runtime.profile
	if client == nil || profile.ExpertStoreKind == "" || runtime.expertAutoOpenAttempted {
		runtime.mu.Unlock()
		return
	}
	if !runtime.nextExpertAutoOpenCheck.IsZero() && now.Before(runtime.nextExpertAutoOpenCheck) {
		runtime.mu.Unlock()
		return
	}
	snapshot := client.Snapshot()
	if snapshot.ExpertStoreActive {
		runtime.expertAutoOpenAttempted = true
		runtime.mu.Unlock()
		return
	}
	if snapshot.State != StateTownReady || snapshot.StoreActive {
		runtime.mu.Unlock()
		return
	}
	runtime.expertAutoOpenAttempted = true
	cost := uint32(500)
	if profile.ExpertStoreCost != nil {
		cost = *profile.ExpertStoreCost
	}
	runtime.mu.Unlock()

	go func() {
		operation := m.BeginOperation("expert-auto-open", profile.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := m.OpenExpertJobStore(ctx, profile.ID, &cost)
		cancel()
		m.CompleteOperation(operation.ID, fmt.Sprintf("robot=%s cost=%d", profile.ID, cost), err)
		if err != nil {
			runtime.mu.Lock()
			runtime.expertAutoOpenAttempted = false
			runtime.nextExpertAutoOpenCheck = time.Now().UTC().Add(expertAutoOpenRetryInterval)
			runtime.mu.Unlock()
			m.recordAutomationError(runtime, fmt.Errorf("expert auto-open: %w", err))
			return
		}
		m.logger.Info("robot expert store automatically opened after login",
			"robot_id", profile.ID,
			"character_id", profile.CharacterID,
			"cost", cost,
			"village", profile.Village,
			"area", profile.Area)
	}()
}
