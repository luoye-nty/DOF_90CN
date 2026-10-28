package dnfbridge

import (
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonKurunStrongBreathDropAmountUsesPVFRankRule(t *testing.T) {
	dungeon := worldmap.Dungeon{
		ID:   3200,
		Path: "Event/Dragon_Kurun_kor/Dragon_Kurun_kor_NateramWarden.dgn",
		SourceSections: []worldmap.RawSection{{
			Name:   "nateram time attack info",
			Tokens: []dnfpvf.Token{{Kind: dnfpvf.TokenInt, Int: 7}, {Kind: dnfpvf.TokenInt, Int: 45000}},
		}},
	}
	for monsterType, want := range map[byte]uint32{0: 0, 1: 1, 2: 1, 3: 7} {
		if got := currentDungeonKurunStrongBreathDropAmount(dungeon, monsterType); got != want {
			t.Fatalf("monster type=%d amount=%d want=%d", monsterType, got, want)
		}
	}
	dungeon.Path = "Dungeon/Ordinary/Test.dgn"
	if got := currentDungeonKurunStrongBreathDropAmount(dungeon, 3); got != 0 {
		t.Fatalf("ordinary dungeon boss amount=%d want=0", got)
	}
}
