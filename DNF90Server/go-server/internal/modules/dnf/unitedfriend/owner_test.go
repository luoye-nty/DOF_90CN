package unitedfriend

import (
	"context"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerKeepsOneWayCaseSensitiveRelations(t *testing.T) {
	ctx := context.Background()
	group := memory.NewMemoryGroup()
	for _, character := range []repository.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Name: "Alice", Level: 90},
		{CharacterID: "2", AccountID: "b", Name: "Bob", Level: 80},
		{CharacterID: "3", AccountID: "c", Name: "bob", Level: 70},
	} {
		if err := group.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(group, func() time.Time { return time.Unix(10, 0) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Add(ctx, "Alice", 1, "Bob", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Add(ctx, "Alice", 1, "bob", 3); err != nil {
		t.Fatal(err)
	}
	list, err := owner.List(ctx, "Alice")
	if err != nil || len(list) != 2 || list[0].Character.Name != "Bob" || list[1].Character.Name != "bob" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	observers, err := owner.Observers(ctx, "Bob")
	if err != nil || len(observers) != 1 || observers[0] != "Alice" {
		t.Fatalf("observers=%v err=%v", observers, err)
	}
	if _, err := owner.Add(ctx, "Alice", 1, "Alice", 1); !errors.Is(err, ErrSelf) {
		t.Fatalf("self error=%v", err)
	}
	if err := owner.Remove(ctx, "Alice", "Bob"); err != nil {
		t.Fatal(err)
	}
	list, _ = owner.List(ctx, "Alice")
	if len(list) != 1 || list[0].Character.Name != "bob" {
		t.Fatalf("remaining=%+v", list)
	}
}

// The friend window sends the op290 unknown-target sentinel because it adds by
// typed name. Requiring the sentinel to equal the resolved character id made
// every such add answer "该角色不存在" (op290 failure 0x15).
func TestOwnerAddAcceptsUnknownTargetIDSentinel(t *testing.T) {
	ctx := context.Background()
	group := memory.NewMemoryGroup()
	for _, character := range []repository.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Name: "鬼剑士", Level: 70},
		{CharacterID: "42", AccountID: "b", Name: "奶爸", Level: 70},
	} {
		if err := group.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(group, func() time.Time { return time.Unix(10, 0) })
	if err != nil {
		t.Fatal(err)
	}
	friend, err := owner.Add(ctx, "鬼剑士", 1, "奶爸", UnknownCharacterID)
	if err != nil || friend.Character.CharacterID != "42" {
		t.Fatalf("add by name friend=%+v err=%v", friend, err)
	}
	// The sentinel must not turn a self add into a relation.
	if _, err := owner.Add(ctx, "鬼剑士", 1, "鬼剑士", UnknownCharacterID); !errors.Is(err, ErrSelf) {
		t.Fatalf("self add error=%v", err)
	}
	// A scene target still carries a real id, and a mismatched one stays rejected.
	if _, err := owner.Add(ctx, "鬼剑士", 1, "奶爸", 7); !errors.Is(err, ErrCharacterNotFound) {
		t.Fatalf("mismatched id error=%v", err)
	}
	if _, err := owner.Add(ctx, "鬼剑士", 1, "无此角色", UnknownCharacterID); !errors.Is(err, ErrCharacterNotFound) {
		t.Fatalf("missing name error=%v", err)
	}
}

// Add 必须把"这条关系什么时候建立的"记下来：商城赠送的 7 天门槛只能靠它算。
// 而老关系（本字段上线之前建立的）必须报 trusted=false，让调用方放行而不是封锁。
func TestOwnerFriendshipSinceReportsTrustedAndLegacyRelations(t *testing.T) {
	ctx := context.Background()
	group := memory.NewMemoryGroup()
	for _, character := range []repository.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Name: "Alice", Level: 90},
		{CharacterID: "2", AccountID: "b", Name: "Bob", Level: 80},
		{CharacterID: "3", AccountID: "c", Name: "Carol", Level: 80},
	} {
		if err := group.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	moment := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	owner, err := NewOwner(group, func() time.Time { return moment })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Add(ctx, "Alice", 1, "Bob", 2); err != nil {
		t.Fatal(err)
	}

	at, trusted, friends, err := owner.FriendshipSince(ctx, "Alice", "Bob")
	if err != nil || !friends || !trusted || !at.Equal(moment) {
		t.Fatalf("fresh relation at=%v trusted=%t friends=%t err=%v", at, trusted, friends, err)
	}

	// 不是好友：friends=false，调用方必须拒绝赠送。
	if _, trusted, friends, err := owner.FriendshipSince(ctx, "Alice", "Carol"); err != nil || friends || trusted {
		t.Fatalf("non-friend trusted=%t friends=%t err=%v", trusted, friends, err)
	}
	// 单向关系同样不算好友：Bob 那边没有 Alice。
	if _, trusted, friends, err := owner.FriendshipSince(ctx, "Bob", "Alice"); err != nil || friends || trusted {
		t.Fatalf("reverse-only relation trusted=%t friends=%t err=%v", trusted, friends, err)
	}

	// 老数据：直接落到仓储上，模拟 friend_since 字段上线之前就存在的关系。
	legacy, found, err := group.UnitedFriend.Load(ctx, "Alice")
	if err != nil || !found {
		t.Fatalf("load legacy record found=%t err=%v", found, err)
	}
	legacy.FriendNames = append(legacy.FriendNames, "Carol")
	legacy.FriendSince["Carol"] = time.Time{}
	if err := group.UnitedFriend.Save(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	at, trusted, friends, err = owner.FriendshipSince(ctx, "Alice", "Carol")
	if err != nil || !friends || trusted || !at.IsZero() {
		t.Fatalf("legacy relation at=%v trusted=%t friends=%t err=%v", at, trusted, friends, err)
	}

	// 删掉再加必须重新计时，不能沿用旧时间戳。
	if err := owner.Remove(ctx, "Alice", "Bob"); err != nil {
		t.Fatal(err)
	}
	later := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	owner, err = NewOwner(group, func() time.Time { return later })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Add(ctx, "Alice", 1, "Bob", 2); err != nil {
		t.Fatal(err)
	}
	at, trusted, friends, err = owner.FriendshipSince(ctx, "Alice", "Bob")
	if err != nil || !friends || !trusted || !at.Equal(later) {
		t.Fatalf("re-added relation at=%v trusted=%t friends=%t err=%v", at, trusted, friends, err)
	}
}
