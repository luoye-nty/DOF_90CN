package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentPetSealOpcode uint16 = uint16(dnfenum.CmdPacketSealCreature)

var currentPetSealCapsuleIDs = map[int64]struct{}{
	2683240: {},
	2683241: {},
	2683804: {},
}

type currentPetSealRequest struct {
	TargetSlot   int16
	TargetItemID uint32
	MaterialSlot int16
}

type currentPetSealResult struct {
	TargetSlot        int16
	EggStack          dnfrepo.ItemStack
	MaterialSlot      int16
	MaterialStack     dnfrepo.ItemStack
	MaterialRemaining int64
}

func decodeCurrentPetSealRequest(body []byte) (currentPetSealRequest, error) {
	if len(body) != 21 {
		return currentPetSealRequest{}, fmt.Errorf("pet seal body length=%d want=21", len(body))
	}
	return currentPetSealRequest{
		TargetSlot:   int16(binary.LittleEndian.Uint16(body[13:15])),
		TargetItemID: binary.LittleEndian.Uint32(body[15:19]),
		MaterialSlot: int16(binary.LittleEndian.Uint16(body[19:21])),
	}, nil
}

func (s *Service) handleCurrentPetSeal(session *gameSession, body []byte) error {
	request, err := decodeCurrentPetSealRequest(body)
	if err != nil || request.TargetSlot < 0 || request.TargetSlot > 139 || request.MaterialSlot < 0 || request.TargetItemID == 0 {
		return s.sendGameUpperFailure(session, currentPetSealOpcode, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterPets == nil {
		return s.sendGameUpperFailure(session, currentPetSealOpcode, 2)
	}
	itemCatalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, currentPetSealOpcode, 2)
	}
	petCatalog, err := s.currentPetPVFCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, currentPetSealOpcode, 2)
	}
	characterID := selectedCharacterIDString(session)
	var committed currentPetSealResult
	err = dnfrepo.WithinCharacterPetsScoped(ctx, repositories.CharacterPets, characterID, func(inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository, pets dnfrepo.PetRepository) error {
		inventory, found, loadErr := inventories.Load(ctx, characterID)
		if loadErr != nil || !found {
			return firstNonNilError(loadErr, errors.New("pet seal inventory missing"))
		}
		petRecord, found, loadErr := pets.Load(ctx, characterID)
		if loadErr != nil || !found {
			return firstNonNilError(loadErr, errors.New("pet seal durable state missing"))
		}
		inventory = dnfrepo.CloneInventory(inventory)
		petRecord = dnfrepo.ClonePet(petRecord)

		materialKey := currentMainInventorySlotKey(request.MaterialSlot)
		material, found := inventory.Slots[materialKey]
		if !found || material.Count <= 0 {
			return errors.New("pet seal capsule missing")
		}
		if _, supported := currentPetSealCapsuleIDs[material.ItemID]; !supported {
			return errors.New("pet seal capsule unsupported")
		}
		if material.ItemID <= 0 || material.ItemID > math.MaxUint32 {
			return errors.New("pet seal capsule id invalid")
		}
		materialDefinition, resolveErr := itemCatalog.ResolveItem(uint32(material.ItemID))
		if resolveErr != nil || materialDefinition.Kind != dungeonDropItemStackable {
			return errors.New("pet seal capsule PVF mismatch")
		}

		targetKey := fmt.Sprintf("7:%d", request.TargetSlot)
		target, found := inventory.Slots[targetKey]
		if !found || target.ItemID != int64(request.TargetItemID) || target.Count != 1 {
			return errors.New("pet seal target mismatch")
		}
		var petKey string
		var petEntry dnfrepo.PetEntry
		for key, entry := range petRecord.Entries {
			if entry.SourceListType == 7 && entry.SourceSlotIndex == request.TargetSlot && entry.ItemID == target.ItemID {
				petKey, petEntry = key, entry
				break
			}
		}
		if petKey == "" || petRecord.EquippedKey == petKey {
			return errors.New("pet seal target missing or equipped")
		}
		eggItemID, parseErr := currentHatchedEggItemID(target, petEntry)
		if parseErr != nil {
			return parseErr
		}
		hatch, resolveErr := petCatalog.ResolveHatch(eggItemID)
		if resolveErr != nil || hatch.HatchedItemID != target.ItemID {
			return errors.New("pet seal hatch mapping mismatch")
		}
		eggDefinition, resolveErr := itemCatalog.ResolveItem(uint32(eggItemID))
		if resolveErr != nil || eggDefinition.Kind != dungeonDropItemEquipment {
			return errors.New("pet seal egg PVF mismatch")
		}

		egg := dnfrepo.ItemStack{
			ItemID:   eggItemID,
			Count:    1,
			Bind:     target.Bind,
			ExpireAt: target.ExpireAt,
			Extra: map[string]string{
				"pet_sealed_from_item_id": strconv.FormatInt(target.ItemID, 10),
			},
		}
		inventory.Slots[targetKey] = egg
		delete(petRecord.Entries, petKey)
		material.Count--
		if material.Count == 0 {
			delete(inventory.Slots, materialKey)
			material = dnfrepo.ItemStack{}
		} else {
			inventory.Slots[materialKey] = material
		}
		now := s.gameplayNow().UTC()
		inventory.UpdatedAt = now
		petRecord.UpdatedAt = now
		if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		if saveErr := dnfrepo.SavePetFields(ctx, pets, petRecord, dnfrepo.PetFieldEntries); saveErr != nil {
			return saveErr
		}
		committed = currentPetSealResult{
			TargetSlot: request.TargetSlot, EggStack: egg, MaterialSlot: request.MaterialSlot,
			MaterialStack: material, MaterialRemaining: material.Count,
		}
		return nil
	})
	if err != nil {
		s.logGameEvent(session, "game-pet-seal-blocked", "target_slot", request.TargetSlot, "target_item_id", request.TargetItemID, "material_slot", request.MaterialSlot, "err", err)
		return s.sendGameUpperFailure(session, currentPetSealOpcode, 3)
	}

	var response packetWriter
	response.writeUint32(0)
	response.writeUint32(uint32(uint16(committed.TargetSlot)) << 16)
	response.writeUint16(uint16(committed.MaterialSlot))
	response.writeUint32(uint32(committed.MaterialRemaining))
	if err := s.sendGameUpperSuccess(session, currentPetSealOpcode, response.bytes()); err != nil {
		return err
	}
	materialUpdate := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, committed.MaterialSlot, committed.MaterialStack)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{materialUpdate}), 0); err != nil {
		return err
	}
	eggUpdate := currentItemListEntryFromStack(7, committed.TargetSlot, committed.EggStack)
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(7, []currentItemListEntry{eggUpdate}), 0)
}

func currentHatchedEggItemID(stack dnfrepo.ItemStack, entry dnfrepo.PetEntry) (int64, error) {
	values := []string{stack.Extra["hatched_from_item_id"], entry.Extra["hatched_from_item_id"]}
	var resolved int64
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 || value > math.MaxUint32 {
			return 0, errors.New("pet seal egg metadata invalid")
		}
		if resolved != 0 && resolved != value {
			return 0, errors.New("pet seal egg metadata conflict")
		}
		resolved = value
	}
	if resolved == 0 {
		return 0, errors.New("pet seal egg metadata missing")
	}
	return resolved, nil
}
