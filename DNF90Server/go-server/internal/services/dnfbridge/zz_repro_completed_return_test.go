package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// TestReproAntonGateReturnToTownWithSelectorSlotID pins the design contract the
// live failures violate: once the server ACKs the settlement page's 返回城镇
// (op72 {1,2}), a scene transition MUST reach the client.
//
// Live 2026-09-25T07:10:01.7443Z: char 1 (game-000053, dungeon_id=210, raid
// 1507329) sent op72 {1,2}; the server logged
// game-raid-dungeon-settlement-exit-ready and game-dungeon-card-exit-ack-sent and
// then not one packet for the rest of the session. Char 2 did the same one second
// later with the same result. Same shape at 06:55:10Z, 05:25:22Z, 05:25:23Z and
// 2026-09-24T17:07:49Z; in each case the player had to close the client.
//
// NOTE ON COVERAGE: this fixture is a TUTORIAL dungeon. isPVFTutorialDungeon
// short-circuits prepareCurrentDungeonTownTransitionForSession
// (dungeon_return_town.go:723) before any origin validation, so this test can NOT
// reach the live failing branch -- it only guards the contract. A non-tutorial
// fixture (disableTutorial) in a raid channel with a bound selector origin and a
// real party owner is what is needed to reproduce the incident itself.
func TestReproAntonGateReturnToTownWithSelectorSlotID(t *testing.T) {
	service, runtime, session, connection, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("completed raid skill repository unavailable")
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")
	session.channel.Type = dnfenum.RaidChannelType
	session.residentChannel.Type = dnfenum.RaidChannelType

	// The live shape: UI selector slot id in the runtime, and a frozen return
	// origin that is NOT the Anton waiting area the gate was entered from.
	runtime.Dungeon.ID = 210
	runtime.Request.DungeonID = 210
	runtime.townReturnOrigin = currentTownPositionSnapshot{
		CharacterID:   99,
		TownID:        38,
		AreaID:        1,
		PositionX:     450,
		PositionY:     234,
		PositionValid: true,
	}
	runtime.settlementPlayResultReceived = true
	runtime.settlementStatisticReceived = true
	runtime.settlementClearRewardSent = true
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseResultShown)
	runtime.raidDungeon = runtimeRaidDungeonBinding{
		RaidKey:           0x10063,
		Phase:             runtimeRaidDungeonPhaseOne,
		MapID:             5314,
		LifecycleToken:    runtime.lifecycleToken,
		selectedNotified:  true,
		loadedNotified:    true,
		completedNotified: true,
	}
	service.raids = map[uint32]*runtimeRaidState{
		runtime.raidDungeon.RaidKey: {
			RaidKey:       runtime.raidDungeon.RaidKey,
			LeaderID:      session.selectedCharacterID,
			Started:       true,
			EntryCostPaid: true,
			Members:       []runtimeRaidMemberState{{CharID: session.selectedCharacterID}},
			FrozenMembers: []runtimeRaidMemberState{{CharID: session.selectedCharacterID}},
			Lifecycle: runtimeRaidLifecycleState{
				Status: runtimeRaidLifecyclePhaseActive,
				Phase:  runtimeRaidDungeonPhaseOne,
			},
		},
	}
	service.onlinePlayers = newOnlinePlayerManager()
	if !isAntonRaidEntryDungeonID(runtime.Dungeon.ID) {
		t.Fatalf("dungeon id %d is not treated as a raid map; the live shape is not reproduced", runtime.Dungeon.ID)
	}
	if origin := runtime.townReturnOrigin; origin.TownID == 19 && origin.AreaID == 2 {
		t.Fatal("the fixture origin is already the Anton waiting area; no mismatch is exercised")
	}
	connection.write.Reset()

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketEplpCommand),
		[]byte{1, 2},
	); err != nil {
		t.Fatalf("leader 返回城镇: %v", err)
	}

	op72, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if op72.Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) ||
		!bytes.Equal(op72.Body, buildCurrentDungeonOp72SuccessBody(1, 2)) {
		t.Fatalf("返回城镇 ack = %+v body=%x", op72.Header, op72.Body)
	}
	t.Logf("bytes after the op72 ack: %d  op24Sent=%t", len(rest), runtime.townReturnOp24Sent)
	foundTransition := false
	for len(rest) > 0 {
		packet, tail := splitGameServerUpperPacket(t, rest)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSceneTransitionMsgID {
			foundTransition = true
		}
		rest = tail
	}
	if !foundTransition {
		t.Fatalf("返回城镇 was ACKed but sent no scene transition: %x", connection.write.Bytes())
	}
}
