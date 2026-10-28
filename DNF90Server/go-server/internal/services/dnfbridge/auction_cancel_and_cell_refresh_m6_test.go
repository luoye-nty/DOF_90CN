package dnfbridge

// The m6 closing pass: one file for the operator reports that are all about a
// *committed* change not being reflected - the cancel notice text and the two
// "the item is back but the bag does not show it" cases.
//
// Every rule here is pinned to client evidence, not to taste:
//
//   - 60091 / 60089 / 60088 are three of the six texts FUN_01d80150 can render,
//     switched on by the notice's own kind byte;
//   - item id -1 is the only value sub_1D73120 treats as a cell delete
//     (`0x1D73912`), and 0 leaves the old object alive so the next row for that
//     cell is dropped by the id-changed check at `0x1D73AB2`.
//
// The third m6 report - a manual cancel forfeiting the 10000 deposit - is a
// domain rule and is pinned in internal/modules/dnf/auction/owner_test.go, next
// to the client dialog text (dstr 60149 「...且无法获得缴纳的押金。」) that
// states it.

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	dnfauction "longheng.io/server/internal/modules/dnf/auction"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// ---------------------------------------------------------------------------
// N4: the source-slot row a listing emits must be a real delete.
// ---------------------------------------------------------------------------

// TestCurrentAuctionRegisterClearsTheSourceSlotWithTheDeleteRow pins the fix for
// "the auction-returned item is invisible in the bag until you reselect".
//
// The registration's op14 row for the emptied source cell used to carry item id
// 0. The client's list-0 reader only removes an object when the id is exactly -1,
// so the old object survived; the mail claim's later row for that cell then hit
// the "id changed" branch, which drops rows whose object type is not 2. Net
// effect: durable state correct, client cell never repainted.
func TestCurrentAuctionRegisterClearsTheSourceSlotWithTheDeleteRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "10", "account-seller", "Seller", 20_000, map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 6001, Count: 2, RawEntry: make([]byte, currentItemListEntryWireSize)},
	})
	service := &Service{
		options:                options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider:     func() (dnfrepo.Group, bool) { return repositories, true },
		auctionItemEligibility: func(itemID uint32) bool { return itemID == 6001 },
	}
	service.nextAuctionListingID.Store(400)
	connection := &bufferConn{}
	seller := &gameSession{conn: connection, selectedCharacterID: 10, accountID: "account-seller"}
	if err := service.registerCurrentAuctionItemLocked(seller, buildCurrentAuctionRegisterRequestForTest(9, 6001, 2, -1, 75, 75, 24)); err != nil {
		t.Fatal(err)
	}
	// ACK, then the source-cell row, then the wallet row.
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketAuctionRegistItem) {
		t.Fatalf("register ACK header=%+v", ack.Header)
	}
	slotRow, rest := splitGameServerUpperPacket(t, rest)
	if slotRow.Header.MsgID != 14 {
		t.Fatalf("source cell row msg id=%d want 14", slotRow.Header.MsgID)
	}
	listType, slot, itemID, amount := decodeSingleItemUpdateRow(t, slotRow.Body)
	if listType != 0 || slot != 9 {
		t.Fatalf("source cell row list=%d slot=%d want list 0 slot 9", listType, slot)
	}
	if itemID != 0xFFFFFFFF {
		t.Fatalf("source cell delete row itemID=0x%08X want 0xFFFFFFFF (-1); 0 leaves the old object alive", itemID)
	}
	if amount != 0 {
		t.Fatalf("delete row amount=%d want 0", amount)
	}
	// The wallet row still follows.
	wallet, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("registration left trailing bytes: %x", trailing)
	}
	if wallet.Header.MsgID != 14 {
		t.Fatalf("wallet row msg id=%d want 14", wallet.Header.MsgID)
	}
	_, walletSlot, walletItem, gold := decodeSingleItemUpdateRow(t, wallet.Body)
	wantGold := uint32(20_000 - dnfauction.ListingDepositGold)
	if walletSlot != 0 || walletItem != 0 || gold != wantGold {
		t.Fatalf("wallet row slot=%d item=%d gold=%d, want 0/0/%d", walletSlot, walletItem, gold, wantGold)
	}
}

