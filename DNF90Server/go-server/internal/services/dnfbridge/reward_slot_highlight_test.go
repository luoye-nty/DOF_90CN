package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentRewardSlotRefreshesFromKeysAreStableAndSorted(t *testing.T) {
	got, err := currentRewardSlotRefreshesFromKeys(map[string]struct{}{
		"7:189": {},
		"0:121": {},
		"1:0":   {},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []alignedcmd.ItemSlotRefresh{
		{ListType: 0, SlotIndex: 121},
		{ListType: 1, SlotIndex: 0},
		{ListType: 7, SlotIndex: 189},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reward slot refreshes=%+v want=%+v", got, want)
	}
	if _, err := currentRewardSlotRefreshesFromKeys(map[string]struct{}{"broken": {}}); err == nil {
		t.Fatal("invalid reward slot key was accepted")
	}
}

func TestCurrentRewardSlotHighlightWaitsThenSendsChangedRowsOneByOne(t *testing.T) {
	repositories := testRepositoryGroup()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 600, Count: 3},
			"1:0":   {ItemID: 700, Count: 1},
			"7:190": {ItemID: 800, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:     queue,
	}
	session := &gameSession{
		conn:                connection,
		connID:              "reward-highlight-test",
		selectedCharacterID: 19,
		characterGeneration: 4,
	}
	service.scheduleCurrentRewardSlotHighlights(session, "test_package", []alignedcmd.ItemSlotRefresh{
		{ListType: 0, SlotIndex: 121},
		{ListType: 1, SlotIndex: 0},
		{ListType: 7, SlotIndex: 190},
	})
	if connection.write.Len() != 0 {
		t.Fatalf("reward highlights were sent before delay: %x", connection.write.Bytes())
	}
	if scheduled, _, active := queue.counts(); scheduled != 2 || active != 2 {
		t.Fatalf("reward highlight schedule=(%d,%d) want=(2,2)", scheduled, active)
	}
	wants := []struct {
		listType byte
		slot     uint16
		itemID   uint32
		count    uint32
	}{
		{listType: 0, slot: 121, itemID: 600, count: 3},
		{listType: 1, slot: 0, itemID: 700, count: 1},
	}
	for index, want := range wants {
		task := queue.task(t, index)
		wantDelay := currentRewardSlotHighlightDelay + time.Duration(index)*currentRewardSlotHighlightStep
		if task.delay != wantDelay {
			t.Fatalf("reward highlight task[%d] delay=%v want=%v", index, task.delay, wantDelay)
		}
		connection.write.Reset()
		queue.fire(task, false)
		packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
		if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
			len(packet.Body) < 13 || packet.Body[0] != want.listType || binary.LittleEndian.Uint16(packet.Body[1:3]) != 1 ||
			binary.LittleEndian.Uint16(packet.Body[3:5]) != want.slot || binary.LittleEndian.Uint32(packet.Body[5:9]) != want.itemID ||
			binary.LittleEndian.Uint32(packet.Body[9:13]) != want.count {
			t.Fatalf("reward highlight[%d] header=%+v body=%x", index, packet.Header, packet.Body)
		}
		if !bytes.Equal(trailing, nil) {
			t.Fatalf("reward highlight[%d] trailing=%x", index, trailing)
		}
	}
}

func TestCurrentRewardSlotHighlightSkipsPetInventoryRows(t *testing.T) {
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{gameplayTimers: queue}
	service.scheduleCurrentRewardSlotHighlights(
		&gameSession{connID: "pet-highlight-skip", selectedCharacterID: 19, characterGeneration: 2},
		"test_pet_package",
		[]alignedcmd.ItemSlotRefresh{{ListType: currentPetInventoryListType, SlotIndex: 190}},
	)
	if scheduled, _, active := queue.counts(); scheduled != 0 || active != 0 {
		t.Fatalf("pet reward highlight schedule=(%d,%d) want=(0,0)", scheduled, active)
	}
}
