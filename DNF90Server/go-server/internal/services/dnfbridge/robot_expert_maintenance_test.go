package dnfbridge

import (
	"context"
	"testing"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRestoreRobotExpertMachineFillsFullDisjointerEnduranceOffline(t *testing.T) {
	repositories := dnfmemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "manual-robot-001", Slot: 1, Name: "机器人", Job: "0", Level: 90,
		Stats: map[string]int64{
			"expert_job_type":              int64(dnfexpertjob.DisjointerType),
			"expert_job_exp":               robotExpertJobFullExperience,
			"expert_job_machine_grade":     11,
			"expert_job_machine_endurance": 5,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{"0:9": {ItemID: 1, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	thresholds := make([]int64, 11)
	repairRules := make([]dnfexpertjob.RepairRule, 11)
	for index := range thresholds {
		thresholds[index] = int64((index + 1) * 100)
		repairRules[index] = dnfexpertjob.RepairRule{FullRepairCost: 10_000, MaxEndurance: int64(300 + index*40)}
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		robotExpertJobCatalogOverride: &fakeRobotExpertJobCatalog{
			disjointer: &dnfexpertjob.DisjointerConfig{
				ExperienceThresholds: thresholds,
				RepairRules:          repairRules,
			},
		},
	}
	before, err := service.RobotExpertMachineState(ctx, "manual-robot-001", 19)
	if err != nil {
		t.Fatal(err)
	}
	if before.Grade != 11 || before.Endurance != 5 || before.MaxEndurance != 700 {
		t.Fatalf("state before repair = %+v", before)
	}
	after, err := service.RestoreRobotExpertMachine(ctx, "manual-robot-001", 19)
	if err != nil {
		t.Fatal(err)
	}
	if after.Grade != 11 || after.Endurance != 700 || after.MaxEndurance != 700 {
		t.Fatalf("state after repair = %+v", after)
	}
	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load repaired character found=%t err=%v", found, err)
	}
	if character.Stats["expert_job_machine_grade"] != 11 ||
		character.Stats["expert_job_machine_endurance"] != 700 {
		t.Fatalf("repaired character stats = %+v", character.Stats)
	}
}
