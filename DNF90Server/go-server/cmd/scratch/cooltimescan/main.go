// Command cooltimescan lists every stackable whose PVF declares
// [cooltime maintenance] (the field that makes the cooldown server-maintained),
// with its [cool time] and [cooltime group]. Temporary scratch tool.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const stackableList = "stackable/stackable.lst"

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	flag.Parse()
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		log.Fatal(err)
	}
	pathToID := map[string]int64{}
	if text, err := archive.ReadText(stackableList); err == nil {
		if doc, err := pvf.Parse(stackableList, text); err == nil {
			for _, entry := range pvf.ParseList(doc) {
				clean := strings.ToLower(strings.TrimSpace(entry.Path))
				clean = strings.ReplaceAll(clean, "\\", "/")
				clean = strings.Trim(clean, "`")
				if entry.ID <= 0 || entry.ID > math.MaxUint32 || clean == "" {
					continue
				}
				pathToID["stackable/"+clean] = entry.ID
			}
		}
	}
	namePattern := regexp.MustCompile("(?m)^\\s*\\[name\\]\\s+`([^`]*)`")
	coolTimePattern := regexp.MustCompile(`(?m)^\s*\[cool time\]\s+(\d+)`)
	groupPattern := regexp.MustCompile(`(?m)^\s*\[cooltime group\]\s+(\d+)`)
	maintenancePattern := regexp.MustCompile(`(?m)^\s*\[cooltime maintenance\]\s*([^\r\n]*)`)
	boosterPattern := regexp.MustCompile(`(?m)^\s*\[booster info\]`)

	type row struct {
		id, coolTime, group int64
		maintenance, name   string
		booster             bool
		path                string
	}
	rows := make([]row, 0, 64)
	archive.RangePaths(func(path string) bool {
		lower := strings.ToLower(path)
		if !strings.HasSuffix(lower, ".stk") {
			return true
		}
		text, err := archive.ReadText(path)
		if err != nil || !maintenancePattern.MatchString(text) {
			return true
		}
		entry := row{path: path}
		if match := maintenancePattern.FindStringSubmatch(text); len(match) == 2 {
			entry.maintenance = strings.TrimSpace(match[1])
		}
		if match := coolTimePattern.FindStringSubmatch(text); len(match) == 2 {
			fmt.Sscanf(match[1], "%d", &entry.coolTime)
		}
		if match := groupPattern.FindStringSubmatch(text); len(match) == 2 {
			fmt.Sscanf(match[1], "%d", &entry.group)
		}
		if match := namePattern.FindStringSubmatch(text); len(match) == 2 {
			entry.name = match[1]
		}
		entry.booster = boosterPattern.MatchString(text)
		entry.id = pathToID[lower]
		rows = append(rows, entry)
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].coolTime > rows[j].coolTime })
	fmt.Printf("documents with [cooltime maintenance] = %d (stackable.lst entries=%d)\n", len(rows), len(pathToID))
	for _, entry := range rows {
		fmt.Printf("item=%-12d group=%-4d cooltime_ms=%-12d booster=%-5t maintenance=%q name=%q path=%s\n",
			entry.id, entry.group, entry.coolTime, entry.booster, entry.maintenance, entry.name, entry.path)
	}
	_ = context.Background()
}
