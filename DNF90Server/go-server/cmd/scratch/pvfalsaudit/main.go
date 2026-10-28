// Command pvfalsaudit summarizes ANI overlay documents so targeted compatibility
// work can reuse an observed no-op shape instead of inventing one.
package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

type row struct {
	path string
	size int
	text string
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" {
		panic("-pvf is required")
	}
	archive, err := platformpvf.Open(*pvfPath)
	check(err)

	empty := make([]row, 0)
	noAdd := make([]row, 0)
	total := 0
	for _, file := range archive.Files() {
		if !strings.HasSuffix(strings.ToLower(file.ArchivePath), ".ani.als") {
			continue
		}
		total++
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil {
			continue
		}
		trimmed := strings.TrimSpace(strings.Trim(text, "\x00"))
		entry := row{path: file.ArchivePath, size: file.Size, text: trimmed}
		if trimmed == "" {
			empty = append(empty, entry)
			continue
		}
		if !strings.Contains(strings.ToLower(trimmed), "[add]") {
			noAdd = append(noAdd, entry)
		}
	}
	sort.Slice(empty, func(i, j int) bool { return empty[i].path < empty[j].path })
	sort.Slice(noAdd, func(i, j int) bool { return noAdd[i].path < noAdd[j].path })
	fmt.Printf("total=%d empty=%d no_add_nonempty=%d\n", total, len(empty), len(noAdd))
	for _, entry := range empty {
		fmt.Printf("EMPTY\t%d\t%s\n", entry.size, entry.path)
	}
	for _, entry := range noAdd {
		text := strings.ReplaceAll(entry.text, "\r", "")
		text = strings.ReplaceAll(text, "\n", "\\n")
		if len(text) > 300 {
			text = text[:300]
		}
		fmt.Printf("NO_ADD\t%d\t%s\t%s\n", entry.size, entry.path, text)
	}
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
