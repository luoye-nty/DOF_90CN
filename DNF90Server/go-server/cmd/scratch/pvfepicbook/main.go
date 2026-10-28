// pvfepicbook lists the archive paths that carry Epic-book exchange data so the
// op788 recipe source can be identified from the PVF rather than guessed.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `runtime/data/dnf/Script.pvf`, "Script.pvf path")
	match := flag.String("match", "epic", "case-insensitive substring of the archive path")
	read := flag.String("read", "", "print one exact archive text path and exit")
	limit := flag.Int("limit", 60, "maximum listed paths")
	lstID := flag.Int64("id", 0, "resolve one item id through every *.lst index and print its document")
	grep := flag.String("grep", "", "print archive paths whose text contains this token")
	grepExt := flag.String("grep-ext", ".etc,.lst,.nut,.cfg", "comma-separated suffixes to search with -grep")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	if *read != "" {
		text, err := archive.ReadText(*read)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		fmt.Println(text)
		return
	}

	if *grep != "" {
		suffixes := strings.Split(strings.ToLower(*grepExt), ",")
		hits := 0
		for _, file := range archive.Files() {
			name := strings.ToLower(file.Name)
			keep := false
			for _, suffix := range suffixes {
				if suffix != "" && strings.HasSuffix(name, suffix) {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
			full := strings.TrimPrefix(file.Path+"/"+file.Name, "/")
			text, err := archive.ReadText(full)
			if err != nil || !strings.Contains(text, *grep) {
				continue
			}
			fmt.Printf("%s  (size=%d)\n", full, file.Size)
			hits++
			if hits >= *limit {
				fmt.Println("... limit reached")
				break
			}
		}
		fmt.Println("content hits:", hits)
		return
	}

	if *lstID != 0 {
		want := strconv.FormatInt(*lstID, 10)
		for _, file := range archive.Files() {
			if !strings.HasSuffix(strings.ToLower(file.Name), ".lst") {
				continue
			}
			listPath := strings.TrimPrefix(file.Path+"/"+file.Name, "/")
			text, err := archive.ReadText(listPath)
			if err != nil {
				continue
			}
			fields := strings.Fields(text)
			for i := 0; i+1 < len(fields); i++ {
				if fields[i] != want {
					continue
				}
				rel := strings.Trim(fields[i+1], "`")
				root := listPath[:strings.LastIndex(listPath, "/")+1]
				docPath := root + rel
				fmt.Printf("=== %s -> %s ===\n", listPath, docPath)
				doc, err := archive.ReadText(docPath)
				if err != nil {
					fmt.Println("read:", err)
					continue
				}
				fmt.Println(doc)
			}
		}
		return
	}

	needle := strings.ToLower(*match)
	paths := make([]string, 0, 128)
	for _, file := range archive.Files() {
		full := strings.TrimPrefix(file.Path+"/"+file.Name, "/")
		if strings.Contains(strings.ToLower(full), needle) {
			paths = append(paths, full)
		}
	}
	sort.Strings(paths)
	fmt.Printf("archive files=%d matching %q=%d\n", archive.FileCount(), needle, len(paths))
	for i, path := range paths {
		if i >= *limit {
			fmt.Printf("... %d more\n", len(paths)-i)
			break
		}
		fmt.Println(path)
	}
}
