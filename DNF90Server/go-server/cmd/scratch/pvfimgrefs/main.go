// pvfimgrefs audits IMG paths and the largest frame number referenced by one
// or more runtime-PVF UI path prefixes.
package main

import (
	"flag"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

var (
	aniImage = regexp.MustCompile(`(?is)\[IMAGE\]\s+` + "`" + `([^` + "`" + `]+\.img)` + "`" + `\s+(-?\d+)`)
	imgPath  = regexp.MustCompile(`(?i)[A-Za-z0-9_./\\-]+\.img`)
	imgIndex = regexp.MustCompile(`(?i)(?:Img|Index)="NUM:(\d+)"`)
)

type reference struct {
	Path      string
	MaxFrame  int
	Documents map[string]struct{}
}

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	prefixes := flag.String("prefix", "ui/raid/luke/,ui/raidwindow/,worldmap/ui/luke_raid.xui", "comma-separated archive prefixes or exact paths")
	flag.Parse()
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	wanted := make([]string, 0)
	for _, prefix := range strings.Split(*prefixes, ",") {
		if prefix = normalize(prefix); prefix != "" {
			wanted = append(wanted, prefix)
		}
	}
	references := make(map[string]*reference)
	for _, file := range archive.Files() {
		archivePath := normalize(file.ArchivePath)
		matched := false
		for _, prefix := range wanted {
			if strings.HasSuffix(prefix, "/") && strings.HasPrefix(archivePath, prefix) || archivePath == prefix {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		text, err := archive.ReadText(file.ArchivePath)
		if err != nil {
			continue
		}
		for _, match := range aniImage.FindAllStringSubmatch(text, -1) {
			frame, _ := strconv.Atoi(match[2])
			addReference(references, match[1], frame, archivePath)
		}
		for _, line := range strings.Split(text, "\n") {
			paths := imgPath.FindAllString(line, -1)
			if len(paths) == 0 {
				continue
			}
			indices := imgIndex.FindAllStringSubmatch(line, -1)
			for _, imagePath := range paths {
				if len(indices) == 0 {
					addReference(references, imagePath, 0, archivePath)
					continue
				}
				for _, index := range indices {
					frame, _ := strconv.Atoi(index[1])
					addReference(references, imagePath, frame, archivePath)
				}
			}
		}
	}
	paths := make([]string, 0, len(references))
	for imagePath := range references {
		paths = append(paths, imagePath)
	}
	sort.Strings(paths)
	for _, imagePath := range paths {
		row := references[imagePath]
		fmt.Printf("img=%s max_frame=%d documents=%d\n", row.Path, row.MaxFrame, len(row.Documents))
	}
}

func addReference(references map[string]*reference, imagePath string, frame int, document string) {
	imagePath = normalize(strings.Trim(imagePath, "`\"'"))
	if imagePath == "" {
		return
	}
	row := references[imagePath]
	if row == nil {
		row = &reference{Path: imagePath, MaxFrame: frame, Documents: make(map[string]struct{})}
		references[imagePath] = row
	}
	if frame > row.MaxFrame {
		row.MaxFrame = frame
	}
	row.Documents[document] = struct{}{}
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/"))
}
