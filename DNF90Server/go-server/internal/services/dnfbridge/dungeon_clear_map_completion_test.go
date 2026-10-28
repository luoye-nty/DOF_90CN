package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonAllRoomsClearedRequiresEveryTopologyCoordinate(t *testing.T) {
	rooms := []worldmap.DungeonRoom{
		{Coordinate: worldmap.RoomCoordinate{X: 0, Y: 0}},
		{Coordinate: worldmap.RoomCoordinate{X: 1, Y: 0}},
	}
	if currentDungeonAllRoomsCleared(nil, nil) {
		t.Fatal("empty topology must not count as all rooms cleared")
	}
	if currentDungeonAllRoomsCleared(rooms, []worldmap.RoomCoordinate{{X: 0, Y: 0}}) {
		t.Fatal("partial cleared set must not count as all rooms cleared")
	}
	if !currentDungeonAllRoomsCleared(rooms, []worldmap.RoomCoordinate{{X: 1, Y: 0}, {X: 0, Y: 0}}) {
		t.Fatal("complete cleared set must count as all rooms cleared")
	}
}

func TestBossDieCheckPersistsRealClearMapPhaseABeforeSettlementAcrossDungeonKinds(t *testing.T) {
	for _, test := range []struct {
		name     string
		tutorial bool
	}{
		{name: "ordinary", tutorial: false},
		{name: "tutorial", tutorial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := tutorialScopeFixtureOptions{singleMonster: true}
			if !test.tutorial {
				fixture.disableTutorial = true
			}
			service, runtime := prepareTutorialScopeRuntime(t, fixture)
			repositories, ok := service.repositoryGroup()
			if !ok {
				t.Fatal("repository group unavailable")
			}
			mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
			service.questCatalog = buildDungeonClearMapCompletionCatalog(t, mapID)
			if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
				CharacterID: "99",
				States: map[int64]dnfrepo.QuestState{
					3145: {Status: "active", ProgressValue: 1, Extra: map[string]string{"kept": "yes"}},
					3146: {Status: "active", ProgressValue: 7},
				},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
				t.Fatal(err)
			}
			targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
			if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
				t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
			}

			conn := &bufferConn{}
			session := &gameSession{
				conn: conn, connID: "clear-map-phase-a-" + test.name,
				selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
			}
			if err := service.handleDungeonBossDieCheck(session, bossDieCheckRequestBody(99, uint16(targetKey))); err != nil {
				t.Fatal(err)
			}
			if test.tutorial {
				assertBossResponseAndSettlementEntry(t, conn.write.Bytes(), uint16(targetKey))
			} else {
				assertClearMapNotificationThenBossResponseAndSettlementEntry(
					t,
					conn.write.Bytes(),
					uint16(targetKey),
					3145,
				)
			}
			if !runtime.clearMapCompletionPhaseAPersisted || runtime.clearMapCompletionKey == "" ||
				runtime.clearMapCompletionAt.IsZero() || !runtime.bossDieCheckResponseSent || !runtime.settlementEntrySent {
				t.Fatalf("phase A/settlement state=%+v", runtime)
			}
			persisted, found, err := repositories.Quest.Load(context.Background(), "99")
			if err != nil || !found {
				t.Fatalf("load persisted quest found=%t err=%v", found, err)
			}
			completed := persisted.States[3145]
			if completed.Status != "active" || completed.ProgressValue != 0 ||
				completed.Extra["completion_key"] != runtime.clearMapCompletionKey ||
				completed.Extra["reward_state"] != "pending" ||
				completed.Extra["completion_dungeon_id"] != fmt.Sprint(runtime.Dungeon.ID) ||
				completed.Extra["completion_map_id"] != fmt.Sprint(mapID) ||
				completed.Extra["kept"] != "yes" {
				t.Fatalf("completed clear-map state=%+v runtime_key=%q", completed, runtime.clearMapCompletionKey)
			}
			if other := persisted.States[3146]; other.ProgressValue != 7 || other.Extra["completion_key"] != "" {
				t.Fatalf("unrelated quest mutated=%+v", other)
			}
			if !strings.Contains(runtime.clearMapCompletionKey, fmt.Sprintf(":dungeon:%d:maze:%d:map:%d:", runtime.Dungeon.ID, runtime.MazeIndex, mapID)) {
				t.Fatalf("completion key lacks owned run facts: %q", runtime.clearMapCompletionKey)
			}
			if test.tutorial {
				assertTutorialCompletionPersistedAndSelectAcked(t, service, 99)
			} else {
				character, found, err := repositories.Character.Load(context.Background(), "99")
				if err != nil || !found || character.Stats[currentDungeonTutorialCompletedKey] != 0 {
					t.Fatalf("ordinary clear-map mutated tutorial state found=%t err=%v character=%+v", found, err, character)
				}
			}
		})
	}
}

