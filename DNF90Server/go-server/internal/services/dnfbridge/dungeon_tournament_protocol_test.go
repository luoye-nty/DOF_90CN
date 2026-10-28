package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentTournamentPathActorsAreMaterializedInOp29(t *testing.T) {
	candidates := make([]worldmap.TournamentActor, 15)
	for index := range candidates {
		candidates[index] = worldmap.TournamentActor{
			Code: uint32(56000 + index), Strength: uint16(100 + index), Name: "actor", Level: 72,
		}
	}
	tournament, err := worldmap.NewTournamentRuntime(worldmap.TournamentDefinition{
		DungeonID: 120, MapID: 24001, DifficultyLevel: 72, PartyCount: 1, Candidates: candidates,
	}, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	plan := runtimeDungeonExtendedActorPlan{NextObjectKey: 402}
	if !tournament.BindFirstActorSequence(402) {
		t.Fatal("bind sequence")
	}
	if err := appendCurrentTournamentActors(&plan, tournament); err != nil {
		t.Fatal(err)
	}
	if plan.NextObjectKey != 406 || len(plan.Actors) != 4 {
		t.Fatalf("plan next=%d actors=%+v", plan.NextObjectKey, plan.Actors)
	}
	room := &runtimeDungeonRoom{
		coordinate:     worldmap.RoomCoordinate{X: 0, Y: 0},
		mapID:          24001,
		extendedActors: plan.Actors,
		extendedByObjectKey: map[uint32]int{
			402: 0, 403: 1, 404: 2, 405: 3,
		},
	}
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{Metadata: worldmap.DungeonMetadata{BasisLevel: worldmap.OptionalInt{Set: true, Value: 72}}},
		Room:    room,
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate: worldmap.RoomCoordinate{X: 0, Y: 0},
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 24001}},
	}
	packet, err := currentDungeonStartMapFromRuntime(runtime, scene, currentDungeonStartMapState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Actors) != 4 {
		t.Fatalf("op29 actors=%+v", packet.Actors)
	}
	for index, actor := range packet.Actors {
		want := tournament.PathActors[index]
		if actor.ObjectKey != uint16(402+index) || actor.Code != want.Code || actor.Level != want.Level || actor.Type != want.Type {
			t.Fatalf("actor[%d]=%+v want key=%d code=%d level=%d type=%d", index, actor, 402+index, want.Code, want.Level, want.Type)
		}
	}
}

