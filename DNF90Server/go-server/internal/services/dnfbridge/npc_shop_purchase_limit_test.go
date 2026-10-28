package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentPurchaseLimitTestBoxItemID = 600

func mustCurrentPurchaseLimitService(t *testing.T, limitSection string, gold int64) (*Service, dnfrepo.Group) {
	t.Helper()
	source := bridgePVFSource{
		"monster/monster.lst":       "",
		"stackable/stackable.lst":   "600 `limited_box.stk`\n",
		"equipment/equipment.lst":   "",
		"itemshop/itemshop.lst":     "1 `test_shop.shp`\n",
		"itemshop/test_shop.shp":    "[item list]\n600\n[/item list]\n",
		"equipment/pricetable.tbl":  "[rate]\n200 150 30\n[repair cost]\n0.08\n",
		"etc/tradingsystem.etc":     "[trade item list]\n88\n[item list]\n[/item list]\n",
		"stackable/limited_box.stk": "[stackable type]\n`[waste]`\n[stack limit]\n999\n[price]\n100\n[value]\n50\n" + limitSection,
	}
	shop, err := newCurrentNPCShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := mustCurrentNPCShopRepositories(t, gold, nil)
	service := currentNPCShopTestService(repositories, shop, items)
	service.purchaseLimitClock = func() time.Time {
		return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	}
	return service, repositories
}

