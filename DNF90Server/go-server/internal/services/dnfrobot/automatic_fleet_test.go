package dnfrobot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

type automaticFleetTestEndpoint struct {
	group         dnfrepo.Group
	mu            sync.Mutex
	created       int
	gatewayChecks int
}

func (e *automaticFleetTestEndpoint) RobotGameEndpoint(int) (string, error) {
	return "", errors.New("fixture listener is intentionally offline")
}

func (e *automaticFleetTestEndpoint) WaitRobotGameEndpointStable(_ context.Context, channelID int, samples int, interval time.Duration) error {
	if channelID != automaticFleetPreferredChannelID || samples != automaticFleetGatewaySamples || interval != automaticFleetGatewayGap {
		return fmt.Errorf("unexpected gateway probe channel=%d samples=%d interval=%s", channelID, samples, interval)
	}
	e.mu.Lock()
	e.gatewayChecks++
	e.mu.Unlock()
	return nil
}

func (e *automaticFleetTestEndpoint) EnsureAutomaticRobotProfile(ctx context.Context, index int) (dnfrobotdomain.Profile, error) {
	accountID := fmt.Sprintf("auto-account-%03d", index)
	characterID := strconv.Itoa(index)
	if _, found, err := e.group.Character.Load(ctx, characterID); err != nil {
		return dnfrobotdomain.Profile{}, err
	} else if !found {
		if err := e.group.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: characterID, AccountID: accountID, Name: fmt.Sprintf("Auto%03d", index), Level: 90,
		}); err != nil {
			return dnfrobotdomain.Profile{}, err
		}
		e.mu.Lock()
		e.created++
		e.mu.Unlock()
	}
	return dnfrobotdomain.Profile{
		ID: fmt.Sprintf("auto-generated-%03d", index), AccountID: accountID,
		CharacterID: uint16(index), ChannelID: 19, AutoStart: false,
		Village: 38, Area: 1, X: uint16(400 + index), Y: 234,
	}, nil
}

