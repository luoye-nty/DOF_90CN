package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// TestAlignedUpperResponsesEmitWornCreatureSlotClearRow pins the exact wire
// shape of the pet-unequip fix: after the class1/op19 ACK the bridge must send
// one class0/op14 row that clears the worn creature slot.
//
// The current client resolves that row in sub_1D73120's list-3 branch: a row
// whose itemID is 0xFFFFFFFF releases the worn object, clears the slot and
// removes the equipment UI row. Before this fix the bridge never emitted it, so
// the worn-pet object survived an unequip until a full mode1 actor rebuild
// (character reselect). The bytes asserted here are exactly what the client's
// own trace shows for the same construct on list 0
// ("00 01 00 78 00 FF FF FF FF ..."), only with list 3 / slot 26.
func TestAlignedUpperResponsesEmitWornCreatureSlotClearRow(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
	}
	session := &gameSession{
		conn:                connection,
		connID:              "pet-worn-slot-clear",
		selectedCharacterID: 19,
	}

	ackBody := []byte{1, 0, 0, 0, 1, 0, 0, 0, 3, 26, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostResponseItemSlotClears: []alignedcmd.ItemSlotRefresh{
			{ListType: 3, SlotIndex: 26},
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}

	// The service is configured for the server16 header, so the matching
	// splitter is the 16-byte one; Body is then exactly what the client parses.
	ack, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != 19 {
		t.Fatalf("first packet msg=%d, want the class1/op19 ACK first", ack.Header.MsgID)
	}

	clear, rest := splitLongHengGameServerUpperPacket(t, rest)
	if clear.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("clear packet msg=%d, want %d (class0/op14)",
			clear.Header.MsgID, uint16(dnfenum.CmdPacketWalkoutPartyMember))
	}
	if clear.Header.Classification != 0 {
		t.Fatalf("clear packet classification=%d, want 0", clear.Header.Classification)
	}
	const clearBodyLen = 1 + 2 + currentItemListEntryWireSize // list type + row count + one 0x77 row
	if len(clear.Body) != clearBodyLen {
		t.Fatalf("clear packet body len=%d, want %d (body=%x)", len(clear.Body), clearBodyLen, clear.Body)
	}
	body := clear.Body
	if body[0] != 3 {
		t.Fatalf("clear packet list type=%d, want 3 (worn equipment)", body[0])
	}
	if count := binary.LittleEndian.Uint16(body[1:3]); count != 1 {
		t.Fatalf("clear packet row count=%d, want 1", count)
	}
	row := body[3:]
	if slot := binary.LittleEndian.Uint16(row[0:2]); slot != 26 {
		t.Fatalf("clear packet slot=%d, want 26 (worn creature)", slot)
	}
	if itemID := binary.LittleEndian.Uint32(row[2:6]); itemID != 0xFFFFFFFF {
		t.Fatalf("clear packet itemID=0x%08X, want 0xFFFFFFFF (delete row)", itemID)
	}
	for offset := 0x0A; offset < len(row); offset++ {
		if row[offset] != 0 {
			t.Fatalf("clear packet row byte 0x%02X=%d, want zero padding", offset, row[offset])
		}
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", rest)
	}
}
