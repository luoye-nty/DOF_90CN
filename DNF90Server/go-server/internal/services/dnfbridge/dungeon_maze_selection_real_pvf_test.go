package dnfbridge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFGentNorthGateQuest3529BuildsAT5TEntrySequence(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run Gent North Gate AT-5T smoke")
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
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 69,
		Stats: map[string]int64{"fatigue": 156},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3529: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:                 options{accountID: "account-1"},
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCharacters,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	runtime, scene, err := service.prepareDungeonRuntime(
		context.Background(),
		&gameSession{selectedCharacterID: 19},
		dungeoncmd.SelectDungeonRequest{DungeonID: 88, Difficulty: 0},
	)
	if err != nil {
		t.Fatalf("prepare Gent North Gate quest 3529 runtime: %v", err)
	}
	if runtime.MazeIndex != 2 || scene.Coordinate != (worldmap.RoomCoordinate{X: 0, Y: 0}) || scene.Map.Map.ID != 92271 {
		t.Fatalf("Gent North Gate runtime maze=%d scene=%s/%d", runtime.MazeIndex, scene.Coordinate, scene.Map.Map.ID)
	}
	if !runtime.RandomizedObjectsPlanned || len(runtime.RandomizedObjectSelections) != 1 || len(runtime.RandomizedObjectSelections[0]) != 1 {
		t.Fatalf("AT-5T randomized selections=%+v planned=%t", runtime.RandomizedObjectSelections, runtime.RandomizedObjectsPlanned)
	}
	vehicle := runtime.RandomizedObjectSelections[0][0]
	if vehicle.Room != (worldmap.RoomCoordinate{X: 0, Y: 0}) || vehicle.Position != (worldmap.Point{X: 615, Y: 244}) ||
		vehicle.Family != currentDungeonRandomizedMonsterFamily || vehicle.DefinitionID != 61235 ||
		vehicle.ObjectKey != 407 || vehicle.Level != 0x44 || vehicle.Team != "neutral" {
		t.Fatalf("AT-5T selection=%+v", vehicle)
	}
	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatalf("build Gent North Gate quest 3529 packets: %v", err)
	}
	if len(packets.DungeonInfo) != 39 || packets.DungeonInfo[13] != 1 || packets.DungeonInfo[14] != 1 ||
		packets.DungeonInfo[15] != 0 || packets.DungeonInfo[16] != 0 {
		t.Fatalf("AT-5T op28 body=%x", packets.DungeonInfo)
	}
	if packets.RandomizedCreatureCount != 1 || len(packets.RandomizedCreatureQueue) != 23 {
		t.Fatalf("AT-5T op479 count=%d body=%x", packets.RandomizedCreatureCount, packets.RandomizedCreatureQueue)
	}
	queue := packets.RandomizedCreatureQueue
	if queue[0] != 1 || binary.LittleEndian.Uint16(queue[1:3]) != 407 ||
		binary.LittleEndian.Uint32(queue[3:7]) != 61235 || binary.LittleEndian.Uint16(queue[7:9]) != 0x44 ||
		binary.LittleEndian.Uint32(queue[9:13]) != 615 || binary.LittleEndian.Uint32(queue[13:17]) != 244 ||
		binary.LittleEndian.Uint32(queue[17:21]) != 0 || queue[21] != 0 || queue[22] != currentDungeonExplicitPositionMode {
		t.Fatalf("AT-5T op479 body=%x", queue)
	}
	if len(packets.StartMap) < 23 {
		t.Fatalf("AT-5T op29 is too short: %x", packets.StartMap)
	}
	tail := packets.StartMap[len(packets.StartMap)-23:]
	if tail[0] != 1 || tail[1] != 1 || binary.LittleEndian.Uint32(tail[2:6]) != 0 ||
		binary.LittleEndian.Uint32(tail[6:10]) != 0 || binary.LittleEndian.Uint32(tail[10:14]) != currentDungeonRandomizedMonsterFamily ||
		binary.LittleEndian.Uint32(tail[14:18]) != 61235 || binary.LittleEndian.Uint32(tail[18:22]) != 407 || tail[22] != 0xff {
		t.Fatalf("AT-5T op29 association tail=%x body=%x", tail, packets.StartMap)
	}
}

