package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type row struct {
	ItemID         int64    `json:"item_id"`
	Path           string   `json:"path"`
	Name           string   `json:"name,omitempty"`
	FontIndex      int64    `json:"font_index"`
	ExpirationInfo []string `json:"expiration_info,omitempty"`
	ItemExpiration string   `json:"item_expiration,omitempty"`
}

func main() {
	input := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "PVF path")
	flag.Parse()
	archive, err := platformpvf.Open(*input)
	if err != nil {
		panic(err)
	}
	listText, err := archive.ReadText("stackable/stackable.lst")
	if err != nil {
		panic(err)
	}
	listDoc, err := dnfpvf.Parse("stackable/stackable.lst", listText)
	if err != nil {
		panic(err)
	}
	rows := make([]row, 0)
	for _, entry := range dnfpvf.ParseList(listDoc) {
		ref := strings.Trim(strings.TrimSpace(entry.Path), "`\"'")
		path := filepath.ToSlash(filepath.Join("stackable", strings.TrimPrefix(ref, "stackable/")))
		text, err := archive.ReadText(path)
		if err != nil {
			continue
		}
		doc, err := dnfpvf.Parse(path, text)
		if err != nil {
			continue
		}
		action, _ := doc.Text("action type")
		if !strings.EqualFold(strings.TrimSpace(action), "[add damage font skin]") {
			continue
		}
		name, _ := doc.Text("name")
		index, _ := doc.Int("index")
		itemExpiry, _ := doc.Text("expiration date")
		rows = append(rows, row{ItemID: entry.ID, Path: path, Name: name, FontIndex: index, ExpirationInfo: doc.Texts("expiration info"), ItemExpiration: itemExpiry})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		panic(fmt.Errorf("encode: %w", err))
	}
}
