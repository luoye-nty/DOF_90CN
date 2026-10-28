package itemlock

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestHandlerSortLockAndUnlockPersistIndependentState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "27",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 64): {
				ItemID: 1001,
				Count:  1,
				Extra:  map[string]string{"equipment_lock_state": lockStateActive},
			},
		},
	})

	request := alignedcmd.Request{
		Body:                []byte{listTypeMain, 0x40, 0x00},
		AccountID:           "acc-lock",
		SelectedCharacterID: 27,
		Repositories:        repos,
	}
	request.Opcode = uint16(dnfenum.CmdPacketRequestItemSortLock)
	locked, err := NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("sort lock: %v", err)
	}
	assertSortLockResponse(t, locked, dnfenum.CmdPacketRequestItemSortLock, listTypeMain, 0x40)
	stack := loadTestInventory(t, ctx, repos, "27").Slots[slotKey(listTypeMain, 64)]
	if !IsSortLocked(stack) || stack.Extra["equipment_lock_state"] != lockStateActive {
		t.Fatalf("independent lock state=%+v", stack.Extra)
	}

	lockedAgain, err := NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("sort lock repeat: %v", err)
	}
	assertSortLockResponse(t, lockedAgain, dnfenum.CmdPacketRequestItemSortLock, listTypeMain, 0x40)

	request.Opcode = uint16(dnfenum.CmdPacketRequestItemSortUnlock)
	unlocked, err := NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("sort unlock: %v", err)
	}
	assertSortLockResponse(t, unlocked, dnfenum.CmdPacketRequestItemSortUnlock, listTypeMain, 0x40)
	stack = loadTestInventory(t, ctx, repos, "27").Slots[slotKey(listTypeMain, 64)]
	if IsSortLocked(stack) || stack.Extra["equipment_lock_state"] != lockStateActive {
		t.Fatalf("sort unlock changed the wrong state: %+v", stack.Extra)
	}
}

// 重发位置表按表限定 list：角色 Slots 只认 0/1，Warehouse 只认 2（个人金库）。
// 个人金库漏了重发 → 锁虽然落了库，每次 op13 重建后客户端的蓝锁都会消失；
// 而 Warehouse 里的 12:* 是历史残留（账号金库唯一 durable owner 是 AccountInventory），
// 反过来不能被当成真的锁重发。
func TestSortLockPositionsFollowCurrentItemSlots(t *testing.T) {
	record := dnfrepo.InventoryRecord{
		Slots: map[string]dnfrepo.ItemStack{
			"1:7":  {ItemID: 2001, Count: 1, Extra: map[string]string{sortLockStateKey: "1"}},
			"0:64": {ItemID: 1001, Count: 1, Extra: map[string]string{sortLockStateKey: "1"}},
			"0:20": {ItemID: 1002, Count: 1},
			// 角色 Slots 里的 list 2 残留不属于个人金库，不参与重发。
			"2:4": {ItemID: 1003, Count: 1, Extra: map[string]string{sortLockStateKey: "1"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			"2:118": {ItemID: 490701740, Count: 2, Extra: map[string]string{sortLockStateKey: "1"}},
			"2:9":   {ItemID: 1004, Count: 1},
			"12:3":  {ItemID: 1005, Count: 1, Extra: map[string]string{sortLockStateKey: "1"}},
		},
	}
	got := SortLockPositions(record)
	want := []SortLockPosition{
		{ListType: 0, SlotIndex: 64},
		{ListType: 1, SlotIndex: 7},
		{ListType: 2, SlotIndex: 118},
	}
	if len(got) != len(want) {
		t.Fatalf("positions=%+v want=%+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("positions=%+v want=%+v", got, want)
		}
	}
}

// 这个用例原来拿 list 2 当"不支持的容器"，等于把个人金库上不了锁这件事固化成了期望值。
// 现在真正的 fail-close 对象是"行不在本 owner 仓储里"的容器：装备栏（3）与宠物栏（7）。
func TestHandlerSortLockRejectsUnsupportedContainerAndMalformedBody(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "27",
		Slots:       map[string]dnfrepo.ItemStack{slotKey(listTypePet, 4): {ItemID: 1001, Count: 1}},
		Warehouse:   map[string]dnfrepo.ItemStack{slotKey(listTypePersonalCargo, 4): {ItemID: 1002, Count: 1}},
	})
	for _, body := range [][]byte{
		{listTypeEquipment, 4, 0},
		{listTypePet, 4, 0},
		{listTypeMain, 4},
		{listTypeMain, 4, 0, 0},
	} {
		got, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode:              uint16(dnfenum.CmdPacketRequestItemSortLock),
			Body:                body,
			SelectedCharacterID: 27,
			Repositories:        repos,
		})
		if err != nil {
			t.Fatalf("Handle body=%x error=%v", body, err)
		}
		if !got.Handled || got.ResponseAllowed || len(got.UpperResponses) != 0 {
			t.Fatalf("body=%x result=%+v", body, got)
		}
	}
	loaded := loadTestInventory(t, ctx, repos, "27")
	if IsSortLocked(loaded.Slots[slotKey(listTypePet, 4)]) {
		t.Fatal("unsupported sort lock mutated a pet item")
	}
	if IsSortLocked(loaded.Warehouse[slotKey(listTypePersonalCargo, 4)]) {
		t.Fatal("unsupported sort lock mutated a personal cargo item")
	}
}

// assertSortLockResponse 校验 class1 成功 ACK 的线格式：success + list + i16 slot。
// 体布局在这里写死而不是复用 BuildSortLockStateBody，否则生产者改错格式时断言会跟着一起错。
func assertSortLockResponse(t *testing.T, result alignedcmd.Result, opcode dnfenum.CmdPacket, listType byte, slotIndex int16) {
	t.Helper()
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	response := result.UpperResponses[0]
	if response.MsgID != uint16(opcode) || response.Classification != dnfproto.DefaultChannelClassification || !response.AllowCodec {
		t.Fatalf("response=%+v", response)
	}
	want := []byte{1, listType, byte(slotIndex), byte(uint16(slotIndex) >> 8)}
	if string(response.Body) != string(want) {
		t.Fatalf("body=%x want=%x", response.Body, want)
	}
}
