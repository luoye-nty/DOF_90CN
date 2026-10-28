package inventory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 本文件覆盖运营口径 2026-09-21 的两条规则：
//
//  1. 账号金库（list 12）只接受 PVF [attach type] 为 free / account / sealing 的道具，
//     其余（trade / trade delete / trade limit / sealing trade / 空值）以及"解析不出来"
//     一律拒绝 —— 这就是"任务道具、SS 装备被拖进金库"的根因：旧代码把入库资格整个交给
//     客户端，服务端任何一条分支都不判。参考口径见 86JP
//     `InventoryInsertService.ValidateCargoInsertCore` / `IsAccountCargoAttachTypeAllowed`，
//     客户端原版文案 dstr 13225「该物品无法转移至帐号金库」。
//
//  2. 跨容器一律不互换。互换会把对方容器那件搬进来（金库这边就是目标格上的不可交易道具
//     跟着进金库），而 op19 的 ACK 只带一个目标槽，客户端不知道被换出去的东西去了哪。
//     目标容器还有空格时走的是"先同类可叠、再第一个空格"的顺延落点，不会走到互换。

// accountCargoAttachResolver 是资格用例专用的运行时 PVF 桩：按 item id 回一条
// [attach type] 原样文本（含反引号与方括号，模拟 PVF 里的写法）；不在表里的 item 直接
// 报"解析不出来"，用来覆盖 fail-close 那一支。
func accountCargoAttachResolver(attachByItem map[int64]string) alignedcmd.StackMergeResolver {
	return func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		attach, ok := attachByItem[itemID]
		if !ok {
			return alignedcmd.StackMergeResolution{}, fmt.Errorf("test runtime PVF has no item %d", itemID)
		}
		return alignedcmd.StackMergeResolution{ItemID: itemID, AttachType: attach}, nil
	}
}

// accountCargoAttachResolverWithBagPage 同上，但额外声明主背包页 9..64（与真实 PVF 的
// 装备页同值）。跨容器落点顺延要靠这一页算"哪个格对这件道具真的可见"，没有它
// moveDestinationUnlockedSlotRange 会 fail-close，用例就考不到顺延本身了。
func accountCargoAttachResolverWithBagPage(attachByItem map[int64]string) alignedcmd.StackMergeResolver {
	return func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		attach, ok := attachByItem[itemID]
		if !ok {
			return alignedcmd.StackMergeResolution{}, fmt.Errorf("test runtime PVF has no item %d", itemID)
		}
		return alignedcmd.StackMergeResolution{
			ItemID:     itemID,
			AttachType: attach,
			SlotStart:  9,
			SlotEnd:    64,
		}, nil
	}
}

// newAccountCargoTestOwner 建一个带 PVF 桩的 owner（入库闸门要看解析结果）。
func newAccountCargoTestOwner(t *testing.T, repos dnfrepo.Group, resolver alignedcmd.StackMergeResolver) *Owner {
	t.Helper()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(resolver)
	return owner
}

// saveBagItem 往角色背包写一行。
func saveBagItem(t *testing.T, ctx context.Context, repos dnfrepo.Group, characterID string, slot int16, stack dnfrepo.ItemStack) {
	t.Helper()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots:       map[string]dnfrepo.ItemStack{slotKey(listTypeMain, slot): stack},
	})
}

// saveCargoItem 往账号金库写一行。
func saveCargoItem(t *testing.T, ctx context.Context, repos dnfrepo.Group, accountID string, slot int16, stack dnfrepo.ItemStack) {
	t.Helper()
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots:     map[string]dnfrepo.ItemStack{slotKey(listTypeAccountCargo, slot): stack},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMoveAccountCargoRejectsUntradeableAttachTypes 不可交易的 [attach type] 一律不让进金库，
// 而且两边都不许动（拒绝是整笔的，不留半截状态）。
func TestMoveAccountCargoRejectsUntradeableAttachTypes(t *testing.T) {
	for _, test := range []struct {
		name   string
		attach string
	}{
		{name: "trade", attach: "`[trade]`"},
		{name: "trade delete", attach: "`[trade delete]`"},
		{name: "trade limit", attach: "`[trade limit]`"},
		{name: "sealing trade", attach: "`[sealing trade]`"},
		{name: "empty", attach: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			const accountID = "dnf:attach-reject"
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, nil)
			saveBagItem(t, ctx, repos, "77", 65, pvfMoveStack(3330, 1, 0))

			owner := newAccountCargoTestOwner(t, repos, accountCargoAttachResolver(map[int64]string{3330: test.attach}))
			_, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID:           accountID,
				SelectedCharacterID: 77,
			}, MoveItemspaceRequest{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      65,
				MoveCount:            1,
				DestinationListType:  listTypeAccountCargo,
				DestinationSlotIndex: 0,
			}))
			if !errors.Is(err, ErrAccountCargoItemNotAllowed) {
				t.Fatalf("err = %v, 期望 ErrAccountCargoItemNotAllowed", err)
			}
			cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
			if len(cargo.Slots) != 0 {
				t.Fatalf("被拒绝的请求不许写金库: %+v", cargo.Slots)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if source := loaded.Slots[slotKey(listTypeMain, 65)]; source.ItemID != 3330 || source.Count != 1 {
				t.Fatalf("被拒绝后源行不许变化: %+v", source)
			}
		})
	}
}

