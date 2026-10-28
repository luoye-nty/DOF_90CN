package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemcooldown "longheng.io/server/internal/modules/dnf/itemcooldown"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 冷却道具的测试夹具：512 = 首日度假礼盒（`[cool time] 86400000` = 1 天、
// `[cooltime group] 66`、`[cooltime maintenance] 1 1 1`），513 = 同组 66 的另一件礼盒，
// 514 = 只有 `[cool time] 10000` 而没有 maintenance 的对照组。
//
// 现场 2026-09-17 运营报"冷却时间为1天的消耗品 小退重上 冷却时间就消失了"：客户端的冷却门
// 只是进程内内存，重上之后它会照常发 op160，服务端必须自己拦住。
func currentBoosterCooltimeTestSource() dungeonDropCatalogTestSource {
	source := currentBoosterTestSource()
	source["stackable/stackable.lst"] += "512 `cash/daily_box.stk`\n513 `cash/daily_box_next.stk`\n514 `cash/local_cooltime_box.stk`\n"
	source["stackable/cash/daily_box.stk"] = "[stackable type]\n`[booster]` 0\n[stack limit]\n5\n" +
		"[cool time]\n86400000\n[cooltime group]\n66\n[cooltime maintenance]\n1 1 1\n" +
		"[booster info]\n[etc]\n1 600 10000 1\n[/etc]\n[/booster info]\n"
	source["stackable/cash/daily_box_next.stk"] = "[stackable type]\n`[booster]` 0\n[stack limit]\n5\n" +
		"[cool time]\n86400000\n[cooltime group]\n66\n[cooltime maintenance]\n1 1 1\n" +
		"[booster info]\n[etc]\n1 600 10000 1\n[/etc]\n[/booster info]\n"
	source["stackable/cash/local_cooltime_box.stk"] = "[stackable type]\n`[booster]` 0\n[stack limit]\n5\n" +
		"[cool time]\n10000\n[cooltime group]\n5\n" +
		"[booster info]\n[etc]\n1 600 10000 1\n[/etc]\n[/booster info]\n"
	return source
}

func mustCurrentBoosterCooltimeCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	catalog, err := newPVFDungeonDropCatalog(currentBoosterCooltimeTestSource())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// 一次带冷却的开启必须在角色身上落下持久窗口，并且重上之后（客户端会照常再发一次）被拦住。
func TestCurrentBoosterCooltimeRefusesReuseAfterRelog(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentBoosterCooltimeCatalog(t)
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
			"0:80": {ItemID: 512, Count: 3},
			"0:81": {ItemID: 513, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	session := &gameSession{conn: connection, connID: "booster-cooltime", selectedCharacterID: 19}
	open := func(slot byte) (byte, []byte) {
		t.Helper()
		connection.write.Reset()
		if err := service.handleGameCommand(
			session,
			byte(dnfenum.GameCmdCommand),
			uint16(dnfenum.CmdPacketUseBoosterItem),
			[]byte{slot, 0},
		); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketUseBoosterItem) || len(packet.Body) == 0 {
			t.Fatalf("booster open header=%+v body=%x", packet.Header, packet.Body)
		}
		return packet.Body[0], packet.Body
	}

	// ① 第一次开启成功，并落下一份 1 天的持久冷却。
	if ok, body := open(80); ok != 1 {
		t.Fatalf("first booster open body=%x", body)
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	expireAt, found := dnfitemcooldown.ExpireAt(character.Stats, 512)
	if !found {
		t.Fatalf("no durable cooltime written: %+v", character.Stats)
	}
	if delta := expireAt - time.Now().Unix(); delta < 86_300 || delta > 86_400 {
		t.Fatalf("cooltime window = %d s, want one day", delta)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if got := inventory.Slots["0:80"].Count; got != 2 {
		t.Fatalf("first open did not consume one: %+v", inventory.Slots["0:80"])
	}

	// ② 小退重上之后客户端照常再发一次（它的冷却记忆没了）：必须被服务端拦住，
	//    而且源道具一个都不能少。
	if ok, body := open(80); ok != 0 {
		t.Fatalf("reuse after relog was not refused: body=%x", body)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "19")
	if got := inventory.Slots["0:80"].Count; got != 2 {
		t.Fatalf("refused reuse consumed the source: %+v", inventory.Slots["0:80"])
	}

	// ③ 同 [cooltime group] 66 的另一件礼盒共用同一个冷却窗口。
	if ok, body := open(81); ok != 0 {
		t.Fatalf("group sibling was not refused: body=%x", body)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "19")
	if got := inventory.Slots["0:81"].Count; got != 1 {
		t.Fatalf("refused group sibling consumed the source: %+v", inventory.Slots["0:81"])
	}

	// ④ 冷却走完之后同一件道具可以再用（把到期时间挪到过去，模拟一天之后）。
	character, _, err = repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	character.Stats[dnfitemcooldown.StatKey(512)] = time.Now().Unix() - 1
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if ok, body := open(80); ok != 1 {
		t.Fatalf("expired cooltime still blocked the item: body=%x", body)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "19")
	if got := inventory.Slots["0:80"].Count; got != 1 {
		t.Fatalf("expired cooltime did not allow the use: %+v", inventory.Slots["0:80"])
	}
}

// op44 的冷却契约完全来自 PVF：`[cool time]`（毫秒）+ `[cooltime group]` +
// `[cooltime maintenance]`。这里验证解析器真的把这三项交给了 owner，并且
// [cooltime group] 解析器能回答**已经在冷却表里的**道具（同组共享窗口必须先知道
// 上一次那件道具属于哪一组）。
func TestAlignedStackableUseResolverReportsPVFCooltimeContract(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "70001 `cash/daily_box.stk`\n70002 `cash/daily_box_next.stk`\n" +
			"70003 `cash/local_cooltime_box.stk`\n70004 `cash/zero_maintenance_box.stk`\n",
		"stackable/cash/daily_box.stk": "[stackable type] `[waste]`\n[cool time]\n86400000\n" +
			"[cooltime group]\n66\n[cooltime maintenance]\n1 1 1\n",
		"stackable/cash/daily_box_next.stk": "[stackable type] `[waste]`\n[cool time]\n86400000\n" +
			"[cooltime group]\n66\n[cooltime maintenance]\n1 1 1\n",
		// 只有 [cool time]：红蓝药那类纯客户端冷却，服务端不记也不拦。
		"stackable/cash/local_cooltime_box.stk": "[stackable type] `[waste]`\n[cool time]\n10000\n[cooltime group]\n5\n",
		// [cooltime maintenance] 段存在但值全 0 = 不跨会话保留。
		"stackable/cash/zero_maintenance_box.stk": "[stackable type] `[waste]`\n[cool time]\n60000\n" +
			"[cooltime group]\n7\n[cooltime maintenance]\n0 0 0\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}

	maintained, err := resolver(70001)
	if err != nil {
		t.Fatal(err)
	}
	if !maintained.MaintainsCooltime || maintained.CooltimeSeconds != 86_400 || maintained.CooltimeGroup != 66 {
		t.Fatalf("1 天冷却的消耗品解析=%+v", maintained)
	}
	local, err := resolver(70003)
	if err != nil {
		t.Fatal(err)
	}
	if local.MaintainsCooltime || local.CooltimeSeconds != 0 || local.CooltimeGroup != 0 {
		t.Fatalf("无 [cooltime maintenance] 的道具解析=%+v", local)
	}
	zero, err := resolver(70004)
	if err != nil {
		t.Fatal(err)
	}
	if zero.MaintainsCooltime {
		t.Fatalf("[cooltime maintenance] 全 0 的道具解析=%+v", zero)
	}

	// 同组解析：冷却表里的 70001 必须被认成 66 组，70003/70004 不参与共享。
	if got := service.currentPVFItemCooltimeGroup(70001); got != 66 {
		t.Fatalf("已冷却道具的 [cooltime group]=%d want=66", got)
	}
	if got := service.currentPVFItemCooltimeGroup(70002); got != 66 {
		t.Fatalf("同组第二件道具的 [cooltime group]=%d want=66", got)
	}
	if got := service.currentPVFItemCooltimeGroup(70003); got != 0 {
		t.Fatalf("无 maintenance 的道具不应参与共享: group=%d", got)
	}
	if got := service.currentPVFItemCooltimeGroup(70004); got != 0 {
		t.Fatalf("maintenance 全 0 的道具不应参与共享: group=%d", got)
	}
}

// 选角阶段必须把仍在冷却的道具重放给客户端（类 0 通知 172），否则 1 天冷却的礼盒重上后
// 图标就是"可用"，玩家只会收到一次静默失败。
func TestSendCurrentItemCooltimeProjectionRestoresActiveWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{
			dnfitemcooldown.StatKey(490702504): now.Add(3 * time.Hour).Unix(),
			dnfitemcooldown.StatKey(490702505): now.Add(-time.Minute).Unix(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn: connection, connID: "cooltime-restore",
		selectedCharacterID: 19,
	}
	if err := service.sendCurrentItemCooltimeProjection(session, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentLoadCooltimeItemInfoMsgID {
		t.Fatalf("cooltime restore header=%+v trailing=%x", packet.Header, trailing)
	}
	// server16 头模式会在包体前带固定的 3 个 0 字节；通知体本身是最后 1+8 字节。
	body := packet.Body
	if len(body) == 12 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:]
	}
	if len(body) != 9 || body[0] != 1 {
		t.Fatalf("cooltime restore body=%x", packet.Body)
	}
	if itemID := binary.LittleEndian.Uint32(body[1:5]); itemID != 490702504 {
		t.Fatalf("cooltime restore item=%d", itemID)
	}
	if remaining := binary.LittleEndian.Uint32(body[5:9]); remaining < 10_700 || remaining > 10_800 {
		t.Fatalf("cooltime restore remaining=%d s, want ~3h", remaining)
	}
	// 过期的那一行顺手清掉，不留垃圾 stat。
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := character.Stats[dnfitemcooldown.StatKey(490702505)]; found {
		t.Fatalf("expired cooltime row survived: %+v", character.Stats)
	}
	// 每个选中角色只重放一次。
	connection.write.Reset()
	if err := service.sendCurrentItemCooltimeProjection(session, "unit_test_duplicate"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("duplicate cooltime restore sent %x", connection.write.Bytes())
	}
}

// 没有 [cooltime maintenance] 的道具（例如 10 秒冷却的红蓝药）保持纯客户端语义：
// 服务端既不允许被拦，也不落 stat。
func TestCurrentBoosterCooltimeIgnoresItemsWithoutMaintenance(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentBoosterCooltimeCatalog(t)
	definition, err := catalog.ResolveItem(514)
	if err != nil {
		t.Fatal(err)
	}
	if definition.HasCooltimeMaintenance {
		t.Fatalf("item 514 must not carry [cooltime maintenance]: %+v", definition)
	}
	if definition.CooltimeMS != 10_000 || definition.CooltimeGroup != 5 {
		t.Fatalf("item 514 cooltime fields = %d/%d", definition.CooltimeMS, definition.CooltimeGroup)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{"0:80": {ItemID: 514, Count: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	session := &gameSession{conn: connection, connID: "booster-local-cooltime", selectedCharacterID: 19}
	for attempt := range 2 {
		connection.write.Reset()
		if err := service.handleGameCommand(
			session,
			byte(dnfenum.GameCmdCommand),
			uint16(dnfenum.CmdPacketUseBoosterItem),
			[]byte{80, 0},
		); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
		if len(packet.Body) == 0 || packet.Body[0] != 1 {
			t.Fatalf("attempt %d refused a client-local cooltime item: body=%x", attempt, packet.Body)
		}
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if entries := dnfitemcooldown.Entries(character.Stats); len(entries) != 0 {
		t.Fatalf("a client-local cooldown was persisted: %+v", entries)
	}
}
