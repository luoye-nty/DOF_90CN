package profession

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFProfessionProfiles(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify profession profiles")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	explicitClassPreviewSkills := 0
	contradictoryFitness := make(map[dnfskill.Key]int)
	for _, definition := range catalog.Skills() {
		explicit := make(map[int]struct{}, len(definition.GrowTypes))
		for _, branch := range definition.GrowTypes {
			explicit[branch] = struct{}{}
		}
		if len(explicit) == 0 {
			continue
		}
		if _, allowsBase := explicit[0]; !allowsBase && len(definition.GrowTypeMaximumLevels) > 0 && definition.GrowTypeMaximumLevels[0] > 0 {
			explicitClassPreviewSkills++
		}
		intersects := false
		for branch := range explicit {
			if branch >= 0 && branch < len(definition.GrowTypeMaximumLevels) && definition.GrowTypeMaximumLevels[branch] > 0 {
				intersects = true
				break
			}
		}
		exclusive := -1
		for branch := 1; branch < len(definition.GrowTypeMaximumLevels); branch++ {
			if definition.GrowTypeMaximumLevels[branch] <= 0 {
				continue
			}
			if exclusive >= 0 {
				exclusive = -2
				break
			}
			exclusive = branch
		}
		if !intersects && exclusive >= 0 {
			contradictoryFitness[definition.Key()] = exclusive
		}
	}
	if explicitClassPreviewSkills != 98 {
		t.Fatalf("explicit class skills with client base preview=%d want=98", explicitClassPreviewSkills)
	}
	wantContradictory := map[dnfskill.Key]int{
		{Job: 5, ID: 16}:   4,
		{Job: 7, ID: 236}:  2,
		{Job: 7, ID: 237}:  2,
		{Job: 12, ID: 126}: 4,
	}
	if !reflect.DeepEqual(contradictoryFitness, wantContradictory) {
		t.Fatalf("exclusive contradictory fitness=%v want=%v", contradictoryFitness, wantContradictory)
	}
	for _, skillID := range []uint16{236, 237} {
		definition, found := catalog.Find(7, skillID)
		if !found || definition.SupportsCharacterGrowth(0x21) || !definition.SupportsCharacterGrowth(0x22) {
			t.Fatalf("male Striker contradictory-fitness skill=%d definition=%+v found=%t", skillID, definition, found)
		}
	}
	for _, example := range []struct {
		job    byte
		id     uint16
		branch int
		path   string
	}{
		{job: 2, id: 11, branch: 1, path: "skill/gunner/fastshoot.skl"},
		{job: 13, id: 90, branch: 4, path: "skill/demoniclancer/doublethrow.skl"},
	} {
		definition, ok := catalog.Find(example.job, example.id)
		if !ok || !definition.SupportsGrowType(example.branch) || definition.SupportsGrowType(0) || definition.Path != example.path {
			t.Fatalf("strict profession ownership mismatch for %+v: definition=%+v found=%t", example, definition, ok)
		}
	}
	profiles, err := LoadProfiles(context.Background(), archive, catalog)
	if err != nil {
		t.Fatal(err)
	}
	berserkerGrants, berserkerSupported := profiles.FreeGrantsAtLevel(catalog, 0, 0x03, 15)
	if !berserkerSupported || grantLevel(berserkerGrants, 10) != 1 || grantLevel(berserkerGrants, 248) != 1 {
		t.Fatalf("berserker level15 system grants=%v supported=%t want skills 10 and 248", berserkerGrants, berserkerSupported)
	}
	snakeSword, snakeSwordFound := catalog.Find(11, 31)
	swordDemonGrants, swordDemonSupported := profiles.FreeGrantsAtLevel(catalog, 11, 0x03, 15)
	if !snakeSwordFound || snakeSword.Active || snakeSword.SkillClass != 3 ||
		!snakeSword.SupportsCharacterGrowth(0x03) ||
		!swordDemonSupported || grantLevel(swordDemonGrants, 31) != 1 {
		t.Fatalf(
			"Sword Demon Snake Sword contract definition=%+v found=%t grants=%v supported=%t",
			snakeSword, snakeSwordFound, swordDemonGrants, swordDemonSupported,
		)
	}
	battleMageClass, battleMageClassSupported := profiles.FreeGrantsAtLevel(catalog, 3, 0x03, 90)
	battleMageFirst, battleMageFirstSupported := profiles.FreeGrantsAtLevel(catalog, 3, 0x13, 90)
	battleMageSecond, battleMageSecondSupported := profiles.FreeGrantsAtLevel(catalog, 3, 0x23, 90)
	if !battleMageClassSupported || len(battleMageClass) != 25 ||
		grantLevel(battleMageClass, 5) != 1 || grantLevel(battleMageClass, 6) != 1 ||
		grantLevel(battleMageClass, 18) != 1 || grantLevel(battleMageClass, 197) != 1 ||
		containsGrant(battleMageClass, 83) || containsGrant(battleMageClass, 84) {
		t.Fatalf("battle-mage class-change baseline=%v supported=%t", battleMageClass, battleMageClassSupported)
	}
	if !battleMageFirstSupported || len(battleMageFirst) != 28 ||
		grantLevel(battleMageFirst, 5) != 1 || grantLevel(battleMageFirst, 6) != 1 ||
		grantLevel(battleMageFirst, 83) != 9 || containsGrant(battleMageFirst, 84) ||
		grantLevel(battleMageFirst, 301) != 1 || grantLevel(battleMageFirst, 305) != 1 ||
		containsGrant(battleMageFirst, 300) {
		t.Fatalf("battle-mage first-awakening baseline=%v supported=%t", battleMageFirst, battleMageFirstSupported)
	}
	if !battleMageSecondSupported || len(battleMageSecond) != 29 ||
		grantLevel(battleMageSecond, 5) != 1 || grantLevel(battleMageSecond, 6) != 1 ||
		grantLevel(battleMageSecond, 83) != 9 || containsGrant(battleMageSecond, 84) || containsGrant(battleMageSecond, 126) ||
		grantLevel(battleMageSecond, 300) != 1 || grantLevel(battleMageSecond, 301) != 1 || grantLevel(battleMageSecond, 305) != 1 {
		t.Fatalf("battle-mage second-awakening baseline=%v supported=%t", battleMageSecond, battleMageSecondSupported)
	}
	paidAwakeningSkills := 0
	freeAwakeningSkills := 0
	for job := byte(0); job <= 15; job++ {
		firstBranch, lastBranch := byte(1), byte(4)
		if job == 8 {
			lastBranch = 5
		}
		if job == 9 || job == 10 {
			firstBranch, lastBranch = 0, 0
		}
		for branch := firstBranch; branch <= lastBranch; branch++ {
			previous, previousOK := profiles.FreeGrants(job, branch, 0)
			if !previousOK {
				continue
			}
			previousLevels := make(map[uint16]int, len(previous))
			for _, grant := range previous {
				previousLevels[grant.SkillID] = grant.Level
			}
			for stage := byte(1); stage <= 2; stage++ {
				declared, declaredOK := profiles.FreeGrants(job, branch, stage)
				protected, protectedOK := profiles.FreeGrantsAtLevel(catalog, job, stage<<4|branch, 90)
				if !declaredOK || !protectedOK {
					t.Fatalf("job=%d branch=%d stage=%d declared=%t protected=%t", job, branch, stage, declaredOK, protectedOK)
				}
				for _, grant := range declared {
					if grant.Level <= previousLevels[grant.SkillID] {
						continue
					}
					definition, found := catalog.Find(job, grant.SkillID)
					if !found {
						t.Fatalf("awakening grant has no skill definition: job=%d branch=%d stage=%d skill=%d", job, branch, stage, grant.SkillID)
					}
					if len(definition.PurchaseCost) == 0 {
						t.Fatalf("awakening grant has no purchase table: job=%d branch=%d stage=%d skill=%d", job, branch, stage, grant.SkillID)
					}
					if definition.LevelCost(0) > 0 {
						paidAwakeningSkills++
						if containsGrant(protected, grant.SkillID) {
							t.Fatalf("paid awakening skill entered protected baseline: job=%d branch=%d stage=%d skill=%d cost=%d", job, branch, stage, grant.SkillID, definition.LevelCost(0))
						}
					} else {
						freeAwakeningSkills++
						if grantLevel(protected, grant.SkillID) < grant.Level {
							t.Fatalf("free awakening skill missing from protected baseline: job=%d branch=%d stage=%d skill=%d", job, branch, stage, grant.SkillID)
						}
					}
				}
				previousLevels = make(map[uint16]int, len(declared))
				for _, grant := range declared {
					previousLevels[grant.SkillID] = grant.Level
				}
			}
		}
	}
	if paidAwakeningSkills != 57 || freeAwakeningSkills != 61 {
		t.Fatalf("awakening purchase classification paid=%d free=%d want paid=57 free=61", paidAwakeningSkills, freeAwakeningSkills)
	}
	expectedSupplements := map[string]struct{}{
		"0/3/10": {}, "0/3/248": {}, "1/3/33": {},
		"1/3/114": {},
		"2/3/256": {}, "3/3/319": {},
		"3/1/33": {}, "3/2/314": {}, "3/3/300": {}, "3/3/301": {},
		"3/3/305": {}, "3/4/195": {}, "3/4/199": {}, "4/3/136": {},
		"4/3/257": {}, "7/3/33": {},
		"7/3/110": {}, "12/1/31": {}, "12/1/45": {}, "12/1/46": {},
		"12/1/47": {}, "12/1/48": {}, "12/1/49": {}, "12/1/77": {},
		"12/1/106": {}, "12/3/13": {}, "12/3/77": {}, "12/4/13": {},
		"12/4/77": {}, "14/1/84": {}, "14/2/84": {},
		"14/4/84": {}, "10/0/268": {},
	}
	foundSupplements := make(map[string]struct{}, len(expectedSupplements))
	for job := byte(0); job <= 15; job++ {
		firstBranch := byte(1)
		lastBranch := byte(4)
		if job == 9 || job == 10 {
			firstBranch, lastBranch = 0, 0
		} else if job == 8 {
			lastBranch = 5
		}
		for branch := firstBranch; branch <= lastBranch; branch++ {
			raw, rawOK := profiles.FreeGrants(job, branch, 2)
			augmented, augmentedOK := profiles.FreeGrantsAtLevel(catalog, job, 0x20|branch, 90)
			if !rawOK || !augmentedOK {
				continue
			}
			rawIDs := make(map[uint16]struct{}, len(raw))
			for _, grant := range raw {
				rawIDs[grant.SkillID] = struct{}{}
			}
			for _, grant := range augmented {
				if _, found := rawIDs[grant.SkillID]; found {
					continue
				}
				key := fmt.Sprintf("%d/%d/%d", job, branch, grant.SkillID)
				if _, expected := expectedSupplements[key]; !expected {
					t.Fatalf("unexpected profession system supplement %s", key)
				}
				foundSupplements[key] = struct{}{}
			}
		}
	}
	if len(foundSupplements) != len(expectedSupplements) {
		for key := range expectedSupplements {
			if _, found := foundSupplements[key]; !found {
				t.Errorf("missing profession system supplement %s", key)
			}
		}
		t.Fatalf("profession system supplements=%d want=%d", len(foundSupplements), len(expectedSupplements))
	}
	witchToggle, found := catalog.Find(3, 167)
	witchBaseline, witchSupported := profiles.FreeGrantsAtLevel(catalog, 3, 0x04, 90)
	if !found || witchToggle.Path != "skill/mage/clearflapper.skl" || len(witchToggle.Prerequisites) != 1 ||
		witchToggle.Prerequisites[0].SkillID != 95 || witchToggle.Prerequisites[0].Level != 1 ||
		!witchSupported || containsGrant(witchBaseline, 167) {
		t.Fatalf("witch clear-flapper optional toggle definition=%+v found=%t baseline=%v supported=%t", witchToggle, found, witchBaseline, witchSupported)
	}
	snapshot := profiles.Snapshot()
	if snapshot.Jobs != 16 || snapshot.InitialGrants != 116 || snapshot.ClassGrants != 453 || snapshot.AwakeningGrants != 118 || snapshot.MissingSkills != 3 {
		t.Fatalf("profession snapshot = %+v", snapshot)
	}
	if name, ok := profiles.DisplayName(11, 0x21); !ok || name != "剑皇" {
		t.Fatalf("female-slayer second-awakening display=%q found=%t want=剑皇", name, ok)
	}
	if name, ok := profiles.DisplayName(2, 0x01); !ok || name != "漫游枪手" {
		t.Fatalf("gunner class display=%q found=%t want=漫游枪手", name, ok)
	}
	fixedLevelSkills := 0
	fixedLevelAt90 := 0
	for _, definition := range catalog.Skills() {
		if !definition.FixedLevelSkill {
			continue
		}
		fixedLevelSkills++
		if definition.FixedLevelForCharacter(90) > 0 {
			fixedLevelAt90++
		}
	}
	gunbladerFirst, ok := profiles.FreeGrants(15, 1, 1)
	if !ok || len(gunbladerFirst) == 0 {
		t.Fatalf("gunblader class1 awakening1 grants = %v, found=%t", gunbladerFirst, ok)
	}
	gunbladerSecond, ok := profiles.FreeGrants(15, 1, 2)
	if !ok || !sameGrants(gunbladerFirst, gunbladerSecond) {
		t.Fatalf("gunblader second awakening must not fabricate free grants: first=%v second=%v", gunbladerFirst, gunbladerSecond)
	}
	thiefFirst, _ := profiles.FreeGrants(6, 3, 1)
	thiefSecond, _ := profiles.FreeGrants(6, 3, 2)
	if containsGrant(thiefFirst, 73) || !containsGrant(thiefSecond, 73) {
		t.Fatalf("thief branch3 second-awakening grant mismatch: first=%v second=%v", thiefFirst, thiefSecond)
	}
	atFighterFirst, _ := profiles.FreeGrants(7, 1, 1)
	atFighterSecond, _ := profiles.FreeGrants(7, 1, 2)
	if containsGrant(atFighterFirst, 242) || !containsGrant(atFighterSecond, 242) {
		t.Fatalf("at-fighter branch1 second-awakening grant mismatch: first=%v second=%v", atFighterFirst, atFighterSecond)
	}
	atSwordMaster, ok := profiles.FreeGrantsAtLevel(catalog, 11, 0x21, 90)
	if !ok || containsGrant(atSwordMaster, 52) || grantLevel(atSwordMaster, 54) <= 0 {
		t.Fatalf("female-slayer sword-master level90 free baseline=%v found=%t", atSwordMaster, ok)
	}
	professionMasteryBranches := 0
	for job := byte(0); job <= 15; job++ {
		if job == 9 || job == 10 { // Dark Knight and Creator are external professions.
			continue
		}
		base, supported := profiles.FreeGrants(job, 0, 0)
		if !supported || containsGrant(base, 197) {
			t.Fatalf("ordinary job=%d base must not own profession mastery skill 197: grants=%v supported=%t", job, base, supported)
		}
		lastBranch := byte(4)
		if job == 8 { // Female mage owns the fifth ordinary class branch in this PVF.
			lastBranch = 5
		}
		for branch := byte(1); branch <= lastBranch; branch++ {
			grants, branchSupported := profiles.FreeGrants(job, branch, 0)
			if !branchSupported || !containsGrant(grants, 197) {
				t.Fatalf("ordinary job=%d branch=%d must grant profession mastery skill 197: grants=%v supported=%t", job, branch, grants, branchSupported)
			}
			professionMasteryBranches++
		}
	}
	if professionMasteryBranches != 57 {
		t.Fatalf("ordinary profession mastery branch count=%d want=57", professionMasteryBranches)
	}
	branchZeroFirstRequest, _ := ParseReward(2, "[awakening type]", []int64{1})
	branchZeroFirst, err := profiles.PlanTransition(9, 0x00, branchZeroFirstRequest)
	if err != nil || branchZeroFirst.NewGrowType != 0x10 {
		t.Fatalf("real PVF branch-zero first awakening = %+v, %v", branchZeroFirst, err)
	}
	branchZeroSecondRequest, _ := ParseReward(3, "[awakening type]", []int64{2})
	branchZeroSecond, err := profiles.PlanTransition(10, 0x10, branchZeroSecondRequest)
	if err != nil || branchZeroSecond.NewGrowType != 0x20 {
		t.Fatalf("real PVF branch-zero second awakening = %+v, %v", branchZeroSecond, err)
	}
	if _, err := profiles.PlanTransition(8, 0x00, branchZeroFirstRequest); err == nil {
		t.Fatal("ordinary branch-zero job unexpectedly exposes awakening capability")
	}
	t.Logf("profession snapshot=%+v fixed_level_skills=%d active_at_90=%d gunblader_class1_awaken1=%v", snapshot, fixedLevelSkills, fixedLevelAt90, gunbladerFirst)
}

