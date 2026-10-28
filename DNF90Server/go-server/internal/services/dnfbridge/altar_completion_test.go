package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfmonster "longheng.io/server/internal/modules/dnf/monster"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonAltarScriptedClearMapPhaseARetriesExactlyOnceOnDuplicateDeath(t *testing.T) {
	service, session, runtime, repositories := prepareCurrentDungeonAltarSettlementStageTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	service.questCatalog = currentDungeonAltarChallengeQuestTestCatalog(t, 120, 11007)
	runtime.altar.progress = currentDungeonAltarProgressDungeonComplete
	runtime.clearMapCompletionPhaseAPersisted = false
	runtime.clearMapCompletionKey = ""
	runtime.clearMapCompletionAt = time.Time{}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			120: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("scripted altar clear-map save failed")
	questStore := &failOnceCurrentDungeonAltarQuestStore{
		QuestRepository: repositories.Quest,
		failErr:         wantErr,
		failures:        1,
	}
	repositories.Quest = questStore
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }

	session.dungeon.mu.Lock()
	if err := service.completeCurrentDungeonAltarLocked(session, runtime, "test_initial_final_op255"); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	session.dungeon.mu.Unlock()
	if runtime.clearMapCompletionPhaseAPersisted || runtime.altar.settlement.phase != currentDungeonAltarSettlementPrepared {
		t.Fatalf("failed phase A advanced runtime: persisted=%t settlement_phase=%d",
			runtime.clearMapCompletionPhaseAPersisted, runtime.altar.settlement.phase)
	}
	if scheduled, _, active := queue.counts(); scheduled != 1 || active != 1 {
		t.Fatalf("phase-A retry queue scheduled=%d active=%d", scheduled, active)
	}
	completionKey := runtime.clearMapCompletionKey
	completionAt := runtime.clearMapCompletionAt
	if completionKey == "" || completionAt.IsZero() {
		t.Fatalf("failed phase A did not freeze identity key=%q at=%v", completionKey, completionAt)
	}

	const duplicateSequence = uint16(60000)
	runtime.altar.retiredSequences[duplicateSequence] = currentDungeonAltarSequenceOwner{
		RoomGeneration:  runtime.altar.Room.Generation,
		RoundGeneration: runtime.altar.generation,
		RoomKey:         runtime.altar.Room.Key,
		MonsterCode:     56004,
		QuestDropSeed:   1,
	}
	if err := service.handleCurrentDungeonAltarDeaths(session, []byte{
		1,
		byte(duplicateSequence & 0xff),
		byte(duplicateSequence >> 8),
	}); err != nil {
		t.Fatal(err)
	}
	if !runtime.clearMapCompletionPhaseAPersisted || runtime.clearMapCompletionKey != completionKey ||
		!runtime.clearMapCompletionAt.Equal(completionAt) {
		t.Fatalf("phase-A retry identity changed: persisted=%t key=%q at=%v",
			runtime.clearMapCompletionPhaseAPersisted, runtime.clearMapCompletionKey, runtime.clearMapCompletionAt)
	}
	if !completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("duplicate op255 did not resume settlement: %+v", runtime.altar.settlement)
	}
	questRecord, found, err := repositories.Quest.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load challenge quest found=%t err=%v", found, err)
	}
	state := questRecord.States[120]
	if state.ProgressValue != 0 || len(runtime.clearMapCompletionQuestIDs) != 1 || runtime.clearMapCompletionQuestIDs[0] != 120 {
		t.Fatalf("challenge_120 state=%+v completions=%v", state, runtime.clearMapCompletionQuestIDs)
	}
	if questStore.saves.Load() != 2 {
		t.Fatalf("phase-A saves=%d want failed+successful", questStore.saves.Load())
	}
	if _, cancelled, active := queue.counts(); cancelled != 1 || active != 0 {
		t.Fatalf("duplicate retry did not retire timer: cancelled=%d active=%d", cancelled, active)
	}
	activeSnapshots := 0
	stream := session.conn.(*bufferConn).write.Bytes()
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.MsgID == currentActiveQuestSnapshotMsgID {
			activeSnapshots++
		}
		stream = rest
	}
	if activeSnapshots != 1 {
		t.Fatalf("altar clear-map active snapshots=%d want=1", activeSnapshots)
	}

	bytesBeforeStale := session.conn.(*bufferConn).write.Len()
	queue.fire(queue.task(t, 0), true)
	if session.conn.(*bufferConn).write.Len() != bytesBeforeStale || questStore.saves.Load() != 2 {
		t.Fatalf("stale retry replayed projection or persistence: bytes=%d->%d saves=%d",
			bytesBeforeStale, session.conn.(*bufferConn).write.Len(), questStore.saves.Load())
	}
}

