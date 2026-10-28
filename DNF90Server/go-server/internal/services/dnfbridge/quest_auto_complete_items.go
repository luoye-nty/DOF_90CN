package dnfbridge

import (
	"context"
	"strconv"
	"time"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// repairCurrentAutoCompletedQuestCarryForwardItems repairs characters that
// were saved before task-manual auto completion preserved required story
// items. It is intentionally invoked before the one-shot selected inventory
// bootstrap, so a repaired row is included in the client's first list-0
// snapshot without an extra packet or repeated per-list catalog scans.
func (s *Service) repairCurrentAutoCompletedQuestCarryForwardItems(
	ctx context.Context,
	session *gameSession,
) (dnfquest.AutoCompleteCarryForwardRepairResult, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return dnfquest.AutoCompleteCarryForwardRepairResult{Idempotent: true}, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return dnfquest.AutoCompleteCarryForwardRepairResult{}, dnfquest.ErrAutoCompleteMainUnavailable
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return dnfquest.AutoCompleteCarryForwardRepairResult{}, err
	}
	itemCatalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return dnfquest.AutoCompleteCarryForwardRepairResult{}, err
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return dnfquest.AutoCompleteCarryForwardRepairResult{}, err
	}
	mainSlotCount := uint16(0)
	if repositories.Settings != nil {
		containerState := s.loadCurrentItemListContainerState(ctx, repositories.Settings, strconv.Itoa(int(session.selectedCharacterID)), dnfrepo.MainInventoryListType)
		mainSlotCount = containerState.MainSlotCount
	}
	return owner.ApplyAutoCompleteCarryForwardRepair(ctx, catalog, dnfquest.AutoCompleteCarryForwardRepairInput{
		CharacterID:  strconv.Itoa(int(session.selectedCharacterID)),
		RepairedAt:   time.Now().UTC(),
		AllocateItem: currentQuestFinishItemAllocator(itemCatalog, mainSlotCount),
	})
}
