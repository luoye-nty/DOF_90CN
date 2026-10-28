package dnfbridge

// 金币寄售 (gold consignment) protocol layer.
//
// Everything in this file is additive: the item auction keeps its own opcodes
// and its own body shapes untouched, and each handler below is reached only
// from a single `body[0] == currentAuctionPayTypeConsignment` branch inserted
// at the top of the matching item handler. See
// `金币寄售_客户端逆向规格与实现方案_20260921.md` for the captures each shape
// came from, and for the ones marked "inferred".

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfauction "longheng.io/server/internal/modules/dnf/auction"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfmail "longheng.io/server/internal/modules/dnf/mail"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// currentAuctionGoldRegisterRequestSize is the 金币寄售 op183 body:
	// {u8 mode=1, u8 0, u16 0, u32 tier item, u32 qty=1, i32 bid=-1,
	//  u32 Cera price, u32 0, u32 lifetime hours}. Live capture
	// 2026-09-21T09:32:48Z: 010000007DEB280001000000FFFFFFFF6F0000000000000018000000
	// (1百万 @ 111 Cera / 24h). Byte for byte the same as 86's
	// GoldConsignmentRegisterRequestParser.
	currentAuctionGoldRegisterRequestSize = 28

	// currentAuctionGoldCancelRequestSize is the 金币寄售 op184 body. INFERRED:
	// the client's own shape has never been captured because listing did not
	// work yet. It mirrors the item auction's op184 ({u8 mode, i64 listingId}).
	currentAuctionGoldCancelRequestSize = 9

	// currentAuctionGoldBuyRequestSize / WithTrailer are the 金币寄售 op185 and
	// op334 bodies. INFERRED the same way: the item auction's compact mode-1
	// shape is {u8 mode, i32 total, i64 listingId} with an optional 4-byte
	// trailer, and the gold panel's price control is the same single Cera field.
	currentAuctionGoldBuyRequestSize            = 13
	currentAuctionGoldBuyRequestSizeWithTrailer = 17

	// currentAuctionGoldCategoryRequestSize is the 金币寄售 op187 body. INFERRED
	// from the item auction's fixed 20-byte category request.
	currentAuctionGoldCategoryRequestSize = 20

	// currentAuctionNoticeMsgID is the class-0 auction notice. Read off the
	// client's own handler registration (registrar FUN_02262980):
	//     FUN_02262980(0xa9, FUN_01d80150, 0)   <- this notice
	//     FUN_02262980(0xb7, FUN_01d80260, 0)   <- the service-open notice
	// FUN_01d80150 reads exactly {u8 route, u8 kind, u32 itemID, u16 count}:
	// two one-byte reads through FUN_034e3970(x,1), a u32 through FUN_034e3a50
	// (cursor -= 4) and a u16 through FUN_034e39e0 (cursor -= 2). Its switch on
	// `kind` resolves dstr 60086..60091, and `route` picks which panel receives
	// it via FUN_02fd3b90: 0 ensures owner 7 (the item auction window, 0x824
	// bytes), 1 ensures owner 8 (the 金币寄售 window, 0x250 bytes).
	currentAuctionNoticeMsgID uint16 = 0x00A9

	// currentAuctionNoticeRouteItem / RouteGold are that route byte.
	currentAuctionNoticeRouteItem = byte(0)
	currentAuctionNoticeRouteGold = byte(1)

	// The six notice kinds, in FUN_01d80150's switch order.
	currentAuctionNoticeKindOutbid       = byte(0) // 60086 竞拍中出现更高价格，金额返还
	currentAuctionNoticeKindSellerSold   = byte(1) // 60087 上架的[%s]已被拍下
	currentAuctionNoticeKindBuyerWon     = byte(2) // 60088 竞拍的[%s]已拍下
	currentAuctionNoticeKindSellerFailed = byte(3) // 60089 上架的[%s]竞拍失败
	currentAuctionNoticeKindCancelRefund = byte(4) // 60090 拍卖被取消，金额返还
	currentAuctionNoticeKindCancelBid    = byte(5) // 60091 竞拍已被取消

	// Row geometry of the two gold list bodies, used to mark the 竞拍价 slot as
	// "no bid". Both inherit the item auction's row painter, whose price triple
	// sits at row+0x09 (0xA8 record) resp. row+0x08 (0x97 record) - the offsets
	// the repository's own
	// TestCurrentAuctionRowsRenderBidTotalSeparatelyFromBuyoutTotal pins for the
	// item panel, and the offsets the 0x97 normalisation below confirms from the
	// client side.
	currentGoldConsignSearchHeaderSize = 7 // mode u8 + total u32 + count u16
	currentGoldConsignSearchRowSize    = 0xA8
	currentGoldConsignSearchBidOffset  = 0x09

	currentGoldConsignOwnedHeaderSize = 2 // mode u8 + count u8
	currentGoldConsignOwnedRowSize    = 0x97
	currentGoldConsignOwnedBidOffset  = 0x08

	// currentGoldConsignNoBid is the "no bid" sentinel the 金币寄售 row painter
	// itself tests, and it is the same value the item auction's own three price
	// slots use - see currentAuctionNoPrice for the four painter compares. It is
	// NOT zero and NOT a flag byte: zero is a real price, which is why the first
	// attempt at this fix rendered "当前竞拍 0 点券" instead of removing the line.
	//
	// Owner 8 is the 0x250-byte 金币寄售 window (FUN_02fd3b90 case 8 ->
	// FUN_034e8520(0x250) -> FUN_0208a690), code at 0x0207xxxx..0x0209xxxx. Its
	// row pipeline is sub_20821A0 (op186, the whole 0xA8 record copied into
	// node+8), sub_2082280 (op188, 0x97), sub_2082360 (op189, 0x9F), each
	// allocated by FUN_00759030 with exactly that many bytes and copied by
	// FUN_02081900 / FUN_02081960 / FUN_020819C0 (0x2a, 0x25 and 0x27 dwords plus
	// a u16 and a u8), then normalised by sub_207DEE0 into ONE 0xA8 struct that
	// sub_207DDE0 stores at widget+0x3C8 and painted by sub_2076540.
	//
	//	020768c9  CMP dword ptr [ESI + 0x3d1], -1   ; widget+0x3C8+0x09 = bid
	//	020768d0  JZ  0x02076a33                    ; -1 => the whole 当前竞拍
	//	                                            ;   block is skipped
	//	020768d6  PUSH 0xeb01 ; GetString(60161) = 当前竞拍
	//	0207696b  ... GetString(0x1134f=70479) = "%s 点券" ... drawn at row+0x15
	//
	//	0207673e  CMP dword ptr [ESI + 0x3d5], EBX(-1) ; widget+0x3C8+0x0D = buyout
	//	02076744  JZ  0x020768c3                       ; -1 => that block is skipped
	//
	// So visibility is decided by comparing the slot against 0xFFFFFFFF, not by a
	// state flag and not by the row length. The 0x97 normalisation proves the same
	// convention from the other side: it hard-codes -1 into the struct's 0x11 slot
	// (`MOV dword ptr [EBP-0x9b],0xffffffff` @0207e14e) precisely so that column
	// draws nothing, and the row tooltip FUN_0207a760 derives 「每个: %s」 from the
	// buyout total whenever it sees that -1 (`CMP [ECX+0x3d9],-1` @0207a84d).
	currentGoldConsignNoBid = currentAuctionNoPrice
)

// buildCurrentGoldConsignRegisterSuccessBody is the 金币寄售 op183 success ack.
//
// The client routes a registration ack on its SECOND body byte, exactly as it
// routes the notice and failure acks: 0 is the item auction's window, 1 is the
// 金币寄售 window. Answering with the item auction's pay byte therefore made the
// client open the ordinary 上架 dialog after a successful consignment - live
// 2026-09-21T10:24:01Z, where an op183 mode-1 request got `0100` and the wrong
// window appeared. The item auction keeps buildCurrentAuctionRegisterSuccessBody
// unchanged and still answers `0100`, which is correct for its own panel.
func buildCurrentGoldConsignRegisterSuccessBody(listingID uint64, robotSessionBound bool) []byte {
	var response packetWriter
	response.writeByte(currentAuctionPayTypeConsignment)
	if robotSessionBound {
		response.writeUint64(listingID)
	}
	return response.bytes()
}

