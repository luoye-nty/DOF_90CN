package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func currentDungeonVariableOwnerSentinelDeathBody(objectKey uint32) []byte {
	body := make([]byte, dungeoncmd.DieMonsterVariableBaseSize)
	binary.LittleEndian.PutUint32(body[0:4], objectKey)
	binary.LittleEndian.PutUint16(body[4:6], ^uint16(0))
	body[22] = 0
	return body
}

func TestCurrentDungeonConsumedLayerBaseDummyBossEntersSettlementWithoutRewards(t *testing.T) {
	service, runtime := prepareSyntheticMoveRuntimeFromPVF(t, bridgeDungeonConsumedLayerBaseDummyBossPVF())
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	repositories, ok := service.repositoryProvider()
	if !ok {
		t.Fatal("fixture repository unavailable")
	}
	beforeCharacter, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character before consumed-layer completion found=%t err=%v", found, err)
	}

	baseScene, ok := runtime.Session.Scene()
	if !ok || !baseScene.Boss || !baseScene.Cleared || len(baseScene.ExpectedHostiles) != 3 ||
		len(baseScene.BlockingHostiles) != 0 {
		t.Fatalf("base team-0 boss scene=%+v ok=%t", baseScene, ok)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatalf("announce base actors: %v", err)
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		t.Fatalf("cache base room: %v", err)
	}
	baseRoom := runtime.Room.Snapshot()
	if len(baseRoom.Monsters) != 3 {
		t.Fatalf("base monster count=%d want=3", len(baseRoom.Monsters))
	}
	var target runtimeDungeonMonster
	for _, monster := range baseRoom.Monsters {
		if currentDungeonOrdinaryMonsterLooksBoss(monster) {
			target = monster
			break
		}
	}
	if target.ObjectKey == 0 {
		t.Fatalf("base dummy boss missing: %+v", baseRoom.Monsters)
	}

	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "consumed-layer-base-dummy-boss-settlement",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	moveBody := make([]byte, dungeoncmd.MoveMapRequestSize)
	moveBody[0] = byte(baseScene.Coordinate.X)
	moveBody[1] = byte(baseScene.Coordinate.Y)
	moveBody[10] = 1
	if err := service.handleDungeonMoveMap(session, moveBody); err != nil {
		t.Fatalf("enter layered story map: %v", err)
	}
	layerRoom := runtime.Room.Snapshot()
	if len(layerRoom.Monsters) != 1 {
		t.Fatalf("layer monster count=%d want=1", len(layerRoom.Monsters))
	}
	if _, cleared, err := runtime.Room.CommitActorDeathReport(layerRoom.Monsters[0].ObjectKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("clear layered story map cleared=%t err=%v", cleared, err)
	}
	if err := service.handleDungeonMoveMap(session, moveBody); err != nil {
		t.Fatalf("restore consumed layered base map: %v", err)
	}
	chain := runtime.LayerChains[baseScene.Coordinate]
	restoredScene := runtime.Session.Snapshot().Scene
	if runtime.LayeredMapActive || runtime.LayeredMapIndex != -1 || chain == nil ||
		!chain.Consumed || !chain.FinalAckPending || restoredScene.Map.Map.ID != baseScene.Map.Map.ID {
		t.Fatalf("restored consumed-layer state active=%t index=%d chain=%+v scene=%+v",
			runtime.LayeredMapActive, runtime.LayeredMapIndex, chain, restoredScene)
	}
	if got, source, accepted := currentDungeonOwnerSentinelConsumedLayerBaseBossTarget(runtime, restoredScene, target.ObjectKey); !accepted || got.ObjectKey != target.ObjectKey || source != "current_pvf_consumed_layer_base_nonblocking_boss" {
		t.Fatalf("consumed-layer target rejected accepted=%t source=%q got=%+v target=%+v",
			accepted, source, got, target)
	}

	conn.write.Reset()
	if err := service.handleDungeonMonsterDeath(session, currentDungeonVariableOwnerSentinelDeathBody(target.ObjectKey)); err != nil {
		t.Fatal(err)
	}
	completed := runtime.Session.Snapshot()
	if completed.Run.Status != worldmap.DungeonRunCompleted || !runtime.ordinaryFinalRoomClearAccepted ||
		!runtime.bossDieCheckAccepted || !runtime.bossDieCheckResponseSent || !runtime.settlementEntrySent {
		t.Fatalf("consumed-layer dummy boss did not enter settlement: runtime=%+v snapshot=%+v", runtime, completed)
	}
	afterRoom := runtime.Room.Snapshot()
	for _, monster := range afterRoom.Monsters {
		if monster.ObjectKey == target.ObjectKey {
			if monster.State != runtimeDungeonMonsterDefeated ||
				!dungeonSceneObjectDefeated(completed.Scene.DefeatedObjects, monster.ObjectKey) {
				t.Fatalf("target dummy boss was not retired: monster=%+v scene=%+v", monster, completed.Scene)
			}
			continue
		}
		if monster.State != runtimeDungeonMonsterAnnounced ||
			dungeonSceneObjectDefeated(completed.Scene.DefeatedObjects, monster.ObjectKey) {
			t.Fatalf("unrelated team-0 story actor was retired: monster=%+v scene=%+v", monster, completed.Scene)
		}
	}

	packets := splitAllFinishBridgePackets(t, conn.write.Bytes())
	if len(packets) < 3 || packets[0].Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		packets[len(packets)-2].Header.MsgID != uint16(dnfenum.CmdPacketNotifyBossDieCheck) ||
		packets[len(packets)-1].Header.MsgID != currentDungeonSettlementEntryMsgID ||
		!bytes.Equal(packets[len(packets)-1].Body, []byte{0}) {
		t.Fatalf("consumed-layer settlement packet sequence=%v", finishBridgePacketIDs(packets))
	}
	deathPackets := 0
	for _, packet := range packets {
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketNotifyDieMonster) {
			deathPackets++
		}
	}
	if deathPackets != 1 {
		t.Fatalf("death packet count=%d want=1 packets=%v", deathPackets, finishBridgePacketIDs(packets))
	}
	afterCharacter, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character after consumed-layer completion found=%t err=%v", found, err)
	}
	if afterCharacter.Stats["exp"] != beforeCharacter.Stats["exp"] {
		t.Fatalf("dummy completion signal awarded monster experience before=%d after=%d",
			beforeCharacter.Stats["exp"], afterCharacter.Stats["exp"])
	}
}

