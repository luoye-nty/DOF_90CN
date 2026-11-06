package dnfbridge

import (
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// creditCurrentDirectExperienceAdventureGrowth applies the runtime
// adventurersystem2018.etc growth rate to the pre-level-cap portion of an
// experience consumable. The inventory owner invokes this inside the character
// settlement transaction and saves the mutated account there.
func (s *Service) creditCurrentDirectExperienceAdventureGrowth(
	account *dnfrepo.AccountRecord,
	preCapExperience uint32,
	updatedAt time.Time,
) error {
	if s == nil || account == nil || preCapExperience == 0 {
		return nil
	}
	s.adventureGroupMu.Lock()
	table := s.adventureGroupTable
	loadErr := s.adventureGroupLoadErr
	s.adventureGroupMu.Unlock()
	if table == nil {
		if loadErr != nil {
			return loadErr
		}
		return errAdventureGroupTableUnavailable
	}
	config := table.Runtime().Growth
	if config.MaximumLevel == 0 {
		return errAdventureGroupTableUnavailable
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string)
	}
	_, err := adventuregroup.ApplyGrowthExperience(
		account,
		config,
		adventuregroup.Summary{},
		preCapExperience,
		updatedAt,
	)
	return err
}
