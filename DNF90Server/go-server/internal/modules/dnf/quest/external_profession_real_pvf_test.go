package quest

import (
	"context"
	"os"
	"reflect"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestExternalProfessionQuestPiecePrerequisitesUseUnlistedPVFGates(t *testing.T) {
	index, err := dnfpvf.Build(context.Background(), externalProfessionQuestPieceSource{
		DefaultList:                             "100 `ordinary.qst`\n2680 `time_piece/side_job01.qst`\n",
		"n_quest/ordinary.qst":                  "[grade]\n`[epic]`\n[level]\n1 99\n",
		"n_quest/time_piece/side_job01.qst":     "[level]\n50 99\n[target character]\n`[demonic swordman]` -1 -1 `[creator mage]` -1 -1\n[/target character]\n[pre required quest]\n3427\n[/pre required quest]\n[job change quest]\n2\n",
		"n_quest/time_piece/lorien_1.qst":       "[level]\n1 99\n[target character]\n`[demonic swordman]` -1 -1 `[creator mage]` -1 -1\n[/target character]\n[pre required quest]\n1777\n[/pre required quest]\n[quest piece]\n1\n",
		"n_quest/time_piece/powerstation_1.qst": "[level]\n79 99\n[target character]\n`[demonic swordman]` -1 -1 `[creator mage]` -1 -1\n[/target character]\n[pre required quest]\n4488\n[/pre required quest]\n[quest piece]\n1\n",
	}, dnfpvf.BuildOptions{Lists: []string{DefaultList}, Prefixes: []string{"n_quest/"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.ExternalProfessionQuestPiecePrerequisites(CharacterEligibility{Level: 90, Job: 9}); !reflect.DeepEqual(got, []int64{1777, 4488}) {
		t.Fatalf("dark-knight quest-piece gates=%v", got)
	}
	if got := catalog.ExternalProfessionQuestPiecePrerequisites(CharacterEligibility{Level: 50, Job: 10}); !reflect.DeepEqual(got, []int64{1777}) {
		t.Fatalf("creator level-gated quest-piece gates=%v", got)
	}
	if got := catalog.ExternalProfessionQuestPiecePrerequisites(CharacterEligibility{Level: 90, Job: 12}); len(got) != 0 {
		t.Fatalf("ordinary profession received external gates=%v", got)
	}
}

func TestRealScriptPVFExternalProfessionQuestAndAwakeningContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify external-profession quests")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists: []string{DefaultList}, Prefixes: []string{"n_quest/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	wantGates := []int64{1777, 3044, 4475, 4477, 4478, 4479, 4480, 4481, 4482, 4483, 4484, 4485, 4486, 4487, 4488, 4489}
	for _, job := range []int{9, 10} {
		if got := catalog.ExternalProfessionQuestPiecePrerequisites(CharacterEligibility{Level: 90, Job: job}); !reflect.DeepEqual(got, wantGates) {
			t.Fatalf("job %d real quest-piece gates=%v want=%v", job, got, wantGates)
		}
	}
	completed3427 := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{3427: {Status: "completed"}}}
	for _, job := range []int{9, 10} {
		if !containsCatalogQuestID(catalog.Acceptable(CharacterEligibility{Level: 50, Job: job}, completed3427).IDs, 2680) {
			t.Fatalf("job %d first-awakening quest 2680 is not acceptable after 3427", job)
		}
	}
}

type externalProfessionQuestPieceSource map[string]string

func (s externalProfessionQuestPieceSource) ReadText(path string) (string, error) {
	value, ok := s[path]
	if !ok {
		return "", os.ErrNotExist
	}
	return value, nil
}

func (s externalProfessionQuestPieceSource) RangePaths(yield func(string) bool) {
	for path := range s {
		if !yield(path) {
			return
		}
	}
}
