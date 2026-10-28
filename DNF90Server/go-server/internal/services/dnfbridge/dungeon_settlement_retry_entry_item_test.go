package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// requiredEntryItemTableForTest 造一张只声明 `[required item]` 的副本表。生产里预检读的
// 就是这张表（入场流程也用同一张表建运行时），所以这里从表注入才能复现真实形状。
func requiredEntryItemTableForTest(t *testing.T, dungeonID int64, itemID int64, count int) *worldmap.Table {
	t.Helper()
	table, err := worldmap.LoadSource(context.Background(), bridgePVFSource{
		worldmap.DefaultWorldMapList: "39 `metrocenter.wdm`\n",
		"worldmap/metrocenter.wdm":   "[dungeon]\n" + strconv.FormatInt(dungeonID, 10) + " -1\n[/dungeon]\n",
		worldmap.DefaultDungeonList:  strconv.FormatInt(dungeonID, 10) + " `test/required_entry_item.dgn`\n",
		"dungeon/test/required_entry_item.dgn": "[required item]\n" +
			strconv.FormatInt(itemID, 10) + " " + strconv.Itoa(count) + " 0\n[/required item]\n",
	}, worldmap.Options{SkipMaps: true})
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// 2026-09-14 22:22 活体：材料不足时点"再次挑战"，服务端先退役了已完成的运行时，然后在真正入场
// 时才发现入场道具缺失（`dungeon entry item is missing: character=1 item=10158964 need=2
// available=0`）。客户端停在结算页，而服务端已经没有可回放的运行时 —— 再点"返回城镇"毫无反应，
// 整局卡死。
//
// 当时的回退是"发现材料不足就保留结算页、只回失败回执与提示"，但那条同样不对：
// 2026-09-24 03:25-03:31 春日影(9)/魔界裂缝(316) 三次被拒、三次靠重登恢复；03:31:36 那次在结算页
// 干等 10 秒被结算页计时器强制回城；03:30:03 那次卡住期间的点击被判成放弃副本，白吃力弱 9。
//
// 而且**失败回执本身不能在结算页上预先发**：2026-09-24 12:44:46 (char 1/dungeon 316) 先发
// op16 失败回执再发 op5/op27，客户端 70ms 后就自己发了 bodyless op132(返回城镇) —— 玩家看到的
// 是"直接回城、完全没在选择副本界面停留"。
//
// 现在的形状（= 2026-09-20 13:52:49 浅笑(13)/dungeon 310 疲劳不足那条线上验证过的走法）：
// 仍然是"先检测材料、后退役"（保住 09-14 的防线），但检测出不足之后只退役已完成的运行、把玩家
// 放回"选择副本"界面（op5+op27），**不预先发失败回执**。客户端落到选择副本界面后自己发起入场
// （自动或玩家点），那一次才吃 op16 失败回执(reason 17)，弹出官方那条
// "因[%s]缺少入场材料，无法进入『%s』。"。
func TestCompletedRetryWithoutEntryItemsRetiresIntoTheSelector(t *testing.T) {
	service, runtime, session, connection, _ := prepareCompletedSettlementRuntime(t)
	// 走完结算门：再次挑战的前置是 completedDungeonSettlementExitReady。
	completeSettlementExitBarriersForTest(t, service, runtime, session)
	runtime.settlementClearRewardSent = true
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseResultShown)
	markCurrentDungeonCardRewardCommittedForTest(runtime)
	connection.write.Reset()

	// 预检读的是副本表；把表的副本换成"需要 2 个精炼的时空石"。
	service.worldMapMu.Lock()
	originalTable := service.worldMapTable
	service.worldMapTable = requiredEntryItemTableForTest(t, runtime.Dungeon.ID, 10158964, 2)
	service.worldMapMu.Unlock()

	retryErr := service.retryCurrentCompletedDungeon(session, runtime, "test_retry_missing_entry_items", false)
	service.worldMapMu.Lock()
	service.worldMapTable = originalTable
	service.worldMapMu.Unlock()
	if retryErr != nil {
		t.Fatal(retryErr)
	}

	// 退役了：玩家被放回选择副本界面，而不是留在结算页干等。
	if session.dungeon.runtime == runtime {
		t.Fatalf("材料不足的再次挑战没有退役已完成的运行时: %+v", session.dungeon.runtime)
	}
	session.townMu.Lock()
	selectorOriginBound := session.townSelectorOriginBound
	session.townMu.Unlock()
	if !selectorOriginBound {
		t.Fatal("退役之后没有留下选择副本界面的回城锚点")
	}

	rejected := connection.write.Bytes()
	if len(rejected) == 0 {
		t.Fatal("材料不足的再次挑战没有任何输出")
	}
	// 两件事都要有：
	//   1) op16 失败回执 reason 17 —— 客户端 sub_1CFC410 → sub_1CFA790 的 254 路 switch
	//      case 17 才弹官方那条 47707（18/26/39/42 在那个 switch 里是静默那一格）；
	//   2) 退役 + op5/op27 —— 玩家被放到"选择副本"界面，而不是留在结算页。
	rest := rejected
	foundFailureAck := false
	for len(rest) > 0 {
		packet, tail := splitGameServerUpperPacket(t, rest)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSelectDungeon) {
			foundFailureAck = true
			if packet.Header.Classification != 1 ||
				!bytes.Equal(packet.Body, []byte{0, currentSelectDungeonEntryItemMissingReason, 0}) {
				t.Fatalf("op16 回执不是 reason-17 失败: header=%+v body=%x", packet.Header, packet.Body)
			}
		}
		rest = tail
	}
	if !foundFailureAck {
		t.Fatalf("材料不足的再次挑战没有回 op16 失败回执（官方提示靠它）: %x", rejected)
	}
	// 客户端随后那发反射式 op132 必须被吞掉，换成选择副本界面；否则玩家会被直接送回城。
	session.dungeon.mu.Lock()
	refusedAt := session.dungeonEntryItemRefusedAt
	session.dungeon.mu.Unlock()
	if refusedAt.IsZero() {
		t.Fatal("入场被拒没有记下反射窗口，客户端那发 op132 会被当成玩家真的要回城")
	}
	connection.write.Reset()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("反射被吞掉之后应留在选择副本界面（无运行时）: %+v", session.dungeon.runtime)
	}
	// 吞掉之后要补一遍选择副本界面（而不是一声不响，那会重现 2026-09-18 的"返回城镇没反应"）。
	reprojected := connection.write.Bytes()
	foundSelectorContext := false
	rest = reprojected
	for len(rest) > 0 {
		packet, tail := splitGameServerUpperPacket(t, rest)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSceneTransitionMsgID {
			t.Fatalf("反射式 op132 不应触发城镇切换: %x", reprojected)
		}
		if packet.Header.MsgID == currentDungeonContextMsgID {
			foundSelectorContext = true
		}
		rest = tail
	}
	if !foundSelectorContext {
		t.Fatalf("反射被吞掉之后没有补选择副本界面: %x", reprojected)
	}

	// 退役之后客户端已经不在结算页，而是在"选择副本"界面上，所以它的"返回城镇"是
	// 无 body 的 op132（选择器返回路线），必须照旧给出一发场景切换 —— 这是 2026-09-14
	// 那次"整局卡死"的防线，也是 2026-09-18 深渊时空裂缝那条"被拒之后回不了城"的防线。
	connection.write.Reset()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() == 0 {
		t.Fatal("被拒的再次挑战之后，选择副本界面的返回城镇没有任何输出")
	}
	rest = connection.write.Bytes()
	foundTransition := false
	for len(rest) > 0 {
		packet, tail := splitGameServerUpperPacket(t, rest)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSceneTransitionMsgID {
			foundTransition = true
		}
		rest = tail
	}
	if !foundTransition {
		t.Fatalf("被拒的再次挑战之后回不了城: %x", connection.write.Bytes())
	}
}

