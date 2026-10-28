package mysql

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

const mysqlEpicPieceBalancesTable = "epic_piece_balances"

type mysqlEpicBookStore struct {
	mysqlStoreBase
}

func (s *mysqlEpicBookStore) Load(ctx context.Context, accountID string) (repository.EpicBookRecord, bool, error) {
	table, err := s.router.readTable(mysqlEpicPieceBalancesTable, accountID)
	if err != nil {
		return repository.EpicBookRecord{}, false, err
	}
	query := s.router.selectQuery("SELECT equipment_id, balance, updated_at FROM " + table + " WHERE account_id = ? ORDER BY equipment_id")
	rows, err := s.router.db.QueryContext(ctx, query, accountID)
	if err != nil {
		return repository.EpicBookRecord{}, false, err
	}
	defer rows.Close()
	record := repository.EpicBookRecord{AccountID: accountID}
	found := false
	for rows.Next() {
		var equipmentID uint32
		var balance uint32
		var updatedAt sql.NullTime
		if err := rows.Scan(&equipmentID, &balance, &updatedAt); err != nil {
			return repository.EpicBookRecord{}, false, err
		}
		if equipmentID == 0 || balance == 0 {
			continue
		}
		if record.Balances == nil {
			record.Balances = make(map[uint32]uint32)
		}
		record.Balances[equipmentID] = balance
		if value := scanTime(updatedAt); value.After(record.UpdatedAt) {
			record.UpdatedAt = value
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return repository.EpicBookRecord{}, false, err
	}
	return repository.CloneEpicBook(record), found, nil
}

func (s *mysqlEpicBookStore) Save(ctx context.Context, record repository.EpicBookRecord) error {
	accountID, err := requireRecordKey(repository.EpicBookKey, record, "epic book")
	if err != nil {
		return err
	}
	table, err := s.router.writeTable(mysqlEpicPieceBalancesTable, accountID)
	if err != nil {
		return err
	}
	updatedAt := timeOrNow(record.UpdatedAt, s.router.now)
	ids := make([]uint32, 0, len(record.Balances))
	for equipmentID, balance := range record.Balances {
		if equipmentID != 0 && balance != 0 {
			ids = append(ids, equipmentID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, err := database.ExecContext(ctx, "DELETE FROM "+table+" WHERE account_id = ?", accountID); err != nil {
			return err
		}
		columns := []string{"account_id", "equipment_id", "balance", "updated_at"}
		args := make([]any, 0, len(ids)*len(columns))
		for _, equipmentID := range ids {
			args = append(args, accountID, equipmentID, record.Balances[equipmentID], updatedAt)
		}
		for start := 0; start < len(ids); start += mysqlReplaceBatchSize {
			end := min(start+mysqlReplaceBatchSize, len(ids))
			if _, err := database.ExecContext(
				ctx,
				buildMySQLBulkInsert(table, columns, end-start),
				args[start*len(columns):end*len(columns)]...,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddBalance 用单行 upsert 给一个碎片余额加量，并回读写入后的余额。
//
// 这里刻意不做整账号区间的锁读。REPEATABLE READ 下，对（可能还是空的）账号
// 区间做 SELECT ... FOR UPDATE 拿到的是**间隙锁**；间隙锁彼此兼容，却与 INSERT
// 需要的**插入意向锁**互斥，于是同一账号的 N 个并发写入两两互等形成死锁环。
// 线上实测：8 件碎片同时拾取时 7 件以 1213 回滚、掉落物留在原地。
// 单行 upsert 只争用 (account_id, equipment_id) 这一行的记录锁，不产生间隙锁：
// 隔离级别不变的前提下实测 N=8 全部成功，且并发自增的余额也全部正确。
//
// 余额上限：这里把 uint32 溢出交给 int unsigned 列 + 严格模式判定。溢出时
// MySQL 报 1264，事务整体回滚，调用方得到 error（掉落物照旧留在原地），与
// repository.ErrBalanceOverflow 的可观察结果一致，只是错误值不同。
func (s *mysqlEpicBookStore) AddBalance(
	ctx context.Context,
	accountID string,
	equipmentID uint32,
	amount uint32,
	now time.Time,
) (uint32, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || equipmentID == 0 || amount == 0 {
		return 0, repository.ErrBalanceIncrementInvalid
	}
	table, err := s.router.writeTable(mysqlEpicPieceBalancesTable, accountID)
	if err != nil {
		return 0, err
	}
	updatedAt := timeOrNow(now, s.router.now)
	var balance uint32
	err = withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(
			ctx,
			buildUpsert(
				table,
				[]string{"account_id", "equipment_id", "balance", "updated_at"},
				[]string{incrementValue("balance"), updateValue("updated_at")},
			),
			accountID, equipmentID, amount, updatedAt,
		); execErr != nil {
			return execErr
		}
		// 回读走非锁定读：本事务刚写过这一行、且仍持有它的排他锁，提交前没有
		// 其它写入者能改动它，因此读到的必然是本次自增后的值。这里若用
		// FOR UPDATE 就等于又引入一次锁定读，没有必要。
		return database.QueryRowContext(
			ctx,
			"SELECT balance FROM "+table+" WHERE account_id = ? AND equipment_id = ?",
			accountID, equipmentID,
		).Scan(&balance)
	})
	if err != nil {
		return 0, err
	}
	return balance, nil
}
