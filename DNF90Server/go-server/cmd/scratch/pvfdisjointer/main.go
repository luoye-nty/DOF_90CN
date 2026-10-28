// Loads the auxiliary-profession catalog out of Script.pvf and reports what the
// disjointer loader actually parsed, then simulates PlanDisjointer across the
// machine-grade / rarity / state matrix so the produced materials can be checked
// against the archive definition field by field.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	archivePath := `runtime/data/dnf/Script.pvf`
	if len(os.Args) > 1 {
		archivePath = os.Args[1]
	}
	archive, err := platformpvf.Open(archivePath)
	if err != nil {
		panic(err)
	}
	if needle := os.Getenv("FINDSTACK"); needle != "" {
		// Resolve stackable items by name substring.
		lst, lstErr := archive.ReadText("stackable/stackable.lst")
		if lstErr != nil {
			panic(lstErr)
		}
		fields := strings.Fields(lst)
		hits := 0
		for i := 0; i+1 < len(fields) && hits < 40; i += 2 {
			rel := strings.Trim(fields[i+1], "`")
			text, readErr := archive.ReadText("stackable/" + rel)
			if readErr != nil || !strings.Contains(text, needle) {
				continue
			}
			doc, parseErr := dnfpvf.Parse(rel, text)
			if parseErr != nil {
				continue
			}
			name, _ := doc.Text("name")
			if !strings.Contains(name, needle) {
				continue
			}
			fmt.Printf("id=%-10s %s\n", fields[i], name)
			hits++
		}
		fmt.Printf("hits=%d\n", hits)
		return
	}
	if needle := os.Getenv("FINDEQUIP"); needle != "" {
		// Resolve a known equipment by name substring and report its [rarity],
		// which is the only way to name a numeric tier from evidence.
		lst, lstErr := archive.ReadText("equipment/equipment.lst")
		if lstErr != nil {
			panic(lstErr)
		}
		fields := strings.Fields(lst)
		hits := 0
		for i := 0; i+1 < len(fields) && hits < 12; i += 2 {
			rel := strings.Trim(fields[i+1], "`")
			text, readErr := archive.ReadText("equipment/" + rel)
			if readErr != nil || !strings.Contains(text, needle) {
				continue
			}
			doc, parseErr := dnfpvf.Parse(rel, text)
			if parseErr != nil {
				continue
			}
			name, _ := doc.Text("name")
			rarity, _ := doc.Int("rarity")
			level, _ := doc.Int("minimum level")
			fmt.Printf("id=%-9s rarity=%d level=%-3d %s\n", fields[i], rarity, level, name)
			hits++
		}
		fmt.Printf("hits=%d\n", hits)
		return
	}
	if os.Getenv("RARITY") != "" {
		// Sample equipment names per [rarity] value so the numeric tier can be
		// named without guessing.
		lst, lstErr := archive.ReadText("equipment/equipment.lst")
		if lstErr != nil {
			panic(lstErr)
		}
		fields := strings.Fields(lst)
		samples := map[int64][]string{}
		for i := 0; i+1 < len(fields); i += 2 {
			if len(samples) >= 7 {
				full := true
				for _, v := range samples {
					if len(v) < 3 {
						full = false
					}
				}
				if full {
					break
				}
			}
			rel := strings.Trim(fields[i+1], "`")
			text, readErr := archive.ReadText("equipment/" + rel)
			if readErr != nil {
				continue
			}
			doc, parseErr := dnfpvf.Parse(rel, text)
			if parseErr != nil {
				continue
			}
			rarity, ok := doc.Int("rarity")
			if !ok || len(samples[rarity]) >= 4 {
				continue
			}
			// Endgame gear only: low-level rows are dominated by avatars and
			// costume pieces whose names say nothing about the tier.
			if level, hasLevel := doc.Int("minimum level"); !hasLevel || level < 85 {
				continue
			}
			name, _ := doc.Text("name")
			samples[rarity] = append(samples[rarity], name)
		}
		keys := make([]int, 0, len(samples))
		for k := range samples {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		for _, k := range keys {
			fmt.Printf("rarity %d -> %v\n", k, samples[int64(k)])
		}
		return
	}
	if needle := os.Getenv("GREPALL"); needle != "" {
		// Locate a UI/message string anywhere in the archive. PVF entries are
		// stored encoded, so a raw byte search over Script.pvf finds nothing;
		// every file has to be decoded before matching.
		hits := 0
		for _, file := range archive.Files() {
			text, readErr := archive.FileText(file.Index)
			if readErr != nil || !strings.Contains(text, needle) {
				continue
			}
			for _, line := range strings.Split(text, "\n") {
				if !strings.Contains(line, needle) {
					continue
				}
				fmt.Printf("%s\t%s\n", file.Path, strings.TrimSpace(line))
				hits++
				if hits >= 60 {
					break
				}
			}
			if hits >= 60 {
				break
			}
		}
		fmt.Printf("hits=%d\n", hits)
		return
	}
	if raw := os.Getenv("RAW"); raw != "" {
		// RAW=1 keeps the original disjointer dump; any other value is read as
		// an archive-relative path so other definitions can be inspected too.
		path := "character/expertjob/disjointer.exj"
		if raw != "1" {
			path = raw
		}
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			panic(readErr)
		}
		fmt.Print(text)
		return
	}
	catalog, err := dnfexpertjob.Load(context.Background(), archive)
	if err != nil {
		panic(err)
	}
	config, ok := catalog.Disjointer()
	if !ok {
		panic("disjointer config missing")
	}

	fmt.Printf("initialEndurance=%d baseConst=%v enduranceReduce=[%d,%d] gainExp=[%d,%d]\n",
		config.InitialEndurance, config.BaseConst,
		config.EnduranceReduceMin, config.EnduranceReduceMax,
		config.ExperienceGainMin, config.ExperienceGainMax)
	fmt.Printf("selfService chance=%d item=%d count=%d\n",
		config.SelfServiceChance, config.SelfServiceItemID, config.SelfServiceItemCount)
	fmt.Printf("results=%d additionalTables=%d bigWinTables=%d\n\n",
		len(config.Results), len(config.AdditionalResults), len(config.BigWinResults))

	dumpTables := func(label string, tables map[int][]dnfexpertjob.DisjointerSelectionRule) {
		keys := make([]int, 0, len(tables))
		for k := range tables {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		fmt.Printf("-- %s --\n", label)
		for _, k := range keys {
			fmt.Printf("  table %d:\n", k)
			for _, r := range tables[k] {
				fmt.Printf("    grade %d..%-2d item=%-9d weight=%-5d countDivisor=%v\n",
					r.MinimumGrade, r.MaximumGrade, r.ItemID, r.Weight, r.CountDivisor)
			}
		}
		fmt.Println()
	}
	dumpTables("additional result", config.AdditionalResults)
	dumpTables("big win result", config.BigWinResults)

	// Deterministic rng so the matrix is reproducible: always take the first
	// eligible weighted row and never roll a big win.
	rng := func(n int) int { return 0 }
	fmt.Println("-- PlanDisjointer matrix (equipmentGrade=90, sellGold=10000, deterministic rng) --")
	for grade := 1; grade <= 11; grade++ {
		for rarity := 0; rarity <= 6; rarity++ {
			for state := 0; state <= 2; state++ {
				meta := dnfexpertjob.EquipmentMetadata{
					Rarity: rarity, Level: 90, Grade: 90, State: state, SellGold: 10000,
				}
				plan, planErr := catalog.PlanDisjointer(100, grade, meta, false, rng)
				if planErr != nil {
					continue
				}
				fmt.Printf("  grade=%-2d rarity=%d state=%d ->", grade, rarity, state)
				for _, m := range plan.Materials {
					fmt.Printf(" %d x%d", m.ItemID, m.Count)
				}
				fmt.Printf("  (endurance-%d exp+%d)\n", plan.EnduranceReduction, plan.ExperienceGain)
			}
		}
	}
}
