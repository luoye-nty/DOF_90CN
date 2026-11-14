package dnfrobot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

var (
	ErrManagerStopped  = errors.New("dnf robot manager is not running")
	ErrProfileNotFound = errors.New("dnf robot profile was not found")
	ErrProfileRunning  = errors.New("dnf robot profile is already running")
)

type EndpointProvider interface {
	RobotGameEndpoint(channelID int) (string, error)
}

type ExpertMachineState struct {
	Kind         string
	Grade        int
	Endurance    int64
	MaxEndurance int64
}

// ExpertMachineMaintenance is implemented by the production bridge. The
// manager uses it to inspect server-owned machine endurance and restore it
// while the robot is offline.
type ExpertMachineMaintenance interface {
	RobotExpertMachineState(ctx context.Context, accountID string, characterID uint16) (ExpertMachineState, error)
	RestoreRobotExpertMachine(ctx context.Context, accountID string, characterID uint16) (ExpertMachineState, error)
}

type PrivateStorePlanner interface {
	RobotPrivateStoreRows(ctx context.Context, characterID uint16, maximumRows int, unitPrice uint32) ([]PrivateStoreRow, error)
}

type ConfiguredPrivateStorePlanner interface {
	PrepareRobotPrivateStoreRows(ctx context.Context, accountID string, characterID uint16, items []dnfrobotdomain.PrivateStoreItem) ([]PrivateStoreRow, error)
}

// PrivateStoreStocker may replenish a robot through the server's authoritative
// PVF/inventory transaction before the normal private-store planner runs. It
// never lets the protocol client fabricate inventory rows.
type PrivateStoreStocker interface {
	EnsureRobotPrivateStoreStock(ctx context.Context, accountID string, characterID uint16, maximumRows int) error
}

// MegaphonePreparer returns the exact main-inventory slot of a real item-36
// stack owned by the robot. The protocol client then sends the same mode-11
// request as a player and the normal chat owner consumes one item.
type MegaphonePreparer interface {
	PrepareRobotServerMegaphone(ctx context.Context, accountID string, characterID uint16) (int16, error)
}

// AutomaticFleetGatewayStability is implemented by the production bridge.
// It prevents the zero-touch scheduler from opening hundreds of sessions
// while the selected game listener is still starting or flapping.
type AutomaticFleetGatewayStability interface {
	WaitRobotGameEndpointStable(ctx context.Context, channelID int, samples int, interval time.Duration) error
}

type RuntimeSnapshot struct {
	Profile           dnfrobotdomain.Profile `json:"profile"`
	Running           bool                   `json:"running"`
	State             State                  `json:"state"`
	Endpoint          string                 `json:"endpoint,omitempty"`
	StartedAt         time.Time              `json:"started_at,omitempty"`
	ReadyAt           time.Time              `json:"ready_at,omitempty"`
	StateSince        time.Time              `json:"state_since,omitempty"`
	LastReceive       time.Time              `json:"last_receive_at,omitempty"`
	LastSend          time.Time              `json:"last_send_at,omitempty"`
	Reconnects        int                    `json:"reconnects"`
	LastError         string                 `json:"last_error,omitempty"`
	CharacterID       uint16                 `json:"selected_character_id,omitempty"`
	Slot              uint8                  `json:"selected_slot,omitempty"`
	Village           uint8                  `json:"village"`
	Area              uint8                  `json:"area"`
	X                 uint16                 `json:"x"`
	Y                 uint16                 `json:"y"`
	SchedulerManaged  bool                   `json:"scheduler_managed,omitempty"`
	Personality       string                 `json:"personality,omitempty"`
	LastAction        string                 `json:"last_action,omitempty"`
	StoreKind         string                 `json:"store_kind,omitempty"`
	StoreActive       bool                   `json:"store_active"`
	ExpertStoreActive bool                   `json:"expert_store_active"`
	RecycleActive     bool                   `json:"recycle_active,omitempty"`
}

// PlacementBounds is the authored movable rectangle that contains an
// operator-requested robot position. An unset value intentionally removes any
// previously stored bounds.
type PlacementBounds struct {
	Set  bool
	MinX uint16
	MaxX uint16
	MinY uint16
	MaxY uint16
}

type profileRuntime struct {
	mu                        sync.RWMutex
	profile                   dnfrobotdomain.Profile
	client                    *Client
	cancel                    context.CancelFunc
	done                      chan struct{}
	startedAt                 time.Time
	reconnects                int
	lastError                 string
	nextMove                  time.Time
	nextChat                  time.Time
	nextMegaphone             time.Time
	nextStore                 time.Time
	storeClose                time.Time
	moveCursor                int
	chatCursor                int
	megaphoneCursor           int
	storeCursor               int
	autoStore                 bool
	storeRetryAt              time.Time
	nextExpertCheck           time.Time
	expertStoreCost           uint32
	expertRecovering          bool
	expertAutoOpenAttempted   bool
	nextExpertAutoOpenCheck   time.Time
	personalAutoOpenAttempted bool
	nextPersonalAutoOpenCheck time.Time
	privateStoreTransition    bool
	recycleActive             bool
	schedulerManaged          bool
	behaviorSeed              uint64
	personality               string
	lastAction                string
}

type Manager struct {
	logger   *slog.Logger
	provider func() (dnfrepo.Group, bool)
	endpoint EndpointProvider
	secret   []byte
	owner    *dnfrobotdomain.Owner

	opMu                sync.Mutex
	historyMu           sync.Mutex
	nextOperationID     int64
	operations          []OperationStatus
	nextScale           time.Time
	automaticFleetReady bool
	fleetProvisionDelay time.Duration
	connectSlots        chan struct{}
	mu                  sync.RWMutex
	ctx                 context.Context
	cancel              context.CancelFunc
	config              dnfrobotdomain.Config
	running             map[string]*profileRuntime
	wg                  sync.WaitGroup
}

func NewManager(logger *slog.Logger, provider func() (dnfrepo.Group, bool), endpoint EndpointProvider, secret string) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:              logger,
		provider:            provider,
		endpoint:            endpoint,
		secret:              []byte(strings.TrimSpace(secret)),
		owner:               dnfrobotdomain.NewOwner(provider),
		running:             make(map[string]*profileRuntime),
		automaticFleetReady: true,
		fleetProvisionDelay: 2 * time.Second,
		connectSlots:        make(chan struct{}, 2),
	}
}

func (m *Manager) Name() string { return "dnf-robot-manager" }

