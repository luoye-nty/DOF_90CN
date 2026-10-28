package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	dnfauction "longheng.io/server/internal/modules/dnf/auction"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfmail "longheng.io/server/internal/modules/dnf/mail"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// ---------------------------------------------------------------------------
// Problem 1: the op183 success ack must tell the two panels apart.
//
// Live 2026-09-21T10:24:01Z: an op183 mode-1 consignment was answered `0100`
// (success + the item auction's pay byte), and the client opened the ordinary
// 上架 window instead of finishing the gold one. The item auction's own `0100`
// at 10:24:41 was correct and must stay that way.
// ---------------------------------------------------------------------------

func TestCurrentAuctionRegisterAckKeepsThePanelsApart(t *testing.T) {
	// The item auction keeps its own builder and its own pay byte.
	if got := buildCurrentAuctionRegisterSuccessBody(101, false); !bytes.Equal(got, []byte{currentAuctionPayTypeGold}) {
		t.Fatalf("item op183 ack body=%x want=[00]", got)
	}
	// The gold panel must hear mode 1, or it opens the wrong window.
	if got := buildCurrentGoldConsignRegisterSuccessBody(101, false); !bytes.Equal(got, []byte{currentAuctionPayTypeConsignment}) {
		t.Fatalf("gold op183 ack body=%x want=[01]", got)
	}
	// On the wire sendGameUpperSuccess prepends the success flag, so the two
	// panels see 01 00 and 01 01 respectively.
	itemWire := upperSuccessBody(buildCurrentAuctionRegisterSuccessBody(101, false))
	goldWire := upperSuccessBody(buildCurrentGoldConsignRegisterSuccessBody(101, false))
	if !bytes.Equal(itemWire, []byte{1, currentAuctionPayTypeGold}) {
		t.Fatalf("item op183 wire=%x want=0100", itemWire)
	}
	if !bytes.Equal(goldWire, []byte{1, currentAuctionPayTypeConsignment}) {
		t.Fatalf("gold op183 wire=%x want=0101", goldWire)
	}
	// A robot session carries the private trailing listing id in both cases.
	itemRobot := buildCurrentAuctionRegisterSuccessBody(101, true)
	if len(itemRobot) != 9 || itemRobot[0] != currentAuctionPayTypeGold ||
		binary.LittleEndian.Uint64(itemRobot[1:9]) != 101 {
		t.Fatalf("item robot ack=%x", itemRobot)
	}
	goldRobot := buildCurrentGoldConsignRegisterSuccessBody(101, true)
	if len(goldRobot) != 9 || goldRobot[0] != currentAuctionPayTypeConsignment ||
		binary.LittleEndian.Uint64(goldRobot[1:9]) != 101 {
		t.Fatalf("gold robot ack=%x", goldRobot)
	}
}

// ---------------------------------------------------------------------------
// Problem 2: a consignment has no bid, so the 竞拍价 line must not render.
//
// The shared painter showed the Cera price in both price slots (an op188 row
// carried `6F000000 6F000000`) because Listing.BidUnitPrice falls back to
// UnitPrice for a buyout-only row.
//
// Zeroing the slot did NOT hide the line - 0 is a real price, so the row simply
// read 「当前竞拍 0 点券」, which is exactly what the operator reported. The gold
// panel's painter (owner 8, FUN_02076540) hides the line only when the slot
// equals 0xFFFFFFFF:
//
//	020768c9  CMP dword ptr [ESI + 0x3d1], -1   ; widget+0x3C8+0x09 = the bid slot
//	020768d0  JZ  0x02076a33                    ; -1 => the 当前竞拍 block is skipped
//	0207673e  CMP dword ptr [ESI + 0x3d5], EBX(-1) ; +0x0D = the buyout slot
//	02076744  JZ  0x020768c3                       ; -1 => that block is skipped
//
// so the row has to carry the sentinel, and nothing else may change.
// ---------------------------------------------------------------------------

