package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestHandleUpperReturnSelectCharacterResetsThenSendsAck(t *testing.T) {
	service := &Service{
		options:       options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameSessions:  make(map[uint16]*gameSession),
		onlinePlayers: newOnlinePlayerManager(),
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                                 connection,
		connID:                               "game-return-select-test",
		channel:                              channelcatalog.Channel{ID: 19},
		selectedCharacterID:                  19,
		enterSelectDungeonSent:               true,
		enterSelectDungeonAckSent:            true,
		enterSelectDungeonContextSent:        true,
		sceneBootstrapTailDeferred:           true,
		sceneBootstrapTailSent:               true,
		runtimeAfterBlacklistSent:            true,
		runtimeFinishLoadingGateSent:         true,
		fpsFinishLoadingGateSent:             true,
		selectedUserInfoRefreshSent:          true,
		selectedUserInfoMode3Sent:            true,
		currentSceneObjectListSent:           true,
		selectedItemListRefreshSent:          true,
		selectedItemListBootstrapCharacterID: 19,
		selectedEquipmentUpdateSent:          true,
		selectPreviewObjectStateSent:         true,
		selectPreviewActorRemoved:            true,
		preDungeonContextPlayerStateSent:     true,
		postStartMapPlayerStateSent:          true,
		currentFinishLoadingStateSent:        true,
		currentFinishLoadingCompletionSent:   true,
		postFinishLoadingPlayerStateSent:     true,
		initialTownRouteCharacterID:          19,
		initialTownRouteStage:                currentInitialTownRoutePlayerStateSent,
		townSceneReadyCharacterID:            19,
		townPositionSnapshot: currentTownPositionSnapshot{
			CharacterID:   19,
			TownID:        38,
			AreaID:        0,
			PositionX:     900,
			PositionY:     250,
			PositionValid: true,
		},
		townSelectorOriginSnapshot: currentTownPositionSnapshot{
			CharacterID:   19,
			TownID:        38,
			AreaID:        0,
			PositionX:     900,
			PositionY:     250,
			PositionValid: true,
		},
		townSelectorOriginBound: true,
		// 预置成"本会话已经补发过药剂 buff 图标、并且还挂着到期定时器"的状态，
		// 用来验证 op7（回选人）会两个都复位。
		statChangePotionBuffProjectionSent: true,
		statChangePotionTimerActive:        true,
		statChangePotionTimerName:          "dnf-stat-change-potion:test:char:19:gen:1",
		statChangePotionTimerCharacterID:   19,
	}
	service.gameSessions[19] = session
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 19,
		TownID:      38,
		AreaID:      0,
		Session:     session,
	})
	// These owners are intentionally detached from worldmap runtime. Returning
	// to character select must still retire them.
	session.dungeon.runToken = 77
	session.dungeon.debugLightningPending = true
	session.dungeon.deathTower = newDeathTowerRuntime([]int{30001}, 60)

	if err := service.handleUpperReturnSelectCharacter(session, nil); err != nil {
		t.Fatalf("handle return select: %v", err)
	}
	if session.selectedCharacterID != 0 {
		t.Fatalf("selected character = %d, want 0", session.selectedCharacterID)
	}
	// 城镇增益药剂（史诗Buff药剂一族）的两件事必须在回选人时复位，否则"小退再上"时
	// 恢复补发会被"只补发一次"的标记跳过 —— 客户端回选人已经把 buff 栏清空，图标就没了
	//（现场 2026-10-03 服主报："史诗buff药剂 小退再上 图标就没了"）。
	if session.statChangePotionBuffProjectionSent {
		t.Fatal("return-select kept the stat-change potion projection marker, so a re-entry would skip the icon restore")
	}
	if session.statChangePotionTimerActive || session.statChangePotionTimerName != "" ||
		session.statChangePotionTimerCharacterID != 0 {
		t.Fatalf("return-select kept the stat-change potion expiry timer: active=%t name=%q char=%d",
			session.statChangePotionTimerActive, session.statChangePotionTimerName,
			session.statChangePotionTimerCharacterID)
	}
	if _, ok := service.gameSessions[19]; ok {
		t.Fatal("old character remains in online-session index")
	}
	if _, ok := service.onlinePlayers.PlayerForCharacter(19); ok {
		t.Fatal("old character remains in town online index")
	}
	if !returnSelectTownReentryPending(session) {
		t.Fatal("successful op7 did not retain the selector re-entry owner marker")
	}
	session.townMu.Lock()
	accountSharedRefreshPending := session.accountSharedInventoryRebindPending
	session.townMu.Unlock()
	if !accountSharedRefreshPending {
		t.Fatal("successful op7 did not retain the account-shared inventory UI refresh marker")
	}
	if !sameCharacterReturnSelect(session, 19) || sameCharacterReturnSelect(session, 20) {
		t.Fatal("return-select previous-character marker does not distinguish same and different character selection")
	}
	if returnSelectSceneStateRetained(session) {
		t.Fatalf("return-select retained scene state: %+v", session)
	}
	if session.dungeon.runToken != 0 || session.dungeon.debugLightningPending || session.dungeon.deathTower != nil {
		t.Fatalf("return-select retained detached dungeon owners token=%d debug=%t death_tower=%p",
			session.dungeon.runToken, session.dungeon.debugLightningPending, session.dungeon.deathTower)
	}

	packets := splitServerUpperPacketsForReturnSelectTest(t, connection.write.Bytes())
	if len(packets) != 2 {
		t.Fatalf("packet count = %d, want 2 (op6 then op7)", len(packets))
	}
	if packets[0][0] != 0 || binary.LittleEndian.Uint16(packets[0][1:3]) != currentTownUserLeaveMsgID ||
		!bytes.Equal(packets[0][16:], buildTownUserLeaveBody(19)) {
		t.Fatalf("first packet = %x, want class0/op6 for old character", packets[0])
	}
	if packets[1][0] != 1 || binary.LittleEndian.Uint16(packets[1][1:3]) != uint16(dnfenum.CmdPacketReturnSelectCharacter) ||
		!bytes.Equal(packets[1][16:], nil) {
		t.Fatalf("second packet = %x, want bodyless class1/op7 ACK", packets[1])
	}
}

