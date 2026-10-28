package dnfbridge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 攻坚失败返还入场次数。服主口径（2026-10-03）：
//
//	「卢克和安徒恩就是攻坚倒计时结束 超时空在此基础上则多加个战斗失败
//	 中途放弃不返还任何东西」
//
// 这条测试走的是安徒恩最常见的形态：真开团（所以三条次数与入场材料都是**真的**
// 被扣掉的，不是夹具假装的），然后倒计时到点 → 发失败演出 → 返还，最后核对次数与
// 材料都回到开团前。返还之后必须能重放而不多发一份：三个调用点全都在定时器回调
// 里，回调重复跑是设计上允许的。
func TestRuntimeRaidFailureRefundsAdmissionCountersAndEntryTicket(t *testing.T) {
	now := time.Now().UTC()
	service, repositories, catalog, session := newRuntimeRaidAdmissionCounterTest(t, 2, 1001)
	// 入场材料要真的能发回背包，所以这条用例必须有能解析 10096296 的道具目录；
	// 夹具本身不建目录（开团只扣不发，用不到）。
	service.pvfItemCatalog = newRuntimeRaidEntryRefundItemCatalog(t)

	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatal(err)
	}
	// 开团成功：三条次数各扣 1，材料券从 2 掉到 1。
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", currentRaidAntonDailyPolicyKey, 1)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.weekly", 1)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.character.weekly", 1)
	if got := runtimeRaidEntryTicketTotal(t, repositories, "1001"); got != 1 {
		t.Fatalf("开团后入场材料 = %d, want 1", got)
	}

	state := runtimeRaidFailureStateForTest(t, service, now)
	failed, err := service.expireRuntimeRaidPhase(state.RaidKey, runtimeRaidDungeonPhaseOne, now)
	if err != nil {
		t.Fatalf("倒计时到点：%v", err)
	}
	// 与 scheduleRuntimeRaidPhaseExpiry 的回调同序：先演出，再返还。
	if err := service.publishFailedRuntimeRaid(failed); err != nil {
		t.Fatalf("发失败演出：%v", err)
	}
	if err := service.refundRuntimeRaidEntryOnFailure(failed, now); err != nil {
		t.Fatalf("攻坚失败返还：%v", err)
	}

	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", currentRaidAntonDailyPolicyKey, 0)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.weekly", 0)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.character.weekly", 0)
	if got := runtimeRaidEntryTicketTotal(t, repositories, "1001"); got != 2 {
		t.Fatalf("返还后入场材料 = %d, want 2（开团前是 2）", got)
	}

	// 重放：定时器回调可能再跑一次，次数不能再涨、材料不能再发。
	if err := service.refundRuntimeRaidEntryOnFailure(failed, now); err != nil {
		t.Fatalf("重放返还：%v", err)
	}
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", currentRaidAntonDailyPolicyKey, 0)
	if got := runtimeRaidEntryTicketTotal(t, repositories, "1001"); got != 2 {
		t.Fatalf("重放后入场材料 = %d, want 2 —— 返还重复发了一份", got)
	}
}

// 中途放弃不返还任何东西：这是服主口径的另一半，也是这套返还最容易被动摇的地方
// （把返还挂到 abandonRuntimeRaid 上只需要一行）。
//
// 断言的是**扣掉的还在**：放弃之后三条次数仍是 1、材料券仍是 1。谁要是把返还接进
// 放弃路径，这条会立刻红。
func TestRuntimeRaidAbandonKeepsTheEntryCharge(t *testing.T) {
	now := time.Now().UTC()
	service, repositories, catalog, session := newRuntimeRaidAdmissionCounterTest(t, 2, 1001)
	service.pvfItemCatalog = newRuntimeRaidEntryRefundItemCatalog(t)

	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatal(err)
	}
	state, ok := service.runtimeRaidForMember(1001)
	if !ok {
		t.Fatal("开团后找不到攻坚队")
	}
	abandoned, err := service.abandonRuntimeRaid(state.RaidKey, 1001, now)
	if err != nil {
		t.Fatalf("中途放弃：%v", err)
	}
	if abandoned.Lifecycle.Status != runtimeRaidLifecycleAbandoned {
		t.Fatalf("放弃后 status=%d, want abandoned", abandoned.Lifecycle.Status)
	}
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", currentRaidAntonDailyPolicyKey, 1)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.character.weekly", 1)
	if got := runtimeRaidEntryTicketTotal(t, repositories, "1001"); got != 1 {
		t.Fatalf("放弃后入场材料 = %d, want 1（放弃不返还）", got)
	}
}

