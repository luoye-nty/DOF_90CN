// attachtypecensus 只读：统计运行库 PVF 里 [attach type] 的取值个数。
//
// 只回答一个问题：`[trade]` 在真实 PVF 里到底存不存在。
// 这决定了把交易白名单收窄成 {free, sealing} 是「只改动测试夹具」还是
// 「会让线上真实物品变成不可交易」。
//
// 用法: go run ./cmd/scratch/attachtypecensus -pvf <Script.pvf>
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

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	total := 0
	byAttach := map[string]int{}
	// 关键取值单独记账：既统计总量，也记下同一个 attach 在 equipment / stackable
	// 两种来源里各有多少，避免只按归一化值看漏分布。
	byKind := map[string]map[string]int{}
	examples := map[string][]string{}

	for _, spec := range []struct{ kind, list string }{
		{"stackable", "stackable/stackable.lst"},
		{"equipment", "equipment/equipment.lst"},
	} {
		text, readErr := archive.ReadText(spec.list)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "list:", spec.list, readErr)
			os.Exit(1)
		}
		document, parseErr := dnfpvf.Parse(spec.list, text)
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, "parse:", spec.list, parseErr)
			os.Exit(1)
		}
		seen := map[int64]bool{}
		for _, entry := range dnfpvf.ParseList(document) {
			if entry.ID <= 0 || seen[entry.ID] {
				continue
			}
			seen[entry.ID] = true
			docPath, docText, readItemErr := readItemText(archive, spec.kind, entry.Path)
			if readItemErr != nil {
				continue
			}
			itemDoc, itemParseErr := dnfpvf.Parse(docPath, docText)
			if itemParseErr != nil {
				continue
			}
			attach, _ := itemDoc.Text("attach type")
			key := normalizeType(attach)
			total++
			byAttach[key]++
			if byKind[key] == nil {
				byKind[key] = map[string]int{}
			}
			byKind[key][spec.kind]++
			if len(examples[key]) < 2 {
				name, _ := itemDoc.Text("name")
				examples[key] = append(examples[key], fmt.Sprintf("%d %s (%s)", entry.ID, name, docPath))
			}
		}
	}

	keys := make([]string, 0, len(byAttach))
	for key := range byAttach {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if byAttach[keys[i]] != byAttach[keys[j]] {
			return byAttach[keys[i]] > byAttach[keys[j]]
		}
		return keys[i] < keys[j]
	})
	fmt.Printf("解析条目总数: %d\n", total)
	fmt.Println("attach_norm\tcount\tequipment\tstackable\texamples")
	for _, key := range keys {
		label := key
		if label == "" {
			label = "(空)"
		}
		fmt.Printf("%s\t%d\t%d\t%d\t%s\n", label, byAttach[key],
			byKind[key]["equipment"], byKind[key]["stackable"], strings.Join(examples[key], " | "))
	}
}
