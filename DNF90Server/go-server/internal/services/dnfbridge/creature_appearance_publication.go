package dnfbridge

import (
	"context"
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 副本内宠物（装备槽 26）变更之后，给"自己"和"同场景队友"补发什么。
//
// 背景与结论（2026-10-08，实机 A/B 定位）：
//
//	换/穿/脱宠物之后，客户端的宠物精灵**只有** op2/mode0 角色行能重新绑定
//	（客户端的宠物层挂在 actor 行的 creature item/serial 对上）。op342+op357
//	只更新纸娃娃、op9+op120+op357 完全不管宠物层 —— 这两个候选实机验证都是
//	"宠物不动、过图正常"。
//
//	但客户端把这一行当成**场景里的一次新绑定**：它会重建"选中的本地 wrapper"。
//	所以房间协议永远是成套的（scene_post_start_map.go）：
//
//	    op9   角色显示   —— 把角色的显示记录登记进当前场景
//	    op120 落位       —— "commits the room object manager"
//	    op357 同状态重绘
//	    op3   用户状态   —— "op3 marks the selected local wrapper active"
//
//	只有走到 op3，房间里的交互几何（出口门/传送点）才会响应该玩家。
//	2026-09-21 那次修复只补了角色行、没补尾巴：宠物是实时了，但 wrapper 停在
//	"已绑定、未激活"，走到门口客户端**连 send() 都不调用**（socket 层实测无任何
//	op45 尝试），于是"换过宠物就过不去图"。
//
//	修法就是本文件：发角色行之后，**按进房间的原顺序把激活尾巴重放一遍**。
//	实机验证：宠物实时刷新 + 过图正常 + 组队视角同样实时。
func (s *Service) publishDungeonCreatureAppearanceChange(session *gameSession, operation string) error {
	if s == nil || session == nil {
		return nil
	}
	source := "dungeon_creature_appearance_" + operation
	var errs []error

	// 1) op2/mode0 角色行：唯一能让客户端重新绑定宠物层的包。场景上下文（0）就是
	//    副本里本地 actor 的 owner。
	if _, err := s.sendSelectedActorMode0RefreshInContextWithBody(
		session,
		currentSceneObjectContext,
		operation,
		source+"_self_mode0",
		true,
	); err != nil {
		errs = append(errs, fmt.Errorf("self mode0 appearance refresh: %w", err))
	} else {
		// 2) 激活尾巴：顺序与进房间完全一致（op9 → op120 → op357 → op3）。
		//    少了它，门口就不会再响应 —— 这正是 09-21 那版修复漏掉的一步。
		if err := s.sendDungeonSceneActorDisplayAndPlacement(session, source); err != nil {
			errs = append(errs, fmt.Errorf("activation tail display/placement: %w", err))
		} else if err := s.sendDungeonLocalWrapperActivation(session, source); err != nil {
			errs = append(errs, fmt.Errorf("activation tail wrapper activation: %w", err))
		}
	}

	// 3) 队友：把"我现在的样子"按远端 actor 投影重发给同场景队友。这条路径从不碰
	//    本地 wrapper，所以不会影响任何人的过图。
	if err := s.broadcastCreatureAppearanceToScenePeers(session, source); err != nil {
		errs = append(errs, err)
	}

	s.logGameEvent(session, "game-dungeon-creature-appearance-publication",
		"conn_id", session.connID,
		"operation", operation,
		"char_id", session.selectedCharacterID)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// sendDungeonSceneActorDisplayAndPlacement 重发"角色显示 + 落位"那两步，用的是
// 进房间时的同一组构造函数：
//
//	op9   当前场景角色显示（fixture 名 csharpCurrentSceneOp9ActorDisplayKind）
//	op120 actor slot + 落位种子，提交房间对象管理器
//	op357 同状态重绘（让重绘立刻可见）
func (s *Service) sendDungeonSceneActorDisplayAndPlacement(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("scene actor display requires an active character")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, charName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 {
		charID = session.selectedCharacterID
	}
	objectKey := currentSceneActorObjectKey(charID)
	if objectKey == 0 {
		return fmt.Errorf("scene actor display has no scene object key")
	}
	ownerChannel := byte(currentSceneObjectContext)

	displayBody := buildCurrentSceneOp9ActorDisplayBodyInContext(
		objectKey,
		character,
		hasCharacter,
		charName,
		ownerChannel,
	)
	s.logPacketEvent("game-upper-dungeon-op9-actor-display-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"owner_channel", ownerChannel,
		"msg_id", uint16(dnfenum.CmdPacketRecoverStamina),
		"classification", 0,
		"body_len", len(displayBody),
		"body_source", "current_exe_sub_1D64CA0_actor_display_after_mode0_rebind")
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRecoverStamina), displayBody, 0); err != nil {
		return err
	}

	placementBody := buildCurrentSceneActorPlacementBody()
	s.logPacketEvent("game-upper-dungeon-actor-placement-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"msg_id", uint16(dnfenum.CmdPacketRequestBlacklist),
		"classification", 0,
		"body_len", len(placementBody),
		"body_source", "current_exe_sub_1D41DA0_actor_slot_scene_commit")
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRequestBlacklist), placementBody, 0); err != nil {
		return err
	}
	return s.sendSelectedActorUserStateRefresh(session, source+"_op357_after_op120")
}

