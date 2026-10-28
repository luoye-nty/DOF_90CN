package inventory

import (
	"fmt"
	"math"
)

// UnsealRandomOptionRequest is current NoPack class1/op401:
// u16 client collection key + u16 inventory-manager state.
type UnsealRandomOptionRequest struct {
	TargetSlotIndex       int16
	InventoryManagerState uint16
}

// ChangeRandomOptionRequest is current NoPack class1/op437:
// u16 client collection key + literal u16 zero + u8 zero-based option index.
type ChangeRandomOptionRequest struct {
	TargetSlotIndex int16
	Reserved        uint16
	OptionIndex     byte
}

// ResetRandomOptionRequest is current NoPack class1/op455 (装备品级调整箱调整
// 封印属性品级): u16 target collection key + u16 material (adjustment box)
// collection key + u16 zero-based seal option index, which is the attribute the
// client's dropdown selected. Live 90 traffic is
// "3100 5300 0000" -> target 49, material 83, index 0.
type ResetRandomOptionRequest struct {
	TargetSlotIndex   int16
	MaterialSlotIndex int16
	OptionIndex       byte
}

func DecodeUnsealRandomOptionRequest(body []byte) (UnsealRandomOptionRequest, error) {
	if len(body) != 4 {
		return UnsealRandomOptionRequest{}, fmt.Errorf("unseal random option body length %d, want 4", len(body))
	}
	slot := uint16(readI16(body, 0))
	if slot > math.MaxInt16 {
		return UnsealRandomOptionRequest{}, fmt.Errorf("unseal random option collection key %d exceeds main inventory range", slot)
	}
	return UnsealRandomOptionRequest{TargetSlotIndex: int16(slot), InventoryManagerState: uint16(readI16(body, 2))}, nil
}

func DecodeChangeRandomOptionRequest(body []byte) (ChangeRandomOptionRequest, error) {
	if len(body) != 5 {
		return ChangeRandomOptionRequest{}, fmt.Errorf("change random option body length %d, want 5", len(body))
	}
	slot := uint16(readI16(body, 0))
	if slot > math.MaxInt16 {
		return ChangeRandomOptionRequest{}, fmt.Errorf("change random option collection key %d exceeds main inventory range", slot)
	}
	reserved := uint16(readI16(body, 2))
	if reserved != 0 {
		return ChangeRandomOptionRequest{}, fmt.Errorf("change random option reserved value 0x%04X, want 0", reserved)
	}
	return ChangeRandomOptionRequest{TargetSlotIndex: int16(slot), Reserved: reserved, OptionIndex: body[4]}, nil
}

// DecodeResetRandomOptionRequest parses the six-byte op455 body. The tracked
// live packet is exactly six bytes; a longer body is accepted because the
// client zero-pads its trailing state, but the first six bytes are authoritative.
func DecodeResetRandomOptionRequest(body []byte) (ResetRandomOptionRequest, error) {
	if len(body) < 6 {
		return ResetRandomOptionRequest{}, fmt.Errorf("reset random option body length %d, want at least 6", len(body))
	}
	target := uint16(readI16(body, 0))
	if target > math.MaxInt16 {
		return ResetRandomOptionRequest{}, fmt.Errorf("reset random option target key %d exceeds main inventory range", target)
	}
	material := uint16(readI16(body, 2))
	if material > math.MaxInt16 {
		return ResetRandomOptionRequest{}, fmt.Errorf("reset random option material key %d exceeds main inventory range", material)
	}
	optionIndex := uint16(readI16(body, 4))
	if optionIndex > randomOptionMaximumCount-1 {
		return ResetRandomOptionRequest{}, fmt.Errorf("reset random option index %d outside 0..%d", optionIndex, randomOptionMaximumCount-1)
	}
	return ResetRandomOptionRequest{
		TargetSlotIndex:   int16(target),
		MaterialSlotIndex: int16(material),
		OptionIndex:       byte(optionIndex),
	}, nil
}
