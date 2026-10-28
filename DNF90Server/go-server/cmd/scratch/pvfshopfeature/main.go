// pvfshopfeature appends one real stackable item to the Cera Shop [premium]
// (client 功能类) section. The input archive is never modified in place.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	ceraShopPath      = "etc/cerashop.etc"
	stackableListPath = "stackable/stackable.lst"
	premiumRowWidth   = 9
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pvfshopfeature:", err)
		os.Exit(1)
	}
}

func run() error {
	input := flag.String("pvf", "", "input PVF path (never modified)")
	output := flag.String("out", "", "output PVF path (must differ from -pvf)")
	itemID := flag.Uint("item", 10164630, "stackable item id")
	price := flag.Uint("price", 1, "Cera price")
	commodityStart := flag.Uint("commodity", 292739, "first preferred commodity id")
	flag.Parse()
	if strings.TrimSpace(*input) == "" || strings.TrimSpace(*output) == "" {
		return fmt.Errorf("-pvf and -out are required")
	}
	if *itemID == 0 || *itemID > math.MaxUint32 || *price > math.MaxInt32 || *commodityStart == 0 || *commodityStart > math.MaxInt32 {
		return fmt.Errorf("invalid item=%d price=%d commodity=%d", *itemID, *price, *commodityStart)
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
	itemPath, err := resolveStackablePath(archive, uint32(*itemID))
	if err != nil {
		return err
	}
	nameMagic, err := sectionStringMagic(archive, itemPath, "[name]")
	if err != nil {
		return err
	}
	itemText, err := archive.ReadText(itemPath)
	if err != nil {
		return err
	}
	itemDocument, err := dnfpvf.Parse(itemPath, itemText)
	if err != nil {
		return err
	}
	name, found := itemDocument.Text("name")
	if !found || strings.TrimSpace(name) == "" {
		return fmt.Errorf("item %d path=%s has no name", *itemID, itemPath)
	}

	raw, err := archive.ReadRaw(ceraShopPath)
	if err != nil {
		return err
	}
	start, end, err := findRawSection(archive, raw, "[premium]", "[/premium]")
	if err != nil {
		return err
	}
	interior := raw[(start+1)*5 : end*5]
	if len(interior)%(premiumRowWidth*5) != 0 {
		return fmt.Errorf("premium section bytes=%d are not %d-token rows", len(interior), premiumRowWidth)
	}
	for offset := 0; offset < len(interior); offset += premiumRowWidth * 5 {
		existingItem, ok := intToken(interior[offset+5 : offset+10])
		if ok && existingItem == int32(*itemID) {
			return fmt.Errorf("item %d is already present in [premium]", *itemID)
		}
	}
	used := allPositiveIntegers(raw)
	commodity := uint32(*commodityStart)
	for {
		if commodity == 0 || commodity > math.MaxInt32 {
			return fmt.Errorf("commodity id space exhausted")
		}
		if _, collision := used[commodity]; !collision {
			break
		}
		commodity++
	}
	row := make([]byte, 0, premiumRowWidth*5)
	row = appendInt(row, int32(commodity))
	row = appendInt(row, int32(*itemID))
	row = appendInt(row, 1)
	row = appendInt(row, 0)
	row = appendInt(row, 0)
	row = appendInt(row, int32(*price))
	row = appendToken(row, 6, nameMagic)
	row = appendInt(row, 0)
	row = appendInt(row, 0)
	patchedRaw := make([]byte, 0, len(raw)+len(row))
	patchedRaw = append(patchedRaw, raw[:end*5]...)
	patchedRaw = append(patchedRaw, row...)
	patchedRaw = append(patchedRaw, raw[end*5:]...)
	outputBytes, err := archive.RepackRaw([]platformpvf.RawReplacement{{Path: ceraShopPath, Data: patchedRaw}})
	if err != nil {
		return err
	}
	verified, err := platformpvf.OpenBytes(outputBytes)
	if err != nil {
		return fmt.Errorf("reopen output: %w", err)
	}
	if err := verifyPremiumRow(verified, commodity, uint32(*itemID), uint32(*price), name); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outputAbs, outputBytes, 0o644); err != nil {
		return err
	}
	fmt.Printf("input_sha256=%X\noutput_sha256=%X\ncommodity=%d item=%d count=1 cera=%d name=%q path=%s output=%s\n", sha256.Sum256(inputBytes), sha256.Sum256(outputBytes), commodity, *itemID, *price, name, itemPath, outputAbs)
	return nil
}

