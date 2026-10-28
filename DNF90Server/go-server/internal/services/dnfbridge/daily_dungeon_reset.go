package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentTowerOfDespairInvitationItemID int64 = 4473
	currentTowerOfGriefInvitationItemID   int64 = 10161065
	currentDailyInvitationCheckSlotStart  int16 = 121
	currentDailyInvitationQuestSlotStart  int16 = 177
	currentDailyInvitationQuestSlotEnd    int16 = 232

	currentTowerOfDespairInvitationPVFPath = "stackable/invitation_drapyus2.stk"
	currentTowerOfGriefInvitationPVFPath   = "stackable/10161001/10161065.stk"
)

func (s *Service) reconcileCurrentDailyDungeonSelection(
	ctx context.Context,
	session *gameSession,
) (dnfdungeonadmission.DailyResetResult, error) {
	return s.reconcileCurrentDailyDungeonSelectionAt(ctx, session, s.gameplayNow().UTC())
}

func (s *Service) reconcileCurrentDailyDungeonSelectionAt(
	ctx context.Context,
	session *gameSession,
	now time.Time,
) (dnfdungeonadmission.DailyResetResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return dnfdungeonadmission.DailyResetResult{}, nil
	}
	s.worldMapMu.RLock()
	admissionCatalog := s.dungeonAdmissionCatalog
	s.worldMapMu.RUnlock()
	if admissionCatalog == nil {
		// Most narrow bridge fixtures intentionally have no runtime PVF. A real
		// service with a loaded archive must never silently skip its daily owner.
		s.initialEquipmentMu.Lock()
		hasRuntimePVF := s.initialEquipmentArchive != nil
		s.initialEquipmentMu.Unlock()
		if hasRuntimePVF {
			return dnfdungeonadmission.DailyResetResult{}, fmt.Errorf("daily dungeon admission catalog is unavailable")
		}
		return dnfdungeonadmission.DailyResetResult{}, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dnfdungeonadmission.DailyResetResult{}, nil
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return dnfdungeonadmission.DailyResetResult{}, err
	}
	invitations, err := s.currentDailyDungeonInvitations()
	if err != nil {
		return dnfdungeonadmission.DailyResetResult{}, err
	}
	result, err := owner.ReconcileDailyReset(
		ctx,
		strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		admissionCatalog.Policies(),
		invitations,
		now.UTC(),
	)
	if err != nil {
		return dnfdungeonadmission.DailyResetResult{}, fmt.Errorf("reconcile daily dungeon selection: %w", err)
	}
	return result, nil
}

func (s *Service) currentDailyDungeonInvitations() ([]dnfdungeonadmission.DailyInvitation, error) {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, fmt.Errorf("load daily dungeon invitation catalog: %w", err)
	}
	type expectedInvitation struct {
		itemID int64
		path   string
	}
	expected := []expectedInvitation{
		{itemID: currentTowerOfDespairInvitationItemID, path: currentTowerOfDespairInvitationPVFPath},
		{itemID: currentTowerOfGriefInvitationItemID, path: currentTowerOfGriefInvitationPVFPath},
	}
	result := make([]dnfdungeonadmission.DailyInvitation, 0, len(expected))
	for _, want := range expected {
		definition, resolveErr := catalog.ResolveItem(uint32(want.itemID))
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve daily dungeon invitation item=%d: %w", want.itemID, resolveErr)
		}
		actualPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
		if definition.Kind != dungeonDropItemStackable ||
			actualPath != strings.ToLower(want.path) ||
			normalizeDungeonDropStackableType(definition.StackableType) != "[material]" ||
			definition.SlotStart != 121 || definition.SlotEnd != 176 {
			return nil, fmt.Errorf(
				"daily dungeon invitation contract mismatch item=%d kind=%s path=%q type=%q slots=%d..%d",
				want.itemID, definition.Kind, definition.PVFPath, definition.StackableType, definition.SlotStart, definition.SlotEnd,
			)
		}
		result = append(result, dnfdungeonadmission.DailyInvitation{
			ItemID:         want.itemID,
			Minimum:        1,
			StackLimit:     definition.StackLimit,
			ListType:       dnfrepo.MainInventoryListType,
			CheckSlotStart: currentDailyInvitationCheckSlotStart,
			CheckSlotEnd:   currentDailyInvitationQuestSlotEnd,
			SlotStart:      currentDailyInvitationQuestSlotStart,
			SlotEnd:        currentDailyInvitationQuestSlotEnd,
			PVFPath:        definition.PVFPath,
			StackableType:  definition.StackableType,
		})
	}
	return result, nil
}

func currentDailyDungeonInvitationRefreshes(result dnfdungeonadmission.DailyResetResult) []alignedcmd.ItemSlotRefresh {
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, 2)
	for _, invitation := range result.Invitations {
		for _, slot := range invitation.ChangedSlots {
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
				ListType:  invitation.Invitation.ListType,
				SlotIndex: slot,
			})
		}
	}
	return refreshes
}
