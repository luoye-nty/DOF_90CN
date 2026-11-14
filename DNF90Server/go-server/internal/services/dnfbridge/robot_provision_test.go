package dnfbridge

import (
	"context"
	"errors"
	"testing"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type fakeRobotExpertJobCatalog struct {
	configs    map[byte]*dnfexpertjob.Config
	enchanter  *dnfexpertjob.EnchanterConfig
	disjointer *dnfexpertjob.DisjointerConfig
}

func (c *fakeRobotExpertJobCatalog) Config(jobType byte) (*dnfexpertjob.Config, bool) {
	config, ok := c.configs[jobType]
	return config, ok
}

func (c *fakeRobotExpertJobCatalog) Enchanter() (*dnfexpertjob.EnchanterConfig, bool) {
	return c.enchanter, c.enchanter != nil
}

func (c *fakeRobotExpertJobCatalog) Disjointer() (*dnfexpertjob.DisjointerConfig, bool) {
	return c.disjointer, c.disjointer != nil
}

func TestGMProvisionRobotCharacterUsesCurrentCreationAggregate(t *testing.T) {
	repositories := testRepositoryGroup()
	experience, err := dnfprogression.LoadExperienceTable(context.Background(), dungeonDropCatalogTestSource{
		dnfprogression.ExperienceTablePath: "100 250 500\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		experienceTable:    experience,
	}
	thresholds := make([]int64, 11)
	for index := range thresholds {
		thresholds[index] = int64((index + 1) * 100)
	}
	repairRules := make([]dnfexpertjob.RepairRule, 11)
	for index := range repairRules {
		repairRules[index] = dnfexpertjob.RepairRule{FullRepairCost: int64(10_000 + index*1_000), MaxEndurance: int64(300 + index*40)}
	}
	service.robotExpertJobCatalogOverride = &fakeRobotExpertJobCatalog{
		disjointer: &dnfexpertjob.DisjointerConfig{
			ExperienceThresholds: thresholds,
			RepairRules:          repairRules,
			MaximumStoreCharge:   10_000,
		},
	}
	prepareTestCharacterInitialization(service, 15)
	service.initialSPTable[2] = 30

	result, err := service.GMProvisionRobotCharacter(context.Background(), GMRobotCharacterProvisionRequest{
		AccountID: "robot-account-1", Name: "机器人甲", Job: 15, GrowType: 2, Level: 2,
		Village: 3, Area: 4, X: 500, Y: 260,
		ExpertStoreKind: "disjoint", ExpertStoreCost: 777,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CharacterID == 0 || result.AccountID != "robot-account-1" || result.Name != "机器人甲" ||
		result.Job != 15 || result.GrowType != 2 || result.Level != 2 || result.Slot != 0 ||
		result.ExpertStoreKind != "disjoint" || result.ExpertStoreCost != 777 {
		t.Fatalf("provision result = %+v", result)
	}
	character, found, err := repositories.Character.Load(context.Background(), "1")
	if err != nil || !found {
		t.Fatalf("character found=%t err=%v", found, err)
	}
	if character.Level != 2 || character.Job != "15" || character.Stats["grow_type"] != 2 ||
		character.Stats["exp"] != 100 || character.Stats["town_id"] != 3 || character.Stats["area_id"] != 4 ||
		character.Stats["pos_x"] != 500 || character.Stats["pos_y"] != 260 ||
		character.Stats["expert_job_type"] != 3 || character.Stats["expert_job_exp"] != 1_000_000_000 ||
		character.Stats["expert_job_machine_grade"] != 11 ||
		character.Stats["expert_job_machine_endurance"] != 700 ||
		character.Stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] != 0 ||
		character.Stats[dnfrepo.CharacterStoryDigestMigrationVersionStatKey] != 0 ||
		character.Stats["ex_equip_slot_stat"] != gmAllEquipSlotsMask {
		t.Fatalf("character = %+v", character)
	}
	if _, found, err := repositories.Inventory.Load(context.Background(), "1"); err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if skill, found, err := repositories.Skill.Load(context.Background(), "1"); err != nil || !found ||
		skill.Points.TotalSP != 30 || skill.Points.RemainingSP != 30 || skill.Points.SyncedLevel != 2 {
		t.Fatalf("skill found=%t err=%v skill=%+v", found, err, skill)
	}
	account, found, err := repositories.Account.Load(context.Background(), "robot-account-1")
	if err != nil || !found || account.State != "active" || account.Metadata["source"] != "dnfbridge" {
		t.Fatalf("account found=%t err=%v account=%+v", found, err, account)
	}

	_, err = service.GMProvisionRobotCharacter(context.Background(), GMRobotCharacterProvisionRequest{
		AccountID: "robot-account-2", Name: "机器人甲", Job: 15, Level: 1,
	})
	if !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("duplicate name error = %v", err)
	}
}

func TestApplyRobotExpertJobProvisionStatsMaxesEnchanterLevelAndEndurance(t *testing.T) {
	thresholds := make([]int64, 11)
	repairRules := make([]dnfexpertjob.RepairRule, 11)
	for index := range thresholds {
		thresholds[index] = int64((index + 1) * 100)
		repairRules[index] = dnfexpertjob.RepairRule{FullRepairCost: 10_000, MaxEndurance: int64(300 + index*40)}
	}
	config := &dnfexpertjob.Config{JobType: dnfexpertjob.EnchanterType, ExperienceThresholds: thresholds}
	catalog := &fakeRobotExpertJobCatalog{
		configs: map[byte]*dnfexpertjob.Config{dnfexpertjob.EnchanterType: config},
		enchanter: &dnfexpertjob.EnchanterConfig{
			Recipes: config, InitialEndurance: 300, RepairRules: repairRules,
		},
	}
	stats := map[string]int64{}
	if err := applyRobotExpertJobProvisionStats(stats, "enchant", catalog); err != nil {
		t.Fatal(err)
	}
	if stats["expert_job_type"] != int64(dnfexpertjob.EnchanterType) ||
		stats["expert_job_exp"] != robotExpertJobFullExperience ||
		stats["expert_job_machine_grade"] != 11 ||
		stats["expert_job_machine_endurance"] != 700 {
		t.Fatalf("enchanter robot stats = %+v", stats)
	}
}
