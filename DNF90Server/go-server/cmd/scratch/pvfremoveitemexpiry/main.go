package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const scriptTokenSize = 5

var removedItemTimeSections = map[string]struct{}{
	"expiration date":               {},
	"usable datetime":               {},
	"usable period":                 {},
	"usable period after unsealing": {},
}

const unlimitedExpirationSection = "expiration info"

type patchCounts struct {
	Removed     map[string]int
	Transformed map[string]int
}

type report struct {
	InputPath             string         `json:"input_path"`
	OutputPath            string         `json:"output_path,omitempty"`
	InputSHA256           string         `json:"input_sha256"`
	OutputSHA256          string         `json:"output_sha256,omitempty"`
	ArchiveFiles          int            `json:"archive_files"`
	ItemDocumentsScanned  int            `json:"item_documents_scanned"`
	ItemDocumentsChanged  int            `json:"item_documents_changed"`
	SectionsRemoved       int            `json:"sections_removed"`
	SectionsTransformed   int            `json:"sections_transformed"`
	BytesRemoved          int            `json:"bytes_removed"`
	SectionsByName        map[string]int `json:"sections_by_name"`
	TransformsByName      map[string]int `json:"transforms_by_name"`
	TimeLikeSections      map[string]int `json:"time_like_sections"`
	FilesVerified         int            `json:"files_verified,omitempty"`
	UnmodifiedFilesStable int            `json:"unmodified_files_stable,omitempty"`
	DryRun                bool           `json:"dry_run"`
}

