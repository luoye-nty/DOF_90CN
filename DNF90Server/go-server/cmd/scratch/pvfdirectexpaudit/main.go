package main

import (
	"flag"
	"fmt"
	"path"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type row struct {
	itemID int64
	path   string
	name   string
	typ    string
	values []int64
}

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	flag.Parse()
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	listText, err := archive.ReadText("stackable/stackable.lst")
	if err != nil {
		panic(err)
	}
	list, err := dnfpvf.Parse("stackable/stackable.lst", listText)
	if err != nil {
		panic(err)
	}
	rows := make([]row, 0, 256)
	counts := make(map[string]int)
	for _, entry := range dnfpvf.ParseList(list) {
		reference := strings.TrimSpace(strings.Trim(entry.Path, "`\"'"))
		var actual string
		for _, candidate := range []string{reference, path.Join("stackable", reference)} {
			if file, found := archive.FindFile(candidate); found {
				actual = file.ArchivePath
				break
			}
		}
		if actual == "" {
			continue
		}
		text, readErr := archive.ReadText(actual)
		if readErr != nil {
			continue
		}
		document, parseErr := dnfpvf.Parse(actual, text)
		if parseErr != nil {
			continue
		}
		typ, found := document.Text("increase status type")
		if !found {
			continue
		}
		typ = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(typ, "`", "")))
		switch typ {
		case "[expup]", "[exppercentup]", "[expupbytable]", "[levelupbytable]":
		default:
			continue
		}
		name, _ := document.Text("name")
		rows = append(rows, row{itemID: entry.ID, path: actual, name: name, typ: typ, values: document.Ints("increase status type")})
		counts[typ]++
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].itemID < rows[j].itemID })
	for _, item := range rows {
		fmt.Printf("%d\t%s\t%v\t%s\t%s\n", item.itemID, item.typ, item.values, item.name, item.path)
	}
	fmt.Printf("TOTAL=%d COUNTS=%v\n", len(rows), counts)
}
