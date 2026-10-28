package mail

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestMailboxOpenBuildsCurrentCountAck(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1"},
			"2": {MailID: "2", Deleted: true},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxOpen),
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if len(got.UpperResponses) != 2 {
		t.Fatalf("response count = %d, want open ACK plus class0/0x61 snapshot", len(got.UpperResponses))
	}
	assertMailboxResponse(t, got.UpperResponses[0], uint16(dnfenum.CmdPacketMailboxOpen), dnfproto.DefaultChannelClassification, []byte{0x01, 0x00, 0x00})
	wantSnapshot := []byte{0x01, 0x00}
	wantSnapshot = appendU32(wantSnapshot, 1) // summary claim object / mail ID
	wantSnapshot = appendU32(wantSnapshot, 0) // sender character
	wantSnapshot = appendU32(wantSnapshot, 0) // sender DSTR
	wantSnapshot = appendU32(wantSnapshot, 0) // gold
	wantSnapshot = appendU32(wantSnapshot, 0) // item ID
	wantSnapshot = append(wantSnapshot, 0)    // has item
	wantSnapshot = appendU32(wantSnapshot, 0)
	wantSnapshot = append(wantSnapshot, 0, 0, 0) // u16 + u8
	wantSnapshot = appendU32(wantSnapshot, 0)
	wantSnapshot = append(wantSnapshot, 0, 0, 0, 0) // two u8 + u16
	wantSnapshot = append(wantSnapshot, make([]byte, mailboxItemRawSize)...)
	wantSnapshot = appendU32(wantSnapshot, 0) // remaining seconds
	wantSnapshot = appendU32(wantSnapshot, 0) // text-only summary seed
	wantSnapshot = append(wantSnapshot, 0)    // mail type
	wantSnapshot = append(wantSnapshot, 0, 0) // deferred page count
	wantSnapshot = append(wantSnapshot, 1, 0) // one detail row
	wantSnapshot = appendU32(wantSnapshot, 1)
	wantSnapshot = appendU32(wantSnapshot, 0)
	wantSnapshot = appendU32(wantSnapshot, 0) // sender DSTR
	wantSnapshot = appendU32(wantSnapshot, 0) // body DSTR
	wantSnapshot = appendU32(wantSnapshot, 0) // creation Unix time
	wantSnapshot = append(wantSnapshot, 1, 0, 0)
	assertMailboxResponse(t, got.UpperResponses[1], mailboxListNotificationMessageID, 0, wantSnapshot)
}

func TestMailboxRecipientCharacterListUsesOtherRolesFromSameAccount(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "77", AccountID: "local-account", Name: "发送者", Slot: 0},
		{CharacterID: "88", AccountID: "local-account", Name: "来来来", Slot: 1},
		{CharacterID: "99", AccountID: "other-account", Name: "别的账号", Slot: 0},
	} {
		mustSaveCharacter(t, repos, character)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketRequestServerCharacterList),
		Body:                []byte{3},
		AccountID:           "local-account",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	nameRaw, err := encodeMailboxText("来来来", mailboxRecipientListNameMaxBytes)
	if err != nil {
		t.Fatalf("encode expected role name: %v", err)
	}
	want := []byte{3, 1}
	want = appendU32(want, 88)
	want = appendU32(want, uint32(len(nameRaw)))
	want = append(want, nameRaw...)
	want = append(want, 0)
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("mailbox account-role result = %+v, want one response", got)
	}
	assertMailboxResponse(t, got.UpperResponses[0], mailboxRecipientListMessageID, 0, want)
}

func TestMailboxRecipientCharacterListRejectsMalformedRequestWithoutReply(t *testing.T) {
	got, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketRequestServerCharacterList),
		Body:   nil,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || got.ResponseAllowed || len(got.UpperResponses) != 0 {
		t.Fatalf("malformed mailbox account-role result = %+v, want handled without a reply", got)
	}
}

func TestMailboxSendTransfersGoldAndStoresRecipientMail(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
		Name:        "sender",
		Stats:       map[string]int64{"gold": 200},
	})
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "88", AccountID: "acc-b", Name: "receiver", Slot: 1})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxSend),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body: currentSendBody(
			"receiver",
			50,
			[]SendAttachment{{}},
			"body",
			0,
			false,
			false,
		),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxSend, []byte{0x01})
	if got.MailboxAlarmRecipientID != 88 {
		t.Fatalf("online mailbox alarm recipient = %d, want 88", got.MailboxAlarmRecipientID)
	}

	sender, ok, err := repos.Character.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load sender ok=%v error=%v", ok, err)
	}
	if sender.Stats["gold"] != 48 {
		t.Fatalf("sender gold = %d, want 48 after 50 transfer + 102 postage", sender.Stats["gold"])
	}
	box, ok, err := repos.Mailbox.Load(ctx, "88")
	if err != nil || !ok {
		t.Fatalf("recipient mailbox ok=%v error=%v", ok, err)
	}
	if len(box.Mails) != 1 {
		t.Fatalf("mail count = %d, want 1: %+v", len(box.Mails), box.Mails)
	}
	for id, mail := range box.Mails {
		if id != "1" || mail.MailID != "1" {
			t.Fatalf("wire mail id = %q/%q, want uint32-compatible 1", id, mail.MailID)
		}
		if mail.SenderCharacterID != "77" || mail.RecipientCharacterID != "88" ||
			mail.Title != "" || mail.Body != "body" || mail.Gold != 50 {
			t.Fatalf("unexpected mail: %+v", mail)
		}
	}
}

func TestMailboxSendAllowsDurablePVFItemWithZeroPrice(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "1",
		AccountID:   "local-account",
		Name:        "sender",
		Stats:       map[string]int64{"gold": 2000},
	})
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "3",
		AccountID:   "local-account",
		Name:        "receiver",
		Slot:        2,
	})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:125": {ItemID: 3166, Count: 3},
		},
	}); err != nil {
		t.Fatalf("save sender inventory: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxSend),
		SelectedCharacterID: 1,
		Repositories:        repos,
		MailboxItemResolver: func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
			if itemID != 3166 {
				t.Fatalf("resolver item = %d, want 3166", itemID)
			}
			return alignedcmd.MailboxItemResolution{
				Price:      0,
				PVFPath:    "stackable/reproduced-item.stk",
				Kind:       "stackable",
				SlotStart:  121,
				SlotEnd:    176,
				StackLimit: 1000,
			}, nil
		},
		Body: currentSendBody("receiver", 0, []SendAttachment{{
			ListType:  0,
			SlotIndex: 125,
			ItemID:    3166,
			Count:     3,
		}}, " ", 0, false, false),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxSend, []byte{0x01})

	sender, found, err := repos.Character.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load sender found=%v err=%v", found, err)
	}
	if got, want := sender.Stats["gold"], int64(900); got != want {
		t.Fatalf("sender gold = %d, want %d after fixed one-attachment postage", got, want)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load sender inventory found=%v err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:125"]; exists {
		t.Fatalf("sent zero-value attachment remains in sender inventory: %+v", inventory.Slots["0:125"])
	}
	box, found, err := repos.Mailbox.Load(ctx, "3")
	if err != nil || !found {
		t.Fatalf("load recipient mailbox found=%v err=%v", found, err)
	}
	mail := box.Mails["1"]
	if len(mail.Attachments) != 1 || mail.Attachments[0].ItemID != 3166 || mail.Attachments[0].Count != 3 {
		t.Fatalf("recipient attachment = %+v, want item 3166 x3", mail.Attachments)
	}
}