func resolveStackablePath(archive *platformpvf.Archive, target uint32) (string, error) {
	text, err := archive.ReadText(stackableListPath)
	if err != nil {
		return "", err
	}
	document, err := dnfpvf.Parse(stackableListPath, text)
	if err != nil {
		return "", err
	}
	for _, entry := range dnfpvf.ParseList(document) {
		if entry.ID != int64(target) {
			continue
		}
		clean := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(entry.Path), "\\", "/"), "/")
		return path.Clean(path.Join("stackable", clean)), nil
	}
	return "", fmt.Errorf("item %d is absent from stackable.lst", target)
}

func verifyPremiumRow(archive *platformpvf.Archive, commodity, itemID, price uint32, name string) error {
	text, err := archive.ReadText(ceraShopPath)
	if err != nil {
		return err
	}
	document, err := dnfpvf.Parse(ceraShopPath, text)
	if err != nil {
		return err
	}
	tokens, found := document.Section("premium")
	if !found {
		return fmt.Errorf("verified premium section missing")
	}
	values := make([]dnfpvf.Token, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenSection {
			values = append(values, token)
		}
	}
	if len(values)%premiumRowWidth != 0 {
		return fmt.Errorf("verified premium row grammar is malformed")
	}
	for offset := 0; offset < len(values); offset += premiumRowWidth {
		if values[offset].Int == int64(commodity) && values[offset+1].Int == int64(itemID) &&
			values[offset+2].Int == 1 && values[offset+5].Int == int64(price) && strings.TrimSpace(values[offset+6].Value) == strings.TrimSpace(name) {
			return nil
		}
	}
	return fmt.Errorf("verified premium row commodity=%d item=%d not found", commodity, itemID)
}

func findRawSection(archive *platformpvf.Archive, raw []byte, open, close string) (int, int, error) {
	start := -1
	for index, offset := 0, 0; offset+5 <= len(raw); index, offset = index+1, offset+5 {
		if raw[offset] != 3 {
			continue
		}
		value := archive.ResolveString(int(int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+5]))))
		if value == open {
			if start >= 0 {
				return 0, 0, fmt.Errorf("duplicate section %s", open)
			}
			start = index
			continue
		}
		if start >= 0 && value == close {
			return start, index, nil
		}
	}
	return 0, 0, fmt.Errorf("section %s...%s not found", open, close)
}

func sectionStringMagic(archive *platformpvf.Archive, itemPath, section string) (int32, error) {
	raw, err := archive.ReadRaw(itemPath)
	if err != nil {
		return 0, err
	}
	found := false
	for offset := 0; offset+5 <= len(raw); offset += 5 {
		tokenType := raw[offset]
		magic := int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+5]))
		if tokenType == 3 {
			if found {
				return 0, fmt.Errorf("path=%s section=%s has no string token", itemPath, section)
			}
			found = archive.ResolveString(int(magic)) == section
			continue
		}
		if found && tokenType == 6 {
			return magic, nil
		}
	}
	return 0, fmt.Errorf("path=%s section=%s not found", itemPath, section)
}

func appendInt(out []byte, value int32) []byte { return appendToken(out, 0, value) }

func appendToken(out []byte, tokenType byte, value int32) []byte {
	var token [5]byte
	token[0] = tokenType
	binary.LittleEndian.PutUint32(token[1:], uint32(value))
	return append(out, token[:]...)
}

func intToken(token []byte) (int32, bool) {
	if len(token) != 5 || token[0] != 0 {
		return 0, false
	}
	return int32(binary.LittleEndian.Uint32(token[1:])), true
}

func allPositiveIntegers(raw []byte) map[uint32]struct{} {
	out := make(map[uint32]struct{})
	for offset := 0; offset+5 <= len(raw); offset += 5 {
		value, ok := intToken(raw[offset : offset+5])
		if ok && value > 0 {
			out[uint32(value)] = struct{}{}
		}
	}
	return out
}
