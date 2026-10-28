package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestAccountSharedInventoryTownUIReadyRefreshReplaysOnlyOccupiedSharedRowsOnce(t *testing.T) {
	service, session, _, connection := newCrystalContractTestRuntime(t, map[string]dnfrepo.ItemStack{
		dnfrepo.AccountSharedInventorySlotKey(dnfrepo.CrystalWarehouseFirstSlot): {
			ItemID: 3033,
			Count:  8005,
		},
		dnfrepo.AccountSharedInventorySlotKey(dnfrepo.SoulWarehouseFirstSlot + 2): {
			ItemID: 10099774,
			Count:  77,
		},
	})
	session.townMu.Lock()
	session.accountSharedInventoryRebindPending = true
	session.townMu.Unlock()

	if err := service.sendCurrentAccountSharedInventoryTownUIReadyRefresh(session, "test_return_select_hud_ready"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(trailing) != 0 {
		t.Fatalf("shared refresh header=%+v trailing=%x", packet.Header, trailing)
	}
	body := packet.Body
	// Only the two occupied warehouse slots travel: empty cells must not reach
	// the client as 0xFFFFFFFF delete rows or the right-hand crystal/soul widget
	// clears itself instead of repainting.
	wantRows := 2
	if len(body) != 3+wantRows*currentItemListEntryWireSize ||
		body[0] != dnfrepo.MainInventoryListType ||
		int(binary.LittleEndian.Uint16(body[1:3])) != wantRows {
		t.Fatalf("shared refresh body len/header=%d/%x", len(body), body[:min(len(body), 3)])
	}
	crystal := body[3 : 3+currentItemListEntryWireSize]
	if gotSlot, gotID, gotCount := binary.LittleEndian.Uint16(crystal[0:2]), binary.LittleEndian.Uint32(crystal[2:6]), binary.LittleEndian.Uint32(crystal[6:10]); gotSlot != uint16(dnfrepo.CrystalWarehouseFirstSlot) || gotID != 3033 || gotCount != 8005 {
		t.Fatalf("crystal row slot/item/count=%d/%d/%d want=%d/3033/8005", gotSlot, gotID, gotCount, dnfrepo.CrystalWarehouseFirstSlot)
	}
	soul := body[3+currentItemListEntryWireSize:]
	if gotSlot, gotID, gotCount := binary.LittleEndian.Uint16(soul[0:2]), binary.LittleEndian.Uint32(soul[2:6]), binary.LittleEndian.Uint32(soul[6:10]); gotSlot != uint16(dnfrepo.SoulWarehouseFirstSlot+2) || gotID != 10099774 || gotCount != 77 {
		t.Fatalf("soul row slot/item/count=%d/%d/%d want=%d/10099774/77", gotSlot, gotID, gotCount, dnfrepo.SoulWarehouseFirstSlot+2)
	}

	firstWriteLen := connection.write.Len()
	if err := service.sendCurrentAccountSharedInventoryTownUIReadyRefresh(session, "test_duplicate_hud_ready"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != firstWriteLen {
		t.Fatalf("duplicate HUD-ready refresh grew stream from %d to %d", firstWriteLen, connection.write.Len())
	}
}

func TestAccountSharedInventoryTownUIReadyRefreshSkipsEmptyWarehouse(t *testing.T) {
	service, session, _, connection := newCrystalContractTestRuntime(t, nil)
	session.townMu.Lock()
	session.accountSharedInventoryRebindPending = true
	session.townMu.Unlock()
	if err := service.sendCurrentAccountSharedInventoryTownUIReadyRefresh(session, "test_return_select_hud_ready"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("empty warehouse rebind sent delete rows=%x", connection.write.Bytes())
	}
	session.townMu.Lock()
	pending := session.accountSharedInventoryRebindPending
	session.townMu.Unlock()
	if pending {
		t.Fatal("empty warehouse rebind left the pending flag armed")
	}
}

func TestAccountSharedInventoryTownUIReadyRefreshIsNotArmedForColdLogin(t *testing.T) {
	service, session, _, connection := newCrystalContractTestRuntime(t, nil)
	if err := service.sendCurrentAccountSharedInventoryTownUIReadyRefresh(session, "test_cold_login"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("cold login unexpectedly sent shared refresh=%x", connection.write.Bytes())
	}
}
