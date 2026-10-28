// skillprobe dumps the PVF learnability fields for one job/skill pair so the
// grow-type rejection can be checked against the shipped Script.pvf.
package main

import (
	"context"
	"flag"
	"fmt"
	"sort"

	"longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `runtime/data/dnf/Script.pvf`, "Script.pvf path")
	job := flag.Int("job", 2, "job byte")
	skill := flag.Int("skill", 28, "skill id")
	list := flag.Bool("list", false, "list every skill of the job with grow-type fitness")
	quests := flag.Bool("quests", false, "list job-change quests of the job with eligibility")
	level := flag.Int("level", 50, "character level for -quests eligibility")
	grow := flag.Int("grow", 0, "packed grow_type for -quests eligibility")
	onlyClassChange := flag.Bool("classchange", false, "with -quests, print only [job change quest] type 1")
	diff := flag.Bool("diff", false, "list the rows the branch relaxation newly permits and the rows the awakening dimension still blocks")
	stageaudit := flag.Bool("stageaudit", false, "distribution of [skill fitness second growtype] across the whole PVF")
	flag.Parse()

	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: *pvfPath})
	if err != nil {
		panic(err)
	}
	index, err := pvf.Build(context.Background(), archive, pvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		panic(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		panic(err)
	}

	if *list {
		rows := make([]dnfskill.Skill, 0)
		for _, definition := range catalog.Skills() {
			if int(definition.Job) == *job {
				rows = append(rows, definition)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		for _, d := range rows {
			fmt.Printf("skill=%-5d lvl=%-3d growTypes=%v growMax=%v secondGrow=%v secondMax=%v supports0=%t supports1=%t supportsC=%t name=%s\n",
				d.ID, d.RequiredLevel, d.GrowTypes, d.GrowTypeMaximumLevels,
				d.SecondGrowTypes, d.SecondGrowTypeMaximumLevels,
				d.SupportsGrowType(0), d.SupportsGrowType(1), d.SupportsCharacterGrowth(0), d.Name)
		}
		return
	}

	if *diff {
		// Report what the tree-authority rule changes for one job/grow type:
		// rows newly learnable through the relaxed profession-branch dimension,
		// and rows still blocked by the awakening-stage dimension.
		first := *grow & 0x0f
		stage := (*grow >> 4) & 0x0f
		newly := make([]dnfskill.Skill, 0)
		blocked := make([]dnfskill.Skill, 0)
		for _, definition := range catalog.Skills() {
			if int(definition.Job) != *job {
				continue
			}
			if !definition.SupportsCharacterAwakening(*grow) {
				// The awakening dimension still gates the purchase, so the row
				// stays unlearnable no matter how the tree renders it.
				if definition.RequiredLevel <= *level {
					blocked = append(blocked, definition)
				}
				continue
			}
			if definition.MaximumLearnableLevelForTree(*level, *grow) > definition.MaximumLearnableLevel(*level, *grow) {
				newly = append(newly, definition)
			}
		}
		sort.Slice(newly, func(i, j int) bool { return newly[i].ID < newly[j].ID })
		sort.Slice(blocked, func(i, j int) bool { return blocked[i].ID < blocked[j].ID })
		fmt.Printf("job=%d level=%d growType=0x%02X (branch=%d awakeningStage=%d)\n", *job, *level, *grow, first, stage)
		fmt.Printf("  newly learnable through the branch relaxation = %d\n", len(newly))
		for _, d := range newly {
			fmt.Printf("    skill=%-5d lvl=%-3d tree=%-3d old=%-3d growTypes=%v secondGrow=%v name=%q\n",
				d.ID, d.RequiredLevel,
				d.MaximumLearnableLevelForTree(*level, *grow), d.MaximumLearnableLevel(*level, *grow),
				d.GrowTypes, d.SecondGrowTypes, d.Name)
		}
		fmt.Printf("  still blocked by the awakening dimension = %d\n", len(blocked))
		for _, d := range blocked {
			fmt.Printf("    skill=%-5d lvl=%-3d secondGrow=%v secondMax=%v name=%q\n",
				d.ID, d.RequiredLevel, d.SecondGrowTypes, d.SecondGrowTypeMaximumLevels, d.Name)
		}
		return
	}

	if *stageaudit {
		// Distribution of the awakening-stage dimension across the whole PVF, so
		// the stage gate can be checked against the shipped document.
		counts := make(map[int]int)
		rows := make([]dnfskill.Skill, 0)
		for _, definition := range catalog.Skills() {
			if len(definition.SecondGrowTypes) == 0 {
				continue
			}
			minimum := 0xff
			for _, stage := range definition.SecondGrowTypes {
				if stage >= 0 && stage < minimum {
					minimum = stage
				}
			}
			counts[minimum]++
			rows = append(rows, definition)
		}
		stages := make([]int, 0, len(counts))
		for stage := range counts {
			stages = append(stages, stage)
		}
		sort.Ints(stages)
		fmt.Printf("skills declaring [skill fitness second growtype] = %d\n", len(rows))
		for _, stage := range stages {
			fmt.Printf("  minimum awakening stage %d -> %d skills\n", stage, counts[stage])
		}
		for stage := range counts {
			if stage < 2 {
				continue
			}
			fmt.Printf("  --- stage >= %d examples ---\n", stage)
			shown := 0
			for _, d := range rows {
				minimum := 0xff
				for _, s := range d.SecondGrowTypes {
					if s >= 0 && s < minimum {
						minimum = s
					}
				}
				if minimum != stage {
					continue
				}
				fmt.Printf("    job=%-3d skill=%-5d lvl=%-3d secondGrow=%v name=%q\n",
					d.Job, d.ID, d.RequiredLevel, d.SecondGrowTypes, d.Name)
				shown++
				if shown >= 8 {
					break
				}
			}
		}
		return
	}

	if path := flag.Arg(0); path != "" {
		text, err := archive.ReadText(path)
		if err != nil {
			panic(err)
		}
		fmt.Printf("=== raw %s ===\n%s\n", path, text)
		return
	}

	if *quests {
		questIndex, err := pvf.Build(context.Background(), archive, pvf.BuildOptions{
			Lists:    []string{dnfquest.DefaultList},
			Prefixes: []string{"n_quest/"},
		})
		if err != nil {
			panic(err)
		}
		questCatalog, err := dnfquest.Load(context.Background(), questIndex)
		if err != nil {
			panic(err)
		}
		eligibility := dnfquest.CharacterEligibility{
			Level: *level, Job: *job, GrowType: *grow,
		}
		result := questCatalog.Acceptable(eligibility, dnfrepo.QuestRecord{})
		acceptable := make(map[int64]struct{}, len(result.IDs))
		for _, id := range result.IDs {
			acceptable[int64(id)] = struct{}{}
		}
		fmt.Printf("job=%d level=%d growType=%d acceptable=%d\n", *job, *level, *grow, len(result.IDs))
		byChange := map[int]int{}
		for _, definition := range questCatalog.Definitions() {
			change := definition.JobChangeQuest
			if change < 1 || change > 20 {
				continue
			}
			byChange[change]++
			if *onlyClassChange && change != 1 {
				continue
			}
			_, ok := acceptable[definition.ID]
			fmt.Printf("  quest=%-6d jobChange=%-2d level=%d-%d job=%q target=%q rules=%v acceptable=%t path=%s reward=%q%v name=%q\n",
				definition.ID, change, definition.LevelMin, definition.LevelMax,
				definition.Job, definition.TargetCharacter, definition.TargetCharacterRules, ok,
				definition.Path, definition.RewardType, definition.RewardIntData, definition.Name)
		}
		counts := make([]int, 0, len(byChange))
		for change := range byChange {
			counts = append(counts, change)
		}
		sort.Ints(counts)
		for _, change := range counts {
			fmt.Printf("  [jobChange=%d definitions=%d]\n", change, byChange[change])
		}
		return
	}

	definition, ok := catalog.Find(byte(*job), uint16(*skill))
	if !ok {
		fmt.Printf("job=%d skill=%d NOT FOUND\n", *job, *skill)
		return
	}
	fmt.Printf("job=%d skill=%d name=%q path=%s\n", definition.Job, definition.ID, definition.Name, definition.Path)
	fmt.Printf("  requiredLevel=%d active=%t skillClass=%d kind=%q\n",
		definition.RequiredLevel, definition.Active, definition.SkillClass, definition.Kind)
	fmt.Printf("  growTypes=%v\n", definition.GrowTypes)
	fmt.Printf("  growTypeMaximumLevels=%v\n", definition.GrowTypeMaximumLevels)
	fmt.Printf("  secondGrowTypes=%v\n", definition.SecondGrowTypes)
	fmt.Printf("  secondGrowTypeMaximumLevels=%v\n", definition.SecondGrowTypeMaximumLevels)
	fmt.Printf("  learnableForTree(level=%d, growType=0x%02X)=%d\n",
		*level, *grow, definition.MaximumLearnableLevelForTree(*level, *grow))
	fmt.Printf("  learnableFitnessAware(level=%d, growType=0x%02X)=%d\n",
		*level, *grow, definition.MaximumLearnableLevel(*level, *grow))
	for branch := 0; branch <= 4; branch++ {
		fmt.Printf("  branch=%d supportsGrowType=%t\n", branch, definition.SupportsGrowType(branch))
	}
	for _, packed := range []int{0, 1, 0x11, 0x21, 0x31} {
		fmt.Printf("  packedGrowType=0x%02X supportsCharacterGrowth=%t\n", packed, definition.SupportsCharacterGrowth(packed))
	}
}