func TestCurrentTournamentInfoBodyUsesCurrentEXEU32DungeonID(t *testing.T) {
	candidates := make([]worldmap.TournamentActor, 15)
	for index := range candidates {
		candidates[index] = worldmap.TournamentActor{Code: uint32(56000 + index), Strength: uint16(300 + index), Name: "actor", Level: 72}
	}
	runtime, err := worldmap.NewTournamentRuntime(worldmap.TournamentDefinition{
		DungeonID: 0x12345678, MapID: 24001, DifficultyLevel: 72, PartyCount: 1, Candidates: candidates,
	}, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.BindFirstActorSequence(400) {
		t.Fatal("bind sequence")
	}
	body, err := buildCurrentTournamentInfoBody(runtime, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 260 || binary.LittleEndian.Uint32(body[:4]) != 0x12345678 || body[4] != 2 || body[5] != 1 {
		t.Fatalf("body header/length=%x len=%d", body[:6], len(body))
	}
	if body[6] != 1 || body[7] != 16 || body[120] != 2 || body[121] != 8 || body[178] != 3 || body[179] != 4 || body[208] != 4 || body[209] != 2 {
		t.Fatalf("round table boundaries are wrong")
	}
	if body[224] != 1 || binary.LittleEndian.Uint16(body[225:227]) != 400 || body[233] != 2 || binary.LittleEndian.Uint16(body[234:236]) != 401 {
		t.Fatalf("path actor table=%x", body[224:])
	}
}

func TestCurrentTournamentMapInfoBodyMatchesCurrentEXEReader(t *testing.T) {
	stateOnly, err := (currentTournamentMapInfo{RoomX: 1, RoomY: 2, Seed: 0x44332211, RoomState: 3}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(stateOnly) != 8 || binary.LittleEndian.Uint32(stateOnly[2:6]) != 0x44332211 || stateOnly[6] != 3 || stateOnly[7] != 0 {
		t.Fatalf("state-only body=%x", stateOnly)
	}
	body, err := (currentTournamentMapInfo{
		RoomX: 4, RoomY: 5, Seed: 6, RoomState: 7, Operation: 1, MapID: 0x12345678,
		Actors:    []currentTournamentMapActor{{Sequence: 8, Code: 9, FieldA: 10, FieldB: 11, FieldC: 12, FieldD: 13, FieldE: 14, FieldF: 15}},
		Extras:    []currentTournamentMapExtra{{Type: 16, ValueA: 17, ValueB: 18, ValueC: 19, ValueD: 20, ValueE: 21, ValueF: 22, ValueG: 23}},
		FinalFlag: 24,
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 49 || binary.LittleEndian.Uint32(body[8:12]) != 0x12345678 || body[12] != 1 || binary.LittleEndian.Uint16(body[13:15]) != 8 || body[27] != 1 || body[48] != 24 {
		t.Fatalf("full body=%x len=%d", body, len(body))
	}
}

func TestCurrentTournamentClearRewardBodyMatchesCurrentEXEReader(t *testing.T) {
	body, err := (currentTournamentClearReward{
		CompletedRounds: 4, Experience: 200000,
		GroupA: []currentTournamentClearRewardEntry{{ItemOrSentinel: 3329, CountOrGold: 3}},
		GroupB: []currentTournamentClearRewardEntry{{ItemOrSentinel: 0, CountOrGold: 5000}},
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 28 || body[0] != 4 || binary.LittleEndian.Uint32(body[1:5]) != 200000 || body[5] != 0 || body[6] != 1 || binary.LittleEndian.Uint32(body[7:11]) != 3329 || body[17] != 1 || binary.LittleEndian.Uint32(body[22:26]) != 5000 {
		t.Fatalf("reward body=%x len=%d", body, len(body))
	}
}

func TestCurrentTournamentRewardEntryUsesCurrentEXEEmptySentinel(t *testing.T) {
	empty := currentTournamentRewardEntry(worldmap.TournamentReward{Empty: true})
	if empty.ItemOrSentinel != currentTournamentEmptyRewardSentinel || empty.CountOrGold != 0 {
		t.Fatalf("empty reward entry=%+v", empty)
	}
	gold := currentTournamentRewardEntry(worldmap.TournamentReward{Gold: true, Count: 5000})
	if gold.ItemOrSentinel != 0 || gold.CountOrGold != 5000 {
		t.Fatalf("gold reward entry=%+v", gold)
	}
	item := currentTournamentRewardEntry(worldmap.TournamentReward{ItemID: 3329, Count: 3})
	if item.ItemOrSentinel != 3329 || item.CountOrGold != 3 {
		t.Fatalf("item reward entry=%+v", item)
	}
}

func TestCurrentTournamentCardlessClearUsesFallbackExperienceAndEntersSettlement(t *testing.T) {
	source := bridgeDungeonPVF(false)
	installCurrentDungeonMonsterExperienceTestPVF(source)
	source[dungeonCardGoldReferencePath] = "[gold drop ref table]\n5 100 0\n"
	service, runtime := prepareSyntheticMoveRuntimeFromPVF(t, source)
	currentDungeonMonsterExperienceResourcesByCatalog.Delete(service.dungeonMonsterTable)
	currentDungeonGoldReferencesByCatalog.Delete(service.dungeonMonsterTable)
	runtime.Dungeon.Metadata.BasisLevel = worldmap.OptionalInt{Set: true, Value: 5}
	runtime.Dungeon.Metadata.TournamentGoldRate = 1
	resources, err := currentDungeonMonsterExperienceResourcesForCatalog(service.dungeonMonsterTable)
	if err != nil {
		t.Fatalf("load fallback experience resources: %v", err)
	}
	wantExperience, err := currentDungeonClearExperience(runtime, currentDungeonSettlementRewardResources{
		Progression:       resources.Progression,
		ExperienceSources: resources.Sources,
	})
	if err != nil || wantExperience == 0 {
		t.Fatalf("calculate fallback experience=%d: %v", wantExperience, err)
	}

	candidates := make([]worldmap.TournamentActor, 15)
	for index := range candidates {
		candidates[index] = worldmap.TournamentActor{
			Code: uint32(56000 + index), Name: "actor", Level: 20,
		}
	}
	tournament, err := worldmap.NewTournamentRuntime(worldmap.TournamentDefinition{
		DungeonID:  700,
		MapID:      100,
		PartyCount: 1,
		Candidates: candidates,
		Experience: map[byte]uint32{4: 0},
		ResultCards: map[byte]worldmap.TournamentResultCard{
			4: {ResultKey: 4, EmptyWeight: 1},
			5: {ResultKey: 5, EmptyWeight: 1},
		},
	}, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !tournament.BindFirstActorSequence(400) {
		t.Fatal("bind tournament sequence")
	}
	for round := byte(0); round < 4; round++ {
		transition := tournament.ApplyActorDeath(
			string([]byte{'r', '0' + round}),
			uint16(400)+uint16(round),
		)
		if round == 3 && transition.Disposition != worldmap.TournamentCompleted {
			t.Fatalf("final transition=%+v", transition)
		}
	}
	runtime.tournament = tournament

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "tournament-cardless-test",
		selectedCharacterID: 99,
	}
	session.dungeon.runtime = runtime

	if err := service.sendCurrentTournamentClearRewardLocked(session, runtime); err != nil {
		t.Fatal(err)
	}
	reward, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if reward.Header.MsgID != currentTournamentClearRewardNotification || reward.Header.Classification != 0 ||
		len(reward.Body) != 8 || reward.Body[0] != 4 || reward.Body[5] != 0 || reward.Body[6] != 0 || reward.Body[7] != 0 {
		t.Fatalf("cardless op388 header=%+v body=%x", reward.Header, reward.Body)
	}
	experience := binary.LittleEndian.Uint32(reward.Body[1:5])
	if experience != wantExperience || experience != runtime.tournamentRewardExperience {
		t.Fatalf("cardless op388 experience=%d want=%d runtime=%d", experience, wantExperience, runtime.tournamentRewardExperience)
	}
	entry, rest := splitGameServerUpperPacket(t, rest)
	if entry.Header.MsgID != currentDungeonSettlementEntryMsgID || entry.Header.Classification != 0 ||
		len(entry.Body) != 1 || entry.Body[0] != 0 || len(rest) != 0 {
		t.Fatalf("settlement entry=%+v body=%x trailing=%x", entry.Header, entry.Body, rest)
	}
	if !runtime.tournament.RewardSelectionComplete() || !runtime.settlementEntrySent ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		t.Fatalf("cardless settlement status=%s reward_complete=%t entry_sent=%t",
			runtime.Session.Snapshot().Run.Status,
			runtime.tournament.RewardSelectionComplete(),
			runtime.settlementEntrySent,
		)
	}
}

func TestCurrentTournamentRewardSelectRequestRequiresExactlyTwoBytes(t *testing.T) {
	request, err := decodeCurrentTournamentRewardSelectRequest([]byte{1, 0})
	if err != nil || request.Side != 1 || request.CardIndex != 0 {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	for _, body := range [][]byte{nil, []byte{0}, []byte{0, 1, 2}} {
		if _, err := decodeCurrentTournamentRewardSelectRequest(body); err == nil {
			t.Fatalf("body length %d was accepted", len(body))
		}
	}
}

func TestCurrentTournamentRewardSelectionStateMatchesFrozenCardGroups(t *testing.T) {
	plan := worldmap.TournamentRewardPlan{Champion: true}
	body, err := (currentTournamentRewardSelectionState{Plan: plan}).build()
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 2, 0xff, 0xff, 2, 0xff, 0xff}; !bytes.Equal(body, want) {
		t.Fatalf("unselected state=%x want=%x", body, want)
	}
}

func TestCurrentTournamentRewardBundleHasNoItemsForGoldAndEmpty(t *testing.T) {
	service := &Service{}
	for _, reward := range []worldmap.TournamentReward{
		{Gold: true, Count: 1234},
		{Empty: true},
	} {
		bundle, err := service.currentTournamentRewardBundle(reward)
		if err != nil {
			t.Fatalf("reward=%+v err=%v", reward, err)
		}
		if len(bundle.Items) != 0 {
			t.Fatalf("reward=%+v items=%+v want none", reward, bundle.Items)
		}
	}
}

func TestCurrentTournamentMissingSecondEmptySelectionIsAutoSelectedIntoSettlement(t *testing.T) {
	service, runtime := prepareSyntheticMoveRuntime(t, false)
	candidates := make([]worldmap.TournamentActor, 15)
	for index := range candidates {
		candidates[index] = worldmap.TournamentActor{
			Code: uint32(56000 + index), Name: "actor", Level: 72,
		}
	}
	tournament, err := worldmap.NewTournamentRuntime(worldmap.TournamentDefinition{
		DungeonID:  123,
		MapID:      95265,
		PartyCount: 1,
		Candidates: candidates,
		ResultCards: map[byte]worldmap.TournamentResultCard{
			4: {EmptyWeight: 1},
			5: {EmptyWeight: 1},
		},
	}, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !tournament.BindFirstActorSequence(400) {
		t.Fatal("bind tournament sequence")
	}
	for round := 0; round < 4; round++ {
		transition := tournament.ApplyActorDeath(
			string([]byte{'r', byte('0' + round)}),
			uint16(400+round),
		)
		if round == 3 && transition.Disposition != worldmap.TournamentCompleted {
			t.Fatalf("final transition=%+v", transition)
		}
	}
	if _, _, err := tournament.PrepareRewardPlan(0, 0, func(maximum int) (int, error) { return 0, nil }); err != nil {
		t.Fatal(err)
	}
	runtime.tournament = tournament
	runtime.tournamentClearRewardSent = true
	runtime.lifecycleToken = 77
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "tournament-test", selectedCharacterID: 99, characterGeneration: 1}
	session.dungeon.runtime = runtime
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue

	if err := service.handleTournamentRewardSelectState(session, nil); err != nil {
		t.Fatal(err)
	}
	state, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if state.Header.MsgID != currentTournamentRewardSelectState || state.Header.Classification != 1 ||
		!bytes.Equal(state.Body, currentTournamentRewardSelectOpenBody) || len(trailing) != 0 {
		t.Fatalf("reward state packet=%+v body=%x trailing=%x", state.Header, state.Body, trailing)
	}
	task := queue.task(t, 0)
	if task.delay != currentTournamentRewardAutoSelectDelay || runtime.tournamentRewardAutoSelectTimerName == "" {
		t.Fatalf("auto-select timer delay=%s name=%q", task.delay, runtime.tournamentRewardAutoSelectTimerName)
	}
	connection.write.Reset()

	if err := service.handleTournamentRewardSelect(session, []byte{0, 1}); err != nil {
		t.Fatal(err)
	}
	first, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != currentTournamentRewardSelect || first.Header.Classification != 1 ||
		!bytes.Equal(first.Body, []byte{1, 2, 0xff, 0, 2, 0xff, 0xff}) || len(trailing) != 0 {
		t.Fatalf("first empty selection packet=%+v body=%x trailing=%x", first.Header, first.Body, trailing)
	}
	if runtime.settlementEntrySent || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive {
		t.Fatalf("first side advanced settlement: status=%s sent=%t", runtime.Session.Snapshot().Run.Status, runtime.settlementEntrySent)
	}
	if !runtime.tournament.RewardSideSelectionComplete(0) || runtime.tournament.RewardSideSelectionComplete(1) {
		t.Fatalf("selection state did not preserve the client-only first side")
	}

	connection.write.Reset()
	queue.fire(task, false)
	second, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	entry, trailing := splitGameServerUpperPacket(t, trailing)
	if second.Header.MsgID != currentTournamentRewardSelect || second.Header.Classification != 1 ||
		!bytes.Equal(second.Body, []byte{1, 2, 0xff, 0, 2, 0, 0xff}) {
		t.Fatalf("second empty selection packet=%+v body=%x", second.Header, second.Body)
	}
	if entry.Header.MsgID != currentDungeonSettlementEntryMsgID || entry.Header.Classification != 0 ||
		len(entry.Body) != 1 || entry.Body[0] != 0 || len(trailing) != 0 {
		t.Fatalf("settlement entry=%+v body=%x trailing=%x", entry.Header, entry.Body, trailing)
	}
	if !runtime.settlementEntrySent || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		t.Fatalf("completed tournament status=%s sent=%t", runtime.Session.Snapshot().Run.Status, runtime.settlementEntrySent)
	}
}
