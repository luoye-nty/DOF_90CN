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

// 这组回归锁定"一个容器里同一道具 id 的总量不得超过 PVF [stack limit]"：
// 邮件领取只能并进现有那一行，容器里已经有该道具的行时不许再开第二行；
// 没写 [stack limit] 的道具不受这条约束。

func mailClaimStackLimitResolution(start, end, stackLimit int64) alignedcmd.MailboxItemResolution {
	return alignedcmd.MailboxItemResolution{
		Kind:             "stackable",
		PVFPath:          "stackable/material/claim_limit.stk",
		StackableType:    "[material]",
		SlotStart:        int16(start),
		SlotEnd:          int16(end),
		StackLimit:       stackLimit,
		PreferQuickSlots: false,
	}
}

// 容器里已经有这道具的行、而且还有额度：必须并进那一行，不能另开一行。
func TestAddAttachmentMergesIntoExistingRowUnderContainerStackLimit(t *testing.T) {
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 1001, Count: 3, Extra: map[string]string{"item_kind": "stackable"}},
	}}
	changed, err := addAttachmentToInventory(&inventory, dnfrepo.MailAttachment{ItemID: 1001, Count: 2},
		mailClaimStackLimitResolution(65, 120, 1000))
	if err != nil {
		t.Fatalf("addAttachmentToInventory error = %v", err)
	}
	if len(changed) != 1 || changed[0] != 70 {
		t.Fatalf("changed slots = %v, want the existing row 70", changed)
	}
	if len(inventory.Slots) != 1 {
		t.Fatalf("claim opened a second row: %+v", inventory.Slots)
	}
	if stack := inventory.Slots["0:70"]; stack.Count != 5 {
		t.Fatalf("merged stack = %+v, want count 5", stack)
	}
}

// [stack limit] 1：容器里已经有一件时，第二件既不能另开一行也不能写进去，
// 按本路径既有规则整封拒绝（ErrMailboxInventoryFull）。
func TestAddAttachmentRefusesSecondRowForStackLimitOne(t *testing.T) {
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 1001, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
	}}
	changed, err := addAttachmentToInventory(&inventory, dnfrepo.MailAttachment{ItemID: 1001, Count: 1},
		mailClaimStackLimitResolution(65, 120, 1))
	if !errors.Is(err, ErrMailboxInventoryFull) {
		t.Fatalf("error = %v, want ErrMailboxInventoryFull", err)
	}
	if changed != nil {
		t.Fatalf("changed slots = %v, want nil", changed)
	}
	if len(inventory.Slots) != 1 || inventory.Slots["0:70"].Count != 1 {
		t.Fatalf("refused claim mutated the container: %+v", inventory.Slots)
	}
}

// 容器是空的但 [stack limit] 1 只允许一件：落点最多写一件，剩下的余量由本路径
// 既有规则报 ErrMailboxInventoryFull 交给调用方（领取事务会丢弃这份候选，信留原样）。
func TestAddAttachmentCapsAmountAtContainerStackLimit(t *testing.T) {
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	changed, err := addAttachmentToInventory(&inventory, dnfrepo.MailAttachment{ItemID: 1001, Count: 2},
		mailClaimStackLimitResolution(65, 120, 1))
	if !errors.Is(err, ErrMailboxInventoryFull) {
		t.Fatalf("error = %v, want ErrMailboxInventoryFull", err)
	}
	if changed != nil {
		t.Fatalf("changed slots = %v, want nil on the refused remainder", changed)
	}
	if len(inventory.Slots) != 1 || inventory.Slots["0:65"].Count != 1 {
		t.Fatalf("capped claim wrote container=%+v, want one row with exactly one unit", inventory.Slots)
	}
}

// 同一个上限在领取路径上的对外行为：两件 [stack limit] 1 的道具永远拿不到，
// 整封拒绝（ack 4），容器里一行都不留，信保留。
func TestMailboxExtractRefusesAttachmentAboveContainerStackLimit(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Attachments: []dnfrepo.MailAttachment{{ItemID: 2001, Count: 2}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(uint32) (alignedcmd.MailboxItemResolution, error) {
			return mailClaimStackLimitResolution(121, 176, 1), nil
		},
		Body: currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x04})
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 1 {
		t.Fatalf("capped claim must retain the attachment: %+v", box.Mails["1"])
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	if len(inventory.Slots) != 0 {
		t.Fatalf("capped claim wrote rows: %+v", inventory.Slots)
	}
}

// 没写 [stack limit] 的道具沿用旧行为：容器里那一行不在本次落点范围内时，
// 仍然可以在范围内开新行（有 [stack limit] 的道具此时必须拒绝）。
func TestAddAttachmentKeepsUncappedAndCappedBehaviorApart(t *testing.T) {
	uncapped := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 1001, Count: 5, Extra: map[string]string{"item_kind": "stackable"}},
	}}
	changed, err := addAttachmentToInventory(&uncapped, dnfrepo.MailAttachment{ItemID: 1001, Count: 3},
		mailClaimStackLimitResolution(65, 120, 0))
	if err != nil {
		t.Fatalf("uncapped addAttachmentToInventory error = %v", err)
	}
	if len(changed) != 1 || changed[0] != 65 {
		t.Fatalf("uncapped changed slots = %v, want a fresh row at 65", changed)
	}
	if stack := uncapped.Slots["0:65"]; stack.ItemID != 1001 || stack.Count != 3 {
		t.Fatalf("uncapped fresh row = %+v", stack)
	}
	if stack := uncapped.Slots["0:9"]; stack.Count != 5 {
		t.Fatalf("uncapped existing row was mutated: %+v", stack)
	}

	capped := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 1001, Count: 5, Extra: map[string]string{"item_kind": "stackable"}},
	}}
	if _, err := addAttachmentToInventory(&capped, dnfrepo.MailAttachment{ItemID: 1001, Count: 3},
		mailClaimStackLimitResolution(65, 120, 1000)); !errors.Is(err, ErrMailboxInventoryFull) {
		t.Fatalf("capped error = %v, want ErrMailboxInventoryFull", err)
	}
	if len(capped.Slots) != 1 {
		t.Fatalf("capped claim opened a second row: %+v", capped.Slots)
	}
}

// 领取路径的对外行为：[stack limit] 1 且容器里已有一件时整封拒绝（ack 4），
// 信件和附件都保留，容器不变。
func TestMailboxExtractRetainsLetterWhenStackLimitOneContainerIsFull(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:121": {ItemID: 2001, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
	}}); err != nil {
		t.Fatalf("save inventory: %v", err)
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
		MailboxItemResolver: func(uint32) (alignedcmd.MailboxItemResolution, error) {
			return mailClaimStackLimitResolution(121, 176, 1), nil
		},
		Body: currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x04})
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 1 {
		t.Fatalf("stack-limit rejection must retain the attachment: %+v", box.Mails["1"])
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	if len(inventory.Slots) != 1 || inventory.Slots["0:121"].Count != 1 {
		t.Fatalf("rejected claim mutated the container: %+v", inventory.Slots)
	}
}
