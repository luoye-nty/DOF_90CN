package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
)

// op541 的包体：前导计数 + 每项 (u32 物品 id, u8 已用次数)。这是客户端处理器
// 0x0197B2B0 自己的读法（先读 1 字节计数并判零返回，再逐项读 id 与字节）。
func TestBuildCurrentItemDailyUseStateBodyEncodesCountedPairs(t *testing.T) {
	body, err := buildCurrentItemDailyUseStateBody([]alignedcmd.DailyUseState{
		{ItemID: 490702338, Used: 3},
		{ItemID: 0x01020304, Used: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		2,
		0x02, 0x86, 0x3F, 0x1D, 3, // 490702338 = 0x1D3F8602
		0x04, 0x03, 0x02, 0x01, 1,
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("body=%x want=%x", body, want)
	}

	if body, err := buildCurrentItemDailyUseStateBody(nil); err != nil || len(body) != 0 {
		t.Fatalf("空状态应无包体: body=%x err=%v", body, err)
	}
	if _, err := buildCurrentItemDailyUseStateBody([]alignedcmd.DailyUseState{{ItemID: 0, Used: 1}}); err == nil {
		t.Fatal("物品 id 为 0 必须被拒绝")
	}
}

// 服务端在回执之后发这个包；目前同时发 class 1 与 class 0 两份（class 1 已证明
// 惰性，作为探测留一轮），两份包体必须一致。
func TestAlignedDailyUseStateFollowsTheAckAsClassOne(t *testing.T) {
	service, _, session, connection, _ := prepareCompletedSettlementRuntime(t)
	connection.write.Reset()

	result := alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       "use_stackable_action",
		UpperResponses: []alignedcmd.UpperResponse{
			{MsgID: 515, Body: []byte{1, 99, 0, 0, 0, 5, 0, 0, 0, 1}, Classification: 1},
		},
		DailyUseStates: []alignedcmd.DailyUseState{{ItemID: 490702338, Used: 1}},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	stream := connection.write.Bytes()
	ack, rest := splitGameServerUpperPacket(t, stream)
	if ack.Header.MsgID != 515 {
		t.Fatalf("回执不是第一个包: header=%+v", ack.Header)
	}
	wantBody := []byte{1, 0x02, 0x86, 0x3F, 0x1D, 1}
	state, trailing := splitGameServerUpperPacket(t, rest)
	if state.Header.MsgID != currentItemDailyUseStateMsgID ||
		state.Header.Classification != currentItemDailyUseStateClass ||
		!bytes.Equal(state.Body, wantBody) || len(trailing) != 0 {
		t.Fatalf("每日次数包 header=%+v body=%x want class=%d body=%x trailing=%x",
			state.Header, state.Body, currentItemDailyUseStateClass, wantBody, trailing)
	}
}

// 没有计数器变化时不得多发这个包。
func TestAlignedDailyUseStateAbsentSendsNothing(t *testing.T) {
	service, _, session, connection, _ := prepareCompletedSettlementRuntime(t)
	connection.write.Reset()
	result := alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       "use_stackable_action",
		UpperResponses: []alignedcmd.UpperResponse{
			{MsgID: 515, Body: []byte{1, 99, 0, 0, 0, 5, 0, 0, 0, 1}, Classification: 1},
		},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != 515 || len(rest) != 0 {
		t.Fatalf("输出被污染: header=%+v rest=%x", packet.Header, rest)
	}
}

// 登录回放：直接把库里的"当日已用"投影给客户端（客户端自己用 上限−已用 显示剩余）。
func TestDailyUseStatesFromCountersReportsRemaining(t *testing.T) {
	stats := map[string]int64{
		dnffatigue.PotionUsageStatPrefix + "490702338": 3,
		dnffatigue.PotionUsageStatPrefix + "1001":      1,
		dnffatigue.PotionUsageStatPrefix + "bad":       9,
		dnffatigue.PotionUsageStatPrefix + "1002":      0,
		"fatigue": 90,
	}
	states := dailyUseStatesFromCounters(stats)
	if len(states) != 3 || states[0].ItemID != 1001 || states[0].Used != 1 ||
		states[1].ItemID != 1002 || states[1].Used != 0 ||
		states[2].ItemID != 490702338 || states[2].Used != 3 {
		t.Fatalf("states=%+v", states)
	}
	if got := dailyUseStatesFromCounters(nil); len(got) != 0 {
		t.Fatalf("空统计应无状态: %+v", got)
	}
}

// 通用 [total usable count] 计数器（任何消耗品，不再限于疲劳药）也必须回放，
// 并且与旧的疲劳药键共存时只算一次（取较大值），否则重登会把当天次数读错。
func TestDailyUseStatesFromCountersReadsGenericAndLegacyKeys(t *testing.T) {
	stats := map[string]int64{
		dnffatigue.ItemUsageStatPrefix + "490003149": 2,
		dnffatigue.ItemUsageStatPrefix + "10153501":  1,
		// Same item under both spellings must not be emitted twice.
		dnffatigue.PotionUsageStatPrefix + "490003149": 3,
		dnffatigue.PotionUsageStatPrefix + "490702338": 1,
		dnffatigue.ItemUsageStatPrefix + "bad":         4,
		dnffatigue.ItemUsageDayStatKey:                 20697,
	}
	states := dailyUseStatesFromCounters(stats)
	if len(states) != 3 {
		t.Fatalf("states=%+v", states)
	}
	if states[0].ItemID != 10153501 || states[0].Used != 1 {
		t.Fatalf("通用键未回放: %+v", states)
	}
	if states[1].ItemID != 490003149 || states[1].Used != 3 {
		t.Fatalf("新旧键共存时应取较大值且只报一次: %+v", states)
	}
	if states[2].ItemID != 490702338 || states[2].Used != 1 {
		t.Fatalf("旧疲劳药键仍须回放: %+v", states)
	}
}
