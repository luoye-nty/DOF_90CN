package dnfbridge

import (
	"bytes"
	"fmt"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func newRuntimeRaidDungeonLifecycleTestService(characterID uint16) (*Service, uint32) {
	raidKey := uint32(0x10000) | uint32(characterID)
	member := runtimeRaidMemberState{CharID: characterID, GroupIndex: 1, SlotOrder: 1}
	return &Service{raids: map[uint32]*runtimeRaidState{
		raidKey: {
			RaidKey:        raidKey,
			LeaderID:       characterID,
			Members:        []runtimeRaidMemberState{member},
			FrozenMembers:  []runtimeRaidMemberState{member},
			Started:        true,
			EntryCostPaid:  true,
			Lifecycle:      newRuntimeRaidLifecycleState(time.Unix(100, 0)),
			ChargedMembers: []uint16{characterID},
		},
	}}, raidKey
}

func TestRuntimeRaidDungeonPhaseForMapUsesPVFStageSets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mapID int
		phase byte
		ok    bool
	}{
		{mapID: 209},
		{mapID: 210, phase: runtimeRaidDungeonPhaseOne, ok: true},
		{mapID: 216, phase: runtimeRaidDungeonPhaseOne, ok: true},
		{mapID: 217},
		{mapID: 218, phase: runtimeRaidDungeonPhaseTwo, ok: true},
		{mapID: 224, phase: runtimeRaidDungeonPhaseTwo, ok: true},
		{mapID: 225},
	}
	for _, test := range tests {
		phase, ok := runtimeRaidDungeonPhaseForMap(test.mapID)
		if phase != test.phase || ok != test.ok {
			t.Fatalf("map %d phase=(%d,%t), want=(%d,%t)", test.mapID, phase, ok, test.phase, test.ok)
		}
	}
}

func TestRuntimeRaidDungeonLifecycleLeavesOrdinaryDungeonUnchanged(t *testing.T) {
	t.Parallel()
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 700},
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
	}
	session := &gameSession{
		selectedCharacterID: 19,
		dungeon:             dungeonSessionState{runtime: runtime},
	}

	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	session.dungeon.mu.Lock()
	if err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	binding := runtime.raidDungeon
	session.dungeon.mu.Unlock()

	if binding != (runtimeRaidDungeonBinding{}) {
		t.Fatalf("ordinary dungeon gained raid binding: %+v", binding)
	}
	service.raidMu.Lock()
	state := service.raids[raidKey]
	dungeonCount := len(state.Lifecycle.Dungeons)
	occupancyCount := len(state.Lifecycle.GroupOccupancy)
	service.raidMu.Unlock()
	if dungeonCount != 0 || occupancyCount != 0 {
		t.Fatalf("ordinary dungeon mutated raid state: dungeons=%d occupancy=%d", dungeonCount, occupancyCount)
	}
}

func TestRuntimeRaidDungeonLifecycleCompletesAttemptAndRetainsGlobalRaid(t *testing.T) {
	t.Parallel()
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
	}
	session := &gameSession{selectedCharacterID: 19, dungeon: dungeonSessionState{runtime: runtime}}

	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatalf("duplicate loaded event: %v", err)
	}
	session.dungeon.mu.Lock()
	if err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatalf("duplicate completed event: %v", err)
	}
	binding := runtime.raidDungeon
	session.dungeon.mu.Unlock()

	service.raidMu.Lock()
	state := service.raids[raidKey]
	if state == nil {
		service.raidMu.Unlock()
		t.Fatal("completed map deleted global raid before rewards")
	}
	progress := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]
	occupancyCount := len(state.Lifecycle.GroupOccupancy)
	terminal := state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{CharacterID: 19, LifecycleKey: 41}]
	service.raidMu.Unlock()

	if binding.RaidKey != raidKey || binding.Phase != runtimeRaidDungeonPhaseOne ||
		binding.MapID != 210 || binding.LifecycleToken != 41 ||
		!binding.selectedNotified || !binding.loadedNotified || !binding.completedNotified || binding.abandonedNotified {
		t.Fatalf("completed binding=%+v", binding)
	}
	if progress.Status != runtimeRaidDungeonProgressCompleted || occupancyCount != 0 || terminal.Kind != runtimeRaidDungeonCompleted {
		t.Fatalf("completed progress=%+v occupancy=%d terminal=%+v", progress, occupancyCount, terminal)
	}
}

