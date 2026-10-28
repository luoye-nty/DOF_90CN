// pvfexpiryaudit reports, for each item id given on the command line, the
// runtime PVF expiration contract: the absolute [expiration date] calendar
// stamp, the relative [usable period] / [available period] duration, and the
// document the values came from. It is a read-only diagnostic for comparing
// PVF intent against the durable expire_at rows.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

type listRef struct {
	prefix string
	lst    string
}

var lists = []listRef{
	{"stackable/", "stackable/stackable.lst"},
	{"equipment/", "equipment/equipment.lst"},
	{"avatar/", "avatar/avatar.lst"},
	{"creature/", "creature/creature.lst"},
}

func main() {
	archive, err := platformpvf.Open(`../runtime/data/dnf/Script.pvf`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	index := map[int64]string{}
	for _, ref := range lists {
		text, readErr := archive.ReadText(ref.lst)
		if readErr != nil {
			continue
		}
		fields := strings.Fields(text)
		for i := 0; i+1 < len(fields); i++ {
			id, parseErr := strconv.ParseInt(fields[i], 10, 64)
			if parseErr != nil {
				continue
			}
			path := strings.Trim(fields[i+1], "`")
			if strings.HasSuffix(path, ".stk") || strings.HasSuffix(path, ".equ") ||
				strings.HasSuffix(path, ".ava") || strings.HasSuffix(path, ".cre") {
				if _, exists := index[id]; !exists {
					index[id] = ref.prefix + path
				}
			}
		}
	}

	ids := os.Args[1:]
	if len(ids) == 0 {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				ids = append(ids, line)
			}
		}
	}
	fmt.Printf("%-12s %-22s %-24s %s\n", "item_id", "expiration_date", "usable_period", "path")
	for _, raw := range ids {
		id, parseErr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if parseErr != nil {
			continue
		}
		path, ok := index[id]
		if !ok {
			fmt.Printf("%-12d %-22s %-24s %s\n", id, "-", "-", "(unresolved)")
			continue
		}
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			fmt.Printf("%-12d %-22s %-24s %s\n", id, "?", "?", path+" (read failed)")
			continue
		}
		fmt.Printf("%-12d %-22s %-24s %s\n", id,
			firstSectionValue(text, "expiration date"),
			firstSectionValue(text, "usable period", "available period"),
			path)
	}
}

// firstSectionValue returns the first line of the named section, flattened to
// one space-separated token run, or "-" when no section matches.
func firstSectionValue(text string, names ...string) string {
	lines := strings.Split(text, "\n")
	for _, name := range names {
		header := "[" + name + "]"
		for i, line := range lines {
			if !strings.EqualFold(strings.TrimSpace(line), header) {
				continue
			}
			for j := i + 1; j < len(lines) && j <= i+3; j++ {
				value := strings.TrimSpace(lines[j])
				if value == "" {
					continue
				}
				if strings.HasPrefix(value, "[") {
					break
				}
				return strings.Join(strings.Fields(strings.Trim(value, "`")), " ")
			}
		}
	}
	return "-"
}
