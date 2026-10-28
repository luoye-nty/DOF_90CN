// Command pvfaria removes the duplicate Aelia NPC 20462 from Hendon Myre
// while preserving the existing Aelia NPC 20449 and every unrelated PVF file.
// It always writes a separate candidate and never edits the active PVF.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	npcListPath      = "npc/npc.lst"
	itemShopListPath = "itemshop/itemshop.lst"
	sourceShopPath   = "itemshop/chn_masang_shop_2017.shp"
	targetNPCPath    = "npc/chn_event_lions10th_2.npc"
	townMapPath      = "map/cataclysm/town/hendonmyre/new_hendon_main.map"
)

type report struct {
	InputPath       string `json:"input_path"`
	OutputPath      string `json:"output_path"`
	RemovedNPCID    int    `json:"removed_npc_id"`
	PreservedNPCID  int    `json:"preserved_npc_id"`
	PreservedX      int    `json:"preserved_x"`
	PreservedY      int    `json:"preserved_y"`
	RemovedShopID   int    `json:"removed_shop_id"`
	SourceShopOwner int    `json:"source_shop_owner"`
	FileCount       int    `json:"file_count"`
	InputSize       int64  `json:"input_size"`
	OutputSize      int    `json:"output_size"`
	OutputSHA256    string `json:"output_sha256"`
}

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "source Script.pvf")
	outPath := flag.String("out", `D:/DNF/runtime/tmp/aria-npc-remove/Script-no-duplicate-aelia.pvf`, "candidate output path")
	flag.Parse()

	inputAbs, err := filepath.Abs(*pvfPath)
	must(err)
	outputAbs, err := filepath.Abs(*outPath)
	must(err)
	if strings.EqualFold(inputAbs, outputAbs) {
		panic("output must differ from input")
	}

	archive, err := platformpvf.Open(inputAbs)
	must(err)
	fileCount := len(archive.Files())
	preservedTargetNPC, err := archive.ReadRaw(targetNPCPath)
	must(err)

	npcListRaw, err := archive.ReadRaw(npcListPath)
	must(err)
	npcListPatched, err := removeListEntry(archive, npcListRaw, 20462, "chn_masang_2017.npc")
	must(err)

	itemShopListRaw, err := archive.ReadRaw(itemShopListPath)
	must(err)
	itemShopListPatched, err := removeListEntry(archive, itemShopListRaw, 101262, "chn_masang_shop_2017.shp")
	must(err)

	sourceShopRaw, err := archive.ReadRaw(sourceShopPath)
	must(err)
	sourceShopPatched, err := replaceSectionIntValue(archive, sourceShopRaw, "npc", 20462, 20325)
	must(err)

	townMapRaw, err := archive.ReadRaw(townMapPath)
	must(err)
	townMapPatched, err := removeTownNPCPlacement(archive, townMapRaw, 20462, "left", 2088, 156, 0)
	must(err)

	patched, err := archive.RepackRaw([]platformpvf.RawReplacement{
		{Path: npcListPath, Data: npcListPatched},
		{Path: itemShopListPath, Data: itemShopListPatched},
		{Path: sourceShopPath, Data: sourceShopPatched},
		{Path: townMapPath, Data: townMapPatched},
	})
	must(err)

	verified, err := platformpvf.OpenBytes(patched)
	must(err)
	if len(verified.Files()) != fileCount {
		panic(fmt.Errorf("file count changed: got=%d want=%d", len(verified.Files()), fileCount))
	}
	must(requireListEntryAbsent(verified, npcListPath, 20462))
	must(requireListEntry(verified, npcListPath, 20449, "chn_event_lions10th_2.npc"))
	must(requireListEntryAbsent(verified, itemShopListPath, 101262))
	must(requireListEntry(verified, itemShopListPath, 400000022, "chn_10th_lions_2.shp"))
	must(requireSectionIntValue(verified, sourceShopPath, "npc", 20325))
	must(requireTownNPCPlacementAbsent(verified, townMapPath, 20462))
	must(requireTownNPCPlacement(verified, townMapPath, 20449, "left", 1478, 354, 0))
	must(requireTownNPCPlacement(verified, townMapPath, 159, "left", 1896, 156, 0))
	must(requireTownNPCPlacement(verified, townMapPath, 45, "right", 2470, 156, 0))
	verifiedTargetNPC, err := verified.ReadRaw(targetNPCPath)
	must(err)
	if string(verifiedTargetNPC) != string(preservedTargetNPC) {
		panic(fmt.Errorf("preserved Aelia NPC %d content changed", 20449))
	}
	verifiedTargetText, err := verified.ReadText(targetNPCPath)
	must(err)
	for _, required := range []string{"[name]\n`艾莉亚`", "`[joust event]` 0", "`[item shop]` 400000022"} {
		if !strings.Contains(verifiedTargetText, required) {
			panic(fmt.Errorf("preserved Aelia NPC is missing %q", required))
		}
	}

	must(os.MkdirAll(filepath.Dir(outputAbs), 0o755))
	must(os.WriteFile(outputAbs, patched, 0o644))
	inputInfo, err := os.Stat(inputAbs)
	must(err)
	sum := sha256.Sum256(patched)
	result := report{
		InputPath:       inputAbs,
		OutputPath:      outputAbs,
		RemovedNPCID:    20462,
		PreservedNPCID:  20449,
		PreservedX:      1478,
		PreservedY:      354,
		RemovedShopID:   101262,
		SourceShopOwner: 20325,
		FileCount:       fileCount,
		InputSize:       inputInfo.Size(),
		OutputSize:      len(patched),
		OutputSHA256:    strings.ToUpper(hex.EncodeToString(sum[:])),
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	must(err)
	fmt.Println(string(encoded))
}

