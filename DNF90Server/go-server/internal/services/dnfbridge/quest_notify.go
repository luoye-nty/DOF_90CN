package dnfbridge

import (
	"encoding/binary"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const currentSaveQuestNotifyMaxIDs = 4

// decodeCurrentSaveQuestNotifyBody matches NoPack sub_1C53400:
// u8 vector length followed by that many u32 quest IDs. This is a client
// notification-list save request; the EXE has no corresponding S2C ACK.
func decodeCurrentSaveQuestNotifyBody(body []byte) ([]uint32, bool) {
	if len(body) == 0 || int(body[0]) > currentSaveQuestNotifyMaxIDs ||
		len(body) != 1+int(body[0])*4 {
		return nil, false
	}
	ids := make([]uint32, int(body[0]))
	for index := range ids {
		ids[index] = binary.LittleEndian.Uint32(body[1+index*4:])
	}
	return ids, true
}

func (s *Service) handleCurrentSaveQuestNotify(session *gameSession, body []byte) error {
	ids, ok := decodeCurrentSaveQuestNotifyBody(body)
	if !ok {
		s.logGameEvent(session, "game-legacy-save-quest-notify-ignored",
			"msg_id", uint16(dnfenum.CmdPacketSaveQuestNotify),
			"body_len", len(body),
			"reason", "current_exe_op506_requires_u8_count_and_repeated_u32_quest_ids")
		return nil
	}
	s.logGameEvent(session, "game-legacy-save-quest-notify-accepted",
		"msg_id", uint16(dnfenum.CmdPacketSaveQuestNotify),
		"body_len", len(body),
		"quest_count", len(ids),
		"quest_ids", ids,
		"response", "none",
		"state_mutation", "none",
		"reason", "current_exe_op506_is_client_owned_notification_list_without_s2c_ack")
	return nil
}
