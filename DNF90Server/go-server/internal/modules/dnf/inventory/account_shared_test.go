package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 2026-09-21 现场（玩家 1111，蓝色小晶块 3036×69）：
//
//	晶体仓库 0:357 → 个人仓库 2:117（正常）
//	个人仓库 2:117 → 背包，客户端声明落点 0:156 → 服务端按固定格送回 0:357，ACK 也如实
//	回执了……但客户端发包前已经把那件物品**乐观地画在 0:156** 上，而服务端从没提过 156；
//	0:156 于是留下一格点不动的幽灵行，**再往外拖时客户端连 op19 都不发**（日志：13:20:08
//	之后该连接零报文，DB 里 0:156 是空的、0:357 才是那 69 个）。
//
// 这条用例锁住修复：ACK 之后必须补一条"客户端声明格"的精确刷新行；那一格在持久化记录里是
// 空的，bridge 会把它写成背包空格的删除行 (0xFFFFFFFF)，客户端据此清掉幽灵行。
func TestHandlerMoveAccountSharedRedirectClearsClientDeclaredBagSlot(t *testing.T) {
	ctx := context.Background()
	const accountID = "dnf:redirect-clears-declared"
	clearCube, clearSlot := int64(3037), dnfrepo.CrystalWarehouseFirstSlot+4 // 0:358
	const declaredSlot = int16(156)

	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 117): pvfMoveStack(clearCube, 69, 99999),
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots:     map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		Body: currentMoveItemspaceBody(
			listTypePersonalCargo, 117, 0, 69,
			listTypeMain, declaredSlot, 0, 0, -1,
		),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver: func(itemID int64) (alignedcmd.StackMergeResolution, error) {
			return alignedcmd.StackMergeResolution{
				ItemID: itemID, Kind: "stackable",
				PVFPath: "stackable/material/cube.stk", StackLimit: 99_999,
				SlotStart: 120, SlotEnd: 151,
			}, nil
		},
		AccountWarehouseSlotResolver: func(itemID int64) (int16, bool) {
			if itemID == clearCube {
				return clearSlot, true
			}
			return 0, false
		},
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	body := got.UpperResponses[0].Body
	if len(body) != 11 || body[0] != 1 {
		t.Fatalf("move ACK body = % X", body)
	}
	if body[1] != listTypePersonalCargo || body[8] != listTypeMain {
		t.Fatalf("move ACK endpoints = list %d -> list %d, want %d -> %d",
			body[1], body[8], listTypePersonalCargo, listTypeMain)
	}
	if srcSlot := int16(binary.LittleEndian.Uint16(body[2:4])); srcSlot != 117 {
		t.Fatalf("move ACK source slot = %d, want 117", srcSlot)
	}
	if dstSlot := int16(binary.LittleEndian.Uint16(body[9:11])); dstSlot != clearSlot {
		t.Fatalf("move ACK destination slot = %d, want the fixed crystal cell %d", dstSlot, clearSlot)
	}

	wantRefresh := alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: declaredSlot}
	cleared := false
	for _, refresh := range got.ItemSlotRefreshes {
		if refresh == wantRefresh {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("ItemSlotRefreshes = %+v, want the client-declared bag slot %d cleared",
			got.ItemSlotRefreshes, declaredSlot)
	}
	// 光补删除行还不够：那一格在客户端的物品表里可能已经被 (itemID=0, count=0) 的"空格"行
	// 变成了金币对象（itemID=0 在这个客户端就是金币），增量行治不了它 —— 现场症状是
	// 面板看着正常、拖出来却是金币、客户端连 op19 都不发，小退（整份 list-0 快照重建）
	// 才恢复。所以落点被改写这一步必须补一次 list-0 整表重建，让权威快照最后落地。
	if !reflect.DeepEqual(got.ItemListRebuilds, []byte{listTypeMain}) {
		t.Fatalf("ItemListRebuilds = %v, want a list-0 rebuild after a redirected account-shared landing",
			got.ItemListRebuilds)
	}

	account, _, err := repos.AccountInventory.Load(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got := account.Slots[slotKey(listTypeMain, clearSlot)].Count; got != 69 {
		t.Fatalf("account crystal cell count = %d, want 69 (slots=%+v)", got, account.Slots)
	}
	record := loadTestInventory(t, ctx, repos, "77")
	if stack, occupied := record.Slots[slotKey(listTypeMain, declaredSlot)]; occupied && stack.ItemID != 0 && stack.Count > 0 {
		t.Fatalf("declared bag slot must stay empty durably: %+v", stack)
	}
	if _, occupied := record.Warehouse[slotKey(listTypePersonalCargo, 117)]; occupied {
		t.Fatalf("warehouse source row must be gone: %+v", record.Warehouse)
	}
}

// 反向守卫：客户端声明的落点上压着**别的**物品时（互换形态），落点照样改写到固定格，
// 但绝不能补"清格"刷新 —— 那一格在服务端有真实物品，删除行会把它从客户端抹掉。
func TestHandlerMoveAccountSharedRedirectKeepsOccupiedDeclaredSlot(t *testing.T) {
	ctx := context.Background()
	const accountID = "dnf:redirect-keeps-occupied"
	clearCube, clearSlot := int64(3037), dnfrepo.CrystalWarehouseFirstSlot+4
	const declaredSlot = int16(156)

	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, declaredSlot): pvfMoveStack(3331, 4, 99999),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 117): pvfMoveStack(clearCube, 69, 99999),
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots:     map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		Body: currentMoveItemspaceBody(
			listTypePersonalCargo, 117, 0, 69,
			listTypeMain, declaredSlot, 0, 0, -1,
		),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver: func(itemID int64) (alignedcmd.StackMergeResolution, error) {
			return alignedcmd.StackMergeResolution{
				ItemID: itemID, Kind: "stackable",
				PVFPath: "stackable/material/cube.stk", StackLimit: 99_999,
				SlotStart: 120, SlotEnd: 151,
			}, nil
		},
		AccountWarehouseSlotResolver: func(itemID int64) (int16, bool) {
			if itemID == clearCube {
				return clearSlot, true
			}
			return 0, false
		},
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	for _, refresh := range got.ItemSlotRefreshes {
		if refresh.ListType == listTypeMain && refresh.SlotIndex == declaredSlot {
			t.Fatalf("occupied declared slot %d must not be cleared: %+v", declaredSlot, got.ItemSlotRefreshes)
		}
	}
	record := loadTestInventory(t, ctx, repos, "77")
	if stack := record.Slots[slotKey(listTypeMain, declaredSlot)]; stack.ItemID != 3331 || stack.Count != 4 {
		t.Fatalf("the occupying item must be untouched: %+v", stack)
	}
}

