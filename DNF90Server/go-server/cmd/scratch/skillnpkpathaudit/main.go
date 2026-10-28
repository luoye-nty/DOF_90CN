// skillnpkpathaudit closes the loop between PVF skill/ANI IMG references and
// the internal IMG indexes of a selected set of NPK archives.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const npkMagic = "NeoplePack_Bill\x00"

var (
	npkNameKey = []byte("puchikon@neople dungeon and fighter " +
		"DNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNF\x00")
	imagePair = regexp.MustCompile("(?i)`([^`]+\\.img)`\\s+(-?\\d+)")
)

type npkEntry struct {
	Path   string
	Offset uint32
	Length uint32
	Frames uint32
}

type documentRefs struct {
	Path string
	Refs []imageRef
}

type imageRef struct {
	Path  string
	Frame int
}

type packageResult struct {
	Name                string `json:"name"`
	IMGEntries          int    `json:"img_entries"`
	ReferencedIMGPaths  int    `json:"referenced_img_paths"`
	ReferencingPVFANIs  int    `json:"referencing_pvf_anis"`
	DirectSkillIconRefs int    `json:"direct_skill_icon_refs"`
}

type unmatchedRef struct {
	Document string `json:"document"`
	IMG      string `json:"img"`
	Frame    int    `json:"frame"`
	OwnerNPK string `json:"owner_npk,omitempty"`
}

type frameFailure struct {
	Document string `json:"document"`
	IMG      string `json:"img"`
	Frame    int    `json:"frame"`
	Frames   uint32 `json:"available_frames"`
}