func TestMailboxSendThenRecipientOpenAndClaimCompletesSameAccountTransfer(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "local-account",
		Name:        "sender",
		Stats:       map[string]int64{"gold": 2000},
	})
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "88",
		AccountID:   "local-account",
		Name:        "receiver",
		Slot:        1,
		Stats:       map[string]int64{"gold": 10},
	})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": {ItemID: 1001, Count: 2, Bind: true},
		},
	}); err != nil {
		t.Fatalf("save sender inventory: %v", err)
	}

	send, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMultiMailboxSend),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
			if itemID != 1001 {
				t.Fatalf("sender resolver item = %d, want 1001", itemID)
			}
			return alignedcmd.MailboxItemResolution{Price: 100, SlotStart: 65, SlotEnd: 120, StackLimit: 99}, nil
		},
		Body: currentSendBody("receiver", 50, []SendAttachment{{ListType: 0, SlotIndex: 65, ItemID: 1001, Count: 2}}, "same-account transfer", 0, false, true),
	})
	if err != nil {
		t.Fatalf("send mail: %v", err)
	}
	assertOneMailboxResponse(t, send, dnfenum.CmdPacketMultiMailboxSend, []byte{0x01})

	opened, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxOpen),
		SelectedCharacterID: 88,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("open recipient mailbox: %v", err)
	}
	if len(opened.UpperResponses) != 2 {
		t.Fatalf("recipient open responses = %d, want ACK plus snapshot", len(opened.UpperResponses))
	}
	assertMailboxResponse(t, opened.UpperResponses[0], uint16(dnfenum.CmdPacketMailboxOpen), dnfproto.DefaultChannelClassification, []byte{0x01, 0x00, 0x00})

	claim, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 88,
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 120, 99),
		Body:                currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("claim recipient mail: %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, claim, currentExtractAck(1))

	receiver, found, err := repos.Character.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load receiver found=%v err=%v", found, err)
	}
	if got, want := receiver.Stats["gold"], int64(60); got != want {
		t.Fatalf("receiver gold = %d, want %d", got, want)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load recipient inventory found=%v err=%v", found, err)
	}
	if got := inventory.Slots["0:3"]; got.ItemID != 1001 || got.Count != 2 || !got.Bind {
		t.Fatalf("recipient claimed item = %+v, want bound item 1001 x2", got)
	}
	mailbox, found, err := repos.Mailbox.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load recipient mailbox found=%v err=%v", found, err)
	}
	if mail := mailbox.Mails["1"]; !mail.Claimed || !mail.Read || mail.Gold != 0 || len(mail.Attachments) != 0 {
		t.Fatalf("claimed mail did not clear durable assets: %+v", mail)
	}
}

func TestMailboxMultiAttachmentSendUsesDurableSlots(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
		Name:        "sender",
		Stats:       map[string]int64{"gold": 5000},
	})
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "88", AccountID: "acc-b", Name: "receiver"})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:3": {ItemID: 1001, Count: 5, Bind: true},
			"1:4": {ItemID: 2002, Count: 1, RawEntry: []byte{1, 2, 3}},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}

	attachments := []SendAttachment{
		{ListType: 0, SlotIndex: 3, ItemID: 1001, Count: 2},
		{ListType: 1, SlotIndex: 4, ItemID: 2002, Count: 1},
	}
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMultiMailboxSend),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
			return alignedcmd.MailboxItemResolution{Price: map[uint32]int64{1001: 100, 2002: 200}[itemID]}, nil
		},
		Body: currentSendBody("receiver", 0, attachments, "two", 0, false, true),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMultiMailboxSend, []byte{0x01})

	inventory, ok, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load sender inventory ok=%v error=%v", ok, err)
	}
	if stack := inventory.Slots["0:3"]; stack.Count != 3 || stack.ItemID != 1001 {
		t.Fatalf("remaining stack = %+v, want item 1001 x3", stack)
	}
	if _, exists := inventory.Slots["1:4"]; exists {
		t.Fatalf("fully attached slot should be removed: %+v", inventory.Slots["1:4"])
	}
	sender, _, _ := repos.Character.Load(ctx, "77")
	if sender.Stats["gold"] != 2880 {
		t.Fatalf("sender gold = %d, want 2880 after 2120 attachment postage", sender.Stats["gold"])
	}
	box, ok, err := repos.Mailbox.Load(ctx, "88")
	if err != nil || !ok {
		t.Fatalf("load recipient mailbox ok=%v error=%v", ok, err)
	}
	mail := box.Mails["1"]
	if len(mail.Attachments) != 2 ||
		mail.Attachments[0].ItemID != 1001 || mail.Attachments[0].Count != 2 ||
		mail.Attachments[1].ItemID != 2002 || !bytes.Equal(mail.Attachments[1].RawEntry, []byte{1, 2, 3}) {
		t.Fatalf("mail attachments = %+v", mail.Attachments)
	}
}

func TestMailboxSendRejectsForgedAttachmentWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		Name:        "sender",
		Stats:       map[string]int64{"gold": 100},
	})
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "88", Name: "receiver"})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{"0:3": {ItemID: 1001, Count: 2}},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxSend),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body: currentSendBody(
			"receiver",
			50,
			[]SendAttachment{{ListType: 0, SlotIndex: 3, ItemID: 9999, Count: 1}},
			"",
			0,
			false,
			false,
		),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxSend, []byte{0x00, 0x07})
	sender, _, _ := repos.Character.Load(ctx, "77")
	if sender.Stats["gold"] != 100 {
		t.Fatalf("rejected send changed gold to %d", sender.Stats["gold"])
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	if inventory.Slots["0:3"].Count != 2 {
		t.Fatalf("rejected send changed inventory: %+v", inventory.Slots["0:3"])
	}
	if _, found, loadErr := repos.Mailbox.Load(ctx, "88"); loadErr != nil || found {
		t.Fatalf("rejected send created mailbox found=%v err=%v", found, loadErr)
	}
}

func TestMailboxExtractClaimsGoldAndItemAtomicallyThenRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
		Name:        "receiver",
		Stats:       map[string]int64{"gold": 10},
	})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {
				MailID: "123",
				Gold:   50,
				Attachments: []dnfrepo.MailAttachment{
					{ItemID: 1001, Count: 2, Bind: true},
				},
			},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	request := alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 120, 99),
		Body:                currentMailIDList(123),
	}
	got, err := NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(123))
	if got.CharacterGoldAfter == nil || *got.CharacterGoldAfter != 60 {
		t.Fatalf("character gold projection = %v, want 60", got.CharacterGoldAfter)
	}
	if len(got.PostActions) != 0 {
		t.Fatalf("post actions = %v, want no full container refresh", got.PostActions)
	}
	if len(got.ItemSlotRefreshes) != 1 || got.ItemSlotRefreshes[0] != (alignedcmd.ItemSlotRefresh{ListType: 0, SlotIndex: 3}) {
		t.Fatalf("item slot refreshes = %+v, want claimed quick slot 0:3", got.ItemSlotRefreshes)
	}

	character, ok, err := repos.Character.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load character ok=%v error=%v", ok, err)
	}
	if character.Stats["gold"] != 60 {
		t.Fatalf("gold = %d, want 60", character.Stats["gold"])
	}
	inventory, ok, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load inventory ok=%v error=%v", ok, err)
	}
	stack := inventory.Slots["0:3"]
	if stack.ItemID != 1001 || stack.Count != 2 || !stack.Bind {
		t.Fatalf("slot 0:3 = %+v, want claimed attachment", stack)
	}
	box, ok, err := repos.Mailbox.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load mailbox ok=%v error=%v", ok, err)
	}
	if !box.Mails["123"].Claimed || !box.Mails["123"].Read || box.Mails["123"].Gold != 0 || len(box.Mails["123"].Attachments) != 0 {
		t.Fatalf("mail should be claimed/read: %+v", box.Mails["123"])
	}

	duplicate, err := NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("duplicate Handle error = %v", err)
	}
	// 86JP claim table (ackType 0x005F): a letter that no longer exists is 21.
	assertOneMailboxResponse(t, duplicate, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x15})
	character, _, _ = repos.Character.Load(ctx, "77")
	inventory, _, _ = repos.Inventory.Load(ctx, "77")
	if character.Stats["gold"] != 60 || inventory.Slots["0:3"].Count != 2 {
		t.Fatalf("duplicate claim mutated assets: gold=%d stack=%+v", character.Stats["gold"], inventory.Slots["0:0"])
	}
}

