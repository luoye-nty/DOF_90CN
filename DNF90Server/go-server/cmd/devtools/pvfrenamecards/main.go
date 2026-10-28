// Command pvfrenamecards lists the items the runtime PVF marks as character
// rename cards.
//
// Why it exists: the server used to recognize rename cards by two hard-coded PVF
// paths, so any other card the operator obtained was rejected with "card is
// invalid". The PVF itself carries the contract in the item's action type:
//
//	[action type]
//		[character name change]
//	[/action type]
//
// This tool reads Script.pvf through the same parser the server uses, so its
// output is exactly the set the server can act on.
//
// Usage:
//
//	go run ./cmd/devtools/pvfrenamecards -pvf ..\runtime\data\dnf\Script.pvf
//	go run ./cmd/devtools/pvfrenamecards -pvf <pvf> -id 2660698 -dump <dir>
//	go run ./cmd/devtools/pvfrenamecards -pvf <pvf> -list equipment/equipment.lst
package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "path to Script.pvf (required)")
	listPath := flag.String("list", "stackable/stackable.lst", "item list inside the archive")
	itemID := flag.Int64("id", 0, "dump one item id's document instead of scanning the list")
	dumpDir := flag.String("dump", "", "write every matching document into this directory")
	showAction := flag.Bool("action-types", false, "print each matching item's action type sections")
	flag.Parse()

	if strings.TrimSpace(*pvfPath) == "" {
		fmt.Fprintln(os.Stderr, "use -pvf <path to Script.pvf>")
		os.Exit(2)
	}
	bundle, err := platformpvf.Load(platformpvf.Options{Path: *pvfPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open pvf: %v\n", err)
		os.Exit(1)
	}
	archive, err := platformpvf.OpenArchive(bundle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse pvf: %v\n", err)
		os.Exit(1)
	}
	// The archive is a dnfpvf.Source: same ReadText/Parse path the server takes.
	var source dnfpvf.Source = archive

	if *itemID > 0 {
		dumpOne(source, archive, *itemID, *listPath, *dumpDir)
		return
	}

	started := time.Now()
	entries, err := readItemList(source, *listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read list %s: %v\n", *listPath, err)
		os.Exit(1)
	}
	fmt.Printf("list %s: %d entries\n", *listPath, len(entries))

	type hit struct {
		id       int64
		pvfPath  string
		name     string
		actions  []string
		autoHint string
	}
	var hits []hit
	missing := 0
	unreadable := 0
	for _, entry := range entries {
		docPath, text, err := readItemDocument(source, *listPath, entry.Path)
		if err != nil {
			unreadable++
			continue
		}
		if !strings.Contains(text, "[character name change]") {
			continue
		}
		document, err := dnfpvf.Parse(docPath, text)
		if err != nil {
			missing++
			continue
		}
		name, _ := document.Text("name")
		hits = append(hits, hit{
			id:       entry.ID,
			pvfPath:  docPath,
			name:     name,
			actions:  actionTypeSections(document),
			autoHint: renameAutoUseHint(docPath),
		})
		if *dumpDir != "" {
			target := filepath.Join(*dumpDir, fmt.Sprintf("%d_%s", entry.ID, filepath.Base(docPath)))
			if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write %s: %v\n", target, err)
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].id < hits[j].id })

	fmt.Printf("scanned %d entries in %s (unreadable=%d parse-failed=%d)\n\n",
		len(entries), time.Since(started).Round(time.Millisecond), unreadable, missing)
	fmt.Printf("%-10s %-8s %-46s %s\n", "item_id", "auto_use", "pvf_path", "name")
	for _, match := range hits {
		fmt.Printf("%-10d %-8s %-46s %s\n", match.id, match.autoHint, match.pvfPath, match.name)
		if *showAction {
			fmt.Printf("%-10s %-8s action types: %v\n", "", "", match.actions)
		}
	}
	fmt.Printf("\n%d rename card(s) recognized by action type [character name change]\n", len(hits))
}

// dumpOne prints a single item's document so its action-type block can be read
// verbatim.
func dumpOne(source dnfpvf.Source, archive *platformpvf.Archive, itemID int64, listPath, dumpDir string) {
	entries, err := readItemList(source, listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read list %s: %v\n", listPath, err)
		os.Exit(1)
	}
	for _, entry := range entries {
		if entry.ID != itemID {
			continue
		}
		docPath, text, err := readItemDocument(source, listPath, entry.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read item %d path=%s: %v\n", itemID, entry.Path, err)
			os.Exit(1)
		}
		fmt.Printf("item_id=%d\npvf_path=%s\narchive_path=%s\n\n%s\n", itemID, docPath, entry.Path, text)
		if dumpDir != "" {
			target := filepath.Join(dumpDir, fmt.Sprintf("%d_%s", itemID, filepath.Base(docPath)))
			if err := os.WriteFile(target, []byte(text), 0o644); err == nil {
				fmt.Printf("\n(dumped to %s)\n", target)
			}
		}
		return
	}
	fmt.Fprintf(os.Stderr, "item %d is not in %s\n", itemID, listPath)
	os.Exit(3)
}

type itemListEntry struct {
	ID   int64
	Path string
}

func readItemList(source dnfpvf.Source, listPath string) ([]itemListEntry, error) {
	text, err := source.ReadText(listPath)
	if err != nil {
		return nil, err
	}
	document, err := dnfpvf.Parse(listPath, text)
	if err != nil {
		return nil, err
	}
	parsed := dnfpvf.ParseList(document)
	out := make([]itemListEntry, 0, len(parsed))
	seen := make(map[int64]struct{}, len(parsed))
	for _, entry := range parsed {
		if entry.ID <= 0 {
			continue
		}
		if _, exists := seen[entry.ID]; exists {
			continue
		}
		seen[entry.ID] = struct{}{}
		out = append(out, itemListEntry{ID: entry.ID, Path: entry.Path})
	}
	return out, nil
}

// readItemDocument resolves one list entry to an archive document. The list
// stores paths relative to the list's own directory, so both the joined and the
// bare candidate are tried, exactly like the server's item resolver.
func readItemDocument(source dnfpvf.Source, listPath, listedPath string) (string, string, error) {
	clean := strings.TrimSpace(strings.ReplaceAll(listedPath, "\\", "/"))
	clean = strings.TrimPrefix(strings.TrimPrefix(clean, "./"), "/")
	root := path.Dir(strings.TrimSpace(strings.ReplaceAll(listPath, "\\", "/")))
	candidates := []string{path.Join(root, clean), clean}
	seen := map[string]struct{}{}
	var lastErr error
	for _, candidate := range candidates {
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		text, err := source.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

// actionTypeSections lists the values declared inside [action type] blocks.
//
// The block is read as a section: the marker itself is one section and the
// payload is a plain string token, e.g.
//
//	[action type]
//	`[character name change]`
//	[/action type]
//
// so document.Texts("action type") yields `[character name change]`.
func actionTypeSections(document *dnfpvf.Document) []string {
	if document == nil {
		return nil
	}
	var out []string
	for _, value := range document.Texts("action type") {
		if clean := strings.TrimSpace(value); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

// renameAutoUseHint reports the legacy path allowlist so the operator can see
// which of the recognized cards the Cera shop auto-uses on purchase.
func renameAutoUseHint(docPath string) string {
	switch strings.ToLower(strings.TrimSpace(docPath)) {
	case "stackable/cash/chn_character_rename.stk":
		return "yes"
	case "stackable/cash/charcter_rename_card.stk":
		return "no"
	default:
		return "no"
	}
}