// markCurrentGoldConsignNoBid writes the painter's own "no bid" sentinel into the
// 竞拍价 slot of every record in a gold list body.
//
// A consignment has no bidding at all: the captured op183 always carries -1 and
// the ledger stores StartPrice -1, but dnfauction.Listing.BidUnitPrice falls
// back to UnitPrice for a buyout-only row, so the shared painter rendered the
// Cera price in the 竞拍价 column as well - live 2026-09-21, where an op188 row
// carried `6F000000 6F000000`.
//
// The first attempt zeroed the slot and did not work: the painter hides the line
// only when the slot equals 0xFFFFFFFF (see currentGoldConsignNoBid for the two
// cmp/jz pairs), so a zero simply rendered "当前竞拍 0 点券". Only the bid slot is
// written; the buyout total, the unit price, the name, the 期限 byte and the 83-byte
// item block all go out untouched, which is what keeps a consignment row's
// 价格(点券) column intact.
func markCurrentGoldConsignNoBid(body []byte, headerSize, rowSize, bidOffset int) {
	if rowSize <= 0 || bidOffset < 0 || bidOffset+4 > rowSize {
		return
	}
	// A record is only rewritten once it is complete. The builders above always
	// emit whole rows, so this is belt and braces: it keeps a future partial row
	// from being written past its own end.
	for row := 0; ; row++ {
		rowStart := headerSize + row*rowSize
		if rowStart < 0 || rowStart+rowSize > len(body) {
			return
		}
		start := rowStart + bidOffset
		binary.LittleEndian.PutUint32(body[start:start+4], currentGoldConsignNoBid)
	}
}

// buildCurrentAuctionNoticeBody is the exact class-0 op0xA9 payload
// FUN_01d80150 reads: {u8 route, u8 kind, u32 itemID, u16 count}. Two one-byte
// reads, a u32, a u16 - eight bytes, no trailing block.
func buildCurrentAuctionNoticeBody(route, kind byte, itemID uint32) []byte {
	var body packetWriter
	body.writeByte(route)
	body.writeByte(kind)
	body.writeUint32(itemID)
	// FUN_01d80150 reads this u16 into a short[3]; only count[0] is consulted,
	// and only to decide whether to run one extra UI refresh. Zero is the quiet
	// value and is what every notice so far should carry.
	body.writeUint16(0)
	return body.bytes()
}

// sendCurrentAuctionNoticeToOnlineRecipient pushes one class-0 op0xA9 auction
// notice to an online character. Like the mailbox alarm it is a bridge-owned
// projection: the durable state (the letter, the sale row) has already
// committed, and an offline recipient simply misses the transient banner.
func (s *Service) sendCurrentAuctionNoticeToOnlineRecipient(characterID uint16, route, kind byte, itemID int64) error {
	if s == nil || characterID == 0 || itemID <= 0 || itemID > math.MaxUint32 {
		return nil
	}
	session, online := s.onlineGameSession(characterID)
	if !online {
		return nil
	}
	return s.sendGameUpperRawClass(
		session,
		currentAuctionNoticeMsgID,
		buildCurrentAuctionNoticeBody(route, kind, uint32(itemID)),
		0,
	)
}

