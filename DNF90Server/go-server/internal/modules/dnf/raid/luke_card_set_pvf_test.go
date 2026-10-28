package raid

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// realLukeCardSetPVFPath mirrors the reward-catalog smoke switch
// (DNF_LUKE_REWARD_REAL_PVF_SMOKE): an explicit path wins, otherwise the shipped
// archive beside the repository is used. Unlike the un-gated luke_pvf_test.go
// sibling, an explicitly configured path that cannot be opened is a FAILURE, not
// a skip: pointing this test at a PVF and silently skipping would hide a broken
// table.
func realLukeCardSetPVFPath() (string, bool) {
	if configured := strings.TrimSpace(os.Getenv("DNF_LUKE_REAL_PVF_SMOKE")); configured != "" {
		return configured, true
	}
	return filepath.Clean(filepath.Join("..", "..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf")), false
}

func openRealLukeCardSetPVF(t *testing.T) *platformpvf.Archive {
	t.Helper()
	pvfPath, configured := realLukeCardSetPVFPath()
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		if configured {
			t.Fatalf("DNF_LUKE_REAL_PVF_SMOKE=%q unavailable: %v", pvfPath, err)
		}
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	return archive
}

// TestRealScriptPVFLukeCardSetTables pins every value the 1181 card display is
// built from, read out of etc/raid/luke/LukeCardSet.etc in the shipped
// Script.pvf. The [PATTERN] keys, the seven [CARD POS] pairs per pattern, the
// variable-length [IMAGE] index list and the 22 major arcana are all asserted
// here rather than assumed.
func TestRealScriptPVFLukeCardSetTables(t *testing.T) {
	archive := openRealLukeCardSetPVF(t)
	config, err := LoadLukeCardSet(archive)
	if err != nil {
		t.Fatalf("LoadLukeCardSet error = %v", err)
	}

	// 8 [PATTERN] blocks; 5054 x2, 5055 x4, 5056 x2.
	if len(config.Patterns) != 8 {
		t.Fatalf("patterns=%d want 8", len(config.Patterns))
	}
	patternKeys := make(map[uint32][]int, 3)
	for _, pattern := range config.Patterns {
		patternKeys[pattern.DungeonID] = append(patternKeys[pattern.DungeonID], pattern.Index)
	}
	wantKeys := map[uint32][]int{5054: {0, 1}, 5055: {0, 1, 2, 3}, 5056: {0, 1}}
	if !reflect.DeepEqual(patternKeys, wantKeys) {
		t.Fatalf("[PATTERN] keys=%v want %v", patternKeys, wantKeys)
	}
	for _, dungeonID := range []uint32{5054, 5055, 5056} {
		if got := config.PatternIndices(dungeonID); !reflect.DeepEqual(got, wantKeys[dungeonID]) {
			t.Fatalf("PatternIndices(%d)=%v want %v", dungeonID, got, wantKeys[dungeonID])
		}
	}

	// Every pattern carries exactly 7 (x, y) pairs = 14 numbers.
	for _, pattern := range config.Patterns {
		if len(pattern.CardPositions) != 7 {
			t.Fatalf("pattern %d %d card positions=%d want 7",
				pattern.DungeonID, pattern.Index, len(pattern.CardPositions))
		}
		if pattern.ImagePath == "" || len(pattern.ImageIndices) == 0 {
			t.Fatalf("pattern %d %d image=%q indices=%v",
				pattern.DungeonID, pattern.Index, pattern.ImagePath, pattern.ImageIndices)
		}
		if pattern.Tooltip == "" {
			t.Fatalf("pattern %d %d has no tooltip", pattern.DungeonID, pattern.Index)
		}
	}

	// [IMAGE] is variable length: `5054 *` names one buff icon, `5055 *` / `5056 *`
	// name two (`2 3`, `4 5`, ...). A fixed-pair reader would mis-parse all six.
	wantImages := map[[2]int][]int{
		{5054, 0}: {0},
		{5054, 1}: {1},
		{5055, 0}: {2, 3},
		{5056, 0}: {10},
		{5056, 1}: {11},
	}
	for key, want := range wantImages {
		pattern, found := config.Pattern(uint32(key[0]), key[1])
		if !found {
			t.Fatalf("pattern %d %d missing", key[0], key[1])
		}
		if !reflect.DeepEqual(pattern.ImageIndices, want) {
			t.Fatalf("pattern %d %d image indices=%v want %v",
				key[0], key[1], pattern.ImageIndices, want)
		}
	}

	// The first pattern is `5054 0`: 属性值弱化系统 with [CARD POS]
	// `0 0 1 0 2 0 3 0 0.50 1 1 2 2 2` -- half-step coordinates arrive as floats.
	first, found := config.Pattern(5054, 0)
	if !found {
		t.Fatal("pattern 5054 0 missing")
	}
	if first.Tooltip != "属性值弱化系统" {
		t.Fatalf("pattern 5054 0 tooltip=%q", first.Tooltip)
	}
	wantPositions := []LukeCardPosition{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 2, Y: 0}, {X: 3, Y: 0},
		{X: 0.5, Y: 1}, {X: 1, Y: 2}, {X: 2, Y: 2},
	}
	if !reflect.DeepEqual(first.CardPositions, wantPositions) {
		t.Fatalf("pattern 5054 0 card positions=%+v want %+v", first.CardPositions, wantPositions)
	}
	if first.ImagePath != `Interface2\raid\luke\luke_card_system\card_buff_icon.img` {
		t.Fatalf("pattern 5054 0 image path=%q", first.ImagePath)
	}

	// 22 major arcana, keyed by monster id, in source order (arcana 0..21).
	if len(config.Cards) != 22 {
		t.Fatalf("cards=%d want 22", len(config.Cards))
	}
	wantMonsters := []uint32{
		59171, 59098, 59873, 59096, 56065, 59874, 56699, 57155, 59278, 56166, 56673,
		59875, 56066, 56674, 56067, 56068, 56063, 56700, 57156, 59872, 59099, 59095,
	}
	if got := config.CardMonsterIDs(); !reflect.DeepEqual(got, wantMonsters) {
		t.Fatalf("card monster ids=%v want %v", got, wantMonsters)
	}
	for index, monsterID := range wantMonsters {
		if !config.HasCard(monsterID) {
			t.Fatalf("HasCard(%d) = false", monsterID)
		}
		if config.Cards[index].MonsterID != monsterID {
			t.Fatalf("card %d monster=%d want %d", index, config.Cards[index].MonsterID, monsterID)
		}
		if config.Cards[index].ImageIndex != index {
			t.Fatalf("card %d image index=%d want %d", index, config.Cards[index].ImageIndex, index)
		}
		if config.Cards[index].ImagePath != `Interface2\raid\luke\luke_card_system\tarotcard.img` {
			t.Fatalf("card %d image path=%q", index, config.Cards[index].ImagePath)
		}
	}
	// The arcana the official announcement names must be the ones the PVF holds:
	// 00 愚者_红色小丑 and 21 世界_噩梦之内尔贝.
	if !strings.Contains(config.Cards[0].Tooltip, "愚者") {
		t.Fatalf("arcana 0 tooltip=%q", config.Cards[0].Tooltip)
	}
	if !strings.Contains(config.Cards[21].Tooltip, "世界") {
		t.Fatalf("arcana 21 tooltip=%q", config.Cards[21].Tooltip)
	}
	if config.HasCard(0) || config.HasCard(1) {
		t.Fatal("HasCard accepted a non-arcana key")
	}

	// The values this test pins, printed so a run leaves the real numbers in its
	// output rather than only a PASS.
	for _, pattern := range config.Patterns {
		t.Logf("pattern dungeon=%d index=%d image_indices=%v card_positions=%d tooltip=%q",
			pattern.DungeonID, pattern.Index, pattern.ImageIndices, len(pattern.CardPositions), pattern.Tooltip)
	}
	t.Logf("cards=%d first_five_monsters=%v", len(config.Cards), config.CardMonsterIDs()[:5])
}

