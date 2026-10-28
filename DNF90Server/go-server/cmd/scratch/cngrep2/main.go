// Scratch tool: search the PVF for hardcoded Chinese needles (command line
// Chinese is mangled by the console code page, so the needles live here).
package main

import (
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

var needles = []string{
	"兵站", "兵营", "作战区域", "罗什", "炮塔", "前哨基地", "雷达基地",
}

func main() {
	path := `runtime\data\dnf\Script.pvf`
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	hits := 0
	for _, file := range archive.Files() {
		full := file.ArchivePath
		if strings.TrimSpace(full) == "" {
			full = file.Path
		}
		if !strings.Contains(strings.ToLower(full), "180621_fiendwar") {
			continue
		}
		text, readErr := archive.ReadText(full)
		if readErr != nil {
			continue
		}
		for _, needle := range needles {
			if !strings.Contains(text, needle) {
				continue
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, needle) {
					fmt.Printf("[%s] %s :: %s\n", needle, full, strings.TrimSpace(line))
					hits++
					if hits > 400 {
						return
					}
				}
			}
		}
	}
	fmt.Println("total hits:", hits)
}
