package main

import (
	"fmt"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func name_of(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func main() {
	archive, err := platformpvf.Open(`DNF_REAL_PVF`)
	if err != nil {
		panic(err)
	}
	paths := []string{}
	archive.RangePaths(func(p string) bool {
		if strings.HasSuffix(strings.ToLower(p), ".equ") {
			paths = append(paths, p)
		}
		return true
	})
	sort.Strings(paths)
	blocks := 0
	noGrow := 0
	orderBad := 0
	nestedLevelupInGroup := 0
	var noGrowList []string
	var orderBadList []string
	containers := 0
	for _, p := range paths {
		text, err := archive.ReadText(p)
		if err != nil {
			continue
		}
		if !strings.Contains(text, "[all skill item]") {
			continue
		}
		doc, perr := dnfpvf.Parse(p, text)
		if perr != nil {
			continue
		}
		depth := 0
		curHasGrow := false
		curHasApply := false
		curApplyBeforeGrow := false
		curStart := 0
		curLevelup := false
		for si, sec := range doc.Sections {
			n := name_of(sec.Name)
			switch n {
			case "all skill item container":
				containers++
			case "all skill item":
				depth++
				blocks++
				curHasGrow, curHasApply, curApplyBeforeGrow, curLevelup = false, false, false, false
				curStart = si
			case "/all skill item":
				if depth > 0 {
					if !curHasGrow {
						noGrow++
						if len(noGrowList) < 25 {
							beg := doc.Sections[curStart].Start
							end := sec.Start
							if end > beg+220 {
								end = beg + 220
							}
							raw := ""
							for _, tk := range doc.Tokens[beg:end] {
								raw += tk.Raw + " "
							}
							noGrowList = append(noGrowList, fmt.Sprintf("%s :: %s", p, strings.ReplaceAll(raw, "\n", " ")))
						}
					}
					if curApplyBeforeGrow {
						orderBad++
						if len(orderBadList) < 10 {
							orderBadList = append(orderBadList, p)
						}
					}
					if curLevelup {
						nestedLevelupInGroup++
					}
					depth--
				}
			case "item growtype":
				if depth > 0 {
					curHasGrow = true
					if curHasApply {
						curApplyBeforeGrow = true
					}
				}
			case "skill apply condition":
				if depth > 0 {
					curHasApply = true
				}
			case "skill levelup":
				if depth > 0 {
					curLevelup = true
				}
			}
		}
	}
	fmt.Printf("containers=%d blocks=%d noGrowBlock=%d applyBeforeGrow=%d levelupInsideGroup=%d\n", containers, blocks, noGrow, orderBad, nestedLevelupInGroup)
	fmt.Println("### blocks without [item growtype]:")
	for _, s := range noGrowList {
		fmt.Println("  " + s)
	}
	fmt.Println("### apply-before-grow files:")
	for _, s := range orderBadList {
		fmt.Println("  " + s)
	}
}
