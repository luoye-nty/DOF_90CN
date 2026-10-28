package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

const mysqlUnitedFriendTable = "united_friend_relations"

type mysqlUnitedFriendStore struct{ mysqlStoreBase }

func (s *mysqlUnitedFriendStore) Load(ctx context.Context, ownerName string) (repository.UnitedFriendRecord, bool, error) {
	ownerName = strings.TrimSpace(ownerName)
	if ownerName == "" {
		return repository.UnitedFriendRecord{}, false, nil
	}
	table, err := s.router.readTable(mysqlUnitedFriendTable, ownerName)
	if err != nil {
		return repository.UnitedFriendRecord{}, false, err
	}
	rows, err := s.router.db.QueryContext(ctx, fmt.Sprintf("SELECT friend_name, updated_at, friend_since FROM %s WHERE owner_name = ? ORDER BY friend_name", table), ownerName) //nolint:gosec
	if err != nil {
		return repository.UnitedFriendRecord{}, false, err
	}
	defer rows.Close()
	record := repository.UnitedFriendRecord{OwnerName: ownerName}
	for rows.Next() {
		var name string
		var updated sql.NullTime
		var since sql.NullTime
		if err := rows.Scan(&name, &updated, &since); err != nil {
			return repository.UnitedFriendRecord{}, false, err
		}
		record.FriendNames = append(record.FriendNames, name)
		if value := scanTime(updated); value.After(record.UpdatedAt) {
			record.UpdatedAt = value
		}
		// friend_since 为 NULL = 这条关系早于该字段上线（老数据）。这里显式落一个零值，
		// Save 时会再写回 NULL，保证 Load→Save 往返不会把"老好友"变成"今天刚加"。
		if record.FriendSince == nil {
			record.FriendSince = make(map[string]time.Time, len(record.FriendNames))
		}
		record.FriendSince[name] = scanTime(since)
	}
	if err := rows.Err(); err != nil {
		return repository.UnitedFriendRecord{}, false, err
	}
	return repository.NormalizeUnitedFriend(record), len(record.FriendNames) != 0, nil
}

func (s *mysqlUnitedFriendStore) Save(ctx context.Context, record repository.UnitedFriendRecord) error {
	record = repository.NormalizeUnitedFriend(record)
	if record.OwnerName == "" {
		return fmt.Errorf("united friend owner name is required")
	}
	table, err := s.router.writeTable(mysqlUnitedFriendTable, record.OwnerName)
	if err != nil {
		return err
	}
	updatedAt := timeOrNow(record.UpdatedAt, s.router.now)
	apply := func(exec interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}) error {
		if _, err := exec.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE owner_name = ?", table), record.OwnerName); err != nil { //nolint:gosec
			return err
		}
		query := fmt.Sprintf("INSERT INTO %s (owner_name, friend_name, updated_at, friend_since) VALUES (?, ?, ?, ?)", table) //nolint:gosec
		now := s.router.now().UTC()
		for _, name := range record.FriendNames {
			// 三种情况，语义必须分清（"互为好友满 7 天"直接依赖它）：
			//   有非零时间 → 原样保留（这条关系到底是什么时候建立的）
			//   有零值     → 写 NULL（老数据：早于本字段上线，读侧按"已满足"处理）
			//   键不存在   → 调用方没给时间 ⇒ 按新建立的关系记 now
			//                （兜底：不能让一条新关系因为没人填时间就"老"起来）
			var since any
			if at, ok := record.FriendSince[name]; ok {
				since = sqlTime(at)
			} else {
				since = now
			}
			if _, err := exec.ExecContext(ctx, query, record.OwnerName, name, updatedAt, since); err != nil {
				return err
			}
		}
		return nil
	}
	if beginner, ok := s.router.db.(sqlTxBeginner); ok {
		tx, err := beginner.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := apply(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return apply(s.router.db)
}

func (s *mysqlUnitedFriendStore) ListObservers(ctx context.Context, friendName string) ([]string, error) {
	friendName = strings.TrimSpace(friendName)
	if friendName == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	for _, database := range s.router.readDBs {
		table := mysqlTable(database, s.router.tablePrefix, mysqlUnitedFriendTable)
		rows, err := s.router.db.QueryContext(ctx, fmt.Sprintf("SELECT owner_name FROM %s WHERE friend_name = ? ORDER BY owner_name", table), friendName) //nolint:gosec
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				_ = rows.Close()
				return nil, err
			}
			seen[owner] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	out := make([]string, 0, len(seen))
	for owner := range seen {
		out = append(out, owner)
	}
	sort.Strings(out)
	return out, nil
}
