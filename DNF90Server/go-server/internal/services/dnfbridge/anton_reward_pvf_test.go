package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfraid "longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func antonRewardPVFTestDefinitionIDs() []uint32 {
	return antonRewardDefinitionOrder(antonRewardPVFTestConfig())
}

func TestRealScriptPVFLukeSingleRewardPoolsResolveFinalItems(t *testing.T) {
	pvfPath := realLukeRewardPVFPath()
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	config, err := dnfraid.LoadLukeConfig(archive)
	if err != nil {
		t.Fatalf("load Luke raid config: %v", err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatalf("load real PVF item catalog: %v", err)
	}
	catalog, err := newRaidRewardPVFCatalog(items, config.Raid, nil, false, currentLukeRewardPhaseCount)
	if err != nil {
		t.Fatalf("load Luke reward catalog: %v", err)
	}
	singlePhaseIndex := len(config.Raid.Phases)
	for _, kind := range []string{"party_card", "squad_item"} {
		pool, found := catalog.Pool(singlePhaseIndex, kind)
		if !found || len(pool.Entries) == 0 || pool.TotalWeight == 0 {
			t.Fatalf("Luke single %s pool=%+v found=%t", kind, pool, found)
		}
		resolved, resolveErr := catalog.ResolvePool(singlePhaseIndex, kind, &antonRewardPVFSequenceRNG{})
		if resolveErr != nil {
			t.Fatalf("resolve Luke single %s: %v", kind, resolveErr)
		}
		definition, definitionErr := catalog.ResolveFinalItem(resolved.ItemID)
		if definitionErr != nil {
			t.Fatalf("resolve Luke single %s final item %d: %v", kind, resolved.ItemID, definitionErr)
		}
		if definition.ItemID != resolved.ItemID || resolved.Count == 0 || resolved.ItemPVFPath == "" {
			t.Fatalf("Luke single %s resolved=%+v definition=%+v", kind, resolved, definition)
		}
	}
}

func TestRealScriptPVFLukeSingleRewardDefinitionsIncludeDarkRootRows(t *testing.T) {
	pvfPath := strings.TrimSpace(os.Getenv("DNF_LUKE_REWARD_REAL_PVF_SMOKE"))
	if pvfPath == "" {
		t.Skip("set DNF_LUKE_REWARD_REAL_PVF_SMOKE to verify transplanted Luke reward rows")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	for itemID, want := range map[uint32][]int64{
		10302724: {10159143, 100000, 4, 10159143, 100000, 5, 10159143, 100000, 6},
		10302725: {10159143, 100000, 2, 10159143, 100000, 3},
	} {
		documentPath := fmt.Sprintf("stackable/10302001/%d.stk", itemID)
		text, readErr := archive.ReadText(documentPath)
		if readErr != nil {
			t.Fatalf("read Luke single reward %d: %v", itemID, readErr)
		}
		document, parseErr := dnfpvf.Parse(documentPath, text)
		if parseErr != nil {
			t.Fatalf("parse Luke single reward %d: %v", itemID, parseErr)
		}
		if got := document.Ints("int data"); !reflect.DeepEqual(got, want) {
			t.Fatalf("Luke single reward %d int data=%v want=%v", itemID, got, want)
		}
	}

	const darkRootBoosterRow = "1 10159143 36500 5 10159143 33255 6 10159143 30250 7"
	for itemID := uint32(10302726); itemID <= 10302733; itemID++ {
		documentPath := fmt.Sprintf("stackable/10302001/%d.stk", itemID)
		text, readErr := archive.ReadText(documentPath)
		if readErr != nil {
			t.Fatalf("read Luke single reward %d: %v", itemID, readErr)
		}
		normalized := strings.Join(strings.Fields(text), " ")
		if !strings.Contains(normalized, "[booster info]") || !strings.Contains(normalized, darkRootBoosterRow) {
			t.Fatalf("Luke single reward %d is missing the 5-7 dark-root booster row", itemID)
		}
	}
}

func realLukeRewardPVFPath() string {
	if configured := strings.TrimSpace(os.Getenv("DNF_LUKE_REWARD_REAL_PVF_SMOKE")); configured != "" {
		return configured
	}
	return filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
}

func TestAntonRewardPVFCatalogReadsOnlyCurrentDefinitionsAndBuildsPhasePools(t *testing.T) {
	items, source, finalByDefinition := newAntonRewardPVFTestItems(t)
	catalog, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	expectedPaths := antonRewardPVFTestDefinitionPaths(t, items.itemRefs)
	if !reflect.DeepEqual(source.paths, expectedPaths) {
		t.Fatalf("read paths=%v\nwant=%v", source.paths, expectedPaths)
	}
	if got, want := catalog.DefinitionIDs(), antonRewardPVFTestDefinitionIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("definition ids=%v", got)
	}
	if snapshot := catalog.Snapshot(); snapshot != (antonRewardPVFCatalogSnapshot{
		Definitions:       12,
		DefinitionEntries: 13,
		PhasePools:        6,
		PhasePoolEntries:  13,
	}) {
		t.Fatalf("snapshot=%+v", snapshot)
	}

	for index, definitionItemID := range antonRewardPVFTestDefinitionIDs() {
		definition, found := catalog.Definition(definitionItemID)
		if !found || definition.DefinitionItemID != definitionItemID || definition.Kind != dungeonDropItemStackable || definition.PVFPath != expectedPaths[index] || definition.TotalWeight == 0 || len(definition.Entries) == 0 {
			t.Fatalf("definition=%d got=%+v found=%t", definitionItemID, definition, found)
		}
		for _, entry := range definition.Entries {
			if entry.ItemID == definitionItemID {
				t.Fatalf("definition item leaked into its final table: %+v", definition)
			}
		}
		resolved, resolveErr := catalog.ResolveDefinition(definitionItemID, &antonRewardPVFSequenceRNG{})
		if resolveErr != nil {
			t.Fatalf("resolve definition=%d: %v", definitionItemID, resolveErr)
		}
		if resolved.ItemID != finalByDefinition[definitionItemID] || resolved.Count == 0 || resolved.DefinitionItemID != definitionItemID || resolved.ItemPVFPath == "" {
			t.Fatalf("definition=%d resolved=%+v", definitionItemID, resolved)
		}
	}

	phaseOneSquad, found := catalog.Pool(0, " SQUAD_ITEM ")
	if !found || phaseOneSquad.TotalWeight != 100 || !reflect.DeepEqual(phaseOneSquad.Entries, []antonRewardPVFPoolEntry{
		{DefinitionItemID: 10157773, Rank: -1, Weight: 50, Flag: 0},
		{DefinitionItemID: 10157775, Rank: -1, Weight: 1, Flag: 1},
		{DefinitionItemID: 10157776, Rank: -1, Weight: 1, Flag: 1},
		{DefinitionItemID: 10157777, Rank: -1, Weight: 48, Flag: 1},
	}) {
		t.Fatalf("phase one squad=%+v found=%t", phaseOneSquad, found)
	}
	phaseTwoSquad, found := catalog.Pool(1, "squad_item")
	if !found || phaseTwoSquad.TotalWeight != 1000 || !reflect.DeepEqual(phaseTwoSquad.Entries, []antonRewardPVFPoolEntry{
		{DefinitionItemID: 10161936, Rank: -1, Weight: 775, Flag: 0},
		{DefinitionItemID: 10157783, Rank: -1, Weight: 145, Flag: 1},
		{DefinitionItemID: 10157784, Rank: -1, Weight: 50, Flag: 1},
		{DefinitionItemID: 10157785, Rank: -1, Weight: 25, Flag: 1},
		{DefinitionItemID: 10157786, Rank: -1, Weight: 5, Flag: 1},
	}) {
		t.Fatalf("phase two squad=%+v found=%t", phaseTwoSquad, found)
	}
	if party, ok := catalog.Pool(0, "party_card"); !ok || len(party.Entries) != 1 || party.Entries[0].DefinitionItemID != 10094735 {
		t.Fatalf("phase one party=%+v found=%t", party, ok)
	}
	if pcroom, ok := catalog.Pool(1, "pcroom_card"); !ok || len(pcroom.Entries) != 1 || pcroom.Entries[0].DefinitionItemID != 10094733 {
		t.Fatalf("phase two pcroom=%+v found=%t", pcroom, ok)
	}
}

func TestAntonRewardPVFPoolsIncludeSingleModeAfterTeamPhases(t *testing.T) {
	config := antonRewardPVFTestConfig()
	config.SingleMode = &dnfraid.AntonSingleModeConfig{Phase: dnfraid.AntonPhase{Rewards: []dnfraid.AntonStateReward{
		{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10164076},
		{Kind: "squad_item", Rank: -1, Weight: 900, DefinitionItemID: 10164077},
	}}}
	pools, _, err := buildAntonRewardPVFPools(config)
	if err != nil {
		t.Fatal(err)
	}
	if pool, found := pools[antonRewardPVFPoolKey{phaseIndex: len(config.Phases), kind: "party_card"}]; !found || len(pool.Entries) != 1 || pool.Entries[0].DefinitionItemID != 10164076 {
		t.Fatalf("single party pool=%+v found=%t", pool, found)
	}
	if pool, found := pools[antonRewardPVFPoolKey{phaseIndex: len(config.Phases), kind: "squad_item"}]; !found || len(pool.Entries) != 1 || pool.Entries[0].DefinitionItemID != 10164077 {
		t.Fatalf("single squad pool=%+v found=%t", pool, found)
	}
	base := antonRewardPVFTestDefinitionIDs()
	if got := antonRewardDefinitionOrder(config); len(got) != len(base)+2 || got[len(got)-2] != 10164076 || got[len(got)-1] != 10164077 {
		t.Fatalf("single definition order=%v", got)
	}
}

func TestAntonRewardPVFWeightedResolveUsesInjectedRNGAndOneNestedDefinition(t *testing.T) {
	items, source, finalByDefinition := newAntonRewardPVFTestItems(t)
	outerPath := mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)
	nestedPath := mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094733)
	firstFinal := finalByDefinition[10094733]
	secondFinal := uint32(29000001)
	items.itemRefs[secondFinal] = dungeonDropItemReference{kind: dungeonDropItemEquipment, path: "anton_reward/second_nested.equ"}
	source.documents[outerPath] = "[int data]\n10094733 1 2\n[/int data]\n"
	source.documents[nestedPath] = fmt.Sprintf("[int data]\n%d 2 3 %d 3 4\n[/int data]\n", firstFinal, secondFinal)

	catalog, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := catalog.Snapshot(); snapshot.NestedReferences != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	first, err := catalog.ResolveDefinition(10094735, &antonRewardPVFSequenceRNG{values: []uint64{0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if first.DefinitionItemID != 10094735 || first.NestedDefinitionItemID != 10094733 || first.ItemID != firstFinal || first.Count != 6 || first.ItemKind != dungeonDropItemEquipment {
		t.Fatalf("first nested resolve=%+v", first)
	}
	second, err := catalog.ResolveDefinition(10094735, &antonRewardPVFSequenceRNG{values: []uint64{0, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if second.NestedDefinitionItemID != 10094733 || second.ItemID != secondFinal || second.Count != 8 || second.ItemKind != dungeonDropItemEquipment || second.ItemPVFPath != "equipment/anton_reward/second_nested.equ" {
		t.Fatalf("second nested resolve=%+v", second)
	}

	selected, err := catalog.ResolvePool(0, "squad_item", &antonRewardPVFSequenceRNG{values: []uint64{50, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if selected.DefinitionItemID != 10157775 || selected.PoolWeight != 1 || selected.Flag != 1 || selected.PhaseIndex != 0 || selected.PoolKind != "squad_item" {
		t.Fatalf("phase pool roll 50=%+v", selected)
	}
	selected, err = catalog.ResolvePool(0, "squad_item", &antonRewardPVFSequenceRNG{values: []uint64{52, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if selected.DefinitionItemID != 10157777 || selected.PoolWeight != 48 {
		t.Fatalf("phase pool roll 52=%+v", selected)
	}
}

func TestRaidRewardPVFDirectBoosterDefinitionGrantsTheBoxItself(t *testing.T) {
	items, source, _ := newAntonRewardPVFTestItems(t)
	definitionItemID := antonRewardPVFTestDefinitionIDs()[0]
	definitionPath := mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, definitionItemID)
	// Luke's current reward definitions are booster boxes with one or more
	// [etc] groups. Those rows describe what opening the box may contain; the
	// Raid reward itself is the box and must not pre-open that second RNG layer.
	source.documents[definitionPath] = "[booster]\n[booster info]\n[etc]\n1 20000001 1000 3\n[etc]\n1 20000002 1000 1\n"

	catalog, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Definition(definitionItemID)
	if !found || !definition.DirectFinal || len(definition.Entries) != 1 ||
		definition.Entries[0] != (antonRewardPVFEntry{ItemID: definitionItemID, Weight: 1, Count: 1}) {
		t.Fatalf("direct booster definition=%+v found=%t", definition, found)
	}
	resolved, err := catalog.ResolveDefinition(definitionItemID, &antonRewardPVFSequenceRNG{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ItemID != definitionItemID || resolved.Count != 1 || resolved.ItemPVFPath != definition.PVFPath {
		t.Fatalf("direct booster resolved=%+v definition=%+v", resolved, definition)
	}
}

func TestRaidRewardPVFBoosterDefinitionExpandsEveryInfoGroup(t *testing.T) {
	items, source, _ := newAntonRewardPVFTestItems(t)
	const definitionItemID = uint32(10157773)
	documentPath := mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, definitionItemID)
	source.documents[documentPath] = "[stackable type]\n`[booster]` 0\n" +
		"[booster info]\n" +
		"[etc]\n1 20000001 1000 10\n[/etc]\n" +
		"[etc]\n1 20000003 100 1 20000005 100 1\n[/etc]\n" +
		"[/booster info]\n"
	catalog, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Definition(definitionItemID)
	if !found || definition.RewardType != "booster" || !definition.DirectFinal {
		t.Fatalf("booster definition=%+v found=%t", definition, found)
	}
	rewards, err := catalog.ResolveDefinitionRewards(
		definitionItemID,
		&antonRewardPVFSequenceRNG{values: []uint64{0, 100}},
		runtimeRaidMemberState{Job: 0, Level: 90},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewards) != 2 ||
		rewards[0].ItemID != 20000001 || rewards[0].Count != 10 ||
		rewards[1].ItemID != 20000005 || rewards[1].Count != 1 {
		t.Fatalf("expanded rewards=%+v", rewards)
	}

	const legacyDefinitionID = uint32(10157775)
	legacy, err := catalog.ResolveDefinitionRewards(
		legacyDefinitionID,
		&antonRewardPVFSequenceRNG{values: []uint64{0}},
		runtimeRaidMemberState{Job: 0, Level: 90},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0].DefinitionItemID != legacyDefinitionID {
		t.Fatalf("legacy reward=%+v", legacy)
	}
}

func TestRealPVFRaidRewardBooster10160382ExpandsMagicRockAndTarot(t *testing.T) {
	pvfPath := strings.TrimSpace(os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE"))
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify raid booster auto-open")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	config := &dnfraid.AntonConfig{Phases: []dnfraid.AntonPhase{{Rewards: []dnfraid.AntonStateReward{{
		Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10160382, Flag: 0,
	}}}}}
	catalog, err := newRaidRewardPVFCatalog(items, config, nil, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	rewards, err := catalog.ResolveDefinitionRewards(
		10160382,
		&antonRewardPVFSequenceRNG{values: []uint64{0, 0}},
		runtimeRaidMemberState{Job: 0, Level: 90},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewards) != 2 ||
		rewards[0].ItemID != 10159623 || rewards[0].Count != 10 ||
		rewards[1].ItemID < 10159697 || rewards[1].ItemID > 10159718 || rewards[1].Count != 1 {
		t.Fatalf("10160382 expanded rewards=%+v", rewards)
	}
}

func TestAntonRewardPVFResolveFinalItemReadsOnlySelectedDocumentAndCachesMetadata(t *testing.T) {
	items, source, finalByDefinition := newAntonRewardPVFTestItems(t)
	catalog, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	finalItemID := finalByDefinition[10094735]
	reference := items.itemRefs[finalItemID]
	finalPath, err := antonRewardPVFReferencePath(reference)
	if err != nil {
		t.Fatal(err)
	}
	source.documents[finalPath] = "[stackable type]\n`[material]`\n[stack limit]\n999\n"
	source.paths = nil

	first, err := catalog.ResolveFinalItem(finalItemID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.ResolveFinalItem(finalItemID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.ItemID != finalItemID || first.Kind != dungeonDropItemStackable || first.PVFPath != finalPath || first.StackableType != "[material]" || first.StackLimit != 999 || first.SlotStart != 121 || first.SlotEnd != 176 {
		t.Fatalf("final metadata first=%+v second=%+v", first, second)
	}
	if !reflect.DeepEqual(source.paths, []string{finalPath}) {
		t.Fatalf("final item reads=%v want exact cached path %q", source.paths, finalPath)
	}
	if _, err := catalog.ResolveFinalItem(10094735); !errors.Is(err, errAntonRewardPVFNestingInvalid) {
		t.Fatalf("definition-as-final error=%v", err)
	}
}

func TestAntonRewardPVFCatalogRejectsMalformedDefinitionTablesAndUnresolvedFinals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *pvfDungeonDropCatalog, *antonRewardPVFRecordingSource)
		want   error
	}{
		{
			name: "missing int data",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[name]\n`missing`\n"
			},
			want: errAntonRewardPVFSectionShape,
		},
		{
			name: "duplicate int data",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				final := uint32(20000001)
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = fmt.Sprintf("[int data]\n%d 1 1\n[/int data]\n[int data]\n%d 1 1\n[/int data]\n", final, final)
			},
			want: errAntonRewardPVFSectionShape,
		},
		{
			name: "unclosed int data",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n20000001 1 1\n"
			},
			want: errAntonRewardPVFSectionShape,
		},
		{
			name: "partial triple",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n20000001 1\n[/int data]\n"
			},
			want: errAntonRewardPVFSectionShape,
		},
		{
			name: "non integer token",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n20000001 1 `one`\n[/int data]\n"
			},
			want: errAntonRewardPVFSectionShape,
		},
		{
			name: "zero item",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n0 1 1\n[/int data]\n"
			},
			want: errAntonRewardPVFValueInvalid,
		},
		{
			name: "zero weight",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n20000001 0 1\n[/int data]\n"
			},
			want: errAntonRewardPVFValueInvalid,
		},
		{
			name: "zero count",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n20000001 1 0\n[/int data]\n"
			},
			want: errAntonRewardPVFValueInvalid,
		},
		{
			name: "unresolved final item",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n29999999 1 1\n[/int data]\n"
			},
			want: errAntonRewardPVFFinalItemUnresolved,
		},
		{
			name: "self reference",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n10094735 1 1\n[/int data]\n"
			},
			want: errAntonRewardPVFNestingInvalid,
		},
		{
			name: "more than one nested definition",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = "[int data]\n10094733 1 1\n[/int data]\n"
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094733)] = "[int data]\n10157773 1 1\n[/int data]\n"
			},
			want: errAntonRewardPVFNestingInvalid,
		},
		{
			name: "nested count overflow",
			mutate: func(t *testing.T, items *pvfDungeonDropCatalog, source *antonRewardPVFRecordingSource) {
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094735)] = fmt.Sprintf("[int data]\n10094733 1 %d\n[/int data]\n", uint64(math.MaxUint32))
				final := uint32(20000002)
				source.documents[mustAntonRewardPVFTestDefinitionPath(t, items.itemRefs, 10094733)] = fmt.Sprintf("[int data]\n%d 1 2\n[/int data]\n", final)
			},
			want: errAntonRewardPVFValueInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			items, source, _ := newAntonRewardPVFTestItems(t)
			test.mutate(t, items, source)
			_, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestAntonRewardPVFCatalogUsesPVFDynamicDefinitionsAndRejectsInvalidValues(t *testing.T) {
	if _, err := newAntonRewardPVFCatalog(nil, antonRewardPVFTestConfig()); !errors.Is(err, errAntonRewardPVFCatalogRequired) {
		t.Fatalf("nil catalog error=%v", err)
	}
	items, _, _ := newAntonRewardPVFTestItems(t)
	if _, err := newAntonRewardPVFCatalog(items, nil); !errors.Is(err, errAntonRewardPVFConfigRequired) {
		t.Fatalf("nil config error=%v", err)
	}

	t.Run("removed definition is no longer required", func(t *testing.T) {
		config := antonRewardPVFTestConfig()
		config.Phases[1].Rewards = config.Phases[1].Rewards[:len(config.Phases[1].Rewards)-1]
		catalog, err := newAntonRewardPVFCatalog(items, config)
		if err != nil {
			t.Fatalf("removed reward definition still rejected: %v", err)
		}
		if slices.Contains(catalog.DefinitionIDs(), 10157786) {
			t.Fatal("removed reward definition is still loaded")
		}
	})
	t.Run("PVF supplied definition is accepted", func(t *testing.T) {
		config := antonRewardPVFTestConfig()
		config.Phases[0].Rewards[0].DefinitionItemID = 10157778
		catalog, err := newAntonRewardPVFCatalog(items, config)
		if err != nil {
			t.Fatalf("PVF supplied definition rejected: %v", err)
		}
		pool, found := catalog.Pool(0, "party_card")
		if !found || len(pool.Entries) != 1 || pool.Entries[0].DefinitionItemID != 10157778 {
			t.Fatalf("PVF supplied pool=%+v found=%t", pool, found)
		}
	})
	t.Run("unknown definition still requires a PVF item", func(t *testing.T) {
		config := antonRewardPVFTestConfig()
		config.Phases[0].Rewards[0].DefinitionItemID = 42
		if _, err := newAntonRewardPVFCatalog(items, config); !errors.Is(err, errAntonRewardPVFDefinitionMissing) {
			t.Fatalf("unknown definition error=%v", err)
		}
	})
	t.Run("nonpositive outer weight", func(t *testing.T) {
		config := antonRewardPVFTestConfig()
		config.Phases[0].Rewards[0].Weight = 0
		if _, err := newAntonRewardPVFCatalog(items, config); !errors.Is(err, errAntonRewardPVFValueInvalid) {
			t.Fatalf("error=%v", err)
		}
	})

	catalog, err := newAntonRewardPVFCatalog(items, antonRewardPVFTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ResolveDefinition(10094735, nil); !errors.Is(err, errAntonRewardPVFRNGRequired) {
		t.Fatalf("nil definition RNG error=%v", err)
	}
	if _, err := catalog.ResolveDefinition(42, &antonRewardPVFSequenceRNG{}); !errors.Is(err, errAntonRewardPVFDefinitionMissing) {
		t.Fatalf("unknown definition error=%v", err)
	}
	if _, err := catalog.ResolvePool(3, "squad_item", &antonRewardPVFSequenceRNG{}); !errors.Is(err, errAntonRewardPVFPoolUnresolved) {
		t.Fatalf("unknown pool error=%v", err)
	}
	if _, err := catalog.ResolvePool(0, "squad_item", nil); !errors.Is(err, errAntonRewardPVFRNGRequired) {
		t.Fatalf("nil pool RNG error=%v", err)
	}
}

func TestRealScriptPVFAntonRewardCatalogReadsOnlyCurrentDefinitionDocuments(t *testing.T) {
	pvfPath := os.Getenv("DNF_ANTON_REWARD_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_ANTON_REWARD_REAL_PVF_SMOKE to verify current Anton reward definitions")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := dnfraid.LoadAntonConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	itemRefs := antonRewardPVFRealItemRefs(t, archive)
	expectedPaths := make([]string, 0, len(antonRewardDefinitionOrder(config)))
	for _, definitionItemID := range antonRewardDefinitionOrder(config) {
		expectedPaths = append(expectedPaths, mustAntonRewardPVFTestDefinitionPath(t, itemRefs, definitionItemID))
	}
	allowed := make(map[string]struct{}, len(expectedPaths))
	for _, documentPath := range expectedPaths {
		allowed[documentPath] = struct{}{}
	}
	source := &antonRewardPVFRecordingSource{delegate: archive, allowed: allowed}
	items := &pvfDungeonDropCatalog{source: source, itemRefs: itemRefs}
	catalog, err := newAntonRewardPVFCatalog(items, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source.paths, expectedPaths) {
		t.Fatalf("real definition reads=%v\nwant=%v", source.paths, expectedPaths)
	}
	definitionOrder := antonRewardDefinitionOrder(config)
	expectedPhasePoolEntries := 0
	for _, phase := range config.Phases {
		expectedPhasePoolEntries += len(phase.Rewards)
	}
	if config.SingleMode != nil {
		expectedPhasePoolEntries += len(config.SingleMode.Phase.Rewards)
	}
	if snapshot := catalog.Snapshot(); snapshot.Definitions != len(definitionOrder) ||
		snapshot.DefinitionEntries == 0 || snapshot.PhasePools == 0 ||
		snapshot.PhasePoolEntries != expectedPhasePoolEntries {
		t.Fatalf("real snapshot=%+v", snapshot)
	}
	for _, definitionItemID := range antonRewardDefinitionOrder(config) {
		definition, found := catalog.Definition(definitionItemID)
		if !found || len(definition.Entries) == 0 || definition.TotalWeight == 0 {
			t.Fatalf("real definition=%d got=%+v found=%t", definitionItemID, definition, found)
		}
		for entryIndex, entry := range definition.Entries {
			if _, isDefinition := catalog.definitions[entry.ItemID]; isDefinition {
				t.Fatalf("real definition=%d entry=%d points to definition=%d", definitionItemID, entryIndex, entry.ItemID)
			}
			if _, found := itemRefs[entry.ItemID]; !found {
				t.Fatalf("real definition=%d entry=%d final item=%d is not indexed", definitionItemID, entryIndex, entry.ItemID)
			}
		}
		t.Logf("anton reward definition=%d path=%s entries=%d total_weight=%d", definitionItemID, definition.PVFPath, len(definition.Entries), definition.TotalWeight)
	}
}

type antonRewardPVFRecordingSource struct {
	documents map[string]string
	delegate  dnfpvf.Source
	allowed   map[string]struct{}
	paths     []string
}

func (source *antonRewardPVFRecordingSource) ReadText(documentPath string) (string, error) {
	source.paths = append(source.paths, documentPath)
	if source.allowed != nil {
		if _, allowed := source.allowed[documentPath]; !allowed {
			return "", fmt.Errorf("unexpected Anton reward PVF read %q", documentPath)
		}
	}
	if source.documents != nil {
		text, found := source.documents[documentPath]
		if !found {
			return "", fmt.Errorf("Anton reward PVF test document missing %q", documentPath)
		}
		return text, nil
	}
	if source.delegate == nil {
		return "", fmt.Errorf("Anton reward PVF source missing %q", documentPath)
	}
	return source.delegate.ReadText(documentPath)
}

type antonRewardPVFSequenceRNG struct {
	values []uint64
	index  int
}

func (rng *antonRewardPVFSequenceRNG) Uint64() uint64 {
	if rng == nil || len(rng.values) == 0 {
		return 0
	}
	if rng.index >= len(rng.values) {
		return rng.values[len(rng.values)-1]
	}
	value := rng.values[rng.index]
	rng.index++
	return value
}

func newAntonRewardPVFTestItems(t *testing.T) (*pvfDungeonDropCatalog, *antonRewardPVFRecordingSource, map[uint32]uint32) {
	t.Helper()
	source := &antonRewardPVFRecordingSource{documents: make(map[string]string)}
	definitionIDs := antonRewardPVFTestDefinitionIDs()
	itemRefs := make(map[uint32]dungeonDropItemReference, len(definitionIDs)*2+1)
	finalByDefinition := make(map[uint32]uint32, len(definitionIDs))
	for index, definitionItemID := range definitionIDs {
		itemRefs[definitionItemID] = dungeonDropItemReference{
			kind: dungeonDropItemStackable,
			path: fmt.Sprintf("anton_reward/%d.stk", definitionItemID),
		}
		finalItemID := uint32(20000001 + index)
		finalByDefinition[definitionItemID] = finalItemID
		kind := dungeonDropItemStackable
		extension := "stk"
		if index%2 == 1 {
			kind = dungeonDropItemEquipment
			extension = "equ"
		}
		itemRefs[finalItemID] = dungeonDropItemReference{
			kind: kind,
			path: fmt.Sprintf("anton_reward/final_%d.%s", finalItemID, extension),
		}
	}
	items := &pvfDungeonDropCatalog{source: source, itemRefs: itemRefs}
	for index, definitionItemID := range definitionIDs {
		documentPath := mustAntonRewardPVFTestDefinitionPath(t, itemRefs, definitionItemID)
		finalItemID := finalByDefinition[definitionItemID]
		if index == 0 {
			extraFinal := uint32(29000000)
			itemRefs[extraFinal] = dungeonDropItemReference{kind: dungeonDropItemStackable, path: "anton_reward/extra_final.stk"}
			source.documents[documentPath] = fmt.Sprintf("[int data]\n%d 2 5 %d 3 7\n[/int data]\n", finalItemID, extraFinal)
			continue
		}
		source.documents[documentPath] = fmt.Sprintf("[int data]\n%d %d %d\n[/int data]\n", finalItemID, index+1, index+1)
	}
	return items, source, finalByDefinition
}

func antonRewardPVFTestConfig() *dnfraid.AntonConfig {
	return &dnfraid.AntonConfig{Phases: []dnfraid.AntonPhase{
		{Rewards: []dnfraid.AntonStateReward{
			{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10094735, Flag: 0},
			{Kind: "pcroom_card", Rank: -1, Weight: 100, DefinitionItemID: 10094733, Flag: 0},
			{Kind: "squad_item", Rank: -1, Weight: 50, DefinitionItemID: 10157773, Flag: 0},
			{Kind: "squad_item", Rank: -1, Weight: 1, DefinitionItemID: 10157775, Flag: 1},
			{Kind: "squad_item", Rank: -1, Weight: 1, DefinitionItemID: 10157776, Flag: 1},
			{Kind: "squad_item", Rank: -1, Weight: 48, DefinitionItemID: 10157777, Flag: 1},
		}},
		{Rewards: []dnfraid.AntonStateReward{
			{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10157778, Flag: 0},
			{Kind: "pcroom_card", Rank: -1, Weight: 100, DefinitionItemID: 10094733, Flag: 0},
			{Kind: "squad_item", Rank: -1, Weight: 775, DefinitionItemID: 10161936, Flag: 0},
			{Kind: "squad_item", Rank: -1, Weight: 145, DefinitionItemID: 10157783, Flag: 1},
			{Kind: "squad_item", Rank: -1, Weight: 50, DefinitionItemID: 10157784, Flag: 1},
			{Kind: "squad_item", Rank: -1, Weight: 25, DefinitionItemID: 10157785, Flag: 1},
			{Kind: "squad_item", Rank: -1, Weight: 5, DefinitionItemID: 10157786, Flag: 1},
		}},
	}}
}

func antonRewardPVFTestDefinitionPaths(t *testing.T, itemRefs map[uint32]dungeonDropItemReference) []string {
	t.Helper()
	definitionIDs := antonRewardPVFTestDefinitionIDs()
	paths := make([]string, 0, len(definitionIDs))
	for _, definitionItemID := range definitionIDs {
		paths = append(paths, mustAntonRewardPVFTestDefinitionPath(t, itemRefs, definitionItemID))
	}
	return paths
}

func mustAntonRewardPVFTestDefinitionPath(t *testing.T, itemRefs map[uint32]dungeonDropItemReference, definitionItemID uint32) string {
	t.Helper()
	reference, found := itemRefs[definitionItemID]
	if !found {
		t.Fatalf("definition item %d is not indexed", definitionItemID)
	}
	documentPath, err := antonRewardPVFReferencePath(reference)
	if err != nil {
		t.Fatalf("definition item %d path: %v", definitionItemID, err)
	}
	return documentPath
}

func antonRewardPVFRealItemRefs(t *testing.T, source dnfpvf.Source) map[uint32]dungeonDropItemReference {
	t.Helper()
	stackablePaths, _, err := loadDungeonDropList(source, dungeonDropStackableList)
	if err != nil {
		t.Fatal(err)
	}
	equipmentPaths, _, err := loadDungeonDropList(source, dungeonDropEquipmentList)
	if err != nil {
		t.Fatal(err)
	}
	itemRefs := make(map[uint32]dungeonDropItemReference, len(stackablePaths)+len(equipmentPaths))
	for itemID, itemPath := range stackablePaths {
		if itemID > 0 && itemID <= math.MaxUint32 {
			itemRefs[uint32(itemID)] = dungeonDropItemReference{kind: dungeonDropItemStackable, path: itemPath}
		}
	}
	for itemID, itemPath := range equipmentPaths {
		if itemID <= 0 || itemID > math.MaxUint32 {
			continue
		}
		if _, found := itemRefs[uint32(itemID)]; !found {
			itemRefs[uint32(itemID)] = dungeonDropItemReference{kind: dungeonDropItemEquipment, path: itemPath}
		}
	}
	return itemRefs
}
