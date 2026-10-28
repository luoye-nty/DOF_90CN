package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"

	dnfauction "longheng.io/server/internal/modules/dnf/auction"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// goldConsignTestService is the minimum a gold handler needs now that the
// ledger answers from real rows instead of a hardcoded empty list: a repository
// group, so EnsureEscrow can create the shared escrow character.
func goldConsignTestService(repositories dnfrepo.Group) *Service {
	return &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
}

// Gold consignment (金币寄售) shares op182 with the ordinary item auction but is
// a distinct request shape. The bodies below are not invented: they are the
// exact bytes the live client sent on 2026-09-21T09:22Z, read out of
// runtime/logs/packet_log.txt, where every one of them was answered with
// body 00 99 00 - failure code 153 「因不明原因操作失败」, the popup the gold
// panel showed the moment it opened.
//
//	09:22:21  body=017DEB280000  mode 1 + item 2681725 (gold_1m,  「1百万」)
//	09:22:29  body=017FEB280000  mode 1 + item 2681727 (gold_3m,  「3百万」)
//	09:22:31  body=017DEB280000  mode 1 + item 2681725
//	09:22:33  body=0185EB280000  mode 1 + item 2681733 (gold_9m,  「9百万」)
//
// All three item ids fall inside the 17-entry denomination table the client
// hardcodes at NoPack.exe .rdata VA 0x4580AA8 (2681725..2681736 then
// 2683069..2683073), which is how the capture and the client table confirm each
// other.
func TestCurrentAuctionGoldAveragePriceAcceptsLiveCaptureShape(t *testing.T) {
	for _, live := range []struct {
		name   string
		body   []byte
		itemID uint32
	}{
		{"1百万 gold_1m", []byte{0x01, 0x7D, 0xEB, 0x28, 0x00, 0x00}, 2681725},
		{"3百万 gold_3m", []byte{0x01, 0x7F, 0xEB, 0x28, 0x00, 0x00}, 2681727},
		{"9百万 gold_9m", []byte{0x01, 0x85, 0xEB, 0x28, 0x00, 0x00}, 2681733},
	} {
		t.Run(live.name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{options: options{
				gameUpperHeader:    gameUpperHeaderChannel13,
				gameUpperBodyCodec: gameUpperBodyCodecPlain,
			}}
			session := &gameSession{conn: connection, selectedCharacterID: 11}

			if err := service.sendCurrentAuctionAveragePriceLocked(session, live.body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(trailing) != 0 {
				t.Fatalf("trailing=%x", trailing)
			}
			if packet.Header.MsgID != uint16(dnfenum.CmdPacketAuctionAskAveragePrice) {
				t.Fatalf("msgID=%d want=%d", packet.Header.MsgID, dnfenum.CmdPacketAuctionAskAveragePrice)
			}
			// Success flag, then the mode byte echoed back as 1, then the same
			// 57-byte envelope the item auction reader consumes.
			if len(packet.Body) != 58 || packet.Body[0] != 1 {
				t.Fatalf("body=%x len=%d", packet.Body, len(packet.Body))
			}
			if packet.Body[1] != currentAuctionPayTypeConsignment {
				t.Fatalf("mode byte=%d want=1 body=%x", packet.Body[1], packet.Body)
			}
			// No consignment ledger exists, so the honest answer is zero average
			// and zero sales count - every remaining word must be zero.
			if !bytes.Equal(packet.Body[2:], make([]byte, 56)) {
				t.Fatalf("non-zero payload for a ledger that has no sales: %x", packet.Body)
			}
			if got := binary.LittleEndian.Uint32(packet.Body[2:6]); got != 0 {
				t.Fatalf("average=%d want=0", got)
			}
			if got := binary.LittleEndian.Uint32(packet.Body[6:10]); got != 0 {
				t.Fatalf("count=%d want=0", got)
			}
			if live.itemID == 0 {
				t.Fatal("fixture lost its item id")
			}
		})
	}
}

