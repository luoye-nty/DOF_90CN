package dnf

import (
	"context"
	"path/filepath"
	"testing"

	"longheng.io/server/internal/modules/dnf/repository"
)

func TestSQLiteRepoComponentLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnf90.db")
	component, err := NewRepoComponent(nil, RepositoryConfig{
		Enabled:    true,
		Driver:     "sqlite",
		SQLitePath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := component.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	if err := component.Start(ctx); err != nil {
		t.Fatal(err)
	}
	group, ok := component.Group()
	if !ok {
		t.Fatal("SQLite repository group is unavailable")
	}
	if group.CharacterSettlement == nil {
		t.Fatal("SQLite repository group does not expose the atomic quest/inventory settlement")
	}
	if err := group.Account.Save(ctx, repository.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if snapshot := component.Snapshot(); snapshot.Driver != "sqlite" || snapshot.SQLite == nil || snapshot.MySQL != nil {
		t.Fatalf("SQLite component snapshot = %+v", snapshot)
	}
	if err := component.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryConfigKeepsLegacyMySQLDefault(t *testing.T) {
	cfg := Config{Repository: RepositoryConfig{
		Enabled:     true,
		MySQLDSN:    "root:password@tcp(127.0.0.1:3306)/dnf",
		ShardID:     "9999",
		TablePrefix: "dnf",
	}}
	cfg.Normalize(nil)
	if cfg.Repository.Driver != "mysql" {
		t.Fatalf("default repository driver = %q, want mysql", cfg.Repository.Driver)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
