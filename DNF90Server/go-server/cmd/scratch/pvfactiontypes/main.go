// Command pvfactiontypes censuses the `[action type]` declarations across every
// stackable in a real Script.pvf.
//
// Why: the pet transport stone (item 490700208) declares
//
//	[action type]
//	`[creature transport stone]` 16
//	[/action type]
//
// and the client was observed searching its own registry for exactly the value
// 16 and missing. This tool answers "which action types exist in this PVF, what
// are their enum values, and how many items use each" without needing the game
// running.
//
//	go run ./cmd/scratch/pvfactiontypes -pvf <Script.pvf>
package main

import (
	"flag"
	"fmt"
	"path"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type bucket struct {
	value int64
	count int
	names []string
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	listPath := flag.String("list", "stackable/stackable.lst", "list file to walk")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	root := path.Dir(*listPath)

	text, err := archive.ReadText(*listPath)
	if err != nil {
		panic(err)
	}
	doc, err := dnfpvf.Parse(*listPath, text)
	if err != nil {
		panic(err)
	}
	entries := dnfpvf.ParseList(doc)

	buckets := map[string]*bucket{}
	withAction := 0
	for _, entry := range entries {
		reference := strings.TrimSpace(strings.Trim(entry.Path, "`\"'"))
		if reference == "" {
			continue
		}
		var itemText string
		for _, candidate := range []string{reference, path.Join(root, reference)} {
			if value, readErr := archive.ReadText(candidate); readErr == nil {
				itemText = value
				break
			}
		}
		if itemText == "" {
			continue
		}
		itemDoc, parseErr := dnfpvf.Parse(reference, itemText)
		if parseErr != nil {
			continue
		}
		values := itemDoc.Texts("action type")
		if len(values) == 0 {
			continue
		}
		ints := itemDoc.Ints("action type")
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := value
			slot, ok := buckets[key]
			if !ok {
				slot = &bucket{value: -1}
				buckets[key] = slot
			}
			slot.count++
			if len(ints) > 0 && slot.value < 0 {
				slot.value = ints[0]
			}
			name, _ := itemDoc.Text("name")
			if len(slot.names) < 4 && name != "" {
				slot.names = append(slot.names, fmt.Sprintf("%d=%s", entry.ID, strings.TrimSpace(name)))
			}
			withAction++
		}
	}

	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if buckets[keys[i]].value != buckets[keys[j]].value {
			return buckets[keys[i]].value < buckets[keys[j]].value
		}
		return keys[i] < keys[j]
	})

	fmt.Printf("%d stackables carry an [action type]; %d distinct values\n\n", withAction, len(keys))
	for _, key := range keys {
		slot := buckets[key]
		fmt.Printf("  value=%-6d %-34s x%-4d  e.g. %s\n",
			slot.value, key, slot.count, strings.Join(slot.names, ", "))
	}
}
