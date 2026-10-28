// Scratch tool: scan every PVF text document for integer tokens equal to one of
// the account-shared materials (cubes 3033..3037 / 3262, souls 10100115 /
// 10100116 / 10099773..10099775 / 10158124), and report where they are
// referenced (directory prefix + section name + document count).
//
//	go run ./cmd/scratch/idscan -pvf <Script.pvf> [-top 40] [-maxdoc 2000000]
//
// Why: answering "which acquisition systems can actually hand out a cube/soul"
// from code alone is guesswork. This reads the data instead -- each hit names a
// system (npc shop list, quest reward, package data, drop table, event config),
// which then maps to exactly one server-side grant path.
package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

var accountSharedIDs = map[int64]string{
	3033: "3033", 3034: "3034", 3035: "3035", 3036: "3036",
	3037: "3037无色", 3262: "3262金",
	10100115: "10100115普通灵魂", 10100116: "10100116高级灵魂",
	10099773: "10099773稀有灵魂", 10099774: "10099774独有灵魂",
	10099775: "10099775传说灵魂", 10158124: "10158124史诗灵魂",
}

type hit struct {
	prefix  string
	section string
	itemID  int64
	docs    int
}

func main() {
	pvfPath := flag.String("pvf", "../runtime/data/dnf/Script.pvf", "Script.pvf path")
	top := flag.Int("top", 40, "rows to print per report")
	maxDoc := flag.Int("maxdoc", 2000000, "skip documents larger than this")
	prefixFilter := flag.String("prefix", "", "only documents under this directory prefix (e.g. etc)")
	sectionFilter := flag.String("section", "", "only this section name (comma separated)")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	type key struct {
		prefix  string
		section string
		itemID  int64
	}
	wantedSections := map[string]struct{}{}
	for _, name := range strings.Split(*sectionFilter, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			wantedSections[name] = struct{}{}
		}
	}
	filtered := *prefixFilter != "" || len(wantedSections) != 0
	paths := map[key][]string{}
	docs := map[key]map[string]struct{}{}
	perItem := map[int64]map[string]struct{}{}
	scanned, parsed, skipped := 0, 0, 0
	for _, file := range archive.Files() {
		full := strings.ReplaceAll(file.ArchivePath, "\\", "/")
		if strings.TrimSpace(full) == "" {
			continue
		}
		if !isTextDocument(full) {
			continue
		}
		if *prefixFilter != "" && !strings.HasPrefix(full, *prefixFilter) {
			continue
		}
		text, readErr := archive.ReadText(full)
		if readErr != nil {
			skipped++
			continue
		}
		if len(text) > *maxDoc {
			skipped++
			continue
		}
		scanned++
		document, parseErr := dnfpvf.Parse(full, text)
		if parseErr != nil {
			continue
		}
		parsed++
		// 每个段名 -> 该段出现的 int token 集合，命中即记一次（按文档去重）。
		for _, section := range document.Sections {
			name := strings.ToLower(strings.TrimSpace(section.Name))
			if len(wantedSections) != 0 {
				if _, ok := wantedSections[name]; !ok {
					continue
				}
			}
			if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
				continue
			}
			found := map[int64]struct{}{}
			for _, token := range document.Tokens[section.Start:section.End] {
				if token.Kind != dnfpvf.TokenInt {
					continue
				}
				if _, ok := accountSharedIDs[token.Int]; !ok {
					continue
				}
				found[token.Int] = struct{}{}
			}
			for itemID := range found {
				slot := key{prefix: firstSegment(full), section: name, itemID: itemID}
				if docs[slot] == nil {
					docs[slot] = map[string]struct{}{}
				}
				docs[slot][full] = struct{}{}
				if filtered && len(paths[slot]) < 6 {
					paths[slot] = append(paths[slot], full)
				}
				if perItem[itemID] == nil {
					perItem[itemID] = map[string]struct{}{}
				}
				perItem[itemID][name] = struct{}{}
			}
		}
	}
	fmt.Printf("files=%d documents=%d parsed=%d skipped=%d\n", len(archive.Files()), scanned, parsed, skipped)

	rows := make([]hit, 0, len(docs))
	for slot, set := range docs {
		rows = append(rows, hit{prefix: slot.prefix, section: slot.section, itemID: slot.itemID, docs: len(set)})
	}
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].docs != rows[b].docs {
			return rows[a].docs > rows[b].docs
		}
		if rows[a].prefix != rows[b].prefix {
			return rows[a].prefix < rows[b].prefix
		}
		if rows[a].section != rows[b].section {
			return rows[a].section < rows[b].section
		}
		return rows[a].itemID < rows[b].itemID
	})
	fmt.Printf("--- top %d (directory prefix / section / item / documents) ---\n", *top)
	for index, entry := range rows {
		if index >= *top {
			break
		}
		fmt.Printf("%-22s [%s] item=%s docs=%d\n",
			entry.prefix, entry.section, accountSharedIDs[entry.itemID], entry.docs)
		if !filtered {
			continue
		}
		for _, documentPath := range paths[key{prefix: entry.prefix, section: entry.section, itemID: entry.itemID}] {
			fmt.Printf("      %s\n", documentPath)
		}
	}

	fmt.Println("--- per item: sections that mention it ---")
	ids := make([]int64, 0, len(perItem))
	for itemID := range perItem {
		ids = append(ids, itemID)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, itemID := range ids {
		names := make([]string, 0, len(perItem[itemID]))
		for name := range perItem[itemID] {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Printf("%s: %s\n", accountSharedIDs[itemID], strings.Join(names, ", "))
	}
}

func firstSegment(full string) string {
	dir := path.Dir(full)
	if dir == "." {
		return "(root)"
	}
	if index := strings.Index(dir, "/"); index >= 0 {
		return dir[:index]
	}
	return dir
}

func isTextDocument(full string) bool {
	lower := strings.ToLower(full)
	for _, suffix := range []string{
		".lst", ".stk", ".equ", ".qst", ".dgn", ".key", ".act", ".ani", ".map",
		".cre", ".npc", ".set", ".etc", ".ui", ".txt", ".chr",
		// 商店(483)、脚本/事件/任务表等：NPC 商店的道具表就在这里(.shp)，
		// 第一版扫描漏了它们，会得出"商店不卖晶块"的错误结论。
		".shp", ".tbl", ".evt", ".rep", ".msn", ".wrd", ".apd", ".cmt",
		".mob", ".als", ".obj", ".skl", ".ai", ".aic", ".ptl",
	} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}
