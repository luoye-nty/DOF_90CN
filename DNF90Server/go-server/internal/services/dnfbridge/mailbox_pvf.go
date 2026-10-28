package dnfbridge

import (
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfepicbook "longheng.io/server/internal/modules/dnf/epicbook"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) alignedMailboxItemResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.MailboxItemResolver, error) {
	needsPrice := opcode == dnfenum.CmdPacketMailboxSend || opcode == dnfenum.CmdPacketMultiMailboxSend
	if !needsPrice && opcode != dnfenum.CmdPacketMailboxExtractItem {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	var epicBookCatalog *dnfepicbook.Catalog
	if opcode == dnfenum.CmdPacketMailboxExtractItem {
		epicBookCatalog, err = s.currentEpicBookCatalog()
		if err != nil {
			return nil, err
		}
	}
	return func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			return alignedcmd.MailboxItemResolution{}, resolveErr
		}
		resolution, resolveErr := mailboxItemResolutionFromPVF(definition, needsPrice)
		if resolveErr != nil {
			return alignedcmd.MailboxItemResolution{}, resolveErr
		}
		if epicBookCatalog != nil {
			if entry, ok := epicBookCatalog.EntryByFragment(itemID); ok {
				resolution.EpicBookEquipmentID = entry.EquipmentID
				resolution.EpicBookFragmentID = entry.FragmentID
			}
		}
		return resolution, nil
	}, nil
}

func mailboxItemResolutionFromPVF(definition dungeonDropItemDefinition, needsPrice bool) (alignedcmd.MailboxItemResolution, error) {
	// A listed, successfully parsed PVF item remains authoritative even when
	// its document omits [price]. The current composer accepts those items and
	// displays only the fixed attachment postage, so an absent price contributes
	// zero value instead of invalidating the durable attachment.
	if needsPrice && definition.Price < 0 {
		return alignedcmd.MailboxItemResolution{}, fmt.Errorf("mailbox item %d has invalid negative runtime-PVF price", definition.ItemID)
	}
	stackLimit := definition.StackLimit
	if definition.Kind == dungeonDropItemEquipment {
		stackLimit = 1
	}
	listType, slotStart, slotEnd, err := currentMailboxPVFPlacement(definition)
	if err != nil {
		return alignedcmd.MailboxItemResolution{}, err
	}
	return alignedcmd.MailboxItemResolution{
		Price:            definition.Price,
		PVFPath:          definition.PVFPath,
		Kind:             string(definition.Kind),
		EquipmentType:    definition.EquipmentType,
		StackableType:    definition.StackableType,
		Durability:       definition.Durability,
		ListType:         listType,
		SlotStart:        slotStart,
		SlotEnd:          slotEnd,
		StackLimit:       stackLimit,
		PreferQuickSlots: definition.Kind == dungeonDropItemStackable && dungeonDropStackablePrefersItemQuickSlots(definition.StackableType),
	}, nil
}

// currentMailboxPVFPlacement returns the native inventory owner and page for
// an attachment.  Mail is only the transport; it must not turn every item
// into a list-0 object.  These are the same current-EXE/PVF category rules
// already used by Cera/booster grants.
func currentMailboxPVFPlacement(definition dungeonDropItemDefinition) (byte, int16, int16, error) {
	if fixed, ok := currentDisjointWarehouseFixedSlots[definition.ItemID]; ok {
		return currentEquipmentPlacementListMain, fixed, fixed, nil
	}

	switch definition.Kind {
	case dungeonDropItemStackable:
		normalized := normalizeDungeonDropStackableType(definition.StackableType)
		switch {
		case normalized == "[feed]":
			return currentPetInventoryListType, currentCeraShopPetConsumableSlotStart, currentCeraShopPetConsumableSlotEnd, nil
		case strings.HasPrefix(normalized, "[flag gem]"):
			return currentGuildMedalInventoryListType, currentGuardianGemPageSlotStart, currentGuardianGemPageSlotEnd, nil
		}
		start, end := definition.SlotStart, definition.SlotEnd
		// Slots 354..365 are fixed account-owned cube/soul cells. Other
		// [material] 4 items keep the preceding character-owned page and may
		// never consume one of those fixed cells merely because it arrived by
		// mail.
		if start < dnfrepo.CrystalWarehouseFirstSlot && end >= dnfrepo.CrystalWarehouseFirstSlot {
			end = dnfrepo.CrystalWarehouseFirstSlot - 1
		}
		return currentEquipmentPlacementListMain, start, end, nil

	case dungeonDropItemEquipment:
		normalized := normalizeEquipmentPlacementPVFType(definition.EquipmentType)
		if strings.Contains(normalized, "creature") {
			return currentPetInventoryListType, 0, currentBoosterPetArtifactStart - 1, nil
		}
		if strings.HasPrefix(strings.Trim(normalized, "[]"), "artifact ") {
			return currentPetInventoryListType, currentBoosterPetArtifactStart, currentBoosterPetArtifactEnd, nil
		}
		if rule, ok := currentEquipmentPlacementRuleForPVFType(definition.EquipmentType); ok {
			switch {
			case rule.class == currentEquipmentPlacementClassAvatar:
				return currentEquipmentPlacementListAvatar, 0, 500, nil
			case rule.pvfType == "[flag]":
				return currentGuildMedalInventoryListType, currentGuildMedalPageSlotStart, currentGuildMedalPageSlotEnd, nil
			}
		}
		return currentEquipmentPlacementListMain, definition.SlotStart, definition.SlotEnd, nil
	default:
		return 0, 0, 0, fmt.Errorf("mailbox item %d has unsupported PVF kind %q", definition.ItemID, definition.Kind)
	}
}
