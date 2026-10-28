package mail

import (
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 领取邮件附件不能落进角色还没解锁的背包格：活体 2026-09-15 出现过附件被塞进
// 未购买的行里，玩家在客户端连点都点不到。
//
// 窗口口径与 inventory.moveDestinationUnlockedSlotRange / dnfbridge.
// currentMainInventoryUnlockedEnd 完全一致：主背包标准分类 7 行 × 8 格 = 56 格，
// 前 32 格随角色解锁，之后按 CharacterContainerState.MainSlotCount(0/8/16/24) 逐行开放，
// 所以"最后一个已解锁格" = PVF 范围起点 + 32 - 1 + MainSlotCount，上界再 +1。
func TestClaimUnlockedMainSlotsWindowMatchesInventoryOwner(t *testing.T) {
	ctx := context.Background()
	// 材料分类 65..120：65 + 32 - 1 + MainSlotCount 是最后一格，上界为 +1。
	material := alignedcmd.MailboxItemResolution{
		Kind: "stackable", StackableType: "[material]",
		SlotStart: 65, SlotEnd: 120, StackLimit: 1,
	}
	cases := []struct {
		name          string
		mainSlotCount string
		want          int16
		why           string
	}{
		{"no expansion", "0", 97, "默认 4 行 ⇒ 最后一格 96"},
		{"one row bought", "8", 105, "5 行 ⇒ 最后一格 104"},
		{"two rows bought", "16", 113, "6 行 ⇒ 最后一格 112"},
		{"three rows bought", "24", 121, "7 行 ⇒ 最后一格 120（整个 PVF 范围）"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, _ := newMailClaimSlotTestOwner(t, ctx, tc.mainSlotCount)
			if got := owner.claimUnlockedMainSlots(ctx, "77", material); got != tc.want {
				t.Fatalf("main_list_param16=%s: bound=%d want=%d (%s)", tc.mainSlotCount, got, tc.want, tc.why)
			}
		})
	}
}

// fail-open：读不到容器状态、或该道具页不按行解锁（快捷栏 3..8、宠物/装扮/守护石页、
// 固定材料 345..359 等）时必须返回 0（= 不设界、保持旧行为）。算错窗口会挡掉全部领取，
// 比原 bug 更糟。
func TestClaimUnlockedMainSlotsFailsOpen(t *testing.T) {
	ctx := context.Background()
	owner, _ := newMailClaimSlotTestOwner(t, ctx, "0")
	material := alignedcmd.MailboxItemResolution{
		Kind: "stackable", StackableType: "[material]",
		SlotStart: 65, SlotEnd: 120, StackLimit: 1,
	}
	if got := owner.claimUnlockedMainSlots(ctx, "", material); got != 0 {
		t.Fatalf("空角色 id 必须 fail-open，实际 %d", got)
	}
	// 没有容器状态行的角色：不能凭空假设 4 行（那是"发明状态"）。
	bare := NewOwner(dnfrepomemory.NewMemoryGroup(), nil)
	if got := bare.claimUnlockedMainSlots(ctx, "78", material); got != 0 {
		t.Fatalf("无容器状态行必须 fail-open，实际 %d", got)
	}
	// 非 56 格区段一律不设界：快捷栏、固定材料页、宠物页、装扮页、守护石页。
	otherPages := []alignedcmd.MailboxItemResolution{
		{Kind: "stackable", SlotStart: 3, SlotEnd: 8, StackLimit: 1, PreferQuickSlots: true},
		{Kind: "stackable", SlotStart: 345, SlotEnd: 359, StackLimit: 1},
		{Kind: "equipment", SlotStart: 0, SlotEnd: 2, StackLimit: 1, ListType: 7},
		{Kind: "equipment", SlotStart: 0, SlotEnd: 2, StackLimit: 1, ListType: 1},
		{Kind: "equipment", SlotStart: 0, SlotEnd: 2, StackLimit: 1, ListType: 38},
		// 账号共享晶体/灵魂槽（354..365）属于账号侧 durable owner，不按角色行走。
		{Kind: "stackable", SlotStart: 354, SlotEnd: 365, StackLimit: 1},
	}
	for _, resolution := range otherPages {
		if got := owner.claimUnlockedMainSlots(ctx, "77", resolution); got != 0 {
			t.Fatalf("list=%d range=%d..%d 必须 fail-open，实际 %d",
				resolution.ListType, resolution.SlotStart, resolution.SlotEnd, got)
		}
	}
}

