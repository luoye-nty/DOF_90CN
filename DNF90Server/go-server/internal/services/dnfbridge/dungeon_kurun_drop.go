package dnfbridge

import (
	"strings"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentDungeonKurunStrongBreathItemID uint32 = 3326

// currentDungeonKurunStrongBreathDropAmount applies the Moonlight Creek
// special-dungeon rule without replacing the ordinary PVF drop pool. Elite
// actors drop one material; boss quantity comes from the dungeon's native
// [nateram time attack info] section (7 in the current Script.pvf).
func currentDungeonKurunStrongBreathDropAmount(dungeon worldmap.Dungeon, monsterType byte) uint32 {
	if !strings.Contains(strings.ToLower(strings.ReplaceAll(dungeon.Path, "\\", "/")), "dragon_kurun_kor/") {
		return 0
	}
	switch monsterType {
	case 1, 2:
		return 1
	case 3:
		for _, section := range dungeon.SourceSections {
			if normalizeDungeonPVFSymbol(section.Name) != "nateram time attack info" {
				continue
			}
			for _, token := range section.Tokens {
				if token.Int > 0 && token.Int <= int64(^uint32(0)) {
					return uint32(token.Int)
				}
			}
		}
	}
	return 0
}