func TestCurrentNPCShopBuyEnforcesAccountWeeklyPurchaseLimit(t *testing.T) {
	service, repositories := mustCurrentPurchaseLimitService(t, "[purchase limit]\n`account` `weekly` 2\n", 100000)
	ctx := context.Background()
	conn := &bufferConn{}
	session := &gameSession{conn: conn, selectedCharacterID: 19}
	for index := 1; index <= 2; index++ {
		conn.write.Reset()
		if err := service.handleCurrentNPCShopBuy(session, currentNPCShopBuyTestBody(currentPurchaseLimitTestBoxItemID, 1, 0, 0)); err != nil {
			t.Fatal(err)
		}
		// 购买成功会先回 op21 成功包，再补一条 op14 增量行；这里只看第一个包。
		ack, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
		if ack.Header.MsgID != uint16(dnfenum.CmdPacketBuyItem) || len(ack.Body) == 0 || ack.Body[0] != 1 {
			t.Fatalf("购买 %d 次未成功: body=%x", index, ack.Body)
		}
	}
	policy := purchaselimit.Policy{
		ItemID: currentPurchaseLimitTestBoxItemID,
		Scope:  purchaselimit.ScopeAccount,
		Period: purchaselimit.PeriodWeekly,
		Limit:  2,
	}
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if got := account.Metadata[purchaselimit.CountKey(policy)]; got != "2" {
		t.Fatalf("账号维度计数=%q want 2 (metadata=%v)", got, account.Metadata)
	}

	// 第三次应被限购拦下：失败包 + 金币/背包不再变化。
	conn.write.Reset()
	if err := service.handleCurrentNPCShopBuy(session, currentNPCShopBuyTestBody(currentPurchaseLimitTestBoxItemID, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || !bytes.Equal(failure.Body, []byte{0, currentPurchaseLimitErrorCode}) {
		t.Fatalf("超限失败包=%x trailing=%d", failure.Body, len(trailing))
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if character.Stats["gold"] != 100000-200 {
		t.Fatalf("超限后金币=%d want %d", character.Stats["gold"], 100000-200)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	assertCurrentNPCShopInventoryAmount(t, inventory, currentPurchaseLimitTestBoxItemID, 2)
}

func TestCurrentNPCShopBuyEnforcesCharacterDailyLegacyLimit(t *testing.T) {
	service, repositories := mustCurrentPurchaseLimitService(t, "[daily purchase limit]\n`character` 1\n", 100000)
	ctx := context.Background()
	conn := &bufferConn{}
	session := &gameSession{conn: conn, selectedCharacterID: 19}
	conn.write.Reset()
	if err := service.handleCurrentNPCShopBuy(session, currentNPCShopBuyTestBody(currentPurchaseLimitTestBoxItemID, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	policy := purchaselimit.Policy{
		ItemID: currentPurchaseLimitTestBoxItemID,
		Scope:  purchaselimit.ScopeCharacter,
		Period: purchaselimit.PeriodDaily,
		Limit:  1,
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := character.Stats[purchaselimit.CountKey(policy)]; got != 1 {
		t.Fatalf("角色维度计数=%d want 1 (stats=%v)", got, character.Stats)
	}
	conn.write.Reset()
	if err := service.handleCurrentNPCShopBuy(session, currentNPCShopBuyTestBody(currentPurchaseLimitTestBoxItemID, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	failure, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if !bytes.Equal(failure.Body, []byte{0, currentPurchaseLimitErrorCode}) {
		t.Fatalf("旧字段超限失败包=%x", failure.Body)
	}
}

func TestCurrentNPCShopPurchaseCountAckCarriesAuthoritativeCount(t *testing.T) {
	service, _ := mustCurrentPurchaseLimitService(t, "[purchase limit]\n`account` `weekly` 10\n", 100000)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, selectedCharacterID: 19}
	if err := service.handleCurrentNPCShopBuy(session, currentNPCShopBuyTestBody(currentPurchaseLimitTestBoxItemID, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	conn.write.Reset()
	request := make([]byte, currentNPCShopPurchaseCountRequestWireSize)
	binary.LittleEndian.PutUint32(request, 1)
	if err := service.handleCurrentNPCShopPurchaseCount(session, request); err != nil {
		t.Fatal(err)
	}
	ack, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || ack.Header.MsgID != uint16(dnfenum.CmdPacketShopPurchaseCount) {
		t.Fatalf("purchase-count ack header=%+v trailing=%d", ack.Header, len(trailing))
	}
	lists := parseCurrentPurchaseLimitAckForTest(t, ack.Body)
	want := lists[currentPurchaseLimitListIndex(purchaselimit.ScopeAccount, purchaselimit.PeriodWeekly)][currentPurchaseLimitTestBoxItemID]
	if want != 1 {
		t.Fatalf("账号周期表[%d]=%d want 1 (lists=%+v)", currentPurchaseLimitTestBoxItemID, want, lists)
	}
	// 映射未证实前 4 张表同值下发：每张表都必须带上这条计数。
	for index := range lists {
		if got := lists[index][currentPurchaseLimitTestBoxItemID]; got != 1 {
			t.Fatalf("表 %d 缺少权威计数: %+v", index, lists)
		}
	}
}

// parseCurrentPurchaseLimitAckForTest 按“成功字节 + 4 张 u32 计数表”的假设解析
// 回包，用于验证服务端真的把计数放进去了。
func parseCurrentPurchaseLimitAckForTest(t *testing.T, body []byte) [4]map[uint32]uint32 {
	t.Helper()
	var lists [4]map[uint32]uint32
	if len(body) < 1+4*4 || body[0] != 1 {
		t.Fatalf("ack body=%x", body)
	}
	offset := 1
	for index := range lists {
		count := binary.LittleEndian.Uint32(body[offset : offset+4])
		offset += 4
		lists[index] = make(map[uint32]uint32, count)
		for entry := uint32(0); entry < count; entry++ {
			if offset+8 > len(body) {
				t.Fatalf("ack body truncated at list=%d entry=%d body=%x", index, entry, body)
			}
			itemID := binary.LittleEndian.Uint32(body[offset : offset+4])
			value := binary.LittleEndian.Uint32(body[offset+4 : offset+8])
			offset += 8
			lists[index][itemID] = value
		}
	}
	if offset != len(body) {
		t.Fatalf("ack body trailing bytes offset=%d len=%d", offset, len(body))
	}
	return lists
}
