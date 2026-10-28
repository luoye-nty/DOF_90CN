// pvfanimrefs prints the IMG references used by ANI documents under selected
// archive prefixes. It is intentionally read-only and is used to build small
// NPK compatibility closures instead of replacing an entire profession tree.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

var imagePair = regexp.MustCompile("(?i)`([^`]+\\.img)`\\s+(-?\\d+)")

type imageRef struct {
	Path      string   `json:"path"`
	Frames    []int    `json:"frames"`
	Documents []string `json:"documents"`
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	prefixesFlag := flag.String("prefix", "", "comma-separated ANI archive prefixes")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" || strings.TrimSpace(*prefixesFlag) == "" {
		fmt.Fprintln(os.Stderr, "-pvf and -prefix are required")
		os.Exit(2)
	}

	archive, err := platformpvf.Open(*pvfPath)
	check(err)
	prefixes := splitNormalized(*prefixesFlag)
	type accumulator struct {
		frames    map[int]struct{}
		documents map[string]struct{}
	}
	refs := make(map[string]*accumulator)
	documentsScanned := 0
	for _, file := range archive.Files() {
		archivePath := normalize(file.ArchivePath)
		if !hasAnyPrefix(archivePath, prefixes) ||
			!(strings.HasSuffix(archivePath, ".ani") || strings.HasSuffix(archivePath, ".ani.als")) {
			continue
		}
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil {
			continue
		}
		documentsScanned++
		for _, match := range imagePair.FindAllStringSubmatch(text, -1) {
			imagePath := normalizeIMG(match[1])
			frame, _ := strconv.Atoi(match[2])
			row := refs[imagePath]
			if row == nil {
				row = &accumulator{frames: make(map[int]struct{}), documents: make(map[string]struct{})}
				refs[imagePath] = row
			}
			row.frames[frame] = struct{}{}
			row.documents[archivePath] = struct{}{}
		}
	}

	paths := make([]string, 0, len(refs))
	for imagePath := range refs {
		paths = append(paths, imagePath)
	}
	sort.Strings(paths)
	rows := make([]imageRef, 0, len(paths))
	for _, imagePath := range paths {
		row := refs[imagePath]
		frames := make([]int, 0, len(row.frames))
		for frame := range row.frames {
			frames = append(frames, frame)
		}
		sort.Ints(frames)
		documents := make([]string, 0, len(row.documents))
		for document := range row.documents {
			documents = append(documents, document)
		}
		sort.Strings(documents)
		rows = append(rows, imageRef{Path: imagePath, Frames: frames, Documents: documents})
	}

	report := struct {
		Documents  int        `json:"documents"`
		References []imageRef `json:"references"`
	}{Documents: documentsScanned, References: rows}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	check(encoder.Encode(report))
}

func splitNormalized(value string) []string {
	parts := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = normalize(item); item != "" {
			parts = append(parts, item)
		}
	}
	return parts
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func normalizeIMG(value string) string {
	value = normalize(strings.Trim(value, "`\"'"))
	return strings.TrimPrefix(value, "sprite/")
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/"))
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
