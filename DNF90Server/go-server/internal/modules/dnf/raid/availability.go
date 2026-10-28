package raid

import "errors"

const (
	// RaidAvailabilityMsgID is the current class-0 notification registered by
	// DNF.exe for the three daily team-raid availability rows. It is written as
	// hexadecimal here deliberately: the generated command enum's decimal 517
	// entry is a different legacy command.
	RaidAvailabilityMsgID uint16 = 0x0517

	RaidAvailabilityRowCount = 3
)

var errRaidAvailabilityRowCount = errors.New("raid availability requires exactly three rows")

// RaidAvailabilityRow is one five-byte row consumed by the current client:
// key, used count, daily limit, and two opaque bytes.
type RaidAvailabilityRow struct {
	Key    byte
	Used   byte
	Limit  byte
	Field3 byte
	Field4 byte
}

// BuildRaidAvailabilityBody builds the exact fixed-width 0x0517 body.
// DNF.exe reads three rows unconditionally, so accepting another count would
// shift the following packet boundary or leave the client waiting for bytes.
func BuildRaidAvailabilityBody(rows []RaidAvailabilityRow) ([]byte, error) {
	if len(rows) != RaidAvailabilityRowCount {
		return nil, errRaidAvailabilityRowCount
	}
	body := make([]byte, 0, RaidAvailabilityRowCount*5)
	for _, row := range rows {
		body = append(body, row.Key, row.Used, row.Limit, row.Field3, row.Field4)
	}
	return body, nil
}

// BuildNativeRaidAvailabilityBody exposes the current local 90CN defaults:
// all three registered team-raid keys are available once today and have not
// been entered yet. The opaque fields remain zero until a client meaning is
// proved independently.
func BuildNativeRaidAvailabilityBody() []byte {
	body, err := BuildRaidAvailabilityBody([]RaidAvailabilityRow{
		{Key: 1, Limit: 1},
		{Key: 2, Limit: 1},
		{Key: 3, Limit: 1},
	})
	if err != nil {
		panic(err)
	}
	return body
}