func TestMoveAccountSharedRejectsSecondPVFCharmInQuickSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	const accountID = "dnf:account-shared-charm"
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 4): {ItemID: 400360003, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	sharedSlot := dnfrepo.CrystalWarehouseFirstSlot
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, sharedSlot): {ItemID: 400360005, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{ItemID: itemID, Kind: "equipment", EquipmentType: "[charm]"}, nil
	})

	_, err = owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID: accountID, SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType: listTypeMain, SourceSlotIndex: sharedSlot, MoveCount: 1,
		DestinationListType: listTypeMain, DestinationSlotIndex: 3,
	}))
	if !errors.Is(err, ErrQuickSlotCharmAlreadyEquipped) {
		t.Fatalf("Move error = %v, want %v", err, ErrQuickSlotCharmAlreadyEquipped)
	}
	character, _, _ := repos.Inventory.Load(ctx, "77")
	if _, found := character.Slots[slotKey(listTypeMain, 3)]; found {
		t.Fatalf("character quick slot partially saved: %+v", character.Slots)
	}
	account, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if got := account.Slots[slotKey(listTypeMain, sharedSlot)].ItemID; got != 400360005 {
		t.Fatalf("account-shared source changed to %d", got)
	}
}

func TestMigrateAccountSharedSlotsMovesLegacyStackWithoutLosingState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	expires := time.Date(2027, time.March, 4, 5, 6, 7, 0, time.FixedZone("CST", 8*60*60))
	legacy := dnfrepo.ItemStack{
		ItemID:   3033,
		Count:    25,
		Bind:     true,
		ExpireAt: expires,
		RawEntry: []byte{0x77, 0x35, 0x40},
		Extra:    map[string]string{"kind": "crystal", "serial": "991"},
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":   {ItemID: 700, Count: 1},
			"0:354": legacy,
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if _, exists := character.Slots["0:354"]; exists {
		t.Fatalf("legacy account-shared slot remains on character: %+v", character.Slots)
	}
	if got := character.Slots["0:9"]; got.ItemID != 700 || got.Count != 1 {
		t.Fatalf("ordinary character slot changed: %+v", got)
	}
	account, exists, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil || !exists {
		t.Fatalf("Load account inventory exists=%t err=%v", exists, err)
	}
	got := account.Slots["0:354"]
	if got.ItemID != legacy.ItemID || got.Count != legacy.Count || got.Bind != legacy.Bind ||
		!got.ExpireAt.Equal(legacy.ExpireAt) || !bytes.Equal(got.RawEntry, legacy.RawEntry) || !maps.Equal(got.Extra, legacy.Extra) {
		t.Fatalf("migrated stack = %+v, want complete legacy state %+v", got, legacy)
	}
}

