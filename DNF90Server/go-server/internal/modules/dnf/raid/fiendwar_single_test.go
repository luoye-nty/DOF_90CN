package raid

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestParseFiendWarSingleConfig(t *testing.T) {
	base := fixtureFiendWarSingleConfig()
	document, err := dnfpvf.Parse(FiendWarSinglePVFPath, fixtureFiendWarSingleText())
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	single, err := parseFiendWarSingleConfig(document, base)
	if err != nil {
		t.Fatalf("parse single config: %v", err)
	}
	if single.TimeLimitSeconds != 1800 || single.MonsterLevel != 90 {
		t.Fatalf("single limits = %+v", single)
	}
	if len(single.Routes) != 3 || len(single.BossAreas) != 2 {
		t.Fatalf("single routes=%d bosses=%v", len(single.Routes), single.BossAreas)
	}
	for i, want := range []int{3, 4, 5} {
		if got := single.Routes[i].BossGateAreas; !slices.Equal(got, []int{want}) {
			t.Fatalf("route %s boss gates = %v, want [%d]", single.Routes[i].Name, got, want)
		}
	}
	if base.Battle.Raid.SingleMode == nil ||
		base.Battle.Raid.SingleMode.Phase.MinimalDungeonClearCount != 1 ||
		len(base.Battle.Raid.SingleMode.Phase.Rewards) != 2 ||
		base.Battle.Raid.SingleStartMinimumMembers != 1 {
		t.Fatalf("single mode was not attached to the raid config: %+v", base.Battle.Raid.SingleMode)
	}
}

func TestParseFiendWarSingleConfigInfersLegacyBossGate(t *testing.T) {
	text := fixtureFiendWarSingleText()
	for _, area := range []string{"3", "4", "5"} {
		text = strings.ReplaceAll(text, "[BOSS GATE AREA]\n"+area+"\n", "")
	}
	document, err := dnfpvf.Parse(FiendWarSinglePVFPath, text)
	if err != nil {
		t.Fatalf("parse legacy fixture: %v", err)
	}
	single, err := parseFiendWarSingleConfig(document, fixtureFiendWarSingleConfig())
	if err != nil {
		t.Fatalf("parse legacy single config: %v", err)
	}
	for i, want := range []int{3, 4, 5} {
		if got := single.Routes[i].BossGateAreas; !slices.Equal(got, []int{want}) {
			t.Fatalf("legacy route %s inferred boss gates = %v, want [%d]", single.Routes[i].Name, got, want)
		}
	}
}

// TestDistributedFiendWarSinglePVFFile verifies the exact file staged for the
// operator against the real Fiend War area graph. It is opt-in because the
// repository also runs without the full client/server PVF installed.
func TestDistributedFiendWarSinglePVFFile(t *testing.T) {
	pvfPath := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	singlePath := os.Getenv("DNF_FIENDWAR_SINGLE_PVF_FILE")
	if pvfPath == "" || singlePath == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF or DNF_FIENDWAR_SINGLE_PVF_FILE not set")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open PVF: %v", err)
	}
	config, err := LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	text, err := os.ReadFile(singlePath)
	if err != nil {
		t.Fatalf("read staged single config: %v", err)
	}
	document, err := dnfpvf.Parse(FiendWarSinglePVFPath, string(text))
	if err != nil {
		t.Fatalf("parse staged single config: %v", err)
	}
	single, err := parseFiendWarSingleConfig(document, config)
	if err != nil {
		t.Fatalf("validate staged single config: %v", err)
	}
	if len(single.Routes) != 3 || len(single.BossAreas) != 2 || config.Battle.Raid.SingleMode == nil {
		t.Fatalf("staged single config incomplete: %+v", single)
	}
	for _, route := range single.Routes {
		for _, bossArea := range single.BossAreas {
			battle, err := NewFiendWarBattle(config.Battle)
			if err != nil {
				t.Fatalf("new battle for route=%s boss=%d: %v", route.Name, bossArea, err)
			}
			if err := battle.ConfigureSingle(route, bossArea); err != nil {
				t.Fatalf("configure route=%s boss=%d: %v", route.Name, bossArea, err)
			}
			if !battle.SingleAreaAllowed(bossArea) {
				t.Fatalf("boss area %d is not allowed for route %s", bossArea, route.Name)
			}
			gateArea := route.BossGateAreas[0]
			gateDungeon := config.Battle.Areas[gateArea].Dungeon
			battle.Areas[gateArea] = FiendWarAreaState{Group: 1}
			if err := battle.Clear(gateDungeon, time.Unix(0, 0)); err != nil {
				t.Fatalf("clear command center route=%s dungeon=%d: %v", route.Name, gateDungeon, err)
			}
			if !battle.AreaOpen(bossArea) {
				t.Fatalf("boss area %d did not open after route=%s command center cleared", bossArea, route.Name)
			}
			otherBossArea := 30
			if bossArea == 30 {
				otherBossArea = 31
			}
			selectedKind := config.Battle.Areas[bossArea].Kind
			otherKind := config.Battle.Areas[otherBossArea].Kind
			if battle.Value(selectedKind+" state") != 1 {
				t.Fatalf("selected %s state = %d, want 1", selectedKind, battle.Value(selectedKind+" state"))
			}
			if battle.Value(otherKind+" state") != 0 {
				t.Fatalf("unselected %s state = %d, want 0 (disabled)", otherKind, battle.Value(otherKind+" state"))
			}
		}
	}
}

