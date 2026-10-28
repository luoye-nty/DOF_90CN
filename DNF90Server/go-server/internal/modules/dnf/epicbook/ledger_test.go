package epicbook

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestAddFragmentPersistsAccountBalance(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "7"}); err != nil {
		t.Fatal(err)
	}
	balance, err := AddFragment(context.Background(), repositories.EpicBookCraft, "account-1", "7", 101020256, 3, testTime())
	if err != nil || balance != 3 {
		t.Fatalf("AddFragment() balance=%d error=%v", balance, err)
	}
	balance, err = AddFragment(context.Background(), repositories.EpicBookCraft, "account-1", "7", 101020256, 2, testTime())
	if err != nil || balance != 5 {
		t.Fatalf("AddFragment() second balance=%d error=%v", balance, err)
	}
	record, found, err := repositories.EpicBook.Load(context.Background(), "account-1")
	if err != nil || !found || record.Balances[101020256] != 5 {
		t.Fatalf("stored record=%+v found=%t error=%v", record, found, err)
	}
}

func TestAddFragmentRejectsOverflowWithoutMutation(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "7"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.EpicBook.Save(context.Background(), dnfrepo.EpicBookRecord{
		AccountID: "account-1",
		Balances:  map[uint32]uint32{101020256: math.MaxUint32},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddFragment(context.Background(), repositories.EpicBookCraft, "account-1", "7", 101020256, 1, testTime()); !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("AddFragment() error=%v, want ErrBalanceOverflow", err)
	}
	record, _, _ := repositories.EpicBook.Load(context.Background(), "account-1")
	if record.Balances[101020256] != math.MaxUint32 {
		t.Fatalf("balance mutated to %d", record.Balances[101020256])
	}
}

func testTime() (value time.Time) {
	return time.Unix(1_700_000_000, 0).UTC()
}
