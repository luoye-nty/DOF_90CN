package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFDungeon96Quest3599CinematicDummyBossSettlement verifies that
// in Quest 3599 (Dungeon 96 / 无法地带营地), where Boss Map 76114 contains only
// team-0 friendly/dummy monsters (Melvin, Marlene, 62959 Dummy Boss), when the
// client returns from layer 76113 and completes the cinematic by sending
// variable DIE_MONSTER (op39) with owner=0xffff for the dummy boss, the server
// accepts the death report and triggers dungeon settlement (op115 + op31).
func TestRealScriptPVFDungeon96Quest3599CinematicDummyBossSettlement(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify quest 3599 cinematic dummy boss settlement")
	}
	ctx := context.Background()
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(ctx, archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	monsters, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	aiCharacters, err := newPVFDungeonAICharacterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	questIndex, err := dnfpvf.Build(ctx, archive, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	questCatalog, err := dnfquest.Load(ctx, questIndex)
	if err != nil {
		t.Fatal(err)
	}
	questDefinition, ok := questCatalog.Find(3599)
	if !ok {
		t.Fatal("real quest 3599 definition not found in PVF")
	}
	t.Logf("real quest 3599 type=%s intData=%v", questDefinition.Type, questDefinition.IntData)

	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Name:        "Quest3599Test",
		Job:         "0",
		Level:       85,
		Stats: map[string]int64{
			"fatigue": 156,
			"exp":     0,
		},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3599: {Status: "active", ProgressValue: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := &Service{
		options:                 options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCharacters,
		questCatalog:            questCatalog,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 1, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "real-pvf-quest3599-dummy-boss", selectedCharacterID: 19}

	// 1. Enter Dungeon 96 (Maze 0 selected via active Quest 3599)
	runtime, startScene, err := service.prepareDungeonRuntime(
		ctx,
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 96, Difficulty: 0},
	)
	if err != nil {
		t.Fatalf("prepare real dungeon 96 quest 3599 runtime: %v", err)
	}
	session.dungeon.runtime = runtime
	if startScene.Coordinate != (worldmap.RoomCoordinate{X: 0, Y: 0}) || startScene.Map.Map.ID != 76112 {
		t.Fatalf("start scene = %s/%d, want 0:0/76112", startScene.Coordinate, startScene.Map.Map.ID)
	}

	// 2. Move from (0, 0) Map 76112 to (1, 0) Boss Map 76114
	moveBody := make([]byte, dungeoncmd.MoveMapRequestSize)
	moveBody[0] = 1
	moveBody[1] = 0
	if err := service.handleDungeonMoveMap(session, moveBody); err != nil {
		t.Fatalf("move to (1,0) boss map 76114: %v", err)
	}
	scene := runtime.Session.Snapshot().Scene
	if scene.Coordinate != (worldmap.RoomCoordinate{X: 1, Y: 0}) || scene.Map.Map.ID != 76114 || !scene.Boss {
		t.Fatalf("boss scene = %s/%d boss=%t, want 1:0/76114 boss=true", scene.Coordinate, scene.Map.Map.ID, scene.Boss)
	}

	// 3. Layered transition to Map 76113 (MoveKind = 1 at same coordinate 1:0)
	layerMoveBody := make([]byte, dungeoncmd.MoveMapRequestSize)
	layerMoveBody[0] = 1
	layerMoveBody[1] = 0
	layerMoveBody[10] = 1 // MoveKind = 1
	if err := service.handleDungeonMoveMap(session, layerMoveBody); err != nil {
		t.Fatalf("layer move to 76113: %v", err)
	}
	layerScene := runtime.Session.Snapshot().Scene
	if layerScene.Map.Map.ID != 76113 {
		t.Fatalf("layer scene map = %d, want 76113", layerScene.Map.Map.ID)
	}

	// 4. Return from layer 76113 to base Map 76114 (MoveKind = 1 at same coordinate 1:0)
	returnMoveBody := make([]byte, dungeoncmd.MoveMapRequestSize)
	returnMoveBody[0] = 1
	returnMoveBody[1] = 0
	returnMoveBody[10] = 1 // MoveKind = 1
	if err := service.handleDungeonMoveMap(session, returnMoveBody); err != nil {
		t.Fatalf("layer return to base map 76114: %v", err)
	}
	baseScene := runtime.Session.Snapshot().Scene
	if baseScene.Map.Map.ID != 76114 {
		t.Fatalf("returned base scene map = %d, want 76114", baseScene.Map.Map.ID)
	}

	// 5. Find the Dummy Boss (62959) in the base room
	roomSnapshot := runtime.Room.Snapshot()
	var dummyBossKey uint32
	for _, m := range roomSnapshot.Monsters {
		if m.Spawn.MonsterID == 62959 {
			dummyBossKey = m.ObjectKey
			break
		}
	}
	if dummyBossKey == 0 {
		t.Fatalf("dummy boss 62959 not found in room monsters: %+v", roomSnapshot.Monsters)
	}
	t.Logf("found dummy boss 62959 objectKey = %d", dummyBossKey)

	// 6. Client sends DIE_MONSTER (op39) for Dummy Boss with owner=0xffff
	conn.write.Reset()
	dieBody := currentDungeonVariableOwnerSentinelDeathBody(dummyBossKey)
	if err := service.handleDungeonMonsterDeath(session, dieBody); err != nil {
		t.Fatalf("handle dummy boss death: %v", err)
	}

	// 7. Verify settlement packets (op38 ACK, op115 boss die response, op31 dungeon clear)
	completed := runtime.Session.Snapshot()
	if completed.Run.Status != worldmap.DungeonRunCompleted {
		t.Fatalf("dungeon run status = %s, want %s", completed.Run.Status, worldmap.DungeonRunCompleted)
	}
	if !runtime.ordinaryFinalRoomClearAccepted {
		t.Fatal("ordinaryFinalRoomClearAccepted is false")
	}
	if !runtime.bossDieCheckAccepted {
		t.Fatal("bossDieCheckAccepted is false")
	}
	if !runtime.bossDieCheckResponseSent {
		t.Fatal("bossDieCheckResponseSent is false (op115 not sent)")
	}
	if !runtime.settlementEntrySent {
		t.Fatal("settlementEntrySent is false (op31 not sent)")
	}

	// Verify the sent packet stream contains op38 (die monster), op115 (boss die check), and op31 (dungeon clear)
	sentBytes := conn.write.Bytes()
	hasOp38, hasOp115, hasOp31 := false, false, false
	for len(sentBytes) > 0 {
		packet, rest := splitGameServerUpperPacket(t, sentBytes)
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketNotifyDieMonster): // op38
			hasOp38 = true
		case uint16(dnfenum.CmdPacketNotifyBossDieCheck): // op115
			hasOp115 = true
		case currentDungeonSettlementEntryMsgID: // op31
			hasOp31 = true
		}
		sentBytes = rest
	}
	if !hasOp38 || !hasOp115 || !hasOp31 {
		t.Fatalf("sent packets missing expected opcodes: op38=%t op115=%t op31=%t", hasOp38, hasOp115, hasOp31)
	}
	t.Logf("SUCCESS: real dungeon 96 quest 3599 dummy boss %d died -> op38/op115/op31 sent -> dungeon settled successfully!", dummyBossKey)
}