// The gold branch may only ever claim the one shape the gold panel actually
// sends. Every neighbouring shape must keep the refusal it had before, so a
// future client change cannot silently widen what this server accepts.
func TestCurrentAuctionAveragePriceGateOnlyWidensForLiveGoldShape(t *testing.T) {
	gold := func(mode byte, itemID uint32, reserved byte) []byte {
		body := []byte{mode, 0, 0, 0, 0, reserved}
		binary.LittleEndian.PutUint32(body[1:5], itemID)
		return body
	}
	item := func(mode byte, itemID uint32) []byte {
		body := make([]byte, 8)
		body[0] = mode
		binary.LittleEndian.PutUint32(body[1:5], itemID)
		return body
	}

	for _, test := range []struct {
		name string
		body []byte
	}{
		{"nil body", nil},
		{"empty body", []byte{}},
		{"6 bytes mode 0 is not consignment", gold(0, 2681725, 0)},
		{"6 bytes mode 2 is not consignment", gold(2, 2681725, 0)},
		{"6 bytes mode 1 item 0 is refused", gold(1, 0, 0)},
		{"5 bytes mode 1 truncated", gold(1, 2681725, 0)[:5]},
		{"7 bytes mode 1 overlong", append(gold(1, 2681725, 0), 0)},
		{"8 bytes mode 1 stays on the item gate", item(1, 6001)},
		{"8 bytes mode 0 without a repository still refuses", item(0, 6001)},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{options: options{
				gameUpperHeader:    gameUpperHeaderChannel13,
				gameUpperBodyCodec: gameUpperBodyCodecPlain,
			}}
			session := &gameSession{conn: connection, selectedCharacterID: 11}

			if err := service.sendCurrentAuctionAveragePriceLocked(session, test.body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(trailing) != 0 {
				t.Fatalf("trailing=%x", trailing)
			}
			// The refusal the gold panel used to get, unchanged: failure flag,
			// code 153, then the pay-feature byte op182 requires.
			want := []byte{0, currentAuctionFailureCode, currentAuctionPayTypeGold}
			if !bytes.Equal(packet.Body, want) {
				t.Fatalf("body=%x want=%x", packet.Body, want)
			}
		})
	}
}

// The 金币寄售 "金币搜索" request is op186 with mode 1 and no 8-byte detail tail.
// This is the exact body the live client sent for the 1百万 tree node on
// 2026-09-21T09:35:47Z (packet_log.txt), which the server answered 00 99 00 -
// failure code 153 「因不明原因操作失败」, the second popup.
//
//	body = 01 00000000 00 1F 01 A59C 7DEB2800
//	       mode  page     -  -  n  cat(=40101) item(=2681725)
func TestCurrentAuctionGoldSearchAcceptsLiveCaptureShape(t *testing.T) {
	live := []byte{
		0x01,                   // mode 1 = 金币寄售
		0x00, 0x00, 0x00, 0x00, // page 0
		0x00, 0x1F, // the two bytes the item panel also sends
		0x01,       // count 1
		0xA5, 0x9C, // category 40101
		0x7D, 0xEB, 0x28, 0x00, // item 2681725 (gold_1m, 「1百万」)
	}
	if len(live) != 14 {
		t.Fatalf("fixture is not the captured 14-byte body: len=%d", len(live))
	}
	if got := binary.LittleEndian.Uint32(live[10:14]); got != 2681725 {
		t.Fatalf("fixture lost its tier id: %d", got)
	}

	connection := &bufferConn{}
	service := goldConsignTestService(dnfrepomemory.NewMemoryGroup())
	session := &gameSession{conn: connection, selectedCharacterID: 11}

	if err := service.searchCurrentAuctionItemsLocked(session, live); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketAuctionSearchByItemkey) {
		t.Fatalf("msgID=%d want=%d", packet.Header.MsgID, dnfenum.CmdPacketAuctionSearchByItemkey)
	}
	// success | mode 1 | u32 total 0 | u16 count 0 - the same 7-byte empty-page
	// envelope the item reader consumes, which
	// TestCurrentAuctionEmptyResultBodiesMatchCurrentEXEReaders already pins for
	// mode 0.
	want := []byte{1, currentAuctionPayTypeConsignment, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("body=%x want=%x", packet.Body, want)
	}
}

