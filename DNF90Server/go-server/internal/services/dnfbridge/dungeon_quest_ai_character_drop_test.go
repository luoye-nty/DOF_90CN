package dnfbridge

import (
	"context"
	"encoding/hex"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestHostileAICharacterDeathGrantsTypeTwoEnemyRewardItem(t *testing.T) {
	ctx := context.Background()
	questIndex, err := dnfpvf.Build(ctx, bridgePVFSource{
		dnfquest.DefaultList: "2949 `mission_archaeology_03.qst`\n",
		"n_quest/mission_archaeology_03.qst": "[grade]\n`[mission]`\n" +
			"[type]\n`[seeking]`\n[int data]\n10164883 1\n" +
			"[enemy reward item]\n323 2 700 1 10164883 1 100 1\n[/enemy reward item]\n",
	}, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	questCatalog, err := dnfquest.Load(ctx, questIndex)
	if err != nil {
		t.Fatal(err)
	}

	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] += "[ai character]\n323 10 20 0 `[monster]` `[boss]` 0 0\n"
	source[defaultDungeonAICharacterList] = "323 `Test/QuestEnemy.aic`\n"
	source["AICharacter/Test/QuestEnemy.aic"] = "[minimum info]\n`Quest Enemy APC` 1 2 3 4 25\n"
	aiCatalog, err := newPVFDungeonAICharacterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, source, aiCatalog)
	if matches := currentQuestDropMatchEnemy(
		questCatalog,
		[]int64{2949},
		runtime.Dungeon.ID,
		int64(runtime.Request.Difficulty),
		323,
		currentDungeonQuestEnemyTypeAICharacter,
	); len(matches) != 1 {
		t.Fatalf("quest AI reward matches=%+v dungeon=%d difficulty=%d", matches, runtime.Dungeon.ID, runtime.Request.Difficulty)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	if code, found := runtime.Room.AnnouncedHostileAICharacterCode(403); !found || code != 323 {
		t.Fatalf("announced hostile AI code=%d found=%t", code, found)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "404", AccountID: "account-1", Level: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "404"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "404",
		States: map[int64]dnfrepo.QuestState{
			2949: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn: connection, connID: "quest-ai-character-drop", selectedCharacterID: 404,
		dungeon: dungeonSessionState{runtime: runtime},
	}
	service := &Service{
		options:            options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       questCatalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: bridgePVFSource{},
			itemCache: map[uint32]dungeonDropItemDefinition{
				10164883: {
					ItemID: 10164883, Kind: dungeonDropItemStackable, StackableType: "[quest]",
					StackLimit: 1, SlotStart: 177, SlotEnd: 232,
				},
			},
		},
	}
	// Current-client hostile APC op39 captured by the existing dungeon actor
	// regression: runtime object 403, player owner 404, one combat row.
	deathBody, err := hex.DecodeString("9301000094010000000000000000910d0000130000000211019401bf0b00000d001101130094010000060064024d01000000001e3c0001000000430d00000000000064024d0164024d0112002f0003000000300280d6")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonMonsterDeath(session, deathBody); err != nil {
		t.Fatal(err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "404")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	stack, found := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 177)]
	if !found || stack.ItemID != 10164883 || stack.Count != 1 {
		t.Fatalf("quest AI reward stack=%+v found=%t", stack, found)
	}
	if _, found := runtime.Room.AnnouncedHostileAICharacterCode(403); found {
		t.Fatal("defeated AI character remained eligible for a duplicate quest reward")
	}

	op14Count := 0
	remaining := connection.write.Bytes()
	for len(remaining) != 0 {
		packet, rest := splitGameServerUpperPacket(t, remaining)
		if packet.Header.MsgID == 14 {
			op14Count++
		}
		remaining = rest
	}
	if op14Count != 1 {
		t.Fatalf("quest item incremental refresh count=%d want=1", op14Count)
	}
}

func TestRealScriptPVFMission2949MountainTearUsesHostileAIReward(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify mission 2949 山之泪")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists: []string{dnfquest.DefaultList}, Prefixes: []string{"n_quest/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	questCatalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := questCatalog.Find(2949)
	if !found || definition.Type != "[seeking]" || len(definition.EnemyRewardItems) != 2 {
		t.Fatalf("quest 2949 definition=%+v found=%t", definition, found)
	}
	for _, difficulty := range []int64{1, 2} {
		matches := currentQuestDropMatchEnemy(
			questCatalog,
			[]int64{2949},
			42,
			difficulty,
			323,
			currentDungeonQuestEnemyTypeAICharacter,
		)
		if len(matches) != 1 || matches[0] != (currentQuestDropCandidate{
			QuestID: 2949, ItemID: 10164883, Count: 1, DropRate: 100, MaxStack: 1,
		}) {
			t.Fatalf("quest 2949 difficulty=%d matches=%+v", difficulty, matches)
		}
	}
	if matches := currentQuestDropMatchEnemy(
		questCatalog, []int64{2949}, 42, 0, 323, currentDungeonQuestEnemyTypeAICharacter,
	); len(matches) != 0 {
		t.Fatalf("normal difficulty unexpectedly granted 山之泪: %+v", matches)
	}

	worldMap, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := worldMap.FindDungeon(42)
	if !found {
		t.Fatal("real PVF dungeon 42 利库天井 is missing")
	}
	actorFound := false
	for _, maze := range dungeon.Mazes {
		for _, specification := range maze.MapSpecifications {
			for _, mapID := range specification.MapIDs {
				mapValue, ok := worldMap.FindMap(mapID)
				if !ok {
					continue
				}
				for _, actor := range mapValue.AICharacters {
					if actor.Code == 323 && normalizeDungeonPVFSymbol(actor.Faction) == "monster" {
						actorFound = true
					}
				}
			}
		}
	}
	if !actorFound {
		t.Fatal("dungeon 42 has no hostile AI character 323 for quest 2949")
	}

	itemCatalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	item, err := itemCatalog.ResolveItem(10164883)
	if err != nil || item.Kind != dungeonDropItemStackable ||
		normalizeDungeonPVFSymbol(item.StackableType) != "quest" {
		t.Fatalf("山之泪 item=%+v err=%v", item, err)
	}
}