// TestMoveAccountCargoAllowsWhitelistedAttachTypes 白名单三种（含 PVF 原样写法与已归一写法）
// 照旧可以入库 —— 这条是"不要修了 A 坏了 B"的护栏：free（可交易材料）、account（帐号绑定，
// 金库里现有几行就是它）、sealing（封装装备）都必须放行。
func TestMoveAccountCargoAllowsWhitelistedAttachTypes(t *testing.T) {
	for _, attach := range []string{"`[free]`", "`[account]`", "`[sealing]`", "free", "ACCOUNT"} {
		t.Run(attach, func(t *testing.T) {
			const accountID = "dnf:attach-allow"
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, nil)
			saveBagItem(t, ctx, repos, "77", 65, pvfMoveStack(3330, 1, 0))

			owner := newAccountCargoTestOwner(t, repos, accountCargoAttachResolver(map[int64]string{3330: attach}))
			result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID:           accountID,
				SelectedCharacterID: 77,
			}, MoveItemspaceRequest{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      65,
				MoveCount:            1,
				DestinationListType:  listTypeAccountCargo,
				DestinationSlotIndex: 0,
			}))
			if err != nil {
				t.Fatalf("合法 attach type 被拒: %v", err)
			}
			if !result.Changed || result.Mode != "move" {
				t.Fatalf("result = %+v", result)
			}
			cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
			if got := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; got.ItemID != 3330 || got.Count != 1 {
				t.Fatalf("金库落点 = %+v", got)
			}
		})
	}
}

// TestMoveAccountCargoDepositFailsClosedWhenEligibilityCannotBeProven 读不出来就不让进：
// 没有解析器、模板解析报错、解析结果对不上这次物品行，三种都按不可交易处理。
func TestMoveAccountCargoDepositFailsClosedWhenEligibilityCannotBeProven(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolver alignedcmd.StackMergeResolver
	}{
		{name: "no resolver", resolver: nil},
		{name: "unresolved item", resolver: accountCargoAttachResolver(map[int64]string{})},
		{
			name: "resolution item mismatch",
			resolver: func(int64) (alignedcmd.StackMergeResolution, error) {
				return alignedcmd.StackMergeResolution{ItemID: 999999, AttachType: "`[free]`"}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const accountID = "dnf:attach-unproven"
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, nil)
			saveBagItem(t, ctx, repos, "77", 65, pvfMoveStack(3330, 1, 0))

			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			if test.resolver != nil {
				owner.SetStackMergeResolver(test.resolver)
			}
			_, err = owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID:           accountID,
				SelectedCharacterID: 77,
			}, MoveItemspaceRequest{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      65,
				MoveCount:            1,
				DestinationListType:  listTypeAccountCargo,
				DestinationSlotIndex: 0,
			}))
			if !errors.Is(err, ErrAccountCargoItemNotAllowed) {
				t.Fatalf("err = %v, 期望 ErrAccountCargoItemNotAllowed（fail-close）", err)
			}
			cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
			if len(cargo.Slots) != 0 {
				t.Fatalf("不许写金库: %+v", cargo.Slots)
			}
		})
	}
}

