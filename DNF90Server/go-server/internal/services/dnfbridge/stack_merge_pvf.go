package dnfbridge

import (
	"fmt"
	"math"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func (s *Service) alignedStackMergeResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.StackMergeResolver, error) {
	if opcode != dnfenum.CmdPacketMoveItemspace && opcode != dnfenum.CmdPacketUpgradeItem {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	return func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		if itemID <= 0 || itemID > math.MaxUint32 {
			return alignedcmd.StackMergeResolution{}, fmt.Errorf("invalid stack merge item id %d", itemID)
		}
		definition, resolveErr := catalog.ResolveItem(uint32(itemID))
		if resolveErr != nil {
			return alignedcmd.StackMergeResolution{}, resolveErr
		}
		return alignedcmd.StackMergeResolution{
			ItemID:        int64(definition.ItemID),
			Kind:          string(definition.Kind),
			PVFPath:       strings.ReplaceAll(definition.PVFPath, "\\", "/"),
			AttachType:    definition.AttachType,
			EquipmentType: definition.EquipmentType,
			StackableType: definition.StackableType,
			StackLimit:    definition.StackLimit,
			SlotStart:     definition.SlotStart,
			SlotEnd:       definition.SlotEnd,
		}, nil
	}, nil
}