// 预检与实际扣除共用 selectCurrentDungeonEntryItems，两者不可能再对"付不付得起"给出不同
// 答案：这正是活体那次"预检放行、扣除失败"的成因。
func TestSelectCurrentDungeonEntryItemsAgreesWithTheConsumeContract(t *testing.T) {
	plan := currentDungeonEntryAdmissionPlan{
		Requirements: []worldmap.TicketItem{{ItemID: 10158964, Count: 2}},
		Source:       "dungeon_required_item",
	}
	_, _, err := selectCurrentDungeonEntryItems(map[string]dnfrepo.ItemStack{
		"0:4": {ItemID: 10158964, Count: 1},
	}, plan)
	var shortage currentDungeonEntryItemShortage
	if !errors.As(err, &shortage) ||
		shortage.ItemID != 10158964 || shortage.Required != 2 || shortage.Available != 1 {
		t.Fatalf("缺料没有报出短缺: %+v err=%v", shortage, err)
	}

	// 票券优先于正式材料，和 PVF 的说明一致（"同时持有时优先消耗通行证"）。
	freePlan := currentDungeonEntryAdmissionPlan{
		FreePasses:   []worldmap.TicketItem{{ItemID: 490006577, Count: 1}},
		Requirements: []worldmap.TicketItem{{ItemID: 10158964, Count: 2}},
		Source:       "worldmap_particularhell_freepass_or_dungeon_required_item",
	}
	consumed, usedFreePass, err := selectCurrentDungeonEntryItems(map[string]dnfrepo.ItemStack{
		"0:129": {ItemID: 490006577, Count: 3},
		"0:136": {ItemID: 10158964, Count: 2},
	}, freePlan)
	if err != nil || !usedFreePass || len(consumed) != 1 || consumed[0].ItemID != 490006577 {
		t.Fatalf("通行证没有被优先选择: consumed=%+v freepass=%t err=%v", consumed, usedFreePass, err)
	}

	// 没有票券时回落到正式材料，并且按需要的数量报告。
	consumed, usedFreePass, err = selectCurrentDungeonEntryItems(map[string]dnfrepo.ItemStack{
		"0:136": {ItemID: 10158964, Count: 5},
	}, freePlan)
	if err != nil || usedFreePass || len(consumed) != 1 || consumed[0].ItemID != 10158964 || consumed[0].Count != 2 {
		t.Fatalf("回落材料不正确: consumed=%+v freepass=%t err=%v", consumed, usedFreePass, err)
	}
}
