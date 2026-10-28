package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestSendAlignedMailboxExtractOrdersAckItemGoldEpicBookThenRemove(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "25",
		Slots: map[string]dnfrepo.ItemStack{
			"0:92": {ItemID: 2660452, Count: 7},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "mailbox-extract-order",
		channel:             channelcatalog.Channel{ID: 252},
		selectedCharacterID: 25,
	}
	ack := []byte{1, 1, 0, 0, 0, 9, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0}
	remove := []byte{1, 0, 0, 0, 9, 0, 0, 0}
	goldAfter := int64(3456)
	result := alignedcmd.Result{
		Operation: "mailbox_extract_ack",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketMailboxExtractItem),
			Body:           ack,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		ItemSlotRefreshes:  []alignedcmd.ItemSlotRefresh{{ListType: 0, SlotIndex: 92}},
		CharacterGoldAfter: &goldAfter,
		EpicBookBalances: []alignedcmd.EpicBookBalanceProjection{{
			FragmentID: 10300528,
			Balance:    17,
		}},
		PostItemSlotRefreshResponses: []alignedcmd.UpperResponse{{
			MsgID:          98,
			Body:           remove,
			Classification: 0,
			AllowCodec:     true,
		}},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	first, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != uint16(dnfenum.CmdPacketMailboxExtractItem) ||
		first.Header.Classification != dnfproto.DefaultChannelClassification || !bytes.Equal(first.Body, ack) {
		t.Fatalf("first packet header=%+v body=%x", first.Header, first.Body)
	}
	second, rest := splitLongHengGameServerUpperPacket(t, rest)
	if second.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		second.Header.Classification != 0 || len(second.Body) < 4 || second.Body[0] != 0 {
		t.Fatalf("second packet header=%+v body=%x", second.Header, second.Body)
	}
	third, rest := splitLongHengGameServerUpperPacket(t, rest)
	if third.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		third.Header.Classification != 0 || !bytes.Equal(third.Body, buildCurrentGoldStateBody(goldAfter)) {
		t.Fatalf("third packet header=%+v body=%x", third.Header, third.Body)
	}
	fourth, rest := splitLongHengGameServerUpperPacket(t, rest)
	wantEpicBook, err := buildCurrentEpicBookInfoBody(1, []currentEpicBookBalanceRow{{FragmentID: 10300528, Balance: 17}})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Header.MsgID != currentEpicBookInfoWireOpcode || fourth.Header.Classification != 0 || !bytes.Equal(fourth.Body, wantEpicBook) {
		t.Fatalf("fourth packet header=%+v body=%x", fourth.Header, fourth.Body)
	}
	fifth, trailing := splitLongHengGameServerUpperPacket(t, rest)
	if fifth.Header.MsgID != 98 || fifth.Header.Classification != 0 || !bytes.Equal(fifth.Body, remove) {
		t.Fatalf("fifth packet header=%+v body=%x", fifth.Header, fifth.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", trailing)
	}
}
