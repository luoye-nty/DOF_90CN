package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 无色小晶块 / 灵魂 这 12 个道具在 90 客户端里住在**账号共享**的晶体仓库
// (list 0 槽 354..359) 与灵魂仓库 (360..365)，权威表见
// repository/account_shared_materials.go。
//
// 2026-09-21 现场：角色 1111 用金币从 NPC 商店买无色小晶块，20 个全部落在
// **角色背包 0:154**，而账号槽 0:358 另有 43890 个 —— 同一条 list-0 报文里出现两行
// 同 itemID，客户端把背包那一行显示成了仓库的数量（小退后显示"恢复"，但物品归属
// 依然是错的，而且背包里那 20 个既不能做晶体兑换材料、也不能被晶体契约消耗）。
//
// 根因：金币购买 (commitCurrentNPCShopBuy) 没有查固定槽表，而它的兄弟分支材料兑换
// (commitCurrentNPCShopMaterialExchange) 查了。本用例把两者口径一致这件事钉死。
func TestCurrentNPCShopGoldBuyLandsWarehouseMaterialInAccountCell(t *testing.T) {
	const (
		clearCubeItemID = uint32(3037)
		accountCubeSlot = int16(358)
		cubeGoldPrice   = 90
		startingGold    = uint32(1000)
		purchasedCount  = uint32(2)
		// 现场形态：账号晶体仓里已经堆着 43890 个无色。
		accountCubeCountBefore = uint32(43890)
	)
	// 真实 PVF 里 3037 是 stackable/material/cubepiece_clear.stk，并且**没有**
	// `[stack limit]`（2026-09-21 用 pvfcat 核对线上 Script.pvf）—— 所以这一个格子
	// 不设上限。这正是它能堆到 43890 的原因，也是"装不下才溢出到背包"那条分支
	// 在本 PVF 里不会触发的原因。夹具照抄这一点，避免测试与现实脱节。
	source := bridgePVFSource{
		"monster/monster.lst":            "",
		"stackable/stackable.lst":        "3037 `cube_clear.stk`\n",
		"equipment/equipment.lst":        "",
		"itemshop/itemshop.lst":          "15 `EquipmentShop7.shp`\n",
		"itemshop/(r)equipmentshop7.shp": "[sell info]\n[tab]\n`其他`\n[item list]\n3037\n[/item list]\n[/tab]\n[/sell info]\n",
		"equipment/pricetable.tbl":       "[rate]\n200 150 30\n",
		"stackable/cube_clear.stk":       "[stackable type]\n`[material]` 2\n[price]\n90\n[value]\n40\n",
	}
	shop, err := newCurrentNPCShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := mustCurrentNPCShopRepositories(t, int64(startingGold), nil)
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: int64(clearCubeItemID), Count: int64(accountCubeCountBefore)}},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := currentNPCShopTestService(repositories, shop, items)
	session := &gameSession{conn: connection, connID: "npc-shop-cube-gold-buy", selectedCharacterID: 19}
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyTestBody(clearCubeItemID, purchasedCount, 15, 2)); err != nil {
		t.Fatal(err)
	}

	wantGold := startingGold - cubeGoldPrice*purchasedCount
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketBuyItem) || ack.Body[0] != 1 ||
		binary.LittleEndian.Uint32(ack.Body[1:5]) != wantGold ||
		binary.LittleEndian.Uint32(ack.Body[19:23]) != clearCubeItemID ||
		binary.LittleEndian.Uint32(ack.Body[23:27]) != purchasedCount {
		t.Fatalf("gold buy ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	// ACK 必须声明真正落库的那个账号槽，而不是某个背包格。
	if slot := int16(binary.LittleEndian.Uint16(ack.Body[17:19])); slot != accountCubeSlot {
		t.Fatalf("buy ACK declares slot %d, want the account crystal cell %d", slot, accountCubeSlot)
	}
	update, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("buy update header=%+v trailing=%d", update.Header, len(trailing))
	}
	assertCurrentNPCShopIncrementalRows(t, update.Body, wantGold, accountCubeSlot, clearCubeItemID, accountCubeCountBefore+purchasedCount)

	account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("account inventory found=%t err=%v", found, err)
	}
	stack, found := account.Slots["0:358"]
	if !found || stack.ItemID != int64(clearCubeItemID) || stack.Count != int64(accountCubeCountBefore+purchasedCount) {
		t.Fatalf("account crystal cell=%+v found=%t want 3037x%d", stack, found, accountCubeCountBefore+purchasedCount)
	}
	if len(stack.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("account crystal row raw entry len=%d want %d", len(stack.RawEntry), currentItemListEntryWireSize)
	}
	if stack.Extra["last_grant_source"] != "npc_shop" || stack.Extra["last_npc_shop_context"] != "15" ||
		stack.Extra["last_npc_shop_actor_context"] != "2" {
		t.Fatalf("account crystal cell provenance=%+v", stack.Extra)
	}

	// 核心断言：角色背包里一个无色都不许有。这正是 2026-09-21 事故的形状。
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	assertCurrentNPCShopInventoryAmount(t, inventory, int64(clearCubeItemID), 0)
	if _, exists := inventory.Slots[currentCeraShopInventorySlotKey(0, 154)]; exists {
		t.Fatalf("bought cube landed in the backpack slot 0:154 again: %+v", inventory.Slots)
	}

	character, found, err := repositories.Character.Load(context.Background(), "19")
	if err != nil || !found || character.Stats["gold"] != int64(wantGold) {
		t.Fatalf("character=%+v found=%t err=%v want gold %d", character, found, err, wantGold)
	}
}

