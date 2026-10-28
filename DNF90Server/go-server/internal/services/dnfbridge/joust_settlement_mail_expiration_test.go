package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	dnfjoust "longheng.io/server/internal/modules/dnf/joust"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestReconcileCurrentMailboxAttachmentExpirationsAddsPVFDeadlineForAnySource(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "19",
		Mails: map[string]dnfrepo.MailRecord{
			"42": {
				MailID: "42",
				Attachments: []dnfrepo.MailAttachment{{
					ItemID: dnfjoust.PermanentCrystalID,
					Count:  232,
				}},
				Metadata: map[string]string{"source": "manual-system-grant"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		pvfItemCatalog:     mustTestJoustCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	changed, err := service.reconcileCurrentMailboxAttachmentExpirations(ctx, 19)
	if err != nil || changed != 1 {
		t.Fatalf("repair changed=%d err=%v", changed, err)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t err=%v", found, err)
	}
	want := time.Date(2028, time.August, 15, 22, 0, 0, 0, time.UTC)
	attachment := mailbox.Mails["42"].Attachments[0]
	if got := attachment.ExpireAt; !got.Equal(want) {
		t.Fatalf("attachment expiry=%s want=%s", got, want)
	}
	if len(attachment.RawEntry) != currentItemListEntryWireSize ||
		binary.LittleEndian.Uint32(attachment.RawEntry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4]) != uint32(want.Unix()) ||
		binary.LittleEndian.Uint16(attachment.RawEntry[0x0b:0x0d]) != math.MaxUint16 {
		t.Fatalf("attachment row was not normalized: %+v", attachment)
	}
	changed, err = service.reconcileCurrentMailboxAttachmentExpirations(ctx, 19)
	if err != nil || changed != 0 {
		t.Fatalf("repeat repair changed=%d err=%v", changed, err)
	}
}
