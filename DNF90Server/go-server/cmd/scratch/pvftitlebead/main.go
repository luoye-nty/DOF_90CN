// Command pvftitlebead dumps the runtime-PVF metadata chain behind the
// enchant-by-bead (op272) route for title beads: bead document -> [monster card
// id] -> card document -> [string data] equipment-type whitelist and the
// card's repeated [enchant index] / [enchant table] blocks.
//
// It exists purely to gather evidence for the "title bead always applies the
// first-listed attribute" investigation. It reads Script.pvf and prints; it
// never writes anything and is not part of the server build.
//
// Typical use on the server machine:
//
//	go run ./cmd/scratch/pvftitlebead                      # scan every bead whose card can enchant [title name]
//	go run ./cmd/scratch/pvftitlebead -bead 490702514      # dump one bead + its card in full
//	go run ./cmd/scratch/pvftitlebead -item 101010438      # dump any item document in full
//	go run ./cmd/scratch/pvftitlebead -name 属性强化        # find beads/cards/titles by name substring
package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const titleEquipmentType = "[title name]"

type itemRef struct {
	kind string
	id   int64
	path string
}

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	beadList := flag.String("bead", "", "comma-separated bead item ids to dump in full (with their resolved cards)")
	itemList := flag.String("item", "", "comma-separated item ids to dump in full (any equipment/stackable)")
	nameQuery := flag.String("name", "", "case-insensitive name substring filter for the scan")
	full := flag.Bool("full", false, "print the complete raw document text for every scan hit")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fail(err)
	}
	index := buildIndex(archive)

	if ids := parseIDs(*itemList); len(ids) > 0 {
		for _, id := range ids {
			dumpItem(archive, index, id)
		}
		return
	}
	if ids := parseIDs(*beadList); len(ids) > 0 {
		for _, id := range ids {
			dumpBeadChain(archive, index, id)
		}
		return
	}
	scanTitleBeads(archive, index, strings.ToLower(strings.TrimSpace(*nameQuery)), *full)
}

// scanTitleBeads walks every stackable, keeps the ones that carry an enchant
// card, resolves that card, and reports the beads whose card whitelists
// [title name]. Those are exactly the beads the op272 route can apply to a
// title, which is the population the reported bug lives in.
func scanTitleBeads(archive *platformpvf.Archive, index map[int64]itemRef, nameQuery string, full bool) {
	refs := make([]itemRef, 0, len(index))
	for _, ref := range index {
		if ref.kind == "stackable" {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].id < refs[j].id })

	hits := 0
	for _, ref := range refs {
		document, text, err := read(archive, ref.path)
		if err != nil {
			continue
		}
		cardIDs := allInts(document, "monster card id")
		if len(cardIDs) == 0 {
			continue
		}
		cardRef, ok := index[cardIDs[0]]
		if !ok {
			continue
		}
		cardDocument, cardText, err := read(archive, cardRef.path)
		if err != nil {
			continue
		}
		if !allowsTitle(cardDocument) {
			continue
		}
		beadName, _ := document.Text("name")
		if nameQuery != "" && !strings.Contains(strings.ToLower(beadName), nameQuery) {
			continue
		}
		hits++
		fmt.Printf("=== BEAD %d %q\n", ref.id, beadName)
		fmt.Printf("    path                     : %s\n", ref.path)
		fmt.Printf("    [stackable type]         : %s\n", firstText(document, "stackable type"))
		fmt.Printf("    [monster card id]        : %v\n", cardIDs)
		fmt.Printf("    [enchant index] (bead)   : %v   <-- ignored by the current resolver\n", allInts(document, "enchant index"))
		fmt.Printf("    [bead limited usable item]: %v\n", allInts(document, "bead limited usable item"))
		fmt.Printf("    [target item id]         : %v\n", allInts(document, "target item id"))
		printCard(cardRef, cardDocument, cardText, full)
		if full {
			fmt.Printf("--- bead raw ---\n%s\n", text)
		}
		fmt.Println()
	}
	fmt.Printf("### %d bead(s) can enchant %s\n", hits, titleEquipmentType)
}

