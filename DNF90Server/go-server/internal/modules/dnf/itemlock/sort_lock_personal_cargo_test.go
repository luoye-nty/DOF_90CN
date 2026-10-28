// 本文件覆盖个人金库（list 2）的整理锁定。
// 86JP IsSupportedLockListType 含 PersonalCargo，现场客户端也确实会在个人金库里发
// op713 list=2（2026-09-27 11:37:34 的 6 连拒），而 list 2 的行存在角色记录的
// Warehouse 而不是 Slots —— 取值表、落库字段、重发表三处都必须跟着容器走。
package itemlock

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// inventoryFieldRecorder 记录 SaveInventoryFields 真正传下去的字段。
// 内存仓储不实现 FieldStore，SaveFields 会退化成整条保存，所以只有字段本身能证明
// "个人金库的锁存进 warehouse、背包的锁存进 slots" —— 线上 mysqlBagStore.SaveFields
// 正是按这个值决定重建哪个集合（case InventoryFieldWarehouse 只重建 warehouse）。
type inventoryFieldRecorder struct {
	inner  dnfrepo.InventoryRepository
	fields []dnfrepo.InventoryField
}

func (r *inventoryFieldRecorder) Load(ctx context.Context, key string) (dnfrepo.InventoryRecord, bool, error) {
	return r.inner.Load(ctx, key)
}

func (r *inventoryFieldRecorder) Save(ctx context.Context, record dnfrepo.InventoryRecord) error {
	return r.inner.Save(ctx, record)
}

func (r *inventoryFieldRecorder) SaveFields(ctx context.Context, record dnfrepo.InventoryRecord, fields ...dnfrepo.InventoryField) error {
	r.fields = append(r.fields, fields...)
	return r.inner.Save(ctx, record)
}

// TestHandlerSortLockFlipsPersonalCargoRowInWarehouse 复刻现场的原始请求体
// （list=2 slot=118）：旧实现在这里回 "itemlock list type is not supported: 2"，
// 不放 ACK，客户端上锁没有任何反应。
func TestHandlerSortLockFlipsPersonalCargoRowInWarehouse(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "27",
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 118): {ItemID: 490701740, Count: 2},
		},
	})

	// 个人金库是角色容器，故意不带 AccountID：走账号 owner 的分支只服务于 list 12。
	locked, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketRequestItemSortLock),
		Body:                []byte{listTypePersonalCargo, 118, 0},
		SelectedCharacterID: 27,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle sort lock error = %v", err)
	}
	assertSortLockResponse(t, locked, dnfenum.CmdPacketRequestItemSortLock, listTypePersonalCargo, 118)

	record := loadTestInventory(t, ctx, repos, "27")
	if !IsSortLocked(record.Warehouse[slotKey(listTypePersonalCargo, 118)]) {
		t.Fatalf("个人金库的行没上锁: warehouse=%+v", record.Warehouse)
	}
	if len(record.Slots) != 0 {
		t.Fatalf("锁写进了角色 Slots: %+v", record.Slots)
	}

	unlocked, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketRequestItemSortUnlock),
		Body:                []byte{listTypePersonalCargo, 118, 0},
		SelectedCharacterID: 27,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle sort unlock error = %v", err)
	}
	assertSortLockResponse(t, unlocked, dnfenum.CmdPacketRequestItemSortUnlock, listTypePersonalCargo, 118)
	if IsSortLocked(loadTestInventory(t, ctx, repos, "27").Warehouse[slotKey(listTypePersonalCargo, 118)]) {
		t.Fatal("个人金库的行解锁后仍然锁着")
	}
}