// TestMoveAccountCargoRejectsUntradeableAutoStackIntoVault 自动叠加也是一条入库路径：
// 不可交易道具不能靠"并进金库已有的同类堆"进去。
func TestMoveAccountCargoRejectsUntradeableAutoStackIntoVault(t *testing.T) {
	const accountID = "dnf:attach-stack"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 3, 100),
	})
	saveBagItem(t, ctx, repos, "77", 65, pvfMoveStack(3330, 2, 100))

	owner := newAccountCargoTestOwner(t, repos, accountCargoAttachResolver(map[int64]string{3330: "`[trade]`"}))
	_, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           accountID,
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      65,
		MoveCount:            2,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 4,
	}))
	if !errors.Is(err, ErrAccountCargoItemNotAllowed) {
		t.Fatalf("err = %v, 期望 ErrAccountCargoItemNotAllowed", err)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if got := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; got.Count != 3 {
		t.Fatalf("金库已有堆不许被叠加: %+v", got)
	}
}

// TestMoveAccountCargoReverseRequestCannotSmuggleUntradeableIntoVault 客户端把已有物品写在
// dst 端的反向请求（与 2026-09-18 金库内反向上报同形）同样是一条入库路径，必须一起判。
func TestMoveAccountCargoReverseRequestCannotSmuggleUntradeableIntoVault(t *testing.T) {
	const accountID = "dnf:attach-reverse"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, nil)
	saveBagItem(t, ctx, repos, "77", 70, pvfMoveStack(3330, 1, 0))

	owner := newAccountCargoTestOwner(t, repos, accountCargoAttachResolver(map[int64]string{3330: "`[trade delete]`"}))
	_, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           accountID,
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		// src 是金库里的空格（服务端这里没有行），dst 才是背包里那件不可交易道具。
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      5,
		MoveCount:            1,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 70,
	}))
	if !errors.Is(err, ErrAccountCargoItemNotAllowed) {
		t.Fatalf("err = %v, 期望 ErrAccountCargoItemNotAllowed", err)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if len(cargo.Slots) != 0 {
		t.Fatalf("反向请求不许把背包道具塞进金库: %+v", cargo.Slots)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if source := loaded.Slots[slotKey(listTypeMain, 70)]; source.ItemID != 3330 || source.Count != 1 {
		t.Fatalf("被拒绝后背包行不许变化: %+v", source)
	}
}

// TestMoveAccountCargoWithdrawsAndMovesExistingUntradeableRows 存量里已经躺在金库的非法行
// 不能变成"搬不动的死账"：取出到背包、金库内部挪格都必须照旧放行（闸门只管进不管出）。
func TestMoveAccountCargoWithdrawsAndMovesExistingUntradeableRows(t *testing.T) {
	const accountID = "dnf:attach-existing"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{}})

	owner := newAccountCargoTestOwner(t, repos, accountCargoAttachResolver(map[int64]string{3330: "`[trade]`"}))

	// ① 金库内部挪格：两端都在 list 12，没有东西从外面进来。
	inner, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           accountID,
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      0,
		MoveCount:            1,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 1,
	}))
	if err != nil || !inner.Changed {
		t.Fatalf("金库内挪格被拒: result=%+v err=%v", inner, err)
	}

	// ② 取出到背包：不可交易道具必须还能拿出去（否则只能找 GM 清）。
	withdraw, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           accountID,
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      1,
		MoveCount:            1,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 70,
	}))
	if err != nil || !withdraw.Changed {
		t.Fatalf("取出存量非法行被拒: result=%+v err=%v", withdraw, err)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 70)]; got.ItemID != 3330 || got.Count != 1 {
		t.Fatalf("取出落点 = %+v", got)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 1)]; exists {
		t.Fatalf("取出后金库源行应被搬空: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoUntradeableDepositAnswersWithOp19FailureFrame handler 层必须回一帧
// 明确的失败包（失败码 17 = 通用拒绝，不是"物品栏空间不足"）：不回帧客户端 sub_1D2DE80 的
// pending 拖拽对象不释放，金库窗口会点不动。
func TestHandlerAccountCargoUntradeableDepositAnswersWithOp19FailureFrame(t *testing.T) {
	const accountID = "dnf:attach-frame"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, nil)
	saveBagItem(t, ctx, repos, "77", 65, pvfMoveStack(3330, 1, 0))

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 1, listTypeAccountCargo, 0, 0, 0, -1),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  accountCargoAttachResolver(map[int64]string{3330: "`[trade]`"}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回失败帧: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceRejectedErrorCode, listTypeMain, listTypeAccountCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if len(cargo.Slots) != 0 {
		t.Fatalf("被拒绝的请求不许写金库: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoQuickSlotSwapIsRejected 用户报告的现场：把金库里的道具拖到
// **人物快捷栏**上已有的道具上。旧代码对 list 0 的 3..8 与账号共享槽豁免了跨容器落点修正，
// 于是走到"互换"分支：快捷栏那件（任务道具 / SS 装备这类不可交易的）被写进金库。
// 现在跨容器一律不互换，操作直接取消，两边数据都不动。
func TestHandlerAccountCargoQuickSlotSwapIsRejected(t *testing.T) {
	for _, quickSlot := range []int16{3, 8} {
		t.Run(fmt.Sprintf("slot_%d", quickSlot), func(t *testing.T) {
			const accountID = "dnf:quickslot-swap"
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
				slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
			})
			// 快捷栏里是一件不可交易道具（任务道具一类）：它就是被换进金库的那件。
			saveBagItem(t, ctx, repos, "77", quickSlot, pvfMoveStack(3331, 1, 0))

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
				Body:                currentMoveItemspaceBody(listTypeAccountCargo, 0, 0, 1, listTypeMain, quickSlot, 0x24, 0, -1),
				AccountID:           accountID,
				SelectedCharacterID: 77,
				Repositories:        repos,
				StackMergeResolver:  accountCargoAttachResolver(map[int64]string{3330: "`[free]`", 3331: "`[trade delete]`"}),
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
				t.Fatalf("必须回失败帧: %+v", got)
			}
			wantFailure := []byte{0, moveItemspaceRejectedErrorCode, listTypeAccountCargo, listTypeMain}
			if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
				t.Fatalf("failure ack=% X, want % X", body, wantFailure)
			}
			cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
			if len(cargo.Slots) != 1 {
				t.Fatalf("金库不允许出现第二行: %+v", cargo.Slots)
			}
			if stayed := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; stayed.ItemID != 3330 {
				t.Fatalf("金库源行不许变化: %+v", stayed)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if quick := loaded.Slots[slotKey(listTypeMain, quickSlot)]; quick.ItemID != 3331 || quick.Count != 1 {
				t.Fatalf("快捷栏那件不许被换走: %+v", quick)
			}
		})
	}
}

// TestHandlerAccountCargoAccountSharedSlotSwapIsRejected 账号共享晶体/灵魂槽（0:354..365）
// 同样豁免了落点修正、旧代码也是互换：会把晶石那件换进金库、把金库那件塞进晶石固定格。
// 现在直接取消。
func TestHandlerAccountCargoAccountSharedSlotSwapIsRejected(t *testing.T) {
	const accountID = "dnf:shared-swap"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
	})
	saveBagItem(t, ctx, repos, "77", dnfrepo.CrystalWarehouseFirstSlot, pvfMoveStack(3037, 10, 0))

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 0, 0, 1, listTypeMain, dnfrepo.CrystalWarehouseFirstSlot, 0x24, 0, -1),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  accountCargoAttachResolver(map[int64]string{3330: "`[free]`", 3037: "`[account]`"}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	wantFailure := []byte{0, moveItemspaceRejectedErrorCode, listTypeAccountCargo, listTypeMain}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 ||
		!reflect.DeepEqual(got.UpperResponses[0].Body, wantFailure) {
		t.Fatalf("result = %+v, want failure % X", got, wantFailure)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if shared := loaded.Slots[slotKey(listTypeMain, dnfrepo.CrystalWarehouseFirstSlot)]; shared.ItemID != 3037 || shared.Count != 10 {
		t.Fatalf("共享槽那件不许被换走: %+v", shared)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if len(cargo.Slots) != 1 {
		t.Fatalf("金库不允许出现第二行: %+v", cargo.Slots)
	}
}

// TestMoveAccountCargoSameContainerSwapStillWorks 金库内部互换保持原语义（两端都在 list 12，
// 没有任何东西跨容器搬动）—— 跨容器不互换不能顺手把这条也砍掉。
func TestMoveAccountCargoSameContainerSwapStillWorks(t *testing.T) {
	const accountID = "dnf:inner-swap"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
		slotKey(listTypeAccountCargo, 2): pvfMoveStack(3331, 1, 0),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{}})

	owner := newAccountCargoTestOwner(t, repos, accountCargoAttachResolver(map[int64]string{3330: "`[trade]`", 3331: "`[free]`"}))
	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           accountID,
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      0,
		MoveCount:            1,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 2,
	}))
	if err != nil {
		t.Fatalf("金库内互换被拒: %v", err)
	}
	if !result.Changed || result.Mode != "swap" {
		t.Fatalf("result = %+v, 期望 swap", result)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if got := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; got.ItemID != 3331 {
		t.Fatalf("12:0 应为 3331: %+v", got)
	}
	if got := cargo.Slots[slotKey(listTypeAccountCargo, 2)]; got.ItemID != 3330 {
		t.Fatalf("12:2 应为 3330: %+v", got)
	}
}