func TestBossDieCheckAutoAcksClearMapTriggerAfterSettlementEntry(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonClearMapCompletionCatalog(t, mapID)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn: connection, connID: "clear-map-auto-op33", selectedCharacterID: 99,
		dungeon: dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonBossDieCheck(session, bossDieCheckRequestBody(99, uint16(targetKey))); err != nil {
		t.Fatal(err)
	}

	op574, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if op574.Header.MsgID != currentActiveQuestSnapshotMsgID || op574.Header.Classification != 0 {
		t.Fatalf("first packet header=%+v", op574.Header)
	}
	op115, rest := splitGameServerUpperPacket(t, rest)
	if op115.Header.MsgID != uint16(dnfenum.CmdPacketNotifyBossDieCheck) {
		t.Fatalf("second packet header=%+v", op115.Header)
	}
	op31, rest := splitGameServerUpperPacket(t, rest)
	if op31.Header.MsgID != currentDungeonSettlementEntryMsgID || op31.Header.Classification != 0 ||
		!bytes.Equal(op31.Body, []byte{0}) {
		t.Fatalf("third packet header=%+v body=%x", op31.Header, op31.Body)
	}
	op33, rest := splitGameServerUpperPacket(t, rest)
	if len(rest) != 0 || op33.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		op33.Header.Classification != dnfproto.DefaultChannelClassification || len(op33.Body) != 7 ||
		!bytes.Equal(op33.Body[:3], []byte{1, 0x49, 0x0c}) || binary.LittleEndian.Uint32(op33.Body[3:7]) != 0 {
		t.Fatalf("auto op33 header=%+v body=%x rest=%x", op33.Header, op33.Body, rest)
	}
	key := newCurrentSetQuestTriggerReplayKey(99, dnfquest.SetTriggerRequest{QuestID: 3145})
	if !session.currentSetQuestTriggerReplaySuppressed(key) {
		t.Fatal("auto op33 did not suppress the matching client replay")
	}
	persisted, found, err := repositories.Quest.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load persisted quest found=%t err=%v", found, err)
	}
	state := persisted.States[3145]
	if state.Status != "active" || state.ProgressValue != 0 || state.Extra["reward_state"] != "pending" {
		t.Fatalf("auto op33 changed clear-map state=%+v", state)
	}

	connection.write.Reset()
	clientOp33 := []byte{0x21, 0x00, 0x49, 0x0c, 0x00, 0x00}
	if err := service.handleCurrentSetQuestTrigger(session, clientOp33); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("duplicate client op33 emitted an ACK=%x", connection.write.Bytes())
	}
}

