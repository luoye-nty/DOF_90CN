// itemnamebyid 按物品 ID 反查 PVF 定义（名称/路径/类型），用于把日志里的
// 裸 ID（如世界掉落 world_selected_item=3027）还原成中文道具名。
//
//	go run ./cmd/scratch/itemnamebyid -pvf <Script.pvf> -ids 3027,3028,3331
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type row struct {
	ItemID        int64  `json:"item_id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	Rarity        int64  `json:"rarity,omitempty"`
	MinimumLevel  int64  `json:"minimum_level,omitempty"`
	StackableType string `json:"stackable_type,omitempty"`
	EquipmentType string `json:"equipment_type,omitempty"`
	StackLimit    int64  `json:"stack_limit,omitempty"`
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	ids := flag.String("ids", "", "逗号分隔的物品 ID")
	flag.Parse()
	if *pvfPath == "" || *ids == "" {
		fmt.Fprintln(os.Stderr, "-pvf and -ids are required")
		os.Exit(2)
	}
	want := make(map[int64]bool)
	for _, field := range strings.Split(*ids, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad id:", field)
			os.Exit(2)
		}
		want[value] = true
	}

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	rows := make([]row, 0, len(want))
	for _, listPath := range []string{"stackable/stackable.lst", "equipment/equipment.lst"} {
		text, readErr := archive.ReadText(listPath)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "read", listPath, readErr)
			continue
		}
		doc, parseErr := dnfpvf.Parse(listPath, text)
		if parseErr != nil {
			panic(parseErr)
		}
		for _, entry := range dnfpvf.ParseList(doc) {
			if !want[entry.ID] {
				continue
			}
			docPath := resolvePath(archive, listPath, entry.Path)
			if docPath == "" {
				continue
			}
			itemText, textErr := archive.ReadText(docPath)
			if textErr != nil {
				continue
			}
			itemDoc, docErr := dnfpvf.Parse(docPath, itemText)
			if docErr != nil {
				continue
			}
			name, _ := itemDoc.Text("name")
			stackableType, _ := itemDoc.Text("stackable type")
			equipmentType, _ := itemDoc.Text("equipment type")
			rarity, _ := itemDoc.Int("rarity")
			minimumLevel, _ := itemDoc.Int("minimum level")
			stackLimit, _ := itemDoc.Int("stack limit")
			rows = append(rows, row{
				ItemID:        entry.ID,
				Kind:          strings.TrimSuffix(path.Base(path.Dir(listPath)), "s"),
				Name:          name,
				Path:          docPath,
				Rarity:        rarity,
				MinimumLevel:  minimumLevel,
				StackableType: stackableType,
				EquipmentType: equipmentType,
				StackLimit:    stackLimit,
			})
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(rows); err != nil {
		panic(err)
	}
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
