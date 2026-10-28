// pvfdupid reports duplicate ids in a .lst index and, for the ids given on the
// command line, every entry they resolve to.  A private-server PVF can carry a
// second row for the same dungeon id, which would make a first-wins reader
// describe a different file than the client actually loads.
//
//	go run ./cmd/scratch/pvfdupid -pvf <Script.pvf> -list dungeon/dungeon.lst 11008 5115
package main

import (
	"flag"
	"fmt"
	"strconv"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	listPath := flag.String("list", "dungeon/dungeon.lst", "list path")
	flag.Parse()

	bundle, err := platformpvf.Load(platformpvf.Options{Path: *pvfPath, MaxBytes: platformpvf.NoMaxBytes})
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	archive, err := platformpvf.OpenArchive(bundle)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	text, err := archive.ReadText(*listPath)
	if err != nil {
		fmt.Println("read list:", err)
		return
	}
	doc, err := dnfpvf.Parse(*listPath, text)
	if err != nil {
		fmt.Println("parse list:", err)
		return
	}
	entries := dnfpvf.ParseList(doc)
	fmt.Println("list entries:", len(entries))

	counts := map[int64][]string{}
	for _, e := range entries {
		counts[e.ID] = append(counts[e.ID], e.Path)
	}
	dups := 0
	for id, paths := range counts {
		if len(paths) > 1 {
			dups++
			if dups <= 20 {
				fmt.Printf("  DUPLICATE id=%d -> %v\n", id, paths)
			}
		}
	}
	fmt.Println("duplicate ids:", dups)

	for _, arg := range flag.Args() {
		id, _ := strconv.ParseInt(arg, 10, 64)
		fmt.Printf("id=%d -> %v\n", id, counts[id])
	}
}
