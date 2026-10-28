// epicpotioncensus 只读：把"城镇增益药剂"这条判据**逐条复刻**出来，枚举全库到底
// 哪些道具会被放行、哪些只是被消耗、哪些仍然会被拒。
//
// 为什么要单独写这个工具：服主问"所有史诗药剂均可使用？是怎么判断为史诗药剂的？"。
// 判据里没有"史诗"两个字、也没有任何 item id，全是 PVF 形状；所以答案必须是一张
// 从真 Script.pvf 算出来的清单，而不是口头描述。
//
// 判据（与 stackable_use_pvf.go 的 currentStatChangePotionDefinition / 解析分支逐字对应）：
//
//	(1) [stackable type] 去反引号小写后 == "[expert town potion]"
//	(2) [action type] 不存在或全空白
//	(3) [stat change duration] > 0
//	(4) [action usable place] 里有一项 == "[village]"（不满足 ⇒ 报错 fail closed，不是静默放行）
//
// 另外：说明文字里含"掉落率" ⇒ 服务端把深渊派对史诗掉落率按 200% 放大；否则只消耗。
//
// 跑法：
//
//	go run ./cmd/scratch/epicpotioncensus -pvf "...\Script.pvf"
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

type row struct {
	id       int64
	path     string
	name     string
	explain  string
	seconds  int64
	percent  int64
	places   string
	hasPlace bool
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf (required)")
	max := flag.Int("max", 60, "max admitted rows to print")
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
	entries := index.List("stackable/stackable.lst")
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "FAILED: stackable.lst is empty")
		os.Exit(1)
	}

	admitted := make([]row, 0, 64)
	// 近似但被某一票否决的：用来证明判据不是"全放行"。
	rejectedByAction := map[string]int{}
	rejectedNoDuration := 0
	townPotionTotal := 0
	for _, entry := range entries {
		text, readErr := archive.ReadText(entry.Path)
		if readErr != nil {
			continue
		}
		doc, parseErr := dnfpvf.Parse(entry.Path, text)
		if parseErr != nil {
			continue
		}
		stackableType := normalize(docText(doc, "stackable type"))
		if stackableType != "[expert town potion]" {
			continue
		}
		townPotionTotal++
		name := docText(doc, "name")
		explain := docText(doc, "explain")
		actionTypes := make([]string, 0, 1)
		for _, value := range doc.Texts("action type") {
			if trimmed := strings.TrimSpace(strings.Trim(value, "`")); trimmed != "" {
				actionTypes = append(actionTypes, strings.ToLower(trimmed))
			}
		}
		if len(actionTypes) > 0 {
			for _, action := range actionTypes {
				rejectedByAction[action]++
			}
			continue
		}
		duration := int64(0)
		if values := doc.Ints("stat change duration"); len(values) > 0 && values[0] > 0 {
			duration = values[0]
		}
		if duration <= 0 {
			rejectedNoDuration++
			continue
		}
		village := false
		places := make([]string, 0, 2)
		for _, value := range doc.Texts("action usable place") {
			trimmed := strings.TrimSpace(strings.Trim(value, "`"))
			places = append(places, trimmed)
			if strings.EqualFold(trimmed, "[village]") {
				village = true
			}
		}
		candidate := row{
			id: entry.ID, path: entry.Path,
			name:    strings.TrimSpace(strings.Trim(name, "`")),
			explain: strings.Join(strings.Fields(strings.Trim(explain, "`")), " "),
			seconds: duration / 1000,
			places:  strings.Join(places, " "), hasPlace: village,
		}
		if len(places) == 0 {
			candidate.places = "(未声明)"
		}
		// 地点字段是"限制"不是"证据"：缺字段、写 [village]、写 [dungeon] 一律放行，
		// 客户端按同一份 PVF 自己判地点（生产代码同口径，见 stackable_use_pvf.go）。
		if strings.Contains(candidate.explain, "掉落率") {
			candidate.percent = 200
		}
		admitted = append(admitted, candidate)
	}
	sort.Slice(admitted, func(i, j int) bool { return admitted[i].id < admitted[j].id })

	withDrop := 0
	for _, item := range admitted {
		if item.percent > 0 {
			withDrop++
		}
	}
	fmt.Printf("=== 判据命中：%d 件（其中影响掉落率 %d 件 / 只消耗 %d 件）===\n\n",
		len(admitted), withDrop, len(admitted)-withDrop)
	limit := *max
	if len(admitted) < limit {
		limit = len(admitted)
	}
	for _, item := range admitted[:limit] {
		fmt.Printf("%-10d %-28s %4ds  掉落率=%-3d  %s\n",
			item.id, item.name, item.seconds, item.percent, truncate(item.explain, 46))
	}
	if len(admitted) > limit {
		fmt.Printf("… 还有 %d 件未打印（-max 调整）\n", len(admitted)-limit)
	}

	fmt.Printf("\n=== 近似但**没有**放行的（证明判据不是全放行）===\n")
	fmt.Printf("[expert town potion] 总数：%d\n", townPotionTotal)
	fmt.Printf("  有 [action type]（走各自通道）：%d\n", sum(rejectedByAction))
	actions := make([]string, 0, len(rejectedByAction))
	for action := range rejectedByAction {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	for _, action := range actions {
		fmt.Printf("      %-40s x%d\n", action, rejectedByAction[action])
	}
	fmt.Printf("  无 action、无 [stat change duration]：%d 件（没有自证的限时效果，不放行）\n", rejectedNoDuration)
	fmt.Printf("  （判据不要求 [action usable place]：缺字段 / [village] / [dungeon] 一律放行，\n")
	fmt.Printf("    客户端按同一份 PVF 自己判地点）\n")
}

func docText(doc *dnfpvf.Document, key string) string {
	value, _ := doc.Text(key)
	return value
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
}

func truncate(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[:n]) + "…"
}

func sum(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
