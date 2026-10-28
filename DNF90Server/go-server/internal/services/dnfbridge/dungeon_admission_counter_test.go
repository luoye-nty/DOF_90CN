package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestDungeonAdmissionCounterSoloRejectsAfterPVFLimit(t *testing.T) {
	service, _, _ := newDungeonAdmissionCounterBridgeTest(t, 11)
	session := &gameSession{selectedCharacterID: 11}
	for index := 0; index < 2; index++ {
		runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
		if batch, err := service.reserveCurrentDungeonAdmissionCounters(context.Background(), session, runtime, nil); err != nil || batch == nil {
			t.Fatalf("admission %d batch=%+v err=%v", index+1, batch, err)
		}
	}
	rejectedRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	batch, err := service.reserveCurrentDungeonAdmissionCounters(context.Background(), session, rejectedRuntime, nil)
	if !errors.Is(err, dnfdungeonadmission.ErrLimitReached) || batch != nil {
		t.Fatalf("over-limit batch=%+v err=%v", batch, err)
	}
	if reason := currentDungeonAdmissionCounterFailureReason(err); reason != currentSelectDungeonDailyEntryLimitReachedReason {
		t.Fatalf("reserved over-limit reason=%#x want %#x", reason, currentSelectDungeonDailyEntryLimitReachedReason)
	}
	if rejectedRuntime.lifecycleToken != 0 {
		t.Fatalf("over-limit runtime token=%d", rejectedRuntime.lifecycleToken)
	}
}

func TestDungeonAdmissionCounterSkipsTicketDrivenTowers(t *testing.T) {
	service := &Service{}
	session := &gameSession{selectedCharacterID: 11}
	for _, testCase := range []struct {
		name    string
		dungeon worldmap.Dungeon
	}{
		{
			name: "despair",
			dungeon: worldmap.Dungeon{
				ID: 11008,
				Metadata: worldmap.DungeonMetadata{
					Flags: map[string]bool{"tower of despair": true},
				},
			},
		},
		{
			name: "grief",
			dungeon: worldmap.Dungeon{
				ID: 5115,
				Metadata: worldmap.DungeonMetadata{
					Flags: map[string]bool{"tower of grief": true},
				},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime := &runtimeDungeonState{Dungeon: testCase.dungeon}
			batch, err := service.reserveCurrentDungeonAdmissionCounters(
				context.Background(), session, runtime, nil)
			if err != nil || batch != nil {
				t.Fatalf("ticket-driven tower reserved a server admission counter: batch=%+v err=%v", batch, err)
			}
		})
	}
}

func TestDungeonAdmissionCounterLimitUsesNativeDailyEntryPrompt(t *testing.T) {
	limitErr := fmt.Errorf("preflight wrapped: %w", &dnfdungeonadmission.LimitError{
		Snapshot: dnfdungeonadmission.Snapshot{Policy: dnfdungeonadmission.Policy{
			Key: "tower_of_grief.account.daily",
		}},
	})
	if reason := currentDungeonAdmissionCounterFailureReason(limitErr); reason != 0xF6 {
		t.Fatalf("limit reason=%#x want native daily-entry-limit marker 0xF6", reason)
	}
	if body := buildSelectDungeonFailureBody(currentDungeonAdmissionCounterFailureReason(limitErr)); !bytes.Equal(body, []byte{0, 0xF6, 0}) {
		t.Fatalf("limit body=%x want 00f600", body)
	}
	if reason := currentDungeonAdmissionCounterFailureReason(errors.New("repository unavailable")); reason != currentSelectDungeonPreparationRejectedReason {
		t.Fatalf("generic reason=%#x want %#x", reason, currentSelectDungeonPreparationRejectedReason)
	}
}

func TestDungeonAdmissionCounterSoloMaterialFailureRestoresReservation(t *testing.T) {
	service, repositories, catalog := newDungeonAdmissionCounterBridgeTest(t, 11)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "11",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	session := &gameSession{selectedCharacterID: 11}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}},
	}}}
	counters, err := service.reserveCurrentDungeonAdmissionCounters(context.Background(), session, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.consumeCurrentDungeonEntryPartyItems(context.Background(), session, runtime, nil); !errors.Is(err, errDungeonEntryItemMissing) {
		t.Fatalf("material failure=%v", err)
	}
	if err := service.restoreCurrentDungeonAdmissionCounterBatch(counters); err != nil {
		t.Fatal(err)
	}
	assertDungeonAdmissionBridgeCounterUsed(t, repositories, catalog, "11", 5006, counters.reservedAt, 0)
}

