// Scratch tool: enumerate the PVF path table by extension and top-level
// directory. Answers whether this 90CN Script.pvf still ships Squirrel (.nut,
// historically under sqr/) or has moved to another scripting surface.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	bundle, err := pvf.Load(pvf.Options{Path: os.Args[1], MaxBytes: pvf.NoMaxBytes})
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	archive, err := pvf.OpenArchive(bundle)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	ext := map[string]int{}
	top := map[string]int{}
	var hits []string
	total := 0
	archive.RangePaths(func(p string) bool {
		total++
		l := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
		if i := strings.LastIndex(l, "."); i >= 0 {
			ext[l[i:]]++
		}
		if i := strings.Index(l, "/"); i > 0 {
			top[l[:i]]++
		} else {
			top["(root)"]++
		}
		if strings.Contains(l, ".nut") || strings.Contains(l, "sqr") || strings.Contains(l, ".lua") {
			if len(hits) < 20 {
				hits = append(hits, p)
			}
		}
		return true
	})
	fmt.Println("format:", archive.Format(), " files:", total)
	dump := func(m map[string]int, label string, n int) {
		type kv struct {
			k string
			v int
		}
		a := make([]kv, 0, len(m))
		for k, v := range m {
			a = append(a, kv{k, v})
		}
		sort.Slice(a, func(i, j int) bool { return a[i].v > a[j].v })
		fmt.Println("===", label)
		for i, e := range a {
			if i >= n {
				break
			}
			fmt.Printf("  %-16s %d\n", e.k, e.v)
		}
	}
	dump(top, "top-level dirs", 25)
	dump(ext, "extensions", 25)
	fmt.Println("=== nut / sqr / lua hits ===")
	if len(hits) == 0 {
		fmt.Println("  (none)")
	}
	for _, h := range hits {
		fmt.Println("  ", h)
	}
}
