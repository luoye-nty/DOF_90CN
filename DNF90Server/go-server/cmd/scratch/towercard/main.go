// Scratch tool: dump the塔 dungeon names / area membership straight out of the
// runtime Script.pvf, to settle where the "绝望之塔(1层)" card title comes from.
//
//	go run ./cmd/scratch/towercard -pvf <Script.pvf> -area 11008
package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `runtime\data\dnf\Script.pvf`, "Script.pvf path")
	area := flag.Int64("area", 11008, "dungeon id used to locate the owning world-map area")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists: []string{"dungeon/dungeon.lst", "worldmap/worldmap.lst"},
	})
	if err != nil {
		fmt.Println("build index:", err)
		return
	}

	fmt.Println("== dungeon/dungeon.lst entries around the towers ==")
	for _, id := range []int64{11007, 11008, 11009, 11010, 11011, 11106, 11107, 11108, 5114, 5115, 5116, 5117, 5118} {
		p, ok := index.Resolve("dungeon/dungeon.lst", id)
		if !ok {
			fmt.Printf("  %6d  <absent>\n", id)
			continue
		}
		name, minLevel, towerSections := "", "", ""
		if text, err := archive.ReadText(p); err == nil {
			name = sectionValue(text, "name")
			minLevel = sectionValue(text, "minimum required level")
			towerSections = grepSections(text, "tower")
		}
		fmt.Printf("  %6d  %-46s name=%-14q lv=%-4q %s\n", id, p, name, minLevel, towerSections)
	}

	fmt.Println()
	fmt.Println("== world-map area owning dungeon", *area, "==")
	table, err := worldmap.Load(context.Background(), index, worldmap.Options{SkipMaps: true, SkipDungeons: true})
	if err != nil {
		fmt.Println("load worldmap:", err)
		return
	}
	owner, ok := table.FindAreaByDungeon(*area)
	if !ok {
		fmt.Println("  <no area references that dungeon>")
		return
	}
	fmt.Printf("  area id=%d path=%s name=%q ui=%q\n", owner.ID, owner.Path, owner.Name, owner.UIPath)
	fmt.Println("  dungeon list:")
	for _, d := range owner.Dungeons {
		fmt.Printf("    id=%-7d quest=%-7d inProgressOnly=%v\n", d.DungeonID, d.QuestID, d.InProgressOnly)
	}

	fmt.Println()
	fmt.Println("== area raw sections ==")
	fmt.Println(owner.SourceSections)
}

// firstQuoted returns the first backtick/quoted literal in a PVF document,
// which for dungeon/<x>.dun is the [name] value.
func firstQuoted(text string) string {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '`' && c != '"' && c != '\'' {
			continue
		}
		end := strings.IndexByte(text[i+1:], c)
		if end < 0 {
			return ""
		}
		value := text[i+1 : i+1+end]
		if strings.TrimSpace(value) != "" {
			return value
		}
		i += 1 + end
	}
	return ""
}

// sectionValue returns the first scalar token after the named `[section]`.
func sectionValue(text, section string) string {
	marker := "[" + section + "]"
	idx := strings.Index(text, marker)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(marker):]
	if end := strings.Index(rest, "\n["); end >= 0 {
		rest = rest[:end]
	}
	return strings.Trim(strings.TrimSpace(firstQuoted(rest)), "`")
}

// grepSections lists the `[section]` headers whose name mentions needle, so a
// tower marker section can be spotted without dumping the whole document.
func grepSections(text, needle string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") &&
			strings.Contains(strings.ToLower(line), needle) {
			out = append(out, line)
		}
	}
	return strings.Join(out, ",")
}
