package dnfbridge

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
)

const (
	currentNormalVendingMachineID   uint32 = 1
	currentAdvancedVendingMachineID uint32 = 3
	currentVendingMachineGroupID    uint32 = 1
	currentVendingMachineError             = byte(4)
)

var (
	errCurrentVendingMachineRequestMalformed = errors.New("dnf current vending-machine request is malformed")
	errCurrentVendingMachineRollInvalid      = errors.New("dnf current vending-machine roll is invalid")
)

// The current EXE writes exactly machine kind (one-based), draw count, and
// the main-inventory slot containing the PVF material. The two live machines
// use kinds 1 and 3; both issue one draw against item-group 1.
type currentVendingMachineRequest struct {
	MachineID    uint32
	DrawCount    uint32
	MaterialSlot int16
}

func parseCurrentVendingMachineRequest(body []byte) (currentVendingMachineRequest, error) {
	if len(body) != 10 {
		return currentVendingMachineRequest{}, errCurrentVendingMachineRequestMalformed
	}
	machineID := binary.LittleEndian.Uint32(body[0:4])
	drawCount := binary.LittleEndian.Uint32(body[4:8])
	slot := binary.LittleEndian.Uint16(body[8:10])
	if (machineID != currentNormalVendingMachineID && machineID != currentAdvancedVendingMachineID) ||
		drawCount != 1 || slot > math.MaxInt16 {
		return currentVendingMachineRequest{}, fmt.Errorf(
			"%w: machine=%d draws=%d material_slot=%d",
			errCurrentVendingMachineRequestMalformed,
			machineID,
			drawCount,
			slot,
		)
	}
	return currentVendingMachineRequest{
		MachineID:    machineID,
		DrawCount:    drawCount,
		MaterialSlot: int16(slot),
	}, nil
}

func secureCurrentVendingMachineRoll(limit uint64) (uint64, error) {
	if limit == 0 {
		return 0, errCurrentVendingMachineRollInvalid
	}
	n, err := cryptorand.Int(cryptorand.Reader, new(big.Int).SetUint64(limit))
	if err != nil {
		return 0, err
	}
	return n.Uint64(), nil
}

func (s *Service) rollCurrentVendingMachineOutput(group currentVendingMachineGroup) (currentVendingMachineOutput, error) {
	if group.TotalWeight == 0 || len(group.Outputs) == 0 {
		return currentVendingMachineOutput{}, errCurrentVendingMachineRollInvalid
	}
	roll := secureCurrentVendingMachineRoll
	if s != nil && s.vendingMachineRoll != nil {
		roll = s.vendingMachineRoll
	}
	value, err := roll(group.TotalWeight)
	if err != nil || value >= group.TotalWeight {
		if err == nil {
			err = fmt.Errorf("%w: value=%d total=%d", errCurrentVendingMachineRollInvalid, value, group.TotalWeight)
		}
		return currentVendingMachineOutput{}, err
	}
	for _, output := range group.Outputs {
		if output.Weight == 0 {
			continue
		}
		if value < output.Weight {
			return output, nil
		}
		value -= output.Weight
	}
	return currentVendingMachineOutput{}, errCurrentVendingMachineRollInvalid
}

type currentVendingMachineRewardRow struct {
	ListType byte
	Entry    currentItemListEntry
}

type currentVendingMachineSuccess struct {
	MaterialSlot      int16
	MaterialRemaining uint32
	RewardItemID      uint32
	RewardCount       uint32
	RewardRows        []currentVendingMachineRewardRow
}

func buildCurrentVendingMachineFailureBody(code byte) []byte {
	return []byte{0, code}
}

func buildCurrentVendingMachineSuccessBody(result currentVendingMachineSuccess) ([]byte, error) {
	if result.MaterialSlot < 0 || result.RewardItemID == 0 || result.RewardCount == 0 ||
		len(result.RewardRows) == 0 || len(result.RewardRows) > math.MaxUint16 {
		return nil, errCurrentVendingMachineRequestMalformed
	}
	var body packetWriter
	body.writeByte(1)
	body.writeUint16(uint16(result.MaterialSlot))
	body.writeUint32(result.MaterialRemaining)
	body.writeUint32(result.RewardItemID)
	body.writeUint32(result.RewardCount)
	// Current NoPack reads these two legacy fields and discards both values.
	body.writeUint32(0)
	body.writeUint32(0)
	body.writeUint16(uint16(len(result.RewardRows)))
	for _, row := range result.RewardRows {
		if row.ListType != 0 ||
			binary.LittleEndian.Uint32(row.Entry.data[2:6]) != result.RewardItemID ||
			binary.LittleEndian.Uint32(row.Entry.data[6:10]) == 0 {
			return nil, fmt.Errorf(
				"%w: reward row list=%d item=%d count=%d",
				errCurrentVendingMachineRequestMalformed,
				row.ListType,
				binary.LittleEndian.Uint32(row.Entry.data[2:6]),
				binary.LittleEndian.Uint32(row.Entry.data[6:10]),
			)
		}
		body.writeByte(row.ListType)
		body.writeBytes(row.Entry.data[:])
	}
	return body.bytes(), nil
}
