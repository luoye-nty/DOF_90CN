package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const currentTownActorSceneSnapshotMsgID uint16 = 0x0320

// buildCurrentTownActorSceneSnapshotBody matches the current EXE's scene
// snapshot reader (sub_1D6C6F0):
//
//	u8 actor_count, actor_count * { u16 existing_actor_key, u8 child_count }
//
// The reader first resolves every actor key through sub_2699A30, then builds
// its scene-side bindings. A selected character has no server-owned children
// at town entry, so child_count is zero. The key is session data, never a
// captured DOVE key.
func buildCurrentTownActorSceneSnapshotBody(actorKey uint16) []byte {
	body := make([]byte, 4)
	body[0] = 1
	binary.LittleEndian.PutUint16(body[1:3], actorKey)
	return body
}

// sendCurrentInitialTownActorSceneSnapshotLocked completes the mode0-created
// selected actor's scene binding before full player state/location packets.
// The caller holds session.townMu.
func (s *Service) sendCurrentInitialTownActorSceneSnapshotLocked(
	session *gameSession,
	characterID uint16,
) error {
	if session == nil || characterID == 0 ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRouteActorBound ||
		session.initialTownActorSceneSnapshotSent {
		return nil
	}
	actorKey := currentSceneActorObjectKey(characterID)
	body := buildCurrentTownActorSceneSnapshotBody(actorKey)
	if err := s.sendCurrentSceneFixedClass0Packet(
		session,
		currentTownActorSceneSnapshotMsgID,
		body,
		"initial_town_actor_scene_snapshot_current_exe_sub_1D6C6F0",
	); err != nil {
		return err
	}
	session.initialTownActorSceneSnapshotSent = true
	s.logGameEvent(session, "game-initial-town-actor-scene-snapshot-sent",
		"char_id", characterID,
		"actor_object_key", actorKey,
		"child_count", 0,
		"sequence", "mode0_actor_created_then_noti0x320_scene_snapshot_then_full_player_state")
	return nil
}

// sendCurrentInitialTownActorRoutePacketsLocked owns completed character-login
// ordering. Unlike a dungeon return, it finishes the full actor state before
// op24 becomes the final transition commit. The caller must hold session.townMu.
func (s *Service) sendCurrentInitialTownActorRoutePacketsLocked(
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
	row currentSceneTransitionRow,
	objectBody []byte,
	mode1Body []byte,
	transitionBody []byte,
) error {
	return s.sendCurrentInitialTownActorRoutePacketsWithOptionsLocked(
		session,
		characterID,
		townID,
		areaID,
		row,
		objectBody,
		mode1Body,
		transitionBody,
		currentInitialTownRoutePolicy{
			includeReportClientSpec: true,
			// The selected actor is created by mode0. The complete mode1 is delayed
			// until the ordinary skill snapshot is ready; state-only expert-job
			// projection follows op19 so existing skills are never interpreted as a
			// newly acquired qualification on login.
			includeSecondFullMode1:                        true,
			skipPrerequisiteMode1:                         true,
			deferClearQuestListUntilLocationNotifications: true,
		},
	)
}

func (s *Service) sendCurrentChannelReconnectTownActorRoutePacketsLocked(
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
	row currentSceneTransitionRow,
	objectBody []byte,
	mode1Body []byte,
	transitionBody []byte,
) error {
	return s.sendCurrentInitialTownActorRoutePacketsWithOptionsLocked(
		session,
		characterID,
		townID,
		areaID,
		row,
		objectBody,
		mode1Body,
		transitionBody,
		currentInitialTownRoutePolicy{
			includeReportClientSpec: true,
			includeSecondFullMode1:  true,
			skipPrerequisiteMode1:   false,
		},
	)
}

