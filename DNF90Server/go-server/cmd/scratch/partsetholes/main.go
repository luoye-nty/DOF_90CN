// Command partsetholes audits every equipment .equ in a PVF for references to
// [part set index], then checks both etc/equipmentpartset.etc and the set file
// named by that index. It is read-only unless -extract is supplied, in which
// case source-PVF set files are written as PVFEditor text files.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	equipmentListPath = "equipment/equipment.lst"
	partSetListPath   = "etc/equipmentpartset.etc"
)

type itemReference struct {
	ID    int64
	Name  string
	Path  string
	SetID int64
}

type setReference struct {
	ID    int64
	Name  string
	Path  string
	Block string
}

func main() {
	currentPath := flag.String("pvf", "", "current Script.pvf")
	sourcePath := flag.String("source-pvf", "", "optional source Script.pvf")
	extractRoot := flag.String("extract", "", "optional directory for recoverable source set files")
	reportPath := flag.String("report", "", "optional detailed TSV report")
	consoleItemLimit := flag.Int("item-limit", 8, "maximum item rows printed per broken set")
	inspectSetID := flag.Int64("inspect-set", 0, "print all equipment references to one set id in both PVFs")
	exportSetID := flag.Int64("export-set", 0, "export one complete source-PVF part set and all equipment references")
	containersSetID := flag.Int64("containers-for-set", 0, "find stackable containers in source PVF that reference equipment in one set")
	flag.Parse()
	if *currentPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf is required")
		os.Exit(2)
	}

	current, err := platformpvf.Open(*currentPath)
	if err != nil {
		fatal("open current PVF", err)
	}
	currentItems, currentScanErrors, err := scanEquipment(current)
	if err != nil {
		fatal("scan current equipment", err)
	}
	currentSets, _, err := scanPartSetIndex(current, partSetListPath)
	if err != nil {
		fatal("scan current part-set index", err)
	}

	var source *platformpvf.Archive
	var sourceSets map[int64]setReference
	if *sourcePath != "" {
		source, err = platformpvf.Open(*sourcePath)
		if err != nil {
			fatal("open source PVF", err)
		}
		sourceSets, _, err = scanPartSetIndex(source, partSetListPath)
		if err != nil {
			fatal("scan source part-set index", err)
		}
	}
	if *inspectSetID > 0 {
		inspectSet(*inspectSetID, currentItems, currentSets, current, source, sourceSets)
	}
	if *exportSetID > 0 {
		if source == nil || *extractRoot == "" {
			fatal("export set", fmt.Errorf("-source-pvf and -extract are required"))
		}
		if err := exportSetBundle(*exportSetID, current, source, sourceSets, *extractRoot); err != nil {
			fatal("export set", err)
		}
		return
	}
	if *containersSetID > 0 {
		if source == nil {
			fatal("find containers", fmt.Errorf("-source-pvf is required"))
		}
		if err := findContainersForSet(*containersSetID, source); err != nil {
			fatal("find containers", err)
		}
		return
	}

	bySet := make(map[int64][]itemReference)
	for _, item := range currentItems {
		bySet[item.SetID] = append(bySet[item.SetID], item)
	}
	setIDs := make([]int64, 0, len(bySet))
	for setID := range bySet {
		setIDs = append(setIDs, setID)
	}
	sort.Slice(setIDs, func(i, j int) bool { return setIDs[i] < setIDs[j] })

	var report *os.File
	var writer *bufio.Writer
	if *reportPath != "" {
		report, err = os.Create(*reportPath)
		if err != nil {
			fatal("create report", err)
		}
		defer report.Close()
		writer = bufio.NewWriter(report)
		defer writer.Flush()
		fmt.Fprintln(writer, "status\tset_id\tset_name\tset_path\titem_id\titem_name\titem_path")
	}

	missingIndex := 0
	missingFile := 0
	recoverable := 0
	unrecoverable := 0
	extracted := 0
	extractedPaths := make(map[string]bool)
	var missingIndexIDs []int64
	for _, setID := range setIDs {
		items := bySet[setID]
		sort.Slice(items, func(i, j int) bool {
			if items[i].Path != items[j].Path {
				return items[i].Path < items[j].Path
			}
			return items[i].ID < items[j].ID
		})

		currentSet, indexed := currentSets[setID]
		issue := ""
		switch {
		case !indexed:
			issue = "missing_index"
			missingIndex++
			missingIndexIDs = append(missingIndexIDs, setID)
		default:
			if _, found := current.FindFile(currentSet.Path); !found {
				issue = "missing_file"
				missingFile++
			}
		}
		if issue == "" {
			continue
		}

		sourceSet, sourceIndexed := sourceSets[setID]
		sourceFileOK := false
		if source != nil && sourceIndexed {
			_, sourceFileOK = source.FindFile(sourceSet.Path)
		}
		sourceStatus := "no_source"
		if source != nil {
			switch {
			case !sourceIndexed:
				sourceStatus = "source_missing_index"
			case !sourceFileOK:
				sourceStatus = "source_missing_file"
			default:
				sourceStatus = "recoverable"
				recoverable++
			}
		}
		if source != nil && sourceStatus != "recoverable" {
			unrecoverable++
		}
		displaySet := currentSet
		if !indexed && sourceIndexed {
			displaySet = sourceSet
		}

		fmt.Printf("%s set=%d name=%q path=%q items=%d source=%s\n",
			strings.ToUpper(issue), setID, displaySet.Name, displaySet.Path, len(items), sourceStatus)
		for index, item := range items {
			if index < *consoleItemLimit {
				fmt.Printf("  ITEM id=%d name=%q path=%s\n", item.ID, item.Name, item.Path)
			}
			if writer != nil {
				fmt.Fprintf(writer, "%s\t%d\t%s\t%s\t%d\t%s\t%s\n",
					issue, setID, cleanField(displaySet.Name), displaySet.Path,
					item.ID, cleanField(item.Name), item.Path)
			}
		}
		if len(items) > *consoleItemLimit {
			fmt.Printf("  ... %d more item rows in report\n", len(items)-*consoleItemLimit)
		}

		if *extractRoot == "" || sourceStatus != "recoverable" || extractedPaths[sourceSet.Path] {
			continue
		}
		text, readErr := source.ReadText(sourceSet.Path)
		if readErr != nil {
			fmt.Printf("  EXTRACT_ERROR %v\n", readErr)
			continue
		}
		target := filepath.Join(*extractRoot, filepath.FromSlash(sourceSet.Path))
		if mkdirErr := os.MkdirAll(filepath.Dir(target), 0o755); mkdirErr != nil {
			fmt.Printf("  EXTRACT_ERROR mkdir %s: %v\n", filepath.Dir(target), mkdirErr)
			continue
		}
		if writeErr := os.WriteFile(target, []byte(pvfEditorText(text)), 0o644); writeErr != nil {
			fmt.Printf("  EXTRACT_ERROR write %s: %v\n", target, writeErr)
			continue
		}
		extractedPaths[sourceSet.Path] = true
		extracted++
		fmt.Printf("  EXTRACTED %s -> %s\n", sourceSet.Path, target)
	}

	if *extractRoot != "" && source != nil && len(missingIndexIDs) > 0 {
		if err := writeMergedPartSetIndex(current, sourceSets, missingIndexIDs, *extractRoot); err != nil {
			fmt.Printf("EXTRACT_INDEX_ERROR %v\n", err)
		}
	}

	fmt.Printf("SUMMARY equipment_files=%d scan_errors=%d referenced_sets=%d index_entries=%d missing_index=%d missing_file=%d recoverable=%d unrecoverable=%d extracted=%d\n",
		len(currentItems), currentScanErrors, len(bySet), len(currentSets), missingIndex, missingFile, recoverable, unrecoverable, extracted)
}