// notifyCurrentAuctionNoticeID parses a character id and pushes one notice,
// logging rather than failing: a missed banner must never undo a committed
// trade.
func (s *Service) notifyCurrentAuctionNoticeID(characterID string, route, kind byte, itemID int64) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(characterID), 10, 16)
	if err != nil || parsed == 0 {
		return
	}
	if err := s.sendCurrentAuctionNoticeToOnlineRecipient(uint16(parsed), route, kind, itemID); err != nil {
		s.logGameEvent(nil, "game-auction-notice-failed",
			"recipient", characterID, "route", route, "kind", kind, "item_id", itemID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Owner access
// ---------------------------------------------------------------------------

func (s *Service) currentGoldConsignOwner() (*dnfauction.GoldConsignOwner, error) {
	if s == nil {
		return nil, dnfauction.ErrUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, dnfauction.ErrUnavailable
	}
	return dnfauction.NewGoldConsignOwner(repositories)
}

// currentGoldConsignFailureCode maps a ledger refusal onto a code the auction
// panel can actually render. The panel only branches on 7, 8, 140..158, 214 and
// 235; every other code draws nothing at all, so 86's level codes 0x74/0x75 are
// deliberately NOT reused - they would be a silent dead button here.
func currentGoldConsignFailureCode(err error) byte {
	switch {
	case err == nil:
		return currentAuctionErrorUnknown
	case errors.Is(err, dnfauction.ErrGoldConsignNotTier),
		errors.Is(err, dnfauction.ErrItemUnavailable):
		return currentAuctionErrorItemRejected
	case errors.Is(err, dnfauction.ErrGoldConsignSellerLevel),
		errors.Is(err, dnfauction.ErrGoldConsignBuyerLevel),
		errors.Is(err, dnfauction.ErrNotOwner):
		// The panel's 「您没有权限」 is the only rendering code that fits a
		// level gate; the client's own level texts are notices, not this ack.
		return currentAuctionErrorForbidden
	case errors.Is(err, dnfauction.ErrGoldConsignBuyerIsSeller):
		return currentAuctionErrorOwnListing
	case errors.Is(err, dnfauction.ErrGoldUnavailable):
		return currentAuctionErrorGoldShort
	case errors.Is(err, dnfauction.ErrGoldConsignLimit):
		return currentAuctionErrorListingLimit
	case errors.Is(err, dnfauction.ErrGoldConsignPriceRejected),
		errors.Is(err, dnfauction.ErrPriceMismatch):
		return currentAuctionErrorPriceInvalid
	case errors.Is(err, dnfauction.ErrListingMissing),
		errors.Is(err, dnfauction.ErrListingExpired),
		errors.Is(err, dnfauction.ErrGoldConsignNotActive),
		errors.Is(err, dnfauction.ErrGoldConsignNotReserved):
		return currentAuctionErrorNoListing
	default:
		return currentAuctionErrorUnknown
	}
}

// ---------------------------------------------------------------------------
// Request decoders
// ---------------------------------------------------------------------------

type currentGoldConsignRegisterRequest struct {
	GoldItemID   int64
	PriceCera    int64
	DurationHour int64
}

// decodeCurrentGoldConsignRegisterRequest reads the captured 28-byte op183.
func decodeCurrentGoldConsignRegisterRequest(body []byte) (currentGoldConsignRegisterRequest, error) {
	if len(body) != currentAuctionGoldRegisterRequestSize || body[0] != currentAuctionPayTypeConsignment {
		return currentGoldConsignRegisterRequest{}, dnfauction.ErrInvalidRequest
	}
	// body[1] and the u16 at 2 are fixed zero in every capture; the quantity is
	// fixed at 1 and the bid price at -1 because a consignment has no stack
	// count control and no bidding. Anything else is not a shape this client
	// can produce, so it is refused rather than reinterpreted.
	if body[1] != 0 || binary.LittleEndian.Uint16(body[2:4]) != 0 {
		return currentGoldConsignRegisterRequest{}, dnfauction.ErrInvalidRequest
	}
	goldItemID := int64(binary.LittleEndian.Uint32(body[4:8]))
	quantity := int64(int32(binary.LittleEndian.Uint32(body[8:12])))
	bidPrice := int64(int32(binary.LittleEndian.Uint32(body[12:16])))
	priceCera := int64(binary.LittleEndian.Uint32(body[16:20]))
	reserved := binary.LittleEndian.Uint32(body[20:24])
	lifetime := int64(binary.LittleEndian.Uint32(body[24:28]))
	if quantity != 1 || bidPrice != -1 || reserved != 0 || priceCera <= 0 {
		return currentGoldConsignRegisterRequest{}, dnfauction.ErrInvalidRequest
	}
	if lifetime != dnfauction.GoldConsignMinDurationHours && lifetime != dnfauction.GoldConsignMaxDurationHours {
		return currentGoldConsignRegisterRequest{}, dnfauction.ErrInvalidRequest
	}
	if _, ok := dnfauction.GoldConsignTierAmount(goldItemID); !ok {
		return currentGoldConsignRegisterRequest{}, dnfauction.ErrGoldConsignNotTier
	}
	return currentGoldConsignRegisterRequest{
		GoldItemID:   goldItemID,
		PriceCera:    priceCera,
		DurationHour: lifetime,
	}, nil
}

type currentGoldConsignBuyRequest struct {
	ListingID    uint64
	ExpectedCera int64
	// GoldItemID is the tier the panel had selected, present only on the
	// captured 17-byte body. It is redundant with the row the listing id
	// resolves to, but comparing the two catches a panel left open across a
	// tier change, so it is checked rather than ignored.
	GoldItemID    uint32
	HasGoldItemID bool
}

// decodeCurrentGoldConsignBuyRequest reads the op185 body.
//
// CAPTURED 2026-09-21T10:25:15Z as 17 bytes:
//
//	01 6F000000 6103B16A00000000 7DEB2800
//	mode i32 total   i64 listingId   u32 tier
//	    (=111 Cera)                 (=2681725, 1百万)
//
// so {u8 mode, i32 total, i64 listingId, u32 tierItemId}, with the total being
// the whole-listing Cera price, matching the 111 the row was registered at. The
// item auction's own op185 is 30 bytes and mode 0, so the two cannot collide.
//
// A 13-byte body ({u8 mode, i32 total, i64 listingId}) stays accepted: it is the
// shape this decoder was written against before the capture, and refusing it
// would break nothing but would also gain nothing. op334 is deliberately NOT
// routed here - its item body has no mode byte at all, so a gold variant of it
// remains unknown.
func decodeCurrentGoldConsignBuyRequest(body []byte) (currentGoldConsignBuyRequest, error) {
	if len(body) != currentAuctionGoldBuyRequestSize &&
		len(body) != currentAuctionGoldBuyRequestSizeWithTrailer {
		return currentGoldConsignBuyRequest{}, dnfauction.ErrInvalidRequest
	}
	if body[0] != currentAuctionPayTypeConsignment {
		return currentGoldConsignBuyRequest{}, dnfauction.ErrInvalidRequest
	}
	expected := int64(int32(binary.LittleEndian.Uint32(body[1:5])))
	listingID := binary.LittleEndian.Uint64(body[5:13])
	if expected <= 0 || listingID == 0 {
		return currentGoldConsignBuyRequest{}, dnfauction.ErrInvalidRequest
	}
	request := currentGoldConsignBuyRequest{ListingID: listingID, ExpectedCera: expected}
	if len(body) == currentAuctionGoldBuyRequestSizeWithTrailer {
		tier := binary.LittleEndian.Uint32(
			body[currentAuctionGoldBuyRequestSize:currentAuctionGoldBuyRequestSizeWithTrailer])
		if tier == 0 {
			return currentGoldConsignBuyRequest{}, dnfauction.ErrInvalidRequest
		}
		request.GoldItemID = tier
		request.HasGoldItemID = true
	}
	return request, nil
}

// decodeCurrentGoldConsignCancelRequest reads the inferred op184 body.
func decodeCurrentGoldConsignCancelRequest(body []byte) (uint64, error) {
	if len(body) != currentAuctionGoldCancelRequestSize ||
		body[0] != currentAuctionPayTypeConsignment {
		return 0, dnfauction.ErrInvalidRequest
	}
	listingID := binary.LittleEndian.Uint64(body[1:9])
	if listingID == 0 {
		return 0, dnfauction.ErrInvalidRequest
	}
	return listingID, nil
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

// currentGoldConsignAsListing projects a consignment into the item auction's
// Listing shape so the proven row painters can render it unchanged. 86 does the
// same thing: GoldConsignmentSearchAckBuilder emits the shared auction record
// size, and the gold panel's list is the same 0xA8/0x97 record with the
// certificate as its item.
//
// It is marked INFERRED at the protocol level: an empty gold list is the only
// response we have ever had accepted on the wire, so the row layout is reused
// on 86's precedent rather than on a capture of our own client.
func currentGoldConsignAsListing(g dnfauction.GoldConsignListing) dnfauction.Listing {
	return dnfauction.Listing{
		ID:         g.ID,
		SellerID:   g.SellerID,
		SellerName: g.SellerName,
		Item:       dnfrepo.ItemStack{ItemID: g.GoldItemID, Count: 1},
		// A consignment has no bidding: the captured op183 always carries -1,
		// so the row is buyout-only and UnitPrice is the Cera price.
		UnitPrice:   g.PriceCera,
		StartPrice:  -1,
		BuyoutPrice: g.PriceCera,
		FeePercent:  g.FeePercent,
		CreatedAt:   g.CreatedAt,
		ExpiresAt:   g.ExpiresAt,
	}
}

func currentGoldConsignAsListings(listings []dnfauction.GoldConsignListing) []dnfauction.Listing {
	out := make([]dnfauction.Listing, 0, len(listings))
	for _, listing := range listings {
		out = append(out, currentGoldConsignAsListing(listing))
	}
	return out
}

// currentGoldConsignTierIDs widens the wire's u32 tier ids for the ledger.
func currentGoldConsignTierIDs(itemIDs []uint32) []int64 {
	out := make([]int64, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		out = append(out, int64(itemID))
	}
	return out
}

// buildCurrentGoldConsignSearchBody renders the 金币搜索 page. It reuses the
// item auction's paged builder and only flips the mode byte, so the envelope
// stays byte-identical to the empty page the client already accepted
// (01 01 00000000 0000).
func buildCurrentGoldConsignSearchBody(listings []dnfauction.GoldConsignListing, page uint32, pageSize int) []byte {
	body := buildCurrentAuctionSearchBodyPage(
		currentGoldConsignAsListings(listings),
		// Gold certificates are deliberately NOT item-auction eligible, so the
		// item predicate must not filter them out here.
		func(uint32) bool { return true },
		page,
		pageSize,
	)
	if len(body) > 0 {
		body[0] = currentAuctionPayTypeConsignment
	}
	markCurrentGoldConsignNoBid(
		body,
		currentGoldConsignSearchHeaderSize,
		currentGoldConsignSearchRowSize,
		currentGoldConsignSearchBidOffset,
	)
	return body
}

// buildCurrentGoldConsignOwnedBody renders 我寄售的金币. Same reuse: the item
// tab's own builder with its seller filter, plus the mode byte and the same
// 竞拍价 blanking the search page needs.
func buildCurrentGoldConsignOwnedBody(listings []dnfauction.GoldConsignListing, sellerID string) []byte {
	body := buildCurrentAuctionOwnedBody(
		currentGoldConsignAsListings(listings),
		sellerID,
		func(uint32) bool { return true },
	)
	if len(body) > 0 {
		body[0] = currentAuctionPayTypeConsignment
	}
	markCurrentGoldConsignNoBid(
		body,
		currentGoldConsignOwnedHeaderSize,
		currentGoldConsignOwnedRowSize,
		currentGoldConsignOwnedBidOffset,
	)
	return body
}

// ---------------------------------------------------------------------------
// Mail
// ---------------------------------------------------------------------------

// currentGoldConsignAttachment renders one consignment letter attachment. Gold
// itself rides the letter's Gold field, so the only attachment kind here is the
// seller's 代币券 certificate - which the mailbox claim path credits to the
// account wallet rather than dropping into the material bag.
func currentGoldConsignVoucherAttachment(proceedsCera int64) dnfrepo.MailAttachment {
	return dnfrepo.MailAttachment{
		ItemID: dnfcerashop.TokenCeraVoucherItemID,
		Count:  proceedsCera,
	}
}

// sendCurrentGoldConsignLetter enqueues one system letter. CompletionKey makes
// the call idempotent, so a retry after a partial failure cannot double-pay.
//
// The live mailbox projection is fired here rather than at each call site so
// every gold letter - purchase, sale proceeds and refund - carries it. The item
// auction fires the class0/op0x63 alarm next to its own sends; without it a
// consignment letter stayed invisible until the player happened to open the
// mailbox, which is what the operator reported first.
//
// The alarm alone turned out to be only half of it: FUN_01d7ff70 draws the
// 「收到新邮件」 banner and bumps a counter, and never repaints the mailbox list
// (see sendMailboxListRefreshToOnlineRecipient for the disassembly). So the gold
// path additionally pushes the class0/op0x61 page, which is the only thing that
// rebuilds the list - the same alarm-then-page pair the GM mailbox-clear refresh
// already ships. The item auction's own delivery path is left alone on purpose:
// it must stay byte-identical.
func (s *Service) sendCurrentGoldConsignLetter(ctx context.Context, request dnfmail.SystemSendRequest) error {
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dnfauction.ErrUnavailable
	}
	if _, err := dnfmail.NewOwner(repositories).SendSystem(ctx, request); err != nil {
		return err
	}
	if characterID, parseErr := strconv.ParseUint(strings.TrimSpace(request.RecipientCharacterID), 10, 16); parseErr == nil && characterID != 0 {
		// The banner first, then the page, so the 「收到新邮件」 notice is not
		// the thing the list rebuild overwrites.
		if alarmErr := s.sendMailboxAlarmToOnlineRecipient(uint16(characterID)); alarmErr != nil {
			s.logGameEvent(nil, "game-goldconsign-mail-alarm-failed",
				"recipient", request.RecipientCharacterID, "source", request.Source, "error", alarmErr)
		}
		if refreshErr := s.sendMailboxListRefreshToOnlineRecipient(uint16(characterID)); refreshErr != nil {
			s.logGameEvent(nil, "game-goldconsign-mail-refresh-failed",
				"recipient", request.RecipientCharacterID, "source", request.Source, "error", refreshErr)
		}
	}
	return nil
}

// deliverCurrentGoldConsignSalesLocked sends the two letters a settled sale
// owes and stamps the row. Both letters are keyed by the listing id, so
// re-running this cannot pay twice, and a sale whose settle step succeeded but
// whose letters did not go out is retried on the next consignment command.
//
// 记录侧同样靠"这一位"保证只写一条：成交收款记录的条件是 MarkDelivered 报告
// 本次真的盖了章，重跑（无论是因为上一轮送信失败，还是因为同一条命令里被调了
// 两次）都只会命中一次。
func (s *Service) deliverCurrentGoldConsignSalesLocked(ctx context.Context, session *gameSession) {
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		return
	}
	pending, err := owner.UndeliveredSales(ctx)
	if err != nil {
		return
	}
	for _, sale := range pending {
		if sale.BuyerID == "" || sale.SellerID == "" {
			continue
		}
		itemName := s.currentAuctionItemDisplayName(sale.GoldItemID)
		// Buyer: the gold itself.
		if err := s.sendCurrentGoldConsignLetter(ctx, dnfmail.SystemSendRequest{
			RecipientCharacterID: sale.BuyerID,
			Title:                "金币寄售购买",
			Body:                 "您购买的金币已通过邮件送达。",
			Gold:                 sale.GoldAmount,
			Source:               "goldconsign_purchase",
			CompletionKey:        "goldconsign:buyer:" + strconv.FormatUint(sale.ID, 10),
		}); err != nil {
			s.logGameEvent(session, "game-goldconsign-buyer-mail-deferred",
				"listing_id", sale.ID, "buyer_id", sale.BuyerID, "error", err)
			continue
		}
		// Seller: 代币券, the bound cash the consignment system pays in. The
		// letter prints the same itemised statement the seller's client shows:
		// what the gold sold for, the commission taken out of it, and the
		// 代币券 that actually arrives.
		sellerTitle, sellerBody := currentGoldConsignSaleStatement(sale, itemName)
		if err := s.sendCurrentGoldConsignLetter(ctx, dnfmail.SystemSendRequest{
			RecipientCharacterID: sale.SellerID,
			Title:                sellerTitle,
			Body:                 sellerBody,
			Attachments:          []dnfrepo.MailAttachment{currentGoldConsignVoucherAttachment(sale.ProceedsCera)},
			Source:               "goldconsign_sale_proceeds",
			CompletionKey:        "goldconsign:seller:" + strconv.FormatUint(sale.ID, 10),
		}); err != nil {
			s.logGameEvent(session, "game-goldconsign-seller-mail-deferred",
				"listing_id", sale.ID, "seller_id", sale.SellerID, "error", err)
			continue
		}
		freshlyDelivered, deliveredErr := owner.MarkDelivered(ctx, sale.BuyerID, sale.ID)
		if deliveredErr != nil {
			s.logGameEvent(session, "game-goldconsign-delivered-stamp-failed",
				"listing_id", sale.ID, "error", deliveredErr)
		}
		// 成交收款记录只写在**这一次真的盖了章**的那一次
		// （freshlyDelivered，见 GoldConsignOwner.MarkDelivered）。
		//
		// 为什么不能用"这一行是从 UndeliveredSales 里取出来的，所以它一定没送过"
		// 来推断：这一族随每条寄售命令重跑，而重跑恰恰就是"上一次送到一半失败"
		// 的正常恢复路径；只要判据稍微偏一点，重试就会为同一单刷出第二条收款
		// 记录 —— 而记录侧（activity_record.record / 管理工具）都不做去重。
		// 唯一的权威是域层自己报告的这次跃迁。
		if freshlyDelivered {
			fields := goldConsignListingFields(sale)
			fields["buyer_id"] = strings.TrimSpace(sale.BuyerID)
			fields["delivered"] = true
			s.recordGoldConsign(session, "sale_delivered", fields)
		}
		// Both parties get the same class-0 op0xA9 banner the item auction
		// shows, on the gold route so it lands in the 金币寄售 panel: the buyer
		// sees 60088 (竞拍的[%s]已拍下) and the seller 60087 (上架的[%s]已被拍下).
		s.notifyCurrentAuctionNoticeID(sale.BuyerID, currentAuctionNoticeRouteGold, currentAuctionNoticeKindBuyerWon, sale.GoldItemID)
		s.notifyCurrentAuctionNoticeID(sale.SellerID, currentAuctionNoticeRouteGold, currentAuctionNoticeKindSellerSold, sale.GoldItemID)
	}
}

// currentGoldConsignSaleStatement renders a settled consignment's seller letter
// in the itemised shape the client shows: what the gold sold for, the commission
// taken out of it, and the 代币券 that actually arrives.
//
//   - 已成功拍卖[1百万] -
//     成交价 : + 188 代币券
//     手续费 : - 56 代币券
//     拍卖[1百万]后， 您获得了132代币券。
//
// The first line is both the subject and the body's first line: the client's
// mail detail pane draws the body only, so a subject on its own never appears
// there, while the mailbox list draws the subject.
//
// The commission is read off the row rather than recomputed from FeePercent, so
// a row written under an older frozen rate still prints the money that actually
// moved. 手续费 is omitted when nothing was charged.
func currentGoldConsignSaleStatement(sale dnfauction.GoldConsignListing, itemName string) (string, string) {
	fee := sale.PriceCera - sale.ProceedsCera
	if fee < 0 {
		fee = 0
	}
	lines := []string{"- 已成功拍卖[" + itemName + "] -"}
	lines = append(lines, fmt.Sprintf("成交价 : + %d 代币券", sale.PriceCera))
	if fee > 0 {
		lines = append(lines, fmt.Sprintf("手续费 : - %d 代币券", fee))
	}
	lines = append(lines, fmt.Sprintf("拍卖[%s]后， 您获得了%d代币券。", itemName, sale.ProceedsCera))
	return lines[0], strings.Join(lines, "\n")
}

// goldConsignListingFields 一条寄售单的公共记录字段（单据、金币档位、价格、
// 手续费、卖家）。
//
// 上架 / 买家买入 / 成交收款 / 流拍退回四条记录都要这几项：运营拿 listing_id
// 就能把一单从头串到尾。为什么不各自手写：寄售单在成交之后就会从订单表消失
// （Settle 会把行从 auction:goldconsign: 搬到 auction:goldconsign:sold:，
// 过期与下架直接删行），记录是这批字段唯一还能翻出来的地方，四处写就容易
// 四处漏字段、口径还不一致。
func goldConsignListingFields(listing dnfauction.GoldConsignListing) map[string]any {
	fields := map[string]any{
		"listing_id":    listing.ID,
		"gold_item_id":  listing.GoldItemID,
		"gold_amount":   listing.GoldAmount,
		"price_cera":    listing.PriceCera,
		"fee_percent":   listing.FeePercent,
		"fee_cera":      goldConsignFeeCera(listing),
		"proceeds_cera": listing.ProceedsCera,
	}
	if sellerID := strings.TrimSpace(listing.SellerID); sellerID != "" {
		fields["seller_id"] = sellerID
	}
	if sellerName := strings.TrimSpace(listing.SellerName); sellerName != "" {
		fields["seller_name"] = sellerName
	}
	if !listing.ExpiresAt.IsZero() {
		fields["expires_at"] = listing.ExpiresAt.UTC().Format(time.RFC3339)
		// 时长由行上的两个时间戳反推，不另存一份：上架时写入的时长在结算阶段
		// 已经拿不到了，而"这单挂了多久"恰是过期/流拍记录里最该有的一项。
		if !listing.CreatedAt.IsZero() {
			if hours := int64(listing.ExpiresAt.Sub(listing.CreatedAt).Hours()); hours > 0 {
				fields["duration_hours"] = hours
			}
		}
	}
	return fields
}

// goldConsignFeeCera 这笔成交抽掉的代币券手续费。
//
// 用售价减实得，而不是拿 FeePercent 重算：行上冻结的费率与实付金额才是权威值
// （历史单据可能按旧费率成交），重算会打出与真实流水对不上的数字。同一个口径
// 也用在卖家邮件正文里（currentGoldConsignSaleStatement），两处必须一致。
func goldConsignFeeCera(listing dnfauction.GoldConsignListing) int64 {
	fee := listing.PriceCera - listing.ProceedsCera
	if fee < 0 {
		return 0
	}
	return fee
}

// refundGoldByLetter returns consigned gold to its seller as a system letter,
// which is the channel both a cancel and an expiry use.
func (s *Service) refundGoldByLetter(ctx context.Context, session *gameSession, listing dnfauction.GoldConsignListing, refund int64, title, body, source string) error {
	if refund <= 0 {
		return nil
	}
	itemName := s.currentAuctionItemDisplayName(listing.GoldItemID)
	return s.sendCurrentGoldConsignLetter(ctx, dnfmail.SystemSendRequest{
		RecipientCharacterID: listing.SellerID,
		Title:                title,
		Body:                 strings.ReplaceAll(body, "%s", itemName),
		Gold:                 refund,
		Source:               source,
		CompletionKey:        "goldconsign:refund:" + source + ":" + strconv.FormatUint(listing.ID, 10),
	})
}

// ---------------------------------------------------------------------------
// op182 average
// ---------------------------------------------------------------------------

// sendCurrentAuctionGoldAveragePriceLocked answers the 金币寄售 price inquiry.
// It reports the rolling recent-sale average for the tier; with no sales yet
// the average is 0, which is both true and the signal the fee resolver uses to
// fall back to the ordinary band.
func (s *Service) sendCurrentAuctionGoldAveragePriceLocked(
	session *gameSession,
	opcode uint16,
	body []byte,
) error {
	if len(body) != currentAuctionGoldAveragePriceRequestSize {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	goldItemID := int64(binary.LittleEndian.Uint32(body[1:5]))
	if goldItemID == 0 {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	var average, samples int64
	if owner, err := s.currentGoldConsignOwner(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		if value, count, queryErr := owner.AverageCeraPrice(ctx, goldItemID); queryErr == nil {
			average, samples = value, count
		} else {
			s.logGameEvent(session, "game-goldconsign-average-failed",
				"item_id", goldItemID, "error", queryErr)
		}
	}
	var response packetWriter
	response.writeByte(currentAuctionPayTypeConsignment)
	response.writeUint32(currentAuctionUint32(average))
	response.writeUint32(currentAuctionUint32(samples))
	for index := 0; index < 6; index++ {
		response.writeUint32(currentAuctionUint32(average))
	}
	for index := 0; index < 6; index++ {
		response.writeUint32(currentAuctionUint32(samples))
	}
	return s.sendGameUpperSuccess(session, opcode, response.bytes())
}

// ---------------------------------------------------------------------------
// op183 register
// ---------------------------------------------------------------------------

// goldConsignRejectReason 把一个域层拒绝翻成记录里能读的原因。
//
// 与 currentGoldConsignFailureCode（给客户端看的错误码）分开：错误码要落在
// 客户端已有的文案表里，而记录是给运营看的，越具体越好。未列出的错误统一
// 落到 "rejected"，配合 logGameEvent 里的原始 error 一起看。
func goldConsignRejectReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, dnfauction.ErrGoldUnavailable):
		return "gold_unavailable"
	case errors.Is(err, dnfauction.ErrGoldConsignNotTier):
		return "not_a_gold_tier"
	case errors.Is(err, dnfauction.ErrGoldConsignPriceRejected):
		return "price_rejected"
	case errors.Is(err, dnfauction.ErrGoldConsignSellerLevel):
		return "seller_level_too_low"
	case errors.Is(err, dnfauction.ErrGoldConsignBuyerLevel):
		return "buyer_level_too_low"
	case errors.Is(err, dnfauction.ErrGoldConsignBuyerIsSeller):
		return "buyer_is_seller"
	case errors.Is(err, dnfauction.ErrGoldConsignNotReserved):
		return "listing_not_reserved"
	case errors.Is(err, dnfauction.ErrGoldConsignNotActive):
		return "listing_not_active"
	case errors.Is(err, dnfauction.ErrGoldConsignLimit), errors.Is(err, dnfauction.ErrListingLimit):
		return "listing_limit"
	case errors.Is(err, dnfauction.ErrNotOwner):
		return "not_owner"
	case errors.Is(err, dnfauction.ErrListingMissing):
		return "listing_missing"
	case errors.Is(err, dnfauction.ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, dnfauction.ErrUnavailable):
		return "ledger_unavailable"
	default:
		return "rejected"
	}
}