type report struct {
	SelectedPackages             int             `json:"selected_packages"`
	SelectedUniqueIMGPaths       int             `json:"selected_unique_img_paths"`
	SelectedDuplicateIMGPaths    int             `json:"selected_duplicate_img_paths"`
	SelectedNPKReadFailures      []string        `json:"selected_npk_read_failures,omitempty"`
	PVFAnimationDocumentsScanned int             `json:"pvf_animation_documents_scanned"`
	RelevantPVFAnimationDocs     int             `json:"relevant_pvf_animation_documents"`
	RelevantPVFIMGReferences     int             `json:"relevant_pvf_img_references"`
	SelectedPVFIMGReferences     int             `json:"selected_pvf_img_references"`
	ExternalPVFIMGReferences     int             `json:"external_pvf_img_references"`
	MissingPVFIMGReferences      int             `json:"missing_pvf_img_references"`
	OutOfRangeFrameReferences    int             `json:"out_of_range_frame_references"`
	ExclusiveSkills              int             `json:"exclusive_skills"`
	ExclusiveSkillIconReferences int             `json:"exclusive_skill_icon_references"`
	MatchedSkillIconReferences   int             `json:"matched_skill_icon_references"`
	ExternalSkillIconReferences  int             `json:"external_skill_icon_references"`
	MissingSkillIconReferences   int             `json:"missing_skill_icon_references"`
	Packages                     []packageResult `json:"packages"`
	PackagesWithoutPVFReference  []string        `json:"packages_without_pvf_reference,omitempty"`
	ExternalReferences           []unmatchedRef  `json:"external_references,omitempty"`
	MissingReferences            []unmatchedRef  `json:"missing_references,omitempty"`
	FrameFailures                []frameFailure  `json:"frame_failures,omitempty"`
	MissingSkillIcons            []unmatchedRef  `json:"missing_skill_icons,omitempty"`
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	manifestPath := flag.String("manifest", "", "selected package CSV containing a filename column")
	selectedDir := flag.String("selected-dir", "", "directory containing selected NPKs")
	allNPKDir := flag.String("all-npk-dir", "", "complete source ImagePacks2 directory")
	skillSpecs := flag.String("skills", "", "comma-separated archive-list:branch pairs")
	aniPrefixes := flag.String("ani-prefixes", "", "comma-separated PVF animation path prefixes")
	flag.Parse()
	for name, value := range map[string]string{
		"-pvf": *pvfPath, "-manifest": *manifestPath, "-selected-dir": *selectedDir,
		"-all-npk-dir": *allNPKDir, "-skills": *skillSpecs, "-ani-prefixes": *aniPrefixes,
	} {
		if strings.TrimSpace(value) == "" {
			fmt.Fprintf(os.Stderr, "%s is required\n", name)
			os.Exit(2)
		}
	}

	selectedNames, err := readManifest(*manifestPath)
	check(err)
	selectedOwners := make(map[string][]string)
	selectedEntries := make(map[string]npkEntry)
	packageEntries := make(map[string]int)
	selectedFailures := make([]string, 0)
	for _, name := range selectedNames {
		entries, readErr := readNPK(filepath.Join(*selectedDir, name), true)
		if readErr != nil {
			selectedFailures = append(selectedFailures, fmt.Sprintf("%s: %v", name, readErr))
			continue
		}
		packageEntries[name] = len(entries)
		for _, entry := range entries {
			key := normalizeIMG(entry.Path)
			selectedOwners[key] = appendUnique(selectedOwners[key], name)
			if _, exists := selectedEntries[key]; !exists {
				selectedEntries[key] = entry
			}
		}
	}

	allOwners := make(map[string]string)
	allFiles, err := filepath.Glob(filepath.Join(*allNPKDir, "*.NPK"))
	check(err)
	for _, filePath := range allFiles {
		entries, readErr := readNPK(filePath, false)
		if readErr != nil {
			continue
		}
		name := filepath.Base(filePath)
		for _, entry := range entries {
			key := normalizeIMG(entry.Path)
			if _, exists := allOwners[key]; !exists {
				allOwners[key] = name
			}
		}
	}

	archive, err := platformpvf.Open(*pvfPath)
	check(err)
	prefixes := splitNormalized(*aniPrefixes)
	documents := make([]documentRefs, 0, 4096)
	for _, file := range archive.Files() {
		archivePath := normalize(file.ArchivePath)
		if !hasAnyPrefix(archivePath, prefixes) || !(strings.HasSuffix(archivePath, ".ani") || strings.HasSuffix(archivePath, ".ani.als")) {
			continue
		}
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil {
			continue
		}
		refs := parseImageRefs(text)
		if len(refs) > 0 {
			documents = append(documents, documentRefs{Path: archivePath, Refs: refs})
		}
	}

	// Generic icon/cut-in/grow-type containers must not make every class ANI a
	// seed. A relevant ANI is seeded by a profession-specific effect archive.
	seedPaths := make(map[string]struct{})
	for imagePath, owners := range selectedOwners {
		for _, owner := range owners {
			lower := strings.ToLower(owner)
			if lower == "sprite_character_swordman_effect.npk" || lower == "sprite_character_mage_effect.npk" ||
				strings.Contains(lower, "effect_cutin") || strings.Contains(lower, "equipment_growtype") {
				continue
			}
			seedPaths[imagePath] = struct{}{}
		}
	}

	packageReferencedPaths := make(map[string]map[string]struct{})
	packageDocuments := make(map[string]map[string]struct{})
	external := make([]unmatchedRef, 0)
	missing := make([]unmatchedRef, 0)
	frameFailures := make([]frameFailure, 0)
	relevantDocuments := 0
	relevantRefs := 0
	selectedRefs := 0
	for _, document := range documents {
		relevant := false
		for _, ref := range document.Refs {
			if _, ok := seedPaths[ref.Path]; ok {
				relevant = true
				break
			}
		}
		if !relevant {
			continue
		}
		relevantDocuments++
		for _, ref := range document.Refs {
			relevantRefs++
			if owners := selectedOwners[ref.Path]; len(owners) > 0 {
				selectedRefs++
				entry := selectedEntries[ref.Path]
				if ref.Frame >= 0 && uint32(ref.Frame) >= entry.Frames {
					frameFailures = append(frameFailures, frameFailure{Document: document.Path, IMG: ref.Path, Frame: ref.Frame, Frames: entry.Frames})
				}
				for _, owner := range owners {
					if packageReferencedPaths[owner] == nil {
						packageReferencedPaths[owner] = make(map[string]struct{})
						packageDocuments[owner] = make(map[string]struct{})
					}
					packageReferencedPaths[owner][ref.Path] = struct{}{}
					packageDocuments[owner][document.Path] = struct{}{}
				}
				continue
			}
			owner := allOwners[ref.Path]
			row := unmatchedRef{Document: document.Path, IMG: ref.Path, Frame: ref.Frame, OwnerNPK: owner}
			if owner == "" {
				missing = append(missing, row)
			} else {
				external = append(external, row)
			}
		}
	}

	exclusiveSkills, iconRefs, matchedIcons, externalIcons, missingIcons := auditSkills(archive, *skillSpecs, selectedOwners, allOwners, packageReferencedPaths)
	packages := make([]packageResult, 0, len(selectedNames))
	withoutRefs := make([]string, 0)
	for _, name := range selectedNames {
		row := packageResult{
			Name: name, IMGEntries: packageEntries[name],
			ReferencedIMGPaths: len(packageReferencedPaths[name]), ReferencingPVFANIs: len(packageDocuments[name]),
		}
		for _, ref := range matchedIcons {
			for _, owner := range selectedOwners[ref.IMG] {
				if owner == name {
					row.DirectSkillIconRefs++
				}
			}
		}
		if row.ReferencedIMGPaths == 0 && row.DirectSkillIconRefs == 0 {
			withoutRefs = append(withoutRefs, name)
		}
		packages = append(packages, row)
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	sort.Strings(withoutRefs)
	report := report{
		SelectedPackages: len(selectedNames), SelectedUniqueIMGPaths: len(selectedOwners),
		SelectedDuplicateIMGPaths: duplicateCount(selectedOwners), SelectedNPKReadFailures: selectedFailures,
		PVFAnimationDocumentsScanned: len(documents), RelevantPVFAnimationDocs: relevantDocuments,
		RelevantPVFIMGReferences: relevantRefs, SelectedPVFIMGReferences: selectedRefs,
		ExternalPVFIMGReferences: len(external), MissingPVFIMGReferences: len(missing),
		OutOfRangeFrameReferences: len(frameFailures), ExclusiveSkills: exclusiveSkills,
		ExclusiveSkillIconReferences: iconRefs, MatchedSkillIconReferences: len(matchedIcons),
		ExternalSkillIconReferences: len(externalIcons), MissingSkillIconReferences: len(missingIcons),
		Packages: packages, PackagesWithoutPVFReference: withoutRefs,
		ExternalReferences: uniqueUnmatched(external), MissingReferences: uniqueUnmatched(missing),
		FrameFailures: frameFailures, MissingSkillIcons: uniqueUnmatched(missingIcons),
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	check(encoder.Encode(report))
}

func auditSkills(archive *platformpvf.Archive, specs string, selected map[string][]string, all map[string]string, packageRefs map[string]map[string]struct{}) (int, int, []unmatchedRef, []unmatchedRef, []unmatchedRef) {
	exclusiveCount := 0
	iconCount := 0
	matched := make([]unmatchedRef, 0)
	external := make([]unmatchedRef, 0)
	missing := make([]unmatchedRef, 0)
	for _, rawSpec := range strings.Split(specs, ",") {
		parts := strings.Split(strings.TrimSpace(rawSpec), ":")
		if len(parts) != 2 {
			panic(fmt.Errorf("invalid skill spec %q", rawSpec))
		}
		branch, err := strconv.Atoi(parts[1])
		check(err)
		listPath := normalize(parts[0])
		listText, err := archive.ReadText(listPath)
		check(err)
		listDoc, err := dnfpvf.Parse(listPath, listText)
		check(err)
		for _, item := range dnfpvf.ParseList(listDoc) {
			docPath := resolve(archive, listPath, item.Path)
			if docPath == "" {
				continue
			}
			text, readErr := archive.ReadText(docPath)
			if readErr != nil {
				continue
			}
			doc, parseErr := dnfpvf.Parse(docPath, text)
			if parseErr != nil || !exclusiveToBranch(doc, branch) {
				continue
			}
			exclusiveCount++
			for _, ref := range parseImageRefs(text) {
				iconCount++
				row := unmatchedRef{Document: docPath, IMG: ref.Path, Frame: ref.Frame, OwnerNPK: all[ref.Path]}
				if owners := selected[ref.Path]; len(owners) > 0 {
					matched = append(matched, row)
					for _, owner := range owners {
						if packageRefs[owner] == nil {
							packageRefs[owner] = make(map[string]struct{})
						}
						packageRefs[owner][ref.Path] = struct{}{}
					}
				} else if row.OwnerNPK != "" {
					external = append(external, row)
				} else {
					missing = append(missing, row)
				}
			}
		}
	}
	return exclusiveCount, iconCount, matched, external, missing
}

func exclusiveToBranch(doc *dnfpvf.Document, wanted int) bool {
	primary := doc.Ints("growtype maximum level")
	second := doc.Ints("second growtype maximum level")
	fitness := doc.Ints("skill fitness growtype")
	if !enabled(primary, second, fitness, wanted) {
		return false
	}
	branches := len(primary)
	if half := len(second) / 2; half > branches {
		branches = half
	}
	for branch := 0; branch < branches; branch++ {
		if branch != wanted && enabled(primary, second, fitness, branch) {
			return false
		}
	}
	return true
}

func enabled(primary, second, fitness []int64, branch int) bool {
	if branch < len(primary) && primary[branch] > 0 {
		return true
	}
	first := branch * 2
	if first < len(second) && second[first] > 0 || first+1 < len(second) && second[first+1] > 0 {
		return true
	}
	for _, value := range fitness {
		if int(value) == branch {
			return true
		}
	}
	return false
}

func parseImageRefs(text string) []imageRef {
	refs := make([]imageRef, 0)
	for _, match := range imagePair.FindAllStringSubmatch(text, -1) {
		frame, _ := strconv.Atoi(match[2])
		refs = append(refs, imageRef{Path: normalizeIMG(match[1]), Frame: frame})
	}
	return refs
}

func readManifest(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty manifest")
	}
	filenameColumn := -1
	for index, name := range rows[0] {
		if strings.EqualFold(strings.TrimSpace(name), "filename") {
			filenameColumn = index
			break
		}
	}
	if filenameColumn < 0 {
		return nil, fmt.Errorf("manifest has no filename column")
	}
	names := make([]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		if filenameColumn < len(row) && strings.TrimSpace(row[filenameColumn]) != "" {
			names = append(names, strings.TrimSpace(row[filenameColumn]))
		}
	}
	return names, nil
}

func readNPK(filePath string, metadata bool) ([]npkEntry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, err
	}
	if string(header[:16]) != npkMagic {
		return nil, fmt.Errorf("invalid magic")
	}
	count := binary.LittleEndian.Uint32(header[16:20])
	if count > 1_000_000 {
		return nil, fmt.Errorf("unreasonable IMG count %d", count)
	}
	entries := make([]npkEntry, 0, count)
	for index := uint32(0); index < count; index++ {
		row := make([]byte, 264)
		if _, err := io.ReadFull(file, row); err != nil {
			return nil, err
		}
		nameBytes := append([]byte(nil), row[8:]...)
		for position := range nameBytes {
			nameBytes[position] ^= npkNameKey[position]
		}
		name := string(nameBytes)
		if zero := strings.IndexByte(name, 0); zero >= 0 {
			name = name[:zero]
		}
		entries = append(entries, npkEntry{Path: strings.TrimSpace(name), Offset: binary.LittleEndian.Uint32(row[:4]), Length: binary.LittleEndian.Uint32(row[4:8])})
	}
	if metadata {
		for index := range entries {
			entries[index].Frames, err = readFrames(file, entries[index])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", entries[index].Path, err)
			}
		}
	}
	return entries, nil
}

