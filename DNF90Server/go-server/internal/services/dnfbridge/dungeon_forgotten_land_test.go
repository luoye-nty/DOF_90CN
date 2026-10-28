package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func markRuntimeAsForgottenLand(runtime *runtimeDungeonState) {
	runtime.Dungeon.Metadata.DungeonType = "lost land"
	runtime.Dungeon.Metadata.LostLandParameters = worldmap.LostLandParameters{
		DayMilliseconds: 120000, NightMilliseconds: 120000, Set: true,
	}
	runtime.Dungeon.Metadata.LimitTime = worldmap.DungeonLimitTime{
		PrimarySeconds: 600, SecondarySeconds: 1200, Set: true,
	}
}

func TestBuildCurrentForgottenLandTimeInfoBodyMatchesCurrentEXEReader(t *testing.T) {
	body := buildCurrentForgottenLandTimeInfoBody(600, 1200)
	if len(body) != 8 || binary.LittleEndian.Uint32(body[:4]) != 600 || binary.LittleEndian.Uint32(body[4:]) != 1200 {
		t.Fatalf("time info body=%x", body)
	}
}

func TestCurrentForgottenLandTimeProjectionUsesTypedPVFLimits(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	markRuntimeAsForgottenLand(runtime)
	if err := service.sendCurrentForgottenLandTimeProjection(session, runtime); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentForgottenLandTimeInfoMsgID ||
		!bytes.Equal(packet.Body, buildCurrentForgottenLandTimeInfoBody(600, 1200)) || len(rest) != 0 {
		t.Fatalf("projection header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
}

func TestForgottenLandEntryProjectsTimeBeforeStartMapAndArmsOneTimer(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(
		source["dungeon/test.dgn"],
		"[basis level]\n20\n",
		"[basis level]\n20\n[dungeon type]\n`lost land`\n[lost land parameters]\n120000 120000\n[clear condition]\n[limit time]\n600 1200\n[/clear condition]\n",
		1,
	)
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99", AccountID: "account-1", Level: 20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:       options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable: table, worldMapResolver: resolver, dungeonMonsterTable: monsters,
		dungeonChoice:      func(int) (int, error) { return 0, nil },
		dungeonSeed:        func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:     queue,
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn: connection, connID: "forgotten-land-entry-test", selectedCharacterID: 99,
		selectPreviewObjectStateSent: true, currentSceneObjectListSent: true,
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
	body := make([]byte, 21)
	binary.LittleEndian.PutUint32(body[:4], 700)
	body[4] = 1
	if err := service.handleDungeonSelectUpper(session, body); err != nil {
		t.Fatal(err)
	}

	packets := splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes())
	infoIndex, timeIndex, startIndex := -1, -1, -1
	for index, packet := range packets {
		switch packet.Header.MsgID {
		case currentDungeonInfoNotification:
			infoIndex = index
		case currentForgottenLandTimeInfoMsgID:
			timeIndex = index
			if packet.Header.Classification != 0 || !bytes.Equal(packet.Body, buildCurrentForgottenLandTimeInfoBody(600, 1200)) {
				t.Fatalf("time projection header=%+v body=%x", packet.Header, packet.Body)
			}
		case currentDungeonStartNotification:
			startIndex = index
		}
	}
	if infoIndex < 0 || timeIndex <= infoIndex || startIndex <= timeIndex {
		t.Fatalf("entry packet indexes op28=%d op603=%d op29=%d", infoIndex, timeIndex, startIndex)
	}
	queue.mu.Lock()
	forgottenLandTasks := 0
	for _, task := range queue.tasks {
		if strings.HasPrefix(task.name, "dnf-forgotten-land:") {
			forgottenLandTasks++
		}
	}
	forgottenLandTimerActive := queue.active[session.dungeon.runtime.forgottenLandTimerName] != nil
	queue.mu.Unlock()
	if forgottenLandTasks != 1 || !forgottenLandTimerActive {
		t.Fatalf("forgotten land tasks=%d active=%t timer=%q", forgottenLandTasks, forgottenLandTimerActive, session.dungeon.runtime.forgottenLandTimerName)
	}
}

func TestCurrentForgottenLandTimerUsesGameplayQueueAndDoesNotReset(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	markRuntimeAsForgottenLand(runtime)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue

	if err := service.armCurrentForgottenLandTimer(session, runtime); err != nil {
		t.Fatal(err)
	}
	firstDeadline := runtime.forgottenLandDeadline
	if err := service.armCurrentForgottenLandTimer(session, runtime); err != nil {
		t.Fatal(err)
	}
	scheduled, cancelled, active := queue.counts()
	if scheduled != 1 || cancelled != 0 || active != 1 {
		t.Fatalf("timer counts scheduled=%d cancelled=%d active=%d", scheduled, cancelled, active)
	}
	if runtime.forgottenLandDeadline != firstDeadline || firstDeadline.Sub(queue.Now()) != 600*time.Second {
		t.Fatalf("deadline=%s first=%s now=%s", runtime.forgottenLandDeadline, firstDeadline, queue.Now())
	}
	task := queue.task(t, 0)
	if task.delay != 600*time.Second {
		t.Fatalf("timer delay=%s", task.delay)
	}

	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	queue.fire(task, false)
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("forgotten-land timeout full route=%v", got)
	}
	if !runtime.townReturnPending || !runtime.townReturnOp24Sent || runtime.forgottenLandTimerActive ||
		session.dungeon.runtime != nil || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned {
		t.Fatalf("timeout state pending=%t op24=%t timer_active=%t", runtime.townReturnPending, runtime.townReturnOp24Sent, runtime.forgottenLandTimerActive)
	}
}

func TestCancelledForgottenLandTimerCannotAffectReplacementRuntime(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	markRuntimeAsForgottenLand(runtime)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if err := service.armCurrentForgottenLandTimer(session, runtime); err != nil {
		t.Fatal(err)
	}
	task := queue.task(t, 0)

	session.dungeon.mu.Lock()
	service.cancelCurrentForgottenLandTimerLocked(session, runtime, "test_runtime_replaced")
	session.dungeon.runtime = nil
	session.dungeon.mu.Unlock()
	session.conn.(*bufferConn).write.Reset()
	queue.fire(task, true)
	if got := session.conn.(*bufferConn).write.Len(); got != 0 {
		t.Fatalf("stale timer wrote %d bytes", got)
	}
}

func TestInterruptedTownReturnCancelsForgottenLandTimerAndRetiresRun(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	markRuntimeAsForgottenLand(runtime)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	if err := service.armCurrentForgottenLandTimer(session, runtime); err != nil {
		t.Fatal(err)
	}
	task := queue.task(t, 0)

	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	_, cancelled, _ := queue.counts()
	queue.mu.Lock()
	timerStillActive := queue.active[task.name] != nil
	queue.mu.Unlock()
	if !runtime.townReturnPending || runtime.forgottenLandTimerActive || cancelled != 1 || timerStillActive ||
		session.dungeon.runtime != nil || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned {
		t.Fatalf("full return pending=%t timer_active=%t cancelled=%d scheduled=%t owner=%p status=%s",
			runtime.townReturnPending, runtime.forgottenLandTimerActive, cancelled, timerStillActive,
			session.dungeon.runtime, runtime.Session.Snapshot().Run.Status)
	}

	connection.write.Reset()
	queue.fire(task, true)
	if connection.write.Len() != 0 {
		t.Fatalf("stale forgotten-land timer wrote after retirement=%x", connection.write.Bytes())
	}
}
