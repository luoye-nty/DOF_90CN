package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	archive, err := platformpvf.Open(os.Args[1])
	if err != nil { fmt.Fprintln(os.Stderr, "open:", err); os.Exit(1) }
	counts := map[string]int{}
	for _, file := range archive.Files() {
		full := strings.ReplaceAll(file.ArchivePath, "\\", "/")
		ext := strings.ToLower(path.Ext(full))
		if ext == "" { ext = "(none)" }
		counts[ext]++
	}
	type row struct{ ext string; n int }
	rows := make([]row, 0, len(counts))
	for ext, n := range counts { rows = append(rows, row{ext, n}) }
	sort.Slice(rows, func(a, b int) bool { return rows[a].n > rows[b].n })
	for _, r := range rows { fmt.Printf("%-10s %d\n", r.ext, r.n) }
}
