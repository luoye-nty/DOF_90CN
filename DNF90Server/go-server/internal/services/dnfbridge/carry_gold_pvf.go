package dnfbridge

import (
	"context"
	"fmt"

	dnfcarrygold "longheng.io/server/internal/modules/dnf/carrygold"
)

// preloadCarryGoldLevelLimits installs the real `[gold limit from level]`
// table before any listener opens. The base carry cap is PVF data keyed by
// character level, not a constant, so a missing or malformed table fails
// startup instead of silently leaving every gold faucet uncapped.
func (s *Service) preloadCarryGoldLevelLimits(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("dnfbridge service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return fmt.Errorf("load carry gold level limit archive: %w", err)
	}
	limits, err := dnfcarrygold.LoadLevelLimits(ctx, archive)
	if err != nil {
		return fmt.Errorf("load carry gold level limit table: %w", err)
	}
	dnfcarrygold.Install(limits)
	s.logPacketEvent("dnf-carry-gold-level-limit-pvf-loaded",
		"path", dnfcarrygold.LevelLimitPath,
		"rows", limits.Len(),
		"level_1_limit", limits.Limit(1),
		"level_60_limit", limits.Limit(60),
		"level_90_limit", limits.Limit(90),
		"max_stage_bonus", dnfcarrygold.StageBonus(dnfcarrygold.MaxStage))
	return nil
}