func TestBossDieCheckRetriesOnlyUnsentClearMapOp33AfterSocketFailure(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonClearMapCompletionCatalog(t, mapID)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99", States: map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}
	wantErr := errors.New("clear-map automatic op33 write failed")
	failing := &failNthDungeonWriteConn{failAt: 4, err: wantErr}
	session := &gameSession{
		conn: failing, connID: "clear-map-auto-op33-retry", selectedCharacterID: 99,
		dungeon: dungeonSessionState{runtime: runtime},
	}
	request := bossDieCheckRequestBody(99, uint16(targetKey))
	if err := service.handleDungeonBossDieCheck(session, request); !errors.Is(err, wantErr) {
		t.Fatalf("automatic op33 failure=%v want=%v", err, wantErr)
	}
	if !runtime.settlementEntrySent {
		t.Fatalf("op31 was not retained after op33 failure: %+v", runtime)
	}
	if session.currentSetQuestTriggerReplaySuppressed(newCurrentSetQuestTriggerReplayKey(99, dnfquest.SetTriggerRequest{QuestID: 3145})) {
		t.Fatal("failed op33 was marked as suppressed")
	}
	first, rest := splitGameServerUpperPacket(t, failing.write.Bytes())
	if first.Header.MsgID != currentActiveQuestSnapshotMsgID {
		t.Fatalf("first packet after failure=%+v", first.Header)
	}
	second, rest := splitGameServerUpperPacket(t, rest)
	third, rest := splitGameServerUpperPacket(t, rest)
	if second.Header.MsgID != uint16(dnfenum.CmdPacketNotifyBossDieCheck) || third.Header.MsgID != currentDungeonSettlementEntryMsgID || len(rest) != 0 {
		t.Fatalf("failed op33 prefix=%d->%d trailing=%x", second.Header.MsgID, third.Header.MsgID, rest)
	}

	connection := &bufferConn{}
	session.conn = connection
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatal(err)
	}
	op33, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || op33.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) || len(op33.Body) != 7 ||
		binary.LittleEndian.Uint16(op33.Body[1:3]) != 3145 || binary.LittleEndian.Uint32(op33.Body[3:7]) != 0 {
		t.Fatalf("retry op33 header=%+v body=%x rest=%x", op33.Header, op33.Body, rest)
	}
}

func TestBossDieCheckClearMapPhaseAFailureDoesNotBlockSettlementAndReplaysDeterministically(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	baseRepositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonClearMapCompletionCatalog(t, mapID)
	if err := baseRepositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("clear-map phase A save failed")
	failingStore := &clearMapCountingQuestStore{QuestRepository: baseRepositories.Quest, err: wantErr}
	failingRepositories := baseRepositories
	failingRepositories.Quest = failingStore
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return failingRepositories, true }
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn: conn, connID: "clear-map-phase-a-failure",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
	}
	request := bossDieCheckRequestBody(99, uint16(targetKey))
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatalf("phase-A persistence failure must keep connection alive: %v", err)
	}
	assertBossResponseAndSettlementEntry(t, conn.write.Bytes(), uint16(targetKey))
	if runtime.clearMapCompletionPhaseAPersisted || !runtime.bossDieCheckResponseSent || !runtime.settlementEntrySent {
		t.Fatalf("failed Phase A state=%+v", runtime)
	}
	if failingStore.saves.Load() != 1 || runtime.clearMapCompletionKey == "" || runtime.clearMapCompletionAt.IsZero() {
		t.Fatalf("failed Phase A saves=%d key=%q at=%v", failingStore.saves.Load(), runtime.clearMapCompletionKey, runtime.clearMapCompletionAt)
	}
	stableKey, stableAt := runtime.clearMapCompletionKey, runtime.clearMapCompletionAt
	stored, found, err := baseRepositories.Quest.Load(context.Background(), "99")
	if err != nil || !found || stored.States[3145].ProgressValue != 1 {
		t.Fatalf("failed Phase A mutated quest found=%t err=%v state=%+v", found, err, stored.States[3145])
	}

	conn.write.Reset()
	countingStore := &clearMapCountingQuestStore{QuestRepository: baseRepositories.Quest}
	retryRepositories := baseRepositories
	retryRepositories.Quest = countingStore
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return retryRepositories, true }
	conn.write.Reset()
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatal(err)
	}
	notification, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	assertClearMapCompletionNotification(t, notification, 3145)
	autoAck, rest := splitGameServerUpperPacket(t, rest)
	if autoAck.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		autoAck.Header.Classification != dnfproto.DefaultChannelClassification || len(autoAck.Body) != 7 ||
		binary.LittleEndian.Uint16(autoAck.Body[1:3]) != 3145 || binary.LittleEndian.Uint32(autoAck.Body[3:7]) != 0 || len(rest) != 0 {
		t.Fatalf("retry must repair clear-map notification then auto-ack op33, header=%+v body=%x rest=%x", autoAck.Header, autoAck.Body, rest)
	}
	if countingStore.saves.Load() != 1 || !runtime.clearMapCompletionPhaseAPersisted ||
		runtime.clearMapCompletionKey != stableKey || !runtime.clearMapCompletionAt.Equal(stableAt) {
		t.Fatalf("retry Phase A saves=%d state=%+v", countingStore.saves.Load(), runtime)
	}
}

