package dnfbridge

import (
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

// protectedProfessionSkillLevels is the single source of truth for skill
// levels that a reset must never remove. The profession profile resolves the
// character's base-job, class-change, first-awakening and second-awakening
// grants cumulatively; the catalog then adds only PVF-proven system grants.
//
// Returning false for an empty positive baseline is intentional. A skill
// reset must fail closed instead of replacing a character's tree with an
// incomplete or empty ruleset when profession data is unavailable.
func protectedProfessionSkillLevels(
	profiles *dnfprofession.Profiles,
	catalog *dnfskill.Table,
	job byte,
	growType byte,
	characterLevel int,
) (map[uint16]int, bool) {
	grants, ok := profiles.FreeGrantsAtLevel(catalog, job, growType, characterLevel)
	if !ok {
		return nil, false
	}
	levels := make(map[uint16]int, len(grants))
	for _, grant := range grants {
		if grant.SkillID == 0 || grant.Level <= 0 {
			continue
		}
		if grant.Level > levels[grant.SkillID] {
			levels[grant.SkillID] = grant.Level
		}
	}
	if len(levels) == 0 {
		return nil, false
	}
	return levels, true
}