// TestCurrentNPCShopGoldBuyOverflowsFullWarehouseCellIntoBackpack 守住兜底那半边：
// 万一固定槽真的装不下（PVF 声明了 `[stack limit]`、或该格被历史脏数据占着），
// 玩家花钱买到的余量必须溢出到背包，绝不静默吞掉。
func TestCurrentNPCShopGoldBuyOverflowsFullWarehouseCellIntoBackpack(t *testing.T) {
	const (
		clearCubeItemID = uint32(3037)
		cubeGoldPrice   = 90
		startingGold    = uint32(1000)
		purchasedCount  = uint32(3)
		stackLimit      = int64(5)
		cellCountBefore = int64(4)
	)
	source := bridgePVFSource{
		"monster/monster.lst":            "",
		"stackable/stackable.lst":        "3037 `cube_clear.stk`\n",
		"equipment/equipment.lst":        "",
		"itemshop/itemshop.lst":          "15 `EquipmentShop7.shp`\n",
		"itemshop/(r)equipmentshop7.shp": "[sell info]\n[tab]\n`其他`\n[item list]\n3037\n[/item list]\n[/tab]\n[/sell info]\n",
		"equipment/pricetable.tbl":       "[rate]\n200 150 30\n",
		"stackable/cube_clear.stk":       "[stackable type]\n`[material]` 2\n[stack limit]\n5\n[price]\n90\n[value]\n40\n",
	}
	shop, err := newCurrentNPCShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := mustCurrentNPCShopRepositories(t, int64(startingGold), nil)
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: int64(clearCubeItemID), Count: cellCountBefore}},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := currentNPCShopTestService(repositories, shop, items)
	session := &gameSession{conn: connection, connID: "npc-shop-cube-gold-buy-overflow", selectedCharacterID: 19}
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyTestBody(clearCubeItemID, purchasedCount, 15, 2)); err != nil {
		t.Fatal(err)
	}
	if _, rest := splitGameServerUpperPacket(t, connection.write.Bytes()); len(rest) == 0 {
		t.Fatal("buy did not emit an incremental update")
	}

	account, _, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != int64(clearCubeItemID) || stack.Count != stackLimit {
		t.Fatalf("account crystal cell=%+v want 3037x%d (capped by [stack limit])", stack, stackLimit)
	}
	inventory, _, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil {
		t.Fatal(err)
	}
	wantBackpack := cellCountBefore + int64(purchasedCount) - stackLimit
	assertCurrentNPCShopInventoryAmount(t, inventory, int64(clearCubeItemID), wantBackpack)
	if _, exists := inventory.Slots[currentCeraShopInventorySlotKey(0, 154)]; exists {
		t.Fatalf("overflow must not land on 0:154: %+v", inventory.Slots)
	}
	character, _, err := repositories.Character.Load(context.Background(), "19")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(startingGold) - cubeGoldPrice*int64(purchasedCount); character.Stats["gold"] != want {
		t.Fatalf("gold=%d want %d (the whole purchase must still be paid for)", character.Stats["gold"], want)
	}
}