// 没开起来的团（没扣过费）返还必须是纯 no-op，而且一次持久化都不该发生：这条守卫
// 是"中途放弃不返还"的另一半保障，也是测试里大量合成 state 能安全调用它的原因。
func TestRuntimeRaidEntryRefundIgnoresAnUnpaidRaid(t *testing.T) {
	service, repositories, _, _ := newRuntimeRaidAdmissionCounterTest(t, 2, 1001)
	service.pvfItemCatalog = newRuntimeRaidEntryRefundItemCatalog(t)
	for name, state := range map[string]runtimeRaidState{
		"never started": {RaidKey: 7, StartAttempt: 1, ChargedMembers: []uint16{1001}},
		"started but unpaid": {
			RaidKey: 7, StartAttempt: 1, Started: true, ChargedMembers: []uint16{1001},
		},
		"paid but nobody charged": {
			RaidKey: 7, StartAttempt: 1, Started: true, EntryCostPaid: true,
		},
	} {
		if err := service.refundRuntimeRaidEntryOnFailure(state, time.Now().UTC()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := runtimeRaidEntryTicketTotal(t, repositories, "1001"); got != 2 {
		t.Fatalf("no-op 返还之后入场材料 = %d, want 2（没扣过就不该发）", got)
	}
}

// 超时空的入场材料由 PVF [RETRY] 驱动的 grantRuntimeFiendWarRetry 退回，而
// fiendwar.etc 把入场券和失败重试券写成同一个道具（490007844）。两条链路都跑就会
// 发两份，所以通用返还必须让开。这里钉的就是这条让开规则本身。
func TestRuntimeRaidEntryMaterialRefundYieldsToTheFiendWarRetryTicket(t *testing.T) {
	cases := []struct {
		name  string
		state runtimeRaidState
		want  bool
	}{
		{
			name: "超时空且 [RETRY] 开着：重试券链路独占",
			state: runtimeRaidState{
				RaidMode:       runtimeRaidOwnerFiendWar,
				FiendWarConfig: &raid.FiendWarConfig{Retry: true},
			},
			want: true,
		},
		{
			name: "超时空但 [RETRY] 关着：没人退，通用返还接手",
			state: runtimeRaidState{
				RaidMode:       runtimeRaidOwnerFiendWar,
				FiendWarConfig: &raid.FiendWarConfig{},
			},
			want: false,
		},
		{name: "安徒恩没有重试券这回事", state: runtimeRaidState{RaidMode: runtimeRaidOwnerAnton}, want: false},
		{
			name:  "超时空但没有 owner 配置",
			state: runtimeRaidState{RaidMode: runtimeRaidOwnerFiendWar},
			want:  false,
		},
	}
	for _, test := range cases {
		if got := runtimeRaidEntryMaterialRefundOwnedByRetry(test.state); got != test.want {
			t.Fatalf("%s: owned=%t want=%t", test.name, got, test.want)
		}
	}
}

// runtimeRaidFailureStateForTest 把一个真开起来的安徒恩团推成"倒计时刚好到点"：
// expireRuntimeRaidPhase 只认活跃阶段 + 已过期的 PhaseDeadline。开团时
// StartDelaySeconds=3600，团停在 Preparing，所以这里要把阶段状态与截止时间改掉，
// 与 raid_phase_timeout_flow_test.go 的做法一致。
func runtimeRaidFailureStateForTest(t *testing.T, service *Service, now time.Time) runtimeRaidState {
	t.Helper()
	state, ok := service.runtimeRaidForMember(1001)
	if !ok || !state.Started || !state.EntryCostPaid {
		t.Fatalf("开团后的状态不是「已开始且已付费」：found=%t state=%+v", ok, state)
	}
	service.raidMu.Lock()
	live := service.raids[state.RaidKey]
	if live == nil {
		service.raidMu.Unlock()
		t.Fatal("攻坚队行在开团后消失了")
	}
	live.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	live.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	live.Lifecycle.PhaseStartedAt = now.Add(-time.Hour)
	live.Lifecycle.PhaseDeadline = now.Add(-time.Minute)
	service.raidMu.Unlock()
	return state
}

// runtimeRaidEntryTicketTotal 数的是整个背包里入场材料的**总量**，不是某一个格子。
// 开团时扣的是夹具手写的 "0:0"，而发回背包走的是 PVF 落点（[material] 的 121..176），
// 两者未必是同一格。
func runtimeRaidEntryTicketTotal(t *testing.T, repositories dnfrepo.Group, characterID string) int64 {
	t.Helper()
	record, found, err := repositories.Inventory.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load inventory %s found=%t err=%v", characterID, found, err)
	}
	total := int64(0)
	for _, stack := range record.Slots {
		if stack.ItemID == int64(runtimeRaidEntryItemID) {
			total += stack.Count
		}
	}
	return total
}

// newRuntimeRaidEntryRefundItemCatalog 是只声明入场材料一个道具的最小道具目录：
// 返还走的是 ResolveItem -> 落点 -> 存档那条真链路，缺了目录会直接报错。
func newRuntimeRaidEntryRefundItemCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst": "",
		"stackable/stackable.lst": fmt.Sprintf(
			"%d `material/raid_entry_refund_ticket.stk`\n", runtimeRaidEntryItemID),
		"equipment/equipment.lst": "",
		"stackable/material/raid_entry_refund_ticket.stk": "[name] `Raid Entry Ticket`\n" +
			"[stackable type] `[material]`\n[stack limit] 999\n",
	})
	if err != nil {
		t.Fatalf("build raid entry refund item catalog: %v", err)
	}
	return catalog
}