func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || m.endpoint == nil || len(m.secret) == 0 {
		return errors.New("dnf robot manager dependencies are unavailable")
	}
	config, err := m.owner.Load(ctx)
	if err != nil {
		return fmt.Errorf("load dnf robot configuration: %w", err)
	}
	_, automaticFleet := m.endpoint.(AutomaticFleetProvider)
	if automaticFleet {
		configChanged := false
		// Robot identities are now created explicitly from the GM console. Keep
		// the legacy online cap for existing installations, but never provision or
		// auto-start the old 500-identity fleet during manager startup.
		if config.Automation.TargetOnline > AutomaticFleetOnlineTarget {
			config.Automation.TargetOnline = AutomaticFleetOnlineTarget
			configChanged = true
		}
		for index := range config.Profiles {
			if strings.HasPrefix(strings.ToLower(config.Profiles[index].ID), automaticFleetProfilePrefix) && config.Profiles[index].AutoStart {
				config.Profiles[index].AutoStart = false
				configChanged = true
			}
		}
		if configChanged {
			config, err = m.owner.Save(ctx, config)
			if err != nil {
				return fmt.Errorf("save default dnf robot automation: %w", err)
			}
		}
	}
	config, err = m.owner.Save(ctx, config)
	if err != nil {
		return fmt.Errorf("persist normalized dnf robot configuration: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		cancel()
		return ErrProfileRunning
	}
	m.ctx, m.cancel, m.config = runCtx, cancel, dnfrobotdomain.CloneConfig(config)
	// Automatic identity provisioning is no longer a background startup task.
	// Treat scheduling as ready so existing/manual profiles still participate in
	// target-online reconciliation.
	m.automaticFleetReady = true
	m.mu.Unlock()
	m.wg.Add(1)
	go m.runAutomation(runCtx)
	for _, profile := range config.Profiles {
		if profile.AutoStart {
			if _, err := m.StartProfile(profile.ID); err != nil {
				m.logger.Warn("dnf robot autostart deferred", "profile", profile.ID, "error", err)
			}
		}
	}
	return nil
}

func (m *Manager) Automation() dnfrobotdomain.AutomationPolicy {
	if m == nil {
		return dnfrobotdomain.AutomationPolicy{}
	}
	m.mu.RLock()
	policy := m.config.Automation
	m.mu.RUnlock()
	policy.ChatMessages = append([]string(nil), policy.ChatMessages...)
	return policy
}

func (m *Manager) UpdateAutomation(ctx context.Context, policy dnfrobotdomain.AutomationPolicy) (dnfrobotdomain.AutomationPolicy, error) {
	if m == nil {
		return dnfrobotdomain.AutomationPolicy{}, ErrManagerStopped
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	m.mu.RUnlock()
	if _, automaticFleet := m.endpoint.(AutomaticFleetProvider); automaticFleet && policy.TargetOnline > AutomaticFleetOnlineTarget {
		policy.TargetOnline = AutomaticFleetOnlineTarget
	}
	config.Automation = policy
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return dnfrobotdomain.AutomationPolicy{}, err
	}
	m.mu.Lock()
	m.config = saved
	runtimes := make([]*profileRuntime, 0, len(m.running))
	for _, runtime := range m.running {
		runtimes = append(runtimes, runtime)
	}
	m.mu.Unlock()
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		if runtime.profile.ExpertStoreKind == "" {
			runtime.nextMove = time.Time{}
			runtime.nextChat = time.Time{}
		}
		runtime.mu.Unlock()
	}
	return m.Automation(), nil
}

type SpeechMode struct {
	ID        string `json:"id"`
	Megaphone bool   `json:"megaphone"`
}