// A cleared gate has to register for the members that cleared it: the current
// client renders the 攻击队 window's 地下城通关 column from the member record's
// clear byte, and the phase settlement rewards only members it can prove cleared
// a gate. Live report 2026-09-16: both members of a squad cleared gate 210 and
// the column still read 0/1, so the phase end flipped no cards.
func TestRuntimeRaidDungeonClearRegistersPerMemberPhaseCount(t *testing.T) {
	t.Parallel()
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	service.raidMu.Lock()
	state := service.raids[raidKey]
	// A second group clears nothing: only the clearing squad may register.
	state.Members = append(state.Members, runtimeRaidMemberState{CharID: 20, GroupIndex: 2, SlotOrder: 1})
	state.FrozenMembers = append(state.FrozenMembers, runtimeRaidMemberState{CharID: 20, GroupIndex: 2, SlotOrder: 1})
	state.AntonConfig = &raid.AntonConfig{Phases: []raid.AntonPhase{{
		Name: "phase one", MinimalDungeonClearCount: 1,
	}}}
	service.raidMu.Unlock()

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
	}
	session := &gameSession{selectedCharacterID: 19, dungeon: dungeonSessionState{runtime: runtime}}
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	session.dungeon.mu.Lock()
	err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime)
	session.dungeon.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	service.raidMu.Lock()
	state = service.raids[raidKey]
	counts := make(map[uint16]uint32, len(state.Lifecycle.PhaseClearCounts))
	for charID, count := range state.Lifecycle.PhaseClearCounts {
		counts[charID] = count
	}
	refresh, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(*state))
	service.raidMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if counts[19] != 1 || counts[20] != 0 {
		t.Fatalf("phase clear registration=%v want member 19:1 member 20:0", counts)
	}
	// The registration is capped at the phase's own requirement, so the column can
	// never read higher than the value it is measured against.
	if minimum := runtimeRaidPhaseMinimalClearCount(*state, runtimeRaidDungeonPhaseOne); minimum != 1 {
		t.Fatalf("phase minimum clear count=%d want=1", minimum)
	}
	// The member record must carry it in the byte the client reads for the column.
	leader := runtimeRaidMemberRecord(*state, state.Members[0])
	if leader.Field56 != 1 {
		t.Fatalf("leader member record clear byte=%d want=1", leader.Field56)
	}
	if other := runtimeRaidMemberRecord(*state, state.Members[1]); other.Field56 != 0 {
		t.Fatalf("non-clearing member record clear byte=%d want=0", other.Field56)
	}
	// The wire body must carry the byte: the only difference between a member
	// list with and without the registration is that single record byte.
	unregistered := *state
	unregistered.Lifecycle = state.Lifecycle
	unregistered.Lifecycle.PhaseClearCounts = nil
	withoutCount, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(unregistered))
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutCount) != len(refresh) {
		t.Fatalf("member refresh length changed: %d vs %d", len(refresh), len(withoutCount))
	}
	changed := -1
	for index := range refresh {
		if refresh[index] == withoutCount[index] {
			continue
		}
		if changed >= 0 {
			t.Fatalf("more than one refresh byte differs: %d and %d", changed, index)
		}
		changed = index
	}
	if changed < 0 || refresh[changed] != 1 || withoutCount[changed] != 0 {
		t.Fatalf("member refresh does not carry the clear byte: diff=%d refresh=%x base=%x",
			changed, refresh, withoutCount)
	}

	// The next phase starts every member at zero again (86JP clears all personal
	// counters when phase two begins).
	service.raidMu.Lock()
	_ = initializeRuntimeRaidPhaseObjectives(&state.Lifecycle, state.AntonConfig, runtimeRaidDungeonPhaseTwo)
	clear(state.Lifecycle.PhaseClearCounts)
	remaining := len(state.Lifecycle.PhaseClearCounts)
	service.raidMu.Unlock()
	if remaining != 0 {
		t.Fatalf("phase two retained %d clear registrations", remaining)
	}
}

func TestRuntimeRaidDungeonParticipationPublishesOnceAfterFinishLoading(t *testing.T) {
	service, _ := newRuntimeRaidDungeonLifecycleTestService(19)
	connection := &bufferConn{}
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 5314},
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
	}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 19,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	service.gameSessions = map[uint16]*gameSession{19: session}

	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if !runtime.raidDungeon.selectedNotified {
		t.Fatalf("selection was not reserved internally: %+v", runtime.raidDungeon)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("op16 selection published premature raid participation=%x", connection.write.Bytes())
	}

	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	// op=0 first: the client's op=0 branch (sub_17C0FB0) drops that dungeon's
	// squad row, so re-entering the same gate cannot stack a second twin row.
	erase, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantErase := []byte{1, 0xc2, 0x14, 0, 0, 0, 1, 19, 0}
	if erase.Header.MsgID != raid.RaidParticipationMsgID ||
		erase.Header.Classification != 0 || !bytes.Equal(erase.Body, wantErase) {
		t.Fatalf("loaded erase header=%+v body=%x want=%x", erase.Header, erase.Body, wantErase)
	}
	packet, rest := splitGameServerUpperPacket(t, rest)
	wantBody := []byte{1, 0xc2, 0x14, 0, 0, 1, 1, 19, 0}
	if packet.Header.MsgID != raid.RaidParticipationMsgID ||
		packet.Header.Classification != 0 || !bytes.Equal(packet.Body, wantBody) || len(rest) != 0 {
		t.Fatalf("loaded participation header=%+v body=%x rest=%x want=%x", packet.Header, packet.Body, rest, wantBody)
	}
	firstLen := connection.write.Len()
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != firstLen {
		t.Fatalf("duplicate FINISH_LOADING republished participation: first=%d now=%d", firstLen, connection.write.Len())
	}
}

func TestRuntimeRaidSingleParticipationProjectsActualPVFDungeonToClientSelector(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	state := service.raids[raidKey]
	state.TailFlag = 1
	state.SingleRaidType = 0
	state.Lifecycle.Objectives[5314] = runtimeRaidDungeonObjective{
		Phase:       runtimeRaidDungeonPhaseOne,
		MapID:       5314,
		State:       raid.AntonDungeonOpen,
		MaxHP:       1,
		RemainingHP: 1,
		PartyMax:    1,
	}
	connection := &bufferConn{}
	runtime := &runtimeDungeonState{
		Request:         dungeoncmd.SelectDungeonRequest{DungeonID: 5314},
		clientDungeonID: 210,
		Character:       dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken:  41,
	}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 19,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	service.gameSessions = map[uint16]*gameSession{19: session}

	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	erase, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantErase := []byte{1, 0xd2, 0, 0, 0, 0, 1, 19, 0}
	if erase.Header.MsgID != raid.RaidParticipationMsgID ||
		erase.Header.Classification != 0 || !bytes.Equal(erase.Body, wantErase) {
		t.Fatalf("single loaded erase header=%+v body=%x want=%x", erase.Header, erase.Body, wantErase)
	}
	packet, rest := splitGameServerUpperPacket(t, rest)
	wantBody := []byte{1, 0xd2, 0, 0, 0, 1, 1, 19, 0}
	if packet.Header.MsgID != raid.RaidParticipationMsgID ||
		packet.Header.Classification != 0 || !bytes.Equal(packet.Body, wantBody) || len(rest) != 0 {
		t.Fatalf("single loaded participation header=%+v body=%x rest=%x want=%x", packet.Header, packet.Body, rest, wantBody)
	}
}

