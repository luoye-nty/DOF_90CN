package dnfbridge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 现场（2026-09-20 08:32:02Z，raid 1507329，两人队打 219 火山）：
// boss 死、op115 发出、参与行与队友 actor 都发了、火山影片符号那一包也发了，
// 然后这条连接 **71.2 秒一个字都没有**，直到客户端 socket 断掉；
// event=completed、phase-clear-registration、结算入口 op31 全部没有。
//
// 形状就是这两个测试钉住的东西：阶段收尾（publishRuntimeRaidObjectiveProgress →
// finishRuntimeRaidPhase → forceReturnRuntimeRaidPhaseGroups，以及火山分支
// afterRuntimeRaidObjectiveAdvance → forceReturnRuntimeRaidGroupsMatching）
// 曾经跑在**通关那个人的 packet 线程**上、握着**他自己的 session.dungeon.mu**，
// 而去把队友送回城时对**队友的 session** 做的是阻塞 Lock()
// （dungeon_return_town.go:1000）。TryLock 只保护了「探测」那一步：探测时队友的锁是空的，
// 于是队友被选为送回目标；等到真正回城时队友自己的 packet 线程已经进了那把锁，
// 两条线程各握一把、又都要对方的 → 永久互等，结算与过场再也不会发生。
//
// 单人：被送回的名单里只剩自己，TryLock 必然失败 → 跳过 → 不可能卡（现场 solo 打 219
// 同一位置齐全，含 op31）。
// 两人：两个人都还在同一张图里时必然踩到 —— 所以「组队通关阶段最后一张图
// （擎天 A/B、火山）不结算」是同一个原因。
//
// 复现是确定性的，不依赖现网日志：夹具里两个成员各有一把真实 run；
// 「探测通过之后、真正回城之前」这段窗口用一次可阻塞的仓库读卡住，
// 队友的线程在这个窗口里进入自己的锁 —— 旧代码必定停在那里。

