package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
)

const currentDungeonAdventureGroupDefaultClearBonusPercent = 10

// currentDungeonAdventureGroupClearBonusExp applies the current client/PVF
// clear-result benefit only to non-capped characters. The active PVF leaves
// charactermanage.etc [exp bonus] empty, while the adventure grow benefit is
// a fixed 10%; keep an explicit fallback only after management level 1.
func (s *Service) currentDungeonAdventureGroupClearBonusExp(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	base uint32,
) (bonus uint32, manageLevel int, percent int64) {
	if s == nil || session == nil || runtime == nil || base == 0 ||
		runtime.Character.Level <= 0 || runtime.Character.Level >= currentAdventureCharacterLevelCap {
		return 0, 0, 0
	}
	summary, err := s.currentAccountAdventureGroupSummary(ctx, runtime.Character, true, session)
	if err != nil || summary.ManageLevel < 1 {
		return 0, summary.ManageLevel, 0
	}
	percent = currentDungeonAdventureGroupClearBonusPercent(summary)
	return currentPercentageBonusUint32(base, percent), summary.ManageLevel, percent
}

func currentDungeonAdventureGroupClearBonusPercent(summary adventuregroup.Summary) int64 {
	if summary.ManageLevel < 1 {
		return 0
	}
	if summary.ExpBonusPercent > 0 {
		return int64(summary.ExpBonusPercent)
	}
	return currentDungeonAdventureGroupDefaultClearBonusPercent
}
