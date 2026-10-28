// party_online.go 处理只依赖当前 game 连接的临时多人组队回包。
// 这里不落 DB，也不替代真正 Party owner；它只把已在线且已选角的 session 组装成客户端可见的队伍快照。
package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/party"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// All online-party mutations below are routed through RuntimePartyManager.
// This bridge keeps the current EXE's packet order and per-client projections
// while the manager remains the sole owner of party identity and membership.

const (
	entryIntoPartyFailureGeneric = 3
	entryIntoPartyFailureInvalid = 19
	entryIntoPartyFailureFull    = 20
	walkoutPartyFailureInvalid   = 19
	walkoutPartyFailureAuthority = 8
	defaultPartyPeerPort         = 10000
)

func (s *Service) handleOnlinePartyCommand(session *gameSession, typ uint16, body []byte) (bool, error) {
	// The old connection can still deliver a buffered party command after a
	// reconnect has made a replacement session authoritative for the same
	// character. Never let that stale packet invite, join, leave, kick, or
	// rebuild the replacement's party. The current session directory is the
	// source of truth; a protocol response to the retired connection would
	// only corrupt its already-disposed client state, so consume it silently.
	if isOnlinePartyCommand(typ) && session != nil && session.selectedCharacterID != 0 {
		identity, current := s.boundGameSessionCharacterSnapshot(session)
		if !current || identity.session != session {
			s.logGameEvent(session, "game-party-command-stale-session-ignored",
				"type", typ,
				"char_id", session.selectedCharacterID,
				"character_generation", session.characterGeneration)
			return true, nil
		}
	}
	switch typ {
	case currentPartyPeerStateOpcode, currentPartyHolePunchOpcode, currentPartyAveragePingOpcode:
		return true, s.handleCurrentPartyTransportTelemetry(session, typ, body)
	}
	switch dnfenum.CmdPacket(typ) {
	case dnfenum.CmdPacketLeaveParty:
		// Always consume a leave request.  The client can send it after a peer
		// disconnect while its old singleton cache is already absent from the
		// central manager; returning it to the legacy path leaves the EXE's
		// non-leader gate intact.
		return true, s.handleOnlineLeaveParty(session, typ)
	case dnfenum.CmdPacketWalkoutPartyMember:
		return true, s.handleOnlineWalkoutPartyMember(session, typ, body)
	case dnfenum.CmdPacketChangeHost:
		return true, s.handleOnlineChangePartyHost(session, body)
	case dnfenum.CmdPacketChangePartyMemberPosition:
		return true, s.handleOnlineChangePartyMemberPosition(session, typ, body)
	case dnfenum.CmdPacketPartyTeleport:
		return true, s.handleCurrentPartyTeleportOrNetworkReport(session, body)
	case dnfenum.CmdPacketPartyTeleportConfirm:
		// The member's consent answer to the 377 prompt. Its encoding is not
		// decoded yet, so it is recorded instead of interpreted.
		return true, s.handleCurrentPartyTeleportConfirm(session, body)
	case dnfenum.CmdPacketReserveLeaveParty:
		if s.hasManagedRuntimeParty(session) || hasMultiMemberRuntimeParty(session) {
			return true, s.handleOnlineReserveLeaveParty(session, typ, body)
		}
		return false, nil
	case dnfenum.CmdPacketRequestPeer:
		return true, s.handleOnlineRequestPeer(session, typ, body)
	case dnfenum.CmdPacketResponsePeer:
		return true, s.handleOnlineResponsePeer(session, typ, body)
	case dnfenum.CmdPacketEntryIntoParty:
		return true, s.handleOnlineEntryIntoParty(session, typ, body)
	case dnfenum.CmdPacketEntryIntoPartyFinish:
		return true, s.handleOnlineEntryIntoPartyFinish(session, typ)
	case dnfenum.CmdPacketEnterWarroom:
		return s.handleRuntimePartyDirectoryJoin(session, body)
	case dnfenum.CmdPacketQuickJoinRoom:
		return s.handleRuntimePartyDirectoryRefresh(session, body)
	default:
		return false, nil
	}
}

func isOnlinePartyCommand(typ uint16) bool {
	switch typ {
	case currentPartyPeerStateOpcode, currentPartyHolePunchOpcode, currentPartyAveragePingOpcode:
		return true
	}
	switch dnfenum.CmdPacket(typ) {
	case dnfenum.CmdPacketLeaveParty,
		dnfenum.CmdPacketWalkoutPartyMember,
		dnfenum.CmdPacketChangeHost,
		dnfenum.CmdPacketChangePartyMemberPosition,
		dnfenum.CmdPacketPartyTeleport,
		dnfenum.CmdPacketPartyTeleportConfirm,
		dnfenum.CmdPacketReserveLeaveParty,
		dnfenum.CmdPacketRequestPeer,
		dnfenum.CmdPacketResponsePeer,
		dnfenum.CmdPacketEntryIntoParty,
		dnfenum.CmdPacketEnterWarroom,
		dnfenum.CmdPacketQuickJoinRoom:
		return true
	default:
		return false
	}
}

// currentPeerChannelScope reports whether two online characters are committed to
// the same channel.
//
// Presence registration is deliberately NOT usable for this. A character inside a
// private scene -- the Seria room every ordinary login lands in, a raid channel's
// Seria room, the PVP lobby -- is never placed in the presence map, so byChar has
// no entry for it and a presence-based comparison reports a channel mismatch that
// does not exist. Measured: an applicant standing in the raid channel's Seria room
// had every mode-10 application dropped by that comparison (13
// game-peer-request-target-outside-channel rows in the live log, all
// raid_admission_requires_same_channel), and the identical click started working
// the moment the character walked out into the public town.
//
// The committed game session carries the channel in every scene, so it is the
// correct authority here. A registration that carries channel zero must not be
// treated as a match, or two unrelated sessions would compare equal.
func (s *Service) currentPeerChannelScope(sourceID, targetID uint16) bool {
	if s == nil || sourceID == 0 || targetID == 0 || sourceID == targetID {
		return false
	}
	sourceSession, sourceOK := s.onlineGameSession(sourceID)
	targetSession, targetOK := s.onlineGameSession(targetID)
	if !sourceOK || !targetOK || sourceSession == nil || targetSession == nil {
		return false
	}
	sourceChannel := townPresenceChannelID(sourceSession)
	targetChannel := townPresenceChannelID(targetSession)
	return sourceChannel != 0 && sourceChannel == targetChannel
}