// TestApplySortLockPersonalCargoPicksWarehouseMapAndField 用同名残留行证明取值确实走了
// Warehouse（读 Slots 会把锁写到残留行上），并钉住落库字段是 warehouse 而不是 slots。
func TestApplySortLockPersonalCargoPicksWarehouseMapAndField(t *testing.T) {
	ctx := context.Background()
	base := dnfrepomemory.NewMemoryGroup()
	if err := base.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "27",
		Slots: map[string]dnfrepo.ItemStack{
			// 同 key 的 Slots 残留：只有取错表才会命中它。
			slotKey(listTypePersonalCargo, 118): {ItemID: 1, Count: 1},
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 118): {ItemID: 490701740, Count: 2},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	recorder := &inventoryFieldRecorder{inner: base.Inventory}
	owner, err := NewOwner(dnfrepo.Group{Inventory: recorder})
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.ApplySortLock(ctx, Command{
		Operation:           "request_item_sort_lock",
		SelectedCharacterID: 27,
		ListType:            listTypePersonalCargo,
		SlotIndex:           118,
	}, true)
	if err != nil {
		t.Fatalf("ApplySortLock error = %v", err)
	}
	if result.ListType != listTypePersonalCargo || result.SlotIndex != 118 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	if len(recorder.fields) != 1 || recorder.fields[0] != dnfrepo.InventoryFieldWarehouse {
		t.Fatalf("落库字段 = %+v, want [warehouse]", recorder.fields)
	}

	loaded, found, err := base.Inventory.Load(ctx, "27")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if !IsSortLocked(loaded.Warehouse[slotKey(listTypePersonalCargo, 118)]) {
		t.Fatalf("金库行没上锁: %+v", loaded.Warehouse)
	}
	if IsSortLocked(loaded.Slots[slotKey(listTypePersonalCargo, 118)]) {
		t.Fatal("锁落到了 Slots 的同名残留行上")
	}
}

// TestApplySortLockBagAndAvatarKeepSavingSlotsField 是"改动没有波及背包/装扮栏"的守卫：
// 这两个容器必须继续走 Slots 表与 slots 字段，取值与落库路径与改动前逐位一致。
func TestApplySortLockBagAndAvatarKeepSavingSlotsField(t *testing.T) {
	ctx := context.Background()
	for _, testCase := range []struct {
		name     string
		listType byte
		slot     int16
	}{
		{name: "main bag", listType: listTypeMain, slot: 64},
		{name: "avatar", listType: listTypeAvatar, slot: 30},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			base := dnfrepomemory.NewMemoryGroup()
			if err := base.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "27",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(testCase.listType, testCase.slot): {ItemID: 490701734, Count: 1},
				},
			}); err != nil {
				t.Fatalf("seed inventory: %v", err)
			}
			recorder := &inventoryFieldRecorder{inner: base.Inventory}
			owner, err := NewOwner(dnfrepo.Group{Inventory: recorder})
			if err != nil {
				t.Fatalf("NewOwner error = %v", err)
			}

			if _, err := owner.ApplySortLock(ctx, Command{
				Operation:           "request_item_sort_lock",
				SelectedCharacterID: 27,
				ListType:            testCase.listType,
				SlotIndex:           testCase.slot,
			}, true); err != nil {
				t.Fatalf("ApplySortLock error = %v", err)
			}
			if len(recorder.fields) != 1 || recorder.fields[0] != dnfrepo.InventoryFieldSlots {
				t.Fatalf("落库字段 = %+v, want [slots]", recorder.fields)
			}
			loaded, _, err := base.Inventory.Load(ctx, "27")
			if err != nil {
				t.Fatalf("load inventory: %v", err)
			}
			if !IsSortLocked(loaded.Slots[slotKey(testCase.listType, testCase.slot)]) {
				t.Fatalf("list=%d slot=%d 没上锁", testCase.listType, testCase.slot)
			}
		})
	}
}

// TestApplySortLockPersonalCargoRejectsEmptySlot：空槽仍然 fail-close，不放 ACK。
func TestApplySortLockPersonalCargoRejectsEmptySlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "27",
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 118): {ItemID: 490701740, Count: 2},
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	if _, err := owner.ApplySortLock(ctx, Command{
		Operation:           "request_item_sort_lock",
		SelectedCharacterID: 27,
		ListType:            listTypePersonalCargo,
		SlotIndex:           119,
	}, true); err == nil {
		t.Fatal("空槽上锁没有报错")
	}
}
