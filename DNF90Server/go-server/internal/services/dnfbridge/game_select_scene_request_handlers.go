package dnfbridge

import (
	"context"
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func selectInitPacketAllowedBeforeScene(packet csharpSelectInitPacket) bool {
	return packet.kind == "select_ack"
}

func selectInitPacketSupersededByLongHengScene(packet csharpSelectInitPacket) bool {
	switch packet.kind {
	case "select_load_empty", "select_scene_probe", "select_scene_pet_list", "select_scene_userinfo", "select_scene_safe_error":
		return true
	}
	return packet.kind == "" &&
		packet.class == 0 &&
		packet.msgID == uint16(dnfenum.CmdPacketLeaveParty) &&
		len(packet.body) == 5 &&
		packet.body[0] <= 2
}

func selectInitPacketRequiresCurrentSceneObject(packet csharpSelectInitPacket) bool {
	return packet.msgID == uint16(dnfenum.CmdPacketInsertOverseer)
}

func (s *Service) sendFinishLoadingStatus(session *gameSession, request []byte) error {
	if session == nil {
		return nil
	}
	// sub_217E0C0 writes two semantic u32 zero values, but the current client
	// can send op37 through more than one protected/legacy boundary. Production
	// evidence on 2026-07-21 shows the live EXE sends the eight-byte semantic
	// body (two zero u32 values) after dungeon op29, while older captures used
	// the 16-byte protected wrapper. Accept only these two proven shapes before
	// sending the ACK or advancing any finish-loading lifecycle gate.
	if !currentFinishLoadingRequestBodyAccepted(request) {
		s.logGameEvent(session, "game-upper-finish-loading-request-rejected",
			"request_body_len", len(request),
			"expected_body_lens", "8_or_16",
			"reason", "current_live_legacy_op37_body_length_mismatch")
		return nil
	}
	actorReady, actorReadySource := s.currentSceneActorReadyForState(session)
	s.logGameEvent(session, "game-upper-finish-loading-status-send",
		"msg_id", uint16(dnfenum.CmdPacketFinishLoading),
		"classification", dnfproto.DefaultChannelClassification,
		"request_body_len", len(request),
		"plain_body_len", 1,
		"body_source", "current_exe_sub_1CF50C0_success_reads_no_body_after_discriminator",
		"actor_ready_source", actorReadySource,
		"main_op37", "sent_after_status_when_selected_actor_and_scene_transition_are_complete")
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketFinishLoading), nil); err != nil {
		return err
	}
	// A completed dungeon return already published its town actor/scene state.
	// The current client nevertheless emits its normal op37 request after that
	// transition. Replaying the class0/op37 character-state reader at this
	// point opens the personal-information refresh UI. Keep the proven empty
	// status ACK, but do not turn this return-side request into a second
	// progression snapshot.
	if session.returnTownFinishLoadingAckOnly {
		if err := s.ensureCurrentConfirmedDungeonReturnPlayerState(
			session,
			"request37_after_confirmed_direct_return",
		); err != nil {
			s.logWarn("dnfbridge deferred confirmed return player state after op37 ACK",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"error", err)
			// The success ACK may already be on the wire, but the town
			// actor/HUD generation is not complete. Propagate the failure so
			// the connection cannot continue in a falsely-ready state; the
			// staged cursor and confirmation marker remain available to the
			// next current-client confirmation or reconnect.
			return err
		}
		s.logGameEvent(session, "game-upper-finish-loading-return-town-ack-only",
			"request_body_len", len(request),
			"reason", "completed_dungeon_town_state_already_published")
		return nil
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	var partyDungeonOwner *runtimePartyDungeonOwner
	if runtime != nil {
		partyDungeonOwner = runtime.partyDungeonOwner
	}
	session.dungeon.mu.Unlock()
	if partyDungeonOwner != nil {
		// A previous barrier release can have completed its actor projection and
		// then failed on the final empty class0/op42 reset. registerReady is
		// intentionally one-shot, so repair that suffix before its duplicate-ready
		// fast path. Never reset before the op120/op19 projection is complete.
		if session.currentFinishLoadingCompletionSent && session.postFinishLoadingPlayerStateSent {
			if err := s.sendCurrentDungeonEntryConsumableUseGateReset(
				session,
				"party_dungeon_duplicate_finish_loading_retry_after_op37_op30",
			); err != nil {
				return err
			}
		}
		registration, err := partyDungeonOwner.registerReady(
			session.selectedCharacterID,
			session.characterGeneration,
		)
		if err != nil {
			return err
		}
		s.logGameEvent(session, "game-party-dungeon-loading-ready",
			"party_id", partyDungeonOwner.partyID,
			"slot", registration.slot,
			"cycle", registration.cycle,
			"first", registration.first,
			"release", registration.release)
		// 团本记录：读条屏障的每一次登记。ready/expected 能直接回答
		// "全队卡在读条时到底谁没到"，is_driver 能回答"是谁在替全队放行"。
		readyBits, expectedBits, released, cycle := partyDungeonOwner.readySnapshot()
		s.recordRaidEvent(session, "loading_ready", map[string]any{
			"party_id":           partyDungeonOwner.partyID,
			"party_leader_id":    partyDungeonOwner.leaderID,
			"loading_cycle":      cycle,
			"registration_cycle": registration.cycle,
			"slot":               int(registration.slot),
			"ready_first":        registration.first,
			"is_driver":          registration.release,
			"ready_mask":         readyBits,
			"expected_mask":      expectedBits,
			"ready_count":        raidBitsCount(readyBits),
			"expected_count":     raidBitsCount(expectedBits),
			"barrier_released":   released,
			"missing_slots":      raidMissingSlots(readyBits, expectedBits),
		})
		if !registration.first || !registration.release {
			return nil
		}
		// op26 makes the leader slot the native UDP/controller authority on every
		// client. Finish every follower's post-loading actor chain before the
		// leader receives its own release; otherwise the leader can start room
		// combat while another peer still lacks the room-ready state. The initial
		// loading cycle also installs op19; later cycles deliberately preserve the
		// live skill/Buff manager instead of rebuilding it on every room change.
		// 并发释放，但顺序必须满足上面的约束：所有【非队长】成员的 release 先完成，
		// 队长最后才 release。TestPartyLeaderDungeonSelectionEntersEveryOnlineMemberWithStablePartyIndex
		// 正是按「follower 的 loading 链整体先于 leader 的 release」断言的。
		//
		// 改前是「逐个成员现开一个 createWriteTimeout 再同步等」，N 个成员就是
		// 3s × (N-1) 的串行累加，把本会话的 event loop 卡住数秒；实测 2026-09-25
		// 00:27 单成员自身就要 5.95s，3s 预算必然超时并让整个 finish-loading
		// 流程中止。并发后总耗时从 Σ 变成 max。
		roster := partyDungeonOwner.loadingReleaseRoster()
		leaderID := partyDungeonOwner.leaderID

		// 自己必须【直接】调用，绝不能走 callGameSession：那是对自己的 event loop
		// 发一次同步等待，而当前正跑在该 loop 上，会自锁到预算耗尽。
		releaseSelf := func() error {
			return s.completeRuntimePartyDungeonLoading(session, partyDungeonOwner, registration.cycle)
		}
		releaseFollower := func(target sessionFanoutTarget) error {
			return s.completeRuntimePartyDungeonLoading(target.session, partyDungeonOwner, registration.cycle)
		}

		var followers []sessionFanoutTarget
		var leaderTarget *sessionFanoutTarget
		selfInRoster, selfIsLeader := false, false
		var structuralErr error
		for _, member := range roster {
			if member.userID == session.selectedCharacterID &&
				member.generation == session.characterGeneration {
				selfInRoster = true
				selfIsLeader = member.userID == leaderID
				continue
			}
			identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
			if !current || identity.session == nil || identity.generation != member.generation {
				// 这个成员的会话已经换了一代（通常是掉线或重连）。改前这里直接中止，
				// 会让排在其后的成员连 release 都拿不到；改为记下错误，先把其余仍然
				// 有效的成员放行，最后再把这个错误交给调用方。
				if structuralErr == nil {
					structuralErr = fmt.Errorf("party dungeon loading member %d session generation changed", member.userID)
				}
				continue
			}
			target := sessionFanoutTarget{
				label:       fmt.Sprintf("%d", member.userID),
				characterID: member.userID,
				session:     identity.session,
			}
			if member.userID == leaderID {
				leader := target
				leaderTarget = &leader
				continue
			}
			followers = append(followers, target)
		}

		releaseStarted := time.Now()
		// 队长会话：自己就是队长时用本会话，否则用上面解析到的那个。
		leaderSession, leaderCharID := session, session.selectedCharacterID
		if leaderTarget != nil {
			leaderSession, leaderCharID = leaderTarget.session, leaderTarget.characterID
		}
		releaseLeader := func() error {
			return s.completeRuntimePartyDungeonLoading(leaderSession, partyDungeonOwner, registration.cycle)
		}

		// 第一步：非队长成员。自己若是 follower，先直接调用放行（必须在自己的
		// loop 上直接调用；走 callGameSession 就是对自己的 loop 发同步等待，会自锁）。
		selfReleasedInline := false
		if selfInRoster && !selfIsLeader {
			if err := releaseSelf(); err != nil {
				return err
			}
			partyDungeonOwner.markLoadingReleased(session.selectedCharacterID, registration.cycle)
			selfReleasedInline = true
		}

		// 第二步：队长最后放行。两条路：
		//
		//   - 没有任何需要先完成的 follower（例如只有队长一人的队伍）：顺序要求空真，
		//     走同步路径，与旧行为完全一致，也不让这种队伍白等一个事件循环轮次；
		//   - 有 follower：必须**投递**。同步等每个成员的 loop 就是本次修复要消灭的
		//     那个 10 秒停摆 —— 多人同时进图时每个 loop 都停在同一个等待里，只能等
		//     预算到点（实测发起者 loop 被停住 10.0004 秒，那 10 秒它一个包都处理不了）。
		//     then 只在全员完成后被调用，因此"follower 的链整体先于队长的 release"
		//     这条线序契约由构造保证；then 可能跑在最后一个完成者的 loop 上，也可能
		//     跑在预算到点的计时器协程上，所以它只允许投递，不允许直接改会话状态。
		leaderRelease := func() error {
			if selfInRoster && selfIsLeader {
				return releaseLeader()
			}
			if leaderTarget != nil {
				return releaseFollower(*leaderTarget)
			}
			return nil
		}
		leaderReleaseInRoster := selfInRoster && selfIsLeader
		if len(followers) == 0 {
			if leaderReleaseInRoster || leaderTarget != nil {
				if err := leaderRelease(); err != nil {
					return err
				}
				partyDungeonOwner.markLoadingReleased(leaderCharID, registration.cycle)
			}
		} else {
			s.fanOutToGameSessionsAsync(context.Background(), followers,
				"party-dungeon-loading-release", releaseFollower, func() {
					if !leaderReleaseInRoster && leaderTarget == nil {
						return
					}
					// 自己就是队长 → 投给自己；否则投给队长。
					target := session
					targetCharID := leaderCharID
					if !leaderReleaseInRoster && leaderTarget != nil {
						target = leaderTarget.session
						targetCharID = leaderTarget.characterID
					}
					if err := s.postGameSessionCharacterEvent(
						target, "raid-loading-leader-last",
						targetCharID, target.characterGeneration,
						func() error {
							if err := leaderRelease(); err != nil {
								return err
							}
							partyDungeonOwner.markLoadingReleased(targetCharID, registration.cycle)
							return nil
						},
					); err != nil {
						s.recordRaidEvent(session, "loading_leader_release_deferred", map[string]any{
							"party_id":      partyDungeonOwner.partyID,
							"loading_cycle": registration.cycle,
							"reason":        "leader_loop_submit_failed",
							"error":         errorText(err),
						})
					}
				})
		}

		// 团本记录：这一次读条放行的形状与耗时。total_wall_ms 现在是"本成员自己这
		// 一步"的墙钟（不再是"等全队"的时间），follower_count 是被投递出去的成员数。
		s.recordRaidEvent(session, "loading_release_driver", map[string]any{
			"party_id":              partyDungeonOwner.partyID,
			"party_leader_id":       partyDungeonOwner.leaderID,
			"loading_cycle":         registration.cycle,
			"is_leader":             selfIsLeader,
			"self_in_roster":        selfInRoster,
			"self_released_inline":  selfReleasedInline,
			"follower_count":        len(followers),
			"leader_target_present": leaderTarget != nil,
			"total_wall_ms":         raidMillis(time.Since(releaseStarted)),
			"structural_error":      errorText(structuralErr),
		})
		return structuralErr
	}
	if !actorReady {
		s.logGameEvent(session, "game-main-finish-loading-state-deferred",
			"request_body_len", len(request),
			"actor_ready_source", actorReadySource,
			"reason", "selected_actor_or_scene_transition_incomplete")
		return nil
	}
	if err := s.sendCurrentFinishLoadingCharacterState(session, "request37_after_upper_status"); err != nil {
		return err
	}
	if err := s.sendCompletedTutorialReentryExit(session, "request37_after_current_finish_loading_state"); err != nil {
		return err
	}
	if err := s.sendRuntimeRaidDungeonParticipationOnFinishLoading(session); err != nil {
		return err
	}
	if err := s.startCurrentSuspiciousVillageElevator(session, "request37_after_current_finish_loading_state"); err != nil {
		return err
	}
	if err := s.sendCurrentHellPartyMonsterInfoAfterFinishLoading(session, "request37_after_scene_completion"); err != nil {
		return err
	}
	// 装载完成后把卢克的阶段复活币已用数补发给这一个成员（重连 / 后进队的人靠它对齐）。
	// 非卢克时该函数内部直接返回，不改动任何既有行为。
	s.syncRuntimeRaidPhaseCoinUsageAfterFinishLoading(session, "request37_after_scene_completion")
	// 物件脚本门（上游 b402882）：装载完成后把被动对象脚本已经打开的 GATE ENABLE 补发出去。
	s.publishCurrentDungeonActionObjectGateAfterFinishLoading(session, "request37_after_scene_completion")
	// op120 and the optional tutorial/raid/elevator/Hell suffix can all run
	// after op37/op30. The current EXE's native item-use wait byte must be reset
	// only after that entire solo loading chain is complete. A failed write keeps
	// the dungeon-generation marker armed for a duplicate op37 retry.
	return s.sendCurrentDungeonEntryConsumableUseGateReset(
		session,
		"request37_after_complete_solo_loading_projection",
	)
}

