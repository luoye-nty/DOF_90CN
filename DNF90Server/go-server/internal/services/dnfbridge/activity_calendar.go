package dnfbridge

import (
	"fmt"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const currentActivityCalendarMsgID uint16 = 0x006c

type activityCalendarRecord struct {
	eventID   uint16
	field0    uint32
	baseText0 []byte
	baseText1 []byte
	extension bool
	text3     string
}

func currentActivityCalendarRecords() []activityCalendarRecord {
	return []activityCalendarRecord{
		{eventID: 181, field0: 0xff6e00ff, extension: true, text3: "ui/event/calendar/2awakeninglevel.xui/2/107&1"},
		{eventID: 192},
		{eventID: 200},
		{eventID: 145},
		{eventID: 143},
		{eventID: 202},
		{eventID: 457},
	}
}

func writeCurrentActivityCalendarRecord(writer *packetWriter, record activityCalendarRecord) error {
	if writer == nil {
		return fmt.Errorf("activity calendar writer is nil")
	}
	writer.writeUint16(record.eventID)
	writer.writeUint32(record.field0)
	writer.writeRawDstr(record.baseText0)
	writer.writeRawDstr(record.baseText1)
	if !record.extension {
		writer.writeByte(0)
		return nil
	}
	text3, err := encodeCurrentActivityCalendarText(record.text3)
	if err != nil {
		return err
	}
	writer.writeByte(1)
	writer.writeByte(0)
	writer.writeByte(0)
	writer.writeRawDstr(nil)
	writer.writeRawDstr(nil)
	writer.writeRawDstr(nil)
	writer.writeUint32(0)
	writer.writeUint32(0)
	writer.writeRawDstr(text3)
	writer.writeRawDstr(nil)
	writer.writeByte(0)
	return nil
}

func encodeCurrentActivityCalendarText(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("encode activity calendar text %q as GB18030: %w", value, err)
	}
	return encoded, nil
}

func buildCurrentActivityCalendarBody() ([]byte, error) {
	records := currentActivityCalendarRecords()
	var writer packetWriter
	writer.writeUint16(uint16(len(records)))
	for _, record := range records {
		if err := writeCurrentActivityCalendarRecord(&writer, record); err != nil {
			return nil, err
		}
	}
	writer.writeByte(0)
	return writer.bytes(), nil
}

func (s *Service) sendCurrentActivityCalendar(session *gameSession, source string) error {
	if s == nil || s.options.pvfPath == "" {
		// Unit/test-only services without a PVF source do not own the client
		// event table. Production always supplies env.PVF or a configured path.
		return nil
	}
	body, err := buildCurrentActivityCalendarBody()
	if err != nil {
		return err
	}
	if len(body) != 184 {
		return fmt.Errorf("activity calendar body length=%d want=184", len(body))
	}
	// The calendar has its own class-0/op0x006C owner. Keep it separate from
	// compressed op108, whose parser installs cumulative-online event state.
	s.logGameEvent(session, "game-activity-calendar-standalone-send",
		"source", source,
		"msg_id", currentActivityCalendarMsgID,
		"record_count", len(currentActivityCalendarRecords()),
		"body_len", len(body),
		"transport", "class0_protected_marker1_plaintext")
	return s.sendCurrentProtectedClass0Packet(
		session,
		currentActivityCalendarMsgID,
		body,
		"current_activity_calendar_plaintext",
		"activity_calendar",
	)
}