func (s *Service) sendCurrentInitialTownActorRoutePacketsWithOptionsLocked(
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
	row currentSceneTransitionRow,
	objectBody []byte,
	mode1Body []byte,
	transitionBody []byte,
	policy currentInitialTownRoutePolicy,
) error {
	prerequisiteMode1Body := mode1Body
	if policy.skipPrerequisiteMode1 {
		prerequisiteMode1Body = nil
	}
	if err := s.sendCurrentTownActorPrerequisitesLocked(session, characterID, objectBody, prerequisiteMode1Body); err != nil {
		return err
	}
	if policy.commitTransitionEarly {
		objectKey := currentSceneActorObjectKey(characterID)
		if err := s.sendCurrentAdventureInfoPushFromAccount(
			session,
			objectKey,
			"return_select_after_mode0_before_single_full_mode1",
		); err != nil {
			return err
		}
		// Cross-character reselect keeps the previous actor's skill manager
		// until op19 is consumed. The equipment-bearing mode1 below immediately
		// validates worn equipment, so preload the selected character's op19
		// first; otherwise weapon-mastery jobs queue a client-authored unequip.
		// sendCurrentTownPlayerStateLocked replays the identical projection at
		// its established final boundary.
		if err := s.sendCurrentReturnSelectSkillInfoPreloadLocked(
			session,
			characterID,
			"return_select_before_single_full_mode1_weapon_validation",
		); err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode1Body, 0); err != nil {
			return err
		}
		if err := captureCurrentPartyActorProjection(session, characterID, objectBody, mode1Body); err != nil {
			s.logGameEvent(session, "game-party-actor-full-projection-refresh-rejected",
				"source", "return_select_single_full_mode1_before_early_op24",
				"char_id", characterID,
				"character_generation", session.characterGeneration,
				"mode0_len", len(objectBody),
				"mode1_len", len(mode1Body),
				"reason", err)
		}
		// The collection-box projection was published with the mode0 actor above.
		// Refresh the selected box again now that the complete equipment-bearing
		// mode1 has bound the native actor the collect-box stat owner belongs to.
		if err := s.replayCollectboxSelectedRefreshAfterMode1(
			context.Background(),
			session,
			"return_select_after_single_full_mode1",
		); err != nil {
			s.logGameEvent(session, "game-collectbox-selection-refresh-deferred",
				"char_id", characterID,
				"source", "return_select_after_single_full_mode1",
				"reason", err)
		}
	}
	if err := s.sendCurrentInitialTownActorSceneSnapshotLocked(session, characterID); err != nil {
		return fmt.Errorf("initialize selected town actor scene snapshot: %w", err)
	}
	if len(policy.earlyActorDisplayBody) != 0 {
		if err := s.sendCurrentInitialTownActorDisplayAndPlacementLocked(
			session,
			"return_select_after_op800_before_location",
			policy.earlyActorDisplayBody,
		); err != nil {
			return err
		}
	}
	// Publish the final channel-owned town/area as soon as the actor scene
	// binding exists.
	if err := s.sendCurrentInitialTownLocationNotificationsLocked(session, characterID, townID, areaID, row); err != nil {
		return fmt.Errorf("initialize selected town location notifications: %w", err)
	}
	if policy.commitTransitionEarly {
		if policy.deferClearQuestListUntilLocationNotifications {
			if err := s.sendCurrentInitialTownClearQuestListAfterLocationLocked(
				session,
				"return_select_after_location_notifications_before_early_op24",
			); err != nil {
				return fmt.Errorf("initialize return-select clear quest list before early transition: %w", err)
			}
		}
		if err := s.sendCurrentSpendTimeInitialStateOnce(
			session,
			"return_select_before_early_op24_activity_state",
		); err != nil {
			return fmt.Errorf("initialize return-select activity state before early transition: %w", err)
		}
		if err := s.sendCurrentTownTransitionBeforePlayerStateLocked(session, characterID, townID, areaID, row, transitionBody); err != nil {
			return err
		}
		if err := s.sendCurrentInitialTownDungeonPermissionSnapshotLocked(
			session,
			"return_select_after_op24_scene_commit",
		); err != nil {
			return err
		}
	}
	if policy.commitTransitionEarly || session.initialTownRouteStage < currentInitialTownRoutePlayerStatePrepared {
		policy.actorMode0Body = append([]byte(nil), objectBody...)
		if err := s.sendCurrentTownPlayerStateLocked(
			session,
			"initial_town_before_final_transition",
			policy,
		); err != nil {
			return fmt.Errorf("initialize selected town actor before transition: %w", err)
		}
	}
	if policy.deferClearQuestListUntilLocationNotifications && !policy.commitTransitionEarly {
		if err := s.sendCurrentInitialTownClearQuestListAfterLocationLocked(
			session,
			"initial_town_after_location_notifications_before_op24",
		); err != nil {
			return fmt.Errorf("initialize selected town clear quest list after location notifications: %w", err)
		}
	}
	if err := s.sendCurrentSpendTimeInitialStateOnce(
		session,
		"initial_town_before_op24_activity_state",
	); err != nil {
		return fmt.Errorf("initialize selected town activity state before transition: %w", err)
	}
	if err := s.sendCurrentTownTransitionLocked(session, characterID, townID, areaID, row, transitionBody); err != nil {
		return err
	}
	// Cold login and return-select commit op24 before their final town task
	// projection. Channel reconnect already owns a separate verified post-op24
	// refresh and must not receive a duplicate sequence.
	if !session.channelReconnect {
		if err := s.sendCurrentPostTownTransitionQuestSnapshotsLocked(
			session,
			"current_exe_initial_town_after_op24_scene_commit",
		); err != nil {
			return err
		}
	}
	// Character selection rebuilds the native scene owner. Restore the
	// repository-backed class-0 JOIN_GUILD_INFO notification only after typed
	// op24 and the route-specific quest/skill refreshes; this notification is
	// what writes the client's primary current-guild ID.
	if err := s.sendCurrentInitialTownGuildStateProjectionLocked(
		session,
		"current_exe_initial_town_after_op24_scene_commit",
	); err != nil {
		return fmt.Errorf("restore current guild after initial town transition: %w", err)
	}
	// Relog/reselect has no local op44 activation event. Restore the durable
	// effect item after op24 and its post-transition skill replay.
	if err := s.sendCurrentExperienceConsumableBuffProjection(session, "initial_town_after_op24"); err != nil {
		return fmt.Errorf("restore experience consumable HUD after initial town transition: %w", err)
	}
	// Same reason, for the durable item cooltime list: the client's cooldown gate
	// is process-local memory, so a 小退/重上 made a 1-day-cooldown consumable look
	// ready again even though the server still refuses it (live 2026-09-17).
	if err := s.sendCurrentStatChangePotionBuffProjection(session, "initial_town_after_op24"); err != nil {
		return err
	}
	if err := s.sendCurrentItemCooltimeProjection(session, "initial_town_after_op24"); err != nil {
		return fmt.Errorf("restore item cooltime list after initial town transition: %w", err)
	}
	// 商城"限购已买满"的商品列表（类 0 通知 860）：客户端的限购记忆只在会话内存里，
	// 不重放的话小退/大退/重启服务端之后那些商品又会出现在商城里（现场 2026-09-18：
	// "买完从商品列表消失、重上又出现"）。计数是持久化的，所以重放结果稳定。
	// 纯展示：失败只记日志，不影响回城/选角流程。
	if err := s.sendCurrentCeraShopBuyRestrictProjection(context.Background(), session, "initial_town_after_op24"); err != nil {
		s.logGameEvent(session, "game-cera-shop-buy-restrict-send-failed", "source", "initial_town_after_op24", "err", err)
	}
	// 商城三个钱包数字（点券 / 代币券 / 欢乐代币券，类 0 通知 53）：客户端没有
	// "请求钱包"的上行包，纯靠服务端推。不推的话选角进城后商城那三个数字是 0，
	// 玩家会以为代币券没到账。
	//
	// 只挂在选角进城这一处，不挂回城：钱包变化只有两个来源——购买（tera_shop_purchase.go
	// 已经在成功后推了真实值）和代币券兑换（兑换点自己推）。回城重放属于投机性多发，
	// 只会污染无关的地城返回包序列，没有实际收益。
	// 纯展示：失败只记日志，不影响选角/进城流程。
	if err := s.sendCurrentCeraShopWalletProjection(context.Background(), session, "initial_town_after_op24"); err != nil {
		s.logGameEvent(session, "game-cera-shop-wallet-send-failed", "source", "initial_town_after_op24", "err", err)
	}
	// 多买多送"已购买次数"（类 0 通知 1035，包体 = 一个 u32 绝对次数）。同样没有上行
	// 请求包：客户端窗口不会自己算次数，只在开窗时读服务端推来的值。活动没配置时
	// 这个函数直接返回，不发包。
	if err := s.sendCurrentCeraShopBuyCountGiftProjection(context.Background(), session, "initial_town_after_op24"); err != nil {
		s.logGameEvent(session, "game-cera-shop-buycount-gift-send-failed", "source", "initial_town_after_op24", "err", err)
	}
	// Same reason, for the weakness health cap: selecting a character rebuilds
	// the local player at a healthy 100, and the subtype0 stamina byte alone
	// does not restore the cap. Without this republish a weakened character
	// came back from character selection at full health -- which looked like
	// re-selecting cured the penalty, even though the stored level never moved.
	if err := s.sendCurrentWeaknessConditionRefresh(session, "initial_town_after_op24"); err != nil {
		return fmt.Errorf("restore weakness health cap after initial town transition: %w", err)
	}
	// 公会地下城 progression. The client keeps this table per session and gates
	// entry on it locally, so it has to be republished whenever the player is
	// rebuilt. See guild_dungeon.go.
	s.sendCurrentGuildDungeonState(session, "initial_town_after_op24")
	// The endpoint handshake is connection-scoped and completes before the
	// selected-town route. Never follow op24 with another class0/op1: it would
	// arm a fresh client watchdog after the one allowed class1/op1 success.
	s.logGameEvent(session, "game-initial-town-route-endpoint-preserved",
		"char_id", characterID,
		"town_id", townID,
		"area_id", areaID,
		"route_stage", session.initialTownRouteStage,
		"resident_notice_sent", session.currentChannelResidentNoticeSent,
		"town_actor_owner_channel", session.townActorOwnerChannel,
		"endpoint_policy", "channelinfo_then_single_requested_class1_op1")
	return nil
}

