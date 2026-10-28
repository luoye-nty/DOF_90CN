package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const currentAvatarOptionChangeRequestSize = 13

var (
	errCurrentAvatarOptionChangeRequestInvalid = errors.New("dnf current avatar option change request is invalid")
	errCurrentAvatarOptionChangeUnavailable    = errors.New("dnf current avatar option change owner or PVF mapping is unavailable")
	errCurrentAvatarOptionChangeToolInvalid    = errors.New("dnf current avatar option change tool is invalid")
	errCurrentAvatarOptionChangeTargetInvalid  = errors.New("dnf current avatar option change target is invalid")
)

// currentAvatarOptionChangeRequest is the current client's class-1/op459
// body. The trailing ability selector is one byte in this client build; the
// class-1/op459 result echoes the same byte after the selected avatar slot.
type currentAvatarOptionChangeRequest struct {
	SourceSlot   int16
	SourceItemID uint32
	TargetSlot   int16
	TargetItemID uint32
	AbilityNo    byte
}

type currentAvatarOptionChangeResult struct {
	SourceRemaining int64
}

func parseCurrentAvatarOptionChangeRequest(body []byte) (currentAvatarOptionChangeRequest, error) {
	if len(body) != currentAvatarOptionChangeRequestSize {
		return currentAvatarOptionChangeRequest{}, fmt.Errorf("%w: body=%d", errCurrentAvatarOptionChangeRequestInvalid, len(body))
	}
	sourceSlot := binary.LittleEndian.Uint16(body[0:2])
	sourceItemID := binary.LittleEndian.Uint32(body[2:6])
	targetSlot := binary.LittleEndian.Uint16(body[6:8])
	targetItemID := binary.LittleEndian.Uint32(body[8:12])
	if sourceSlot > 0x7fff || targetSlot > 0x7fff || sourceItemID == 0 || targetItemID == 0 {
		return currentAvatarOptionChangeRequest{}, errCurrentAvatarOptionChangeRequestInvalid
	}
	return currentAvatarOptionChangeRequest{
		SourceSlot:   int16(sourceSlot),
		SourceItemID: sourceItemID,
		TargetSlot:   int16(targetSlot),
		TargetItemID: targetItemID,
		AbilityNo:    body[12],
	}, nil
}

func buildCurrentAvatarOptionChangeSuccessBody(request currentAvatarOptionChangeRequest) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeUint16(uint16(request.TargetSlot))
	body.writeByte(request.AbilityNo)
	return body.bytes()
}

func currentAvatarOptionChangeFailureBody() []byte {
	// The native class-1 generic failure reader consumes an opaque error byte
	// after success=0. Its business mapping is not closed, so keep it zero.
	return []byte{0, 0}
}
