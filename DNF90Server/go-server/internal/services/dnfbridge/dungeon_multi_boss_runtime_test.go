package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentPVFMultipleOrdinaryBossesRequireSeparateDeaths(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial: true,
	})
	runtime.Room.mu.Lock()
	if len(runtime.Room.monsters) != 2 {
		runtime.Room.mu.Unlock()
		t.Fatalf("fixture monster count=%d want=2", len(runtime.Room.monsters))
	}
	runtime.Room.monsters[1].Spawn.Rank = "[boss]"
	runtime.Room.mu.Unlock()
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}

	room := runtime.Room.Snapshot()
	firstBossKey := room.Monsters[0].ObjectKey
	questBossKey := room.Monsters[1].ObjectKey
	initialScene := runtime.Session.Snapshot().Scene
	gate, active := currentDungeonPVFMultiBossDeathGate(runtime, initialScene)
	if !active || gate.Ready || len(gate.BossObjectKeys) != 2 {
		t.Fatalf("initial multi-boss gate active=%t gate=%+v", active, gate)
	}
	nonBlockingScene := initialScene
	nonBlockingScene.BlockingHostiles = nonBlockingScene.BlockingHostiles[:1]
	if _, active := currentDungeonPVFMultiBossDeathGate(runtime, nonBlockingScene); active {
		t.Fatal("a non-blocking second boss row activated the multi-boss gate")
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "current-pvf-multi-ordinary-boss-separate-deaths",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonMonsterDeath(session, storyAIBossDeathBody(firstBossKey)); err != nil {
		t.Fatal(err)
	}
	if got := storyAIBossDeathNotificationKeys(t, connection.write.Bytes()); len(got) != 1 || got[0] != firstBossKey {
		t.Fatalf("first death notifications=%v want only first boss %d", got, firstBossKey)
	}
	afterFirst := runtime.Session.Snapshot()
	afterFirstRoom := runtime.Room.Snapshot()
	if afterFirst.Run.Status != worldmap.DungeonRunActive || afterFirst.Scene.Cleared ||
		runtime.ordinaryFinalRoomClearAccepted || runtime.settlementEntrySent {
		t.Fatalf("first boss death completed room early: runtime=%+v snapshot=%+v", runtime, afterFirst)
	}
	if afterFirstRoom.Monsters[0].State != runtimeDungeonMonsterDefeated ||
		afterFirstRoom.Monsters[1].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("first boss fabricated quest boss death: room=%+v", afterFirstRoom.Monsters)
	}
	gate, active = currentDungeonPVFMultiBossDeathGate(runtime, afterFirst.Scene)
	if !active || gate.Ready {
		t.Fatalf("first death multi-boss gate active=%t gate=%+v", active, gate)
	}

	connection.write.Reset()
	if err := service.handleDungeonBossDieCheck(
		session,
		bossDieCheckRequestBody(99, uint16(firstBossKey)),
	); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 || runtime.bossDieCheckAccepted || runtime.settlementEntrySent {
		t.Fatalf("first boss op117 bypassed remaining boss: runtime=%+v bytes=%x", runtime, connection.write.Bytes())
	}
	if runtime.Room.Snapshot().Monsters[1].State != runtimeDungeonMonsterAnnounced {
		t.Fatal("first boss op117 retired the quest-drop boss")
	}

	connection.write.Reset()
	if err := service.handleDungeonMonsterDeath(session, storyAIBossDeathBody(questBossKey)); err != nil {
		t.Fatal(err)
	}
	if got := storyAIBossDeathNotificationKeys(t, connection.write.Bytes()); len(got) != 1 || got[0] != questBossKey {
		t.Fatalf("second death notifications=%v want only quest boss %d", got, questBossKey)
	}
	completed := runtime.Session.Snapshot()
	if completed.Run.Status != worldmap.DungeonRunCompleted || !completed.Scene.Cleared ||
		!runtime.ordinaryFinalRoomClearAccepted || !runtime.settlementEntrySent {
		t.Fatalf("two authoritative boss deaths did not complete room: runtime=%+v snapshot=%+v", runtime, completed)
	}
	gate, active = currentDungeonPVFMultiBossDeathGate(runtime, completed.Scene)
	if !active || !gate.Ready {
		t.Fatalf("completed multi-boss gate active=%t gate=%+v", active, gate)
	}
}

