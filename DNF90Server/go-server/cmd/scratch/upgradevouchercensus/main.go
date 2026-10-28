// upgradevouchercensus 只读：统计声明 [upgrade inventory]/[upgrade cargo]/[upgrade account cargo]
// 的可叠加道具，打印 id、名称、[stackable type]、[stack limit]、[action usable place]。
//
// 现场 2026-10-03（服主报）：490701690/490701691/490701692 三件扩容券"使用后没效果"。
// 要判断服务端该按什么条件认这些道具（不能只按那三个 id 硬编码），先看全库这一族长什么样。
//
// 只读。跑法：
//
//	go run ./cmd/scratch/upgradevouchercensus -pvf "...\Script.pvf"
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
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists:    []string{"stackable/stackable.lst"},
		Prefixes: []string{"stackable/"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: build index:", err)
		os.Exit(1)
	}
	listEntries := index.List("stackable/stackable.lst")
	if len(listEntries) == 0 {
		fmt.Fprintln(os.Stderr, "FAILED: stackable.lst is empty")
		os.Exit(1)
	}

	type row struct {
		id       int64
		path     string
		name     string
		kind     string
		limit    int64
		places   string
		actions  []string
		rarity   string
		explain  string
		fileKind string
	}
	rows := make([]row, 0, 64)
	counts := map[string]int{}
	for _, entry := range listEntries {
		text, readErr := archive.ReadText(entry.Path)
		if readErr != nil {
			continue
		}
		doc, parseErr := dnfpvf.Parse(entry.Path, text)
		if parseErr != nil {
			continue
		}
		actions := make([]string, 0, 2)
		for _, value := range doc.Texts("action type") {
			cleaned := strings.ToLower(strings.TrimSpace(strings.Trim(value, "`")))
			for _, target := range []string{"upgrade inventory", "upgrade cargo", "upgrade account cargo"} {
				if cleaned == "["+target+"]" {
					actions = append(actions, target)
				}
			}
		}
		if len(actions) == 0 {
			continue
		}
		for _, action := range actions {
			counts[action]++
		}
		name, _ := doc.Text("name")
		kind, _ := doc.Text("stackable type")
		places, _ := doc.Text("action usable place")
		explain, _ := doc.Text("explain")
		rarity, _ := doc.Text("rarity")
		limit := int64(0)
		if values := doc.Ints("stack limit"); len(values) > 0 {
			limit = values[0]
		}
		rows = append(rows, row{
			id: entry.ID, path: entry.Path,
			name: strings.TrimSpace(strings.Trim(name, "`")), kind: strings.TrimSpace(kind),
			limit: limit, places: strings.Join(strings.Fields(places), " "),
			actions: actions, rarity: strings.TrimSpace(rarity),
			explain: strings.TrimSpace(strings.Trim(explain, "`")),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	fmt.Printf("声明这三种 action type 的可叠加道具：%d 件\n", len(rows))
	for _, action := range []string{"upgrade inventory", "upgrade cargo", "upgrade account cargo"} {
		fmt.Printf("  [%s] : %d\n", action, counts[action])
	}
	kinds := map[string]int{}
	for _, item := range rows {
		kinds[item.kind]++
	}
	kindNames := make([]string, 0, len(kinds))
	for name := range kinds {
		kindNames = append(kindNames, name)
	}
	sort.Strings(kindNames)
	fmt.Printf("\n按 [stackable type] 分布：\n")
	for _, name := range kindNames {
		fmt.Printf("  %-24s %d\n", name, kinds[name])
	}
	fmt.Printf("\n明细：\n")
	for _, item := range rows {
		fmt.Printf("  %-11d %-28s type=%-10s limit=%-4d actions=%v\n", item.id, item.name, item.kind, item.limit, item.actions)
		if item.places != "" {
			fmt.Printf("              usable=%s\n", item.places)
		}
		if item.explain != "" && len(item.explain) < 160 {
			fmt.Printf("              explain=%s\n", item.explain)
		}
	}
}
