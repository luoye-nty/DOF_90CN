package dnfbridge

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
	"longheng.io/server/internal/platform/timequeue"
)

// 「开始/成功都有横幅，失败没有」——失败那一条的包**一个都不缺**，缺的是**顺序**。
//
// 真机 2026-10-02 00:00:36（raid 12058625，分片 20261002-001）逐条：
//
//	16:00:36.619  1453{6,0} / 585 / 597{0,0} / 587{6,1} / 601{ResultType 1}  ← 门点亮
//	16:00:36.621  game-initial-town-route-armed
//	16:00:36.666  game-dungeon-full-town-route-send source=fiendwar_failed_waiting_area
//	16:00:36.717  同上（另一个成员）
//	16:00:36.927  game-dungeon-full-town-route-committed
//	16:00:41.620  587{0} 释放（+591/633/753 的收尾）
//
// 也就是说横幅门点亮之后 **48 毫秒**客户端就收到了回待机区的场景切换，5 秒的"演出
// 时间"全花在一个已经被换掉的场景上。客户端那个 30210 横幅是逐帧判断
// `raidObj+0x28 == 6 && raidObj+0x2C == 1` 的（FUN_01f67bc0，
// tools/_fw3_fixes_spec.md:240-256、:313-318），所以修法是在待机区落地之后
// **把同一对字节再点一次**：电平判断重复发同值只重新点亮，不改任何状态。
//
// 本用例钉住三件事：
//
//	① 上弦两件事：先补灯（+2s）、后释放（+5s），补灯必须是先臂上的那个；
//	② 补灯真的再发一次 587{6,1} 与 601{ResultType 1}（两个成员各一份）；
//	③ 释放之后再跑补灯是**空转** —— 攻坚队已经回到大厅，不许再点亮横幅。
func TestFiendWarFailureBannerIsRelitAfterTheWaitingRoomTeleport(t *testing.T) {
	f := newRuntimeRaidRewardTestFixture(t, true)
	s := f.service
	state := s.raids[f.raidKey]
	state.RaidMode, state.RouteOrRaidType = runtimeRaidOwnerFiendWar, 2
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	for i := range state.Members {
		state.Members[i].GroupIndex, state.Members[i].SlotOrder = 1, byte(i)
	}
	for i := range state.FrozenMembers {
		state.FrozenMembers[i].GroupIndex, state.FrozenMembers[i].SlotOrder = 1, byte(i)
	}
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		4962: {Phase: runtimeRaidDungeonPhaseOne, MapID: 4962, State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1},
	}
	queue := &fiendWarFailureScheduleQueue{now: f.now}
	s.gameplayTimers = queue

	failed, err := s.failRuntimeFiendWarRaid(state.RaidKey, f.now)
	if err != nil {
		t.Fatalf("fail the Fiend War run: %v", err)
	}
	if err := s.publishFiendWarFailurePresentation(failed); err != nil {
		t.Fatalf("publish failure presentation: %v", err)
	}

	// ① 先补灯、后释放。
	reassertIndex := queue.indexOf("fiendwar-failure-banner:")
	releaseIndex := queue.indexOf("fiendwar-failure-release:")
	if reassertIndex < 0 || releaseIndex < 0 {
		t.Fatalf("失败收尾只上弦了 %v，want 补灯 + 释放两条", queue.names())
	}
	if reassertIndex > releaseIndex {
		t.Fatalf("补灯排在释放之后（%v）：待机区落地时门已经被 587{0} 放掉了", queue.names())
	}
	if got := queue.delays()[reassertIndex]; got != runtimeRaidFiendWarFailureBannerReassert {
		t.Fatalf("补灯的延后 = %s, want %s", got, runtimeRaidFiendWarFailureBannerReassert)
	}
	if got := queue.delays()[releaseIndex]; got != runtimeRaidFiendWarFailureHold {
		t.Fatalf("释放的延后 = %s, want %s", got, runtimeRaidFiendWarFailureHold)
	}

	// ② 补灯真的再发一对。
	for _, peer := range []*gameSession{f.leader, f.member} {
		peer.conn.(*bufferConn).write.Reset()
	}
	queue.callbacks[reassertIndex](f.now.Add(runtimeRaidFiendWarFailureBannerReassert))
	for _, peer := range []*gameSession{f.leader, f.member} {
		raw := peer.conn.(*bufferConn).write.Bytes()
		stateByte, stateFound := fixtureLastRaidStateByte(t, raw)
		if !stateFound || stateByte != runtimeRaidFiendWarFailureState {
			t.Fatalf("char %d 补灯后没有 587{%d}（读到 %d, found=%t）：横幅门的 state 那一半没点亮",
				peer.selectedCharacterID, runtimeRaidFiendWarFailureState, stateByte, stateFound)
		}
		resultType, resultFound := fixtureLastRaidResultType(t, raw)
		if !resultFound || resultType != runtimeRaidFiendWarFailResultType {
			t.Fatalf("char %d 补灯后没有 601{ResultType %d}（读到 %d, found=%t）："+
				"raidObj+0x2C 只有 601 能写，少了它就只剩一半的门",
				peer.selectedCharacterID, runtimeRaidFiendWarFailResultType, resultType, resultFound)
		}
	}

	// ③ 释放之后补灯空转。
	for _, peer := range []*gameSession{f.leader, f.member} {
		peer.conn.(*bufferConn).write.Reset()
	}
	queue.callbacks[releaseIndex](f.now.Add(runtimeRaidFiendWarFailureHold))
	after := make([][]byte, 2)
	for i, peer := range []*gameSession{f.leader, f.member} {
		after[i] = append([]byte(nil), peer.conn.(*bufferConn).write.Bytes()...)
		peer.conn.(*bufferConn).write.Reset()
	}
	queue.callbacks[reassertIndex](f.now.Add(runtimeRaidFiendWarFailureHold + time.Second))
	for i, peer := range []*gameSession{f.leader, f.member} {
		if wrote := peer.conn.(*bufferConn).write.Bytes(); len(wrote) != 0 {
			t.Fatalf("释放之后补灯还在写信箱（char %d, % x）；释放前那一段是 % x",
				peer.selectedCharacterID, wrote, after[i])
		}
		if current := s.raids[state.RaidKey]; current == nil || current.Lifecycle.Status == runtimeRaidLifecycleFailed {
			t.Fatalf("释放没有把攻坚队带回大厅：status=%v", current)
		}
	}
}

