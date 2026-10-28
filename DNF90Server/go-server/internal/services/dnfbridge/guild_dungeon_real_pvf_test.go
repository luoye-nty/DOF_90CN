package dnfbridge

import (
	"encoding/binary"
	"os"
	"testing"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The catalog drives entry permission, so it has to survive the real archive:
// the style list names the layout with mixed case, the layout nests blocks, and
// a stage read wrong silently locks or unlocks the wrong dungeon.
func TestRealPVFGuildDungeonCatalogMatchesTheLiveLayout(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to parse the live guild dungeon layout")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfguild.LoadDungeonCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.FinalDungeon != 8508 {
		t.Fatalf("final dungeon=%d want 8508", catalog.FinalDungeon)
	}
	for _, want := range []struct {
		index        uint32
		gaugeOnClear uint32
		ratio        uint32
		required     []uint32
	}{
		{index: 8500, gaugeOnClear: 150, ratio: 50},
		{index: 8504, gaugeOnClear: 75, ratio: 50, required: []uint32{8500}},
		{index: 8507, gaugeOnClear: 50, ratio: 50, required: []uint32{8500}},
		{index: 8505, gaugeOnClear: 50, ratio: 50, required: []uint32{8504}},
		{index: 8503, gaugeOnClear: 75, ratio: 50, required: []uint32{8507}},
		{index: 8508, gaugeOnClear: 30, ratio: 100, required: []uint32{8503, 8505}},
	} {
		stage, found := catalog.Stage(want.index)
		if !found {
			t.Fatalf("stage %d missing", want.index)
		}
		if stage.GaugeLimit != 10000 || stage.GaugeOnClear != want.gaugeOnClear ||
			stage.ConquestGaugeRatio != want.ratio {
			t.Fatalf("stage %d=%+v want clear=%d ratio=%d",
				want.index, stage, want.gaugeOnClear, want.ratio)
		}
		if len(stage.RequiredConquest) != len(want.required) {
			t.Fatalf("stage %d prerequisites=%v want %v",
				want.index, stage.RequiredConquest, want.required)
		}
		for i, required := range want.required {
			if stage.RequiredConquest[i] != required {
				t.Fatalf("stage %d prerequisites=%v want %v",
					want.index, stage.RequiredConquest, want.required)
			}
		}
	}

	// A guild that has never cleared anything sees exactly 伤城, which is what
	// the operator observed when the table was first seeded with empty gauges.
	empty := map[uint32]uint32{}
	for _, stage := range catalog.Stages {
		unlocked := catalog.Unlocked(empty, stage.Index)
		if (stage.Index == 8500) != unlocked {
			t.Fatalf("with no progress, stage %d unlocked=%t", stage.Index, unlocked)
		}
	}

	// The wire carries clears, not the gauge: the client multiplies by
	// [gauge on clear] itself. 9000 of 8500's 10000 is 60 clears of 150, which
	// is the 90% the operator asked for; sending 9000 rendered 100% because the
	// client read it as nine thousand clears.
	if got := currentGuildDungeonClears(mustStage(t, catalog, 8500), 9000); got != 60 {
		t.Fatalf("8500 gauge 9000 -> %d clears, want 60", got)
	}
	if got := currentGuildDungeonClears(mustStage(t, catalog, 8500), 150); got != 1 {
		t.Fatalf("8500 gauge 150 -> %d clears, want 1", got)
	}
	// A gauge past the limit must not render past a full bar.
	if got := currentGuildDungeonClears(mustStage(t, catalog, 8500), 99_999); got != 10000/150 {
		t.Fatalf("8500 over-credited -> %d clears, want %d", got, 10000/150)
	}

	// Wire shape: u8 flag + 3 u32 + u32 count + count*(u32,u32).
	body := buildCurrentGuildDungeonUnlockBody(catalog, map[uint32]uint32{8500: 9000})
	if want := 17 + 8*len(catalog.Stages); len(body) != want {
		t.Fatalf("body len=%d want %d", len(body), want)
	}
	if body[0] != currentGuildDungeonUnlockFlag {
		t.Fatalf("flag=%d", body[0])
	}
	if got := binary.LittleEndian.Uint32(body[13:17]); got != uint32(len(catalog.Stages)) {
		t.Fatalf("count=%d want %d", got, len(catalog.Stages))
	}
	for index, stage := range catalog.Stages {
		offset := 17 + 8*index
		if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != stage.Index {
			t.Fatalf("pair %d index=%d want %d", index, got, stage.Index)
		}
		want := uint32(0)
		if stage.Index == 8500 {
			want = 60
		}
		if got := binary.LittleEndian.Uint32(body[offset+4 : offset+8]); got != want {
			t.Fatalf("pair %d clears=%d want %d", index, got, want)
		}
	}
}

func mustStage(t *testing.T, catalog dnfguild.DungeonCatalog, index uint32) dnfguild.DungeonStage {
	t.Helper()
	stage, found := catalog.Stage(index)
	if !found {
		t.Fatalf("stage %d missing", index)
	}
	return stage
}

// The per-stage reward counts differ (3 through 7 公会贡献证书), so a parser that
// dropped the [reward] blocks or shared one block across stages would show up
// here rather than as a silent under-payment in production.
func TestRealPVFGuildDungeonRewardsParsePerStage(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to parse the live guild dungeon rewards")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfguild.LoadDungeonCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range catalog.Stages {
		if len(stage.Rewards) != 2 {
			t.Fatalf("stage %d has %d reward blocks, want 2 (99%% and 100%%)",
				stage.Index, len(stage.Rewards))
		}
		for _, reward := range stage.Rewards {
			if reward.GaugeRatio != 99 && reward.GaugeRatio != 100 {
				t.Fatalf("stage %d reward ratio=%d", stage.Index, reward.GaugeRatio)
			}
			// 公会贡献证书; the count is what varies per stage.
			if reward.ItemID != 10154439 || reward.ItemCount == 0 || reward.Contribute != 20 {
				t.Fatalf("stage %d reward=%+v", stage.Index, reward)
			}
		}
	}
	first := mustStage(t, catalog, 8500)
	if first.Rewards[0].ItemCount != 3 {
		t.Fatalf("8500 reward count=%d want 3", first.Rewards[0].ItemCount)
	}
}
