package dnfbridge

import (
	"fmt"
	"sort"
)

// party_selector_copresence.go —— 选择地下城页面（op15 选图页）上的**队内**共现。
//
// 症状与定位
//
//	玩家报的"出副本有时候再进会提示队友不在附近"是**客户端**自己判的距离：它手里的
//	队伍名册来自 op9，而"队友站在哪"只来自场景演员包（对端 mode0/mode1 + op23）。
//	服务端的同名判定是 currentPartyEntryCoPresence（同 城镇/区域/实例/频道），它
//	在这条路径上从来**没有**拒绝过（203.0.113.12 全天 game-party-dungeon-entry-
//	copresence-refused = 0），也就是说服务端认为大家在同一个格子，客户端手里却没有
//	那些演员。
//
//	原因在 leaveTownPresenceForDungeonSelector 的运营决定里：角色一打开选图页就
//	从城镇名册上摘掉（选图页上的人不该被城镇里其他人看见）。这条摘除只针对**城镇
//	可见性**，但它顺带把 party 侧的演员扇出也一起断了 —— broadcastCurrentPartyActor
//	Projection 走的是 PeersInSameArea（名册），名册里没有这条记录，于是：
//	  · 选图页上的人收不到队友的演员包；
//	  · 队友也收不到他的演员包。
//	两者在同一个 op6 里本来是被**刻意保留**的（"op6 对同队成员不下发"，否则客户端
//	会把它当成退队），也就是说"队内互相看得见"本来就是既定语义，缺的只是这两份
//	演员投影。
//
// 本文件做什么
//
//	在选图页那条路径上，按**队伍**（而不是城镇名册）补一次双向演员投影，使用的都是
//	已被回城共现验证过的包型（对端 mode0/mode1 + op23 区域通知），不碰 op11（op11 是
//	组队握手，重放会把已经健康的 P2P 链路拆掉，重新变成"连接中"）。
//
// 边界
//
//   - 只发给同一个队伍里**锚点一致**的成员（同一 城镇/区域/实例/频道）。锚点不一致
//     时客户端的"不在附近"是正确的，不去掩盖它，只记原因。
//   - 只有多人队才发；单人开页面是高频动作，不能因此产生任何包或日志。
//   - 攻坚频道整体跳过：等待地图是服务端私有实例，队友演员由攻坚自己的投影
//     （raid_dungeon_loaded_raid_mate_actors）负责，不在这里叠第二套。
//   - 出错不返回错误：这条路径挂在 op15 的 ACK 之后，选图页不能因为一次演员投影
//     失败而回滚。
func (s *Service) rebuildRuntimePartySelectorPageCoPresence(session *gameSession, source string) int {
	if s == nil || session == nil || session.selectedCharacterID == 0 || s.onlinePlayers == nil {
		return 0
	}
	state := runtimePartyStateSnapshot(session)
	members := runtimePartyMembers(state)
	if state.PartyID <= 0 || len(members) <= 1 {
		return 0
	}
	if _, raidChannel := currentRaidTownSpecForSession(session); raidChannel {
		s.logGameEvent(session, "game-party-selector-copresence-skipped",
			"source", source,
			"party_id", state.PartyID,
			"member_count", len(members),
			"char_id", session.selectedCharacterID,
			"reason", "raid_waiting_instance_owns_its_own_mate_actor_projection")
		return 0
	}
	selfPresence, selfResolved := s.currentTownActorAreaDetachPresence(session)
	if !selfResolved {
		s.logGameEvent(session, "game-party-selector-copresence-deferred",
			"source", source,
			"party_id", state.PartyID,
			"member_count", len(members),
			"char_id", session.selectedCharacterID,
			"reason", "selector_owner_has_no_resolvable_town_anchor")
		s.recordRaidEvent(session, "town_return_copresence", map[string]any{
			"source":           source,
			"scope":            "selector_page_party_only",
			"party_id":         state.PartyID,
			"member_count":     len(members),
			"projected":        0,
			"result":           "deferred",
			"reason":           "selector_owner_has_no_resolvable_town_anchor",
			"packet_sequence":  "remote_mode0_mode1_then_op23_both_directions",
			"op11_replayed":    false,
			"visibility_scope": "party_members_only",
		})
		return 0
	}

	projected := 0
	skipped := make(map[string][]uint16)
	recordSkip := func(reason string, memberID uint16) {
		skipped[reason] = append(skipped[reason], memberID)
	}
	for _, member := range members {
		if member.UserID == 0 || member.UserID == session.selectedCharacterID {
			continue
		}
		memberSession, online := s.onlineGameSession(member.UserID)
		if !online || memberSession == nil || memberSession == session {
			recordSkip("member_session_offline", member.UserID)
			continue
		}
		// The anchor test is the same one party dungeon entry makes, so this pass
		// can never make the client believe two members share a spot the server
		// would refuse.
		if coPresent, reason := s.currentPartyEntryCoPresence(session, memberSession); !coPresent {
			recordSkip(reason, member.UserID)
			continue
		}
		memberPresence, memberResolved := s.currentTownActorAreaDetachPresence(memberSession)
		if !memberResolved {
			recordSkip("member_has_no_resolvable_town_anchor", member.UserID)
			continue
		}
		if err := s.projectSelectorPartyActorPair(session, selfPresence, memberSession, memberPresence, source); err != nil {
			s.logGameEvent(session, "game-party-selector-copresence-projection-failed",
				"source", source,
				"party_id", state.PartyID,
				"char_id", session.selectedCharacterID,
				"member_id", member.UserID,
				"error", err)
			recordSkip("projection_write_failed", member.UserID)
			continue
		}
		projected++
	}

	reasons := make([]string, 0, len(skipped))
	for reason := range skipped {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	fields := map[string]any{
		"source":           source,
		"scope":            "selector_page_party_only",
		"party_id":         state.PartyID,
		"member_count":     len(members),
		"projected":        projected,
		"skipped":          len(members) - 1 - projected,
		"result":           "rebuilt",
		"reason":           "selector_page_party_members_need_each_others_town_actor_for_the_client_side_proximity_check",
		"packet_sequence":  "remote_mode0_mode1_then_op23_both_directions",
		"op11_replayed":    false,
		"visibility_scope": "party_members_only",
	}
	if len(reasons) != 0 {
		fields["skip_reasons"] = reasons
		for _, reason := range reasons {
			fields["skipped_"+reason] = skipped[reason]
		}
	}
	s.logGameEvent(session, "game-party-selector-copresence-rebuilt", fields)
	s.recordRaidEvent(session, "town_return_copresence", fields)
	return projected
}

// projectSelectorPartyActorPair writes both directions of one party pair's town
// actor projection. The selector-page session is not on the town roster, so the
// ordinary area fan-out skips it; the actor bodies themselves are the same ones
// the completed-return co-presence rebuild uses.
//
// Deliberately the RECORD form (mode0 + the op9 party frame, no mode1 binding).
// mode1 describes the *receiver's own* HP/MP/stat and worn-equipment rows: a
// peer's copy has no consumer here and the client's native handler applies it to
// the actor it controls. It is also the expensive half -- the measured raid
// fan-out carried 15.7 MB of mode1 in 1.7 h, every full body recomputing
// equipment attributes through the PVF (online_player_manager.go, "every one of
// them carrying a mode1 half") -- and the operator's rule is that too much mode1
// on the wire stutters the client. The record form is what the channel-wide
// co-presence pass (sendChannelWidePeerActors) already uses.
func (s *Service) projectSelectorPartyActorPair(
	selector *gameSession,
	selectorPresence onlinePlayerInfo,
	memberSession *gameSession,
	memberPresence onlinePlayerInfo,
	source string,
) error {
	if err := s.sendTownRemoteActorRecord(memberSession, selectorPresence, source+"_selector_owner_to_member"); err != nil {
		return fmt.Errorf("selector owner actor to member %d: %w", selectedCharacterID(memberSession), err)
	}
	if err := s.sendSelectorPartyActorAreaNotification(memberSession, selectorPresence, source+"_selector_owner_area_to_member"); err != nil {
		return err
	}
	if err := s.sendTownRemoteActorRecord(selector, memberPresence, source+"_member_to_selector_owner"); err != nil {
		return fmt.Errorf("member %d actor to selector owner: %w", selectedCharacterID(memberSession), err)
	}
	return s.sendSelectorPartyActorAreaNotification(selector, memberPresence, source+"_member_area_to_selector_owner")
}

func (s *Service) sendSelectorPartyActorAreaNotification(
	receiver *gameSession,
	actor onlinePlayerInfo,
	source string,
) error {
	if receiver == nil || actor.CharacterID == 0 {
		return nil
	}
	body := buildCurrentTownUserAreaNotificationBody(
		currentSceneActorObjectKey(actor.CharacterID),
		actor.TownID,
		actor.AreaID,
		actor.PositionX,
		actor.PositionY,
		actor.Direction,
		actor.AreaState,
	)
	return s.sendCurrentSceneFixedClass0Packet(receiver, currentTownUserAreaNotificationMsgID, body, source)
}