// TestHandlerAccountCargoOccupiedBagSlotRedirectsToFreeSlotWithoutSwap 用户口径的第一条：
// 金库道具拖到背包格上已有的**不可交易**道具上时，背包有空位就不互换，直接找个空格放 ——
// 目标那件必须原地不动。
func TestHandlerAccountCargoOccupiedBagSlotRedirectsToFreeSlotWithoutSwap(t *testing.T) {
	const accountID = "dnf:bag-redirect"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
	})
	// 背包 0:70 上是一件不可交易道具（任务道具一类），它是"被拖到的目标"。
	saveBagItem(t, ctx, repos, "77", 70, pvfMoveStack(3331, 1, 0))
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		// 落点就是目标那件所在的 0:70（客户端声明目标格有物品：dstInst 非 0）。
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 0, 0, 1, listTypeMain, 70, 0x24, 0, -1),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver: accountCargoAttachResolverWithBagPage(map[int64]string{
			3330: "`[free]`",
			3331: "`[trade delete]`",
		}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	_, _, dstList, dstSlot := parseMoveSuccessAckEndpoints(t, got.UpperResponses[0].Body)
	if dstList != listTypeMain || dstSlot == 70 {
		t.Fatalf("ACK 落点 = (%d,%d), 期望背包里的空格而不是声明格 0:70", dstList, dstSlot)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if target := loaded.Slots[slotKey(listTypeMain, 70)]; target.ItemID != 3331 || target.Count != 1 {
		t.Fatalf("目标那件不许被换走: %+v", target)
	}
	if landed := loaded.Slots[slotKey(listTypeMain, dstSlot)]; landed.ItemID != 3330 || landed.Count != 1 {
		t.Fatalf("金库道具应落在 %d: %+v", dstSlot, landed)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if len(cargo.Slots) != 0 {
		t.Fatalf("金库源行应被搬空: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoFullBagCancelsInsteadOfSwapping 用户口径的第二条：背包没有空位、
// 目标又是不可交易道具时，不允许互换，操作取消，两边都不动。
func TestHandlerAccountCargoFullBagCancelsInsteadOfSwapping(t *testing.T) {
	const accountID = "dnf:bag-full"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
	})
	// 主背包解锁到 0:48（mainSlotCount=8），9..48 全部占满：目标格 0:70 不在这个范围里，
	// 所以把目标放在 9，其余填满 → 顺延找不到空格。
	slots := make(map[string]dnfrepo.ItemStack, 40)
	for slot := int16(9); slot <= 48; slot++ {
		slots[slotKey(listTypeMain, slot)] = pvfMoveStack(int64(5000+slot), 1, 0)
	}
	slots[slotKey(listTypeMain, 9)] = pvfMoveStack(3331, 1, 0)
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots})
	saveMoveContainerState(t, ctx, repos, "77", 8, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 0, 0, 1, listTypeMain, 9, 0x24, 0, -1),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver: accountCargoAttachResolverWithBagPage(map[int64]string{
			3330: "`[free]`",
			3331: "`[trade delete]`",
		}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回失败帧: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeAccountCargo, listTypeMain}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if target := loaded.Slots[slotKey(listTypeMain, 9)]; target.ItemID != 3331 || target.Count != 1 {
		t.Fatalf("目标那件不许被换走: %+v", target)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if len(cargo.Slots) != 1 {
		t.Fatalf("金库不许出现第二行: %+v", cargo.Slots)
	}
	if stayed := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; stayed.ItemID != 3330 || stayed.Count != 1 {
		t.Fatalf("金库源行不许变化: %+v", stayed)
	}
}
