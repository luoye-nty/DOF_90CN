package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/timequeue"
)

// clearQuestReplayTestQueue records what the HUD-ready replay arms and lets the
// test fire the callback synchronously instead of waiting for wall-clock time.
type clearQuestReplayTestQueue struct {
	now       time.Time
	names     []string
	delays    []time.Duration
	callbacks []timequeue.Callback
	// dailyNames/dailyCallbacks record RegisterDaily separately so the
	// one-shot replay assertions above keep their name/delay alignment.
	dailyNames     []string
	dailyCallbacks []timequeue.Callback
}

func (q *clearQuestReplayTestQueue) Now() time.Time { return q.now }

func (q *clearQuestReplayTestQueue) ScheduleAfter(
	name string,
	delay time.Duration,
	callback timequeue.Callback,
) error {
	q.names = append(q.names, name)
	q.delays = append(q.delays, delay)
	q.callbacks = append(q.callbacks, callback)
	return nil
}

func (q *clearQuestReplayTestQueue) RegisterDaily(
	name string,
	_, _ int,
	callback timequeue.Callback,
) error {
	q.dailyNames = append(q.dailyNames, name)
	q.dailyCallbacks = append(q.dailyCallbacks, callback)
	return nil
}

func (q *clearQuestReplayTestQueue) Cancel(string) bool { return false }

func (q *clearQuestReplayTestQueue) Start(context.Context) {}

func currentClearQuestListFrames(stream []byte) int {
	count := 0
	for len(stream) > 0 {
		if len(stream) < 16 {
			break
		}
		total := int(uint32(stream[3]) | uint32(stream[4])<<8 | uint32(stream[5])<<16 | uint32(stream[6])<<24)
		if total <= 0 || total > len(stream) {
			break
		}
		msgID := uint16(stream[1]) | uint16(stream[2])<<8
		if msgID == currentClearQuestListMsgID {
			count++
		}
		stream = stream[total:]
	}
	return count
}

// currentClearQuestListBody returns the op356 body inside a stream that also
// carries the active/acceptable pair the replay publishes around it.
func currentClearQuestListBody(t *testing.T, stream []byte) []byte {
	t.Helper()
	for len(stream) > 0 {
		if len(stream) < 16 {
			break
		}
		total := int(uint32(stream[3]) | uint32(stream[4])<<8 | uint32(stream[5])<<16 | uint32(stream[6])<<24)
		if total <= 0 || total > len(stream) {
			break
		}
		msgID := uint16(stream[1]) | uint16(stream[2])<<8
		if msgID == currentClearQuestListMsgID {
			return stream[16:total]
		}
		stream = stream[total:]
	}
	t.Fatalf("no op356 frame in %x", stream)
	return nil
}

// currentClearQuestListFrameIDs returns the message ids of every frame in a
// session write stream, in write order.
func currentClearQuestListFrameIDs(stream []byte) []uint16 {
	ids := make([]uint16, 0, 4)
	for len(stream) > 0 {
		if len(stream) < 16 {
			break
		}
		total := int(uint32(stream[3]) | uint32(stream[4])<<8 | uint32(stream[5])<<16 | uint32(stream[6])<<24)
		if total <= 0 || total > len(stream) {
			break
		}
		ids = append(ids, uint16(stream[1])|uint16(stream[2])<<8)
		stream = stream[total:]
	}
	return ids
}

func requireCompletedQuestFlag(t *testing.T, body []byte, questID int) {
	t.Helper()
	plain, err := zlibDecompress(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4+currentClearQuestListPayloadSize {
		t.Fatalf("replayed op356 len=%d want=%d", len(plain), 4+currentClearQuestListPayloadSize)
	}
	if got := plain[4+questID]; got != 1 {
		t.Fatalf("replayed op356 quest %d flag=%d want 1", questID, got)
	}
}

func newClearQuestReplayTest(t *testing.T, quests map[int64]dnfrepo.QuestState) (*Service, *gameSession, *clearQuestReplayTestQueue) {
	t.Helper()
	service, session, repositories := newTownMoveTest(t)
	// The HUD-ready refresh only sends when the selected character has a usable
	// job/level and the quest catalog is loaded; the town-move fixture leaves both
	// out, so give this test its own catalog-backed character.
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "29",
		AccountID:   "dnf:1",
		Name:        "hudready",
		Job:         "2",
		Level:       5,
		Stats:       map[string]int64{"grow_type": 0, "expert_job_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	service.questCatalog = buildQuestListTestCatalog(t)
	if len(quests) > 0 {
		if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
			CharacterID: "29",
			States:      quests,
		}); err != nil {
			t.Fatal(err)
		}
	}
	queue := &clearQuestReplayTestQueue{now: time.Now()}
	service.gameplayTimers = queue
	session.initialTownRouteCharacterID = session.selectedCharacterID
	session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
	return service, session, queue
}

