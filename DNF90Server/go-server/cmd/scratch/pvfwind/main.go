// pvfwind scans every decodable archive entry for a needle and prints the path
// plus a short, newline-collapsed window around each hit. Unlike pvfstringgrep
// it never prints a whole giant line, so one 1 MB .stk row cannot bury the hit
// that matters.
//
//	go run ./cmd/scratch/pvfwind -pvf <Script.pvf> -win 80 "层)"
package main

import (
	"flag"
	"fmt"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	maxPaths := flag.Int("max", 30, "stop after this many matching files")
	maxPerFile := flag.Int("per", 3, "hits reported per file")
	win := flag.Int("win", 70, "characters of context around each hit")
	flag.Parse()
	needles := flag.Args()
	if len(needles) == 0 {
		fmt.Println("usage: pvfwind [-pvf path] needle...")
		return
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	paths := 0
	for _, file := range archive.Files() {
		if paths >= *maxPaths {
			break
		}
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil || text == "" {
			continue
		}
		reported := 0
		for _, needle := range needles {
			from := 0
			for reported < *maxPerFile {
				idx := strings.Index(text[from:], needle)
				if idx < 0 {
					break
				}
				at := from + idx
				lo := at - *win
				if lo < 0 {
					lo = 0
				}
				hi := at + len(needle) + *win
				if hi > len(text) {
					hi = len(text)
				}
				snippet := strings.NewReplacer("\n", "\\n", "\r", "", "\t", " ").Replace(text[lo:hi])
				fmt.Printf("%s @%d  %s\n", file.ArchivePath, at, snippet)
				reported++
				from = at + len(needle)
				if from >= len(text) {
					break
				}
			}
			if reported >= *maxPerFile {
				break
			}
		}
		if reported > 0 {
			paths++
		}
	}
	fmt.Println("matching files:", paths)
}