// sendDungeonLocalWrapperActivation 重发 op3（CmdPacketNotifyUserState）—— 进房间链里
// **"把选中的本地 wrapper 标记为 active"**的那一步，也是门口恢复响应的关键。
// body 与进房间时同一个构造函数，语义完全一致
// （working_current_client_trace_op3_after_op120_room_object_manager_commit）。
func (s *Service) sendDungeonLocalWrapperActivation(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("local wrapper activation requires an active character")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, _, _ := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 {
		charID = session.selectedCharacterID
	}
	objectKey := currentSceneActorObjectKey(charID)
	if objectKey == 0 {
		return fmt.Errorf("local wrapper activation has no scene object key")
	}
	body, err := buildCurrentDungeonUserStateBody(objectKey)
	if err != nil {
		return fmt.Errorf("build dungeon user state body: %w", err)
	}
	s.logPacketEvent("game-upper-dungeon-op3-wrapper-activation-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"msg_id", uint16(dnfenum.CmdPacketNotifyUserState),
		"classification", 0,
		"body_len", len(body),
		"user_state", currentDungeonPlayerUserState,
		"body_source", "working_current_client_trace_op3_after_op120_room_object_manager_commit")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyUserState), body, 0)
}

// broadcastCreatureAppearanceToScenePeers 把「我现在的样子」重发给同场景队友。
// 官方 df_game_r 的 send_all 语义是"发给同一场景所有人（含自己）"；本服此前只有城镇
// 区域那一版（broadcastTownActorAppearanceRefresh 走 PeersInSameArea），副本里够不着。
func (s *Service) broadcastCreatureAppearanceToScenePeers(session *gameSession, source string) error {
	charID := session.selectedCharacterID
	if charID == 0 {
		return nil
	}
	peers := s.creatureAppearanceScenePeerSessions(session, charID)
	if len(peers) == 0 {
		s.logGameEvent(session, "game-dungeon-creature-appearance-peers-skipped",
			"char_id", charID,
			"reason", "no_peer_in_the_same_scene",
			"source", source)
		return nil
	}

	// 公会徽章只跟演员有关，整个扇出解析一次（与城镇广播同一做法）。
	auraCtx, auraCancel := s.currentGuildContext()
	guild, guildErr := s.currentGuildForCharacterID(auraCtx, charID)
	auraCancel()
	if guildErr != nil {
		s.logGameEvent(session, "game-dungeon-creature-appearance-guild-lookup-failed",
			"char_id", charID, "error", guildErr)
		guild = nil
	}

	actor := onlinePlayerInfo{CharacterID: charID, Session: session}
	sent := 0
	for _, peer := range peers {
		if peer == nil || peer == session || peer.conn == nil {
			continue
		}
		if err := s.sendTownRemoteActorAppearanceRefresh(peer, actor, guild, source); err != nil {
			s.logGameEvent(session, "game-dungeon-creature-appearance-peer-failed",
				"peer_char_id", peer.selectedCharacterID,
				"error", err)
			continue
		}
		sent++
	}
	s.logGameEvent(session, "game-dungeon-creature-appearance-peers",
		"char_id", charID,
		"peer_count", len(peers),
		"sent", sent,
		"source", source)
	return nil
}

// creatureAppearanceScenePeerSessions 取「和我在同一个场景里的其他人」：
// 队伍副本优先（同一张图），否则落回城镇区域注册表；两条都拿不到就返回空，
// 调用方只记一条日志，绝不影响已经提交的物品移动。
func (s *Service) creatureAppearanceScenePeerSessions(session *gameSession, charID uint16) []*gameSession {
	var out []*gameSession
	session.dungeon.mu.Lock()
	var owner *runtimePartyDungeonOwner
	if session.dungeon.runtime != nil {
		owner = session.dungeon.runtime.partyDungeonOwner
	}
	session.dungeon.mu.Unlock()
	if owner != nil {
		// 与 dungeon_card_flow.go 的队伍扇出同一套选人规则：跳过自己（userID + generation
		// 都对上才算自己），并且要求在线会话的 generation 与 roster 条目一致 —— 对不上
		// 说明这个队友已经掉线/重登（roster 里留的是旧条目），发给他会落到错误的场景代际。
		for _, member := range owner.roster() {
			if member.userID == 0 {
				continue
			}
			if member.userID == charID && member.generation == session.characterGeneration {
				continue
			}
			identity, online := s.onlineGameSessionCharacterSnapshot(member.userID)
			if !online || identity.session == nil || identity.generation != member.generation {
				continue
			}
			out = append(out, identity.session)
		}
		if len(out) > 0 {
			return out
		}
	}
	if s.onlinePlayers == nil {
		return nil
	}
	for _, peer := range s.onlinePlayers.PeersInSameArea(charID) {
		if peer.Session != nil && peer.CharacterID != charID {
			out = append(out, peer.Session)
		}
	}
	return out
}
