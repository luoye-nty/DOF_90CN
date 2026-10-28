package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"reflect"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestSendAlignedUpperResponsesSuppressesUnsafeEquipmentSlotPostActionBeforeMode0Appearance(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "15",
		Level:       1,
		Stats:       map[string]int64{currentGameOptionUserStateStat: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"13": {
				SlotIndex: 13,
				ItemID:    100312425,
				RawEntry:  make([]byte, currentItemListEntryWireSize),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}, Warehouse: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "aligned-equipment-post-action",
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		initialTownRouteCharacterID:     19,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
	}
	ackBody := []byte{1, 0, 9, 0, 1, 0, 0, 0, 3, 12, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedItemContainers,
			alignedcmd.PostActionRefreshSelectedEquipmentSlots,
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != 19 || first.Header.Classification != dnfproto.DefaultChannelClassification || len(first.Body) != len(ackBody)+3 || !bytes.Equal(first.Body[:3], []byte{0, 0, 0}) || !bytes.Equal(first.Body[3:], ackBody) {
		t.Fatalf("first packet header=%+v body=%x", first.Header, first.Body)
	}
	for _, wantListType := range currentSelectedItemListTypes {
		refresh, remaining := splitCurrentSceneItemListPacket(t, rest)
		if refresh.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) || refresh.Header.Classification != 0 || len(refresh.Body) == 0 || refresh.Body[0] != wantListType {
			t.Fatalf("list%d refresh header=%+v body=%x", wantListType, refresh.Header, refresh.Body)
		}
		rest = remaining
	}
	// The rebuilt containers drop the client's slot-keyed lock model, so the
	// container post action pairs its op13 lists with the durable op251 snapshot
	// the same way the select-character bootstrap does. This inventory holds no
	// locked row, which still requires the explicit empty list.
	lockSnapshot, rest := splitGameServerUpperPacket(t, rest)
	if lockSnapshot.Header.MsgID != dnfitemlock.LockListMessageID || lockSnapshot.Header.Classification != 0 ||
		!bytes.Equal(lockSnapshot.Body, []byte{0, 0, 0, 0, 0}) {
		t.Fatalf("lock snapshot header=%+v body=%x", lockSnapshot.Header, lockSnapshot.Body)
	}
	second, rest := splitGameServerUpperPacket(t, rest)
	const codecPrefix = 3
	const currentObjectKeyOffset = 0x4c
	if second.Header.MsgID != 2 || second.Header.Classification != 0 ||
		len(second.Body) < codecPrefix+currentObjectKeyOffset+2 ||
		!bytes.Equal(second.Body[:codecPrefix], []byte{0, 0, 0}) ||
		second.Body[codecPrefix] != 0 ||
		binary.LittleEndian.Uint16(second.Body[codecPrefix+currentObjectKeyOffset:]) != 19 {
		t.Fatalf("second packet header=%+v body=%x", second.Header, second.Body)
	}
	userState, rest := splitGameServerUpperPacket(t, rest)
	if userState.Header.MsgID != currentActorUserStateMsgID || userState.Header.Classification != 0 ||
		!bytes.Equal(userState.Body, append([]byte{0, 0, 0}, buildCurrentActorUserStateBody(19, 1)...)) {
		t.Fatalf("post-mode0 user-state header=%+v body=%x", userState.Header, userState.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes after post-mode0 user state: %x", rest)
	}
}

func TestSortItemAckThenAuthoritativeList0KeepsGoldAndAccountCrystals(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:sort-owner",
		Name:        "SortOwner",
		Job:         "0",
		Level:       90,
		Stats:       map[string]int64{"gold": 106241},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:12": {ItemID: 200, Count: 1, Extra: map[string]string{"item_kind": "equipment"}},
			"0:20": {ItemID: 100, Count: 1, Extra: map[string]string{"item_kind": "avatar"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	crystalRaw := make([]byte, currentItemListEntryWireSize)
	crystalRaw[0x59] = 0x5A
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:sort-owner",
		Slots: map[string]dnfrepo.ItemStack{
			"0:354": {ItemID: 3033, Count: 13, RawEntry: crystalRaw},
		},
	}); err != nil {
		t.Fatal(err)
	}

	result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
		Command:             byte(dnfenum.GameCmdCommand),
		CommandKnown:        true,
		Opcode:              uint16(dnfenum.CmdPacketSortItem),
		Body:                []byte{6, 0, 0, 0, 16, 0, 24, 1, 32, 0},
		AccountID:           "dnf:sort-owner",
		SelectedCharacterID: 19,
		Repositories:        repositories,
	})
	if err != nil || !routed {
		t.Fatalf("route sort item routed=%t err=%v", routed, err)
	}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 ||
		len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedItemContainers {
		t.Fatalf("sort result=%+v", result)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:sort-owner",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "sort-authoritative-list0",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error=%v", err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSortItem) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(ack.Body) != 12 || !bytes.Equal(ack.Body[3:], []byte{1, 4, 0, 0, 0, 8, 0, 16, 0}) {
		t.Fatalf("sort ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	mainList, rest := splitCurrentSceneItemListPacket(t, rest)
	if len(mainList.Body) < 5 || mainList.Body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("main list body=%x", mainList.Body)
	}
	rows := decodeCurrentItemListTestRows(t, mainList.Body[5:])
	wantRows := map[uint16]struct {
		itemID uint32
		count  uint32
	}{
		0:   {itemID: 0, count: 106241},
		9:   {itemID: 100, count: 1},
		10:  {itemID: 200, count: 1},
		354: {itemID: 3033, count: 13},
	}
	for _, row := range rows {
		slot := binary.LittleEndian.Uint16(row[0:2])
		want, ok := wantRows[slot]
		if !ok {
			continue
		}
		if itemID := binary.LittleEndian.Uint32(row[2:6]); itemID != want.itemID {
			t.Fatalf("slot %d item=%d want=%d row=%x", slot, itemID, want.itemID, row[:16])
		}
		if count := binary.LittleEndian.Uint32(row[6:10]); count != want.count {
			t.Fatalf("slot %d count=%d want=%d row=%x", slot, count, want.count, row[:16])
		}
		delete(wantRows, slot)
	}
	if len(wantRows) != 0 {
		t.Fatalf("authoritative list0 omitted slots=%v rows=%d body=%x", wantRows, len(rows), mainList.Body)
	}
	for range currentSelectedItemListTypes[1:] {
		_, rest = splitCurrentSceneItemListPacket(t, rest)
	}
	// Sorting moves whole rows; the client's slot-keyed lock icons would stay on
	// the vacated slots, so the rebuilt containers are followed by the durable
	// op251 snapshot (empty here: no locked row in this fixture).
	lockSnapshot, rest := splitGameServerUpperPacket(t, rest)
	if lockSnapshot.Header.MsgID != dnfitemlock.LockListMessageID || lockSnapshot.Header.Classification != 0 {
		t.Fatalf("lock snapshot header=%+v body=%x", lockSnapshot.Header, lockSnapshot.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing packets=%x", rest)
	}
}

// TestAccountSharedWithdrawalPublishesEmptyRowForEmptiedCrystalSlot 钉住"搬空晶体/灵魂
// 仓库一格要在 ACK 之后补一行 (itemID=0, count=0) 的'空格'行"。
//
// 现场 2026-09-17：把无色小晶块（3037）从晶体仓库（list 0 slot 358）拖到个人仓库之后，
// 客户端右侧晶体面板仍然显示原来的 40449，而且那一行点不动（东西其实已经不在仓库里）。
// 运营的对照很关键：**卖出晶体、拍卖行上架、摆摊交易都能正确减少晶体仓库数量**，只有拖拽
// 不行 —— 因为那三条路径清空这一格用的是 (itemID=0, count=0)（见拍卖行的
// sendCurrentAuctionInventorySlot 与摆摊 private_store 的 sourceUpdate），而拖拽路径发的是
// 0xFFFFFFFF 删除行，右侧晶体/灵魂面板不认它（0xFFFFFFFF 只对角色背包格有效）。只搬一部分
// 时不能补行，否则会多出一行（2026-09-13 的重复显示）。
func TestAccountSharedWithdrawalPublishesEmptyRowForEmptiedCrystalSlot(t *testing.T) {
	ctx := context.Background()
	crystalSlot := int16(dnfrepo.CrystalWarehouseFirstSlot + 4) // 358 = 无色小晶块
	cases := []struct {
		name             string
		moveCount        uint32
		wantRefreshes    []alignedcmd.ItemSlotRefresh
		wantCrystalCount int64
	}{
		{
			name:             "full withdrawal empties the cell",
			moveCount:        2_000,
			wantRefreshes:    []alignedcmd.ItemSlotRefresh{{ListType: dnfrepo.MainInventoryListType, SlotIndex: crystalSlot}},
			wantCrystalCount: 0,
		},
		{
			name:             "partial withdrawal keeps the cell occupied",
			moveCount:        1_000,
			wantRefreshes:    nil,
			wantCrystalCount: 1_000,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19",
				AccountID:   "dnf:crystal-emptied",
				Name:        "CrystalEmptied",
				Job:         "0",
				Level:       90,
				Stats:       map[string]int64{"gold": 1_000},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "19",
				Slots:       map[string]dnfrepo.ItemStack{},
				Warehouse:   map[string]dnfrepo.ItemStack{},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
				AccountID: "dnf:crystal-emptied",
				Slots: map[string]dnfrepo.ItemStack{
					dnfrepo.AccountSharedInventorySlotKey(crystalSlot): {
						ItemID: 3037,
						Count:  2_000,
						Extra: map[string]string{
							"item_kind":      "stackable",
							"pvf_path":       "stackable/material/cube_clear.stk",
							"stackable_type": "[material]",
							"stack_limit":    "99999",
						},
					},
				},
			}); err != nil {
				t.Fatal(err)
			}

			body := make([]byte, 28)
			body[0] = dnfrepo.MainInventoryListType
			binary.LittleEndian.PutUint16(body[1:3], uint16(crystalSlot))
			binary.LittleEndian.PutUint32(body[7:11], testCase.moveCount)
			body[11] = currentEquipmentPlacementListPersonalCargo
			binary.LittleEndian.PutUint16(body[12:14], uint16(8))
			binary.LittleEndian.PutUint32(body[22:26], math.MaxUint32)

			result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
				Command:             byte(dnfenum.GameCmdCommand),
				CommandKnown:        true,
				Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
				Body:                body,
				AccountID:           "dnf:crystal-emptied",
				SelectedCharacterID: 19,
				Repositories:        repositories,
			})
			if err != nil || !routed {
				t.Fatalf("route crystal withdrawal routed=%t err=%v", routed, err)
			}
			if len(result.ItemSlotRefreshes) != len(testCase.wantRefreshes) {
				t.Fatalf("incremental slot refreshes = %+v, want %+v",
					result.ItemSlotRefreshes, testCase.wantRefreshes)
			}
			for index := range testCase.wantRefreshes {
				if result.ItemSlotRefreshes[index] != testCase.wantRefreshes[index] {
					t.Fatalf("incremental slot refresh[%d] = %+v, want %+v",
						index, result.ItemSlotRefreshes[index], testCase.wantRefreshes[index])
				}
			}
			// 晶体/灵魂那一格的重绘只走"钱包 + 仍占用的晶体/灵魂行"，不重建整表容器快照
			// （拍卖行、摆摊那两条能正常减少数量的路径也只发一行）。
			gotContainersRefresh := false
			gotWalletsRefresh := false
			for _, action := range result.PostActions {
				switch action {
				case alignedcmd.PostActionRefreshSelectedItemContainers:
					gotContainersRefresh = true
				case alignedcmd.PostActionRefreshSelectedMainWallets:
					gotWalletsRefresh = true
				}
			}
			if !gotWalletsRefresh {
				t.Fatalf("crystal move post actions = %+v, want the wallet/crystal row repaint", result.PostActions)
			}
			if gotContainersRefresh {
				t.Fatalf("crystal move post actions = %+v, must not rebuild every container", result.PostActions)
			}
			account, _, err := repositories.AccountInventory.Load(ctx, "dnf:crystal-emptied")
			if err != nil {
				t.Fatal(err)
			}
			key := dnfrepo.AccountSharedInventorySlotKey(crystalSlot)
			if got := account.Slots[key].Count; got != testCase.wantCrystalCount {
				t.Fatalf("account crystal count = %d, want %d", got, testCase.wantCrystalCount)
			}

			connection := &bufferConn{}
			service := &Service{
				options: options{
					accountID:          "dnf:crystal-emptied",
					gameUpperHeader:    gameUpperHeaderServer16,
					gameUpperBodyCodec: gameUpperBodyCodecPlain,
				},
				repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
			}
			session := &gameSession{
				conn:                connection,
				connID:              "crystal-emptied-" + testCase.name,
				channel:             channelcatalog.Channel{ID: 19},
				selectedCharacterID: 19,
			}
			if err := service.sendAlignedUpperResponses(session, result); err != nil {
				t.Fatalf("sendAlignedUpperResponses error=%v", err)
			}
			ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) {
				t.Fatalf("crystal withdrawal ACK header=%+v", ack.Header)
			}
			if len(testCase.wantRefreshes) == 0 {
				// 部分取出：ACK 之后直接是钱包重绘，不应出现 358 的任何行。
				wallets, trailing := splitGameServerUpperPacket(t, rest)
				if wallets.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || len(trailing) != 0 {
					t.Fatalf("partial withdrawal packets: wallets=%+v trailing=%x", wallets.Header, trailing)
				}
				return
			}
			// 顺序：ACK → (itemID=0, count=0) 的空格行 → 钱包/占用行重绘，且再无其它包。
			rows, trailing := splitGameServerUpperPacket(t, rest)
			if rows.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
				rows.Header.Classification != 0 || len(rows.Body) < 6 {
				t.Fatalf("crystal empty-row packet header=%+v body=%x", rows.Header, rows.Body)
			}
			if rows.Body[3] != dnfrepo.MainInventoryListType {
				t.Fatalf("crystal empty-row list=%d body=%x", rows.Body[3], rows.Body)
			}
			emptyRows := decodeCurrentItemListTestRows(t, rows.Body[6:])
			if len(emptyRows) != 1 {
				t.Fatalf("crystal empty-cell refresh rows=%d want 1 body=%x", len(emptyRows), rows.Body)
			}
			if gotSlot, gotItem, gotCount := binary.LittleEndian.Uint16(emptyRows[0][0x00:0x02]), binary.LittleEndian.Uint32(emptyRows[0][0x02:0x06]), binary.LittleEndian.Uint32(emptyRows[0][0x06:0x0A]); gotSlot != uint16(crystalSlot) || gotItem != 0 || gotCount != 0 {
				t.Fatalf("crystal empty-cell row slot=%d item=%d count=%d want slot=%d item=0 count=0",
					gotSlot, gotItem, gotCount, crystalSlot)
			}
			wallets, trailing := splitGameServerUpperPacket(t, trailing)
			if wallets.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
				t.Fatalf("crystal move must repaint the wallet/crystal rows, got %+v", wallets.Header)
			}
			if len(trailing) != 0 {
				t.Fatalf("crystal move must not rebuild every container, trailing=%x", trailing)
			}
		})
	}
}