func goldConsignFixtureListing() dnfauction.GoldConsignListing {
	return dnfauction.GoldConsignListing{
		ID:         77,
		SellerID:   "11",
		SellerName: "卖家",
		GoldItemID: 2681725,
		GoldAmount: 1_000_000,
		PriceCera:  111,
		FeePercent: 5,
	}
}

func TestCurrentGoldConsignRowsMarkTheBidSlotAsNoBid(t *testing.T) {
	listings := []dnfauction.GoldConsignListing{goldConsignFixtureListing()}

	search := buildCurrentGoldConsignSearchBody(listings, 0, 100)
	if len(search) <= currentGoldConsignSearchHeaderSize {
		t.Fatalf("search body too short: %x", search)
	}
	if search[0] != currentAuctionPayTypeConsignment {
		t.Fatalf("search mode byte=%d want=1", search[0])
	}
	row := search[currentGoldConsignSearchHeaderSize:]
	if len(row) < currentGoldConsignSearchRowSize {
		t.Fatalf("search row short: %d", len(row))
	}
	bid := binary.LittleEndian.Uint32(row[currentGoldConsignSearchBidOffset : currentGoldConsignSearchBidOffset+4])
	buyout := binary.LittleEndian.Uint32(row[0x0D:0x11])
	if bid != currentGoldConsignNoBid {
		t.Fatalf("op186 gold row 竞拍价=0x%08X want=0x%08X (row=%x)", bid, currentGoldConsignNoBid, row[:0x20])
	}
	if buyout != 111 {
		t.Fatalf("op186 gold row 一口价=%d want=111 - the sentinel must not touch the buyout", buyout)
	}

	owned := buildCurrentGoldConsignOwnedBody(listings, "11")
	if len(owned) <= currentGoldConsignOwnedHeaderSize {
		t.Fatalf("owned body too short: %x", owned)
	}
	if owned[0] != currentAuctionPayTypeConsignment {
		t.Fatalf("owned mode byte=%d want=1", owned[0])
	}
	if owned[1] != 1 {
		t.Fatalf("owned count=%d want=1", owned[1])
	}
	ownedRow := owned[currentGoldConsignOwnedHeaderSize:]
	ownedBid := binary.LittleEndian.Uint32(ownedRow[currentGoldConsignOwnedBidOffset : currentGoldConsignOwnedBidOffset+4])
	ownedBuyout := binary.LittleEndian.Uint32(ownedRow[0x0C:0x10])
	if ownedBid != currentGoldConsignNoBid {
		t.Fatalf("op188 gold row 竞拍价=0x%08X want=0x%08X (row=%x)", ownedBid, currentGoldConsignNoBid, ownedRow[:0x20])
	}
	if ownedBuyout != 111 {
		t.Fatalf("op188 gold row 一口价=%d want=111", ownedBuyout)
	}
	// The unit price the 一口价 column multiplies from must survive too.
	if unit := binary.LittleEndian.Uint32(ownedRow[0x93:0x97]); unit != 111 {
		t.Fatalf("op188 gold row unit price=%d want=111", unit)
	}
}

// The sentinel is the value FUN_02076540 compares against, so it must be the
// exact all-ones dword - not 0, not -1 as a signed 64-bit value, not a flag.
func TestCurrentGoldConsignNoBidSentinelMatchesThePainterCompare(t *testing.T) {
	if currentGoldConsignNoBid != 0xFFFFFFFF {
		t.Fatalf("no-bid sentinel=0x%08X want=0xFFFFFFFF", currentGoldConsignNoBid)
	}
	// A zero slot is a real price and is exactly what the rejected first attempt
	// wrote; pin that the two can never be confused again.
	if currentGoldConsignNoBid == 0 {
		t.Fatal("the no-bid sentinel must not be zero")
	}
}