func TestMigrateAccountSharedSlotsRemovesIdenticalCharacterDuplicate(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	stack := dnfrepo.ItemStack{
		ItemID:   4012,
		Count:    9,
		Bind:     true,
		ExpireAt: time.Date(2028, time.January, 2, 3, 4, 5, 0, time.UTC),
		RawEntry: []byte{4, 0, 1, 2},
		Extra:    map[string]string{"kind": "soul"},
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{"0:360": stack},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots:     map[string]dnfrepo.ItemStack{"0:360": stack},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}
	if _, exists := loadTestInventory(t, ctx, repos, "77").Slots["0:360"]; exists {
		t.Fatal("identical character duplicate was not removed")
	}
	account, exists, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil || !exists {
		t.Fatalf("Load account inventory exists=%t err=%v", exists, err)
	}
	if got := account.Slots["0:360"]; !accountSharedStacksEqual(got, stack) {
		t.Fatalf("account owner changed: %+v", got)
	}
}

func TestMigrateAccountSharedSlotsConflictRollsBackEverySlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	first := dnfrepo.ItemStack{ItemID: 3033, Count: 5, RawEntry: []byte{3, 5, 4}, Extra: map[string]string{"origin": "legacy"}}
	conflictingLegacy := dnfrepo.ItemStack{ItemID: 3034, Count: 6, RawEntry: []byte{3, 5, 5}}
	conflictingAccount := dnfrepo.ItemStack{ItemID: 9999, Count: 1, RawEntry: []byte{9}}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:354": first,
			"0:355": conflictingLegacy,
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots:     map[string]dnfrepo.ItemStack{"0:355": conflictingAccount},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	err = owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77")
	if !errors.Is(err, ErrAccountSharedMigrationConflict) {
		t.Fatalf("MigrateAccountSharedSlots error = %v, want ErrAccountSharedMigrationConflict", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if !accountSharedStacksEqual(character.Slots["0:354"], first) ||
		!accountSharedStacksEqual(character.Slots["0:355"], conflictingLegacy) {
		t.Fatalf("character slots changed after conflict: %+v", character.Slots)
	}
	account, exists, loadErr := repos.AccountInventory.Load(ctx, "dnf:1")
	if loadErr != nil || !exists {
		t.Fatalf("Load account inventory exists=%t err=%v", exists, loadErr)
	}
	if _, exists := account.Slots["0:354"]; exists {
		t.Fatalf("earlier slot partially migrated despite later conflict: %+v", account.Slots)
	}
	if !accountSharedStacksEqual(account.Slots["0:355"], conflictingAccount) {
		t.Fatalf("conflicting account slot changed: %+v", account.Slots["0:355"])
	}
}

func TestMigrateAccountSharedSlotsValidatesOwnersAndKeys(t *testing.T) {
	ctx := context.Background()
	owner, err := NewOwner(dnfrepomemory.NewMemoryGroup())
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	if err := owner.MigrateAccountSharedSlots(ctx, "", "77"); !errors.Is(err, ErrAccountRequired) {
		t.Fatalf("empty account error = %v", err)
	}
	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", ""); !errors.Is(err, ErrCharacterRequired) {
		t.Fatalf("empty character error = %v", err)
	}
	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); !errors.Is(err, ErrInventoryNotFound) {
		t.Fatalf("missing character error = %v", err)
	}
}

