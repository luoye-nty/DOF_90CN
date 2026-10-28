package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestBosslessPVFTutorialFinalCompletesAfterAllRealBlockersDie(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		bossRank:      "[normal]",
		singleMonster: true,
	})
	service.dungeonTutorialScripts = &pvfDungeonTutorialScriptCatalog{
		byMapID: make(map[int64]map[int][]dungeonTutorialScriptEvidence),
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "bossless-tutorial-final-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if err := service.handleDungeonMonsterDeath(session, ordinaryTutorialDeathBody(targetKey, 99)); err != nil {
		t.Fatal(err)
	}

	death, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) {
		t.Fatalf("first packet=%+v body=%x", death.Header, death.Body)
	}
	entry, rest := splitGameServerUpperPacket(t, rest)
	if entry.Header.MsgID != currentDungeonSettlementEntryMsgID || !bytes.Equal(entry.Body, []byte{0}) || len(rest) != 0 {
		t.Fatalf("bossless settlement entry=%+v body=%x rest=%x", entry.Header, entry.Body, rest)
	}
	snapshot := runtime.Session.Snapshot()
	if snapshot.Run.Status != worldmap.DungeonRunCompleted || !snapshot.Scene.Cleared ||
		!runtime.tutorialFinalRoomClearAccepted || runtime.tutorialFinalRoomClearPending ||
		!runtime.tutorialCompletionPersisted || !runtime.settlementEntrySent ||
		runtime.bossDieCheckAccepted || runtime.bossDieCheckResponseSent {
		t.Fatalf("bossless final state=%+v runtime=%+v", snapshot, runtime)
	}
	assertTutorialCompletionBirthRoomPersisted(t, service, 99)
}

func TestCMTTutorialFinalGivesImmediateOp117PriorityThenFallsBack(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		bossRank:      "[normal]",
		singleMonster: true,
	})
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "cmt-tutorial-final-grace-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if err := service.handleDungeonMonsterDeath(session, ordinaryTutorialDeathBody(targetKey, 99)); err != nil {
		t.Fatal(err)
	}
	death, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) || len(rest) != 0 ||
		!runtime.tutorialFinalRoomClearPending || runtime.tutorialFinalRoomClearAccepted ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive {
		t.Fatalf("grace state death=%+v rest=%x runtime=%+v", death.Header, rest, runtime)
	}
	task := queue.task(t, 0)
	if task.name != "dnf-dungeon-tutorial-final:cmt-tutorial-final-grace-test:run:0" ||
		task.delay != 750*time.Millisecond {
		t.Fatalf("tutorial grace task=%+v", task)
	}

	conn.write.Reset()
	queue.fire(task, false)
	entry, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if entry.Header.MsgID != currentDungeonSettlementEntryMsgID || !bytes.Equal(entry.Body, []byte{0}) || len(rest) != 0 {
		t.Fatalf("fallback entry=%+v body=%x rest=%x", entry.Header, entry.Body, rest)
	}
	if !runtime.tutorialFinalRoomClearAccepted || runtime.tutorialFinalRoomClearPending ||
		runtime.bossDieCheckAccepted || runtime.bossDieCheckResponseSent {
		t.Fatalf("fallback incorrectly used op117 owner=%+v", runtime)
	}
}

func TestCMTTutorialFinalOp117WinsBeforeFallback(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		bossRank:      "[normal]",
		singleMonster: true,
	})
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "cmt-tutorial-final-op117-wins-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if err := service.handleDungeonMonsterDeath(session, ordinaryTutorialDeathBody(targetKey, 99)); err != nil {
		t.Fatal(err)
	}
	if !runtime.tutorialFinalRoomClearPending {
		t.Fatal("CMT final did not arm op117 grace")
	}
	conn.write.Reset()
	if err := service.handleDungeonBossDieCheck(session, bossDieCheckRequestBody(99, uint16(targetKey))); err != nil {
		t.Fatal(err)
	}
	assertBossResponseAndSettlementEntry(t, conn.write.Bytes(), uint16(targetKey))
	if !runtime.bossDieCheckAccepted || !runtime.bossDieCheckResponseSent ||
		runtime.tutorialFinalRoomClearAccepted || runtime.tutorialFinalRoomClearPending {
		t.Fatalf("op117 did not retain settlement ownership=%+v", runtime)
	}
}

func TestBossRankTutorialFinalNeverUsesBosslessFallback(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{singleMonster: true})
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "boss-rank-no-fallback-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if err := service.handleDungeonMonsterDeath(session, ordinaryTutorialDeathBody(targetKey, 99)); err != nil {
		t.Fatal(err)
	}
	_, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 || runtime.tutorialFinalRoomClearPending ||
		runtime.tutorialFinalRoomClearAccepted || runtime.settlementEntrySent {
		t.Fatalf("boss-ranked final entered fallback rest=%x runtime=%+v", rest, runtime)
	}
}