// markCurrentGoldConsignNoBid must never walk off the end of a body, including
// the empty and header-only shapes the client accepts.
func TestCurrentGoldConsignNoBidMarkingIsBoundsSafe(t *testing.T) {
	for _, test := range []struct {
		name             string
		body             []byte
		header, row, off int
	}{
		{"nil", nil, 7, 0xA8, 0x09},
		{"empty", []byte{}, 7, 0xA8, 0x09},
		{"header only", make([]byte, 7), 7, 0xA8, 0x09},
		{"zero row size", make([]byte, 64), 7, 0, 0x09},
		{"single byte", []byte{1}, 7, 0xA8, 0x09},
	} {
		t.Run(test.name, func(t *testing.T) {
			markCurrentGoldConsignNoBid(test.body, test.header, test.row, test.off)
		})
	}
	// One full row plus a partial second must rewrite exactly the first.
	body := make([]byte, 7+0xA8+0x20)
	for i := range body {
		body[i] = 0x00
	}
	markCurrentGoldConsignNoBid(body, 7, 0xA8, 0x09)
	if got := binary.LittleEndian.Uint32(body[7+0x09 : 7+0x0D]); got != currentGoldConsignNoBid {
		t.Fatalf("first row bid=0x%08X want the sentinel", got)
	}
	// The bytes around the four-byte slot must be untouched.
	if body[7+0x08] != 0 || body[7+0x0D] != 0 {
		t.Fatalf("sentinel write leaked outside the slot: %x", body[7+0x08:7+0x0E])
	}
	if body[7+0xA8+0x09] != 0 {
		t.Fatal("marking reached into an incomplete second row")
	}
}

// ---------------------------------------------------------------------------
// Problems 4 and 5: the system notice.
//
// FUN_01d80150 is registered as class-0 msg 0xA9 by
// FUN_02262980(0xa9, FUN_01d80150, 0). It reads two bytes through
// FUN_034e3970(x,1), a u32 through FUN_034e3a50 (cursor -= 4) and a u16 through
// FUN_034e39e0 (cursor -= 2) - eight bytes, with no trailing block, because
// FUN_02fd3b90 is a panel factory (owner 7 = 0x824 bytes, owner 8 = 0x250
// bytes), not a cursor advance.
// ---------------------------------------------------------------------------

func TestCurrentAuctionNoticeBodyMatchesTheClientReader(t *testing.T) {
	body := buildCurrentAuctionNoticeBody(
		currentAuctionNoticeRouteGold,
		currentAuctionNoticeKindSellerSold,
		2681725,
	)
	// route u8 | kind u8 | itemID u32 | count u16
	want := []byte{0x01, 0x01, 0x7D, 0xEB, 0x28, 0x00, 0x00, 0x00}
	if !bytes.Equal(body, want) {
		t.Fatalf("notice body=%x want=%x", body, want)
	}
	if len(body) != 8 {
		t.Fatalf("notice body len=%d want=8", len(body))
	}
}

// The route byte is what sends the banner to owner 7 (item auction) or owner 8
// (金币寄售). Every kind in FUN_01d80150's switch must round-trip.
func TestCurrentAuctionNoticeKindTableMatchesTheClientSwitch(t *testing.T) {
	for kind, wantItem := range map[byte]bool{
		currentAuctionNoticeKindOutbid:       true,
		currentAuctionNoticeKindSellerSold:   true,
		currentAuctionNoticeKindBuyerWon:     true,
		currentAuctionNoticeKindSellerFailed: true,
		currentAuctionNoticeKindCancelRefund: true,
		currentAuctionNoticeKindCancelBid:    true,
	} {
		_ = wantItem
		body := buildCurrentAuctionNoticeBody(currentAuctionNoticeRouteItem, kind, 6001)
		if body[0] != currentAuctionNoticeRouteItem || body[1] != kind {
			t.Fatalf("kind %d round trip=%x", kind, body)
		}
	}
	if currentAuctionNoticeMsgID != 0x00A9 {
		t.Fatalf("notice msg id=0x%04X want=0x00A9", currentAuctionNoticeMsgID)
	}
}