func removeListEntry(archive *platformpvf.Archive, raw []byte, id int, wantPath string) ([]byte, error) {
	if archive == nil || len(raw)%5 != 0 {
		return nil, fmt.Errorf("list token stream is invalid: bytes=%d", len(raw))
	}
	out := make([]byte, 0, len(raw)-10)
	matches := 0
	for offset := 0; offset < len(raw); {
		if offset+10 <= len(raw) && raw[offset] == 0 && scriptTokenInt(raw[offset:offset+5]) == id {
			pathToken := raw[offset+5 : offset+10]
			if pathToken[0] != 6 {
				return nil, fmt.Errorf("list id %d is not followed by a string path", id)
			}
			gotPath := archive.ResolveString(scriptTokenInt(pathToken))
			if !strings.EqualFold(gotPath, wantPath) {
				return nil, fmt.Errorf("list id %d path=%q, want=%q", id, gotPath, wantPath)
			}
			matches++
			offset += 10
			continue
		}
		out = append(out, raw[offset:offset+5]...)
		offset += 5
	}
	if matches != 1 {
		return nil, fmt.Errorf("list id %d matches=%d, want=1", id, matches)
	}
	return out, nil
}

func removeTownNPCPlacement(archive *platformpvf.Archive, raw []byte, npcID int, direction string, x, y, z int) ([]byte, error) {
	if archive == nil || len(raw)%5 != 0 {
		return nil, fmt.Errorf("map token stream is invalid: bytes=%d", len(raw))
	}
	out := make([]byte, 0, len(raw)-25)
	inNPCSection := false
	matches := 0
	for offset := 0; offset < len(raw); {
		token := raw[offset : offset+5]
		if token[0] == 3 {
			inNPCSection = normalizedLabel(archive.ResolveString(scriptTokenInt(token))) == "npc"
		}
		if inNPCSection && offset+25 <= len(raw) && token[0] == 0 && scriptTokenInt(token) == npcID {
			directionToken := raw[offset+5 : offset+10]
			xToken := raw[offset+10 : offset+15]
			yToken := raw[offset+15 : offset+20]
			zToken := raw[offset+20 : offset+25]
			if directionToken[0] != 6 || normalizedLabel(archive.ResolveString(scriptTokenInt(directionToken))) != normalizedLabel(direction) ||
				xToken[0] != 0 || scriptTokenInt(xToken) != x || yToken[0] != 0 || scriptTokenInt(yToken) != y ||
				zToken[0] != 0 || scriptTokenInt(zToken) != z {
				return nil, fmt.Errorf("town NPC %d tuple differs from the expected duplicate placement", npcID)
			}
			matches++
			offset += 25
			continue
		}
		out = append(out, token...)
		offset += 5
	}
	if matches != 1 {
		return nil, fmt.Errorf("town NPC %d placement matches=%d, want=1", npcID, matches)
	}
	return out, nil
}

func replaceSectionIntValue(archive *platformpvf.Archive, raw []byte, section string, oldValue, newValue int) ([]byte, error) {
	if archive == nil || len(raw)%5 != 0 {
		return nil, fmt.Errorf("script token stream is invalid: bytes=%d", len(raw))
	}
	out := append([]byte(nil), raw...)
	matches := 0
	for offset := 0; offset+10 <= len(out); offset += 5 {
		token := out[offset : offset+5]
		if (token[0] != 3 && token[0] != 6) || normalizedLabel(archive.ResolveString(scriptTokenInt(token))) != normalizedLabel(section) {
			continue
		}
		valueToken := out[offset+5 : offset+10]
		if valueToken[0] != 0 || scriptTokenInt(valueToken) != oldValue {
			return nil, fmt.Errorf("section [%s] value does not match %d", section, oldValue)
		}
		binary.LittleEndian.PutUint32(valueToken[1:5], uint32(int32(newValue)))
		matches++
	}
	if matches != 1 {
		return nil, fmt.Errorf("section [%s] matches=%d, want=1", section, matches)
	}
	return out, nil
}

