package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 开箱夹具：520 是声明了 `[total usable count] 1` 的礼盒（每天只能开 1 次），
// 521 是同样的盒子但**没有**这个字段（对照组，验证"没声明就不多做任何事"）。
//
// 现场问题：`[total usable count]` 的执行此前只挂在 op44/op515 上，礼盒走的是
// op160/op27，于是那 61 件 `[booster]` + 5 件 `[booster selection]` 的"每天可用
// 次数"完全不存在 —— 既不被拦，也从不发 op541（客户端那份计数只由 op541 写入，
// 所以它连本地都拦不住）。
func currentBoosterDailyUseTestSource() dungeonDropCatalogTestSource {
	source := currentBoosterTestSource()
	source["stackable/stackable.lst"] += "520 `cash/daily_limited_box.stk`\n521 `cash/plain_box.stk`\n"
	source["stackable/cash/daily_limited_box.stk"] = "[stackable type]\n`[booster]` 0\n[stack limit]\n5\n" +
		"[total usable count]\n1\n" +
		"[booster info]\n[etc]\n1 600 10000 1\n[/etc]\n[/booster info]\n"
	source["stackable/cash/plain_box.stk"] = "[stackable type]\n`[booster]` 0\n[stack limit]\n5\n" +
		"[booster info]\n[etc]\n1 600 10000 1\n[/etc]\n[/booster info]\n"
	return source
}

func mustCurrentBoosterDailyUseCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	catalog, err := newPVFDungeonDropCatalog(currentBoosterDailyUseTestSource())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// 开箱必须按礼盒**自己的** PVF 上限计数：第一次成功并下发 op541，第二次被拒、
// 源礼盒不被消耗、计数不再增长。
func TestCurrentBoosterChargesTheBoxesOwnPVFDailyLimit(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentBoosterDailyUseCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:80": {ItemID: 520, Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
	session := &gameSession{conn: connection, connID: "booster-daily-use", selectedCharacterID: 19}
	open := func() {
		t.Helper()
		connection.write.Reset()
		if err := service.handleGameCommand(
			session,
			byte(dnfenum.GameCmdCommand),
			uint16(dnfenum.CmdPacketUseBoosterItem),
			[]byte{80, 0},
		); err != nil {
			t.Fatal(err)
		}
	}

	// 第一次：开箱落地，源礼盒 3 -> 2，计数 1，并补一包 op541（class 0，字节是已用次数）。
	open()
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseBoosterItem) || len(ack.Body) == 0 || ack.Body[0] != 1 {
		t.Fatalf("第一次开箱没有成功: header=%+v body=%x", ack.Header, ack.Body)
	}
	rest = assertCurrentBoosterSourceRefresh(t, rest, 80, 520, 2)
	_, rest = splitCurrentSceneItemListPacket(t, rest)
	state, trailing := splitGameServerUpperPacket(t, rest)
	if state.Header.MsgID != currentItemDailyUseStateMsgID ||
		state.Header.Classification != currentItemDailyUseStateClass {
		t.Fatalf("开箱后没有下发 op541: header=%+v body=%x trailing=%x", state.Header, state.Body, trailing)
	}
	wantState := []byte{1, 0, 0, 0, 0, 1}
	binary.LittleEndian.PutUint32(wantState[1:5], 520)
	if !bytes.Equal(state.Body, wantState) {
		t.Fatalf("op541 body=%x want=%x", state.Body, wantState)
	}
	if len(trailing) != 0 {
		t.Fatalf("op541 之后不该再有包: %x", trailing)
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 520); got != 1 {
		t.Fatalf("开箱计数=%d want 1 stats=%+v", got, character.Stats)
	}

	// 第二次：到上限，必须拒绝，且**不消耗礼盒**、计数不变、不再发 op541。
	open()
	rejected, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if rejected.Header.MsgID != uint16(dnfenum.CmdPacketUseBoosterItem) ||
		!bytes.Equal(rejected.Body, []byte{0, 4}) {
		t.Fatalf("到上限后仍被放行: header=%+v body=%x", rejected.Header, rejected.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("被拒的开箱不该产生后续包: %x", rest)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if inventory.Slots["0:80"].Count != 2 {
		t.Fatalf("被拒的开箱消耗了礼盒: %+v", inventory.Slots)
	}
	character, _, _ = repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 520); got != 1 {
		t.Fatalf("被拒的开箱改动了计数: %d stats=%+v", got, character.Stats)
	}
}

