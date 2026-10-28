package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeonpermission"
	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestComponentPersistsRepositoriesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	group, ok := component.Group()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	if err := group.Account.Save(ctx, repository.AccountRecord{AccountID: "account-1", State: "active"}); err != nil {
		t.Fatal(err)
	}
	creator := group.Character.(repository.CharacterCreator)
	if err := creator.CreateCharacter(ctx, repository.CharacterRecord{
		CharacterID: "1001", AccountID: "account-1", Slot: 0, Name: "hero", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{
		CharacterID: "1001",
		Slots:       map[string]repository.ItemStack{"0:0": {ItemID: 1234, Count: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := group.DungeonPermission.UpsertMax(ctx, "1001", 55, 3); err != nil {
		t.Fatal(err)
	}
	if got := component.Snapshot().RecordCount; got != 4 {
		t.Fatalf("record count = %d, want 4", got)
	}
	if err := component.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, restartedDB := startTestComponent(t, path)
	defer restartedDB.Close()
	defer restarted.Stop(ctx)
	restored, _ := restarted.Group()
	character, found, err := restored.Character.Load(ctx, "1001")
	if err != nil || !found || character.Name != "hero" || character.Level != 90 {
		t.Fatalf("restored character = %+v found=%t err=%v", character, found, err)
	}
	inventory, found, err := restored.Inventory.Load(ctx, "1001")
	if err != nil || !found || inventory.Slots["0:0"].Count != 2 {
		t.Fatalf("restored inventory = %+v found=%t err=%v", inventory, found, err)
	}
	permission, found, err := restored.DungeonPermission.Load(ctx, "1001")
	if err != nil || !found || len(permission.Entries) != 1 || permission.Entries[0].ClearState != 3 {
		t.Fatalf("restored permission = %+v found=%t err=%v", permission, found, err)
	}
}

func TestComponentPersistsCaseSensitiveUnitedFriendsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	group, ok := component.Group()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	if err := group.UnitedFriend.Save(ctx, repository.UnitedFriendRecord{
		OwnerName:   "Owner",
		FriendNames: []string{"Friend", "friend"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := component.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, restartedDB := startTestComponent(t, path)
	defer restartedDB.Close()
	defer restarted.Stop(ctx)
	restored, _ := restarted.Group()
	record, found, err := restored.UnitedFriend.Load(ctx, "Owner")
	if err != nil || !found || len(record.FriendNames) != 2 ||
		record.FriendNames[0] != "Friend" || record.FriendNames[1] != "friend" {
		t.Fatalf("restored united friends=%+v found=%t err=%v", record, found, err)
	}
	upper, err := restored.UnitedFriend.ListObservers(ctx, "Friend")
	if err != nil || len(upper) != 1 || upper[0] != "Owner" {
		t.Fatalf("uppercase observers=%v err=%v", upper, err)
	}
	lower, err := restored.UnitedFriend.ListObservers(ctx, "friend")
	if err != nil || len(lower) != 1 || lower[0] != "Owner" {
		t.Fatalf("lowercase observers=%v err=%v", lower, err)
	}
}

func TestComponentPersistsAccountDungeonPermissionOwnerAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	group, ok := component.Group()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	accountKey, err := dungeonpermission.AccountOwnerKey("account-shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := group.DungeonPermission.UpsertMax(ctx, accountKey, 700, 4); err != nil || !changed {
		t.Fatalf("account permission changed=%t err=%v", changed, err)
	}
	if err := component.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, restartedDB := startTestComponent(t, path)
	defer restartedDB.Close()
	defer restarted.Stop(ctx)
	restored, _ := restarted.Group()
	permission, found, err := restored.DungeonPermission.Load(ctx, accountKey)
	if err != nil || !found || permission.CharacterID != accountKey ||
		len(permission.Entries) != 1 || permission.Entries[0].DungeonID != 700 ||
		permission.Entries[0].ClearState != 4 {
		t.Fatalf("restored account permission=%+v found=%t err=%v", permission, found, err)
	}
}

func TestSQLiteAccountFindIDByLoginUsernameUsesLoginMapping(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	if _, err := database.ExecContext(ctx, `CREATE TABLE dnf_login_accounts (
account_id TEXT NOT NULL PRIMARY KEY,
username TEXT NOT NULL UNIQUE,
state TEXT NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO dnf_login_accounts (account_id, username, state) VALUES (?, ?, ?)`, "dnf:1", "account-one", "active"); err != nil {
		t.Fatal(err)
	}
	group, ok := component.Group()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	finder, ok := group.Account.(repository.LoginAccountUsernameFinder)
	if !ok {
		t.Fatal("SQLite account repository has no login username finder")
	}
	accountID, found, err := finder.FindAccountIDByLoginUsername(ctx, " ACCOUNT-ONE ")
	if err != nil || !found || accountID != "dnf:1" {
		t.Fatalf("FindAccountIDByLoginUsername() account=%q found=%t error=%v", accountID, found, err)
	}
}

func TestComponentPeriodicAndStopSyncKeepLatestSaveAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	previousInterval := periodicSyncEvery
	periodicSyncEvery = 10 * time.Millisecond
	defer func() { periodicSyncEvery = previousInterval }()

	component, database := startTestComponent(t, path)
	group, ok := component.Group()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Name:        "latest",
		Level:       15,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for component.Snapshot().SyncCount == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	periodicSnapshot := component.Snapshot()
	if periodicSnapshot.SyncCount == 0 || periodicSnapshot.LastSyncAt == "" || periodicSnapshot.LastSyncError != "" {
		t.Fatalf("periodic sync snapshot = %+v", periodicSnapshot)
	}
	if err := component.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stoppedSnapshot := component.Snapshot()
	if stoppedSnapshot.SyncCount <= periodicSnapshot.SyncCount || stoppedSnapshot.LastSyncError != "" {
		t.Fatalf("stop sync snapshot = %+v, periodic = %+v", stoppedSnapshot, periodicSnapshot)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if walInfo, err := os.Stat(path + "-wal"); err == nil && walInfo.Size() > 0 {
		t.Fatalf("WAL still contains %d bytes after stop", walInfo.Size())
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	restarted, restartedDB := startTestComponent(t, path)
	defer restartedDB.Close()
	defer restarted.Stop(ctx)
	restored, _ := restarted.Group()
	character, found, err := restored.Character.Load(ctx, "1001")
	if err != nil || !found || character.Name != "latest" || character.Level != 15 {
		t.Fatalf("restored character = %+v found=%t err=%v", character, found, err)
	}
}

func TestOrdinarySaveIsDirtyUntilBackgroundSync(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Name:        "async",
		Level:       11,
	}); err != nil {
		t.Fatal(err)
	}
	if !component.Snapshot().Dirty {
		t.Fatal("ordinary save was not marked dirty")
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM dnf_repository_records WHERE record_kind = 'character' AND record_key = '1001'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ordinary save reached SQLite before background sync: count=%d", count)
	}
	if err := component.syncRuntime(ctx, checkpointModePassive, sqliteSnapshotSyncSourcePeriodic); err != nil {
		t.Fatal(err)
	}
	if component.Snapshot().Dirty {
		t.Fatal("successful background sync left dirty state")
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM dnf_repository_records WHERE record_kind = 'character' AND record_key = '1001'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("background sync did not persist ordinary save: count=%d", count)
	}
}

func TestCharacterAssetsSyncPersistsOnlySelectedCharacterDelta(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()

	// Ordinary saves are intentionally deferred. They model unrelated scene
	// state already waiting for SQLite's periodic sync when a pickup arrives.
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Stats:       map[string]int64{"gold": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{
		CharacterID: "1001",
		Slots:       map[string]repository.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Quest.Save(ctx, repository.QuestRecord{
		CharacterID: "unrelated",
		States:      map[int64]repository.QuestState{7: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}

	scopedAssets, ok := group.CharacterAssets.(repository.CharacterAssetScopedUnitOfWork)
	if !ok {
		t.Fatal("SQLite character assets do not expose the scoped pickup transaction")
	}
	err := scopedAssets.WithinCharacterAssetsScoped(ctx, "1001", func(
		_ repository.CharacterRepository,
		inventories repository.InventoryRepository,
		_ repository.EquipmentRepository,
	) error {
		inventory, found, loadErr := inventories.Load(ctx, "1001")
		if loadErr != nil || !found {
			return fmt.Errorf("load selected inventory found=%t: %w", found, loadErr)
		}
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]repository.ItemStack)
		}
		inventory.Slots["0:121"] = repository.ItemStack{ItemID: 3227, Count: 1}
		return inventories.Save(ctx, inventory)
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		kind string
		key  string
	}{
		{kind: kindCharacter, key: "1001"},
		{kind: kindInventory, key: "1001"},
	} {
		var count int
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dnf_repository_records WHERE record_kind = ? AND record_key = ?`,
			want.kind,
			want.key,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("durable %s %q count=%d err=%v", want.kind, want.key, count, err)
		}
	}
	var unrelatedCount int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dnf_repository_records WHERE record_kind = ? AND record_key = ?`,
		kindQuest,
		"unrelated",
	).Scan(&unrelatedCount); err != nil || unrelatedCount != 0 {
		t.Fatalf("unrelated async record count=%d err=%v", unrelatedCount, err)
	}
	if !component.Snapshot().Dirty {
		t.Fatal("selected character sync cleared unrelated pending SQLite state")
	}
}

func TestQuestStateEntryWriterMergesObjectivesWithoutStaleFullRecordOverwrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()
	if err := group.Quest.Save(ctx, repository.QuestRecord{
		CharacterID: "1001",
		States: map[int64]repository.QuestState{
			101: {Status: "active", ProgressValue: 3, Extra: map[string]string{"source": "seed-a"}},
			102: {Status: "active", ProgressValue: 5, Extra: map[string]string{"source": "seed-b"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	writer, ok := group.Quest.(repository.QuestStateEntryWriter)
	if !ok {
		t.Fatal("SQLite quest repository has no single-entry writer")
	}
	firstAt := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Second)
	if err := writer.SaveQuestStateEntry(ctx, "1001", repository.QuestFieldStates, 101, repository.QuestState{
		Status: "active", ProgressValue: 2, UpdatedAt: firstAt, Extra: map[string]string{"source": "op33"},
	}, firstAt); err != nil {
		t.Fatal(err)
	}
	if err := writer.SaveQuestStateEntry(ctx, "1001", repository.QuestFieldStates, 102, repository.QuestState{
		Status: "active", ProgressValue: 4, UpdatedAt: secondAt, Extra: map[string]string{"source": "drop"},
	}, secondAt); err != nil {
		t.Fatal(err)
	}
	record, found, err := group.Quest.Load(ctx, "1001")
	if err != nil || !found {
		t.Fatalf("load merged quest found=%t err=%v", found, err)
	}
	if got := record.States[101]; got.ProgressValue != 2 || got.Extra["source"] != "op33" {
		t.Fatalf("quest 101=%+v", got)
	}
	if got := record.States[102]; got.ProgressValue != 4 || got.Extra["source"] != "drop" {
		t.Fatalf("quest 102=%+v", got)
	}
	if !record.UpdatedAt.Equal(secondAt) {
		t.Fatalf("quest updated_at=%s want=%s", record.UpdatedAt, secondAt)
	}
}

func TestCriticalUnitOfWorkFlushesPendingAndCurrentStateImmediately(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "1001"}); err != nil {
		t.Fatal(err)
	}
	if !component.Snapshot().Dirty {
		t.Fatal("ordinary inventory seed was not dirty")
	}
	if err := group.CharacterItems.WithinCharacterItems(ctx, "1001", func(inventory repository.InventoryRepository, _ repository.EquipmentRepository) error {
		record, found, err := inventory.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load inventory found=%t: %w", found, err)
		}
		record.Slots = map[string]repository.ItemStack{"0:0": {ItemID: 9001, Count: 2}}
		return inventory.Save(ctx, record)
	}); err != nil {
		t.Fatal(err)
	}
	if component.Snapshot().Dirty {
		t.Fatal("critical unit of work did not flush pending state")
	}
	state, err := loadState(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Inventories) != 1 || state.Inventories[0].Slots["0:0"].Count != 2 {
		t.Fatalf("critical state was not immediately persisted: %+v", state.Inventories)
	}
}

func TestCriticalUnitOfWorkFlushesUnrelatedPendingState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Name:        "pending-character",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "1001"}); err != nil {
		t.Fatal(err)
	}
	if err := group.CharacterItems.WithinCharacterItems(ctx, "1001", func(inventory repository.InventoryRepository, _ repository.EquipmentRepository) error {
		record, found, err := inventory.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load inventory found=%t: %w", found, err)
		}
		record.Slots = map[string]repository.ItemStack{"0:0": {ItemID: 9001, Count: 1}}
		return inventory.Save(ctx, record)
	}); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Characters) != 1 || state.Characters[0].Name != "pending-character" {
		t.Fatalf("unrelated pending character was not flushed: %+v", state.Characters)
	}
	if len(state.Inventories) != 1 || state.Inventories[0].Slots["0:0"].ItemID != 9001 {
		t.Fatalf("critical inventory was not flushed: %+v", state.Inventories)
	}
}

func TestCriticalUnitOfWorkAllowsOuterRepositoryReadWithoutSelfDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(context.Background())
	group, _ := component.Group()
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Name:        "outer-read",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "1001"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- group.CharacterItems.WithinCharacterItems(ctx, "1001", func(inventory repository.InventoryRepository, _ repository.EquipmentRepository) error {
			// This deliberately uses the outer SQLite wrapper, matching the op19
			// placement validator that used to re-enter Component.mu forever.
			character, found, err := group.Character.Load(ctx, "1001")
			if err != nil || !found || character.Level != 90 {
				return fmt.Errorf("load outer character found=%t level=%d: %w", found, character.Level, err)
			}
			record, found, err := inventory.Load(ctx, "1001")
			if err != nil || !found {
				return fmt.Errorf("load transaction inventory found=%t: %w", found, err)
			}
			record.Slots = map[string]repository.ItemStack{"1:34": {ItemID: 112540232, Count: 1}}
			return inventory.Save(ctx, record)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("outer repository read self-deadlocked: %v", ctx.Err())
	}
}

func TestMailboxAssetUnitOfWorkAllowsScopedAccountSaveWithoutSelfDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(context.Background())
	group, _ := component.Group()
	if err := group.Account.Save(ctx, repository.AccountRecord{AccountID: "account-1", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Name:        "mailbox-assets",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "1001"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- group.MailboxAssets.WithinMailboxAssets(ctx, "1001", "1001", func(
			accounts repository.AccountRepository,
			_ repository.AccountInventoryRepository,
			_ repository.CharacterRepository,
			inventory repository.InventoryRepository,
			_ repository.MailboxRepository,
		) error {
			// The Seria box is an account-and-character atomic write.  It must
			// save through the repository scoped by this unit of work, rather
			// than re-enter the outer SQLite Account wrapper and wait on itself.
			account, found, err := accounts.Load(ctx, "account-1")
			if err != nil || !found {
				return fmt.Errorf("load scoped account found=%t: %w", found, err)
			}
			account.Metadata = map[string]string{"seria_luck_value": "1"}
			if err := accounts.Save(ctx, account); err != nil {
				return fmt.Errorf("save scoped account: %w", err)
			}
			record, found, err := inventory.Load(ctx, "1001")
			if err != nil || !found {
				return fmt.Errorf("load transaction inventory found=%t: %w", found, err)
			}
			record.Slots = map[string]repository.ItemStack{"0:65": {ItemID: 2600014, Count: 1}}
			return inventory.Save(ctx, record)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("scoped account save self-deadlocked: %v", ctx.Err())
	}
	account, found, err := group.Account.Load(context.Background(), "account-1")
	if err != nil || !found || account.Metadata["seria_luck_value"] != "1" {
		t.Fatalf("saved account found=%t err=%v account=%+v", found, err, account)
	}
	bag, found, err := group.Inventory.Load(context.Background(), "1001")
	if err != nil || !found || bag.Slots["0:65"].ItemID != 2600014 {
		t.Fatalf("saved inventory found=%t err=%v inventory=%+v", found, err, bag)
	}
}

func TestMutationContentionHonorsContextDeadline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- component.mutateSync(ctx, func(repository.Group) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	err := group.Character.Save(waitCtx, repository.CharacterRecord{CharacterID: "1001"})
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-done
		t.Fatalf("contended mutation error=%v, want deadline exceeded", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestComponentUnitOfWorkRollbackDoesNotReachSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "1001"}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("reject mutation")
	err := group.CharacterItems.WithinCharacterItems(ctx, "1001", func(inventory repository.InventoryRepository, _ repository.EquipmentRepository) error {
		record, _, loadErr := inventory.Load(ctx, "1001")
		if loadErr != nil {
			return loadErr
		}
		record.Slots = map[string]repository.ItemStack{"0:0": {ItemID: 9999, Count: 1}}
		if saveErr := inventory.Save(ctx, record); saveErr != nil {
			return saveErr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("transaction error = %v, want %v", err, wantErr)
	}
	record, found, err := group.Inventory.Load(ctx, "1001")
	if err != nil || !found || len(record.Slots) != 0 {
		t.Fatalf("rolled-back inventory = %+v found=%t err=%v", record, found, err)
	}
}

func TestComponentRejectsCorruptRecordChecksum(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, database := startTestComponent(t, path)
	group, _ := component.Group()
	if err := group.Account.Save(ctx, repository.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := component.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE dnf_repository_records SET checksum = 'bad'`); err != nil {
		t.Fatal(err)
	}
	corrupt := NewComponent(database, path)
	if err := corrupt.Start(ctx); err == nil {
		t.Fatal("corrupt checksum was accepted")
	}
	database.Close()
}

func TestReplaceStateValidatesAndReplacesAllRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dnf90.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	defer database.Close()

	first := memory.State{
		Version: memory.StateVersion,
		Accounts: []repository.AccountRecord{{
			AccountID: "old-account",
		}},
	}
	if err := ReplaceState(ctx, database, first); err != nil {
		t.Fatal(err)
	}
	second := memory.State{
		Version: memory.StateVersion,
		Accounts: []repository.AccountRecord{{
			AccountID: "account-1",
		}},
		Characters: []repository.CharacterRecord{{
			CharacterID: "1",
			AccountID:   "account-1",
			Slot:        0,
			Name:        "Test",
			Level:       90,
		}},
	}
	if err := ReplaceState(ctx, database, second); err != nil {
		t.Fatal(err)
	}

	component := NewComponent(database, path)
	if err := component.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer component.Stop(ctx)
	if got := component.Snapshot().RecordCount; got != 2 {
		t.Fatalf("record count = %d, want 2", got)
	}
	group, _ := component.Group()
	if _, found, err := group.Account.Load(ctx, "old-account"); err != nil || found {
		t.Fatalf("old account found=%t err=%v", found, err)
	}
	character, found, err := group.Character.Load(ctx, "1")
	if err != nil || !found || character.Name != "Test" || character.Level != 90 {
		t.Fatalf("character = %+v found=%t err=%v", character, found, err)
	}
}

func startTestComponent(t *testing.T, path string) (*Component, *sql.DB) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	component := NewComponent(database, path)
	if err := component.Start(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return component, database
}