func (s *Service) sendContentsPlayInfoState(session *gameSession, body []byte) error {
	s.logGameEvent(session, "game-contents-play-info-deferred",
		"body_len", len(body),
		"reason", "dove_scene_flow_has_no_s2c_704_and_current_client_opens_side_panel")
	// DOVE 完整进场景 S2C 流没有 704；当前 NoPack 收到 704 成功体会打开右侧公告/聊天面板。
	return nil
}

func (s *Service) sendRequestBlacklistState(session *gameSession, source string, request []byte) error {
	if session == nil {
		return nil
	}
	// Hold the answer back until the select-character scene tail is committed. The
	// client builds each row against the scene it is standing in, and a row built
	// while the bootstrap is still pending comes out with an empty 黑名单日期 cell (its
	// level is 0 for the same reason). Live 2026-09-22 23:46 (conn game-000245): the
	// client asked six times, the first three bracketed the tail commit and rendered
	// the date, then a route transition re-deferred the tail and the last three
	// answers rebuilt the row without it. The client asks again on its own, and
	// sendCurrentDeferredSelectSceneTail flushes the pending answer when the tail
	// commits, so nothing is lost when it does not.
	if !session.sceneBootstrapTailSent {
		session.blacklistListAnswerPending = true
		s.logPacketEvent("game-upper-request-blacklist-held-until-scene-tail",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketRequestBlacklist),
			"request_body_len", len(request),
			"reason", "select_character_scene_tail_not_committed_a_row_built_now_loses_its_blacklist_date")
		return nil
	}
	return s.sendCurrentBlacklistListAnswer(session, source, request)
}