func main() {
	input := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "immutable source Script.pvf")
	output := flag.String("out", "", "new candidate PVF path; must not exist")
	dryRun := flag.Bool("dry-run", false, "audit item time-limit sections without writing")
	flag.Parse()

	result, err := run(*input, *output, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pvfremoveitemexpiry:", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pvfremoveitemexpiry: encode report:", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func run(inputPath, outputPath string, dryRun bool) (report, error) {
	var result report
	inputPath, err := filepath.Abs(filepath.Clean(strings.TrimSpace(inputPath)))
	if err != nil {
		return result, fmt.Errorf("resolve input path: %w", err)
	}
	if inputPath == "" {
		return result, errors.New("input PVF path is required")
	}
	if !dryRun {
		outputPath, err = filepath.Abs(filepath.Clean(strings.TrimSpace(outputPath)))
		if err != nil {
			return result, fmt.Errorf("resolve output path: %w", err)
		}
		if outputPath == "" || strings.EqualFold(inputPath, outputPath) {
			return result, errors.New("output PVF must be a distinct new path")
		}
		if _, statErr := os.Stat(outputPath); statErr == nil {
			return result, fmt.Errorf("output path already exists: %s", outputPath)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return result, fmt.Errorf("inspect output path: %w", statErr)
		}
		if parentInfo, statErr := os.Stat(filepath.Dir(outputPath)); statErr != nil || !parentInfo.IsDir() {
			return result, fmt.Errorf("output directory is unavailable: %s", filepath.Dir(outputPath))
		}
	}

	inputHash, err := sha256File(inputPath)
	if err != nil {
		return result, err
	}
	archive, err := platformpvf.Open(inputPath)
	if err != nil {
		return result, fmt.Errorf("open source PVF: %w", err)
	}
	files := archive.Files()
	result = report{
		InputPath:        inputPath,
		OutputPath:       outputPath,
		InputSHA256:      inputHash,
		ArchiveFiles:     len(files),
		SectionsByName:   make(map[string]int, len(removedItemTimeSections)),
		TransformsByName: make(map[string]int, 1),
		TimeLikeSections: make(map[string]int),
		DryRun:           dryRun,
	}
	unlimitedMagic, found := archive.FindStringMagicOffsets([]string{"[unlimit]"})["[unlimit]"]
	if !found {
		return result, errors.New("source PVF string pool has no [unlimit] value")
	}

	replacements := make([]platformpvf.RawReplacement, 0, 4096)
	expectedByPath := make(map[string][]byte)
	for _, file := range files {
		if !isItemDefinition(file) {
			continue
		}
		result.ItemDocumentsScanned++
		raw, readErr := archive.FileRaw(file.Index)
		if readErr != nil {
			return result, fmt.Errorf("read item document %s: %w", file.ArchivePath, readErr)
		}
		for section, count := range itemSectionCounts(raw, archive.ResolveString) {
			if isTimeLikeItemSection(section) {
				result.TimeLikeSections[section] += count
			}
		}
		patched, counts, removeErr := rewriteItemTimeLimitSections(raw, archive.ResolveString, unlimitedMagic)
		if removeErr != nil {
			return result, fmt.Errorf("remove item time limits from %s: %w", file.ArchivePath, removeErr)
		}
		removed := 0
		for section, count := range counts.Removed {
			result.SectionsByName[section] += count
			removed += count
		}
		transformed := 0
		for section, count := range counts.Transformed {
			result.TransformsByName[section] += count
			transformed += count
		}
		if removed+transformed == 0 {
			continue
		}
		result.ItemDocumentsChanged++
		result.SectionsRemoved += removed
		result.SectionsTransformed += transformed
		result.BytesRemoved += len(raw) - len(patched)
		pathKey := strings.ToLower(filepath.ToSlash(file.ArchivePath))
		expectedByPath[pathKey] = patched
		replacements = append(replacements, platformpvf.RawReplacement{Path: file.ArchivePath, Data: patched})
	}
	if dryRun {
		return result, nil
	}
	if len(replacements) == 0 {
		return result, errors.New("source PVF contains no item expiration sections")
	}

	currentHash, err := sha256File(inputPath)
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(currentHash, inputHash) {
		return result, errors.New("source PVF changed after audit; refusing stale repack")
	}
	repacked, err := archive.RepackRaw(replacements)
	if err != nil {
		return result, fmt.Errorf("repack item expiration removals: %w", err)
	}
	verified, err := platformpvf.OpenBytes(repacked)
	if err != nil {
		return result, fmt.Errorf("reopen candidate PVF: %w", err)
	}
	candidateUnlimitedMagic, found := verified.FindStringMagicOffsets([]string{"[unlimit]"})["[unlimit]"]
	if !found {
		return result, errors.New("candidate PVF string pool has no [unlimit] value")
	}
	verifiedFiles := verified.Files()
	if len(verifiedFiles) != len(files) {
		return result, fmt.Errorf("candidate file count=%d want=%d", len(verifiedFiles), len(files))
	}
	for index, originalFile := range files {
		candidateFile := verifiedFiles[index]
		pathKey := strings.ToLower(filepath.ToSlash(originalFile.ArchivePath))
		expected, changed := expectedByPath[pathKey]
		expectedMetadata := originalFile
		if changed {
			expectedMetadata.Size = len(expected)
		}
		if expectedMetadata != candidateFile {
			return result, fmt.Errorf("candidate metadata changed outside planned size at file index %d: want=%+v after=%+v", index, expectedMetadata, candidateFile)
		}
		candidateRaw, readErr := verified.FileRaw(candidateFile.Index)
		if readErr != nil {
			return result, fmt.Errorf("read candidate file %s: %w", candidateFile.ArchivePath, readErr)
		}
		if changed {
			if !bytes.Equal(candidateRaw, expected) {
				return result, fmt.Errorf("candidate changed item %s does not match planned token deletion", originalFile.ArchivePath)
			}
			if _, counts, removeErr := rewriteItemTimeLimitSections(candidateRaw, verified.ResolveString, candidateUnlimitedMagic); removeErr != nil {
				return result, fmt.Errorf("verify candidate item %s: %w", originalFile.ArchivePath, removeErr)
			} else if countPatchSections(counts) != 0 {
				return result, fmt.Errorf("candidate item %s still contains time-limit sections", originalFile.ArchivePath)
			}
		} else {
			originalRaw, readErr := archive.FileRaw(originalFile.Index)
			if readErr != nil {
				return result, fmt.Errorf("reread source file %s: %w", originalFile.ArchivePath, readErr)
			}
			if !bytes.Equal(candidateRaw, originalRaw) {
				return result, fmt.Errorf("unmodified file changed: %s", originalFile.ArchivePath)
			}
			result.UnmodifiedFilesStable++
		}
		result.FilesVerified++
	}

	sum := sha256.Sum256(repacked)
	result.OutputSHA256 = strings.ToUpper(hex.EncodeToString(sum[:]))
	if err := writeNewFile(outputPath, repacked); err != nil {
		return result, err
	}
	diskHash, err := sha256File(outputPath)
	if err != nil {
		_ = os.Remove(outputPath)
		return result, err
	}
	if !strings.EqualFold(diskHash, result.OutputSHA256) {
		_ = os.Remove(outputPath)
		return result, errors.New("candidate PVF hash changed while writing")
	}
	return result, nil
}

func itemSectionCounts(raw []byte, resolve func(int) string) map[string]int {
	counts := make(map[string]int)
	if len(raw)%scriptTokenSize != 0 {
		return counts
	}
	for offset := 0; offset < len(raw); offset += scriptTokenSize {
		if raw[offset] != 3 {
			continue
		}
		value := int(int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+scriptTokenSize])))
		if section := normalizeSectionName(resolve(value)); section != "" {
			counts[section]++
		}
	}
	return counts
}