func TestManagerAutomaticallyEnsuresFiveHundredPersistentProfilesWithoutDuplicates(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	endpoint := &automaticFleetTestEndpoint{group: group}
	provider := func() (dnfrepo.Group, bool) { return group, true }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	startAndWait := func() *Manager {
		manager := NewManager(logger, provider, endpoint, "fixture-secret")
		manager.fleetProvisionDelay = 0
		if err := manager.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(manager.List()) != AutomaticFleetTarget {
			if time.Now().After(deadline) {
				t.Fatalf("automatic fleet size = %d, want %d", len(manager.List()), AutomaticFleetTarget)
			}
			time.Sleep(time.Millisecond)
		}
		return manager
	}

	first := startAndWait()
	policy := first.Automation()
	if !policy.Enabled || policy.TargetOnline != AutomaticFleetOnlineTarget || policy.ScaleUpBatch != 2 || policy.StoreConcurrent != 2 ||
		policy.StoreMaximumRows != 8 || policy.MegaphoneProfileModulo != 50 || len(policy.ChatMessages) == 0 {
		t.Fatalf("default automatic policy = %+v", policy)
	}
	for _, snapshot := range first.List() {
		if snapshot.Profile.AutoStart {
			t.Fatalf("automatic snapshot = %+v", snapshot)
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := first.Stop(stopCtx); err != nil {
		cancel()
		t.Fatalf("first Stop: %v", err)
	}
	cancel()

	second := startAndWait()
	stopCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := second.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	endpoint.mu.Lock()
	created := endpoint.created
	gatewayChecks := endpoint.gatewayChecks
	endpoint.mu.Unlock()
	if created != AutomaticFleetTarget {
		t.Fatalf("native characters created = %d, want exactly %d across two starts", created, AutomaticFleetTarget)
	}
	if gatewayChecks != 2 {
		t.Fatalf("gateway stability checks = %d, want one before each manager start", gatewayChecks)
	}
	config, err := dnfrobotdomain.NewOwner(provider).Load(context.Background())
	if err != nil || len(config.Profiles) != AutomaticFleetTarget || config.AutomaticFleetVersion != automaticFleetConfigVersion {
		t.Fatalf("persisted automatic config = %d profiles, err=%v", len(config.Profiles), err)
	}
}

func TestAutomationPersonalitiesStaggerActionsDeterministically(t *testing.T) {
	profileA := dnfrobotdomain.Profile{ID: "auto-a", AccountID: "a", CharacterID: 1}
	profileB := dnfrobotdomain.Profile{ID: "auto-b", AccountID: "b", CharacterID: 2}
	seedA, seedB := automationBehaviorSeed(profileA), automationBehaviorSeed(profileB)
	if seedA == seedB || automationPersonality(seedA) == "" || automationPersonality(seedB) == "" {
		t.Fatalf("behavior seeds/personalities are not stable: %d/%s %d/%s", seedA, automationPersonality(seedA), seedB, automationPersonality(seedB))
	}
	delayA := automationBehaviorDelay(30, 120, 0, seedA, automationPersonality(seedA), automationActionMove)
	delayB := automationBehaviorDelay(30, 120, 0, seedB, automationPersonality(seedB), automationActionMove)
	if delayA <= 0 || delayB <= 0 || delayA == delayB {
		t.Fatalf("staggered delays = %v / %v", delayA, delayB)
	}
	if repeat := automationBehaviorDelay(30, 120, 0, seedA, automationPersonality(seedA), automationActionMove); repeat != delayA {
		t.Fatalf("behavior delay changed across identical decisions: %v / %v", delayA, repeat)
	}
}

func TestAutomaticFleetProvisioningOnlyArmsTheScaleLoopOnce(t *testing.T) {
	wantNext := time.Unix(1_800_000_000, 0).UTC()
	manager := &Manager{automaticFleetReady: true, nextScale: wantNext, fleetProvisionDelay: 250 * time.Millisecond}
	if delay := manager.markAutomaticFleetProfileEnsured(); delay != 250*time.Millisecond || !manager.nextScale.Equal(wantNext) {
		t.Fatalf("already-ready fleet delay=%s next=%s", delay, manager.nextScale)
	}
	manager.automaticFleetReady = false
	if delay := manager.markAutomaticFleetProfileEnsured(); delay != 250*time.Millisecond || !manager.automaticFleetReady || !manager.nextScale.IsZero() {
		t.Fatalf("first-ready fleet delay=%s ready=%t next=%s", delay, manager.automaticFleetReady, manager.nextScale)
	}
}

func TestRobotNativeLoginSlotsBoundConcurrentHandshakes(t *testing.T) {
	manager := &Manager{connectSlots: make(chan struct{}, 2)}
	releaseFirst, err := manager.acquireConnectSlot(context.Background())
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	releaseSecond, err := manager.acquireConnectSlot(context.Background())
	if err != nil {
		releaseFirst()
		t.Fatalf("second slot: %v", err)
	}

	blockedCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := manager.acquireConnectSlot(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		releaseSecond()
		releaseFirst()
		t.Fatalf("third slot err=%v, want deadline", err)
	}
	releaseFirst()
	releaseThird, err := manager.acquireConnectSlot(context.Background())
	if err != nil {
		releaseSecond()
		t.Fatalf("released slot was not reusable: %v", err)
	}
	releaseThird()
	releaseSecond()
	if got := len(manager.connectSlots); got != 0 {
		t.Fatalf("connect slots leaked: %d", got)
	}
}

func TestAutomaticFleetClampsCurrentVersionPersistedTarget(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	provider := func() (dnfrepo.Group, bool) { return group, true }
	owner := dnfrobotdomain.NewOwner(provider)
	if _, err := owner.Save(context.Background(), dnfrobotdomain.Config{
		AutomaticFleetVersion: automaticFleetConfigVersion,
		Automation: dnfrobotdomain.AutomationPolicy{
			Enabled: true, TargetOnline: AutomaticFleetTarget,
		},
	}); err != nil {
		t.Fatalf("save oversized current-version policy: %v", err)
	}

	endpoint := &automaticFleetTestEndpoint{group: group}
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), provider, endpoint, "fixture-secret")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Start(canceled); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := manager.Automation().TargetOnline; got != AutomaticFleetOnlineTarget {
		t.Fatalf("live automatic target = %d, want %d", got, AutomaticFleetOnlineTarget)
	}
	persisted, err := owner.Load(context.Background())
	if err != nil || persisted.Automation.TargetOnline != AutomaticFleetOnlineTarget {
		t.Fatalf("persisted automatic target = %d, err=%v", persisted.Automation.TargetOnline, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestAutomaticFleetSchedulerHardCapsLegacyInMemoryTarget(t *testing.T) {
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		&automaticFleetTestEndpoint{}, "fixture-secret",
	)
	manager.config.Automation = dnfrobotdomain.AutomationPolicy{
		Enabled: true, TargetOnline: AutomaticFleetTarget,
		ScaleIntervalSeconds: 1, ScaleUpBatch: 2, ScaleDownBatch: 2,
	}
	for index := 1; index <= AutomaticFleetOnlineTarget+1; index++ {
		profile := dnfrobotdomain.Profile{
			ID:          fmt.Sprintf("auto-generated-%03d", index),
			AccountID:   fmt.Sprintf("account-%03d", index),
			CharacterID: uint16(index), ChannelID: 19,
		}
		manager.config.Profiles = append(manager.config.Profiles, profile)
		done := make(chan struct{})
		close(done)
		manager.running[profileKey(profile.ID)] = &profileRuntime{
			profile: profile, done: done, schedulerManaged: true,
		}
	}

	manager.reconcileTargetOnline(time.Unix(1_000, 0).UTC())
	status := manager.Status()
	if status.Running != AutomaticFleetOnlineTarget || status.SchedulerManaged != AutomaticFleetOnlineTarget {
		t.Fatalf("hard-capped automatic status = %+v", status)
	}
}

func TestAutomaticFleetSchedulerStopsScalingAtPendingActorLimit(t *testing.T) {
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		&automaticFleetTestEndpoint{}, "fixture-secret",
	)
	manager.config.Automation = dnfrobotdomain.AutomationPolicy{
		Enabled: true, TargetOnline: AutomaticFleetOnlineTarget,
		ScaleIntervalSeconds: 1, ScaleUpBatch: 2, ScaleDownBatch: 2,
	}
	for index := 1; index <= automaticFleetPendingLimit+1; index++ {
		profile := dnfrobotdomain.Profile{
			ID:          fmt.Sprintf("auto-generated-%03d", index),
			AccountID:   fmt.Sprintf("account-%03d", index),
			CharacterID: uint16(index), ChannelID: 19,
		}
		manager.config.Profiles = append(manager.config.Profiles, profile)
		if index <= automaticFleetPendingLimit {
			manager.running[profileKey(profile.ID)] = &profileRuntime{
				profile: profile, done: make(chan struct{}), schedulerManaged: true,
			}
		}
	}

	manager.reconcileTargetOnline(time.Unix(2_000, 0).UTC())
	status := manager.Status()
	if status.Running != automaticFleetPendingLimit || status.SchedulerManaged != automaticFleetPendingLimit {
		t.Fatalf("pending-capped automatic status = %+v", status)
	}
	if got := automaticFleetPendingRuntimes(manager.running); got != automaticFleetPendingLimit {
		t.Fatalf("pending automatic runtimes = %d, want %d", got, automaticFleetPendingLimit)
	}
}