func TestRealScriptPVFProfessionGrantFloorsAboveTransitionLevel(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify profession grant floors")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := LoadProfiles(context.Background(), archive, catalog)
	if err != nil {
		t.Fatal(err)
	}

	type professionBoundary struct {
		growType byte
		level    int
	}
	risky := make([]string, 0)
	boundaries := 0
	levelRisks := 0
	prerequisiteRisks := 0
	for rawJob, profile := range profiles.jobs {
		states := []professionBoundary{{growType: 0, level: 1}}
		firstBranch, lastBranch := byte(1), byte(4)
		if rawJob == 9 || rawJob == 10 {
			firstBranch, lastBranch = 0, 0
		} else if rawJob == 8 {
			lastBranch = 5
		}
		for branch := firstBranch; branch <= lastBranch; branch++ {
			if branch > 0 {
				states = append(states, professionBoundary{growType: branch, level: 15})
			}
			for stage := byte(1); stage <= 2; stage++ {
				if !profile.awakeningSupported[branch][stage] {
					continue
				}
				level := 50
				if stage == 2 {
					level = 75
				}
				states = append(states, professionBoundary{growType: stage<<4 | branch, level: level})
			}
		}
		for _, state := range states {
			boundaries++
			grants, ok := profiles.FreeGrantsAtLevel(catalog, rawJob, state.growType, state.level)
			if !ok {
				t.Fatalf("job=%d grow_type=0x%02x boundary=%d has no free baseline", rawJob, state.growType, state.level)
			}
			grantLevels := make(map[uint16]int, len(grants))
			for _, grant := range grants {
				if grant.Level > grantLevels[grant.SkillID] {
					grantLevels[grant.SkillID] = grant.Level
				}
			}
			for _, grant := range grants {
				definition, found := catalog.Find(rawJob, grant.SkillID)
				if !found {
					continue // MissingSkills is audited by TestRealScriptPVFProfessionProfiles.
				}
				if int(definition.RequiredLevel) > state.level {
					levelRisks++
					risky = append(risky, fmt.Sprintf(
						"level:%d/0x%02x/%d:floor%d:req%d:level%d",
						rawJob, state.growType, grant.SkillID, grant.Level, definition.RequiredLevel, state.level,
					))
				}
				for _, prerequisite := range definition.Prerequisites {
					if grantLevels[prerequisite.SkillID] >= prerequisite.Level {
						continue
					}
					prerequisiteRisks++
					risky = append(risky, fmt.Sprintf(
						"prerequisite:%d/0x%02x/%d:floor%d:needs%d@%d:has%d",
						rawJob, state.growType, grant.SkillID, grant.Level,
						prerequisite.SkillID, prerequisite.Level, grantLevels[prerequisite.SkillID],
					))
					break
				}
			}
		}
	}
	sort.Strings(risky)
	if boundaries != 191 || levelRisks != 63 || prerequisiteRisks != 1 || len(risky) != 64 {
		t.Fatalf(
			"profession floor audit boundaries=%d level_risks=%d prerequisite_risks=%d total=%d risks=%v",
			boundaries, levelRisks, prerequisiteRisks, len(risky), risky,
		)
	}
	t.Logf(
		"profession boundaries=%d permanent-floor risks=%d (level=%d prerequisite=%d): %v",
		boundaries, len(risky), levelRisks, prerequisiteRisks, risky,
	)
}

func containsGrant(grants []Grant, skillID uint16) bool {
	for _, grant := range grants {
		if grant.SkillID == skillID {
			return true
		}
	}
	return false
}

func sameGrants(left []Grant, right []Grant) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