// TestWarehouseItemRetrievedFromPersonalCargoLandsInCrystalSlot 钉住运营口径
// 2026-09-17："只要是放回背包，就一定要放回晶体仓库"。
//
// 现场：把无色小晶块从个人仓库取回时，客户端把落点声明成材料背包格（0:156 / 0:121 /
// 0:150），于是晶体落在背包里、晶体仓库那一格还是空的。晶体/灵魂类道具（六个晶块
// 354..359、六个灵魂 360..365）在自己账号里只有一个固定格，服务端要按 ACK 回显真实
// 落点（op19 的 RedirectedReservedMainSlot），随后重绘那两排。
func TestWarehouseItemRetrievedFromPersonalCargoLandsInCrystalSlot(t *testing.T) {
	ctx := context.Background()
	clearSlot := int16(dnfrepo.CrystalWarehouseFirstSlot + 4) // 358 = 无色小晶块
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:cube-home",
		Name:        "CubeHome",
		Job:         "0",
		Level:       90,
		Stats:       map[string]int64{"gold": 1_000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	// 个人仓库 2:43 放 40 个无色小晶块（客户端取回时声明的落点是背包 0:121）。
	record, _, err := repositories.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	record.Warehouse = map[string]dnfrepo.ItemStack{
		"2:43": {
			ItemID: 3037,
			Count:  40,
			Extra: map[string]string{
				"item_kind":      "stackable",
				"pvf_path":       "stackable/material/cube_clear.stk",
				"stackable_type": "[material]",
				"stack_limit":    "99999",
			},
		},
	}
	if err := repositories.Inventory.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:cube-home",
		Slots:     map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 28)
	body[0] = 2 // personal cargo
	binary.LittleEndian.PutUint16(body[1:3], 43)
	binary.LittleEndian.PutUint32(body[7:11], 40)
	body[11] = dnfrepo.MainInventoryListType
	binary.LittleEndian.PutUint16(body[12:14], 121)
	binary.LittleEndian.PutUint32(body[22:26], math.MaxUint32)

	result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
		Command:                      byte(dnfenum.GameCmdCommand),
		CommandKnown:                 true,
		Opcode:                       uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                         body,
		AccountID:                    "dnf:cube-home",
		SelectedCharacterID:          19,
		Repositories:                 repositories,
		AccountWarehouseSlotResolver: currentAccountWarehouseFixedSlot,
		StackMergeResolver: func(itemID int64) (alignedcmd.StackMergeResolution, error) {
			return alignedcmd.StackMergeResolution{
				ItemID: itemID, Kind: "stackable",
				PVFPath: "stackable/material/cube_clear.stk", StackLimit: 99_999,
				SlotStart: 120, SlotEnd: 151,
			}, nil
		},
	})
	if err != nil || !routed {
		t.Fatalf("route cube retrieval routed=%t err=%v", routed, err)
	}
	if !result.ResponseAllowed {
		t.Fatalf("cube retrieval refused: %+v", result)
	}
	// 晶体归位后要重绘晶体/灵魂两排（ACK 自己不重绘那个面板）。
	foundWalletRefresh := false
	for _, action := range result.PostActions {
		if action == alignedcmd.PostActionRefreshSelectedMainWallets {
			foundWalletRefresh = true
		}
	}
	if !foundWalletRefresh {
		t.Fatalf("cube retrieval post actions = %+v, want the crystal/soul widget repaint", result.PostActions)
	}

	account, _, err := repositories.AccountInventory.Load(ctx, "dnf:cube-home")
	if err != nil {
		t.Fatal(err)
	}
	key := dnfrepo.AccountSharedInventorySlotKey(clearSlot)
	if stack := account.Slots[key]; stack.ItemID != 3037 || stack.Count != 40 {
		t.Fatalf("crystal warehouse after retrieval = %+v, want 3037 x40 at slot %d", stack, clearSlot)
	}
	record, _, err = repositories.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := record.Warehouse["2:43"]; exists {
		t.Fatalf("personal cargo still holds the cube: %+v", record.Warehouse)
	}
	if _, exists := record.Slots["0:121"]; exists {
		t.Fatalf("cube landed in the declared backpack slot: %+v", record.Slots)
	}

	// ACK 必须回显真实落点 0:358，客户端据此重画；否则它会把晶体画在背包 0:121。
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:cube-home",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn: connection, connID: "cube-home-retrieval",
		channel: channelcatalog.Channel{ID: 19}, selectedCharacterID: 19,
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error=%v", err)
	}
	ack, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || len(ack.Body) < 11 {
		t.Fatalf("cube retrieval ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	// server16 头模式会在 class-1 包体前面带固定的 3 个 0 字节；op19 布局是最后 11 字节。
	ackBody := ack.Body[len(ack.Body)-11:]
	if ackBody[0] != 1 {
		t.Fatalf("cube retrieval ACK is not a success: %x", ackBody)
	}
	if gotSrcList, gotSrcSlot := ackBody[1], binary.LittleEndian.Uint16(ackBody[2:4]); gotSrcList != 2 || gotSrcSlot != 43 {
		t.Fatalf("cube retrieval ACK source = %d:%d, want 2:43 (body=%x)", gotSrcList, gotSrcSlot, ackBody)
	}
	if gotCount := binary.LittleEndian.Uint32(ackBody[4:8]); gotCount != 40 {
		t.Fatalf("cube retrieval ACK count = %d, want 40 (body=%x)", gotCount, ackBody)
	}
	if gotDstList, gotDstSlot := ackBody[8], binary.LittleEndian.Uint16(ackBody[9:11]); gotDstList != dnfrepo.MainInventoryListType || gotDstSlot != uint16(clearSlot) {
		t.Fatalf("cube retrieval ACK destination = %d:%d, want %d:%d (body=%x)",
			gotDstList, gotDstSlot, dnfrepo.MainInventoryListType, clearSlot, ackBody)
	}
}

func TestPersonalCargoDepositRestoresGoldAndAccountSharedRowsAfterAck(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:cargo-wallets",
		Name:        "CargoWallets",
		Job:         "0",
		Level:       90,
		Stats:       map[string]int64{"gold": 7_268_086},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	stack := func(itemID, count int64) dnfrepo.ItemStack {
		return dnfrepo.ItemStack{
			ItemID: itemID,
			Count:  count,
			Extra: map[string]string{
				"item_kind":      "stackable",
				"pvf_path":       "stackable/material/cargo_wallet_test.stk",
				"stackable_type": "[material]",
				"stack_limit":    "99999",
			},
		}
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:cargo-wallets",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(354): stack(3033, 73),
			dnfrepo.AccountSharedInventorySlotKey(358): stack(3037, 2_000),
			dnfrepo.AccountSharedInventorySlotKey(362): stack(10099773, 115),
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 28)
	body[0] = dnfrepo.MainInventoryListType
	binary.LittleEndian.PutUint16(body[1:3], uint16(358))
	binary.LittleEndian.PutUint32(body[7:11], uint32(1_000))
	body[11] = currentEquipmentPlacementListPersonalCargo
	binary.LittleEndian.PutUint16(body[12:14], uint16(8))
	binary.LittleEndian.PutUint32(body[22:26], math.MaxUint32)
	result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
		Command:             byte(dnfenum.GameCmdCommand),
		CommandKnown:        true,
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                body,
		AccountID:           "dnf:cargo-wallets",
		SelectedCharacterID: 19,
		Repositories:        repositories,
	})
	if err != nil || !routed {
		t.Fatalf("route cargo deposit routed=%t err=%v", routed, err)
	}
	if !result.Handled || !result.ResponseAllowed ||
		!reflect.DeepEqual(result.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
		t.Fatalf("cargo deposit result=%+v", result)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:cargo-wallets",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "cargo-wallet-restore",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error=%v", err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("move ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	wallets, trailing := splitGameServerUpperPacket(t, rest)
	if wallets.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		wallets.Header.Classification != 0 || len(wallets.Body) < 6 ||
		!bytes.Equal(wallets.Body[:3], []byte{0, 0, 0}) ||
		wallets.Body[3] != dnfrepo.MainInventoryListType {
		t.Fatalf("wallet refresh header=%+v body=%x", wallets.Header, wallets.Body)
	}
	walletBody := wallets.Body[3:]
	rows := decodeCurrentItemListTestRows(t, walletBody[3:])
	want := map[uint16]struct {
		itemID uint32
		count  uint32
	}{
		0:   {itemID: 0, count: 7_268_086},
		354: {itemID: 3033, count: 73},
		358: {itemID: 3037, count: 1_000},
		362: {itemID: 10099773, count: 115},
	}
	for _, row := range rows {
		slot := binary.LittleEndian.Uint16(row[0:2])
		expected, ok := want[slot]
		if !ok {
			t.Fatalf("unexpected wallet slot=%d row=%x", slot, row[:16])
		}
		if itemID := binary.LittleEndian.Uint32(row[2:6]); itemID != expected.itemID {
			t.Fatalf("wallet slot=%d item=%d want=%d", slot, itemID, expected.itemID)
		}
		if count := binary.LittleEndian.Uint32(row[6:10]); count != expected.count {
			t.Fatalf("wallet slot=%d count=%d want=%d", slot, count, expected.count)
		}
		delete(want, slot)
	}
	if len(want) != 0 || len(trailing) != 0 {
		t.Fatalf("wallet refresh missing=%v trailing=%x", want, trailing)
	}

	account, found, err := repositories.AccountInventory.Load(ctx, "dnf:cargo-wallets")
	if err != nil || !found || account.Slots[dnfrepo.AccountSharedInventorySlotKey(358)].Count != 1_000 {
		t.Fatalf("account crystal after deposit found=%t account=%+v err=%v", found, account, err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found || inventory.Warehouse["2:8"].ItemID != 3037 || inventory.Warehouse["2:8"].Count != 1_000 {
		t.Fatalf("personal cargo after deposit found=%t inventory=%+v err=%v", found, inventory, err)
	}
}

// TestAlignedCargoPadResetPostActionSendsOp391 扩容类操作（开通/升级账号金库、仓库扩容券）
// 在 ACK 之后必须先补一发 cargo-pad 重置（op391），否则客户端面板上新解锁的格子还画着锁
// （现场 2026-09-18：扩大账号金库后显示仍锁、实际能放东西，切一下仓库页才正常）。
func TestAlignedCargoPadResetPostActionSendsOp391(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{
		options: options{accountID: "dnf:cargo-pad", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
	}
	session := &gameSession{
		conn:                connection,
		connID:              "cargo-pad-reset",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendAlignedPostAction(
		session,
		"upgrade_account_cargo",
		alignedcmd.PostActionResetCargoPadBeforeItemLists,
	); err != nil {
		t.Fatalf("sendAlignedPostAction: %v", err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketCancelCargoPad) || packet.Header.Classification != 0 {
		t.Fatalf("packet=%+v, 期望 op391 class0", packet.Header)
	}
	wantTail := bytes.Repeat([]byte{0xff}, currentCancelCargoPadProtectedTailSize)
	if !bytes.HasSuffix(packet.Body, wantTail) {
		t.Fatalf("body 缺少 cargo-pad 保护尾: % X", packet.Body)
	}
}

func TestPersonalCargoSortRefreshesOnlyList2AndDoesNotReplayBackpack(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 9999, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			"2:7": {ItemID: 10008121, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
			"2:5": {ItemID: 7273, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope: dnfrepo.CharacterContainerStateScope("19"),
		Values: map[string]string{
			"main_list_param16":           "24",
			"personal_cargo_list_param16": "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
		Command: byte(dnfenum.GameCmdCommand), CommandKnown: true,
		Opcode: uint16(dnfenum.CmdPacketSortItem), Body: []byte{2, 11, 0},
		AccountID: "dnf:cargo-sort", SelectedCharacterID: 19, Repositories: repositories,
	})
	if err != nil || !routed {
		t.Fatalf("route routed=%t err=%v", routed, err)
	}
	if len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedPersonalCargo {
		t.Fatalf("result=%+v", result)
	}

	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "dnf:cargo-sort", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "cargo-sort-list2-only", channel: channelcatalog.Channel{ID: 19}, selectedCharacterID: 19}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses: %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSortItem) || ack.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("ack=%+v", ack.Header)
	}
	cargo, rest := splitCurrentSceneItemListPacket(t, rest)
	if len(cargo.Body) < 6 || cargo.Body[0] != 2 {
		t.Fatalf("cargo body=%x", cargo.Body)
	}
	rows := decodeCurrentItemListTestRows(t, cargo.Body[5:len(cargo.Body)-1])
	if len(rows) != 2 || binary.LittleEndian.Uint16(rows[0][0:2]) != 0 || binary.LittleEndian.Uint32(rows[0][2:6]) != 7273 ||
		binary.LittleEndian.Uint16(rows[1][0:2]) != 1 || binary.LittleEndian.Uint32(rows[1][2:6]) != 10008121 {
		t.Fatalf("cargo rows=%x", rows)
	}
	// The rebuilt list-2 container drops the client's slot-keyed lock model, so
	// the single-container refresh is followed by the durable op251 snapshot
	// and nothing else (no list0 replay).
	lockSnapshot, rest := splitGameServerUpperPacket(t, rest)
	if lockSnapshot.Header.MsgID != dnfitemlock.LockListMessageID || lockSnapshot.Header.Classification != 0 {
		t.Fatalf("lock snapshot header=%+v body=%x", lockSnapshot.Header, lockSnapshot.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected packet after list2 (list0 replay regression): %x", rest)
	}
}

// 宠物本体/宠物装备的穿脱只重建 list 7。list 0/1/2/38 在这次操作里一个字节都不会变，
// 但重发它们代价极大：当前 EXE 在 op19 ACK 之后要先把已经收到的整批容器表处理完，才
// 轮到宠物行与 mode0。2026-09-24 公网实测（30 秒 op1206 心跳标定的时钟差恒定 −4.384s、
// 抖动 ±10ms，说明链路没有在途延迟）：ACK 到达 11ms 就被处理，宠物行与 mode0 却晚了
// 1.65~1.71 秒——这 1.7 秒就是客户端在重建背包页；本机同一套表只要 ~70ms，因为环回的
// 电报一次性到达、客户端只重建一次。现场表现就是"脱下后背包图标要等一下才出现"
// "穿上后要几秒宠物才出现"。这个测试锁死"只发一张 list 7"，防止退回五表刷新。
func TestWornCreatureInventoryPostActionRebuildsOnlyList7(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			// list 0 特意放一行：以前它会陪跑重发（背包整表），现在必须一个包都不发。
			"0:5":  {ItemID: 9999, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
			"7:10": {ItemID: 400990199, Count: 1, Extra: map[string]string{"item_kind": "creature"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:pet-move",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "pet-move-list7-only",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendAlignedPostAction(
		session,
		"move_itemspace",
		alignedcmd.PostActionRefreshSelectedWornCreatureInventory,
	); err != nil {
		t.Fatalf("sendAlignedPostAction: %v", err)
	}
	petList, rest := splitCurrentSceneItemListPacket(t, connection.write.Bytes())
	// list 7 的体是 listType(1) + count(u16) + 行，没有 list 2 那种分组尾。
	if len(petList.Body) < 3 || petList.Body[0] != currentPetInventoryListType {
		t.Fatalf("pet list body=%x", petList.Body)
	}
	if got := binary.LittleEndian.Uint16(petList.Body[1:3]); got != 1 {
		t.Fatalf("pet list entry count=%d body=%x", got, petList.Body)
	}
	rows := decodeCurrentItemListTestRows(t, petList.Body[3:])
	if len(rows) != 1 {
		t.Fatalf("pet rows=%x", rows)
	}
	assertCurrentItemListTestRow(t, rows[0], 10, 400990199, 1)
	// op13 重建会丢掉客户端进程内的锁标记，所以单表重建之后照旧补发 op251。
	lockSnapshot, rest := splitGameServerUpperPacket(t, rest)
	if lockSnapshot.Header.MsgID != dnfitemlock.LockListMessageID || lockSnapshot.Header.Classification != 0 {
		t.Fatalf("lock snapshot header=%+v body=%x", lockSnapshot.Header, lockSnapshot.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected packet after list7 (unrelated container replay regression): %x", rest)
	}
}

// 宠物穿脱必须继续刷新战斗力：宠物属性会改变战斗力，少发 op413 客户端的战斗力面板就
// 停在上一次的值。这个断言与上面的"只重建 list 7"配套，防止收窄容器表时连战斗力投影
// 一起丢掉。
func TestWornCreatureInventoryPostActionStillTouchesCombatPower(t *testing.T) {
	if string(alignedcmd.PostActionRefreshSelectedWornCreatureInventory) != "refresh_selected_worn_creature_inventory" {
		t.Fatalf("post action wire value=%q", alignedcmd.PostActionRefreshSelectedWornCreatureInventory)
	}
	if !alignedPostActionTouchesCombatPower(alignedcmd.PostActionRefreshSelectedWornCreatureInventory) {
		t.Fatal("worn creature inventory refresh must still schedule the combat-power projection")
	}
	// 普通装备/背包路径的集合不能被这次收窄改动影响。
	if !alignedPostActionTouchesCombatPower(alignedcmd.PostActionRefreshSelectedItemContainers) {
		t.Fatal("shared container refresh must keep scheduling the combat-power projection")
	}
}

func TestSendAlignedUpperResponsesKeepsNativeSkinAvatarMoveAckThenMode0Only(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "20",
		AccountID:   "dnf:1",
		Name:        "Actor20",
		Job:         "0",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "20",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"8": {
				SlotIndex: 8,
				ItemID:    112580095,
				RawEntry:  make([]byte, currentItemListEntryWireSize),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "20",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "native-equipment-move-projection",
		channel:                         channelcatalog.Channel{ID: 20},
		selectedCharacterID:             20,
		initialTownRouteCharacterID:     20,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       20,
		connectionTownActorOwnerChannel: 20,
		townActorOwnerChannel:           20,
	}
	ackBody := []byte{1, 0, 31, 0, 1, 0, 0, 0, 3, 12, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketMoveItemspace),
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(ack.Body) != len(ackBody)+3 ||
		!bytes.Equal(ack.Body[:3], []byte{0, 0, 0}) ||
		!bytes.Equal(ack.Body[3:], ackBody) {
		t.Fatalf("move ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	mode0, trailing := splitGameServerUpperPacket(t, rest)
	if mode0.Header.MsgID != 2 || mode0.Header.Classification != 0 ||
		len(mode0.Body) < 4 || !bytes.Equal(mode0.Body[:3], []byte{0, 0, 0}) ||
		mode0.Body[3] != 0 {
		t.Fatalf("mode0 header=%+v body=%x", mode0.Header, mode0.Body)
	}
	if eventArg, ok := currentSceneObjectTailActorStateEventArgForLog(mode0.Body[3:]); !ok || eventArg != 0 {
		t.Fatalf("post-wear mode0 actor-state event arg=%d ok=%t, want suppressed zero", eventArg, ok)
	}
	if skinItemID, ok := currentSceneObjectAppearanceSlotItemID(mode0.Body[3:], 8); !ok || skinItemID != 112580095 {
		t.Fatalf("post-wear mode0 skin row=%d ok=%t, want 112580095", skinItemID, ok)
	}
	if len(trailing) != 0 {
		t.Fatalf("ordinary equipment move emitted container/list3 packets after mode0: %x", trailing)
	}
}

func TestSendAlignedUpperResponsesDoesNotSendPartialListsOrStaleMode0WhenRefreshPlanFails(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "0",
		Level:       1,
		Stats:       map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	failing := &failAfterInventoryLoads{
		InventoryRepository: repositories.Inventory,
		failAfter:           2,
	}
	repositories.Inventory = failing

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "aligned-refresh-plan-failure",
		channel:                         channelcatalog.Channel{ID: 253},
		selectedCharacterID:             19,
		initialTownRouteCharacterID:     19,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 253,
		townActorOwnerChannel:           253,
	}
	ackBody := []byte{1, 0, 9, 0, 1, 0, 0, 0, 3, 11, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedItemContainers,
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("post-commit projection failure disconnected session: %v", err)
	}
	ack, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != 19 ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, ackBody) {
		t.Fatalf("ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("failed five-list plan leaked partial list or stale mode0=%x", trailing)
	}
	if failing.loads <= failing.failAfter {
		t.Fatalf("inventory failure was not exercised loads=%d", failing.loads)
	}
}

func TestSendAlignedUpperResponsesKeepsCommittedAckWhenMode0WearStatusCannotLoadCharacter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "20",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {SlotIndex: 26, ItemID: 9001},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		connID:                      "aligned-equipment-missing-actor-post-action",
		channel:                     channelcatalog.Channel{ID: 19},
		selectedCharacterID:         19,
		initialTownRouteCharacterID: 19,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:   19,
		townActorOwnerChannel:       currentSceneObjectContext,
	}
	ackBody := []byte{1, 0, 9, 0, 1, 0, 0, 0, 3, 12, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("committed op19 ACK became fatal after deferred refresh: %v", err)
	}
	ack, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != 19 ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, ackBody) ||
		len(trailing) != 0 {
		t.Fatalf("committed ACK header=%+v body=%x trailing=%x", ack.Header, ack.Body, trailing)
	}
}

func TestCurrentTownSelfActorAppearanceReadyRequiresFinalizedLocalActor(t *testing.T) {
	session := &gameSession{
		selectedCharacterID:       19,
		townSceneReadyCharacterID: 19,
		townActorOwnerChannel:     currentSceneObjectContext,
	}
	if !currentTownSelfActorAppearanceReady(session) {
		t.Fatal("finalized local selected actor was not appearance-ready")
	}
	session.connectionTownActorOwnerChannel = 253
	session.townActorOwnerChannel = 253
	if !currentTownSelfActorAppearanceReady(session) {
		t.Fatal("finalized CHANNELINFO-owned selected actor was not appearance-ready")
	}
	session.townActorOwnerChannel = 7
	if currentTownSelfActorAppearanceReady(session) {
		t.Fatal("remote scene-context actor was appearance-ready")
	}
	session.townActorOwnerChannel = 253
	session.townSceneReadyCharacterID = 20
	if currentTownSelfActorAppearanceReady(session) {
		t.Fatal("stale different-character scene was appearance-ready")
	}
}

func TestSelectedActorAppearanceRefreshClearsUnequippedSlotsWithMode0ThenFullMode1(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "15",
		Level:       90,
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "aligned-unequip-mode0-refresh",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendSelectedActorAppearanceRefresh(session, "move_itemspace", "test_unequip"); err != nil {
		t.Fatal(err)
	}

	mode0Packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	const codecPrefix = 3
	if mode0Packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		mode0Packet.Header.Classification != 0 ||
		len(mode0Packet.Body) <= codecPrefix ||
		!bytes.Equal(mode0Packet.Body[:codecPrefix], []byte{0, 0, 0}) {
		t.Fatalf("unequip mode0 header=%+v body=%x", mode0Packet.Header, mode0Packet.Body)
	}
	assertCurrentTypedMode0FullAppearanceForTest(t, mode0Packet.Body[codecPrefix:], "Actor19", 3, currentActorMode0AppearanceEmptyItem)

	mode1Packet, trailing := splitGameServerUpperPacket(t, rest)
	if mode1Packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		mode1Packet.Header.Classification != 0 ||
		len(mode1Packet.Body) <= codecPrefix+currentMode1CreateCountOffset ||
		!bytes.Equal(mode1Packet.Body[:codecPrefix], []byte{0, 0, 0}) {
		t.Fatalf("unequip full mode1 header=%+v body=%x", mode1Packet.Header, mode1Packet.Body)
	}
	mode1 := mode1Packet.Body[codecPrefix:]
	if mode1[0] != 1 ||
		binary.LittleEndian.Uint16(mode1[21:23]) != 19 ||
		mode1[currentMode1CreateCountOffset] != 0 {
		t.Fatalf("unequip full mode1 body=%x", mode1)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected unequip op13/op357/trailing=%x", trailing)
	}
}

func TestSelectedActorMode1AppearanceRefreshClearsUnequippedSlots(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "15",
		Level:       90,
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "aligned-unequip-mode1-refresh",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendSelectedActorMode1AppearanceRefresh(session, "move_itemspace", "test_unequip"); err != nil {
		t.Fatal(err)
	}

	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	const codecPrefix = 3
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		packet.Header.Classification != 0 ||
		len(packet.Body) <= codecPrefix+currentMode1CreateCountOffset ||
		!bytes.Equal(packet.Body[:codecPrefix], []byte{0, 0, 0}) {
		t.Fatalf("unequip mode1 refresh header=%+v body=%x", packet.Header, packet.Body)
	}
	mode1 := packet.Body[codecPrefix:]
	if mode1[0] != 1 ||
		binary.LittleEndian.Uint16(mode1[21:23]) != 19 ||
		mode1[currentMode1CreateCountOffset] != 0 {
		t.Fatalf("unequip mode1 body=%x", mode1)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected unequip mode1 trailing=%x", trailing)
	}
}

func TestSendAlignedUpperResponsesRefreshesOnlyUsedStackableRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:5": {
				ItemID: 500,
				Count:  108,
				Extra: map[string]string{
					"item_kind":       "stackable",
					"amount":          "117",
					"amount_or_count": "117",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "stackable-use-incremental", selectedCharacterID: 19}
	ackBody := []byte{1, 5, 0, 0, 0x11, 0x22, 0x33, 0x44, 0xF4, 0x01, 0, 0}
	result := alignedcmd.Result{
		Operation: "use_stackable",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketUseStackable),
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{ListType: 0, SlotIndex: 5}},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseStackable) {
		t.Fatalf("ack header=%+v", ack.Header)
	}
	update, trailing := splitGameServerUpperPacket(t, rest)
	const codecPrefix = 3
	updateBody := update.Body[codecPrefix:]
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		update.Header.Classification != 0 ||
		len(updateBody) != 3+currentItemListEntryWireSize ||
		updateBody[0] != 0 ||
		binary.LittleEndian.Uint16(updateBody[1:3]) != 1 {
		t.Fatalf("incremental update header=%+v body=%x", update.Header, update.Body)
	}
	row := updateBody[3:]
	if binary.LittleEndian.Uint16(row[0:2]) != 5 ||
		binary.LittleEndian.Uint32(row[2:6]) != 500 ||
		binary.LittleEndian.Uint32(row[6:10]) != 108 {
		t.Fatalf("incremental row=%x", row)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected full-list/trailing packets=%x", trailing)
	}
}

func TestSendAlignedUpperResponsesRefreshesRepairAllEquipmentAsSingleRows(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	entries := make(map[string]dnfrepo.EquipmentEntry, 2)
	for _, slot := range []int16{12, 14} {
		itemID := int64(10000 + slot)
		raw := make([]byte, currentItemListEntryWireSize)
		binary.LittleEndian.PutUint16(raw[0:2], uint16(slot))
		binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
		binary.LittleEndian.PutUint16(raw[0x0B:0x0D], uint16(30+slot))
		entries[strconv.Itoa(int(slot))] = dnfrepo.EquipmentEntry{
			SlotIndex: slot,
			ItemID:    itemID,
			RawEntry:  raw,
		}
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: entries}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "repair-all-equipment-refresh", selectedCharacterID: 19}
	result := alignedcmd.Result{
		Operation: "repair_equipment",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketRepairEquipment),
			Body:           []byte{1, 0, 0, 0, 0, 3, 0xFF, 0xFF, 0, 0},
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		EquipmentSlotRefreshes: []int16{12, 14},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketRepairEquipment) {
		t.Fatalf("repair-all ACK header=%+v", ack.Header)
	}
	for _, wantSlot := range []int16{12, 14} {
		update, remaining := splitGameServerUpperPacket(t, rest)
		const codecPrefix = 3
		body := update.Body[codecPrefix:]
		if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
			update.Header.Classification != 0 ||
			len(body) != 3+currentItemListEntryWireSize ||
			body[0] != currentSocketListEquipment ||
			binary.LittleEndian.Uint16(body[1:3]) != 1 {
			t.Fatalf("slot %d update header=%+v body=%x", wantSlot, update.Header, update.Body)
		}
		row := body[3:]
		if int16(binary.LittleEndian.Uint16(row[0:2])) != wantSlot ||
			binary.LittleEndian.Uint32(row[2:6]) != uint32(10000+wantSlot) ||
			binary.LittleEndian.Uint16(row[0x0B:0x0D]) != uint16(30+wantSlot) {
			t.Fatalf("slot %d row=%x", wantSlot, row)
		}
		rest = remaining
	}
	if len(rest) != 0 {
		t.Fatalf("repair-all emitted trailing/full-list packets=%x", rest)
	}
}

func TestSendAlignedUpperResponsesRefreshesRemovedStackableAndReviveWalletRows(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: 1, Count: 4},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "revive-use-incremental", selectedCharacterID: 19}
	result := alignedcmd.Result{
		Operation: "use_stackable",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketUseStackable),
			Body:           []byte{1, 5, 0, 0, 0x11, 0x22, 0x33, 0x44, 42, 0, 0, 0},
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{
			{ListType: 0, SlotIndex: 5},
			{ListType: 0, SlotIndex: 1},
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	update, trailing := splitGameServerUpperPacket(t, rest)
	const codecPrefix = 3
	updateBody := update.Body[codecPrefix:]
	if len(updateBody) != 3+2*currentItemListEntryWireSize ||
		binary.LittleEndian.Uint16(updateBody[1:3]) != 2 {
		t.Fatalf("incremental update body=%x", update.Body)
	}
	removed := updateBody[3 : 3+currentItemListEntryWireSize]
	wallet := updateBody[3+currentItemListEntryWireSize:]
	if binary.LittleEndian.Uint16(removed[0:2]) != 5 ||
		binary.LittleEndian.Uint32(removed[2:6]) != math.MaxUint32 ||
		binary.LittleEndian.Uint32(removed[6:10]) != 0 {
		t.Fatalf("removed source row=%x", removed)
	}
	if binary.LittleEndian.Uint16(wallet[0:2]) != 1 ||
		binary.LittleEndian.Uint32(wallet[2:6]) != 1 ||
		binary.LittleEndian.Uint32(wallet[6:10]) != 4 {
		t.Fatalf("wallet row=%x", wallet)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected full-list/trailing packets=%x", trailing)
	}
}