func findContainersForSet(setID int64, source *platformpvf.Archive) error {
	items, _, err := scanEquipment(source)
	if err != nil {
		return err
	}
	referenceIDs := make(map[int64]itemReference)
	for _, item := range items {
		if item.SetID == setID && item.ID > 0 {
			referenceIDs[item.ID] = item
		}
	}
	if len(referenceIDs) == 0 {
		return fmt.Errorf("set %d has no listed equipment", setID)
	}
	_, stackableByPath := scanList(source, "stackable/stackable.lst")

	type containerMatch struct {
		ID      int64
		Name    string
		Path    string
		Matches []int64
	}
	var matches []containerMatch
	covered := make(map[int64]bool)
	for _, file := range source.Files() {
		archivePath := strings.ToLower(strings.ReplaceAll(file.ArchivePath, "\\", "/"))
		if !strings.HasPrefix(archivePath, "stackable/") || !strings.HasSuffix(archivePath, ".stk") {
			continue
		}
		text, readErr := source.FileText(file.Index)
		if readErr != nil {
			continue
		}
		doc, parseErr := dnfpvf.Parse(file.ArchivePath, text)
		if parseErr != nil {
			continue
		}
		seen := make(map[int64]bool)
		for _, token := range doc.Tokens {
			if token.Kind != dnfpvf.TokenInt || token.Int <= 0 {
				continue
			}
			if _, ok := referenceIDs[token.Int]; ok {
				seen[token.Int] = true
				covered[token.Int] = true
			}
		}
		if len(seen) == 0 {
			continue
		}
		name, _ := doc.Text("name")
		ids := make([]int64, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		matches = append(matches, containerMatch{
			ID: stackableByPath[archivePath], Name: name, Path: file.ArchivePath, Matches: ids,
		})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
	for _, match := range matches {
		fmt.Printf("CONTAINER id=%d name=%q path=%s matched=%d\n",
			match.ID, match.Name, match.Path, len(match.Matches))
		if len(match.Matches) <= 20 {
			fmt.Printf("  ITEM_IDS %v\n", match.Matches)
		}
	}
	fmt.Printf("CONTAINER_SUMMARY set=%d equipment_ids=%d covered=%d uncovered=%d containers=%d\n",
		setID, len(referenceIDs), len(covered), len(referenceIDs)-len(covered), len(matches))
	return nil
}

func exportSetBundle(
	setID int64,
	current *platformpvf.Archive,
	source *platformpvf.Archive,
	sourceSets map[int64]setReference,
	extractRoot string,
) error {
	sourceItems, scanErrors, err := scanEquipment(source)
	if err != nil {
		return err
	}
	var items []itemReference
	for _, item := range sourceItems {
		if item.SetID == setID {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return fmt.Errorf("source PVF has no equipment referencing set %d", setID)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	sourceSet, indexed := sourceSets[setID]
	if !indexed {
		return fmt.Errorf("source PVF has no index entry for set %d", setID)
	}
	if _, found := source.FindFile(sourceSet.Path); !found {
		return fmt.Errorf("source PVF has no set file %s", sourceSet.Path)
	}

	_, currentListPaths := scanEquipmentList(current)
	_, sourceListPaths := scanEquipmentList(source)
	reportPath := filepath.Join(extractRoot, fmt.Sprintf("set_%d_items.tsv", setID))
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		return err
	}
	report, err := os.Create(reportPath)
	if err != nil {
		return err
	}
	defer report.Close()
	writer := bufio.NewWriter(report)
	defer writer.Flush()
	fmt.Fprintln(writer, "item_id\tname\tpath\tcurrent_file\tcurrent_set_id\tcurrent_equipment_lst_id\tsource_equipment_lst_id")

	extracted := 0
	alreadyLinked := 0
	currentMissing := 0
	currentDifferent := 0
	listMissing := 0
	for _, item := range items {
		text, readErr := source.ReadText(item.Path)
		if readErr != nil {
			return fmt.Errorf("read source item %s: %w", item.Path, readErr)
		}
		target := filepath.Join(extractRoot, filepath.FromSlash(item.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(pvfEditorText(text)), 0o644); err != nil {
			return err
		}
		extracted++

		currentFile := false
		currentSetID := int64(0)
		if _, found := current.FindFile(item.Path); found {
			currentFile = true
			currentText, currentErr := current.ReadText(item.Path)
			if currentErr != nil {
				return fmt.Errorf("read current item %s: %w", item.Path, currentErr)
			}
			currentDoc, currentErr := dnfpvf.Parse(item.Path, currentText)
			if currentErr != nil {
				return fmt.Errorf("parse current item %s: %w", item.Path, currentErr)
			}
			currentSetID, _ = currentDoc.Int("part set index")
			if currentSetID == setID {
				alreadyLinked++
			} else {
				currentDifferent++
			}
		} else {
			currentMissing++
		}
		currentListID := currentListPaths[strings.ToLower(item.Path)]
		sourceListID := sourceListPaths[strings.ToLower(item.Path)]
		if currentListID == 0 {
			listMissing++
		}
		fmt.Fprintf(writer, "%d\t%s\t%s\t%t\t%d\t%d\t%d\n",
			item.ID, cleanField(item.Name), item.Path, currentFile, currentSetID, currentListID, sourceListID)
	}

	setFileText, err := source.ReadText(sourceSet.Path)
	if err != nil {
		return err
	}
	setTarget := filepath.Join(extractRoot, filepath.FromSlash(sourceSet.Path))
	if err := os.MkdirAll(filepath.Dir(setTarget), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(setTarget, []byte(pvfEditorText(setFileText)), 0o644); err != nil {
		return err
	}
	if strings.TrimSpace(sourceSet.Block) == "" {
		return fmt.Errorf("source PVF index block for set %d is unavailable", setID)
	}
	appendTarget := filepath.Join(extractRoot, "etc", fmt.Sprintf("equipmentpartset_%d_append.txt", setID))
	if err := os.MkdirAll(filepath.Dir(appendTarget), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(appendTarget, []byte(strings.TrimSpace(sourceSet.Block)+"\n"), 0o644); err != nil {
		return err
	}
	indexText, err := current.ReadText(partSetListPath)
	if err != nil {
		return err
	}
	mergedIndex := strings.TrimRight(pvfEditorText(indexText), "\n") + "\n\n" +
		strings.TrimSpace(sourceSet.Block) + "\n"
	mergedIndexTarget := filepath.Join(extractRoot, filepath.FromSlash(partSetListPath))
	if err := os.MkdirAll(filepath.Dir(mergedIndexTarget), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(mergedIndexTarget, []byte(mergedIndex), 0o644); err != nil {
		return err
	}

	if listMissing > 0 {
		mergedList, err := mergedEquipmentListText(current, source, items)
		if err != nil {
			return err
		}
		listTarget := filepath.Join(extractRoot, filepath.FromSlash(equipmentListPath))
		if err := os.MkdirAll(filepath.Dir(listTarget), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(listTarget, []byte(mergedList), 0o644); err != nil {
			return err
		}
	}

	fmt.Printf("EXPORT_SET set=%d name=%q refs=%d scan_errors=%d extracted_items=%d current_missing=%d current_different=%d current_same=%d current_list_missing=%d\n",
		setID, sourceSet.Name, len(items), scanErrors, extracted, currentMissing, currentDifferent, alreadyLinked, listMissing)
	fmt.Printf("EXPORTED_SET_FILE %s\nEXPORTED_INDEX_APPEND %s\nEXPORTED_MERGED_INDEX %s\nEXPORTED_REPORT %s\n",
		setTarget, appendTarget, mergedIndexTarget, reportPath)
	return nil
}

func scanEquipmentList(archive *platformpvf.Archive) (map[int64]string, map[string]int64) {
	return scanList(archive, equipmentListPath)
}

func scanList(archive *platformpvf.Archive, listPath string) (map[int64]string, map[string]int64) {
	text, err := archive.ReadText(listPath)
	if err != nil {
		return nil, nil
	}
	doc, err := dnfpvf.Parse(listPath, text)
	if err != nil {
		return nil, nil
	}
	byID := make(map[int64]string)
	byPath := make(map[string]int64)
	for _, entry := range dnfpvf.ParseList(doc) {
		resolved := resolveReference(listPath, entry.Path)
		byID[entry.ID] = resolved
		byPath[strings.ToLower(resolved)] = entry.ID
	}
	return byID, byPath
}

func mergedEquipmentListText(current, source *platformpvf.Archive, items []itemReference) (string, error) {
	currentText, err := current.ReadText(equipmentListPath)
	if err != nil {
		return "", err
	}
	_, currentPaths := scanEquipmentList(current)
	sourceText, err := source.ReadText(equipmentListPath)
	if err != nil {
		return "", err
	}
	sourceDoc, err := dnfpvf.Parse(equipmentListPath, sourceText)
	if err != nil {
		return "", err
	}
	additions := make([]string, 0)
	for _, entry := range dnfpvf.ParseList(sourceDoc) {
		resolved := resolveReference(equipmentListPath, entry.Path)
		itemPath := strings.ToLower(resolved)
		if currentPaths[itemPath] != 0 {
			continue
		}
		for _, item := range items {
			if strings.ToLower(item.Path) != itemPath {
				continue
			}
			additions = append(additions, fmt.Sprintf("%d `%s`", entry.ID, entry.Path))
			currentPaths[itemPath] = entry.ID
			break
		}
	}
	result := strings.TrimRight(pvfEditorText(currentText), "\n")
	if len(additions) > 0 {
		result += "\n" + strings.Join(additions, "\n")
	}
	return result + "\n", nil
}

func inspectSet(
	setID int64,
	currentItems []itemReference,
	currentSets map[int64]setReference,
	current *platformpvf.Archive,
	source *platformpvf.Archive,
	sourceSets map[int64]setReference,
) {
	type sourceInspection struct {
		name  string
		items []itemReference
		sets  map[int64]setReference
	}
	currentInspection := sourceInspection{name: "current", items: currentItems, sets: currentSets}
	sourceInspectionValue := sourceInspection{name: "source", sets: sourceSets}
	if source != nil {
		items, scanErrors, err := scanEquipment(source)
		if err != nil {
			fmt.Printf("INSPECT source_scan_error=%v\n", err)
		} else {
			sourceInspectionValue.items = items
			fmt.Printf("INSPECT source_scan_errors=%d\n", scanErrors)
		}
	}
	for _, inspection := range []sourceInspection{currentInspection, sourceInspectionValue} {
		var items []itemReference
		for _, item := range inspection.items {
			if item.SetID == setID {
				items = append(items, item)
			}
		}
		set, indexed := inspection.sets[setID]
		fileOK := false
		var archive *platformpvf.Archive
		if inspection.name == "current" {
			archive = current
		} else {
			archive = source
		}
		if indexed && archive != nil {
			_, fileOK = archive.FindFile(set.Path)
		}
		fmt.Printf("INSPECT pvf=%s set=%d indexed=%t name=%q path=%q file_exists=%t equipment_refs=%d\n",
			inspection.name, setID, indexed, set.Name, set.Path, fileOK, len(items))
		for index, item := range items {
			if index >= 20 {
				fmt.Printf("  ... %d more references\n", len(items)-20)
				break
			}
			fmt.Printf("  ITEM id=%d name=%q path=%s\n", item.ID, item.Name, item.Path)
		}
	}
}

func writeMergedPartSetIndex(
	current *platformpvf.Archive,
	sourceSets map[int64]setReference,
	missingIDs []int64,
	extractRoot string,
) error {
	text, err := current.ReadText(partSetListPath)
	if err != nil {
		return err
	}
	base := strings.TrimRight(pvfEditorText(text), "\n")
	var blocks []string
	for _, setID := range missingIDs {
		sourceSet, ok := sourceSets[setID]
		if !ok || strings.TrimSpace(sourceSet.Block) == "" {
			continue
		}
		blocks = append(blocks, strings.TrimSpace(sourceSet.Block))
	}
	if len(blocks) == 0 {
		return fmt.Errorf("no recoverable part-set index blocks")
	}
	merged := base + "\n\n" + strings.Join(blocks, "\n\n") + "\n"
	mergedPath := filepath.Join(extractRoot, filepath.FromSlash(partSetListPath))
	if err := os.MkdirAll(filepath.Dir(mergedPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(mergedPath, []byte(merged), 0o644); err != nil {
		return err
	}
	addendumPath := filepath.Join(extractRoot, "etc", "equipmentpartset_missing_blocks.txt")
	if err := os.WriteFile(addendumPath, []byte(strings.Join(blocks, "\n\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("EXTRACTED_INDEX %s (%d blocks) and %s\n", mergedPath, len(blocks), addendumPath)
	return nil
}

func scanEquipment(archive *platformpvf.Archive) ([]itemReference, int, error) {
	listText, err := archive.ReadText(equipmentListPath)
	if err != nil {
		return nil, 0, err
	}
	listDoc, err := dnfpvf.Parse(equipmentListPath, listText)
	if err != nil {
		return nil, 0, err
	}
	listIDs := make(map[string]int64)
	for _, entry := range dnfpvf.ParseList(listDoc) {
		listIDs[strings.ToLower(resolveReference(equipmentListPath, entry.Path))] = entry.ID
	}

	var items []itemReference
	scanErrors := 0
	files := archive.Files()
	for _, file := range files {
		archivePath := strings.ToLower(strings.ReplaceAll(file.ArchivePath, "\\", "/"))
		if !strings.HasPrefix(archivePath, "equipment/") || !strings.HasSuffix(archivePath, ".equ") {
			continue
		}
		text, readErr := archive.FileText(file.Index)
		if readErr != nil {
			scanErrors++
			continue
		}
		doc, parseErr := dnfpvf.Parse(file.ArchivePath, text)
		if parseErr != nil {
			scanErrors++
			continue
		}
		setID, found := doc.Int("part set index")
		if !found || setID <= 0 {
			continue
		}
		name, _ := doc.Text("name")
		itemID := listIDs[strings.ToLower(file.ArchivePath)]
		if itemID == 0 {
			base := strings.TrimSuffix(path.Base(file.ArchivePath), path.Ext(file.ArchivePath))
			itemID, _ = strconv.ParseInt(base, 10, 64)
		}
		items = append(items, itemReference{
			ID: itemID, Name: name, Path: file.ArchivePath, SetID: setID,
		})
	}
	return items, scanErrors, nil
}

func scanPartSetIndex(archive *platformpvf.Archive, indexPath string) (map[int64]setReference, int, error) {
	text, err := archive.ReadText(indexPath)
	if err != nil {
		return nil, 0, err
	}
	blocks, blockErrors := parseSetBlocks(indexPath, text)
	doc, err := dnfpvf.Parse(indexPath, text)
	if err != nil {
		return nil, blockErrors, err
	}
	sets := make(map[int64]setReference)
	for index := 0; index+1 < len(doc.Tokens); index++ {
		id := doc.Tokens[index]
		ref := doc.Tokens[index+1]
		if id.Kind != dnfpvf.TokenInt || id.Int <= 0 ||
			(ref.Kind != dnfpvf.TokenString && ref.Kind != dnfpvf.TokenIdent) ||
			!strings.HasSuffix(strings.ToLower(strings.TrimSpace(ref.Value)), ".equ") {
			continue
		}
		entry := setReference{ID: id.Int, Path: resolveReference(indexPath, ref.Value), Block: blocks[id.Int]}
		if index+2 < len(doc.Tokens) {
			nameToken := doc.Tokens[index+2]
			if nameToken.Kind == dnfpvf.TokenString || nameToken.Kind == dnfpvf.TokenIdent {
				entry.Name = nameToken.Value
			}
		}
		if _, exists := sets[entry.ID]; !exists {
			sets[entry.ID] = entry
		}
	}
	return sets, blockErrors, nil
}

func parseSetBlocks(name, text string) (map[int64]string, int) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	blocks := make(map[int64]string)
	errors := 0
	start := -1
	for index, line := range lines {
		tag := strings.ToLower(strings.TrimSpace(line))
		switch tag {
		case "[equipment part set]":
			start = index
		case "[/equipment part set]":
			if start < 0 {
				continue
			}
			blockText := strings.Join(lines[start:index+1], "\n")
			doc, err := dnfpvf.Parse(name, blockText)
			if err != nil {
				errors++
				start = -1
				continue
			}
			for tokenIndex := 0; tokenIndex+1 < len(doc.Tokens); tokenIndex++ {
				id := doc.Tokens[tokenIndex]
				ref := doc.Tokens[tokenIndex+1]
				if id.Kind == dnfpvf.TokenInt && id.Int > 0 &&
					(ref.Kind == dnfpvf.TokenString || ref.Kind == dnfpvf.TokenIdent) &&
					strings.HasSuffix(strings.ToLower(strings.TrimSpace(ref.Value)), ".equ") {
					blocks[id.Int] = blockText
					break
				}
			}
			start = -1
		}
	}
	return blocks, errors
}

func resolveReference(listPath, reference string) string {
	reference = strings.TrimSpace(strings.Trim(reference, "`\"'"))
	reference = strings.ReplaceAll(reference, "\\", "/")
	if strings.HasPrefix(strings.ToLower(reference), "equipment/") {
		return path.Clean(reference)
	}
	if path.Clean(listPath) == partSetListPath {
		// equipmentpartset.etc stores paths relative to the equipment root,
		// even though the index itself lives under etc/.
		return path.Clean(path.Join("equipment", reference))
	}
	return path.Clean(path.Join(path.Dir(listPath), reference))
}

func pvfEditorText(text string) string {
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.TrimLeft(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	if !strings.HasPrefix(text, "#PVF_File") {
		text = "#PVF_File\n\n" + text
	}
	return strings.TrimRight(text, "\n") + "\n"
}

func cleanField(value string) string {
	value = strings.ReplaceAll(value, "\t", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

func fatal(operation string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", operation, err)
	os.Exit(1)
}