func requireListEntry(archive *platformpvf.Archive, path string, id int, wantPath string) error {
	raw, err := archive.ReadRaw(path)
	if err != nil {
		return err
	}
	matches := 0
	for offset := 0; offset+10 <= len(raw); offset += 5 {
		if raw[offset] != 0 || scriptTokenInt(raw[offset:offset+5]) != id {
			continue
		}
		pathToken := raw[offset+5 : offset+10]
		if pathToken[0] != 6 || !strings.EqualFold(archive.ResolveString(scriptTokenInt(pathToken)), wantPath) {
			return fmt.Errorf("list %s id %d does not map to %q", path, id, wantPath)
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("list %s id %d matches=%d, want=1", path, id, matches)
	}
	return nil
}

func requireListEntryAbsent(archive *platformpvf.Archive, path string, id int) error {
	raw, err := archive.ReadRaw(path)
	if err != nil {
		return err
	}
	for offset := 0; offset+5 <= len(raw); offset += 5 {
		if raw[offset] == 0 && scriptTokenInt(raw[offset:offset+5]) == id {
			return fmt.Errorf("list %s still contains id %d", path, id)
		}
	}
	return nil
}

func requireSectionIntValue(archive *platformpvf.Archive, path, section string, want int) error {
	raw, err := archive.ReadRaw(path)
	if err != nil {
		return err
	}
	matches := 0
	for offset := 0; offset+10 <= len(raw); offset += 5 {
		token := raw[offset : offset+5]
		if (token[0] != 3 && token[0] != 6) || normalizedLabel(archive.ResolveString(scriptTokenInt(token))) != normalizedLabel(section) {
			continue
		}
		valueToken := raw[offset+5 : offset+10]
		if valueToken[0] != 0 || scriptTokenInt(valueToken) != want {
			return fmt.Errorf("section [%s] does not contain required integer %d", section, want)
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("section [%s] matches=%d, want=1", section, matches)
	}
	return nil
}

func requireTownNPCPlacement(archive *platformpvf.Archive, path string, npcID int, direction string, x, y, z int) error {
	raw, err := archive.ReadRaw(path)
	if err != nil {
		return err
	}
	inNPCSection := false
	matches := 0
	for offset := 0; offset+25 <= len(raw); offset += 5 {
		token := raw[offset : offset+5]
		if token[0] == 3 {
			inNPCSection = normalizedLabel(archive.ResolveString(scriptTokenInt(token))) == "npc"
			continue
		}
		if !inNPCSection || token[0] != 0 || scriptTokenInt(token) != npcID {
			continue
		}
		directionToken := raw[offset+5 : offset+10]
		xToken := raw[offset+10 : offset+15]
		yToken := raw[offset+15 : offset+20]
		zToken := raw[offset+20 : offset+25]
		if directionToken[0] != 6 || normalizedLabel(archive.ResolveString(scriptTokenInt(directionToken))) != normalizedLabel(direction) ||
			xToken[0] != 0 || scriptTokenInt(xToken) != x || yToken[0] != 0 || scriptTokenInt(yToken) != y ||
			zToken[0] != 0 || scriptTokenInt(zToken) != z {
			return fmt.Errorf("town NPC %d placement differs from required tuple", npcID)
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("town NPC %d placement matches=%d, want=1", npcID, matches)
	}
	return nil
}

func requireTownNPCPlacementAbsent(archive *platformpvf.Archive, path string, npcID int) error {
	raw, err := archive.ReadRaw(path)
	if err != nil {
		return err
	}
	inNPCSection := false
	for offset := 0; offset+5 <= len(raw); offset += 5 {
		token := raw[offset : offset+5]
		if token[0] == 3 {
			inNPCSection = normalizedLabel(archive.ResolveString(scriptTokenInt(token))) == "npc"
			continue
		}
		if inNPCSection && token[0] == 0 && scriptTokenInt(token) == npcID {
			return fmt.Errorf("town map %s still contains NPC %d", path, npcID)
		}
	}
	return nil
}

func normalizedLabel(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "[]"))
}

func scriptTokenInt(token []byte) int {
	return int(int32(binary.LittleEndian.Uint32(token[1:5])))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
