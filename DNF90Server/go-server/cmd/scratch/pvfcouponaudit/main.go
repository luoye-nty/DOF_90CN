// pvfcouponaudit inventories cash-shop package contents that resolve to coupon,
// flower-basket, or exchange-related stackable items.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type packageRow struct {
	CommodityID int64  `json:"commodity_id"`
	ItemID      int64  `json:"item_id"`
	Count       int64  `json:"count"`
	Cera        int64  `json:"cera"`
	Name        string `json:"name"`
	Job         int64  `json:"job"`
}

type itemMetadata struct {
	ItemID      int64    `json:"item_id"`
	Path        string   `json:"path"`
	Name        string   `json:"name"`
	Explain     string   `json:"explain"`
	FlavorText  string   `json:"flavor_text"`
	Stackable   string   `json:"stackable_type"`
	MatchTerms  []string `json:"match_terms"`
	MatchFields []string `json:"match_fields"`
	Description string   `json:"description"`
}

type auditRow struct {
	CommodityID     int64  `json:"commodity_id"`
	PackageItemID   int64  `json:"package_item_id"`
	PackageName     string `json:"package_name"`
	PackagePath     string `json:"package_path"`
	PackageCera     int64  `json:"package_cera"`
	PackageJob      int64  `json:"package_job"`
	CandidateItemID int64  `json:"candidate_item_id"`
	CandidateName   string `json:"candidate_name"`
	CandidatePath   string `json:"candidate_path"`
	StackableType   string `json:"stackable_type"`
	MatchTerms      string `json:"match_terms"`
	MatchFields     string `json:"match_fields"`
	Description     string `json:"description"`
	Depth           int    `json:"depth"`
	Chain           string `json:"chain"`
}

type auditOutput struct {
	Source                  string         `json:"source"`
	PackageRows             int            `json:"package_rows"`
	UniquePackageItems      int            `json:"unique_package_items"`
	CandidateItemCount      int            `json:"candidate_item_count"`
	MatchedPackageItemCount int            `json:"matched_package_item_count"`
	MatchedAssociationCount int            `json:"matched_association_count"`
	Candidates              []itemMetadata `json:"candidates"`
	Rows                    []auditRow     `json:"rows"`
}

type itemRef struct {
	ID   int64
	Path string
}