func TestCurrentDungeonAltarClearMapNotificationFailureRetriesBeforeSettlement(t *testing.T) {
	service, session, runtime, repositories := prepareCurrentDungeonAltarSettlementStageTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	service.questCatalog = currentDungeonAltarChallengeQuestTestCatalog(t, 120, 11007)
	runtime.altar.progress = currentDungeonAltarProgressDungeonComplete
	runtime.clearMapCompletionPhaseAPersisted = false
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "99",
		States: map[int64]dnfrepo.QuestState{
			120: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	wantWriteErr := errors.New("altar op574 write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}

	session.dungeon.mu.Lock()
	if err := service.completeCurrentDungeonAltarLocked(session, runtime, "test_op574_failure"); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	session.dungeon.mu.Unlock()
	if !runtime.clearMapCompletionPhaseAPersisted || runtime.clearMapCompletionActiveSnapshotSent ||
		runtime.clearMapCompletionNotificationClosed || runtime.altar.settlement.phase != currentDungeonAltarSettlementPrepared {
		t.Fatalf("failed notification advanced stage: phase_a=%t snapshot=%t closed=%t settlement=%d",
			runtime.clearMapCompletionPhaseAPersisted, runtime.clearMapCompletionActiveSnapshotSent,
			runtime.clearMapCompletionNotificationClosed, runtime.altar.settlement.phase)
	}
	if scheduled, _, active := queue.counts(); scheduled != 1 || active != 1 {
		t.Fatalf("notification retry queue scheduled=%d active=%d", scheduled, active)
	}

	retryConn := &bufferConn{}
	session.conn = retryConn
	task := queue.task(t, 0)
	queue.fire(task, false)
	if !runtime.clearMapCompletionNotificationClosed || !completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("notification retry did not finish: closed=%t settlement=%+v",
			runtime.clearMapCompletionNotificationClosed, runtime.altar.settlement)
	}
	activeSnapshots := 0
	stream := retryConn.write.Bytes()
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.MsgID == currentActiveQuestSnapshotMsgID {
			activeSnapshots++
		}
		stream = rest
	}
	if activeSnapshots != 1 {
		t.Fatalf("notification retry active snapshots=%d want=1", activeSnapshots)
	}
	bytesBeforeStale := retryConn.write.Len()
	queue.fire(task, true)
	if retryConn.write.Len() != bytesBeforeStale {
		t.Fatalf("stale notification retry resent packets: bytes=%d->%d", bytesBeforeStale, retryConn.write.Len())
	}
}