func TestMailboxExtractRoutesCrystalAndSoulAttachmentsToAccountInventory(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
		Name:        "receiver",
	})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {
				MailID: "123",
				Attachments: []dnfrepo.MailAttachment{
					{ItemID: 3001, Count: 2},
					{ItemID: 3002, Count: 3},
				},
			},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
			switch itemID {
			case 3001:
				return alignedcmd.MailboxItemResolution{
					Kind:       "stackable",
					PVFPath:    "stackable/material/crystal.stk",
					SlotStart:  dnfrepo.CrystalWarehouseFirstSlot,
					SlotEnd:    dnfrepo.CrystalWarehouseLastSlot,
					StackLimit: 9999,
				}, nil
			case 3002:
				return alignedcmd.MailboxItemResolution{
					Kind:       "stackable",
					PVFPath:    "stackable/material/soul.stk",
					SlotStart:  dnfrepo.SoulWarehouseFirstSlot,
					SlotEnd:    dnfrepo.SoulWarehouseLastSlot,
					StackLimit: 9999,
				}, nil
			default:
				return alignedcmd.MailboxItemResolution{}, ErrMailboxAssetMismatch
			}
		},
		Body: currentMailIDList(123),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	// Crystal and soul are two attachment slots, so the ACK reports both.
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAckSlots(123, 2))
	wantRefreshes := []alignedcmd.ItemSlotRefresh{
		{ListType: dnfrepo.MainInventoryListType, SlotIndex: dnfrepo.CrystalWarehouseFirstSlot},
		{ListType: dnfrepo.MainInventoryListType, SlotIndex: dnfrepo.SoulWarehouseFirstSlot},
	}
	if !reflect.DeepEqual(got.ItemSlotRefreshes, wantRefreshes) {
		t.Fatalf("item slot refreshes = %+v, want %+v", got.ItemSlotRefreshes, wantRefreshes)
	}

	accountInventory, found, err := repos.AccountInventory.Load(ctx, "acc-a")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%v err=%v", found, err)
	}
	if stack := accountInventory.Slots[dnfrepo.AccountSharedInventorySlotKey(dnfrepo.CrystalWarehouseFirstSlot)]; stack.ItemID != 3001 || stack.Count != 2 {
		t.Fatalf("crystal warehouse stack = %+v, want item=3001 count=2", stack)
	}
	if stack := accountInventory.Slots[dnfrepo.AccountSharedInventorySlotKey(dnfrepo.SoulWarehouseFirstSlot)]; stack.ItemID != 3002 || stack.Count != 3 {
		t.Fatalf("soul warehouse stack = %+v, want item=3002 count=3", stack)
	}
	if inventory, found, err := repos.Inventory.Load(ctx, "77"); err != nil || found || len(inventory.Slots) != 0 {
		t.Fatalf("account-owned mail created character inventory found=%v err=%v inventory=%+v", found, err, inventory)
	}
	mailbox, found, err := repos.Mailbox.Load(ctx, "77")
	if err != nil || !found || !mailbox.Mails["123"].Claimed || len(mailbox.Mails["123"].Attachments) != 0 {
		t.Fatalf("mailbox claim found=%v err=%v mail=%+v", found, err, mailbox.Mails["123"])
	}
}

func TestMailboxExtractCreditsEpicFragmentToAccountBookWithoutMaterialStack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
		Name:        "receiver",
	})
	if err := repos.EpicBook.Save(ctx, dnfrepo.EpicBookRecord{
		AccountID: "acc-a",
		Balances:  map[uint32]uint32{101020256: 10},
	}); err != nil {
		t.Fatalf("save epic book: %v", err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {
				MailID: "123",
				Attachments: []dnfrepo.MailAttachment{{
					ItemID: 10300528,
					Count:  7,
				}},
			},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
			if itemID != 10300528 {
				return alignedcmd.MailboxItemResolution{}, ErrMailboxAssetMismatch
			}
			return alignedcmd.MailboxItemResolution{
				Kind:                "stackable",
				PVFPath:             "stackable/epic/fragment.stk",
				StackableType:       "[material]",
				ListType:            dnfrepo.MainInventoryListType,
				SlotStart:           121,
				SlotEnd:             126,
				StackLimit:          9999,
				EpicBookEquipmentID: 101020256,
				EpicBookFragmentID:  10300528,
			}, nil
		},
		Body: currentMailIDList(123),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(123))
	if len(got.ItemSlotRefreshes) != 0 {
		t.Fatalf("epic fragment emitted material-slot refreshes: %+v", got.ItemSlotRefreshes)
	}
	wantBalances := []alignedcmd.EpicBookBalanceProjection{{FragmentID: 10300528, Balance: 17}}
	if !reflect.DeepEqual(got.EpicBookBalances, wantBalances) {
		t.Fatalf("epic book projection = %+v, want %+v", got.EpicBookBalances, wantBalances)
	}
	book, found, err := repos.EpicBook.Load(ctx, "acc-a")
	if err != nil || !found || book.Balances[101020256] != 17 {
		t.Fatalf("epic book found=%v err=%v record=%+v", found, err, book)
	}
	if inventory, found, err := repos.Inventory.Load(ctx, "77"); err != nil || found || len(inventory.Slots) != 0 {
		t.Fatalf("epic fragment entered material inventory found=%v err=%v inventory=%+v", found, err, inventory)
	}
	mailbox, found, err := repos.Mailbox.Load(ctx, "77")
	if err != nil || !found || !mailbox.Mails["123"].Claimed || len(mailbox.Mails["123"].Attachments) != 0 {
		t.Fatalf("mailbox claim found=%v err=%v mail=%+v", found, err, mailbox.Mails["123"])
	}
}

func TestMailboxExtractUsesResolvedNativeContainerForEveryInventoryFamily(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc-a", Name: "receiver"})
	attachments := []dnfrepo.MailAttachment{
		{ItemID: 6101, Count: 1}, // avatar
		{ItemID: 6102, Count: 1}, // creature body
		{ItemID: 6103, Count: 1}, // creature artifact
		{ItemID: 6104, Count: 2}, // creature feed
		{ItemID: 6105, Count: 1}, // guild medal
		{ItemID: 6106, Count: 3}, // guardian gem
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {MailID: "123", Attachments: attachments},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
			switch itemID {
			case 6101:
				return alignedcmd.MailboxItemResolution{Kind: "equipment", EquipmentType: "[skin avatar]", ListType: 1, SlotStart: 0, SlotEnd: 2, StackLimit: 1}, nil
			case 6102:
				return alignedcmd.MailboxItemResolution{Kind: "equipment", EquipmentType: "[creature]", ListType: 7, SlotStart: 0, SlotEnd: 2, StackLimit: 1}, nil
			case 6103:
				return alignedcmd.MailboxItemResolution{Kind: "equipment", EquipmentType: "[artifact red]", ListType: 7, SlotStart: 140, SlotEnd: 142, StackLimit: 1}, nil
			case 6104:
				return alignedcmd.MailboxItemResolution{Kind: "stackable", StackableType: "[feed]", ListType: 7, SlotStart: 189, SlotEnd: 191, StackLimit: 100}, nil
			case 6105:
				return alignedcmd.MailboxItemResolution{Kind: "equipment", EquipmentType: "[flag]", ListType: 38, SlotStart: 0, SlotEnd: 2, StackLimit: 1}, nil
			case 6106:
				return alignedcmd.MailboxItemResolution{Kind: "stackable", StackableType: "[flag gem]", ListType: 38, SlotStart: 49, SlotEnd: 51, StackLimit: 100}, nil
			default:
				return alignedcmd.MailboxItemResolution{}, ErrMailboxAssetMismatch
			}
		},
		Body: currentMailIDList(123),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Six attachment slots, six ACK rows, each with its own claim object.
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAckSlots(123, len(attachments)))
	wantRefreshes := []alignedcmd.ItemSlotRefresh{
		{ListType: 1, SlotIndex: 0},
		{ListType: 7, SlotIndex: 0},
		{ListType: 7, SlotIndex: 140},
		{ListType: 7, SlotIndex: 189},
		{ListType: 38, SlotIndex: 0},
		{ListType: 38, SlotIndex: 49},
	}
	if !reflect.DeepEqual(got.ItemSlotRefreshes, wantRefreshes) {
		t.Fatalf("refreshes=%+v want=%+v", got.ItemSlotRefreshes, wantRefreshes)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	for key, want := range map[string]int64{
		"1:0": 6101, "7:0": 6102, "7:140": 6103, "7:189": 6104, "38:0": 6105, "38:49": 6106,
	} {
		if stack := inventory.Slots[key]; stack.ItemID != want {
			t.Fatalf("slot %s=%+v want item=%d; all=%+v", key, stack, want, inventory.Slots)
		}
	}
	for _, wrong := range []string{"0:9", "0:65", "0:121"} {
		if _, exists := inventory.Slots[wrong]; exists {
			t.Fatalf("native-container attachment leaked into main bag %s: %+v", wrong, inventory.Slots)
		}
	}
}

