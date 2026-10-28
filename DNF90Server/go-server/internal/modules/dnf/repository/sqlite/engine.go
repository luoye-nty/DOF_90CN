package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

var ErrDatabaseRequired = errors.New("sqlite repository database is required")

const (
	checkpointModePassive  = "PASSIVE"
	checkpointModeTruncate = "TRUNCATE"
	periodicSyncInterval   = time.Minute

	sqliteSnapshotSyncSourcePeriodic = "periodic"
	sqliteSnapshotSyncSourceStop     = "stop"
)

var periodicSyncEvery = periodicSyncInterval

type checkpointResult struct {
	Busy               int
	LogFrames          int
	CheckpointedFrames int
}

type Snapshot struct {
	Driver        string `json:"driver"`
	Path          string `json:"path"`
	Started       bool   `json:"started"`
	StateVersion  int    `json:"state_version"`
	RecordCount   int    `json:"record_count"`
	Dirty         bool   `json:"dirty"`
	SyncCount     uint64 `json:"sync_count"`
	LastSyncAt    string `json:"last_sync_at,omitempty"`
	LastSyncError string `json:"last_sync_error,omitempty"`
}

type Component struct {
	mu            sync.RWMutex
	mutationGate  chan struct{}
	db            *sql.DB
	path          string
	runtime       *memory.Runtime
	persisted     memory.State
	group         repository.Group
	started       bool
	dirty         bool
	syncCancel    context.CancelFunc
	syncDone      chan struct{}
	syncCount     uint64
	lastSyncAt    time.Time
	lastSyncError string
}

func NewComponent(database *sql.DB, path string) *Component {
	return &Component{
		db:           database,
		path:         strings.TrimSpace(path),
		mutationGate: make(chan struct{}, 1),
	}
}

func (c *Component) acquireMutation(ctx context.Context) error {
	if c == nil || c.mutationGate == nil {
		return ErrDatabaseRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case c.mutationGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Component) releaseMutation() {
	if c != nil && c.mutationGate != nil {
		<-c.mutationGate
	}
}

func (c *Component) Name() string { return "dnf-repository-sqlite" }

func (c *Component) Preflight(ctx context.Context) error {
	if c == nil || c.db == nil {
		return ErrDatabaseRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return c.db.PingContext(ctx)
}

func (c *Component) Start(ctx context.Context) error {
	if c == nil || c.db == nil {
		return ErrDatabaseRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if err := configureDatabase(ctx, c.db); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create sqlite repository schema: %w", err)
	}
	state, err := loadState(ctx, c.db)
	if err != nil {
		return err
	}
	runtime, err := memory.NewRuntimeFromState(ctx, state)
	if err != nil {
		return fmt.Errorf("restore sqlite repository state: %w", err)
	}
	c.runtime = runtime
	c.group = buildGroup(c)
	c.started = true
	c.dirty = false
	persisted := runtime.Snapshot()
	if err := persistTransition(ctx, c.db, state, persisted); err != nil {
		c.started = false
		c.group = repository.Group{}
		c.runtime = nil
		return err
	}
	c.persisted = persisted
	c.startPeriodicSyncLocked()
	return nil
}

// ReplaceState validates and atomically replaces every domain record in a
// SQLite repository. It is intended for explicit migration and release-save
// export tools; normal game writes continue to flow through Component.
func ReplaceState(ctx context.Context, database *sql.DB, state memory.State) error {
	if database == nil {
		return ErrDatabaseRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Version == 0 {
		state.Version = memory.StateVersion
	}
	if _, err := memory.NewRuntimeFromState(ctx, state); err != nil {
		return fmt.Errorf("validate sqlite replacement state: %w", err)
	}
	if err := configureDatabase(ctx, database); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create sqlite repository schema: %w", err)
	}
	before, err := loadState(ctx, database)
	if err != nil {
		return err
	}
	return persistTransition(ctx, database, before, state)
}

func configureDatabase(ctx context.Context, database *sql.DB) error {
	for _, pragma := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := database.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure sqlite repository: %w", err)
		}
	}
	return nil
}

func (c *Component) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.stopPeriodicSync(ctx); err != nil {
		return err
	}
	if err := c.acquireMutation(ctx); err != nil {
		return err
	}
	defer c.releaseMutation()
	c.mu.Lock()
	var syncErr error
	if c.started && c.runtime != nil {
		syncErr = c.syncRuntimeLocked(ctx, checkpointModeTruncate, sqliteSnapshotSyncSourceStop)
	}
	c.started = false
	c.dirty = false
	c.group = repository.Group{}
	c.runtime = nil
	c.persisted = memory.State{}
	c.mu.Unlock()
	return syncErr
}