// blockedCharacterLoadRepository 把目标角色的一次仓库读改成可阻塞，用来把
// 「probed 通过之后、真正回城之前」这段窗口拉长到测试可控。
type blockedCharacterLoadRepository struct {
	dnfrepo.CharacterRepository
	blockedKey  string
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func (r *blockedCharacterLoadRepository) Load(
	ctx context.Context,
	key string,
) (dnfrepo.CharacterRecord, bool, error) {
	if key != r.blockedKey {
		return r.CharacterRepository.Load(ctx, key)
	}
	r.startedOnce.Do(func() { close(r.started) })
	<-r.release
	return r.CharacterRepository.Load(ctx, key)
}

// releaseLoads lets every blocked read through, so a deferred forced return can
// finish once the test has opened the window it was holding shut.
func (r *blockedCharacterLoadRepository) releaseLoads() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// blockCharacterLoadsAfterSetup swaps the fixture's character repository for one
// whose read of blockedCharacterID blocks until the test releases it. The fixture's
// own setup runs first, so only the forced-return path is affected.
func blockCharacterLoadsAfterSetup(
	t *testing.T,
	fixture runtimeRaidPhaseForceReturnFixture,
	blockedCharacterID uint16,
) *blockedCharacterLoadRepository {
	t.Helper()
	group, ok := fixture.service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	blocked := &blockedCharacterLoadRepository{
		CharacterRepository: group.Character,
		blockedKey:          fmt.Sprint(blockedCharacterID),
		started:             make(chan struct{}),
		release:             make(chan struct{}),
	}
	group.Character = blocked
	fixture.service.repositoryProvider = func() (dnfrepo.Group, bool) { return group, true }
	return blocked
}

// mateSessionLockInterleaving models the mate's own packet goroutine: it enters its
// session lock only when the test opens the window, so the completer's TryLock probe
// still sees a free lock and really does pick the mate as a forced-return target.
type mateSessionLockInterleaving struct {
	hold      chan struct{}
	holdOnce  sync.Once
	held      chan struct{}
	release   chan struct{}
	releaseDo sync.Once
	done      chan struct{}
}

func holdMateSessionLock(t *testing.T, session *gameSession) *mateSessionLockInterleaving {
	t.Helper()
	interleaving := &mateSessionLockInterleaving{
		hold:    make(chan struct{}),
		held:    make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(interleaving.done)
		<-interleaving.hold
		session.dungeon.mu.Lock()
		close(interleaving.held)
		<-interleaving.release
		session.dungeon.mu.Unlock()
	}()
	return interleaving
}

// enterLock opens the window: the mate's goroutine takes its own session lock now.
func (i *mateSessionLockInterleaving) enterLock() { i.holdOnce.Do(func() { close(i.hold) }) }

// releaseLock always unwinds, even when the window was never opened, so a failing
// assertion cannot leave a goroutine parked on the fixture.
func (i *mateSessionLockInterleaving) releaseLock() {
	i.holdOnce.Do(func() { close(i.hold) })
	i.releaseDo.Do(func() { close(i.release) })
	<-i.done
}

// TestRuntimeRaidPhaseCompletionNeverWaitsOnAMateSessionLock covers the
// phase-completing clear (擎天 A/B 打完最后一下，或任何把阶段打满的那一张图)
// with a mate still inside one of the phase's gates.
func TestRuntimeRaidPhaseCompletionNeverWaitsOnAMateSessionLock(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	// 通关的人自己也在图里：90CN 现场就是这样（两人同图，一个 owner 一个 joined）。
	fixture.enterGateRun(t, 99, 11, antonBlackFogSourceMapID)
	// 队友还在另一张一阶段图里 —— 阶段一完成的收尾会把他送回城。
	fixture.enterGateRun(t, 100, 12, antonBlackFogSourceMapID)

	blocked := blockCharacterLoadsAfterSetup(t, fixture, 100)
	mate, ok := fixture.service.onlineGameSession(100)
	if !ok {
		t.Fatal("mate session missing")
	}
	mateHeld := holdMateSessionLock(t, mate)
	defer mateHeld.releaseLock()

	done := make(chan error, 1)
	go func() {
		leader, online := fixture.service.onlineGameSession(99)
		if !online {
			done <- errRuntimeRaidNotFound
			return
		}
		// 通关那个人的 packet 线程：completeCurrentDungeonBossDieCheckLocked 就是
		// 在握着这把锁的情况下走到阶段收尾的。
		leader.dungeon.mu.Lock()
		defer leader.dungeon.mu.Unlock()
		done <- fixture.service.publishRuntimeRaidObjectiveProgress(
			fixture.raidKey, runtimeRaidDungeonPhaseOne)
	}()

	// 仓库读在「探测之后、回城之前」触发。它在通关调用返回之前就响，说明强制回城
	// 是内联跑的（旧形状）；此时让队友的线程进入自己的锁，就是现场的交错。
	inlineReturn := false
	select {
	case <-blocked.started:
		inlineReturn = true
		mateHeld.enterLock()
		select {
		case <-mateHeld.held:
		case <-time.After(3 * time.Second):
			t.Fatal("the mate never took its own session lock")
		}
	case <-time.After(2 * time.Second):
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("phase completion under the completing member's own lock: %v", err)
		}
	case <-time.After(3 * time.Second):
		fixture.drainScheduledPhaseTail(t)
		t.Fatal("阶段收尾停在了队友的 session 锁上：两个成员各自握着自己的锁时这是永久互等，" +
			"现象就是 08:32:02Z 那次 —— op115 之后 completed / 结算 / op31 全无。")
	}
	if inlineReturn {
		t.Fatal("阶段收尾仍然在通关者的调用栈里执行强制回城（队友的 session 锁在这个窗口里被占着，" +
			"这正是互等的成因）；收尾必须交给队列，不在 packet 线程的锁里跑。")
	}

	// 收尾必须仍然发生（只是不再跑在通关者的锁里）：放开仓库读与队友的锁，
	// 跑一次被推迟的收尾。
	blocked.releaseLoads()
	mateHeld.releaseLock()
	fixture.fireScheduledPhaseTail(t)
	if runtime := fixture.sessionRuntime(t, 100); runtime != nil {
		t.Fatalf("the mate was never returned to town: still inside %d", runtime.Dungeon.ID)
	}
	if runs := fixture.openRunCount(t); runs != 0 {
		t.Fatalf("the phase completed with %d gate runs still open", runs)
	}
	if status := fixture.lifecycle().Status; status != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("the phase movie did not open: status=%d", status)
	}
}

