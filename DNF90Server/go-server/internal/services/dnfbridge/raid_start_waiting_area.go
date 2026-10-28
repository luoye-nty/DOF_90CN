package dnfbridge

import (
	"fmt"
	"strings"
)

// This file owns the start-time waiting-area gate for C2S 666 (START_RAID).
//
// WHY THIS EXISTS, AND WHY IT MUST NOT BE DELETED WHEN ALIGNING WITH 86JP
// ----------------------------------------------------------------------
// The reference server has NO equivalent check: neither 86JP tree contains a
// single town/area test in its start-raid handler, and the only readiness
// re-check 86JP has is _86ref's "+3 seconds later, verify the whole roster is
// ready again" timer. This gate is therefore a deliberate EXTRA line of defence
// on our side, not a port. Do not remove it as "not in the reference".
//
// The failure it prevents was measured on 2026-09-20 (raid 1507329, channel 23):
// the leader stood in the Anton waiting area and pressed 开始 while the second
// member was still on the public deck (BattleShip_Main, area 1, instance 0).
// The server accepted the start and activated the raid phase, and THAT member's
// client stopped dispatching 0.24 s after the phase-activation burst -- the last
// line of 90CN_trace.log is 23 ms before the first storm packet -- and then went
// into a pure UDP retransmit storm: 17,320,521 datagrams in 273 seconds
// (~63,400/s), payload pinned at the same 9 bytes with the inner counter frozen
// at 0, and zero TCP commands for the rest of the connection's life until the
// socket was force-closed. The control experiment proved the trigger: with BOTH
// members standing in the waiting area the storm never appears.
//
// A same-channel member is not enough: the deck (area 1) and the waiting area
// (area 2) are both channel 23 and both town 19, and they are separated by the
// instance key as well (the waiting area is registered with InstanceKey ==
// RaidKey, the deck with 0 -- see EnterArea in town_move.go). The test below
// therefore reads area AND instance AND town, never the channel alone.
const (
	// The text is composed server-side from the roster's own character names, so
	// it names who the leader is waiting for. Every member of the raid receives
	// it, not only the leader: the member who is out of position is the one who
	// has to walk in. Each name is wrapped in <> as the operator specified.
	currentRaidStartWaitingAreaNoticePrefix = "还有队员未在攻坚等候区，无法开始攻坚。未进入等候区的队员："
	// The overlay is the only notice surface whose colour the server controls
	// (client_notice.go), so this is the red the operator asked for.
	//
	// Measured 2026-09-20 against the deployed DLL: the colour does NOT reach the
	// screen yet, because 90CN.dll's notice path carries no colour at all --
	// LuaClientNotice is {length, style, text} (90CN.cpp:871) and its overlay
	// painter hardcodes SetTextColor(dc, RGB(255,248,224)) (90CN.cpp:6684). What
	// the operator sees today is the native magenta line below, which is the
	// client's own colour for that message type and cannot be overridden from the
	// server. This field stays red so it takes effect the moment the DLL honours
	// it; see the report note for the one-line DLL change that would do it.
	currentRaidStartWaitingAreaNoticeColor  = uint32(0xFFFF0000)
	currentRaidStartWaitingAreaNoticeSource = "raid_start_member_not_in_waiting_area"
)

// runtimeRaidTownSpecForOwnerMode maps the raid owner the start command is being
// served for to the town its waiting area lives in. It mirrors
// currentRaidTownSpecForSession, but takes the owner instead of a session: the
// gate has to describe the whole roster's destination, and a member's own
// session may already be in the wrong place -- that is exactly what is being
// tested.
func runtimeRaidTownSpecForOwnerMode(mode byte) (runtimeRaidTownSpec, bool) {
	switch mode {
	case runtimeRaidOwnerAnton:
		return runtimeRaidTownSpec{
			townID:      currentAntonRaidTownID,
			areaID:      currentAntonRaidAreaID,
			waitingArea: currentAntonRaidWaitingAreaID,
			ownerName:   "anton",
		}, true
	case runtimeRaidOwnerLuke:
		return runtimeRaidTownSpec{
			townID:      currentLukeRaidTownID,
			areaID:      currentLukeRaidAreaID,
			waitingArea: currentLukeRaidWaitingAreaID,
			ownerName:   "luke",
		}, true
	default:
		return runtimeRaidTownSpec{}, false
	}
}

