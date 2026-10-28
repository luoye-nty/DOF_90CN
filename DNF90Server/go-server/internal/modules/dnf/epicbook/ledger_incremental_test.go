package epicbook

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 拾取落库必须优先走"单行增量"。
//
// 这条断言是线上死锁事故（同一账号并发拾取 8 件碎片，7 件以 1213 回滚）的回归
// 闸门：把 AddFragment 改回"锁读整本账 + 删光重写"，increments 会变成 0、
// loads/saves 会变成非 0，用例立刻失败。
func TestAddFragmentPrefersInPlaceIncrement(t *testing.T) {
	store := &recordingIncrementStore{balances: map[uint32]uint32{}}
	uow := &stubEpicBookCraftUnitOfWork{store: store}

	balance, err := AddFragment(context.Background(), uow, "account-1", "7", 101020256, 3, testTime())
	if err != nil || balance != 3 {
		t.Fatalf("AddFragment() balance=%d error=%v", balance, err)
	}
	if store.increments != 1 {
		t.Fatalf("in-place increments = %d, want 1", store.increments)
	}
	if store.loads != 0 || store.saves != 0 {
		t.Fatalf("increment path must not rewrite the whole ledger: loads=%d saves=%d",
			store.loads, store.saves)
	}

	// 同一账号连续两次自增必须累加，而不是覆盖。
	if balance, err := AddFragment(context.Background(), uow, "account-1", "7", 101020256, 2, testTime()); err != nil || balance != 5 {
		t.Fatalf("second AddFragment() balance=%d error=%v", balance, err)
	}
	if store.increments != 2 {
		t.Fatalf("in-place increments = %d, want 2", store.increments)
	}
}

// 不支持增量的后端（内存、SQLite）必须继续可用，且语义与增量路径一致。
// 这条守住"修 A 不能坏 B"：新增的增量路径不能把别的后端变成不可用。
func TestAddFragmentFallsBackToWholeLedgerRewrite(t *testing.T) {
	store := &rewriteOnlyStore{balances: map[uint32]uint32{101020256: 4}}
	uow := &stubEpicBookCraftUnitOfWork{store: store}

	balance, err := AddFragment(context.Background(), uow, "account-1", "7", 101020256, 3, testTime())
	if err != nil || balance != 7 {
		t.Fatalf("AddFragment() balance=%d error=%v", balance, err)
	}
	if store.saves != 1 {
		t.Fatalf("fallback saves = %d, want 1", store.saves)
	}
	if store.balances[101020256] != 7 {
		t.Fatalf("fallback balance = %d, want 7", store.balances[101020256])
	}
	if store.updatedAt != testTime() {
		t.Fatalf("fallback UpdatedAt = %v, want %v", store.updatedAt, testTime())
	}

	// 整本重写路径的余额上限判定必须原样保留，且不得落库。
	store.balances[101020256] = math.MaxUint32
	savesBefore := store.saves
	if _, err := AddFragment(context.Background(), uow, "account-1", "7", 101020256, 1, testTime()); !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("overflow error = %v, want ErrBalanceOverflow", err)
	}
	if store.saves != savesBefore {
		t.Fatalf("overflow must not persist: saves=%d want=%d", store.saves, savesBefore)
	}
	if store.balances[101020256] != math.MaxUint32 {
		t.Fatalf("overflow mutated balance to %d", store.balances[101020256])
	}
}

// 参数校验仍然发生在进入事务之前，保持原错误值不变。
func TestAddFragmentKeepsPreTransactionValidation(t *testing.T) {
	store := &recordingIncrementStore{balances: map[uint32]uint32{}}
	uow := &stubEpicBookCraftUnitOfWork{store: store}

	cases := []struct {
		name        string
		uow         dnfrepo.EpicBookCraftUnitOfWork
		accountID   string
		characterID string
		equipmentID uint32
		amount      uint32
	}{
		{"nil unit of work", nil, "account-1", "7", 101020256, 1},
		{"blank account", uow, "  ", "7", 101020256, 1},
		{"blank character", uow, "account-1", " ", 101020256, 1},
		{"zero equipment", uow, "account-1", "7", 0, 1},
		{"zero amount", uow, "account-1", "7", 101020256, 0},
	}
	for _, tc := range cases {
		if _, err := AddFragment(context.Background(), tc.uow, tc.accountID, tc.characterID, tc.equipmentID, tc.amount, testTime()); !errors.Is(err, ErrLedgerUnavailable) {
			t.Fatalf("%s: AddFragment() error = %v, want ErrLedgerUnavailable", tc.name, err)
		}
	}
	if store.increments != 0 {
		t.Fatalf("invalid calls reached the store: increments=%d", store.increments)
	}
}

type stubEpicBookCraftUnitOfWork struct {
	store dnfrepo.EpicBookRepository
}

func (u *stubEpicBookCraftUnitOfWork) WithinEpicBookCraft(
	_ context.Context,
	_, _ string,
	apply func(dnfrepo.EpicBookRepository, dnfrepo.AccountInventoryRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	return apply(u.store, nil, nil, nil)
}

// recordingIncrementStore 实现 EpicBookBalanceIncrementer，用来证明调用方确实
// 走了增量分支、且完全绕开了整本账的 Load/Save。
type recordingIncrementStore struct {
	balances   map[uint32]uint32
	increments int
	loads      int
	saves      int
}

func (s *recordingIncrementStore) Load(context.Context, string) (dnfrepo.EpicBookRecord, bool, error) {
	s.loads++
	return dnfrepo.EpicBookRecord{AccountID: "account-1", Balances: s.balances}, true, nil
}

func (s *recordingIncrementStore) Save(context.Context, dnfrepo.EpicBookRecord) error {
	s.saves++
	return nil
}

func (s *recordingIncrementStore) AddBalance(_ context.Context, _ string, equipmentID uint32, amount uint32, _ time.Time) (uint32, error) {
	s.increments++
	s.balances[equipmentID] += amount
	return s.balances[equipmentID], nil
}

// rewriteOnlyStore 故意不实现 EpicBookBalanceIncrementer，代表内存/SQLite 后端。
type rewriteOnlyStore struct {
	balances  map[uint32]uint32
	saves     int
	updatedAt time.Time
}

func (s *rewriteOnlyStore) Load(context.Context, string) (dnfrepo.EpicBookRecord, bool, error) {
	cloned := make(map[uint32]uint32, len(s.balances))
	for equipmentID, balance := range s.balances {
		cloned[equipmentID] = balance
	}
	return dnfrepo.EpicBookRecord{AccountID: "account-1", Balances: cloned}, true, nil
}

func (s *rewriteOnlyStore) Save(_ context.Context, record dnfrepo.EpicBookRecord) error {
	s.saves++
	s.balances = record.Balances
	s.updatedAt = record.UpdatedAt
	return nil
}
