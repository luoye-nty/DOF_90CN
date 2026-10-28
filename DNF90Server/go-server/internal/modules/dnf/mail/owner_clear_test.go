package mail

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerClearAllRemovesEveryMailboxRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "88",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {
				MailID:      "1",
				Gold:        120,
				Attachments: []dnfrepo.MailAttachment{{ItemID: 5001, Count: 1}},
				CreatedAt:   now,
			},
			"2": {
				MailID:    "2",
				Read:      true,
				Claimed:   true,
				CreatedAt: now,
			},
			"3": {
				MailID:    "3",
				Deleted:   true,
				CreatedAt: now,
			},
		},
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := NewOwner(repositories).ClearAll(ctx, "88")
	if err != nil {
		t.Fatalf("ClearAll() error = %v", err)
	}
	if result.Removed != 3 || result.Unread != 1 || result.Claimable != 1 || result.Gold != 120 || result.Attachments != 1 {
		t.Fatalf("ClearAll() = %+v", result)
	}
	record, found, err := repositories.Mailbox.Load(ctx, "88")
	if err != nil || !found || len(record.Mails) != 0 {
		t.Fatalf("mailbox after clear found=%t error=%v record=%+v", found, err, record)
	}
}