func TestSendFinishLoadingStatusReportsRaidDungeonLoadedAfterCurrentFlow(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                               connection,
		selectedCharacterID:                19,
		dungeon:                            dungeonSessionState{runtime: runtime},
		postStartMapPlayerStateSent:        true,
		currentFinishLoadingStateSent:      true,
		currentFinishLoadingCompletionSent: true,
		postFinishLoadingPlayerStateSent:   true,
	}

	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.raidDungeon.loadedNotified {
		t.Fatalf("binding loaded before op37: %+v", runtime.raidDungeon)
	}
	if err := service.sendFinishLoadingStatus(session, make([]byte, currentFinishLoadingProtectedRequestBodySize)); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() == 0 {
		t.Fatal("op37 status ACK was not written")
	}
	if !runtime.raidDungeon.loadedNotified {
		t.Fatalf("binding not loaded after op37: %+v", runtime.raidDungeon)
	}

	service.raidMu.Lock()
	state := service.raids[raidKey]
	progress := runtimeRaidDungeonProgress{}
	if state != nil {
		progress = state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]
	}
	service.raidMu.Unlock()
	if state == nil || progress.Status != runtimeRaidDungeonProgressLoaded || progress.LoadedAt.IsZero() {
		t.Fatalf("global raid=%+v progress=%+v", state, progress)
	}
}

func TestRuntimeRaidDungeonLifecycleAbandonsAttemptAndRetainsGlobalRaid(t *testing.T) {
	t.Parallel()
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(23)
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "23"},
		lifecycleToken: 51,
	}
	session := &gameSession{selectedCharacterID: 23, dungeon: dungeonSessionState{runtime: runtime}}

	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	session.dungeon.mu.Lock()
	if err := service.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatalf("duplicate abandoned event: %v", err)
	}
	binding := runtime.raidDungeon
	session.dungeon.mu.Unlock()

	service.raidMu.Lock()
	state := service.raids[raidKey]
	if state == nil {
		service.raidMu.Unlock()
		t.Fatal("abandoned map attempt deleted global raid")
	}
	_, mapStillActive := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]
	occupancyCount := len(state.Lifecycle.GroupOccupancy)
	terminal := state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{CharacterID: 23, LifecycleKey: 51}]
	service.raidMu.Unlock()

	if !binding.selectedNotified || !binding.abandonedNotified || binding.completedNotified {
		t.Fatalf("abandoned binding=%+v", binding)
	}
	if mapStillActive || occupancyCount != 0 || terminal.Kind != runtimeRaidDungeonAbandoned {
		t.Fatalf("abandoned map_active=%t occupancy=%d terminal=%+v", mapStillActive, occupancyCount, terminal)
	}
}

func TestUnbindGameSessionReleasesCurrentRaidAttemptAndRetainsRaidProgress(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	raidFixture, raidKey := newRuntimeRaidDungeonLifecycleTestService(session.selectedCharacterID)
	service.raids = raidFixture.raids
	runtime.Request.DungeonID = 210
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	service.gameSessions = map[uint16]*gameSession{session.selectedCharacterID: session}

	service.unbindGameSession(session)

	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("disconnected run status=%s want abandoned", status)
	}
	if session.dungeon.runtime != nil || !runtime.payloadReleased {
		t.Fatalf("disconnected runtime=%p payload_released=%t", session.dungeon.runtime, runtime.payloadReleased)
	}
	if _, online := service.gameSessions[session.selectedCharacterID]; online {
		t.Fatal("disconnected session remained canonical")
	}
	service.raidMu.Lock()
	state := service.raids[raidKey]
	_, activeAttempt := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]
	occupancyCount := len(state.Lifecycle.GroupOccupancy)
	terminal := state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
		CharacterID:  session.selectedCharacterID,
		LifecycleKey: runtime.lifecycleToken,
	}]
	service.raidMu.Unlock()
	if state == nil || activeAttempt || occupancyCount != 0 || terminal.Kind != runtimeRaidDungeonAbandoned {
		t.Fatalf("raid=%+v active_attempt=%t occupancy=%d terminal=%+v", state, activeAttempt, occupancyCount, terminal)
	}
}

func TestDungeonLifecycleTokenDoesNotRestartWithFreshSession(t *testing.T) {
	t.Parallel()
	service := &Service{}
	firstSessionToken := service.allocateDungeonLifecycleToken()
	secondSessionToken := service.allocateDungeonLifecycleToken()
	if firstSessionToken == 0 || secondSessionToken == 0 || firstSessionToken == secondSessionToken {
		t.Fatalf("process lifecycle tokens first=%d second=%d", firstSessionToken, secondSessionToken)
	}
}

func TestRuntimeRaidDungeonLifecycleRejectsStaleToken(t *testing.T) {
	t.Parallel()
	service := &Service{}
	runtime := &runtimeDungeonState{
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 42,
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey:        100,
			Phase:          runtimeRaidDungeonPhaseOne,
			MapID:          210,
			LifecycleToken: 41,
		},
	}
	session := &gameSession{selectedCharacterID: 19, dungeon: dungeonSessionState{runtime: runtime}}

	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	if runtime.raidDungeon.selectedNotified || runtime.raidDungeon.loadedNotified {
		t.Fatalf("stale lifecycle token advanced binding: %+v", runtime.raidDungeon)
	}
}

func TestRuntimeRaidDungeonLifecycleTerminalEventsAreExclusiveAndIdempotent(t *testing.T) {
	t.Parallel()
	service := &Service{}
	runtime := &runtimeDungeonState{
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey:           100,
			Phase:             runtimeRaidDungeonPhaseOne,
			MapID:             210,
			LifecycleToken:    41,
			selectedNotified:  true,
			loadedNotified:    true,
			completedNotified: true,
		},
	}
	session := &gameSession{selectedCharacterID: 19, dungeon: dungeonSessionState{runtime: runtime}}

	session.dungeon.mu.Lock()
	if err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
		session.dungeon.mu.Unlock()
		t.Fatal(err)
	}
	binding := runtime.raidDungeon
	session.dungeon.mu.Unlock()

	if !binding.selectedNotified || !binding.loadedNotified || !binding.completedNotified || binding.abandonedNotified {
		t.Fatalf("unexpected lifecycle flags: %+v", binding)
	}
}