// 对照组：PVF 没写 `[total usable count]` 的礼盒必须与挂载之前**逐字节相同** ——
// 不写计数、不追加 op541。既有用例
// TestCurrentBoosterRandomRouteConsumesSourceAndGrantsPVFReward 用无上限的 500 号
// 盒子断言了"list 包之后没有尾部包"，这条同时是本次改动"纯增量"的证明；这里再用
// 同夹具里的 521 明确钉一次。
func TestCurrentBoosterWithoutDeclaredLimitSendsNoDailyUseState(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentBoosterDailyUseCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:81": {ItemID: 521, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
	session := &gameSession{conn: connection, connID: "booster-no-daily-use", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketUseBoosterItem),
		[]byte{81, 0},
	); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketUseBoosterItem) {
		t.Fatalf("ack header=%+v", ack.Header)
	}
	rest = assertCurrentBoosterSourceRefresh(t, rest, 81, 521, 1)
	_, trailing := splitCurrentSceneItemListPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("没有声明上限的礼盒不得多发包: %x", trailing)
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 521); got != 0 {
		t.Fatalf("没有声明上限的礼盒不得写计数: %d stats=%+v", got, character.Stats)
	}
}

// op27 抽奖罐的契约同样生效（拦截与计数），但那条路径**刻意不发 op541**：
// 见 lottery_item.go 与 booster_item_test.go 里被钉死的"结果包之后不得再有包"
// 约束（真机崩溃教训）。这里把"服务端确实拦住了"钉下来，避免以后误以为罐子没管。
func TestCurrentLotteryJarEnforcesDeclaredDailyLimitWithoutExtraPacket(t *testing.T) {
	ctx := context.Background()
	source := currentBoosterTestSource()
	source["stackable/stackable.lst"] += "522 `cash/limited_jar.stk`\n"
	source["stackable/cash/limited_jar.stk"] = "[stackable type]\n`[upgradable legacy]` 0\n[stack limit]\n100\n" +
		"[total usable count]\n1\n[int data]\n600 10000 3\n[/int data]\n"
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:114": {ItemID: 522, Count: 2},
			"0:121": {ItemID: 600, Count: 8},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	session := &gameSession{conn: connection, connID: "lottery-daily-use", selectedCharacterID: 19}
	open := func() {
		t.Helper()
		connection.write.Reset()
		if err := service.handleGameCommand(
			session,
			byte(dnfenum.GameCmdCommand),
			uint16(dnfenum.CmdPacketUseLotteryItem),
			[]byte{0, 0, 114, 0},
		); err != nil {
			t.Fatal(err)
		}
	}

	open()
	resultPacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if resultPacket.Header.MsgID != uint16(dnfenum.CmdPacketUseLotteryItem) ||
		len(resultPacket.Body) != currentLotteryNativeResultBodySize || resultPacket.Body[0] != 1 {
		t.Fatalf("第一次开罐没有成功: header=%+v body=%x", resultPacket.Header, resultPacket.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("op27 之后不得插入任何包: %x", rest)
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 522); got != 1 {
		t.Fatalf("开罐计数=%d want 1 stats=%+v", got, character.Stats)
	}

	open()
	rejected, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if rejected.Header.MsgID != uint16(dnfenum.CmdPacketUseLotteryItem) || rejected.Body[0] != 0 {
		t.Fatalf("到上限后仍被放行: header=%+v body=%x", rejected.Header, rejected.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("被拒的开罐不该产生后续包: %x", rest)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if inventory.Slots["0:114"].Count != 1 {
		t.Fatalf("被拒的开罐消耗了抽奖罐: %+v", inventory.Slots)
	}
	character, _, _ = repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 522); got != 1 {
		t.Fatalf("被拒的开罐改动了计数: %d stats=%+v", got, character.Stats)
	}
}
