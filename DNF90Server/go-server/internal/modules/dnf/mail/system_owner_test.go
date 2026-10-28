package mail

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestSystemMailOwnerCommitsGoldAndAttachmentThroughMailboxUnitOfWork(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7",
		AccountID:   "dnf:test",
		Name:        "Receiver",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := NewOwner(repositories).SendSystem(ctx, SystemSendRequest{
		RecipientName: "Receiver",
		Title:         "GM delivery",
		Body:          "Native mailbox transaction",
		Gold:          1234,
		Attachments: []dnfrepo.MailAttachment{{
			ItemID: 3227,
			Count:  5,
			Bind:   true,
		}},
		Source: "gm_admin_system_mail",
	})
	if err != nil {
		t.Fatalf("SendSystem() error = %v", err)
	}
	if result.MailID != "1" || result.RecipientCharacterID != "7" || result.RecipientName != "Receiver" {
		t.Fatalf("SendSystem() = %+v", result)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "7")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t error=%v", found, err)
	}
	got := mailbox.Mails["1"]
	if got.Gold != 1234 || got.SenderName != "系统" || got.RecipientName != "Receiver" ||
		len(got.Attachments) != 1 || got.Attachments[0].ItemID != 3227 || got.Attachments[0].Count != 5 ||
		got.Metadata["system_mail"] != "true" || got.Metadata["unlimited"] != "true" {
		t.Fatalf("durable system mail = %+v", got)
	}
}

func TestSystemMailOwnerRejectsMismatchedRecipientSelector(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "7", Name: "Receiver"}); err != nil {
		t.Fatal(err)
	}
	_, err := NewOwner(repositories).SendSystem(ctx, SystemSendRequest{
		RecipientCharacterID: "7",
		RecipientName:        "Different",
		Body:                 "should fail",
		Source:               "gm_admin_system_mail",
	})
	if err == nil {
		t.Fatal("SendSystem() accepted mismatched character id and name")
	}
	if _, found, loadErr := repositories.Mailbox.Load(ctx, "7"); loadErr != nil || found {
		t.Fatalf("mismatched request changed mailbox found=%t error=%v", found, loadErr)
	}
}

func TestSystemMailOwnerCompletionKeyReplaysOneAuctionPayoutMail(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "7", Name: "Receiver"}); err != nil {
		t.Fatal(err)
	}
	request := SystemSendRequest{
		RecipientCharacterID: "7", RecipientName: "Receiver", Title: "拍卖行成交款",
		Gold: 150, Source: "auction_sale_proceeds", CompletionKey: "auction:payout:17:0",
	}
	first, err := NewOwner(repositories).SendSystem(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewOwner(repositories).SendSystem(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "7")
	if err != nil || !found || first.MailID != second.MailID || len(mailbox.Mails) != 1 ||
		mailbox.Mails[first.MailID].Metadata["completion_key"] != request.CompletionKey {
		t.Fatalf("first=%+v second=%+v found=%t err=%v mailbox=%+v", first, second, found, err, mailbox)
	}
}
