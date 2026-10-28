package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestParseTitleBookEtcUsesSlotMarkerFiveTuple(t *testing.T) {
	text := `
[title collection info]
	` + "`general`" + `	200
	0	0	6501	1	26596
	1	-1
	2	0	6503	1	26598
[/title collection info]
[title collection info]
	` + "`specific`" + `	160
	0	0	6532	1	26648
	1	0	6533	1	26649
[/title collection info]
`
	mapping := parseTitleBookEtc(text)
	if len(mapping) != 4 {
		t.Fatalf("mapping count = %d, want 4: %+v", len(mapping), mapping)
	}
	if got := mapping[6503]; got.category != 0 || got.bookIndex != 2 || got.titleItemID != 26598 {
		t.Fatalf("general mapping = %+v", got)
	}
	if got := mapping[6533]; got.category != 1 || got.bookIndex != 1 || got.titleItemID != 26649 || got.rewardCount != 1 {
		t.Fatalf("specific mapping = %+v", got)
	}
}

func TestBuildCurrentTitleBookListBodyIncludesRawLengthPerRow(t *testing.T) {
	body := buildCurrentTitleBookListBody(19, 1, []currentTitleBookEntry{{
		SlotIndex: 7,
		ItemID:    26649,
		Value:     1,
	}})
	if len(body) != 11+22+4 {
		t.Fatalf("body len = %d, want 37: %x", len(body), body)
	}
	if binary.LittleEndian.Uint32(body[7:11]) != 1 ||
		binary.LittleEndian.Uint16(body[11:13]) != 7 ||
		binary.LittleEndian.Uint32(body[13:17]) != 26649 ||
		binary.LittleEndian.Uint32(body[len(body)-4:]) != 0 {
		t.Fatalf("body mismatch: %x", body)
	}
}

func TestCurrentAchievementTargetsUsesPVFCheckCount(t *testing.T) {
	got := currentAchievementTargets(dnfquest.Definition{CheckCount: []int64{10000}})
	if got != [3]uint16{10000, 0, 0} {
		t.Fatalf("targets = %v", got)
	}
	got = currentAchievementTargets(dnfquest.Definition{
		Type:    "[seeking]",
		IntData: []int64{-7, 5},
	})
	if got != [3]uint16{5, 0, 0} {
		t.Fatalf("seeking targets = %v", got)
	}
}

func TestCurrentInitialTitleAchievementRowsUseOnlyMappedUnpersistedDefinitions(t *testing.T) {
	definitions := []dnfquest.Definition{
		{ID: 6501, Grade: "[title]", CheckCount: []int64{12}},
		{ID: 6502, Grade: "[title]", Type: "[seeking]", IntData: []int64{10158124, 20}},
		{ID: 6503, Grade: "[title]", CheckCount: []int64{30}},
	}
	mapping := map[int32]titleBookMappingEntry{
		6501: {titleItemID: 26596, category: 0, bookIndex: 0},
		6502: {titleItemID: 26597, category: 0, bookIndex: 1},
	}
	seen := map[uint32]struct{}{6501: {}}
	rows := currentInitialTitleAchievementRows(definitions, mapping, seen)
	if len(rows) != 1 || rows[0].achievementID != 6502 || rows[0].p1 != 20 || rows[0].p2 != 0 || rows[0].p3 != 0 {
		t.Fatalf("initial title rows=%+v, want q6502 remaining20", rows)
	}
	if _, ok := seen[6502]; !ok {
		t.Fatal("seeded title row was not marked seen")
	}
}

func TestBuildCurrentInsertOverseerSeedsVisibleTitleAchievementFromPVF(t *testing.T) {
	ctx := context.Background()
	source := bridgePVFSource{
		dnfquest.DefaultList: "6501 `Title/titlebook_test.qst`\n",
		"n_quest/Title/titlebook_test.qst": "[name]\n`Synthetic title`\n" +
			"[grade]\n`[title]`\n[type]\n`[clear quest]`\n[level]\n10 99\n" +
			"[check count]\n12\n",
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	repos := testRepositoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "12",
		Job:         "0",
		Level:       50,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
		questCatalog:       catalog,
		titleBookMappingCache: map[int32]titleBookMappingEntry{
			6501: {titleItemID: 26596, category: 0, bookIndex: 0},
		},
	}
	body := service.buildCurrentInsertOverseerBodyForSession(ctx,
		&gameSession{connID: "title-seed-test", selectedCharacterID: 12},
		buildCurrentInsertOverseerBody())
	if got := binary.LittleEndian.Uint32(body[:4]); got != 1 {
		t.Fatalf("op359 row count=%d body=%x, want one PVF title achievement", got, body)
	}
	row := body[4 : 4+currentInsertOverseerRowWireSize]
	if binary.LittleEndian.Uint32(row[0:4]) != 6501 ||
		binary.LittleEndian.Uint16(row[4:6]) != 12 ||
		binary.LittleEndian.Uint16(row[6:8]) != 0 ||
		binary.LittleEndian.Uint16(row[8:10]) != 0 {
		t.Fatalf("op359 seeded row=%x, want q6501 remaining12", row)
	}
}
