package worldmap

import (
	"errors"
	"testing"
)

func TestParseTournamentPVFShape(t *testing.T) {
	dungeonDoc := parseTestDocument(t, "dungeon/tournament.dgn", `
[basis level]
72
[maze info]
[size]
1 1
[greed]
`+"`BB`"+`
[start map]
0 0
[boss map]
0 0
[tournament dungeon]
1
[required item]
4726 1 1
[/required item]
[tournament round fatigue]
2
[result card]
[reward item rate]
3329 60 1 3329 35 2 3329 5 3
[/reward item rate]
0 0 0 100 1 70 0 30 2 80 0 20 3 90 0 10 4 100 0 0 5 0 100 0
[/result card]
[tournament clear reward exp]
0 0 1 30000 2 50000 3 100000 4 200000
[/tournament clear reward exp]
[limit party count]
1
[tournament clear reward gold rate]
16
`)
	dungeon := ParseDungeon(120, "dungeon/tournament.dgn", dungeonDoc)
	if !dungeon.Metadata.Flags["tournament dungeon"] || dungeon.Metadata.TournamentRoundFatigue != 2 || dungeon.Metadata.TournamentGoldRate != 16 {
		t.Fatalf("tournament scalar metadata=%+v", dungeon.Metadata)
	}
	if len(dungeon.Metadata.RequiredItems) != 1 || dungeon.Metadata.RequiredItems[0] != (TicketItem{ItemID: 4726, Count: 1}) {
		t.Fatalf("required items=%+v", dungeon.Metadata.RequiredItems)
	}
	if len(dungeon.Metadata.TournamentRewardItemRates) != 3 || dungeon.Metadata.TournamentRewardItemRates[2].Count != 3 {
		t.Fatalf("reward item rates=%+v", dungeon.Metadata.TournamentRewardItemRates)
	}
	if len(dungeon.Metadata.TournamentResultCards) != 6 || dungeon.Metadata.TournamentResultCards[5].ItemWeight != 100 {
		t.Fatalf("result cards=%+v", dungeon.Metadata.TournamentResultCards)
	}
	if len(dungeon.Metadata.TournamentClearRewardExp) != 5 || dungeon.Metadata.TournamentClearRewardExp[4].Experience != 200000 {
		t.Fatalf("clear reward experience=%+v", dungeon.Metadata.TournamentClearRewardExp)
	}

	mapDoc := parseTestDocument(t, "map/tournament.map", `
[dungeon]
120
[/dungeon]
[tournament start area]
1 360 270 1
[tournament start area]
1 760 270 2
[tournament enemies]
1 `+"`[monster]`"+` 56048 900 `+"`A`"+` 56047 300 `+"`B`"+`
[/tournament enemies]
`)
	parsedMap := ParseMap(24001, "map/tournament.map", mapDoc)
	if parsedMap.TournamentMalformed || len(parsedMap.TournamentStartAreas) != 2 || len(parsedMap.TournamentEnemies) != 2 {
		t.Fatalf("tournament map=%+v", parsedMap)
	}
	if parsedMap.TournamentEnemies[0].Kind != "[monster]" || parsedMap.TournamentEnemies[1].Strength != 300 {
		t.Fatalf("tournament enemies=%+v", parsedMap.TournamentEnemies)
	}
}