func TestDungeonAdmissionCounterPartyFollowerMaterialFailureRestoresEveryone(t *testing.T) {
	service, repositories, catalog := newDungeonAdmissionCounterBridgeTest(t, 11, 12)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "11",
		Slots: map[string]dnfrepo.ItemStack{
			"0:180": {ItemID: 4726, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "12", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	newRuntime := func() *runtimeDungeonState {
		return &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006, Metadata: worldmap.DungeonMetadata{
			RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}},
		}}}
	}
	leader := &gameSession{selectedCharacterID: 11}
	follower := &gameSession{selectedCharacterID: 12}
	leaderRuntime, followerRuntime := newRuntime(), newRuntime()
	prepared := []*runtimePartyPreparedDungeonEntry{{session: follower, runtime: followerRuntime}}
	counters, err := service.reserveCurrentDungeonAdmissionCounters(context.Background(), leader, leaderRuntime, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.consumeCurrentDungeonEntryPartyItems(context.Background(), leader, leaderRuntime, prepared); !errors.Is(err, errDungeonEntryItemMissing) {
		t.Fatalf("follower material failure=%v", err)
	}
	if err := service.restoreCurrentDungeonAdmissionCounterBatch(counters); err != nil {
		t.Fatal(err)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "11")
	if err != nil || !found || inventory.Slots["0:180"].Count != 1 {
		t.Fatalf("leader material not restored found=%t inventory=%+v err=%v", found, inventory, err)
	}
	for _, characterID := range []string{"11", "12"} {
		assertDungeonAdmissionBridgeCounterUsed(t, repositories, catalog, characterID, 5006, counters.reservedAt, 0)
	}
}

func TestDungeonAdmissionCounterActiveOp16ReplayDoesNotReserveAgain(t *testing.T) {
	service, session, active := newBackToVillageRuntime(t)
	source := dungeonAdmissionBridgeCatalogSource()
	source[dnfdungeonadmission.LicenseDungeonInfoPath] = "[dungeon license info]\n1 700 1 10 2\n[/dungeon license info]\n[daily enter count]\n2\n"
	catalog, err := dnfdungeonadmission.LoadCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service.dungeonAdmissionCatalog = catalog
	body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(body[:4], uint32(active.Request.DungeonID))
	body[4] = active.Request.Difficulty
	if err := service.handleDungeonSelectUpperPlanned(session, body, nil, true, false); err != nil {
		t.Fatal(err)
	}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository unavailable")
	}
	assertDungeonAdmissionBridgeCounterUsed(t, repositories, catalog, "99", 700, time.Now().UTC(), 0)
}

