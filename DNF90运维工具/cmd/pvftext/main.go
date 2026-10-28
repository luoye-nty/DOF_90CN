package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"dnf90ops/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: pvftext <Script.pvf> dump|list|search <arg> [query]")
		os.Exit(1)
	}
	archive, err := pvf.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	switch os.Args[2] {
	case "dump":
		text, err := archive.ReadText(os.Args[3])
		if err != nil {
			panic(err)
		}
		fmt.Print(text)
	case "list":
		pattern, err := regexp.Compile("(?i)" + os.Args[3])
		if err != nil {
			panic(err)
		}
		for _, file := range archive.Files() {
			if pattern.MatchString(file.ArchivePath) {
				fmt.Printf("%s\t%d\t%d\n", file.ArchivePath, file.Size, file.DataType)
			}
		}
	case "search":
		if len(os.Args) < 5 {
			panic("search requires path-regex and query")
		}
		pathPattern, err := regexp.Compile("(?i)" + os.Args[3])
		if err != nil {
			panic(err)
		}
		query := strings.ToLower(os.Args[4])
		for _, file := range archive.Files() {
			if !pathPattern.MatchString(file.ArchivePath) {
				continue
			}
			if file.DataType != 1 && file.DataType != 3 {
				continue
			}
			text, err := archive.FileText(file.Index)
			if err != nil || !strings.Contains(strings.ToLower(text), query) {
				continue
			}
			for index, line := range strings.Split(text, "\n") {
				if strings.Contains(strings.ToLower(line), query) {
					fmt.Printf("%s:%d: %s\n", file.ArchivePath, index+1, strings.TrimSpace(line))
				}
			}
		}
	default:
		panic("unknown mode")
	}
}
