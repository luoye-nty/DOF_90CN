package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// newAlignedItemLockService builds a session whose durable inventory holds one
// locked main-list row and one locked personal-cargo row, which is the state the
// op251 snapshot has to republish after a container rebuild.
func newAlignedItemLockService(t *testing.T, connID string) (*Service, *gameSession, *bufferConn) {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "Actor19",
		Job:         "15",
		Level:       1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:120": {ItemID: 1001, Count: 1, Extra: map[string]string{"equipment_lock_state": "1"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			"2:4": {ItemID: 2001, Count: 1, Extra: map[string]string{"equipment_lock_state": "locked"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
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
		conn:                            connection,
		connID:                          connID,
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		initialTownRouteCharacterID:     19,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
	}
	return service, session, connection
}

// wantAlignedItemLockSnapshotBody mirrors itemlock.BuildLockListSnapshot for the
// fixture above: [u16 count] then [u8 listType][i16 slot][u8 state] per row.
var wantAlignedItemLockSnapshotBody = []byte{2, 0, 0, 120, 0, 1, 2, 4, 0, 1}

func assertAlignedItemLockSnapshotPacket(t *testing.T, packet dnfproto.ChannelPacket) {
	t.Helper()
	if packet.Header.MsgID != dnfitemlock.LockListMessageID || packet.Header.Classification != 0 {
		t.Fatalf("lock snapshot header=%+v body=%x", packet.Header, packet.Body)
	}
	if !bytes.Equal(packet.Body, wantAlignedItemLockSnapshotBody) {
		t.Fatalf("lock snapshot body=%x want=%x", packet.Body, wantAlignedItemLockSnapshotBody)
	}
}

// A container rebuild destroys and recreates the client's item rows. The select
// bootstrap already answers its op13 lists with an op251 snapshot; the aligned
// post action has to keep that pairing or an equipped-then-unequipped item comes
// back without its lock icon.
func TestSendAlignedContainerRefreshPostActionAppendsDurableItemLockSnapshot(t *testing.T) {
	service, session, connection := newAlignedItemLockService(t, "aligned-container-lock-snapshot")

	ackBody := []byte{1, 0, 9, 0, 1, 0, 0, 0, 3, 12, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedItemContainers,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	_, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	for _, wantListType := range currentSelectedItemListTypes {
		refresh, remaining := splitCurrentSceneItemListPacket(t, rest)
		if len(refresh.Body) == 0 || refresh.Body[0] != wantListType {
			t.Fatalf("list%d refresh header=%+v body=%x", wantListType, refresh.Header, refresh.Body)
		}
		rest = remaining
	}
	snapshot, trailing := splitLongHengGameServerUpperPacket(t, rest)
	assertAlignedItemLockSnapshotPacket(t, snapshot)
	if len(trailing) != 0 {
		t.Fatalf("container refresh appended packets after the lock snapshot: %x", trailing)
	}
}

// The single-container rebuild (personal-cargo sort) drops the same slot-keyed
// lock model, so it republishes the durable snapshot after its op13 list.
func TestSendAlignedPersonalCargoRefreshPostActionAppendsDurableItemLockSnapshot(t *testing.T) {
	service, session, connection := newAlignedItemLockService(t, "aligned-cargo-lock-snapshot")

	ackBody := []byte{1, 2, 0, 0, 0, 8, 0, 16, 0}
	result := alignedcmd.Result{
		Operation: "sort_item",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          20,
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedPersonalCargo,
		},
	}

	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatalf("sendAlignedUpperResponses error = %v", err)
	}
	_, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	cargo, rest := splitCurrentSceneItemListPacket(t, rest)
	if len(cargo.Body) == 0 || cargo.Body[0] != currentEquipmentPlacementListPersonalCargo {
		t.Fatalf("cargo refresh header=%+v body=%x", cargo.Header, cargo.Body)
	}
	snapshot, trailing := splitLongHengGameServerUpperPacket(t, rest)
	assertAlignedItemLockSnapshotPacket(t, snapshot)
	if len(trailing) != 0 {
		t.Fatalf("cargo refresh appended packets after the lock snapshot: %x", trailing)
	}
}
