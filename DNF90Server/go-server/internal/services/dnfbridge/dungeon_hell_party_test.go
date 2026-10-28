package dnfbridge

import (
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonHellPartyRequest(t *testing.T) {
	tests := []struct {
		name    string
		request dungeoncmd.SelectDungeonRequest
		want    bool
	}{
		{
			name: "ordinary dungeon",
			request: dungeoncmd.SelectDungeonRequest{
				DungeonID:    86,
				RuntimeToken: 0xffff,
			},
		},
		{
			name: "current dedicated selector",
			request: dungeoncmd.SelectDungeonRequest{
				DungeonID:     86,
				SelectionMode: 1,
				RuntimeToken:  0xffff,
			},
			want: true,
		},
		{
			name: "current manual selector",
			request: dungeoncmd.SelectDungeonRequest{
				DungeonID:    86,
				RuntimeToken: 0xffff,
				SpecialMode:  1,
			},
			want: true,
		},
		{
			name: "unowned mixed mode",
			request: dungeoncmd.SelectDungeonRequest{
				DungeonID:     86,
				SelectionMode: 1,
				RuntimeToken:  7,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := currentDungeonHellPartyRequest(test.request); got != test.want {
				t.Fatalf("currentDungeonHellPartyRequest(%+v) = %t, want %t", test.request, got, test.want)
			}
		})
	}
}

func TestCurrentDungeonHellRouteClassificationKeepsOrdinaryAndStandaloneOwnersSeparate(t *testing.T) {
	request := dungeoncmd.SelectDungeonRequest{
		DungeonID:     7542,
		SelectionMode: 1,
		RuntimeToken:  0xffff,
	}
	tests := []struct {
		name               string
		dungeon            worldmap.Dungeon
		mazeIndex          int
		wantSelectedEscape bool
		wantExplicit       bool
		wantStandalone     bool
		wantTimeRift       bool
	}{
		{
			name:           "ordinary embedded hell party",
			dungeon:        worldmap.Dungeon{Mazes: []worldmap.Maze{{}}},
			mazeIndex:      0,
			wantExplicit:   true,
			wantStandalone: false,
		},
		{
			name: "inferno time rift",
			dungeon: worldmap.Dungeon{
				Metadata: worldmap.DungeonMetadata{InfernoDungeon: true},
				Mazes:    []worldmap.Maze{{}},
			},
			mazeIndex:      0,
			wantStandalone: true,
			wantTimeRift:   true,
		},
		{
			name: "particular hell magic rift",
			dungeon: worldmap.Dungeon{
				Metadata: worldmap.DungeonMetadata{DungeonType: "[particular hell]"},
				Mazes:    []worldmap.Maze{{}},
			},
			mazeIndex:      0,
			wantStandalone: true,
		},
		{
			name: "selected escape hell maze",
			dungeon: worldmap.Dungeon{
				Metadata: worldmap.DungeonMetadata{IntegerValues: map[string]int64{"hell dungeon": 1}},
				Mazes: []worldmap.Maze{
					{EscapeHell: worldmap.OptionalInt{Value: 1, Set: true}},
					{},
				},
			},
			mazeIndex:          0,
			wantSelectedEscape: true,
			wantStandalone:     true,
			wantTimeRift:       true,
		},
		{
			name: "unmarked sibling of escape hell maze",
			dungeon: worldmap.Dungeon{
				Metadata: worldmap.DungeonMetadata{IntegerValues: map[string]int64{"hell dungeon": 1}},
				Mazes: []worldmap.Maze{
					{EscapeHell: worldmap.OptionalInt{Value: 1, Set: true}},
					{},
				},
			},
			mazeIndex:      1,
			wantExplicit:   true,
			wantStandalone: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selectedEscape := currentDungeonSelectedMazeEscapeHell(request, test.dungeon, test.mazeIndex)
			if selectedEscape != test.wantSelectedEscape {
				t.Fatalf("selected escape=%t want %t", selectedEscape, test.wantSelectedEscape)
			}
			explicit := currentDungeonExplicitHellPartyRequest(request, test.dungeon, test.mazeIndex)
			if explicit != test.wantExplicit {
				t.Fatalf("explicit hell party=%t want %t", explicit, test.wantExplicit)
			}
			runtime := &runtimeDungeonState{Request: request, Dungeon: test.dungeon, MazeIndex: test.mazeIndex}
			standalone := currentDungeonStandaloneRiftRuntime(runtime)
			if standalone != test.wantStandalone {
				t.Fatalf("runtime standalone rift=%t want %t", standalone, test.wantStandalone)
			}
			timeRift := currentDungeonTimeRiftRuntime(runtime)
			if timeRift != test.wantTimeRift {
				t.Fatalf("runtime time rift=%t want %t", timeRift, test.wantTimeRift)
			}
		})
	}

	ordinaryRequest := request
	ordinaryRequest.SelectionMode = 0
	ordinaryRequest.RuntimeToken = 0
	selectedEscapeDungeon := tests[3].dungeon
	if currentDungeonSelectedMazeEscapeHell(ordinaryRequest, selectedEscapeDungeon, 0) {
		t.Fatal("ordinary selector was misclassified as selected escape hell")
	}
	if currentDungeonStandaloneRiftRuntime(nil) {
		t.Fatal("nil runtime was classified as standalone rift")
	}
	if currentDungeonTimeRiftRuntime(nil) {
		t.Fatal("nil runtime was classified as time rift")
	}
}