// sendCurrentReturnSelectSkillInfoPreloadLocked installs the selected
// character's op19 before the single equipment-bearing mode1 of the
// return-select route. It mirrors the cold-login weapon-validation preload and
// is a no-op when the skill projection is not ready yet.
func (s *Service) sendCurrentReturnSelectSkillInfoPreloadLocked(
	session *gameSession,
	characterID uint16,
	source string,
) error {
	if s == nil || session == nil || characterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if !hasCharacter || charID != characterID {
		return nil
	}
	projection, ready, err := s.buildCurrentSceneSkillInfoProjection(session, ctx, character, source)
	if err != nil {
		return fmt.Errorf("build return-select skill preload: %w", err)
	}
	if !ready {
		return nil
	}
	return s.sendCurrentSceneSkillInfoProjectionBeforeEquipment(session, projection, source)
}

func (s *Service) sendCurrentInitialTownDungeonPermissionSnapshotLocked(
	session *gameSession,
	source string,
) error {
	if session == nil || session.initialTownDungeonPermissionSent {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if err := s.sendCurrentDungeonPermissionSnapshot(ctx, session, source); err != nil {
		return err
	}
	session.initialTownDungeonPermissionSent = true
	return nil
}

// sendCurrentTownTransitionBeforePlayerStateLocked commits the return-select
// map without completing the still-pending player-state stream.
func (s *Service) sendCurrentTownTransitionBeforePlayerStateLocked(
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
	row currentSceneTransitionRow,
	transitionBody []byte,
) error {
	if session == nil || characterID == 0 ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRouteActorBound ||
		session.initialTownRouteStage >= currentInitialTownRouteTransitionSent {
		return nil
	}
	if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, transitionBody, 0); err != nil {
		return err
	}
	setCurrentTownPositionSceneFromTransitionLocked(session, characterID, townID, areaID, row)
	session.initialTownRouteStage = currentInitialTownRouteTransitionSent
	return nil
}

