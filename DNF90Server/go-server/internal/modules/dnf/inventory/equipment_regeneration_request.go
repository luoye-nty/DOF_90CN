package inventory

import (
	"encoding/binary"
	"fmt"
)

// EquipmentRegenerationRequest is current NoPack class1/op407. IDA sender
// sub_15D9460 writes exactly i16 source slot, u16 mode and u16 choose-part.
type EquipmentRegenerationRequest struct {
	SourceSlotIndex int16
	Mode            uint16
	Part            uint16
}

func DecodeEquipmentRegenerationRequest(body []byte) (EquipmentRegenerationRequest, error) {
	if len(body) != 6 {
		return EquipmentRegenerationRequest{}, fmt.Errorf("equipment regeneration body length %d, want 6", len(body))
	}
	return EquipmentRegenerationRequest{
		SourceSlotIndex: int16(binary.LittleEndian.Uint16(body[0:2])),
		Mode:            binary.LittleEndian.Uint16(body[2:4]),
		Part:            binary.LittleEndian.Uint16(body[4:6]),
	}, nil
}