func TestDungeonAdmissionCounterRestartNoncePreventsDurableReceiptCollision(t *testing.T) {
	serviceOne, repositories, catalog := newDungeonAdmissionCounterBridgeTest(t, 11)
	serviceTwo := &Service{
		dungeonAdmissionCatalog: catalog,
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	firstRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	secondRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	first, err := serviceOne.reserveCurrentDungeonAdmissionCounters(
		context.Background(), &gameSession{selectedCharacterID: 11}, firstRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := serviceTwo.reserveCurrentDungeonAdmissionCounters(
		context.Background(), &gameSession{selectedCharacterID: 11}, secondRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.commits) != 1 || len(second.commits) != 1 || first.commits[0].receipt == second.commits[0].receipt {
		t.Fatalf("restart receipts first=%+v second=%+v", first.commits, second.commits)
	}
	if !strings.Contains(first.commits[0].receipt, ":attempt:1:") ||
		!strings.Contains(second.commits[0].receipt, ":attempt:1:") {
		t.Fatalf("services did not independently restart attempt sequence: first=%q second=%q",
			first.commits[0].receipt, second.commits[0].receipt)
	}
	if firstRuntime.lifecycleToken == 0 || secondRuntime.lifecycleToken == 0 {
		t.Fatalf("runtime tokens first=%d second=%d", firstRuntime.lifecycleToken, secondRuntime.lifecycleToken)
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	policies := catalog.PoliciesForDungeon(5006)
	if _, err := owner.Commit(context.Background(), "11", policies, "third-real-admission", first.reservedAt); !errors.Is(err, dnfdungeonadmission.ErrLimitReached) {
		t.Fatalf("two restart-spanning admissions were not both counted: %v", err)
	}
}

func TestDungeonAdmissionCounterPartyPreflightsEveryoneBeforeAnyReservation(t *testing.T) {
	service, repositories, catalog := newDungeonAdmissionCounterBridgeTest(t, 11, 12)
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	policies := catalog.PoliciesForDungeon(5006)
	for index := 0; index < 2; index++ {
		if _, err := owner.Commit(context.Background(), "12", policies, fmt.Sprintf("follower-used-%d", index), serviceTimeForDungeonAdmissionTest()); err != nil {
			t.Fatal(err)
		}
	}
	leader := &gameSession{selectedCharacterID: 11}
	follower := &gameSession{selectedCharacterID: 12}
	leaderRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	followerRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	_, err = service.reserveCurrentDungeonAdmissionCounters(context.Background(), leader, leaderRuntime, []*runtimePartyPreparedDungeonEntry{{
		session: follower,
		runtime: followerRuntime,
	}})
	if !errors.Is(err, dnfdungeonadmission.ErrLimitReached) {
		t.Fatalf("party preflight err=%v want limit", err)
	}
	leaderState, err := owner.Preflight(context.Background(), "11", policies, serviceTimeForDungeonAdmissionTest())
	if err != nil || len(leaderState) != 1 || leaderState[0].Used != 0 {
		t.Fatalf("leader was reserved before follower preflight state=%+v err=%v", leaderState, err)
	}
	if leaderRuntime.lifecycleToken != 0 || followerRuntime.lifecycleToken != 0 {
		t.Fatalf("failed preflight allocated runtime tokens leader=%d follower=%d", leaderRuntime.lifecycleToken, followerRuntime.lifecycleToken)
	}
}

func TestDungeonAdmissionCounterPartyRuntimeFailureRestoresRuntimeInventoryAndCountersInReverseOrder(t *testing.T) {
	service, repositories, catalog := newDungeonAdmissionCounterBridgeTest(t, 11, 12)
	leader := &gameSession{selectedCharacterID: 11}
	follower := &gameSession{selectedCharacterID: 12}
	leaderRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	followerRuntime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}}
	prepared := []*runtimePartyPreparedDungeonEntry{{session: follower, runtime: followerRuntime}}
	counters, err := service.reserveCurrentDungeonAdmissionCounters(
		context.Background(), leader, leaderRuntime, prepared)
	if err != nil {
		t.Fatal(err)
	}
	// Model the already-committed follower boundary. The deliberately incomplete
	// leader runtime then fails commit, exercising the real compensation helper.
	follower.dungeon.runtime = followerRuntime
	follower.dungeon.runToken = followerRuntime.lifecycleToken
	prepared[0].committed = true
	inventoryRestored := false
	inventory := &currentDungeonEntryInventoryBatch{commits: []*currentDungeonEntryInventoryCommit{{
		session: leader,
		restore: func() error {
			if prepared[0].committed || follower.dungeon.runtime != nil {
				return errors.New("inventory restored before follower runtime rollback")
			}
			inventoryRestored = true
			return nil
		},
	}}}
	if err := service.commitDungeonRuntimeWithPartyAdmission(
		leader, leaderRuntime, prepared, inventory, counters); err == nil {
		t.Fatal("incomplete leader runtime unexpectedly committed")
	}
	if !inventoryRestored || !inventory.commits[0].restored || prepared[0].committed || follower.dungeon.runtime != nil {
		t.Fatalf("rollback runtime=%p committed=%t inventory_restored=%t marker=%t",
			follower.dungeon.runtime, prepared[0].committed, inventoryRestored, inventory.commits[0].restored)
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	policies := catalog.PoliciesForDungeon(5006)
	for _, characterID := range []string{"11", "12"} {
		state, preflightErr := owner.Preflight(context.Background(), characterID, policies, counters.reservedAt)
		if preflightErr != nil || len(state) != 1 || state[0].Used != 0 {
			t.Fatalf("character=%s counter not restored state=%+v err=%v", characterID, state, preflightErr)
		}
	}
}

func TestDungeonAdmissionCounterPracticeAndUnrestrictedMapsSkipOwner(t *testing.T) {
	service, _, _ := newDungeonAdmissionCounterBridgeTest(t, 11)
	session := &gameSession{selectedCharacterID: 11}
	practice := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModePractice,
		Dungeon:      worldmap.Dungeon{ID: 5006},
	}
	if batch, err := service.reserveCurrentDungeonAdmissionCounters(context.Background(), session, practice, nil); err != nil || batch != nil {
		t.Fatalf("practice batch=%+v err=%v", batch, err)
	}
	unrestricted := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 700}}
	if batch, err := service.reserveCurrentDungeonAdmissionCounters(context.Background(), session, unrestricted, nil); err != nil || batch != nil {
		t.Fatalf("unrestricted batch=%+v err=%v", batch, err)
	}
}

