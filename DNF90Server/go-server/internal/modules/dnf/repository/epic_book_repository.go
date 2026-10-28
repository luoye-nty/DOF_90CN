package repository

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"longheng.io/server/internal/platform/db"
)

type EpicBookRepository interface {
	db.Store[EpicBookRecord]
}

var (
	// ErrBalanceOverflow 表示碎片余额已到 uint32 上限，本次增加被整体拒绝。
	ErrBalanceOverflow = errors.New("dnf epic book balance overflow")
	// ErrBalanceIncrementInvalid 表示增量入参不合法。调用方（epicbook.AddFragment）
	// 会先做同样的校验，这里只是让仓储原语自身不会被非法参数静默放过。
	ErrBalanceIncrementInvalid = errors.New("dnf epic book balance increment is invalid")
)

// EpicBookBalanceIncrementer 由"能在原行上自增余额"的存储实现。
//
// 为什么需要它：整账号账本的写模型是「锁读整本 + DELETE 全量 + 批量 INSERT」。
// 在 REPEATABLE READ 下，对空区间（例如新账号还没有这个碎片的账本行）做
// SELECT ... FOR UPDATE 拿到的是**间隙锁**，而间隙锁之间互相兼容、却与
// INSERT 需要的**插入意向锁**互斥。于是同一账号的 N 个并发写入会两两互等，
// 形成死锁环：实测 N=8 时恰好 1 个提交、7 个被回滚（(N-1)/N）。
// 单行 upsert 只争用 (account_id, equipment_id) 这一行的记录锁，实测 N=8
// 全部成功、余额也全部正确。
//
// 内存与 SQLite 后端不需要实现它：内存后端在进程内用互斥锁串行化整个事务，
// SQLite 是单写者，两者都不会出现上述锁环，继续走整记录重写即可。
//
// 与 Save 类调用方共存的前提：本增量路径不做区间锁读，因此它与
// Save（整本重写）之间**没有**互相串行化，靠的是下面这两条互斥来保证不丢更新——
//
//	1. Save 类事务先做 Load（lockReads=true → SELECT ... FOR UPDATE），
//	   它持有的间隙锁会挡住本路径的 INSERT，把本路径排到它提交之后；
//	2. 本路径先 INSERT 时持有新行的排他锁，会挡住 Save 类事务的 DELETE。
//
// 两种交错都不会形成等待环，但**前提是其余写史诗图鉴账本的路径（账号仓库运输、
// 助力礼包、邮件附件、图鉴合成）继续使用锁定读**。若将来把它们的 lockReads 关掉，
// 整本重写就会用陈旧快照覆盖掉本路径刚写入的碎片，需要重新评估。
type EpicBookBalanceIncrementer interface {
	// AddBalance 把 amount 加到 (accountID, equipmentID) 的余额上，
	// 返回写入后的余额。
	AddBalance(ctx context.Context, accountID string, equipmentID uint32, amount uint32, now time.Time) (uint32, error)
}

// AddEpicBookBalance 给单个史诗碎片余额加量。
//
// 存储支持增量时走增量路径，否则退回整账号账本重写。两条路径的**结果语义**
// 一致：余额累加、UpdatedAt、记录不存在时按空账本起算，余额超限时整体失败
// 且不落库。唯一差异是超限时的错误值——整本重写路径返回 ErrBalanceOverflow，
// MySQL 增量路径由 int unsigned 列在严格模式下报 1264（同样整体回滚），
// 调用方按 error 判定成败，两者可观察结果相同。
func AddEpicBookBalance(
	ctx context.Context,
	store EpicBookRepository,
	accountID string,
	equipmentID uint32,
	amount uint32,
	now time.Time,
) (uint32, error) {
	if incrementer, ok := store.(EpicBookBalanceIncrementer); ok {
		return incrementer.AddBalance(ctx, accountID, equipmentID, amount, now)
	}
	return rewriteEpicBookBalance(ctx, store, accountID, equipmentID, amount, now)
}

// rewriteEpicBookBalance 是原始的整账号账本重写路径：读出整本账、改一格、
// 整本写回。它既是不支持增量的后端的兜底实现，也是增量路径的语义基准。
func rewriteEpicBookBalance(
	ctx context.Context,
	store EpicBookRepository,
	accountID string,
	equipmentID uint32,
	amount uint32,
	now time.Time,
) (uint32, error) {
	if store == nil {
		return 0, ErrBalanceIncrementInvalid
	}
	record, found, err := store.Load(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if !found {
		record = EpicBookRecord{AccountID: accountID, Balances: make(map[uint32]uint32)}
	} else {
		record = CloneEpicBook(record)
		if record.Balances == nil {
			record.Balances = make(map[uint32]uint32)
		}
	}
	if record.Balances[equipmentID] > math.MaxUint32-amount {
		return 0, ErrBalanceOverflow
	}
	record.Balances[equipmentID] += amount
	record.UpdatedAt = now
	balance := record.Balances[equipmentID]
	if err := store.Save(ctx, record); err != nil {
		// 与原实现一致：Save 失败也把已算出的余额返回，调用方按 error 判定成败。
		return balance, err
	}
	return balance, nil
}

type EpicBookRecord struct {
	AccountID string            `json:"account_id"`
	Balances  map[uint32]uint32 `json:"balances,omitempty"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

func CloneEpicBook(record EpicBookRecord) EpicBookRecord {
	if len(record.Balances) == 0 {
		record.Balances = nil
		return record
	}
	balances := record.Balances
	record.Balances = make(map[uint32]uint32, len(balances))
	for equipmentID, balance := range balances {
		record.Balances[equipmentID] = balance
	}
	return record
}

func EpicBookKey(record EpicBookRecord) string {
	return strings.TrimSpace(record.AccountID)
}
