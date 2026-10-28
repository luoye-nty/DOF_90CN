// avatarcompoundcheck 用来核对时装合成器的 PVF 数据。
//
//	go run ./cmd/scratch/avatarcompoundcheck scan            列出每个职业分表的稀有池/普通池及各候选自己的 grade
//	go run ./cmd/scratch/avatarcompoundcheck 112510093 ...   打印指定装备的 name/grade/rarity/equipment type
//
// scan 的结论：16 张分表 586 个稀有候选的 PVF grade 全是 3，普通候选全是 2，
// 也就是分表里的 [grade] 1 与 [upper grade] 2 说的是**投入的来源装扮**，
// 不是产出物品的档次。
package main

import (
	"flag"
	"fmt"
	"path"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func resolvePath(archive *platformpvf.Archive, listPath, reference string) string {
	reference = strings.TrimSpace(strings.Trim(reference, "`\"'"))
	for _, candidate := range []string{reference, path.Join(path.Dir(listPath), reference)} {
		if file, ok := archive.FindFile(candidate); ok {
			return file.ArchivePath
		}
	}
	return ""
}

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	flag.Parse()
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	text, err := archive.ReadText("equipment/equipment.lst")
	if err != nil {
		panic(err)
	}
	doc, err := dnfpvf.Parse("equipment/equipment.lst", text)
	if err != nil {
		panic(err)
	}
	byID := map[int64]string{}
	for _, entry := range dnfpvf.ParseList(doc) {
		byID[entry.ID] = resolvePath(archive, "equipment/equipment.lst", entry.Path)
	}

	grade := func(id int64) int64 {
		p := byID[id]
		if p == "" {
			return -1
		}
		txt, e := archive.ReadText(p)
		if e != nil {
			return -1
		}
		d, e := dnfpvf.Parse(p, txt)
		if e != nil {
			return -1
		}
		g, _ := d.Int("grade")
		return g
	}

	if len(flag.Args()) == 1 && flag.Args()[0] == "scan" {
		idxText, _ := archive.ReadText("etc/chn_compound/compoundavatar.etc")
		idxDoc, _ := dnfpvf.Parse("etc/chn_compound/compoundavatar.etc", idxText)
		seen := map[int64]int{}
		for _, tok := range idxDoc.Tokens {
			name := strings.TrimSpace(strings.Trim(tok.Value, "`"))
			if !strings.HasSuffix(strings.ToLower(name), ".etc") {
				continue
			}
			tp := "etc/chn_compound/" + strings.ToLower(name)
			tt, readErr := archive.ReadText(tp)
			if readErr != nil {
				fmt.Println(tp, "ERR", readErr)
				continue
			}
			td, parseErr := dnfpvf.Parse(tp, tt)
			if parseErr != nil {
				continue
			}
			g, _ := td.Int("grade")
			ug, _ := td.Int("upper grade")
			fmt.Printf("== %s grade=%d upper=%d\n", name, g, ug)
			for _, part := range []string{"hat", "hair", "face", "neck", "coat", "pants", "belt", "shoes"} {
				vals := td.Ints(part + " avatar")
				if len(vals) < 3 {
					continue
				}
				n := int(vals[0])
				pairs := vals[1:]
				specials := ""
				for i := 0; i*2+1 < len(pairs) && i < n; i++ {
					gr := grade(pairs[i*2])
					specials += fmt.Sprintf(" %d(w=%d,g=%d)", pairs[i*2], pairs[i*2+1], gr)
					seen[gr]++
				}
				normals := ""
				for i := n; i*2+1 < len(pairs); i++ {
					normals += fmt.Sprintf(" %d(w=%d,g=%d)", pairs[i*2], pairs[i*2+1], grade(pairs[i*2]))
				}
				fmt.Printf("  %-6s specials:%s | normals:%s\n", part, specials, normals)
			}
		}
		fmt.Println("special grade histogram:", seen)
		return
	}

	for _, id := range flag.Args() {
		var itemID int64
		fmt.Sscan(id, &itemID)
		p := byID[itemID]
		if p == "" {
			fmt.Printf("%d: NOT IN equipment.lst\n", itemID)
			continue
		}
		t, readErr := archive.ReadText(p)
		if readErr != nil {
			fmt.Printf("%d %s: read error %v\n", itemID, p, readErr)
			continue
		}
		d, parseErr := dnfpvf.Parse(p, t)
		if parseErr != nil {
			fmt.Printf("%d %s: parse error %v\n", itemID, p, parseErr)
			continue
		}
		g, hasGrade := d.Int("grade")
		rarity, hasRarity := d.Int("rarity")
		name := ""
		if tokens, ok := d.Section("name"); ok && len(tokens) > 0 {
			name = tokens[0].Value
		}
		eqType := ""
		if tokens, ok := d.Section("equipment type"); ok && len(tokens) > 0 {
			eqType = tokens[0].Value
		}
		fmt.Printf("%d %s name=%q grade=%d(%v) rarity=%d(%v) type=%s\n",
			itemID, p, name, g, hasGrade, rarity, hasRarity, eqType)
	}
}
