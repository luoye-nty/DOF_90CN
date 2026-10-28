package dnfbridge

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file pins the two repairs that make a 战况表 cell resolvable again without
// waiting for the raid's next full sweep. Both come from the same live report
// (2026-09-29 12:25:33..12:32:16, raid 1507329, four squads of four, one member
// per squad entering 218 in turn):
//
//	12:25:33 selected map=218 char=35   squad 1's SLOT-1 member enters
//	12:31:01 the board rows published   P1[1 35 36 37] P2[2 39 40 41] ...
//	12:31:22 2队's window: its own row fine, squad 1's row 单人 + black avatar
//	12:31:32 phase activation's full sweep installs every mate; the row draws
//
//   - the boundary install scope is the moved member's board ROW, not the moved
//     member alone (TestRuntimeRaidDungeonBoundaryInstallsTheMovedMembersBoardRow
//     in raid_scene_boundary_pair_scope_test.go), and
//   - the board window's own refresh re-installs the row's record, so a record
//     lost to a scene rebuild or a mid-transition drop repairs within one repaint
//     (the first test below), and
//   - a participation change schedules the row refresh instead of leaving the row
//     to whatever the client happens to ask for next (the second test below).

// TestOnlineRaidSituationWindowRefreshInstallsTheRowRecordTheCellNeeds fails if a
// C2S 695 board refresh answers with the rows but leaves the members those rows are
// keyed by without an actor record in the requesting client's user table.
//
// The board resolves one cell per (dungeon, squad) row through the record of the
// row's FIRST member -- so that is the record the refresh must install, and it must
// go to the window that asked, not to the raid at large.
func TestOnlineRaidSituationWindowRefreshInstallsTheRowRecordTheCellNeeds(t *testing.T) {
	fixture := newStressRaidProjectionFixture(t, runtimeRaidGroupSize)
	state := fixture.service.raids[fixture.raidKey]
	if state == nil {
		t.Fatal("fixture has no raid state")
	}
	// The row's first member stands in a gate: that is what makes the board draw
	// the row at all, and it is the member the cell is resolved through.
	occupant := fixture.members[0]
	occupant.session.dungeon = dungeonSessionState{runtime: &runtimeDungeonState{
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey:        fixture.raidKey,
			Phase:          state.Lifecycle.Phase,
			MapID:          218,
			LifecycleToken: 1,
		},
	}}
	// The window that asks is a squadmate, i.e. NOT the member the row starts with.
	requester := fixture.members[1]

	logPath := filepath.Join(t.TempDir(), "raid-board-row-actors.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.packetLog.Store(logger)
	handled, err := fixture.service.handleOnlineRaidSituationMonsterHp(
		requester.session, 695, []byte{0, 0, 0, 0, 0})
	if err != nil {
		t.Fatalf("the board refresh returned an error: %v", err)
	}
	if !handled {
		t.Fatal("the board refresh was not claimed by the raid handler")
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	data := stressRaidReadLog(t, logPath)
	if !strings.Contains(data, "source=raid_situation_window_board_row_actors") {
		t.Fatalf("the board refresh installed no row record at all, so the cell stays 单人 with a black avatar:\n%s",
			firstLinesMentioning(data, "game-raid-peer-actor"))
	}
	installs := raidMateActorInstalls(t, data)
	seen := map[uint16]int{}
	for _, install := range installs {
		if install.source != "raid_situation_window_board_row_actors" {
			continue
		}
		if install.target != requester.charID {
			t.Errorf("installed into %d, want the requesting window's client %d",
				install.target, requester.charID)
		}
		if install.actor != occupant.charID {
			t.Errorf("installed actor %d, want the row's first member %d",
				install.actor, occupant.charID)
		}
		seen[install.actor]++
	}
	if seen[occupant.charID] == 0 {
		t.Fatalf("the row's first member %d was never installed into the asking window %d",
			occupant.charID, requester.charID)
	}
	// One install per row, and never the window's own character into itself.
	if len(seen) != 1 {
		t.Errorf("the refresh installed %d distinct actors, want exactly the one row identity: %v",
			len(seen), seen)
	}
}

// TestRaidParticipationPublishSchedulesTheBoardRowRefresh fails if a gate's
// participation rows (584) are published without the board's own row data (634)
// following them.
//
// 86JP pairs the two ("每一次参战行变更之后都紧跟一条怪物状态", Situation.cs:550/605);
// live 2026-09-29 12:25:36 a squad entered 218 and the next 634 did not go out
// until 12:31:01, when a different squad entered -- five minutes in which the gate's
// row carried nothing but its old values.
func TestRaidParticipationPublishSchedulesTheBoardRowRefresh(t *testing.T) {
	fixture := newStressRaidProjectionFixture(t, runtimeRaidGroupSize)
	state := fixture.service.raids[fixture.raidKey]
	if state == nil {
		t.Fatal("fixture has no raid state")
	}
	queue := &raidStartRecordingTimeQueue{now: time.Now().UTC()}
	fixture.service.gameplayTimers = queue

	event := runtimeRaidDungeonLifecycleEvent{
		Kind:           runtimeRaidDungeonLoaded,
		RaidKey:        fixture.raidKey,
		Phase:          state.Lifecycle.Phase,
		MapID:          218,
		LifecycleToken: 1,
		CharacterID:    fixture.members[0].charID,
	}

	logPath := filepath.Join(t.TempDir(), "raid-participation-row-refresh.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.packetLog.Store(logger)
	if err := fixture.service.publishRuntimeRaidDungeonParticipation(event); err != nil {
		t.Fatalf("publishing the participation rows failed: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	data := stressRaidReadLog(t, logPath)
	if !strings.Contains(data, "msg_id=584") {
		t.Errorf("the participation rows were not published at all:\n%s",
			firstLinesMentioning(data, "game-raid-dungeon-lifecycle"))
	}
	want := fmt.Sprintf("raid-monster-state:%d:raid_participation_loaded", fixture.raidKey)
	found := false
	for _, name := range queue.scheduled {
		if name == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the board rows were not scheduled after the participation rows: scheduled=%v want %s",
			queue.scheduled, want)
	}
}

// TestBoardRowRefreshInstallsAtMostTheBoundedBurst pins the cost bound of the
// board-refresh self-heal: one install per row, and never more than
// currentRuntimeRaidBoardRowActorInstallLimit in a single refresh.
//
// The scope itself (one install per row) only grows with the number of squads
// standing in gates, and this path runs inside the client's own refresh request on
// that client's loop, so an unbounded burst would be paid by the very window it is
// repairing. A wider raid converges over the next refresh or two instead.
func TestBoardRowRefreshInstallsAtMostTheBoundedBurst(t *testing.T) {
	memberCount := runtimeRaidMaxGroups * runtimeRaidGroupSize
	fixture := newStressRaidProjectionFixture(t, memberCount)
	state := fixture.service.raids[fixture.raidKey]
	if state == nil {
		t.Fatal("fixture has no raid state")
	}
	// One member per squad stands in a gate, so the board has one row per squad.
	for index := 0; index < memberCount; index += runtimeRaidGroupSize {
		fixture.members[index].session.dungeon = dungeonSessionState{runtime: &runtimeDungeonState{
			raidDungeon: runtimeRaidDungeonBinding{
				RaidKey:        fixture.raidKey,
				Phase:          state.Lifecycle.Phase,
				MapID:          218,
				LifecycleToken: 1,
			},
		}}
	}
	// Ask from a squad member that is NOT its row's first member, so every row is
	// eligible and the cap is what decides the count.
	requester := fixture.members[1]

	logPath := filepath.Join(t.TempDir(), "raid-board-row-burst.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.packetLog.Store(logger)
	if _, err := fixture.service.handleOnlineRaidSituationMonsterHp(
		requester.session, 695, []byte{0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("the board refresh returned an error: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	installs := raidMateActorInstalls(t, stressRaidReadLog(t, logPath))
	rows := 0
	for _, install := range installs {
		if install.source == "raid_situation_window_board_row_actors" {
			rows++
		}
	}
	if rows == 0 {
		t.Fatal("the board refresh installed nothing at all")
	}
	if rows > currentRuntimeRaidBoardRowActorInstallLimit {
		t.Fatalf("one refresh installed %d row records, above the %d bound: that burst is paid by the client's own loop",
			rows, currentRuntimeRaidBoardRowActorInstallLimit)
	}
	// And it must be the bound, not luck: every squad is standing in a gate here.
	if rows != currentRuntimeRaidBoardRowActorInstallLimit {
		t.Errorf("refresh installed %d row records with %d squads in gates, want the bound %d",
			rows, runtimeRaidMaxGroups, currentRuntimeRaidBoardRowActorInstallLimit)
	}
}

// firstLinesMentioning keeps a failure message short enough to read: the packet log
// is megabytes long and only a handful of its lines matter to these assertions.
func firstLinesMentioning(data string, needle string) string {
	var builder strings.Builder
	count := 0
	for _, line := range strings.Split(data, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		builder.WriteString(line)
		builder.WriteByte('\n')
		count++
		if count >= 8 {
			break
		}
	}
	if count == 0 {
		return "  (no line mentions " + needle + ")"
	}
	return builder.String()
}