// UpdateSpeechModes persists the per-robot choice between ordinary local chat
// and server-megaphone announcements. Expert-store profiles keep the setting,
// but their stall maintenance still takes precedence over ambient speech.
func (m *Manager) UpdateSpeechModes(ctx context.Context, modes []SpeechMode) ([]RuntimeSnapshot, error) {
	if m == nil {
		return nil, ErrManagerStopped
	}
	if len(modes) == 0 || len(modes) > dnfrobotdomain.MaxProfiles {
		return nil, fmt.Errorf("%w: invalid robot speech mode count", dnfrobotdomain.ErrInvalid)
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	m.mu.RUnlock()
	foundModes := make(map[string]SpeechMode, len(modes))
	for _, mode := range modes {
		key := profileKey(mode.ID)
		if key == "" {
			return nil, fmt.Errorf("%w: robot speech mode id is empty", dnfrobotdomain.ErrInvalid)
		}
		if _, duplicate := foundModes[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate robot speech mode %q", dnfrobotdomain.ErrInvalid, mode.ID)
		}
		foundModes[key] = SpeechMode{ID: mode.ID, Megaphone: mode.Megaphone}
	}
	found := make(map[string]bool, len(foundModes))
	for index := range config.Profiles {
		key := profileKey(config.Profiles[index].ID)
		mode, ok := foundModes[key]
		if !ok {
			continue
		}
		config.Profiles[index].MegaphoneEnabled = mode.Megaphone
		if mode.Megaphone {
			config.Profiles[index].SpeechMode = dnfrobotdomain.SpeechModeMegaphone
		} else {
			config.Profiles[index].SpeechMode = dnfrobotdomain.SpeechModeNormal
		}
		found[key] = true
	}
	for key := range foundModes {
		if !found[key] {
			return nil, fmt.Errorf("%w: robot profile %q was not found", ErrProfileNotFound, foundModes[key].ID)
		}
	}
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.config = saved
	runtimes := make(map[string]*profileRuntime, len(foundModes))
	for key := range foundModes {
		if runtime := m.running[key]; runtime != nil {
			runtimes[key] = runtime
		}
	}
	m.mu.Unlock()
	for key, runtime := range runtimes {
		runtime.mu.Lock()
		runtime.profile, _ = findProfile(saved, key)
		runtime.nextChat = time.Time{}
		runtime.nextMegaphone = time.Time{}
		runtime.mu.Unlock()
	}
	return m.List(), nil
}

func (m *Manager) RecyclePolicy() dnfrobotdomain.RecyclePolicy {
	if m == nil {
		return dnfrobotdomain.RecyclePolicy{}
	}
	m.mu.RLock()
	policy := m.config.Recycle
	m.mu.RUnlock()
	policy.BuyItems = append([]dnfrobotdomain.RecycleBuyItem(nil), policy.BuyItems...)
	return policy
}

func (m *Manager) UpdateRecyclePolicy(ctx context.Context, policy dnfrobotdomain.RecyclePolicy) (dnfrobotdomain.RecyclePolicy, error) {
	if m == nil {
		return dnfrobotdomain.RecyclePolicy{}, ErrManagerStopped
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	m.mu.RUnlock()
	config.Recycle = policy
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return dnfrobotdomain.RecyclePolicy{}, err
	}
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	return m.RecyclePolicy(), nil
}

func (m *Manager) UpdateProfileRecyclePolicy(
	ctx context.Context,
	id string,
	policy dnfrobotdomain.RecyclePolicy,
) (RuntimeSnapshot, error) {
	if m == nil {
		return RuntimeSnapshot{}, ErrManagerStopped
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	found := false
	for index := range config.Profiles {
		if profileKey(config.Profiles[index].ID) != profileKey(id) {
			continue
		}
		if config.Profiles[index].RobotType != dnfrobotdomain.RobotTypeNormal {
			return RuntimeSnapshot{}, fmt.Errorf(
				"%w: only normal robots can configure recycle trade",
				dnfrobotdomain.ErrInvalid,
			)
		}
		config.Profiles[index].Recycle = policy
		found = true
		break
	}
	if !found {
		return RuntimeSnapshot{}, ErrProfileNotFound
	}
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	profile, _ := findProfile(saved, id)
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	if runtime != nil {
		runtime.mu.Lock()
		runtime.profile.Recycle = cloneRecyclePolicy(profile.Recycle)
		runtime.mu.Unlock()
	}
	return snapshotRuntime(profile, runtime), nil
}

func cloneRecyclePolicy(policy dnfrobotdomain.RecyclePolicy) dnfrobotdomain.RecyclePolicy {
	policy.BuyItems = append([]dnfrobotdomain.RecycleBuyItem(nil), policy.BuyItems...)
	return policy
}

// BeginRecycle reserves an ambient, town-ready robot for one player trade.
// Expert-store and private-store robots are deliberately excluded because a
// second native interaction would compete with their store state machine.
func (m *Manager) BeginRecycle(characterID uint16) (dnfrobotdomain.Profile, bool) {
	if m == nil || characterID == 0 {
		return dnfrobotdomain.Profile{}, false
	}
	m.mu.RLock()
	runtimes := make([]*profileRuntime, 0, len(m.running))
	for _, runtime := range m.running {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		client := runtime.client
		profile := runtime.profile
		eligible := client != nil && !runtime.recycleActive &&
			profile.RobotType == dnfrobotdomain.RobotTypeNormal && profile.Recycle.Enabled
		var state State
		var storeActive, expertStoreActive bool
		if eligible {
			snapshot := client.Snapshot()
			state = snapshot.State
			storeActive = snapshot.StoreActive
			expertStoreActive = snapshot.ExpertStoreActive
		}
		eligible = eligible && profile.CharacterID == characterID && state == StateTownReady &&
			!storeActive && !expertStoreActive
		if eligible {
			runtime.recycleActive = true
			runtime.nextMove = time.Time{}
			runtime.nextChat = time.Time{}
			runtime.nextMegaphone = time.Time{}
			runtime.lastAction = "recycle_trade"
		}
		runtime.mu.Unlock()
		if eligible {
			return profile, true
		}
	}
	return dnfrobotdomain.Profile{}, false
}

func (m *Manager) EndRecycle(id string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	if runtime.recycleActive {
		runtime.recycleActive = false
		runtime.nextMove = time.Time{}
		runtime.nextChat = time.Time{}
	}
	runtime.mu.Unlock()
}

func (m *Manager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	cancel := m.cancel
	m.cancel, m.ctx = nil, nil
	runtimes := make([]*profileRuntime, 0, len(m.running))
	for _, runtime := range m.running {
		runtimes = append(runtimes, runtime)
	}
	m.running = make(map[string]*profileRuntime)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, runtime := range runtimes {
		m.stopRuntime(runtime)
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (m *Manager) List() []RuntimeSnapshot {
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	running := make(map[string]*profileRuntime, len(m.running))
	for key, runtime := range m.running {
		running[key] = runtime
	}
	m.mu.RUnlock()
	result := make([]RuntimeSnapshot, 0, len(config.Profiles))
	for _, profile := range config.Profiles {
		runtime := running[profileKey(profile.ID)]
		result = append(result, snapshotRuntime(profile, runtime))
	}
	return result
}

func (m *Manager) Snapshot(id string) (RuntimeSnapshot, error) {
	if m == nil {
		return RuntimeSnapshot{}, ErrManagerStopped
	}
	id = strings.TrimSpace(id)
	for _, snapshot := range m.List() {
		if profileKey(snapshot.Profile.ID) == profileKey(id) {
			return snapshot, nil
		}
	}
	return RuntimeSnapshot{}, ErrProfileNotFound
}

func (m *Manager) Upsert(ctx context.Context, profile dnfrobotdomain.Profile) (RuntimeSnapshot, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	normalized, err := dnfrobotdomain.Normalize(dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{profile}})
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	profile = normalized.Profiles[0]
	if err := m.validateCharacter(ctx, profile); err != nil {
		return RuntimeSnapshot{}, err
	}
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	_, managerRunning := m.running[profileKey(profile.ID)]
	m.mu.RUnlock()
	found := false
	for index := range config.Profiles {
		if profileKey(config.Profiles[index].ID) == profileKey(profile.ID) {
			if config.Profiles[index].RobotType != profile.RobotType {
				return RuntimeSnapshot{}, fmt.Errorf(
					"%w: robot type is immutable after creation",
					dnfrobotdomain.ErrInvalid,
				)
			}
			config.Profiles[index], found = profile, true
			break
		}
	}
	if !found {
		config.Profiles = append(config.Profiles, profile)
	}
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	if managerRunning {
		_ = m.stopProfileUnlocked(profile.ID)
	}
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	if managerRunning || profile.AutoStart {
		return m.StartProfile(profile.ID)
	}
	return snapshotRuntime(profile, nil), nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrProfileNotFound
	}
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	m.mu.RUnlock()
	filtered := config.Profiles[:0]
	found := false
	for _, profile := range config.Profiles {
		if profileKey(profile.ID) == profileKey(id) {
			found = true
			continue
		}
		filtered = append(filtered, profile)
	}
	if !found {
		return ErrProfileNotFound
	}
	config.Profiles = filtered
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return err
	}
	_ = m.stopProfileUnlocked(id)
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	return nil
}

func (m *Manager) StartProfile(id string) (RuntimeSnapshot, error) {
	return m.startProfile(id, false)
}

func (m *Manager) startProfile(id string, schedulerManaged bool) (RuntimeSnapshot, error) {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	if m.ctx == nil {
		m.mu.Unlock()
		return RuntimeSnapshot{}, ErrManagerStopped
	}
	profile, ok := findProfile(m.config, id)
	if !ok {
		m.mu.Unlock()
		return RuntimeSnapshot{}, ErrProfileNotFound
	}
	key := profileKey(profile.ID)
	if runtime := m.running[key]; runtime != nil {
		if !schedulerManaged {
			runtime.mu.Lock()
			runtime.schedulerManaged = false
			runtime.mu.Unlock()
		}
		m.mu.Unlock()
		return snapshotRuntime(profile, runtime), nil
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	runtime := &profileRuntime{
		profile: profile, cancel: cancel, done: make(chan struct{}), startedAt: time.Now().UTC(),
		schedulerManaged: schedulerManaged,
	}
	runtime.behaviorSeed = automationBehaviorSeed(profile)
	runtime.personality = automationPersonality(runtime.behaviorSeed)
	m.running[key] = runtime
	m.wg.Add(1)
	m.mu.Unlock()
	go m.runProfile(runCtx, runtime)
	return snapshotRuntime(profile, runtime), nil
}

func (m *Manager) StopProfile(id string) (RuntimeSnapshot, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	profile, ok := findProfile(m.config, id)
	m.mu.RUnlock()
	if !ok {
		return RuntimeSnapshot{}, ErrProfileNotFound
	}
	if err := m.stopProfileUnlocked(id); err != nil {
		return RuntimeSnapshot{}, err
	}
	return snapshotRuntime(profile, nil), nil
}

func (m *Manager) Move(id string, village, area uint8, x, y uint16, moveType uint8) error {
	client, err := m.activeClient(id)
	if err != nil {
		return err
	}
	return client.Move(village, area, x, y, moveType)
}

// MoveAndPersist performs one operator-requested move and updates the saved
// profile so a later stop/start keeps the new town, area and position.
func (m *Manager) MoveAndPersist(ctx context.Context, id string, village, area uint8, x, y uint16, moveType uint8, bounds PlacementBounds) error {
	client, err := m.activeClient(id)
	if err != nil {
		return err
	}
	if err := client.Move(village, area, x, y, moveType); err != nil {
		return err
	}
	key := profileKey(id)
	m.mu.Lock()
	config := dnfrobotdomain.CloneConfig(m.config)
	found := false
	for index := range config.Profiles {
		if profileKey(config.Profiles[index].ID) != key {
			continue
		}
		config.Profiles[index].Village = village
		config.Profiles[index].Area = area
		config.Profiles[index].X = x
		config.Profiles[index].Y = y
		config.Profiles[index].MoveType = moveType
		config.Profiles[index].MoveBoundsSet = bounds.Set
		if bounds.Set {
			config.Profiles[index].MoveMinX = bounds.MinX
			config.Profiles[index].MoveMaxX = bounds.MaxX
			config.Profiles[index].MoveMinY = bounds.MinY
			config.Profiles[index].MoveMaxY = bounds.MaxY
		} else {
			config.Profiles[index].MoveMinX = 0
			config.Profiles[index].MoveMaxX = 0
			config.Profiles[index].MoveMinY = 0
			config.Profiles[index].MoveMaxY = 0
		}
		found = true
		break
	}
	m.mu.Unlock()
	if !found {
		return ErrProfileNotFound
	}
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	return nil
}

// Relocate persists an operator-selected channel, town, area and position.
// A channel change requires a new game connection, so a running profile is
// stopped and started again after the configuration has been committed.
func (m *Manager) Relocate(ctx context.Context, id string, channelID int, village, area uint8, x, y uint16, moveType uint8, bounds PlacementBounds) (RuntimeSnapshot, error) {
	return m.RelocateWithAutoStart(ctx, id, channelID, village, area, x, y, moveType, bounds, nil)
}

// RelocateWithAutoStart updates location and, when autoStart is non-nil, the
// persisted restart policy. A nil pointer preserves the existing profile flag
// for callers that only move a robot.
func (m *Manager) RelocateWithAutoStart(
	ctx context.Context,
	id string,
	channelID int,
	village, area uint8,
	x, y uint16,
	moveType uint8,
	bounds PlacementBounds,
	autoStart *bool,
) (RuntimeSnapshot, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	key := profileKey(strings.TrimSpace(id))
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	_, managerRunning := m.running[key]
	m.mu.RUnlock()

	found := false
	for index := range config.Profiles {
		if profileKey(config.Profiles[index].ID) != key {
			continue
		}
		profile := &config.Profiles[index]
		profile.ChannelID = channelID
		profile.Village = village
		profile.Area = area
		profile.X = x
		profile.Y = y
		profile.MoveType = moveType
		if autoStart != nil {
			profile.AutoStart = *autoStart
		}
		profile.MoveBoundsSet = bounds.Set
		if bounds.Set {
			profile.MoveMinX = bounds.MinX
			profile.MoveMaxX = bounds.MaxX
			profile.MoveMinY = bounds.MinY
			profile.MoveMaxY = bounds.MaxY
		} else {
			profile.MoveMinX = 0
			profile.MoveMaxX = 0
			profile.MoveMinY = 0
			profile.MoveMaxY = 0
		}
		found = true
		break
	}
	if !found {
		return RuntimeSnapshot{}, ErrProfileNotFound
	}
	normalized, err := dnfrobotdomain.Normalize(config)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	saved, err := m.owner.Save(ctx, normalized)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	m.mu.Lock()
	m.config = saved
	profile, _ := findProfile(saved, id)
	m.mu.Unlock()
	if managerRunning {
		if err := m.stopProfileUnlocked(id); err != nil {
			return RuntimeSnapshot{}, err
		}
		return m.StartProfile(id)
	}
	return snapshotRuntime(profile, nil), nil
}

func (m *Manager) Chat(id string, mode uint8, message string) error {
	client, err := m.activeClient(id)
	if err != nil {
		return err
	}
	return client.Chat(mode, strings.TrimSpace(message))
}

func (m *Manager) OpenPrivateStore(ctx context.Context, id, title string, maximumRows int, unitPrice uint32) error {
	client, profile, err := m.profileClient(id, StateTownReady)
	if err != nil {
		return err
	}
	return m.openPrivateStoreForClient(ctx, client, profile, title, maximumRows, unitPrice)
}

func (m *Manager) openPrivateStoreForClient(ctx context.Context, client *Client, profile dnfrobotdomain.Profile, title string, maximumRows int, unitPrice uint32) error {
	if client == nil {
		return ErrNotReady
	}
	planner, ok := m.endpoint.(PrivateStorePlanner)
	if !ok {
		return errors.New("dnf robot private store planner is unavailable")
	}
	if maximumRows <= 0 {
		maximumRows = privateStoreMaximumRows
	}
	if maximumRows > privateStoreMaximumRows || unitPrice == 0 {
		return errors.New("invalid dnf robot private store request")
	}
	if stocker, ok := m.endpoint.(PrivateStoreStocker); ok {
		if err := stocker.EnsureRobotPrivateStoreStock(ctx, profile.AccountID, profile.CharacterID, maximumRows); err != nil {
			return err
		}
	}
	rows, err := planner.RobotPrivateStoreRows(ctx, profile.CharacterID, maximumRows, unitPrice)
	if err != nil {
		return err
	}
	return client.OpenPrivateStore(strings.TrimSpace(title), rows)
}

func (m *Manager) ClosePrivateStore(id string) error {
	client, _, err := m.profileClient(id, StateStoreReady)
	if err != nil {
		return err
	}
	return client.ClosePrivateStore()
}

// OpenExpertJobStore opens the configured decomposer or enchanter stall for a
// town-ready robot. The profession is prepared during manual provisioning and
// the current server remains authoritative for endurance and eligibility.
func (m *Manager) OpenExpertJobStore(ctx context.Context, id string, requestedCost *uint32) error {
	client, profile, err := m.profileClient(id, StateTownReady)
	if err != nil {
		return err
	}
	if profile.ExpertStoreKind == "" {
		return errors.New("robot has no configured expert store kind")
	}
	cost := uint32(0)
	if requestedCost != nil {
		cost = *requestedCost
	} else if profile.ExpertStoreCost != nil {
		cost = *profile.ExpertStoreCost
	} else {
		cost = defaultExpertStoreCost
	}
	if err := client.OpenExpertJobStore(ctx, cost); err != nil {
		return err
	}
	m.mu.RLock()
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	if runtime != nil {
		runtime.mu.Lock()
		runtime.expertStoreCost = cost
		runtime.mu.Unlock()
	}
	return nil
}

func (m *Manager) SetExpertStoreCost(ctx context.Context, id string, cost uint32) (RuntimeSnapshot, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	id = strings.TrimSpace(id)
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	found := false
	for index := range config.Profiles {
		if profileKey(config.Profiles[index].ID) != profileKey(id) {
			continue
		}
		costCopy := cost
		config.Profiles[index].ExpertStoreCost = &costCopy
		found = true
		break
	}
	if !found {
		return RuntimeSnapshot{}, ErrProfileNotFound
	}
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	profile, _ := findProfile(saved, id)
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	if runtime != nil {
		runtime.mu.Lock()
		costCopy := cost
		runtime.profile.ExpertStoreCost = &costCopy
		runtime.expertStoreCost = cost
		runtime.mu.Unlock()
	}
	return snapshotRuntime(profile, runtime), nil
}

// CloseExpertJobStore closes the active expert stall.
func (m *Manager) CloseExpertJobStore(id string) error {
	client, _, err := m.profileClient(id, StateStoreReady)
	if err != nil {
		return err
	}
	if !client.Snapshot().ExpertStoreActive {
		return errors.New("robot does not have an active expert store")
	}
	return client.CloseExpertJobStore(context.Background())
}

func (m *Manager) runProfile(ctx context.Context, runtime *profileRuntime) {
	defer m.wg.Done()
	defer close(runtime.done)
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		runtime.mu.RLock()
		profile := runtime.profile
		runtime.mu.RUnlock()
		endpoint, err := m.endpoint.RobotGameEndpoint(profile.ChannelID)
		if err == nil {
			client := NewClient(profile, endpoint, m.secret)
			runtime.mu.Lock()
			runtime.client = client
			runtime.expertAutoOpenAttempted = false
			runtime.nextExpertAutoOpenCheck = time.Time{}
			runtime.personalAutoOpenAttempted = false
			runtime.nextPersonalAutoOpenCheck = time.Time{}
			runtime.mu.Unlock()
			err = m.runClientWithConnectLimit(ctx, client)
			runtime.mu.Lock()
			runtime.client = nil
			if err != nil && !errors.Is(err, context.Canceled) {
				runtime.lastError = err.Error()
				runtime.reconnects++
			}
			runtime.mu.Unlock()
		} else {
			runtime.mu.Lock()
			runtime.lastError = err.Error()
			runtime.reconnects++
			runtime.mu.Unlock()
		}
		if ctx.Err() != nil {
			return
		}
		// Per-profile jitter prevents a listener flap from waking every actor on
		// the same second.  The global connect slot below remains the authority
		// for the number of concurrent native login/selection handshakes.
		jitter := time.Duration(automationMix(runtime.behaviorSeed, uint64(runtime.reconnects), 0x7265636f6e6e6563)%2000) * time.Millisecond
		wait := delay + jitter
		m.logger.Warn("dnf robot reconnect scheduled", "profile", profile.ID, "delay", wait, "error", err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (m *Manager) runClientWithConnectLimit(ctx context.Context, client *Client) error {
	if client == nil {
		return ErrNotReady
	}
	release, err := m.acquireConnectSlot(ctx)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()
	select {
	case err := <-done:
		release()
		return err
	case <-client.ready:
		// A town-ready client no longer consumes repository-heavy login capacity.
		release()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			_ = client.Close()
			return ctx.Err()
		}
	case <-ctx.Done():
		release()
		_ = client.Close()
		return ctx.Err()
	}
}

func (m *Manager) acquireConnectSlot(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || m.connectSlots == nil {
		return func() {}, nil
	}
	select {
	case m.connectSlots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-m.connectSlots })
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) runAutomation(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.automationTick(now.UTC())
		}
	}
}