func TestMailboxExtractRoutesReviveCoinRewardsToFixedWallet(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 9001, Count: 4},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {MailID: "123", Attachments: []dnfrepo.MailAttachment{{ItemID: 42, Count: 6}}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body:                currentMailIDList(123),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(123))
	if !reflect.DeepEqual(got.ItemSlotRefreshes, []alignedcmd.ItemSlotRefresh{{ListType: 0, SlotIndex: 1}}) {
		t.Fatalf("refreshes=%+v", got.ItemSlotRefreshes)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if wallet := inventory.Slots["0:1"]; wallet.ItemID != 1 || wallet.Count != 6 {
		t.Fatalf("wallet=%+v inventory=%+v", wallet, inventory.Slots)
	}
	if material := inventory.Slots["0:121"]; material.ItemID != 9001 || material.Count != 4 {
		t.Fatalf("unrelated material was overwritten: %+v", material)
	}
}

func TestMailboxExtractHappyTokenCeraVoucherCreditsAccountWalletWithoutInventory(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "acc-a",
		Metadata: map[string]string{
			"account_cera":             "700",
			"account_token_cera":       "100",
			"account_happy_token_cera": "5",
		},
	}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
		Name:        "receiver",
	})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {
				MailID: "123",
				Attachments: []dnfrepo.MailAttachment{
					{ItemID: HappyTokenCeraVoucherItemID, Count: 99999},
				},
			},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body:                currentMailIDList(123),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(123))
	if got.CeraWallet == nil || *got.CeraWallet != (alignedcmd.CeraWalletProjection{
		Cera: 700, TokenCera: 100, HappyTokenCera: 100004,
	}) {
		t.Fatalf("wallet projection = %+v", got.CeraWallet)
	}
	if len(got.ItemSlotRefreshes) != 0 {
		t.Fatalf("voucher emitted inventory refreshes: %+v", got.ItemSlotRefreshes)
	}
	account, found, err := repos.Account.Load(ctx, "acc-a")
	if err != nil || !found || account.Metadata["account_happy_token_cera"] != "100004" {
		t.Fatalf("account wallet found=%v err=%v metadata=%v", found, err, account.Metadata)
	}
	if inventory, found, err := repos.Inventory.Load(ctx, "77"); err != nil || found || len(inventory.Slots) != 0 {
		t.Fatalf("voucher created material inventory found=%v err=%v inventory=%+v", found, err, inventory)
	}
	mailbox, found, err := repos.Mailbox.Load(ctx, "77")
	if err != nil || !found || !mailbox.Mails["123"].Claimed || len(mailbox.Mails["123"].Attachments) != 0 {
		t.Fatalf("mailbox claim found=%v err=%v mail=%+v", found, err, mailbox.Mails["123"])
	}
}

func TestMailboxExtractUsesPVFItemTabWithInitialExpansionMarker(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	if err := repos.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope: dnfrepo.CharacterContainerStateScope("77"),
		Values: map[string]string{
			"main_list_param16":           "0",
			"avatar_list_param16":         "0",
			"personal_cargo_list_param16": "8",
		},
	}); err != nil {
		t.Fatalf("save initial expansion marker: %v", err)
	}
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
			return alignedcmd.MailboxItemResolution{
				Kind:             "stackable",
				PVFPath:          "stackable/cash/old_booster_selection.stk",
				StackableType:    "[booster selection]",
				SlotStart:        121,
				SlotEnd:          176,
				StackLimit:       99,
				PreferQuickSlots: false,
			}, nil
		},
		Body: currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(1))
	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%v err=%v", found, err)
	}
	if stack := inventory.Slots["0:121"]; stack.ItemID != 2001 || stack.Count != 2 {
		t.Fatalf("material claim = %+v, want item 2001 x2 in material slot 0:121", stack)
	}
	stack := inventory.Slots["0:121"]
	if stack.Extra["item_kind"] != "stackable" ||
		stack.Extra["pvf_path"] != "stackable/cash/old_booster_selection.stk" ||
		stack.Extra["stackable_type"] != "[booster selection]" ||
		stack.Extra["stack_limit"] != "99" {
		t.Fatalf("mail claim extension metadata = %+v, want runtime-PVF authority", stack.Extra)
	}
	if _, exists := inventory.Slots["0:3"]; exists {
		t.Fatalf("automatic mail material claim entered quick slot: %+v", inventory.Slots)
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if !box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 0 {
		t.Fatalf("successful claim should mark only after the material-tab insert: %+v", box.Mails["1"])
	}
}

func TestMailboxExtractAddsRequiredEquipmentExtensionMetadata(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	raw := make([]byte, mailboxItemRawSize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], 0x1fd58e8e)
	raw[0x0A] = 31
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 31)
	raw[0x13] = 1
	binary.LittleEndian.PutUint16(raw[0x14:0x16], 7)
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Attachments: []dnfrepo.MailAttachment{{ItemID: 5001, Count: 1, RawEntry: raw}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(uint32) (alignedcmd.MailboxItemResolution, error) {
			return alignedcmd.MailboxItemResolution{
				Kind:          "equipment",
				PVFPath:       "equipment/weapon/test.equ",
				EquipmentType: "[weapon]",
				Durability:    57,
				SlotStart:     9,
				SlotEnd:       64,
				StackLimit:    1,
			}, nil
		},
		Body: currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(1))
	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%v err=%v", found, err)
	}
	stack := inventory.Slots["0:9"]
	if stack.ItemID != 5001 || stack.Count != 1 || stack.Extra["item_kind"] != "equipment" ||
		stack.Extra["pvf_path"] != "equipment/weapon/test.equ" ||
		stack.Extra["equipment_type"] != "[weapon]" ||
		stack.Extra["durability"] != "31" || stack.Extra["max_durability"] != "57" {
		t.Fatalf("equipment mail extension metadata = %+v", stack)
	}
	if len(stack.RawEntry) != mailboxItemRawSize || binary.LittleEndian.Uint32(stack.RawEntry[0x06:0x0A]) != 0x1fd58e8e ||
		stack.RawEntry[0x0A]&0x1f != 31 || stack.RawEntry[0x13] != 1 || binary.LittleEndian.Uint16(stack.RawEntry[0x14:0x16]) != 7 {
		t.Fatalf("equipment mail claim raw = %x", stack.RawEntry)
	}
}

func TestMailboxExtractKeepsAttachmentsWhenTheirPVFTabIsFull(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver", Stats: map[string]int64{"gold": 10}})
	slots := make(map[string]dnfrepo.ItemStack)
	for slot := int16(3); slot <= 8; slot++ {
		slots["0:"+strconv.Itoa(int(slot))] = dnfrepo.ItemStack{ItemID: 8000 + int64(slot), Count: 1}
	}
	for slot := int16(65); slot <= 120; slot++ {
		slots["0:"+strconv.Itoa(int(slot))] = dnfrepo.ItemStack{ItemID: 9000 + int64(slot), Count: 1}
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots}); err != nil {
		t.Fatalf("save full consumable tab: %v", err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Gold: 5, Attachments: []dnfrepo.MailAttachment{{ItemID: 2001, Count: 1}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 120, 99),
		Body:                currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	// 86JP claim table: an attachment that cannot be inserted is code 4, after
	// which the client runs its own inventory check and draws the no-space
	// message. The former 6 was outside that table, so the button looked dead.
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x04})
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 1 {
		t.Fatalf("full-tab rejection must retain the attachment: %+v", box.Mails["1"])
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 10 {
		t.Fatalf("full-tab rejection must roll back gold, got %d", character.Stats["gold"])
	}
}