func TestBossDieCheckClearMapNotificationWriteFailureDoesNotBlockSettlementAndReplaysWithoutPhaseAWrite(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	baseRepositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonClearMapCompletionCatalog(t, mapID)
	if err := baseRepositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	countingStore := &clearMapCountingQuestStore{QuestRepository: baseRepositories.Quest}
	repositories := baseRepositories
	repositories.Quest = countingStore
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}

	wantWriteErr := errors.New("clear-map op574 write failed")
	session := &gameSession{
		conn: &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}, connID: "clear-map-op574-retry",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
	}
	request := bossDieCheckRequestBody(99, uint16(targetKey))
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatalf("op574 write failure must not block settlement: %v", err)
	}
	assertBossResponseAndSettlementEntry(t, session.conn.(*failNthDungeonWriteConn).write.Bytes(), uint16(targetKey))
	if countingStore.saves.Load() != 1 || !runtime.clearMapCompletionPhaseAPersisted ||
		runtime.clearMapCompletionNotificationClosed || !runtime.bossDieCheckResponseSent ||
		!runtime.settlementEntrySent {
		t.Fatalf("failed op574 saves=%d state=%+v wrote=%x",
			countingStore.saves.Load(), runtime, session.conn.(*failNthDungeonWriteConn).write.Bytes())
	}

	conn := &bufferConn{}
	session.conn = conn
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatal(err)
	}
	notification, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	assertClearMapCompletionNotification(t, notification, 3145)
	if len(rest) != 0 {
		t.Fatalf("op574 replay should only repair notification, trailing=%x", rest)
	}
	if countingStore.saves.Load() != 1 || !runtime.clearMapCompletionNotificationClosed ||
		!runtime.bossDieCheckResponseSent || !runtime.settlementEntrySent {
		t.Fatalf("op574 replay repeated Phase-A or missed settlement: saves=%d state=%+v",
			countingStore.saves.Load(), runtime)
	}
}

func TestBossDieCheckClearMapFinalLinkedChildUsesNativeParentCompletionAndDoesNotBlockTownExit(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonQuestClearSubQuestCompletionCatalog(t, mapID)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			3146: {Status: "active", ProgressValue: 1},
			3157: {Status: "completed", ProgressValue: 0},
			3054: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}

	conn := &bufferConn{}
	session := &gameSession{
		conn: conn, connID: "clear-map-no-reward-subquest-parent-refresh",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonBossDieCheck(session, bossDieCheckRequestBody(99, uint16(targetKey))); err != nil {
		t.Fatal(err)
	}
	active, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	assertClearMapQuestTriggerNotification(t, active, 3146, 1)
	assertClearMapQuestTriggerNotification(t, active, 3054, 0)
	assertBossResponseAndSettlementEntry(t, rest, uint16(targetKey))
	if got := runtime.clearMapCompletionQuestIDs; len(got) != 1 || got[0] != 3054 {
		t.Fatalf("clear-map completed quest ids=%v, want [3054]", got)
	}
	if got := runtime.clearMapCompletionCompletedQuestIDs; len(got) != 1 || got[0] != 3054 {
		t.Fatalf("clear-map objective-complete ids=%v, want [3054]", got)
	}
	if got := runtime.clearMapCompletionPendingQuestIDs; len(got) != 1 || got[0] != 3054 {
		t.Fatalf("final linked child pending ids=%v, want [3054]", got)
	}
	persisted, found, err := repositories.Quest.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load persisted quest found=%t err=%v", found, err)
	}
	if parent := persisted.States[3146]; parent.Status != "active" || parent.ProgressValue != 1 {
		t.Fatalf("parent quest 3146 state=%+v, want active trigger one until native finish", parent)
	}
	child := persisted.States[3054]
	if child.Status != "active" || child.ProgressValue != 0 ||
		child.Extra["reward_state"] != "pending" ||
		child.Extra["completion_key"] != runtime.clearMapCompletionKey {
		t.Fatalf("child quest 3054 state=%+v key=%q", child, runtime.clearMapCompletionKey)
	}
}

