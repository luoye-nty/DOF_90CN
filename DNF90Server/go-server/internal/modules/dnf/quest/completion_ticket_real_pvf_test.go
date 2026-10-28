package quest

import (
	"context"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFLevelUpTicketProtectsExternalAndFeatureUnlockTasks(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify level-up ticket quest guards")
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

	for _, questID := range []int64{649, 650, 2635, 2636} {
		definition, found := catalog.Find(questID)
		if !found || !ticketOptionalExternalQuest(definition) {
			t.Fatalf("feature/external quest %d definition=%+v found=%t is not protected", questID, definition, found)
		}
	}
	for _, questID := range []int64{2002, 2900, 4391, 4539} {
		definition, found := catalog.Find(questID)
		if !found || !catalog.HasHellPartyUnlock(questID) {
			t.Fatalf("Hell Party quest %d definition=%+v found=%t is not protected", questID, definition, found)
		}
	}
}
