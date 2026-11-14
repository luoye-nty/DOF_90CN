package dnfrobot

import (
	"context"
	"errors"
	"fmt"
	"time"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

const personalStoreAutoOpenRetryInterval = 15 * time.Second

func profileMovementPolicy(profile dnfrobotdomain.Profile) dnfrobotdomain.AutomationPolicy {
	if profile.RobotType != dnfrobotdomain.RobotTypeNormal {
		return dnfrobotdomain.AutomationPolicy{}
	}
	return dnfrobotdomain.AutomationPolicy{
		Enabled:                     true,
		MoveIntervalMinSeconds:      profile.MoveIntervalMinSeconds,
		MoveIntervalMaxSeconds:      profile.MoveIntervalMaxSeconds,
		MoveRadius:                  profile.MoveRadius,
		ChatIntervalMinSeconds:      0,
		ChatIntervalMaxSeconds:      0,
		MegaphoneIntervalMinSeconds: 0,
		MegaphoneIntervalMaxSeconds: 0,
		StoreIntervalMinSeconds:     0,
		StoreIntervalMaxSeconds:     0,
	}
}

func (m *Manager) maintainProfileSpeech(now time.Time, runtime *profileRuntime) {
	if m == nil || runtime == nil {
		return
	}
	runtime.mu.Lock()
	client := runtime.client
	profile := runtime.profile
	if client == nil || runtime.privateStoreTransition || runtime.recycleActive ||
		len(profile.SpeechMessages) == 0 || profile.SpeechIntervalMinSeconds <= 0 {
		runtime.nextChat = time.Time{}
		runtime.nextMegaphone = time.Time{}
		runtime.mu.Unlock()
		return
	}
	clientState := client.Snapshot().State
	if clientState != StateTownReady && clientState != StateStoreReady {
		runtime.mu.Unlock()
		return
	}
	mode := profile.SpeechMode
	if mode == "" {
		mode = dnfrobotdomain.SpeechModeNormal
	}
	var due bool
	var cursor int
	if mode == dnfrobotdomain.SpeechModeMegaphone {
		runtime.nextChat = time.Time{}
		if runtime.nextMegaphone.IsZero() {
			runtime.nextMegaphone = now.Add(automationBehaviorDelay(
				profile.SpeechIntervalMinSeconds, profile.SpeechIntervalMaxSeconds,
				runtime.megaphoneCursor, runtime.behaviorSeed, runtime.personality, automationActionMegaphone,
			))
		}
		due = !now.Before(runtime.nextMegaphone)
		cursor = runtime.megaphoneCursor
		if due {
			runtime.megaphoneCursor++
			runtime.nextMegaphone = now.Add(automationBehaviorDelay(
				profile.SpeechIntervalMinSeconds, profile.SpeechIntervalMaxSeconds,
				runtime.megaphoneCursor, runtime.behaviorSeed, runtime.personality, automationActionMegaphone,
			))
		}
	} else {
		runtime.nextMegaphone = time.Time{}
		if runtime.nextChat.IsZero() {
			runtime.nextChat = now.Add(automationBehaviorDelay(
				profile.SpeechIntervalMinSeconds, profile.SpeechIntervalMaxSeconds,
				runtime.chatCursor, runtime.behaviorSeed, runtime.personality, automationActionChat,
			))
		}
		due = !now.Before(runtime.nextChat)
		cursor = runtime.chatCursor
		if due {
			runtime.chatCursor++
			runtime.nextChat = now.Add(automationBehaviorDelay(
				profile.SpeechIntervalMinSeconds, profile.SpeechIntervalMaxSeconds,
				runtime.chatCursor, runtime.behaviorSeed, runtime.personality, automationActionChat,
			))
		}
	}
	runtime.mu.Unlock()
	if !due {
		return
	}
	message := profile.SpeechMessages[automationMessageIndex(cursor, runtime.behaviorSeed, len(profile.SpeechMessages))]
	if mode == dnfrobotdomain.SpeechModeMegaphone {
		preparer, ok := m.endpoint.(MegaphonePreparer)
		if !ok {
			m.recordAutomationError(runtime, errors.New("dnf robot megaphone preparer is unavailable"))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		slot, err := preparer.PrepareRobotServerMegaphone(ctx, profile.AccountID, profile.CharacterID)
		cancel()
		if err == nil {
			err = client.Megaphone(slot, message)
		}
		if err != nil {
			m.recordAutomationError(runtime, fmt.Errorf("robot megaphone speech: %w", err))
			return
		}
		runtime.mu.Lock()
		runtime.lastAction = automationActionMegaphone
		runtime.mu.Unlock()
		return
	}
	if err := client.Chat(3, message); err != nil {
		m.recordAutomationError(runtime, fmt.Errorf("robot normal speech: %w", err))
		return
	}
	runtime.mu.Lock()
	runtime.lastAction = automationActionChat
	runtime.mu.Unlock()
}

// maintainPersonalStoreAutoOpen opens one configured personal store after each
// successful connection. The configured position is applied by
// OpenConfiguredPrivateStore before the op85/op87 store sequence starts.
func (m *Manager) maintainPersonalStoreAutoOpen(now time.Time, runtime *profileRuntime) {
	if m == nil || runtime == nil {
		return
	}
	runtime.mu.Lock()
	client := runtime.client
	profile := runtime.profile
	if client == nil || profile.RobotType != dnfrobotdomain.RobotTypePersonalStore ||
		runtime.personalAutoOpenAttempted {
		runtime.mu.Unlock()
		return
	}
	if !runtime.nextPersonalAutoOpenCheck.IsZero() && now.Before(runtime.nextPersonalAutoOpenCheck) {
		runtime.mu.Unlock()
		return
	}
	snapshot := client.Snapshot()
	if snapshot.StoreActive && !snapshot.ExpertStoreActive {
		runtime.personalAutoOpenAttempted = true
		runtime.mu.Unlock()
		return
	}
	if snapshot.State != StateTownReady || snapshot.StoreActive {
		runtime.mu.Unlock()
		return
	}
	runtime.personalAutoOpenAttempted = true
	runtime.mu.Unlock()

	go func() {
		operation := m.BeginOperation("personal-store-auto-open", profile.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		err := m.OpenConfiguredPrivateStore(ctx, profile.ID)
		cancel()
		m.CompleteOperation(operation.ID, fmt.Sprintf("robot=%s items=%d", profile.ID, len(profile.PrivateStoreItems)), err)
		if err != nil {
			runtime.mu.Lock()
			runtime.personalAutoOpenAttempted = false
			runtime.nextPersonalAutoOpenCheck = time.Now().UTC().Add(personalStoreAutoOpenRetryInterval)
			runtime.mu.Unlock()
			m.recordAutomationError(runtime, fmt.Errorf("personal-store auto-open: %w", err))
			return
		}
		m.logger.Info("robot personal store automatically opened after login",
			"robot_id", profile.ID,
			"character_id", profile.CharacterID,
			"items", len(profile.PrivateStoreItems),
			"village", profile.Village,
			"area", profile.Area)
	}()
}
