// Scratch tool: inspect the social (friend / guild / party) UI definitions and
// any UI text inside the shipped Script.pvf.
//
// usage:
//
//	go run ./cmd/scratch/pvfsocial <pvf> list <pathSubstring> [max]
//	go run ./cmd/scratch/pvfsocial <pvf> dump <pathSubstring> [maxFiles]
//	go run ./cmd/scratch/pvfsocial <pvf> find <text> [pathFilter] [maxHits]
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("usage: pvfsocial <pvf> list|dump|find <arg> [extra] [max]")
		return
	}
	path, mode, needle := os.Args[1], os.Args[2], os.Args[3]
	extra := ""
	if len(os.Args) > 4 {
		extra = os.Args[4]
	}
	limit := 0
	if len(os.Args) > 5 {
		if n, err := strconv.Atoi(os.Args[5]); err == nil && n > 0 {
			limit = n
		}
	}

	bundle, err := pvf.Load(pvf.Options{Path: path, MaxBytes: pvf.NoMaxBytes})
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	archive, err := pvf.OpenArchive(bundle)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	fmt.Printf("archive format=%s files=%d readable=%v\n", archive.Format(), archive.FileCount(), archive.CanReadFileData())

	switch mode {
	case "list":
		printList(archive, needle, limit)
	case "dump":
		dump(archive, strings.ToLower(needle), limit)
	case "find":
		search(archive, needle, strings.ToLower(extra), limit)
	default:
		fmt.Println("unknown mode", mode)
	}
}

func printList(archive *pvf.Archive, needle string, limit int) {
	if limit == 0 {
		limit = 400
	}
	low := strings.ToLower(strings.ReplaceAll(needle, "\\", "/"))
	count := 0
	archive.RangePaths(func(p string) bool {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), low) {
			count++
			if count <= limit {
				fmt.Println("  " + p)
			}
		}
		return true
	})
	fmt.Printf("paths containing %q: %d (shown %d)\n", needle, count, min(count, limit))
}

func dump(archive *pvf.Archive, low string, maxFiles int) {
	if maxFiles == 0 {
		maxFiles = 4
	}
	shown := 0
	var matches []string
	archive.RangePaths(func(p string) bool {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), low) {
			matches = append(matches, p)
		}
		return true
	})
	for _, m := range matches {
		if shown >= maxFiles {
			break
		}
		text, err := archive.ReadText(m)
		if err != nil {
			fmt.Printf("=== %s: read error: %v\n", m, err)
			continue
		}
		shown++
		fmt.Printf("\n================ %s (%d chars) ================\n", m, len(text))
		fmt.Println(text)
	}
	if shown == 0 {
		fmt.Println("no path matched")
	}
}

// search reads the text of every file whose path contains pathFilter and
// reports the ones whose body contains needle.
func search(archive *pvf.Archive, needle, pathFilter string, maxHits int) {
	if pathFilter == "" {
		pathFilter = "ui/"
	}
	if maxHits == 0 {
		maxHits = 40
	}
	scanned, hits := 0, 0
	var paths []string
	archive.RangePaths(func(p string) bool {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), pathFilter) {
			paths = append(paths, p)
		}
		return true
	})
	fmt.Printf("scanning %d files under %q for %q\n", len(paths), pathFilter, needle)
	for _, p := range paths {
		text, err := archive.ReadText(p)
		if err != nil {
			continue
		}
		scanned++
		if !strings.Contains(text, needle) {
			continue
		}
		hits++
		fmt.Printf("HIT %s\n", p)
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, needle) {
				trimmed := strings.TrimSpace(line)
				if len(trimmed) > 260 {
					trimmed = trimmed[:260] + "..."
				}
				fmt.Printf("      %s\n", trimmed)
			}
		}
		if hits >= maxHits {
			fmt.Println("  (hit limit reached)")
			break
		}
	}
	fmt.Printf("scanned=%d hits=%d\n", scanned, hits)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
