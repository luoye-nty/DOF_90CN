package raid

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestLoadAntonConfigReadsOnlyTargetAndPreservesSourceOrder(t *testing.T) {
	source := &antonRecordingSource{text: antonTestDocument()}
	config, err := LoadAntonConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source.paths, []string{AntonPVFPath}) {
		t.Fatalf("read paths=%v want only %q", source.paths, AntonPVFPath)
	}

	want := &AntonConfig{
		PhaseMax:                  2,
		MemberMax:                 20,
		StartMinimumMembers:       10,
		SingleStartMinimumMembers: 1,
		StartDelaySeconds:         3,
		DisconnMemberWaitTime:     300,
		PhaseTimeoutSeconds:       []int{2400, 2400},
		PhaseBreakSeconds:         600,
		MovieEndSeconds:           []int{20, 21, 22, 23},
		EntryCost:                 AntonEntryCost{ItemID: 10096296, Count: 1},
		WeeklyEnterCount:          [2]int{30, 3},
		Phases: []AntonPhase{
			{
				Name: "phase one", MinimalDungeonClearCount: 1,
				Rewards: []AntonStateReward{
					{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10094735, Flag: 0},
					{Kind: "squad_item", Rank: -1, Weight: 50, DefinitionItemID: 10157773, Flag: 1},
				},
				Dungeons: []AntonDungeonInfo{
					{Index: 210, InitState: AntonDungeonOpen, HP: 4, PartyMax: 5},
					{Index: 211, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				},
				RankConditions: antonTestRankConditions(),
			},
			{
				Name: "phase two", MinimalDungeonClearCount: 1,
				Rewards: []AntonStateReward{
					{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10157778, Flag: 0},
					{Kind: "squad_item", Rank: -1, Weight: 775, DefinitionItemID: 10161936, Flag: 1},
				},
				Dungeons: []AntonDungeonInfo{
					{Index: 218, InitState: AntonDungeonOpen, HP: 1, PartyMax: 5},
					{Index: 220, InitState: AntonDungeonHide, HP: 5, PartyMax: 5},
				},
				RankConditions: antonTestRankConditions(),
			},
		},
		SingleMode: &AntonSingleModeConfig{
			Name: "single phase", TimeLimitSeconds: 2100,
			Phase: AntonPhase{
				Name: "single phase", MinimalDungeonClearCount: 1,
				Rewards:        []AntonStateReward{{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10094735, Flag: 0}},
				Dungeons:       []AntonDungeonInfo{{Index: 9999, InitState: AntonDungeonOpen, HP: 1, PartyMax: 0}},
				RankConditions: antonTestRankConditions(),
			},
		},
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("config=%+v\nwant=%+v", config, want)
	}
}

func TestAntonConfigModeChoicesExposeBothModesWithoutSharingSlices(t *testing.T) {
	config, err := LoadAntonConfig(&antonRecordingSource{text: antonTestDocument()})
	if err != nil {
		t.Fatal(err)
	}
	want := []AntonRaidModeChoice{
		{
			Kind: AntonRaidModeTeam, PhaseNames: []string{"phase one", "phase two"},
			StartMinimumMembers: 10, TimeLimitSeconds: []int{2400, 2400},
		},
		{
			Kind: AntonRaidModeSingle, PhaseNames: []string{"single phase"},
			StartMinimumMembers: 1, TimeLimitSeconds: []int{2100},
		},
	}
	choices := config.ModeChoices()
	if !reflect.DeepEqual(choices, want) {
		t.Fatalf("choices=%+v\nwant=%+v", choices, want)
	}
	choices[0].PhaseNames[0] = "mutated"
	choices[0].TimeLimitSeconds[0] = 1
	choices[1].PhaseNames[0] = "mutated"
	choices[1].TimeLimitSeconds[0] = 1
	if !reflect.DeepEqual(config.ModeChoices(), want) {
		t.Fatalf("mode choices share mutable slices with caller or config: %+v", config.ModeChoices())
	}
	var nilConfig *AntonConfig
	if choices := nilConfig.ModeChoices(); choices != nil {
		t.Fatalf("nil config choices=%+v want nil", choices)
	}
}

func TestLoadAntonConfigRejectsNilSourceAndPropagatesTargetReadFailure(t *testing.T) {
	if _, err := LoadAntonConfig(nil); !errors.Is(err, ErrAntonPVFSourceRequired) {
		t.Fatalf("nil source error=%v", err)
	}
	source := &antonRecordingSource{err: os.ErrNotExist}
	if _, err := LoadAntonConfig(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read error=%v", err)
	}
	if !reflect.DeepEqual(source.paths, []string{AntonPVFPath}) {
		t.Fatalf("failed read paths=%v", source.paths)
	}
}