func TestSendAlignedUpperResponsesRefreshesCrossContainerMergeRows(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse: map[string]dnfrepo.ItemStack{
			"2:8": {ItemID: 3227, Count: 5},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "cargo-merge-incremental", selectedCharacterID: 19}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketMoveItemspace),
			Body:           []byte{1, 0, 65, 0, 2, 0, 0, 0, 2, 8, 0},
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{
			{ListType: 0, SlotIndex: 65},
			{ListType: 2, SlotIndex: 8},
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, append([]byte{0, 0, 0}, result.UpperResponses[0].Body...)) {
		t.Fatalf("move ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	sourceUpdate, rest := splitGameServerUpperPacket(t, rest)
	destinationUpdate, trailing := splitGameServerUpperPacket(t, rest)
	const codecPrefix = 3
	sourceBody := sourceUpdate.Body[codecPrefix:]
	destinationBody := destinationUpdate.Body[codecPrefix:]
	if sourceUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(sourceBody) != 3+currentItemListEntryWireSize ||
		sourceBody[0] != 0 ||
		binary.LittleEndian.Uint16(sourceBody[1:3]) != 1 {
		t.Fatalf("source update header=%+v body=%x", sourceUpdate.Header, sourceUpdate.Body)
	}
	source := sourceBody[3:]
	if binary.LittleEndian.Uint16(source[0:2]) != 65 ||
		binary.LittleEndian.Uint32(source[2:6]) != math.MaxUint32 ||
		binary.LittleEndian.Uint32(source[6:10]) != 0 {
		t.Fatalf("source deletion row=%x", source)
	}
	if destinationUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(destinationBody) != 3+currentItemListEntryWireSize ||
		destinationBody[0] != 2 ||
		binary.LittleEndian.Uint16(destinationBody[1:3]) != 1 {
		t.Fatalf("destination update header=%+v body=%x", destinationUpdate.Header, destinationUpdate.Body)
	}
	destination := destinationBody[3:]
	if binary.LittleEndian.Uint16(destination[0:2]) != 8 ||
		binary.LittleEndian.Uint32(destination[2:6]) != 3227 ||
		binary.LittleEndian.Uint32(destination[6:10]) != 5 {
		t.Fatalf("destination merged row=%x", destination)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected full-list/trailing packets=%x", trailing)
	}
}

func TestSendAlignedPetWearAppendsOnlyCreatureState(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "15",
		Level:       1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {
				SlotIndex: 26,
				ItemID:    63000,
				RawEntry:  testCurrentEquippedCreatureEnchantRaw(26, 63000, 37, 10008705, 0),
				Extra: map[string]string{
					"value_a":                  "10008705",
					"pet_enchant_card_item_id": "10008705",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.PetEntry{
			"37": {
				PetKey:          "37",
				CreatureKey:     37,
				ItemID:          63000,
				Name:            "Pet37",
				NameRaw:         []byte("Pet37"),
				Satiety:         73,
				Level:           3,
				Exp:             12,
				SourceListType:  3,
				SourceSlotIndex: 26,
			},
		},
		EquippedKey: "37",
		TownDisplay: true,
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "aligned-pet-post-actions",
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		initialTownRouteCharacterID:     19,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
		selectedCreatureStateTableSent:  true,
	}
	ackBody := []byte{1, 7, 0x30, 0, 1, 0, 0, 0, 17, 26, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedCreatureState,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != 19 || ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, ackBody) {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	itemRefresh, rest := splitLongHengGameServerUpperPacket(t, rest)
	assertEquippedCreatureItemRefreshPacket(t, itemRefresh, 63000, 37, 10008705, 0)
	state, rest := splitLongHengGameServerUpperPacket(t, rest)
	wantStateBody := []byte{
		1,
		37, 0, 0, 0,
		73,
		0,
		12, 0, 0, 0,
		3,
		5, 0, 0, 0, 'P', 'e', 't', '3', '7',
		0,
	}
	if state.Header.MsgID != currentCreatureStateTableMsgID || state.Header.Classification != 0 ||
		!bytes.Equal(state.Body, wantStateBody) {
		t.Fatalf("op105 header=%+v body=%x want=%x", state.Header, state.Body, wantStateBody)
	}
	growth, rest := splitLongHengGameServerUpperPacket(t, rest)
	wantGrowth := []byte{3, 0, 12, 0, 0, 0}
	if growth.Header.MsgID != currentCreatureGrowthMsgID || growth.Header.Classification != 0 ||
		!bytes.Equal(growth.Body, wantGrowth) {
		t.Fatalf("growth header=%+v body=%x want=%x", growth.Header, growth.Body, wantGrowth)
	}
	if !session.selectedCreatureStateTableSent {
		t.Fatal("op105 state was not marked sent after successful write")
	}
	if len(rest) != 0 {
		t.Fatalf("pet wear appended packets after targeted slot26 op14/op105/op102=%x", rest)
	}
}

func TestSendAlignedPetUnequipAppendsOnlyCreatureTableWithoutGrowth(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "15",
		Level:       1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.PetEntry{
			"37": {
				PetKey:          "37",
				CreatureKey:     37,
				ItemID:          63000,
				Name:            "Pet37",
				NameRaw:         []byte("Pet37"),
				Satiety:         73,
				Level:           3,
				Exp:             12,
				SourceListType:  7,
				SourceSlotIndex: 48,
			},
		},
		EquippedKey: "",
		TownDisplay: false,
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "aligned-pet-unequip-post-actions",
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		initialTownRouteCharacterID:     19,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
		selectedCreatureStateTableSent:  true,
	}
	ackBody := []byte{1, 7, 0x30, 0, 1, 0, 0, 0, 17, 26, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedCreatureState,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != 19 ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, ackBody) {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	state, rest := splitLongHengGameServerUpperPacket(t, rest)
	if state.Header.MsgID != currentCreatureStateTableMsgID ||
		state.Header.Classification != 0 {
		t.Fatalf("op105 header=%+v body=%x", state.Header, state.Body)
	}
	if len(rest) != 0 {
		next, _ := splitLongHengGameServerUpperPacket(t, rest)
		t.Fatalf("pet unequip appended packet op=%d body=%x; op102 must be absent", next.Header.MsgID, next.Body)
	}
}

func TestRuntimeSceneReadySequenceDoesNotReplaySelectedContainers(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1", Name: "Actor19", Job: "15", Level: 1}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {ItemID: 700, Count: 1, RawEntry: []byte{0x70}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19"}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                          connection,
		connID:                        "post-hud-container-refresh",
		channel:                       channelcatalog.Channel{ID: 19},
		selectedCharacterID:           19,
		selectedUserInfoRefreshSent:   true,
		selectedItemListRefreshSent:   true,
		selectedRentalWalletStateSent: true,
	}
	if err := service.sendRuntimeSceneReadySequence(session, "test"); err != nil {
		t.Fatalf("sendRuntimeSceneReadySequence error = %v", err)
	}

	itemListCount := 0
	for stream := connection.write.Bytes(); len(stream) > 0; {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketLeaveParty) && packet.Header.Classification == 0 && len(packet.Body) != 0 {
			itemListCount++
		}
		stream = rest
	}
	if itemListCount != 0 {
		t.Fatalf("post-scene item-list refreshes=%d want=0; actor-bound pre-mode1 stage already owns the container bootstrap", itemListCount)
	}
}