func TestBossDieCheckNoRewardCompletionOp21RetryDoesNotRepeatOp574(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonQuestClearSubQuestCompletionCatalog(t, mapID)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			3146: {Status: "active", ProgressValue: 2},
			3157: {Status: "active", ProgressValue: 1},
			3054: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}

	wantWriteErr := errors.New("clear-map no-reward op21 write failed")
	failingConn := &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}
	session := &gameSession{
		conn: failingConn, connID: "clear-map-no-reward-op21-retry",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
	}
	request := bossDieCheckRequestBody(99, uint16(targetKey))
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatalf("op21 write failure must not block settlement: %v", err)
	}
	active, rest := splitGameServerUpperPacket(t, failingConn.write.Bytes())
	assertClearMapQuestTriggerNotification(t, active, 3146, 1)
	assertBossResponseAndSettlementEntry(t, rest, uint16(targetKey))
	if !runtime.clearMapCompletionActiveSnapshotSent || runtime.clearMapCompletionNotificationClosed ||
		!runtime.bossDieCheckResponseSent || !runtime.settlementEntrySent {
		t.Fatalf("failed no-reward op21 state=%+v", runtime)
	}

	retryConn := &bufferConn{}
	session.conn = retryConn
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatal(err)
	}
	acceptable, trailing := splitGameServerUpperPacket(t, retryConn.write.Bytes())
	if acceptable.Header.MsgID != currentAcceptableQuestListMsgID || acceptable.Header.Classification != 0 || len(trailing) != 0 {
		t.Fatalf("op21 retry header=%+v trailing=%x", acceptable.Header, trailing)
	}
	if !runtime.clearMapCompletionNotificationClosed {
		t.Fatalf("op21 retry did not close notification state=%+v", runtime)
	}
}

func TestBossDieCheckOp115RetryDoesNotRepeatClearMapPhaseAWrite(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true, singleMonster: true})
	baseRepositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	mapID := runtime.Session.Snapshot().Scene.Map.Map.ID
	service.questCatalog = buildDungeonClearMapCompletionCatalog(t, mapID)
	if err := baseRepositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	countingStore := &clearMapCountingQuestStore{QuestRepository: baseRepositories.Quest}
	repositories := baseRepositories
	repositories.Quest = countingStore
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	targetKey := runtime.Room.Snapshot().Monsters[0].ObjectKey
	if _, cleared, err := runtime.Room.CommitActorDeathReport(targetKey, runtime.Session); err != nil || !cleared {
		t.Fatalf("commit boss death cleared=%t err=%v", cleared, err)
	}
	wantWriteErr := errors.New("first op115 write failed after Phase A")
	session := &gameSession{
		conn: &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}, connID: "clear-map-op115-retry",
		selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime},
	}
	request := bossDieCheckRequestBody(99, uint16(targetKey))
	if err := service.handleDungeonBossDieCheck(session, request); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first op115 error=%v want=%v", err, wantWriteErr)
	}
	firstNotification, firstRest := splitGameServerUpperPacket(t, session.conn.(*failNthDungeonWriteConn).write.Bytes())
	assertClearMapCompletionNotification(t, firstNotification, 3145)
	if len(firstRest) != 0 {
		t.Fatalf("failed op115 emitted bytes after op574=%x", firstRest)
	}
	if countingStore.saves.Load() != 1 || !runtime.clearMapCompletionPhaseAPersisted ||
		!runtime.clearMapCompletionNotificationClosed || runtime.bossDieCheckResponseSent {
		t.Fatalf("first op115 state saves=%d runtime=%+v", countingStore.saves.Load(), runtime)
	}

	conn := &bufferConn{}
	session.conn = conn
	if err := service.handleDungeonBossDieCheck(session, request); err != nil {
		t.Fatal(err)
	}
	assertBossResponseAndSettlementEntry(t, conn.write.Bytes(), uint16(targetKey))
	if countingStore.saves.Load() != 1 {
		t.Fatalf("op115 replay repeated Phase-A write: saves=%d", countingStore.saves.Load())
	}
}