func dumpBeadChain(archive *platformpvf.Archive, index map[int64]itemRef, beadID int64) {
	ref, ok := index[beadID]
	if !ok {
		fmt.Printf("=== BEAD %d NOT FOUND in equipment.lst/stackable.lst\n", beadID)
		return
	}
	document, text, err := read(archive, ref.path)
	if err != nil {
		fmt.Printf("=== BEAD %d read %s failed: %v\n", beadID, ref.path, err)
		return
	}
	beadName, _ := document.Text("name")
	fmt.Printf("=== BEAD %d %q (%s)\n", beadID, beadName, ref.path)
	fmt.Printf("    [stackable type]          : %s\n", firstText(document, "stackable type"))
	fmt.Printf("    [monster card id]         : %v\n", allInts(document, "monster card id"))
	fmt.Printf("    [enchant index] (bead)    : %v   <-- ignored by the current resolver\n", allInts(document, "enchant index"))
	fmt.Printf("    [bead limited usable item]: %v\n", allInts(document, "bead limited usable item"))
	fmt.Printf("    [target item id]          : %v\n", allInts(document, "target item id"))
	fmt.Printf("--- bead raw ---\n%s\n", text)

	cardIDs := allInts(document, "monster card id")
	if len(cardIDs) == 0 {
		fmt.Println("### bead carries no [monster card id]")
		return
	}
	cardRef, ok := index[cardIDs[0]]
	if !ok {
		fmt.Printf("### card %d not present in the item lists\n", cardIDs[0])
		return
	}
	cardDocument, cardText, err := read(archive, cardRef.path)
	if err != nil {
		fmt.Printf("### card %d read %s failed: %v\n", cardIDs[0], cardRef.path, err)
		return
	}
	printCard(cardRef, cardDocument, cardText, true)
}

func dumpItem(archive *platformpvf.Archive, index map[int64]itemRef, itemID int64) {
	ref, ok := index[itemID]
	if !ok {
		fmt.Printf("=== ITEM %d NOT FOUND\n", itemID)
		return
	}
	document, text, err := read(archive, ref.path)
	if err != nil {
		fmt.Printf("=== ITEM %d read %s failed: %v\n", itemID, ref.path, err)
		return
	}
	name, _ := document.Text("name")
	fmt.Printf("=== ITEM %d %q kind=%s (%s)\n", itemID, name, ref.kind, ref.path)
	fmt.Printf("    [equipment type] : %s\n", firstText(document, "equipment type"))
	fmt.Printf("    [stackable type] : %s\n", firstText(document, "stackable type"))
	fmt.Printf("--- raw ---\n%s\n", text)
}

// printCard reports the two card fields that decide what the client renders:
// the [string data] equipment-type whitelist and every repeated [enchant index]
// block. More than one [enchant index] block means the card holds several stat
// rows and the raw+0x12 byte selects between them.
func printCard(ref itemRef, document *dnfpvf.Document, text string, full bool) {
	name, _ := document.Text("name")
	indexBlocks := sectionInts(document, "enchant index")
	tableBlocks := sectionInts(document, "enchant table")
	fmt.Printf("    -> CARD %d %q\n", ref.id, name)
	fmt.Printf("       path                 : %s\n", ref.path)
	fmt.Printf("       [string data]        : %v\n", allTexts(document, "string data"))
	fmt.Printf("       [enchant index] blocks: %d %v\n", len(indexBlocks), indexBlocks)
	fmt.Printf("       [enchant table] blocks: %d %v\n", len(tableBlocks), tableBlocks)
	if len(indexBlocks) > 1 || len(tableBlocks) > 1 {
		fmt.Printf("       *** multi-block card: raw+0x12 selects the row, the server always writes 0 ***\n")
	}
	if full {
		fmt.Printf("--- card raw ---\n%s\n", text)
	}
}

