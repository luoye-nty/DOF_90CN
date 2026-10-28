package dnfbridge

import (
	"context"
	"net/netip"
	"strconv"
)

// selectedCharacterForClientIP returns the character this client last selected
// on any of its connections. The client-account registry is updated on every
// op4, so a PVP channel connection opened right after leaving town still knows
// which character the player is carrying over.
func (s *Service) selectedCharacterForClientIP(address netip.Addr) uint16 {
	if s == nil || !address.IsValid() {
		return 0
	}
	s.clientAccounts.mu.Lock()
	defer s.clientAccounts.mu.Unlock()
	entry, found := s.clientAccounts.byRemoteIP[address.Unmap()]
	if !found {
		return 0
	}
	return entry.selectedCharacterID
}

// selectedCharacterForAccount finds the character another live connection of
// the same account already selected. Arena entry opens a second game socket
// while the town connection is still bound, so this is the most reliable
// source after a channel switch that never sends op4 on the new socket.
func (s *Service) selectedCharacterForAccount(accountID string, exclude *gameSession) uint16 {
	if s == nil || accountID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.gameSessions {
		if other == nil || other == exclude || other.accountID != accountID {
			continue
		}
		if other.selectedCharacterID != 0 {
			return other.selectedCharacterID
		}
	}
	return 0
}

// sendCurrentPVPChannelQuestBootstrap republishes the completed-quest bitmap on
// the arena channel connection.
//
// Why this exists: entering the arena opens a second game connection, and the
// current client evaluates its PVP eligibility gate on that new connection. A
// normal channel change reselects the character (op4) and therefore replays
// op356 in the login scene; the arena entry does not send op4, so the fresh
// connection never received quest 2670's "completed" bit. The client then shows
// "该角色没有完成风云崛起(1/5)" and never finishes loading the lobby, even
// though the database and the roster entry both say the quest is complete.
func (s *Service) sendCurrentPVPChannelQuestBootstrap(session *gameSession) {
	if s == nil || session == nil || !isPVPChannel(session.channel) {
		if session != nil {
			s.logInfo("dnfbridge pvp channel quest bootstrap skipped",
				"conn_id", session.connID,
				"channel_id", session.channel.ID,
				"channel_group", session.channel.Group,
				"reason", "not_a_pvp_channel")
		}
		return
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		characterID = s.selectedCharacterForAccount(session.accountID, session)
	}
	if characterID == 0 {
		characterID = s.selectedCharacterForClientIP(session.accountSourceIP)
	}
	if characterID == 0 {
		s.logInfo("dnfbridge pvp channel quest bootstrap skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"reason", "no_selected_character")
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	record, found, err := repositories.Quest.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil || !found {
		s.logInfo("dnfbridge pvp channel quest bootstrap skipped",
			"conn_id", session.connID,
			"character_id", characterID,
			"found", found,
			"error", err)
		return
	}
	transport, err := zlibCompress(buildCurrentClearQuestListBody(record, true))
	if err != nil {
		s.logInfo("dnfbridge pvp channel quest bootstrap compress failed",
			"conn_id", session.connID,
			"character_id", characterID,
			"error", err)
		return
	}
	if err := s.sendGameUpperFixed16Transport(
		session,
		currentClearQuestListMsgID,
		transport,
		0,
		1,
		true,
		currentClearQuestListTransportCodec,
	); err != nil {
		s.logInfo("dnfbridge pvp channel quest bootstrap send failed",
			"conn_id", session.connID,
			"character_id", characterID,
			"error", err)
		return
	}
	s.logInfo("dnfbridge pvp channel quest bootstrap sent",
		"conn_id", session.connID,
		"character_id", characterID,
		"quest_2670", record.States[2670].Status)
}
