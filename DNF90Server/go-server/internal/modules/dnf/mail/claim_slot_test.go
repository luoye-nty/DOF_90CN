package mail

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestMailboxAttachmentClaimObjectRoundTrip(t *testing.T) {
	for _, mailID := range []uint32{1, 7, 52, 65535} {
		for index := 0; index < mailboxMaxAttachmentsPerMail; index++ {
			claimObject, err := MailboxAttachmentClaimObject(mailID, index)
			if err != nil {
				t.Fatalf("mail %d index %d: %v", mailID, index, err)
			}
			if claimObject < MailboxAttachmentSlotFlag {
				t.Fatalf("claim object %d must carry the slot flag", claimObject)
			}
			gotMail, gotIndex, single := DecodeMailboxClaimObject(claimObject)
			if !single || gotMail != mailID || gotIndex != index {
				t.Fatalf("decode(%d) = (%d, %d, %t), want (%d, %d, true)", claimObject, gotMail, gotIndex, single, mailID, index)
			}
		}
	}
	if _, err := MailboxAttachmentClaimObject(7, mailboxMaxAttachmentsPerMail); err == nil {
		t.Fatal("an out-of-range attachment index must be rejected")
	}
	// A plain mail id addresses the whole letter, which is what this client sends.
	if gotMail, gotIndex, single := DecodeMailboxClaimObject(52); single || gotMail != 52 || gotIndex != 0 {
		t.Fatalf("decode(52) = (%d, %d, %t), want the whole letter", gotMail, gotIndex, single)
	}
}

func mailboxSlotTestRepos(t *testing.T, ctx context.Context, attachments []dnfrepo.MailAttachment) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc-a", Name: "receiver"})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails:       map[string]dnfrepo.MailRecord{"1": {MailID: "1", Attachments: attachments}},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	return repos
}

func mailboxSlotClaim(repos dnfrepo.Group, claimObject uint32) alignedcmd.Request {
	return alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID: 77,
		AccountID:           "acc-a",
		Repositories:        repos,
		MailboxItemResolver: mailboxClaimTestResolver(65, 120, 0),
		Body:                currentMailIDList(claimObject),
	}
}

// TestMailboxSnapshotKeepsOneRowPerAttachment pins what the page carries: one
// summary row per attachment, each with its own claim object and all of them
// keyed to the same letter. sub_2709990 merges the rows by that letter id but
// aborts the merge whenever the incoming claim object already sits in the
// entry's vector, so equal claim objects collapse a letter to one visible item.
func TestMailboxSnapshotKeepsOneRowPerAttachment(t *testing.T) {
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	mail := dnfrepo.MailRecord{
		MailID:            "52",
		SenderCharacterID: "11",
		SenderName:        "系统",
		Body:              "背包空间不足",
		CreatedAt:         now,
		Attachments: []dnfrepo.MailAttachment{
			{ItemID: 42, Count: 7},
			{ItemID: 44, Count: 1},
			{ItemID: 10000391, Count: 2},
		},
	}
	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{mail}, 0, now)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	if body[0] != 3 {
		t.Fatalf("summary count = %d, want one row per attachment", body[0])
	}
	want, err := MailboxAttachmentClaimObject(52, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(body[2:6]); got != want {
		t.Fatalf("leading dword = %d, want the first slot's claim object %d", got, want)
	}
	// Every slot must reach the client with its own claim object, or the merge
	// drops it and the letter renders short.
	for index := range mail.Attachments {
		claimObject, err := MailboxAttachmentClaimObject(52, index)
		if err != nil {
			t.Fatal(err)
		}
		var encoded [4]byte
		binary.LittleEndian.PutUint32(encoded[:], claimObject)
		if !bytes.Contains(body, encoded[:]) {
			t.Fatalf("slot %d claim object %d is missing from the page", index, claimObject)
		}
	}
}

