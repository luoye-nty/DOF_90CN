package dnfbridge

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 现场 2026-10-03（服主）：角色 1 礼白666 用的
// stackable/10159001/10159717.stk（暗之偶像阿努比斯 塔罗牌）是**混合奖池**：
//
//	[int data]
//	108040415 175000 1  100270516 175000 1  114030107 175000 1  10159142 300000 250
//	[/int data]
//
// 前三条是装备（血红战斧 / 阿努比斯陶瓷长靴 / 战戟），第四条才是「光之根源」。
// 装备行按权重占 63.6%，而**第一行就是装备** —— 于是"roll 恒为 0"这个 bug 让这张卡
// 100% 用不了，日志上表现为连点 6 次全是同一个 item=108040415。
//
// 这条用例把那个形状原样搬进内存夹具：第一行装备、第二行堆叠品。
const (
	tarotMixedTestCardItemID  = uint32(513)
	tarotMixedTestCardSlot    = int16(72)
	tarotMixedTestGold        = int64(300000)
	tarotMixedTestGoldCost    = int64(100000)
	tarotMixedTestEquipmentID = int64(703)
	tarotMixedTestMaterialID  = int64(600)
	tarotMixedTestDurability  = 45
)

func currentTarotMixedTableTestSource() dungeonDropCatalogTestSource {
	source := currentBoosterTestSource()
	source["stackable/stackable.lst"] += "513 `cash/tarot_mixed.stk`\n"
	// 与真档同形：equipment.lst 记的是**相对 equipment/ 的路径**，归档里才是全路径。
	source["equipment/equipment.lst"] += "703 `character/priest/weapon/axe/tarot_reward.equ`\n"
	source["stackable/cash/tarot_mixed.stk"] =
		"[stackable type]\n`[monster tarot card]` 19\n" +
			"[stack limit]\n100\n" +
			"[lottery use cost]\n100000\n" +
			// 第一行是装备，第二行是可叠加物 —— 正是 10159717 的形状。
			"[int data]\n703 175000 1 600 300000 250\n[/int data]\n"
	source["equipment/character/priest/weapon/axe/tarot_reward.equ"] =
		"[name]\n`tarot axe`\n[equipment type]\n`[weapon]` 24\n[attach type]\n`[sealing]`\n[durability]\n45\n"
	return source
}

func mustCurrentTarotMixedTableTestCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	catalog, err := newPVFDungeonDropCatalog(currentTarotMixedTableTestSource())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func newTarotMixedTableFixture(
	t *testing.T,
	roll int64,
) (*Service, *gameSession, *bufferConn, context.Context, dnfrepo.Group) {
	t.Helper()
	ctx := context.Background()
	catalog := mustCurrentTarotMixedTableTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"gold": tarotMixedTestGold},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:72": {ItemID: int64(tarotMixedTestCardItemID), Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	service.dungeonChoice = func(int) (int, error) { return int(roll), nil }
	session := &gameSession{conn: connection, connID: "tarot-mixed", selectedCharacterID: 19}
	return service, session, connection, ctx, repositories
}