// runtimeRaidMemberWaitingAreaStanding reports whether one member currently
// counts as standing in this raid's waiting area, plus the evidence for the log.
//
// A missing presence row is NOT the waiting area: the member is then between
// scenes (the raid channel's Seria room, a portal walk) or on a connection that
// suppressed its registration, and the start would pull that client into the
// activation burst exactly like a member on the deck. The diagnostic line
// records where the member actually was, so a rejection is always explainable
// after the fact.
func (s *Service) runtimeRaidMemberWaitingAreaStanding(
	spec runtimeRaidTownSpec,
	raidKey uint32,
	characterID uint16,
) (bool, string) {
	if s == nil || s.onlinePlayers == nil || characterID == 0 {
		return false, "presence_registry_unavailable"
	}
	presence, ok := s.onlinePlayers.PlayerForCharacter(characterID)
	if !ok {
		return false, "presence_row_absent"
	}
	if presence.TownID != byte(spec.townID) {
		return false, fmt.Sprintf("town=%d area=%d instance=%d", presence.TownID, presence.AreaID, presence.InstanceKey)
	}
	if presence.AreaID != spec.waitingArea {
		return false, fmt.Sprintf("town=%d area=%d instance=%d", presence.TownID, presence.AreaID, presence.InstanceKey)
	}
	if raidKey == 0 || presence.InstanceKey != raidKey {
		return false, fmt.Sprintf("town=%d area=%d instance=%d", presence.TownID, presence.AreaID, presence.InstanceKey)
	}
	return true, fmt.Sprintf("town=%d area=%d instance=%d", presence.TownID, presence.AreaID, presence.InstanceKey)
}

// runtimeRaidMembersOutsideWaitingArea lists the frozen members that are not
// standing in the raid's waiting area, in roster order.
//
// A member with no bound session is never listed: a client that is not there
// cannot be pulled into the activation burst, and the first start already
// refuses an offline member through validateRuntimeRaidRosterReady. Blocking the
// start on an absent client would only strand a raid whose member dropped
// between phases -- which the raid lifecycle already tolerates elsewhere
// (RewardSkippedMembers).
func (s *Service) runtimeRaidMembersOutsideWaitingArea(
	state runtimeRaidState,
	spec runtimeRaidTownSpec,
) []runtimeRaidMemberState {
	outside := make([]runtimeRaidMemberState, 0, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		if member.CharID == 0 {
			continue
		}
		if _, online := s.onlineGameSession(member.CharID); !online {
			continue
		}
		if standing, _ := s.runtimeRaidMemberWaitingAreaStanding(spec, state.RaidKey, member.CharID); standing {
			continue
		}
		outside = append(outside, member)
	}
	return outside
}

// runtimeRaidWaitingAreaNoticeText composes the refusal: the fixed sentence
// followed by the names of the members that are not in the area, in roster
// order, each wrapped in <> as the operator specified.
func runtimeRaidWaitingAreaNoticeText(members []runtimeRaidMemberState) string {
	var builder strings.Builder
	builder.WriteString(currentRaidStartWaitingAreaNoticePrefix)
	for _, member := range members {
		name := strings.TrimSpace(member.Name)
		if name == "" {
			// A roster row always carries a name (see
			// newRuntimeRaidMemberLocked); this only keeps a nameless row from
			// silently producing a sentence with nothing after it.
			name = fmt.Sprintf("角色%d", member.CharID)
		}
		builder.WriteString("<")
		builder.WriteString(name)
		builder.WriteString(">")
	}
	return builder.String()
}

