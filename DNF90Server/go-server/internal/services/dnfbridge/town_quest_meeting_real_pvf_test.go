package dnfbridge

import (
	"context"
	"os"
	"reflect"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFLukeQuest2903MeetingMerylUsesTownNPC347(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Luke quest 2903 meeting NPC ownership")
	}
	ctx := context.Background()
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	towns, err := dnftown.Load(ctx, archive, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	area, ok := towns.FindArea(22, 3)
	if !ok {
		t.Fatal("real PVF town22 area3 is missing")
	}
	spawnFound := false
	for _, spawn := range area.NPCs {
		if spawn.NPCID == 347 && spawn.X == 494 && spawn.Y == 223 {
			spawnFound = true
			break
		}
	}
	if !spawnFound {
		t.Fatalf("real PVF map %q NPCs=%+v, want Meryl NPC347 at 494,223", area.MapPath, area.NPCs)
	}

	questIndex, err := dnfpvf.Build(ctx, archive, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, questIndex)
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		2903: {Status: "active", ProgressValue: 517},
	}}
	want := []dnfquest.MeetingNPCTrigger{
		{QuestID: 2903, TriggerType: 0x10},
		{QuestID: 2903, TriggerType: 0x20},
	}
	if got := catalog.ActiveMeetingNPCTriggers(record, 347); !reflect.DeepEqual(got, want) {
		t.Fatalf("real PVF quest2903 meeting triggers=%+v want=%+v", got, want)
	}
}
