package dnfbridge

import (
	"strings"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// DNF.exe 1EA4EDB and 1EA281D use 1CF3800, which reads the native channel
// number and accepts 75..77. Type 41 is the DPL preliminary tournament and
// additionally opens obsolete tournament assets; it is not this entry.
func isCurrentPVPTotalChannel(channel channelcatalog.Channel) bool {
	return isPVPChannel(channel) && channel.Type == 8 && channel.ID >= 75 && channel.ID <= 77
}

func isPVPChannel(channel channelcatalog.Channel) bool {
	return strings.EqualFold(strings.TrimSpace(channel.Group), "pvp")
}

func (s *Service) freeDuelAvailable() bool {
	if s == nil {
		return false
	}
	catalog := s.currentCatalog()
	if catalog == nil {
		return false
	}
	for _, channel := range catalog.Channels() {
		if isPVPChannel(channel) {
			return true
		}
	}
	return false
}

func (s *Service) handlePVPChannelInfo(session *gameSession, body []byte, transport string) error {
	requestValid := len(body) == 0
	success := requestValid && s.freeDuelAvailable()
	response := []byte{0, 0x15}
	if success {
		response = []byte{1, 0, 0, 0, 0, 0}
	}
	if session != nil {
		s.logGameEvent(session, "game-pvp-channel-info",
			"transport", transport,
			"body_len", len(body),
			"available", s.freeDuelAvailable(),
			"success", success,
			"response_body", response)
	}
	return s.sendGameFixed15Route(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketPVPChannelInfo),
		response,
		0,
	)
}