func TestFiendWarSingleBattleRestrictsRouteAndCompletesOneBoss(t *testing.T) {
	b := newFixtureSingleBattle(t)
	if b.AreaOpen(31) {
		t.Fatal("unselected boss started open")
	}
	if err := b.MoveMember(1, 1, 31, time.Unix(0, 0)); err == nil {
		t.Fatal("single run moved onto an unselected route")
	}
	if err := b.MoveMember(1, 1, 3, time.Unix(0, 0)); err != nil {
		t.Fatalf("move to selected route: %v", err)
	}
	b.GroupAreas[1] = 3
	if err := b.Enter(1, 4954, time.Unix(0, 0)); err != nil {
		t.Fatalf("enter selected route dungeon: %v", err)
	}
	if err := b.Clear(4954, time.Unix(0, 0)); err != nil {
		t.Fatalf("clear selected route dungeon: %v", err)
	}
	if !b.AreaOpen(30) {
		t.Fatal("selected boss did not open after the required route cleared")
	}
	if b.AreaOpen(31) {
		t.Fatal("the other boss opened in single mode")
	}
	for _, name := range []string{"top command center state", "mid command center state", "bot command center state"} {
		if got := b.Value(name); got != 0 {
			t.Fatalf("%s = %d, want 0 so the client quantum-gate precondition passes", name, got)
		}
	}
	if got := b.Value("top boss state"); got != 1 {
		t.Fatalf("selected top boss state = %d, want 1", got)
	}
	if got := b.Value("bot boss state"); got != 10 {
		t.Fatalf("unselected bot boss state = %d, want 10 (finished/non-selectable)", got)
	}

	b.Areas[30] = FiendWarAreaState{Group: 1}
	b.set("top boss state", 9)
	b.set("top boss hp", 0)
	if err := b.ClearClientBossRoom(4978, time.Unix(0, 0)); err != nil {
		t.Fatalf("clear selected boss: %v", err)
	}
	if !b.Complete {
		t.Fatal("single Fiend War did not complete after one selected boss")
	}
}

func TestFiendWarSingleBossPatternAssignsAllFourPlanetsToOneSide(t *testing.T) {
	b := newFixtureSingleBattle(t)
	b.Areas[30] = FiendWarAreaState{Group: 1}
	b.StartSingleBossPattern(30)
	if got := b.Value("bot boss state"); got != 10 {
		t.Fatalf("absent bottom side received pattern state %d", got)
	}
	for i := 0; i < 4; i++ {
		state := b.Value("top boss state")
		if state < 5 || state > 8 {
			t.Fatalf("planet %d state = %d, want 5..8", i+1, state)
		}
		if err := b.BossReport(4978, 1, state, time.Unix(0, 0)); err != nil {
			t.Fatalf("report planet %d: %v", i+1, err)
		}
	}
	if got := b.Value("top boss state"); got != 9 {
		t.Fatalf("single side after four planets = %d, want 9", got)
	}
}