// waitCurrentInitialTownSceneCommitLocked preserves the former visual-cover
// boundary as a zero-duration test seam. The caller continues directly into
// the route-specific scene finalizers.
func (s *Service) waitCurrentInitialTownSceneCommitLocked(
	session *gameSession,
	characterID uint16,
) {
	if s == nil || session == nil || s.initialTownEntryWait == nil {
		return
	}
	s.logGameEvent(session, "game-initial-town-client-loading-cover-disabled",
		"char_id", characterID,
		"duration", currentInitialTownEntryDelay.String(),
		"owner", "none",
		"server_packet_stream_blocked", false)
	s.initialTownEntryWait(currentInitialTownEntryDelay)
}

// sendCurrentTownActorRoutePacketsLocked preserves the accepted completed
// dungeon-return ordering. The result-page actor remains addressable by its
// stable key, so the mode0 row is an event-suppressed state refresh; the
// following equipment-bearing mode1 rebinds its durable equipment graph. The
// caller must hold session.townMu.
func (s *Service) sendCurrentTownActorRoutePacketsLocked(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
	row currentSceneTransitionRow,
	objectBody []byte,
	mode1Body []byte,
	transitionBody []byte,
	refreshCompletedCreatureState bool,
	seedCompletedQuestState bool,
) error {
	if err := s.sendCurrentTownActorPrerequisitesLocked(session, characterID, objectBody, nil); err != nil {
		return err
	}
	if err := s.sendCurrentCompletedTownReturnPlayerStateLocked(session, characterID, objectBody, mode1Body); err != nil {
		return err
	}
	if refreshCompletedCreatureState &&
		session.initialTownRouteStage < currentInitialTownRouteCreatureStateSent {
		if err := s.sendSelectedCreatureCompletedTownReturnStateAfterMode1(
			session,
			"completed_dungeon_return_after_mode1_before_op24",
		); err != nil {
			return fmt.Errorf("rehydrate completed-return creature state: %w", err)
		}
		session.initialTownRouteStage = currentInitialTownRouteCreatureStateSent
	}
	if _, err := s.sendSelectedGuardianShieldDeckInfo(
		session,
		"completed_dungeon_return_guardian_shield_deck_after_mode1_before_op24",
	); err != nil {
		return fmt.Errorf("rehydrate completed-return guardian shield deck: %w", err)
	}
	if _, err := s.sendSelectedGuardianShieldTownAppearanceRefreshOnce(
		session,
		"completed_dungeon_return_guardian_shield_appearance_after_deck_before_op24",
	); err != nil {
		return fmt.Errorf("rehydrate completed-return guardian shield appearance: %w", err)
	}
	if _, err := s.sendSelectedChatEmoticonCatalog(
		session,
		"completed_dungeon_return_chat_emoticon_catalog_after_mode1_before_op24",
	); err != nil {
		return fmt.Errorf("rehydrate completed-return chat emoticon catalog: %w", err)
	}
	// Completed dungeon return deliberately bypasses the heavy cold-login
	// container bootstrap. Rehydrate the durable slot-32 row after its first
	// equipment-bearing mode1 and before the town transition.
	if !session.initialTownGuardianGemRefreshSent {
		if err := s.sendSelectedGuardianGemWornMedalRefresh(
			session,
			"completed_dungeon_return_after_mode1_before_op24",
		); err != nil {
			return fmt.Errorf("rehydrate completed-return guardian-gem medal: %w", err)
		}
		session.initialTownGuardianGemRefreshSent = true
	}
	// Same boundary, same reason: the worn title's raw random-enchant bytes are
	// not carried by the create rows either (random_title_enchant_refresh.go).
	if err := s.sendSelectedWornTitleEnchantRowRefresh(
		session,
		"completed_dungeon_return_after_mode1_before_op24",
	); err != nil {
		return fmt.Errorf("rehydrate completed-return worn title enchant row: %w", err)
	}
	// The current client rebuilds NPC task markers from op21/op574 only after
	// the selected actor exists. Completed dungeon return bypasses the full
	// cold-login initialization, so publish the repository/PVF snapshots at
	// the same safe pre-op24 boundary instead of leaving the task manual stale.
	// The completed bitmap (op356) must seed BEFORE these snapshots: op574 only
	// upserts active rows, so an earlier bitmap is what drops the just-finished
	// dungeon child from the HUD and keeps the map [quest condition] gates
	// addressable when the typed op24 below rebuilds the town scene. This is the
	// reference completed-return order (mode1 -> op356 -> op21/op574 -> op22/op23
	// -> op24) and differs from the dungeon-selector prefix which still emits
	// op22/op23 before op356.
	//
	// An interrupted (abandon) return is different: no dungeon quest was finished,
	// so the client still owns the pre-entry quest state it left the town with.
	// Replaying op356/op21/op574 here rebuilds that state from the repository and
	// drops the client's cached [quest condition] town gates. The reference
	// active-giveup route (sendCurrentDungeonReturnToTownLocked) publishes only
	// mode0/mode1 then op22/op23/op24 and leaves the quest state untouched, so
	// this caller passes seedCompletedQuestState=false and only the location pair
	// is emitted below.
	if seedCompletedQuestState {
		if !session.initialTownLocationNotificationsSent {
			if err := s.sendCurrentDungeonClearQuestSeedLocked(
				ctx,
				session,
				characterID,
				"current_exe_dungeon_return_town",
			); err != nil {
				return err
			}
		}
		if err := s.sendCurrentInitialTownQuestSnapshotsLocked(
			session,
			"completed_dungeon_return_after_mode1_before_typed_op24",
		); err != nil {
			return err
		}
	}
	if !session.initialTownLocationNotificationsSent {
		if err := s.sendCurrentTownLocationNotifications(
			session,
			characterID,
			townID,
			areaID,
			row,
			"completed_dungeon_return_pre_op24",
		); err != nil {
			return err
		}
		session.initialTownLocationNotificationsSent = true
	}
	if err := s.sendCurrentTownTransitionLocked(session, characterID, townID, areaID, row, transitionBody); err != nil {
		return err
	}
	// 回城和登录一样会重建客户端场景：HUD 上的持久投影（经验药水 / 公会状态+属性 buff /
	// 属性药水 / 物品冷却）都要重放。公会那条尤其不能只补 buff —— 客户端的公会状态
	// （guild ID、徽章 aura）也在重建中被清掉，只重发 buff 会出现"两个公会属性只回来一个"
	//（2026-10-08 现场：448 基础 +120 → 只回 +60）。
	if err := s.restoreCurrentSceneHudProjections(session, "dungeon_return_town_after_op24"); err != nil {
		return fmt.Errorf("restore scene HUD projections after dungeon return: %w", err)
	}
	// 婚房状态（家具/情侣树）是结婚戒指上"婚房属性"的唯一数据源，场景重建后必须补发，
	// 否则回城之后那枚戒指既不显示也不生效（只能靠小退重进婚房救回来）。
	s.restoreCurrentSceneWeddingRoomState(session, characterID, "dungeon_return_town_after_op24")
	if err := s.sendCurrentCeraShopBuyRestrictProjection(context.Background(), session, "dungeon_return_town_after_op24"); err != nil {
		s.logGameEvent(session, "game-cera-shop-buy-restrict-send-failed", "source", "dungeon_return_town_after_op24", "err", err)
	}
	// 这里刻意不推商城钱包（见 initial_town 处的说明）：回城的包序列已经被大量
	// 地城返回测试逐步钉死，钱包值在这条路径上不会变化，多发只会惊动无关用例。

	return nil
}

