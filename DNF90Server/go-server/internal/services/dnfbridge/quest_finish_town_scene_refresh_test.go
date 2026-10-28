package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftown "longheng.io/server/internal/modules/dnf/town"
)

func newQuestGatedTownSceneRefreshService(t *testing.T) (*Service, *bufferConn, *gameSession) {
	t.Helper()
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":           "39 `new_HendonMyre.twn`",
		"town/new_HendonMyre.twn": "[area]\n0 `Cataclysm/Town/HendonMyre/new_hendon_main.map` `[normal]`\n[/area]\n[name]\n`HendonMyre`",
		"map/Cataclysm/Town/HendonMyre/new_hendon_main.map": "[quest condition]\n3186\n[/quest condition]\n" +
			"[quest condition]\n10100\n[/quest condition]\n",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:     options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		townCatalog: table,
	}
	session := &gameSession{
		conn:                      connection,
		connID:                    "quest-gated-town-scene-refresh",
		selectedCharacterID:       19,
		townSceneReadyCharacterID: 19,
		townPositionSnapshot: currentTownPositionSnapshot{
			CharacterID:   19,
			TownID:        39,
			AreaID:        0,
			PositionX:     620,
			PositionY:     280,
			MovementCode:  5,
			PositionValid: true,
		},
	}
	return service, connection, session
}

func questGatedTownSceneRefreshResult(questID int64) dnfquest.FinishCommitResult {
	return dnfquest.FinishCommitResult{
		AtomicCommitted: true,
		CharacterID:     "19",
		QuestID:         questID,
		PostCommitCharacter: dnfrepo.CharacterRecord{
			CharacterID: "19",
			Stats: map[string]int64{
				"town_id":    39,
				"area_id":    0,
				"area_state": 3,
			},
		},
		PostCommitQuest: dnfrepo.QuestRecord{
			CharacterID: "19",
			States: map[int64]dnfrepo.QuestState{
				questID: {Status: "completed"},
			},
		},
	}
}

// Finishing a quest that the loaded map names in a [quest condition] block
// must rebuild the same-area scene: op22, op23, the committed op356 seed, then
// a same-area op24 whose final row is the selected actor at its op35 position.
func TestCurrentQuestGatedTownSceneRefreshSeedsCompletedQuestBeforeSameAreaOp24(t *testing.T) {
	service, connection, session := newQuestGatedTownSceneRefreshService(t)
	if err := service.sendCurrentQuestGatedTownSceneRefresh(session, questGatedTownSceneRefreshResult(10100), "test_op34_tail"); err != nil {
		t.Fatal(err)
	}
	packets := splitAllFinishBridgePackets(t, connection.write.Bytes())
	if got := finishBridgePacketIDs(packets); len(got) != 4 ||
		got[0] != "0/22" || got[1] != "0/23" ||
		got[2] != "0/356" || got[3] != "0/24" {
		t.Fatalf("quest-gated scene packet order=%v", got)
	}
	assertFinishBridgeClearQuestProjection(t, packets[2], 10100)
	op24 := packets[3].Body
	if len(op24) != 12 || op24[0] != 39 || op24[1] != 0 ||
		binary.LittleEndian.Uint16(op24[2:4]) != 1 ||
		binary.LittleEndian.Uint16(op24[4:6]) != currentSceneActorObjectKey(19) ||
		binary.LittleEndian.Uint16(op24[6:8]) != 620 ||
		binary.LittleEndian.Uint16(op24[8:10]) != 280 || op24[10] != 5 || op24[11] != 3 {
		t.Fatalf("same-area op24 body=%x", op24)
	}
}

// The op1429 bulk close scans the whole changed-quest set: a [quest condition]
// gate may belong to a non-highest quest in the batch, so the same-area rebuild
// must trigger when ANY changed quest gates the loaded map, not only the
// highest completed one. A batch with no gating quest stays silent.
func TestCurrentQuestGatedTownSceneRefreshScansWholeBulkChangedQuestSet(t *testing.T) {
	service, connection, session := newQuestGatedTownSceneRefreshService(t)
	// Highest completed quest 10101 does not gate the map; the gating 10100 is
	// inside the changed set, so the scene must still rebuild. The committed
	// quest record carries every quest the bulk finished, including 10100.
	result := questGatedTownSceneRefreshResult(10101)
	result.PostCommitQuest.States[10100] = dnfrepo.QuestState{Status: "completed"}
	if err := service.sendCurrentQuestGatedTownSceneRefreshForQuestIDs(
		session, result, []int64{10101, 10100}, "test_op1429_bulk_tail",
	); err != nil {
		t.Fatal(err)
	}
	packets := splitAllFinishBridgePackets(t, connection.write.Bytes())
	if got := finishBridgePacketIDs(packets); len(got) != 4 ||
		got[0] != "0/22" || got[1] != "0/23" ||
		got[2] != "0/356" || got[3] != "0/24" {
		t.Fatalf("bulk gating changed-set packet order=%v", got)
	}
	assertFinishBridgeClearQuestProjection(t, packets[2], 10100)
	assertFinishBridgeClearQuestProjection(t, packets[2], 10101)

	// The client-feature gate (12829) also rebuilds when present in a bulk set,
	// even if the highest completed quest is an ordinary one.
	connection.write.Reset()
	if err := service.sendCurrentQuestGatedTownSceneRefreshForQuestIDs(
		session, result, []int64{10101, currentAntonRaidChannelUnlockQuestID}, "test_op1429_feature_gate",
	); err != nil {
		t.Fatal(err)
	}
	if got := finishBridgePacketIDs(splitAllFinishBridgePackets(t, connection.write.Bytes())); len(got) != 4 || got[3] != "0/24" {
		t.Fatalf("bulk client feature gate packet order=%v", got)
	}

	// A bulk set with no gating quest writes nothing.
	connection.write.Reset()
	if err := service.sendCurrentQuestGatedTownSceneRefreshForQuestIDs(
		session, result, []int64{10101, 10102}, "test_op1429_non_gating_bulk",
	); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("non-gating bulk set wrote %d bytes", connection.write.Len())
	}
}

// Quests the map does not gate on, and the Anton channel-unlock feature gate,
// bound the refresh: the former stays silent, the latter rebuilds even though
// no map declares it.
func TestCurrentQuestGatedTownSceneRefreshOnlyForGatingQuests(t *testing.T) {
	service, connection, session := newQuestGatedTownSceneRefreshService(t)
	if err := service.sendCurrentQuestGatedTownSceneRefresh(session, questGatedTownSceneRefreshResult(10101), "test_non_gate_quest"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("non-gating quest wrote %d bytes", connection.write.Len())
	}
	if err := service.sendCurrentQuestGatedTownSceneRefresh(session, questGatedTownSceneRefreshResult(currentAntonRaidChannelUnlockQuestID), "test_feature_gate"); err != nil {
		t.Fatal(err)
	}
	if got := finishBridgePacketIDs(splitAllFinishBridgePackets(t, connection.write.Bytes())); len(got) != 4 || got[3] != "0/24" {
		t.Fatalf("client feature gate quest packet order=%v", got)
	}

	// Inside a dungeon runtime, or before the town scene is ready, nothing is
	// sent: the scene the client owns is not the town one.
	connection.write.Reset()
	session.townSceneReadyCharacterID = 0
	if err := service.sendCurrentQuestGatedTownSceneRefresh(session, questGatedTownSceneRefreshResult(10100), "test_scene_not_ready"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("scene-not-ready refresh wrote %d bytes", connection.write.Len())
	}
}