// TestRuntimeRaidVolcanoClearNeverWaitsOnAMateSessionLock covers 火山（219）那一支：
// 黑火山倒下要把 218/219/221..224 里的队伍全部送回城（86JP ClearBlackVolcanoAsync），
// 而 219 自己就在这份名单里 —— 现场「char22 完成、char1 还站在 219」正是它。
func TestRuntimeRaidVolcanoClearNeverWaitsOnAMateSessionLock(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	fixture.switchToPhaseTwo(t)
	fixture.setVolcanoOpen(t, true)
	// 通关的人在 219 里（它自己的 run），队友在 218 里。
	fixture.enterGateRun(t, 99, 21, antonBlackVolcanoMapID)
	fixture.enterGateRun(t, 100, 22, antonEnergyInterceptionMapID)

	blocked := blockCharacterLoadsAfterSetup(t, fixture, 100)
	mate, ok := fixture.service.onlineGameSession(100)
	if !ok {
		t.Fatal("mate session missing")
	}
	mateHeld := holdMateSessionLock(t, mate)
	defer mateHeld.releaseLock()

	done := make(chan error, 1)
	go func() {
		leader, online := fixture.service.onlineGameSession(99)
		if !online {
			done <- errRuntimeRaidNotFound
			return
		}
		leader.dungeon.mu.Lock()
		defer leader.dungeon.mu.Unlock()
		done <- fixture.service.afterRuntimeRaidObjectiveAdvance(
			fixture.raidKey, runtimeRaidDungeonPhaseTwo, antonBlackVolcanoMapID)
	}()

	inlineReturn := false
	select {
	case <-blocked.started:
		inlineReturn = true
		mateHeld.enterLock()
		select {
		case <-mateHeld.held:
		case <-time.After(3 * time.Second):
			t.Fatal("the mate never took its own session lock")
		}
	case <-time.After(2 * time.Second):
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("volcano clear under the completing member's own lock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("火山通关后的强制回城停在了队友的 session 锁上（现场 08:32:02Z）")
	}
	if inlineReturn {
		t.Fatal("火山通关仍然在通关者的调用栈里强制送回队友 —— 219 自己就在送回名单里，" +
			"这正是现场互等的成因。")
	}

	// 强制回城仍然必须发生：放开仓库读与队友的锁，跑一次被推迟的回城。
	blocked.releaseLoads()
	mateHeld.releaseLock()
	fixture.fireScheduledForcedReturns(t)
	if runtime := fixture.sessionRuntime(t, 100); runtime != nil {
		t.Fatalf("the mate was never returned to town: still inside %d", runtime.Dungeon.ID)
	}
}

// fireScheduledPhaseTail runs the deferred phase tail the way the gameplay timer
// goroutine would (the fake queue only records tasks).
func (f runtimeRaidPhaseForceReturnFixture) fireScheduledPhaseTail(t *testing.T) {
	t.Helper()
	name := fmt.Sprintf("raid-phase-complete:%d:%d", f.raidKey, runtimeRaidDungeonPhaseOne)
	queue := f.timerQueue(t)
	task := queue.taskByName(name)
	if task == nil {
		t.Fatalf("the phase tail was not scheduled as %q", name)
	}
	queue.fire(task, false)
}

// fireScheduledForcedReturns runs every deferred forced-return sweep.
func (f runtimeRaidPhaseForceReturnFixture) fireScheduledForcedReturns(t *testing.T) {
	t.Helper()
	queue := f.timerQueue(t)
	for _, task := range queue.tasksNamed("raid-phase-return-groups") {
		queue.fire(task, false)
	}
}

// drainScheduledPhaseTail is the failure-path cleanup: run whatever was queued so a
// failing assertion does not leave the fixture half-finished.
func (f runtimeRaidPhaseForceReturnFixture) drainScheduledPhaseTail(t *testing.T) {
	t.Helper()
	queue := f.timerQueue(t)
	for _, task := range queue.allTasks() {
		queue.fire(task, false)
	}
}

func (f runtimeRaidPhaseForceReturnFixture) timerQueue(t *testing.T) *fakeCurrentDungeonDeathTimerQueue {
	t.Helper()
	queue, ok := f.service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	if !ok {
		t.Fatalf("gameplay timer queue is %T", f.service.gameplayTimers)
	}
	return queue
}

func (q *fakeCurrentDungeonDeathTimerQueue) taskByName(name string) *fakeCurrentDungeonDeathTimerTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.active[name]
}

func (q *fakeCurrentDungeonDeathTimerQueue) tasksNamed(prefix string) []*fakeCurrentDungeonDeathTimerTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*fakeCurrentDungeonDeathTimerTask, 0, len(q.tasks))
	for _, task := range q.tasks {
		if len(task.name) >= len(prefix) && task.name[:len(prefix)] == prefix {
			out = append(out, task)
		}
	}
	return out
}

func (q *fakeCurrentDungeonDeathTimerQueue) allTasks() []*fakeCurrentDungeonDeathTimerTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*fakeCurrentDungeonDeathTimerTask(nil), q.tasks...)
}
