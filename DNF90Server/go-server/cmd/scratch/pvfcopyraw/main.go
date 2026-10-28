// Command pvfcopyraw copies an explicit allowlist of existing raw members
// from a donor PVF into a base PVF. It never edits either input in place.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	basePath := flag.String("base", "", "base PVF")
	donorPath := flag.String("donor", "", "donor PVF")
	outputPath := flag.String("out", "", "output PVF")
	files := flag.String("files", "", "comma-separated existing archive members")
	remapType1 := flag.Bool("remap-type1", false, "remap type-1 string tokens by value into the base PVF string pool")
	flag.Parse()

	if strings.TrimSpace(*basePath) == "" || strings.TrimSpace(*donorPath) == "" ||
		strings.TrimSpace(*outputPath) == "" || strings.TrimSpace(*files) == "" {
		panic("-base, -donor, -out and -files are required")
	}
	baseAbsolute := mustAbsolute(*basePath)
	donorAbsolute := mustAbsolute(*donorPath)
	outputAbsolute := mustAbsolute(*outputPath)
	if strings.EqualFold(baseAbsolute, outputAbsolute) || strings.EqualFold(donorAbsolute, outputAbsolute) {
		panic("output must differ from both inputs")
	}

	base, err := platformpvf.Open(baseAbsolute)
	if err != nil {
		panic(err)
	}
	donor, err := platformpvf.Open(donorAbsolute)
	if err != nil {
		panic(err)
	}
	paths := parsePaths(*files)
	type pendingReplacement struct {
		path string
		raw  []byte
	}
	pending := make([]pendingReplacement, 0, len(paths))
	wantedStrings := make([]string, 0)
	wantedWide := make(map[string]bool)
	for _, memberPath := range paths {
		if _, found := base.FindFile(memberPath); !found {
			panic(fmt.Sprintf("base member does not exist: %s", memberPath))
		}
		donorRaw, readErr := donor.ReadRaw(memberPath)
		if readErr != nil {
			panic(readErr)
		}
		baseRaw, readErr := base.ReadRaw(memberPath)
		if readErr != nil {
			panic(readErr)
		}
		if bytes.Equal(baseRaw, donorRaw) {
			panic(fmt.Sprintf("member is already identical: %s", memberPath))
		}
		pending = append(pending, pendingReplacement{path: memberPath, raw: donorRaw})
		if *remapType1 {
			if len(donorRaw)%5 != 0 {
				panic(fmt.Sprintf("type-1 member length is not token-aligned: %s bytes=%d", memberPath, len(donorRaw)))
			}
			for offset := 0; offset < len(donorRaw); offset += 5 {
				if donorRaw[offset] != 3 && donorRaw[offset] != 6 {
					continue
				}
				magic := int(int32(binary.LittleEndian.Uint32(donorRaw[offset+1 : offset+5])))
				value := donor.ResolveString(magic)
				wantedStrings = append(wantedStrings, value)
				wantedWide[value] = wantedWide[value] || magic&1 != 0
			}
		}
	}
	stringOffsets := base.FindStringMagicOffsets(wantedStrings)
	if *remapType1 && len(stringOffsets) < len(wantedWide) {
		appends := make([]platformpvf.StringAppend, 0, len(wantedWide)-len(stringOffsets))
		for value, wide := range wantedWide {
			if _, found := stringOffsets[value]; found || value == "" {
				continue
			}
			appends = append(appends, platformpvf.StringAppend{Value: value, Wide: wide})
		}
		if len(appends) > 0 {
			withStrings, appendErr := base.RepackAppendStrings(appends)
			if appendErr != nil {
				panic(appendErr)
			}
			base, err = platformpvf.OpenBytes(withStrings)
			if err != nil {
				panic(err)
			}
			stringOffsets = base.FindStringMagicOffsets(wantedStrings)
		}
	}
	replacements := make([]platformpvf.RawReplacement, 0, len(pending))
	for _, replacement := range pending {
		raw := append([]byte(nil), replacement.raw...)
		if *remapType1 {
			for offset := 0; offset < len(raw); offset += 5 {
				if raw[offset] != 3 && raw[offset] != 6 {
					continue
				}
				donorMagic := int(int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+5])))
				value := donor.ResolveString(donorMagic)
				baseMagic, found := stringOffsets[value]
				if !found {
					panic(fmt.Sprintf("base string pool is missing %q (donor_magic=%d wide=%t) required by %s", value, donorMagic, donorMagic&1 != 0, replacement.path))
				}
				binary.LittleEndian.PutUint32(raw[offset+1:offset+5], uint32(int32(baseMagic)))
			}
		}
		replacements = append(replacements, platformpvf.RawReplacement{Path: replacement.path, Data: raw})
	}

	output, err := base.RepackRaw(replacements)
	if err != nil {
		panic(err)
	}
	verified, err := platformpvf.OpenBytes(output)
	if err != nil {
		panic(err)
	}
	for _, replacement := range replacements {
		got, readErr := verified.ReadRaw(replacement.Path)
		if readErr != nil {
			panic(readErr)
		}
		if !bytes.Equal(got, replacement.Data) {
			panic(fmt.Sprintf("verification mismatch: %s", replacement.Path))
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputAbsolute), 0o755); err != nil {
		panic(err)
	}
	if _, err := os.Stat(outputAbsolute); err == nil {
		panic(fmt.Sprintf("output already exists: %s", outputAbsolute))
	} else if !os.IsNotExist(err) {
		panic(err)
	}
	if err := os.WriteFile(outputAbsolute, output, 0o644); err != nil {
		panic(err)
	}
	hash := sha256.Sum256(output)
	fmt.Printf("copied=%d size=%d sha256=%X output=%s\n", len(replacements), len(output), hash, outputAbsolute)
	for _, replacement := range replacements {
		fmt.Printf("COPIED %s bytes=%d\n", replacement.Path, len(replacement.Data))
	}
}

func mustAbsolute(value string) string {
	absolute, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil {
		panic(err)
	}
	return absolute
}

func parsePaths(value string) []string {
	paths := make([]string, 0)
	seen := make(map[string]struct{})
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(strings.ReplaceAll(candidate, "\\", "/"))
		candidate = strings.TrimPrefix(candidate, "/")
		if candidate == "" {
			continue
		}
		key := strings.ToLower(candidate)
		if _, duplicate := seen[key]; duplicate {
			panic(fmt.Sprintf("duplicate member: %s", candidate))
		}
		seen[key] = struct{}{}
		paths = append(paths, candidate)
	}
	if len(paths) == 0 {
		panic("no archive members supplied")
	}
	return paths
}