func TestMailboxExtractFullMaterialTabDoesNotSpillIntoEmptyQuickSlots(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver", Stats: map[string]int64{"gold": 10}})
	slots := make(map[string]dnfrepo.ItemStack)
	for slot := int16(121); slot <= 176; slot++ {
		slots["0:"+strconv.Itoa(int(slot))] = dnfrepo.ItemStack{ItemID: 9000 + int64(slot), Count: 1}
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots}); err != nil {
		t.Fatalf("save full material tab: %v", err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Gold: 5, Attachments: []dnfrepo.MailAttachment{{ItemID: 2001, Count: 1}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: func(uint32) (alignedcmd.MailboxItemResolution, error) {
			return alignedcmd.MailboxItemResolution{
				Kind:             "stackable",
				SlotStart:        121,
				SlotEnd:          176,
				StackLimit:       99,
				PreferQuickSlots: false,
			}, nil
		},
		Body: currentMailIDList(1),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	// 86JP claim table: an attachment that cannot be inserted is code 4, after
	// which the client runs its own inventory check and draws the no-space
	// message. The former 6 was outside that table, so the button looked dead.
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x04})
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if box.Mails["1"].Claimed || len(box.Mails["1"].Attachments) != 1 {
		t.Fatalf("full material-tab rejection must retain the attachment: %+v", box.Mails["1"])
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	for slot := int16(3); slot <= 8; slot++ {
		if _, exists := inventory.Slots["0:"+strconv.Itoa(int(slot))]; exists {
			t.Fatalf("full material tab spilled into quick slot %d: %+v", slot, inventory.Slots)
		}
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 10 {
		t.Fatalf("full material-tab rejection must roll back gold, got %d", character.Stats["gold"])
	}
}

func TestMailboxExtractBatchFillsQuickSlotsAndItemTabBeforeLeavingFullMail(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver", Stats: map[string]int64{"gold": 0}})
	mails := make(map[string]dnfrepo.MailRecord)
	for id := uint32(1); id <= 8; id++ {
		mails[strconv.FormatUint(uint64(id), 10)] = dnfrepo.MailRecord{
			MailID: strconv.FormatUint(uint64(id), 10),
			Gold:   1,
			Attachments: []dnfrepo.MailAttachment{{
				ItemID: int64(2000 + id),
				Count:  99,
			}},
		}
	}
	mails["9"] = dnfrepo.MailRecord{
		MailID: "9",
		Gold:   9,
		Attachments: []dnfrepo.MailAttachment{{
			ItemID: 2009,
			Count:  1,
		}},
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{CharacterID: "77", Mails: mails}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 66, 99),
		Body:                currentMailIDList(1, 2, 3, 4, 5, 6, 7, 8, 9),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(1, 2, 3, 4, 5, 6, 7, 8))
	if len(got.PostActions) != 0 {
		t.Fatalf("post actions = %v, want no full container refresh", got.PostActions)
	}
	wantRefreshes := []alignedcmd.ItemSlotRefresh{
		{ListType: 0, SlotIndex: 3}, {ListType: 0, SlotIndex: 4}, {ListType: 0, SlotIndex: 5},
		{ListType: 0, SlotIndex: 6}, {ListType: 0, SlotIndex: 7}, {ListType: 0, SlotIndex: 8},
		{ListType: 0, SlotIndex: 65}, {ListType: 0, SlotIndex: 66},
	}
	if !reflect.DeepEqual(got.ItemSlotRefreshes, wantRefreshes) {
		t.Fatalf("item slot refreshes = %+v, want %+v", got.ItemSlotRefreshes, wantRefreshes)
	}

	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%v err=%v", found, err)
	}
	for index, slot := range []int16{3, 4, 5, 6, 7, 8, 65, 66} {
		stack := inventory.Slots["0:"+strconv.Itoa(int(slot))]
		if stack.ItemID != int64(2001+index) || stack.Count != 99 {
			t.Fatalf("slot 0:%d = %+v, want item=%d count=99", slot, stack, 2001+index)
		}
	}
	box, _, err := repos.Mailbox.Load(ctx, "77")
	if err != nil {
		t.Fatalf("load mailbox: %v", err)
	}
	for id := 1; id <= 8; id++ {
		mail := box.Mails[strconv.Itoa(id)]
		if !mail.Claimed || mail.Gold != 0 || len(mail.Attachments) != 0 {
			t.Fatalf("mail %d should be committed: %+v", id, mail)
		}
	}
	if mail := box.Mails["9"]; mail.Claimed || mail.Gold != 9 || len(mail.Attachments) != 1 {
		t.Fatalf("full mail should remain claimable: %+v", mail)
	}
	character, _, err := repos.Character.Load(ctx, "77")
	if err != nil || character.Stats["gold"] != 8 {
		t.Fatalf("gold = %d err=%v, want only the eight committed letters", character.Stats["gold"], err)
	}
}

func TestMailboxExtractBatchSkipsAlreadyClaimedLetters(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Claimed: true, Read: true, Attachments: []dnfrepo.MailAttachment{{ItemID: 1001, Count: 1}}},
			"2": {MailID: "2", Attachments: []dnfrepo.MailAttachment{{ItemID: 1002, Count: 1}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 120, 99),
		Body:                currentMailIDList(1, 2),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertMailboxExtractSuccessWithIncrementalRemove(t, got, currentExtractAck(2))
	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%v err=%v", found, err)
	}
	if stack := inventory.Slots["0:3"]; stack.ItemID != 1002 || stack.Count != 1 {
		t.Fatalf("pending claim stack = %+v", stack)
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if !box.Mails["1"].Claimed || !box.Mails["2"].Claimed || len(box.Mails["2"].Attachments) != 0 {
		t.Fatalf("claim state = %+v", box.Mails)
	}
}

func TestMailboxPresentationTranslatesExistingOverflowMailWithoutMutatingIt(t *testing.T) {
	durable := dnfrepo.MailRecord{
		SenderName: "System",
		Title:      "Inventory full: box reward",
		Body:       "Your box reward was delivered here because the inventory is full.",
		Metadata:   map[string]string{"source": "magic_box_reward_inventory_full"},
	}
	presented := mailboxPresentationMail(durable)
	if presented.SenderName != "系统" || presented.Title != "背包已满：礼盒奖励" ||
		presented.Body != "背包空间不足，礼盒奖励已通过邮件发送。请清理对应道具分页后领取。" {
		t.Fatalf("presented overflow mail = %+v", presented)
	}
	if durable.SenderName != "System" || durable.Title != "Inventory full: box reward" ||
		durable.Body != "Your box reward was delivered here because the inventory is full." {
		t.Fatalf("presentation must not rewrite durable mail: %+v", durable)
	}
}

func TestMailboxOpenRepairsAssetsLeftOnAnAlreadyClaimedLetter(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", Claimed: true, Read: true, Gold: 7, Attachments: []dnfrepo.MailAttachment{{ItemID: 1001, Count: 1}}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	opened, err := NewOwner(repos).Open(ctx, "77")
	if err != nil {
		t.Fatalf("open mailbox: %v", err)
	}
	if len(opened.Mails) != 1 || opened.Mails[0].Gold != 0 || len(opened.Mails[0].Attachments) != 0 {
		t.Fatalf("opened claimed letter still projects assets: %+v", opened.Mails)
	}
	box, _, err := repos.Mailbox.Load(ctx, "77")
	if err != nil || box.Mails["1"].Gold != 0 || len(box.Mails["1"].Attachments) != 0 {
		t.Fatalf("legacy claimed letter was not repaired: mailbox=%+v err=%v", box, err)
	}
}

func TestMailboxConcurrentExtractCommitsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		Name:        "receiver",
		Stats:       map[string]int64{"gold": 10},
	})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {MailID: "123", Gold: 50},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	request := alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body:                currentMailIDList(123),
	}

	var wait sync.WaitGroup
	results := make(chan alignedcmd.Result, 2)
	errors := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := NewHandler().Handle(ctx, request)
			results <- result
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent Handle error = %v", err)
		}
	}
	successes := 0
	alreadyClaimed := 0
	for result := range results {
		body := result.UpperResponses[0].Body
		switch {
		case bytes.Equal(body, currentExtractAckWholeLetter(123)):
			successes++
		case bytes.Equal(body, []byte{0x00, 0x15}):
			// 86JP claim table: an already-claimed letter no longer exists.
			alreadyClaimed++
		default:
			t.Fatalf("unexpected concurrent extract body: % X", body)
		}
	}
	if successes != 1 || alreadyClaimed != 1 {
		t.Fatalf("successes=%d already_claimed=%d, want 1/1", successes, alreadyClaimed)
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 60 {
		t.Fatalf("concurrent extract gold = %d, want exactly one grant to 60", character.Stats["gold"])
	}
}

func TestMailboxExtractRejectsMissingMailID(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc-a", Name: "receiver"})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{CharacterID: "77"}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body:                currentMailIDList(999),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	// 86JP claim table: no such mail is code 21.
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x15})
	if len(got.PostActions) != 0 {
		t.Fatalf("post actions = %v, want none on rejection", got.PostActions)
	}
}