func TestFiendWarSingleBossAbandonKeepsOnlyTheSelectedBossAvailable(t *testing.T) {
	b := newFixtureSingleBattle(t)
	b.Areas[30] = FiendWarAreaState{Group: 1}
	b.DungeonStates[4954] = "clear"
	b.set("top boss state", 1)
	b.set("bot boss state", 10)

	if err := b.Abandon(4978, time.Unix(0, 0)); err != nil {
		t.Fatalf("abandon selected boss: %v", err)
	}
	if got := b.Value("top boss state"); got != 1 {
		t.Fatalf("selected boss after abandon = %d, want 1", got)
	}
	if got := b.Value("bot boss state"); got != 10 {
		t.Fatalf("unselected boss after abandon = %d, want 10", got)
	}
	if !b.AreaOpen(30) || b.AreaOpen(31) {
		t.Fatalf("boss availability after abandon: top=%t bot=%t", b.AreaOpen(30), b.AreaOpen(31))
	}
}

func newFixtureSingleBattle(t *testing.T) *FiendWarBattle {
	t.Helper()
	config := &FiendWarBattleConfig{
		Raid:        &AntonConfig{},
		WaitingArea: 2,
		Areas: map[int]FiendWarArea{
			2:  {Index: 2, Dungeon: -1, Next: []int{3, 31}},
			3:  {Index: 3, Dungeon: 4954, Kind: "top command center", Next: []int{2, 4, 30}, MemberMax: 4},
			4:  {Index: 4, Dungeon: 4955, Next: []int{3, 30}, MemberMax: 4},
			19: {Index: 19, Dungeon: 4967, Kind: "top outpost", MemberMax: 4},
			30: {Index: 30, Dungeon: 4978, Kind: "top boss", Symbol: 200, Next: []int{3}, MemberMax: 4},
			31: {Index: 31, Dungeon: 4953, Kind: "bot boss", Symbol: 201, Next: []int{2}, MemberMax: 4},
		},
		Symbols: map[string]uint32{
			"top boss state": 200, "bot boss state": 201,
			"top boss hp": 202, "bot boss hp": 203,
			"top boss view hp": 204, "bot boss view hp": 205,
			"boss pattern state": 206, "top command center state": 207,
		},
		Rules: []FiendWarRule{{}},
	}
	b, err := NewFiendWarBattle(config)
	if err != nil {
		t.Fatalf("new single battle: %v", err)
	}
	if err := b.ConfigureSingle(FiendWarSingleRoute{
		Name:          "top",
		RequiredAreas: []int{3, 4},
		BossGateAreas: []int{3},
	}, 30); err != nil {
		t.Fatalf("configure single: %v", err)
	}
	b.GroupAreas[1] = 2
	b.MemberAreas[1] = 2
	b.set("top command center state", 1)
	b.Areas[3] = FiendWarAreaState{Enemy: FiendWarUnit{ID: 1, Type: 1}}
	return b
}

func TestFiendWarSingleIgnoresWarningsOutsideSelectedRoute(t *testing.T) {
	b := newFixtureSingleBattle(t)
	now := time.Unix(0, 0)
	b.act(FiendWarInstruction{Op: "warning enemy", Args: []dnfpvf.Token{{Kind: dnfpvf.TokenInt, Int: 19}}}, now)
	if b.Areas[19].Warning {
		t.Fatal("unselected area 19 entered warning state")
	}
	b.act(FiendWarInstruction{Op: "warning enemy", Args: []dnfpvf.Token{{Kind: dnfpvf.TokenInt, Int: 3}}}, now)
	if !b.Areas[3].Warning {
		t.Fatal("selected route warning was suppressed")
	}
}

