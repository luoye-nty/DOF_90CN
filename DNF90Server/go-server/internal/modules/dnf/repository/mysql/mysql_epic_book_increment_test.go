package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
)

// 线上事故的回归闸门。
//
// 事故：同一账号一次拾取 8 件史诗碎片，服务端为每件开一个并发事务，7 件以
// MySQL 1213 (Deadlock) 回滚，掉落物留在原地、玩家实际丢件。
//
// 成因：碎片余额原先走「锁读整个账号区间 + DELETE 全量 + 批量 INSERT」。
// REPEATABLE READ 下对（可能还是空的）账号区间做 SELECT ... FOR UPDATE 会拿到
// **间隙锁**；间隙锁彼此兼容，却与 INSERT 需要的**插入意向锁**互斥，于是 N 个
// 并发写两两互等，形成死锁环，损失率恰为 (N-1)/N。
//
// 因此下面两个用例直接盯住发出的 SQL 形状，而不是只验结果值：一旦有人把实现
// 改回"整区间锁定读 + 整本账重写"，它们必然失败。
func TestMySQLAddBalanceUsesSingleRowUpsertWithoutRangeLock(t *testing.T) {
	sqlDB := &fakeSQLDB{rows: []fakeSQLRow{{values: []any{uint32(7)}}}}
	store := &mysqlEpicBookStore{mysqlStoreBase: mysqlStoreBase{router: newTestEpicBookRouter(t, sqlDB)}}

	balance, err := store.AddBalance(context.Background(), "account-1", 101020256, 3, epicBookIncrementTime())
	if err != nil {
		t.Fatalf("AddBalance() error = %v", err)
	}
	if balance != 7 {
		t.Fatalf("AddBalance() balance = %d, want the re-read value 7", balance)
	}
	if len(sqlDB.execs) != 2 {
		t.Fatalf("AddBalance() issued %d statements, want 2 (upsert + read-back): %v",
			len(sqlDB.execs), execQueries(sqlDB))
	}

	upsert := sqlDB.execs[0]
	assertContains(t, upsert.Query, "INSERT INTO `dnf_s1_w1`.`dnf_epic_piece_balances`")
	assertContains(t, upsert.Query, "ON DUPLICATE KEY UPDATE")
	// 冲突时必须让数据库端自增。若改成把客户端算好的总数写回去，并发下会丢更新。
	assertContains(t, upsert.Query, "`balance` = `balance` + VALUES(`balance`)")
	if got := upsert.Args; len(got) != 4 ||
		got[0] != "account-1" || got[1] != uint32(101020256) || got[2] != uint32(3) {
		t.Fatalf("upsert args = %#v", got)
	}

	readBack := sqlDB.execs[1]
	assertContains(t, readBack.Query, "SELECT balance FROM `dnf_s1_w1`.`dnf_epic_piece_balances`")
	assertContains(t, readBack.Query, "WHERE account_id = ? AND equipment_id = ?")
	if got := readBack.Args; len(got) != 2 || got[0] != "account-1" || got[1] != uint32(101020256) {
		t.Fatalf("read-back args = %#v", got)
	}

	// FOR UPDATE 是死锁的燃料，DELETE + 全排序读是写放大与区间锁的来源。
	for _, call := range sqlDB.execs {
		assertNotContains(t, call.Query, "FOR UPDATE")
		assertNotContains(t, call.Query, "DELETE")
		assertNotContains(t, call.Query, "ORDER BY equipment_id")
	}
}

// 事务内的作用域包装必须保持与 Load/Save 相同的账号范围校验：跨账号写入要在
// 触碰数据库之前就被拒绝，否则等于给"事务只能改本账号"这条不变量开了口子。
func TestMySQLScopedEpicBookAddBalanceKeepsAccountScope(t *testing.T) {
	sqlDB := &fakeSQLDB{rows: []fakeSQLRow{{values: []any{uint32(4)}}}}
	scoped := &epicBookScopedRepository{
		accountID: "account-1",
		base:      &mysqlEpicBookStore{mysqlStoreBase: mysqlStoreBase{router: newTestEpicBookRouter(t, sqlDB)}},
	}

	if _, err := scoped.AddBalance(context.Background(), "account-2", 101020256, 1, epicBookIncrementTime()); !errors.Is(err, db.ErrRecordKeyRequired) {
		t.Fatalf("cross-account AddBalance() error = %v, want key rejection", err)
	}
	if len(sqlDB.execs) != 0 {
		t.Fatalf("cross-account AddBalance() must not reach the database, calls=%v", execQueries(sqlDB))
	}

	// 同一个包装也必须真的把增量转发到 MySQL 存储上（而不是静默退回重写）。
	balance, err := scoped.AddBalance(context.Background(), "account-1", 101020256, 1, epicBookIncrementTime())
	if err != nil || balance != 4 {
		t.Fatalf("AddBalance() balance=%d error=%v", balance, err)
	}
	if len(sqlDB.execs) != 2 {
		t.Fatalf("scoped AddBalance() issued %d statements, want 2: %v", len(sqlDB.execs), execQueries(sqlDB))
	}
	assertContains(t, sqlDB.execs[0].Query, "ON DUPLICATE KEY UPDATE")
}

// 非法入参必须在构造 SQL 之前就被拒，避免发出没有 (account_id, equipment_id)
// 主键的写语句。
func TestMySQLAddBalanceRejectsInvalidArguments(t *testing.T) {
	cases := []struct {
		name        string
		accountID   string
		equipmentID uint32
		amount      uint32
	}{
		{"blank account", "   ", 101020256, 1},
		{"zero equipment", "account-1", 0, 1},
		{"zero amount", "account-1", 101020256, 0},
	}
	for _, tc := range cases {
		sqlDB := &fakeSQLDB{}
		store := &mysqlEpicBookStore{mysqlStoreBase: mysqlStoreBase{router: newTestEpicBookRouter(t, sqlDB)}}
		if _, err := store.AddBalance(context.Background(), tc.accountID, tc.equipmentID, tc.amount, epicBookIncrementTime()); !errors.Is(err, repository.ErrBalanceIncrementInvalid) {
			t.Fatalf("%s: AddBalance() error = %v, want ErrBalanceIncrementInvalid", tc.name, err)
		}
		if len(sqlDB.execs) != 0 {
			t.Fatalf("%s: invalid AddBalance() reached the database: %v", tc.name, execQueries(sqlDB))
		}
	}
}

func newTestEpicBookRouter(t *testing.T, sqlDB SQLDB) mysqlRouter {
	t.Helper()
	router, err := newMySQLRouter(sqlDB, MySQLGroupOptions{
		DatabasePlan: repository.DatabasePlan{
			WriteDatabases: []string{"dnf_s1_w1"},
			ReadDatabases:  []string{"dnf_s1_r1"},
		},
		Now: func() time.Time {
			return time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("newMySQLRouter() error = %v", err)
	}
	return router
}

func epicBookIncrementTime() time.Time {
	return time.Date(2026, 9, 26, 10, 40, 51, 0, time.UTC)
}
