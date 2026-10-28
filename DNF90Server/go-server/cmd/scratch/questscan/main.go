// Scratch tool: scan every .qst in a Script.pvf for reward declarations that
// reference one of the account-shared materials (cubes 3033..3037 / 3262,
// souls 10100115 / 10100116 / 10099773..10099775 / 10158124).
//
//	go run ./cmd/scratch/questscan -pvf <Script.pvf>
//
// It reports, per section kind:
//   - monster/enemy reward rows (fixed-width tuples, item id at a known index)
//   - item/count pair sections ([reward int data], [reward selection int data],
//     [clear reward item], [sub reward]) by locating the id in the token stream
//
// Purpose: find every quest-side acquisition path that can hand out a cube or
// soul, and read the declared max_stack for the drop-roll rows.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

var accountSharedIDs = map[int64]string{
	3033: "3033", 3034: "3034", 3035: "3035", 3036: "3036",
	3037: "3037 无色小晶块", 3262: "3262",
	10100115: "10100115", 10100116: "10100116",
	10099773: "10099773", 10099774: "10099774", 10099775: "10099775",
	10158124: "10158124",
}

// pairSections are item/count (or item/count/job) declarations.
var pairSections = []string{
	"reward int data",
	"reward selection int data",
	"clear reward item",
	"sub reward",
}

func main() {
	pvfPath := flag.String("pvf", "../runtime/data/dnf/Script.pvf", "Script.pvf path")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	var (
		scanned   int
		dropRows  []string
		pairHits  []string
		maxStacks = map[int64]int{}
		pairCount = map[string]int{}
	)
	for _, file := range archive.Files() {
		full := strings.ReplaceAll(file.ArchivePath, "\\", "/")
		if strings.TrimSpace(full) == "" {
			full = strings.ReplaceAll(file.Path, "\\", "/")
		}
		if !strings.HasSuffix(strings.ToLower(full), ".qst") {
			continue
		}
		text, readErr := archive.ReadText(full)
		if readErr != nil {
			continue
		}
		scanned++
		doc, parseErr := dnfpvf.Parse(full, text)
		if parseErr != nil {
			continue
		}
		for _, section := range []string{"enemy reward item", "monster reward item"} {
			tokens, ok := doc.Section(section)
			if !ok {
				continue
			}
			width := 8
			if section == "monster reward item" {
				width = 7
			}
			ints := sectionInts(tokens)
			for i := 0; i+width-1 < len(ints); i += width {
				itemIndex := 4
				if section == "monster reward item" {
					itemIndex = 3
				}
				itemID := ints[i+itemIndex]
				if _, ok := accountSharedIDs[itemID]; !ok {
					continue
				}
				maxStacks[ints[i+width-1]]++
				dropRows = append(dropRows, fmt.Sprintf("%s [%s] %v item=%d max_stack=%d",
					full, section, ints[i:i+width], itemID, ints[i+width-1]))
			}
		}
		for _, section := range pairSections {
			tokens, ok := doc.Section(section)
			if !ok {
				continue
			}
			ints := sectionInts(tokens)
			for index, value := range ints {
				name, ok := accountSharedIDs[value]
				if !ok {
					continue
				}
				// 打印 id 前后的 token，看清是 (id,count) 还是别的形状。
				low := index - 2
				if low < 0 {
					low = 0
				}
				high := index + 3
				if high > len(ints) {
					high = len(ints)
				}
				pairCount[section]++
				pairHits = append(pairHits, fmt.Sprintf("%s [%s] idx=%d window=%v (%s)",
					full, section, index, ints[low:high], name))
			}
		}
	}
	sort.Strings(dropRows)
	sort.Strings(pairHits)
	fmt.Printf("scanned .qst=%d\n", scanned)
	fmt.Printf("--- drop-roll rows (enemy/monster reward item) ---\n")
	for _, line := range dropRows {
		fmt.Println(line)
	}
	fmt.Println("--- max_stack histogram (drop-roll rows) ---")
	stacks := make([]int64, 0, len(maxStacks))
	for stack := range maxStacks {
		stacks = append(stacks, stack)
	}
	sort.Slice(stacks, func(a, b int) bool { return stacks[a] < stacks[b] })
	for _, stack := range stacks {
		fmt.Printf("max_stack=%d rows=%d\n", stack, maxStacks[stack])
	}
	fmt.Printf("--- item/count section hits ---\n")
	for _, line := range pairHits {
		fmt.Println(line)
	}
	fmt.Println("--- hits per section ---")
	names := make([]string, 0, len(pairCount))
	for name := range pairCount {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s hits=%d\n", name, pairCount[name])
	}
}

func sectionInts(tokens []dnfpvf.Token) []int64 {
	ints := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			ints = append(ints, token.Int)
		}
	}
	return ints
}