// sendCurrentCompletedTownReturnPlayerStateLocked rebuilds only the selected
// actor data that is absent after a completed-dungeon scene change, then writes
// the first equipment-bearing mode1. The dungeon scene already owns the skill
// manager, and replaying its full op19 snapshot here makes the current client
// interpret existing auxiliary-profession skills as a newly acquired
// qualification. Each successful native frame advances the route cursor so a
// transport failure retries only the remaining suffix. The caller must hold
// session.townMu.
func (s *Service) sendCurrentCompletedTownReturnPlayerStateLocked(
	session *gameSession,
	characterID uint16,
	objectBody []byte,
	mode1Body []byte,
) error {
	if session == nil || characterID == 0 ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRouteActorBound {
		return nil
	}
	if session.initialTownRouteStage < currentInitialTownRouteSkillStateSent {
		// A completed return keeps the learned-skill manager from the dungeon
		// generation. Native 86JP's return route likewise does not send op19.
		// Replaying it after a new town mode0 is not an initialization packet on
		// this 90CN client: it opens the expert-job qualification dialog.
		session.initialTownSkillInfoPrepared = false
		session.initialTownSkillInfoSent = true
		session.initialTownSkillInfo = currentSceneSkillInfoProjection{}
		session.initialTownRouteStage = currentInitialTownRouteSkillStateSent
		s.logGameEvent(session, "game-dungeon-completed-town-skill-projection-suppressed",
			"char_id", characterID,
			"reason", "dungeon_owned_skill_manager_must_not_replay_op19_as_a_qualification_notification")
	}
	if session.initialTownRouteStage < currentInitialTownRouteEquipmentMode1Sent {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketSetUDPIPPort),
			mode1Body,
			0,
		); err != nil {
			return err
		}
		if err := captureCurrentPartyActorProjection(session, characterID, objectBody, mode1Body); err != nil {
			s.logGameEvent(session, "game-party-actor-owner-projection-rejected",
				"char_id", characterID,
				"character_generation", session.characterGeneration,
				"mode0_len", len(objectBody),
				"mode1_len", len(mode1Body),
				"reason", err)
		}
		// Same boundary as the return-select route: the town-side op1027
		// projection is published between mode0 and this mode1, so the selected
		// box is refreshed once more against the final actor.
		if err := s.replayCollectboxSelectedRefreshAfterMode1(
			context.Background(),
			session,
			"completed_dungeon_return_after_full_mode1",
		); err != nil {
			s.logGameEvent(session, "game-collectbox-selection-refresh-deferred",
				"char_id", characterID,
				"source", "completed_dungeon_return_after_full_mode1",
				"reason", err)
		}
		session.initialTownRouteStage = currentInitialTownRouteEquipmentMode1Sent
	}
	if session.initialTownRouteStage < currentInitialTownRouteSkinAppearanceRefreshSent {
		// The initial mode0 carries the correct skin row, but the subsequent
		// complete mode1 initializes slot 8's detail object after that paper-doll
		// pass. Reapply the complete mode0 table only when the skin row is
		// occupied. This is the native existing-actor incremental refresh: its
		// actor-state event is suppressed, so it cannot replay qualification UI.
		if skinItemID, found := currentSceneObjectAppearanceSlotItemID(objectBody, 8); found &&
			skinItemID != currentActorMode0AppearanceEmptyItem {
			if err := s.sendSelectedActorMode0AppearanceRefresh(
				session,
				"completed_dungeon_return_skin_avatar",
				"mode0_generation_then_mode1_then_event_suppressed_mode0_skin_avatar_refresh_before_op559",
			); err != nil {
				return fmt.Errorf("rehydrate completed-return skin avatar appearance: %w", err)
			}
		}
		session.initialTownRouteStage = currentInitialTownRouteSkinAppearanceRefreshSent
	}
	if session.initialTownRouteStage < currentInitialTownRouteBuffRegistrationSent {
		// op24 creates the final town scene feature used by NoPack's op559
		// calculator. Advance the retry cursor here, but let the transition writer
		// send the actual registration immediately after op24. Keeping an explicit
		// pending bit makes a failed post-op24 write resume without replaying op24.
		session.completedTownReturnBuffRegistrationPending = true
		session.initialTownRouteStage = currentInitialTownRouteBuffRegistrationSent
	}
	if session.initialTownRouteStage < currentInitialTownRouteProfessionStateSent {
		// Completed return deliberately retains the dungeon skill manager and does
		// not replay op19. Restoring the durable expert-job projection here is
		// therefore only safe while it differs from the exact op205 body this
		// client already accepted: on this 90CN client op205 is an
		// acquisition/mutation notification, so replaying an unchanged
		// profession after a scene rebuild opens the "副职业资格认证" banner and
		// the one-shot "您学会新技能了" message. A real state change still
		// publishes; BUFF registration stays deferred until the final typed op24
		// scene commit below.
		if err := s.sendCurrentExpertJobInfoForSceneRebuildForSelectedCharacter(
			session,
			"completed_dungeon_return_after_mode1_before_typed_op24",
		); err != nil {
			return fmt.Errorf("rehydrate completed-return expert-job state: %w", err)
		}
		session.initialTownRouteStage = currentInitialTownRouteProfessionStateSent
		s.logGameEvent(session, "game-dungeon-completed-town-expert-job-state-restored",
			"char_id", characterID,
			"order", "event_suppressed_mode0_mode1_retained_skills_then_changed_state_only_op205",
			"qualification_packets_replayed", false)
	}
	if session.initialTownRouteStage < currentInitialTownRoutePlayerStatePrepared {
		session.initialTownRouteStage = currentInitialTownRoutePlayerStatePrepared
		session.selectedUserInfoRefreshSent = true
		s.logGameEvent(session, "game-dungeon-town-route-player-state-ready",
			"char_id", characterID,
			"route_stage", session.initialTownRouteStage,
			"sequence", "event_suppressed_mode0_equipment_mode1_optional_event_suppressed_skin_mode0_state_only_op205_typed_op24_then_op14_registered_chronicle_op559_buff_registration",
			"reason", "completed_return_keeps_the_dungeon_skill_manager_and_restores_only_expert_job_state_without_acquisition")
	}
	return nil
}