func TestCurrentDungeonScriptedAltarPhaseAMatchesBothChallengeQuests(t *testing.T) {
	tests := []struct {
		name      string
		questID   int64
		dungeonID int64
		prepare   func(*testing.T) (*Service, *gameSession, *runtimeDungeonState)
	}{
		{
			name:    "challenge_119_endless",
			questID: 119, dungeonID: 11006,
			prepare: prepareCurrentDungeonEndlessAltarCompletionTest,
		},
		{
			name:    "challenge_120_ultimate",
			questID: 120, dungeonID: 11007,
			prepare: func(t *testing.T) (*Service, *gameSession, *runtimeDungeonState) {
				service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
				return service, session, runtime
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime := test.prepare(t)
			repositories, ok := service.repositoryGroup()
			if !ok {
				t.Fatal("repository group unavailable")
			}
			service.questCatalog = currentDungeonAltarChallengeQuestTestCatalog(t, test.questID, test.dungeonID)
			if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
				CharacterID: "99",
				States: map[int64]dnfrepo.QuestState{
					test.questID: {Status: "active", ProgressValue: 1},
				},
			}); err != nil {
				t.Fatal(err)
			}
			counter := &failOnceCurrentDungeonAltarQuestStore{QuestRepository: repositories.Quest}
			repositories.Quest = counter
			service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
			if err := runtime.Session.MarkCurrentRoomScriptedCleared(); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Session.Complete(); err != nil {
				t.Fatal(err)
			}
			runtime.altar.completedRounds = runtime.altar.MaxRounds
			runtime.altar.progress = currentDungeonAltarProgressDungeonComplete

			session.dungeon.mu.Lock()
			snapshot := runtime.Session.Snapshot()
			err := service.persistCurrentDungeonScriptedAltarClearMapCompletionPhaseA(
				context.Background(), session, runtime, snapshot,
			)
			if err == nil {
				err = service.persistCurrentDungeonScriptedAltarClearMapCompletionPhaseA(
					context.Background(), session, runtime, snapshot,
				)
			}
			session.dungeon.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			record, found, err := repositories.Quest.Load(context.Background(), "99")
			if err != nil || !found {
				t.Fatalf("load quest found=%t err=%v", found, err)
			}
			if state := record.States[test.questID]; state.ProgressValue != 0 || counter.saves.Load() != 1 ||
				len(runtime.clearMapCompletionQuestIDs) != 1 || runtime.clearMapCompletionQuestIDs[0] != test.questID {
				t.Fatalf("quest=%d state=%+v saves=%d completions=%v",
					test.questID, state, counter.saves.Load(), runtime.clearMapCompletionQuestIDs)
			}
		})
	}
}

func TestCurrentDungeonAltarSettlementFailureRetriesAutonomouslyAndStaleTimerNoOps(t *testing.T) {
	service, session, runtime, _ := prepareCurrentDungeonAltarSettlementStageTest(t)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	runtime.altar.progress = currentDungeonAltarProgressDungeonComplete
	runtime.clearMapCompletionPhaseAPersisted = true
	roundsBefore := runtime.altar.completedRounds
	wantWriteErr := errors.New("altar op208 write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}

	session.dungeon.mu.Lock()
	if err := service.completeCurrentDungeonAltarLocked(session, runtime, "test_op208_failure"); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	session.dungeon.mu.Unlock()
	if runtime.altar.settlement.phase != currentDungeonAltarSettlementPrepared {
		t.Fatalf("failed op208 advanced settlement phase=%d", runtime.altar.settlement.phase)
	}
	if scheduled, _, active := queue.counts(); scheduled != 1 || active != 1 {
		t.Fatalf("settlement retry queue scheduled=%d active=%d", scheduled, active)
	}
	task := queue.task(t, 0)
	retryConn := &bufferConn{}
	session.conn = retryConn
	queue.fire(task, false)
	if !completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("timer retry did not finish settlement: %+v", runtime.altar.settlement)
	}
	if runtime.altar.completedRounds != roundsBefore {
		t.Fatalf("timer retry replayed rounds=%d want=%d", runtime.altar.completedRounds, roundsBefore)
	}
	ranking, reward := 0, 0
	stream := retryConn.write.Bytes()
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		switch packet.Header.MsgID {
		case currentDungeonAltarRankingMsgID:
			ranking++
		case currentDungeonAltarRewardMsgID:
			reward++
		}
		stream = rest
	}
	if ranking != 1 || reward != 1 {
		t.Fatalf("retry altar UI ranking=%d reward=%d", ranking, reward)
	}
	bytesBeforeStale := retryConn.write.Len()
	queue.fire(task, true)
	if retryConn.write.Len() != bytesBeforeStale || runtime.altar.completedRounds != roundsBefore {
		t.Fatalf("stale completion timer mutated state: bytes=%d->%d rounds=%d",
			bytesBeforeStale, retryConn.write.Len(), runtime.altar.completedRounds)
	}
}