func TestCommitPendingDungeonReturnReportsRaidAttemptAbandoned(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	raidFixture, raidKey := newRuntimeRaidDungeonLifecycleTestService(session.selectedCharacterID)
	service.raids = raidFixture.raids
	runtime.Request.DungeonID = 210
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.commitPendingDungeonReturnForSceneRequest(session, "raid_lifecycle_abandon_test"); err != nil {
		t.Fatal(err)
	}

	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("run status=%s, want abandoned", status)
	}
	if session.dungeon.runtime != nil || !runtime.raidDungeon.abandonedNotified {
		t.Fatalf("runtime owner=%p binding=%+v", session.dungeon.runtime, runtime.raidDungeon)
	}
	service.raidMu.Lock()
	state := service.raids[raidKey]
	terminal := state.Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
		CharacterID:  session.selectedCharacterID,
		LifecycleKey: runtime.lifecycleToken,
	}]
	service.raidMu.Unlock()
	if state == nil || terminal.Kind != runtimeRaidDungeonAbandoned {
		t.Fatalf("global raid=%+v terminal=%+v", state, terminal)
	}
}

func TestBossSettlementReportsRaidMapCompletedBeforeGlobalCleanup(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	raidFixture, raidKey := newRuntimeRaidDungeonLifecycleTestService(session.selectedCharacterID)
	service.raids = raidFixture.raids
	runtime.Request.DungeonID = 210
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatal(err)
	}
	runtime.bossDieCheckAccepted = true
	runtime.bossDieCheckTargetObjectKey = 1

	session.dungeon.mu.Lock()
	err := service.completeCurrentDungeonBossDieCheckLocked(session, runtime, "raid_lifecycle_completion_test")
	session.dungeon.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.raidDungeon.completedNotified {
		t.Fatalf("binding=%+v", runtime.raidDungeon)
	}
	service.raidMu.Lock()
	state := service.raids[raidKey]
	progress := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]
	service.raidMu.Unlock()
	if state == nil || progress.Status != runtimeRaidDungeonProgressCompleted {
		t.Fatalf("global raid=%+v progress=%+v", state, progress)
	}
}

func TestRaidCompletionPublishFailureKeepsTheClearedRunSettlement(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	connection := session.conn.(*bufferConn)
	raidFixture, raidKey := newRuntimeRaidDungeonLifecycleTestService(session.selectedCharacterID)
	service.raids = raidFixture.raids
	runtime.Request.DungeonID = 210
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	// Retire the authoritative attempt after the binding was frozen, so the
	// completion publish fails the way the live 2026-09-16 11:14:18Z incident did.
	// That bookkeeping failure must never cost the player the connection: inbound
	// packets are fatal async events, so returning the error closed the socket at
	// the exact moment the raid member's gate cleared.
	delete(service.raids, raidKey)
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatal(err)
	}
	runtime.bossDieCheckAccepted = true
	runtime.bossDieCheckTargetObjectKey = 1
	connection.write.Reset()

	session.dungeon.mu.Lock()
	err := service.completeCurrentDungeonBossDieCheckLocked(session, runtime, "raid_completion_publish_failure_test")
	session.dungeon.mu.Unlock()
	if err != nil {
		t.Fatalf("cleared run settlement failed on raid bookkeeping: %v", err)
	}
	if runtime.raidDungeon.completedNotified {
		t.Fatalf("failed publish marked the binding completed: %+v", runtime.raidDungeon)
	}
	found := false
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes()) {
		if packet.Header.MsgID == currentDungeonSettlementEntryMsgID {
			found = true
		}
	}
	if !found {
		t.Fatalf("cleared run emitted no settlement entry stream=%x", connection.write.Bytes())
	}
}

func TestFinalSingleRaidDungeonHandsSettlementToNativeResultCards(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	// The phase tail is no longer published on the completing member's own call
	// stack (it force-returns teammates and therefore must not run under that
	// member's session lock -- see scheduleRuntimeRaidPhaseCompletion). Capture it
	// in a deterministic queue and run it once, exactly like the gameplay timer
	// goroutine would.
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	connection := session.conn.(*bufferConn)
	config := runtimeRaidBridgeTestSingleAntonConfig()
	now := time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC)
	lifecycle, err := newRuntimeRaidLifecycleStateFromAntonModeAndType(config, now, true, 3)
	if err != nil {
		t.Fatalf("create single lifecycle: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.PhaseStartedAt = now
	lifecycle.PhaseDeadline = now.Add(35 * time.Minute)
	for mapID, objective := range lifecycle.Objectives {
		objective.State = raid.AntonDungeonHide
		objective.RemainingHP = 0
		lifecycle.Objectives[mapID] = objective
	}
	heart := lifecycle.Objectives[5320]
	heart.State = raid.AntonDungeonOpen
	heart.RemainingHP = 1
	lifecycle.Objectives[5320] = heart

	characterID := session.selectedCharacterID
	raidKey := uint32(0x00170000) | uint32(characterID)
	member := runtimeRaidMemberState{CharID: characterID, GroupIndex: 1}
	service.raids = map[uint32]*runtimeRaidState{
		raidKey: {
			RaidKey:        raidKey,
			LeaderID:       characterID,
			TailFlag:       1,
			SingleRaidType: 3,
			AntonConfig:    config,
			Members:        []runtimeRaidMemberState{member},
			FrozenMembers:  []runtimeRaidMemberState{member},
			Started:        true,
			EntryCostPaid:  true,
			ChargedMembers: []uint16{characterID},
			Lifecycle:      lifecycle,
		},
	}
	service.gameSessions = map[uint16]*gameSession{characterID: session}
	runtime.Request.DungeonID = 5320
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatalf("freeze final heart: %v", err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatalf("load final heart: %v", err)
	}
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	runtime.bossDieCheckAccepted = true
	runtime.bossDieCheckTargetObjectKey = 1
	connection.write.Reset()

	session.dungeon.mu.Lock()
	err = service.completeCurrentDungeonBossDieCheckLocked(session, runtime, "single_raid_final_native_test")
	session.dungeon.mu.Unlock()
	if err != nil {
		t.Fatalf("complete final single raid settlement: %v", err)
	}
	// Run the deferred phase tail the timer goroutine would have run.
	tailTask := queue.taskByName(fmt.Sprintf("raid-phase-complete:%d:%d", raidKey, runtimeRaidDungeonPhaseOne))
	if tailTask == nil {
		t.Fatal("the completed single raid phase scheduled no tail")
	}
	queue.fire(tailTask, false)

	var sawResult, sawState3, sawBossResponse, sawOrdinaryEntry bool
	packetIndex, bossResponseIndex, resultIndex := 0, -1, -1
	stream := connection.write.Bytes()
	for len(stream) != 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		switch packet.Header.MsgID {
		case raid.RaidResultMsgID:
			sawResult = true
			resultIndex = packetIndex
		case raid.RaidStateMsgID:
			if bytes.Equal(packet.Body, raid.BuildRaidStateBody(raid.RaidState{State: 3})) {
				sawState3 = true
			}
		case 115:
			sawBossResponse = true
			bossResponseIndex = packetIndex
		case currentDungeonSettlementEntryMsgID:
			sawOrdinaryEntry = true
		}
		packetIndex++
		stream = rest
	}
	if !sawResult || !sawState3 || !sawBossResponse || sawOrdinaryEntry || runtime.settlementEntrySent {
		t.Fatalf("result=%t state3=%t op115=%t op31=%t settlement_sent=%t",
			sawResult, sawState3, sawBossResponse, sawOrdinaryEntry, runtime.settlementEntrySent)
	}
	if bossResponseIndex < 0 || resultIndex <= bossResponseIndex {
		t.Fatalf("native raid result ordering op115_index=%d result_index=%d", bossResponseIndex, resultIndex)
	}
	if got := service.raids[raidKey].Lifecycle.Status; got != runtimeRaidLifecycleAwaitingMovie {
		t.Fatalf("final lifecycle status=%d want awaiting movie", got)
	}
}