func TestTournamentRuntimeBuildsBracketAndRejectsStaleDeaths(t *testing.T) {
	definition := tournamentRuntimeTestDefinition()
	runtime, err := NewTournamentRuntime(definition, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Rounds) != 4 || len(runtime.Rounds[0].Teams) != 16 || len(runtime.Rounds[3].Teams) != 2 || len(runtime.PathActors) != 4 {
		t.Fatalf("rounds=%+v path=%+v", runtime.Rounds, runtime.PathActors)
	}
	if !runtime.BindFirstActorSequence(100) || !runtime.BindFirstActorSequence(100) || runtime.BindFirstActorSequence(101) {
		t.Fatal("first actor sequence binding is not idempotent")
	}
	if got := runtime.ApplyActorDeath("future", 101); got.Disposition != TournamentDeathRejected {
		t.Fatalf("future death=%+v", got)
	}
	for round := byte(1); round <= 4; round++ {
		sequence := uint16(99 + round)
		transition := runtime.ApplyActorDeath(string([]byte{'e', '0' + round}), sequence)
		want := TournamentRoundCompleted
		if round == 4 {
			want = TournamentCompleted
		}
		if transition.Disposition != want || transition.CompletedRound != round {
			t.Fatalf("round=%d transition=%+v want=%d", round, transition, want)
		}
		if duplicate := runtime.ApplyActorDeath(string([]byte{'e', '0' + round}), sequence); duplicate.Disposition != TournamentDeathDuplicate {
			t.Fatalf("round=%d duplicate=%+v", round, duplicate)
		}
	}
	current, completed, terminal := runtime.Snapshot()
	if current != 4 || completed != 4 || terminal != TournamentChampion {
		t.Fatalf("terminal snapshot=%d/%d/%d", current, completed, terminal)
	}
}

