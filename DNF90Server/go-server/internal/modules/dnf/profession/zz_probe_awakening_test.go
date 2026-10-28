package profession

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestZZProbeAwakeningStages(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE")
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
	maxStage := byte(0)
	for job, profile := range profiles.jobs {
		for branch, stages := range profile.awakeningSupported {
			for stage := range stages {
				if stage > maxStage {
					maxStage = stage
				}
			}
			_ = branch
		}
		fmt.Printf("PROBE job=%d branchesWithAwakening=%d\n", job, len(profile.awakeningSupported))
		keys := make([]int, 0, len(profile.awakeningSupported))
		for branch := range profile.awakeningSupported {
			keys = append(keys, int(branch))
		}
		sort.Ints(keys)
		for _, branch := range keys {
			stages := make([]int, 0, 4)
			for stage := range profile.awakeningSupported[byte(branch)] {
				stages = append(stages, int(stage))
			}
			sort.Ints(stages)
			fmt.Printf("PROBE   job=%d branch=%d stages=%v\n", job, branch, stages)
		}
	}
	fmt.Printf("PROBE maxSupportedAwakeningStage=%d\n", maxStage)

	stageHistogram := map[int]int{}
	globalMax := 0
	for _, definition := range catalog.Skills() {
		for _, stage := range definition.SecondGrowTypes {
			stageHistogram[stage]++
			if stage > globalMax {
				globalMax = stage
			}
		}
	}
	fmt.Printf("PROBE skillSecondGrowType(max=%d) histogram=%v\n", globalMax, stageHistogram)
}
