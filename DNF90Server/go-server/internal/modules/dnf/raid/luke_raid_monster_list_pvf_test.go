package raid

import (
	"reflect"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// TestParseLukeRaidMonsterListReadsMazesInSourceOrder pins the table the 1181
// card display is derived from: one [DUNGEON] per raid dungeon, one [MAZE] per
// maze index, monsters in the order the document lists them, [PASSIVE] skipped
// (it names a map object, not a monster) and a dungeon without any [MAZE] kept
// as an empty list rather than rejected (the shipped 5310 is exactly that).
func TestParseLukeRaidMonsterListReadsMazesInSourceOrder(t *testing.T) {
	document, err := dnfpvf.Parse(LukeRaidMonsterListPVFPath, "[RAID MONSTER LIST]\n"+
		"[DUNGEON]\n5054\n"+
		"[MAZE]\n0\n[BOSS]\n56063\n[/MAZE]\n"+
		"[MAZE]\n1\n[NAMED]\n56674\n[BOSS]\n56700\n[/MAZE]\n"+
		"[MAZE]\n2\n[PASSIVE]\n64062\n[NAMED]\n59873\n[BOSS]\n56673\n[/MAZE]\n"+
		"[/DUNGEON]\n"+
		"[DUNGEON]\n5310\n[PASSIVE]\n64062\n[/DUNGEON]\n"+
		"[/RAID MONSTER LIST]\n")
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseLukeRaidMonsterList(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Dungeons) != 2 {
		t.Fatalf("dungeons=%+v", config.Dungeons)
	}
	dungeon, ok := config.Dungeon(5054)
	if !ok || dungeon.DungeonID != 5054 {
		t.Fatalf("dungeon 5054=%+v ok=%t", dungeon, ok)
	}
	if count, ok := config.MazeCount(5054); !ok || count != 3 {
		t.Fatalf("5054 maze count=%d ok=%t want 3", count, ok)
	}
	want := [][]uint32{{56063}, {56674, 56700}, {59873, 56673}}
	for index, monsters := range want {
		got, ok := config.MazeMonsters(5054, index)
		if !ok || !reflect.DeepEqual(got, monsters) {
			t.Fatalf("maze %d monsters=%v ok=%t want %v", index, got, ok, monsters)
		}
	}
	// [PASSIVE] must not have been taken for a monster.
	for _, maze := range dungeon.Mazes {
		for _, monsterID := range maze.MonsterIDs {
			if monsterID == 64062 {
				t.Fatalf("maze %d kept the [PASSIVE] object as a monster: %v", maze.Index, maze.MonsterIDs)
			}
		}
	}
	empty, ok := config.Dungeon(5310)
	if !ok || len(empty.Mazes) != 0 {
		t.Fatalf("dungeon 5310=%+v ok=%t want no mazes", empty, ok)
	}
	if _, ok := config.MazeMonsters(5310, 0); ok {
		t.Fatal("a dungeon without [MAZE] answered a maze lookup")
	}
	if _, ok := config.Dungeon(9999); ok {
		t.Fatal("an unknown dungeon was found")
	}
}

// TestParseLukeRaidMonsterListRejectsOtherDocuments keeps luke.etc and the tarot
// tables from being read as the monster list.
func TestParseLukeRaidMonsterListRejectsOtherDocuments(t *testing.T) {
	if _, err := ParseLukeRaidMonsterList(nil); err == nil {
		t.Fatal("a nil document was accepted")
	}
	document, err := dnfpvf.Parse(LukePVFPath, "[RAID MONSTER LIST]\n[DUNGEON]\n5054\n[MAZE]\n0\n[BOSS]\n56063\n[/MAZE]\n[/DUNGEON]\n[/RAID MONSTER LIST]\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseLukeRaidMonsterList(document); err == nil {
		t.Fatal("luke.etc was accepted as the raid monster list")
	}
	if _, err := LoadLukeRaidMonsterList(nil); err == nil {
		t.Fatal("a nil source was accepted")
	}
}