func currentDungeonAltarChallengeQuestTestCatalog(t *testing.T, questID int64, dungeonID int64) *dnfquest.Catalog {
	t.Helper()
	source := bridgePVFSource{
		dnfquest.DefaultList: fmt.Sprintf("%d `challenge.qst`\n", questID),
		"n_quest/challenge.qst": "[grade]\n`[challenge]`\n" +
			"[type]\n`[condition under clear]`\n" +
			"[sub type]\n6\n" +
			fmt.Sprintf("[int data]\n%d -1 1\n", dungeonID),
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

func prepareCurrentDungeonEndlessAltarCompletionTest(
	t *testing.T,
) (*Service, *gameSession, *runtimeDungeonState) {
	t.Helper()
	source := currentDungeonEndlessAltarCompletionPVFTestSource()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       70,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "endless-altar-completion-test",
		selectedCharacterID: 99,
		characterGeneration: 1,
	}
	runtime, _, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 11006, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.lifecycleToken = 1
	session.dungeon.runtime = runtime
	return service, session, runtime
}

func currentDungeonEndlessAltarCompletionPVFTestSource() bridgePVFSource {
	phaseRows := ""
	for round := 0; round < 5; round++ {
		phaseRows += fmt.Sprintf("%d 0 0 1 0 0\n", round)
	}
	return bridgePVFSource{
		worldmap.DefaultMapList: "17020 `dungeon/altar/endless.map`\n",
		"map/dungeon/altar/endless.map": "[map name]\n`endless`\n" +
			"[dungeon]\n11006\n" +
			"[type]\n`[start]`\n" +
			"[blood monster]\n61010 0 440 222 0 0 0 1 0 1\n[/blood monster]\n" +
			"[blood phase time]\n" + phaseRows + "[/blood phase time]\n",
		worldmap.DefaultDungeonList: "11006 `altar/endless.dgn`\n",
		"dungeon/altar/endless.dgn": "[name]\n`Endless Altar`\n" +
			"[minimum required level]\n1\n[basis level]\n70\n[no fatigue]\n" +
			"[blood dungeon]\n1\n[blood max round]\n15\n" +
			"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
			"[map specification]\n`map` 0 0 17020\n[start map]\n0 0\n[boss map]\n0 0\n",
		worldmap.DefaultWorldMapList: "1 `altar.wdm`\n",
		"worldmap/altar.wdm":         "[name]\n`Altar`\n[dungeon]\n11006 -1\n[/dungeon]\n",
		dnfmonster.DefaultList:       "1 `dummy.gob`\n",
		"monster/dummy.gob":          "[name]\n`dummy`\n[level]\n1\n[hp]\n1\n[exp]\n0\n",
	}
}

type failOnceCurrentDungeonAltarQuestStore struct {
	dnfrepo.QuestRepository
	failErr  error
	failures int64
	saves    atomic.Int64
}

func (store *failOnceCurrentDungeonAltarQuestStore) Save(ctx context.Context, record dnfrepo.QuestRecord) error {
	store.saves.Add(1)
	if store.failures > 0 {
		store.failures--
		return store.failErr
	}
	return store.QuestRepository.Save(ctx, record)
}

var _ dnfrepo.QuestRepository = (*failOnceCurrentDungeonAltarQuestStore)(nil)