// TestMailboxExtractClaimsOneSlot covers the slot-addressed claim the decoder
// supports: it delivers exactly that attachment, keeps the other slots at their
// original positions and leaves the letter claimable.
func TestMailboxExtractClaimsOneSlot(t *testing.T) {
	ctx := context.Background()
	repos := mailboxSlotTestRepos(t, ctx, []dnfrepo.MailAttachment{
		{ItemID: 2001, Count: 1},
		{ItemID: 2002, Count: 3},
		{ItemID: 2003, Count: 1},
	})
	claimObject, err := MailboxAttachmentClaimObject(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewHandler().Handle(ctx, mailboxSlotClaim(repos, claimObject))
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	body := got.UpperResponses[0].Body
	if len(body) == 2 && body[0] == 0x00 {
		t.Fatalf("slot claim was rejected with code %d", body[1])
	}
	// (count, letter id, claim object, mode) - the letter id keys the client's
	// container and the claim object selects the slot inside it.
	if binary.LittleEndian.Uint32(body[1:5]) != 1 ||
		binary.LittleEndian.Uint32(body[5:9]) != 1 ||
		binary.LittleEndian.Uint32(body[9:13]) != claimObject ||
		binary.LittleEndian.Uint32(body[13:17]) != 0 {
		t.Fatalf("ack must report the letter and the claimed object: % X", body)
	}
	// A letter with slots still pending is not retired from the client's list.
	if len(got.PostItemSlotRefreshResponses) != 1 ||
		!bytes.Equal(got.PostItemSlotRefreshResponses[0].Body, buildMailboxRemoveNotification(nil)) {
		t.Fatalf("partial claim must not remove the letter: %+v", got.PostItemSlotRefreshResponses)
	}

	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	for _, stack := range inventory.Slots {
		if stack.ItemID == 2001 || stack.ItemID == 2003 {
			t.Fatalf("only the addressed slot may be delivered: %+v", stack)
		}
	}
	delivered := int64(0)
	for _, stack := range inventory.Slots {
		if stack.ItemID == 2002 {
			delivered += stack.Count
		}
	}
	if delivered != 3 {
		t.Fatalf("claimed slot delivered %d, want 3", delivered)
	}

	box, _, _ := repos.Mailbox.Load(ctx, "77")
	mail := box.Mails["1"]
	if mail.Claimed {
		t.Fatalf("a letter with pending slots must stay claimable: %+v", mail)
	}
	if len(mail.Attachments) != 3 || mail.Attachments[0].ItemID != 2001 || mail.Attachments[1].ItemID != 0 || mail.Attachments[2].ItemID != 2003 {
		t.Fatalf("attachments = %+v, want the claimed slot blanked in place", mail.Attachments)
	}
}

// TestMailboxExtractWholeLetterClaimsEveryAttachment is what this client
// actually sends: the letter id, which hands over everything the letter holds
// and reports one row per slot.
func TestMailboxExtractWholeLetterClaimsEveryAttachment(t *testing.T) {
	ctx := context.Background()
	repos := mailboxSlotTestRepos(t, ctx, []dnfrepo.MailAttachment{
		{ItemID: 2001, Count: 1},
		{ItemID: 2002, Count: 1},
	})
	got, err := NewHandler().Handle(ctx, mailboxSlotClaim(repos, 1))
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	body := got.UpperResponses[0].Body
	if len(body) != 5+2*12 || binary.LittleEndian.Uint32(body[1:5]) != 2 {
		t.Fatalf("whole-letter ack must report both slots: % X", body)
	}
	for index := 0; index < 2; index++ {
		want, _ := MailboxAttachmentClaimObject(1, index)
		offset := 5 + index*12
		if binary.LittleEndian.Uint32(body[offset:offset+4]) != 1 || binary.LittleEndian.Uint32(body[offset+4:offset+8]) != want {
			t.Fatalf("ack row %d = % X, want letter 1 slot object %d", index, body[offset:offset+12], want)
		}
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if mail := box.Mails["1"]; !mail.Claimed || len(mail.Attachments) != 0 {
		t.Fatalf("a whole-letter claim must close the letter: %+v", mail)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	delivered := 0
	for _, stack := range inventory.Slots {
		if stack.ItemID == 2001 || stack.ItemID == 2002 {
			delivered += int(stack.Count)
		}
	}
	if delivered != 2 {
		t.Fatalf("both attachments must be delivered, got %d", delivered)
	}
}

// TestMailboxSnapshotRendersALetterPastTheAddressableSlots keeps the other
// "mailbox will not open" shape closed: a letter carrying more attachments than
// a claim object can address must still produce a page. The slots past the
// sixteenth fall back to the letter id and merge into its first slot instead of
// failing the whole snapshot.
func TestMailboxSnapshotRendersALetterPastTheAddressableSlots(t *testing.T) {
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	attachments := make([]dnfrepo.MailAttachment, 0, mailboxMaxAttachmentsPerMail+4)
	for index := 0; index < cap(attachments); index++ {
		attachments = append(attachments, dnfrepo.MailAttachment{ItemID: int64(2000 + index), Count: 1})
	}
	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{{
		MailID:            "52",
		SenderCharacterID: "11",
		SenderName:        "系统",
		CreatedAt:         now,
		Attachments:       attachments,
	}}, 0, now)
	if err != nil {
		t.Fatalf("an unaddressable slot must never fail the whole page: %v", err)
	}
	if int(body[0]) != len(attachments) {
		t.Fatalf("summary count = %d, want %d", body[0], len(attachments))
	}
	var letterID [4]byte
	binary.LittleEndian.PutUint32(letterID[:], 52)
	if !bytes.Contains(body, letterID[:]) {
		t.Fatal("the fallback rows must address the letter itself")
	}
}

// TestMailboxSnapshotSkipsEmptiedSlots is the "mailbox will not open"
// regression: a letter carrying an already-emptied slot must still render.
func TestMailboxSnapshotSkipsEmptiedSlots(t *testing.T) {
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	mail := dnfrepo.MailRecord{
		MailID:            "52",
		SenderCharacterID: "11",
		SenderName:        "系统",
		CreatedAt:         now,
		Attachments: []dnfrepo.MailAttachment{
			{},
			{ItemID: 44, Count: 1},
			{ItemID: 0, Count: 5},
		},
	}
	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{mail}, 0, now)
	if err != nil {
		t.Fatalf("an emptied slot must never fail the whole page: %v", err)
	}
	if body[0] != 1 {
		t.Fatalf("summary count = %d, want only the one remaining attachment", body[0])
	}
	// The surviving row keeps the claim object of its own position, so a slot the
	// client already holds never changes address while its neighbours are emptied.
	want, err := MailboxAttachmentClaimObject(52, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(body[2:6]); got != want {
		t.Fatalf("leading dword = %d, want slot 1's claim object %d", got, want)
	}
}