// addAttachmentToInventoryBounded 的第二个参数是**排他上界**（最后一格 + 1）：
// 未解锁格既不能被占用、也不能算作可用空间。
func TestAddAttachmentToInventoryRespectsUnlockedMainSlots(t *testing.T) {
	// 合成夹具，不依赖 PVF：主背包合法区段是 3..23，上界 8 表示"只有 3..7 已解锁"。
	resolution := alignedcmd.MailboxItemResolution{
		Kind:             "stackable",
		PVFPath:          "stackable/material/unlocked_slot.stk",
		StackableType:    "[material]",
		SlotStart:        3,
		SlotEnd:          23,
		StackLimit:       1,
		PreferQuickSlots: false,
	}
	attachment := dnfrepo.MailAttachment{ItemID: 700, Count: 1}

	t.Run("只落在已解锁行内", func(t *testing.T) {
		inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
		changed, err := addAttachmentToInventoryBounded(&inventory, attachment, resolution, 8)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(changed) != 1 || changed[0] < 3 || changed[0] >= 8 {
			t.Fatalf("落到未解锁格: changed=%v", changed)
		}
	})

	t.Run("已解锁格占满时报背包已满而不是塞进未解锁格", func(t *testing.T) {
		inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
		for slot := int16(3); slot < 8; slot++ {
			inventory.Slots[mailboxInventorySlotKey(dnfrepo.MainInventoryListType, slot)] =
				dnfrepo.ItemStack{ItemID: 900, Count: 1}
		}
		if _, err := addAttachmentToInventoryBounded(&inventory, attachment, resolution, 8); !errors.Is(err, ErrMailboxInventoryFull) {
			t.Fatalf("err=%v want ErrMailboxInventoryFull", err)
		}
		for key, stack := range inventory.Slots {
			if stack.ItemID == 700 {
				t.Fatalf("附件仍被放进 %s", key)
			}
		}
	})

	t.Run("未知解锁数时保持旧行为", func(t *testing.T) {
		inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
		for slot := int16(3); slot < 8; slot++ {
			inventory.Slots[mailboxInventorySlotKey(dnfrepo.MainInventoryListType, slot)] =
				dnfrepo.ItemStack{ItemID: 900, Count: 1}
		}
		changed, err := addAttachmentToInventoryBounded(&inventory, attachment, resolution, 0)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(changed) != 1 || changed[0] < 8 {
			t.Fatalf("未知解锁数时应允许更后面的格子: changed=%v", changed)
		}
	})
}

// 端到端：材料页 65..120，角色只买了 1 行扩展（main_list_param16=8，即第 5 行
// 97..104 已解锁）。把已解锁的 65..96 填满后领取，附件必须落在 97 而不是任何地方都行；
// 同一份背包在"0 行扩展"（只解锁 65..96）时必须报背包已满并保留附件。
func TestMailboxExtractPlacesAttachmentInsideUnlockedRowsOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		mainSlotCount string
		wantFull      bool
		wantSlot      int16
	}{
		{name: "one row bought places in slot 97", mainSlotCount: "8", wantSlot: 97},
		{name: "no expansion reports the bag full", mainSlotCount: "0", wantFull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repos := newMailClaimSlotTestOwner(t, ctx, tc.mainSlotCount)
			slots := make(map[string]dnfrepo.ItemStack)
			for slot := int16(65); slot <= 96; slot++ {
				slots[mailboxInventorySlotKey(dnfrepo.MainInventoryListType, slot)] =
					dnfrepo.ItemStack{ItemID: 9000 + int64(slot), Count: 1}
			}
			if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots}); err != nil {
				t.Fatalf("save occupied unlocked rows: %v", err)
			}
			if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
				CharacterID: "77",
				Mails: map[string]dnfrepo.MailRecord{
					"1": {MailID: "1", Attachments: []dnfrepo.MailAttachment{{ItemID: 2001, Count: 1}}},
				},
			}); err != nil {
				t.Fatalf("save mailbox: %v", err)
			}

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
				SelectedCharacterID: 77,
				Repositories:        repos,
				MailboxItemResolver: mailboxClaimTestResolver(65, 120, 1),
				Body:                currentMailIDList(1),
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			box, _, _ := repos.Mailbox.Load(ctx, "77")
			if tc.wantFull {
				// ackType 0x005F code 4：附件放不进去，客户端据此自己画"背包已满"。
				assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x04})
				if box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 1 {
					t.Fatalf("领取失败必须保留附件: %+v", box.Mails["1"])
				}
				return
			}

			inventory, _, _ := repos.Inventory.Load(ctx, "77")
			if stack, ok := inventory.Slots[mailboxInventorySlotKey(dnfrepo.MainInventoryListType, tc.wantSlot)]; !ok || stack.ItemID != 2001 {
				t.Fatalf("附件没有落在已解锁的 %d 格: key=%v stack=%+v", tc.wantSlot, ok, stack)
			}
			for slot := int16(105); slot <= 120; slot++ {
				if _, occupied := inventory.Slots[mailboxInventorySlotKey(dnfrepo.MainInventoryListType, slot)]; occupied {
					t.Fatalf("附件落进了未解锁的第 %d 格（只买了 1 行扩展）", slot)
				}
			}
			if !box.Mails["1"].Claimed {
				t.Fatalf("领取成功应结清整封信: %+v", box.Mails["1"])
			}
		})
	}
}