// TestBossRankTutorialFinalCompletesAfterTheClientSpentItsOp117 covers the
// live hang a freshly created character hit in tutorial 7112 room 4:0: the
// client sent its single op117 for the boss at 08:58:51.79 - 14ms before that
// boss's own op39, so the server dropped it as not-yet-defeated - then killed
// the room's last ordinary blocker at 08:58:58.62. The boss-rank branch went
// on waiting for a second op117 that the client never sends, and the tutorial
// could only be left by force-quitting.
//
// Once the sighting is recorded for this room the branch has to fall through
// to the same grace timer the CMT branch uses.
func TestBossRankTutorialFinalCompletesAfterTheClientSpentItsOp117(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{singleMonster: true})
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "boss-rank-spent-op117-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	scene := runtime.Session.Snapshot().Scene
	// The client already spent its op117 in this room before the last blocker
	// died, exactly as the live run did.
	runtime.bossDieCheckObserved = true
	runtime.bossDieCheckObservedRoom = scene.Coordinate
	runtime.bossDieCheckObservedMapID = scene.Map.Map.ID

	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if err := service.handleDungeonMonsterDeath(session, ordinaryTutorialDeathBody(targetKey, 99)); err != nil {
		t.Fatal(err)
	}
	if !runtime.tutorialFinalRoomClearPending || runtime.tutorialFinalRoomClearAccepted {
		t.Fatalf("spent-op117 boss final did not arm the grace timer: %+v", runtime)
	}
	task := queue.task(t, 0)
	if task.delay != 750*time.Millisecond {
		t.Fatalf("tutorial grace task=%+v", task)
	}
	conn.write.Reset()
	queue.fire(task, false)
	entry, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if entry.Header.MsgID != currentDungeonSettlementEntryMsgID || !bytes.Equal(entry.Body, []byte{0}) || len(rest) != 0 {
		t.Fatalf("settlement entry=%+v body=%x rest=%x", entry.Header, entry.Body, rest)
	}
	if !runtime.tutorialFinalRoomClearAccepted || runtime.tutorialFinalRoomClearPending {
		t.Fatalf("tutorial final never completed: %+v", runtime)
	}
}

// TestBossRankTutorialFinalStillDefersWhenOp117WasSpentElsewhere keeps the
// marker room-scoped: an op117 spent in an earlier room must not make a later
// boss-ranked final skip its op117 owner.
func TestBossRankTutorialFinalStillDefersWhenOp117WasSpentElsewhere(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{singleMonster: true})
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "boss-rank-spent-elsewhere-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	scene := runtime.Session.Snapshot().Scene
	runtime.bossDieCheckObserved = true
	runtime.bossDieCheckObservedRoom = scene.Coordinate
	runtime.bossDieCheckObservedMapID = scene.Map.Map.ID + 1

	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if err := service.handleDungeonMonsterDeath(session, ordinaryTutorialDeathBody(targetKey, 99)); err != nil {
		t.Fatal(err)
	}
	if runtime.tutorialFinalRoomClearPending || runtime.tutorialFinalRoomClearAccepted ||
		runtime.settlementEntrySent {
		t.Fatalf("op117 spent in another room wrongly released the final: %+v", runtime)
	}
}

func ordinaryTutorialDeathBody(objectKey uint32, ownerObjectKey uint16) []byte {
	body := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(body[0:4], objectKey)
	binary.LittleEndian.PutUint16(body[4:6], ownerObjectKey)
	return body
}

func assertTutorialCompletionBirthRoomPersisted(t *testing.T, service *Service, characterID uint16) {
	t.Helper()
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Character == nil {
		t.Fatal("tutorial character repository unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), strconv.Itoa(int(characterID)))
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	want := map[string]int64{
		currentDungeonTutorialCompletedKey: currentDungeonTutorialCompleteFlag,
		"town_id":                          newCharacterInitialTownID,
		"area_id":                          newCharacterInitialAreaID,
		"pos_x":                            newCharacterInitialPosX,
		"pos_y":                            newCharacterInitialPosY,
		"direction":                        newCharacterInitialDirection,
		"area_state":                       newCharacterInitialAreaState,
	}
	for key, value := range want {
		if character.Stats[key] != value {
			t.Fatalf("persisted %s=%d want=%d stats=%v", key, character.Stats[key], value, character.Stats)
		}
	}
	if !bytes.Equal([]byte(character.Name), []byte("ATSwordman")) {
		t.Fatalf("completion changed character base record=%+v", character)
	}
}