func TestCurrentAuctionNoticeLandsAsClass0OpA9(t *testing.T) {
	connection := &bufferConn{}
	recipient := &gameSession{conn: connection, connID: "notice-recipient", selectedCharacterID: 88}
	service := &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		gameSessions: map[uint16]*gameSession{88: recipient},
	}

	if err := service.sendCurrentAuctionNoticeToOnlineRecipient(
		88, currentAuctionNoticeRouteGold, currentAuctionNoticeKindBuyerWon, 2681725); err != nil {
		t.Fatalf("send notice: %v", err)
	}
	// The service above runs the 16-byte class-0 header, so the matching splitter
	// is the long-Heng one; with the 13-byte splitter the body is mis-sliced by
	// the three header bytes the packet carries before the payload.
	packet, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	if packet.Header.MsgID != currentAuctionNoticeMsgID || packet.Header.Classification != 0 {
		t.Fatalf("notice header=%+v want class0/0x00A9", packet.Header)
	}
	want := []byte{currentAuctionNoticeRouteGold, currentAuctionNoticeKindBuyerWon, 0x7D, 0xEB, 0x28, 0x00, 0x00, 0x00}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("notice wire body=%x want=%x", packet.Body, want)
	}
	if len(packet.Body) != 8 {
		t.Fatalf("notice wire body len=%d want=8 (no trailing block)", len(packet.Body))
	}

	// An offline recipient is a silent no-op, not an error.
	connection.write.Reset()
	if err := service.sendCurrentAuctionNoticeToOnlineRecipient(99, currentAuctionNoticeRouteGold, 1, 2681725); err != nil {
		t.Fatalf("offline notice: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatal("offline notice wrote to the wire")
	}
	// A zero or out of range item id never invents a banner.
	if err := service.sendCurrentAuctionNoticeToOnlineRecipient(88, 0, 0, 0); err != nil {
		t.Fatalf("zero item notice: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatal("notice with no item id wrote to the wire")
	}
}

// ---------------------------------------------------------------------------
// op185: the captured body is 17 bytes and ends with the tier the panel showed.
//
//	10:25:15  01 6F000000 6103B16A00000000 7DEB2800
//	          mode i32 total   i64 listingId   u32 tier(2681725)
// ---------------------------------------------------------------------------

func TestCurrentGoldConsignBuyDecoderReadsTheCapturedBody(t *testing.T) {
	// Exactly the bytes from packet_log.txt at 2026-09-21T10:25:15Z.
	captured := []byte{
		0x01,
		0x6F, 0x00, 0x00, 0x00,
		0x61, 0x03, 0xB1, 0x6A, 0x00, 0x00, 0x00, 0x00,
		0x7D, 0xEB, 0x28, 0x00,
	}
	if len(captured) != currentAuctionGoldBuyRequestSizeWithTrailer {
		t.Fatalf("fixture len=%d want=%d", len(captured), currentAuctionGoldBuyRequestSizeWithTrailer)
	}
	request, err := decodeCurrentGoldConsignBuyRequest(captured)
	if err != nil {
		t.Fatalf("decode captured: %v", err)
	}
	if request.ExpectedCera != 111 {
		t.Fatalf("expected cera=%d want=111", request.ExpectedCera)
	}
	if request.ListingID != 0x6AB10361 {
		t.Fatalf("listing id=0x%X want=0x6AB10361", request.ListingID)
	}
	if !request.HasGoldItemID || request.GoldItemID != 2681725 {
		t.Fatalf("tier=%d present=%t want 2681725 present", request.GoldItemID, request.HasGoldItemID)
	}

	// The pre-capture 13-byte shape stays accepted and simply carries no tier.
	short := captured[:currentAuctionGoldBuyRequestSize]
	shortRequest, err := decodeCurrentGoldConsignBuyRequest(short)
	if err != nil {
		t.Fatalf("decode 13-byte: %v", err)
	}
	if shortRequest.HasGoldItemID {
		t.Fatal("13-byte body must not claim a tier")
	}
	if shortRequest.ExpectedCera != 111 || shortRequest.ListingID != 0x6AB10361 {
		t.Fatalf("13-byte decode=%+v", shortRequest)
	}

	for _, test := range []struct {
		name string
		body []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"mode 0", append([]byte{0x00}, captured[1:]...)},
		{"truncated to 12", captured[:12]},
		{"one byte too long", append(append([]byte(nil), captured...), 0x00)},
		{"zero price", append([]byte{0x01, 0, 0, 0, 0}, captured[5:]...)},
		{"zero listing", append(append([]byte(nil), captured[:5]...), make([]byte, 12)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeCurrentGoldConsignBuyRequest(test.body); err == nil {
				t.Fatalf("accepted %x", test.body)
			}
		})
	}
	// A zero tier is a malformed trailer rather than an absent one.
	zeroTier := append([]byte(nil), captured...)
	for i := 13; i < 17; i++ {
		zeroTier[i] = 0
	}
	if _, err := decodeCurrentGoldConsignBuyRequest(zeroTier); err == nil {
		t.Fatal("accepted a zero tier trailer")
	}
}