func TestMailboxChangeStateUsesCurrentOp134ResultLayout(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", Name: "receiver"})
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {MailID: "123", Claimed: true},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketChangeLetterStat),
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body:                currentStateBody(0, 123),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketChangeLetterStat, currentStateAck(0, 123))
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if !box.Mails["123"].Deleted {
		t.Fatalf("mail state was not committed: %+v", box.Mails["123"])
	}
}

func TestMailboxChangeStateMarksReadAndSavesCurrentLetterStates(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"123": {MailID: "123", CreatedAt: time.Now().UTC()},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	request := alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketChangeLetterStat),
		SelectedCharacterID: 77,
		Repositories:        repos,
	}
	request.Body = currentStateBody(2, 123)
	got, err := NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketChangeLetterStat, currentStateAck(2, 123))

	request.Body = currentStateBody(3, 123)
	got, err = NewHandler().Handle(ctx, request)
	if err != nil {
		t.Fatalf("save mail: %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketChangeLetterStat, currentStateAck(3, 123))
	box, _, err := repos.Mailbox.Load(ctx, "77")
	if err != nil {
		t.Fatalf("load mailbox: %v", err)
	}
	mail := box.Mails["123"]
	if !mail.Read || !mailboxMailSaved(mail) || mailboxLetterStat(mail) != 3 {
		t.Fatalf("mail state = %+v, want read/saved/stat3", mail)
	}
}

func TestMailboxOpenExpiresNormalMailButKeepsSavedMail(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {MailID: "1", CreatedAt: created},
			"2": {MailID: "2", CreatedAt: created, Metadata: map[string]string{"mailbox_saved": "true"}},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	owner := NewOwner(repos)
	owner.now = func() time.Time { return created.Add(mailboxNormalLifetime + time.Second) }
	result, err := owner.Open(ctx, "77")
	if err != nil {
		t.Fatalf("open mailbox: %v", err)
	}
	if result.Total != 1 || len(result.Mails) != 1 || result.Mails[0].MailID != "2" || mailboxLetterStat(result.Mails[0]) != 3 {
		t.Fatalf("open result = %+v, want only saved mail", result)
	}
}

func TestMailboxAlarmNotificationUsesExactOneWordBody(t *testing.T) {
	if got := BuildAlarmNotification(513); !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Fatalf("class0/0x63 body = % X, want 01 02", got)
	}
}

func TestUnlimitedSystemMailDoesNotFallBackToFifteenDayExpiry(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mail := dnfrepo.MailRecord{
		CreatedAt: created,
		Metadata: map[string]string{
			"system_mail": "true",
			"unlimited":   "true",
		},
	}
	now := created.Add(365 * 24 * time.Hour)
	if mailExpired(mail, now) {
		t.Fatal("unlimited system mail expired through the ordinary 15-day fallback")
	}
	if remaining := mailboxRemainingSeconds(mail, now); remaining != 0 {
		t.Fatalf("unlimited system mail remaining=%d want=0/no-limit", remaining)
	}
}

func TestMailboxAttachmentRawProjectsLiveStackableUsePeriod(t *testing.T) {
	now := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	expiresAt := now.Add(30 * 24 * time.Hour)
	raw := mailboxAttachmentRaw(dnfrepo.MailAttachment{
		ItemID:   490007794,
		Count:    1,
		ExpireAt: expiresAt,
		Extra:    map[string]string{"item_kind": "stackable", "stackable_type": "[waste]"},
	}, now)
	if got := binary.LittleEndian.Uint32(raw[0x38:0x3c]); got != uint32(expiresAt.Unix()) {
		t.Fatalf("attachment expire=%d want=%d", got, expiresAt.Unix())
	}
	if got := binary.LittleEndian.Uint16(raw[0x0b:0x0d]); got != math.MaxUint16 {
		t.Fatalf("attachment use period=%d want=%d", got, uint16(math.MaxUint16))
	}
	raw = mailboxAttachmentRaw(dnfrepo.MailAttachment{
		ItemID:   490007794,
		Count:    1,
		ExpireAt: now.Add(9*time.Second + time.Nanosecond),
		Extra:    map[string]string{"item_kind": "stackable"},
	}, now)
	if got := binary.LittleEndian.Uint16(raw[0x0b:0x0d]); got != 10 {
		t.Fatalf("final attachment use period=%d want=10", got)
	}
}

func TestMailboxSnapshotProjectsCurrentAttachmentRowAndCreatureFraming(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC)
	raw := make([]byte, mailboxItemRawSize)
	binary.LittleEndian.PutUint16(raw[0:2], 14)
	binary.LittleEndian.PutUint32(raw[2:6], 9999)
	binary.LittleEndian.PutUint32(raw[6:10], 99)
	binary.LittleEndian.PutUint16(raw[0x0b:0x0d], 55)
	raw[0x0d] = 2
	binary.LittleEndian.PutUint32(raw[0x0e:0x12], 12345)
	raw[0x12] = 6
	raw[0x13] = 7
	binary.LittleEndian.PutUint16(raw[0x14:0x16], 8)
	raw[0x20] = 0xA5

	mail := dnfrepo.MailRecord{
		MailID:            "7",
		SenderCharacterID: "11",
		SenderName:        "发件人",
		Body:              "附件邮件",
		CreatedAt:         now.Add(-time.Hour),
		ExpireAt:          now.Add(time.Hour),
		Gold:              50,
		Metadata:          map[string]string{"mail_type": "4"},
		Attachments: []dnfrepo.MailAttachment{{
			ItemID:   9999,
			Count:    2,
			Bind:     true,
			RawEntry: raw,
			Extra:    map[string]string{"mailbox_equipment_type": "creature"},
		}},
	}
	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{mail}, 3, now)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	if body[0] != 1 || body[1] != 0 {
		t.Fatalf("snapshot header = % X, want one initial summary", body[:2])
	}
	offset := 2
	// The leading dword is this slot's claim object; the letter id keying the row
	// sits at its tail. See the claim-object note in body.go.
	wantClaimObject, err := MailboxAttachmentClaimObject(7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != wantClaimObject {
		t.Fatalf("summary claim object = %d, want %d", got, wantClaimObject)
	}
	offset += 4
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 11 {
		t.Fatalf("summary sender ID = %d, want 11", got)
	}
	offset += 4
	senderLength := int(binary.LittleEndian.Uint32(body[offset : offset+4]))
	offset += 4 + senderLength
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 50 {
		t.Fatalf("summary gold = %d, want 50", got)
	}
	offset += 4
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 9999 || body[offset+4] != 1 {
		t.Fatalf("summary item header = % X", body[offset:offset+5])
	}
	offset += 5
	if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != 2 {
		t.Fatalf("summary item count = %d, want transferred count 2", got)
	}
	offset += 4 + 2 + 1 + 4 + 1 + 1 + 2
	if !bytes.Equal(body[offset:offset+5], []byte{0, 0, 0, 0, 0}) {
		t.Fatalf("creature framing = % X, want u32 zero plus zero detail flag", body[offset:offset+5])
	}
	offset += 5
	wantRaw := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint16(wantRaw[0:2], 0)
	binary.LittleEndian.PutUint32(wantRaw[2:6], 9999)
	binary.LittleEndian.PutUint32(wantRaw[6:10], 2)
	if !bytes.Equal(body[offset:offset+mailboxItemRawSize], wantRaw) {
		t.Fatalf("projected 0x77 item row = % X, want % X", body[offset:offset+mailboxItemRawSize], wantRaw)
	}
}

// The runtime PVF stores `[equipment type]` as a backtick-quoted bracket tag,
// so the gates that decide the current client's optional summary fields must
// accept `[creature]` and `[weapon avatar]` exactly as they are persisted.
func TestMailboxSummaryOptionalFieldGatesFollowRuntimePVFEquipmentTags(t *testing.T) {
	cases := []struct {
		tag     string
		class26 bool
		avatar  bool
	}{
		{tag: "[creature]", class26: true},
		{tag: "creature", class26: true},
		{tag: "`[creature]`", class26: true},
		{tag: "[weapon avatar]", avatar: true},
		{tag: "`[hat avatar]`", avatar: true},
		{tag: "[aurora avatar]", avatar: true},
		{tag: "weapon avatar", avatar: true},
		{tag: "[artifact red]"},
		{tag: "[weapon]"},
		{tag: "[coat]"},
		{tag: ""},
	}
	for _, testCase := range cases {
		attachment := dnfrepo.MailAttachment{Extra: map[string]string{"mailbox_equipment_type": testCase.tag}}
		if got := mailboxAttachmentRequiresClass26(attachment); got != testCase.class26 {
			t.Fatalf("tag %q class26 = %v, want %v", testCase.tag, got, testCase.class26)
		}
		if got := mailboxAttachmentRequiresAvatarBlobs(attachment); got != testCase.avatar {
			t.Fatalf("tag %q avatar blobs = %v, want %v", testCase.tag, got, testCase.avatar)
		}
	}
}