func TestCurrentDungeonOwnerSentinelAnnouncedOrdinaryMonsterRetiresWithoutBossPromotion(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:    true,
		currentRoomNonBoss: true,
		bossRank:           "[normal]",
	})
	runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	room := runtime.Room.Snapshot()
	if len(room.Monsters) != 2 {
		t.Fatalf("fixture monster count=%d want=2", len(room.Monsters))
	}
	target := room.Monsters[1]
	remaining := room.Monsters[0]
	scene := runtime.Session.Snapshot().Scene
	if got, ok := currentDungeonOwnerSentinelAnnouncedOrdinaryMonster(runtime, scene, target.ObjectKey); !ok || got.ObjectKey != target.ObjectKey {
		t.Fatalf("announced ordinary sentinel target rejected ok=%t got=%+v target=%+v", ok, got, target)
	}
	if _, source, ok := currentDungeonOwnerSentinelBlockingClearTarget(runtime, scene, target.ObjectKey); ok {
		t.Fatalf("ordinary sentinel target was promoted to clear target source=%q target=%+v", source, target)
	}

	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "owner-sentinel-announced-ordinary-monster",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonMonsterDeath(
		session,
		currentDungeonVariableOwnerSentinelDeathBody(target.ObjectKey),
	); err != nil {
		t.Fatal(err)
	}

	afterRoom := runtime.Room.Snapshot()
	afterScene := runtime.Session.Snapshot().Scene
	if afterRoom.Monsters[1].State != runtimeDungeonMonsterDefeated ||
		afterRoom.Monsters[0].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("ordinary sentinel retirement changed monster state unexpectedly: before=%+v after=%+v", room.Monsters, afterRoom.Monsters)
	}
	if afterScene.Cleared ||
		!dungeonSceneObjectDefeated(afterScene.DefeatedObjects, target.ObjectKey) ||
		dungeonSceneObjectDefeated(afterScene.DefeatedObjects, remaining.ObjectKey) {
		t.Fatalf("ordinary sentinel retirement promoted room clear: target=%d remaining=%d scene=%+v",
			target.ObjectKey, remaining.ObjectKey, afterScene)
	}

	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	wantBody, err := buildCurrentDungeonDeathNotificationBody(
		target.ObjectKey,
		currentDungeonDeathResponseMonster,
	)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		packet.Header.Classification != 0 || !bytes.Equal(packet.Body, wantBody) || len(rest) != 0 {
		t.Fatalf("ordinary sentinel death notification: header=%+v body=%x rest=%x want=%x",
			packet.Header, packet.Body, rest, wantBody)
	}
}