func TestAuthorizeCurrentDungeonHellPartyUsesPersistedRegionPermission(t *testing.T) {
	ctx := context.Background()
	table, err := worldmap.LoadSource(ctx, hellPartyWorldMapTestSource{
		worldmap.DefaultWorldMapList: "290 `worldmap/metrocenter.wdm`\n",
		"worldmap/metrocenter.wdm":   "[name]\n`Metro Center`\n[dungeon]\n700 -1\n[/dungeon]\n[hell dungeon]\n1\n[hell quest]\n2900\n[/hell quest]\n",
	}, worldmap.Options{SkipMaps: true, SkipDungeons: true})
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
	session := &gameSession{selectedCharacterID: 19}
	if err := service.authorizeCurrentDungeonHellParty(ctx, session, table, 700); !errors.Is(err, errCurrentHellPartyLocked) {
		t.Fatalf("locked hell-party entry error=%v", err)
	}
	if _, _, err := repos.HellPartyPermission.Upsert(ctx, "19", 290, 2900); err != nil {
		t.Fatal(err)
	}
	if err := service.authorizeCurrentDungeonHellParty(ctx, session, table, 700); err != nil {
		t.Fatalf("unlocked hell-party entry error=%v", err)
	}

	// A completed [hell quest] without a permission row (GM unlock, records
	// that predate the permission table) must unlock the same region.
	questOnly := &gameSession{selectedCharacterID: 23}
	if err := service.authorizeCurrentDungeonHellParty(ctx, questOnly, table, 700); !errors.Is(err, errCurrentHellPartyLocked) {
		t.Fatalf("locked hell-party entry without quest error=%v", err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "23",
		States:      map[int64]dnfrepo.QuestState{2900: {Status: "completed", Extra: map[string]string{"gm_all_hell_unlocked": "true"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.authorizeCurrentDungeonHellParty(ctx, questOnly, table, 700); err != nil {
		t.Fatalf("quest-completed hell-party entry error=%v", err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "23",
		States:      map[int64]dnfrepo.QuestState{2900: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.authorizeCurrentDungeonHellParty(ctx, questOnly, table, 700); !errors.Is(err, errCurrentHellPartyLocked) {
		t.Fatalf("active-quest hell-party entry error=%v", err)
	}
}

type hellPartyWorldMapTestSource map[string]string

func (s hellPartyWorldMapTestSource) ReadText(path string) (string, error) {
	value, ok := s[path]
	if !ok {
		return "", dnfpvf.ErrDocNotFound
	}
	return value, nil
}