func TestMailboxSnapshotWritesAvatarOptionalBlobsAfterItemRow(t *testing.T) {
	now := time.Date(2026, 9, 3, 1, 12, 0, 0, time.UTC)
	raw := make([]byte, mailboxItemRawSize)
	socket := make([]byte, mailboxAvatarSocketBytes)
	for index := range socket {
		socket[index] = byte(index + 1)
	}

	newMail := func(extra map[string]string) dnfrepo.MailRecord {
		return dnfrepo.MailRecord{
			MailID:     "8",
			SenderName: "GM",
			CreatedAt:  now,
			ExpireAt:   now.Add(time.Hour),
			Attachments: []dnfrepo.MailAttachment{{
				ItemID:   112600001,
				Count:    1,
				RawEntry: raw,
				Extra:    extra,
			}},
		}
	}

	// The summary is two header bytes, the fixed sender/gold/item fields, then
	// the 0x77 row. Keep the offset explicit so the regression locks the wire
	// boundary between the row, the two blobs and the remaining time.
	rowEnd := 2 + 4 + 4 + 4 + 2 + 4 + 4 + 1 + 4 + 2 + 1 + 4 + 1 + 1 + 2 + mailboxItemRawSize

	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{newMail(map[string]string{
		"mailbox_equipment_type": "[weapon avatar]",
		"avatar_socket_data":     hex.EncodeToString(socket),
		"tail_data":              "0100020003000400",
	})}, 0, now)
	if err != nil {
		t.Fatalf("build avatar snapshot: %v", err)
	}
	want := make([]byte, 0, 8+mailboxAvatarSocketBytes+mailboxAvatarColorBytes)
	want = binary.LittleEndian.AppendUint32(want, mailboxAvatarSocketBytes)
	want = append(want, socket...)
	want = binary.LittleEndian.AppendUint32(want, mailboxAvatarColorBytes)
	// Only the leading colour pair fits the client's single-DWORD target; the
	// rest of an over-long durable tail_data must never reach the wire.
	want = append(want, 0x01, 0x00, 0x02, 0x00)
	if got := body[rowEnd : rowEnd+len(want)]; !bytes.Equal(got, want) {
		t.Fatalf("avatar blobs = % X, want % X", got, want)
	}
	if got := binary.LittleEndian.Uint32(body[rowEnd+len(want) : rowEnd+len(want)+4]); got != 3600 {
		t.Fatalf("avatar remaining seconds = %d, want 3600", got)
	}

	body, err = buildMailboxListNotification([]dnfrepo.MailRecord{newMail(map[string]string{
		"mailbox_equipment_type": "[weapon avatar]",
	})}, 0, now)
	if err != nil {
		t.Fatalf("build bare avatar snapshot: %v", err)
	}
	if got := body[rowEnd : rowEnd+8]; !bytes.Equal(got, make([]byte, 8)) {
		t.Fatalf("bare avatar blobs = % X, want two zero lengths", got)
	}
	if got := binary.LittleEndian.Uint32(body[rowEnd+8 : rowEnd+12]); got != 3600 {
		t.Fatalf("bare avatar remaining seconds = %d, want 3600", got)
	}

	body, err = buildMailboxListNotification([]dnfrepo.MailRecord{newMail(map[string]string{
		"mailbox_equipment_type": "[weapon]",
		"avatar_socket_data":     hex.EncodeToString(socket),
	})}, 0, now)
	if err != nil {
		t.Fatalf("build ordinary equipment snapshot: %v", err)
	}
	if got := binary.LittleEndian.Uint32(body[rowEnd : rowEnd+4]); got != 3600 {
		t.Fatalf("ordinary equipment remaining seconds = %d, want the row to be followed by the trailer", got)
	}
}

func TestMailboxQueryUsesCurrentOp324Layout(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "88",
		Name:        "receiver",
		Job:         "3",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 2},
	})
	body := append(dstrings("receiver"), 0x01)
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:       uint16(dnfenum.CmdPacketQueryCharacInfoMailbox),
		Repositories: repos,
		Body:         body,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	want := []byte{0x01}
	want = append(want, dstrings("receiver")...)
	want = append(want, 0x02, 90, 0, 0x03, 0x00, 0x01, 0x00, 0x00)
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketQueryCharacInfoMailbox, want)
}

func TestMailboxQueryRejectsMissingDurableProjection(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{
		CharacterID: "88",
		Name:        "receiver",
		Job:         "not-a-current-job",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 2},
	})
	body := append(dstrings("receiver"), 0x01)
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:       uint16(dnfenum.CmdPacketQueryCharacInfoMailbox),
		Repositories: repos,
		Body:         body,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketQueryCharacInfoMailbox, []byte{0x00, 0x01})
}

func TestMailboxMalformedCurrentBodiesAreRejectedWithoutMutation(t *testing.T) {
	for _, opcode := range []dnfenum.CmdPacket{
		dnfenum.CmdPacketMailboxSend,
		dnfenum.CmdPacketMailboxExtractItem,
		dnfenum.CmdPacketMailboxOpen,
		dnfenum.CmdPacketChangeLetterStat,
		dnfenum.CmdPacketMultiMailboxSend,
		dnfenum.CmdPacketQueryCharacInfoMailbox,
	} {
		t.Run(strconv.Itoa(int(opcode)), func(t *testing.T) {
			got, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
				Opcode: uint16(opcode),
				Body:   []byte{0xFF, 0x00, 0x01},
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			assertOneMailboxResponse(t, got, opcode, []byte{0x00, 0x01})
		})
	}
}

func TestCurrentMailboxPostageMatchesIDAFormula(t *testing.T) {
	tests := []struct {
		name            string
		gold            int64
		attachments     int
		attachmentValue int64
		want            int64
	}{
		{name: "message only", want: 100},
		{name: "gold", gold: 50, want: 102},
		{name: "one attachment", attachments: 1, attachmentValue: 200, want: 1110},
		{name: "two attachments", attachments: 2, attachmentValue: 400, want: 2120},
		{name: "all components", gold: 1000, attachments: 2, attachmentValue: 400, want: 2170},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := currentMailboxPostage(test.gold, test.attachments, test.attachmentValue)
			if err != nil {
				t.Fatalf("currentMailboxPostage error = %v", err)
			}
			if got != test.want {
				t.Fatalf("currentMailboxPostage = %d, want %d", got, test.want)
			}
		})
	}
}

func assertOneMailboxResponse(t *testing.T, got alignedcmd.Result, opcode dnfenum.CmdPacket, body []byte) {
	t.Helper()
	if !got.Handled {
		t.Fatalf("%d should be handled", opcode)
	}
	if !got.ResponseAllowed {
		t.Fatalf("%d should allow response", opcode)
	}
	if len(got.UpperResponses) != 1 {
		t.Fatalf("response count = %d, want 1", len(got.UpperResponses))
	}
	assertMailboxResponse(t, got.UpperResponses[0], uint16(opcode), dnfproto.DefaultChannelClassification, body)
}

