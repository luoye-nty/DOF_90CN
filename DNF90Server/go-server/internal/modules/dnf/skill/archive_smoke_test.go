package skill

import (
	"context"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestArchiveSmoke(t *testing.T) {
	archivePath := os.Getenv("DNF_SKILL_PVF")
	if archivePath == "" {
		t.Skip("set DNF_SKILL_PVF to run the real archive smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: archivePath})
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatalf("build skill index: %v", err)
	}
	table, err := Load(context.Background(), index, Options{})
	if err != nil {
		t.Fatalf("load skill catalog: %v", err)
	}
	snapshot := table.Snapshot()
	t.Logf("real skill catalog: jobs=%d skills=%d", snapshot.Jobs, snapshot.Skills)
	if snapshot.Jobs != 16 || snapshot.Skills == 0 {
		t.Fatalf("unexpected real catalog snapshot: %+v", snapshot)
	}
	upperSlash, ok := table.Find(0, 46)
	if !ok || upperSlash.RequiredLevel != 1 || upperSlash.MaximumLevel <= 0 || !upperSlash.Active {
		t.Fatalf("unexpected swordman skill 46: %+v ok=%t", upperSlash, ok)
	}
	weaponCombo, ok := table.Find(0, 67)
	if !ok || weaponCombo.RequiredLevel != 15 || weaponCombo.RequiredLevelRange != 15 ||
		weaponCombo.MaximumLearnableLevel(90, 0x21) != 6 {
		t.Fatalf("unexpected swordman skill 67: %+v ok=%t", weaponCombo, ok)
	}
	battleMageAwakening, ok := table.Find(3, 83)
	if !ok || !battleMageAwakening.FixedLevelSkill || battleMageAwakening.FixedLevelBase != 1 ||
		battleMageAwakening.FixedLevelInterval != 5 || battleMageAwakening.FixedLevelIncrement != 1 ||
		battleMageAwakening.RequiredLevelRange != 5 || battleMageAwakening.FixedLevelForCharacter(90) != 9 {
		t.Fatalf("unexpected battle-mage awakening skill 83: %+v ok=%t", battleMageAwakening, ok)
	}
	battleGroove, ok := table.Find(3, 84)
	if !ok || len(battleGroove.SecondGrowTypeMaximumLevels) <= 6 ||
		battleGroove.SecondGrowTypeMaximumLevels[6] != 30 || battleGroove.MaximumLearnableLevel(90, 0x13) != 15 {
		t.Fatalf("unexpected battle-mage passive skill 84: %+v ok=%t", battleGroove, ok)
	}
}