func (s *Service) handleOnlineRequestPeer(session *gameSession, typ uint16, body []byte) error {
	parsed, err := party.DecodeRequestPeerRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-peer-request-body-invalid",
			"type", typ,
			"body_len", len(body),
			"error", err)
		return nil
	}
	sourceID := selectedCharacterID(session)
	if sourceID == 0 || parsed.TargetID == 0 || parsed.TargetID == sourceID || s.onlinePlayers == nil {
		s.logGameEvent(session, "game-peer-request-target-outside-area",
			"type", typ,
			"source_char_id", sourceID,
			"target_id", parsed.TargetID,
			"mode", parsed.Mode)
		return nil
	}
	// Attack-party admission (mode 10) is directory-driven, not co-presence
	// driven. The client resolved the target character from the raid list, so
	// the target legitimately stands in another area of the same town: the
	// leader waits inside the raid's private instance while the applicant is
	// still on the public deck. Requiring a shared area drops the application
	// before handleRuntimeRaidPeerRequest can record it, which is exactly the
	// observed "参加攻坚队 does nothing" symptom.
	//
	// Reference semantics: the 86JP C# server's PartyHandler.Handle_REQUEST_PEER
	// gates a peer request on IsSameGameChannel and rejects the cross-channel
	// case; it never compares town area. Keep that channel-level scope here and
	// leave the raid-specific validation (direction, raid open for admission,
	// leader online) to handleRuntimeRaidPeerRequest.
	crossAreaRaidAdmission := parsed.Mode == runtimeRaidPeerRequestMode &&
		runtimeRaidModeForSession(session) != 0
	if crossAreaRaidAdmission {
		if !s.currentPeerChannelScope(sourceID, parsed.TargetID) {
			s.logGameEvent(session, "game-peer-request-target-outside-channel",
				"type", typ,
				"source_char_id", sourceID,
				"target_id", parsed.TargetID,
				"mode", parsed.Mode,
				"reason", "raid_admission_requires_same_channel")
			return nil
		}
	} else if !s.onlinePlayers.PeerInSameArea(sourceID, parsed.TargetID) {
		s.logGameEvent(session, "game-peer-request-target-outside-area",
			"type", typ,
			"source_char_id", sourceID,
			"target_id", parsed.TargetID,
			"mode", parsed.Mode)
		return nil
	}
	s.logGameEvent(session, "game-peer-request-accepted",
		"type", typ,
		"source_char_id", sourceID,
		"target_id", parsed.TargetID,
		"mode", parsed.Mode,
		"cross_area_raid_admission", crossAreaRaidAdmission,
		"body_source", "current_exe_sub_325F9D0_request_sub_2261E30_ack_and_sub_1D82CF0_mode15_selection_notice")
	if err := s.sendGameUpperRawClassCodec(
		session,
		typ,
		party.BuildRequestPeerAck(),
		dnfproto.DefaultChannelClassification,
		true,
	); err != nil {
		return err
	}
	if parsed.Mode == 15 {
		// The town interaction menu reads the target actor's current party link,
		// not an invite/join flag from the mode-15 selection notice. Refresh that
		// link at click time from the target's authoritative runtime state so a
		// partyless target offers invite while an active target offers join. This
		// also repairs a stale client-side link left by an earlier party lifecycle.
		targetSession, online := s.onlineGameSession(parsed.TargetID)
		if !online {
			s.logGameEvent(session, "game-peer-selection-party-state-deferred",
				"source_char_id", sourceID,
				"target_id", parsed.TargetID,
				"reason", "target_resident_session_unavailable")
			return nil
		}
		targetParty := s.runtimePartyPeerSelectionState(targetSession)
		targetPartyActive := runtimePartySelectionTargetActive(targetParty)
		// A one-member party is invisible to its own owner and joinable by
		// nobody, so it has to be projected to the clicking client exactly like
		// the partyless case: the clearing record plus the 0xffff selection
		// marker. See runtimePartySelectionTargetActive for the live incident.
		projectedState := targetParty
		if !targetPartyActive {
			projectedState = alignedcmd.PartyState{}
		}
		projected, projectionErr := s.sendCurrentPartyActorFrameProjection(
			session,
			targetSession,
			projectedState,
			"peer_selection_dynamic_target_party_state",
		)
		if projectionErr != nil {
			return projectionErr
		}
		if !projected {
			s.logGameEvent(session, "game-peer-selection-party-state-deferred",
				"source_char_id", sourceID,
				"target_id", parsed.TargetID,
				"target_party_id", targetParty.PartyID,
				"reason", "target_actor_projection_unavailable")
			return nil
		}
		selectionBody := party.BuildRequestPeerSelectionNotice(parsed.TargetID, parsed.Value0, targetPartyActive)
		s.logGameEvent(session, "game-peer-selection-notice-send",
			"type", 0x0007,
			"source_char_id", sourceID,
			"target_id", parsed.TargetID,
			"target_party_id", targetParty.PartyID,
			"target_party_active", targetPartyActive,
			"projected_frame_party_id", projectedState.PartyID,
			"party_marker", func() string {
				if targetPartyActive {
					return "0000"
				}
				return "ffff"
			}(),
			"mode", parsed.Mode,
			"body_len", len(selectionBody),
			"body_source", "current_exe_sub_1D82CF0_mode15_party_marker_ffff_absent_else_present_then_sub_325FA70_opens_menu")
		return s.sendGameUpperRawClassCodec(session, 0x0007, selectionBody, 0, true)
	}

	// Mode 10 inside a team channel is attack-party admission, not a
	// four-member party invite. It must never reach the PartyManager path.
	if handled, err := s.handleRuntimeRaidPeerRequest(session, parsed); handled {
		return err
	}

	if !isForwardedPeerRequestMode(parsed.Mode) {
		return nil
	}
	sourceIdentity, sourceBound := s.boundGameSessionCharacterSnapshot(session)
	targetIdentity, targetBound := s.onlineGameSessionCharacterSnapshot(parsed.TargetID)
	if !sourceBound || !targetBound || targetIdentity.session == nil {
		return nil
	}
	// Direction. One op10 mode-0/12 shape carries both ordinary party requests,
	// so the packet cannot say whether the sender invited the target (邀请组队)
	// or applied to join the target's party (加入) -- the current client sends
	// the same bytes for both. What separates them is the mode-15 selection
	// notice that opened the menu: a target who already owns an ACTIVE party was
	// answered with party_marker 0000, which is exactly what makes the client
	// offer 加入 instead of 邀请组队 (see runtimePartySelectionTargetActive). Ask
	// the same question here, against the same projection, so the bridge cannot
	// read the click differently from the menu the player just used.
	//
	// Live 2026-09-26 13:23:17 is why this matters: char 2 applied to char 1's
	// party 4, the bridge read it as an invitation, and the acceptance moved
	// char 1 OUT of party 4 -- disbanding it, stranding char 20, and building a
	// fresh party for char 1 + char 2 instead of letting char 2 into party 4.
	joinApplication := isPartyInviteMode(parsed.Mode) &&
		runtimePartySelectionTargetActive(s.runtimePartyPeerSelectionState(targetIdentity.session))
	invitePartyID := uint16(0)
	inviteRecorded := false
	if manager := s.runtimePartyManagerForService(); manager != nil {
		if snapshot, found := manager.SnapshotByUser(sourceIdentity.character, sourceIdentity.generation); found {
			invitePartyID = snapshot.ID
		}
		if joinApplication {
			inviteRecorded = manager.RecordJoinApplication(
				targetIdentity.character,
				targetIdentity.generation,
				sourceIdentity.character,
				sourceIdentity.generation,
				invitePartyID,
				parsed.Mode,
			)
		} else {
			inviteRecorded = manager.RecordInvite(
				targetIdentity.character,
				targetIdentity.generation,
				sourceIdentity.character,
				sourceIdentity.generation,
				invitePartyID,
				parsed.Mode,
			)
		}
	}
	if !inviteRecorded {
		s.logGameEvent(session, "game-peer-request-invite-deferred",
			"type", typ,
			"source_char_id", sourceID,
			"target_id", parsed.TargetID,
			"mode", parsed.Mode,
			"reason", "session_generation_changed_before_invite_registration")
		return nil
	}
	if isCurrentAssaultMode(parsed.Mode) {
		if err := s.sendGameUpperRawClassCodec(targetIdentity.session, 443, party.BuildAssaultInvite(sourceID, parsed.Mode), 0, true); err != nil {
			return err
		}
		s.logGameEvent(session, "game-assault-invite-sent", "target_id", parsed.TargetID, "mode", parsed.Mode)
		return s.sendGameUpperRawClassCodec(session, 444, party.BuildAssaultInvite(parsed.TargetID, parsed.Mode), 0, true)
	}
	noticeBody := party.BuildRequestPeerNotice(sourceID, parsed, s.currentPeerNoticeStats(session, parsed))
	s.logGameEvent(session, "game-peer-request-notice-forward",
		"type", 0x0007,
		"source_char_id", sourceID,
		"target_id", parsed.TargetID,
		"mode", parsed.Mode,
		"join_application", joinApplication,
		"body_len", len(noticeBody),
		"body_source", "current_exe_sub_1D82CF0_mode_specific_peer_prompt")
	return s.sendGameUpperRawClassCodec(targetIdentity.session, 0x0007, noticeBody, 0, true)
}