// fiendWarFailureScheduleQueue 记录**全部**上弦项（共享的
// runtimeRaidPhaseTestQueue 只留最后一次，补灯那一条会被释放覆盖掉）。
type fiendWarFailureScheduleQueue struct {
	now       time.Time
	names_    []string
	delays_   []time.Duration
	callbacks []timequeue.Callback
}

func (q *fiendWarFailureScheduleQueue) Now() time.Time { return q.now }

func (q *fiendWarFailureScheduleQueue) ScheduleAfter(
	name string,
	delay time.Duration,
	callback timequeue.Callback,
) error {
	q.names_ = append(q.names_, name)
	q.delays_ = append(q.delays_, delay)
	q.callbacks = append(q.callbacks, callback)
	return nil
}

func (q *fiendWarFailureScheduleQueue) RegisterDaily(string, int, int, timequeue.Callback) error {
	return nil
}

func (q *fiendWarFailureScheduleQueue) Cancel(string) bool { return false }

func (q *fiendWarFailureScheduleQueue) Start(context.Context) {}

func (q *fiendWarFailureScheduleQueue) names() []string { return q.names_ }

func (q *fiendWarFailureScheduleQueue) delays() []time.Duration { return q.delays_ }

func (q *fiendWarFailureScheduleQueue) indexOf(prefix string) int {
	for index, name := range q.names_ {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			return index
		}
	}
	return -1
}

// fixtureLastRaidStateByte 读最后一条 587 的 state 字节。
func fixtureLastRaidStateByte(t *testing.T, raw []byte) (byte, bool) {
	t.Helper()
	value, found := byte(0), false
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		if packet.Header.MsgID != raid.RaidStateMsgID || len(packet.Body) < 1 {
			continue
		}
		value, found = packet.Body[0], true
	}
	return value, found
}

// fixtureLastRaidResultType 读最后一条 601 的 ResultType（首字节）。
func fixtureLastRaidResultType(t *testing.T, raw []byte) (byte, bool) {
	t.Helper()
	value, found := byte(0), false
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		if packet.Header.MsgID != raid.RaidResultMsgID || len(packet.Body) != 10 {
			continue
		}
		value, found = packet.Body[0], true
	}
	return value, found
}