func newDungeonAdmissionCounterBridgeTest(
	t *testing.T,
	characterIDs ...uint16,
) (*Service, dnfrepo.Group, *dnfdungeonadmission.Catalog) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "counter-account"}); err != nil {
		t.Fatal(err)
	}
	for _, characterID := range characterIDs {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: fmt.Sprint(characterID),
			AccountID:   "counter-account",
		}); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		dungeonAdmissionCatalog: catalog,
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	return service, repositories, catalog
}

func assertDungeonAdmissionBridgeCounterUsed(
	t *testing.T,
	repositories dnfrepo.Group,
	catalog *dnfdungeonadmission.Catalog,
	characterID string,
	dungeonID uint32,
	now time.Time,
	want uint32,
) {
	t.Helper()
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	state, err := owner.Preflight(context.Background(), characterID, catalog.PoliciesForDungeon(dungeonID), now)
	if err != nil || len(state) != 1 || state[0].Used != want {
		t.Fatalf("character=%s dungeon=%d counter=%+v err=%v want_used=%d", characterID, dungeonID, state, err, want)
	}
}

// serviceTimeForDungeonAdmissionTest is within the same Beijing 06:00 service
// day as the bridge's current test execution without relying on a hard-coded
// calendar period.
func serviceTimeForDungeonAdmissionTest() time.Time { return time.Now().UTC() }

type dungeonAdmissionBridgeSource map[string]string

func (s dungeonAdmissionBridgeSource) ReadText(path string) (string, error) {
	for candidate, text := range s {
		if strings.EqualFold(candidate, path) {
			return text, nil
		}
	}
	return "", fmt.Errorf("missing %s", path)
}

func dungeonAdmissionBridgeCatalogSource() dungeonAdmissionBridgeSource {
	return dungeonAdmissionBridgeSource{
		dnfdungeonadmission.DungeonListPath:      "8000 `Crackofdimension/Crackofdimension.dgn`\n",
		dnfdungeonadmission.CrackOfDimensionPath: "[daily enter count]\n10\n[/daily enter count]\n",
		dnfdungeonadmission.DimensionDungeonInfoPath: "[dimension dungeon info]\n0 7165 1 7166\n[/dimension dungeon info]\n" +
			"[max daily enter count]\n10\n",
		dnfdungeonadmission.LicenseDungeonInfoPath: "[dungeon license info]\n1 5006 1 10 2 1 5007 2 7 4\n[/dungeon license info]\n" +
			"[daily enter count]\n2\n",
		dnfdungeonadmission.DimensionGatePath: "[daily default enter count]\n5\n[dimensiondungeon list]\n62 25 0 64 25 0\n[/dimensiondungeon list]\n" +
			"[recently enter dungeon count]\n3\n",
		dnfdungeonadmission.WeeklyDungeonPath: "[reset day index]\n4\n[weekly default enter count]\n6\n",
		dnfdungeonadmission.AccountWeeklyDungeonPath: "[dungeon enter max count]\n4906 2 4907 1\n[/dungeon enter max count]\n" +
			"[default enterable count]\n5\n" +
			"[adventure group add enterable count chn]\n1 5 0 6 10 1\n" +
			"[weekly dungeon index]\n0 4907 1 4906\n[/weekly dungeon index]\n",
		dnfdungeonadmission.TowerOfDespairPath: "[enterable count]\n3\n[account enterable max count]\n3\n" +
			"[each layer matching dungeon]\n1 11008 -1\n[/each layer matching dungeon]\n",
		dnfdungeonadmission.TowerOfDespairInvitationPath: "[name]\n`绝望之邀请函`\n[explain]\n`通关成功后当日无法再继续挑战绝望之塔。`\n[stackable type]\n`[material]` 0\n",
		dnfdungeonadmission.SAOTowerOfDespairPath:        "[enterable count]\n3\n[each layer matching dungeon]\n1 3600 -1\n[/each layer matching dungeon]\n",
		dnfdungeonadmission.TowerOfGriefPath: "[enterable count]\n2\n[account enterable max count]\n1\n" +
			"[each layer matching dungeon]\n1 5115 -1\n[/each layer matching dungeon]\n" +
			"[use allow item list in dungeon]\n10160849 10 10163185 -1\n[/use allow item list in dungeon]\n",
		dnfdungeonadmission.BeastDungeonPath: "[limit enter count]\n2\n",
		dnfdungeonadmission.AntonRaidPath:    "[daily account enter limit]\n10\n[weekly enter count]\n30 3\n",
		dnfdungeonadmission.LukeRaidPath:     "[daily account enter limit]\n10\n[weekly enter count]\n20 2\n",
		dnfdungeonadmission.FiendWarRaidPath: "[daily account enter limit]\n10\n[weekly enter count]\n20 2\n",
	}
}