type edge struct {
	ItemID int64
	Count  int64
	Kind   string
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	outPath := flag.String("out", "", "optional JSON output path")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" {
		fmt.Fprintln(os.Stderr, "-pvf is required")
		os.Exit(2)
	}

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	refs, pathToID, err := loadStackableRefs(archive)
	if err != nil {
		panic(err)
	}
	rows, err := loadPackageRows(archive)
	if err != nil {
		panic(err)
	}

	candidates, err := scanCandidates(archive, pathToID)
	if err != nil {
		panic(err)
	}

	docCache := make(map[int64]*dnfpvf.Document)
	edgesCache := make(map[int64][]edge)
	var associations []auditRow
	matchedPackages := make(map[int64]struct{})
	for _, row := range rows {
		if row.ItemID <= 0 {
			continue
		}
		rootRef, ok := refs[row.ItemID]
		if !ok {
			continue
		}
		packageName := row.Name
		if rootDoc := docCache[row.ItemID]; rootDoc != nil {
			if name := firstText(rootDoc, "name"); name != "" {
				packageName = name
			}
		} else if rootDoc, loadErr := readDocument(archive, rootRef.Path); loadErr == nil {
			docCache[row.ItemID] = rootDoc
			if name := firstText(rootDoc, "name"); name != "" {
				packageName = name
			}
		}
		type visit struct {
			ID       int64
			Depth    int
			Relation []string
		}
		queue := []visit{{ID: row.ItemID, Depth: 0}}
		seen := map[int64]bool{row.ItemID: true}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if candidate, ok := candidates[current.ID]; ok {
				matchedPackages[row.ItemID] = struct{}{}
				chain := rootRef.Path
				if len(current.Relation) > 0 {
					chain += " -> " + strings.Join(current.Relation, " -> ")
				}
				associations = append(associations, auditRow{
					CommodityID: row.CommodityID, PackageItemID: row.ItemID,
					PackageName: packageName, PackagePath: rootRef.Path,
					PackageCera: row.Cera, PackageJob: row.Job,
					CandidateItemID: current.ID, CandidateName: candidate.Name,
					CandidatePath: candidate.Path, StackableType: candidate.Stackable,
					MatchTerms:  strings.Join(candidate.MatchTerms, "、"),
					MatchFields: strings.Join(candidate.MatchFields, "、"),
					Description: candidate.Description, Depth: current.Depth, Chain: chain,
				})
			}
			children, found := edgesCache[current.ID]
			if !found {
				children = extractEdges(archive, refs, docCache, current.ID)
				edgesCache[current.ID] = children
			}
			for _, child := range children {
				if child.ItemID <= 0 || seen[child.ItemID] {
					continue
				}
				seen[child.ItemID] = true
				childRef := refs[child.ItemID]
				relationLabel := fmt.Sprintf("%d %s x%d [%s]", child.ItemID, childRef.Path, child.Count, child.Kind)
				queue = append(queue, visit{
					ID: child.ItemID, Depth: current.Depth + 1,
					Relation: append(append([]string(nil), current.Relation...), relationLabel),
				})
			}
		}
	}

	candidateRows := make([]itemMetadata, 0, len(candidates))
	for _, candidate := range candidates {
		candidateRows = append(candidateRows, candidate)
	}
	sort.Slice(candidateRows, func(i, j int) bool {
		if candidateRows[i].Name != candidateRows[j].Name {
			return candidateRows[i].Name < candidateRows[j].Name
		}
		return candidateRows[i].ItemID < candidateRows[j].ItemID
	})
	sort.Slice(associations, func(i, j int) bool {
		if associations[i].PackageName != associations[j].PackageName {
			return associations[i].PackageName < associations[j].PackageName
		}
		if associations[i].PackageItemID != associations[j].PackageItemID {
			return associations[i].PackageItemID < associations[j].PackageItemID
		}
		return associations[i].CandidateItemID < associations[j].CandidateItemID
	})

	result := auditOutput{
		Source:                  *pvfPath,
		PackageRows:             len(rows),
		UniquePackageItems:      uniquePackageItems(rows),
		CandidateItemCount:      len(candidateRows),
		MatchedPackageItemCount: len(matchedPackages),
		MatchedAssociationCount: len(associations),
		Candidates:              candidateRows,
		Rows:                    associations,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			panic(err)
		}
		return
	}
	fmt.Println(string(data))
}

func uniquePackageItems(rows []packageRow) int {
	seen := make(map[int64]struct{})
	for _, row := range rows {
		if row.ItemID > 0 {
			seen[row.ItemID] = struct{}{}
		}
	}
	return len(seen)
}

func loadPackageRows(archive *platformpvf.Archive) ([]packageRow, error) {
	doc, err := readDocument(archive, "etc/cerashop.etc")
	if err != nil {
		return nil, err
	}
	tokens, ok := doc.Section("package")
	if !ok {
		return nil, fmt.Errorf("etc/cerashop.etc [package] not found")
	}
	values := make([]dnfpvf.Token, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenSection {
			values = append(values, token)
		}
	}
	const rowWidth = 11
	if len(values)%rowWidth != 0 {
		return nil, fmt.Errorf("[package] has %d non-section tokens, not %d-wide rows", len(values), rowWidth)
	}
	rows := make([]packageRow, 0, len(values)/rowWidth)
	for offset := 0; offset < len(values); offset += rowWidth {
		row := values[offset : offset+rowWidth]
		rows = append(rows, packageRow{
			CommodityID: row[0].Int,
			ItemID:      row[1].Int,
			Count:       row[2].Int,
			Cera:        row[5].Int,
			Name:        strings.TrimSpace(row[6].Value),
			Job:         row[10].Int,
		})
	}
	return rows, nil
}

func loadStackableRefs(archive *platformpvf.Archive) (map[int64]itemRef, map[string]int64, error) {
	doc, err := readDocument(archive, "stackable/stackable.lst")
	if err != nil {
		return nil, nil, err
	}
	refs := make(map[int64]itemRef)
	pathToID := make(map[string]int64)
	for _, entry := range dnfpvf.ParseList(doc) {
		resolved := resolvePath(archive, "stackable/stackable.lst", entry.Path)
		if resolved == "" {
			continue
		}
		ref := itemRef{ID: entry.ID, Path: resolved}
		refs[entry.ID] = ref
		pathToID[strings.ToLower(path.Clean(resolved))] = entry.ID
	}
	return refs, pathToID, nil
}

