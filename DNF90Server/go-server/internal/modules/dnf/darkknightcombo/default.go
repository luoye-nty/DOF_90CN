package darkknightcombo

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

var comboRootPathRE = regexp.MustCompile(`(?i)(^|/)comboset\d+\.skl$`)

type TextSource interface {
	ReadText(relativePath string) (string, error)
}

// BuildDefaultPages derives combo roots and children only from the active PVF
// and learned skills. No root or child skill ID is hard-coded.
func BuildDefaultPages(source TextSource, catalog *dnfskill.Table, record dnfrepo.SkillRecord) ([]Page, error) {
	if source == nil || catalog == nil {
		return nil, fmt.Errorf("dark knight combo PVF resources are unavailable")
	}
	pages := make([]Page, 0, MaximumPageCount)
	for pageIndex := 0; pageIndex < MaximumPageCount; pageIndex++ {
		state, found := dnfrepo.SkillTree(record, pageIndex)
		if !found || len(state.Skills) == 0 {
			continue
		}
		page, ok, err := buildDefaultPage(source, catalog, byte(pageIndex), state, record.Layouts[pageIndex])
		if err != nil {
			return nil, err
		}
		if ok {
			pages = append(pages, page)
		}
	}
	return pages, nil
}

func buildDefaultPage(source TextSource, catalog *dnfskill.Table, pageIndex byte, state dnfrepo.SkillTreeState, layout dnfrepo.SkillLayout) (Page, bool, error) {
	learned := make(map[uint16]struct{}, len(state.Skills))
	for skillID, skillState := range state.Skills {
		if skillState.Enabled && skillState.Level > 0 && skillID > 0 && skillID <= 0xffff {
			learned[uint16(skillID)] = struct{}{}
		}
	}
	if len(learned) == 0 {
		return Page{}, false, nil
	}
	ordered := orderedLearnedSkillIDs(layout, learned)
	page := Page{Index: pageIndex}
	for _, skillID := range ordered {
		definition, found := catalog.Find(Job, skillID)
		if !found || !comboRootPathRE.MatchString(strings.ReplaceAll(definition.Path, `\`, "/")) {
			continue
		}
		children, err := loadComboSet(source, definition.Path)
		if err != nil {
			return Page{}, false, fmt.Errorf("load dark knight combo root %d (%s): %w", skillID, definition.Path, err)
		}
		root := Root{SkillID: skillID}
		for _, child := range children {
			if _, found := learned[child]; found {
				root.Children = append(root.Children, child)
			}
		}
		page.Roots = append(page.Roots, root)
	}
	if len(page.Roots) == 0 {
		return Page{}, false, nil
	}
	page, err := NormalizePage(page)
	return page, err == nil, err
}

func orderedLearnedSkillIDs(layout dnfrepo.SkillLayout, learned map[uint16]struct{}) []uint16 {
	slots := make([]int, 0, len(layout))
	for slot := range layout {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	ordered := make([]uint16, 0, len(learned))
	seen := make(map[uint16]struct{}, len(learned))
	for _, slot := range slots {
		skillID := layout[slot]
		if _, ok := learned[skillID]; !ok {
			continue
		}
		if _, ok := seen[skillID]; ok {
			continue
		}
		seen[skillID] = struct{}{}
		ordered = append(ordered, skillID)
	}
	missing := make([]int, 0, len(learned)-len(ordered))
	for skillID := range learned {
		if _, ok := seen[skillID]; !ok {
			missing = append(missing, int(skillID))
		}
	}
	sort.Ints(missing)
	for _, skillID := range missing {
		ordered = append(ordered, uint16(skillID))
	}
	return ordered
}

func loadComboSet(source TextSource, skillPath string) ([]uint16, error) {
	clean := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(skillPath), `\`, "/"), "/")
	if !strings.HasPrefix(strings.ToLower(clean), "skill/") {
		clean = path.Join("skill", clean)
	}
	text, err := source.ReadText(clean)
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse(clean, text)
	if err != nil {
		return nil, err
	}
	values := doc.Ints("combo set")
	children := make([]uint16, 0, len(values))
	for _, value := range values {
		if value > 0 && value <= 0xffff {
			children = append(children, uint16(value))
		}
	}
	return children, nil
}
