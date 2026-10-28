package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// newDungeonCreatureAppearanceTest builds the smallest session that can carry a
// worn-creature move: one character, one worn row and an inventory. The dungeon
// runtime is a bare marker because the creature appearance path only asks
// whether a dungeon scene owns the local actor - it never reads run state.
func newDungeonCreatureAppearanceTest(t *testing.T, inDungeon bool) (*Service, *gameSession, *bufferConn) {
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
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     map[string]dnfrepo.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
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
		connID:                          "aligned-dungeon-creature-appearance",
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		initialTownRouteCharacterID:     19,
		initialTownRouteStage:           currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
	}
	if inDungeon {
		session.postStartMapPlayerStateSent = true
		session.dungeon.mu.Lock()
		session.dungeon.runtime = &runtimeDungeonState{}
		session.dungeon.mu.Unlock()
	}
	return service, session, connection
}

func dungeonCreatureMoveResult() alignedcmd.Result {
	return alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          19,
			Body:           []byte{1, 0, 9, 0, 1, 0, 0, 0, 3, 26, 0},
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
		PostActions: []alignedcmd.PostAction{
			alignedcmd.PostActionRefreshSelectedItemContainers,
			alignedcmd.PostActionRefreshSelectedCreatureState,
			alignedcmd.PostActionRefreshSelectedActorAppearance,
		},
	}
}

// TestTownCreatureMoveKeepsTownOnlyAppearancePath proves the dungeon branch is
// scoped: outside a dungeon the same action list keeps the previous behaviour,
// which defers the refresh while the town actor generation is not ready.
func TestTownCreatureMoveKeepsTownOnlyAppearancePath(t *testing.T) {
	service, session, connection := newDungeonCreatureAppearanceTest(t, false)
	// The fixture has no finalized town actor generation, which is exactly the
	// state the town path already refuses to refresh.
	session.townSceneReadyCharacterID = 0

	if err := service.sendAlignedUpperResponses(session, dungeonCreatureMoveResult()); err != nil {
		t.Fatalf("town creature move: %v", err)
	}
	if _, _, found := firstMode0WithOwnerContext(t, connection.write.Bytes()); found {
		t.Fatalf("town creature move unexpectedly wrote a mode0 appearance refresh: %x", connection.write.Bytes())
	}
}

// firstMode0WithOwnerContext returns the first class-0 op2 scene-object (mode0)
// packet in the stream together with its owner-context byte.
//
// The upper writer prefixes a raw class-0 body with three zero bytes (the same
// prefix sendAlignedUpperResponses asserts for its ACK bodies), so the mode0
// layout - mode byte, uint16 version 1, route, owner channel - sits at wire
// offsets 3..7 and the owner is body[7].
func firstMode0WithOwnerContext(t *testing.T, data []byte) (dnfproto.ChannelPacket, byte, bool) {
	t.Helper()
	const upperRawBodyPrefix = 3
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		data = rest
		if packet.Header.Classification != 0 ||
			packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) {
			continue
		}
		if len(packet.Body) < upperRawBodyPrefix+5 {
			continue
		}
		body := packet.Body[upperRawBodyPrefix:]
		if body[0] != 0 || binary.LittleEndian.Uint16(body[1:3]) != 1 {
			continue
		}
		return packet, body[4], true
	}
	return dnfproto.ChannelPacket{}, 0, false
}