// The op186 diversion must claim mode 1 only, and the shared parser must keep
// rejecting mode 1 for the item panel. Both directions are pinned here so a
// later change cannot silently swap which panel owns which shape.
func TestCurrentAuctionSearchGateAndParserKeepPanelsApart(t *testing.T) {
	gold := []byte{
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1F, 0x01, 0xA5, 0x9C, 0x7D, 0xEB, 0x28, 0x00,
	}
	// The item panel appends an 8-byte detail tail; the gold panel omits it.
	item := append([]byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1F, 0x01, 0xB7, 0x9C, 0x95, 0x6E, 0x00, 0x00,
	}, make([]byte, 8)...)

	// Parser level: the shared body parser is mode-strict in both directions.
	if _, items, ok := decodeCurrentAuctionSearchItemIDs(gold); ok || items != nil {
		t.Fatal("item parser accepted a mode-1 gold search")
	}
	if _, items, ok := decodeCurrentAuctionSearchItemIDs(item); !ok || len(items) != 1 || items[0] != 28309 {
		t.Fatalf("item parser no longer reads the item shape: ok=%t items=%v", ok, items)
	}
	if _, items, ok := decodeCurrentAuctionSearchBody(gold, currentAuctionPayTypeConsignment); !ok ||
		len(items) != 1 || items[0] != 2681725 {
		t.Fatalf("gold parser cannot read the captured shape: ok=%t items=%v", ok, items)
	}
	if _, _, ok := decodeCurrentAuctionSearchBody(item, currentAuctionPayTypeConsignment); ok {
		t.Fatal("gold parser accepted a mode-0 item search")
	}

	modeTwo := append([]byte(nil), gold...)
	modeTwo[0] = 0x02
	countZero := append([]byte(nil), gold...)
	countZero[7] = 0x00

	// Handler level: only mode 1 is diverted. A mode-0 body keeps the item path,
	// which without a repository still answers the 153 refusal it always did.
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"item search keeps the item path", item},
		{"mode 2 is not diverted", modeTwo},
		{"truncated gold search", gold[:5]},
		{"gold search with count 0", countZero},
		{"empty body", []byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{options: options{
				gameUpperHeader:    gameUpperHeaderChannel13,
				gameUpperBodyCodec: gameUpperBodyCodecPlain,
			}}
			session := &gameSession{conn: connection, selectedCharacterID: 11}

			if err := service.searchCurrentAuctionItemsLocked(session, test.body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(trailing) != 0 {
				t.Fatalf("trailing=%x", trailing)
			}
			want := []byte{0, currentAuctionFailureCode, currentAuctionPayTypeGold}
			if !bytes.Equal(packet.Body, want) {
				t.Fatalf("body=%x want=%x", packet.Body, want)
			}
		})
	}
}

// The 金币寄售 tab 我寄售的金币 is op188 with the same one-byte body as the item
// tab but mode 1. Live capture 2026-09-21T09:43:18Z (packet_log.txt): op188 body
// `01`, answered 00 99 00 - failure code 153, the third popup.
func TestCurrentAuctionGoldOwnedItemsAcceptsLiveCaptureShape(t *testing.T) {
	connection := &bufferConn{}
	service := goldConsignTestService(dnfrepomemory.NewMemoryGroup())
	session := &gameSession{conn: connection, selectedCharacterID: 11}

	if err := service.sendCurrentAuctionOwnedItemsLocked(session, []byte{currentAuctionPayTypeConsignment}); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketAuctionMyRegistedItemInfo) {
		t.Fatalf("msgID=%d want=%d", packet.Header.MsgID, dnfenum.CmdPacketAuctionMyRegistedItemInfo)
	}
	// success | mode 1 | count 0 - the empty 我寄售的金币 list.
	want := []byte{1, currentAuctionPayTypeConsignment, 0}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("body=%x want=%x", packet.Body, want)
	}
}

