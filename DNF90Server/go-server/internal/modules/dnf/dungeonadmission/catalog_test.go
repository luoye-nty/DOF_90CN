package dungeonadmission

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type catalogTestSource map[string]string

func (s catalogTestSource) ReadText(path string) (string, error) {
	for candidate, text := range s {
		if strings.EqualFold(candidate, path) {
			return text, nil
		}
	}
	return "", fmt.Errorf("missing %s", path)
}

func TestCatalogBindsOnlyPVFProvedPeriodsAndMembership(t *testing.T) {
	catalog, err := LoadCatalog(syntheticCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	assertDungeonPolicy(t, catalog, 8000, "crack_of_dimension.character.daily", 10, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 7165, "dimension_dungeon.character.daily", 10, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5006, "license_dungeon.character.daily", 2, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 62, "dimension_gate.character.daily", 5, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5115, "tower_of_grief.character.daily", 2, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 5115, "tower_of_grief.account.daily", 1, PeriodDaily, 0)
	for dungeonID, limit := range map[uint32]uint32{4906: 2, 4907: 1} {
		assertDungeonPolicy(t, catalog, dungeonID,
			fmt.Sprintf("account_character_dungeon.%d.character.daily", dungeonID), limit, PeriodDaily, 0)
		assertDungeonPolicy(t, catalog, dungeonID,
			AccountCharacterDungeonAccountPolicyKey, 5, PeriodDaily, 0)
	}
	for level, want := range map[uint32]uint32{1: 5, 5: 5, 6: 6, 10: 6, 11: 7} {
		if got := catalog.AccountCharacterDungeonDailyLimit(level); got != want {
			t.Fatalf("adventure level %d daily limit=%d want=%d", level, got, want)
		}
	}

	assertDungeonPolicy(t, catalog, 11008, "tower_of_despair.character.daily", 3, PeriodDaily, 0)
	assertDungeonPolicy(t, catalog, 11008, "tower_of_despair.account.daily", 3, PeriodDaily, 0)
	evidence := catalog.EvidenceForDungeon(11008)
	if len(evidence) != 2 {
		t.Fatalf("tower evidence=%+v want character/account", evidence)
	}
	for _, row := range evidence {
		if !row.Enforced || row.Period != PeriodDaily || row.Limit != 3 {
			t.Fatalf("tower evidence incorrectly enforceable=%+v", row)
		}
	}
	if history := findEvidence(catalog, "dimension_gate.recent_history"); history.Enforced || history.Period != 0 || history.Limit != 3 {
		t.Fatalf("recent-history count became a limit=%+v", history)
	}

	for _, key := range []string{"raid.anton.account.daily", "raid.luke.account.daily", "raid.fiend_war.account.daily"} {
		policy, found := catalog.Policy(key)
		if !found || policy.Scope != ScopeAccount || policy.Period != PeriodDaily || policy.Limit != 10 || len(policy.DungeonIDs) != 0 {
			t.Fatalf("raid-start policy %s found=%t policy=%+v", key, found, policy)
		}
	}
	// Every raid's [WEEKLY ENTER COUNT] row is now BOTH scopes, not evidence:
	// the shipped documents declare `30 3` / `20 2` / `20 2` and the client's two
	// team-mode windows render exactly those as 帐号周入场场数 (first operand) and
	// 角色周入场次数 (second operand), so the operand meaning is read off the
	// client rather than inferred. All six carry no dungeon ids for the same
	// reason the daily policies do not: these are raid-START limits looked up by
	// semantic key, so PoliciesForDungeon must never apply them per gate.
	for _, weekly := range []struct {
		key   string
		path  string
		scope Scope
		limit uint32
	}{
		{AntonWeeklyAccountPolicyKey, AntonRaidPath, ScopeAccount, 30},
		{AntonWeeklyCharacterPolicyKey, AntonRaidPath, ScopeCharacter, 3},
		{LukeWeeklyAccountPolicyKey, LukeRaidPath, ScopeAccount, 20},
		{LukeWeeklyCharacterPolicyKey, LukeRaidPath, ScopeCharacter, 2},
		{FiendWarWeeklyAccountPolicyKey, FiendWarRaidPath, ScopeAccount, 20},
		{FiendWarWeeklyCharacterPolicyKey, FiendWarRaidPath, ScopeCharacter, 2},
	} {
		policy, found := catalog.Policy(weekly.key)
		if !found || policy.Scope != weekly.scope || policy.Period != PeriodWeekly ||
			policy.Limit != weekly.limit || policy.ResetWeekday != time.Thursday ||
			len(policy.DungeonIDs) != 0 || policy.SourcePath != weekly.path ||
			policy.SourceSection != "weekly enter count" {
			t.Fatalf("raid weekly policy %s found=%t policy=%+v", weekly.key, found, policy)
		}
	}
	// The shipped fiendwar.etc repeats the [WEEKLY ENTER COUNT] section verbatim.
	// A repeated-but-identical pair is tolerated; the pair that lands in the
	// catalog is the first one and both scopes must agree with it.
	if repeated, found := catalog.Policy(FiendWarWeeklyAccountPolicyKey); !found || repeated.Limit != 20 {
		t.Fatalf("duplicated fiendwar weekly section produced %+v found=%t", repeated, found)
	}
	if beast := findEvidence(catalog, "beast_dungeon.unbound"); beast.Enforced || beast.Period != 0 || beast.Limit != 2 {
		t.Fatalf("beast period was guessed=%+v", beast)
	}

	// Luke's [WEEKLY ENTER COUNT] `20 2` additionally gets a real Policy, because its
	// per-character share is corroborated for this client's version window instead of
	// merely guessed. It owns the counter and the window, and it carries NO dungeon
	// ids on purpose: PoliciesForDungeon must keep returning nothing for Luke's gates
	// so that nothing starts refusing until the reset node is confirmed.
	weekly, found := catalog.Policy(LukeWeeklyCharacterPolicyKey)
	if !found || weekly.Scope != ScopeCharacter || weekly.Period != PeriodWeekly || weekly.Limit != 2 ||
		weekly.ResetWeekday != time.Thursday || len(weekly.DungeonIDs) != 0 ||
		weekly.SourcePath != LukeRaidPath || weekly.SourceSection != "weekly enter count" {
		t.Fatalf("luke weekly policy found=%t policy=%+v", found, weekly)
	}
	for _, dungeonID := range []uint32{5054, 5057, 5061, 5323} {
		if policies := catalog.PoliciesForDungeon(dungeonID); len(policies) != 0 {
			t.Fatalf("luke weekly policy became enforceable dungeon=%d policies=%+v", dungeonID, policies)
		}
	}
}

func TestCatalogRejectsWeeklyDungeonMissingFromDayIndex(t *testing.T) {
	source := syntheticCatalogSource()
	source[AccountWeeklyDungeonPath] = "[dungeon enter max count]\n4906 2 4907 1\n[/dungeon enter max count]\n" +
		"[default enterable count]\n5\n" +
		"[adventure group add enterable count chn]\n1 5 0\n" +
		"[weekly dungeon index]\n0 4906\n[/weekly dungeon index]\n"
	if _, err := LoadCatalog(source); err == nil || !strings.Contains(err.Error(), "4907 absent") {
		t.Fatalf("weekly mismatch err=%v", err)
	}
}

func TestCatalogPoliciesAreImmutableCopies(t *testing.T) {
	catalog, err := LoadCatalog(syntheticCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	policies := catalog.PoliciesForDungeon(7165)
	policies[0].DungeonIDs[0] = 99
	again := catalog.PoliciesForDungeon(7165)
	if len(again) != 1 || again[0].DungeonIDs[0] != 7165 {
		t.Fatalf("catalog mutated through copy=%+v", again)
	}
}

func TestCatalogKeepsOrderedTowerLayersAndEntryPairs(t *testing.T) {
	catalog, err := LoadCatalog(syntheticCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	despair, found := catalog.TowerRoute(TowerFamilyDespair)
	if !found || len(despair.DungeonIDs) != 2 || despair.DungeonIDs[0] != 11008 || despair.DungeonIDs[1] != 11009 || len(despair.UseAllowItems) != 0 {
		t.Fatalf("despair route=%+v found=%t", despair, found)
	}
	grief, found := catalog.TowerRoute(TowerFamilyGrief)
	if !found || len(grief.DungeonIDs) != 1 || grief.DungeonIDs[0] != 5115 || len(grief.UseAllowItems) != 2 ||
		grief.UseAllowItems[0] != (TowerUseItem{ItemID: 10160849, Count: 10}) ||
		grief.UseAllowItems[1] != (TowerUseItem{ItemID: 10163185, Count: -1}) {
		t.Fatalf("grief route=%+v found=%t", grief, found)
	}
	despair.DungeonIDs[0] = 1
	again, _ := catalog.TowerRoute(TowerFamilyDespair)
	if again.DungeonIDs[0] != 11008 {
		t.Fatalf("tower route leaked mutable slice=%+v", again)
	}
}

func assertDungeonPolicy(t *testing.T, catalog *Catalog, dungeonID uint32, key string, limit uint32, period Period, weekday time.Weekday) {
	t.Helper()
	policies := catalog.PoliciesForDungeon(dungeonID)
	for _, policy := range policies {
		if policy.Key == key {
			if policy.Limit != limit || policy.Period != period || policy.ResetWeekday != weekday {
				t.Fatalf("dungeon=%d policy=%+v", dungeonID, policy)
			}
			return
		}
	}
	t.Fatalf("dungeon=%d policies=%+v missing=%s", dungeonID, policies, key)
}

func findEvidence(catalog *Catalog, key string) Evidence {
	for _, evidence := range catalog.Evidence() {
		if evidence.Key == key {
			return evidence
		}
	}
	return Evidence{}
}

func syntheticCatalogSource() catalogTestSource {
	return catalogTestSource{
		DungeonListPath:      "8000 `Crackofdimension/Crackofdimension.dgn`\n",
		CrackOfDimensionPath: "[daily enter count]\n10\n[/daily enter count]\n",
		DimensionDungeonInfoPath: "[dimension dungeon info]\n0 7165 1 7166 2 7167\n[/dimension dungeon info]\n" +
			"[max daily enter count]\n10\n",
		LicenseDungeonInfoPath: "[dungeon license info]\n1 5006 1 10 2 1 5007 2 7 4\n[/dungeon license info]\n" +
			"[daily enter count]\n2\n",
		DimensionGatePath: "[daily default enter count]\n5\n[dimensiondungeon list]\n62 25 0 64 25 0\n[/dimensiondungeon list]\n" +
			"[recently enter dungeon count]\n3\n",
		WeeklyDungeonPath: "[reset day index]\n4\n[weekly default enter count]\n6\n",
		AccountWeeklyDungeonPath: "[dungeon enter max count]\n4906 2 4907 1\n[/dungeon enter max count]\n" +
			"[default enterable count]\n5\n" +
			"[adventure group add enterable count chn]\n1 5 0 6 10 1 11 15 2\n" +
			"[weekly dungeon index]\n0 4907 1 4906\n[/weekly dungeon index]\n",
		TowerOfDespairPath: "[enterable count]\n3\n[account enterable max count]\n3\n" +
			"[each layer matching dungeon]\n1 11008 -1 2 11009 -1\n[/each layer matching dungeon]\n",
		TowerOfDespairInvitationPath: "[name]\n`绝望之邀请函`\n[explain]\n`通关成功后当日无法再继续挑战绝望之塔。`\n[stackable type]\n`[material]` 0\n",
		SAOTowerOfDespairPath:        "[enterable count]\n3\n[each layer matching dungeon]\n1 3600 -1\n[/each layer matching dungeon]\n",
		TowerOfGriefPath: "[enterable count]\n2\n[account enterable max count]\n1\n" +
			"[each layer matching dungeon]\n1 5115 -1\n[/each layer matching dungeon]\n" +
			"[use allow item list in dungeon]\n10160849 10 10163185 -1\n[/use allow item list in dungeon]\n",
		BeastDungeonPath: "[limit enter count]\n2\n",
		AntonRaidPath:    "[daily account enter limit]\n10\n[weekly enter count]\n30 3\n",
		LukeRaidPath:     "[daily account enter limit]\n10\n[weekly enter count]\n20 2\n",
		FiendWarRaidPath: "[daily account enter limit]\n10\n[weekly enter count]\n20 2\n",
	}
}