func isTimeLikeItemSection(section string) bool {
	for _, fragment := range []string{"expir", "usable period", "use period", "duration", "deadline", "date"} {
		if strings.Contains(section, fragment) {
			return true
		}
	}
	return false
}

func isItemDefinition(file platformpvf.File) bool {
	if file.DataType != 1 {
		return false
	}
	archivePath := strings.ToLower(filepath.ToSlash(strings.TrimSpace(file.ArchivePath)))
	if !strings.HasPrefix(archivePath, "stackable/") && !strings.HasPrefix(archivePath, "equipment/") {
		return false
	}
	extension := strings.ToLower(filepath.Ext(archivePath))
	return extension == ".stk" || extension == ".equ"
}

func removeItemTimeLimitSections(raw []byte, resolve func(int) string) ([]byte, map[string]int, error) {
	patched, counts, err := rewriteItemTimeLimitSections(raw, resolve, -1)
	return patched, counts.Removed, err
}

func rewriteItemTimeLimitSections(raw []byte, resolve func(int) string, unlimitedMagic int) ([]byte, patchCounts, error) {
	if len(raw)%scriptTokenSize != 0 {
		return nil, patchCounts{}, fmt.Errorf("type-1 token bytes=%d are not divisible by %d", len(raw), scriptTokenSize)
	}
	counts := patchCounts{
		Removed:     make(map[string]int, len(removedItemTimeSections)),
		Transformed: make(map[string]int, 1),
	}
	patched := make([]byte, 0, len(raw))
	for token := 0; token < len(raw)/scriptTokenSize; {
		offset := token * scriptTokenSize
		if raw[offset] != 3 {
			patched = append(patched, raw[offset:offset+scriptTokenSize]...)
			token++
			continue
		}
		value := int(int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+scriptTokenSize])))
		section := normalizeSectionName(resolve(value))
		if _, remove := removedItemTimeSections[section]; remove {
			counts.Removed[section]++
			token++
			for token < len(raw)/scriptTokenSize && raw[token*scriptTokenSize] != 3 {
				token++
			}
			continue
		}
		if section == unlimitedExpirationSection && unlimitedMagic >= 0 {
			valueStart := token + 1
			valueEnd := valueStart
			for valueEnd < len(raw)/scriptTokenSize && raw[valueEnd*scriptTokenSize] != 3 {
				valueEnd++
			}
			if valueEnd == valueStart+1 && raw[valueStart*scriptTokenSize] == 6 {
				value := int(int32(binary.LittleEndian.Uint32(raw[valueStart*scriptTokenSize+1 : valueStart*scriptTokenSize+scriptTokenSize])))
				if strings.EqualFold(strings.TrimSpace(resolve(value)), "[unlimit]") {
					patched = append(patched, raw[offset:valueEnd*scriptTokenSize]...)
					token = valueEnd
					continue
				}
			}
			patched = append(patched, raw[offset:offset+scriptTokenSize]...)
			unlimited := make([]byte, scriptTokenSize)
			unlimited[0] = 6
			binary.LittleEndian.PutUint32(unlimited[1:], uint32(int32(unlimitedMagic)))
			patched = append(patched, unlimited...)
			counts.Transformed[section]++
			token = valueEnd
			continue
		}
		patched = append(patched, raw[offset:offset+scriptTokenSize]...)
		token++
	}
	if countPatchSections(counts) == 0 {
		return append([]byte(nil), raw...), counts, nil
	}
	return patched, counts, nil
}

func normalizeSectionName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) >= 2 && value[0] == '[' && value[len(value)-1] == ']' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func countSections(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func countPatchSections(counts patchCounts) int {
	return countSections(counts.Removed) + countSections(counts.Transformed)
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for hashing: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return strings.ToUpper(hex.EncodeToString(hash.Sum(nil))), nil
}

func writeNewFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create candidate PVF: %w", err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write candidate PVF: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush candidate PVF: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close candidate PVF: %w", err)
	}
	ok = true
	return nil
}
