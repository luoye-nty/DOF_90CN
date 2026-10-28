package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfavatarfitting "longheng.io/server/internal/modules/dnf/avatarfitting"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type currentAvatarFittingRoomResult struct {
	Domain  dnfavatarfitting.Result
	Changed [currentAvatarFittingRoomPartCount]bool
}

func (s *Service) commitCurrentAvatarFittingRoom(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	request currentAvatarFittingRoomRequest,
) (currentAvatarFittingRoomResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || catalog == nil || catalog.source == nil {
		return currentAvatarFittingRoomResult{}, errCurrentAvatarFittingRoomUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.CharacterSettlement == nil {
		return currentAvatarFittingRoomResult{}, errCurrentAvatarFittingRoomUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if accountID == "" {
		return currentAvatarFittingRoomResult{}, errCurrentAvatarFittingRoomUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || strings.TrimSpace(character.AccountID) != accountID {
		return currentAvatarFittingRoomResult{}, errors.Join(errCurrentAvatarFittingRoomUnavailable, err)
	}
	job := numericCharacterStat(character.Job)
	if job < 0 || job > math.MaxUint8 {
		return currentAvatarFittingRoomResult{}, errCurrentAvatarFittingRoomUnavailable
	}
	config, err := loadCurrentAvatarFittingRoomConfig(catalog.source)
	if err != nil {
		return currentAvatarFittingRoomResult{}, err
	}
	selectedSet, err := currentAvatarFittingRoomSetForJob(config, byte(job))
	if err != nil {
		return currentAvatarFittingRoomResult{}, err
	}
	outputs := selectedSet.Jobs[byte(job)]
	command := dnfavatarfitting.Command{
		AccountID:      accountID,
		CharacterID:    characterID,
		InDungeon:      currentCrystalContractDungeonOwnedBySession(session),
		TicketItemID:   int64(config.TicketItemID),
		GaugeThreshold: config.GaugeThreshold,
	}
	var result currentAvatarFittingRoomResult
	for index, record := range request.Records {
		if record.ObjectIndex == -1 && record.CurrentItemID == -1 {
			command.Changes[index] = dnfavatarfitting.Change{
				EquipmentSlot: -1,
				ObjectIndex:   -1,
				CurrentItemID: -1,
			}
			continue
		}
		if record.ObjectIndex < 0 || record.CurrentItemID <= 0 || outputs[index] == 0 {
			return currentAvatarFittingRoomResult{}, fmt.Errorf("%w: part=%d", errCurrentAvatarFittingRoomRequestInvalid, index)
		}
		currentDefinition, currentDocument, resolveErr := currentAvatarCompoundDefinition(catalog, uint32(record.CurrentItemID))
		if resolveErr != nil {
			return currentAvatarFittingRoomResult{}, errors.Join(errCurrentAvatarFittingRoomEquipment, resolveErr)
		}
		currentPart, partOK := currentAvatarCompoundDocumentPart(currentDocument)
		if !partOK || currentPart != currentAvatarFittingRoomPartNames[index] {
			return currentAvatarFittingRoomResult{}, fmt.Errorf("%w: part=%d current=%s", errCurrentAvatarFittingRoomEquipment, index, currentPart)
		}
		outputDefinition, outputDocument, resolveErr := currentAvatarCompoundDefinition(catalog, outputs[index])
		if resolveErr != nil {
			return currentAvatarFittingRoomResult{}, errors.Join(errCurrentAvatarFittingRoomEquipment, resolveErr)
		}
		outputPart, partOK := currentAvatarCompoundDocumentPart(outputDocument)
		if !partOK || outputPart != currentAvatarFittingRoomPartNames[index] {
			return currentAvatarFittingRoomResult{}, fmt.Errorf("%w: part=%d output=%s item=%d", errCurrentAvatarFittingRoomEquipment, index, outputPart, outputs[index])
		}
		command.Changes[index] = dnfavatarfitting.Change{
			EquipmentSlot: currentAvatarFittingRoomEquipmentSlots[index],
			ObjectIndex:   record.ObjectIndex,
			CurrentItemID: int64(record.CurrentItemID),
			OutputItemID:  int64(outputs[index]),
			PVFPath:       outputDefinition.PVFPath,
			EquipmentType: outputDefinition.EquipmentType,
		}
		result.Changed[index] = true
		_ = currentDefinition
	}

	if config.GaugeThreshold > 0 && config.GaugeRewardID > 0 && config.GaugeRewardCount > 0 {
		rewardDefinition, resolveErr := catalog.ResolveItem(config.GaugeRewardID)
		if resolveErr != nil {
			return currentAvatarFittingRoomResult{}, errors.Join(errCurrentAvatarFittingRoomUnavailable, resolveErr)
		}
		rewardDefinition, resolveErr = currentPVFItemDefinitionForGrantAt(rewardDefinition, time.Now().UTC())
		if resolveErr != nil || rewardDefinition.Kind != dungeonDropItemStackable || rewardDefinition.SlotStart < 0 || rewardDefinition.SlotEnd < rewardDefinition.SlotStart {
			return currentAvatarFittingRoomResult{}, errors.Join(errCurrentAvatarFittingRoomUnavailable, resolveErr)
		}
		stackLimit := rewardDefinition.StackLimit
		if stackLimit <= 0 {
			stackLimit = math.MaxUint32
		}
		command.Reward = &dnfavatarfitting.Reward{
			ItemID:           int64(config.GaugeRewardID),
			CountPerCrossing: config.GaugeRewardCount,
			SlotStart:        rewardDefinition.SlotStart,
			SlotEnd:          rewardDefinition.SlotEnd,
			StackLimit:       stackLimit,
			BuildStack: func(slot int16, count int64) (dnfrepo.ItemStack, error) {
				if count <= 0 || count > math.MaxUint32 {
					return dnfrepo.ItemStack{}, errCurrentAvatarFittingRoomUnavailable
				}
				return buildCurrentDungeonPickupStack(slot, rewardDefinition, uint32(count))
			},
		}
	}
	owner, err := dnfavatarfitting.NewOwner(repositories)
	if err != nil {
		return currentAvatarFittingRoomResult{}, errors.Join(errCurrentAvatarFittingRoomUnavailable, err)
	}
	result.Domain, err = owner.Commit(ctx, command)
	if err != nil {
		return currentAvatarFittingRoomResult{}, err
	}
	return result, nil
}