func assertMailboxExtractSuccessWithIncrementalRemove(t *testing.T, got alignedcmd.Result, ack []byte) {
	t.Helper()
	if !got.Handled || !got.ResponseAllowed {
		t.Fatalf("mailbox extract should be handled with a response: %+v", got)
	}
	if len(got.UpperResponses) != 1 {
		t.Fatalf("response count = %d, want only op95 ACK before item refresh", len(got.UpperResponses))
	}
	assertMailboxResponse(t, got.UpperResponses[0], uint16(dnfenum.CmdPacketMailboxExtractItem), dnfproto.DefaultChannelClassification, ack)
	if len(got.PostItemSlotRefreshResponses) != 1 {
		t.Fatalf("post-item response count = %d, want one class0/0x62 removal", len(got.PostItemSlotRefreshResponses))
	}
	if len(ack) < 5 {
		t.Fatalf("short mailbox extract ACK: % X", ack)
	}
	count := int(binary.LittleEndian.Uint32(ack[1:5]))
	if len(ack) != 5+count*12 {
		t.Fatalf("mailbox extract ACK count=%d body=% X", count, ack)
	}
	// Each row is (letter id, claim object, mode). The claim object is either the
	// letter id itself (a gold-only row) or that letter's encoded attachment slot;
	// a letter with several attachments reports one row per slot, and the remove
	// notification then names each closed letter once.
	mailIDs := make([]uint32, 0, count)
	for index := 0; index < count; index++ {
		offset := 5 + index*12
		mailID := binary.LittleEndian.Uint32(ack[offset : offset+4])
		claimObject := binary.LittleEndian.Uint32(ack[offset+4 : offset+8])
		status := binary.LittleEndian.Uint32(ack[offset+8 : offset+12])
		decodedMailID, _, _ := DecodeMailboxClaimObject(claimObject)
		if mailID == 0 || decodedMailID != mailID || status != 0 {
			t.Fatalf("mailbox extract ACK result %d mail=%d claim_object=%d status=%d", index, mailID, claimObject, status)
		}
		if len(mailIDs) == 0 || mailIDs[len(mailIDs)-1] != mailID {
			mailIDs = append(mailIDs, mailID)
		}
	}
	assertMailboxResponse(t, got.PostItemSlotRefreshResponses[0], mailboxRemoveNotificationMessageID, 0, buildMailboxRemoveNotification(mailIDs))
}

func assertMailboxResponse(t *testing.T, response alignedcmd.UpperResponse, messageID uint16, classification byte, body []byte) {
	t.Helper()
	if response.MsgID != messageID {
		t.Fatalf("msgID = %d, want %d", response.MsgID, messageID)
	}
	if response.Classification != classification {
		t.Fatalf("classification = %d, want %d", response.Classification, classification)
	}
	if !response.AllowCodec {
		t.Fatalf("%d should use normal game upper codec path", messageID)
	}
	if !bytes.Equal(response.Body, body) {
		t.Fatalf("body = % X, want % X", response.Body, body)
	}
}

func mustSaveCharacter(t *testing.T, repos dnfrepo.Group, record dnfrepo.CharacterRecord) {
	t.Helper()
	if err := repos.Character.Save(context.Background(), record); err != nil {
		t.Fatalf("save character %s: %v", record.CharacterID, err)
	}
}

func mailboxClaimTestResolver(start int16, end int16, stackLimit int64) alignedcmd.MailboxItemResolver {
	return func(uint32) (alignedcmd.MailboxItemResolution, error) {
		return alignedcmd.MailboxItemResolution{
			Kind:             "stackable",
			SlotStart:        start,
			SlotEnd:          end,
			StackLimit:       stackLimit,
			PreferQuickSlots: true,
		}, nil
	}
}

func TestAddAttachmentStacksCurrentPVFMaterialDespiteRawAndTracingMetadata(t *testing.T) {
	raw := make([]byte, mailboxItemRawSize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0a], 9)
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:65": {
			ItemID: 3244, Count: 9, RawEntry: raw,
			Extra: map[string]string{"source": "dungeon_pvf_drop_pickup", "pvf_path": "stackable/old.stk"},
		},
	}}
	resolution := alignedcmd.MailboxItemResolution{
		Kind: "stackable", PVFPath: "stackable/material/crystal_pure.stk", StackableType: "[material]",
		SlotStart: 65, SlotEnd: 120, StackLimit: 0,
	}
	changed, err := addAttachmentToInventory(&inventory, dnfrepo.MailAttachment{
		ItemID: 3244, Count: 4, Extra: map[string]string{"source": "gm_mail", "last_grant_source": "mail"},
	}, resolution)
	if err != nil {
		t.Fatalf("addAttachmentToInventory error = %v", err)
	}
	if !reflect.DeepEqual(changed, []int16{65}) {
		t.Fatalf("changed slots = %v", changed)
	}
	if len(inventory.Slots) != 1 {
		t.Fatalf("mail claim created another slot: %+v", inventory.Slots)
	}
	stack := inventory.Slots["0:65"]
	if stack.Count != 13 || binary.LittleEndian.Uint32(stack.RawEntry[0x06:0x0a]) != 13 {
		t.Fatalf("merged mail stack = %+v raw=%x", stack, stack.RawEntry[:10])
	}
	if stack.Extra["item_kind"] != "stackable" || stack.Extra["pvf_path"] != "stackable/material/crystal_pure.stk" ||
		stack.Extra["stackable_type"] != "[material]" {
		t.Fatalf("canonical mail metadata = %+v", stack.Extra)
	}
}

func currentSendBody(
	recipient string,
	gold uint32,
	attachments []SendAttachment,
	message string,
	special uint32,
	global bool,
	multi bool,
) []byte {
	out := dstrings(recipient)
	out = appendU32(out, gold)
	if multi {
		out = append(out, byte(len(attachments)))
	}
	for _, attachment := range attachments {
		out = append(out, attachment.ListType)
		var slot [2]byte
		binary.LittleEndian.PutUint16(slot[:], attachment.SlotIndex)
		out = append(out, slot[:]...)
		out = appendU32(out, attachment.ItemID)
		out = appendU32(out, attachment.Count)
	}
	out = append(out, dstrings(message)...)
	out = appendU32(out, special)
	if global {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	return out
}

func currentMailIDList(ids ...uint32) []byte {
	out := appendU32(nil, uint32(len(ids)))
	for _, id := range ids {
		out = appendU32(out, id)
	}
	return out
}

// currentExtractAck is the acknowledgement for letters that handed over their
// first attachment slot. sub_27156D0 reads each triple as (letter id, claim
// object, mode): the letter id keys its container and the claim object selects
// the slot through sub_2708220, so both halves have to be on the wire.
func currentExtractAck(ids ...uint32) []byte {
	out := []byte{1}
	out = appendU32(out, uint32(len(ids)))
	for _, id := range ids {
		out = appendU32(out, id)
		out = appendU32(out, MailboxAttachmentSlotFlag|id*mailboxMaxAttachmentsPerMail)
		out = appendU32(out, 0)
	}
	return out
}

// currentExtractAckSlots is the acknowledgement for one letter that handed over
// its first `slots` attachment slots: one (letter id, slot claim object, 0) row
// per slot, in letter order.
func currentExtractAckSlots(id uint32, slots int) []byte {
	out := []byte{1}
	out = appendU32(out, uint32(slots))
	for index := 0; index < slots; index++ {
		out = appendU32(out, id)
		out = appendU32(out, MailboxAttachmentSlotFlag|id*mailboxMaxAttachmentsPerMail|uint32(index))
		out = appendU32(out, 0)
	}
	return out
}

// currentExtractAckWholeLetter is the acknowledgement for a letter that had no
// attachment to hand over, only gold. Such a summary row carries the letter id
// as its own claim object, so the claim is reported with that value.
func currentExtractAckWholeLetter(ids ...uint32) []byte {
	out := []byte{1}
	out = appendU32(out, uint32(len(ids)))
	for _, id := range ids {
		out = appendU32(out, id)
		out = appendU32(out, id)
		out = appendU32(out, 0)
	}
	return out
}

func currentStateBody(status uint16, ids ...uint32) []byte {
	out := currentMailIDList(ids...)
	var state [2]byte
	binary.LittleEndian.PutUint16(state[:], status)
	return append(out, state[:]...)
}

func currentStateAck(status uint16, ids ...uint32) []byte {
	out := []byte{1}
	out = appendU32(out, uint32(len(ids)))
	for _, id := range ids {
		out = appendU32(out, id)
		var state [2]byte
		binary.LittleEndian.PutUint16(state[:], status)
		out = append(out, state[:]...)
	}
	return out
}

func dstrings(values ...string) []byte {
	var out []byte
	for _, value := range values {
		out = appendU32(out, uint32(len([]byte(value))))
		out = append(out, []byte(value)...)
	}
	return out
}

func appendU32(out []byte, value uint32) []byte {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], value)
	return append(out, encoded[:]...)
}