// broadcastRuntimeRaidStartNotice sends one notice to every member of the raid.
//
// It reuses the two notice channels the tree already has for "the server
// refused, and the client's own message table has no line for it": the DLL-owned
// overlay (the only surface whose color the server controls, so the text can be
// red) followed by the native magenta 系统 tab line, exactly as
// sendCurrentDungeonEntryItemMissingFailure and the auto-complete refusals do
// (quest_auto_complete.go). Nothing new is invented here.
func (s *Service) broadcastRuntimeRaidStartNotice(state runtimeRaidState, text string, source string) {
	if s == nil || text == "" {
		return
	}
	for _, member := range state.FrozenMembers {
		if member.CharID == 0 {
			continue
		}
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil || session.conn == nil {
			continue
		}
		if err := s.sendNotice(session, noticeRequest{
			Surface: noticeSurfaceOverlay,
			Text:    text,
			Color:   currentRaidStartWaitingAreaNoticeColor,
			Source:  source,
		}); err != nil {
			s.logGameEvent(session, "game-raid-start-waiting-area-notice-failed",
				"char_id", member.CharID,
				"source", source,
				"surface", noticeSurfaceOverlay.Name,
				"error", err)
		}
		if err := s.sendNotice(session, noticeRequest{
			Surface: noticeSurfaceSystemMagenta,
			Text:    text,
			Source:  source + "_fallback",
		}); err != nil {
			s.logGameEvent(session, "game-raid-start-waiting-area-notice-failed",
				"char_id", member.CharID,
				"source", source,
				"surface", noticeSurfaceSystemMagenta.Name,
				"error", err)
		}
	}
}

// rejectRuntimeRaidStartOutsideWaitingArea answers one refused C2S 666: the
// reservation is rolled back first (the start must not stay half-open), then the
// leader's own command is failed with the generic envelope, and then every
// member gets the red reason. stage names which start boundary refused
// (initial_start or next_phase_start) so the two are distinguishable in the log.
func (s *Service) rejectRuntimeRaidStartOutsideWaitingArea(
	session *gameSession,
	typ uint16,
	reservation runtimeRaidStartReservation,
	state runtimeRaidState,
	spec runtimeRaidTownSpec,
	outside []runtimeRaidMemberState,
	stage string,
) error {
	names := make([]string, 0, len(outside))
	positions := make([]string, 0, len(outside))
	for _, member := range outside {
		_, evidence := s.runtimeRaidMemberWaitingAreaStanding(spec, state.RaidKey, member.CharID)
		name := strings.TrimSpace(member.Name)
		if name == "" {
			name = fmt.Sprintf("角色%d", member.CharID)
		}
		names = append(names, fmt.Sprintf("%d:%s", member.CharID, name))
		positions = append(positions, fmt.Sprintf("%d:%s", member.CharID, evidence))
	}
	s.logGameEvent(session, "game-raid-start-waiting-area-rejected",
		"type", typ,
		"raid_key", state.RaidKey,
		"source_char_id", session.selectedCharacterID,
		"stage", stage,
		"owner", spec.ownerName,
		"waiting_town_id", spec.townID,
		"waiting_area_id", spec.waitingArea,
		"waiting_instance_key", state.RaidKey,
		"roster_size", len(state.FrozenMembers),
		"outside_count", len(outside),
		"outside_members", strings.Join(names, ","),
		"outside_positions", strings.Join(positions, ","),
		"notice_text_prefix", currentRaidStartWaitingAreaNoticePrefix,
		"reason", "member_not_standing_in_the_raid_waiting_area")
	s.cancelRuntimeRaidStart(reservation)
	s.broadcastRuntimeRaidStartNotice(
		state,
		runtimeRaidWaitingAreaNoticeText(outside),
		currentRaidStartWaitingAreaNoticeSource+"_"+stage,
	)
	return s.sendRaidCommandFailure(session, typ)
}
