package dnfbridge

import (
	"context"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestReconcileAutoCompletedQuestClearLinkedSubQuestsRepairsHistoricalParent(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", Job: "0", Level: 90}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3249: {
				Status: "completed",
				Extra: map[string]string{
					"auto_completed":       "true",
					"auto_complete_reason": "epic_below_character_level_no_reward",
					"reward_state":         "granted",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(ctx, questListTestSource{
		dnfquest.DefaultList: "3249 `parent.qst`\n3609 `first.qst`\n3610 `second.qst`\n",
		"n_quest/parent.qst": "[grade]\n`[epic]`\n[level]\n35 99\n[job]\n`[all]`\n[exposed by npc]\n303\n[type]\n`[quest clear]`\n[sub type]\n2\n[int data]\n3609 3610\n[reward type]\n`[item]`\n[reward int data]\n0 0\n",
		"n_quest/first.qst":  "[grade]\n`[sub]`\n[attribute]\n`not give exp quest`\n[job]\n`[all]`\n[type]\n`[meet npc]`\n[main quest]\n3249\n[reward type]\n`[item]`\n[reward int data]\n0 0\n",
		"n_quest/second.qst": "[grade]\n`[sub]`\n[attribute]\n`not give exp quest`\n[job]\n`[all]`\n[type]\n`[seek n meet npc]`\n[main quest]\n3249\n[reward type]\n`[item]`\n[reward int data]\n0 0\n",
	}, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       catalog,
	}
	session := &gameSession{connID: "auto-complete-reconcile", selectedCharacterID: 19}
	reconciled, err := service.reconcileAutoCompletedQuestClearLinkedSubQuests(ctx, session, "test_initial_town")
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled {
		t.Fatal("reconcile returned false, want historical child closure persisted")
	}
	persisted, found, err := repositories.Quest.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load persisted found=%t err=%v", found, err)
	}
	for _, questID := range []int64{3609, 3610} {
		state := persisted.States[questID]
		if state.Status != "completed" || state.Extra["auto_completed_by_parent"] != "3249" {
			t.Fatalf("linked quest %d state=%+v", questID, state)
		}
	}
}
