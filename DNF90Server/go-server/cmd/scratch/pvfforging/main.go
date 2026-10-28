// pvfforging dumps the weapon-forging data needed by the op438 separate
// upgrade flow: the etc/upgrade_separate.etc table, every stackable that
// carries an [equipment separate reinforcement ticket] section, and the
// equipment definition of an optional target item id.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	target := flag.Int64("target", 0, "optional equipment item id to dump")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	if text, readErr := archive.ReadText("etc/upgrade_separate.etc"); readErr == nil {
		fmt.Println("==== etc/upgrade_separate.etc ====")
		fmt.Println(text)
	} else {
		fmt.Println("etc/upgrade_separate.etc read failed:", readErr)
	}

	fmt.Println("==== stackables with [equipment separate reinforcement ticket] ====")
	for _, file := range archive.Files() {
		path := strings.ToLower(file.ArchivePath)
		if !strings.HasPrefix(path, "stackable/") || !strings.HasSuffix(path, ".stk") {
			continue
		}
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil || !strings.Contains(text, "[equipment separate reinforcement ticket]") {
			continue
		}
		fmt.Println("---", file.ArchivePath)
		fmt.Println(text)
	}

	if *target > 0 {
		fmt.Println("==== target equipment", *target, "====")
		lst, readErr := archive.ReadText("equipment/equipment.lst")
		if readErr != nil {
			fmt.Println("equipment.lst read failed:", readErr)
			return
		}
		fields := strings.Fields(lst)
		for i := 0; i+1 < len(fields); i++ {
			id, parseErr := strconv.ParseInt(fields[i], 10, 64)
			if parseErr != nil || id != *target {
				continue
			}
			rel := strings.Trim(fields[i+1], "`")
			docPath := "equipment/" + rel
			text, docErr := archive.ReadText(docPath)
			if docErr != nil {
				fmt.Println("read", docPath, "failed:", docErr)
				return
			}
			for _, line := range strings.Split(text, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				fmt.Println(trimmed)
				if strings.HasPrefix(trimmed, "[equipment type]") {
					// keep dumping; full doc is small enough
				}
			}
			return
		}
		fmt.Println("target not found in equipment.lst")
	}
}
