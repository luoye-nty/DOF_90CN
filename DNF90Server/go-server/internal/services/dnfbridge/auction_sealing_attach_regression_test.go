package dnfbridge

import (
	"context"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 2026-09-21 报障：一件「远古遗愿」(28309) 在拍卖行和摆摊都上不了架。
//
// 数据库里的真实那一行（dnf_local, dnf_inventory_items 角色 1 槽位 0:11）：
//
//	item_id=28309 count=1 bind_flag=0 raw_len=119 raw_entry[0x0D]=0x01
//	dnf_inventory_item_extra: seal_flag=1, quality_seed=589339874
//	                          last_grant_source=gm_admin_system_mail
//
// 也就是说它是**已封装**的（raw_entry[0x0D]=1 且 seal_flag=1），只是 bind_flag 为 0。
// PVF 里 28309 是 `[attach type] `[sealing]“ + `[rarity] 3`。
//
// 旧代码在三个地方各拦一次，所以修好任何一个都还是上不了架：
//  1. currentDungeonDiscardTradeable 白名单只有 free/trade，没有 sealing；
//  2. 两条准入策略都用 stack.Bind 当封装位，封装但 bind_flag=0 被判成「解封」；
//  3. 品级白名单只有 {0,1} 与 {2,6}，rarity 3 落到 fail-closed 的 default。
//
// 这条用例把「sealing + rarity 3 + seal_flag=1 + bind_flag=0」这个组合钉住。
func TestSealingAttachRarityThreeSealedEquipmentIsListable(t *testing.T) {
	definition := dungeonDropItemDefinition{
		ItemID: 28309, Kind: dungeonDropItemEquipment, AttachType: "[sealing]", Rarity: 3,
	}
	stack := currentAuctionRealPVFSealedBeamswordStack()

	if !currentDungeonDiscardTradeable(definition) {
		t.Fatal("[sealing] 必须在交易白名单里")
	}
	if !currentDungeonDiscardSealRequired(definition) {
		t.Fatal("[sealing] 必须要求封装态")
	}
	if !currentDungeonDiscardSealed(stack) {
		t.Fatal("seal_flag=1 / raw_entry[0x0D]=1 必须读成已封装（即使 bind_flag=0）")
	}

	// 上架策略冻结的是 escrow owner 回查的 stack.Bind，这里 bind_flag=0，
	// 所以两个返回值都必须是 false —— 回传封装位会让托管那一步打回。
	expected, allow, eligible := currentAuctionRegistrationPolicyForDefinition(definition, stack)
	if !eligible || expected || allow {
		t.Fatalf("拍卖策略=(expected=%t allow=%t eligible=%t), 期望 (false,false,true)", expected, allow, eligible)
	}
	expected, allow, eligible = currentPrivateStoreRegistrationPolicyForDefinition(definition, stack)
	if !eligible || expected || allow {
		t.Fatalf("摆摊策略=(expected=%t allow=%t eligible=%t), 期望 (false,false,true)", expected, allow, eligible)
	}

	// 解封件仍然不放行：raw_entry[0x0D]=0 且没有 seal extra。
	unsealed := stack
	unsealed.RawEntry = append([]byte(nil), stack.RawEntry...)
	unsealed.RawEntry[0x0D] = 0
	unsealed.Extra = map[string]string{"item_kind": "equipment", "quality_seed": "589339874"}
	if currentDungeonDiscardSealed(unsealed) {
		t.Fatal("raw_entry[0x0D]=0 且无 seal extra 必须读成解封")
	}
	if _, _, eligible := currentAuctionRegistrationPolicyForDefinition(definition, unsealed); eligible {
		t.Fatal("解封的 [sealing] 装备不允许上架")
	}
	if _, _, eligible := currentPrivateStoreRegistrationPolicyForDefinition(definition, unsealed); eligible {
		t.Fatal("解封的 [sealing] 装备不允许摆摊")
	}
}

// currentAuctionRealPVFSealedBeamswordStack 复刻数据库里 28309 那一行的原始
// raw_entry（119 字节，0x77 线上行），避免手搓字段和线上不一致。
func currentAuctionRealPVFSealedBeamswordStack() dnfrepo.ItemStack {
	const rawHex = "0900956E0000E29C202300300001000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000"
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		panic(err)
	}
	if len(raw) != currentItemListEntryWireSize || raw[0x0D] != 1 {
		panic("28309 raw_entry 必须是 119 字节且 +0x0D=1")
	}
	return dnfrepo.ItemStack{
		ItemID:   28309,
		Count:    1,
		Bind:     false, // 线上 bind_flag=0
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":    "equipment",
			"seal_flag":    "1",
			"seal":         "1",
			"quality_seed": "589339874",
		},
	}
}

