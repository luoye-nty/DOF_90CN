package inventory

import (
	"encoding/binary"
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

func buildChronicleGrowthErrorAck(code byte) []byte { return []byte{0, code} }

func buildChronicleGrowthSuccessAck(result ChronicleGrowthResult) []byte {
	body := make([]byte, 3+len(result.Consumptions)*7)
	body[0] = 1
	if result.Succeeded {
		body[1] = 1
	}
	body[2] = byte(len(result.Consumptions))
	offset := 3
	for _, item := range result.Consumptions {
		body[offset] = item.ListType
		binary.LittleEndian.PutUint16(body[offset+1:offset+3], uint16(item.Slot))
		binary.LittleEndian.PutUint32(body[offset+3:offset+7], uint32(item.Count))
		offset += 7
	}
	return body
}

func ownerAppliedChronicleGrowthResult(opcode uint16, cmd Command, result ChronicleGrowthResult) alignedcmd.Result {
	if result.Conversion {
		refreshes := make([]alignedcmd.ItemSlotRefresh, 0, 1+len(result.Consumptions))
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: result.TargetSlot})
		for _, item := range result.Consumptions {
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: item.ListType, SlotIndex: item.Slot})
		}
		return alignedcmd.Result{
			Handled:         true,
			ResponseAllowed: true,
			Operation:       cmd.Operation,
			Reason: fmt.Sprintf("equipment emancipation applied: target=%d source=%d output=%d ticket=%d consumed_rows=%d",
				cmd.ChronicleTargetSlot, cmd.ChronicleTargetID, result.OutputItemID, cmd.ChronicleTicketID, len(result.Consumptions)),
			UpperResponses:             []alignedcmd.UpperResponse{class1Response(opcode, buildChronicleGrowthSuccessAck(result))},
			PostResponseItemSlotClears: []alignedcmd.ItemSlotRefresh{{ListType: listTypeMain, SlotIndex: result.TargetSlot}},
			ItemSlotRefreshes:          refreshes,
		}
	}
	return alignedcmd.Result{
		Handled:         true,
		ResponseAllowed: true,
		Operation:       cmd.Operation,
		Reason: fmt.Sprintf("chronicle growth applied: target=%d level=%d->%d ticket=%d material=%d count=%d",
			cmd.ChronicleTargetID, result.OldLevel, result.NewLevel, cmd.ChronicleTicketID, cmd.ChronicleMaterialID, result.MaterialCount),
		// The current client resolves the target immediately from its inventory
		// after the ACK. Publish only that updated row before the ACK; the ACK's
		// consumed rows are the authoritative ticket/material decrement.
		PreResponseItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{ListType: listTypeMain, SlotIndex: result.TargetSlot}},
		UpperResponses:               []alignedcmd.UpperResponse{class1Response(opcode, buildChronicleGrowthSuccessAck(result))},
	}
}
