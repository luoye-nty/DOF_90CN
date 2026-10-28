// Command pvfshell is a small read-only helper for inspecting PVF entries.
package main

import (
	"fmt"
	"os"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: pvfshell <pvf> list <substring> | cat <path>")
		os.Exit(2)
	}
	archive, err := pvf.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	switch os.Args[2] {
	case "list":
		needle := strings.ToLower(os.Args[3])
		count := 0
		for _, file := range archive.Files() {
			if strings.Contains(strings.ToLower(file.ArchivePath), needle) {
				fmt.Printf("%s\t%d\n", file.ArchivePath, file.Size)
				count++
			}
		}
		fmt.Fprintf(os.Stderr, "matched %d file(s)\n", count)
	case "cat":
		text, err := archive.ReadText(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		fmt.Print(text)
	case "grep":
		prefix := strings.ToLower(os.Args[3])
		needle := strings.ToLower(os.Args[4])
		limit := 40
		if len(os.Args) > 5 {
			fmt.Sscanf(os.Args[5], "%d", &limit)
		}
		matched := 0
		for _, file := range archive.Files() {
			path := strings.ToLower(file.ArchivePath)
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			text, err := archive.ReadText(file.ArchivePath)
			if err != nil || !strings.Contains(strings.ToLower(text), needle) {
				continue
			}
			fmt.Printf("=== %s\n", file.ArchivePath)
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(strings.ToLower(line), needle) {
					fmt.Printf("    %s\n", strings.TrimSpace(line))
				}
			}
			matched++
			if matched >= limit {
				break
			}
		}
		fmt.Fprintf(os.Stderr, "matched %d file(s)\n", matched)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[2])
		os.Exit(2)
	}
}
