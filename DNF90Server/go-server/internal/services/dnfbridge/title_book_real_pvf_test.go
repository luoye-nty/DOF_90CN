package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFTitleAchievementInitialProjection(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify title achievement projection")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	text, err := archive.ReadText("etc/titlebook.etc")
	if err != nil {
		t.Fatal(err)
	}
	mapping := parseTitleBookEtc(text)
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.TitleAchievementDefinitions(dnfquest.CharacterEligibility{Level: 90, Job: 0})
	rows := currentInitialTitleAchievementRows(definitions, mapping, make(map[uint32]struct{}))
	byID := make(map[uint32]currentInsertOverseerRow, len(rows))
	for _, row := range rows {
		byID[row.achievementID] = row
	}
	for questID, want := range map[uint32]uint16{
		6501: 1,
		6588: 20,
		6597: 500,
	} {
		row, ok := byID[questID]
		if !ok || row.p1 != want || row.p2 != 0 || row.p3 != 0 {
			t.Fatalf("real title achievement q%d row=%+v found=%t want remaining=%d", questID, row, ok, want)
		}
	}
}
