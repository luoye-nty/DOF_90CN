package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// sendCurrentRaidAvailabilityOnce sends the current client's fixed three-row
// team-raid availability table after character selection. The client defaults
// each row's limit to zero when this notification is absent, which is the
// direct source of the "today is not a team-mode development day" message.
func (s *Service) sendCurrentRaidAvailabilityOnce(session *gameSession, source string) error {
	if session == nil || session.raidAvailabilitySent {
		return nil
	}
	body := raid.BuildNativeRaidAvailabilityBody()
	if err := s.sendGameUpperRawClass(session, raid.RaidAvailabilityMsgID, body, 0); err != nil {
		return err
	}
	session.raidAvailabilitySent = true
	s.logGameEvent(session, "game-current-raid-availability-sent",
		"msg_id", raid.RaidAvailabilityMsgID,
		"classification", 0,
		"row_count", raid.RaidAvailabilityRowCount,
		"body_len", len(body),
		"body_hex", fmt.Sprintf("%X", body),
		"source", source,
		"body_source", "current_exe_sub_017B71B0_three_rows_key_field_used_limit_field")
	return nil
}

func isCurrentRaidDirectoryChannel(session *gameSession) bool {
	if session == nil {
		return false
	}
	return isCurrentTeamChannelType(session.channel.Type)
}

func isCurrentTeamChannelType(channelType uint8) bool {
	switch channelType {
	case dnfenum.RaidChannelType, dnfenum.HiddenRaidType, 42:
		return true
	default:
		return false
	}
}

// sendCurrentRaidDirectoryReadyOnce initializes the current client's raid
// directory cooldown. In team-channel states, sub_17B18B0 keeps owner 0x74
// closed while the manager timestamp is -1. The current EXE registers
// sub_34463B0 for op1228 at sub_344AAD8; that handler first consumes one
// reserved byte, then records the timestamp through sub_17B1890. After its
// native three-second delay the system-menu owner can open and issue C2S 831
// for the public raid page.
func (s *Service) sendCurrentRaidDirectoryReadyOnce(session *gameSession, source string) error {
	if session == nil ||
		session.raidDirectoryReadySent ||
		!isCurrentRaidDirectoryChannel(session) ||
		session.sceneBootstrapTailDeferred ||
		!session.sceneBootstrapTailSent {
		return nil
	}
	body := raid.BuildRaidDirectoryReadyBody()
	if err := s.sendGameUpperRawClass(session, raid.RaidDirectoryReadyMsgID, body, 0); err != nil {
		return err
	}
	session.raidDirectoryReadySent = true
	s.logGameEvent(session, "game-current-raid-directory-ready-sent",
		"msg_id", raid.RaidDirectoryReadyMsgID,
		"classification", 0,
		"body_len", len(body),
		"source", source,
		"body_source", "current_exe_sub_34463B0_sets_sub_17B18B0_ready_timestamp")
	return nil
}
