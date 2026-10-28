// pvfgoldkaleidoearring extends legacy golden-kaleido special-equipment
// filters with the earring type introduced after those items were authored.
// The input archive is never modified in place.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const stackableListPath = "stackable/stackable.lst"

type goldKaleidoEntry struct {
	itemID     int64
	path       string
	name       string
	equipTypes []string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pvfgoldkaleidoearring:", err)
		os.Exit(1)
	}
}

func run() error {
	input := flag.String("pvf", "", "input PVF path (never modified)")
	output := flag.String("out", "", "output PVF path (must differ from input)")
	flag.Parse()
	if strings.TrimSpace(*input) == "" || strings.TrimSpace(*output) == "" {
		return fmt.Errorf("-pvf and -out are required")
	}
	inputAbs, err := filepath.Abs(*input)
	if err != nil {
		return err
	}
	outputAbs, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	if strings.EqualFold(inputAbs, outputAbs) {
		return fmt.Errorf("output must differ from input")
	}
	inputBytes, err := os.ReadFile(inputAbs)
	if err != nil {
		return err
	}
	archive, err := platformpvf.OpenBytes(inputBytes)
	if err != nil {
		return err
	}
	entries, err := loadGoldKaleidoEntries(archive)
	if err != nil {
		return err
	}
	var targets []goldKaleidoEntry
	for _, entry := range entries {
		if shouldAddEarring(entry.equipTypes) {
			targets = append(targets, entry)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no legacy golden-kaleido special-equipment filter requires an earring patch")
	}
	earringMagic, err := findEarringMagic(archive, entries)
	if err != nil {
		return err
	}
	replacements := make([]platformpvf.RawReplacement, 0, len(targets))
	for _, target := range targets {
		raw, readErr := archive.ReadRaw(target.path)
		if readErr != nil {
			return readErr
		}
		patched, patchErr := appendSectionStringToken(archive, raw, "[usable equip type]", "[/usable equip type]", "[earring]", earringMagic)
		if patchErr != nil {
			return fmt.Errorf("item=%d path=%s: %w", target.itemID, target.path, patchErr)
		}
		replacements = append(replacements, platformpvf.RawReplacement{Path: target.path, Data: patched})
	}
	outputBytes, err := archive.RepackRaw(replacements)
	if err != nil {
		return err
	}
	verified, err := platformpvf.OpenBytes(outputBytes)
	if err != nil {
		return fmt.Errorf("reopen output: %w", err)
	}
	verifiedEntries, err := loadGoldKaleidoEntries(verified)
	if err != nil {
		return err
	}
	for _, entry := range verifiedEntries {
		if hasType(entry.equipTypes, "[support]") || hasType(entry.equipTypes, "[magic stone]") {
			if !hasType(entry.equipTypes, "[earring]") {
				return fmt.Errorf("verification item=%d path=%s still omits [earring]", entry.itemID, entry.path)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outputAbs, outputBytes, 0o644); err != nil {
		return err
	}
	fmt.Printf("input_sha256=%X\noutput_sha256=%X\npatched=%d output=%s\n", sha256.Sum256(inputBytes), sha256.Sum256(outputBytes), len(targets), outputAbs)
	for _, target := range targets {
		fmt.Printf("item=%d name=%q path=%s added=[earring]\n", target.itemID, target.name, target.path)
	}
	return nil
}

func loadGoldKaleidoEntries(archive *platformpvf.Archive) ([]goldKaleidoEntry, error) {
	listText, err := archive.ReadText(stackableListPath)
	if err != nil {
		return nil, err
	}
	listDocument, err := dnfpvf.Parse(stackableListPath, listText)
	if err != nil {
		return nil, err
	}
	entries := make([]goldKaleidoEntry, 0, 32)
	for _, listed := range dnfpvf.ParseList(listDocument) {
		docPath := path.Clean(path.Join("stackable", strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(listed.Path), "\\", "/"), "/")))
		text, readErr := archive.ReadText(docPath)
		if readErr != nil {
			continue
		}
		document, parseErr := dnfpvf.Parse(docPath, text)
		if parseErr != nil {
			return nil, parseErr
		}
		stackableType, _ := document.Text("stackable type")
		actionType, _ := document.Text("action type")
		if !isGoldKaleido(stackableType, actionType) {
			continue
		}
		name, _ := document.Text("name")
		entries = append(entries, goldKaleidoEntry{
			itemID:     listed.ID,
			path:       docPath,
			name:       strings.TrimSpace(name),
			equipTypes: normalizedTypes(document.Texts("usable equip type")),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].itemID < entries[j].itemID })
	return entries, nil
}

func isGoldKaleido(stackableType, actionType string) bool {
	return normalizedType(stackableType) == "[gold kaleido]" || normalizedType(actionType) == "[gold kaleido box]"
}

func shouldAddEarring(types []string) bool {
	return (hasType(types, "[support]") || hasType(types, "[magic stone]")) && !hasType(types, "[earring]")
}

func normalizedTypes(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = normalizedType(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func normalizedType(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
}

func hasType(types []string, wanted string) bool {
	wanted = normalizedType(wanted)
	for _, value := range types {
		if normalizedType(value) == wanted {
			return true
		}
	}
	return false
}

func findEarringMagic(archive *platformpvf.Archive, entries []goldKaleidoEntry) (int32, error) {
	for _, entry := range entries {
		if !hasType(entry.equipTypes, "[earring]") {
			continue
		}
		raw, err := archive.ReadRaw(entry.path)
		if err != nil {
			return 0, err
		}
		for offset := 0; offset+5 <= len(raw); offset += 5 {
			if raw[offset] != 6 {
				continue
			}
			magic := int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+5]))
			if normalizedType(archive.ResolveString(int(magic))) == "[earring]" {
				return magic, nil
			}
		}
	}
	return 0, fmt.Errorf("shared string token for [earring] was not found in a proven golden-kaleido definition")
}

func appendSectionStringToken(archive *platformpvf.Archive, raw []byte, open, close, value string, magic int32) ([]byte, error) {
	if len(raw)%5 != 0 {
		return nil, fmt.Errorf("raw token bytes=%d are not aligned", len(raw))
	}
	inside := false
	closeOffset := -1
	for offset := 0; offset+5 <= len(raw); offset += 5 {
		kind := raw[offset]
		tokenMagic := int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+5]))
		if kind == 3 {
			resolved := archive.ResolveString(int(tokenMagic))
			switch resolved {
			case open:
				if inside || closeOffset >= 0 {
					return nil, fmt.Errorf("duplicate section %s", open)
				}
				inside = true
			case close:
				if inside {
					closeOffset = offset
					inside = false
				}
			}
			continue
		}
		if inside && kind == 6 && normalizedType(archive.ResolveString(int(tokenMagic))) == normalizedType(value) {
			return nil, fmt.Errorf("section already contains %s", value)
		}
	}
	if closeOffset < 0 {
		return nil, fmt.Errorf("section %s...%s not found", open, close)
	}
	var token [5]byte
	token[0] = 6
	binary.LittleEndian.PutUint32(token[1:], uint32(magic))
	patched := make([]byte, 0, len(raw)+len(token))
	patched = append(patched, raw[:closeOffset]...)
	patched = append(patched, token[:]...)
	patched = append(patched, raw[closeOffset:]...)
	return patched, nil
}