// TestRuntimeRaidCompletedPublishesClearNoticeForEveryGate pins the op=4 row of
// 0x0248: handler sub_1D438D0 prints nothing for op=0 and
// "其中1支战队已通关[%s]地下城。" (dstr 32049) for op=4. The old whitelist in
// publishRuntimeRaidDungeonParticipation dropped op=4 for 213/215/220 -- the gates
// whose first clear still leaves RemainingHP > 0 -- so a squadmate's newest notice
// stayed the previous dungeon (live 2026-09-16T14:52:09Z: 擎天之柱 A cleared, no
// line, and the 擎天之柱 notice only appeared 10s later on the second clear).
func TestRuntimeRaidCompletedPublishesClearNoticeForEveryGate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		mapID       int
		maxHP       int
		remainingHP int
	}{
		{name: "first clear of a two-stage gate", mapID: 213, maxHP: 2, remainingHP: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
			phase, phaseOK := runtimeRaidDungeonPhaseForMap(test.mapID)
			if !phaseOK {
				t.Fatalf("map %d has no raid phase", test.mapID)
			}
			service.raidMu.Lock()
			state := service.raids[raidKey]
			state.Lifecycle.Phase = phase
			state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
				test.mapID: {
					Phase: phase, MapID: test.mapID,
					State: raid.AntonDungeonOpen, MaxHP: test.maxHP, RemainingHP: test.remainingHP, PartyMax: 5,
				},
			}
			service.raidMu.Unlock()

			runtime := &runtimeDungeonState{
				Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(test.mapID)},
				Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
				lifecycleToken: 41,
			}
			connection := &bufferConn{}
			session := &gameSession{
				conn:                connection,
				selectedCharacterID: 19,
				dungeon:             dungeonSessionState{runtime: runtime},
			}
			service.gameSessions = map[uint16]*gameSession{19: session}
			if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
				t.Fatal(err)
			}
			if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
				t.Fatal(err)
			}
			before := connection.write.Len()
			session.dungeon.mu.Lock()
			err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime)
			session.dungeon.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			rest := connection.write.Bytes()[before:]
			found := false
			ops := make([]int, 0, 2)
			for len(rest) > 0 {
				packet, trailing := splitGameServerUpperPacket(t, rest)
				rest = trailing
				if packet.Header.MsgID != raid.RaidParticipationMsgID || len(packet.Body) < 6 {
					continue
				}
				if packet.Body[0] != 1 {
					continue
				}
				if got := int(packet.Body[1]) | int(packet.Body[2])<<8 | int(packet.Body[3])<<16 | int(packet.Body[4])<<24; got != test.mapID {
					continue
				}
				ops = append(ops, int(packet.Body[5]))
				if int(packet.Body[5]) != 4 {
					continue
				}
				found = true
			}
			if !found {
				t.Fatalf("map %d completed without an op=4 clear notice: %x", test.mapID, connection.write.Bytes()[before:])
			}
			// The erase op=0 erases this dungeon's squad row in the client, so it has
			// to run BEFORE the clear notice that names the row. Leaving it after
			// op=4 blanked the 战况表 cell on every clear; dropping it entirely also
			// stopped the 黑雾之源 cutscene (live 2026-09-17T07:4xZ).
			if len(ops) != 2 || ops[0] != 0 || ops[1] != 4 {
				t.Fatalf("map %d clear published ops=%v, want [0 4]", test.mapID, ops)
			}
		})
	}
}

// raidParticipationOpsForTest collects the op bytes of every 584 row a captured
// raidParticipationRowsForTest decodes every 584 row a captured write buffer carries
// for one dungeon id.
func raidParticipationRowsForTest(t *testing.T, raw []byte, mapID int) []raidParticipationRowForTest {
	t.Helper()
	rows := make([]raidParticipationRowForTest, 0, 2)
	for len(raw) > 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		if packet.Header.MsgID != raid.RaidParticipationMsgID || len(packet.Body) < 6 || packet.Body[0] != 1 {
			continue
		}
		if got := int(packet.Body[1]) | int(packet.Body[2])<<8 | int(packet.Body[3])<<16 | int(packet.Body[4])<<24; got != mapID {
			continue
		}
		count := int(packet.Body[6])
		keys := make([]uint16, 0, count)
		for index := 0; index < count && 7+index*2+1 < len(packet.Body); index++ {
			keys = append(keys, uint16(packet.Body[7+index*2])|uint16(packet.Body[8+index*2])<<8)
		}
		rows = append(rows, raidParticipationRowForTest{Op: int(packet.Body[5]), Keys: keys})
	}
	return rows
}

type raidParticipationRowForTest struct {
	Op   int
	Keys []uint16
}

// write buffer carries for one dungeon id.
func raidParticipationOpsForTest(t *testing.T, raw []byte, mapID int) []int {
	t.Helper()
	rows := raidParticipationRowsForTest(t, raw, mapID)
	ops := make([]int, 0, len(rows))
	for _, row := range rows {
		ops = append(ops, row.Op)
	}
	return ops
}