func (s *Service) handleOnlineLeaveParty(session *gameSession, typ uint16) error {
	sourceID := selectedCharacterID(session)
	state := runtimePartyStateSnapshot(session)
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if sourceID == 0 || !bound {
		return nil
	}
	nextState, result, removed := s.leaveManagedRuntimeParty(identity)
	if !removed {
		// Do not let an unimportable one-member cache survive just because the
		// manager has already removed it.  A successful native leave ACK plus an
		// empty roster is idempotent and restores solo dungeon permission.
		s.clearRuntimePartyProjection(identity.character, identity.generation)
		s.logGameEvent(session, "game-party-leave-stale-projection-cleared",
			"type", typ,
			"source_char_id", sourceID,
			"reason", result.Reason)
		if err := s.sendGameUpperRawClassCodec(session, typ, []byte{1}, dnfproto.DefaultChannelClassification, true); err != nil {
			return err
		}
		return s.sendRuntimePartyMemberRemoved(session, uint16(state.PartyID), alignedcmd.PartyState{})
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, []byte{1}, dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	if err := s.sendRuntimePartyMemberRemoved(session, uint16(state.PartyID), nextState); err != nil {
		return err
	}
	if nextState.PartyID > 0 {
		s.syncRuntimePartyUDPRelay(nextState)
	} else {
		s.closeRuntimePartyUDPRelay(state.PartyID)
	}
	if err := s.broadcastRuntimePartyMemberRemoved(session, state, nextState, sourceID, session); err != nil {
		return err
	}
	// Leaving the party also leaves the attack squad: the raid roster must stop
	// claiming a squad this member is no longer in. See
	// unassignRuntimeRaidSquadOnPartyLeave.
	s.unassignRuntimeRaidSquadOnPartyLeave(sourceID)
	return nil
}

func (s *Service) handleOnlineWalkoutPartyMember(session *gameSession, typ uint16, body []byte) error {
	parsed, err := party.DecodeWalkoutPartyMemberRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-party-walkout-body-short", "type", typ, "body_len", len(body), "error", err)
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByUser(identity.character, identity.generation)
	if !found {
		snapshot, found = s.bootstrapManagedRuntimePartyForSession(session)
	}
	if !found {
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	if snapshot.Leader != identity.character {
		s.logGameEvent(session, "game-party-walkout-not-leader",
			"type", typ,
			"source_char_id", identity.character,
			"leader_id", snapshot.Leader,
			"slot", parsed.Slot)
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureAuthority)
	}
	var target party.RuntimePartyMember
	for _, member := range snapshot.Members {
		if member.Slot == parsed.Slot {
			target = member
			break
		}
	}
	if target.UserID == 0 || target.UserID == identity.character {
		s.logGameEvent(session, "game-party-walkout-invalid-slot",
			"type", typ,
			"source_char_id", identity.character,
			"slot", parsed.Slot,
			"member_count", len(snapshot.Members))
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	previousState := snapshot.StateFor(identity.character)
	result := manager.Kick(identity.character, identity.generation, target.UserID, target.SessionGeneration)
	// 踢人结果必须留痕。
	//
	// 以前这里是**完全静默**的：manager.Kick 的失败原因（invalid_kick / not_in_party /
	// stale_session / not_leader / target_not_member / stale_target_session）一个日志都
	// 不打，客户端只收到一个"无效"，玩家反馈成"踢不掉"，而排查时服务器上查不到任何
	// 证据。最典型的是 stale_target_session：被踢的人往往正是"连接中"那个（他刚重开
	// 过客户端），会话代际一变，踢人就永远失败。
	if !result.OK {
		s.logGameEvent(session, "game-party-kick-failed",
			"type", typ,
			"source_char_id", identity.character,
			"target_char_id", target.UserID,
			"target_slot", int(parsed.Slot),
			"source_generation", identity.generation,
			"target_generation", target.SessionGeneration,
			"party_id", previousState.PartyID,
			"member_count", len(snapshot.Members),
			"reason", result.Reason)
		s.recordRaidEvent(session, "party_kick_result", map[string]any{
			"party_id":          previousState.PartyID,
			"target_char_id":    target.UserID,
			"target_slot":       int(parsed.Slot),
			"member_count":      len(snapshot.Members),
			"kick_ok":           false,
			"kick_reason":       result.Reason,
			"source_generation": identity.generation,
			"target_generation": target.SessionGeneration,
		})
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	s.recordRaidEvent(session, "party_kick_result", map[string]any{
		"party_id":       previousState.PartyID,
		"target_char_id": target.UserID,
		"target_slot":    int(parsed.Slot),
		"member_count":   len(snapshot.Members),
		"kick_ok":        true,
	})
	nextState := s.publishRuntimePartyMembershipSnapshot(result.Party)
	if err := s.sendGameUpperRawClassCodec(session, typ, []byte{1}, dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	if err := s.broadcastRuntimePartyWalkoutNotice(session, previousState, parsed.Slot, 0); err != nil {
		return err
	}
	if nextState.PartyID <= 0 {
		s.closeRuntimePartyUDPRelay(int(result.Party.ID))
	} else {
		s.syncRuntimePartyUDPRelay(nextState)
	}
	// target.UserID is never the acting character here (rejected above), so a
	// failure below can only be the KICKED member's own socket. It must not
	// become the kicker's failure; see swallowPeerFanoutWrite.
	if targetSession, ok := s.onlineGameSession(target.UserID); ok {
		s.clearRuntimePartyProjection(target.UserID, target.SessionGeneration)
		if err := s.swallowPeerFanoutWrite(
			session,
			targetSession,
			"runtime_party_member_removed",
			0,
			s.sendRuntimePartyMemberRemoved(targetSession, uint16(previousState.PartyID), nextState),
		); err != nil {
			return err
		}
	}
	if err := s.broadcastRuntimePartyMemberRemoved(session, previousState, nextState, target.UserID, nil); err != nil {
		return err
	}
	// A kicked member left the party just like one that left on its own, so the
	// raid roster drops its squad label too.
	s.unassignRuntimeRaidSquadOnPartyLeave(target.UserID)
	return nil
}

// handleOnlineChangePartyHost implements the current client's real delegate-
// leader command. NoPack sends class1/op121 with one fixed party slot. The
// party model and member slots remain unchanged; only the trailing leader-slot
// byte of the complete op9 aggregate changes. Replaying op11 here would restart
// the already-connected P2P transport.
func (s *Service) handleOnlineChangePartyHost(session *gameSession, body []byte) error {
	if len(body) < 1 {
		return nil
	}
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return nil
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByUser(identity.character, identity.generation)
	if !found {
		snapshot, found = s.bootstrapManagedRuntimePartyForSession(session)
	}
	if !found || snapshot.Leader != identity.character {
		return nil
	}
	var target party.RuntimePartyMember
	for _, member := range snapshot.Members {
		if member.Slot == body[0] {
			target = member
			break
		}
	}
	if target.UserID == 0 || target.UserID == identity.character {
		return nil
	}
	result := manager.TransferLeader(identity.character, identity.generation, target.UserID, target.SessionGeneration)
	if !result.OK {
		return nil
	}
	state := s.publishRuntimePartySnapshot(result.Party)
	if err := s.sendManagedRuntimePartyAggregateRefresh(state, "op121_change_host"); err != nil {
		return err
	}
	s.logGameEvent(session, "game-party-change-host-committed",
		"source_char_id", identity.character,
		"new_leader_id", target.UserID,
		"selected_slot", body[0],
		"party_id", result.Party.ID,
		"member_count", len(result.Party.Members),
		"wire_contract", "same_version_stable_party_and_slots_op9_leader_slot_then_op153_without_op11")
	return nil
}

func (s *Service) handleOnlineChangePartyMemberPosition(session *gameSession, typ uint16, body []byte) error {
	parsed, err := party.DecodeChangePartyMemberPositionRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-party-change-position-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByUser(identity.character, identity.generation)
	if !found {
		snapshot, found = s.bootstrapManagedRuntimePartyForSession(session)
	}
	if !found || len(snapshot.Members) <= 1 {
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	if snapshot.Leader != identity.character {
		s.logGameEvent(session, "game-party-change-position-not-leader",
			"type", typ,
			"source_char_id", identity.character,
			"leader_id", snapshot.Leader,
			"slot", parsed.Slot,
			"position", parsed.Position)
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureAuthority)
	}
	fromSlot := parsed.Slot
	if _, sourceOK := runtimePartyMemberBySlot(snapshot.StateFor(identity.character), fromSlot); !sourceOK {
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	toSlot := int(fromSlot)
	switch parsed.Position {
	case 3:
		toSlot--
	case 1:
		toSlot++
	}
	maxMembers := snapshot.Settings.MaxMembers
	if maxMembers == 0 || maxMembers > 4 {
		maxMembers = 4
	}
	if toSlot < 0 || toSlot >= int(maxMembers) {
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	result := manager.Reposition(identity.character, identity.generation, fromSlot, byte(toSlot))
	if !result.OK {
		s.logGameEvent(session, "game-party-change-position-noop",
			"type", typ,
			"source_char_id", identity.character,
			"slot", parsed.Slot,
			"position", parsed.Position,
			"member_count", len(snapshot.Members))
		return s.sendPartyCommandFailure(session, typ, walkoutPartyFailureInvalid)
	}
	nextState := s.publishRuntimePartySnapshot(result.Party)
	if err := s.broadcastRuntimePartyChangePosition(session, nextState, fromSlot, byte(toSlot)); err != nil {
		return err
	}
	return nil
}

// swallowPeerFanoutWrite classifies one write that a party/raid fan-out made on
// behalf of a member session.
//
// A write failure belongs to the session that was being written, not to the
// session that is running the fan-out. When the target is another session the
// error is recorded and swallowed; only the origin's own socket failure keeps
// the original failure boundary (returned to the caller, which is what
// game_session_events.go turns into a closed connection via session.conn.Close()).
//
// Live incident 2026-09-25 06:02:14Z: leader 1111 (game-002957, channel 121)
// kicked a member. The fan-out still resolved char 37 to game-000333 -- the
// socket of the PREVIOUS raid generation, already closed at 05:37:50 -- and the
// op10 write returned "set tcp 10.1.0.16:10023: use of closed network
// connection". It travelled up through handleOnlineWalkoutPartyMember into
// game_session_events.go, whose fatal path calls session.conn.Close(), so the
// KICKER lost his OWN connection with 「网络连接中断」. Char 9 (06:02:33) and
// char 36 (06:04:26) were lost through the same stale-target write.
//
// Only the classification of a write that ALREADY failed changes here; a
// successful write still emits exactly the frame it emitted before, so no
// packet stream is altered.
func (s *Service) swallowPeerFanoutWrite(
	origin *gameSession,
	target *gameSession,
	source string,
	msgID uint16,
	err error,
) error {
	if err == nil {
		return nil
	}
	if origin == nil || target == nil || origin == target {
		// The player's own socket failed. Keep the established boundary: the
		// caller returns this error and the session is torn down for it.
		return err
	}
	s.logGameEvent(origin, "game-party-fanout-stale-target-write-skipped",
		"reason", "party_member_session_write_failed_is_not_the_origin_failure",
		"source", source,
		"target_conn_id", target.connID,
		"target_char_id", target.selectedCharacterID,
		"target_generation", target.characterGeneration,
		"msg_id", msgID,
		"error", err)
	return nil
}

// sendGameUpperRawClassCodecToPartyMember sends one fan-out copy of a party
// notice to one member session through swallowPeerFanoutWrite.
func (s *Service) sendGameUpperRawClassCodecToPartyMember(
	origin *gameSession,
	target *gameSession,
	msgID uint16,
	body []byte,
	classification byte,
	allowCodec bool,
) error {
	if target == nil {
		return nil
	}
	return s.swallowPeerFanoutWrite(
		origin,
		target,
		"party_notice_fanout",
		msgID,
		s.sendGameUpperRawClassCodec(target, msgID, body, classification, allowCodec),
	)
}

func (s *Service) broadcastRuntimePartyWalkoutNotice(origin *gameSession, state alignedcmd.PartyState, slot byte, mode byte) error {
	body := party.BuildWalkoutPartyMemberNotice(slot, mode)
	for _, member := range runtimePartyMembers(state) {
		session, ok := s.onlineGameSession(member.UserID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodecToPartyMember(origin, session, uint16(dnfenum.CmdPacketRequestPeer), body, dnfproto.DefaultChannelClassification, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) broadcastRuntimePartyChangePosition(origin *gameSession, state alignedcmd.PartyState, fromSlot byte, toSlot byte) error {
	body := party.BuildChangePartyMemberPositionAck(fromSlot, toSlot)
	for _, member := range runtimePartyMembers(state) {
		session, ok := s.onlineGameSession(member.UserID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodecToPartyMember(origin, session, uint16(dnfenum.CmdPacketChangePartyMemberPosition), body, dnfproto.DefaultChannelClassification, true); err != nil {
			return err
		}
		if err := s.sendRuntimePartyRosterLocal(session, state, "change_party_member_position_after_ack"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) handleOnlineReserveLeaveParty(session *gameSession, typ uint16, body []byte) error {
	parsed, err := party.DecodeReserveLeavePartyRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-party-reserve-leave-body-short", "type", typ, "body_len", len(body), "error", err)
		return s.sendGameUpperRawClassCodec(session, typ, []byte{0}, dnfproto.DefaultChannelClassification, true)
	}
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return s.sendGameUpperRawClassCodec(session, typ, []byte{0}, dnfproto.DefaultChannelClassification, true)
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByUser(identity.character, identity.generation)
	if !found {
		snapshot, found = s.bootstrapManagedRuntimePartyForSession(session)
	}
	if !found {
		return s.sendGameUpperRawClassCodec(session, typ, []byte{0}, dnfproto.DefaultChannelClassification, true)
	}
	bodyResp := party.BuildReserveLeavePartyAck(parsed.Flag, identity.character)
	for _, member := range snapshot.Members {
		targetSession, ok := s.onlineGameSession(member.UserID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodecToPartyMember(session, targetSession, typ, bodyResp, dnfproto.DefaultChannelClassification, true); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-party-reserve-leave-broadcast",
		"type", typ,
		"source_char_id", identity.character,
		"flag", parsed.Flag,
		"member_count", len(snapshot.Members))
	return nil
}

func (s *Service) handleOnlineResponsePeer(session *gameSession, typ uint16, body []byte) error {
	answer, err := decodeCurrentPeerResponseAnswer(body)
	if err != nil {
		s.logGameEvent(session, "game-party-response-peer-body-short", "type", typ, "body_len", len(body), "error", err)
		return nil
	}
	parsed := answer.Request
	s.logGameEvent(session, "game-party-response-peer-received",
		"type", typ,
		"source_char_id", selectedCharacterID(session),
		"target_id", parsed.TargetID,
		"mode", parsed.Mode,
		"value", parsed.Value,
		"refused", answer.Refused,
		"refusal_reason", answer.Reason)
	if handled, err := s.handleRuntimeRaidPeerResponse(session, parsed, !answer.Refused); handled {
		return err
	}
	if !isForwardedPeerRequestMode(parsed.Mode) {
		return nil
	}
	if isCurrentAssaultMode(parsed.Mode) && parsed.Value > 1 {
		return nil
	}
	acceptorIdentity, acceptorBound := s.boundGameSessionCharacterSnapshot(session)
	targetIdentity, targetBound := s.onlineGameSessionCharacterSnapshot(parsed.TargetID)
	peerRequest := party.RuntimePartyPeerRequestContext{}
	requestConsumed := false
	if acceptorBound && targetBound {
		if manager := s.runtimePartyManagerForService(); manager != nil {
			peerRequest, requestConsumed = manager.ConsumePeerRequest(
				acceptorIdentity.character,
				acceptorIdentity.generation,
				targetIdentity.character,
				targetIdentity.generation,
				parsed.Mode,
			)
		}
	}
	if !requestConsumed {
		s.logGameEvent(session, "game-peer-response-without-pending-request",
			"type", typ,
			"source_char_id", selectedCharacterID(session),
			"target_id", parsed.TargetID,
			"mode", parsed.Mode)
		return nil
	}
	_ = peerRequest.PartyID // recorded generation is revalidated by create/join below.
	if isCurrentAssaultMode(parsed.Mode) {
		return s.respondCurrentAssault(session, targetIdentity.session, parsed)
	}
	ackPayload := party.BuildResponsePeerAckPayload(parsed.TargetID, parsed.Mode)
	if err := s.sendGameUpperSuccess(session, typ, ackPayload); err != nil {
		return err
	}
	targetSession := targetIdentity.session
	if targetSession == nil || session == nil || session.selectedCharacterID == 0 {
		s.logGameEvent(session, "game-party-response-peer-target-offline",
			"type", typ,
			"source_char_id", selectedCharacterID(session),
			"target_id", parsed.TargetID)
		return nil
	}
	if answer.Refused || peerResponseRejected(parsed) {
		reason := answer.Reason
		if reason == 0 {
			reason = party.ResponsePeerRefusalReason
		}
		s.logGameEvent(session, "game-peer-response-rejected",
			"type", typ,
			"source_char_id", selectedCharacterID(session),
			"target_id", parsed.TargetID,
			"mode", parsed.Mode,
			"value", parsed.Value,
			"reason", reason)
		return nil
	}
	if parsed.Mode == 1 {
		// Class-0/op8 is the native generic peer-action notification used by
		// item trade. Party invitation responses do not send it: the same-version
		// flow completes the invitee with class-1/op11, then publishes the native
		// op11 -> op9 -> op153 party frame directly to both members. Injecting an
		// op8 into the inviter's stream enters the generic peer-action controller
		// immediately before party-model creation and leaves that client bound to
		// a different local roster than the invitee.
		responseNotice := party.BuildResponsePeerNotice(session.selectedCharacterID, parsed.Mode, parsed.Value)
		if err := s.sendGameUpperRawClassCodec(targetSession, 0x0008, responseNotice, 0, true); err != nil {
			return err
		}
		s.beginOnlineItemTrade(session, targetSession)
		s.logGameEvent(session, "game-trade-response-peer-forwarded",
			"type", typ,
			"source_char_id", session.selectedCharacterID,
			"target_id", parsed.TargetID,
			"mode", parsed.Mode,
			"value", parsed.Value,
			"body_source", "current_exe_class1_op11_trade_ack_and_class0_op8_peer_notice")
		return nil
	}
	if !isPartyInviteResponse(parsed) {
		return nil
	}
	// Whose party survives is not decided by who answered: it was decided when
	// the request arrived, and the recorded direction says which way it goes.
	//
	// An INVITATION (op7/marker 邀请组队) moves the answering player -- the op11
	// sender -- into the requester's party. A JOIN APPLICATION (op7/marker 加入)
	// moves the requester into the answering player's party instead, because the
	// answering player here is the owner of the party that was applied to: he
	// accepted an application, he did not accept an invitation. Swapping the two
	// is the 2026-09-26 defect -- the leader was detached from his own party and
	// the two players who applied/got applied to ended up in a new one.
	joiner, anchor := session, targetSession
	if peerRequest.JoinApplication {
		joiner, anchor = targetSession, session
	}
	state, result, ok := s.createOrJoinManagedRuntimeParty(joiner, anchor)
	if !ok {
		s.logGameEvent(session, "game-party-response-peer-party-full",
			"type", typ,
			"source_char_id", session.selectedCharacterID,
			"target_id", parsed.TargetID,
			"mode", parsed.Mode,
			"join_application", peerRequest.JoinApplication,
			"reason", result.Reason)
		return nil
	}
	if peerRequest.JoinApplication {
		// The applicant never answers an op11 of its own, so this event is the
		// only place its acceptance is visible without reading the party frame.
		s.logGameEvent(session, "game-party-join-application-accepted",
			"type", typ,
			"decider_char_id", session.selectedCharacterID,
			"applicant_char_id", parsed.TargetID,
			"party_id", state.PartyID,
			"leader_char_id", state.UserID,
			"member_count", len(runtimePartyMembers(state)))
	}
	return s.sendManagedRuntimePartySnapshots(state)
}

func (s *Service) handleOnlineEntryIntoParty(session *gameSession, typ uint16, body []byte) error {
	parsed, err := party.DecodeEntryIntoPartyRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-party-entry-body-short", "type", typ, "body_len", len(body), "error", err)
		return s.sendPartyCommandFailure(session, typ, entryIntoPartyFailureGeneric)
	}
	if session == nil || session.selectedCharacterID == 0 || parsed.TargetID == 0 || parsed.TargetID > 0xffff {
		s.logGameEvent(session, "game-party-entry-invalid-context",
			"type", typ,
			"source_char_id", selectedCharacterID(session),
			"target_id", parsed.TargetID)
		return s.sendPartyCommandFailure(session, typ, entryIntoPartyFailureInvalid)
	}
	targetID := uint16(parsed.TargetID)
	if targetID == session.selectedCharacterID {
		s.logGameEvent(session, "game-party-entry-self-rejected", "type", typ, "char_id", session.selectedCharacterID)
		return s.sendPartyCommandFailure(session, typ, entryIntoPartyFailureInvalid)
	}
	targetSession, ok := s.onlineGameSession(targetID)
	if !ok {
		s.logGameEvent(session, "game-party-entry-target-offline",
			"type", typ,
			"source_char_id", session.selectedCharacterID,
			"target_id", targetID)
		return s.sendPartyCommandFailure(session, typ, entryIntoPartyFailureGeneric)
	}
	// op706 is emitted by the existing party owner while it attaches the
	// selected target. Its source is therefore the anchor/leader, unlike the
	// op8 invite response where the source is the accepting joiner.
	state, result, ok := s.createOrJoinManagedRuntimeParty(targetSession, session)
	if !ok {
		s.logGameEvent(session, "game-party-entry-party-full",
			"type", typ,
			"source_char_id", session.selectedCharacterID,
			"target_id", targetID,
			"reason", result.Reason)
		return s.sendPartyCommandFailure(session, typ, entryIntoPartyFailureFull)
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, party.BuildEntryIntoPartyAck(parsed.TargetID, uint32(session.selectedCharacterID)), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	if err := s.sendManagedRuntimePartySnapshots(state); err != nil {
		return err
	}
	s.logGameEvent(session, "game-party-entry-linked-online-sessions",
		"type", typ,
		"source_char_id", session.selectedCharacterID,
		"target_id", targetID,
		"member_count", len(state.Members))
	return nil
}

func (s *Service) handleOnlineEntryIntoPartyFinish(session *gameSession, typ uint16) error {
	state := runtimePartyStateSnapshot(session)
	if state.PartyID > 0 && len(state.Members) > 1 {
		userState := state.UserState
		if userState == 0 {
			userState = 1
		}
		return s.sendGameUpperRawClassCodec(session, typ, party.BuildEntryIntoPartyFinishEmptyBody(userState), 0, false)
	}
	s.logGameEvent(session, "game-party-entry-finish-missing-context", "type", typ, "source_char_id", selectedCharacterID(session))
	// Current EXE registers only the class0 state/count reader for op706. It has
	// no class1 failure envelope, so the old {0,error} reply would be parsed as
	// state/count and consume bytes from following packets.
	return nil
}

func isPartyInviteMode(mode byte) bool {
	return mode == 0 || mode == 12
}

func isPartyInviteResponse(req party.ResponsePeerRequest) bool {
	return isPartyInviteMode(req.Mode)
}

func isForwardedPeerRequestMode(mode byte) bool {
	return mode == 0 || mode == 1 || mode == 4 || mode == 12 || mode == 13
}

func peerResponseRejected(req party.ResponsePeerRequest) bool {
	// Current EXE sub_269FBF0 accepts ordinary mode-0/1/12 prompts by sending
	// value zero. Street-assault modes 4/13 use explicit value 1 for accept and
	// value 0 for reject (sub_2114B10/sub_2114BB0).
	return (req.Mode == 4 || req.Mode == 13) && req.Value == 0
}

// runtimePartySelectionTargetActive decides whether the interaction menu opened on
// one character treats that character as somebody who already has a party.
//
// A ONE-MEMBER party is deliberately kept by this server -- the native kind-2
// transition is what turns it into the client's solo window, and it keeps dungeon
// authority coherent (publishRuntimePartyMembershipSnapshot, and the sole-member
// rule in party_forced_exit_leave_test.go). But that client never renders a party
// for a single member: its owner sees no party window and therefore has no 离队 to
// press. Reporting such a party as active here told every OTHER player's menu "he
// already has a party", which removes the 邀请 entry -- so nobody could group with
// him and he could not get out of the state either.
//
// Live 2026-09-20, channel 23, four operators (raid 1507338/1507329): 浅笑 (char 12)
// was left alone in party 23 when his teammate moved to another party. Clicking him
// produced
//
//	game-current-party-frame-op9-send … source=peer_selection_dynamic_target_party_state
//	    char_id=12 party_model_key=23 party_active=true slot_count=1
//	game-peer-selection-notice-send … target_party_id=23 target_party_active=true party_marker=0000
//
// while the same click on char 9 produced target_party_id=0 party_marker=ffff (invite).
// The operator's report was exactly that: "我们三个怎么组都没问题 就是组不上他", and the
// person himself could neither see nor leave his party.
//
// Reporting him as partyless is all that is needed: accepting an invite detaches him
// from the stale party inside the same manager lock (RuntimePartyManager.Join calls
// leaveLocked for the prior membership), so the leftover dissolves by itself.
func runtimePartySelectionTargetActive(state alignedcmd.PartyState) bool {
	return state.PartyID > 0 && len(runtimePartyMembers(state)) > 1
}

// runtimePartyPeerSelectionState answers "what party does this character's client
// think it has" for the two peer-selection steps that have to agree with each
// other: the mode-15 marker that makes the client draw 邀请组队 or 加入, and the
// mode-0/12 request that follows the click. Both read this one projection, so a
// menu that offered 加入 cannot be answered as an invitation.
//
// The central manager is authoritative and is asked first. The per-session cache
// is the fallback for a client whose projection predates the manager (the party
// lifecycle tests build sessions directly) and for a first-contact packet from a
// window opened before the manager existed.
func (s *Service) runtimePartyPeerSelectionState(session *gameSession) alignedcmd.PartyState {
	if session == nil {
		return alignedcmd.PartyState{}
	}
	if identity, bound := s.boundGameSessionCharacterSnapshot(session); bound {
		if manager := s.runtimePartyManagerForService(); manager != nil {
			if snapshot, found := manager.SnapshotByUser(identity.character, identity.generation); found {
				return snapshot.StateFor(identity.character)
			}
		}
	}
	return runtimePartyStateSnapshot(session)
}

func (s *Service) sendPartyCommandFailure(session *gameSession, typ uint16, code byte) error {
	return s.sendGameUpperRawClassCodec(session, typ, []byte{0, code}, dnfproto.DefaultChannelClassification, true)
}

func (s *Service) sendRuntimePartySnapshot(session *gameSession, state alignedcmd.PartyState) error {
	frameSent, err := s.sendRuntimePartySnapshotLocal(session, state)
	if err != nil || !frameSent {
		return err
	}
	return s.broadcastCurrentPartyActorProjection(session, state, "runtime_party_snapshot")
}

func (s *Service) sendRuntimePartySnapshotLocal(session *gameSession, state alignedcmd.PartyState) (bool, error) {
	partyActive := state.PartyID > 0 && len(runtimePartyMembers(state)) > 0
	var beforeFrame func() error
	if partyActive && len(runtimePartyMembers(state)) > 1 {
		if !s.runtimePartyPublishablePeerEndpoints(session, state) {
			s.logGameEvent(session, "game-runtime-party-snapshot-deferred",
				"char_id", selectedCharacterID(session),
				"party_id", state.PartyID,
				"member_count", len(runtimePartyMembers(state)),
				"reason", "same_version_native_peer_endpoint_table_incomplete_before_first_op9")
			return false, nil
		}
		beforeFrame = func() error {
			return s.sendRuntimePartyPeerEndpointInfoLocal(session, state, "single_session_snapshot")
		}
	}
	frameSent, err := s.sendCurrentPartyActorFrameProjectionBefore(
		session,
		session,
		state,
		"runtime_party_snapshot",
		beforeFrame,
	)
	if err != nil {
		return false, err
	}
	if !frameSent {
		return false, nil
	}
	if !partyActive {
		if err := s.sendGameUpperRawClassCodec(session, 0x0099, party.BuildEmptyRealtimeInfo(), 0, true); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := s.sendGameUpperRawClassCodec(session, 0x0099, party.BuildSingleMemberRealtimeInfo(state), 0, true); err != nil {
		return false, err
	}
	return true, nil
}

// sendRuntimePartyPeerEndpointInfoLocal installs the native reliable-UDP peer
// table without starting the scene-side party actor. Multi-session publishers
// use this as phase one for every member before any op9 is emitted: current
// NoPack starts its native peer controller when op9 binds the party actor, so
// allowing the first member to receive op9 while another member still lacks
// op11 deterministically drops the first handshake and leaves a one-way link.
func (s *Service) sendRuntimePartyPeerEndpointInfoLocal(
	session *gameSession,
	state alignedcmd.PartyState,
	source string,
) error {
	if session == nil {
		return errors.New("runtime party peer endpoint target is unavailable")
	}
	if state.PartyID <= 0 || len(runtimePartyMembers(state)) <= 1 {
		return nil
	}
	if !s.runtimePartyPublishablePeerEndpoints(session, state) {
		return fmt.Errorf("runtime party %d peer endpoint table is incomplete for member %d",
			state.PartyID, selectedCharacterID(session))
	}
	body := s.buildRuntimePartyPeerEndpointInfoForReceiver(session, state)
	s.logPacketEvent("game-runtime-party-peer-endpoints-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"party_id", state.PartyID,
		"peer_count", body[0],
		"body_len", len(body),
		"msg_id", 0x000b,
		"classification", 0,
		"source", source,
		"ordering", "all_members_op11_barrier_before_any_op9_then_op153")
	return s.sendGameUpperRawClassCodec(session, 0x000b, body, 0, true)
}

// sendRuntimePartyRosterLocal refreshes the current client's scene-owned
// party tables. The complete op9 aggregate owns fixed slots and leadership;
// op153 follows it with the P2P connection percentage used by the HUD/minimap.
// The dynamic UDP endpoints in op11 are a party-formation handshake, not a
// general scene refresh: replaying op11 during every town/selector transition
// makes the current client tear down a healthy peer link and enter
// "connecting" again. Therefore scene reconstruction is strictly
// op9-then-op153, without an op11 re-handshake.
func (s *Service) sendRuntimePartyRosterLocal(session *gameSession, state alignedcmd.PartyState, source string) error {
	if _, err := s.sendCurrentPartyActorFrameProjection(session, session, state, source); err != nil {
		return err
	}
	return s.sendRuntimePartyRealtimeInfoLocal(session, state)
}

func (s *Service) buildRuntimePartyPeerEndpointInfo(state alignedcmd.PartyState) []byte {
	members := runtimePartyMembers(state)
	endpoints := make([]party.PeerEndpoint, 0, len(members))
	for _, member := range members {
		if member.UserID == 0 {
			continue
		}
		endpoint := s.runtimePartyPeerEndpoint(member, [4]byte{127, 0, 0, 1}, defaultPartyPeerPort)
		if peer, ok := s.onlineGameSession(member.UserID); ok && peer.conn != nil {
			registration := currentPartyPeerEndpointSnapshot(peer)
			if registration.Port != 0 {
				endpoint.Port = registration.Port
			}
			if registration.InnerIPv4 != [4]byte{} {
				endpoint.InnerIPv4 = registration.InnerIPv4
			}
			if registration.OuterIPv4 != [4]byte{} {
				endpoint.OuterIPv4 = registration.OuterIPv4
			}
			if tcpAddress, ok := peer.conn.RemoteAddr().(*net.TCPAddr); ok {
				if ipv4 := tcpAddress.IP.To4(); len(ipv4) == net.IPv4len {
					copy(endpoint.OuterIPv4[:], ipv4)
				}
			}
		}
		endpoints = append(endpoints, endpoint)
	}
	return party.BuildPeerEndpointInfo(endpoints)
}

func (s *Service) buildRuntimePartyPeerEndpointInfoDirectForReceiver(receiver *gameSession, state alignedcmd.PartyState) []byte {
	members := runtimePartyMembers(state)
	endpoints := make([]party.PeerEndpoint, 0, len(members))
	for _, member := range members {
		if member.UserID == 0 || receiver != nil && member.UserID == receiver.selectedCharacterID {
			continue
		}
		endpoint := s.runtimePartyPeerEndpoint(member, [4]byte{127, 0, 0, 1}, defaultPartyPeerPort)
		if peer, ok := s.onlineGameSession(member.UserID); ok && peer.conn != nil {
			registration := currentPartyPeerEndpointSnapshot(peer)
			if registration.Port != 0 {
				endpoint.Port = registration.Port
			}
			if registration.InnerIPv4 != [4]byte{} {
				endpoint.InnerIPv4 = registration.InnerIPv4
			}
			if registration.OuterIPv4 != [4]byte{} {
				endpoint.OuterIPv4 = registration.OuterIPv4
			}
			if tcpAddress, ok := peer.conn.RemoteAddr().(*net.TCPAddr); ok {
				if ipv4 := tcpAddress.IP.To4(); len(ipv4) == net.IPv4len {
					copy(endpoint.OuterIPv4[:], ipv4)
				}
			}
		}
		// The same-version 90CN directory publishes NAT type 1 for the proven
		// direct path. Zero is not a neutral value here: NoPack treats it as an
		// unresolved transport and leaves the member row in "connecting".
		endpoint.NATType = 1
		endpoints = append(endpoints, endpoint)
	}
	return party.BuildPeerEndpointInfo(endpoints)
}

// runtimePartyNativePeerEndpointsReady enforces the same admission boundary
// as the 90CN DirectorySessionTracker: every remote row must already have an
// authenticated, non-zero native probe endpoint before the first op9 membership
// update starts NoPack's P2P controller. Publishing a made-up port cannot be
// repaired by a later op11 because the client does not restart that member.
func (s *Service) runtimePartyNativePeerEndpointsReady(receiver *gameSession, state alignedcmd.PartyState) bool {
	for _, member := range runtimePartyMembers(state) {
		if member.UserID == 0 || receiver != nil && member.UserID == receiver.selectedCharacterID {
			continue
		}
		peer, ok := s.onlineGameSession(member.UserID)
		if !ok || peer == nil || peer.conn == nil {
			return false
		}
		registration := currentPartyPeerEndpointSnapshot(peer)
		if registration.Port == 0 {
			return false
		}
		routingKey, pathMTU := s.partyTransportIdentity(peer)
		if routingKey == 0 || !currentPartyPathMTU(pathMTU) {
			return false
		}
	}
	return true
}

// runtimePartyPublishablePeerEndpoints reports whether the op11 peer endpoint
// table this service would actually publish to receiver can be built right now.
//
// It is deliberately the union of the two tables
// buildRuntimePartyPeerEndpointInfoForReceiver can produce, and it must stay in
// step with that function: a gate that admits only one of them strands the
// parties that can only be built from the other. The direct table needs every
// remote member's native probe endpoint; the relay table needs only the
// directed legs, whose ingress is authenticated by the peer's TCP IPv4 and
// session generation. Checking only the native table is what left a party with
// one unprobed member permanently unpublished -- the client shows 连接中 forever
// because no op11/op9/op153 was ever sent, even though the relay could carry it.
func (s *Service) runtimePartyPublishablePeerEndpoints(receiver *gameSession, state alignedcmd.PartyState) bool {
	if s.runtimePartyNativePeerEndpointsReady(receiver, state) {
		return true
	}
	return s.runtimePartyUDPRelayPeerEndpointsReady(receiver, state)
}

// runtimePartyUDPRelayPeerEndpointsReady reports whether every remote member of
// state has a directed relay leg from receiver, which is exactly what
// buildRuntimePartyPeerEndpointInfoForReceiver's relay branch needs to publish a
// complete table. A missing probe port does not disqualify a member here: its leg
// still exists and learns the return endpoint from live traffic.
func (s *Service) runtimePartyUDPRelayPeerEndpointsReady(receiver *gameSession, state alignedcmd.PartyState) bool {
	if receiver == nil || receiver.selectedCharacterID == 0 {
		return false
	}
	if state.PartyID <= 0 || state.PartyID >= int(^uint16(0)) || len(runtimePartyMembers(state)) <= 1 {
		return false
	}
	relay := s.currentPartyUDPRelay()
	if relay == nil || !relay.Enabled() {
		return false
	}
	if !s.syncRuntimePartyUDPRelay(state) {
		return false
	}
	for _, member := range runtimePartyMembers(state) {
		if member.UserID == 0 || member.UserID == receiver.selectedCharacterID {
			continue
		}
		if _, _, ok := relay.Endpoint(uint16(state.PartyID), receiver.selectedCharacterID, member.UserID); !ok {
			return false
		}
	}
	return true
}

// buildRuntimePartyPeerEndpointInfoForReceiver publishes the same-version
// 90CN native PARTY_IP_INFO table: remote members only, using each remote
// connection's authenticated native-probe UDP endpoint. NoPack owns the
// receiver's local identity from login success and must contact the peer
// endpoint directly; inserting a server relay changes the native hole-punch
// state machine and can strand both HUD rows in "connecting".
func (s *Service) buildRuntimePartyPeerEndpointInfoForReceiver(receiver *gameSession, state alignedcmd.PartyState) []byte {
	// The relay is a real deployment setting, not a dormant escape hatch: the
	// code default is off (options.go reads
	// DNFBRIDGE_PARTY_UDP_RELAY_ENABLED as false), but the shipped profile turns
	// it on -- runtime\config\instance.json carries
	// "partyUdpRelayEnabled": true with ports 30000+64, measured in the live
	// deployment on 2026-09-20. So this branch is the one normal play takes, and
	// the native branch below is the fallback. It publishes a relay table only
	// after the whole directed topology was installed; partial relay rows still
	// fall back to the same-version direct table.
	if relay := s.currentPartyUDPRelay(); relay != nil && relay.Enabled() && receiver != nil &&
		state.PartyID > 0 && state.PartyID < int(^uint16(0)) {
		_ = s.syncRuntimePartyUDPRelay(state)
		members := runtimePartyMembers(state)
		endpoints := make([]party.PeerEndpoint, 0, len(members))
		complete := true
		for _, member := range members {
			if member.UserID == 0 || member.UserID == receiver.selectedCharacterID {
				continue
			}
			address, port, ok := relay.Endpoint(
				uint16(state.PartyID),
				receiver.selectedCharacterID,
				member.UserID,
			)
			if !ok {
				complete = false
				break
			}
			endpoint := s.runtimePartyPeerEndpoint(member, address, port)
			endpoint.NATType = 1
			endpoints = append(endpoints, endpoint)
		}
		if complete && len(endpoints) > 0 {
			return party.BuildPeerEndpointInfo(endpoints)
		}
	}
	return s.buildRuntimePartyPeerEndpointInfoDirectForReceiver(receiver, state)
}

func (s *Service) runtimePartyPeerEndpoint(member alignedcmd.PartyMemberState, address [4]byte, port uint16) party.PeerEndpoint {
	endpoint := party.PeerEndpoint{
		UserID:    member.UserID,
		InnerIPv4: address,
		OuterIPv4: address,
		Port:      port,
		NATType:   1,
		PathMTU:   currentPartyPeerDefaultPathMTU,
		UserState: member.UserState,
	}
	if endpoint.UserState == 0 || endpoint.UserState == 1 {
		endpoint.UserState = 100
	}
	if peer, ok := s.onlineGameSession(member.UserID); ok {
		routingKey, pathMTU := s.partyTransportIdentity(peer)
		// The route key is the remote connection's native identity from its
		// login success. Sharing the outer compatibility token across members
		// aliases both actors to one reliable-UDP route and leaves them forever
		// in the connecting state.
		endpoint.RoutingKey = routingKey
		if currentPartyPathMTU(pathMTU) {
			endpoint.PathMTU = pathMTU
		}
	}
	// Tests and compatibility imports can construct an active party before a
	// live session has completed its login handshake. Keep the row structurally
	// valid while production always uses the assigned connection identity.
	if endpoint.RoutingKey == 0 {
		endpoint.RoutingKey = uint32(member.UserID)
	}
	return endpoint
}

func (s *Service) currentPartyUDPRelay() *currentPartyUDPRelay {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	relay := s.partyUDPRelay
	s.mu.Unlock()
	return relay
}

// syncRuntimePartyUDPRelay ensures that the directed relay set exactly
// matches the current live party. It refuses to use a relay until every party
// member still owns the selected TCP session and that session has an observed
// IPv4 endpoint; the caller then safely falls back to the proven LAN form.
func (s *Service) syncRuntimePartyUDPRelay(state alignedcmd.PartyState) bool {
	return s.syncRuntimePartyUDPRelayLegs(state, true)
}

// syncRuntimePartyUDPRelayLegs installs the party relay's forwarding table.
//
// A member whose UDP probe answer has not been observed yet no longer aborts the whole
// sync. Aborting left every squadmate frozen in place for the party's entire life --
// live 2026-09-20, raid parties 15 and 16 carried ZERO relay packets from creation to
// dissolution while the re-formed party 17 relayed immediately, and the operator's
// report is exactly that: "我们两个人互相看不到对方在动 … 重新组队后 就又可以了".
// The legs that ARE known start forwarding now, the gap is logged loudly, and a bounded
// retry re-syncs once the missing registration lands.
func (s *Service) syncRuntimePartyUDPRelayLegs(state alignedcmd.PartyState, allowRetry bool) bool {
	relay := s.currentPartyUDPRelay()
	if state.PartyID > 0 && state.PartyID < int(^uint16(0)) {
		manager := s.runtimePartyManagerForService()
		snapshot, found := manager.SnapshotByID(uint16(state.PartyID))
		if !found {
			return false
		}
		state = snapshot.StateFor(snapshot.Leader)
	}
	members := runtimePartyMembers(state)
	if relay == nil || !relay.Enabled() || state.PartyID <= 0 || len(members) < 2 {
		return false
	}
	bindings := make([]currentPartyUDPRelayBinding, 0, len(members))
	missing := make([]uint16, 0, len(members))
	for _, member := range members {
		identity, ok := s.onlineGameSessionCharacterSnapshot(member.UserID)
		if !ok || identity.session == nil || identity.generation == 0 || identity.session.conn == nil {
			return false
		}
		tcpAddress, ok := identity.session.conn.RemoteAddr().(*net.TCPAddr)
		if !ok || tcpAddress.IP == nil {
			return false
		}
		ipv4 := tcpAddress.IP.To4()
		if len(ipv4) != net.IPv4len {
			return false
		}
		registration := currentPartyPeerEndpointSnapshot(identity.session)
		if registration.Port == 0 {
			// The client's UDP probe answer has not been observed yet. This member's leg
			// is still installed: a relay leg is keyed by the authenticated TCP identity
			// (character + session generation) and by the TCP-owned IPv4 that
			// readLoop enforces on ingress, never by the probe port. Leaving the port at
			// zero only means the leg has no *seed* return endpoint and learns it from the
			// member's first datagram (newLegLocked already supports exactly that).
			//
			// Skipping the member instead is what made one unprobed client unlink the
			// whole party: relay.Endpoint() then failed for every receiver, so
			// buildRuntimePartyPeerEndpointInfoForReceiver fell back to the direct table,
			// runtimePartyNativePeerEndpointsReady refused that table too, and
			// sendManagedRuntimePartySnapshots returned without publishing anything at
			// all. The party stayed half-formed on the wire with the acceptor holding a
			// class1 ACK and nobody receiving op11/op9/op153 -- the operator's "组队显示
			// 连接中" (live 2026-09-29, server 203.0.113.12: party 48 member 32, twelve
			// deferred snapshots and ten unresolved leg retries).
			missing = append(missing, member.UserID)
		}
		binding := currentPartyUDPRelayBinding{
			characterID: identity.character,
			generation:  identity.generation,
			port:        registration.Port,
		}
		copy(binding.address[:], ipv4)
		bindings = append(bindings, binding)
	}
	if len(missing) != 0 {
		s.logPacketEvent("game-party-udp-relay-leg-missing",
			"party_id", state.PartyID,
			"missing_members", missing,
			"bound_members", len(bindings),
			"member_count", len(members),
			"reason", "the_client_udp_probe_answer_for_that_member_has_not_been_observed",
			"resolution", "leg_installed_without_seed_endpoint_and_learned_from_live_datagram")
		s.recordRaidEvent(nil, "relay_leg_missing", map[string]any{
			"party_id":        state.PartyID,
			"missing_members": missing,
			"member_count":    len(members),
			"bound_members":   len(bindings),
			"result":          "legs_installed_without_seed",
			"reason":          "the_client_udp_probe_answer_for_that_member_has_not_been_observed",
			"client_visible":  "party_still_forms_over_the_relay_instead_of_showing_connecting",
		})
		if allowRetry {
			s.schedulePartyUDPRelayLegRetry(state)
		}
	}
	return relay.Sync(uint16(state.PartyID), bindings)
}

func (s *Service) closeRuntimePartyUDPRelay(partyID int) {
	if partyID <= 0 || partyID > int(^uint16(0)) {
		return
	}
	if relay := s.currentPartyUDPRelay(); relay != nil {
		relay.CloseParty(uint16(partyID))
	}
}

func (s *Service) sendRuntimePartyRealtimeInfoLocal(session *gameSession, state alignedcmd.PartyState) error {
	if session == nil || state.PartyID <= 0 || len(runtimePartyMembers(state)) == 0 {
		return nil
	}
	return s.sendGameUpperRawClassCodec(session, 0x0099, party.BuildSingleMemberRealtimeInfo(state), 0, true)
}

func (s *Service) broadcastRuntimePartyMemberRemoved(origin *gameSession, previousState, nextState alignedcmd.PartyState, removedID uint16, skip *gameSession) error {
	if nextState.PartyID <= 0 {
		// The party is gone, not merely smaller: the members it left behind must
		// come out of it too. See publishRuntimePartyDisbandToSurvivors.
		return s.publishRuntimePartyDisbandToSurvivors(
			origin,
			previousState,
			removedID,
			skip,
			"broadcast_runtime_party_member_removed",
		)
	}
	members := runtimePartyMembers(nextState)
	for _, member := range members {
		if member.UserID == removedID {
			continue
		}
		session, ok := s.onlineGameSession(member.UserID)
		if !ok || session == skip {
			continue
		}
		storeRuntimePartyState(session, nextState)
		if err := s.swallowPeerFanoutWrite(
			origin,
			session,
			"runtime_party_membership_reduction",
			0,
			s.sendRuntimePartyMembershipReduction(session, nextState),
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendRuntimePartyMemberRemoved(session *gameSession, previousPartyID uint16, remaining alignedcmd.PartyState) error {
	if session == nil || previousPartyID == 0 {
		return nil
	}
	if remaining.PartyID > 0 && len(runtimePartyMembers(remaining)) > 0 {
		if err := s.sendRuntimePartyMembershipBody(session, remaining, 2); err != nil {
			return err
		}
	} else {
		body := buildCurrentSceneOp9ActorRemovalBodyInContext(previousPartyID, currentPartySceneOwnerContext(session))
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRecoverStamina), body, 0); err != nil {
			return err
		}
	}
	if err := s.sendGameUpperRawClassCodec(session, 0x0099, party.BuildEmptyRealtimeInfo(), 0, true); err != nil {
		return err
	}
	return s.broadcastCurrentPartyActorProjection(session, alignedcmd.PartyState{}, "runtime_party_member_removed")
}

func (s *Service) sendRuntimePartyMembershipReduction(session *gameSession, state alignedcmd.PartyState) error {
	if err := s.sendRuntimePartyMembershipBody(session, state, 2); err != nil {
		return err
	}
	return s.sendGameUpperRawClassCodec(session, 0x0099, party.BuildSingleMemberRealtimeInfo(state), 0, true)
}

func (s *Service) sendRuntimePartyMembershipBody(session *gameSession, state alignedcmd.PartyState, kind byte) error {
	if session == nil || state.PartyID <= 0 || state.PartyID >= int(^uint16(0)) {
		return fmt.Errorf("runtime party membership frame has invalid party %d", state.PartyID)
	}
	members := runtimePartyMembers(state)
	leader, found := runtimePartyMemberSlotByUserID(members, state.UserID)
	if !found {
		return fmt.Errorf("runtime party %d membership frame missing leader %d", state.PartyID, state.UserID)
	}
	body := buildCurrentSceneOp9PartyMembershipBodyInContext(
		uint16(state.PartyID), currentPartySceneOwnerContext(session), members, leader, kind,
	)
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRecoverStamina), body, 0)
}

func runtimePartyMemberSlotByUserID(members []alignedcmd.PartyMemberState, userID uint16) (byte, bool) {
	for _, member := range members {
		if member.UserID == userID {
			return member.Slot, true
		}
	}
	return 0, false
}

func (s *Service) sendCurrentPartyFrameProjection(session *gameSession, state alignedcmd.PartyState, source string) (bool, error) {
	return s.sendCurrentPartyActorFrameProjection(session, session, state, source)
}

func (s *Service) sendCurrentPartyActorFrameProjection(receiver *gameSession, actor *gameSession, state alignedcmd.PartyState, source string) (bool, error) {
	return s.sendCurrentPartyActorFrameProjectionBefore(receiver, actor, state, source, nil)
}

func (s *Service) sendCurrentPartyActorFrameProjectionBefore(
	receiver *gameSession,
	actor *gameSession,
	state alignedcmd.PartyState,
	source string,
	beforeFrame func() error,
) (bool, error) {
	receiverIdentity, ok := s.boundGameSessionCharacterSnapshot(receiver)
	if !ok {
		return false, nil
	}
	actorIdentity, ok := s.boundGameSessionCharacterSnapshot(actor)
	if !ok {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := actorIdentity.character
	if characterID == 0 {
		return false, nil
	}
	members := runtimePartyMembers(state)
	partyActive := state.PartyID > 0
	projections := make([]currentSceneOp9PartyMemberProjection, 0, len(members))
	projectionIdentities := make([]boundGameSessionCharacter, 0, len(members))
	for _, member := range members {
		memberIdentity := actorIdentity
		if member.UserID != characterID {
			var found bool
			memberIdentity, found = s.onlineGameSessionCharacterSnapshot(member.UserID)
			if !found {
				continue
			}
		}
		memberID, memberName, memberCharacter, memberHasCharacter := s.characterForEnter(
			ctx,
			memberIdentity.session,
			memberIdentity.character,
		)
		if memberID != member.UserID {
			continue
		}
		memberLevel := byte(1)
		memberJob := byte(0)
		memberGrow := byte(0)
		if memberHasCharacter {
			if memberCharacter.Level > 0 && memberCharacter.Level < 256 {
				memberLevel = byte(memberCharacter.Level)
			}
			memberJob = byte(numericCharacterStat(memberCharacter.Job))
			memberGrow = byte(numericCharacterStatValue(memberCharacter, "grow_type"))
		}
		projections = append(projections, currentSceneOp9PartyMemberProjection{
			State: member,
			Name:  memberName,
			Job:   memberJob,
			Level: memberLevel,
			Grow:  memberGrow,
		})
		projectionIdentities = append(projectionIdentities, memberIdentity)
	}
	projections = currentSceneOp9PartyMembers(projections)
	leaderSlot := byte(0)
	if partyActive {
		if receiver == actor {
			if _, localFound := currentSceneOp9SelectedPartySlot(projections, characterID); !localFound {
				return false, fmt.Errorf(
					"current party projection missing local member %d in party %d",
					characterID,
					state.PartyID,
				)
			}
		}
		var leaderFound bool
		leaderSlot, leaderFound = currentSceneOp9SelectedPartySlot(projections, state.UserID)
		if !leaderFound {
			return false, fmt.Errorf(
				"current party projection missing leader %d in party %d",
				state.UserID,
				state.PartyID,
			)
		}
	}
	if !s.boundGameSessionCharacterCurrent(receiverIdentity) {
		s.logPacketEvent("game-current-party-frame-op9-deferred",
			"source", source,
			"char_id", characterID,
			"party_id", state.PartyID,
			"reason", "receiving_session_character_changed")
		return false, nil
	}
	if !s.boundGameSessionCharacterCurrent(actorIdentity) {
		s.logPacketEvent("game-current-party-frame-op9-deferred",
			"source", source,
			"char_id", characterID,
			"party_id", state.PartyID,
			"reason", "projected_actor_session_character_changed")
		return false, nil
	}
	for _, identity := range projectionIdentities {
		if s.boundGameSessionCharacterCurrent(identity) {
			continue
		}
		s.logPacketEvent("game-current-party-frame-op9-deferred",
			"source", source,
			"char_id", characterID,
			"party_id", state.PartyID,
			"member_id", identity.character,
			"reason", "projected_member_session_character_changed")
		return false, nil
	}
	objectKey := currentSceneActorObjectKey(characterID)
	if !partyActive {
		// Party op9 is parsed against the connected scene channel. The selected
		// actor's mode0/mode1 owner is intentionally context 0, but reusing that
		// value here makes the current EXE choose the wrong party-record branch.
		partyOwner := currentPartySceneOwnerContext(receiver)
		// The record that empties the client's party window has to be addressed
		// to the party object itself, not to the leaving character's scene actor.
		// buildCurrentSceneOp9PartyInformationBodyInContext above keys the
		// populated record by the shared party model id; current EXE sub_1D64CA0
		// resolves every non-zero-kind record through sub_26A1200(ownerChannel,
		// objectKey), whose lookup key is the eight-byte pair of both arguments,
		// and only the kind-3 branch on that same object fills all eight member
		// uid slots with 0xFFFF. Addressed to the actor key instead, the lookup
		// missed the party object entirely and the handler destroyed a scene
		// record while the party window kept every slot it had - the visible
		// symptom after a forced dungeon exit.
		//
		// The party id is not recoverable here: this branch is reached with an
		// already-empty state, so the identity of the last populated record is
		// replayed from the receiver's own memo.
		removalKey := objectKey
		removalOwnerChannel := partyOwner
		removalTarget := "character_actor_no_bound_party_frame"
		if identity, bound := takeRuntimePartyFrameIdentity(receiver, characterID); bound {
			// The remembered pair names the PARTY OBJECT the actor's last
			// populated frame was addressed to, and every member of that party
			// holds the same object. When the observer is one of them, the
			// remembered key IS the observer's own party window, so replaying the
			// clear here empties a party that is still alive -- and nothing
			// repopulates it, because the roster never changed and no frame
			// follows.
			//
			// Live incident 2026-09-27 15:50Z (3-member party 2, town channel
			// 41): the leader closed the game with op3 EXIT, the bridge detached
			// him correctly and re-elected char 20, he logged back in, and the
			// moment his new actor reached the public town this co-presence
			// projection sent op9 kind-3 keyed (2, 41) to chars 20 and 22 --
			// their own party object. Both clients' party windows emptied while
			// the server still held the party. His departure had already reduced
			// that object through the kind-2 membership row, so nothing stale is
			// left to clear and the correct action is to send nothing at all.
			if s.runtimePartyFrameKeyIsLivePartyOf(receiverIdentity, identity.modelKey) {
				s.logPacketEvent("game-current-party-frame-op9-remove-skipped",
					"source", source,
					"receiver_char_id", receiverIdentity.character,
					"char_id", characterID,
					"party_model_key", identity.modelKey,
					"owner_channel", identity.ownerChannel,
					"actor_object_key", objectKey,
					"reason", "the_remembered_party_object_is_the_receivers_own_live_party",
					"body_source", "current_exe_sub_1D64CA0_kind3_party_object_destroy")
				return false, nil
			}
			removalKey = identity.modelKey
			removalOwnerChannel = identity.ownerChannel
			removalTarget = "last_populated_party_model_frame"
		}
		body := buildCurrentSceneOp9ActorRemovalBodyInContext(removalKey, removalOwnerChannel)
		s.logPacketEvent("game-current-party-frame-op9-remove-send",
			"conn_id", receiver.connID,
			"source", source,
			"receiver_char_id", receiverIdentity.character,
			"char_id", characterID,
			"object_key", removalKey,
			"actor_object_key", objectKey,
			"owner_channel", removalOwnerChannel,
			"removal_target", removalTarget,
			"party_id", state.PartyID,
			"body_len", len(body),
			"msg_id", uint16(dnfenum.CmdPacketRecoverStamina),
			"classification", 0,
			"body_source", "current_exe_sub_1D64CA0_kind3_party_object_destroy")
		if err := s.sendGameUpperRawClass(receiver, uint16(dnfenum.CmdPacketRecoverStamina), body, 0); err != nil {
			return false, err
		}
		return true, nil
	}
	partyOwner := currentPartySceneOwnerContext(receiver)
	partyModelKey := uint16(state.PartyID)
	ownerServer := byte(s.channelServerID())
	if receiver.channel.ServerID > 0 && receiver.channel.ServerID <= 0xff {
		ownerServer = byte(receiver.channel.ServerID)
	}
	body := buildCurrentSceneOp9PartyInformationBodyInContext(
		partyModelKey,
		ownerServer,
		partyOwner,
		state,
		projections,
		leaderSlot,
	)
	// The current party transport establishes the realtime roster and P2P
	// endpoints before op9 binds the scene-side member table. Sending op9 first
	// leaves the client in its "connecting" state and prevents teammate map
	// projection from becoming authoritative.
	if beforeFrame != nil {
		if err := beforeFrame(); err != nil {
			return false, err
		}
	}
	s.logPacketEvent("game-current-party-frame-op9-send",
		"conn_id", receiver.connID,
		"source", source,
		"receiver_char_id", receiverIdentity.character,
		"char_id", characterID,
		"party_model_key", partyModelKey,
		"owner_channel", partyOwner,
		"local_channel", partyOwner,
		"party_id", state.PartyID,
		"party_active", partyActive,
		"slot_count", len(projections),
		"leader_slot", leaderSlot,
		"body_len", len(body),
		"msg_id", uint16(dnfenum.CmdPacketRecoverStamina),
		"classification", 0,
		"body_source", "same_version_sub_1D64CA0_kind0_complete_party_information_and_fixed_slot_table")
	if err := s.sendGameUpperRawClass(receiver, uint16(dnfenum.CmdPacketRecoverStamina), body, 0); err != nil {
		return false, err
	}
	// This client now owns a party object keyed by exactly this pair. Remember
	// it against the projected actor so the record that empties the same object
	// can repeat the pair after the party id has left every session state.
	storeRuntimePartyFrameIdentity(receiver, characterID, partyModelKey, partyOwner)
	return true, nil
}

// storeRuntimePartyFrameIdentity records the (objectKey, ownerChannel) pair a
// populated op9 kind-0 party record was addressed to, keyed by the actor the
// record was projected for. Keying by actor matters because one receiver can
// hold several party objects at once: every same-area peer's party record is
// broadcast to it, so a single per-session slot would let one player's leave
// clear another player's party window.
func storeRuntimePartyFrameIdentity(session *gameSession, characterID uint16, modelKey uint16, ownerChannel byte) {
	if session == nil || characterID == 0 || modelKey == 0 {
		return
	}
	session.party.mu.Lock()
	defer session.party.mu.Unlock()
	if session.party.frameIdentities == nil {
		session.party.frameIdentities = make(map[uint16]runtimePartyFrameIdentity, 4)
	}
	session.party.frameIdentities[characterID] = runtimePartyFrameIdentity{
		modelKey:     modelKey,
		ownerChannel: ownerChannel,
	}
}

// takeRuntimePartyFrameIdentity consumes the remembered party-object identity
// for one actor. It is a one-shot: once the clearing record has been addressed
// to that object there is nothing left to clear, and a stale pair must never be
// reused for a later party.
func takeRuntimePartyFrameIdentity(session *gameSession, characterID uint16) (runtimePartyFrameIdentity, bool) {
	if session == nil || characterID == 0 {
		return runtimePartyFrameIdentity{}, false
	}
	session.party.mu.Lock()
	defer session.party.mu.Unlock()
	identity, bound := session.party.frameIdentities[characterID]
	if !bound {
		return runtimePartyFrameIdentity{}, false
	}
	delete(session.party.frameIdentities, characterID)
	return identity, true
}

// runtimePartyFrameKeyIsLivePartyOf reports whether the remembered party-object
// key a clearing record would be addressed to is the party the observer stands
// in right now.
//
// The memo is keyed per (receiver, actor) but the value it stores is a SHARED
// object: two members of one party remember the same model key, which is exactly
// why that key must not be replayed at one of them. The central manager is the
// only authority asked here -- a session cache can be empty while the roster is
// live, and the roster is what makes the object alive.
func (s *Service) runtimePartyFrameKeyIsLivePartyOf(identity boundGameSessionCharacter, modelKey uint16) bool {
	if s == nil || modelKey == 0 || identity.character == 0 || identity.generation == 0 {
		return false
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return false
	}
	snapshot, found := manager.SnapshotByUser(identity.character, identity.generation)
	return found && snapshot.ID == modelKey
}

// broadcastCurrentPartyActorProjection tells every same-area peer which party this
// actor fights in.
//
// The projection is a kind-0 op9, and op153 is the only carrier of the per-member
// HP/MP rows (party.BuildSingleMemberRealtimeInfo). When the observer stands in that
// same ordinary party, the record IS the observer's own party frame, so leaving it as
// the last party packet empties the observer's health bars -- the operator's
// "空血条" every time a squadmate re-enters the area (entering a gate, returning to
// town, or a phase-end forced return). Live 2026-09-20T13:16:52Z, conn game-000169
// (char 1 entering 224): three consecutive op9 records for chars 20/22/21 with
// party_model_key=4 and no op153 behind any of them. The observer's own rows are sent
// from the observer's own state, never from the actor's.
func (s *Service) broadcastCurrentPartyActorProjection(actor *gameSession, state alignedcmd.PartyState, source string) error {
	if s == nil || s.onlinePlayers == nil || actor == nil || actor.selectedCharacterID == 0 {
		return nil
	}
	for _, peer := range s.onlinePlayers.PeersInSameArea(actor.selectedCharacterID) {
		if peer.Session == nil {
			continue
		}
		if _, err := s.sendCurrentPartyActorFrameProjection(peer.Session, actor, state, source+"_to_area_peer"); err != nil {
			return err
		}
		if state.PartyID <= 0 {
			// A clearing record (a member left / the party dissolved) is not a frame
			// that needs health rows; the removal path publishes its own empty op153.
			continue
		}
		observer := runtimePartyStateSnapshot(peer.Session)
		if observer.PartyID != state.PartyID {
			continue
		}
		if err := s.sendRuntimePartyRealtimeInfoLocal(peer.Session, observer); err != nil {
			return err
		}
	}
	return nil
}

func runtimePartyMember(session *gameSession, state alignedcmd.PartyState) alignedcmd.PartyMemberState {
	member := alignedcmd.PartyMemberState{
		UserID:    session.selectedCharacterID,
		UserState: 100,
		HPPercent: 100,
		MPPercent: 100,
	}
	for _, existing := range state.Members {
		if existing.UserID == session.selectedCharacterID {
			member.Slot = existing.Slot
			if existing.UserState != 0 {
				member.UserState = existing.UserState
			}
			if existing.HPPercent != 0 {
				member.HPPercent = existing.HPPercent
			}
			if existing.MPPercent != 0 {
				member.MPPercent = existing.MPPercent
			}
			break
		}
	}
	return member
}

func hasMultiMemberRuntimeParty(session *gameSession) bool {
	return runtimePartySelectionTargetActive(runtimePartyStateSnapshot(session))
}

func runtimePartyStateSnapshot(session *gameSession) alignedcmd.PartyState {
	if session == nil {
		return alignedcmd.PartyState{}
	}
	session.party.mu.Lock()
	defer session.party.mu.Unlock()
	return cloneRuntimePartyState(session.party.state)
}

func storeRuntimePartyState(session *gameSession, state alignedcmd.PartyState) {
	if session == nil {
		return
	}
	session.party.mu.Lock()
	defer session.party.mu.Unlock()
	state.Members = runtimePartyMembers(state)
	session.party.state = cloneRuntimePartyState(state)
}

func cloneRuntimePartyState(state alignedcmd.PartyState) alignedcmd.PartyState {
	state.NameBytes = append([]byte(nil), state.NameBytes...)
	state.Members = append([]alignedcmd.PartyMemberState(nil), state.Members...)
	return state
}

func runtimePartyMemberBySlot(state alignedcmd.PartyState, slot byte) (alignedcmd.PartyMemberState, bool) {
	for _, member := range runtimePartyMembers(state) {
		if member.Slot == slot {
			return member, true
		}
	}
	return alignedcmd.PartyMemberState{}, false
}

func runtimePartyMembers(state alignedcmd.PartyState) []alignedcmd.PartyMemberState {
	maxMembers := state.MaxMembers
	if maxMembers == 0 || maxMembers > 4 {
		maxMembers = 4
	}
	members := make([]alignedcmd.PartyMemberState, 0, int(maxMembers))
	for _, member := range state.Members {
		members = appendRuntimePartyMember(members, member, maxMembers)
	}
	// Member slots are fixed native identities. Leadership is carried by the
	// trailing op9 leader-slot field and must never reorder this table.
	sort.Slice(members, func(i, j int) bool { return members[i].Slot < members[j].Slot })
	return members
}

func appendRuntimePartyMember(members []alignedcmd.PartyMemberState, member alignedcmd.PartyMemberState, maxMembers byte) []alignedcmd.PartyMemberState {
	if member.UserID == 0 || containsRuntimePartyMember(members, member.UserID) || len(members) >= int(maxMembers) {
		return members
	}
	if member.UserState == 0 {
		member.UserState = 100
	}
	if member.HPPercent == 0 {
		member.HPPercent = 100
	}
	if member.MPPercent == 0 {
		member.MPPercent = 100
	}
	occupied := func(slot byte) bool {
		for _, existing := range members {
			if existing.Slot == slot {
				return true
			}
		}
		return false
	}
	if member.Slot >= maxMembers || occupied(member.Slot) {
		for candidate := byte(0); candidate < maxMembers; candidate++ {
			if !occupied(candidate) {
				member.Slot = candidate
				break
			}
		}
	}
	return append(members, member)
}

func containsRuntimePartyMember(members []alignedcmd.PartyMemberState, userID uint16) bool {
	for _, member := range members {
		if member.UserID == userID {
			return true
		}
	}
	return false
}

func selectedCharacterID(session *gameSession) uint16 {
	if session == nil {
		return 0
	}
	return session.selectedCharacterID
}