func TestSendAlignedUpperResponsesSendsSkillInitAckBeforeCurrentSkillInfo(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "0",
		Level:       90,
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "19",
		Skills:      map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
		Points:      dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 90},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {
				Skills: map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
				Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 90},
			},
		},
		Layouts: map[int]dnfrepo.SkillLayout{0: {0: 46}, 1: {150: 46}},
	}); err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(ctx, initialEquipmentMemSource{
		"skill/skilllist.lst":    "0 `job0.lst`\n",
		"skill/job0.lst":         "46 `job0/initial.skl`\n",
		"skill/job0/initial.skl": "[skill type]\n`active`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		skillCatalog:       catalog,
		initialSPTable:     map[int]int{1: 100},
		initialTPTable:     map[int]int{50: 10},
	}
	session := &gameSession{
		conn:                connection,
		connID:              "aligned-skill-reset-post-action",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	result := alignedcmd.Result{
		Operation: "skill_init",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketSkillInit),
			Body:           []byte{1, 0, 1},
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedActorSkills},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSkillInit) || ack.Header.Classification != dnfproto.DefaultChannelClassification || !bytes.Equal(ack.Body, []byte{0, 0, 0, 1, 0, 1}) {
		t.Fatalf("skill init ack header=%+v body=%x", ack.Header, ack.Body)
	}
	refresh, trailing := splitGameServerUpperPacket(t, rest)
	if refresh.Header.MsgID != currentSkillInfoMsgID || refresh.Header.Classification != 0 {
		t.Fatalf("skill refresh header=%+v body=%x", refresh.Header, refresh.Body)
	}
	if len(refresh.Body) < 3 || !bytes.Equal(refresh.Body[:3], []byte{0, 0, 0}) {
		t.Fatalf("skill refresh codec prefix=%x", refresh.Body)
	}
	if slots := currentSkillInfoFirstTreeSlots(t, refresh.Body[3:]); len(slots) != 1 || slots[0] != 46 {
		t.Fatalf("skill refresh slots=%v, want slot0=46", slots)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", trailing)
	}
}