// An unassigned member still gets its 584 row -- it IS inside the dungeon and the node
// has to show it -- but the row's CONTENT says who is in it: nobody but that member.
// Folding it onto squad 1 (runtimeRaidNormalizedGroup) is what put squad 1's whole
// roster into the row an unassigned member opened, which drew it as squad 1 in the
// 战况表 (live report 2026-09-17 21:5x). The squad a member belongs to is not in this
// packet at all: the client reads each member's own roster byte (op 0x024F), where an
// unassigned member's 0 renders as 单人.
func TestRuntimeRaidUnassignedMemberPublishesARowWithOnlyItself(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	service.raids[raidKey].Members = []runtimeRaidMemberState{
		{CharID: 19, Name: "squad-one", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "standby", GroupIndex: 0},
	}
	service.raids[raidKey].FrozenMembers = append([]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	service.raids[raidKey].Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	service.raids[raidKey].Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		210: {
			Phase: runtimeRaidDungeonPhaseOne, MapID: 210,
			State: raid.AntonDungeonOpen, MaxHP: 4, RemainingHP: 4, PartyMax: 5,
		},
	}

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "20"},
		lifecycleToken: 41,
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 20,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	// 584 rows are broadcast to the raid's online members, so the session has to be
	// reachable for the row to be written at all.
	service.gameSessions = map[uint16]*gameSession{20: session}
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	rows := raidParticipationRowsForTest(t, connection.write.Bytes(), 210)
	if len(rows) != 2 || rows[0].Op != 0 || rows[1].Op != 1 {
		t.Fatalf("an unassigned member's entry published rows=%+v, want [0 1]", rows)
	}
	for _, row := range rows {
		if len(row.Keys) != 1 || row.Keys[0] != 20 {
			t.Fatalf("row op=%d keys=%v, want only the unassigned member", row.Op, row.Keys)
		}
	}

	before := connection.write.Len()
	session.dungeon.mu.Lock()
	err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime)
	session.dungeon.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// The clear notice still goes out with the erase that precedes it, exactly like a
	// squad's clear: op=0 erases this dungeon's row and op=4 is the raid-wide chat.
	clearRows := raidParticipationRowsForTest(t, connection.write.Bytes()[before:], 210)
	if len(clearRows) != 2 || clearRows[0].Op != 0 || clearRows[1].Op != 4 {
		t.Fatalf("an unassigned member's clear published rows=%+v, want [0 4]", clearRows)
	}
	for _, row := range clearRows {
		if len(row.Keys) != 1 || row.Keys[0] != 20 {
			t.Fatalf("clear row op=%d keys=%v, want only the unassigned member", row.Op, row.Keys)
		}
	}
}

// 2026-09-30 23:54:54 现场回归（raid 1835009，char 22 未编制，解密包日志）：
// 未编制成员进图时 584 行发出来是 **空成员表** `mc=0 keys=[]`，客户端于是没有任何
// 成员可分类，战况表格子与提示标志都画不出来。服主原话：「然后单人（没队伍编制
// 显示的标志不对 应该显示单）」。
//
// 成因：行内小队号取自 FROZEN 名册里那个成员的小队字节，而成员过滤用的是 LIVE 行。
// 两者可以合法地不一致 —— 攻坚进行中被移出小队的成员，frozen 行仍保留原小队用于阶段
// 通关记功（clearRuntimeRaidSquadLabel：`frozen_row=kept_for_phase_credit`），而
// live 的 0x024F 字节已经变成 0。于是"用 frozen 的 2 去筛 live 的人"筛出空集。
//
// 这条用例同时钉死修复的两半，缺一半都会红：
//
//	① 行不能是空的（现场就是空表）—— 否则客户端没东西可分类；
//	② 行不能错列成 frozen 的旧队 —— 所以夹具特意让 2 队里**还有活人**（18），
//	   只做①不做②的话这一行会点名 18，而不是真正在图里的 20。
func TestRuntimeRaidDriftedFrozenSquadStillPublishesARowNamingTheMover(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	state := service.raids[raidKey]
	// live：20 已被移出小队(0)；18 仍在 2 队；19 在 1 队。
	// frozen：20 还留着 2 队（阶段通关记功用）。
	state.Members = []runtimeRaidMemberState{
		{CharID: 18, Name: "squad-two", GroupIndex: 2, SlotOrder: 0},
		{CharID: 19, Name: "squad-one", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "drifted", GroupIndex: 0},
	}
	state.FrozenMembers = []runtimeRaidMemberState{
		{CharID: 18, Name: "squad-two", GroupIndex: 2, SlotOrder: 0},
		{CharID: 19, Name: "squad-one", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "drifted", GroupIndex: 2, SlotOrder: 1},
	}
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		210: {
			Phase: runtimeRaidDungeonPhaseOne, MapID: 210,
			State: raid.AntonDungeonOpen, MaxHP: 4, RemainingHP: 4, PartyMax: 5,
		},
	}

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "20"},
		lifecycleToken: 41,
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 20,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	service.gameSessions = map[uint16]*gameSession{20: session}
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	rows := raidParticipationRowsForTest(t, connection.write.Bytes(), 210)
	if len(rows) != 2 || rows[0].Op != 0 || rows[1].Op != 1 {
		t.Fatalf("frozen/live 漂移的未编制成员进图发布了 rows=%+v，want [0 1]", rows)
	}
	for _, row := range rows {
		if len(row.Keys) == 0 {
			t.Fatalf("op=%d 的参战行一个成员都没写（现场 mc=0）：客户端没法把这一格画成「单」", row.Op)
		}
		if len(row.Keys) != 1 || row.Keys[0] != 20 {
			t.Fatalf("op=%d keys=%v，want 恰好 [20]（既不能空，也不能错列成 frozen 旧队里的 18）", row.Op, row.Keys)
		}
	}
}