func (s *Service) sendCurrentTownActorPrerequisitesLocked(
	session *gameSession,
	characterID uint16,
	objectBody []byte,
	mode1Body []byte,
) error {
	if session == nil || characterID == 0 ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRouteArmed {
		return nil
	}
	if session.initialTownRouteStage < currentInitialTownRouteActionTableSent {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketPVPMissionHpPercent),
			buildCurrentActionTableStateBody(),
			0,
		); err != nil {
			return err
		}
		session.initialTownRouteStage = currentInitialTownRouteActionTableSent
	}
	if session.returnSelectTownReentryPending &&
		session.returnSelectPreviousCharacterID == characterID &&
		!session.returnSelectRetainedActorRemoved {
		if err := s.sendCurrentSceneFixedClass0Packet(
			session,
			currentTownUserLeaveMsgID,
			buildTownUserLeaveBody(characterID),
			"same_character_return_select_before_mode0",
		); err != nil {
			return err
		}
		session.returnSelectRetainedActorRemoved = true
		s.logGameEvent(session, "game-return-select-retained-actor-remove-send",
			"char_id", characterID,
			"msg_id", currentTownUserLeaveMsgID,
			"reason", "same_character_reselect_removes_existing_town_actor_before_mode0")
	}
	if session.initialTownRouteStage < currentInitialTownRouteObjectSent {
		objectPacket := csharpSelectInitPacket{
			class: 0,
			msgID: uint16(dnfenum.CmdPacketSetUDPIPPort),
			kind:  csharpCurrentSceneObjectListKind,
		}
		if err := s.sendCSharpSelectInitPacket(session, objectPacket, objectBody); err != nil {
			return err
		}
		session.initialTownRouteStage = currentInitialTownRouteObjectSent
	}
	if session.initialTownRouteStage < currentInitialTownRouteActorBound {
		if len(mode1Body) > 0 {
			if err := s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketSetUDPIPPort),
				mode1Body,
				0,
			); err != nil {
				return err
			}
			if err := captureCurrentPartyActorProjection(
				session,
				characterID,
				objectBody,
				mode1Body,
			); err != nil {
				// This cache is a party-admission prerequisite, not a login
				// prerequisite. Keep the already-proved single-player town route
				// unchanged and let party admission fail closed if an exact row pair
				// was unavailable.
				s.logGameEvent(session, "game-party-actor-owner-projection-rejected",
					"char_id", characterID,
					"character_generation", session.characterGeneration,
					"mode0_len", len(objectBody),
					"mode1_len", len(mode1Body),
					"reason", err)
			} else {
				s.logGameEvent(session, "game-party-actor-owner-projection-captured",
					"char_id", characterID,
					"character_generation", session.characterGeneration,
					"owner_server", objectBody[3],
					"owner_channel", objectBody[4],
					"mode0_len", len(objectBody),
					"mode1_len", len(mode1Body),
					"source", "successful_selected_actor_mode0_mode1_route")
			}
		} else {
			s.logPacketEvent("game-initial-town-prerequisite-mode1-skipped",
				"conn_id", session.connID,
				"channel_id", session.channel.ID,
				"char_id", characterID,
				"object_key", currentSceneActorObjectKey(characterID),
				"reason", "route_policy_omits_actor_binding_mode1")
		}
		// A successfully created selected actor is a new native generation. The
		// next post-skill op205 must not be suppressed by the preceding actor's
		// character-ID cache; retries skip this reset because the route cursor has
		// already advanced to ActorBound.
		session.expertJobInfoCharacterID = 0
		session.initialTownRouteStage = currentInitialTownRouteActorBound
	}
	// mode0 has created the selected actor. Replay the authoritative collection-
	// box state at this generation boundary so the native owner applies its
	// combat-stat bonuses before the scene snapshot/op24 transition.
	if !session.collectboxSceneProjectionSent {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		err := s.sendCollectboxSceneInitializationStates(ctx, session)
		cancel()
		if err != nil {
			s.logGameEvent(session, "game-collectbox-scene-projection-deferred",
				"char_id", characterID,
				"source", "town_actor_after_mode1",
				"reason", err)
		}
	}
	return nil
}