// TestLukeCardSetRejectsShapesTheClientCouldNotDraw keeps the parser fail-closed:
// every one of these documents would make the 1181 body describe something the
// client cannot resolve, and the caller must not send a half-described packet.
func TestLukeCardSetRejectsShapesTheClientCouldNotDraw(t *testing.T) {
	header := "[PATTERN]\n5054 0 \n[IMAGE]\n`a.img` 0 \n[CARD POS]\n0 0 1 0 \n[/CARD POS]\n[TOOLTIP TEXT]\n`t` \n[/PATTERN]\n"
	card := "[CARD]\n59171 \n[IMAGE]\n`c.img` 0 \n[TOOLTIP TEXT]\n`x` \n[/CARD]\n"
	cases := []struct {
		name string
		text string
	}{
		{"no pattern", card},
		{"no card", header},
		{"odd card pos", "[PATTERN]\n5054 0 \n[IMAGE]\n`a.img` 0 \n[CARD POS]\n0 0 1 \n[/CARD POS]\n[TOOLTIP TEXT]\n`t` \n[/PATTERN]\n" + card},
		{"empty card pos", "[PATTERN]\n5054 0 \n[IMAGE]\n`a.img` 0 \n[CARD POS]\n[/CARD POS]\n[TOOLTIP TEXT]\n`t` \n[/PATTERN]\n" + card},
		{"pattern without image", "[PATTERN]\n5054 0 \n[CARD POS]\n0 0 \n[/CARD POS]\n[TOOLTIP TEXT]\n`t` \n[/PATTERN]\n" + card},
		{"pattern key too wide", "[PATTERN]\n5054 0 7 \n[IMAGE]\n`a.img` 0 \n[CARD POS]\n0 0 \n[/CARD POS]\n[TOOLTIP TEXT]\n`t` \n[/PATTERN]\n" + card},
		{"duplicate pattern key", header + header + card},
		{"duplicate card monster", header + card + card},
		{"card with two image indices", header + "[CARD]\n59171 \n[IMAGE]\n`c.img` 0 1 \n[TOOLTIP TEXT]\n`x` \n[/CARD]\n"},
	}
	for _, testCase := range cases {
		document, err := dnfpvf.Parse(LukeCardSetPVFPath, testCase.text)
		if err != nil {
			t.Fatalf("%s: parse error = %v", testCase.name, err)
		}
		if _, err := ParseLukeCardSet(document); err == nil {
			t.Fatalf("%s: ParseLukeCardSet accepted the document", testCase.name)
		}
	}
	// The same shape with a bad coordinate is rejected too.
	document, err := dnfpvf.Parse(LukeCardSetPVFPath, "[PATTERN]\n5054 0 \n[IMAGE]\n`a.img` 0 \n[CARD POS]\n0 -1 \n[/CARD POS]\n[TOOLTIP TEXT]\n`t` \n[/PATTERN]\n"+card)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if _, err := ParseLukeCardSet(document); err == nil {
		t.Fatal("a negative [CARD POS] coordinate was accepted")
	}
	// A document for another path is refused, so a mis-wired caller is loud.
	other, err := dnfpvf.Parse(LukePVFPath, header+card)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if _, err := ParseLukeCardSet(other); err == nil {
		t.Fatal("ParseLukeCardSet accepted luke.etc")
	}
	if _, err := ParseLukeCardSet(nil); err == nil {
		t.Fatal("ParseLukeCardSet accepted a nil document")
	}
}