// 拍卖行端到端：用真机 PVF 解析 28309，走完整的 op183 上架（含 escrow 托管）。
// 这条覆盖的是「策略放行 + owner 事务回查绑定位」两段，单测策略通过但托管
// 打回的情况不会漏网。
func TestCurrentAuctionRealPVFRegistersSealedSealingEquipment(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to register sealed [sealing] equipment")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := catalog.ResolveItem(28309)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Kind != dungeonDropItemEquipment || definition.Rarity != 3 ||
		normalizeMagicBoxPVFType(definition.AttachType) != "sealing" {
		t.Fatalf("28309 定义=%+v，期望 equipment/rarity 3/[sealing]", definition)
	}

	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	stack := currentAuctionRealPVFSealedBeamswordStack()
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "1", "dnf:1", "Seller", 10_000_000,
		map[string]dnfrepo.ItemStack{"0:11": stack})

	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.nextAuctionListingID.Store(2000)
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	// buildCurrentAuctionRegisterRequestForTest(slot, itemID, count, startPrice, instantPrice, unitPrice, durationHours)
	body := buildCurrentAuctionRegisterRequestForTest(11, 28309, 1, -1, 6000, 6000, 24)
	if err := service.registerCurrentAuctionItemLocked(session, body); err != nil {
		t.Fatal(err)
	}
	ack, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(183) || len(ack.Body) != 2 || ack.Body[0] != 1 {
		t.Fatalf("上架 ACK=%x（body[1] 是失败码，145 就是旧行为的「无法上架到拍卖行的物品」）", ack.Body)
	}
	// 成功上架后服务端还会接着推自己的在售列表刷新，所以这里不校验 trailing。
	if ack.Body[1] != 0 {
		t.Fatalf("上架 pay/失败码=%d，期望 0", ack.Body[1])
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("读回背包 found=%t err=%v", found, err)
	}
	if remaining := inventory.Slots["0:11"]; remaining.ItemID != 0 || remaining.Count != 0 {
		t.Fatalf("上架成功后源格必须清空，实际=%+v", remaining)
	}
}

// 摆摊端到端：op87 用的是 2026-09-21 真机失败时抓到的**原始包体**
// （packet_log 里 body_hex=06000000B0A1B0A1B0A101000007B20100000B00E29C2023FFFF，
// 当时服务端回 row_source_stack_unusable）。现在同样的包体必须开摊成功。
func TestCurrentPrivateStoreRealPVFAcceptsSealedSealingEquipment(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to complete the captured sealed [sealing] stall body")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:1",
		Metadata: map[string]string{
			premium.MetadataKey(premium.TypePrivateStore): strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: "dnf:1", Name: "Seller", Stats: map[string]int64{"gold": 10_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{"0:11": currentAuctionRealPVFSealedBeamswordStack()},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 1, accountID: "dnf:1"}
	service := &Service{
		options:              options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		pvfItemCatalog:       catalog,
		repositoryProvider:   func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:         map[uint16]*gameSession{1: session},
		privateStores:        map[uint16]*currentPrivateStore{1: {OwnerID: 1, OwnerSession: session}},
		privateStoreVisitors: make(map[*gameSession]uint16),
		onlinePlayers:        newOnlinePlayerManager(),
	}
	// 真机抓到的原始 op87 包体：标题 6 字节、1 行、格子 0:11、一口价 111111、
	// 实例 589339874、权限位 0xFFFF（没有摆摊人偶）。
	body, err := hex.DecodeString("06000000B0A1B0A1B0A101000007B20100000B00E29C2023FFFF")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.completeCurrentPrivateStore(session, body); err != nil {
		t.Fatal(err)
	}
	store := service.privateStores[1]
	if store == nil || !store.Active || len(store.Rows) != 1 {
		t.Fatalf("摆摊未开成功：%+v", store)
	}
	row := store.Rows[0]
	if row.ItemID != 28309 || row.Count != 1 || row.InstanceValue != 589339874 ||
		row.Price != 111111 || row.Slot != 11 {
		t.Fatalf("摆摊行=%+v，期望 item=28309 count=1 instance=589339874 price=111111 slot=11", row)
	}
	// escrow owner 回查的必须是 bind_flag 本身：这一行 bind_flag=0。
	if row.ExpectedBind {
		t.Fatalf("ExpectedBind 必须冻结 bind_flag=false，实际=%t（回传封装位会让交接被打回）", row.ExpectedBind)
	}
}