func (c *Component) Group() (repository.Group, bool) {
	if c == nil {
		return repository.Group{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.group, c.started
}

func (c *Component) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{Driver: "sqlite"}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot := Snapshot{
		Driver:        "sqlite",
		Path:          c.path,
		Started:       c.started,
		StateVersion:  memory.StateVersion,
		Dirty:         c.dirty,
		SyncCount:     c.syncCount,
		LastSyncError: c.lastSyncError,
	}
	if !c.lastSyncAt.IsZero() {
		snapshot.LastSyncAt = c.lastSyncAt.UTC().Format(time.RFC3339Nano)
	}
	if c.runtime != nil {
		state := c.runtime.Snapshot()
		snapshot.RecordCount = len(state.Accounts) + len(state.AccountInventories) + len(state.Characters) + len(state.Inventories) + len(state.Equipment) + len(state.Pets) + len(state.Quests) + len(state.Skills) + len(state.DungeonPermissions) + len(state.HellPartyPermissions) + len(state.PacketTemplates) + len(state.Settings) + len(state.Mailboxes) + len(state.EpicBooks) + len(state.UnitedFriends)
	}
	return snapshot
}

func (c *Component) check(ctx context.Context) error {
	if c == nil || c.db == nil {
		return ErrDatabaseRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.db.PingContext(ctx); err != nil {
		return err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastSyncError != "" {
		return fmt.Errorf("sqlite background save is unhealthy: %s", c.lastSyncError)
	}
	return nil
}

func (c *Component) read(ctx context.Context, apply func(repository.Group) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.started || c.runtime == nil {
		return ErrDatabaseRequired
	}
	return apply(c.runtime.Group())
}

func (c *Component) mutateAsync(ctx context.Context, apply func(repository.Group) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.acquireMutation(ctx); err != nil {
		return err
	}
	defer c.releaseMutation()
	c.mu.RLock()
	if !c.started || c.runtime == nil {
		c.mu.RUnlock()
		return ErrDatabaseRequired
	}
	group := c.runtime.Group()
	c.mu.RUnlock()
	if err := apply(group); err != nil {
		return err
	}
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
	return nil
}

func (c *Component) mutateSync(ctx context.Context, apply func(repository.Group) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.acquireMutation(ctx); err != nil {
		return err
	}
	defer c.releaseMutation()
	c.mu.RLock()
	if !c.started || c.runtime == nil {
		c.mu.RUnlock()
		return ErrDatabaseRequired
	}
	before := c.runtime.Snapshot()
	persistedBefore := c.persisted
	dirtyBefore := c.dirty
	group := c.runtime.Group()
	c.mu.RUnlock()
	if err := apply(group); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	after := c.runtime.Snapshot()
	if err := persistTransition(ctx, c.db, persistedBefore, after); err != nil {
		restored, restoreErr := memory.NewRuntimeFromState(context.Background(), before)
		if restoreErr == nil {
			c.runtime = restored
		}
		c.dirty = dirtyBefore
		return errors.Join(err, restoreErr)
	}
	c.persisted = after
	c.dirty = false
	return nil
}

// mutateCharacterAssetsSync keeps the same durable, all-or-nothing contract
// as mutateSync, but its persistence boundary is deliberately the three
// records a CharacterAssetUnitOfWork is allowed to mutate.  In particular a
// dungeon pickup must not serialize/checksum every quest, mailbox and other
// character record merely to add one stack to the selected character's bag.
//
// The component-wide mutation gate remains necessary: the in-memory aggregate
// and SQLite have one authoritative writer.  Narrowing the persisted delta
// shortens that gate's hold time without weakening transaction ordering.
func (c *Component) mutateCharacterAssetsSync(
	ctx context.Context,
	characterID string,
	apply func(repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error,
) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || apply == nil {
		return repository.ErrCharacterAssetTransactionUnavailable
	}
	return c.mutateRecordScopeSync(ctx, []scopedRecordRef{
		{kind: kindCharacter, key: characterID},
		{kind: kindInventory, key: characterID},
		{kind: kindEquipment, key: characterID},
	}, func(group repository.Group) error {
		return group.CharacterAssets.WithinCharacterAssets(ctx, characterID, apply)
	})
}

type characterAssetsSnapshot struct {
	character      repository.CharacterRecord
	characterFound bool
	inventory      repository.InventoryRecord
	inventoryFound bool
	equipment      repository.EquipmentRecord
	equipmentFound bool
}

func loadCharacterAssetsSnapshot(ctx context.Context, group repository.Group, characterID string) (characterAssetsSnapshot, error) {
	character, characterFound, err := group.Character.Load(ctx, characterID)
	if err != nil {
		return characterAssetsSnapshot{}, err
	}
	inventory, inventoryFound, err := group.Inventory.Load(ctx, characterID)
	if err != nil {
		return characterAssetsSnapshot{}, err
	}
	equipment, equipmentFound, err := group.Equipment.Load(ctx, characterID)
	if err != nil {
		return characterAssetsSnapshot{}, err
	}
	return characterAssetsSnapshot{
		character:      repository.CloneCharacter(character),
		characterFound: characterFound,
		inventory:      repository.CloneInventory(inventory),
		inventoryFound: inventoryFound,
		equipment:      repository.CloneEquipment(equipment),
		equipmentFound: equipmentFound,
	}, nil
}

func applyCharacterAssetsSnapshotToState(state *memory.State, snapshot characterAssetsSnapshot) {
	if state == nil {
		return
	}
	if snapshot.characterFound {
		state.Characters = replaceCharacterStateRecord(state.Characters, snapshot.character)
	}
	if snapshot.inventoryFound {
		state.Inventories = replaceInventoryStateRecord(state.Inventories, snapshot.inventory)
	}
	if snapshot.equipmentFound {
		state.Equipment = replaceEquipmentStateRecord(state.Equipment, snapshot.equipment)
	}
}

func replaceCharacterStateRecord(records []repository.CharacterRecord, record repository.CharacterRecord) []repository.CharacterRecord {
	for index := range records {
		if records[index].CharacterID == record.CharacterID {
			records[index] = repository.CloneCharacter(record)
			return records
		}
	}
	return append(records, repository.CloneCharacter(record))
}

func replaceInventoryStateRecord(records []repository.InventoryRecord, record repository.InventoryRecord) []repository.InventoryRecord {
	for index := range records {
		if records[index].CharacterID == record.CharacterID {
			records[index] = repository.CloneInventory(record)
			return records
		}
	}
	return append(records, repository.CloneInventory(record))
}

func replaceEquipmentStateRecord(records []repository.EquipmentRecord, record repository.EquipmentRecord) []repository.EquipmentRecord {
	for index := range records {
		if records[index].CharacterID == record.CharacterID {
			records[index] = repository.CloneEquipment(record)
			return records
		}
	}
	return append(records, repository.CloneEquipment(record))
}

func (c *Component) now() time.Time { return time.Now().UTC() }

func (c *Component) startPeriodicSyncLocked() {
	if c == nil || c.syncCancel != nil || periodicSyncEvery <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.syncCancel = cancel
	c.syncDone = done
	go c.runPeriodicSync(ctx, done, periodicSyncEvery)
}

func (c *Component) runPeriodicSync(ctx context.Context, done chan<- struct{}, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.syncRuntime(ctx, checkpointModePassive, sqliteSnapshotSyncSourcePeriodic)
		}
	}
}

func (c *Component) stopPeriodicSync(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.syncCancel
	done := c.syncDone
	c.syncCancel = nil
	c.syncDone = nil
	c.mu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop sqlite periodic save: %w", ctx.Err())
	}
}