func (s *Service) registerCurrentGoldConsignLocked(session *gameSession, opcode uint16, body []byte) error {
	request, err := decodeCurrentGoldConsignRegisterRequest(body)
	if err != nil {
		// 被拒绝的尝试也要留一行：运营最常问的不是"谁挂上了"，而是
		// "为什么他挂不上" —— 只有成功记录时这个问题无法回答。
		fields := activityRejectionFields("malformed_body")
		fields["body_len"] = len(body)
		s.recordGoldConsign(session, "list", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	sellerID := selectedCharacterIDString(session)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if sellerID == "" || accountID == "" {
		fields := activityRejectionFields("identity_missing")
		fields["gold_item_id"] = request.GoldItemID
		fields["price_cera"] = request.PriceCera
		s.recordGoldConsign(session, "list", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionErrorForbidden)
	}
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		fields := activityRejectionFields("ledger_unavailable")
		fields["gold_item_id"] = request.GoldItemID
		fields["price_cera"] = request.PriceCera
		s.recordGoldConsign(session, "list", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	// Expiries and undelivered sale letters ride along with any command, the
	// same way the item auction settles won listings on every request.
	s.returnExpiredGoldConsignLocked(ctx, session, owner, sellerID, accountID)
	s.deliverCurrentGoldConsignSalesLocked(ctx, session)

	// The seller's display name comes from the character row the ledger loads
	// inside the transaction, so the wire name can never disagree with the
	// wallet that was actually debited.
	result, err := owner.Register(ctx, dnfauction.GoldConsignRegisterCommand{
		ListingID:    s.nextAuctionListingID.Add(1),
		SellerID:     sellerID,
		AccountID:    accountID,
		GoldItemID:   request.GoldItemID,
		PriceCera:    request.PriceCera,
		DurationHour: request.DurationHour,
		HasCoupon:    s.currentGoldConsignHasCoupon(session),
	})
	if err != nil {
		s.logGameEvent(session, "game-goldconsign-register-rejected",
			"item_id", request.GoldItemID, "price_cera", request.PriceCera, "error", err)
		fields := activityRejectionFields(goldConsignRejectReason(err))
		fields["gold_item_id"] = request.GoldItemID
		fields["price_cera"] = request.PriceCera
		fields["duration_hours"] = request.DurationHour
		activityIdentityOverride(fields, accountID, sellerID)
		s.recordGoldConsign(session, "list", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	if err := s.sendGameUpperSuccess(session, opcode,
		buildCurrentGoldConsignRegisterSuccessBody(result.Listing.ID, session != nil && session.robotSessionBound)); err != nil {
		return err
	}
	// The seller's gold moved, so the wallet row has to be republished.
	if err := s.sendCurrentGoldConsignGoldRefresh(session, result.GoldAfter); err != nil {
		s.logGameEvent(session, "game-goldconsign-register-refresh-failed", "error", err)
	}
	s.logGameEvent(session, "game-goldconsign-register-committed",
		"listing_id", result.Listing.ID, "item_id", result.Listing.GoldItemID,
		"gold", result.Listing.GoldAmount, "price_cera", result.Listing.PriceCera,
		"fee_percent", result.Listing.FeePercent, "proceeds_cera", result.Listing.ProceedsCera,
		"expires_at", result.Listing.ExpiresAt.Unix())
	// 上架记录写在 ACK 之后、与 logGameEvent 并列：Register 已经提交（金币进了
	// 托管），这一单在上架这一点上就是既成事实，客户端 ACK 发不出去也不改变它。
	fields := goldConsignListingFields(result.Listing)
	fields["duration_hours"] = request.DurationHour
	fields["gold_after"] = result.GoldAfter
	activityIdentityOverride(fields, accountID, sellerID)
	s.recordGoldConsign(session, "list", fields)
	return nil
}

// currentGoldConsignHasCoupon reports whether the seller holds the auction
// coupon the 40..150% band can discount. It reuses the item auction's own
// entitlement check so both panels agree on what "has the coupon" means.
func (s *Service) currentGoldConsignHasCoupon(session *gameSession) bool {
	if s == nil || session == nil {
		return false
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	_, _, feePercent := s.currentAuctionEntitlementLocked(ctx, session, repositories)
	return feePercent == dnfauction.DiscountedSaleFeePercent
}

// ---------------------------------------------------------------------------
// op184 cancel
// ---------------------------------------------------------------------------

func (s *Service) cancelCurrentGoldConsignLocked(session *gameSession, opcode uint16, body []byte) error {
	listingID, err := decodeCurrentGoldConsignCancelRequest(body)
	if err != nil {
		fields := activityRejectionFields("malformed_body")
		fields["body_len"] = len(body)
		s.recordGoldConsign(session, "cancel", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	sellerID := selectedCharacterIDString(session)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		fields := activityRejectionFields("ledger_unavailable")
		fields["listing_id"] = listingID
		s.recordGoldConsign(session, "cancel", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := owner.Cancel(ctx, sellerID, accountID, listingID)
	if err != nil {
		s.logGameEvent(session, "game-goldconsign-cancel-rejected",
			"listing_id", listingID, "error", err)
		fields := activityRejectionFields(goldConsignRejectReason(err))
		fields["listing_id"] = listingID
		activityIdentityOverride(fields, accountID, sellerID)
		s.recordGoldConsign(session, "cancel", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	if err := s.sendGameUpperSuccess(session, opcode, []byte{currentAuctionPayTypeConsignment}); err != nil {
		return err
	}
	if err := s.refundGoldByLetter(ctx, session, result.Listing, result.RefundGold,
		"金币寄售下架", "您下架的%s已通过邮件返还。", "goldconsign_cancel_return"); err != nil {
		s.logGameEvent(session, "game-goldconsign-cancel-mail-deferred",
			"listing_id", listingID, "error", err)
	}
	// The seller gets the same banner the item auction's cancel sends, on the
	// gold route: kind 5 renders dstr 60091 「[%s]的竞拍已被取消，请确认邮件」.
	// Without it a consignment cancel was completely silent - the client's own
	// op184 handler only pops dstr 60084 (已取消拍卖物品的上架), which is the item
	// panel's wording, and FUN_01d80150 is the only sender for 60091.
	s.notifyCurrentAuctionNoticeID(sellerID, currentAuctionNoticeRouteGold,
		currentAuctionNoticeKindCancelBid, result.Listing.GoldItemID)
	// The seller's gold is credited when they claim the letter, not here, so
	// the cancel itself owes the wallet no refresh.
	s.logGameEvent(session, "game-goldconsign-cancel-committed",
		"listing_id", listingID, "refund_gold", result.RefundGold)
	// 下架记录同时带上退还金币：寄售下架是把托管里的金币原样退回，
	// 与拍卖行的"取消上架没收押金"正好相反，这一栏是两类下架最容易看混的地方。
	fields := goldConsignListingFields(result.Listing)
	fields["refund_gold"] = result.RefundGold
	activityIdentityOverride(fields, accountID, sellerID)
	s.recordGoldConsign(session, "cancel", fields)
	return nil
}

// ---------------------------------------------------------------------------
// op185 / op334 buy
// ---------------------------------------------------------------------------

// buyCurrentGoldConsignLocked runs the buy saga described at the top of the
// ledger module: reserve, pay, settle, deliver. Every step after the reserve is
// either idempotent or retried by the next command, so a crash mid-way can
// neither double-charge nor lose the consignment.
func (s *Service) buyCurrentGoldConsignLocked(session *gameSession, opcode uint16, body []byte) error {
	request, err := decodeCurrentGoldConsignBuyRequest(body)
	if err != nil {
		fields := activityRejectionFields("malformed_body")
		fields["body_len"] = len(body)
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	buyerID := selectedCharacterIDString(session)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if buyerID == "" || accountID == "" {
		fields := activityRejectionFields("identity_missing")
		fields["listing_id"] = request.ListingID
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionErrorForbidden)
	}
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		fields := activityRejectionFields("ledger_unavailable")
		fields["listing_id"] = request.ListingID
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.deliverCurrentGoldConsignSalesLocked(ctx, session)

	reservation, err := owner.Reserve(ctx, buyerID, accountID, request.ListingID)
	if err != nil {
		s.logGameEvent(session, "game-goldconsign-buy-reserve-rejected",
			"listing_id", request.ListingID, "error", err)
		fields := activityRejectionFields(goldConsignRejectReason(err))
		fields["listing_id"] = request.ListingID
		activityIdentityOverride(fields, accountID, buyerID)
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	listing := reservation.Listing
	if listing.PriceCera != request.ExpectedCera {
		_ = owner.Release(ctx, buyerID, request.ListingID)
		fields := activityRejectionFields("price_mismatch")
		fields["listing_id"] = listing.ID
		fields["price_cera"] = listing.PriceCera
		fields["panel_price_cera"] = request.ExpectedCera
		activityIdentityOverride(fields, accountID, buyerID)
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionErrorPriceMismatch)
	}
	// The captured 17-byte body also names the tier the panel had selected. It
	// must agree with the row the listing id resolved to, or the buyer is
	// looking at a stale panel and would receive a different denomination than
	// the one they confirmed. The refusal is the panel-refresh code, the same
	// one a changed price uses.
	if request.HasGoldItemID &&
		(listing.GoldItemID <= 0 || uint32(listing.GoldItemID) != request.GoldItemID) {
		_ = owner.Release(ctx, buyerID, request.ListingID)
		s.logGameEvent(session, "game-goldconsign-buy-tier-mismatch",
			"listing_id", listing.ID, "buyer_id", buyerID,
			"panel_tier", request.GoldItemID, "row_tier", listing.GoldItemID)
		fields := activityRejectionFields("tier_mismatch")
		fields["listing_id"] = listing.ID
		fields["gold_item_id"] = listing.GoldItemID
		fields["panel_gold_item_id"] = request.GoldItemID
		activityIdentityOverride(fields, accountID, buyerID)
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentAuctionErrorPriceMismatch)
	}
	before, after, charged, err := s.payCurrentGoldConsignCera(ctx, accountID, buyerID, listing)
	if err != nil {
		_ = owner.Release(ctx, buyerID, request.ListingID)
		s.logGameEvent(session, "game-goldconsign-buy-payment-rejected",
			"listing_id", listing.ID, "price_cera", listing.PriceCera, "error", err)
		// 点券不足是这类拒绝里最常见的一种（运营最常被问"他为什么买不了"），
		// 所以它必须留下一条 result=rejected 的记录，而不是只进 packet_log。
		fields := activityRejectionFields(goldConsignRejectReason(err))
		fields["listing_id"] = listing.ID
		fields["gold_item_id"] = listing.GoldItemID
		fields["gold_amount"] = listing.GoldAmount
		fields["price_cera"] = listing.PriceCera
		fields["seller_id"] = strings.TrimSpace(listing.SellerID)
		activityIdentityOverride(fields, accountID, buyerID)
		s.recordGoldConsign(session, "buy", fields)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	// 点券消费记录必须记在 settle **之前**：钱已经扣掉并落库了，后面无论
	// settle 成功、还是失败后由 reconciler 补完，这笔消费都已经真实发生。
	// 记在 settle 成功处会漏掉"钱花了但 settle 延后"的那批（对账时最难查）。
	// charged=false 表示这是重放同一个 op（钱包没再扣），绝不能重复记一条。
	if charged {
		s.recordGoldConsignCeraSpend(session, accountID, buyerID, listing, before, after)
	}
	if _, err := owner.Settle(ctx, buyerID, listing.ID); err != nil {
		// The buyer has paid and their marker is durable; the reconciler
		// finishes this on the next command rather than the money vanishing.
		s.logGameEvent(session, "game-goldconsign-buy-settle-deferred",
			"listing_id", listing.ID, "buyer_id", buyerID, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGoldConsignFailureCode(err))
	}
	if err := s.sendGameUpperSuccess(session, opcode, []byte{currentAuctionPayTypeConsignment}); err != nil {
		return err
	}
	s.deliverCurrentGoldConsignSalesLocked(ctx, session)
	// 钱包刷新顺带把刷新出去的余额回传，买入记录里的"买家代币券余额"就不必
	// 再查一次库：埋点位于玩法主路径上，为凑一个字段多一次仓储往返不划算
	// （刷新失败时这一栏留空，而不是去补一次查询）。
	wallet, walletErr := s.sendCurrentGoldConsignWalletRefresh(session)
	if walletErr != nil {
		s.logGameEvent(session, "game-goldconsign-buy-refresh-failed", "error", walletErr)
	}
	s.logGameEvent(session, "game-goldconsign-buy-committed",
		"listing_id", listing.ID, "buyer_id", buyerID, "seller_id", listing.SellerID,
		"gold", listing.GoldAmount, "price_cera", listing.PriceCera,
		"fee_percent", listing.FeePercent, "proceeds_cera", listing.ProceedsCera)
	// 买入记录同样只在 charged（本次真的扣了钱）时写：重放同一个 op185 时
	// 钱包不会再动，凭 err==nil 记账会写出第二条"买家买入"。
	if charged {
		fields := goldConsignListingFields(listing)
		fields["buyer_id"] = buyerID
		if buyerName := s.activityCharacterName(session); buyerName != "" {
			fields["buyer_name"] = buyerName
		}
		fields["cera_spent"] = listing.PriceCera
		fields["cera_before"] = before
		fields["cera_after"] = after
		if walletErr == nil {
			fields["token_cera_after"] = wallet.TokenCera
		}
		activityIdentityOverride(fields, accountID, buyerID)
		s.recordGoldConsign(session, "buy", fields)
	}
	return nil
}

// payCurrentGoldConsignCera debits the buyer's Cera wallet and records the pay
// marker in the same transaction, so the reconciler can tell a paid-but-
// unsettled reservation from an abandoned one.
//
// Cera is account-scoped metadata, which no character-trade boundary reaches;
// the mailbox boundary is the narrowest one that yields the Account repository
// together with the buyer's own character.
//
// 返回值专门为"点券消费记录"服务（2026-09）：
//   - before/after 本次实际扣费前后的点券余额，供记录里的"消费前/后点券"；
//   - charged 本次调用**是否真的扣了钱**。这一位是必需的：下面那句
//     "A retry after a deferred settle must not charge again" 会让重放同一个
//     op185 时直接 return nil —— 与"本次刚扣完"返回值完全一样。
//     调用方只凭 err==nil 无法区分两者，照着记就会出现两条重复消费；
//     而记录侧（activity_record.record / 管理工具）都不做去重。
//   - 任何失败路径都返回 charged=false，避免"扣费没落库、记录却写了一条"。
func (s *Service) payCurrentGoldConsignCera(ctx context.Context, accountID, buyerID string, listing dnfauction.GoldConsignListing) (before, after int64, charged bool, err error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		return 0, 0, false, dnfauction.ErrUnavailable
	}
	err = repositories.MailboxAssets.WithinMailboxAssets(
		ctx, buyerID, buyerID,
		func(
			accounts dnfrepo.AccountRepository,
			_ dnfrepo.AccountInventoryRepository,
			_ dnfrepo.CharacterRepository,
			_ dnfrepo.InventoryRepository,
			_ dnfrepo.MailboxRepository,
		) error {
			account, found, err := accounts.Load(ctx, accountID)
			if err != nil {
				return err
			}
			if !found {
				return dnfauction.ErrInvalidRequest
			}
			// A retry after a deferred settle must not charge again.
			if _, paid := dnfauction.GoldConsignReadPayMarker(account, listing.ID); paid {
				return nil
			}
			wallet := dnfcerashop.WalletBalance(account)
			if wallet.Cera < listing.PriceCera {
				return dnfauction.ErrGoldUnavailable
			}
			updated := dnfrepo.CloneAccount(account)
			if updated.Metadata == nil {
				updated.Metadata = map[string]string{}
			}
			dnfcerashop.SetBalance(&updated, wallet.Cera-listing.PriceCera)
			updated.Metadata[dnfauction.GoldConsignPayMarkerKey(listing.ID)] = dnfauction.BuildPayMarker(buyerID, listing)
			// 先记下余额变化，再落库；Save 失败会在下面被清掉，
			// 保证"没落库就一定 charged=false"。
			before, after, charged = wallet.Cera, wallet.Cera-listing.PriceCera, true
			return accounts.Save(ctx, updated)
		})
	if err != nil {
		return 0, 0, false, err
	}
	return before, after, charged, nil
}

// ---------------------------------------------------------------------------
// op186 / op187 search
// ---------------------------------------------------------------------------

func (s *Service) searchCurrentAuctionGoldItemsLocked(session *gameSession, opcode uint16, body []byte) error {
	page, itemIDs, ok := decodeCurrentAuctionSearchBody(body, currentAuctionPayTypeConsignment)
	if !ok {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	// A stale reserved row from an aborted purchase goes back on sale here,
	// which is the cheapest place to notice one.
	s.reconcileGoldConsignReservationsLocked(ctx, session, owner)
	listings, err := owner.Search(ctx, currentGoldConsignTierIDs(itemIDs))
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	return s.sendGameUpperSuccess(session, opcode,
		buildCurrentGoldConsignSearchBody(listings, page, GoldConsignSearchPageSize))
}

// GoldConsignSearchPageSize is the gold page size the panel pages through.
const GoldConsignSearchPageSize = 100

func (s *Service) searchCurrentGoldConsignCategoryLocked(session *gameSession, opcode uint16, body []byte) error {
	// INFERRED: the 金币分类 request has not been captured. The item panel
	// sends a fixed 20-byte body, so the gold panel is assumed to send the same
	// 20 bytes with mode 1; anything else is refused rather than guessed at.
	if len(body) != currentAuctionGoldCategoryRequestSize || body[0] != currentAuctionPayTypeConsignment {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	page := binary.LittleEndian.Uint32(body[1:5])
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.reconcileGoldConsignReservationsLocked(ctx, session, owner)
	listings, err := owner.Search(ctx, nil)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	return s.sendGameUpperSuccess(session, opcode,
		buildCurrentGoldConsignSearchBody(listings, page, GoldConsignSearchPageSize))
}

// ---------------------------------------------------------------------------
// op188 own listings
// ---------------------------------------------------------------------------

func (s *Service) sendCurrentAuctionGoldOwnedItemsLocked(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 1 || body[0] != currentAuctionPayTypeConsignment {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	sellerID := selectedCharacterIDString(session)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	owner, err := s.currentGoldConsignOwner()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.returnExpiredGoldConsignLocked(ctx, session, owner, sellerID, accountID)
	s.deliverCurrentGoldConsignSalesLocked(ctx, session)
	s.reconcileGoldConsignReservationsLocked(ctx, session, owner)
	listings, err := owner.ListOwned(ctx, sellerID)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentAuctionFailureCode)
	}
	return s.sendGameUpperSuccess(session, opcode,
		buildCurrentGoldConsignOwnedBody(listings, sellerID))
}

// ---------------------------------------------------------------------------
// Sweeps
// ---------------------------------------------------------------------------

// returnExpiredGoldConsignLocked pulls this seller's timed-out consignments and
// sends their gold home. It mirrors the item auction's lazy expired sweep: any
// consignment command any player runs is enough to settle somebody's expiry.
func (s *Service) returnExpiredGoldConsignLocked(ctx context.Context, session *gameSession, owner *dnfauction.GoldConsignOwner, sellerID, accountID string) {
	if owner == nil || sellerID == "" || accountID == "" {
		return
	}
	expired, err := owner.ReturnExpired(ctx, sellerID, accountID)
	if err != nil {
		return
	}
	for _, item := range expired {
		if err := s.refundGoldByLetter(ctx, session, item.Listing, item.Refund,
			"金币寄售流拍", "您寄售的%s已到期流拍，通过邮件返还。", "goldconsign_expired_return"); err != nil {
			s.logGameEvent(session, "game-goldconsign-expiry-mail-deferred",
				"listing_id", item.Listing.ID, "error", err)
			continue
		}
		s.logGameEvent(session, "game-goldconsign-expired",
			"listing_id", item.Listing.ID, "refund_gold", item.Refund)
		// 过期记录写在信件投递成功之后。ReturnExpired 返回的就是它这一次真的
		// 摘下来的行（摘完即从托管表删除），重跑同一个扫描不会再返回同一单，
		// 所以这里不需要额外的去重判据——但邮件失败的那一轮不记：
		// "退回信没发出去"和"金币已经退回去了"是两件事，记录不能替它下结论。
		fields := goldConsignListingFields(item.Listing)
		fields["refund_gold"] = item.Refund
		s.recordGoldConsign(session, "expired", fields)
	}
}

// reconcileGoldConsignReservationsLocked releases reservations nobody paid for.
// A reservation whose buyer left a pay marker is settled instead: the buyer's
// Cera is already spent, so the consignment must complete rather than go back
// on sale.
func (s *Service) reconcileGoldConsignReservationsLocked(ctx context.Context, session *gameSession, owner *dnfauction.GoldConsignOwner) {
	if owner == nil {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return
	}
	for _, listing := range s.goldConsignReservedListings(ctx, owner) {
		if listing.ReservedAt.IsZero() {
			continue
		}
		if time.Since(listing.ReservedAt) < dnfauction.GoldConsignReservationTTL {
			continue
		}
		if s.goldConsignBuyerPaid(ctx, repositories, listing) {
			if _, err := owner.Settle(ctx, listing.BuyerID, listing.ID); err != nil {
				s.logGameEvent(session, "game-goldconsign-reconcile-settle-failed",
					"listing_id", listing.ID, "error", err)
				continue
			}
			s.logGameEvent(session, "game-goldconsign-reconcile-settled", "listing_id", listing.ID)
			continue
		}
		if err := owner.Release(ctx, listing.BuyerID, listing.ID); err != nil {
			s.logGameEvent(session, "game-goldconsign-reconcile-release-failed",
				"listing_id", listing.ID, "error", err)
			continue
		}
		s.logGameEvent(session, "game-goldconsign-reconcile-released", "listing_id", listing.ID)
	}
}

// goldConsignReservedListings finds every reserved row by walking the owned tab
// of each reservation's buyer, which is the only listing view the ledger
// exposes for a non-active status.
func (s *Service) goldConsignReservedListings(ctx context.Context, owner *dnfauction.GoldConsignOwner) []dnfauction.GoldConsignListing {
	reserved, err := owner.ReservedListings(ctx)
	if err != nil {
		return nil
	}
	return reserved
}

// goldConsignBuyerPaid reads the pay marker the payment step wrote.
func (s *Service) goldConsignBuyerPaid(ctx context.Context, repositories dnfrepo.Group, listing dnfauction.GoldConsignListing) bool {
	if repositories.Account == nil || listing.BuyerID == "" {
		return false
	}
	character, found, err := repositories.Character.Load(ctx, listing.BuyerID)
	if err != nil || !found {
		return false
	}
	accountID := strings.TrimSpace(character.AccountID)
	if accountID == "" {
		return false
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil || !found {
		return false
	}
	_, paid := dnfauction.GoldConsignReadPayMarker(account, listing.ID)
	return paid
}

// ---------------------------------------------------------------------------
// Wallet refresh
// ---------------------------------------------------------------------------

// sendCurrentGoldConsignGoldRefresh republishes the seller's gold after the
// escrow debit, on the same class-0 op14 projection the item auction uses.
func (s *Service) sendCurrentGoldConsignGoldRefresh(session *gameSession, goldAfter int64) error {
	if s == nil || session == nil {
		return nil
	}
	return s.sendGameUpperRawClass(session, 14, buildCurrentGoldStateBody(goldAfter), 0)
}

// sendCurrentGoldConsignWalletRefresh republishes the Cera wallet the buyer just
// spent from: the class-0 op53 projection the cera shop already owns.
//
// 返回值是刚发布出去的余额快照（发布失败时为零值），供买入记录填
// token_cera_after：记录侧要用到的这个数就在函数里，回传比再查一次库便宜。
func (s *Service) sendCurrentGoldConsignWalletRefresh(session *gameSession) (dnfcerashop.Wallet, error) {
	if s == nil || session == nil {
		return dnfcerashop.Wallet{}, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return dnfcerashop.Wallet{}, nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return dnfcerashop.Wallet{}, nil
	}
	account, found, err := repositories.Account.Load(context.Background(), accountID)
	if err != nil || !found {
		return dnfcerashop.Wallet{}, err
	}
	wallet := dnfcerashop.WalletBalance(account)
	if err := s.sendGameUpperRawClass(session, currentCeraShopUpdateMsgID,
		buildCurrentCeraShopWalletBody(wallet.Cera, wallet.TokenCera, wallet.HappyTokenCera), 0); err != nil {
		return dnfcerashop.Wallet{}, err
	}
	return wallet, nil
}
