package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// currentTownActorAreaDetachPresence resolves where a character stands in town
// for co-presence decisions: the live roster first, then the record retired by
// the dungeon-selector page. Opening that page removes the character from
// OnlinePlayerManager by operator instruction, so a live-only lookup is empty
// exactly while the selector is up.
func (s *Service) currentTownActorAreaDetachPresence(session *gameSession) (onlinePlayerInfo, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return onlinePlayerInfo{}, false
	}
	characterID := session.selectedCharacterID
	if s.onlinePlayers != nil {
		if presence, registered := s.onlinePlayers.PlayerForCharacter(characterID); registered {
			return presence, true
		}
	}
	session.townSelectorPresenceMu.Lock()
	presence := session.townSelectorRetiredPresence
	owner := session.townSelectorRetiredPresenceCharacterID
	session.townSelectorPresenceMu.Unlock()
	if owner != characterID || presence.CharacterID != characterID {
		return onlinePlayerInfo{}, false
	}
	return presence, true
}

// currentPartyEntryMembersShareTownArea decides party dungeon-entry co-presence.
//
// It cannot ask the live roster alone. Party entry is reached from the
// dungeon-selection page, and opening that page retires town presence by
// operator instruction, so by the time the leader confirms the dungeon every
// member - the leader included - has usually already been removed from
// OnlinePlayerManager. A live-only check would refuse every party entry with
// "party member N is not present in the leader town area".
//
// The retired record is the authority inside that window, and it is compared on
// the fields the manager keys areas by (areaKey{TownID, AreaID, InstanceKey})
// plus the session channel, so a member browsing the selector in another
// channel, another town or another area is refused.
func (s *Service) currentPartyEntryMembersShareTownArea(leader, follower *gameSession) bool {
	coPresent, _ := s.currentPartyEntryCoPresence(leader, follower)
	return coPresent
}

// bindSettlementRetryMemberPresence leaves the selector-page anchor for every party
// member that is still standing on its own completed run of this dungeon.
//
// The co-presence gate needs BOTH anchors (currentPartyEntryCoPresence), and a member
// that entered this dungeon with the leader has been off the town roster since that
// entry: without this the leader's 再次挑战 is refused with
// member_has_no_resolvable_town_anchor before it ever reaches the member-driven
// relaxation in prepareRuntimePartyDungeonEntry.
//
// Deliberately minimal, and this is the part that matters: it writes ONE session memo
// per member and nothing else -- no run is retired, no packet is sent to the member,
// no town/party state is mutated. The teammate's own client is driven by the leader's
// entry request itself (86JP's PARTY_LOADING_GATE shape).
func (s *Service) bindSettlementRetryMemberPresence(leader *gameSession, dungeonID int64, source string) int {
	if s == nil || leader == nil || leader.selectedCharacterID == 0 || dungeonID == 0 {
		return 0
	}
	bound := 0
	for _, member := range runtimePartyMembers(runtimePartyStateSnapshot(leader)) {
		if member.UserID == 0 || member.UserID == leader.selectedCharacterID {
			continue
		}
		follower, ok := s.onlineGameSession(member.UserID)
		if !ok || follower == nil {
			continue
		}
		follower.dungeon.mu.Lock()
		runtime := follower.dungeon.runtime
		origin := currentTownPositionSnapshot{}
		inside := runtime != nil && runtime.Session != nil &&
			dungeonRuntimeOwnsCharacter(runtime, member.UserID) &&
			runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunCompleted &&
			runtime.Session.Snapshot().Run.DungeonID == dungeonID
		if inside {
			origin = runtime.townReturnOrigin
		}
		follower.dungeon.mu.Unlock()
		if !inside || !origin.PositionValid {
			continue
		}
		if s.bindCompletedRunSelectorPresence(follower, origin, source) {
			bound++
		}
	}
	if bound > 0 {
		s.logGameEvent(leader, "game-dungeon-retry-member-presence-bound",
			"source", source,
			"leader_char_id", leader.selectedCharacterID,
			"dungeon_id", dungeonID,
			"bound_members", bound,
			"reason", "the_leader_owns_retry_so_the_member_needs_its_town_anchor_from_it")
	}
	return bound
}

// settlementRetryDrivesMemberFromItsCompletedRun reports whether one party member is
// standing on the settlement page of its own completed run of the very dungeon the
// leader is re-entering.
//
// That is the only state in which the leader's 再次挑战 may drive a member whose
// selector page is not up: the member's client cannot start the retry itself (a member
// offers only 返回城镇), it is still inside its own finished instance, and the fan-out
// below replaces that run for it. The match is deliberately narrow -- the same dungeon
// id, or the member's own raid gate binding for it -- so a member that is somewhere
// else, or in another dungeon, is still refused.
func (s *Service) settlementRetryDrivesMemberFromItsCompletedRun(
	session *gameSession,
	characterID uint16,
	request dungeoncmd.SelectDungeonRequest,
) bool {
	if s == nil || session == nil || characterID == 0 || request.DungeonID == 0 {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil ||
		!dungeonRuntimeOwnsCharacter(runtime, characterID) ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		return false
	}
	runDungeonID := runtime.Session.Snapshot().Run.DungeonID
	if runDungeonID == int64(request.DungeonID) {
		return true
	}
	return runtime.raidDungeon.RaidKey != 0 && runtime.raidDungeon.MapID == int(request.DungeonID)
}

// currentPartyEntryCoPresence is the diagnosing form of the boolean above. The
// pair test alone cannot say WHICH side failed to resolve, and the refusal it
// produces names the party member - so a leader (or follower) that resolves
// nowhere reads as "party member N is not present in the leader town area" even
// when the member is exactly where it should be. That ambiguity cost a live
// investigation on 2026-09-19: 16 refused op16 requests, a silent client, and no
// server-side line saying that neither the leader's roster row nor its selector
// memo existed. The reason string and the two resolved anchors go into
// game-party-dungeon-entry-copresence-refused.
func (s *Service) currentPartyEntryCoPresence(leader, follower *gameSession) (bool, string) {
	if s == nil || leader == nil || follower == nil || leader == follower {
		return false, "leader_and_follower_sessions_are_not_distinct"
	}
	if leader.selectedCharacterID == 0 || follower.selectedCharacterID == 0 ||
		leader.selectedCharacterID == follower.selectedCharacterID {
		return false, "character_identity_missing_or_duplicated"
	}
	if leader.channel.ID != follower.channel.ID {
		return false, "party_members_are_in_different_channels"
	}
	leaderPresence, leaderResolved := s.currentTownActorAreaDetachPresence(leader)
	followerPresence, followerResolved := s.currentTownActorAreaDetachPresence(follower)
	switch {
	case !leaderResolved && !followerResolved:
		return false, "neither_member_has_a_resolvable_town_anchor"
	case !leaderResolved:
		return false, "leader_has_no_resolvable_town_anchor"
	case !followerResolved:
		return false, "member_has_no_resolvable_town_anchor"
	}
	if leaderPresence.TownID != followerPresence.TownID {
		return false, "party_members_are_in_different_towns"
	}
	if leaderPresence.AreaID != followerPresence.AreaID {
		return false, "party_members_are_in_different_areas"
	}
	if leaderPresence.InstanceKey != followerPresence.InstanceKey {
		return false, "party_members_are_in_different_instances"
	}
	return true, "same_town_area_and_instance"
}