func TestFiendWarSingleIsolatesTimersAndEnemiesOutsideSelectedRoute(t *testing.T) {
	b := newFixtureSingleBattle(t)
	now := time.Unix(0, 0)
	setTimer := func(area int, seconds int64) FiendWarInstruction {
		return FiendWarInstruction{
			Op: "set timer",
			Args: []dnfpvf.Token{
				{Kind: dnfpvf.TokenString, Value: "warning_enemy"},
				{Kind: dnfpvf.TokenInt, Int: int64(area)},
				{Kind: dnfpvf.TokenInt, Int: seconds},
			},
		}
	}

	b.act(setTimer(19, 1), now)
	if _, found := b.Timers[FiendWarTimerKey{19, "warning_enemy"}]; found {
		t.Fatal("unselected route timer was scheduled")
	}
	b.act(setTimer(3, 1), now)
	if _, found := b.Timers[FiendWarTimerKey{3, "warning_enemy"}]; !found {
		t.Fatal("selected route timer was suppressed")
	}

	b.act(FiendWarInstruction{
		Op:   "create enemy",
		Args: []dnfpvf.Token{{Kind: dnfpvf.TokenInt, Int: 19}},
	}, now)
	if b.Areas[19].Enemy.ID != 0 {
		t.Fatal("unselected route received an enemy")
	}

	// A timer left by an already-running or older state is discarded on the
	// first tick instead of producing its warning/move/fail chain.
	b.Timers[FiendWarTimerKey{19, "warning_enemy"}] = FiendWarTimer{
		StartedAt: now,
		Deadline:  now.Add(time.Second),
	}
	if err := b.Tick(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, found := b.Timers[FiendWarTimerKey{19, "warning_enemy"}]; found {
		t.Fatal("unselected route timer survived a tick")
	}
	if b.Failed {
		t.Fatal("unselected route timer failed the single raid")
	}
}

func fixtureFiendWarSingleConfig() *FiendWarConfig {
	return &FiendWarConfig{
		Battle: &FiendWarBattleConfig{
			Raid: &AntonConfig{
				PhaseMax: 1,
				Phases: []AntonPhase{{Dungeons: []AntonDungeonInfo{
					{Index: 4954, InitState: AntonDungeonHide, HP: 1, PartyMax: 1},
					{Index: 4978, InitState: AntonDungeonHide, HP: 1, PartyMax: 1},
					{Index: 4953, InitState: AntonDungeonHide, HP: 1, PartyMax: 1},
				}}},
			},
			Areas: map[int]FiendWarArea{
				3:  {Index: 3, Dungeon: 4954, Kind: "top command center"},
				4:  {Index: 4, Dungeon: 4955, Kind: "mid command center"},
				5:  {Index: 5, Dungeon: 4956, Kind: "bot command center"},
				15: {Index: 15, Dungeon: 4963, Kind: "radar"},
				17: {Index: 17, Dungeon: 4965, Kind: "battery"},
				30: {Index: 30, Dungeon: 4978, Kind: "top boss"},
				31: {Index: 31, Dungeon: 4953, Kind: "bot boss"},
			},
		},
	}
}

func fixtureFiendWarSingleText() string {
	return `[ENABLED]
1
[SINGLE RAID PHASE]
[PHASE NAME]
` + "`超时空漩涡 - 单人`" + `
[SINGLE RAID TIME OVER]
1800
[MINIMAL DUNGEON CLEAR COUNT]
1
[MONSTER LEVEL]
90
[CLEAR REWARD ITEM]
[STATE REWARD]
` + "`party_card` -1 1 490008037 0" + `
[STATE REWARD]
` + "`squad_item` -1 1 490008038 0" + `
[/CLEAR REWARD ITEM]
[SINGLE RAID BOSS AREA]
30 31
[SINGLE RAID ROUTE]
[NAME]
` + "`top`" + `
[REQUIRED AREA]
3
[BOSS GATE AREA]
3
[OPTIONAL AREA]
15
[/SINGLE RAID ROUTE]
[SINGLE RAID ROUTE]
[NAME]
` + "`mid`" + `
[REQUIRED AREA]
4
[BOSS GATE AREA]
4
[/SINGLE RAID ROUTE]
[SINGLE RAID ROUTE]
[NAME]
` + "`bot`" + `
[REQUIRED AREA]
5
[BOSS GATE AREA]
5
[OPTIONAL AREA]
17
[/SINGLE RAID ROUTE]
[/SINGLE RAID PHASE]
`
}