// ---------------------------------------------------------------------------
// Problem 3: a gold letter must raise the live mailbox indicator.
//
// The item auction fires class0/op0x63 next to its own mail sends; without it a
// consignment letter stayed invisible until the player opened the mailbox.
// ---------------------------------------------------------------------------

func TestCurrentGoldConsignLetterFiresTheMailboxAlarm(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88",
		AccountID:   "acct",
		Name:        "收件人",
	}); err != nil {
		t.Fatalf("save recipient: %v", err)
	}
	connection := &bufferConn{}
	recipient := &gameSession{conn: connection, connID: "gold-letter-recipient", selectedCharacterID: 88}
	service := &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{88: recipient},
	}

	if err := service.sendCurrentGoldConsignLetter(ctx, dnfmail.SystemSendRequest{
		RecipientCharacterID: "88",
		Title:                "金币寄售成交",
		Body:                 "测试",
		Source:               "goldconsign_test",
		CompletionKey:        "goldconsign:test:1",
	}); err != nil {
		t.Fatalf("send gold letter: %v", err)
	}
	// The alarm body is the unread count as one u16; the 16-byte class-0 header
	// leaves exactly those two bytes behind the splitter.
	alarm, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if alarm.Header.MsgID != 0x0063 || alarm.Header.Classification != 0 {
		t.Fatalf("alarm header=%+v want class0/0x0063", alarm.Header)
	}
	// One unread letter, so the count word is 1.
	if !bytes.Equal(alarm.Body, []byte{0x01, 0x00}) {
		t.Fatalf("alarm body=% X want 01 00", alarm.Body)
	}
	// Immediately behind it comes the mailbox list page. Without that page the
	// client only bumps a counter, and an already-open mailbox never reloads.
	page, trailing := splitLongHengGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("gold letter left trailing bytes on the wire: %x", trailing)
	}
	if page.Header.MsgID != 0x0061 || page.Header.Classification != 0 {
		t.Fatalf("mailbox page header=%+v want class0/0x0061", page.Header)
	}
	// One summary row, and the second byte is the clear-first flag the client's
	// sub_1D913C0 uses to reset both mail containers before the snapshot.
	if len(page.Body) < 2 || page.Body[0] != 1 || page.Body[1] != 0 {
		t.Fatalf("mailbox page body=% X want one summary and a zero clear flag", page.Body)
	}
}