func TestCurrentDungeonOwnerSentinelAnnouncedHostileAIRequiresExactBinding(t *testing.T) {
	_, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:    true,
		currentRoomNonBoss: true,
		bossRank:           "[normal]",
	})
	runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
	reference := worldmap.HostileReference{Kind: worldmap.HostileAICharacter, Index: 0}
	if err := runtime.Room.AttachExtendedActors(runtimeDungeonExtendedActorPlan{
		Actors: []runtimeDungeonExtendedActor{{
			Kind:             runtimeDungeonActorAICharacter,
			ObjectKey:        404,
			Packet:           currentDungeonStartMapActor{ObjectKey: 404, Code: 4001, Type: 5},
			HostileReference: &reference,
			State:            runtimeDungeonMonsterAnnounced,
		}},
		AICharacterCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	scene := runtime.Session.Snapshot().Scene
	scene.RuntimeObjects[404] = reference
	scene.ExpectedHostiles = append(scene.ExpectedHostiles, reference)
	scene.Cleared = true
	if !currentDungeonOwnerSentinelAnnouncedHostileAI(runtime, scene, 404) {
		t.Fatalf("valid hostile AI owner-sentinel target rejected: room=%+v scene=%+v", runtime.Room.Snapshot(), scene)
	}
	scene.RuntimeObjects[404] = worldmap.HostileReference{Kind: worldmap.HostileAICharacter, Index: 99}
	if currentDungeonOwnerSentinelAnnouncedHostileAI(runtime, scene, 404) {
		t.Fatal("hostile AI with mismatched runtime binding accepted")
	}
}

func TestCurrentDungeonOwnerSentinelBossSuffixClearsIntermediateRoomAndAllowsMove(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:    true,
		currentRoomNonBoss: true,
		bossRank:           "[dummy]",
		bossSuffixMarker:   "[boss]",
	})
	runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	room := runtime.Room.Snapshot()
	if len(room.Monsters) != 2 {
		t.Fatalf("fixture monster count=%d want=2", len(room.Monsters))
	}
	targetKey := room.Monsters[0].ObjectKey
	remainingKey := room.Monsters[1].ObjectKey
	scene := runtime.Session.Snapshot().Scene
	if _, source, ok := currentDungeonOwnerSentinelBlockingClearTarget(runtime, scene, targetKey); !ok || source != "current_pvf_monster_suffix_boss" {
		t.Fatalf("owner-sentinel target proof ok=%t source=%q room=%+v scene=%+v", ok, source, room, scene)
	}

	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "owner-sentinel-boss-suffix-intermediate-room",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonMonsterDeath(
		session,
		currentDungeonVariableOwnerSentinelDeathBody(targetKey),
	); err != nil {
		t.Fatal(err)
	}
	after := runtime.Session.Snapshot()
	if !after.Scene.Cleared || after.Run.Status != worldmap.DungeonRunActive ||
		runtime.ordinaryFinalRoomClearAccepted || runtime.settlementEntrySent {
		t.Fatalf("intermediate owner-sentinel clear state runtime=%+v snapshot=%+v", runtime, after)
	}
	if !dungeonSceneObjectDefeated(after.Scene.DefeatedObjects, targetKey) ||
		!dungeonSceneObjectDefeated(after.Scene.DefeatedObjects, remainingKey) {
		t.Fatalf("boss clear did not retire both blockers: defeated=%v target=%d remaining=%d",
			after.Scene.DefeatedObjects, targetKey, remainingKey)
	}

	moveBody := make([]byte, dungeoncmd.MoveMapRequestSize)
	moveBody[0] = byte(runtime.BossCoordinate.X)
	moveBody[1] = byte(runtime.BossCoordinate.Y)
	if err := service.handleDungeonMoveMap(session, moveBody); err != nil {
		t.Fatal(err)
	}
	moved := runtime.Session.Snapshot()
	if moved.Scene.Coordinate != runtime.BossCoordinate || moved.Scene.Map.Map.ID != 101 {
		t.Fatalf("cleared intermediate room did not open next room: boss=%s moved=%+v",
			runtime.BossCoordinate, moved.Scene)
	}
}

