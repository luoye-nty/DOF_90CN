package worldmap

import (
	"os"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFForgottenLandDefinition(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNF_WORLDMAP_REAL_PVF_SMOKE is not set")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	const dungeonPath = "dungeon/Fun_test/Fun_test2.dgn"
	source, err := archive.ReadText(dungeonPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse(dungeonPath, source)
	if err != nil {
		t.Fatal(err)
	}
	dungeon := ParseDungeon(4000, dungeonPath, document)
	if dungeon.Metadata.DungeonType != "lost land" {
		t.Fatalf("dungeon type=%q", dungeon.Metadata.DungeonType)
	}
	if got := dungeon.Metadata.LostLandParameters; !got.Set || got.DayMilliseconds != 120000 || got.NightMilliseconds != 120000 {
		t.Fatalf("lost land parameters=%+v", got)
	}
	if got := dungeon.Metadata.LimitTime; !got.Set || got.PrimarySeconds != 600 || got.SecondarySeconds != 1200 {
		for _, section := range dungeon.SourceSections {
			if strings.Contains(sectionKey(section.Name), "limit") || strings.Contains(sectionKey(section.Name), "lost land") {
				t.Logf("section name=%q scope=%q block=%t tokens=%+v", section.Name, section.Scope, section.Block, section.Tokens)
			}
		}
		t.Fatalf("limit time=%+v", got)
	}
	if len(dungeon.WarpMapConditions) != 50 {
		t.Fatalf("warp transition count=%d", len(dungeon.WarpMapConditions))
	}
	wantSource := Point{X: 8, Y: 7}
	wantDestination := Point{X: 0, Y: 9}
	found := false
	for _, condition := range dungeon.WarpMapConditions {
		if condition.Source == wantSource && condition.Destination == wantDestination {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing real Forgotten Land warp %v -> %v", wantSource, wantDestination)
	}
}
