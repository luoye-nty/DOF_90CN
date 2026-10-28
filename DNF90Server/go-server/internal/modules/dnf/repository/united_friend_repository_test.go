package repository

import (
	"testing"
	"time"
)

// 好友关系的"建立时间"是商城赠送规则（互为好友满 7 天）的唯一数据来源。
// 这里锁死三件事：深拷贝隔离、删好友后时间戳不残留、老数据必须被判为"不可信"
// 而不是"刚加的好友"。
func TestUnitedFriendSinceSeparatesTrustedFromLegacyRelations(t *testing.T) {
	founded := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	record := UnitedFriendRecord{
		OwnerName:   "Alice",
		FriendNames: []string{"Old", "New"},
		FriendSince: map[string]time.Time{"New": founded},
	}
	if at, trusted := UnitedFriendSince(record, "New"); !trusted || !at.Equal(founded) {
		t.Fatalf("new friend since=%v trusted=%t, want %v/true", at, trusted, founded)
	}
	// 键缺失 = 老数据：必须 trusted=false（调用方按"已满 7 天"放行）。
	if at, trusted := UnitedFriendSince(record, "Old"); trusted || !at.IsZero() {
		t.Fatalf("legacy friend since=%v trusted=%t, want zero/false", at, trusted)
	}
	// 键存在但零值同样是老数据（MySQL 里 NULL 读回来就是零值）。
	record.FriendSince["Old"] = time.Time{}
	if at, trusted := UnitedFriendSince(record, "Old"); trusted || !at.IsZero() {
		t.Fatalf("null-since friend since=%v trusted=%t, want zero/false", at, trusted)
	}
	if _, trusted := UnitedFriendSince(record, "Missing"); trusted {
		t.Fatal("unknown name must not be treated as a trusted relation")
	}
}

func TestCloneUnitedFriendCopiesFriendSinceMap(t *testing.T) {
	founded := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	original := UnitedFriendRecord{
		OwnerName:   "Alice",
		FriendNames: []string{"Bob"},
		FriendSince: map[string]time.Time{"Bob": founded},
	}
	clone := CloneUnitedFriend(original)
	clone.FriendSince["Bob"] = time.Time{}
	clone.FriendSince["Eve"] = founded
	clone.FriendNames[0] = "Mallory"
	if !original.FriendSince["Bob"].Equal(founded) {
		t.Fatalf("clone mutation leaked into original: %v", original.FriendSince["Bob"])
	}
	if _, ok := original.FriendSince["Eve"]; ok {
		t.Fatal("clone added a key to the original map")
	}
	if original.FriendNames[0] != "Bob" {
		t.Fatalf("clone mutation leaked into original names: %v", original.FriendNames)
	}
}

// 删好友必须把时间戳一起清掉：残留的旧时间会让"删了再加"的人立刻满足 7 天规则。
func TestNormalizeUnitedFriendPrunesSinceForRemovedFriends(t *testing.T) {
	founded := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	normalized := NormalizeUnitedFriend(UnitedFriendRecord{
		OwnerName:   " Alice ",
		FriendNames: []string{" Bob ", "Bob", "Carol", ""},
		FriendSince: map[string]time.Time{"Bob": founded, "Carol": founded, "Dave": founded},
	})
	if len(normalized.FriendNames) != 2 || normalized.FriendNames[0] != "Bob" || normalized.FriendNames[1] != "Carol" {
		t.Fatalf("friend names = %v, want [Bob Carol]", normalized.FriendNames)
	}
	if len(normalized.FriendSince) != 2 {
		t.Fatalf("friend since = %v, want only the names still on the list", normalized.FriendSince)
	}
	if _, ok := normalized.FriendSince["Dave"]; ok {
		t.Fatal("removed friend kept its since timestamp")
	}
	if at, trusted := UnitedFriendSince(normalized, "Bob"); !trusted || !at.Equal(founded) {
		t.Fatalf("kept relation since=%v trusted=%t, want %v/true", at, trusted, founded)
	}
}

func TestNormalizeUnitedFriendKeepsEmptySinceMapNil(t *testing.T) {
	normalized := NormalizeUnitedFriend(UnitedFriendRecord{OwnerName: "Alice", FriendNames: []string{"Bob"}})
	if normalized.FriendSince != nil {
		t.Fatalf("friend since = %v, want nil (no timestamps supplied)", normalized.FriendSince)
	}
}