// 运营口径 2026-09-17：晶体/灵魂类道具（六个晶块 + 六个灵魂，各有唯一固定仓库格）只能
// 住在账号的晶体/灵魂仓库里。客户端把"从个人仓库/账号金库取回背包"的落点声明成材料背包
// 格（现场 0:156 / 0:121 / 0:150），服务端必须把它改回固定格；固定格被别的道具占着、或者
// 并进去会超上限时保持原落点，绝不做跨容器互换。
func TestMoveAccountSharedRedirectsWarehouseItemAwayFromBackpack(t *testing.T) {
	ctx := context.Background()
	const accountID = "dnf:warehouse-redirect"
	clearCube, clearSlot := int64(3037), dnfrepo.CrystalWarehouseFirstSlot+4 // 0:358
	otherCube, otherSlot := int64(3033), dnfrepo.CrystalWarehouseFirstSlot   // 0:354
	cases := []struct {
		name            string
		sourceList      byte
		sourceSlot      int16
		sourceCount     int64
		destinationSlot int16
		// declaredSlotOccupant 非 0 时，在"客户端声明的那一格"上预置一件别的物品（互换形态）。
		declaredSlotOccupant int64
		accountSlots         map[string]dnfrepo.ItemStack
		wantDestination int16
		wantAccount     map[int16]int64
		wantRedirected  bool
		// wantClearedDeclaredSlot 是"客户端声明的那一格"：落点被改写到固定格之后，它必须被
		// 显式清掉，否则客户端会留下点不动的幽灵行（2026-09-21 现场 0:156）。只有该格在
		// 持久化记录里确实是空的才回报 —— 声明格被别的物品占着时必须是 0，不能删掉真实物品。
		wantClearedDeclaredSlot int16
	}{
		{
			name:       "storage withdrawal lands in the fixed crystal slot",
			sourceList: listTypePersonalCargo, sourceSlot: 43, sourceCount: 40, destinationSlot: 121,
			wantDestination: clearSlot, wantRedirected: true,
			wantAccount:             map[int16]int64{clearSlot: 40},
			wantClearedDeclaredSlot: 121,
		},
		{
			name:       "bag to bag move also lands in the fixed crystal slot",
			sourceList: listTypeMain, sourceSlot: 150, sourceCount: 7, destinationSlot: 151,
			wantDestination: clearSlot, wantRedirected: true,
			wantAccount:             map[int16]int64{clearSlot: 7},
			wantClearedDeclaredSlot: 151,
		},
		{
			name:       "existing warehouse stack is merged into",
			sourceList: listTypePersonalCargo, sourceSlot: 43, sourceCount: 40, destinationSlot: 121,
			accountSlots:    map[string]dnfrepo.ItemStack{slotKey(listTypeMain, clearSlot): pvfMoveStack(clearCube, 100, 99999)},
			wantDestination: clearSlot, wantRedirected: true,
			wantAccount:             map[int16]int64{clearSlot: 140},
			wantClearedDeclaredSlot: 121,
		},
		{
			name:       "the client's own warehouse destination is left alone",
			sourceList: listTypePersonalCargo, sourceSlot: 43, sourceCount: 40, destinationSlot: clearSlot,
			wantDestination: clearSlot, wantRedirected: false,
			wantAccount: map[int16]int64{clearSlot: 40},
		},
		{
			name:       "a foreign stack squatting on the fixed slot keeps the declared landing",
			sourceList: listTypePersonalCargo, sourceSlot: 43, sourceCount: 40, destinationSlot: 121,
			accountSlots:    map[string]dnfrepo.ItemStack{slotKey(listTypeMain, clearSlot): pvfMoveStack(otherCube, 5, 99999)},
			wantDestination: 121, wantRedirected: false,
			wantAccount: map[int16]int64{clearSlot: 5},
		},
		{
			// 声明格上压着别的物品（客户端想做互换）：落点照样改写到固定格，但**绝不能**
			// 回报清格 —— 那一格在服务端有真实物品，删除行会把它从客户端抹掉。
			name:       "declared landing occupied by another item is not cleared",
			sourceList: listTypePersonalCargo, sourceSlot: 43, sourceCount: 40, destinationSlot: 121,
			declaredSlotOccupant: 3331,
			accountSlots:         map[string]dnfrepo.ItemStack{},
			wantDestination:      clearSlot, wantRedirected: true,
			wantAccount: map[int16]int64{clearSlot: 40},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repos := dnfrepomemory.NewMemoryGroup()
			record := dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots:       map[string]dnfrepo.ItemStack{},
				Warehouse: map[string]dnfrepo.ItemStack{
					slotKey(listTypePersonalCargo, 43): pvfMoveStack(clearCube, testCase.sourceCount, 99999),
				},
			}
			if testCase.declaredSlotOccupant != 0 {
				record.Slots[slotKey(listTypeMain, testCase.destinationSlot)] =
					pvfMoveStack(testCase.declaredSlotOccupant, 1, 99999)
			}
			if testCase.sourceList == listTypeMain {
				record.Warehouse = map[string]dnfrepo.ItemStack{}
				record.Slots = map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, testCase.sourceSlot): pvfMoveStack(clearCube, testCase.sourceCount, 99999),
				}
			}
			if err := repos.Inventory.Save(ctx, record); err != nil {
				t.Fatal(err)
			}
			accountSlots := testCase.accountSlots
			if accountSlots == nil {
				accountSlots = map[string]dnfrepo.ItemStack{}
			}
			if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
				AccountID: accountID, Slots: accountSlots,
			}); err != nil {
				t.Fatal(err)
			}

			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
				return alignedcmd.StackMergeResolution{
					ItemID: itemID, Kind: "stackable",
					PVFPath: "stackable/material/cube.stk", StackLimit: 99_999,
					SlotStart: 120, SlotEnd: 151,
				}, nil
			})
			owner.SetAccountWarehouseSlotResolver(func(itemID int64) (int16, bool) {
				switch itemID {
				case clearCube:
					return clearSlot, true
				case otherCube:
					return otherSlot, true
				}
				return 0, false
			})

			result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID: accountID, SelectedCharacterID: 77,
			}, MoveItemspaceRequest{
				SourceListType: testCase.sourceList, SourceSlotIndex: testCase.sourceSlot,
				MoveCount:           int32(testCase.sourceCount),
				DestinationListType: listTypeMain, DestinationSlotIndex: testCase.destinationSlot,
			}))
			if err != nil {
				t.Fatalf("Move error = %v", err)
			}
			if result.DestinationSlotIndex != testCase.wantDestination {
				t.Fatalf("committed destination slot = %d, want %d (redirected=%t)",
					result.DestinationSlotIndex, testCase.wantDestination, result.RedirectedReservedMainSlot)
			}
			if result.RedirectedReservedMainSlot != testCase.wantRedirected {
				t.Fatalf("RedirectedReservedMainSlot = %t, want %t",
					result.RedirectedReservedMainSlot, testCase.wantRedirected)
			}
			if result.ClearedRedirectedMainSlot != testCase.wantClearedDeclaredSlot {
				t.Fatalf("ClearedRedirectedMainSlot = %d, want %d (redirected=%t declared=%d)",
					result.ClearedRedirectedMainSlot, testCase.wantClearedDeclaredSlot,
					result.RedirectedReservedMainSlot, testCase.destinationSlot)
			}
			account, _, err := repos.AccountInventory.Load(ctx, accountID)
			if err != nil {
				t.Fatal(err)
			}
			for slot, want := range testCase.wantAccount {
				if got := account.Slots[slotKey(listTypeMain, slot)].Count; got != want {
					t.Fatalf("account slot %d count = %d, want %d (slots=%+v)", slot, got, want, account.Slots)
				}
			}
			if !testCase.wantRedirected &&
				!dnfrepo.IsAccountSharedInventorySlot(listTypeMain, testCase.destinationSlot) {
				// 落点没被改：客户端声明的背包格必须真的收到那一行。
				record, _, _ := repos.Inventory.Load(ctx, "77")
				if got := record.Slots[slotKey(listTypeMain, testCase.destinationSlot)].Count; got != testCase.sourceCount {
					t.Fatalf("declared backpack slot %d count = %d, want %d (slots=%+v)",
						testCase.destinationSlot, got, testCase.sourceCount, record.Slots)
				}
			}
		})
	}
}
func TestMoveAccountSharedReportsEmptiedCrystalSlots(t *testing.T) {
	ctx := context.Background()
	const accountID = "dnf:account-shared-emptied"
	crystalSlot := dnfrepo.CrystalWarehouseFirstSlot + 4 // 358 = 无色小晶块
	cases := []struct {
		name         string
		request      MoveItemspaceRequest
		wantMode     string
		wantEmptied  []int16
		wantAccountN int64
	}{
		{
			name: "withdrawal to the backpack empties the crystal cell",
			request: MoveItemspaceRequest{
				SourceListType: listTypeMain, SourceSlotIndex: crystalSlot, MoveCount: 40,
				DestinationListType: listTypeMain, DestinationSlotIndex: 150,
			},
			wantMode:     "move",
			wantEmptied:  []int16{crystalSlot},
			wantAccountN: 0,
		},
		{
			name: "partial withdrawal keeps the cell occupied",
			request: MoveItemspaceRequest{
				SourceListType: listTypeMain, SourceSlotIndex: crystalSlot, MoveCount: 10,
				DestinationListType: listTypeMain, DestinationSlotIndex: 150,
			},
			wantMode:     "split",
			wantEmptied:  nil,
			wantAccountN: 30,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repos := dnfrepomemory.NewMemoryGroup()
			if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots:       map[string]dnfrepo.ItemStack{},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
				AccountID: accountID,
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, crystalSlot): {ItemID: 3037, Count: 40},
				},
			}); err != nil {
				t.Fatal(err)
			}
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
				return alignedcmd.StackMergeResolution{
					ItemID: itemID, Kind: "stackable",
					PVFPath: "stackable/material/cube_clear.stk", StackLimit: 100000,
					SlotStart: 120, SlotEnd: 151,
				}, nil
			})
			result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID: accountID, SelectedCharacterID: 77,
			}, testCase.request))
			if err != nil {
				t.Fatalf("Move error = %v", err)
			}
			if result.Mode != testCase.wantMode {
				t.Fatalf("move mode = %q, want %q", result.Mode, testCase.wantMode)
			}
			if !reflect.DeepEqual(result.EmptiedAccountSharedSlots, testCase.wantEmptied) {
				t.Fatalf("emptied account-shared slots = %v, want %v",
					result.EmptiedAccountSharedSlots, testCase.wantEmptied)
			}
			account, exists, loadErr := repos.AccountInventory.Load(ctx, accountID)
			if loadErr != nil || !exists {
				t.Fatalf("load account inventory exists=%t err=%v", exists, loadErr)
			}
			if got := account.Slots[slotKey(listTypeMain, crystalSlot)].Count; got != testCase.wantAccountN {
				t.Fatalf("account crystal count = %d, want %d", got, testCase.wantAccountN)
			}
		})
	}
}