func decodeSingleItemUpdateRow(t *testing.T, body []byte) (listType byte, slot uint16, itemID uint32, amount uint32) {
	t.Helper()
	rows := decodeItemUpdateRows(t, body)
	if len(rows) != 1 {
		t.Fatalf("item update entries=%d want 1", len(rows))
	}
	row := rows[0]
	return body[0], row.slot, row.itemID, row.amount
}

type m6ItemUpdateRow struct {
	slot   uint16
	itemID uint32
	amount uint32
}

func decodeItemUpdateRows(t *testing.T, body []byte) []m6ItemUpdateRow {
	t.Helper()
	if len(body) < 3 {
		t.Fatalf("item update body too short: %d", len(body))
	}
	count := int(binary.LittleEndian.Uint16(body[1:3]))
	if len(body) != 3+count*currentItemListEntryWireSize {
		t.Fatalf("item update body len=%d want %d for %d entries",
			len(body), 3+count*currentItemListEntryWireSize, count)
	}
	rows := make([]m6ItemUpdateRow, 0, count)
	for index := 0; index < count; index++ {
		row := body[3+index*currentItemListEntryWireSize:]
		rows = append(rows, m6ItemUpdateRow{
			slot:   binary.LittleEndian.Uint16(row[0x00:0x02]),
			itemID: binary.LittleEndian.Uint32(row[0x02:0x06]),
			amount: binary.LittleEndian.Uint32(row[0x06:0x0A]),
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// N2: the cancel/expiry/purchase banners are three different texts.
// ---------------------------------------------------------------------------

// TestCurrentAuctionDeliveryNoticesNameTheRightText pins the mapping onto the
// six strings FUN_01d80150 can render. Every return delivery used to be
// announced as kind 2, so a cancelling seller was told they had *won* their own
// auction.
func TestCurrentAuctionDeliveryNoticesNameTheRightText(t *testing.T) {
	for _, test := range []struct {
		name string
		kind currentAuctionItemMailKind
		want byte
	}{
		{"purchase", currentAuctionPurchaseMail, currentAuctionNoticeKindBuyerWon},
		{"cancel return", currentAuctionReturnMail, currentAuctionNoticeKindCancelBid},
		{"expiry return", currentAuctionExpiredMail, currentAuctionNoticeKindSellerFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.kind.noticeKind != test.want {
				t.Fatalf("notice kind=%d want %d", test.kind.noticeKind, test.want)
			}
		})
	}
	// The three kinds really are three distinct texts in the client switch.
	if currentAuctionNoticeKindBuyerWon == currentAuctionNoticeKindCancelBid ||
		currentAuctionNoticeKindCancelBid == currentAuctionNoticeKindSellerFailed ||
		currentAuctionNoticeKindSellerFailed == currentAuctionNoticeKindBuyerWon {
		t.Fatal("the three delivery notices collapsed onto one kind again")
	}
	// 60091 is kind 5, 60089 is kind 3 and 60088 is kind 2 - the exact values
	// FUN_01d80150 switches on, so the names cannot drift silently.
	if currentAuctionNoticeKindCancelBid != 5 || currentAuctionNoticeKindSellerFailed != 3 ||
		currentAuctionNoticeKindBuyerWon != 2 {
		t.Fatalf("notice kind table moved: cancel=%d failed=%d won=%d",
			currentAuctionNoticeKindCancelBid, currentAuctionNoticeKindSellerFailed, currentAuctionNoticeKindBuyerWon)
	}
}

// TestCurrentAuctionCancelAnnounces60091 walks a real cancel end to end: the ACK
// first, then the class-0 60091 banner carrying the returned listing's item id.
func TestCurrentAuctionCancelAnnounces60091(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "10", "account-seller", "Seller", 20_000, map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 6001, Count: 2, RawEntry: make([]byte, currentItemListEntryWireSize)},
	})
	connection := &bufferConn{}
	seller := &gameSession{conn: connection, selectedCharacterID: 10, accountID: "account-seller"}
	service := &Service{
		options:                options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider:     func() (dnfrepo.Group, bool) { return repositories, true },
		auctionItemEligibility: func(itemID uint32) bool { return itemID == 6001 },
		gameSessions:           map[uint16]*gameSession{10: seller},
	}
	service.nextAuctionListingID.Store(400)
	if err := service.registerCurrentAuctionItemLocked(seller, buildCurrentAuctionRegisterRequestForTest(9, 6001, 2, -1, 75, 75, 24)); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()

	cancel := make([]byte, 9)
	cancel[0] = currentAuctionPayTypeGold
	binary.LittleEndian.PutUint64(cancel[1:9], 401)
	if err := service.cancelCurrentAuctionItemLocked(seller, cancel); err != nil {
		t.Fatal(err)
	}
	// The ACK, then the return letter's mailbox alarm (class0/0x63), then the
	// 60091 banner on its own class0/0xA9 packet.
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketAuctionRegistCancel) {
		t.Fatalf("cancel ACK header=%+v", ack.Header)
	}
	alarm, rest := splitGameServerUpperPacket(t, rest)
	if alarm.Header.MsgID != 0x0063 || alarm.Header.Classification != 0 {
		t.Fatalf("mailbox alarm header=%+v want class0/0x0063", alarm.Header)
	}
	notice, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("cancel left trailing bytes: %x", trailing)
	}
	if notice.Header.MsgID != currentAuctionNoticeMsgID || notice.Header.Classification != 0 {
		t.Fatalf("notice header=%+v want class0/0x00A9", notice.Header)
	}
	want := []byte{
		currentAuctionNoticeRouteItem,
		currentAuctionNoticeKindCancelBid,
		0x71, 0x17, 0x00, 0x00, // 6001 little endian
		0x00, 0x00,
	}
	if len(notice.Body) != 8 || !bytes.Equal(notice.Body, want) {
		t.Fatalf("cancel notice body=% X want % X", notice.Body, want)
	}
	// The forfeited deposit: the return letter carries the item and no gold, and
	// the 10000 is still sitting in escrow instead of having been paid back.
	mailbox, found, err := repositories.Mailbox.Load(ctx, "10")
	if err != nil || !found || len(mailbox.Mails) != 1 {
		t.Fatalf("seller mailbox found=%t err=%v mails=%+v", found, err, mailbox.Mails)
	}
	for _, returnMail := range mailbox.Mails {
		if returnMail.Metadata["source"] != currentAuctionReturnMail.source {
			t.Fatalf("return mail source=%q want %q", returnMail.Metadata["source"], currentAuctionReturnMail.source)
		}
		if returnMail.Gold != 0 {
			t.Fatalf("return mail carries %d gold, want 0 (a manual cancel forfeits the deposit)", returnMail.Gold)
		}
		if len(returnMail.Attachments) != 1 || returnMail.Attachments[0].ItemID != 6001 || returnMail.Attachments[0].Count != 2 {
			t.Fatalf("return mail attachments=%+v want the 2x 6001 stack", returnMail.Attachments)
		}
	}
	escrowCharacter, _, _ := repositories.Character.Load(ctx, dnfauction.EscrowCharacterID)
	if escrowCharacter.Stats["gold"] != dnfauction.ListingDepositGold {
		t.Fatalf("escrow gold=%d, want the forfeited %d kept",
			escrowCharacter.Stats["gold"], dnfauction.ListingDepositGold)
	}
	// Nothing is left owed in the outbox.
	owner, _ := dnfauction.NewOwner(repositories)
	if pending, err := owner.PendingDeliveries(ctx, "10"); err != nil || len(pending) != 0 {
		t.Fatalf("outbox after the return mail=%+v err=%v", pending, err)
	}
}

