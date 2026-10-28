package quest

import (
	"context"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func loadRealQuestCatalogForCarryForward(t *testing.T) *Catalog {
	t.Helper()
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify carry-forward story items against the runtime PVF")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// Runtime PVF: 3226 (alphraira_epic_03, lvl 30) rewards 10164786 斯卡迪女王的
// 书信; 3610 (child of the lvl-35 [quest clear] 3249) is [seek n meet npc]
// {10164786 1 303}. A level-35 character auto-completes 3226 under cutoff 34
// and must then play 3249, so parent closure never reaches 3610 - the letter
// has to be carried forward.
func TestRealScriptPVFAutoCompletePreservesSkadiLetterForQuest3610(t *testing.T) {
	catalog := loadRealQuestCatalogForCarryForward(t)
	producer, found := catalog.Find(3226)
	if !found || len(producer.RewardItems) != 1 || producer.RewardItems[0].ItemID != 10164786 {
		t.Fatalf("quest 3226 definition=%+v found=%t; the letter is expected as its reward item", producer, found)
	}
	consumer, found := catalog.Find(3610)
	if !found || consumer.MainQuestID != 3249 || len(consumer.IntData) < 2 || consumer.IntData[0] != 10164786 || consumer.IntData[1] != 1 {
		t.Fatalf("quest 3610 definition=%+v found=%t; expected [seek n meet npc] of one letter under 3249", consumer, found)
	}
	record := dnfrepo.QuestRecord{
		CharacterID: "7",
		States: map[int64]dnfrepo.QuestState{
			3226: {
				Status: "completed",
				Extra: map[string]string{
					"auto_completed":       "true",
					"auto_complete_reason": "epic_below_character_level_no_reward",
					"reward_state":         finishRewardGranted,
				},
			},
			3249: {Status: "active"},
			3609: {Status: "completed"},
			3610: {Status: "active"},
		},
	}
	grants, err := catalog.autoCompleteCarryForwardGrants(record, CharacterEligibility{Level: 35, Job: 0, GrowType: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].ProducerQuestID != 3226 || grants[0].ConsumerQuestID != 3610 || grants[0].ItemID != 10164786 || grants[0].Count != 1 {
		t.Fatalf("Skadi letter continuity grants=%+v", grants)
	}
	// A character who actually played 3226 has no auto_completed marker and
	// must not be handed a second letter.
	played := dnfrepo.CloneQuest(record)
	played.States[3226] = dnfrepo.QuestState{Status: "completed"}
	if grants, err := catalog.autoCompleteCarryForwardGrants(played, CharacterEligibility{Level: 35, Job: 0, GrowType: 3}); err != nil || len(grants) != 0 {
		t.Fatalf("played producer must yield no grant: grants=%+v err=%v", grants, err)
	}
}

// Runtime PVF: 3449/3456/3460/3483 each reward one 10164800 毁灭纪碎片, and the
// [quest clear] producers 3458/3469 contribute through their children; 3509 is
// [seek n meet npc] {10164800 10 310}. The join must reconstruct the net ten.
func TestRealScriptPVFAutoCompleteRebuildsDestructionEraFragmentsForQuest3509(t *testing.T) {
	catalog := loadRealQuestCatalogForCarryForward(t)
	completedNoReward := dnfrepo.QuestState{
		Status: "completed",
		Extra: map[string]string{
			"auto_completed":       "true",
			"auto_complete_reason": "epic_below_character_level_no_reward",
		},
	}
	record := dnfrepo.QuestRecord{
		CharacterID: "7",
		States: map[int64]dnfrepo.QuestState{
			3449: completedNoReward,
			3456: completedNoReward,
			3458: completedNoReward,
			3460: completedNoReward,
			3469: completedNoReward,
			3483: completedNoReward,
			3509: {Status: "active"},
		},
	}
	grants, err := catalog.autoCompleteCarryForwardGrants(record, CharacterEligibility{Level: 62, Job: 6, GrowType: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].ProducerQuestID != 3449 || grants[0].ConsumerQuestID != 3509 ||
		grants[0].ItemID != 10164800 || grants[0].Count != 10 {
		t.Fatalf("quest 3509 carry-forward grants=%+v", grants)
	}
}