func (m *Manager) automationTick(now time.Time) {
	m.reconcileTargetOnline(now)
	m.mu.RLock()
	runtimes := make([]*profileRuntime, 0, len(m.running))
	for _, runtime := range m.running {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	for _, runtime := range runtimes {
		m.expirePendingRuntime(now, runtime)
		m.maintainExpertAutoOpen(now, runtime)
		m.maintainPersonalStoreAutoOpen(now, runtime)
		m.maintainExpertMachine(now, runtime)
		m.maintainProfileSpeech(now, runtime)
	}
	for _, runtime := range runtimes {
		runtime.mu.RLock()
		profile := runtime.profile
		runtime.mu.RUnlock()
		if profile.RobotType == dnfrobotdomain.RobotTypeNormal {
			m.automateRuntime(now, profileMovementPolicy(profile), runtime)
			continue
		}
		runtime.mu.Lock()
		runtime.nextMove, runtime.nextStore = time.Time{}, time.Time{}
		runtime.autoStore = false
		runtime.mu.Unlock()
	}
}

func (m *Manager) reconcileTargetOnline(now time.Time) {
	if m == nil {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	policy := m.config.Automation
	if !m.automaticFleetReady {
		m.nextScale = time.Time{}
		m.mu.Unlock()
		return
	}
	if !policy.Enabled || policy.TargetOnline <= 0 {
		m.nextScale = time.Time{}
		m.mu.Unlock()
		return
	}
	if !m.nextScale.IsZero() && now.Before(m.nextScale) {
		m.mu.Unlock()
		return
	}
	m.nextScale = now.Add(time.Duration(policy.ScaleIntervalSeconds) * time.Second)
	profiles := append([]dnfrobotdomain.Profile(nil), m.config.Profiles...)
	running := make(map[string]*profileRuntime, len(m.running))
	for key, runtime := range m.running {
		running[key] = runtime
	}
	m.mu.Unlock()

	target := policy.TargetOnline
	// Defend the live scheduler as well as persisted migration/update paths.
	// This prevents an in-memory or legacy configuration with target=500 from
	// recreating hundreds of independent reconnect loops before it is saved.
	_, automaticFleet := m.endpoint.(AutomaticFleetProvider)
	if automaticFleet && target > AutomaticFleetOnlineTarget {
		target = AutomaticFleetOnlineTarget
	}
	if target > len(profiles) {
		target = len(profiles)
	}
	if len(running) < target {
		limit := policy.ScaleUpBatch
		pending := 0
		if automaticFleet {
			pending = automaticFleetPendingRuntimes(running)
		}
		for _, profile := range profiles {
			if limit <= 0 || len(running) >= target || automaticFleet && pending >= automaticFleetPendingLimit {
				break
			}
			key := profileKey(profile.ID)
			if running[key] != nil {
				continue
			}
			snapshot, err := m.startProfile(profile.ID, true)
			if err != nil || !snapshot.Running {
				continue
			}
			m.mu.RLock()
			running[key] = m.running[key]
			m.mu.RUnlock()
			if automaticFleet {
				// A newly started runtime is pending until its native client reaches a
				// town/store state. Count it immediately instead of racing its goroutine.
				pending++
			}
			limit--
		}
		return
	}
	if len(running) <= target {
		return
	}
	limit := policy.ScaleDownBatch
	for index := len(profiles) - 1; index >= 0 && limit > 0 && len(running) > target; index-- {
		profile := profiles[index]
		key := profileKey(profile.ID)
		runtime := running[key]
		if runtime == nil {
			continue
		}
		runtime.mu.RLock()
		managed := runtime.schedulerManaged
		runtime.mu.RUnlock()
		if !managed {
			continue
		}
		if err := m.stopProfileUnlocked(profile.ID); err != nil {
			continue
		}
		delete(running, key)
		limit--
	}
}

func automaticFleetPendingRuntimes(running map[string]*profileRuntime) int {
	pending := 0
	for _, runtime := range running {
		if runtime == nil {
			continue
		}
		runtime.mu.RLock()
		managed, client := runtime.schedulerManaged, runtime.client
		runtime.mu.RUnlock()
		if !managed {
			continue
		}
		if client == nil {
			pending++
			continue
		}
		switch client.Snapshot().State {
		case StateTownReady, StateStoreOpening, StateStoreReady, StateStoreClosing:
		default:
			pending++
		}
	}
	return pending
}

func (m *Manager) expirePendingRuntime(now time.Time, runtime *profileRuntime) {
	if runtime == nil {
		return
	}
	runtime.mu.RLock()
	client := runtime.client
	runtime.mu.RUnlock()
	if client == nil {
		return
	}
	state, expired := client.pendingOperationTimedOut(now, 30*time.Second)
	if !expired {
		return
	}
	err := fmt.Errorf("dnf robot %s acknowledgement timed out", state)
	m.recordAutomationError(runtime, err)
	_ = client.Close()
}

func (m *Manager) automateRuntime(now time.Time, policy dnfrobotdomain.AutomationPolicy, runtime *profileRuntime) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	client := runtime.client
	if runtime.privateStoreTransition {
		runtime.mu.Unlock()
		return
	}
	if client == nil {
		runtime.nextMove, runtime.nextChat, runtime.nextMegaphone, runtime.nextStore, runtime.storeClose = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
		runtime.autoStore = false
		runtime.mu.Unlock()
		return
	}
	if runtime.recycleActive {
		runtime.nextMove, runtime.nextChat, runtime.nextMegaphone, runtime.nextStore, runtime.storeClose = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
		runtime.mu.Unlock()
		return
	}
	clientSnapshot := client.Snapshot()
	state := clientSnapshot.State
	if state == StateStoreOpening {
		runtime.mu.Unlock()
		if _, err := client.retryPrivateStoreDisplay(now, false); err != nil {
			m.recordAutomationError(runtime, err)
			_ = client.Close()
		}
		return
	}
	if state == StateStoreReady {
		if clientSnapshot.ExpertStoreActive {
			runtime.mu.Unlock()
			return
		}
		closeDue := runtime.autoStore && !runtime.storeClose.IsZero() && !now.Before(runtime.storeClose)
		if closeDue {
			runtime.autoStore = false
			runtime.storeClose = time.Time{}
		}
		runtime.mu.Unlock()
		if closeDue {
			if err := client.ClosePrivateStore(); err != nil {
				m.recordAutomationError(runtime, err)
			}
		}
		return
	}
	if state != StateTownReady {
		runtime.nextMove, runtime.nextChat, runtime.nextMegaphone, runtime.nextStore, runtime.storeClose = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
		runtime.autoStore = false
		runtime.mu.Unlock()
		return
	}
	storeRejected := false
	if runtime.autoStore {
		// A rejected create/complete request returns the client to town-ready.
		runtime.autoStore = false
		runtime.storeClose = time.Time{}
		storeRejected = strings.TrimSpace(clientSnapshot.LastError) != ""
	}
	profile := runtime.profile
	if profile.AutomaticStoreDisabled {
		runtime.nextStore = time.Time{}
		runtime.storeRetryAt = time.Time{}
	}
	moveDue := policy.MoveIntervalMinSeconds > 0 && !runtime.nextMove.IsZero() && !now.Before(runtime.nextMove)
	useMegaphone := profile.MegaphoneEnabled
	chatDue := !useMegaphone && policy.ChatIntervalMinSeconds > 0 &&
		len(policy.ChatMessages) > 0 && !runtime.nextChat.IsZero() && !now.Before(runtime.nextChat)
	megaphoneEligible := useMegaphone && policy.MegaphoneIntervalMinSeconds > 0 &&
		len(policy.MegaphoneMessages) > 0
	megaphoneDue := megaphoneEligible && !runtime.nextMegaphone.IsZero() && !now.Before(runtime.nextMegaphone)
	storeDue := !profile.AutomaticStoreDisabled && policy.StoreIntervalMinSeconds > 0 && !runtime.nextStore.IsZero() && !now.Before(runtime.nextStore)
	if policy.MoveIntervalMinSeconds > 0 && runtime.nextMove.IsZero() {
		runtime.nextMove = now.Add(automationBehaviorDelay(policy.MoveIntervalMinSeconds, policy.MoveIntervalMaxSeconds, runtime.moveCursor, runtime.behaviorSeed, runtime.personality, automationActionMove))
	}
	if !useMegaphone && policy.ChatIntervalMinSeconds > 0 && len(policy.ChatMessages) > 0 && runtime.nextChat.IsZero() {
		runtime.nextChat = now.Add(automationBehaviorDelay(policy.ChatIntervalMinSeconds, policy.ChatIntervalMaxSeconds, runtime.chatCursor, runtime.behaviorSeed, runtime.personality, automationActionChat))
	}
	if megaphoneEligible && runtime.nextMegaphone.IsZero() {
		runtime.nextMegaphone = now.Add(automationBehaviorDelay(policy.MegaphoneIntervalMinSeconds, policy.MegaphoneIntervalMaxSeconds, runtime.megaphoneCursor, runtime.behaviorSeed, runtime.personality, automationActionMegaphone))
	}
	if !profile.AutomaticStoreDisabled && policy.StoreIntervalMinSeconds > 0 && runtime.nextStore.IsZero() {
		if !runtime.storeRetryAt.IsZero() {
			runtime.nextStore = runtime.storeRetryAt
			runtime.storeRetryAt = time.Time{}
		} else {
			runtime.nextStore = now.Add(automationBehaviorDelay(policy.StoreIntervalMinSeconds, policy.StoreIntervalMaxSeconds, runtime.storeCursor, runtime.behaviorSeed, runtime.personality, automationActionStore))
		}
	}
	moveCursor, chatCursor, megaphoneCursor := runtime.moveCursor, runtime.chatCursor, runtime.megaphoneCursor
	if storeDue {
		runtime.storeCursor++
		runtime.nextStore = now.Add(automationBehaviorDelay(policy.StoreIntervalMinSeconds, policy.StoreIntervalMaxSeconds, runtime.storeCursor, runtime.behaviorSeed, runtime.personality, automationActionStore))
	}
	if moveDue {
		runtime.moveCursor++
		runtime.nextMove = now.Add(automationBehaviorDelay(policy.MoveIntervalMinSeconds, policy.MoveIntervalMaxSeconds, runtime.moveCursor, runtime.behaviorSeed, runtime.personality, automationActionMove))
	}
	if chatDue {
		runtime.chatCursor++
		runtime.nextChat = now.Add(automationBehaviorDelay(policy.ChatIntervalMinSeconds, policy.ChatIntervalMaxSeconds, runtime.chatCursor, runtime.behaviorSeed, runtime.personality, automationActionChat))
	}
	if megaphoneDue {
		runtime.megaphoneCursor++
		runtime.nextMegaphone = now.Add(automationBehaviorDelay(policy.MegaphoneIntervalMinSeconds, policy.MegaphoneIntervalMaxSeconds, runtime.megaphoneCursor, runtime.behaviorSeed, runtime.personality, automationActionMegaphone))
	}
	runtime.mu.Unlock()
	if storeRejected {
		err := errors.New(clientSnapshot.LastError)
		m.recordAutomationError(runtime, err)
		m.relocateAutomationStoreFailure(now, runtime)
		_ = client.Close()
		return
	}

	if storeDue {
		if !m.reserveAutomationStore(runtime, policy.StoreConcurrent) {
			return
		}
		m.mu.RLock()
		ctx := m.ctx
		m.mu.RUnlock()
		if ctx == nil {
			ctx = context.Background()
		}
		planCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		price := automationStorePrice(policy.StoreUnitPrice, runtime.behaviorSeed, runtime.storeCursor)
		err := m.openPrivateStoreForClient(planCtx, client, profile, policy.StoreTitle, policy.StoreMaximumRows, price)
		cancel()
		if err != nil {
			m.releaseAutomationStore(runtime)
			m.recordAutomationError(runtime, err)
			m.relocateAutomationStoreFailure(now, runtime)
			_ = client.Close()
			return
		}
		runtime.mu.Lock()
		runtime.autoStore = true
		runtime.storeClose = now.Add(time.Duration(policy.StoreDurationSeconds) * time.Second)
		runtime.lastAction = automationActionStore
		runtime.mu.Unlock()
		return
	}
	if moveDue {
		x, y := automationBehaviorPositionForProfile(profile, policy.MoveRadius, moveCursor, runtime.behaviorSeed)
		if err := client.Move(profile.Village, profile.Area, x, y, profile.MoveType); err != nil {
			m.recordAutomationError(runtime, err)
		} else {
			runtime.mu.Lock()
			runtime.lastAction = automationActionMove
			runtime.mu.Unlock()
		}
	}
	if chatDue {
		message := policy.ChatMessages[automationMessageIndex(chatCursor, runtime.behaviorSeed, len(policy.ChatMessages))]
		if err := client.Chat(policy.ChatMode, message); err != nil {
			m.recordAutomationError(runtime, err)
		} else {
			runtime.mu.Lock()
			runtime.lastAction = automationActionChat
			runtime.mu.Unlock()
		}
	}
	if megaphoneDue {
		preparer, ok := m.endpoint.(MegaphonePreparer)
		if !ok {
			m.recordAutomationError(runtime, errors.New("dnf robot megaphone preparer is unavailable"))
			return
		}
		m.mu.RLock()
		ctx := m.ctx
		m.mu.RUnlock()
		if ctx == nil {
			ctx = context.Background()
		}
		prepareCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		slot, err := preparer.PrepareRobotServerMegaphone(prepareCtx, profile.AccountID, profile.CharacterID)
		cancel()
		if err == nil {
			message := policy.MegaphoneMessages[automationMessageIndex(
				megaphoneCursor,
				runtime.behaviorSeed^0x6d65676170686f6e,
				len(policy.MegaphoneMessages),
			)]
			err = client.Megaphone(slot, message)
		}
		if err != nil {
			m.recordAutomationError(runtime, err)
			return
		}
		runtime.mu.Lock()
		runtime.lastAction = automationActionMegaphone
		runtime.mu.Unlock()
	}
}

func (m *Manager) reserveAutomationStore(runtime *profileRuntime, limit int) bool {
	if runtime == nil {
		return false
	}
	if limit <= 0 {
		limit = 1
	}
	m.mu.RLock()
	running := make([]*profileRuntime, 0, len(m.running))
	for _, item := range m.running {
		running = append(running, item)
	}
	m.mu.RUnlock()
	count := 0
	runtime.mu.RLock()
	targetVillage, targetArea := runtime.profile.Village, runtime.profile.Area
	runtime.mu.RUnlock()
	for _, item := range running {
		item.mu.RLock()
		active := item.autoStore && item.profile.Village == targetVillage && item.profile.Area == targetArea
		item.mu.RUnlock()
		if active {
			count++
		}
	}
	return count < limit
}

func (m *Manager) releaseAutomationStore(runtime *profileRuntime) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	runtime.autoStore = false
	runtime.storeClose = time.Time{}
	runtime.mu.Unlock()
}

