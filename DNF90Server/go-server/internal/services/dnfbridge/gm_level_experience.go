package dnfbridge

import (
	"context"

	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
)

func (s *Service) currentPVFLevelEntryExperience(ctx context.Context, level int) (uint32, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.experienceMu.Lock()
	defer s.experienceMu.Unlock()
	if s.experienceTable == nil && s.experienceTableLoadErr == nil {
		s.initialEquipmentMu.Lock()
		archive, err := s.initialEquipmentArchiveLocked()
		s.initialEquipmentMu.Unlock()
		if err != nil {
			s.experienceTableLoadErr = err
		} else {
			s.experienceTable, s.experienceTableLoadErr = dnfprogression.LoadExperienceTable(ctx, archive)
		}
	}
	if s.experienceTableLoadErr != nil {
		return 0, s.experienceTableLoadErr
	}
	return s.experienceTable.EntryExperience(level)
}
