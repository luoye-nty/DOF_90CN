package inventory

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func handleForgingRequest(t *testing.T, repos dnfrepo.Group, resolver alignedcmd.ForgingResolver, body []byte) alignedcmd.Result {
	t.Helper()
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUpgradeItemSeparate),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repos,
		ForgingResolver:     resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHandlerForgingSendsNativeResultAckThenOp14WithoutLowLevelNotice(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	result := handleForgingRequest(t, repos, testForgingResolver(10000, 3), separateUpgradeRequestBodyForTest(0, 25, 700, 121, "test"))
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 2 {
		t.Fatalf("result = %+v", result)
	}
	ack := result.UpperResponses[0]
	wantAck := []byte{1, 121, 0, 7, 0, 0, 0, 2, 0, 3, 0, 25, 0}
	if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketUpgradeItemSeparate) || string(ack.Body) != string(wantAck) {
		t.Fatalf("ack = %+v body=%x", ack, ack.Body)
	}
	update := result.UpperResponses[1]
	if update.Classification != 0 || update.MsgID != msgItemListUpdate || update.Body[0] != listTypeMain || binary.LittleEndian.Uint16(update.Body[1:3]) != 2 {
		t.Fatalf("update = %+v body=%x", update, update.Body)
	}
	first := update.Body[3 : 3+currentItemListEntrySize]
	if first[currentForgingRawOffset] != 3 || first[0x62] != 0xA5 || first[0x68] != 0xB6 || first[0x6D] != 0xC7 {
		t.Fatalf("target row = %x", first)
	}
	if len(result.AreaBroadcastResponses) != 0 {
		t.Fatalf("level 3 area broadcasts = %+v, want none", result.AreaBroadcastResponses)
	}
}

func TestHandlerForgingRejectedRequestReturnsNativeErrorBody(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	result := handleForgingRequest(t, repos, nil, []byte{1, 2, 3})
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 || string(result.UpperResponses[0].Body) != string([]byte{0, 4}) {
		t.Fatalf("result = %+v", result)
	}
}

func TestHandlerForgingBroadcastStartsWhenLevelFiveSucceedsToSix(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	target := record.Slots[slotKey(listTypeMain, 25)]
	target.RawEntry[currentForgingRawOffset] = 5
	target.RawEntry[0x0A] = 13
	target.Extra["forging_level"] = "5"
	target.Extra["reinforce"] = "13"
	record.Slots[slotKey(listTypeMain, 25)] = target
	saveTestInventory(t, ctx, repos, record)

	result := handleForgingRequest(t, repos, testForgingResolver(10000, 6), separateUpgradeRequestBodyForTest(0, 25, 700, 121, "test"))
	if len(result.UpperResponses) != 3 || len(result.AreaBroadcastResponses) != 1 {
		t.Fatalf("responses local=%d area=%d, want result + update + local notice and one area notice", len(result.UpperResponses), len(result.AreaBroadcastResponses))
	}
	wantAck := []byte{1, 121, 0, 7, 0, 0, 0, 5, 0, 6, 0, 25, 0}
	if string(result.UpperResponses[0].Body) != string(wantAck) {
		t.Fatalf("result ack body=%x, want %x", result.UpperResponses[0].Body, wantAck)
	}
	notice := result.UpperResponses[2]
	want := []byte{14, 1, 77, 0, 0xBC, 0x02, 0, 0, 13, 6}
	if notice.Classification != 0 || notice.MsgID != msgUpgradeNotice || string(notice.Body) != string(want) {
		t.Fatalf("notice = class=%d msg=%d body=%x, want %x", notice.Classification, notice.MsgID, notice.Body, want)
	}
	if area := result.AreaBroadcastResponses[0]; area.MsgID != msgUpgradeNotice || string(area.Body) != string(want) {
		t.Fatalf("area notice = class=%d msg=%d body=%x, want %x", area.Classification, area.MsgID, area.Body, want)
	}
}

func TestForgingBroadcastThreshold(t *testing.T) {
	if forgingBroadcastRequired(5) || !forgingBroadcastRequired(6) {
		t.Fatal("forging broadcast must start at resulting level 6")
	}
}

func TestHandlerForgingFailureAtLevelFiveDoesNotBroadcast(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	target := record.Slots[slotKey(listTypeMain, 25)]
	target.RawEntry[currentForgingRawOffset] = 5
	target.Extra["forging_level"] = "5"
	record.Slots[slotKey(listTypeMain, 25)] = target
	saveTestInventory(t, ctx, repos, record)

	result := handleForgingRequest(t, repos, testForgingResolver(0, 6), separateUpgradeRequestBodyForTest(0, 25, 700, 121, "test"))
	if len(result.UpperResponses) != 2 || len(result.AreaBroadcastResponses) != 0 {
		t.Fatalf("responses local=%d area=%d, want result dialog and update without chat notice", len(result.UpperResponses), len(result.AreaBroadcastResponses))
	}
	wantAck := []byte{1, 121, 0, 7, 0, 0, 0, 5, 1, 5, 0, 25, 0}
	if string(result.UpperResponses[0].Body) != string(wantAck) {
		t.Fatalf("failure result ack body=%x, want %x", result.UpperResponses[0].Body, wantAck)
	}
}

func TestHandlerForgingFailurePreservesRawLevelWithoutExtra(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	result := handleForgingRequest(t, repos, testForgingResolver(0, 3), separateUpgradeRequestBodyForTest(0, 25, 700, 121, "test"))
	if len(result.UpperResponses) != 2 {
		t.Fatalf("response count = %d, want result dialog + update without low-level chat notice", len(result.UpperResponses))
	}
	row := result.UpperResponses[1].Body[3 : 3+currentItemListEntrySize]
	if row[currentForgingRawOffset] != 2 || row[0x6D] != 0xC7 {
		t.Fatalf("failed forging row level=%d opaque6d=%02x", row[currentForgingRawOffset], row[0x6D])
	}
}

func TestHandlerForgingFailureAtLevelSixBroadcasts(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	target := record.Slots[slotKey(listTypeMain, 25)]
	target.RawEntry[currentForgingRawOffset] = 6
	target.Extra["forging_level"] = "6"
	record.Slots[slotKey(listTypeMain, 25)] = target
	saveTestInventory(t, ctx, repos, record)

	result := handleForgingRequest(t, repos, testForgingResolver(0, 7), separateUpgradeRequestBodyForTest(0, 25, 700, 121, "test"))
	if len(result.UpperResponses) != 3 || len(result.AreaBroadcastResponses) != 1 {
		t.Fatalf("responses local=%d area=%d, want local and area failure notices", len(result.UpperResponses), len(result.AreaBroadcastResponses))
	}
	wantAck := []byte{1, 121, 0, 7, 0, 0, 0, 6, 1, 6, 0, 25, 0}
	if string(result.UpperResponses[0].Body) != string(wantAck) {
		t.Fatalf("failure result ack body=%x, want %x", result.UpperResponses[0].Body, wantAck)
	}
	notice := result.UpperResponses[2]
	want := []byte{14, 0, 77, 0, 0xBC, 0x02, 0, 0, 0, 6}
	if notice.Classification != 0 || notice.MsgID != msgUpgradeNotice || string(notice.Body) != string(want) {
		t.Fatalf("notice = class=%d msg=%d body=%x, want %x", notice.Classification, notice.MsgID, notice.Body, want)
	}
}