func TestCurrentPVFMultiBossGateDoesNotCountStoryAIAsOrdinaryBoss(t *testing.T) {
	_, runtime, _, _ := prepareStoryAIBossDeathGateRuntime(t)
	scene := runtime.Session.Snapshot().Scene
	if _, active := currentDungeonPVFMultiBossDeathGate(runtime, scene); active {
		t.Fatal("story AI plus one ordinary dummy was misclassified as multiple ordinary PVF boss rows")
	}
}

func TestRealScriptPVFLukeScientistDutyMechanicalThroneDropOwner(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	}
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the Mechanical Throne quest-drop owner")
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
	definition, found := questCatalog.Find(2902)
	if !found || !strings.EqualFold(definition.Path, "n_quest/new_quest/metrocenter/daily_90_metro_02.qst") ||
		definition.Name != "[卢克]科学家的义务" || definition.Type != "[seeking]" {
		t.Fatalf("quest 2902 definition=%+v found=%t", definition, found)
	}
	candidates := currentQuestDropMatchMonster(questCatalog, []int64{2902}, 5106, 0, 66184)
	if len(candidates) != 1 || candidates[0] != (currentQuestDropCandidate{
		QuestID: 2902, ItemID: 10159407, Count: 1, DropRate: 100, MaxStack: 1,
	}) {
		t.Fatalf("Mechanical Throne essence candidate=%+v", candidates)
	}
	if got := currentQuestDropMatchMonster(questCatalog, []int64{2902}, 5106, 0, 66468); len(got) != 0 {
		t.Fatalf("self-retiring Luke fixture incorrectly owns the quest drop: %+v", got)
	}

	worldMap, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := worldMap.FindDungeon(5106)
	if !found || dungeon.Metadata.Name != "机械王座" {
		t.Fatalf("dungeon 5106=%+v found=%t", dungeon, found)
	}
	bossMap, found := worldMap.FindMap(55956)
	if !found || len(bossMap.Monsters) != 2 {
		t.Fatalf("Mechanical Throne boss map=%+v found=%t", bossMap, found)
	}
	if bossMap.Monsters[0].MonsterID != 66468 || bossMap.Monsters[1].MonsterID != 66184 ||
		normalizeDungeonPVFSymbol(bossMap.Monsters[0].Rank) != "boss" ||
		normalizeDungeonPVFSymbol(bossMap.Monsters[1].Rank) != "boss" {
		t.Fatalf("Mechanical Throne boss rows=%+v", bossMap.Monsters)
	}

	monsterCatalog, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	lukeFixture, found, err := monsterCatalog.Find(66468)
	if err != nil || !found || !strings.Contains(strings.ToLower(lukeFixture.Path), "/luke_small/") {
		t.Fatalf("Luke fixture=%+v found=%t err=%v", lukeFixture, found, err)
	}
	machineGolem, found, err := monsterCatalog.Find(66184)
	if err != nil || !found || !strings.Contains(strings.ToLower(machineGolem.Path), "/machinegolem/") {
		t.Fatalf("Machine Golem=%+v found=%t err=%v", machineGolem, found, err)
	}

	itemCatalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	essence, err := itemCatalog.ResolveItem(10159407)
	if err != nil || essence.Kind != dungeonDropItemStackable ||
		normalizeDungeonPVFSymbol(essence.StackableType) != "quest" {
		t.Fatalf("Mechanical Throne essence item=%+v err=%v", essence, err)
	}
}
