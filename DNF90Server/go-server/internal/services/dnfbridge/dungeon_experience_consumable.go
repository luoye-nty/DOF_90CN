package dnfbridge

import (
	"context"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/experiencebonus"
)

func (s *Service) currentDungeonExperienceConsumableBonusExp(
	ctx context.Context,
	accountID string,
	characterID string,
	base uint32,
) (bonus uint32, effect experiencebonus.Effect, active bool) {
	if s == nil || base == 0 || strings.TrimSpace(accountID) == "" || strings.TrimSpace(characterID) == "" {
		return 0, experiencebonus.Effect{}, false
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return 0, experiencebonus.Effect{}, false
	}
	account, found, err := repositories.Account.Load(ctx, strings.TrimSpace(accountID))
	if err != nil || !found {
		return 0, experiencebonus.Effect{}, false
	}
	effect, active = experiencebonus.ActiveForCharacter(account, characterID, time.Now().UTC())
	if !active {
		return 0, experiencebonus.Effect{}, false
	}
	return currentPercentageBonusUint32(base, effect.BonusPercent), effect, true
}