func (m *Manager) relocateAutomationStoreFailure(now time.Time, runtime *profileRuntime) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	profile := runtime.profile
	if profile.AutomaticStoreDisabled {
		runtime.nextStore = time.Time{}
		runtime.storeRetryAt = time.Time{}
		runtime.autoStore = false
		runtime.storeClose = time.Time{}
		runtime.mu.Unlock()
		return
	}
	cursor := runtime.storeCursor + runtime.reconnects + 1
	x, y := automationBehaviorPositionForProfile(profile, 96, cursor, runtime.behaviorSeed^0x73746f7265726574)
	if x == profile.X && y == profile.Y {
		x = uint16(clampAutomationCoordinate(int(profile.X) + 48))
		if profile.MoveBoundsSet {
			x = clampAutomationRange(x, profile.MoveMinX, profile.MoveMaxX)
			if x == profile.X {
				x = clampAutomationRange(uint16(clampAutomationCoordinate(int(profile.X)-48)), profile.MoveMinX, profile.MoveMaxX)
			}
		}
	}
	runtime.profile.X = x
	runtime.profile.Y = y
	runtime.nextStore = time.Time{}
	runtime.storeRetryAt = now.Add(30 * time.Second)
	runtime.autoStore = false
	runtime.storeClose = time.Time{}
	runtime.lastAction = "store_retry_relocate"
	runtime.mu.Unlock()
}

