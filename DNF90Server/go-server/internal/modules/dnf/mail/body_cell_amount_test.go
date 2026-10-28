package mail

// The amount a mailbox cell prints comes from the op0x61 summary's gold slot,
// not from the item row's count. The current reader takes it there and only
// overrides it for the gold certificates it can price from the item id:
//
//	FUN_034e3a50(&gold);     ; the summary's amount slot
//	FUN_034e3a50(&itemID);
//	if (itemID - 0x28eb7dU < 9)  gold = (itemID - 0x28eb7c) * 1000000;
//	else if (itemID - 0x28eb86U < 3) gold = (itemID - 0x28eb85) * 10000000;
//	FUN_0270bfb0(0, claim, .., gold, ..);
//
// so a 代币券 attachment - money, credited to the account wallet on claim - must
// carry its count in that slot, or the seller's sale letter shows an icon and a
// zero. This is what the live consignment letter did.

import (
	"encoding/binary"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// mailboxSummaryWireFields is one decoded summary row of the op0x61 page.
type mailboxSummaryWireFields struct {
	claimObject uint32
	amount      uint32
	itemID      uint32
	itemCount   uint32
	mailID      uint32
}

func parseMailboxSummaryWire(t *testing.T, body []byte) []mailboxSummaryWireFields {
	t.Helper()
	if len(body) < 2 {
		t.Fatalf("mailbox page too short: %d", len(body))
	}
	count := int(body[0])
	off := 2 // summary count + clear-first flag
	out := make([]mailboxSummaryWireFields, 0, count)
	readU32 := func() uint32 {
		if off+4 > len(body) {
			t.Fatalf("mailbox page ran out at %d", off)
		}
		v := binary.LittleEndian.Uint32(body[off : off+4])
		off += 4
		return v
	}
	for index := 0; index < count; index++ {
		var fields mailboxSummaryWireFields
		fields.claimObject = readU32()
		readU32() // sender wire id
		nameLen := int(readU32())
		off += nameLen
		fields.amount = readU32()
		fields.itemID = readU32()
		off++ // has-item byte
		fields.itemCount = readU32()
		off += 2 + 1 + 4 + 1 + 1 + 2 // use count, bind, dword, two bytes, u16
		off += mailboxItemRawSize
		off += 4 // remaining seconds
		fields.mailID = readU32()
		off++ // mail type
		out = append(out, fields)
	}
	return out
}

func TestMailboxPagePricesWalletVouchersInTheCellAmountSlot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	mail := dnfrepo.MailRecord{
		MailID:      "21",
		SenderName:  "系统",
		Title:       "- 已成功拍卖[1百万] -",
		Body:        "成交价 : + 188 代币券",
		CreatedAt:   now,
		Attachments: []dnfrepo.MailAttachment{{ItemID: 2681762, Count: 132}},
	}
	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{mail}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	summaries := parseMailboxSummaryWire(t, body)
	if len(summaries) != 1 {
		t.Fatalf("summaries=%d want 1", len(summaries))
	}
	got := summaries[0]
	if got.itemID != 2681762 || got.itemCount != 132 {
		t.Fatalf("voucher row item=%d count=%d want 2681762/132", got.itemID, got.itemCount)
	}
	if got.amount != 132 {
		t.Fatalf("voucher cell amount=%d want the 代币券 count 132; the client prints this slot, not the item count", got.amount)
	}

	// The happy-token voucher is money too.
	happy := mail
	happy.MailID = "22"
	happy.Attachments = []dnfrepo.MailAttachment{{ItemID: 2681917, Count: 7}}
	body, err = buildMailboxListNotification([]dnfrepo.MailRecord{happy}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseMailboxSummaryWire(t, body)[0]; got.amount != 7 {
		t.Fatalf("happy voucher cell amount=%d want 7", got.amount)
	}

	// An ordinary item is not money: its cell keeps the zero amount and the
	// client draws the item itself, so the count must not leak into this slot.
	ordinary := mail
	ordinary.MailID = "23"
	ordinary.Attachments = []dnfrepo.MailAttachment{{ItemID: 28309, Count: 48}}
	body, err = buildMailboxListNotification([]dnfrepo.MailRecord{ordinary}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseMailboxSummaryWire(t, body)[0]; got.amount != 0 {
		t.Fatalf("ordinary item cell amount=%d want 0", got.amount)
	}
}

// A letter that carries gold as well keeps the gold on the row that owns it; the
// voucher row prices itself, and a single row cannot be both.
func TestMailboxPageKeepsLetterGoldAndVoucherAmountApartOnTheSameLetter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	mail := dnfrepo.MailRecord{
		MailID:      "31",
		SenderName:  "系统",
		Title:       "混合信件",
		Body:        "测试",
		Gold:        19_500,
		CreatedAt:   now,
		Attachments: []dnfrepo.MailAttachment{{ItemID: 2681762, Count: 132}},
	}
	body, err := buildMailboxListNotification([]dnfrepo.MailRecord{mail}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	summaries := parseMailboxSummaryWire(t, body)
	if len(summaries) != 1 {
		t.Fatalf("summaries=%d want 1", len(summaries))
	}
	// The first (and here only) row owns the letter's gold, so that is the amount
	// it must print; the voucher count stays on the item row beside it.
	if summaries[0].amount != 19_500 {
		t.Fatalf("mixed letter cell amount=%d want the letter gold 19500", summaries[0].amount)
	}
	if summaries[0].itemCount != 132 {
		t.Fatalf("mixed letter item count=%d want 132", summaries[0].itemCount)
	}
}