// 附件并进一行**锁定**的同类堆时必须被拒绝：装备锁的语义是不可交易/强化/分解/丢弃，
// 把锁定件数量悄悄加一等于绕过了锁。错误必须能被上层识别成"锁定"，而不是"背包已满"
// （后者会让玩家去清空背包，但问题根本不在空间）。
func TestMailboxExtractRejectsAttachmentsMergingIntoALockedRow(t *testing.T) {
	ctx := context.Background()
	_, repos := newMailClaimSlotTestOwner(t, ctx, "0")
	locked := dnfrepo.ItemStack{
		ItemID: 2001, Count: 1,
		Extra: map[string]string{"equipment_lock_state": "1"},
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			mailboxInventorySlotKey(dnfrepo.MainInventoryListType, 65): locked,
		},
	}); err != nil {
		t.Fatalf("save locked row: %v", err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Attachments: []dnfrepo.MailAttachment{{ItemID: 2001, Count: 1}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	resolution, err := mailboxClaimTestResolver(65, 120, 1)(2001)
	if err != nil {
		t.Fatalf("resolve attachment: %v", err)
	}
	if _, err := addAttachmentToInventoryBounded(
		&dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
			mailboxInventorySlotKey(dnfrepo.MainInventoryListType, 65): locked,
		}},
		dnfrepo.MailAttachment{ItemID: 2001, Count: 1},
		resolution,
		97,
	); !errors.Is(err, ErrMailboxItemLocked) {
		t.Fatalf("err=%v want ErrMailboxItemLocked", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 120, 1),
		Body:                currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	// ackType 0x005F 表里没有"锁定"这一格时用 4：客户端会自己跑一次背包检查并画提示。
	// 这里断言"确实回了失败码、而且不是 0"，码值本身由 mailboxClaimErrorCode 决定。
	body := got.UpperResponses[0].Body
	if len(body) != 2 || body[0] != 0x00 || body[1] == 0 {
		t.Fatalf("锁定领取必须回非零失败码，实际 % X", body)
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 1 {
		t.Fatalf("锁定被拒必须保留附件: %+v", box.Mails["1"])
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	if stack := inventory.Slots[mailboxInventorySlotKey(dnfrepo.MainInventoryListType, 65)]; stack.Count != 1 {
		t.Fatalf("锁定堆的数量被改成了 %d", stack.Count)
	}
}

// newMailClaimSlotTestOwner 建一个带容器状态行的角色 77：main_list_param16 就是
// CharacterContainerState.MainSlotCount（0/8/16/24）。
func newMailClaimSlotTestOwner(t *testing.T, ctx context.Context, mainSlotCount string) (Owner, dnfrepo.Group) {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc-a", Name: "receiver"})
	if err := repos.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope: dnfrepo.CharacterContainerStateScope("77"),
		Values: map[string]string{
			"main_list_param16":           mainSlotCount,
			"avatar_list_param16":         "0",
			"personal_cargo_list_param16": "8",
		},
	}); err != nil {
		t.Fatalf("save container state (main_list_param16=%s): %v", mainSlotCount, err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	return NewOwner(repos, nil), repos
}
