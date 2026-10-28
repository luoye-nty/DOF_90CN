package dnfbridge

// Dungeon-selector town presence.
//
// This file implements an operator product decision that deliberately
// contradicts the recorded behaviour of the external release: a character that
// opens the dungeon-selection page must stop being visible to the other players
// in its town area.
//
// The release does the opposite, and that was proved rather than assumed. In
// `captures/01-external-friend-chat-party/` the three selector openings
// (12:08:10, 12:32:00 twice) and the three town-leave broadcasts
// (`town_copresence_leave_0x0006` at 12:09:54, 12:29:04, 12:30:38) have zero
// overlap, and the real presence retirement happens at dungeon entry - 12:32:05,
// five seconds after the selector page opened. The client's own op23 area=0xFF
// while the page is up is a local rendering detail of that page, not a
// co-presence statement. So the release keeps a selector-browsing character
// fully visible; this server, by instruction, does not.
//
// The retirement and its restore are one pair. Opening the page retires the
// roster record; the op132 selector return registers it again from the exact
// record that was retired. Every other way off the page already re-registers on
// its own - op36 SET_USER_AREA and prev-village both run a full EnterArea, the
// completed dungeon return restores its own presence, and dungeon entry is
// supposed to leave the character retired - so those routes only drop the memo.

// leaveTownPresenceForDungeonSelector retires the town presence of the one
// character that opened the dungeon-selection page and tells its former
// same-area peers to destroy the actor.
//
// It is the single-session shape of leaveTownPresenceForDungeon: same
// LeaveAreaSession call, same op6 broadcast, no new mechanism. Four deliberate
// differences from the dungeon-entry retirement:
//
//   - Only the opener is retired. A selector opened by a leader does move every
//     party member onto the page through synchronizeRuntimePartyMembersEnterSelect,
//     but the followers' presence is left alone per instruction, which also keeps
//     every party path that reads the town roster working exactly as accepted.
//   - The op6 destroy is withheld from the opener's own party members, exactly
//     as broadcastTownPlayerDungeonTransition withholds it from fellow entrants.
//     That is not a style choice: this client answers op6 on a party member with
//     LEAVE_PARTY, so broadcasting it to a teammate would disband the party the
//     moment anybody opened the dungeon gate. Unlike the dungeon transition no
//     op23 replacement is sent either, because the opener is not moving areas -
//     it is still standing where it was.
//   - No friend-list delta is pushed. leaveTownPresenceForDungeon follows its
//     retirement with a busy notifyCurrentUnitedFriendPresence because entering a
//     dungeon is what the release projects as busy at 12:32:05. Browsing the
//     selector is not that event, so the friend projection is left untouched.
//   - Raid channels are excluded. The Anton waiting room is already a
//     server-owned private instance, so there is nobody to hide from, and
//     runtimeRaidSelectorAdmission requires the opener to still hold that
//     waiting-instance presence on every following op15 - the current client
//     retries op15 after its post-start-map state, so retiring here would refuse
//     the raid portal on the client's own retry.
func (s *Service) leaveTownPresenceForDungeonSelector(session *gameSession, source string) {
	if s == nil || s.onlinePlayers == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	if _, raidChannel := currentRaidTownSpecForSession(session); raidChannel {
		return
	}
	characterID := session.selectedCharacterID
	presence, registered := s.onlinePlayers.PlayerForCharacter(characterID)
	if !registered || presence.Session != session {
		return
	}
	_, others, removed := s.onlinePlayers.LeaveAreaSession(characterID, session)
	if !removed {
		return
	}
	session.townSelectorPresenceMu.Lock()
	session.townSelectorRetiredPresence = presence
	session.townSelectorRetiredPresenceCharacterID = characterID
	session.townSelectorPresenceMu.Unlock()

	partyMembers := make(map[uint16]bool)
	for _, member := range runtimePartyMembers(runtimePartyStateSnapshot(session)) {
		partyMembers[member.UserID] = true
	}
	outsiders := make([]onlinePlayerInfo, 0, len(others))
	for _, other := range others {
		if partyMembers[other.CharacterID] {
			continue
		}
		outsiders = append(outsiders, other)
	}
	if len(outsiders) > 0 {
		s.broadcastTownPlayerLeave(characterID, outsiders)
	}
	s.logGameEvent(session, "game-town-enter-select-presence-retired",
		"source", source,
		"char_id", characterID,
		"town_id", presence.TownID,
		"area_id", presence.AreaID,
		"peer_count", len(others),
		"notified_peer_count", len(outsiders),
		"party_member_peer_count", len(others)-len(outsiders),
		"decision", "operator_product_decision_selector_page_is_not_visible_in_town",
		"capture_behaviour", "external_release_keeps_the_selector_character_visible_and_retires_only_at_dungeon_entry")
}

