package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestQuestPassiveObjectOp39Completes3157AndRecomputes3146(t *testing.T) {
	ctx := context.Background()
	index, err := dnfpvf.Build(ctx, bridgePVFSource{
		dnfquest.DefaultList:  "3146 `parent.qst`\n3157 `passive.qst`\n",
		"n_quest/parent.qst":  "[grade]\n`[epic]`\n[type]\n`[quest clear]`\n[int data]\n3157\n",
		"n_quest/passive.qst": "[grade]\n`[sub]`\n[type]\n`[hunt enemy]`\n[main quest]\n3146\n[int data]\n3 -1 13099 3 1\n",
	}, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "acc", Level: 5}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		3146: {Status: "active", ProgressValue: 1},
		3157: {Status: "active", ProgressValue: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	firstMonsterKey := uint32(437)
	room := &runtimeDungeonRoom{
		monsters:    []runtimeDungeonMonster{{ObjectKey: firstMonsterKey, State: runtimeDungeonMonsterAnnounced}},
		byObjectKey: map[uint32]int{firstMonsterKey: 0},
	}
	runtime := &runtimeDungeonState{
		Request:   dungeoncmd.SelectDungeonRequest{DungeonID: 3, Difficulty: 0},
		Dungeon:   worldmap.Dungeon{ID: 3},
		Character: dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "acc", Level: 5},
		Session:   &worldmap.DungeonSession{},
		Room:      room,
		MazeIndex: 2,
	}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "quest-passive-object", selectedCharacterID: 19, dungeon: dungeonSessionState{runtime: runtime}}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	scene := worldmap.DungeonRoomScene{Map: worldmap.ResolvedMap{Map: worldmap.Map{ID: 76136}}}

	handled, err := service.handleCurrentDungeonQuestPassiveObjectDeathLocked(session, runtime, scene, firstMonsterKey-1)
	if err != nil || !handled {
		t.Fatalf("handle passive object: handled=%t err=%v", handled, err)
	}
	firstPacket, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if firstPacket.Header.MsgID != 38 || firstPacket.Header.Classification != 0 {
		t.Fatalf("passive death ACK header=%+v body=%x", firstPacket.Header, firstPacket.Body)
	}
	questPacket, trailing := splitGameServerUpperPacket(t, rest)
	if questPacket.Header.MsgID != currentActiveQuestSnapshotMsgID || questPacket.Header.Classification != 0 || len(trailing) != 0 {
		t.Fatalf("quest snapshot header=%+v body=%x trailing=%x", questPacket.Header, questPacket.Body, trailing)
	}
	persisted, found, err := repositories.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load persisted quest: found=%t err=%v", found, err)
	}
	if child := persisted.States[3157]; child.Status != "completed" || child.ProgressValue != 0 || child.Extra["completion_enemy_code"] != "13099" {
		t.Fatalf("3157=%+v", child)
	}
	if parent := persisted.States[3146]; parent.Status != "active" || parent.ProgressValue != 0 {
		t.Fatalf("3146=%+v", parent)
	}
}

func TestQuestPassiveObjectCandidateRejectsNonAdjacentUnknownObject(t *testing.T) {
	room := &runtimeDungeonRoom{monsters: []runtimeDungeonMonster{{ObjectKey: 437, State: runtimeDungeonMonsterAnnounced}}}
	if currentDungeonQuestObjectImmediatelyPrecedesAnnouncedActor(room, 435) {
		t.Fatal("non-adjacent unknown object was accepted")
	}
	if !currentDungeonQuestObjectImmediatelyPrecedesAnnouncedActor(room, 436) {
		t.Fatal("adjacent quest-scene object was not recognized")
	}
}

