// pvflookup is a read-only inspector for one PVF archive.
//
//	go run ./cmd/scratch/pvflookup <pvf-path> list <substring>
//	go run ./cmd/scratch/pvflookup <pvf-path> read <archive-path>
//	go run ./cmd/scratch/pvflookup <pvf-path> grep <substring> [path-prefix]
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: pvflookup <pvf-path> <list|read|grep> ...")
		os.Exit(2)
	}
	archive, err := pvf.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "format=%s files=%d\n", archive.Format(), archive.FileCount())

	switch os.Args[2] {
	case "list":
		needle := strings.ToLower(os.Args[3])
		var hits []string
		for _, file := range archive.Files() {
			if strings.Contains(strings.ToLower(file.ArchivePath), needle) {
				hits = append(hits, file.ArchivePath)
			}
		}
		sort.Strings(hits)
		for _, hit := range hits {
			fmt.Println(hit)
		}
		fmt.Fprintf(os.Stderr, "matches=%d\n", len(hits))
	case "read":
		text, err := archive.ReadText(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		fmt.Print(text)
	case "grep":
		needle := strings.ToLower(os.Args[3])
		prefix := ""
		if len(os.Args) > 4 {
			prefix = strings.ToLower(os.Args[4])
		}
		scanned, matched := 0, 0
		for _, file := range archive.Files() {
			path := strings.ToLower(file.ArchivePath)
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			scanned++
			text, err := archive.FileText(file.Index)
			if err != nil || text == "" {
				continue
			}
			if strings.Contains(strings.ToLower(text), needle) {
				matched++
				fmt.Println(file.ArchivePath)
			}
		}
		fmt.Fprintf(os.Stderr, "scanned=%d matched=%d\n", scanned, matched)
	case "zerogold":
		// Find booster / int-data rows whose item slot is 0 (the gold pseudo
		// item). A row is "<item> <weight> <count>" or
		// "<drawCount> <item> <weight> <count>".
		row := regexp.MustCompile(`(?m)^\s*(?:\d+\s+)?0\s+\d+\s+\d+\s*$`)
		scanned, matched := 0, 0
		for _, file := range archive.Files() {
			path := strings.ToLower(file.ArchivePath)
			if !strings.HasPrefix(path, "stackable/") {
				continue
			}
			scanned++
			text, err := archive.FileText(file.Index)
			if err != nil || text == "" {
				continue
			}
			lower := strings.ToLower(text)
			if !strings.Contains(lower, "[booster info]") && !strings.Contains(lower, "[int data]") {
				continue
			}
			if hits := row.FindAllString(text, -1); len(hits) > 0 {
				matched++
				fmt.Printf("%s :: %s\n", file.ArchivePath, strings.Join(hits, " | "))
			}
		}
		fmt.Fprintf(os.Stderr, "scanned=%d matched=%d\n", scanned, matched)
	case "idmap":
		// Resolve quest ids through n_quest/quest.lst (`<id> `<path>``).
		listText, err := archive.ReadText("n_quest/quest.lst")
		if err != nil {
			fmt.Fprintln(os.Stderr, "read quest.lst:", err)
			os.Exit(1)
		}
		entry := regexp.MustCompile("(\\d+)\\s+`([^`]+)`")
		wanted := make(map[string]bool, len(os.Args)-3)
		for _, id := range os.Args[3:] {
			wanted[id] = true
		}
		for _, match := range entry.FindAllStringSubmatch(listText, -1) {
			if wanted[match[1]] {
				fmt.Printf("%s -> %s\n", match[1], match[2])
			}
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[2])
		os.Exit(2)
	}
}
