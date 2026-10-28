package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnftown "longheng.io/server/internal/modules/dnf/town"
)

// A character last saved in CentralPark's dungeon-gate area logs into the
// Seria room. Its exit must land on the PVF door of CentralPark's main area,
// not on the unrelated pre-login gate-area coordinates.
func TestCharacterListLoginSeriaExitUsesPVFDoorInsteadOfPreSelectArea(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst": "31 `CentralPark.twn` 38 `new_Elvengard.twn`",
		"town/CentralPark.twn": "[area]\n1 `Town/CentralPark/CentralPark.map` `[normal]`\n[/area]\n" +
			"[area]\n2 `Town/CentralPark/CentralPark_Waiting.map` `[dungeon gate]` 39\n[/area]\n[name]\n`CentralPark`",
		"town/new_Elvengard.twn": "[area]\n1 `new_seria_room.map` `[gate]` 450 234\n[/area]\n[name]\n`Elvengard`",
		"map/Town/CentralPark/CentralPark.map": "[town movable area with animation]\n" +
			"[movable area]\n[area]\n1841 172 100 30\n[move town]\n38 1\n[/movable area]\n" +
			"[/town movable area with animation]\n[virtual movable area]\n30 220 2420 220\n",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service.townCatalog = table
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 31
	character.Stats["area_id"] = 2
	character.Stats["pos_x"] = 856
	character.Stats["pos_y"] = 341
	character.Stats["direction"] = 8
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	_, townID, areaID, row, mapPath, err := service.currentCharacterListLoginTransition(context.Background(), session, 29, character)
	if err != nil {
		t.Fatal(err)
	}
	if townID != newCharacterInitialTownID || areaID != newCharacterInitialAreaID ||
		row.Value1 != newCharacterInitialPosX || row.Value2 != newCharacterInitialPosY ||
		mapPath != "new_seria_room.map" {
		t.Fatalf("Seria login town=%d area=%d row=%+v map=%q", townID, areaID, row, mapPath)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found ||
		stored.Stats[currentTownPrevVillageTownStat] != 31 ||
		stored.Stats[currentTownPrevVillageAreaStat] != 1 ||
		stored.Stats[currentTownPrevVillagePosXStat] != 1891 ||
		stored.Stats[currentTownPrevVillagePosYStat] != 234 ||
		stored.Stats[currentTownPrevVillageDirectionStat] != newCharacterInitialDirection {
		t.Fatalf("login did not project the previous village to the PVF Seria door found=%t err=%v stats=%+v", found, err, stored.Stats)
	}

	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketPrevVillage), nil); err != nil {
		t.Fatal(err)
	}
	wantBody, err := buildCurrentSceneTransitionBody(31, 1, []currentSceneTransitionRow{{
		ObjectOrResourceKey: currentSceneActorObjectKey(29),
		Value1:              1891,
		Value2:              234,
		Value3:              newCharacterInitialDirection,
		Value4:              newCharacterInitialAreaState,
	}})
	if err != nil {
		t.Fatal(err)
	}
	foundTransition := false
	for _, packet := range splitAllFinishBridgePackets(t, connection.write.Bytes()) {
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSceneTransitionMsgID {
			foundTransition = bytes.Equal(packet.Body, wantBody)
			if !foundTransition {
				t.Fatalf("PVF Seria-door return op24 body=%x want=%x", packet.Body, wantBody)
			}
		}
	}
	if !foundTransition {
		t.Fatalf("op1425 did not emit the same-town op24 to the PVF door; wrote %d bytes", connection.write.Len())
	}
	storedTown, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || storedTown.Stats["town_id"] != 31 || storedTown.Stats["area_id"] != 1 ||
		storedTown.Stats["pos_x"] != 1891 || storedTown.Stats["pos_y"] != 234 {
		t.Fatalf("PVF Seria-door return location found=%t err=%v stats=%+v", found, err, storedTown.Stats)
	}
}

// Towns whose map declares no Seria portal keep the origin exactly as bound.
func TestCurrentTownPrevVillageDoorOriginLeavesTownsWithoutPortalUntouched(t *testing.T) {
	service := &Service{}
	origin := currentTownPositionSnapshot{CharacterID: 29, TownID: 7, AreaID: 3, PositionX: 100, PositionY: 200, MovementCode: 5, PositionValid: true}
	projected, mapPath, ok := service.currentTownPrevVillageDoorOrigin(origin)
	if ok || mapPath != "" || projected != origin {
		t.Fatalf("no-catalog projection=(%+v,%q,%t)", projected, mapPath, ok)
	}
}
