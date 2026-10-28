package dnfbridge

import (
	"context"
	"slices"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// dailyQuestResetRefreshPVF registers one daily, one repeatable and one one-shot
// quest that the level-5 fixture character can actually see.
//
//	200 [daily]          cleared at the boundary
//	201 [normaly repeat] never cleared
//	202 [epic]           one-shot, never cleared
func dailyQuestResetRefreshPVF() questListTestSource {
	return questListTestSource{
		dnfquest.DefaultList:  "200 `daily.qst`\n201 `repeat.qst`\n202 `oneshot.qst`\n",
		"n_quest/daily.qst":   "[grade]\n`[daily]`\n[level]\n1 10\n[job]\n`[gunner]`\n[exposed by npc]\n1\n",
		"n_quest/repeat.qst":  "[grade]\n`[normaly repeat]`\n[level]\n1 10\n[job]\n`[gunner]`\n[exposed by npc]\n1\n",
		"n_quest/oneshot.qst": "[grade]\n`[epic]`\n[level]\n1 10\n[job]\n`[gunner]`\n[exposed by npc]\n1\n",
	}
}

func buildDailyQuestResetRefreshCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	index, err := dnfpvf.Build(
		context.Background(),
		dailyQuestResetRefreshPVF(),
		dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}},
	)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// requireClearQuestListFlag asserts one quest's bit in a raw op356 body.
func requireClearQuestListFlag(t *testing.T, body []byte, questID int, want byte) {
	t.Helper()
	plain, err := zlibDecompress(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4+currentClearQuestListPayloadSize {
		t.Fatalf("op356 len=%d want=%d", len(plain), 4+currentClearQuestListPayloadSize)
	}
	if got := plain[4+questID]; got != want {
		t.Fatalf("op356 quest %d flag=%d want %d", questID, got, want)
	}
}

// dailyResetRefreshTestFixture builds the bridge fixture around one character
// whose daily was completed in an earlier service day.
func dailyResetRefreshTestFixture(t *testing.T) (*Service, *gameSession, dnfrepo.Group, *clearQuestReplayTestQueue) {
	t.Helper()
	service, session, repositories := newTownMoveTest(t)
	// The town-move fixture leaves the character without a job; the quest list and
	// the reset both need a usable level/job.
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "29",
		AccountID:   "dnf:1",
		Name:        "dailyreset",
		Job:         "2",
		Level:       5,
		Stats:       map[string]int64{"grow_type": 0, "expert_job_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	service.questCatalog = buildDailyQuestResetRefreshCatalog(t)
	yesterday := time.Now().Add(-30 * time.Hour)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "29",
		States: map[int64]dnfrepo.QuestState{
			200: {Status: "completed", UpdatedAt: yesterday},
			202: {Status: "completed", UpdatedAt: yesterday},
		},
	}); err != nil {
		t.Fatal(err)
	}
	queue := &clearQuestReplayTestQueue{now: time.Now()}
	service.gameplayTimers = queue
	// The town-move fixture returns a standalone session; register it the way a
	// real login does so the boundary sweep finds it in s.gameSessions.
	service.gameSessions = map[uint16]*gameSession{session.selectedCharacterID: session}
	return service, session, repositories, queue
}

// TestDailyQuestResetTimerRepublishesOnlineTaskState covers the case a read-path
// reset cannot reach on its own: a player who was already online when 06:00
// passed. The client keeps the task list it was last sent, so yesterday's daily
// only comes back if the server pushes the authoritative trio at the boundary
// and the push is built from already-settled state.
func TestDailyQuestResetTimerRepublishesOnlineTaskState(t *testing.T) {
	service, session, repositories, queue := dailyResetRefreshTestFixture(t)

	service.registerDailyQuestResetTimer()
	if len(queue.dailyNames) != 1 || queue.dailyNames[0] != currentDailyQuestResetTimerName {
		t.Fatalf("registered daily timers=%v, want [%s]", queue.dailyNames, currentDailyQuestResetTimerName)
	}

	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	queue.dailyCallbacks[0](queue.now)
	stream := connection.write.Bytes()

	// The trio, in the order the accept path publishes it.
	ids := currentClearQuestListFrameIDs(stream)
	wantOrder := []uint16{
		currentActiveQuestSnapshotMsgID,
		currentClearQuestListMsgID,
		currentAcceptableQuestListMsgID,
	}
	if !slices.Equal(ids, wantOrder) {
		t.Fatalf("daily reset push frame ids=%v, want %v", ids, wantOrder)
	}

	// op356 is a replacement table: the finished daily must be cleared while an
	// unrelated completed one-shot survives, so the reset is targeted.
	bitmap := currentClearQuestListBody(t, stream)
	requireClearQuestListFlag(t, bitmap, 200, 0)
	requireClearQuestListFlag(t, bitmap, 202, 1)

	// The settlement ran before the projection was built, so the daily itself is
	// gone from storage rather than merely hidden from the packet.
	record, found, err := repositories.Quest.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load quest record found=%t err=%v", found, err)
	}
	if _, exists := record.States[200]; exists {
		t.Fatal("the daily row survived the online 06:00 refresh")
	}
	if state, exists := record.States[202]; !exists || state.Status != "completed" {
		t.Fatalf("the one-shot quest row was disturbed: %+v", record.States[202])
	}
	if state, exists := record.States[201]; exists {
		t.Fatalf("an untouched repeatable quest appeared: %+v", state)
	}
}

// TestDailyQuestResetTimerRegistersAtTheServiceDayBoundary pins the moment to
// the same 06:00 line premium.ServiceDayID rolls on, so the notification and the
// authority cannot drift apart.
func TestDailyQuestResetTimerRegistersAtTheServiceDayBoundary(t *testing.T) {
	if currentDailyQuestResetHour != 6 || currentDailyQuestResetMinute != 0 {
		t.Fatalf("daily reset timer fires at %02d:%02d, want 06:00 Beijing",
			currentDailyQuestResetHour, currentDailyQuestResetMinute)
	}
	// timequeue.New defaults to a fixed UTC+8, which is the boundary
	// premium.ServiceDayID encodes, so a 06:00 registration is 06:00 Beijing
	// regardless of the host time zone.
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 9, 10, 5, 30, 0, 0, location)
	due := time.Date(2026, 9, 10, currentDailyQuestResetHour, currentDailyQuestResetMinute, 0, 0, location)
	if !due.After(now) {
		t.Fatal("probe moment is not after the boundary")
	}
	if due.UTC().Hour() != 22 || due.UTC().Day() != 9 {
		t.Fatalf("06:00 Beijing resolves to %s UTC, want 22:00 UTC on the previous day", due.UTC())
	}
}