// Only mode 1 may be diverted on op188. A mode-0 body keeps the item tab, which
// without a repository still answers the refusal it always did.
func TestCurrentAuctionOwnedItemsGateOnlyDivertsGoldMode(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"item tab keeps the item path", []byte{currentAuctionPayTypeGold}},
		{"mode 2 is not diverted", []byte{0x02}},
		{"trailing byte is refused", []byte{currentAuctionPayTypeConsignment, 0x00}},
		{"empty body", []byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{options: options{
				gameUpperHeader:    gameUpperHeaderChannel13,
				gameUpperBodyCodec: gameUpperBodyCodecPlain,
			}}
			session := &gameSession{conn: connection, selectedCharacterID: 11}

			if err := service.sendCurrentAuctionOwnedItemsLocked(session, test.body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(trailing) != 0 {
				t.Fatalf("trailing=%x", trailing)
			}
			want := []byte{0, currentAuctionFailureCode, currentAuctionPayTypeGold}
			if !bytes.Equal(packet.Body, want) {
				t.Fatalf("body=%x want=%x", packet.Body, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Full consignment flow
// ---------------------------------------------------------------------------

// seedGoldConsignProtocolCharacter writes everything a gold handler reads: a
// character with a level and a gold wallet, its account (with whatever wallet
// metadata the case needs), and an empty inventory.
func seedGoldConsignProtocolCharacter(t *testing.T, ctx context.Context, repositories dnfrepo.Group, characterID, accountID, name string, level int, gold int64, metadata map[string]string) {
	t.Helper()
	if metadata == nil {
		metadata = map[string]string{}
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: characterID, AccountID: accountID, Name: name, Level: level,
		Stats: map[string]int64{"gold": gold},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: accountID, Metadata: metadata,
	}); err != nil {
		t.Fatal(err)
	}
}

func goldConsignProtocolGold(t *testing.T, ctx context.Context, repositories dnfrepo.Group, characterID string) int64 {
	t.Helper()
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load %s: found=%t err=%v", characterID, found, err)
	}
	return character.Stats["gold"]
}

// goldConsignLiveRegisterBody is the exact live capture for 1百万 @ 111 Cera /
// 24h, reused by the end-to-end cases.
func goldConsignLiveRegisterBody() []byte {
	return []byte{
		0x01, 0x00, 0x00, 0x00, // mode 1, 0, u16 0
		0x7D, 0xEB, 0x28, 0x00, // item 2681725 (gold_1m, 「1百万」)
		0x01, 0x00, 0x00, 0x00, // quantity 1
		0xFF, 0xFF, 0xFF, 0xFF, // bid -1
		0x6F, 0x00, 0x00, 0x00, // 111 Cera
		0x00, 0x00, 0x00, 0x00, // reserved
		0x18, 0x00, 0x00, 0x00, // 24 hours
	}
}

func goldConsignOwnerForTest(t *testing.T, repositories dnfrepo.Group) *dnfauction.GoldConsignOwner {
	t.Helper()
	owner, err := dnfauction.NewGoldConsignOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// The captured op183 body now escrows real gold and answers the item panel's
// own registration ACK.
func TestGoldConsignRegisterEndToEndEscrowsGold(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "卖家", 40, 5_000_000, nil)
	connection := &bufferConn{}
	service := goldConsignTestService(repositories)
	session := &gameSession{conn: connection, selectedCharacterID: 11, accountID: "account-seller"}

	if err := service.registerCurrentAuctionItemLocked(session, goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	ack, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketAuctionRegistItem) || ack.Body[0] != 1 {
		t.Fatalf("register ack header=%+v body=%x", ack.Header, ack.Body)
	}
	// The escrow debit also republishes the seller's gold on class-0 op14.
	if len(trailing) < 3 || trailing[0] != 0 || binary.LittleEndian.Uint16(trailing[1:3]) != 14 {
		t.Fatalf("expected a class-0 op14 gold refresh after the ack, trailing=%x", trailing)
	}
	if gold := goldConsignProtocolGold(t, ctx, repositories, "11"); gold != 4_000_000 {
		t.Fatalf("seller gold = %d, want 4000000", gold)
	}
	if gold := goldConsignProtocolGold(t, ctx, repositories, "system:auction:escrow"); gold != 1_000_000 {
		t.Fatalf("escrow gold = %d, want 1000000", gold)
	}
}

// Only the captured 28-byte mode-1 shape may be diverted; every neighbouring
// shape keeps the refusal it had before.
func TestGoldConsignRegisterGateOnlyDivertsTheCapturedShape(t *testing.T) {
	captured := goldConsignLiveRegisterBody()
	truncated := append([]byte(nil), captured[:27]...)
	overlong := append(append([]byte(nil), captured...), 0x00)
	quantityTwo := append([]byte(nil), captured...)
	binary.LittleEndian.PutUint32(quantityTwo[8:12], 2)
	badLifetime := append([]byte(nil), captured...)
	binary.LittleEndian.PutUint32(badLifetime[24:28], 12)
	notATier := append([]byte(nil), captured...)
	binary.LittleEndian.PutUint32(notATier[4:8], 2681741)

	for _, test := range []struct {
		name string
		body []byte
	}{
		{"exact capture minus one byte", truncated},
		{"exact capture plus one byte", overlong},
		{"quantity is not fixed at 1", quantityTwo},
		{"lifetime outside 24/48", badLifetime},
		{"item is not a denomination", notATier},
		{"empty body", []byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
			connection := &bufferConn{}
			service := goldConsignTestService(repositories)
			session := &gameSession{conn: connection, selectedCharacterID: 11, accountID: "account-seller"}

			if err := service.registerCurrentAuctionItemLocked(session, test.body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(trailing) != 0 {
				t.Fatalf("trailing=%x", trailing)
			}
			if packet.Body[0] != 0 {
				t.Fatalf("a malformed consignment was accepted: body=%x", packet.Body)
			}
			if gold := goldConsignProtocolGold(t, ctx, repositories, "11"); gold != 5_000_000 {
				t.Fatalf("refused register moved gold: %d", gold)
			}
		})
	}
}

// Cancel returns the gold as a letter rather than into the bag, and the row
// leaves the shop.
func TestGoldConsignCancelSendsGoldHomeByLetter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	connection := &bufferConn{}
	service := goldConsignTestService(repositories)
	session := &gameSession{conn: connection, selectedCharacterID: 11, accountID: "account-seller"}
	if err := service.registerCurrentAuctionItemLocked(session, goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()
	connection.write.Reset()

	cancel := make([]byte, currentAuctionGoldCancelRequestSize)
	cancel[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint64(cancel[1:9], listingID)
	if err := service.cancelCurrentAuctionItemLocked(session, cancel); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	if packet.Body[0] != 1 || packet.Body[1] != currentAuctionPayTypeConsignment {
		t.Fatalf("cancel ack body=%x", packet.Body)
	}
	if gold := goldConsignProtocolGold(t, ctx, repositories, "system:auction:escrow"); gold != 0 {
		t.Fatalf("escrow after cancel = %d, want 0", gold)
	}
	// The seller's wallet is credited on claim, not on cancel.
	if gold := goldConsignProtocolGold(t, ctx, repositories, "11"); gold != 4_000_000 {
		t.Fatalf("seller gold after cancel = %d, want 4000000", gold)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("mailbox: found=%t err=%v", found, err)
	}
	wantKey := "goldconsign:refund:goldconsign_cancel_return:" + strconv.FormatUint(listingID, 10)
	refunded := false
	for _, mail := range mailbox.Mails {
		if mail.Metadata["completion_key"] == wantKey {
			refunded = true
			if mail.Gold != 1_000_000 {
				t.Fatalf("refund letter carries %d gold, want 1000000", mail.Gold)
			}
		}
	}
	if !refunded {
		t.Fatalf("no refund letter was enqueued; mails=%+v", mailbox.Mails)
	}
}

// The buy saga takes Cera from the buyer and enqueues a gold letter for them
// plus a 代币券 letter for the seller.
func TestGoldConsignBuyTakesCeraAndPaysTokenCeraByLetter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "5000"})
	connection := &bufferConn{}
	service := goldConsignTestService(repositories)
	seller := &gameSession{conn: connection, selectedCharacterID: 11, accountID: "account-seller"}
	if err := service.registerCurrentAuctionItemLocked(seller, goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()

	buyerConn := &bufferConn{}
	buyer := &gameSession{conn: buyerConn, selectedCharacterID: 22, accountID: "account-buyer"}
	buy := make([]byte, currentAuctionGoldBuyRequestSize)
	buy[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint32(buy[1:5], 111)
	binary.LittleEndian.PutUint64(buy[5:13], listingID)
	if err := service.buyCurrentAuctionItemLocked(buyer, uint16(dnfenum.CmdPacketAuctionBidding), buy, false); err != nil {
		t.Fatal(err)
	}
	ack, trailing := splitGameServerUpperPacket(t, buyerConn.write.Bytes())
	if ack.Body[0] != 1 || ack.Body[1] != currentAuctionPayTypeConsignment {
		t.Fatalf("buy ack body=%x", ack.Body)
	}
	// The Cera debit also republishes the buyer's wallet on class-0 op53.
	if len(trailing) < 3 || trailing[0] != 0 || binary.LittleEndian.Uint16(trailing[1:3]) != currentCeraShopUpdateMsgID {
		t.Fatalf("expected a class-0 op%d wallet refresh after the ack, trailing=%x",
			currentCeraShopUpdateMsgID, trailing)
	}
	// 111 Cera at the base 5% leaves the seller 105: 86 floors the proceeds.
	buyerAccount, found, err := repositories.Account.Load(ctx, "account-buyer")
	if err != nil || !found {
		t.Fatal(err)
	}
	if got := buyerAccount.Metadata["account_cera"]; got != "4889" {
		t.Fatalf("buyer Cera = %s, want 4889", got)
	}
	if gold := goldConsignProtocolGold(t, ctx, repositories, "system:auction:escrow"); gold != 0 {
		t.Fatalf("escrow after sale = %d, want 0", gold)
	}
	buyerBox, _, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil {
		t.Fatal(err)
	}
	buyerLetter := false
	for _, mail := range buyerBox.Mails {
		if mail.Gold == 1_000_000 {
			buyerLetter = true
		}
	}
	if !buyerLetter {
		t.Fatalf("buyer got no gold letter: %+v", buyerBox.Mails)
	}
	sellerBox, _, err := repositories.Mailbox.Load(ctx, "11")
	if err != nil {
		t.Fatal(err)
	}
	voucherLetter := false
	for _, mail := range sellerBox.Mails {
		for _, attachment := range mail.Attachments {
			if attachment.ItemID == 2681762 && attachment.Count == 105 {
				voucherLetter = true
			}
		}
	}
	if !voucherLetter {
		t.Fatalf("seller got no 代币券 letter: %+v", sellerBox.Mails)
	}
	// The sale now feeds the fee history.
	owner := goldConsignOwnerForTest(t, repositories)
	if average, samples, err := owner.AverageCeraPrice(ctx, 2681725); err != nil || average != 111 || samples != 1 {
		t.Fatalf("average after the sale = %d/%d err=%v, want 111/1", average, samples, err)
	}
}

// A buyer who cannot pay is refused, and the row stays on sale.
func TestGoldConsignBuyRefusesWithoutCeraAndLeavesTheRowOnSale(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "50"})
	connection := &bufferConn{}
	service := goldConsignTestService(repositories)
	seller := &gameSession{conn: connection, selectedCharacterID: 11, accountID: "account-seller"}
	if err := service.registerCurrentAuctionItemLocked(seller, goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()

	buyerConn := &bufferConn{}
	buyer := &gameSession{conn: buyerConn, selectedCharacterID: 22, accountID: "account-buyer"}
	buy := make([]byte, currentAuctionGoldBuyRequestSize)
	buy[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint32(buy[1:5], 111)
	binary.LittleEndian.PutUint64(buy[5:13], listingID)
	if err := service.buyCurrentAuctionItemLocked(buyer, uint16(dnfenum.CmdPacketAuctionBidding), buy, false); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, buyerConn.write.Bytes())
	if packet.Body[0] != 0 {
		t.Fatalf("an unaffordable buy succeeded: body=%x", packet.Body)
	}
	if gold := goldConsignProtocolGold(t, ctx, repositories, "system:auction:escrow"); gold != 1_000_000 {
		t.Fatalf("a refused buy released the escrow: %d", gold)
	}
	owner := goldConsignOwnerForTest(t, repositories)
	live, err := owner.Search(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Status != dnfauction.GoldConsignActive {
		t.Fatalf("a refused buy left the row unavailable: %+v", live)
	}
}