func readFrames(file *os.File, entry npkEntry) (uint32, error) {
	if _, err := file.Seek(int64(entry.Offset), io.SeekStart); err != nil {
		return 0, err
	}
	reader := bufio.NewReader(file)
	magic, err := reader.ReadString(0)
	if err != nil {
		return 0, err
	}
	var metadata [16]byte
	if _, err := io.ReadFull(reader, metadata[:]); err != nil {
		return 0, err
	}
	if strings.HasPrefix(magic, "Neople Image File") {
		return binary.LittleEndian.Uint32(metadata[10:14]), nil
	}
	if strings.HasPrefix(magic, "Neople Img File") {
		return binary.LittleEndian.Uint32(metadata[12:16]), nil
	}
	return 0, fmt.Errorf("unsupported IMG magic %q", strings.TrimSuffix(magic, "\x00"))
}

func resolve(archive *platformpvf.Archive, listPath, reference string) string {
	reference = strings.TrimSpace(strings.Trim(reference, "`\"'"))
	for _, candidate := range []string{reference, path.Join(path.Dir(listPath), reference)} {
		if file, ok := archive.FindFile(candidate); ok {
			return file.ArchivePath
		}
	}
	return ""
}

func uniqueUnmatched(rows []unmatchedRef) []unmatchedRef {
	seen := make(map[string]struct{})
	result := make([]unmatchedRef, 0, len(rows))
	for _, row := range rows {
		key := row.Document + "\x00" + row.IMG + "\x00" + strconv.Itoa(row.Frame)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Document != result[j].Document {
			return result[i].Document < result[j].Document
		}
		if result[i].IMG != result[j].IMG {
			return result[i].IMG < result[j].IMG
		}
		return result[i].Frame < result[j].Frame
	})
	return result
}

func duplicateCount(owners map[string][]string) int {
	count := 0
	for _, names := range owners {
		if len(names) > 1 {
			count++
		}
	}
	return count
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
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