func (m *Manager) recordAutomationError(runtime *profileRuntime, err error) {
	if runtime == nil || err == nil {
		return
	}
	runtime.mu.Lock()
	runtime.lastError = err.Error()
	runtime.mu.Unlock()
}

func automationDelay(minSeconds, maxSeconds, cursor int) time.Duration {
	if minSeconds <= 0 {
		return 0
	}
	seconds := minSeconds
	if width := maxSeconds - minSeconds + 1; width > 1 {
		seconds += cursor % width
	}
	return time.Duration(seconds) * time.Second
}

func automationPosition(originX, originY, radius uint16, cursor int) (uint16, uint16) {
	if radius == 0 {
		return originX, originY
	}
	x, y := int(originX), int(originY)
	switch cursor & 3 {
	case 0:
		x += int(radius)
	case 1:
		y += int(radius)
	case 2:
		x -= int(radius)
	default:
		y -= int(radius)
	}
	if x < 0 {
		x = 0
	} else if x > 65535 {
		x = 65535
	}
	if y < 0 {
		y = 0
	} else if y > 65535 {
		y = 65535
	}
	return uint16(x), uint16(y)
}

func (m *Manager) stopProfileUnlocked(id string) error {
	key := profileKey(id)
	m.mu.Lock()
	runtime := m.running[key]
	delete(m.running, key)
	m.mu.Unlock()
	if runtime == nil {
		return nil
	}
	m.stopRuntime(runtime)
	select {
	case <-runtime.done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("timed out stopping dnf robot")
	}
}