func TestQuestPassiveObjectEnemyRewardTypeThreeGrantsOncePerMapInstance(t *testing.T) {
	ctx := context.Background()
	index, err := dnfpvf.Build(ctx, bridgePVFSource{
		dnfquest.DefaultList: "4000 `passive_reward.qst`\n",
		"n_quest/passive_reward.qst": "[grade]\n`[epic]`\n" +
			"[type]\n`[seeking]`\n" +
			"[enemy reward item]\n" +
			"16111 3 102 -1 10164801 1 100 15\n" +
			"[/enemy reward item]\n",
	}, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		4000: {Status: "active"},
	}}); err != nil {
		t.Fatal(err)
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate: worldmap.RoomCoordinate{X: 2, Y: 1},
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 76515}},
		PassiveObjects: []worldmap.PassiveObject{
			{ObjectID: 16111}, {ObjectID: 16112}, {ObjectID: 16111}, {ObjectID: 16111},
		},
	}
	room := &runtimeDungeonRoom{coordinate: scene.Coordinate, mapID: scene.Map.Map.ID}
	runtime := &runtimeDungeonState{
		Request:   dungeoncmd.SelectDungeonRequest{DungeonID: 102, Difficulty: 3},
		Dungeon:   worldmap.Dungeon{ID: 102},
		Character: dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "acc"},
		Session:   &worldmap.DungeonSession{},
		Room:      room,
		MazeIndex: 2,
		Seed:      7,
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
			runtimeDungeonRoomKeyFromScene(scene): {Scene: scene, Room: room, Seed: 7, DropRNG: 7},
		},
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn: conn, connID: "quest-passive-object-reward", selectedCharacterID: 19,
		dungeon: dungeonSessionState{runtime: runtime},
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: bridgePVFSource{},
			itemCache: map[uint32]dungeonDropItemDefinition{
				10164801: {
					ItemID: 10164801, Kind: dungeonDropItemStackable, StackableType: "[quest]",
					StackLimit: 999, SlotStart: 177, SlotEnd: 232,
				},
			},
		},
	}

	for attempt := 0; attempt < 4; attempt++ {
		handled, handleErr := service.handleCurrentDungeonQuestPassiveObjectRewardDeathLocked(
			session, runtime, scene, 16111,
		)
		if handleErr != nil || !handled {
			t.Fatalf("attempt %d handled=%t err=%v", attempt+1, handled, handleErr)
		}
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	stack, found := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 177)]
	if !found || stack.ItemID != 10164801 || stack.Count != 3 {
		t.Fatalf("quest passive-object stack=%+v found=%t want item=10164801 count=3", stack, found)
	}
	visit := runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)]
	if got := visit.QuestPassiveObjectDeaths[16111]; got != 3 {
		t.Fatalf("retired passive-object count=%d want=3", got)
	}
	packets := 0
	remaining := conn.write.Bytes()
	for len(remaining) != 0 {
		_, remaining = splitGameServerUpperPacket(t, remaining)
		packets++
	}
	if packets != 7 {
		t.Fatalf("packet count=%d want=7", packets)
	}
}

func TestQuestPassiveObjectEnemyRewardAboveUint16GrantsCurrentPVFQuestItemsWithoutOp38(t *testing.T) {
	tests := []struct {
		name     string
		rows     string
		items    map[uint32]dungeonDropItemDefinition
		expected map[int64]int64
	}{
		{
			name: "discover tera stone",
			rows: "85176 3 310 -1 10164804 1 100 10\n",
			items: map[uint32]dungeonDropItemDefinition{
				10164804: {
					ItemID: 10164804, Kind: dungeonDropItemStackable, StackableType: "[quest]",
					StackLimit: 10, SlotStart: 177, SlotEnd: 232,
				},
			},
			expected: map[int64]int64{10164804: 1},
		},
		{
			name: "avoid imperial attention",
			rows: "85176 3 310 -1 10164804 1 100 10\n" +
				"85176 3 310 -1 10164805 2 100 20\n",
			items: map[uint32]dungeonDropItemDefinition{
				10164804: {
					ItemID: 10164804, Kind: dungeonDropItemStackable, StackableType: "[quest]",
					StackLimit: 10, SlotStart: 177, SlotEnd: 232,
				},
				10164805: {
					ItemID: 10164805, Kind: dungeonDropItemStackable, StackableType: "[quest]",
					StackLimit: 20, SlotStart: 177, SlotEnd: 232,
				},
			},
			expected: map[int64]int64{10164804: 1, 10164805: 2},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			index, err := dnfpvf.Build(ctx, bridgePVFSource{
				dnfquest.DefaultList: "4000 `tera_reward.qst`\n",
				"n_quest/tera_reward.qst": "[grade]\n`[epic]`\n" +
					"[type]\n`[seeking]`\n" +
					"[enemy reward item]\n" + test.rows + "[/enemy reward item]\n",
			}, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := dnfquest.Load(ctx, index)
			if err != nil {
				t.Fatal(err)
			}
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19"}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
				CharacterID: "19",
				States:      map[int64]dnfrepo.QuestState{4000: {Status: "active"}},
			}); err != nil {
				t.Fatal(err)
			}
			scene := worldmap.DungeonRoomScene{
				Coordinate:     worldmap.RoomCoordinate{X: 2, Y: 1},
				Map:            worldmap.ResolvedMap{Map: worldmap.Map{ID: 76721}},
				PassiveObjects: []worldmap.PassiveObject{{ObjectID: 85176}},
			}
			room := &runtimeDungeonRoom{coordinate: scene.Coordinate, mapID: scene.Map.Map.ID}
			runtime := &runtimeDungeonState{
				Request:   dungeoncmd.SelectDungeonRequest{DungeonID: 310, Difficulty: 3},
				Dungeon:   worldmap.Dungeon{ID: 310},
				Character: dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "acc"},
				Session:   &worldmap.DungeonSession{},
				Room:      room,
				MazeIndex: 5,
				Seed:      7,
				Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
					runtimeDungeonRoomKeyFromScene(scene): {Scene: scene, Room: room, Seed: 7, DropRNG: 7},
				},
			}
			conn := &bufferConn{}
			session := &gameSession{
				conn: conn, connID: "tera-quest-passive-object", selectedCharacterID: 19,
				dungeon: dungeonSessionState{runtime: runtime},
			}
			service := &Service{
				options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
				questCatalog:       catalog,
				repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
				pvfItemCatalog: &pvfDungeonDropCatalog{
					source:    bridgePVFSource{},
					itemCache: test.items,
				},
			}

			for attempt := 0; attempt < 2; attempt++ {
				handled, handleErr := service.handleCurrentDungeonQuestPassiveObjectRewardDeathLocked(
					session, runtime, scene, 85176,
				)
				if handleErr != nil || !handled {
					t.Fatalf("attempt %d handled=%t err=%v", attempt+1, handled, handleErr)
				}
			}
			inventory, found, err := repositories.Inventory.Load(ctx, "19")
			if err != nil || !found {
				t.Fatalf("load inventory found=%t err=%v", found, err)
			}
			actual := make(map[int64]int64, len(test.expected))
			for _, stack := range inventory.Slots {
				if _, wanted := test.expected[stack.ItemID]; wanted {
					actual[stack.ItemID] += stack.Count
				}
			}
			for itemID, want := range test.expected {
				if got := actual[itemID]; got != want {
					t.Fatalf("item=%d count=%d want=%d inventory=%+v", itemID, got, want, inventory.Slots)
				}
			}
			visit := runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)]
			if got := visit.QuestPassiveObjectDeaths[85176]; got != 1 {
				t.Fatalf("retired tera object count=%d want=1", got)
			}
			packet, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
			if packet.Header.MsgID != 14 || packet.Header.Classification != 0 || len(trailing) != 0 {
				t.Fatalf("above-u16 reward packets first=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
			}
		})
	}
}