// A real squad's row lists that squad and only that squad, even while an unassigned
// member is inside the same gate under its own seat.
func TestRuntimeRaidSquadRowExcludesTheUnassignedMember(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	service.raids[raidKey].Members = []runtimeRaidMemberState{
		{CharID: 19, Name: "squad-one", GroupIndex: 1, SlotOrder: 0},
		{CharID: 21, Name: "squadmate", GroupIndex: 1, SlotOrder: 1},
		{CharID: 20, Name: "standby", GroupIndex: 0},
	}
	service.raids[raidKey].FrozenMembers = append([]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	service.raids[raidKey].Lifecycle.Phase = runtimeRaidDungeonPhaseOne

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: "19"},
		lifecycleToken: 41,
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 19,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	service.gameSessions = map[uint16]*gameSession{19: session}
	if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		t.Fatal(err)
	}
	rows := raidParticipationRowsForTest(t, connection.write.Bytes(), 210)
	if len(rows) == 0 || rows[len(rows)-1].Op != 1 {
		t.Fatalf("squad entry rows=%+v", rows)
	}
	keys := rows[len(rows)-1].Keys
	if len(keys) != 2 || keys[0] != 19 || keys[1] != 21 {
		t.Fatalf("squad row keys=%v, want the squad's two members and not the unassigned one", keys)
	}
}

// The clear registration follows the same rule: an unassigned member's clear fills
// its own 地下城通关 cell and nobody else's, while a squad's clear still registers the
// whole squad.
func TestRuntimeRaidClearRegistrationFollowsTheSquad(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	service.raids[raidKey].Members = []runtimeRaidMemberState{
		{CharID: 19, Name: "squad-one", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "squadmate", GroupIndex: 1, SlotOrder: 1},
		{CharID: 21, Name: "standby", GroupIndex: 0},
	}
	service.raids[raidKey].FrozenMembers = append([]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	state := service.raids[raidKey]

	runtimeRaidNotePhaseClearLocked(state, 21, runtimeRaidDungeonPhaseOne)
	if counts := state.Lifecycle.PhaseClearCounts; counts[21] != 1 || counts[19] != 0 || counts[20] != 0 {
		t.Fatalf("clear counts=%+v want only the unassigned member registered", counts)
	}

	runtimeRaidNotePhaseClearLocked(state, 19, runtimeRaidDungeonPhaseOne)
	if counts := state.Lifecycle.PhaseClearCounts; counts[19] != 1 || counts[20] != 1 || counts[21] != 1 {
		t.Fatalf("clear counts=%+v want the whole squad plus the standing member", counts)
	}
}

// The reveal board is a squad's board: an unassigned member's row travels on its own
// board instead of riding along with squad 1's.
func TestRuntimeRaidRewardSplitKeepsTheUnassignedMemberApart(t *testing.T) {
	delivery := runtimeRaidSettlementRewardDelivery{
		Raid: runtimeRaidState{FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1},
			{CharID: 1002, GroupIndex: 0},
		}},
		Rewards: raid.RaidSettlementRewards{
			ResultType: runtimeRaidRewardResultType,
			Members: []raid.RaidSettlementMemberRewards{
				{MemberKey: 1001, CardIndex: 2, Rewards: []raid.RaidSettlementReward{{ItemIDOrValue: 150001, CountOrAux: 5}}},
				{MemberKey: 1002, CardIndex: 0, Rewards: []raid.RaidSettlementReward{{ItemIDOrValue: 150001, CountOrAux: 5}}},
			},
		},
	}
	revealBody, err := raid.BuildRaidSettlementRewardsBody(delivery.Rewards)
	if err != nil {
		t.Fatalf("build reveal body: %v", err)
	}
	bodies, err := runtimeRaidRewardSquadBodies(delivery, revealBody)
	if err != nil {
		t.Fatalf("split reveal body: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("the split produced %d boards, want one per squad including standby", len(bodies))
	}
	wantSquadOne, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: runtimeRaidRewardResultType,
		Members:    []raid.RaidSettlementMemberRewards{delivery.Rewards.Members[0]},
	})
	if err != nil {
		t.Fatalf("build squad one board: %v", err)
	}
	wantStandby, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: runtimeRaidRewardResultType,
		Members:    []raid.RaidSettlementMemberRewards{delivery.Rewards.Members[1]},
	})
	if err != nil {
		t.Fatalf("build standby board: %v", err)
	}
	if !bytes.Equal(bodies[runtimeRaidDefaultGroup], wantSquadOne) {
		t.Fatalf("squad 1 board=% X want % X", bodies[runtimeRaidDefaultGroup], wantSquadOne)
	}
	if !bytes.Equal(bodies[0], wantStandby) {
		t.Fatalf("standby board=% X want % X", bodies[0], wantStandby)
	}
}

// The flip pays only the members the phase registered a clear for: a member whose
// 地下城通关 cell reads 0/1 cleared no gate, so it is not in the settlement's roster,
// its click grants nothing, it is not waited for, and no board carries its row. 86JP
// builds the reward flow's eligible set from `HasClearedDungeon`
// (RaidHandler.Progression.cs:477-480) and every card operation checks it
// (Rewards.cs:66-69, :132-135, :399).
func TestRuntimeRaidSettlementSkipsMembersWithNoClearThisPhase(t *testing.T) {
	service, raidKey := newRuntimeRaidDungeonLifecycleTestService(19)
	service.raids[raidKey].Members = []runtimeRaidMemberState{
		{CharID: 19, Name: "cleared", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "never-entered", GroupIndex: 1, SlotOrder: 1},
	}
	service.raids[raidKey].FrozenMembers = append([]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	state := service.raids[raidKey]
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	// Only 19 has a clear: 20 is the 0/1 row in the attack-party window.
	seedRuntimeRaidPhaseClearsForTest(&state.Lifecycle, 19)

	eligible := service.runtimeRaidRewardEligibleMembersLocked(state)
	if len(eligible) != 1 || eligible[0].CharID != 19 {
		t.Fatalf("eligible=%+v want only the member with a clear", eligible)
	}

	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	if _, confirmed, err := service.selectRuntimeRaidSettlementCard(
		20,
		runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 1, CardIndex: 0},
		time.Now().UTC(),
	); err != nil || confirmed {
		t.Fatalf("a member with no clear was allowed to flip: confirmed=%t err=%v", confirmed, err)
	}
	if _, picked := state.Lifecycle.RewardSelections[20]; picked {
		t.Fatalf("a member with no clear got a card recorded: %+v", state.Lifecycle.RewardSelections)
	}
	// The roster it is not part of does not wait for it either: 19 confirming closes
	// the flip even though 20 never picked.
	if _, confirmed, err := service.selectRuntimeRaidSettlementCard(
		19,
		runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 1, CardIndex: 2},
		time.Now().UTC(),
	); err != nil || confirmed {
		t.Fatalf("preview: confirmed=%t err=%v", confirmed, err)
	}
	if _, confirmed, err := service.selectRuntimeRaidSettlementCard(
		19,
		runtimeRaidDungeonPhaseOne,
		raid.RaidRewardCardRequest{Mode: 0, CardIndex: 2},
		time.Now().UTC(),
	); err != nil || !confirmed {
		t.Fatalf("the member with no clear still held the roster open: confirmed=%t err=%v", confirmed, err)
	}
}