func TestTournamentRuntimeRejectsInvalidRandomIndex(t *testing.T) {
	_, err := NewTournamentRuntime(tournamentRuntimeTestDefinition(), func(maximum int) (int, error) { return maximum, nil })
	if !errors.Is(err, ErrTournamentRandomInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestTournamentRuntimeEliminationIsTerminalAndIdempotent(t *testing.T) {
	runtime, err := NewTournamentRuntime(tournamentRuntimeTestDefinition(), func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.BindFirstActorSequence(100) {
		t.Fatal("bind sequence")
	}
	if !runtime.Eliminate() || runtime.Eliminate() {
		t.Fatal("elimination was not a one-way transition")
	}
	current, completed, terminal := runtime.Snapshot()
	if current != 1 || completed != 0 || terminal != TournamentEliminated {
		t.Fatalf("eliminated snapshot=%d/%d/%d", current, completed, terminal)
	}
	if transition := runtime.ApplyActorDeath("after-elimination", 100); transition.Disposition != TournamentDeathRejected {
		t.Fatalf("post-elimination death=%+v", transition)
	}
}

func tournamentRuntimeTestDefinition() TournamentDefinition {
	candidates := make([]TournamentActor, 15)
	for index := range candidates {
		candidates[index] = TournamentActor{Code: uint32(56000 + index), Strength: uint16(100 + index), Name: "actor", Level: 72}
	}
	return TournamentDefinition{DungeonID: 120, MapID: 24001, DifficultyLevel: 72, PartyCount: 1, Candidates: candidates}
}

func TestTournamentRewardPlanUsesUpperCompletedRoundsAndChampionLowerKey(t *testing.T) {
	definition := TournamentDefinition{
		PartyCount: 1,
		ResultCards: map[byte]TournamentResultCard{
			0: {ResultKey: 0, EmptyWeight: 1}, 1: {ResultKey: 1, EmptyWeight: 1},
			2: {ResultKey: 2, EmptyWeight: 1}, 3: {ResultKey: 3, EmptyWeight: 1},
			4: {ResultKey: 4, GoldWeight: 1}, 5: {ResultKey: 5, ItemWeight: 1},
		},
		RewardItems: []TournamentRewardItemRate{{ItemID: 3329, Weight: 1, Count: 3}},
	}
	runtime := &TournamentRuntime{Definition: definition, terminal: TournamentChampion, completedRounds: tournamentRoundCount}
	plan, created, err := runtime.PrepareRewardPlan(200000, 777, func(maximum int) (int, error) { return 0, nil })
	if err != nil || !created {
		t.Fatalf("prepare plan created=%t err=%v", created, err)
	}
	for index, reward := range plan.Upper {
		if !reward.Gold || reward.Count != 777 || reward.Empty {
			t.Fatalf("upper[%d]=%+v, want frozen gold", index, reward)
		}
	}
	for index, reward := range plan.Lower {
		if reward.ItemID != 3329 || reward.Count != 3 || reward.Gold || reward.Empty {
			t.Fatalf("lower[%d]=%+v, want item", index, reward)
		}
	}
	second, created, err := runtime.PrepareRewardPlan(1, 2, func(maximum int) (int, error) { return 0, nil })
	if err != nil || created || second.Experience != 200000 || second.Upper[0].Count != 777 {
		t.Fatalf("second prepare created=%t plan=%+v err=%v", created, second, err)
	}
}

func TestTournamentRewardPlanSelectionIsTwoPhaseAndIdempotent(t *testing.T) {
	plan := TournamentRewardPlan{Champion: false, Upper: [2]TournamentReward{{ItemID: 3329, Count: 3}}}
	plan.initSelection()
	reward, reserved, replay := plan.ReserveSelection(0, 0)
	if !reserved || replay || reward.ItemID != 3329 {
		t.Fatalf("reserve=%+v reserved=%t replay=%t", reward, reserved, replay)
	}
	if _, second, _ := plan.ReserveSelection(0, 1); second {
		t.Fatal("second card reservation unexpectedly accepted")
	}
	if !plan.CommitSelection(0, 0) || !plan.IsSelectionComplete() {
		t.Fatal("committed non-champion selection is not complete")
	}
	replayReward, replayReserved, replay := plan.ReserveSelection(0, 0)
	if replayReserved || !replay || replayReward.ItemID != 3329 {
		t.Fatalf("replay=%+v reserved=%t replay=%t", replayReward, replayReserved, replay)
	}
	if _, differentReserved, differentReplay := plan.ReserveSelection(0, 1); differentReserved || differentReplay {
		t.Fatalf("different card was accepted after commit: reserved=%t replay=%t", differentReserved, differentReplay)
	}
}

func TestTournamentRewardPlanSelectedCardOnlyReportsCommittedSelection(t *testing.T) {
	plan := TournamentRewardPlan{Champion: true}
	plan.initSelection()
	if _, ok := plan.SelectedCard(0); ok {
		t.Fatal("unselected card was projected")
	}
	if _, reserved, _ := plan.ReserveSelection(0, 1); !reserved {
		t.Fatal("selection was not reserved")
	}
	if _, ok := plan.SelectedCard(0); ok {
		t.Fatal("uncommitted card was projected")
	}
	if !plan.CommitSelection(0, 1) {
		t.Fatal("selection was not committed")
	}
	if index, ok := plan.SelectedCard(0); !ok || index != 1 {
		t.Fatalf("selected card index=%d ok=%t, want 1/true", index, ok)
	}
}

func TestTournamentCardlessRewardPlanCommitsWithoutSelectionRequest(t *testing.T) {
	runtime := &TournamentRuntime{
		Definition: TournamentDefinition{ResultCards: map[byte]TournamentResultCard{
			4: {ResultKey: 4, EmptyWeight: 1},
			5: {ResultKey: 5, EmptyWeight: 1},
		}},
		terminal:        TournamentChampion,
		completedRounds: tournamentRoundCount,
	}
	plan, _, err := runtime.PrepareRewardPlan(123, 456, func(maximum int) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Cardless() || runtime.RewardSelectionComplete() {
		t.Fatalf("initial cardless plan=%+v complete=%t", plan, runtime.RewardSelectionComplete())
	}
	if !runtime.CommitCardlessRewardPlan() || !runtime.RewardSelectionComplete() {
		t.Fatal("cardless reward plan did not cross the delivery gate")
	}
	if !runtime.CommitCardlessRewardPlan() {
		t.Fatal("cardless reward completion replay was not idempotent")
	}
}