// The HUD-ready task refresh must arm the scheduled replays on the shared
// gameplay queue, and the refresh itself must not carry op356 (the pre-rebuild
// write is what the late rebuild drops).
func TestCurrentClearQuestListHUDReadyReplayArmsDelayedSends(t *testing.T) {
	service, session, queue := newClearQuestReplayTest(t, map[int64]dnfrepo.QuestState{
		3149: {Status: "completed"},
	})

	if err := service.sendCurrentQuestHUDReadyRefresh(session, "test_hud_ready"); err != nil {
		t.Fatal(err)
	}
	if !session.initialTownQuestHUDRefreshSent {
		t.Fatal("HUD-ready refresh did not mark the session")
	}
	if len(queue.callbacks) != len(currentClearQuestListHUDReadyReplayDelays) {
		t.Fatalf("armed timers=%v want %d", queue.names, len(currentClearQuestListHUDReadyReplayDelays))
	}
	for index, want := range currentClearQuestListHUDReadyReplayDelays {
		if queue.delays[index] != want {
			t.Fatalf("replay delay[%d]=%s want %s", index, queue.delays[index], want)
		}
		if name := currentClearQuestListHUDReadyReplayTimerName(session, 29, index); queue.names[index] != name {
			t.Fatalf("replay timer name[%d]=%q want %q", index, queue.names[index], name)
		}
	}
	connection := session.conn.(*bufferConn)
	if got := currentClearQuestListFrames(connection.write.Bytes()); got != 0 {
		t.Fatalf("HUD-ready refresh emitted %d op356 frames, want 0", got)
	}

	// Firing any armed replay publishes the active/acceptable pair around the
	// persisted completed-quest bitmap: the pair is what makes the client
	// re-request the channel directory, the bitmap is what the rebuilt row reads.
	connection.write.Reset()
	queue.callbacks[len(queue.callbacks)-1](queue.now)
	stream := connection.write.Bytes()
	if got := currentClearQuestListFrames(stream); got != 1 {
		t.Fatalf("replay emitted %d op356 frames, want 1 (stream=%x)", got, stream)
	}
	requireCompletedQuestFlag(t, currentClearQuestListBody(t, stream), 3149)

	// The order inside the trio is load-bearing: the bitmap must land between the
	// active snapshot (whose tracker row would otherwise be rebuilt after it) and
	// the acceptable list (which is what makes the client re-request the channel
	// directory and rebuild the team-mode row).
	ids := currentClearQuestListFrameIDs(stream)
	bitmap := -1
	for index, id := range ids {
		if id == currentClearQuestListMsgID {
			bitmap = index
			break
		}
	}
	if bitmap <= 0 || bitmap+1 >= len(ids) ||
		ids[bitmap-1] != currentActiveQuestSnapshotMsgID ||
		ids[bitmap+1] != currentAcceptableQuestListMsgID {
		t.Fatalf("replay frame order=%v, want ...%d, %d, %d...",
			ids, currentActiveQuestSnapshotMsgID, currentClearQuestListMsgID, currentAcceptableQuestListMsgID)
	}
}

// A character switch or relog before the timer fires must make the replay a
// no-op instead of writing one character's table into another session state.
func TestCurrentClearQuestListHUDReadyReplayDropsStaleGeneration(t *testing.T) {
	service, session, queue := newClearQuestReplayTest(t, map[int64]dnfrepo.QuestState{
		3149: {Status: "completed"},
	})

	if err := service.sendCurrentQuestHUDReadyRefresh(session, "test_hud_ready"); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	session.characterGeneration++
	queue.callbacks[0](queue.now)
	if got := connection.write.Len(); got != 0 {
		t.Fatalf("stale replay wrote %d bytes: %x", got, connection.write.Bytes())
	}
}

func TestCurrentClearQuestListHUDReadyReplayDropsStaleCharacter(t *testing.T) {
	service, session, queue := newClearQuestReplayTest(t, map[int64]dnfrepo.QuestState{
		3149: {Status: "completed"},
	})

	if err := service.sendCurrentQuestHUDReadyRefresh(session, "test_hud_ready"); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	session.selectedCharacterID = 30
	queue.callbacks[0](queue.now)
	if got := connection.write.Len(); got != 0 {
		t.Fatalf("stale-character replay wrote %d bytes: %x", got, connection.write.Bytes())
	}
}

// A character without any completed quest must not receive an empty op356: the
// sender already skips records that cannot activate a visibility gate. The task
// pair still goes out, because the client needs it to rebuild its channel row.
func TestCurrentClearQuestListHUDReadyReplaySkipsCharacterWithoutCompletedQuests(t *testing.T) {
	service, session, queue := newClearQuestReplayTest(t, nil)

	if err := service.sendCurrentQuestHUDReadyRefresh(session, "test_hud_ready"); err != nil {
		t.Fatal(err)
	}
	if len(queue.callbacks) != len(currentClearQuestListHUDReadyReplayDelays) {
		t.Fatalf("armed timers=%v want %d", queue.names, len(currentClearQuestListHUDReadyReplayDelays))
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	queue.callbacks[len(queue.callbacks)-1](queue.now)
	stream := connection.write.Bytes()
	if got := currentClearQuestListFrames(stream); got != 0 {
		t.Fatalf("empty-record replay emitted %d op356 frames: %x", got, stream)
	}
	if len(stream) == 0 {
		t.Fatal("empty-record replay published nothing; the client needs the task pair to rebuild its channel row")
	}
}
