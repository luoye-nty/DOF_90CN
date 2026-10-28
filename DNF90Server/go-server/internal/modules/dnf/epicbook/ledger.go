package epicbook

import (
	"context"
	"errors"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrLedgerUnavailable = errors.New("dnf epic book ledger is unavailable")
	// ErrBalanceOverflow 表示余额已到 uint32 上限、本次增加被整体拒绝。
	//
	// 余额上限的实际判定，以及"单行增量"与"整本账重写"两条路径的分流，都在
	// repository 层（repository.AddEpicBookBalance）。这里保留包内旧名字作为
	// 别名：它与 repository.ErrBalanceOverflow 是同一个值，调用方与该值的
	// errors.Is 比较行为不变。
	ErrBalanceOverflow = dnfrepo.ErrBalanceOverflow
)

// AddFragment 给账号的某个史诗碎片余额加量，并返回写入后的余额。
//
// 落库细节（走单行 upsert 还是整本账重写）由 repository 决定：MySQL 后端用
// 单行 upsert，避免同一账号并发拾取时在 REPEATABLE READ 的间隙锁上形成死锁；
// 内存/SQLite 后端没有该锁环，继续走整本账重写。两条路径结果语义一致。
func AddFragment(
	ctx context.Context,
	uow dnfrepo.EpicBookCraftUnitOfWork,
	accountID string,
	characterID string,
	equipmentID uint32,
	amount uint32,
	now time.Time,
) (uint32, error) {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if uow == nil || accountID == "" || characterID == "" || equipmentID == 0 || amount == 0 {
		return 0, ErrLedgerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var balance uint32
	err := uow.WithinEpicBookCraft(ctx, accountID, characterID, func(epicBooks dnfrepo.EpicBookRepository, _ dnfrepo.AccountInventoryRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		var addErr error
		balance, addErr = dnfrepo.AddEpicBookBalance(ctx, epicBooks, accountID, equipmentID, amount, now)
		return addErr
	})
	// 与原实现一致：出错时也把回调里已经算出的余额一并返回，调用方按 error
	// 判定成败，不会读这个值。
	return balance, err
}
