package mysql

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/repository"
)

func TestMySQLNextCharacterTombstoneSlotSupportsConsecutiveDeletes(t *testing.T) {
	sqlDB := &fakeSQLDB{rows: []fakeSQLRow{
		{err: sql.ErrNoRows},
		{values: []any{-1}},
	}}
	table := mysqlTable("dnf_s1_w1", "dnf", mysqlCharTable)

	first, err := mysqlNextCharacterTombstoneSlot(context.Background(), sqlDB, table, "dnf:account")
	if err != nil {
		t.Fatalf("first tombstone slot: %v", err)
	}
	second, err := mysqlNextCharacterTombstoneSlot(context.Background(), sqlDB, table, "dnf:account")
	if err != nil {
		t.Fatalf("second tombstone slot: %v", err)
	}
	if first != -1 || second != -2 {
		t.Fatalf("tombstone slots = (%d, %d), want (-1, -2)", first, second)
	}
	if len(sqlDB.execs) != 2 {
		t.Fatalf("query count = %d, want 2", len(sqlDB.execs))
	}
	for _, call := range sqlDB.execs {
		assertContains(t, call.Query, "WHERE account_id = ? AND delete_flag = 1 AND slot < 0")
		assertContains(t, call.Query, "ORDER BY slot ASC LIMIT 1 FOR UPDATE")
		if len(call.Args) != 1 || call.Args[0] != "dnf:account" {
			t.Fatalf("query args = %#v, want account id", call.Args)
		}
	}
}

func TestMySQLNextCharacterTombstoneSlotRejectsIntegerExhaustion(t *testing.T) {
	sqlDB := &fakeSQLDB{rows: []fakeSQLRow{{values: []any{mysqlCharacterSlotMin}}}}
	table := mysqlTable("dnf_s1_w1", "dnf", mysqlCharTable)

	_, err := mysqlNextCharacterTombstoneSlot(context.Background(), sqlDB, table, "dnf:account")
	if !errors.Is(err, repository.ErrCharacterDeleteUnavailable) {
		t.Fatalf("tombstone slot error = %v, want ErrCharacterDeleteUnavailable", err)
	}
}
