package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const probePVFPath = `DNF_REAL_PVF`

type probeItem struct {
	path string
	doc  *dnfpvf.Document
	text string
}

func loadProbeEqu(t *testing.T) (*platformpvf.Archive, []probeItem) {
	t.Helper()
	archive, err := platformpvf.Open(probePVFPath)
	if err != nil {
		t.Fatal(err)
	}
	items := []probeItem{}
	archive.RangePaths(func(p string) bool {
		low := strings.ToLower(p)
		if !strings.HasSuffix(low, ".equ") {
			return true
		}
		text, err := archive.ReadText(p)
		if err != nil {
			return true
		}
		if !strings.Contains(text, "[skill levelup]") && !strings.Contains(text, "[all skill item]") {
			return true
		}
		doc, perr := dnfpvf.Parse(p, text)
		if perr != nil {
			return true
		}
		items = append(items, probeItem{path: p, doc: doc, text: text})
		return true
	})
	return archive, items
}

func TestZZProbeRealBuffCoverage(t *testing.T) {
	archive, items := loadProbeEqu(t)
	ctx := context.Background()
	table, err := buildSkillCatalogFromSource(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}

	character := dnfrepo.CharacterRecord{Job: "4", Stats: map[string]int64{"grow_type": 1}}
	candidates, err := skillSwitchSupportSkillsByPVFSource(archive, character)
	if err != nil {
		t.Fatal(err)
	}
	jobTag := "priest"
	stage := byte(1)
	fmt.Printf("PROBE items scanned=%d priest(4) grow=1 candidates=%v\n", len(items), candidates)
	for _, cand := range candidates {
		def, ok := table.Find(4, cand)
		req, active, name := 0, false, ""
		if ok {
			req, active, name = def.RequiredLevel, def.Active, def.Name
		}
		exact, ranged, both, totalNonZero := 0, 0, 0, 0
		best := ""
		for _, it := range items {
			bonus := skillSwitchDocumentBuffLevelBonus(it.doc, jobTag, cand, req, active, stage)
			_ = best
			// split contributions: exact-only by re-running with range disabled is
			// not exposed; approximate by checking block presence.
			if bonus == 0 {
				continue
			}
			totalNonZero++
			hasLevelup := strings.Contains(it.text, "[skill levelup]")
			hasAllSkill := strings.Contains(it.text, "[all skill item]")
			switch {
			case hasLevelup && hasAllSkill:
				both++
			case hasLevelup:
				exact++
			case hasAllSkill:
				ranged++
			}
		}
		fmt.Printf("PROBE cand=%d name=%q requiredLevel=%d active=%v -> itemsWithBonus=%d (exactOnly=%d rangeOnly=%d mixed=%d)\n",
			cand, name, req, active, totalNonZero, exact, ranged, both)
	}

	// Sample the levelup rows for a couple of concrete items to inspect tag layout.
	samples := 0
	for _, it := range items {
		if samples >= 3 {
			break
		}
		for _, sec := range it.doc.Sections {
			if sectionName(sec.Name) != "skill levelup" {
				continue
			}
			if !strings.Contains(it.text, "[swordman]") {
				continue
			}
			raw := ""
			for _, tok := range it.doc.Tokens[sec.Start:sec.End] {
				raw += tok.Raw + " "
			}
			fmt.Printf("PROBE sample levelup path=%s rows=%s\n", it.path, strings.TrimSpace(raw))
			samples++
			break
		}
	}

	// Multi-tag same-skill rows: evidence for job-tag exclusivity.
	dupSamples := 0
	for _, it := range items {
		if dupSamples >= 4 {
			break
		}
		for _, sec := range it.doc.Sections {
			if sectionName(sec.Name) != "skill levelup" {
				continue
			}
			bySkill := map[int64][]string{}
			toks := it.doc.Tokens[sec.Start:sec.End]
			for i := 0; i+2 < len(toks); i += 3 {
				if toks[i+1].Kind == dnfpvf.TokenInt {
					bySkill[toks[i+1].Int] = append(bySkill[toks[i+1].Int], toks[i].Raw)
				}
			}
			for skill, tags := range bySkill {
				if len(tags) > 1 {
					fmt.Printf("PROBE multiset path=%s skill=%d tags=%v\n", it.path, skill, tags)
					dupSamples++
					break
				}
			}
			break
		}
	}
}

func TestZZProbeCommonTagAndSkillLevelTag(t *testing.T) {
	archive, items := loadProbeEqu(t)
	ctx := context.Background()
	table, err := buildSkillCatalogFromSource(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	commonSkills := map[int64]int{}
	for _, it := range items {
		for _, sec := range it.doc.Sections {
			if sectionName(sec.Name) != "skill levelup" {
				continue
			}
			toks := it.doc.Tokens[sec.Start:sec.End]
			for i := 0; i+2 < len(toks); i += 3 {
				if strings.ToLower(strings.Trim(toks[i].Raw, "`")) != "common" {
					continue
				}
				if toks[i+1].Kind == dnfpvf.TokenInt {
					commonSkills[toks[i+1].Int]++
				}
			}
		}
	}
	keys := make([]int, 0, len(commonSkills))
	for k := range commonSkills {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	fmt.Printf("PROBE [common] rows distinctSkills=%d\n", len(keys))
	shown := 0
	for _, k := range keys {
		name := ""
		for job := byte(0); job < 16; job++ {
			if def, ok := table.Find(job, uint16(k)); ok {
				name = def.Name + " (job " + fmt.Sprint(job) + ")"
				break
			}
		}
		fmt.Printf("PROBE   common skill=%d rows=%d name=%q\n", k, commonSkills[int64(k)], name)
		shown++
		if shown >= 20 {
			break
		}
	}

	// Which file types carry [SKILL_LEVEL]? Only the small data/config
	// extensions are decompressed here to keep the probe cheap.
	byExt := map[string]int{}
	archive.RangePaths(func(p string) bool {
		low := strings.ToLower(p)
		want := false
		for _, ext := range []string{".dat", ".etc", ".lst", ".skl", ".txt", ".ini"} {
			if strings.HasSuffix(low, ext) {
				want = true
				break
			}
		}
		if !want {
			return true
		}
		text, err := archive.ReadText(p)
		if err != nil {
			return true
		}
		if strings.Contains(text, "[SKILL_LEVEL]") {
			idx := strings.LastIndex(low, ".")
			ext := low[idx:]
			byExt[ext]++
			if byExt[ext] <= 3 {
				fmt.Printf("PROBE [SKILL_LEVEL] hit in %s\n", p)
			}
		}
		return true
	})
	fmt.Printf("PROBE [SKILL_LEVEL] by extension: %v\n", byExt)
}