// The gold refresh is a gold-only projection: the shared alarm helper used by
// every other mail path must still put exactly one packet on the wire, so the
// item auction's delivery stays byte-identical.
func TestCurrentGoldConsignMailboxRefreshStaysOutOfOtherPaths(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "acct",
		Name:        "其他收件人",
	}); err != nil {
		t.Fatalf("save recipient: %v", err)
	}
	connection := &bufferConn{}
	recipient := &gameSession{conn: connection, connID: "plain-recipient", selectedCharacterID: 99}
	service := &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{99: recipient},
	}
	if err := service.sendMailboxAlarmToOnlineRecipient(99); err != nil {
		t.Fatalf("alarm: %v", err)
	}
	packet, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("the shared alarm helper grew a second packet: %x", trailing)
	}
	if packet.Header.MsgID != 0x0063 {
		t.Fatalf("shared alarm msg id=0x%04X want 0x0063", packet.Header.MsgID)
	}
	// An offline or unknown recipient is a silent no-op for the page too.
	connection.write.Reset()
	if err := service.sendMailboxListRefreshToOnlineRecipient(0); err != nil {
		t.Fatalf("zero id: %v", err)
	}
	if err := service.sendMailboxListRefreshToOnlineRecipient(77); err != nil {
		t.Fatalf("offline recipient: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatal("an offline recipient was written to")
	}
}

// ---------------------------------------------------------------------------
// op185's trailing tier must agree with the row its listing id resolves to, or
// the buyer is confirming a panel that has since changed denomination.
// ---------------------------------------------------------------------------

func TestCurrentGoldConsignBuyRefusesAStaleTierPanel(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "5000"})
	// goldConsignTestService runs the 13-byte class-0 header, so the default
	// splitter is the matching one here.
	service := goldConsignTestService(repositories)
	seller := &gameSession{conn: &bufferConn{}, selectedCharacterID: 11, accountID: "account-seller"}
	// goldConsignLiveRegisterBody registers tier 2681725 (1百万) at 111 Cera.
	if err := service.registerCurrentAuctionItemLocked(seller, goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()

	buyerConn := &bufferConn{}
	buyer := &gameSession{conn: buyerConn, selectedCharacterID: 22, accountID: "account-buyer"}
	// The captured 17-byte shape, but naming a different tier than the row.
	buy := make([]byte, currentAuctionGoldBuyRequestSizeWithTrailer)
	buy[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint32(buy[1:5], 111)
	binary.LittleEndian.PutUint64(buy[5:13], listingID)
	binary.LittleEndian.PutUint32(buy[13:17], 2681726)

	if err := service.buyCurrentAuctionItemLocked(buyer, uint16(dnfenum.CmdPacketAuctionBidding), buy, false); err != nil {
		t.Fatal(err)
	}
	ack, _ := splitGameServerUpperPacket(t, buyerConn.write.Bytes())
	if ack.Body[0] != 0 {
		t.Fatalf("stale-tier buy was accepted: ack=%x", ack.Body)
	}
	// The reservation has to be released: the gold is still escrowed and the row
	// stays buyable at its real tier.
	if gold := goldConsignProtocolGold(t, ctx, repositories, "system:auction:escrow"); gold != 1_000_000 {
		t.Fatalf("escrow after stale-tier refusal = %d want 1000000 (reservation leaked)", gold)
	}
	if _, found, err := repositories.Mailbox.Load(ctx, "22"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("a refused buy wrote a letter")
	}

	// The same body carrying the tier the row actually holds still succeeds.
	goodConn := &bufferConn{}
	goodBuyer := &gameSession{conn: goodConn, selectedCharacterID: 22, accountID: "account-buyer"}
	good := append([]byte(nil), buy...)
	binary.LittleEndian.PutUint32(good[13:17], 2681725)
	if err := service.buyCurrentAuctionItemLocked(goodBuyer, uint16(dnfenum.CmdPacketAuctionBidding), good, false); err != nil {
		t.Fatal(err)
	}
	goodAck, _ := splitGameServerUpperPacket(t, goodConn.write.Bytes())
	if goodAck.Body[0] != 1 {
		t.Fatalf("matching-tier buy was refused: ack=%x", goodAck.Body)
	}
}
