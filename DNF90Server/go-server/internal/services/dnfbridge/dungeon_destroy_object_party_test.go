package dnfbridge

import (
	"context"
	"os"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The party-wide destroy-object clear.
//
// 线上事实（2026-09-29 22:28-22:38 本地，raid 1835059，二阶段暗卢克）：
// 5066 能源储藏室的 boss 房 1:1/74247 里两个人**同时**进去，全队只有**一个人**的
// 客户端会报那个共享被动对象的 op39 → 只有他被结算，另一个人 11~28 秒后被迫 op42
// 弃权（三次 run 三次如此）。修法见
// completeCurrentDungeonDestroyObjectClearForPartyLocked。
//
// 这一组用例用真 Script.pvf 跑完整链路：两个人各自的实例、各自走到同一个 boss 房、
// 只有一个人报 op39，然后断言**两个人各自**都拿到 op115 + op31（且只有一次）。
// 走迷宫时不设队伍状态，队伍状态只在最后一步之前装上 —— 这样被测的只有 fan-out
// 本身，不牵扯组队进图的加载屏障。

// destroyObjectPartyCase is one real dungeon whose boss room is cleared by
// destroying a client-owned passive object.
type destroyObjectPartyCase struct {
	name       string
	dungeonID  uint32
	bossMapID  int64
	bossObject uint32
}

// 5063 能源控制室（祭坛那个，上一轮已修）与 5066 能源储藏室（机械王座那个，
// 服主这轮报的）逐字同形：都是 [clear condition][destroy object] <id> 1，boss 房
// 里没有 [monster]、只有一个 [passive object]。见
// TestRealScriptPVFNoAntonDungeonDeclaresDestroyObjectClearCondition 的全表扫描。
var destroyObjectPartyCases = []destroyObjectPartyCase{
	{name: "5063 energy control", dungeonID: 5063, bossMapID: 74232, bossObject: 64062},
	{name: "5066 energy save", dungeonID: 5066, bossMapID: 74247, bossObject: 64060},
}

// destroyObjectPartyMember is one walked member of the fixture party.
type destroyObjectPartyMember struct {
	charID  uint16
	session *gameSession
	runtime *runtimeDungeonState
}

// destroyObjectPartyFixture prepares one real dungeon run per character and walks
// every one of them into the dungeon's boss room.
func destroyObjectPartyFixture(
	t *testing.T,
	testCase destroyObjectPartyCase,
	charIDs []uint16,
) (*Service, []destroyObjectPartyMember, []worldmap.RoomCoordinate) {
	t.Helper()
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run the real-PVF party destroy-object smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
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
	repositories := dnfrepoMemoryGroup(t, charIDs)
	service := &Service{
		options:                 options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		initialEquipmentArchive: archive,
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCharacters,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 1, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:            map[uint16]*gameSession{},
		onlinePlayers:           newOnlinePlayerManager(),
	}

	var path []worldmap.RoomCoordinate
	members := make([]destroyObjectPartyMember, 0, len(charIDs))
	for _, charID := range charIDs {
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "party-destroy-object-" + strconv.Itoa(int(charID)),
			selectedCharacterID: charID,
		}
		runtime, startScene, err := service.prepareDungeonRuntime(
			context.Background(),
			session,
			dungeoncmd.SelectDungeonRequest{DungeonID: testCase.dungeonID, Difficulty: 0},
		)
		if err != nil {
			t.Fatalf("char %d prepare real dungeon %d: %v", charID, testCase.dungeonID, err)
		}
		session.dungeon.runtime = runtime
		service.mu.Lock()
		service.gameSessions[charID] = session
		service.mu.Unlock()
		if path == nil {
			path = destroyObjectPartyBossPath(t, resolver, runtime, startScene)
		}
		members = append(members, destroyObjectPartyMember{
			charID: charID, session: session, runtime: runtime,
		})
	}
	if len(path) < 2 {
		t.Fatalf("dungeon %d has no path to its boss room: %v", testCase.dungeonID, path)
	}
	boss := path[len(path)-1]
	for index := range members {
		destroyObjectPartyWalk(t, service, members[index], path)
		scene := members[index].runtime.Session.Snapshot().Scene
		if !scene.Boss || scene.Map.Map.ID != testCase.bossMapID || scene.Coordinate != boss {
			t.Fatalf("char %d landed in %+v map=%d want boss %s map %d",
				members[index].charID, scene.Coordinate, scene.Map.Map.ID, boss, testCase.bossMapID)
		}
		if source := currentDungeonDestroyObjectClearConditionSource(
			members[index].runtime, scene, testCase.bossObject); source == "" {
			t.Fatalf("char %d boss room does not carry the maze's destroy-object condition: %+v",
				members[index].charID, scene.PassiveObjects)
		}
	}
	return service, members, path
}