// ---------------------------------------------------------------------------
// N3: a gold consignment cancel announces 60091 on the gold route.
// ---------------------------------------------------------------------------

// TestCurrentGoldConsignCancelAnnounces60091 pins the fix for "cancelling a
// consignment says nothing". The gold panel has no notice of its own on the
// cancel path - the client's op184 handler only pops dstr 60084 (已取消拍卖物品的
// 上架), which is the item panel's wording - so the seller has to be told, and
// the text the auction notice table provides for it is kind 5:
// dstr 60091 「[%s]的竞拍已被取消，请确认邮件」.
func TestCurrentGoldConsignCancelAnnounces60091(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	connection := &bufferConn{}
	seller := &gameSession{conn: connection, selectedCharacterID: 11, accountID: "account-seller"}
	service := goldConsignTestService(repositories)
	service.gameSessions = map[uint16]*gameSession{11: seller}
	if err := service.registerCurrentAuctionItemLocked(seller, goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()
	connection.write.Reset()

	cancel := make([]byte, currentAuctionGoldCancelRequestSize)
	cancel[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint64(cancel[1:9], listingID)
	if err := service.cancelCurrentAuctionItemLocked(seller, cancel); err != nil {
		t.Fatal(err)
	}
	// The full wire: the ACK, then the return letter's two mailbox packets - the
	// class0/0x63 alarm and the class0/0x61 page the gold letter path adds so an
	// open mailbox actually reloads - and last the 60091 banner.
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketAuctionRegistCancel) || !bytes.Equal(ack.Body, []byte{1, 1}) {
		t.Fatalf("gold cancel ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	alarm, rest := splitGameServerUpperPacket(t, rest)
	if alarm.Header.MsgID != 0x0063 || !bytes.Equal(alarm.Body, []byte{1, 0}) {
		t.Fatalf("gold cancel alarm header=%+v body=%x want class0/0x0063 count 1", alarm.Header, alarm.Body)
	}
	page, rest := splitGameServerUpperPacket(t, rest)
	if page.Header.MsgID != 0x0061 {
		t.Fatalf("gold cancel mailbox page header=%+v want class0/0x0061", page.Header)
	}
	notice, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("gold cancel left trailing bytes: %x", trailing)
	}
	if notice.Header.MsgID != currentAuctionNoticeMsgID {
		t.Fatalf("gold cancel notice header=%+v want class0/0x00A9", notice.Header)
	}
	want := []byte{
		currentAuctionNoticeRouteGold,
		currentAuctionNoticeKindCancelBid,
		0x7D, 0xEB, 0x28, 0x00, // 2681725 (1百万) little endian
		0x00, 0x00,
	}
	if !bytes.Equal(notice.Body, want) {
		t.Fatalf("gold cancel notice body=% X want % X", notice.Body, want)
	}
}

// ---------------------------------------------------------------------------
// N5: closing a stall republishes the cells it was selling from.
// ---------------------------------------------------------------------------

// newM6PrivateStoreService is the minimum a stall needs: a licensed seller in a
// town area, a repository group, and the two per-store maps.
func newM6PrivateStoreService(t *testing.T, ctx context.Context, repositories dnfrepo.Group) (*Service, *gameSession) {
	t.Helper()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "seller-account",
		Metadata: map[string]string{
			premium.MetadataKey(currentPrivateStorePremiumType): strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	sellerConnection := &bufferConn{}
	seller := &gameSession{conn: sellerConnection, selectedCharacterID: 10, accountID: "seller-account"}
	service := &Service{
		options:                     options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider:          func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:                map[uint16]*gameSession{10: seller},
		privateStores:               make(map[uint16]*currentPrivateStore),
		privateStoreVisitors:        make(map[*gameSession]uint16),
		privateStoreItemEligibility: func(uint32) bool { return true },
		expertJobStores:             make(map[uint16]*currentExpertJobStore),
		onlinePlayers:               newOnlinePlayerManager(),
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 10, AccountID: "seller-account", Name: "Seller",
		TownID: 1, AreaID: 2, PositionX: 100, PositionY: 200, Session: seller,
	})
	return service, seller
}

// m6PrivateStoreCompleteBody builds the op87 body: title, one row count, then
// rows of {display u16, price u32, list u8, slot u16, instance u32} and the
// trailing permission slot.
func m6PrivateStoreCompleteBody(rows ...[5]uint32) []byte {
	var complete packetWriter
	complete.writeRawDstr([]byte("shop"))
	complete.writeByte(byte(len(rows)))
	for _, row := range rows {
		complete.writeUint16(uint16(row[0]))
		complete.writeUint32(row[1])
		complete.writeByte(byte(row[2]))
		complete.writeUint16(uint16(row[3]))
		complete.writeUint32(row[4])
	}
	complete.writeUint16(0xFFFF)
	return complete.bytes()
}

// TestCurrentPrivateStoreCloseRepublishesUnsoldSourceSlots covers the second
// "invisible until reselect" report. A stall never moves its items - the rows
// name live cells - so nothing was ever pushed back when it closed and the
// client's bag kept the stacks hidden.
func TestCurrentPrivateStoreCloseRepublishesUnsoldSourceSlots(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "10", AccountID: "seller-account", Name: "Seller",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "10", Slots: map[string]dnfrepo.ItemStack{
		"0:4": {ItemID: 7001, Count: 5},
		"0:6": {ItemID: 7002, Count: 3},
	}}); err != nil {
		t.Fatal(err)
	}
	service, seller := newM6PrivateStoreService(t, ctx, repositories)
	if err := service.createCurrentPrivateStore(seller, []byte{1, 2, 100, 0, 200, 0, 0xFF, 0xFF}); err != nil {
		t.Fatal(err)
	}
	if err := service.completeCurrentPrivateStore(seller, m6PrivateStoreCompleteBody(
		[5]uint32{0, 100, 0, 4, 1},
		[5]uint32{1, 200, 0, 6, 1},
	)); err != nil {
		t.Fatal(err)
	}
	seller.conn.(*bufferConn).write.Reset()

	if err := service.closeCurrentPrivateStoreCommand(seller, uint16(dnfenum.CmdPacketRemovePrivateStore), nil); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, seller.conn.(*bufferConn).write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketRemovePrivateStore) || ack.Body[0] != 1 {
		t.Fatalf("close ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	// Then one op14 row per source cell, read back from the durable inventory.
	// Both cells are list 0, so the helper groups them into a single packet.
	seen := map[uint16]uint32{}
	for len(rest) != 0 {
		packet, next := splitGameServerUpperPacket(t, rest)
		rest = next
		if packet.Header.MsgID != 14 || packet.Header.Classification != 0 {
			t.Fatalf("close emitted a non-item-update packet: %+v", packet.Header)
		}
		if packet.Body[0] != 0 {
			t.Fatalf("close refresh list=%d want 0", packet.Body[0])
		}
		for _, row := range decodeItemUpdateRows(t, packet.Body) {
			seen[row.slot] = row.itemID
		}
	}
	if len(seen) != 2 || seen[4] != 7001 || seen[6] != 7002 {
		t.Fatalf("close refresh rows=%+v want cell 4 -> 7001 and cell 6 -> 7002", seen)
	}
	if _, stillOpen := service.privateStores[10]; stillOpen {
		t.Fatal("the stall survived its own close")
	}
}

// A cell the stall emptied (its whole stack sold) is republished as a delete
// row, so a sold-out cell cannot be left holding a phantom object either.
func TestCurrentPrivateStoreCloseRepublishesSoldOutCellsAsDeletes(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "10", AccountID: "seller-account", Name: "Seller",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "10", Slots: map[string]dnfrepo.ItemStack{
		"0:4": {ItemID: 7001, Count: 5},
	}}); err != nil {
		t.Fatal(err)
	}
	service, seller := newM6PrivateStoreService(t, ctx, repositories)
	if err := service.createCurrentPrivateStore(seller, []byte{1, 2, 100, 0, 200, 0, 0xFF, 0xFF}); err != nil {
		t.Fatal(err)
	}
	if err := service.completeCurrentPrivateStore(seller, m6PrivateStoreCompleteBody(
		[5]uint32{0, 100, 0, 4, 1},
	)); err != nil {
		t.Fatal(err)
	}
	// Simulate the sale having emptied the cell durably.
	inventory, _, _ := repositories.Inventory.Load(ctx, "10")
	delete(inventory.Slots, "0:4")
	if err := repositories.Inventory.Save(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	seller.conn.(*bufferConn).write.Reset()
	if err := service.closeCurrentPrivateStoreCommand(seller, uint16(dnfenum.CmdPacketRemovePrivateStore), nil); err != nil {
		t.Fatal(err)
	}
	_, rest := splitGameServerUpperPacket(t, seller.conn.(*bufferConn).write.Bytes())
	row, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("trailing=%x", trailing)
	}
	_, slot, itemID, _ := decodeSingleItemUpdateRow(t, row.Body)
	if slot != 4 || itemID != 0xFFFFFFFF {
		t.Fatalf("sold-out close row slot=%d itemID=0x%08X want slot 4 / 0xFFFFFFFF", slot, itemID)
	}
}
