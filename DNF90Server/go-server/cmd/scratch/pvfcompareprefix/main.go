package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	leftPath := flag.String("left", "", "left PVF")
	rightPath := flag.String("right", "", "right PVF")
	prefixesFlag := flag.String("prefix", "ui/raid/luke/,ui/raidwindow/,worldmap/ui/luke_raid.xui,etc/raid/luke/", "comma-separated prefixes or exact paths")
	flag.Parse()
	left, err := platformpvf.Open(*leftPath)
	if err != nil {
		panic(err)
	}
	right, err := platformpvf.Open(*rightPath)
	if err != nil {
		panic(err)
	}
	prefixes := make([]string, 0)
	for _, prefix := range strings.Split(*prefixesFlag, ",") {
		prefix = normalize(prefix)
		if prefix != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	paths := make(map[string]struct{})
	for _, archive := range []*platformpvf.Archive{left, right} {
		for _, file := range archive.Files() {
			filePath := normalize(file.ArchivePath)
			for _, prefix := range prefixes {
				if strings.HasSuffix(prefix, "/") && strings.HasPrefix(filePath, prefix) || filePath == prefix {
					paths[filePath] = struct{}{}
					break
				}
			}
		}
	}
	ordered := make([]string, 0, len(paths))
	for filePath := range paths {
		ordered = append(ordered, filePath)
	}
	sort.Strings(ordered)
	identical, different, leftOnly, rightOnly := 0, 0, 0, 0
	for _, filePath := range ordered {
		leftFile, leftFound := left.FindFile(filePath)
		rightFile, rightFound := right.FindFile(filePath)
		switch {
		case !leftFound:
			rightOnly++
			fmt.Printf("RIGHT_ONLY %s\n", filePath)
		case !rightFound:
			leftOnly++
			fmt.Printf("LEFT_ONLY %s\n", filePath)
		default:
			leftText, leftErr := left.ReadText(leftFile.ArchivePath)
			rightText, rightErr := right.ReadText(rightFile.ArchivePath)
			if leftErr != nil || rightErr != nil {
				fmt.Printf("READ_ERROR %s left=%v right=%v\n", filePath, leftErr, rightErr)
				continue
			}
			leftDecoded := []byte(strings.TrimRight(leftText, "\x00\r\n \t"))
			rightDecoded := []byte(strings.TrimRight(rightText, "\x00\r\n \t"))
			if sha256.Sum256(leftDecoded) == sha256.Sum256(rightDecoded) {
				identical++
				continue
			}
			different++
			fmt.Printf("DIFFERENT %s left_text_bytes=%d right_text_bytes=%d\n", filePath, len(leftDecoded), len(rightDecoded))
		}
	}
	fmt.Printf("SUMMARY identical=%d different=%d left_only=%d right_only=%d total=%d\n", identical, different, leftOnly, rightOnly, len(ordered))
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/"))
}
