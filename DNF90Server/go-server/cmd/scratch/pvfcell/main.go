// pvfcell scans every decodable archive entry for a needle and prints only the
// hits whose path is NOT in the noise directories, so a UI/dungeon string is not
// buried under thousands of item names.
//
//	go run ./cmd/scratch/pvfcell -pvf <Script.pvf> 层
package main

import (
	"flag"
	"fmt"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	maxPaths := flag.Int("max", 40, "stop after this many matching files")
	listUnreadable := flag.Bool("unreadable", false, "list every entry whose text cannot be decoded")
	flag.Parse()
	needles := flag.Args()
	if len(needles) == 0 && !*listUnreadable {
		fmt.Println("usage: pvfcell [-pvf path] [-unreadable] needle...")
		return
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	skip := []string{"stackable/", "etc/itemdictionary/", "equipment/", "etc/etc.", "n_quest/",
		"aicharacter/", "monster/", "passiveobject/", "map/", "character/", "creature/",
		"avatar/", "etc/quest", "etc/stringtable"}
	readFail := 0
	matched := 0
	for _, file := range archive.Files() {
		if matched >= *maxPaths {
			break
		}
		p := strings.ToLower(strings.ReplaceAll(file.ArchivePath, "\\", "/"))
		noisy := false
		for _, s := range skip {
			if strings.Contains(p, s) {
				noisy = true
				break
			}
		}
		if noisy {
			continue
		}
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil || text == "" {
			readFail++
			if *listUnreadable {
				fmt.Printf("UNREADABLE %s\n", file.ArchivePath)
			}
			continue
		}
		for _, needle := range needles {
			idx := strings.Index(text, needle)
			if idx < 0 {
				continue
			}
			lo := idx - 60
			if lo < 0 {
				lo = 0
			}
			hi := idx + len(needle) + 60
			if hi > len(text) {
				hi = len(text)
			}
			snippet := strings.NewReplacer("\n", "\\n", "\r", "", "\t", " ").Replace(text[lo:hi])
			fmt.Printf("%s @%d  %s\n", file.ArchivePath, idx, snippet)
			matched++
			break
		}
	}
	fmt.Println("matched files:", matched, " unreadable:", readFail)
}