func scanCandidates(archive *platformpvf.Archive, pathToID map[string]int64) (map[int64]itemMetadata, error) {
	allTerms := []string{"优惠券", "花篮", "兑换券", "代金券", "礼券", "兑换"}
	results := make(map[int64]itemMetadata)
	for _, file := range archive.Files() {
		lower := strings.ToLower(file.ArchivePath)
		if !strings.HasPrefix(lower, "stackable/") || !strings.HasSuffix(lower, ".stk") {
			continue
		}
		raw, err := archive.FileText(file.Index)
		if err != nil || !containsAny(raw, allTerms) {
			continue
		}
		id, ok := pathToID[strings.ToLower(path.Clean(file.ArchivePath))]
		if !ok {
			continue
		}
		doc, err := dnfpvf.Parse(file.ArchivePath, raw)
		if err != nil {
			continue
		}
		name := firstText(doc, "name")
		explain := firstText(doc, "explain")
		flavor := firstText(doc, "flavor text")
		stackable := firstText(doc, "stackable type")
		fields := []struct {
			Name string
			Text string
		}{
			{"名称", name},
			{"描述", explain},
			{"风味文本", flavor},
		}
		var terms []string
		var matchedFields []string
		for _, field := range fields {
			for _, term := range allTerms {
				if strings.Contains(field.Text, term) {
					terms = appendUnique(terms, term)
					matchedFields = appendUnique(matchedFields, field.Name)
				}
			}
		}
		if len(terms) == 0 {
			continue
		}
		description := strings.TrimSpace(strings.Join(nonEmpty([]string{explain, flavor}), "\n"))
		results[id] = itemMetadata{
			ItemID: id, Path: file.ArchivePath, Name: name, Explain: explain,
			FlavorText: flavor, Stackable: stackable, MatchTerms: terms,
			MatchFields: matchedFields, Description: description,
		}
	}
	return results, nil
}

func extractEdges(
	archive *platformpvf.Archive,
	refs map[int64]itemRef,
	cache map[int64]*dnfpvf.Document,
	itemID int64,
) []edge {
	ref, ok := refs[itemID]
	if !ok {
		return nil
	}
	doc := cache[itemID]
	if doc == nil {
		loaded, err := readDocument(archive, ref.Path)
		if err != nil {
			return nil
		}
		cache[itemID] = loaded
		doc = loaded
	}
	out := make([]edge, 0)
	appendPairs := func(values []int64, kind string) {
		if len(values)%2 != 0 {
			return
		}
		for offset := 0; offset < len(values); offset += 2 {
			out = append(out, edge{ItemID: values[offset], Count: values[offset+1], Kind: kind})
		}
	}
	if values := doc.Ints("package data"); len(values) > 0 {
		appendPairs(values, "package data")
	}
	for _, section := range doc.Sections {
		if strings.EqualFold(strings.TrimSpace(section.Name), "package data selection") {
			appendPairs(sectionInts(doc, section), "package data selection")
		}
		if strings.EqualFold(strings.TrimSpace(section.Name), "etc") {
			values := sectionInts(doc, section)
			if len(values) > 0 && len(values)%4 == 0 {
				for offset := 0; offset < len(values); offset += 4 {
					out = append(out, edge{ItemID: values[offset+1], Count: values[offset+3], Kind: "booster info"})
				}
			}
		}
	}
	return out
}

func sectionInts(doc *dnfpvf.Document, section dnfpvf.Section) []int64 {
	if section.Start < 0 || section.End > len(doc.Tokens) || section.Start > section.End {
		return nil
	}
	out := make([]int64, 0, section.End-section.Start)
	for _, token := range doc.Tokens[section.Start:section.End] {
		if token.Kind == dnfpvf.TokenInt {
			out = append(out, token.Int)
		}
	}
	return out
}

func firstText(doc *dnfpvf.Document, section string) string {
	values := doc.Texts(section)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func readDocument(archive *platformpvf.Archive, documentPath string) (*dnfpvf.Document, error) {
	text, err := archive.ReadText(documentPath)
	if err != nil {
		return nil, err
	}
	return dnfpvf.Parse(documentPath, text)
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

func containsAny(text string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}