// 抽到**装备行**必须真的发出去，而且发出去的必须是一件"装备"而不是一件叠加物。
// 旧代码在这里回 op27 {0,4}（成功位=0），源卡与金币都不动 —— 也就是服主看到的"用不了"。
func TestTarotLotteryGrantsAnEquipmentRow(t *testing.T) {
	// roll=0 落在第一行（装备）。旧代码的 bug 就是"永远 roll=0"，所以这一行
	// 恰好就是线上每一张卡都会撞上的那一行。
	service, session, connection, ctx, repositories := newTarotMixedTableFixture(t, 0)

	openTarotLotteryCard(t, service, session, tarotMixedTestCardSlot)

	result, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if result.Header.MsgID != uint16(dnfenum.CmdPacketUseLotteryItem) {
		t.Fatalf("第一个包不是 op27: header=%+v body=%x", result.Header, result.Body)
	}
	if result.Body[0] != 1 || len(result.Body) != currentLotteryNativeResultBodySize {
		t.Fatalf("装备行被拒了: body=%x（装备行必须发得出去）", result.Body)
	}
	row := result.Body[3 : 3+currentItemListEntryWireSize]
	if got := binary.LittleEndian.Uint32(row[2:6]); got != uint32(tarotMixedTestEquipmentID) {
		t.Fatalf("op27 行里的道具 id=%d want %d", got, tarotMixedTestEquipmentID)
	}
	// 装备行由服务端完全拥有 +0x0B（耐久）与 +0x0D（封装）：PVF [durability] 45 与
	// [sealing] 必须落在这两个字节上，否则客户端把新装备画成 0 耐久 / 未封装。
	if got := binary.LittleEndian.Uint16(row[0x0B:0x0D]); got != tarotMixedTestDurability {
		t.Fatalf("op27 行 +0x0B 耐久=%d want %d", got, tarotMixedTestDurability)
	}
	if row[0x0D] != 1 {
		t.Fatalf("op27 行 +0x0D 封装位=%d want 1（[sealing]）", row[0x0D])
	}

	// op27 之后只允许一个钱包包（源卡花钱了）。
	wallet, rest := splitGameServerUpperPacket(t, trailing)
	if wallet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("第二个包不是钱包同步: header=%+v body=%x", wallet.Header, wallet.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("op27+钱包之后多了包: %x", rest)
	}

	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := character.Stats["gold"]; got != tarotMixedTestGold-tarotMixedTestGoldCost {
		t.Fatalf("金币=%d want %d", got, tarotMixedTestGold-tarotMixedTestGoldCost)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if source := inventory.Slots["0:72"]; source.ItemID != 0 || source.Count != 0 {
		t.Fatalf("源卡没有被消耗: %+v", source)
	}
	key, granted, ok := findRealTarotLotteryGrantedEquipment(inventory, tarotMixedTestEquipmentID)
	if !ok {
		t.Fatalf("背包里没有发出去的装备: %v", realTarotLotterySlotSummary(inventory))
	}
	if granted.Count != 1 {
		t.Fatalf("装备数量=%d want 1（一格一件）", granted.Count)
	}
	if granted.Extra["item_kind"] != "equipment" || granted.Extra["equipment_type"] != "[weapon]" {
		t.Fatalf("装备行元数据不对: %v", granted.Extra)
	}
	if granted.Extra["seal_flag"] != "1" {
		t.Fatalf("[sealing] 没有写进 seal_flag: %v", granted.Extra)
	}
	if got := granted.Extra["durability"]; got != "45" {
		t.Fatalf("耐久=%q want 45", got)
	}
	if seed := granted.Extra["quality_seed"]; seed == "" || seed == "0" {
		t.Fatalf("品级种子=%q（不写的话客户端把奖励装备画成最下级）", seed)
	}
	if len(granted.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("RawEntry 长=%d want %d", len(granted.RawEntry), currentItemListEntryWireSize)
	}
	t.Logf("装备行落点 %s: %+v", key, granted.Extra)
}

// 抽到可叠加行必须与改动前逐字同形：数量来自 [int data] 的 count 列（250），
// 走原有的合并/开行逻辑。
func TestTarotLotteryGrantsAStackableRowAsBefore(t *testing.T) {
	// 175000 是第一行的权重，也就是第二行的起始 roll。
	service, session, connection, ctx, repositories := newTarotMixedTableFixture(t, 175000)

	openTarotLotteryCard(t, service, session, tarotMixedTestCardSlot)

	result, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if result.Body[0] != 1 {
		t.Fatalf("可叠加行被拒了: body=%x", result.Body)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	key, granted, ok := findRealTarotLotteryGrantedEquipment(inventory, tarotMixedTestMaterialID)
	if !ok {
		t.Fatalf("背包里没有发出去的材料: %v", realTarotLotterySlotSummary(inventory))
	}
	if granted.Count != 250 {
		t.Fatalf("材料数量=%d want 250（[int data] 的 count 列）", granted.Count)
	}
	if !strings.EqualFold(granted.Extra["item_kind"], "stackable") {
		t.Fatalf("材料行的 item_kind=%q want stackable", granted.Extra["item_kind"])
	}
	t.Logf("可叠加行落点 %s: count=%d", key, granted.Count)
}

// 「修 A 不坏 B」：非塔罗牌的 op27 道具必须原样落回罐子路径，本改动不得改变分派。
// （tarot_lottery_item_test.go 的 TestCurrentTarotLotteryDispatchLeavesEveryOtherOp27ItemAlone
// 钉的是分派本身，这里补一条：混合表夹具里的**非塔罗牌**槽位同样不被接管。）
func TestTarotMixedTableLeavesTheJarRouteAlone(t *testing.T) {
	service, session, _, ctx, _ := newTarotMixedTableFixture(t, 0)
	catalog := service.pvfItemCatalog
	// 71 号槽是空的；90 号也是。两者都必须 handled=false。
	for _, slot := range []int16{71, 90} {
		handled, err := service.tryHandleCurrentTarotLotteryItem(ctx, session, catalog, false, slot)
		if err != nil {
			t.Fatalf("slot=%d err=%v", slot, err)
		}
		if handled {
			t.Fatalf("slot=%d 被塔罗牌分支接管了，但它不是塔罗牌", slot)
		}
	}
}
