package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	currentAvatarFittingRoomPartCount   = 8
	currentAvatarFittingRoomRecordSize  = 13
	currentAvatarFittingRoomPrefixSize  = 13
	currentAvatarFittingRoomRequestSize = currentAvatarFittingRoomPrefixSize + currentAvatarFittingRoomPartCount*currentAvatarFittingRoomRecordSize
)

var (
	errCurrentAvatarFittingRoomRequestInvalid = errors.New("dnf current avatar fitting room request is invalid")
	errCurrentAvatarFittingRoomUnavailable    = errors.New("dnf current avatar fitting room is unavailable")
	errCurrentAvatarFittingRoomEquipment      = errors.New("dnf current avatar fitting room equipment is invalid")
)

type currentAvatarFittingRoomRecord struct {
	Raw           [currentAvatarFittingRoomRecordSize]byte
	ObjectIndex   int32
	CurrentItemID int32
	PartCode      byte
}

type currentAvatarFittingRoomRequest struct {
	Prefix  [currentAvatarFittingRoomPrefixSize]byte
	Records [currentAvatarFittingRoomPartCount]currentAvatarFittingRoomRecord
}

func parseCurrentAvatarFittingRoomRequest(body []byte) (currentAvatarFittingRoomRequest, error) {
	if len(body) != currentAvatarFittingRoomRequestSize {
		return currentAvatarFittingRoomRequest{}, fmt.Errorf("%w: body=%d", errCurrentAvatarFittingRoomRequestInvalid, len(body))
	}
	var request currentAvatarFittingRoomRequest
	copy(request.Prefix[:], body[:currentAvatarFittingRoomPrefixSize])
	for index := 0; index < currentAvatarFittingRoomPartCount; index++ {
		offset := currentAvatarFittingRoomPrefixSize + index*currentAvatarFittingRoomRecordSize
		record := body[offset : offset+currentAvatarFittingRoomRecordSize]
		copy(request.Records[index].Raw[:], record)
		request.Records[index].ObjectIndex = int32(binary.LittleEndian.Uint32(record[0:4]))
		request.Records[index].CurrentItemID = int32(binary.LittleEndian.Uint32(record[4:8]))
		request.Records[index].PartCode = record[8]
		if request.Records[index].ObjectIndex < -1 || request.Records[index].CurrentItemID < -1 || request.Records[index].PartCode > 7 {
			return currentAvatarFittingRoomRequest{}, fmt.Errorf("%w: part=%d object=%d item=%d code=%d", errCurrentAvatarFittingRoomRequestInvalid, index, request.Records[index].ObjectIndex, request.Records[index].CurrentItemID, request.Records[index].PartCode)
		}
		if (request.Records[index].ObjectIndex == -1) != (request.Records[index].CurrentItemID == -1) {
			return currentAvatarFittingRoomRequest{}, fmt.Errorf("%w: partial absent part=%d", errCurrentAvatarFittingRoomRequestInvalid, index)
		}
	}
	return request, nil
}

// The current EXE applies op1488 only after the corresponding equipment
// objects exist. Each changed row therefore echoes the original 13-byte
// object reference and sets the trailing changed marker to one.
func buildCurrentAvatarFittingRoomSuccessBody(request currentAvatarFittingRoomRequest, changed [currentAvatarFittingRoomPartCount]bool) []byte {
	body := make([]byte, 1+currentAvatarFittingRoomPartCount*currentAvatarFittingRoomRecordSize)
	body[0] = 1
	for index := 0; index < currentAvatarFittingRoomPartCount; index++ {
		offset := 1 + index*currentAvatarFittingRoomRecordSize
		copy(body[offset:offset+currentAvatarFittingRoomRecordSize], request.Records[index].Raw[:])
		marker := uint32(0)
		if changed[index] {
			marker = 1
		}
		binary.LittleEndian.PutUint32(body[offset+9:offset+13], marker)
	}
	return body
}

func currentAvatarFittingRoomFailureBody() []byte { return []byte{0} }
