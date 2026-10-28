package dnfbridge

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// partyFanoutBudget 是「一次多人扇出」的总预算。
//
// 扇出跑在发起者（通常是队长）【唯一】的 event loop 上，所以这个值必须同时满足两件事：
//
//   - 足够长，让每个成员真正做完。实测 party-dungeon-loading-release 单成员最长
//     5.95s（2026-09-25 00:27 慢事件日志），改前那个 per-member 的
//     createWriteTimeout(3s) 根本不够，于是发起者在 3s 处超时、中止后续流程。
//   - 有界。不能无限等：客户端约 20s 收不到响应就判「网络连接中断」。
//
// 10s 给实测最坏值留了余量，又远低于客户端超时阈值。
const partyFanoutBudget = 10 * time.Second

// sessionFanoutTarget 是一次扇出里的单个目标会话。
//
// characterID 允许 run 在执行时（而不是收集时）用
// onlineGameSessionCharacterSnapshot 重新校验绑定，避免拿一个在等待期间
// 已经过期的快照去操作会话。
type sessionFanoutTarget struct {
	label       string
	characterID uint16
	session     *gameSession
}

// fanOutToGameSessions 并发地把同一段工作投递给多个会话。
//
// 改前的写法是 for 循环里给每个成员现开一个 createWriteTimeout 再同步等：
// N 个成员就是 3s × (N-1) 的【串行累加】，而这段代码跑在队长的 event loop 上，
// 于是队长的循环被卡住数秒。2026-09-24 23:24 与 2026-09-25 00:27 两次多人进副本
// 集体掉线，日志里的 game-stream-packet 慢事件就是卡在这里。
//
// 并发执行把总耗时从 Σ 变成 max，同时整次扇出共用一个总预算。每个目标会话仍然
// 在【它自己的】event loop 上执行，所以这里不引入跨会话的并发数据访问：与逐个
// 执行的唯一差别是它们在时间上重叠，而不是在数据上重叠。
//
// 错误按声明顺序取第一个返回，保持调用方「出错即中止后续流程」的原有语义；
// 区别只在于出错时其余成员已经各自尝试过，不会像改前那样被整段跳过。
func (s *Service) fanOutToGameSessions(
	ctx context.Context,
	targets []sessionFanoutTarget,
	source string,
	run func(target sessionFanoutTarget) error,
) error {
	if s == nil || run == nil || len(targets) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fanoutCtx, cancel := context.WithTimeout(ctx, partyFanoutBudget)
	defer cancel()

	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		target := targets[i]
		if target.session == nil {
			continue
		}
		wg.Add(1)
		go func(index int, target sessionFanoutTarget) {
			defer wg.Done()
			errs[index] = s.callGameSession(fanoutCtx, target.session, source, func() error {
				return run(target)
			})
		}(i, target)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("%s: member %s: %w", source, targets[i].label, err)
		}
	}
	return nil
}

// fanOutToGameSessionsAsync 是 fanOutToGameSessions 的**不等待**版本。
//
// 为什么需要它
//
//	同步版本等的是「别的会话的 event loop」：每个目标的工作跑在它自己的 loop 上，
//	callGameSession 会一直把调用方停在原地。多人同时进图时，每个人的 loop 都停在
//	同一个等待里，谁都推进不了，只能等共用预算（partyFanoutBudget，10s）到点。
//	2026-09-28 用 20 客户端夹具实测：发起者的 loop 被停住 10.0004 秒，那 10 秒里
//	它一个包都处理不了（repro_20raid_test.go，TestRepro20RaidSyncFanoutParksInitiator）。
//
// 它保证了什么（同步版本被依赖的那些性质）
//
//  1. 每个目标的工作**仍然跑在该目标自己的 loop 上**——单归属规则不变，
//     这是"投递方式"的改变，不是并发模型的改变。
//  2. `then` 只在**所有目标都完成之后**才被调用，因此"必须先做完的人先做、
//     必须最后做的人最后做"这类线序契约由**构造**保证，而不是靠计时。
//     `then` 里只允许投递，不允许直接改别的会话的状态。
//  3. 没有任何 loop 等待另一个 loop：调用方立刻返回。
//  4. 失败仍然看得见：每个目标的错误都单独记录，`then` 照样执行 ——
//     一个坏成员绝不能拖住全队。
//
// 恰有一次调用 `then`：最后一个完成的目标、预算到点、或投递失败。
func (s *Service) fanOutToGameSessionsAsync(
	ctx context.Context,
	targets []sessionFanoutTarget,
	source string,
	run func(target sessionFanoutTarget) error,
	then func(),
) {
	if s == nil {
		if then != nil {
			then()
		}
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			if then != nil {
				then()
			}
		})
	}
	if run == nil || len(targets) == 0 {
		finish()
		return
	}

	// 只要有一个目标会话真的拥有 event loop，"等别人的 loop"才可能存在 —— 也正是
	// 必须避免的那件事。反过来，如果一个都没有（测试夹具、或还没 startGameSessionEvents
	// 的会话），等待是廉价且确定的，就保持同步语义：调用方返回时全部已完成。
	//
	// 这条分界不是为了让测试通过，而是"死锁的成立条件"：卡住的前提是有另一个 loop
	// 正在停在自己的等待里；没有 loop 就没有这个前提。
	hasEventLoop := false
	for i := range targets {
		if session := targets[i].session; session != nil &&
			session.events != nil && session.events.loop != nil {
			hasEventLoop = true
			break
		}
	}
	if !hasEventLoop {
		var wg sync.WaitGroup
		for i := range targets {
			target := targets[i]
			if target.session == nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if runErr := run(target); runErr != nil {
					s.logWarn("dnf session fanout target failed",
						"source", source, "member", target.label, "error", runErr)
				}
			}()
		}
		wg.Wait()
		finish()
		return
	}

	var pending atomic.Int64
	pending.Store(int64(len(targets)))
	remaining := func() {
		if pending.Add(-1) == 0 {
			finish()
		}
	}

	// 预算是保险丝，不是机制：只有在某个目标始终不完成（loop 已销毁）时才用得上。
	budget := time.AfterFunc(partyFanoutBudget, func() {
		s.logWarn("dnf session fanout budget expired",
			"source", source,
			"targets", len(targets),
			"pending", pending.Load())
		finish()
	})
	defer budget.Stop()

	for i := range targets {
		target := targets[i]
		if target.session == nil {
			remaining()
			continue
		}
		work := func() error {
			defer remaining()
			if runErr := run(target); runErr != nil {
				// 逐目标可见的失败（等于参照实现的 RAID_NOTIFY_FAILED），
				// 且绝不允许它中止其他成员。
				s.logWarn("dnf session fanout target failed",
					"source", source, "member", target.label, "error", runErr)
				return runErr
			}
			return nil
		}
		// 没有事件循环的会话（测试夹具、或尚未 startGameSessionEvents 的会话）没有
		// 邮箱可用；这种情况下必须**每个目标一个 goroutine**，否则这些目标会串行
		// 执行 —— 这正是 stress_dungeon_entry_test.go 断言的那条性质
		//（"同一次扇出的两个 follower 必须时间上重叠"）。
		if target.session.events == nil || target.session.events.loop == nil {
			go func() { _ = work() }()
			continue
		}
		err := s.postGameSessionCharacterEvent(
			target.session,
			source,
			target.characterID,
			target.session.characterGeneration,
			work,
		)
		if err != nil {
			s.logWarn("dnf session fanout submit failed",
				"source", source, "member", target.label, "error", err)
			remaining()
		}
	}
}