// A squad fights one raid gate at a time, so the second member to walk in keeps
// the binding the first one opened and its run is never registered -- but it does
// walk its own instance of the gate (each session moves into the same gate with a
// different seed) and reports its own completion. That completion is the SAME party
// clear: the gate loses one point for the run, not one per member.
//
// Live 2026-09-17T12:45-12:46Z, raid 1507329, is what pins this. Characters 1 and 22
// cleared the heart together twice (12:45:47, 12:45:54) and character 1 cleared it
// three more times alone -- five party runs -- and the gate booked five. That was
// read at the time as "the second member's clears were dropped" and this path was
// changed to spend a point per member, which is what let a two-member squad take
// 黑雾之源 down in two runs and a four-member squad take it down in one (operator
// report 2026-09-19). Five booked runs was the party count all along: 86JP keeps one
// RaidDungeonParticipation per (dungeon, party) and one scalar clear count per
// (raid, dungeon), and TryClearDungeon increments every member's own counter from
// that single party clear.
func TestRuntimeRaidSquadmateClearSharesTheRunsSingleGatePoint(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	service.raids[raidKey].Members = []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "squadmate", GroupIndex: 1, SlotOrder: 1},
	}
	service.raids[raidKey].FrozenMembers = append(
		[]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	service.raids[raidKey].Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		antonHeartMapID: {
			Phase:       runtimeRaidDungeonPhaseTwo,
			MapID:       antonHeartMapID,
			State:       raid.AntonDungeonOpen,
			MaxHP:       5,
			RemainingHP: 5,
			PartyMax:    5,
		},
	}
	service.raids[raidKey].Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	service.raids[raidKey].Lifecycle.Status = runtimeRaidLifecyclePhaseActive

	enter := func(characterID uint16, token uint64) (*gameSession, *runtimeDungeonState) {
		runtime := &runtimeDungeonState{
			Request:        dungeoncmd.SelectDungeonRequest{DungeonID: antonHeartMapID},
			Character:      dnfrepo.CharacterRecord{CharacterID: strconv.Itoa(int(characterID))},
			lifecycleToken: token,
		}
		session := &gameSession{
			selectedCharacterID: characterID,
			dungeon:             dungeonSessionState{runtime: runtime},
		}
		if err := service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
			t.Fatalf("char %d entry: %v", characterID, err)
		}
		if err := service.notifyRuntimeRaidDungeonLoaded(session); err != nil {
			t.Fatalf("char %d loaded: %v", characterID, err)
		}
		return session, runtime
	}
	complete := func(session *gameSession, runtime *runtimeDungeonState) {
		t.Helper()
		session.dungeon.mu.Lock()
		defer session.dungeon.mu.Unlock()
		if err := service.notifyRuntimeRaidDungeonCompletedLocked(session, runtime); err != nil {
			t.Fatalf("char %d completed: %v", session.selectedCharacterID, err)
		}
	}

	leader, leaderRuntime := enter(1001, 11)
	squadmate, squadmateRuntime := enter(1002, 22)
	if leaderRuntime.raidDungeon.LifecycleToken != 11 {
		t.Fatalf("the opener's binding=%+v", leaderRuntime.raidDungeon)
	}
	if squadmateRuntime.raidDungeon.LifecycleToken != leaderRuntime.raidDungeon.LifecycleToken {
		t.Fatalf("the squadmate opened its own run: %+v", squadmateRuntime.raidDungeon)
	}

	complete(leader, leaderRuntime)
	service.raidMu.Lock()
	afterLeader := service.raids[raidKey].Lifecycle.Objectives[antonHeartMapID]
	service.raidMu.Unlock()
	if afterLeader.RemainingHP != 4 || afterLeader.ClearCount != 1 {
		t.Fatalf("objective after the owner's clear=%+v", afterLeader)
	}

	complete(squadmate, squadmateRuntime)
	service.raidMu.Lock()
	afterBoth := service.raids[raidKey].Lifecycle.Objectives[antonHeartMapID]
	counts := service.raids[raidKey].Lifecycle.PhaseClearCounts
	runs := len(service.raids[raidKey].Lifecycle.Dungeons)
	_, booked := service.raids[raidKey].Lifecycle.ClosedDungeonAttempts[runtimeRaidDungeonAttemptKey{
		CharacterID: 1002, LifecycleKey: 22,
	}]
	service.raidMu.Unlock()
	if afterBoth.RemainingHP != 4 || afterBoth.ClearCount != 1 {
		t.Fatalf("objective after the squadmate's clear=%+v want the run's single point", afterBoth)
	}
	if counts[1001] < 1 || counts[1002] < 1 {
		t.Fatalf("member clear counts=%+v want the clearing squad registered", counts)
	}
	if runs != 0 {
		t.Fatalf("the squadmate's clear left %d run records", runs)
	}
	if !booked {
		t.Fatal("the squadmate's own run was not recorded as a closed attempt")
	}

	// A replayed report of the same joined run earns nothing: the attempt is
	// already closed under the squadmate's own lifecycle token.
	complete(squadmate, squadmateRuntime)
	service.raidMu.Lock()
	afterReplay := service.raids[raidKey].Lifecycle.Objectives[antonHeartMapID]
	service.raidMu.Unlock()
	if afterReplay.RemainingHP != 4 || afterReplay.ClearCount != 1 {
		t.Fatalf("the replay spent another point: %+v", afterReplay)
	}
}