func TestRealScriptPVFTeraQuestPassiveObjectRewards(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	}
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE or DNF_WORLDMAP_REAL_PVF_SMOKE to verify quests 3954..3957")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	expectedItems := map[int64][]dnfquest.EnemyRewardItemEntry{
		3954: {{EnemyCode: 85176, EnemyType: 3, DungeonID: 310, Difficulty: -1, ItemID: 10164804, Count: 1, DropRate: 100, MaxStack: 10}},
		3955: {{EnemyCode: 85176, EnemyType: 3, DungeonID: 310, Difficulty: -1, ItemID: 10164804, Count: 1, DropRate: 100, MaxStack: 10}},
		3956: {
			{EnemyCode: 85176, EnemyType: 3, DungeonID: 310, Difficulty: -1, ItemID: 10164804, Count: 1, DropRate: 100, MaxStack: 10},
			{EnemyCode: 85176, EnemyType: 3, DungeonID: 310, Difficulty: -1, ItemID: 10164805, Count: 2, DropRate: 100, MaxStack: 20},
		},
		3957: {
			{EnemyCode: 85176, EnemyType: 3, DungeonID: 310, Difficulty: -1, ItemID: 10164804, Count: 1, DropRate: 100, MaxStack: 10},
			{EnemyCode: 85176, EnemyType: 3, DungeonID: 310, Difficulty: -1, ItemID: 10164805, Count: 2, DropRate: 100, MaxStack: 20},
		},
	}
	for questID, want := range expectedItems {
		definition, found := catalog.Find(questID)
		if !found {
			t.Fatalf("real quest %d missing", questID)
		}
		if len(definition.EnemyRewardItems) != len(want) {
			t.Fatalf("quest %d enemy reward rows=%+v want=%+v", questID, definition.EnemyRewardItems, want)
		}
		for index := range want {
			if definition.EnemyRewardItems[index] != want[index] {
				t.Fatalf("quest %d row %d=%+v want=%+v", questID, index, definition.EnemyRewardItems[index], want[index])
			}
		}
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []uint32{10164804, 10164805} {
		definition, err := items.ResolveItem(itemID)
		if err != nil {
			t.Fatalf("resolve quest item %d: %v", itemID, err)
		}
		if definition.Kind != dungeonDropItemStackable || normalizeDungeonPVFSymbol(definition.StackableType) != "quest" ||
			definition.SlotStart != 177 || definition.SlotEnd != 232 {
			t.Fatalf("quest item %d definition=%+v", itemID, definition)
		}
	}
	objectText, err := archive.ReadText("passiveobject/actionobject/map/cataclysm/metrocenter/tera/85176_tera_quest_2.obj")
	if err != nil {
		t.Fatal(err)
	}
	objectText = strings.ToLower(objectText)
	if !strings.Contains(objectText, "[hp destroy]") || !strings.Contains(objectText, "[quest item]") {
		t.Fatalf("passive object 85176 is not a destructible quest-item object: %s", objectText)
	}
	for _, stage := range []string{"3954_", "3955_", "3956_", "3957_"} {
		found := false
		for _, file := range archive.Files() {
			path := strings.ToLower(file.ArchivePath)
			if !strings.HasPrefix(path, "map/cataclysm/metrocenter/01_time_plaza/"+stage) {
				continue
			}
			text, readErr := archive.ReadText(file.ArchivePath)
			if readErr == nil && strings.Contains(text, "85176") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("real Time Plaza stage %s has no passive object 85176", stage)
		}
	}
}
