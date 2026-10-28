package dnfbridge

import (
	"context"
	"encoding/binary"
	"strconv"
)

const (
	currentMoodProjectionMsgID      = uint16(413)
	currentMoodProjectionType       = byte(2)
	currentMoodProjectionVersion    = byte(1)
	currentMoodProjectionBodyLength = 4
	currentMoodProjectionMax        = uint16(0xff)
	currentMoodProjectionPrivateABI = "90cn_private_mood_projection_v1"
)

func buildCurrentMoodProjectionBody(moodValue uint16) []byte {
	body := make([]byte, currentMoodProjectionBodyLength)
	body[0] = currentMoodProjectionType
	body[1] = currentMoodProjectionVersion
	binary.LittleEndian.PutUint16(body[2:4], moodValue)
	return body
}

func (s *Service) sendCurrentMoodProjection(session *gameSession, moodValue uint16, source string) error {
	if s == nil || session == nil || moodValue > currentMoodProjectionMax {
		return nil
	}
	body := buildCurrentMoodProjectionBody(moodValue)
	s.logGameEvent(session, "game-mood-private-projection-send", "source", source, "char_id", session.selectedCharacterID, "msg_id", currentMoodProjectionMsgID, "classification", 0, "body_len", len(body), "mood_value", moodValue, "private_abi", currentMoodProjectionPrivateABI, "wire_owner", "90CN.dll", "native_exe_protocol", false)
	return s.sendGameUpperRawClass(session, currentMoodProjectionMsgID, body, 0)
}

func (s *Service) selectedCurrentMoodValue(session *gameSession) (uint16, bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return 0, false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return 0, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		return 0, false, err
	}
	moodValue := numericCharacterStatValue(character, "mood_value")
	if moodValue < 0 || moodValue > int64(currentMoodProjectionMax) {
		return 0, false, nil
	}
	return uint16(moodValue), true, nil
}
