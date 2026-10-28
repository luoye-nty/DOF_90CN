package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestLegacyQuickSlotSecondCharmFailureAckKeepsSessionUsable(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4":  {ItemID: 400360003, Count: 1},
			"0:9":  {ItemID: 400360005, Count: 1},
			"0:20": {ItemID: 3244, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":                         "",
		"equipment/equipment.lst":                     "400360003 `character/common/charm/test_a.equ`\n400360005 `character/common/charm/test_b.equ`\n",
		"equipment/character/common/charm/test_a.equ": "[name] `test charm a`\n[equipment type] `[charm]`\n",
		"equipment/character/common/charm/test_b.equ": "[name] `test charm b`\n[equipment type] `[charm]`\n",
		"stackable/stackable.lst":                     "3244 `material/test.stk`\n",
		"stackable/material/test.stk":                 "[name] `test material`\n[stackable type] `[material]`\n[stack limit] 100\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:quickslot-charm-wire",
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog:     catalog,
	}
	session := &gameSession{conn: connection, connID: "quickslot-charm-wire", selectedCharacterID: 77}

	failureFrame := buildLegacyGamePacketForBridgeTest(
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketMoveItemspace),
		1,
		buildOnlineItemTradeMoveRequest(0, 9, 0, 1, 0, 3),
	)
	if err := service.handleLegacyGamePacket(session, failureFrame); err != nil {
		t.Fatalf("handle rejected charm move: %v", err)
	}
	ack, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantFailure := []byte{0, 17, 0, 0}
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		!bytes.Equal(ack.Body, wantFailure) || len(trailing) != 0 {
		t.Fatalf("failure ACK header=%+v body=% X trailing=% X, want % X", ack.Header, ack.Body, trailing, wantFailure)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, exists := stored.Slots["0:3"]; exists || stored.Slots["0:4"].ItemID != 400360003 || stored.Slots["0:9"].ItemID != 400360005 {
		t.Fatalf("rejected move changed inventory: %+v", stored.Slots)
	}

	// A native failure must release the pending op19 objects. Prove the same
	// session can immediately complete an unrelated legal move.
	connection.write.Reset()
	successFrame := buildLegacyGamePacketForBridgeTest(
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketMoveItemspace),
		2,
		buildOnlineItemTradeMoveRequest(0, 20, 0, 1, 0, 21),
	)
	if err := service.handleLegacyGamePacket(session, successFrame); err != nil {
		t.Fatalf("handle legal move after rejection: %v", err)
	}
	ack, trailing = splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		len(ack.Body) != 11 || ack.Body[0] != 1 || len(trailing) != 0 {
		t.Fatalf("success after rejection header=%+v body=% X trailing=% X", ack.Header, ack.Body, trailing)
	}
	stored, _, _ = repositories.Inventory.Load(ctx, "77")
	if _, exists := stored.Slots["0:20"]; exists || stored.Slots["0:21"].ItemID != 3244 {
		t.Fatalf("legal follow-up move was not committed: %+v", stored.Slots)
	}
}

func TestLegacyQuickSlotMoveFailsClosedWhenPVFCatalogIsUnavailable(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":  {ItemID: 400360005, Count: 1},
			"0:20": {ItemID: 3244, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:quickslot-catalog-failure",
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider:    func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalogLoadErr: errors.New("test PVF catalog unavailable"),
	}
	session := &gameSession{conn: connection, connID: "quickslot-catalog-failure", selectedCharacterID: 77}

	frame := buildLegacyGamePacketForBridgeTest(
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketMoveItemspace),
		1,
		buildOnlineItemTradeMoveRequest(0, 9, 0, 1, 0, 3),
	)
	if err := service.handleLegacyGamePacket(session, frame); err != nil {
		t.Fatalf("handle quick-slot move with missing catalog: %v", err)
	}
	ack, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if !bytes.Equal(ack.Body, []byte{0, 17, 0, 0}) || len(trailing) != 0 {
		t.Fatalf("catalog failure ACK header=%+v body=% X trailing=% X", ack.Header, ack.Body, trailing)
	}
	stored, _, _ := repositories.Inventory.Load(ctx, "77")
	if _, exists := stored.Slots["0:3"]; exists || stored.Slots["0:9"].ItemID != 400360005 {
		t.Fatalf("catalog failure changed quick-slot move: %+v", stored.Slots)
	}

	// The fail-closed policy is scoped to quick-slot endpoints; ordinary op19
	// remains available even while the PVF catalog is unavailable.
	connection.write.Reset()
	frame = buildLegacyGamePacketForBridgeTest(
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketMoveItemspace),
		2,
		buildOnlineItemTradeMoveRequest(0, 20, 0, 1, 0, 21),
	)
	if err := service.handleLegacyGamePacket(session, frame); err != nil {
		t.Fatalf("handle ordinary move with missing catalog: %v", err)
	}
	ack, trailing = splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || len(ack.Body) != 11 || ack.Body[0] != 1 || len(trailing) != 0 {
		t.Fatalf("ordinary move ACK header=%+v body=% X trailing=% X", ack.Header, ack.Body, trailing)
	}
	stored, _, _ = repositories.Inventory.Load(ctx, "77")
	if _, exists := stored.Slots["0:20"]; exists || stored.Slots["0:21"].ItemID != 3244 {
		t.Fatalf("ordinary move was blocked by catalog failure: %+v", stored.Slots)
	}
}
