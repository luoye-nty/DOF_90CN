package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

const unitedFriendSinceColumns = "SELECT friend_name, updated_at, friend_since FROM"

// 老数据（friend_since IS NULL）读回来必须是零值：这一条"先来后到"的例外是整套
// 7 天规则的底线——一旦读成"现在"，所有老好友都会被新规则追溯封锁。
func TestLoadUnitedFriendKeepsNullSinceAsUntrustedZeroTime(t *testing.T) {
	updated := time.Date(2026, 9, 1, 4, 5, 6, 0, time.UTC)
	founded := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sqlDB := &fakeSQLDB{
		rowsets: []fakeSQLRows{{
			rows: []fakeSQLRow{
				{values: []any{"Old", updated, nil}},
				{values: []any{"New", updated, founded}},
			},
		}},
	}
	repositories := newTestMySQLGroup(t, sqlDB)
	record, found, err := repositories.UnitedFriend.Load(context.Background(), "Alice")
	if err != nil || !found {
		t.Fatalf("load found=%t err=%v", found, err)
	}
	if len(record.FriendNames) != 2 {
		t.Fatalf("friend names = %v", record.FriendNames)
	}
	if at, trusted := repository.UnitedFriendSince(record, "Old"); trusted || !at.IsZero() {
		t.Fatalf("legacy relation at=%v trusted=%t, want zero/false", at, trusted)
	}
	if at, trusted := repository.UnitedFriendSince(record, "New"); !trusted || !at.Equal(founded) {
		t.Fatalf("founded relation at=%v trusted=%t, want %v/true", at, trusted, founded)
	}
	call := requireExecContaining(t, sqlDB, unitedFriendSinceColumns)
	if len(call.Args) != 1 || call.Args[0] != "Alice" {
		t.Fatalf("load args = %#v, want [Alice]", call.Args)
	}
}

// Load→Save 往返：读到的老数据必须再写回 NULL，不能因为"零值没法存"就变成今天。
func TestSaveUnitedFriendRoundTripsNullSinceBackToNull(t *testing.T) {
	updated := time.Date(2026, 9, 1, 4, 5, 6, 0, time.UTC)
	founded := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sqlDB := &fakeSQLDB{
		rowsets: []fakeSQLRows{{
			rows: []fakeSQLRow{
				{values: []any{"Old", updated, nil}},
				{values: []any{"New", updated, founded}},
			},
		}},
	}
	repositories := newTestMySQLGroup(t, sqlDB)
	record, found, err := repositories.UnitedFriend.Load(context.Background(), "Alice")
	if err != nil || !found {
		t.Fatalf("load found=%t err=%v", found, err)
	}
	if err := repositories.UnitedFriend.Save(context.Background(), record); err != nil {
		t.Fatalf("save error = %v", err)
	}
	inserts := unitedFriendInserts(t, sqlDB)
	if len(inserts) != 2 {
		t.Fatalf("insert count = %d, want 2; calls=%v", len(inserts), execQueries(sqlDB))
	}
	if got := inserts["Old"][3]; got != nil {
		t.Fatalf("legacy friend_since = %#v, want nil so the relation stays grandfathered", got)
	}
	if got, ok := inserts["New"][3].(time.Time); !ok || !got.Equal(founded) {
		t.Fatalf("founded friend_since = %#v, want %v", inserts["New"][3], founded)
	}
}

// 直接把一条新关系交给仓储（没有 friend_since）时，兜底必须是 now：
// 否则这条"今天刚加的好友"会被当成老数据，立刻满足 7 天规则。
func TestSaveUnitedFriendStampsMissingSinceWithRouterClock(t *testing.T) {
	routerNow := time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC)
	sqlDB := &fakeSQLDB{}
	repositories := newTestMySQLGroup(t, sqlDB)
	err := repositories.UnitedFriend.Save(context.Background(), repository.UnitedFriendRecord{
		OwnerName:   "Alice",
		FriendNames: []string{"Bob"},
	})
	if err != nil {
		t.Fatalf("save error = %v", err)
	}
	inserts := unitedFriendInserts(t, sqlDB)
	if got, ok := inserts["Bob"][3].(time.Time); !ok || !got.Equal(routerNow) {
		t.Fatalf("friend_since = %#v, want router clock %v", inserts["Bob"][3], routerNow)
	}
}

// 显式零值（不是"没给"，而是"这条关系属于老数据"）同样要落成 NULL。
func TestSaveUnitedFriendWritesExplicitZeroSinceAsNull(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repositories := newTestMySQLGroup(t, sqlDB)
	err := repositories.UnitedFriend.Save(context.Background(), repository.UnitedFriendRecord{
		OwnerName:   "Alice",
		FriendNames: []string{"Bob"},
		FriendSince: map[string]time.Time{"Bob": {}},
	})
	if err != nil {
		t.Fatalf("save error = %v", err)
	}
	inserts := unitedFriendInserts(t, sqlDB)
	if got := inserts["Bob"][3]; got != nil {
		t.Fatalf("friend_since = %#v, want nil", got)
	}
}

func unitedFriendInserts(t *testing.T, sqlDB *fakeSQLDB) map[string][]any {
	t.Helper()
	out := make(map[string][]any)
	for _, call := range sqlDB.execs {
		if !strings.Contains(call.Query, "INSERT INTO") || !strings.Contains(call.Query, "friend_since") {
			continue
		}
		if len(call.Args) != 4 {
			t.Fatalf("insert args = %#v, want 4 columns", call.Args)
		}
		name, ok := call.Args[1].(string)
		if !ok {
			t.Fatalf("insert friend name = %#v", call.Args[1])
		}
		out[name] = call.Args
	}
	return out
}