func TestParseAntonConfigRejectsWrongDocumentPath(t *testing.T) {
	document, err := dnfpvf.Parse("etc/raid/luke.etc", antonTestDocument())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAntonConfig(document); !errors.Is(err, ErrAntonPVFDocumentInvalid) {
		t.Fatalf("wrong path error=%v", err)
	}
	if _, err := ParseAntonConfig(nil); !errors.Is(err, ErrAntonPVFDocumentInvalid) {
		t.Fatalf("nil document error=%v", err)
	}
}

func TestAntonConfigRejectsMissingDuplicateAndInvalidCurrentFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
		want   error
	}{
		{
			name: "missing global field",
			mutate: func(text string) string {
				return strings.Replace(text, "[PHASE BREAK TIME]\n600\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "duplicate global field",
			mutate: func(text string) string {
				return strings.Replace(text, "[PHASE MAX]\n2", "[PHASE MAX]\n2\n[PHASE MAX]\n2", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "unsupported ordinary phase max",
			mutate: func(text string) string {
				return strings.Replace(text, "[PHASE MAX]\n2", "[PHASE MAX]\n3", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "member max exceeds protocol roster",
			mutate: func(text string) string {
				return strings.Replace(text, "[RAID MEMBER MAX]\n20", "[RAID MEMBER MAX]\n21", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "minimum members exceeds member max",
			mutate: func(text string) string {
				return strings.Replace(text, "[RAID START MINIMUM MEMBER]\n10", "[RAID START MINIMUM MEMBER]\n21", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "phase timeout row is partial",
			mutate: func(text string) string {
				return strings.Replace(text, "[PHASE TIME OVER]\n2400 2400", "[PHASE TIME OVER]\n2400", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "movie end value is nonpositive",
			mutate: func(text string) string {
				return strings.Replace(text, "[MOVIE END TIME]\n20 21 22 23", "[MOVIE END TIME]\n20 21 0 23", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "entry cost block is unclosed",
			mutate: func(text string) string {
				return strings.Replace(text, "[/ENTRY COST]\n", "", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "missing weekly enter count",
			mutate: func(text string) string {
				return strings.Replace(text, "[WEEKLY ENTER COUNT]\n30 3\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "weekly enter count row is partial",
			mutate: func(text string) string {
				return strings.Replace(text, "[WEEKLY ENTER COUNT]\n30 3", "[WEEKLY ENTER COUNT]\n30", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "weekly enter count value is nonpositive",
			mutate: func(text string) string {
				return strings.Replace(text, "[WEEKLY ENTER COUNT]\n30 3", "[WEEKLY ENTER COUNT]\n30 0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "missing phase rank table",
			mutate: func(text string) string {
				return strings.Replace(text, "[CONDITION FOR RANK]\n[DEAD COUNT]\n0 1 0\n[DEAD COUNT]\n2 5 1\n[/CONDITION FOR RANK]\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "rank table carries a foreign section",
			mutate: func(text string) string {
				return strings.Replace(text, "[DEAD COUNT]\n0 1 0", "[COIN COUNT]\n1", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "dead count row is partial",
			mutate: func(text string) string {
				return strings.Replace(text, "[DEAD COUNT]\n0 1 0", "[DEAD COUNT]\n0 1", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "dead count range is reversed",
			mutate: func(text string) string {
				return strings.Replace(text, "[DEAD COUNT]\n2 5 1", "[DEAD COUNT]\n5 2 1", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "phase coin limit is nonpositive",
			mutate: func(text string) string {
				return strings.Replace(text, "[MINIMAL DUNGEON CLEAR COUNT]\n1", "[PHASE COIN LIMIT]\n0\n[MINIMAL DUNGEON CLEAR COUNT]\n1", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "lockout type row is partial",
			mutate: func(text string) string {
				return strings.Replace(text, "[210 DGN HP]\n4\n[PARTY MAX]\n5", "[210 DGN HP]\n4\n[PARTY MAX]\n5\n[LOCKOUT TYPE]\n4", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "lockout type value is negative",
			mutate: func(text string) string {
				return strings.Replace(text, "[210 DGN HP]\n4\n[PARTY MAX]\n5", "[210 DGN HP]\n4\n[PARTY MAX]\n5\n[LOCKOUT TYPE]\n-1 0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "destroy object info precedes party max",
			mutate: func(text string) string {
				return strings.Replace(text, "[210 DGN HP]\n4\n[PARTY MAX]\n5", "[210 DGN HP]\n4\n[DESTROY OBJECT INFO]\n[PARTY MAX]\n5", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "destroy object info declares no group",
			mutate: func(text string) string {
				return strings.Replace(text, "[210 DGN HP]\n4\n[PARTY MAX]\n5", "[210 DGN HP]\n4\n[PARTY MAX]\n5\n[DESTROY OBJECT INFO]", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "destroy object group carries values",
			mutate: func(text string) string {
				return strings.Replace(text, "[210 DGN HP]\n4\n[PARTY MAX]\n5",
					"[210 DGN HP]\n4\n[PARTY MAX]\n5\n[DESTROY OBJECT INFO]\n[BIRTH SANCTUM OBJECT]\n1", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "entry cost count is invalid",
			mutate: func(text string) string {
				return strings.Replace(text, "10096296 1", "10096296 0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "ordinary phase missing",
			mutate: func(text string) string {
				start := strings.Index(text, "[PHASE]\n[PHASE NAME]\n`phase two`")
				end := strings.Index(text[start:], "[/PHASE]") + start + len("[/PHASE]\n")
				return text[:start] + text[end:]
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "ordinary phase is unclosed",
			mutate: func(text string) string {
				return strings.Replace(text, "[/PHASE]\n[PHASE]\n[PHASE NAME]\n`phase two`", "[PHASE]\n[PHASE NAME]\n`phase two`", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "phase name missing",
			mutate: func(text string) string {
				return strings.Replace(text, "[PHASE NAME]\n`phase one`\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "reward block missing close",
			mutate: func(text string) string {
				return strings.Replace(text, "[/CLEAR REWARD ITEM]\n[MINIMAL DUNGEON CLEAR COUNT]", "[MINIMAL DUNGEON CLEAR COUNT]", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "reward row is partial",
			mutate: func(text string) string {
				return strings.Replace(text, "`party_card` -1 100 10094735 0", "`party_card` -1 100 10094735", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "reward rank below native sentinel",
			mutate: func(text string) string {
				return strings.Replace(text, "`party_card` -1 100 10094735 0", "`party_card` -2 100 10094735 0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "reward weight is zero",
			mutate: func(text string) string {
				return strings.Replace(text, "`party_card` -1 100 10094735 0", "`party_card` -1 0 10094735 0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "reward flag is outside boolean domain",
			mutate: func(text string) string {
				return strings.Replace(text, "`party_card` -1 100 10094735 0", "`party_card` -1 100 10094735 2", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "minimum clear count exceeds dungeon count",
			mutate: func(text string) string {
				return strings.Replace(text, "[MINIMAL DUNGEON CLEAR COUNT]\n1", "[MINIMAL DUNGEON CLEAR COUNT]\n3", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "dungeon repeats across phases",
			mutate: func(text string) string {
				text = strings.Replace(text, "[INDEX]\n218", "[INDEX]\n210", 1)
				return strings.Replace(text, "[218 DGN HP]", "[210 DGN HP]", 1)
			},
			want: ErrAntonPVFDuplicateDungeon,
		},
		{
			name: "dungeon index is zero",
			mutate: func(text string) string {
				text = strings.Replace(text, "[INDEX]\n210", "[INDEX]\n0", 1)
				return strings.Replace(text, "[210 DGN HP]", "[0 DGN HP]", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "dungeon state is unknown",
			mutate: func(text string) string {
				return strings.Replace(text, "[INIT STATE]\n`open`", "[INIT STATE]\n`ready`", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "dungeon hp is zero",
			mutate: func(text string) string {
				return strings.Replace(text, "[210 DGN HP]\n4", "[210 DGN HP]\n0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "dungeon party max exceeds raid member max",
			mutate: func(text string) string {
				return strings.Replace(text, "[PARTY MAX]\n5", "[PARTY MAX]\n21", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "dungeon field is duplicated",
			mutate: func(text string) string {
				return strings.Replace(text, "[INDEX]\n210", "[INDEX]\n210\n[INDEX]\n210", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "dungeon fields are out of order",
			mutate: func(text string) string {
				return strings.Replace(text, "[HP INFO]\n[210 DGN HP]\n4\n[PARTY MAX]\n5", "[PARTY MAX]\n5\n[HP INFO]\n[210 DGN HP]\n4", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "dungeon block has unsupported field",
			mutate: func(text string) string {
				return strings.Replace(text, "[PARTY MAX]\n5\n[/DUNGEON INFO]", "[PARTY MAX]\n5\n[UNKNOWN]\n1\n[/DUNGEON INFO]", 1)
			},
			want: ErrAntonPVFSectionShape,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &antonRecordingSource{text: test.mutate(antonTestDocument())}
			_, err := LoadAntonConfig(source)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want errors.Is(%v)", err, test.want)
			}
			if !reflect.DeepEqual(source.paths, []string{AntonPVFPath}) {
				t.Fatalf("read paths=%v", source.paths)
			}
		})
	}
}

func TestAntonConfigSingleModeValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
		want   error
	}{
		{
			name: "missing single minimum members",
			mutate: func(text string) string {
				return strings.Replace(text, "[SINGLE RAID START MINIUM MEMBER]\n1\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "missing single timeout",
			mutate: func(text string) string {
				return strings.Replace(text, "[SINGLE RAID TIME OVER]\n2100\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
		{
			name: "nonpositive single timeout",
			mutate: func(text string) string {
				return strings.Replace(text, "[SINGLE RAID TIME OVER]\n2100", "[SINGLE RAID TIME OVER]\n0", 1)
			},
			want: ErrAntonPVFValueInvalid,
		},
		{
			name: "duplicate single phase",
			mutate: func(text string) string {
				start := strings.Index(text, "[SINGLE RAID PHASE]\n")
				end := strings.Index(text[start:], "[/SINGLE RAID PHASE]") + start + len("[/SINGLE RAID PHASE]\n")
				return text + text[start:end]
			},
			want: ErrAntonPVFSectionShape,
		},
		{
			name: "single phase name missing",
			mutate: func(text string) string {
				return strings.Replace(text, "[PHASE NAME]\n`single phase`\n", "", 1)
			},
			want: ErrAntonPVFSectionMissing,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadAntonConfig(&antonRecordingSource{text: test.mutate(antonTestDocument())})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestAntonConfigWithoutSinglePhaseKeepsOrdinaryTeamMode(t *testing.T) {
	text := antonTestDocument()
	start := strings.Index(text, "[SINGLE RAID TIME OVER]\n")
	end := strings.Index(text, "[/SINGLE RAID PHASE]") + len("[/SINGLE RAID PHASE]\n")
	text = text[:start] + text[end:]
	text = strings.Replace(text, "[SINGLE RAID START MINIUM MEMBER]\n1\n", "", 1)
	config, err := LoadAntonConfig(&antonRecordingSource{text: text})
	if err != nil {
		t.Fatal(err)
	}
	if config.SingleMode != nil {
		t.Fatalf("single config=%+v", config)
	}
	if config.SingleStartMinimumMembers != 0 {
		t.Fatalf("single minimum members=%d want 0", config.SingleStartMinimumMembers)
	}
	choices := config.ModeChoices()
	if len(choices) != 1 || choices[0].Kind != AntonRaidModeTeam {
		t.Fatalf("choices=%+v want team-only", choices)
	}
}

func TestRealScriptPVFAntonConfigMatchesCurrentTwoPhaseCatalog(t *testing.T) {
	pvfPath := os.Getenv("DNF_ANTON_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_ANTON_REAL_PVF_SMOKE to verify etc/raid/anton.etc")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadAntonConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	if config.PhaseMax != 2 || config.MemberMax != 20 || config.StartMinimumMembers != 10 ||
		config.SingleStartMinimumMembers != 1 || config.StartDelaySeconds != 3 ||
		// [DISCONN MEMBER WAIT TIME] is the drop grace the raid runtime reads
		// (dnfbridge runtimeRaidDisconnectGraceForState). Pinned here because the
		// runtime used to carry the same 300 as a bare constant: if this ever
		// stops being 300, the grace silently changes with it.
		config.DisconnMemberWaitTime != 300 ||
		config.PhaseBreakSeconds != 600 || config.EntryCost != (AntonEntryCost{ItemID: 10096296, Count: 1}) ||
		!reflect.DeepEqual(config.PhaseTimeoutSeconds, []int{2400, 2400}) ||
		!reflect.DeepEqual(config.MovieEndSeconds, []int{20, 20, 20, 20}) ||
		config.WeeklyEnterCount != [2]int{30, 3} {
		t.Fatalf("real global config=%+v", config)
	}

	// Both ordinary phases and the single-mode phase share one rank table, and
	// no Anton phase declares a revive-coin cap.
	wantRank := []AntonRankCondition{
		{MinimumDeaths: 0, MaximumDeaths: 1, Rank: 0},
		{MinimumDeaths: 2, MaximumDeaths: 5, Rank: 1},
		{MinimumDeaths: 6, MaximumDeaths: 9, Rank: 2},
		{MinimumDeaths: 10, MaximumDeaths: 14, Rank: 3},
	}
	wantPhases := []AntonPhase{
		{
			Name: "\u963b\u622a\u5b89\u5f92\u6069", MinimalDungeonClearCount: 1,
			Rewards: []AntonStateReward{
				{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10094735, Flag: 0},
				{Kind: "pcroom_card", Rank: -1, Weight: 100, DefinitionItemID: 10094733, Flag: 0},
				{Kind: "squad_item", Rank: -1, Weight: 50, DefinitionItemID: 10157773, Flag: 0},
				{Kind: "squad_item", Rank: -1, Weight: 1, DefinitionItemID: 10157775, Flag: 1},
				{Kind: "squad_item", Rank: -1, Weight: 1, DefinitionItemID: 10157776, Flag: 1},
				{Kind: "squad_item", Rank: -1, Weight: 48, DefinitionItemID: 10157777, Flag: 1},
			},
			Dungeons: []AntonDungeonInfo{
				{Index: 210, InitState: AntonDungeonOpen, HP: 4, PartyMax: 5},
				{Index: 211, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 212, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 213, InitState: AntonDungeonHide, HP: 2, PartyMax: 5},
				{Index: 214, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 215, InitState: AntonDungeonHide, HP: 2, PartyMax: 5},
				{Index: 216, InitState: AntonDungeonHide, HP: 1, PartyMax: 1},
			},
			RankConditions: wantRank,
		},
		{
			Name: "\u706d\u6740\u5b89\u5f92\u6069", MinimalDungeonClearCount: 1,
			Rewards: []AntonStateReward{
				{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10157778, Flag: 0},
				{Kind: "pcroom_card", Rank: -1, Weight: 100, DefinitionItemID: 10094733, Flag: 0},
				{Kind: "squad_item", Rank: -1, Weight: 775, DefinitionItemID: 10161936, Flag: 0},
				{Kind: "squad_item", Rank: -1, Weight: 145, DefinitionItemID: 10157783, Flag: 1},
				{Kind: "squad_item", Rank: -1, Weight: 50, DefinitionItemID: 10157784, Flag: 1},
				{Kind: "squad_item", Rank: -1, Weight: 25, DefinitionItemID: 10157785, Flag: 1},
				{Kind: "squad_item", Rank: -1, Weight: 5, DefinitionItemID: 10157786, Flag: 1},
			},
			Dungeons: []AntonDungeonInfo{
				{Index: 218, InitState: AntonDungeonOpen, HP: 1, PartyMax: 5},
				{Index: 219, InitState: AntonDungeonOpen, HP: 1, PartyMax: 5},
				{Index: 220, InitState: AntonDungeonHide, HP: 5, PartyMax: 5},
				{Index: 221, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 222, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 223, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
				{Index: 224, InitState: AntonDungeonHide, HP: 1, PartyMax: 5},
			},
			RankConditions: wantRank,
		},
	}
	if !reflect.DeepEqual(config.Phases, wantPhases) {
		t.Fatalf("real phases=%+v\nwant=%+v", config.Phases, wantPhases)
	}
}

func TestRealScriptPVFAntonConfigExposesNativeModeChoices(t *testing.T) {
	pvfPath := os.Getenv("DNF_ANTON_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_ANTON_REAL_PVF_SMOKE to verify native Anton mode choices")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadAntonConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	if config.SingleMode == nil || config.SingleMode.Name != "\u5355\u4eba\u5b89\u5f92\u6069" || config.SingleMode.TimeLimitSeconds != 2100 {
		t.Fatalf("single mode=%+v", config.SingleMode)
	}
	choices := config.ModeChoices()
	if len(choices) != 2 {
		t.Fatalf("mode choices=%+v want two native modes", choices)
	}
	if choices[0].Kind != AntonRaidModeTeam || choices[0].StartMinimumMembers != 10 ||
		!reflect.DeepEqual(choices[0].PhaseNames, []string{"\u963b\u622a\u5b89\u5f92\u6069", "\u706d\u6740\u5b89\u5f92\u6069"}) ||
		!reflect.DeepEqual(choices[0].TimeLimitSeconds, []int{2400, 2400}) {
		t.Fatalf("team choice=%+v", choices[0])
	}
	if choices[1].Kind != AntonRaidModeSingle || choices[1].StartMinimumMembers != 1 ||
		!reflect.DeepEqual(choices[1].PhaseNames, []string{config.SingleMode.Name}) ||
		!reflect.DeepEqual(choices[1].TimeLimitSeconds, []int{2100}) {
		t.Fatalf("single choice=%+v", choices[1])
	}
}

type antonRecordingSource struct {
	text  string
	err   error
	paths []string
}

func (source *antonRecordingSource) ReadText(path string) (string, error) {
	source.paths = append(source.paths, path)
	if path != AntonPVFPath {
		return "", fmt.Errorf("unexpected path %q", path)
	}
	if source.err != nil {
		return "", source.err
	}
	return source.text, nil
}

// antonTestRankConditions mirrors the [CONDITION FOR RANK] block every phase of
// antonTestDocument declares.
func antonTestRankConditions() []AntonRankCondition {
	return []AntonRankCondition{
		{MinimumDeaths: 0, MaximumDeaths: 1, Rank: 0},
		{MinimumDeaths: 2, MaximumDeaths: 5, Rank: 1},
	}
}

func antonTestDocument() string {
	return `[PHASE MAX]
2
[RAID MEMBER MAX]
20
[RAID START MINIMUM MEMBER]
10
[SINGLE RAID START MINIUM MEMBER]
1
[START DELAY TIME]
3
[PHASE TIME OVER]
2400 2400
[PHASE BREAK TIME]
600
[MOVIE END TIME]
20 21 22 23
[DISCONN MEMBER WAIT TIME]
300
[ENTRY COST]
10096296 1
[/ENTRY COST]
[WEEKLY ENTER COUNT]
30 3
[PHASE]
[PHASE NAME]
` + "`phase one`" + `
[CONDITION FOR RANK]
[DEAD COUNT]
0 1 0
[DEAD COUNT]
2 5 1
[/CONDITION FOR RANK]
[CLEAR REWARD ITEM]
[STATE REWARD]
` + "`party_card`" + ` -1 100 10094735 0
[STATE REWARD]
` + "`squad_item`" + ` -1 50 10157773 1
[/CLEAR REWARD ITEM]
[MINIMAL DUNGEON CLEAR COUNT]
1
[DUNGEON INFO]
[INDEX]
210
[INIT STATE]
` + "`open`" + `
[HP INFO]
[210 DGN HP]
4
[PARTY MAX]
5
[/DUNGEON INFO]
[DUNGEON INFO]
[INDEX]
211
[INIT STATE]
` + "`hide`" + `
[HP INFO]
[211 DGN HP]
1
[PARTY MAX]
5
[/DUNGEON INFO]
[/PHASE]
[PHASE]
[PHASE NAME]
` + "`phase two`" + `
[CONDITION FOR RANK]
[DEAD COUNT]
0 1 0
[DEAD COUNT]
2 5 1
[/CONDITION FOR RANK]
[CLEAR REWARD ITEM]
[STATE REWARD]
` + "`party_card`" + ` -1 100 10157778 0
[STATE REWARD]
` + "`squad_item`" + ` -1 775 10161936 1
[/CLEAR REWARD ITEM]
[MINIMAL DUNGEON CLEAR COUNT]
1
[DUNGEON INFO]
[INDEX]
218
[INIT STATE]
` + "`open`" + `
[HP INFO]
[218 DGN HP]
1
[PARTY MAX]
5
[/DUNGEON INFO]
[DUNGEON INFO]
[INDEX]
220
[INIT STATE]
` + "`hide`" + `
[HP INFO]
[220 DGN HP]
5
[PARTY MAX]
5
[/DUNGEON INFO]
[/PHASE]
[SINGLE RAID TIME OVER]
2100
[SINGLE RAID PHASE]
[PHASE NAME]
` + "`single phase`" + `
[CONDITION FOR RANK]
[DEAD COUNT]
0 1 0
[DEAD COUNT]
2 5 1
[/CONDITION FOR RANK]
[CLEAR REWARD ITEM]
[STATE REWARD]
` + "`party_card`" + ` -1 100 10094735 0
[/CLEAR REWARD ITEM]
[DUNGEON INFO]
[INDEX]
9999
[INIT STATE]
` + "`open`" + `
[HP INFO]
[9999 DGN HP]
1
[PARTY MAX]
0
[/DUNGEON INFO]
[/SINGLE RAID PHASE]
`
}
