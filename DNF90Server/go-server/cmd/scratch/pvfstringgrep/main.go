// pvfstringgrep scans every decodable script in the archive for the query and
// prints the path plus the matching line, so a client toast can be traced back
// to the script that carries it.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	maxHits := flag.Int("max", 40, "stop after this many matches")
	flag.Parse()
	queries := flag.Args()
	if len(queries) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pvfstringgrep [-pvf path] query...")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	hits := 0
	for _, file := range archive.Files() {
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			matched := false
			for _, query := range queries {
				if strings.Contains(line, query) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			fmt.Printf("%s: %s\n", file.ArchivePath, strings.TrimSpace(line))
			hits++
			if hits >= *maxHits {
				return
			}
		}
	}
}
