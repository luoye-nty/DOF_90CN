package dnfbridge

import (
	"errors"
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// alignedUpgradeProtectionResolverForCommand keeps protection-ticket PVF
// access request-scoped. The normal op50 owner selects only the exact optional
// slot sent by the client; this resolver proves the stack's action family and
// its failure-retain level.
func (s *Service) alignedUpgradeProtectionResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.UpgradeProtectionResolver, error) {
	if opcode != dnfenum.CmdPacketUpgradeItem {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, errDungeonDropSourceRequired
	}
	s.initialEquipmentMu.Lock()
	source, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	return func(ticketItemID int64) (alignedcmd.UpgradeProtectionResolution, error) {
		return resolveCurrentUpgradeProtectionMetadata(catalog, source, ticketItemID)
	}, nil
}

func resolveCurrentUpgradeProtectionMetadata(catalog *pvfDungeonDropCatalog, source dnfpvf.Source, ticketItemID int64) (alignedcmd.UpgradeProtectionResolution, error) {
	if catalog == nil || source == nil {
		return alignedcmd.UpgradeProtectionResolution{}, errDungeonDropSourceRequired
	}
	resolution := alignedcmd.UpgradeProtectionResolution{}
	if ticketItemID <= 0 || ticketItemID > int64(^uint32(0)) {
		return resolution, nil
	}

	definition, err := catalog.ResolveItem(uint32(ticketItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return alignedcmd.UpgradeProtectionResolution{}, fmt.Errorf("resolve upgrade protection item=%d: %w", ticketItemID, err)
	}
	if definition.Kind != dungeonDropItemStackable {
		return resolution, nil
	}
	document, err := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if err != nil {
		return alignedcmd.UpgradeProtectionResolution{}, fmt.Errorf("parse upgrade protection item=%d path=%s: %w", ticketItemID, definition.PVFPath, err)
	}
	action, found := document.Text("action type")
	if !found {
		return resolution, nil
	}
	action = strings.ToLower(strings.TrimSpace(action))
	values := document.Ints("action type")
	switch action {
	case "[protect equipment]":
		resolution.TicketMode = "reinforce"
		resolution.FailureRetainLevel = 0
	case "[amplify protect equipment]":
		resolution.TicketMode = "amplify"
		resolution.FailureRetainLevel = 0
	case "[new protect equipment]":
		resolution.TicketMode = "reinforce"
		if len(values) != 1 || values[0] < 0 || values[0] > 30 {
			return alignedcmd.UpgradeProtectionResolution{}, fmt.Errorf("upgrade protection item=%d path=%s has invalid retain level %v", ticketItemID, definition.PVFPath, values)
		}
		resolution.FailureRetainLevel = int(values[0])
	case "[new amplify protect equipment]":
		resolution.TicketMode = "amplify"
		if len(values) != 1 || values[0] < 0 || values[0] > 30 {
			return alignedcmd.UpgradeProtectionResolution{}, fmt.Errorf("amplify protection item=%d path=%s has invalid retain level %v", ticketItemID, definition.PVFPath, values)
		}
		resolution.FailureRetainLevel = int(values[0])
	default:
		return resolution, nil
	}
	resolution.TicketPVFPath = definition.PVFPath
	return resolution, nil
}
