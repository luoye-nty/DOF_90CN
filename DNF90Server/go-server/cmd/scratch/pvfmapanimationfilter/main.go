// Command pvfmapanimationfilter removes selected visual-only entries from a
// map's direct [animation] section while preserving every other map token.
// It can also remap individual animation filenames to an existing string in
// the same PVF. The input archive is never changed in place.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value is required")
	}
	*l = append(*l, value)
	return nil
}

type replacement struct {
	old string
	new string
}

type replacementList []replacement

func (l *replacementList) String() string {
	values := make([]string, 0, len(*l))
	for _, entry := range *l {
		values = append(values, entry.old+"="+entry.new)
	}
	return strings.Join(values, ",")
}

func (l *replacementList) Set(value string) error {
	oldValue, newValue, found := strings.Cut(value, "=")
	if !found {
		return fmt.Errorf("animation remap must use old=new")
	}
	oldValue = strings.TrimSpace(oldValue)
	newValue = strings.TrimSpace(newValue)
	if oldValue == "" || newValue == "" {
		return fmt.Errorf("animation remap old and new values are required")
	}
	*l = append(*l, replacement{old: oldValue, new: newValue})
	return nil
}

type filterResult struct {
	removed int
	mapped  int
}

func main() {
	pvfPath := flag.String("pvf", "", "base PVF")
	outputPath := flag.String("out", "", "output PVF")
	memberPath := flag.String("file", "", "existing map member path")
	var dropPatterns stringList
	var remaps replacementList
	flag.Var(&dropPatterns, "drop", "case-insensitive filename substring to remove from direct [animation] (repeatable)")
	flag.Var(&remaps, "map", "remap an animation filename to an existing PVF string: old=new (repeatable)")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" || strings.TrimSpace(*outputPath) == "" || strings.TrimSpace(*memberPath) == "" {
		panic("-pvf, -out and -file are required")
	}
	if len(dropPatterns) == 0 && len(remaps) == 0 {
		panic("at least one -drop or -map is required")
	}

	baseAbsolute := mustAbsolute(*pvfPath)
	outputAbsolute := mustAbsolute(*outputPath)
	if strings.EqualFold(baseAbsolute, outputAbsolute) {
		panic("output must differ from base PVF")
	}
	if _, err := os.Stat(outputAbsolute); err == nil {
		panic("output already exists: " + outputAbsolute)
	} else if !os.IsNotExist(err) {
		panic(err)
	}

	archive, err := platformpvf.Open(baseAbsolute)
	if err != nil {
		panic(err)
	}
	if len(remaps) != 0 {
		wanted := make([]string, 0, len(remaps))
		for _, remap := range remaps {
			wanted = append(wanted, remap.new)
		}
		existing := archive.FindStringMagicOffsets(wanted)
		appends := make([]platformpvf.StringAppend, 0, len(wanted)-len(existing))
		seen := make(map[string]struct{}, len(wanted))
		for _, value := range wanted {
			if _, duplicate := seen[value]; duplicate {
				continue
			}
			seen[value] = struct{}{}
			if _, found := existing[value]; !found {
				appends = append(appends, platformpvf.StringAppend{Value: value})
			}
		}
		if len(appends) != 0 {
			withStrings, appendErr := archive.RepackAppendStrings(appends)
			if appendErr != nil {
				panic(appendErr)
			}
			archive, err = platformpvf.OpenBytes(withStrings)
			if err != nil {
				panic(err)
			}
		}
	}
	memberPathValue := normalizePath(*memberPath)
	raw, err := archive.ReadRaw(memberPathValue)
	if err != nil {
		panic(err)
	}
	filtered, result, err := filterAnimations(archive, raw, dropPatterns, remaps)
	if err != nil {
		panic(err)
	}
	filtered, remappedOutsideDirectAnimation, err := remapRemainingStrings(archive, filtered, remaps)
	if err != nil {
		panic(err)
	}
	result.mapped += remappedOutsideDirectAnimation
	if result.removed == 0 && result.mapped == 0 {
		panic("no direct [animation] entry changed")
	}
	patched, err := archive.RepackRaw([]platformpvf.RawReplacement{{Path: memberPathValue, Data: filtered}})
	if err != nil {
		panic(err)
	}
	verified, err := platformpvf.OpenBytes(patched)
	if err != nil {
		panic(err)
	}
	text, err := verified.ReadText(memberPathValue)
	if err != nil {
		panic(err)
	}
	for _, remap := range remaps {
		if strings.Contains(strings.ToLower(text), strings.ToLower(remap.old)) {
			panic("remapped filename remains in verified map: " + remap.old)
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputAbsolute), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(outputAbsolute, patched, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("file=%s removed=%d remapped=%d raw_bytes=%d->%d output=%s\n", memberPathValue, result.removed, result.mapped, len(raw), len(filtered), outputAbsolute)
}

func filterAnimations(archive *platformpvf.Archive, raw []byte, dropPatterns []string, remaps []replacement) ([]byte, filterResult, error) {
	if len(raw)%5 != 0 {
		return nil, filterResult{}, fmt.Errorf("map raw token stream is not 5-byte aligned: %d", len(raw))
	}
	patterns := make([]string, 0, len(dropPatterns))
	for _, pattern := range dropPatterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	remapByOld := make(map[string]replacement, len(remaps))
	wantedNew := make([]string, 0, len(remaps))
	for _, entry := range remaps {
		key := strings.ToLower(strings.TrimSpace(entry.old))
		if _, found := remapByOld[key]; found {
			return nil, filterResult{}, fmt.Errorf("duplicate animation remap: %s", entry.old)
		}
		remapByOld[key] = entry
		wantedNew = append(wantedNew, entry.new)
	}
	stringMagics := archive.FindStringMagicOffsets(wantedNew)
	for _, entry := range remaps {
		if _, found := stringMagics[entry.new]; !found {
			return nil, filterResult{}, fmt.Errorf("replacement animation is absent from PVF string pool: %s", entry.new)
		}
	}

	result := filterResult{}
	output := make([]byte, 0, len(raw))
	for offset := 0; offset < len(raw); {
		token := raw[offset : offset+5]
		if tokenString(archive, token) != "[animation]" {
			output = append(output, token...)
			offset += 5
			continue
		}
		output = append(output, token...)
		offset += 5
		for {
			if offset >= len(raw) {
				return nil, filterResult{}, fmt.Errorf("unterminated [animation] section")
			}
			if tokenString(archive, raw[offset:offset+5]) == "[/animation]" {
				output = append(output, raw[offset:offset+5]...)
				offset += 5
				break
			}
			const animationEntryBytes = 5 * 5 // filename, layer, x, y, z
			if offset+animationEntryBytes > len(raw) {
				return nil, filterResult{}, fmt.Errorf("truncated direct animation entry")
			}
			entry := raw[offset : offset+animationEntryBytes]
			filenameToken := entry[:5]
			layerToken := entry[5:10]
			filename := tokenString(archive, filenameToken)
			if filename == "" || tokenString(archive, layerToken) == "" {
				return nil, filterResult{}, fmt.Errorf("direct animation entry is not filename/layer string tokens")
			}
			if remap, found := remapByOld[strings.ToLower(filename)]; found {
				patchedEntry := append([]byte(nil), entry...)
				binary.LittleEndian.PutUint32(patchedEntry[1:5], uint32(int32(stringMagics[remap.new])))
				output = append(output, patchedEntry...)
				result.mapped++
			} else if matchesAny(filename, patterns) {
				result.removed++
			} else {
				output = append(output, entry...)
			}
			offset += animationEntryBytes
		}
	}
	return output, result, nil
}

// remapRemainingStrings also covers a map's [background animation] metadata,
// whose [ani info]/[filename] structure is distinct from direct [animation].
func remapRemainingStrings(archive *platformpvf.Archive, raw []byte, remaps []replacement) ([]byte, int, error) {
	if len(raw)%5 != 0 {
		return nil, 0, fmt.Errorf("map raw token stream is not 5-byte aligned: %d", len(raw))
	}
	wanted := make([]string, 0, len(remaps))
	remapByOld := make(map[string]replacement, len(remaps))
	for _, remap := range remaps {
		remapByOld[strings.ToLower(remap.old)] = remap
		wanted = append(wanted, remap.new)
	}
	stringMagics := archive.FindStringMagicOffsets(wanted)
	output := append([]byte(nil), raw...)
	count := 0
	for offset := 0; offset < len(output); offset += 5 {
		token := output[offset : offset+5]
		remap, found := remapByOld[strings.ToLower(tokenString(archive, token))]
		if !found {
			continue
		}
		magic, found := stringMagics[remap.new]
		if !found {
			return nil, 0, fmt.Errorf("replacement animation is absent from PVF string pool: %s", remap.new)
		}
		binary.LittleEndian.PutUint32(token[1:5], uint32(int32(magic)))
		count++
	}
	return output, count, nil
}

func tokenString(archive *platformpvf.Archive, token []byte) string {
	if len(token) != 5 || (token[0] != 3 && token[0] != 6) {
		return ""
	}
	return archive.ResolveString(int(int32(binary.LittleEndian.Uint32(token[1:5]))))
}

func matchesAny(value string, patterns []string) bool {
	value = strings.ToLower(value)
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func normalizePath(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")), "/")
}

func mustAbsolute(value string) string {
	absolute, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil {
		panic(err)
	}
	return absolute
}
