package dnfbridge

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/avatartitle"
)

const (
	currentAvatarCompoundRequestSize = 22
	currentAvatarCompoundErrorCode   = 0x16
)

var (
	errCurrentAvatarCompoundRequestInvalid = errors.New("dnf current avatar compound request is invalid")
	errCurrentAvatarCompoundUnavailable    = errors.New("dnf current avatar compound owner or PVF mapping is unavailable")
	errCurrentAvatarCompoundSourceInvalid  = errors.New("dnf current avatar compound source is invalid")
	errCurrentAvatarCompoundMaterial       = errors.New("dnf current avatar compound synthesizer is invalid")
	errCurrentAvatarCompoundOutputInvalid  = errors.New("dnf current avatar compound output is invalid")
)

type currentAvatarCompoundRequest struct {
	ConsumeSlot       int16
	Slot1             int16
	ExpectedItemID1   uint32
	Slot2             int16
	ExpectedItemID2   uint32
	RequestedItemID   uint32
	AbilityNo         uint16
	RequestedItemFlag byte
	MaterialClass     byte
}

type currentAvatarCompoundResult struct {
	MaterialItemID uint32
	MaterialRemain int64
	SourceItemIDs  [2]uint32
	NewSlots       []int16
	NewItemIDs     []uint32
	RareRate       int64
	RareRoll       uint64
	RareHit        bool
}

func parseCurrentAvatarCompoundRequest(body []byte) (currentAvatarCompoundRequest, error) {
	if len(body) != currentAvatarCompoundRequestSize {
		return currentAvatarCompoundRequest{}, fmt.Errorf("%w: body=%d", errCurrentAvatarCompoundRequestInvalid, len(body))
	}
	decoded, err := avatartitle.DecodeCompoundAvatarRequest(body)
	if err != nil {
		return currentAvatarCompoundRequest{}, errors.Join(errCurrentAvatarCompoundRequestInvalid, err)
	}
	if decoded.ConsumeSlot < 0 || decoded.Slot1 < 0 || decoded.Slot2 < 0 || decoded.Slot1 == decoded.Slot2 ||
		decoded.ExpectedItemID1 <= 0 || decoded.ExpectedItemID2 <= 0 || decoded.RequestItemID <= 0 ||
		decoded.RequestedItemFlag > 1 {
		return currentAvatarCompoundRequest{}, errCurrentAvatarCompoundRequestInvalid
	}
	return currentAvatarCompoundRequest{
		ConsumeSlot:       decoded.ConsumeSlot,
		Slot1:             decoded.Slot1,
		ExpectedItemID1:   uint32(decoded.ExpectedItemID1),
		Slot2:             decoded.Slot2,
		ExpectedItemID2:   uint32(decoded.ExpectedItemID2),
		RequestedItemID:   uint32(decoded.RequestItemID),
		AbilityNo:         decoded.AbilityNo,
		RequestedItemFlag: decoded.RequestedItemFlag,
		MaterialClass:     decoded.MaterialClass,
	}, nil
}

// buildCurrentAvatarCompoundSuccessBody is the exact class-1/op99 body read
// by NoPack.exe sub_1D1C6D0. The leading byte is the common success status;
// it is followed by three deletion rows and exactly two result records.
func buildCurrentAvatarCompoundSuccessBody(request currentAvatarCompoundRequest, result currentAvatarCompoundResult) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeByte(3)
	body.writeByte(currentAvatarInventoryListType)
	body.writeUint16(uint16(request.Slot1))
	body.writeUint32(1)
	body.writeByte(currentAvatarInventoryListType)
	body.writeUint16(uint16(request.Slot2))
	body.writeUint32(1)
	body.writeByte(0)
	body.writeUint16(uint16(request.ConsumeSlot))
	body.writeUint32(1)
	for index := 0; index < 2; index++ {
		if index < len(result.NewItemIDs) && index < len(result.NewSlots) {
			body.writeUint16(uint16(result.NewSlots[index]))
			body.writeUint32(result.NewItemIDs[index])
		} else {
			body.writeUint16(^uint16(0))
			body.writeUint32(0)
		}
		body.writeUint32(0)
		body.writeUint16(request.AbilityNo)
		body.writeUint32(30)
		body.writeZero(30)
		body.writeUint32(4)
		body.writeZero(4)
	}
	return body.bytes()
}

func currentAvatarCompoundFailureBody() []byte {
	// The current op99 sender/reference includes the third zero tail. The
	// class-1 dispatcher consumes status/error and safely ignores the tail.
	return []byte{0, currentAvatarCompoundErrorCode, 0}
}