// bindCompletedRunSelectorPresence leaves the selector-page presence memo for a
// character whose COMPLETED run has just been retired into the other-dungeon
// selector page (选择其他地下城).
//
// It is the synthesized twin of leaveTownPresenceForDungeonSelector. That route
// always starts from a live roster record and moves it into the memo; this one
// runs after dungeon entry has already removed the record and spent the memo
// (leaveTownPresenceForDungeon), so without it the character resolves nowhere:
// the live roster has no row and there is no retired record. The party entry
// preflight reads exactly that resolution
// (currentPartyEntryMembersShareTownArea), so every following op16 was refused
// with the retryable reason-3 rejection - a reply this client answers with
// silence, which the player reports as 点不动. Measured 2026-09-19 13:19:23Z:
// a two-member party cleared a run, the leader took 选择其他地下城, and 16
// identical op16 requests for dungeon 3 each ended in
// game-party-dungeon-entry-blocked error="party member 22 is not present in the
// leader town area".
//
// The anchor is the run's own frozen pre-entry origin, i.e. the exact spot the
// selector page returns the character to, so nothing is invented: the memo stays
// invisible in town (no EnterArea, no broadcast - the operator decision that a
// selector-browsing character is not visible), and it is consumed the same way
// the live-record memo is, by dungeon entry or by any real town registration.
//
// It refuses to act while the character still holds a live registration: the
// live row wins in currentTownActorAreaDetachPresence anyway, and the route that
// owns that row (the op15 selector page) leaves its own memo from it.
func (s *Service) bindCompletedRunSelectorPresence(
	session *gameSession,
	origin currentTownPositionSnapshot,
	source string,
) bool {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		!origin.PositionValid || origin.CharacterID != session.selectedCharacterID {
		return false
	}
	characterID := session.selectedCharacterID
	if s.onlinePlayers != nil {
		if _, registered := s.onlinePlayers.PlayerForCharacter(characterID); registered {
			s.logGameEvent(session, "game-dungeon-completed-selector-presence-skipped",
				"source", source,
				"char_id", characterID,
				"reason", "character_already_registered_in_a_town_area")
			return false
		}
	}
	// Mirror the live-record instance key: ordinary towns and the public raid
	// lobby use zero, and only the server-owned Anton waiting area is keyed by
	// the member's RaidKey. The area test is the same one
	// currentDungeonTownReturnOriginOwner uses, so a raid member whose run
	// started in an ordinary town keeps the ordinary key.
	instanceKey := uint32(0)
	if raidKey := s.runtimeRaidKeyForMember(characterID); raidKey != 0 {
		if spec, raidChannel := currentRaidTownSpecForSession(session); raidChannel &&
			origin.TownID == byte(spec.townID) && origin.AreaID == spec.waitingArea {
			instanceKey = raidKey
		}
	}
	presence := onlinePlayerInfo{
		CharacterID: characterID,
		ChannelID:   townPresenceChannelID(session),
		TownID:      origin.TownID,
		AreaID:      origin.AreaID,
		InstanceKey: instanceKey,
		PositionX:   origin.PositionX,
		PositionY:   origin.PositionY,
		Direction:   origin.MovementCode,
		Session:     session,
	}
	session.townSelectorPresenceMu.Lock()
	previous := session.townSelectorRetiredPresence
	hadPrevious := session.townSelectorRetiredPresenceCharacterID == characterID
	session.townSelectorRetiredPresence = presence
	session.townSelectorRetiredPresenceCharacterID = characterID
	session.townSelectorPresenceMu.Unlock()
	s.logGameEvent(session, "game-dungeon-completed-selector-presence-bound",
		"source", source,
		"char_id", characterID,
		"channel_id", presence.ChannelID,
		"town_id", presence.TownID,
		"area_id", presence.AreaID,
		"instance_key", presence.InstanceKey,
		"position_x", presence.PositionX,
		"position_y", presence.PositionY,
		"replaced_previous_binding", hadPrevious,
		"previous_town_id", previous.TownID,
		"previous_area_id", previous.AreaID,
		"reason", "completed_run_retired_into_other_dungeon_selector_needs_a_town_anchor_for_party_entry")
	return true
}