func (m *Manager) stopRuntime(runtime *profileRuntime) {
	if runtime == nil {
		return
	}
	if runtime.cancel != nil {
		runtime.cancel()
	}
	runtime.mu.RLock()
	client := runtime.client
	runtime.mu.RUnlock()
	if client != nil {
		_ = client.Close()
	}
}

func (m *Manager) activeClient(id string) (*Client, error) {
	client, _, err := m.profileClient(id, StateTownReady)
	return client, err
}

func (m *Manager) profileClient(id string, state State) (*Client, dnfrobotdomain.Profile, error) {
	m.mu.RLock()
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	if runtime == nil {
		return nil, dnfrobotdomain.Profile{}, ErrNotReady
	}
	runtime.mu.RLock()
	client, profile := runtime.client, runtime.profile
	runtime.mu.RUnlock()
	if client == nil || client.Snapshot().State != state {
		return nil, dnfrobotdomain.Profile{}, ErrNotReady
	}
	return client, profile, nil
}

func (m *Manager) validateCharacter(ctx context.Context, profile dnfrobotdomain.Profile) error {
	if m == nil || m.provider == nil {
		return dnfrobotdomain.ErrUnavailable
	}
	group, ok := m.provider()
	if !ok || group.Character == nil {
		return dnfrobotdomain.ErrUnavailable
	}
	record, ok, err := group.Character.Load(ctx, strconv.Itoa(int(profile.CharacterID)))
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(record.AccountID) != profile.AccountID {
		return fmt.Errorf("%w: character %d is not owned by account %q", dnfrobotdomain.ErrInvalid, profile.CharacterID, profile.AccountID)
	}
	return nil
}