func assertClearMapNotificationThenBossResponseAndSettlementEntry(
	t *testing.T,
	stream []byte,
	targetObjectKey uint16,
	questID uint16,
) {
	t.Helper()
	notification, rest := splitGameServerUpperPacket(t, stream)
	assertClearMapCompletionNotification(t, notification, questID)
	assertBossResponseAndSettlementEntry(t, rest, targetObjectKey)
}

func assertClearMapCompletionNotification(t *testing.T, packet dnfproto.ChannelPacket, questID uint16) {
	t.Helper()
	assertClearMapQuestTriggerNotification(t, packet, questID, 0)
}

func assertClearMapQuestTriggerNotification(t *testing.T, packet dnfproto.ChannelPacket, questID uint16, trigger uint32) {
	t.Helper()
	if packet.Header.MsgID != currentActiveQuestSnapshotMsgID || packet.Header.Classification != 0 || len(packet.Body) < 4 {
		t.Fatalf("clear-map op574 header=%+v body=%x", packet.Header, packet.Body)
	}
	count := int(binary.LittleEndian.Uint32(packet.Body[:4]))
	if len(packet.Body) != 4+count*6 {
		t.Fatalf("clear-map op574 count=%d body_len=%d body=%x", count, len(packet.Body), packet.Body)
	}
	for offset := 4; offset < len(packet.Body); offset += 6 {
		if binary.LittleEndian.Uint16(packet.Body[offset:offset+2]) == questID &&
			binary.LittleEndian.Uint32(packet.Body[offset+2:offset+6]) == trigger {
			return
		}
	}
	t.Fatalf("clear-map op574 lacks quest=%d trigger=%d body=%x", questID, trigger, packet.Body)
}

type clearMapCountingQuestStore struct {
	dnfrepo.QuestRepository
	saves atomic.Int64
	err   error
}

func (s *clearMapCountingQuestStore) Save(ctx context.Context, record dnfrepo.QuestRecord) error {
	s.saves.Add(1)
	if s.err != nil {
		return s.err
	}
	return s.QuestRepository.Save(ctx, record)
}

func buildDungeonClearMapCompletionCatalog(t *testing.T, mapID int64) *dnfquest.Catalog {
	t.Helper()
	source := questListTestSource{
		dnfquest.DefaultList: "3145 `clear_map.qst`\n3146 `other.qst`\n",
		"n_quest/clear_map.qst": "[grade]\n`[epic]`\n[level]\n1 99\n[job]\n`[gunner]`\n" +
			"[type]\n`[clear map]`\n[int data]\n" + fmt.Sprint(mapID) + "\n",
		"n_quest/other.qst": "[grade]\n`[epic]`\n[level]\n1 99\n[job]\n`[gunner]`\n" +
			"[type]\n`[clear map]`\n[int data]\n99999\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func buildDungeonQuestClearSubQuestCompletionCatalog(t *testing.T, mapID int64) *dnfquest.Catalog {
	t.Helper()
	source := questListTestSource{
		dnfquest.DefaultList: "3146 `parent.qst`\n3157 `hunt.qst`\n3054 `clear_sub.qst`\n",
		"n_quest/parent.qst": "[grade]\n`[epic]`\n[level]\n1 99\n[job]\n`[gunner]`\n" +
			"[type]\n`[quest clear]`\n[int data]\n3157 3054\n",
		"n_quest/hunt.qst": "[grade]\n`[sub]`\n[level]\n1 99\n[job]\n`[gunner]`\n" +
			"[type]\n`[hunt enemy]`\n[main quest]\n3146\n[int data]\n3 -1 13099 3 1\n",
		"n_quest/clear_sub.qst": "[grade]\n`[sub]`\n[level]\n1 99\n[job]\n`[gunner]`\n" +
			"[type]\n`[clear map]`\n[main quest]\n3146\n[int data]\n" + fmt.Sprint(mapID) + "\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

var _ dnfrepo.QuestRepository = (*clearMapCountingQuestStore)(nil)