// restoreTownPresenceAfterDungeonSelector registers the retired record again
// when the player closes the page and stays in town. Without it the retirement
// is permanent: the op132 selector return deliberately preserves the finalized
// town scene instead of running a completed-return presence restore, so nothing
// else on that route ever touches the roster.
//
// The retired record is replayed verbatim rather than rebuilt from the session's
// latest op35. A position report can still arrive while the page is up, and the
// typed op24 this return has just written is built from the frozen selector
// origin - which is the position the record was retired at. Replaying the record
// keeps the roster and that op24 on the same coordinates.
//
// It refuses to act when the character already holds a live registration, so a
// route that re-registered on its own can never end up with a duplicate actor.
func (s *Service) restoreTownPresenceAfterDungeonSelector(session *gameSession, source string) bool {
	if s == nil || s.onlinePlayers == nil || session == nil || session.selectedCharacterID == 0 {
		return false
	}
	characterID := session.selectedCharacterID
	session.townSelectorPresenceMu.Lock()
	presence := session.townSelectorRetiredPresence
	owner := session.townSelectorRetiredPresenceCharacterID
	session.townSelectorPresenceMu.Unlock()
	if owner != characterID || presence.CharacterID != characterID {
		return false
	}
	if _, registered := s.onlinePlayers.PlayerForCharacter(characterID); registered {
		clearTownSelectorRetiredPresence(session)
		s.logGameEvent(session, "game-town-enter-select-presence-restore-skipped",
			"source", source,
			"char_id", characterID,
			"reason", "character_already_registered_in_a_town_area")
		return false
	}
	player := presence
	player.Session = session
	others := s.onlinePlayers.EnterArea(&player)
	s.promoteResidentGameSession(session, characterID)
	clearTownSelectorRetiredPresence(session)
	s.broadcastTownPlayerEnter(&player, others)
	s.notifyCurrentUnitedFriendPresence(
		player.Name,
		currentFriendPresence{ChannelID: uint16(session.channel.ID)},
		source+"_friend_online",
	)
	s.replayCurrentExpertJobStores(session, player.TownID, player.AreaID)
	s.replayCurrentPrivateStores(session, player.TownID, player.AreaID)
	s.logGameEvent(session, "game-town-enter-select-presence-restored",
		"source", source,
		"char_id", characterID,
		"town_id", player.TownID,
		"area_id", player.AreaID,
		"position_x", player.PositionX,
		"position_y", player.PositionY,
		"peer_count", len(others),
		"sequence", "remote_mode0_then_mode1_then_op9_then_op23_both_directions")
	return true
}

// clearTownSelectorRetiredPresence drops the remembered record. Every route
// that gives the character a real town registration again calls this, so the
// restore above can never resurrect a stale actor after the page is gone.
//
// townSelectorPresenceMu is a leaf: several callers (SET_USER_AREA,
// prev-village, the initial town route) hold townMu across their whole body, so
// this memo deliberately does not share that lock.
func clearTownSelectorRetiredPresence(session *gameSession) {
	if session == nil {
		return
	}
	session.townSelectorPresenceMu.Lock()
	session.townSelectorRetiredPresence = onlinePlayerInfo{}
	session.townSelectorRetiredPresenceCharacterID = 0
	session.townSelectorPresenceMu.Unlock()
}
