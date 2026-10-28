package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 战况表回城 (the battle-status table's own 回城 button) is the squad leader's
// mid-run whole-party exit. Operator's rule, 2026-09-20:
//
//	"战况表回城本就是设计给小队队长在副本中途带领全队回城的（回城后应当依然还是
//	 组队状态）现在是只能小队成员自己退出 然后重新组队"
//
// The live log agrees the button exists and does nothing today: 61 clicks in three
// hours, every one answered silently by the settlement gate --
//
//	game-dungeon-card-exit-blocked … request_value_a=1 request_value_b=2
//	    reason=settlement_result_not_ready        (dungeon_card_flow.go:703-711)
//
// Deliberately narrow: only a raid gate (the 战况表 exists nowhere else), only before the
// client's boss-die handshake (a squad standing on a finished boss still goes through the
// settlement page and keeps its clear), and only for the squad leader. Each member walks
// out through the interrupted-run town route with the party kept
// (sendCurrentActiveDungeonReturnToTownKeepingParty), which is what "
// 回城后应当依然还是组队状态" asks for.

// leadRuntimePartyHomeFromActiveRun runs the leader's whole-squad exit. It answers the
// leader's own op72 first (the client waits for that echo before it stops driving the
// exit itself -- the settlement exit answers with the same body), then returns every
// member that is standing in its own run right now.
//
// It returns false when the gesture is not this character's to make or when nobody
// could be returned, so the caller falls through to its own refusal path unchanged.
func (s *Service) leadRuntimePartyHomeFromActiveRun(session *gameSession, source string) bool {
	if s == nil || session == nil {
		return false
	}
	leaderID := session.selectedCharacterID
	if leaderID == 0 {
		return false
	}
	// The 战况表 exit belongs to the squad leader: a member's own click stays with the
	// existing owner path (handleDungeonEplpCommand consumes a non-leader's request for
	// a shared party run), so a member can never pull the squad out from under its leader.
	party := runtimePartyStateSnapshot(session)
	if party.PartyID > 0 && party.UserID != leaderID {
		return false
	}
	members := runtimePartyMembers(party)
	if len(members) == 0 {
		// 单人攻坚（`TailFlag == 1`）没有小队编制，`runtimePartyMembers` 因此是空的，
		// 这个函数原来直接返回 false，调用方于是落到 `settlement_result_not_ready`
		// 的静默拒绝里 —— 玩家看到的就是「战况表的返回城镇没反应」。
		// 服主 2026-09-29 实测单人卢克：「9999是单人 没队伍编制 … 应该是没队伍编制
		// 不给用 正常应该给用的」。
		//
		// 战况表回城在这个客户端上就是 op72{1,2}（`dungeon_card_flow.go:508`），而
		// "带队回城"必须有一支小队。所以这里把"根本没有小队"当成"一支只有自己的
		// 小队"：点的人自己走同一条带编制回城的路，不需要任何新的退出通道。
		//
		// 只在 **PartyID == 0** 时兜底：一个存在但成员表为空的 party 是另一回事
		// （畸形数据），那里的旧行为（返回 false，交给上层）原样保留。
		if party.PartyID != 0 {
			return false
		}
		members = []alignedcmd.PartyMemberState{{UserID: leaderID, Slot: 0}}
	}
	returned := 0
	for _, member := range members {
		target, online := s.onlineGameSession(member.UserID)
		if !online || target == nil {
			continue
		}
		if !target.dungeon.mu.TryLock() {
			// That member is inside its own packet path. Skipping it is the documented
			// discipline: waiting here would deadlock two sessions against each other
			// (see scheduleRuntimeRaidForcedGroupReturn).
			s.logGameEvent(session, "game-party-war-table-return-skipped-busy-session",
				"char_id", member.UserID,
				"party_id", party.PartyID,
				"source", source)
			continue
		}
		targetRuntime := target.dungeon.runtime
		inside := targetRuntime != nil && dungeonRuntimeOwnsCharacter(targetRuntime, member.UserID)
		target.dungeon.mu.Unlock()
		if !inside {
			continue
		}
		if member.UserID == leaderID {
			// The leader's own echo: the client waits for its op72 answer.
			if err := s.sendGameUpperRawClass(
				target,
				uint16(dnfenum.CmdPacketEplpCommand),
				buildCurrentDungeonOp72SuccessBody(1, 2),
				currentDungeonCardResponseClass,
			); err != nil {
				return false
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, target, member.UserID)
		cancel()
		if err != nil {
			s.logGameEvent(session, "game-party-war-table-return-blocked",
				"char_id", member.UserID,
				"party_id", party.PartyID,
				"dungeon_id", targetRuntime.Dungeon.ID,
				"source", source,
				"reason", "town_transition_unavailable_for_war_table_return",
				"error", err)
			continue
		}
		if err := s.sendCurrentActiveDungeonReturnToTownKeepingParty(
			target,
			targetRuntime,
			transition,
			uint16(dnfenum.CmdPacketEplpCommand),
			source,
		); err != nil {
			s.logGameEvent(session, "game-party-war-table-return-blocked",
				"char_id", member.UserID,
				"party_id", party.PartyID,
				"dungeon_id", targetRuntime.Dungeon.ID,
				"source", source,
				"reason", "war_table_town_return_failed",
				"error", err)
			continue
		}
		returned++
		s.logGameEvent(target, "game-party-war-table-return-member-returned",
			"char_id", member.UserID,
			"leader_char_id", leaderID,
			"party_id", party.PartyID,
			"dungeon_id", targetRuntime.Dungeon.ID,
			"raid_key", targetRuntime.raidDungeon.RaidKey,
			"raid_map_id", targetRuntime.raidDungeon.MapID,
			"source", source,
			"reason", "the_squad_leader_led_the_squad_home_mid_run")
	}
	if returned == 0 {
		return false
	}
	s.logGameEvent(session, "game-party-war-table-return-committed",
		"char_id", leaderID,
		"party_id", party.PartyID,
		"returned_members", returned,
		"source", source)
	return true
}
