package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestTrainingRoomDirectEntryReplaysBuffRegistrationAfterFinishLoading(t *testing.T) {
	service, _ := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("training-room fixture repositories unavailable")
	}
	const (
		chronicleItemID uint32 = 116010135
		chronicleSerial uint32 = 20
	)
	chronicleRaw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(chronicleRaw[0:2], 17)
	binary.LittleEndian.PutUint32(chronicleRaw[2:6], chronicleItemID)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: int64(chronicleItemID), Count: 1, RawEntry: chronicleRaw, Extra: map[string]string{"item_serial": "20"}},
		},
	}); err != nil {
		t.Fatalf("save registered Chronicle inventory: %v", err)
	}
	if err := saveSkillSwitchInventoryState(context.Background(), repositories.Settings, "99", skillSwitchInventoryState{
		SkillID: 40,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: chronicleSerial, ItemID: chronicleItemID}},
	}); err != nil {
		t.Fatalf("save training-room BUFF registration: %v", err)
	}

	newSession := func() (*bufferConn, *gameSession) {
		connection := &bufferConn{}
		return connection, &gameSession{
			conn:                connection,
			channel:             channelcatalog.Channel{ID: 16},
			residentChannel:     channelcatalog.Channel{ID: 16},
			selectedCharacterID: 99,
		}
	}

	// Without the direct-entry arm nothing is replayed: every other route owns
	// its own op559 and must not get a second one here.
	idleConnection, idle := newSession()
	if err := service.sendCurrentTrainingRoomBuffRegistration(idle, "test_not_training_room"); err != nil {
		t.Fatal(err)
	}
	if idleConnection.write.Len() != 0 {
		t.Fatalf("unarmed session sent %d bytes", idleConnection.write.Len())
	}

	connection, session := newSession()
	session.trainingRoomBuffRegistrationPending = true
	if err := service.sendCurrentTrainingRoomBuffRegistration(session, "test_training_room"); err != nil {
		t.Fatal(err)
	}
	if session.trainingRoomBuffRegistrationPending {
		t.Fatal("training-room BUFF replay stayed armed after sending")
	}
	found := false
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == skillSwitchInventoryAllDataWire {
			found = true
			if len(packet.Body) < 3 || packet.Body[0] != 40 || packet.Body[2] != 1 {
				t.Fatalf("training-room BUFF registration body=%x", packet.Body)
			}
		}
		stream = rest
	}
	if !found {
		t.Fatal("training-room finish loading did not replay the BUFF registration")
	}

	// A second finish loading in the same scene must not send a duplicate.
	duplicateConnection := &bufferConn{}
	session.conn = duplicateConnection
	if err := service.sendCurrentTrainingRoomBuffRegistration(session, "test_training_room_again"); err != nil {
		t.Fatal(err)
	}
	if duplicateConnection.write.Len() != 0 {
		t.Fatalf("second finish loading resent the BUFF registration: %d bytes", duplicateConnection.write.Len())
	}

	// And the hook is really reached from the finish-loading player state, not
	// only callable in isolation - that is the whole point of deferring it.
	hookConnection, hookSession := newSession()
	// The direct-entry projection already installed the dungeon skill manager,
	// so finish loading only publishes state - same as the live route.
	hookSession.dungeonSceneSkillInfoSent = true
	hookSession.trainingRoomBuffRegistrationPending = true
	if err := service.sendCurrentPostFinishLoadingPlayerState(hookSession, "test_training_room_finish_loading"); err != nil {
		t.Fatal(err)
	}
	if hookSession.trainingRoomBuffRegistrationPending {
		t.Fatal("finish-loading player state did not consume the training-room BUFF replay")
	}
	hookFound := false
	stream = hookConnection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == skillSwitchInventoryAllDataWire {
			hookFound = true
		}
		stream = rest
	}
	if !hookFound {
		t.Fatal("finish-loading player state did not replay the training-room BUFF registration")
	}
}