// dnfrepoMemoryGroup stores the characters the party fixture needs.
func dnfrepoMemoryGroup(t *testing.T, charIDs []uint16) dnfrepo.Group {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, charID := range charIDs {
		id := strconv.Itoa(int(charID))
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "account-1",
			Name:        "party-member",
			Job:         "2",
			Level:       90,
			Stats:       map[string]int64{"fatigue": 200},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: id,
			Slots:       map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return repositories
}

// joinDestroyObjectParty puts the walked members into one ordinary party. It runs
// after the walk so the dungeon-entry machinery is untouched by the new behaviour.
func joinDestroyObjectParty(members []destroyObjectPartyMember) {
	partyMembers := make([]alignedcmd.PartyMemberState, 0, len(members))
	for slot, member := range members {
		partyMembers = append(partyMembers, alignedcmd.PartyMemberState{
			UserID: member.charID,
			Slot:   byte(slot),
		})
	}
	for _, member := range members {
		storeRuntimePartyState(member.session, alignedcmd.PartyState{
			PartyID:    3,
			MaxMembers: 4,
			Members:    partyMembers,
		})
	}
}

// destroyObjectPartyBossPath is the maze's own start -> boss room walk.
func destroyObjectPartyBossPath(
	t *testing.T,
	resolver *worldmap.Resolver,
	runtime *runtimeDungeonState,
	startScene worldmap.DungeonRoomScene,
) []worldmap.RoomCoordinate {
	t.Helper()
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		runtime.Dungeon.ID,
		runtime.MazeIndex,
		func(choice worldmap.DungeonMapChoice) (int64, error) { return choice.Candidates[0].ID, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	queue := []worldmap.RoomCoordinate{startScene.Coordinate}
	visited := map[worldmap.RoomCoordinate]bool{startScene.Coordinate: true}
	previous := make(map[worldmap.RoomCoordinate]worldmap.RoomCoordinate)
	for len(queue) != 0 && !visited[runtime.BossCoordinate] {
		current := queue[0]
		queue = queue[1:]
		room, ok := topology.Room(current)
		if !ok {
			t.Fatalf("path room missing: %s", current)
		}
		for _, neighbor := range room.Neighbors {
			if visited[neighbor.Coordinate] {
				continue
			}
			visited[neighbor.Coordinate] = true
			previous[neighbor.Coordinate] = current
			queue = append(queue, neighbor.Coordinate)
		}
	}
	if !visited[runtime.BossCoordinate] {
		t.Fatalf("boss room %s unreachable from %s", runtime.BossCoordinate, startScene.Coordinate)
	}
	path := []worldmap.RoomCoordinate{runtime.BossCoordinate}
	for path[len(path)-1] != startScene.Coordinate {
		path = append(path, previous[path[len(path)-1]])
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
}

// destroyObjectPartyWalk drives one member along the path, clearing each room the
// way the real client does.
func destroyObjectPartyWalk(
	t *testing.T,
	service *Service,
	member destroyObjectPartyMember,
	path []worldmap.RoomCoordinate,
) {
	t.Helper()
	clearRoom := func() {
		room := member.runtime.Room.Snapshot()
		planned := false
		for _, monster := range room.Monsters {
			planned = planned || monster.State == runtimeDungeonMonsterPlanned
		}
		for _, actor := range room.ExtendedActors {
			planned = planned || actor.State == runtimeDungeonMonsterPlanned
		}
		if planned {
			if _, err := member.runtime.Room.AnnounceAllActors(member.runtime.Session); err != nil {
				t.Fatalf("char %d announce room: %v", member.charID, err)
			}
		}
		scene := member.runtime.Session.Snapshot().Scene
		for _, reference := range scene.BlockingHostiles {
			var objectKey uint32
			for candidate, bound := range scene.RuntimeObjects {
				if bound == reference {
					objectKey = candidate
					break
				}
			}
			if objectKey == 0 {
				t.Fatalf("char %d blocker has no runtime object: %+v", member.charID, reference)
			}
			if _, _, defeated := currentDungeonDefeatedActor(member.runtime, scene, objectKey); defeated {
				continue
			}
			if _, _, err := member.runtime.Room.CommitActorDeathReport(objectKey, member.runtime.Session); err != nil {
				t.Fatalf("char %d defeat blocker key=%d: %v", member.charID, objectKey, err)
			}
			scene = member.runtime.Session.Snapshot().Scene
		}
		if !member.runtime.Session.Snapshot().Scene.Cleared {
			t.Fatalf("char %d room did not clear: %+v", member.charID, member.runtime.Session.Snapshot().Scene)
		}
	}
	for _, target := range path[1:] {
		clearRoom()
		body := make([]byte, dungeoncmd.MoveMapRequestSize)
		body[0] = byte(target.X)
		body[1] = byte(target.Y)
		member.session.conn.(*bufferConn).write.Reset()
		if err := service.handleDungeonMoveMap(member.session, body); err != nil {
			t.Fatalf("char %d move to %s: %v", member.charID, target, err)
		}
		if scene := member.runtime.Session.Snapshot().Scene; scene.Coordinate != target {
			t.Fatalf("char %d move committed %s want %s", member.charID, scene.Coordinate, target)
		}
	}
}

// settlementPacketsOnWire counts the two packets that make up one member's own
// settlement entry: op115 (boss-die check) and op31 (settlement entry).
func settlementPacketsOnWire(t *testing.T, member destroyObjectPartyMember) (bossDieChecks, settlementEntries int) {
	t.Helper()
	for _, packet := range splitAllFinishBridgePackets(t, member.session.conn.(*bufferConn).write.Bytes()) {
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketNotifyBossDieCheck):
			bossDieChecks++
		case currentDungeonSettlementEntryMsgID:
			settlementEntries++
		}
	}
	return bossDieChecks, settlementEntries
}

// TestRealScriptPVFDestroyObjectClearReachesEveryPartyMember is the fix's pin:
// one shared object, one op39, and **both** members of the party must each get
// their own op115 + op31. The second half is the idempotence half: when the other
// member's own op39 arrives afterwards, nobody may be settled a second time.
func TestRealScriptPVFDestroyObjectClearReachesEveryPartyMember(t *testing.T) {
	for _, testCase := range destroyObjectPartyCases {
		t.Run(testCase.name, func(t *testing.T) {
			service, members, _ := destroyObjectPartyFixture(t, testCase, []uint16{51, 10})
			joinDestroyObjectParty(members)
			reporter, peer := members[0], members[1]
			for _, member := range members {
				member.session.conn.(*bufferConn).write.Reset()
			}

			// Only ONE client reports the shared object, exactly like the live run.
			if err := service.handleDungeonMonsterDeath(
				reporter.session,
				tutorialScopeVariableZeroCombatDeathBody(testCase.bossObject),
			); err != nil {
				t.Fatalf("reporter op39: %v", err)
			}
			for _, member := range members {
				acks, entries := settlementPacketsOnWire(t, member)
				if acks != 1 || entries != 1 {
					t.Fatalf("char %d got op115=%d op31=%d want one each "+
						"(the shared clear object must reach every member standing in the boss room)",
						member.charID, acks, entries)
				}
				if !member.runtime.ordinaryFinalRoomClearAccepted {
					t.Fatalf("char %d did not commit its final-room clear", member.charID)
				}
			}

			// Idempotence: the peer's own op39 arrives late. Nobody settles twice.
			for _, member := range members {
				member.session.conn.(*bufferConn).write.Reset()
			}
			if err := service.handleDungeonMonsterDeath(
				peer.session,
				tutorialScopeVariableZeroCombatDeathBody(testCase.bossObject),
			); err != nil {
				t.Fatalf("peer op39: %v", err)
			}
			for _, member := range members {
				acks, entries := settlementPacketsOnWire(t, member)
				if acks != 0 || entries != 0 {
					t.Fatalf("char %d was settled twice: op115=%d op31=%d", member.charID, acks, entries)
				}
			}
		})
	}
}

// TestRealScriptPVFDestroyObjectClearLeavesOtherRoomsAlone is the boundary pin:
// a party member still standing in another room must NOT be settled by somebody
// else's clear.
func TestRealScriptPVFDestroyObjectClearLeavesOtherRoomsAlone(t *testing.T) {
	testCase := destroyObjectPartyCases[0]
	service, members, path := destroyObjectPartyFixture(t, testCase, []uint16{51, 10})
	joinDestroyObjectParty(members)

	// Walk the peer back one room, so the two members are in different rooms of the
	// same run when the reporter clears the boss room. The step is the room the
	// walk came through, which is adjacent by construction.
	body := make([]byte, dungeoncmd.MoveMapRequestSize)
	body[0] = byte(path[len(path)-2].X)
	body[1] = byte(path[len(path)-2].Y)
	if err := service.handleDungeonMoveMap(members[1].session, body); err != nil {
		t.Fatalf("peer move back one room: %v", err)
	}
	if scene := members[1].runtime.Session.Snapshot().Scene; scene.Coordinate == members[0].runtime.BossCoordinate {
		t.Fatal("the peer is still standing in the boss room")
	}
	for _, member := range members {
		member.session.conn.(*bufferConn).write.Reset()
	}

	if err := service.handleDungeonMonsterDeath(
		members[0].session,
		tutorialScopeVariableZeroCombatDeathBody(testCase.bossObject),
	); err != nil {
		t.Fatalf("reporter op39: %v", err)
	}
	if acks, entries := settlementPacketsOnWire(t, members[0]); acks != 1 || entries != 1 {
		t.Fatalf("reporter got op115=%d op31=%d want one each", acks, entries)
	}
	if acks, entries := settlementPacketsOnWire(t, members[1]); acks != 0 || entries != 0 {
		t.Fatalf("a member in another room was settled: op115=%d op31=%d", acks, entries)
	}
	if members[1].runtime.ordinaryFinalRoomClearAccepted {
		t.Fatal("a member in another room had its final room committed")
	}
}

// TestRealScriptPVFDestroyObjectClearSoloRunUnchanged keeps the single-player run
// byte-for-byte on the old behaviour: one member, one op39, one settlement.
func TestRealScriptPVFDestroyObjectClearSoloRunUnchanged(t *testing.T) {
	for _, testCase := range destroyObjectPartyCases {
		t.Run(testCase.name, func(t *testing.T) {
			service, members, _ := destroyObjectPartyFixture(t, testCase, []uint16{51})
			solo := members[0]
			solo.session.conn.(*bufferConn).write.Reset()
			if err := service.handleDungeonMonsterDeath(
				solo.session,
				tutorialScopeVariableZeroCombatDeathBody(testCase.bossObject),
			); err != nil {
				t.Fatalf("solo op39: %v", err)
			}
			if acks, entries := settlementPacketsOnWire(t, solo); acks != 1 || entries != 1 {
				t.Fatalf("solo run got op115=%d op31=%d want one each", acks, entries)
			}
		})
	}
}