func TestHandleUpperReturnSelectCharacterRejectsMalformedBody(t *testing.T) {
	service := &Service{}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 19, sceneBootstrapTailSent: true}
	if err := service.handleUpperReturnSelectCharacter(session, []byte{0}); err != nil {
		t.Fatalf("handle return select: %v", err)
	}
	if connection.write.Len() != 0 || session.selectedCharacterID != 19 || !session.sceneBootstrapTailSent {
		t.Fatalf("rejected request mutated state: bytes=%d session=%+v", connection.write.Len(), session)
	}
}

func TestHandleGameUpperRoutesOnlyClass1ReturnSelectCharacter(t *testing.T) {
	for _, test := range []struct {
		name               string
		classification     byte
		wantPackets        int
		wantSelectedCharID uint16
	}{
		{name: "class1 command", classification: 1, wantPackets: 2, wantSelectedCharID: 0},
		{name: "class0 notification", classification: 0, wantPackets: 0, wantSelectedCharID: 19},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &bufferConn{}
			service := &Service{
				options:      options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
				gameSessions: make(map[uint16]*gameSession),
			}
			session := &gameSession{conn: connection, connID: "game-return-select-route-test", selectedCharacterID: 19}
			service.gameSessions[19] = session
			request := make([]byte, 13)
			request[0] = test.classification
			binary.LittleEndian.PutUint16(request[1:3], uint16(dnfenum.CmdPacketReturnSelectCharacter))
			binary.LittleEndian.PutUint32(request[3:7], uint32(len(request)))

			if err := service.handleGameUpper(session, request); err != nil {
				t.Fatalf("handle game upper: %v", err)
			}
			if session.selectedCharacterID != test.wantSelectedCharID {
				t.Fatalf("selected character = %d, want %d", session.selectedCharacterID, test.wantSelectedCharID)
			}
			packets := splitServerUpperPacketsForReturnSelectTest(t, connection.write.Bytes())
			if len(packets) != test.wantPackets {
				t.Fatalf("packet count = %d, want %d", len(packets), test.wantPackets)
			}
		})
	}
}

func returnSelectSceneStateRetained(session *gameSession) bool {
	return session.enterSelectDungeonSent || session.enterSelectDungeonAckSent || session.enterSelectDungeonContextSent ||
		session.sceneBootstrapTailDeferred || session.sceneBootstrapTailSent || session.runtimeAfterBlacklistSent ||
		session.runtimeFinishLoadingGateSent || session.fpsFinishLoadingGateSent || session.selectedUserInfoRefreshSent ||
		session.selectedUserInfoMode3Sent || session.currentSceneObjectListSent ||
		session.selectedItemListRefreshSent || session.selectedItemListBootstrapCharacterID != 0 ||
		session.selectedEquipmentUpdateSent || session.selectPreviewObjectStateSent ||
		session.selectPreviewActorRemoved || session.preDungeonContextPlayerStateSent || session.postStartMapPlayerStateSent ||
		session.currentFinishLoadingStateSent || session.currentFinishLoadingCompletionSent ||
		session.postFinishLoadingPlayerStateSent ||
		session.initialTownRouteCharacterID != 0 || session.initialTownRouteStage != currentInitialTownRouteIdle ||
		session.initialTownQuestSnapshotsSent ||
		session.townSceneReadyCharacterID != 0 || session.townSelectorOriginBound ||
		session.townSelectorOriginSnapshot.CharacterID != 0
}

func splitServerUpperPacketsForReturnSelectTest(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var packets [][]byte
	for len(stream) != 0 {
		if len(stream) < 16 {
			t.Fatalf("short upper header: %d bytes", len(stream))
		}
		packetLen := int(binary.LittleEndian.Uint32(stream[3:7]))
		if packetLen < 16 || packetLen > len(stream) {
			t.Fatalf("invalid upper packet length %d in %d-byte stream", packetLen, len(stream))
		}
		packets = append(packets, append([]byte(nil), stream[:packetLen]...))
		stream = stream[packetLen:]
	}
	return packets
}
