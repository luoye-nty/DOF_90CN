// pvfkeys collects the bracketed dictionary keys used by stackable/equipment
// scripts so an item rule can be gated on a real PVF field instead of a guess.
//
//	go run ./cmd/scratch/pvfkeys [-pvf path] [-prefix stackable] [-filter sell]
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

var keyPattern = regexp.MustCompile(`^\s*\[([^\]/][^\]]*)\]\s*$`)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	prefixes := flag.String("prefix", "stackable,equipment", "comma separated archive path prefixes")
	filter := flag.String("filter", "", "only report keys containing this substring")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	wanted := map[string]bool{}
	for _, prefix := range strings.Split(*prefixes, ",") {
		wanted[strings.TrimSpace(prefix)] = true
	}
	counts := map[string]int{}
	examples := map[string]string{}
	scanned := 0
	for _, file := range archive.Files() {
		path := file.ArchivePath
		matched := false
		for prefix := range wanted {
			if strings.HasPrefix(path, prefix+"/") {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			continue
		}
		scanned++
		for _, line := range strings.Split(text, "\n") {
			match := keyPattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(match[1]))
			if key == "" {
				continue
			}
			counts[key]++
			if _, seen := examples[key]; !seen {
				examples[key] = path
			}
		}
	}

	keys := make([]string, 0, len(counts))
	for key := range counts {
		if *filter != "" && !strings.Contains(key, *filter) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Printf("scanned=%d distinct_keys=%d filter=%q\n", scanned, len(counts), *filter)
	for _, key := range keys {
		fmt.Printf("%6d  [%s]  e.g. %s\n", counts[key], key, examples[key])
	}
}