func TestRealScriptPVFDungeon3SelectsActiveQuest3145Maze(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real dungeon maze selection smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeon(3)
	if !ok {
		t.Fatal("real PVF dungeon 3 is missing")
	}
	wantIndex := -1
	for index := range dungeon.Mazes {
		connection := dungeon.Mazes[index].QuestConnection
		if len(connection) >= 2 && connection[0] == 0 && connection[1] == 3145 &&
			(len(connection) < 3 || connection[2] < 0 || connection[2] <= 0) {
			wantIndex = index
			break
		}
	}
	if wantIndex < 0 {
		t.Fatalf("real PVF dungeon 3 has no difficulty-0 active quest connection for 3145: %+v", dungeon.Mazes)
	}
	selected, err := selectDungeonMaze(dungeon.Mazes, 0, dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3145: {Status: "active", ProgressValue: 1},
	}}, func(int) (int, error) {
		t.Fatal("unique real quest maze must not invoke random chooser")
		return 0, nil
	})
	if err != nil || selected.Index != wantIndex || selected.QuestID != 3145 || selected.Reason != "active_quest_connection" {
		t.Fatalf("real dungeon 3 selection=%+v want_index=%d err=%v", selected, wantIndex, err)
	}
}

func TestRealScriptPVFDungeon3ActiveQuestRuntimePrepares(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real dungeon runtime smoke")
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
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       1,
		Stats:       map[string]int64{"fatigue": 156},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:                 options{accountID: "account-1"},
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCharacters,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 1, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	runtime, scene, err := service.prepareDungeonRuntime(
		context.Background(),
		&gameSession{selectedCharacterID: 19},
		dungeoncmd.SelectDungeonRequest{DungeonID: 3, Difficulty: 0},
	)
	if err != nil {
		t.Fatalf("prepare real dungeon 3 quest runtime: %v", err)
	}
	if runtime == nil || runtime.Dungeon.ID != 3 || runtime.MazeIndex < 0 || scene.Map.Map.ID <= 0 {
		t.Fatalf("real dungeon 3 runtime=%+v scene=%+v", runtime, scene)
	}
	connection := runtime.Dungeon.Mazes[runtime.MazeIndex].QuestConnection
	if len(connection) < 2 || connection[0] != 0 || connection[1] != 3145 {
		t.Fatalf("real dungeon 3 selected maze=%d quest_connection=%v", runtime.MazeIndex, connection)
	}
}