func TestAccountCargoAutoStackSendsAckWithRealLandingSlotThenAccountCargoSnapshot(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:account-auto-stack",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	stack := func(count int64) dnfrepo.ItemStack {
		return dnfrepo.ItemStack{
			ItemID: 3227,
			Count:  count,
			Extra: map[string]string{
				"item_kind":      "stackable",
				"pvf_path":       "stackable/material/test_3227.stk",
				"stackable_type": "[material]",
				"stack_limit":    "10",
			},
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:66": stack(3),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:account-auto-stack",
		Slots: map[string]dnfrepo.ItemStack{
			"12:6": stack(2),
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 28)
	body[0] = 12
	binary.LittleEndian.PutUint16(body[1:3], 6)
	binary.LittleEndian.PutUint32(body[7:11], 2)
	body[11] = 0
	binary.LittleEndian.PutUint16(body[12:14], 70)
	result, routed, err := gameAlignedRegistry.Route(ctx, alignedcmd.Request{
		Command:             byte(dnfenum.GameCmdCommand),
		CommandKnown:        true,
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                body,
		AccountID:           "dnf:account-auto-stack",
		SelectedCharacterID: 19,
		Repositories:        repositories,
	})
	if err != nil || !routed {
		t.Fatalf("route account cargo move routed=%t err=%v", routed, err)
	}
	if !result.Handled || !result.ResponseAllowed {
		t.Fatalf("aligned result=%+v", result)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:account-auto-stack",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "account-cargo-auto-stack", selectedCharacterID: 19}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error=%v", err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(ack.Body) < 14 || binary.LittleEndian.Uint32(ack.Body[7:11]) != 2 {
		t.Fatalf("move ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	// 真实落点是背包里已有的那一堆 0:66（客户端拖过去的是空格 0:70）；ACK 必须报 66。
	if got := binary.LittleEndian.Uint16(ack.Body[12:14]); got != 66 {
		t.Fatalf("move ACK destination slot=%d, want the real landing slot 66; body=%x", got, ack.Body)
	}
	// 角色容器不再补整 list 快照（那会把 list-0 的金币钱包行一起下发），
	// 所以 ACK 之后直接是金库列表快照。
	accountCargo, trailing := splitCurrentSceneItemListPacket(t, rest)
	if len(accountCargo.Body) < 9 || accountCargo.Body[0] != 12 ||
		binary.LittleEndian.Uint16(accountCargo.Body[7:9]) != 0 {
		t.Fatalf("account cargo body=%x", accountCargo.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing packets=%x", trailing)
	}
}