func TestCurrentDungeonOwnerSentinelBlockingClearTargetStrictOwnership(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:    true,
		currentRoomNonBoss: true,
		bossRank:           "[dummy]",
		bossSuffixMarker:   "[boss]",
	})
	_ = service
	runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	room := runtime.Room.Snapshot()
	targetKey := room.Monsters[0].ObjectKey
	scene := runtime.Session.Snapshot().Scene
	if _, _, ok := currentDungeonOwnerSentinelBlockingClearTarget(runtime, scene, targetKey); !ok {
		t.Fatal("valid blocking boss target rejected")
	}

	tests := []struct {
		name   string
		mutate func(*worldmap.DungeonRoomScene) uint32
	}{
		{
			name: "not blocking",
			mutate: func(scene *worldmap.DungeonRoomScene) uint32 {
				scene.BlockingHostiles = nil
				return targetKey
			},
		},
		{
			name: "stale other room map",
			mutate: func(scene *worldmap.DungeonRoomScene) uint32 {
				scene.Map.Map.ID++
				return targetKey
			},
		},
		{
			name: "runtime binding mismatch",
			mutate: func(scene *worldmap.DungeonRoomScene) uint32 {
				scene.RuntimeObjects[targetKey] = worldmap.HostileReference{Kind: worldmap.HostileMonster, Index: 999}
				return targetKey
			},
		},
		{
			name: "already defeated scene",
			mutate: func(scene *worldmap.DungeonRoomScene) uint32 {
				scene.DefeatedObjects = append(scene.DefeatedObjects, targetKey)
				return targetKey
			},
		},
		{
			name: "extended apc binding is not an ordinary monster",
			mutate: func(scene *worldmap.DungeonRoomScene) uint32 {
				key := targetKey + 1000
				reference := worldmap.HostileReference{Kind: worldmap.HostileAICharacter, Index: 0}
				scene.RuntimeObjects[key] = reference
				scene.ExpectedHostiles = append(scene.ExpectedHostiles, reference)
				scene.BlockingHostiles = append(scene.BlockingHostiles, reference)
				return key
			},
		},
		{
			name: "passive client object has no announced binding",
			mutate: func(_ *worldmap.DungeonRoomScene) uint32 {
				return targetKey + 2000
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := scene
			candidate.RuntimeObjects = make(map[uint32]worldmap.HostileReference, len(scene.RuntimeObjects)+1)
			for key, reference := range scene.RuntimeObjects {
				candidate.RuntimeObjects[key] = reference
			}
			candidate.ExpectedHostiles = append([]worldmap.HostileReference(nil), scene.ExpectedHostiles...)
			candidate.BlockingHostiles = append([]worldmap.HostileReference(nil), scene.BlockingHostiles...)
			candidate.DefeatedObjects = append([]uint32(nil), scene.DefeatedObjects...)
			key := test.mutate(&candidate)
			if _, source, ok := currentDungeonOwnerSentinelBlockingClearTarget(runtime, candidate, key); ok {
				t.Fatalf("unowned target accepted source=%q scene=%+v key=%d", source, candidate, key)
			}
		})
	}
}

func TestCurrentDungeonOwnerSentinelHostileRejectsUnprovedRequestShapes(t *testing.T) {
	for _, test := range []struct {
		name string
		body func(uint32) []byte
	}{
		{
			name: "fixed layout",
			body: func(key uint32) []byte {
				body := make([]byte, dungeoncmd.DieMonsterRequestSize)
				binary.LittleEndian.PutUint32(body[0:4], key)
				binary.LittleEndian.PutUint16(body[4:6], ^uint16(0))
				return body
			},
		},
		{
			name: "multiple variable combat entries",
			body: func(key uint32) []byte {
				body := make([]byte, dungeoncmd.DieMonsterVariableBaseSize+2*dungeoncmd.DieMonsterVariableCombatEntrySize)
				binary.LittleEndian.PutUint32(body[0:4], key)
				binary.LittleEndian.PutUint16(body[4:6], ^uint16(0))
				body[22] = 2
				return body
			},
		},
		{
			name: "opaque tail",
			body: func(key uint32) []byte {
				return append(currentDungeonVariableOwnerSentinelDeathBody(key), 0xaa)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
				disableTutorial:    true,
				currentRoomNonBoss: true,
				bossRank:           "[dummy]",
				bossSuffixMarker:   "[boss]",
			})
			if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
				t.Fatal(err)
			}
			targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
			runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
			conn := &bufferConn{}
			session := &gameSession{
				conn:                conn,
				connID:              "owner-sentinel-hostile-strict-request-boundary",
				selectedCharacterID: 99,
				dungeon:             dungeonSessionState{runtime: runtime},
			}
			if err := service.handleDungeonMonsterDeath(session, test.body(targetKey)); err != nil {
				t.Fatal(err)
			}
			if conn.write.Len() != 0 || runtime.Session.Snapshot().Scene.Cleared ||
				runtime.Room.Snapshot().Monsters[0].State != runtimeDungeonMonsterAnnounced {
				t.Fatalf("unproved sentinel request mutated state: writes=%x runtime=%+v scene=%+v",
					conn.write.Bytes(), runtime.Room.Snapshot(), runtime.Session.Snapshot().Scene)
			}
		})
	}
}