// sendCurrentBlacklistListAnswer is the type=120 answer itself.
//
// The stored list is projected onto the layout the client's op120 reader expects
// (see blacklist.go): `u8 flag | u8 count | count * (u32 name | name | y | m | d)`.
// An empty list answers with currentRequestBlacklistResponseBody verbatim, which is
// byte for byte what this path sent before the layout was known.
func (s *Service) sendCurrentBlacklistListAnswer(session *gameSession, source string, request []byte) error {
	records := s.loadCurrentBlacklistRecordsForSession(session)
	responseBody := buildCurrentBlacklistListBody(records)
	session.blacklistListAnswerPending = false
	s.logPacketEvent("game-upper-request-blacklist-passive-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketRequestBlacklist),
		"classification", dnfproto.DefaultChannelClassification,
		"request_body_len", len(request),
		"plain_body_len", len(responseBody),
		"blacklist_count", len(records),
		"body_source", "current_upper_op120_blacklist_result_flag_count_name_date",
		"reason", "current_nopack_upper_op120_is_distinct_from_main_scene_op120")
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRequestBlacklist), responseBody, dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if !session.sceneBootstrapTailSent {
		s.logPacketEvent("game-upper-request-blacklist-before-hud-replay-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "current_object_create_mode0_is_one_shot_runtime_uses_after_blacklist_structures")
	}
	if !session.postStartMapPlayerStateSent {
		s.logPacketEvent("game-upper-request-blacklist-runtime-seed-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "blacklist_ack_is_request_driven_but_runtime_seed_waits_for_op29_player_state")
		return nil
	}
	if err := s.sendRuntimeAfterBlacklistSeedOnce(session, source+"_runtime_seed"); err != nil {
		return err
	}
	s.logPacketEvent("game-upper-request-blacklist-runtime-seed-current-struct-complete",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"reason", "runtime_after_blacklist_sequence_sent_without_replaying_before_hud_object_create")
	return nil
}

func (s *Service) sendGuildAllMemberListState(session *gameSession, source string, request []byte) error {
	s.logPacketEvent("game-upper-guild-all-member-list-passive-deferred",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketGuildAllMemberList),
		"classification", dnfproto.DefaultChannelClassification,
		"request_body_len", len(request),
		"plain_body_len", 0,
		"reason", "defer_guild_member_list_during_select_scene_bootstrap_no_dove_replay")
	// 当前 NoPack 在选角进场景前收到 op140 大表会拉起右侧社交/系统面板；先延后响应，等场景稳定后再恢复真实公会面板。
	return nil
}
