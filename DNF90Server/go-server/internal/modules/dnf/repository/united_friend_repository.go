package repository

import (
	"context"
	"slices"
	"strings"
	"time"

	"longheng.io/server/internal/platform/db"
)

// UnitedFriendRepository stores one-way, case-sensitive friend edges.  The
// owner name is the record key so list projection and character rename/delete
// maintenance never depend on a live session.
type UnitedFriendRepository interface {
	db.Store[UnitedFriendRecord]
	ListObservers(context.Context, string) ([]string, error)
}

type UnitedFriendRecord struct {
	OwnerName   string    `json:"owner_name"`
	FriendNames []string  `json:"friend_names,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	// FriendSince 记录每条好友关系**建立**的时间（键 = 好友名）。
	//
	// 2026-09-27 新增：商城赠送要求"互为好友满 7 天"，而 UpdatedAt 是"最后一次保存
	// 整张名单"的时间——Save 会把同一 owner 的所有行刷成同一个时间，任何一次增删都会
	// 重置它，**不能当好友年龄用**。
	//
	// 约定：
	//   - 键缺失 或 值为零值 time.Time ⇒ **该关系早于本字段上线**（老数据）。
	//     调用方必须按"已满足 7 天"处理（先来后到：老好友不能被新规则追溯封锁）。
	//   - 非零值 ⇒ 关系建立时间。
	// 零值会在 MySQL 里落成 NULL，所以 Load→Save 的往返不会把"老数据"变成"刚认识"。
	FriendSince map[string]time.Time `json:"friend_since,omitempty"`
}

func UnitedFriendKey(record UnitedFriendRecord) string { return strings.TrimSpace(record.OwnerName) }

func CloneUnitedFriend(record UnitedFriendRecord) UnitedFriendRecord {
	record.FriendNames = append([]string(nil), record.FriendNames...)
	if len(record.FriendSince) != 0 {
		since := make(map[string]time.Time, len(record.FriendSince))
		for name, at := range record.FriendSince {
			since[name] = at
		}
		record.FriendSince = since
	}
	return record
}

// UnitedFriendSince 返回指定好友的关系建立时间；第二个返回值表示这个时间是否可信。
// 老数据（键缺失或零值）返回 (zero, false)——调用方按"已满 7 天"处理。
func UnitedFriendSince(record UnitedFriendRecord, friendName string) (time.Time, bool) {
	at, found := record.FriendSince[strings.TrimSpace(friendName)]
	if !found || at.IsZero() {
		return time.Time{}, false
	}
	return at, true
}

func NormalizeUnitedFriend(record UnitedFriendRecord) UnitedFriendRecord {
	record = CloneUnitedFriend(record)
	record.OwnerName = strings.TrimSpace(record.OwnerName)
	out := record.FriendNames[:0]
	seen := make(map[string]struct{}, len(record.FriendNames))
	for _, name := range record.FriendNames {
		name = strings.TrimSpace(name)
		if name == "" || name == record.OwnerName {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	record.FriendNames = out
	slices.Sort(record.FriendNames)
	// FriendSince 只保留仍在名单里的键，避免删掉好友后残留时间戳。
	if len(record.FriendSince) != 0 {
		since := make(map[string]time.Time, len(record.FriendSince))
		for name, at := range record.FriendSince {
			name = strings.TrimSpace(name)
			if _, ok := seen[name]; !ok {
				continue
			}
			since[name] = at
		}
		record.FriendSince = since
	}
	return record
}