func (c *Component) syncRuntime(ctx context.Context, checkpointMode, source string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.acquireMutation(ctx); err != nil {
		return err
	}
	defer c.releaseMutation()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.runtime == nil {
		return ErrDatabaseRequired
	}
	return c.syncRuntimeLocked(ctx, checkpointMode, source)
}

func (c *Component) syncRuntimeLocked(ctx context.Context, checkpointMode, source string) error {
	var err error
	if c.dirty {
		state := c.runtime.Snapshot()
		err = persistTransition(ctx, c.db, c.persisted, state)
		if err == nil {
			c.persisted = state
		}
		if err == nil {
			c.dirty = false
		}
	}
	if err == nil {
		_, err = checkpointDatabase(ctx, c.db, checkpointMode)
	}
	c.syncCount++
	c.lastSyncAt = time.Now().UTC()
	if err != nil {
		c.lastSyncError = fmt.Sprintf("%s: %v", source, err)
		return fmt.Errorf("%s sqlite repository save: %w", source, err)
	}
	c.lastSyncError = ""
	return nil
}

func checkpointDatabase(ctx context.Context, database *sql.DB, mode string) (checkpointResult, error) {
	if database == nil {
		return checkpointResult{}, ErrDatabaseRequired
	}
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if mode != checkpointModePassive && mode != checkpointModeTruncate {
		return checkpointResult{}, fmt.Errorf("unsupported sqlite checkpoint mode %q", mode)
	}
	var result checkpointResult
	err := database.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(
		&result.Busy,
		&result.LogFrames,
		&result.CheckpointedFrames,
	)
	if err != nil {
		return checkpointResult{}, fmt.Errorf("checkpoint sqlite repository: %w", err)
	}
	if result.Busy != 0 {
		return result, fmt.Errorf(
			"checkpoint sqlite repository is busy (log_frames=%d checkpointed_frames=%d)",
			result.LogFrames,
			result.CheckpointedFrames,
		)
	}
	return result, nil
}