func TestCurrentDungeonOwnerSentinelExactOrdinaryMonsterAcceptsSingleCombatEntry(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:    true,
		currentRoomNonBoss: true,
		bossRank:           "[dummy]",
		bossSuffixMarker:   "[boss]",
	})
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
	body := make([]byte, dungeoncmd.DieMonsterVariableBaseSize+dungeoncmd.DieMonsterVariableCombatEntrySize)
	binary.LittleEndian.PutUint32(body[0:4], targetKey)
	binary.LittleEndian.PutUint16(body[4:6], ^uint16(0))
	body[22] = 1

	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "owner-sentinel-single-combat-entry",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonMonsterDeath(session, body); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() == 0 {
		t.Fatal("exact ordinary owner-sentinel death did not send an acknowledgement")
	}
	snapshot := runtime.Room.Snapshot()
	if snapshot.Monsters[0].State != runtimeDungeonMonsterDefeated || !runtime.Session.Snapshot().Scene.Cleared {
		t.Fatalf("exact ordinary owner-sentinel death was not committed: room=%+v scene=%+v",
			snapshot, runtime.Session.Snapshot().Scene)
	}
}

func TestCurrentDungeonOwnerSentinelExplicitPVFClearTargetAccepted(t *testing.T) {
	_, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:    true,
		currentRoomNonBoss: true,
		bossRank:           "[normal]",
		clearCondition:     "[clear condition]\n[hunt monster]\n3001 1\n[/clear condition]\n",
	})
	runtime.Dungeon.Path = "dungeon/ordinary/owner_sentinel.dgn"
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	if normalizeDungeonPVFSymbol(monster.Spawn.Rank) == "boss" ||
		normalizeDungeonPVFSymbol(monster.Spawn.SuffixMarker) == "boss" {
		t.Fatalf("fixture unexpectedly uses boss syntax: %+v", monster.Spawn)
	}
	if _, source, ok := currentDungeonOwnerSentinelBlockingClearTarget(
		runtime,
		runtime.Session.Snapshot().Scene,
		monster.ObjectKey,
	); !ok || source == "" {
		t.Fatalf("explicit PVF clear target rejected ok=%t source=%q monster=%+v", ok, source, monster)
	}
}

// A cinematic boss room whose only spawn is a team-0 dummy boss is Cleared on
// entry; the client still reports the dummy boss death with owner=0xffff and
// that report is the sole settlement signal (quest 3599 / dungeon 96 / map 76114).
func TestCurrentDungeonOwnerSentinelDummyBossInZeroBlockingBossRoomSettlesDungeon(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial:  true,
		bossRank:         "[dummy]",
		bossSuffixMarker: "[boss]",
		singleMonster:    true,
	})
	runtime.Dungeon.Path = "dungeon/ordinary/cinematic_boss.dgn"
	runtime.BossSet = true
	runtime.BossCoordinate = worldmap.RoomCoordinate{X: 0, Y: 0}

	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey

	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "owner-sentinel-dummy-boss-cinematic-settle",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}

	if err := service.handleDungeonMonsterDeath(
		session,
		currentDungeonVariableOwnerSentinelDeathBody(targetKey),
	); err != nil {
		t.Fatalf("handle dummy boss death: %v", err)
	}

	completed := runtime.Session.Snapshot()
	if completed.Run.Status != worldmap.DungeonRunCompleted {
		t.Fatalf("run status = %s, want %s", completed.Run.Status, worldmap.DungeonRunCompleted)
	}
	if !runtime.ordinaryFinalRoomClearAccepted || !runtime.bossDieCheckAccepted ||
		!runtime.bossDieCheckResponseSent || !runtime.settlementEntrySent {
		t.Fatalf("completion flags incomplete: runtime=%+v", runtime)
	}
}