func allowsTitle(cardDocument *dnfpvf.Document) bool {
	for _, value := range allTexts(cardDocument, "string data") {
		if strings.EqualFold(strings.TrimSpace(value), titleEquipmentType) {
			return true
		}
	}
	return false
}

// allInts collects tokens from EVERY section with the given name.
// Document.Ints only reads the first matching section, which is what hides
// repeated blocks from the running server.
func allInts(document *dnfpvf.Document, name string) []int64 {
	out := make([]int64, 0, 8)
	for _, tokens := range sectionTokens(document, name) {
		for _, token := range tokens {
			if token.Kind == dnfpvf.TokenInt {
				out = append(out, token.Int)
			}
		}
	}
	return out
}

func allTexts(document *dnfpvf.Document, name string) []string {
	out := make([]string, 0, 8)
	for _, tokens := range sectionTokens(document, name) {
		for _, token := range tokens {
			if token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent {
				out = append(out, token.Value)
			}
		}
	}
	return out
}

// sectionInts keeps each repeated section separate so the block count is visible.
func sectionInts(document *dnfpvf.Document, name string) [][]int64 {
	blocks := make([][]int64, 0, 4)
	for _, tokens := range sectionTokens(document, name) {
		block := make([]int64, 0, len(tokens))
		for _, token := range tokens {
			if token.Kind == dnfpvf.TokenInt {
				block = append(block, token.Int)
			}
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func sectionTokens(document *dnfpvf.Document, name string) [][]dnfpvf.Token {
	if document == nil {
		return nil
	}
	blocks := make([][]dnfpvf.Token, 0, 4)
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), name) {
			continue
		}
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start >= section.End {
			blocks = append(blocks, nil)
			continue
		}
		blocks = append(blocks, document.Tokens[section.Start:section.End])
	}
	return blocks
}

func firstText(document *dnfpvf.Document, name string) string {
	value, ok := document.Text(name)
	if !ok {
		return "<absent>"
	}
	return value
}

func read(archive *platformpvf.Archive, archivePath string) (*dnfpvf.Document, string, error) {
	text, err := archive.ReadText(archivePath)
	if err != nil {
		return nil, "", err
	}
	document, err := dnfpvf.Parse(archivePath, text)
	if err != nil {
		return nil, text, err
	}
	return document, text, nil
}

func buildIndex(archive *platformpvf.Archive) map[int64]itemRef {
	index := make(map[int64]itemRef, 65536)
	for _, source := range []struct{ kind, listPath string }{
		{"equipment", "equipment/equipment.lst"},
		{"stackable", "stackable/stackable.lst"},
	} {
		text, err := archive.ReadText(source.listPath)
		if err != nil {
			fail(fmt.Errorf("read %s: %w", source.listPath, err))
		}
		document, err := dnfpvf.Parse(source.listPath, text)
		if err != nil {
			fail(fmt.Errorf("parse %s: %w", source.listPath, err))
		}
		for _, entry := range dnfpvf.ParseList(document) {
			resolved := resolvePath(archive, source.listPath, entry.Path)
			if resolved == "" {
				continue
			}
			index[entry.ID] = itemRef{kind: source.kind, id: entry.ID, path: resolved}
		}
	}
	return index
}

func resolvePath(archive *platformpvf.Archive, listPath, reference string) string {
	reference = strings.TrimSpace(strings.Trim(reference, "`\"'"))
	for _, candidate := range []string{reference, path.Join(path.Dir(listPath), reference)} {
		if file, ok := archive.FindFile(candidate); ok {
			return file.ArchivePath
		}
	}
	return ""
}

func parseIDs(raw string) []int64 {
	ids := make([]int64, 0, 4)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil || value <= 0 {
			continue
		}
		ids = append(ids, value)
	}
	return ids
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
