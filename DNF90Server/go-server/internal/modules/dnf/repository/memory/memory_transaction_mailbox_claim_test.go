package memory

import (
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/repository"
)

func TestMemoryMailboxClaimAssetsRollsBackEpicBookAndMailboxTogether(t *testing.T) {
	ctx := context.Background()
	repositories := NewMemoryGroup()
	if err := repositories.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Mailbox.Save(ctx, repository.MailboxRecord{
		CharacterID: "77",
		Mails: map[string]repository.MailRecord{
			"1": {MailID: "1", Attachments: []repository.MailAttachment{{ItemID: 10300528, Count: 7}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.EpicBook.Save(ctx, repository.EpicBookRecord{
		AccountID: "acc-a",
		Balances:  map[uint32]uint32{101020256: 10},
	}); err != nil {
		t.Fatal(err)
	}
	claimAssets, ok := repositories.MailboxAssets.(repository.MailboxClaimAssetUnitOfWork)
	if !ok {
		t.Fatal("memory mailbox transaction lacks Epic Book claim capability")
	}
	wantErr := errors.New("reject staged mail claim")
	err := claimAssets.WithinMailboxClaimAssets(ctx, "77", "77", func(
		_ repository.AccountRepository,
		_ repository.AccountInventoryRepository,
		_ repository.CharacterRepository,
		_ repository.InventoryRepository,
		mailboxes repository.MailboxRepository,
		epicBooks repository.EpicBookRepository,
	) error {
		book, _, loadErr := epicBooks.Load(ctx, "acc-a")
		if loadErr != nil {
			return loadErr
		}
		book.Balances[101020256] = 17
		if saveErr := epicBooks.Save(ctx, book); saveErr != nil {
			return saveErr
		}
		mailbox, _, loadErr := mailboxes.Load(ctx, "77")
		if loadErr != nil {
			return loadErr
		}
		mail := mailbox.Mails["1"]
		mail.Claimed = true
		mail.Attachments = nil
		mailbox.Mails["1"] = mail
		if saveErr := mailboxes.Save(ctx, mailbox); saveErr != nil {
			return saveErr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithinMailboxClaimAssets error=%v, want=%v", err, wantErr)
	}
	book, found, err := repositories.EpicBook.Load(ctx, "acc-a")
	if err != nil || !found || book.Balances[101020256] != 10 {
		t.Fatalf("rolled-back Epic Book found=%v err=%v record=%+v", found, err, book)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "77")
	if err != nil || !found || mailbox.Mails["1"].Claimed || len(mailbox.Mails["1"].Attachments) != 1 {
		t.Fatalf("rolled-back mailbox found=%v err=%v record=%+v", found, err, mailbox)
	}
}
