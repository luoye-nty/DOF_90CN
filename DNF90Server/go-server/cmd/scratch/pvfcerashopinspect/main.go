package main

import (
	"flag"
	"fmt"
	"path"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type itemRef struct {
	ID   int64
	Path string
}

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	sectionName := flag.String("section", "premium", "cerashop section")
	limit := flag.Int("limit", 40, "maximum rows to print; zero prints all")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	document, err := readDocument(archive, "etc/cerashop.etc")
	if err != nil {
		panic(err)
	}
	refs, err := loadStackableRefs(archive)
	if err != nil {
		panic(err)
	}
	tokens, found := document.Section(*sectionName)
	if !found {
		panic(fmt.Errorf("section %q not found", *sectionName))
	}
	values := make([]dnfpvf.Token, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenSection {
			continue
		}
		values = append(values, token)
	}
	const rowWidth = 9
	if len(values)%rowWidth != 0 {
		panic(fmt.Errorf("section %q values=%d not %d-token rows", *sectionName, len(values), rowWidth))
	}
	rows := len(values) / rowWidth
	fmt.Printf("section=%s rows=%d\n", *sectionName, rows)
	printed := 0
	for offset := 0; offset < len(values); offset += rowWidth {
		if *limit > 0 && printed >= *limit {
			break
		}
		commodity := values[offset].Int
		itemID := values[offset+1].Int
		count := values[offset+2].Int
		price := values[offset+5].Int
		ref := refs[itemID]
		name := strings.TrimSpace(values[offset+6].Value)
		fmt.Printf("commodity=%d item=%d count=%d cera=%d name=%q path=%s\n", commodity, itemID, count, price, name, ref.Path)
		printed++
	}
}

func loadStackableRefs(archive *platformpvf.Archive) (map[int64]itemRef, error) {
	document, err := readDocument(archive, "stackable/stackable.lst")
	if err != nil {
		return nil, err
	}
	refs := make(map[int64]itemRef)
	for _, entry := range dnfpvf.ParseList(document) {
		resolved := path.Clean(path.Join("stackable", strings.TrimSpace(entry.Path)))
		refs[entry.ID] = itemRef{ID: entry.ID, Path: resolved}
	}
	return refs, nil
}

func readDocument(archive *platformpvf.Archive, documentPath string) (*dnfpvf.Document, error) {
	text, err := archive.ReadText(documentPath)
	if err != nil {
		return nil, err
	}
	document, err := dnfpvf.Parse(documentPath, text)
	if err != nil {
		return nil, err
	}
	return document, nil
}