func TestRealScriptPVFTrainingDungeon5000BuildsCurrentEntryPackets(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real training-room entry smoke")
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
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       90,
		Stats: map[string]int64{
			"fatigue": 156, "town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:                 options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCharacters,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 1, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	capturedBody, err := hex.DecodeString("881300000000000001ffff00000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	capturedRequest, err := dungeoncmd.DecodeSelectDungeonRequest(capturedBody)
	if err != nil {
		t.Fatalf("decode captured training-room op16 body: %v", err)
	}

	runtime, scene, err := service.prepareDungeonRuntime(
		context.Background(),
		&gameSession{selectedCharacterID: 19},
		capturedRequest,
	)
	if err != nil {
		t.Fatalf("prepare real training-room runtime: %v", err)
	}
	if runtime == nil || runtime.Dungeon.ID != 5000 || runtime.MazeIndex != 0 ||
		scene.Map.Map.ID != 36250 || scene.Coordinate != (worldmap.RoomCoordinate{X: 0, Y: 0}) {
		t.Fatalf("real training-room runtime=%+v scene=%+v", runtime, scene)
	}
	if runtime.Request.RuntimeState != 1 || runtime.Request.RuntimeToken != 0xffff {
		t.Fatalf("real training-room captured request was not retained: %+v", runtime.Request)
	}
	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatalf("build current training-room entry packets: %v", err)
	}
	if len(packets.DungeonInfo) == 0 || len(packets.StartMap) == 0 {
		t.Fatalf("training-room entry packets are empty: info=%x start=%x", packets.DungeonInfo, packets.StartMap)
	}

	connection := &bufferConn{}
	channel := channelcatalog.Channel{ServerID: 1, ID: 206, Type: 1, Name: "ch.206", Port: 10206}
	session := &gameSession{
		conn:                  connection,
		channel:               channel,
		residentChannel:       channel,
		selectedCharacterID:   19,
		townActorOwnerChannel: byte(channel.ID),
		// Prove the direct route invalidates stale town/bootstrap gates before
		// hydrating the actor carried by op15.
		selectedItemListBootstrapCharacterID: 19,
		selectedCreatureStateTableSent:       true,
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
	if err := service.handleDungeonSelectUpper(session, capturedBody); err != nil {
		t.Fatalf("handle captured training-room op16 body: %v", err)
	}
	var packetIDs []uint16
	var dungeonInfo, startMap, fullMode1, creatureState bool
	containerLists := make(map[byte]int)
	remaining := connection.write.Bytes()
	for len(remaining) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, remaining)
		packetIDs = append(packetIDs, packet.Header.MsgID)
		switch packet.Header.MsgID {
		case currentDungeonInfoNotification:
			dungeonInfo = len(packet.Body) > 0
		case currentDungeonStartNotification:
			startMap = len(packet.Body) > 0
		case uint16(dnfenum.CmdPacketLeaveParty):
			if len(packet.Body) > 0 {
				containerLists[packet.Body[0]]++
			}
		case uint16(dnfenum.CmdPacketSetUDPIPPort):
			fullMode1 = len(packet.Body) > 0 && packet.Body[0] == 1
		case currentCreatureStateTableMsgID:
			creatureState = true
		}
		remaining = rest
	}
	// The direct button gets op28 then op29 first and never an op16 ACK; the
	// ordinary placement chain (op9/op120/op3 and friends) must stay absent.
	if len(packetIDs) < 2 || !reflect.DeepEqual(packetIDs[:2], []uint16{currentDungeonInfoNotification, currentDungeonStartNotification}) ||
		!dungeonInfo || !startMap || !fullMode1 || !creatureState {
		t.Fatalf("captured training-room direct response sequence: ids=%v op28=%t op29=%t mode1=%t creature=%t", packetIDs, dungeonInfo, startMap, fullMode1, creatureState)
	}
	for _, listType := range currentSelectInventoryBootstrapListTypes {
		if containerLists[listType] != 1 {
			t.Fatalf("training-room container list%d count=%d want=1 all=%v", listType, containerLists[listType], containerLists)
		}
	}
	for _, forbidden := range []uint16{
		uint16(dnfenum.CmdPacketSelectDungeon),
		uint16(dnfenum.CmdPacketRecoverStamina),
		uint16(dnfenum.CmdPacketRequestBlacklist),
		uint16(dnfenum.CmdPacketNotifyUserState),
	} {
		for _, msgID := range packetIDs {
			if msgID == forbidden {
				t.Fatalf("training-room direct response restored forbidden placement msg id %d: ids=%v", forbidden, packetIDs)
			}
		}
	}
	if !session.preDungeonContextPlayerStateSent || !session.postStartMapPlayerStateSent ||
		!session.sceneBootstrapTailSent || session.sceneBootstrapTailDeferred || session.townSceneReadyCharacterID != 0 ||
		session.selectedItemListBootstrapCharacterID != 19 || !session.selectedCreatureStateTableSent {
		t.Fatalf("training-room carried scene flags pre=%t post=%t tail=%t deferred=%t town_ready=%d inventory_char=%d creature=%t",
			session.preDungeonContextPlayerStateSent,
			session.postStartMapPlayerStateSent,
			session.sceneBootstrapTailSent,
			session.sceneBootstrapTailDeferred,
			session.townSceneReadyCharacterID,
			session.selectedItemListBootstrapCharacterID,
			session.selectedCreatureStateTableSent)
	}
}

