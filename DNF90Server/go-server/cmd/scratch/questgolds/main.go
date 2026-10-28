// questgolds 只读扫描 Script.pvf 里的任务定义，统计"哪些任务要求交付金币"。
//
// 现场 2026-10-03（服主报）：所有消耗金币的任务都不扣钱。开耳环槽位的任务 [int data] 是
//
//	0 1000000 3034 1000 3033 1000 3035 1000 3036 1000 10158964 5
//
// 服主给定口径：**[int data] 是 (ID, 数量) 成对，ID 0 = 金币。**
// 服务端把 itemID <= 0 的 pair 当非法数据跳过了，所以 (0, 1000000) 整条被丢掉 —— 金币不扣。
//
// 本工具用来量范围：全库有多少任务在交付里要金币、合计多少。
// 注意 quest.lst 里不少条目指向的文档在本 PVF 中并不存在（例如 2636 →
// new_quest/MetroCenter/system_90_earring_02.qst），所以按**实际存在的 .qst 文档**扫。
//
// 只读。跑法：
//
//	go run ./cmd/scratch/questgolds -pvf "D:\...\runtime\data\dnf\Script.pvf"
//	go run ./cmd/scratch/questgolds -pvf ... -file "n_quest/....qst"   # 打印单个文档
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf (required)")
	questFile := flag.String("file", "", "optional: dump one .qst document verbatim")
	verbose := flag.Bool("v", false, "also print the full [int data] of every gold quest")
	limit := flag.Int("limit", 40, "max rows to print")
	flag.Parse()
	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -pvf is required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: open pvf:", err)
		os.Exit(1)
	}
	if *questFile != "" {
		text, readErr := archive.ReadText(*questFile)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "FAILED: read:", readErr)
			os.Exit(1)
		}
		fmt.Printf("=== %s ===\n%s\n", *questFile, text)
		return
	}

	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists:    []string{"n_quest/quest.lst"},
		Prefixes: []string{"n_quest/", "quest/"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: build index:", err)
		os.Exit(1)
	}
	paths := make([]string, 0, 16384)
	for _, path := range index.Paths() {
		if strings.HasSuffix(strings.ToLower(path), ".qst") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	fmt.Printf("扫描 .qst 文档=%d\n", len(paths))

	type hit struct {
		path   string
		kind   string
		source string
		values []int64
	}
	hits := make([]hit, 0, 256)
	goldOnly := 0
	var goldTotal, goldMax int64
	byType := map[string]int{}
	goldPair := func(values []int64) bool {
		for index := 0; index+1 < len(values); index += 2 {
			if values[index] == 0 && values[index+1] > 0 {
				return true
			}
		}
		return false
	}
	goldSum := func(values []int64) int64 {
		total := int64(0)
		for index := 0; index+1 < len(values); index += 2 {
			if values[index] == 0 && values[index+1] > 0 {
				total += values[index+1]
			}
		}
		return total
	}
	for _, path := range paths {
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			continue
		}
		doc, parseErr := dnfpvf.Parse(path, text)
		if parseErr != nil {
			continue
		}
		kind, _ := doc.Text("type")
		kind = strings.TrimSpace(kind)
		intData := doc.Ints("int data")
		dependData := doc.Ints("depend give item")
		for _, section := range []struct {
			name   string
			values []int64
		}{
			{name: "[int data]", values: intData},
			{name: "[depend give item]", values: dependData},
		} {
			if !goldPair(section.values) {
				continue
			}
			hits = append(hits, hit{path: path, kind: kind, source: section.name, values: section.values})
			if section.name == "[int data]" {
				byType[kind]++
			}
			goldOnly++
			goldTotal += goldSum(section.values)
			if amount := goldSum(section.values); amount > goldMax {
				goldMax = amount
			}
		}
	}
	fmt.Printf("交付段里含 (0, 金额) 金币消耗的任务文档=%d\n", len(hits))
	fmt.Printf("金币总额=%d，单笔最大=%d\n", goldTotal, goldMax)
	types := make([]string, 0, len(byType))
	for name := range byType {
		types = append(types, name)
	}
	sort.Slice(types, func(i, j int) bool { return byType[types[i]] > byType[types[j]] })
	bySource := map[string]int{}
	for _, item := range hits {
		bySource[item.source]++
	}
	fmt.Printf("\n按所在段分布（决定语义的是这里）：\n")
	sources := make([]string, 0, len(bySource))
	for name := range bySource {
		sources = append(sources, name)
	}
	sort.Strings(sources)
	for _, name := range sources {
		fmt.Printf("  %-22s %d\n", name, bySource[name])
	}
	fmt.Printf("\n按 [type] 分布（仅 [int data]）：\n")
	for _, name := range types {
		fmt.Printf("  %-24s %d\n", name, byType[name])
	}
	fmt.Printf("\n明细（前 %d 条）：\n", *limit)
	for index, item := range hits {
		if index >= *limit {
			fmt.Printf("  ...（还有 %d 条）\n", len(hits)-index)
			break
		}
		fmt.Printf("  %-20s type=%-22s %s\n", item.source, item.kind, item.path)
		if *verbose {
			fmt.Printf("      [int data]=%v\n", item.values)
		}
	}
	if goldOnly > 0 {
		fmt.Printf("\n纯金币交付样例：\n")
		shown := 0
		for _, item := range hits {
			if len(item.values) != 2 {
				continue
			}
			fmt.Printf("  金币 %-10d %s\n", item.values[1], item.path)
			shown++
			if shown >= 10 {
				break
			}
		}
	}
}
