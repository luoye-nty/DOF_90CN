package progression

import (
	"errors"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestDungeonLocalExperienceSourcesLoadAndCacheRawRows(t *testing.T) {
	sources, err := LoadDungeonLocalExperienceSources(dungeonLocalExperienceTestSource{
		DungeonDefinitionListPath: "7146 `dungeon/story_a.dgn` 7155 `dungeon/story_b.dgn` 4907 `raid/legacy.dgn`",
		"dungeon/story_a.dgn":     "[increase exp rate]\n100 130\n[/increase exp rate]",
		"dungeon/story_b.dgn":     "[other]\n1\n[/other]",
		"dungeon/raid/legacy.dgn": "[increase exp rate]\n10 -10\n[/increase exp rate]",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := sources.Snapshot(); snapshot != (DungeonLocalExperienceSnapshot{DungeonDefinitions: 3}) {
		t.Fatalf("initial snapshot=%+v", snapshot)
	}
	row, pathFound, err := sources.Rule(7146)
	if err != nil || !pathFound || !row.Present || row.DungeonID != 7146 || row.Path != "dungeon/story_a.dgn" || row.Base != 100 || row.Bonus != 130 {
		t.Fatalf("7146 row=%+v path_found=%t err=%v", row, pathFound, err)
	}
	if snapshot := sources.Snapshot(); snapshot.LoadedDefinitions != 1 {
		t.Fatalf("loaded snapshot=%+v", snapshot)
	}
	row, pathFound, err = sources.Rule(7155)
	if err != nil || !pathFound || row.Present {
		t.Fatalf("missing local section row=%+v path_found=%t err=%v", row, pathFound, err)
	}
	if row, pathFound, err = sources.Rule(3); err != nil || pathFound || row.Present {
		t.Fatalf("unknown dungeon got fallback row=%+v path_found=%t err=%v", row, pathFound, err)
	}
	if _, _, err := sources.Rule(0); !errors.Is(err, ErrDungeonLocalLookup) {
		t.Fatalf("zero lookup error=%v", err)
	}
}

func TestDungeonLocalExperienceSourcesRejectMalformedDefinitions(t *testing.T) {
	if _, err := LoadDungeonLocalExperienceSources(nil); !errors.Is(err, ErrDungeonLocalSourceRequired) {
		t.Fatalf("nil source error=%v", err)
	}
	tests := []struct {
		name   string
		source dungeonLocalExperienceTestSource
	}{
		{
			name: "odd list",
			source: dungeonLocalExperienceTestSource{
				DungeonDefinitionListPath: "7146 `dungeon/story_a.dgn` 7155",
			},
		},
		{
			name: "duplicate id",
			source: dungeonLocalExperienceTestSource{
				DungeonDefinitionListPath: "7146 `dungeon/story_a.dgn` 7146 `dungeon/story_b.dgn`",
			},
		},
		{
			name: "malformed row",
			source: dungeonLocalExperienceTestSource{
				DungeonDefinitionListPath: "7146 `dungeon/story_a.dgn`",
				"dungeon/story_a.dgn":     "[increase exp rate]\n100\n[/increase exp rate]",
			},
		},
		{
			name: "negative effective rate",
			source: dungeonLocalExperienceTestSource{
				DungeonDefinitionListPath: "7146 `dungeon/story_a.dgn`",
				"dungeon/story_a.dgn":     "[increase exp rate]\n100 -101\n[/increase exp rate]",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sources, err := LoadDungeonLocalExperienceSources(test.source)
			if test.name == "malformed row" || test.name == "negative effective rate" {
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := sources.Rule(7146); !errors.Is(err, ErrDungeonLocalSectionShape) {
					t.Fatalf("row error=%v", err)
				}
				return
			}
			if !errors.Is(err, ErrDungeonLocalSectionShape) {
				t.Fatalf("load error=%v", err)
			}
		})
	}
}

func TestRealScriptPVFDungeonLocalExperienceSourcesPreserveRawRows(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real dungeon local experience sources")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := LoadDungeonLocalExperienceSources(archive)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := sources.Snapshot(); snapshot.DungeonDefinitions < 100 {
		t.Fatalf("real dungeon local snapshot=%+v", snapshot)
	}
	row, found, err := sources.Rule(7146)
	if err != nil || !found || row.DungeonID != 7146 || row.Path == "" {
		t.Fatalf("real dungeon 7146 row=%+v found=%t err=%v", row, found, err)
	}
	if snapshot := sources.Snapshot(); snapshot.LoadedDefinitions != 1 {
		t.Fatalf("real loaded snapshot=%+v", snapshot)
	}
}

type dungeonLocalExperienceTestSource map[string]string

func (source dungeonLocalExperienceTestSource) ReadText(relativePath string) (string, error) {
	if text, found := source[relativePath]; found {
		return text, nil
	}
	return "", errors.New("missing test PVF path: " + relativePath)
}
