package dnfbridge

import (
	"context"
	"encoding/binary"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentPeerUserInfoMode = byte(3)

// currentPeerInfoTargetVisible reports whether targetID is a legitimate
// information target for session.
//
// Scope widened 2026-09-26 on the operator's instruction ("只要同频道…都应该放开"):
// the directory relation is same-channel now, not same-area, so the op7 prompt's
// two read-only panels (查看信息 op8 / 查看冒险团信息 op1403) open for anyone
// standing in the same channel even from another area of the town. This narrows
// nothing: one area is always one channel, so PeerInSameChannel strictly
// contains the area scope it replaces.
//
// Live evidence for the bug it fixes. On 2026-09-26 08:33:33 (op1403) and
// 08:33:34 (op8) the leader's client asked for the applicant's two panels and
// the server refused both with target_not_in_same_area_and_not_party_member;
// receiving nothing, the client rendered both buttons as dead, and the operator
// reported exactly that ("不在一个区域…点了没反应"). The same-area case worked
// only because it slipped past the old check.
//
// Two more relations cover the characters the public directory cannot answer
// for at all: the runtime party both already share, and an outstanding raid
// admission prompt. leaveTownPresenceForDungeon retires the presence entry on
// dungeon admission, and a Seria room is a private scene the directory never
// holds -- and the raid prompt path documents that exact case as reachable.
//
// Trading, assault and private stores keep the strict town-area rule; only
// read-only information panels use this wider scope.
func (s *Service) currentPeerInfoTargetVisible(session *gameSession, sourceID, targetID uint16) bool {
	if s == nil || session == nil || sourceID == 0 || targetID == 0 || sourceID == targetID {
		return false
	}
	if s.onlinePlayers != nil && s.onlinePlayers.PeerInSameChannel(sourceID, targetID) {
		return true
	}
	state := runtimePartyStateSnapshot(session)
	if state.PartyID > 0 {
		for _, member := range runtimePartyMembers(state) {
			if member.UserID == targetID {
				return true
			}
		}
	}
	// 未决的攻坚入团申请：申请人的 op7 弹框此刻正开在 sourceID 的屏幕上，框里那两个
	// 按钮要显示的就是 targetID 的资料 —— 弹框本身就是这次查看的授权。跨频道申请
	// （走浏览窗口，申请人不必在队伍频道里）时两人不同频道，上面那条同频道关系答不了，
	// 只能靠这条。方向只有一个，理由见下。
	return s.currentRaidPeerPromptVisible(sourceID, targetID)
}

// currentRaidPeerPromptVisible reports whether responder is the client an
// outstanding raid admission prompt was sent to and requester is the peer that
// prompt is about.
//
// Only that one orientation counts. The prompt goes to the responder
// (raid_peer_admission.go: sendGameUpperRawCodec(responder.session, 0x0007, ...))
// and both of its buttons render the requester's data, so "responder looks at
// requester" is the relation the dialog itself establishes. The reverse
// direction has no prompt behind it and stays refused, which is what keeps a
// plain stranger -- who has neither co-presence nor a shared party nor a pending
// request -- out.
func (s *Service) currentRaidPeerPromptVisible(responder, requester uint16) bool {
	if s == nil || responder == 0 || requester == 0 {
		return false
	}
	now := s.gameplayNow()
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	pending, found := s.raidPeerAdmissions[runtimeRaidPeerAdmissionKey{
		Responder: responder,
		Requester: requester,
	}]
	return found && now.Before(pending.ExpiresAt)
}

// handleCurrentPeerUserInfoRequest owns the current-client three-byte
// GET_USERINFO request emitted by the town interaction menu. The same legacy
// opcode also owns the role-selector bootstrap, so only the exact mode-3 shape
// is consumed here.
func (s *Service) handleCurrentPeerUserInfoRequest(session *gameSession, body []byte) (bool, error) {
	if len(body) != 3 || body[2] != currentPeerUserInfoMode {
		return false, nil
	}
	targetID := binary.LittleEndian.Uint16(body[:2])
	sourceID := selectedCharacterID(session)
	if sourceID == 0 || targetID == 0 {
		s.logGameEvent(session, "game-peer-userinfo-request-rejected",
			"source_char_id", sourceID,
			"target_char_id", targetID,
			"mode", body[2],
			"reason", "invalid_or_not_in_same_area")
		return true, nil
	}

	// The native character-information command uses the same three-byte shape
	// for a self click and a town-player click.  Self requests must be answered
	// here on demand; proactively sending this mode during login opens the
	// personal-information panel without user input.
	targetSession := session
	ownerChannel := currentTownActorOwnerContext(session)
	if targetID != sourceID {
		if !s.currentPeerInfoTargetVisible(session, sourceID, targetID) {
			s.logGameEvent(session, "game-peer-userinfo-request-rejected",
				"source_char_id", sourceID,
				"target_char_id", targetID,
				"mode", body[2],
				"reason", "not_in_same_area_and_not_party_member")
			return true, nil
		}
		var online bool
		targetSession, online = s.onlineGameSession(targetID)
		if !online {
			s.logGameEvent(session, "game-peer-userinfo-request-rejected",
				"source_char_id", sourceID,
				"target_char_id", targetID,
				"mode", body[2],
				"reason", "target_offline")
			return true, nil
		}
		ownerChannel = currentTownRemoteActorOwnerContext(session, targetID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedID, _, character, found := s.characterForEnter(ctx, targetSession, targetID)
	if !found || loadedID != targetID {
		s.logGameEvent(session, "game-peer-userinfo-request-rejected",
			"source_char_id", sourceID,
			"target_char_id", targetID,
			"mode", body[2],
			"reason", "target_repository_record_unavailable")
		return true, nil
	}
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	if repositories, ok := s.repositoryGroup(); ok {
		legacyRepo = repositories.LegacyUserInfo
	}
	response := s.buildCurrentSelectedUserInfoMode3BodyInContext(
		ctx,
		targetSession,
		legacyRepo,
		character,
		true,
		targetID,
		ownerChannel,
	)
	eventName := "game-peer-userinfo-response-send"
	if targetID == sourceID {
		eventName = "game-self-userinfo-response-send"
	}
	s.logGameEvent(session, eventName,
		"source_char_id", sourceID,
		"target_char_id", targetID,
		"mode", body[2],
		"msg_id", uint16(dnfenum.CmdPacketSetUDPIPPort),
		"classification", 0,
		"body_len", len(response),
		"body_source", "current_exe_mode3_peer_userinfo",
		"request_scope", map[bool]string{true: "self", false: "peer"}[targetID == sourceID])
	return true, s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketSetUDPIPPort),
		response,
		0,
	)
}