func (s *Service) sendCurrentTownTransitionLocked(
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
	row currentSceneTransitionRow,
	transitionBody []byte,
) error {
	if session == nil || characterID == 0 ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRouteActorBound {
		return nil
	}
	if session.initialTownRouteStage < currentInitialTownRouteTransitionSent {
		if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, transitionBody, 0); err != nil {
			return err
		}
		setCurrentTownPositionSceneFromTransitionLocked(session, characterID, townID, areaID, row)
		session.initialTownRouteStage = currentInitialTownRouteTransitionSent
	}
	if session.initialTownRouteStage == currentInitialTownRouteTransitionSent {
		if session.completedTownReturnBuffRegistrationPending {
			ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			err := s.sendSkillSwitchInventoryAllDataRefresh(
				ctx,
				session,
				"completed_dungeon_return_after_typed_op24",
			)
			cancel()
			if err != nil {
				return fmt.Errorf("rehydrate completed-return BUFF registration after op24: %w", err)
			}
			session.completedTownReturnBuffRegistrationPending = false
		}
		// The selected actor's authoritative op19 was installed before mode1 and
		// op24. Consume its route invariant without replaying it: a duplicate
		// initial snapshot is interpreted as a newly learned qualification skill.
		if err := s.commitPreparedInitialTownSkillInfoAfterTransition(
			session,
			"initial_town_after_typed_op24",
		); err != nil {
			return err
		}
		session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
		session.townSceneReadyCharacterID = characterID
	}
	return nil
}
