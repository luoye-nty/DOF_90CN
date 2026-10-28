package dnfbridge

import (
	"context"
	"encoding/binary"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfemotion "longheng.io/server/internal/modules/dnf/emotion"
)

const (
	currentChangeEmotionOpcode uint16 = uint16(dnfenum.CmdPacketChangeEmotion)
	currentEmotionIndexMax     uint16 = 0xff
	currentEmotionFailureCode  byte   = 1
)

func (s *Service) handleCurrentChangeEmotion(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if len(body) < 2 {
		s.logGameEvent(session, "game-change-emotion-blocked", "body_len", len(body), "reason", "body_too_short")
		return nil
	}
	emotionIndex := int16(binary.LittleEndian.Uint16(body[0:2]))
	if emotionIndex < 0 || uint16(emotionIndex) > currentEmotionIndexMax {
		s.logGameEvent(session, "game-change-emotion-blocked", "emotion_index", emotionIndex, "reason", "emotion_index_out_of_range")
		return s.sendGameUpperFailure(session, currentChangeEmotionOpcode, currentEmotionFailureCode)
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok {
		s.logGameEvent(session, "game-change-emotion-save-failed", "char_id", characterID, "emotion_index", emotionIndex, "reason", "repository_unavailable")
		return s.sendGameUpperFailure(session, currentChangeEmotionOpcode, currentEmotionFailureCode)
	}
	owner, err := dnfemotion.NewOwner(repos)
	if err == nil {
		_, err = owner.Apply(ctx, dnfemotion.Command{AccountID: s.accountIDForSession(session), SelectedCharacterID: session.selectedCharacterID, EmotionIndex: uint16(emotionIndex)})
	}
	if err != nil {
		s.logGameEvent(session, "game-change-emotion-save-failed", "char_id", characterID, "emotion_index", emotionIndex, "reason", err)
		return s.sendGameUpperFailure(session, currentChangeEmotionOpcode, currentEmotionFailureCode)
	}

	s.logGameEvent(session, "game-change-emotion",
		"char_id", characterID,
		"emotion_index", emotionIndex)

	// ACK: IDA sub_1D18900 reads u16 emotionIndex from success body.
	// Note: extended-login dispatch re-registers 254 as DoNothing, but the
	// lobby handler sub_1CF5280 may still consume the body.
	var w packetWriter
	w.writeUint16(uint16(emotionIndex))
	if err := s.sendGameUpperSuccess(session, currentChangeEmotionOpcode, w.bytes()); err != nil {
		return err
	}
	return s.sendCurrentMoodProjection(session, uint16(emotionIndex), "change_emotion_after_ack")
}