func snapshotRuntime(profile dnfrobotdomain.Profile, runtime *profileRuntime) RuntimeSnapshot {
	result := RuntimeSnapshot{Profile: profile, State: StateStopped, Village: profile.Village, Area: profile.Area, X: profile.X, Y: profile.Y}
	if runtime == nil {
		return result
	}
	runtime.mu.RLock()
	client, startedAt, reconnects, lastError, schedulerManaged := runtime.client, runtime.startedAt, runtime.reconnects, runtime.lastError, runtime.schedulerManaged
	personality, lastAction, recycleActive := runtime.personality, runtime.lastAction, runtime.recycleActive
	runtime.mu.RUnlock()
	result.Running, result.StartedAt, result.Reconnects, result.LastError = true, startedAt, reconnects, lastError
	result.SchedulerManaged = schedulerManaged
	result.Personality, result.LastAction = personality, lastAction
	result.RecycleActive = recycleActive
	if client != nil {
		clientState := client.Snapshot()
		result.State, result.Endpoint = clientState.State, clientState.Endpoint
		result.ReadyAt, result.CharacterID, result.Slot = clientState.ReadyAt, clientState.CharacterID, clientState.Slot
		result.StateSince, result.LastReceive, result.LastSend = clientState.StateSince, clientState.LastReceive, clientState.LastSend
		result.Village, result.Area, result.X, result.Y = clientState.Village, clientState.Area, clientState.X, clientState.Y
		result.StoreKind = clientState.StoreKind
		result.StoreActive = clientState.StoreActive
		result.ExpertStoreActive = clientState.ExpertStoreActive
		if clientState.LastError != "" {
			result.LastError = clientState.LastError
		}
	} else if result.LastError != "" {
		result.State = StateError
	} else {
		result.State = StateConnecting
	}
	return result
}

func findProfile(config dnfrobotdomain.Config, id string) (dnfrobotdomain.Profile, bool) {
	key := profileKey(id)
	for _, profile := range config.Profiles {
		if profileKey(profile.ID) == key {
			return profile, true
		}
	}
	return dnfrobotdomain.Profile{}, false
}

func profileKey(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