// The direct route is keyed on the exact button request; any other owned field
// keeps the ordinary selector path.
func TestIsCurrentPungjinTrainingDirectEntryRequiresTheExactButtonShape(t *testing.T) {
	capturedBody, err := hex.DecodeString("881300000000000001ffff00000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	request, err := dungeoncmd.DecodeSelectDungeonRequest(capturedBody)
	if err != nil {
		t.Fatal(err)
	}
	if !isCurrentPungjinTrainingDirectEntry(request) {
		t.Fatalf("captured button request was not recognized: %+v", request)
	}
	for name, mutate := range map[string]func(*dungeoncmd.SelectDungeonRequest){
		"other dungeon":       func(r *dungeoncmd.SelectDungeonRequest) { r.DungeonID = 5001 },
		"ordinary state":      func(r *dungeoncmd.SelectDungeonRequest) { r.RuntimeState = 0 },
		"practice token":      func(r *dungeoncmd.SelectDungeonRequest) { r.RuntimeToken = 0 },
		"nonzero opaque":      func(r *dungeoncmd.SelectDungeonRequest) { r.LeaderObjectKey = 1 },
		"party state":         func(r *dungeoncmd.SelectDungeonRequest) { r.PartyState = 1 },
		"hell selection mode": func(r *dungeoncmd.SelectDungeonRequest) { r.SelectionMode = 1 },
	} {
		mutated := request
		mutate(&mutated)
		if isCurrentPungjinTrainingDirectEntry(mutated) {
			t.Fatalf("%s must not be treated as the training-room button: %+v", name, mutated)
		}
	}
}

// The job-change trial experience "领悟时间" (7142) sends the same direct-entry
// shape as the training-room button with only the dungeon id replaced. It must
// keep the direct route while staying distinct from the room-specific training
// diagnostic.
func TestIsCurrentDirectDungeonEntryRequestCoversJobChangeTrialExperience(t *testing.T) {
	capturedBody, err := hex.DecodeString("881300000000000001ffff00000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	request, err := dungeoncmd.DecodeSelectDungeonRequest(capturedBody)
	if err != nil {
		t.Fatal(err)
	}
	if !isCurrentPungjinTrainingDirectEntry(request) {
		t.Fatalf("captured training-room button request was not recognized: %+v", request)
	}
	request.DungeonID = currentJobChangeTrialDungeonID
	if !isCurrentDirectDungeonEntryRequest(request) {
		t.Fatalf("job-change trial experience direct entry was not recognized: %+v", request)
	}
	if isCurrentPungjinTrainingDirectEntry(request) {
		t.Fatal("job-change trial experience must not be mislabeled as the training-room button")
	}
}

func TestRealScriptPVFDungeon3SelectsActiveQuest3146StoryMazeWithClearMap3054(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real dungeon 3 quest 3146 smoke")
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
	dungeon, ok := table.FindDungeon(3)
	if !ok {
		t.Fatal("real PVF dungeon 3 is missing")
	}
	selected, err := selectDungeonMaze(dungeon.Mazes, 0, dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3146: {Status: "active", ProgressValue: 2},
		3157: {Status: "completed", ProgressValue: 0},
	}}, func(int) (int, error) {
		t.Fatal("unique real quest 3146 maze must not invoke random chooser")
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := dungeon.Mazes[selected.Index].QuestConnection
	if len(connection) < 2 || connection[0] != 0 || connection[1] != 3146 {
		t.Fatalf("real dungeon 3 quest 3146 selection=%+v quest_connection=%v", selected, connection)
	}
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		dungeon.ID,
		selected.Index,
		func(choice worldmap.DungeonMapChoice) (int64, error) {
			return choice.Candidates[0].ID, nil
		},
	)
	if err != nil {
		t.Fatalf("build real dungeon 3 quest 3146 topology: %v", err)
	}
	foundClearMap := false
	for _, room := range topology.Rooms() {
		if room.Map != nil && room.Map.Map.ID == 76136 {
			foundClearMap = true
			t.Logf("real dungeon 3 quest 3146 clear-map target room=%s boss=%t", room.Coordinate, room.Boss)
		}
	}
	if !foundClearMap {
		t.Fatalf("real dungeon 3 quest 3146 maze=%d has no quest 3054 clear-map target 76136", selected.Index)
	}
}
