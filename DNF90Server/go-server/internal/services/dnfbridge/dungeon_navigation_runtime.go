package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentSelectDungeonEntryLevelTooLowReason byte = 14
const currentSelectDungeonEntryItemMissingReason byte = 17

// 深渊（inferno / 玩家勾选深渊派对）的入场道具失败用 reason 27：客户端 254 路
// switch 的 case 27 才会弹邀请函那条（42462 带道具名 / 27156 通用），并且只有这个
// 分支会 sub_3419C60(1,1) 把界面元素解锁；case 17 的文案是通用的
// "因[%s]缺少入场材料，无法进入『%s』。"（47707），其余入场道具继续用它。
const currentSelectDungeonHellInvitationMissingReason byte = 27
const currentSelectDungeonRaidNotReadyReason byte = 3
const currentSelectDungeonPreparationRejectedReason byte = 3
const currentSelectDungeonDailyEntryLimitReachedReason byte = 0xF6
const currentSelectDungeonPartyFatigueExhaustedReason byte = 254
const currentSelectDungeonFatigueExhaustedReason byte = 255

func (s *Service) handleDungeonSelectUpper(session *gameSession, body []byte) error {
	return s.handleDungeonSelectUpperPlanned(session, body, nil, true, false)
}

// handleDungeonSelectUpperForSettlementRetry is the ordinary entry driven by the
// settlement dialog's 再次挑战.
//
// It differs in exactly one check: a party member that is still sitting on its own
// completed run's settlement page is accepted and driven by the server instead of
// being refused for "selector is not ready". Only the attack-party leader owns 再次挑战
// (a member's client offers only 返回城镇), so the leader's single request has to carry
// the team -- which is also how 86JP does it: DungeonEntryHandler's PARTY_LOADING_GATE
// preloads the entry to every member session and waits for their FINISH_LOADING, and
// never asks whether a member's selector page is up (DungeonEntryHandler.cs:2460-2560).
// See prepareRuntimePartyDungeonEntry for the exact scope of the relaxation.
func (s *Service) handleDungeonSelectUpperForSettlementRetry(session *gameSession, body []byte) error {
	return s.handleDungeonSelectUpperPlanned(session, body, nil, true, true)
}

type runtimePartyDungeonEntryPlan struct {
	dungeonID int64
	mazeIndex int
	maps      map[worldmap.RoomCoordinate]int64
	room      worldmap.RoomCoordinate
	mapID     int64
	boss      worldmap.RoomCoordinate
	seed      uint32
}

type runtimePartyPreparedDungeonEntry struct {
	session          *gameSession
	runtime          *runtimeDungeonState
	scene            worldmap.DungeonRoomScene
	plan             runtimePartyDungeonEntryPlan
	entryItemUpdates []currentItemListEntry
	committed        bool
}

func (s *Service) handleDungeonSelectUpperPlanned(
	session *gameSession,
	body []byte,
	prepared *runtimePartyPreparedDungeonEntry,
	fanoutParty bool,
	retryFromSettlement bool,
) error {
	if len(body) != dungeoncmd.SelectDungeonRequestSize {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"body_len", len(body),
			"expected_body_len", dungeoncmd.SelectDungeonRequestSize,
			"reason", "current_exe_op16_request_boundary_mismatch")
		return nil
	}
	request, err := dungeoncmd.DecodeSelectDungeonRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"body_len", len(body),
			"reason", "current_exe_op16_request_malformed",
			"error", err)
		return nil
	}
	partyStateAtRequest := runtimePartyStateSnapshot(session)
	partyMemberIndex, partyMemberIndexed := runtimePartyMemberIndex(session.selectedCharacterID, partyStateAtRequest)
	s.logGameEvent(session, "game-dungeon-select-request-decoded",
		"body_len", len(body),
		"dungeon_id", request.DungeonID,
		"difficulty", request.Difficulty,
		"entry_option", request.EntryOption,
		"selection_mode", request.SelectionMode,
		"runtime_state", request.RuntimeState,
		"runtime_token", request.RuntimeToken,
		"reserved", request.Reserved,
		"party_state", request.PartyState,
		"offset16_unproven", request.LeaderObjectKey,
		"special_mode", request.SpecialMode,
		"source", "current_exe_sub_342EA40_fixed_21_byte_writer")
	if err := s.commitPendingDungeonReturnForSceneRequest(
		session,
		"current_exe_op16_after_pending_town_transition",
	); err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"dungeon_id", request.DungeonID,
			"difficulty", request.Difficulty,
			"reason", "pending_town_transition_commit_failed",
			"error", err)
		return nil
	}
	if prepared == nil {
		if handled, err := s.replayDungeonSelectAckForActiveRuntime(session, request, len(body)); handled || err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	var runtime *runtimeDungeonState
	var scene worldmap.DungeonRoomScene
	if prepared != nil {
		if prepared.session != session || prepared.runtime == nil || prepared.runtime.Session == nil ||
			!sameSelectDungeonRequest(prepared.runtime.Request, request) {
			return fmt.Errorf("prepared party dungeon entry does not belong to the target session")
		}
		runtime, scene = prepared.runtime, prepared.scene
	} else {
		var err error
		runtime, scene, err = s.prepareDungeonRuntime(ctx, session, request)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-select-blocked",
				"dungeon_id", request.DungeonID,
				"difficulty", request.Difficulty,
				"reason", "dungeon_owner_validation_failed",
				"error", err)
			// 卢克的路线锁拒绝必须显式告诉玩家：客户端不会在另一条路线的门上画锁，
			// 不给提示的话这次点击就是弹回选图页、毫无解释。
			if errors.Is(err, errRuntimeRaidLukeRouteLocked) {
				if noticeErr := s.sendNotice(session, noticeRequest{
					Surface: noticeSurfaceOverlay,
					Text:    currentRaidLukeRouteLockedNoticeText,
					Color:   currentClientNoticeDefaultColor,
					Source:  "luke_phase_two_route_locked",
				}); noticeErr != nil {
					s.logGameEvent(session, "game-raid-luke-route-notice-failed",
						"dungeon_id", request.DungeonID, "error", noticeErr)
				}
			}
			if reason, reply := selectDungeonFailureReason(err); reply {
				if errors.Is(err, errDungeonFatigueExhausted) {
					return s.sendSelectDungeonFatigueFailure(
						session,
						selectDungeonFatiguePromptReason(session, partyStateAtRequest),
					)
				}
				return s.sendSelectDungeonFailure(session, reason)
			}
			return nil
		}
	}
	runtime.partyMemberIndex, runtime.partyMemberIndexed = partyMemberIndex, partyMemberIndexed
	// A raid member moving between its own runs of the active phase -- the energy
	// interception gate's wall warp into a hatchery (218 -> 221..224), or the same
	// completed gate's retry -- arrives as an ordinary op16, but the TOWN co-presence
	// preflight cannot apply to it: that preflight demands the whole party stand in
	// one town area, which is impossible while they are inside the raid. Routing the
	// warp through it is what once refused the requesting member and then threw its
	// client back to town: live 2026-09-19 14:36:30Z / 14:39:15Z / 14:41:14Z, raid
	// 1507338 ("孵化中心只能进去一个人，其他的人强制返回城镇").
	//
	// What must NOT change is that this is a PARTY entry. 安徒恩的孵化场正常就是组队进的:
	// the squad walks to the portal together and the whole squad changes dungeon. Three
	// earlier shapes were all wrong, and the live logs say why:
	//
	//   * refusing when the party could not be assembled (the original bug), and
	//   * letting the opener warp solo (live 2026-09-19 23:36:14Z, raid 1507329:
	//     char 1 entered 222 alone, char 22 stayed in 218 for 235 s with every op45
	//     consumed as shared_room_transition_is_leader_owned until the server forced
	//     it back to town), and
	//   * driving the squadmates separately AFTER a solo opener (live 2026-09-20
	//     00:50:37Z: the party entry parks an all-member registration barrier in
	//     emitRuntimePartyDungeonStart, so an opener that never registers leaves its
	//     own client waiting on the loading gate and its squadmate stopped right
	//     after op5 -- neither of them could enter).
	//
	// So the warp stays an ordinary party entry end to end, with only the two gates
	// that cannot hold inside a raid relaxed (see prepareRuntimePartyDungeonEntry's
	// inRunTransfer argument). A party that cannot be prepared is REFUSED, never split:
	// everyone stays together in the gate. The solo path below is reached only when
	// there is no party to carry (PartyID <= 0, or a one-member party), which is not a
	// fallback but the absence of a party.
	inRunTransfer := false
	if prepared == nil && fanoutParty && s.currentRaidInRunTransferEntry(session, request) {
		s.logGameEvent(session, "game-raid-in-run-transfer-entry",
			"char_id", session.selectedCharacterID,
			"dungeon_id", request.DungeonID,
			"maze_index", runtime.MazeIndex,
			"reason", "raid_member_transfers_inside_its_own_gate_as_a_party_entry")
		inRunTransfer = true
	}
	var preparedFollowers []*runtimePartyPreparedDungeonEntry
	if prepared == nil && fanoutParty {
		var err error
		preparedFollowers, err = s.prepareRuntimePartyDungeonEntry(
			ctx, session, partyStateAtRequest, request, runtime, scene, retryFromSettlement, inRunTransfer)
		if err != nil {
			s.logGameEvent(session, "game-party-dungeon-entry-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"difficulty", request.Difficulty,
				"reason", "party_entry_preflight_failed_before_any_member_commit",
				"error", err)
			if errors.Is(err, errDungeonFatigueExhausted) {
				return s.sendSelectDungeonFatigueFailure(
					session,
					currentSelectDungeonPartyFatigueExhaustedReason,
				)
			}
			return s.sendSelectDungeonFailure(session, currentDungeonAdmissionCounterFailureReason(err))
		}
	}
	if prepared == nil && len(preparedFollowers) > 0 {
		if err := s.attachRuntimePartyDungeonOwner(session, runtime, preparedFollowers, partyStateAtRequest); err != nil {
			s.logGameEvent(session, "game-party-dungeon-entry-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"difficulty", request.Difficulty,
				"reason", "shared_party_dungeon_owner_preflight_failed",
				"error", err)
			return s.sendSelectDungeonFailure(session, currentSelectDungeonPreparationRejectedReason)
		}
	}
	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"dungeon_id", request.DungeonID,
			"maze_index", runtime.MazeIndex,
			"reason", "ordinary_dungeon_town_return_origin_unavailable",
			"error", err)
		return nil
	}
	initialVisit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"dungeon_id", request.DungeonID,
			"maze_index", runtime.MazeIndex,
			"reason", "initial_room_visit_unavailable_for_passive_object_drop_plan",
			"error", err)
		return nil
	}
	passiveDropPlan, err := s.planCurrentDungeonPassiveObjectDrops(
		session,
		runtime,
		scene,
		runtime.NextObjectKey,
		initialVisit.DropRNG,
	)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"dungeon_id", request.DungeonID,
			"maze_index", runtime.MazeIndex,
			"reason", "initial_passive_object_drop_preflight_failed",
			"error", err)
		return nil
	}
	if _, err := buildCurrentDungeonEntryPacketsWithPassiveDrops(runtime, scene, passiveDropPlan.ExtraEntries); err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"dungeon_id", request.DungeonID,
			"maze_index", runtime.MazeIndex,
			"reason", "dungeon_entry_packet_preflight_failed",
			"error", err)
		return nil
	}
	// The direct-button transitions (training room 5000 and job-change trial
	// experience 7142, state 1, token UINT16_MAX, solo) carry the op15 actor
	// straight into op28/op29; replaying the ordinary op16 ACK/actor rebind/op27
	// chain on them builds a second scene owner. They must also bypass the
	// ordinary admission-counter, altar and party-item commitments below: the
	// reference flow enters these guided rooms with no resource consumption.
	if prepared == nil && partyStateAtRequest.PartyID <= 0 && isCurrentDirectDungeonEntryRequest(request) {
		if err := s.commitDungeonRuntime(session, runtime); err != nil {
			s.logGameEvent(session, "game-dungeon-select-blocked",
				"dungeon_id", request.DungeonID,
				"maze_index", runtime.MazeIndex,
				"reason", "dungeon_runtime_commit_failed_before_direct_entry",
				"error", err)
			return s.sendSelectDungeonFailure(session, currentSelectDungeonPreparationRejectedReason)
		}
		return s.sendCurrentPungjinTrainingDirectEntry(session, runtime, scene, passiveDropPlan)
	}
	var dungeonAdmissionCounterBatch *currentDungeonAdmissionCounterBatch
	if prepared == nil && !currentDungeonRuntimeIsPractice(runtime) {
		dungeonAdmissionCounterBatch, err = s.reserveCurrentDungeonAdmissionCounters(
			ctx, session, runtime, preparedFollowers)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-select-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"party_member_count", len(preparedFollowers)+1,
				"reason", "party_admission_counter_preflight_or_reserve_failed_before_inventory_commit",
				"error", err)
			return s.sendSelectDungeonFailure(session, currentSelectDungeonPreparationRejectedReason)
		}
	}
	var altarAdmission *currentDungeonAltarAdmissionReservation
	if prepared == nil && !currentDungeonRuntimeIsPractice(runtime) {
		altarAdmission, err = s.reserveCurrentDungeonAltarAdmission(ctx, session, runtime)
		if err != nil {
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(dungeonAdmissionCounterBatch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			s.logGameEvent(session, "game-dungeon-select-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"reason", "altar_admission_gold_or_counter_reserve_failed",
				"error", err)
			return s.sendSelectDungeonFailure(session, currentSelectDungeonPreparationRejectedReason)
		}
	}
	var dungeonEntryItemUpdates []currentItemListEntry
	var dungeonEntryInventoryBatch *currentDungeonEntryInventoryBatch
	if prepared != nil {
		dungeonEntryItemUpdates = append([]currentItemListEntry(nil), prepared.entryItemUpdates...)
	} else if !currentDungeonRuntimeIsPractice(runtime) {
		dungeonEntryInventoryBatch, err = s.consumeCurrentDungeonEntryPartyItems(
			ctx, session, runtime, preparedFollowers)
		if err != nil {
			if restoreErr := s.restoreCurrentDungeonAltarAdmission(altarAdmission); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(dungeonAdmissionCounterBatch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			s.logGameEvent(session, "game-dungeon-select-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"party_member_count", len(preparedFollowers)+1,
				"reason", "party_admission_item_missing_or_consume_failed_before_any_runtime_commit",
				"error", err)
			if errors.Is(err, errDungeonEntryItemMissing) {
				return s.sendCurrentDungeonEntryItemMissingFailure(session, runtime, err)
			}
			// 走到这里的是入场计划本身建立不起来（例如 PVF 里既没有
			// [hell party required item count] 也没有 [minimum required level]）。
			// 以前这里 return nil：服务端一个字都不回，客户端停在选择页上不动
			// （2026-09-26 现场 193 号副本卡了 26 秒）。失败必须让玩家看见。
			return s.sendCurrentDungeonEntryAdmissionPlanFailure(session, runtime, err)
		}
		dungeonEntryItemUpdates = dungeonEntryInventoryBatch.updatesFor(session)
	}
	if prepared == nil && len(preparedFollowers) > 0 {
		if err := s.commitRuntimePartyPreparedFollowers(preparedFollowers); err != nil {
			if restoreErr := s.restoreCurrentDungeonEntryInventoryBatch(dungeonEntryInventoryBatch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			if restoreErr := s.restoreCurrentDungeonAltarAdmission(altarAdmission); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(dungeonAdmissionCounterBatch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			s.logGameEvent(session, "game-party-dungeon-entry-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"difficulty", request.Difficulty,
				"reason", "party_follower_runtime_commit_failed_before_leader_commit",
				"error", err)
			return s.sendSelectDungeonFailure(session, currentSelectDungeonPreparationRejectedReason)
		}
	}
	if prepared == nil || !prepared.committed {
		if err := s.commitDungeonRuntimeWithPartyAdmission(
			session,
			runtime,
			preparedFollowers,
			dungeonEntryInventoryBatch,
			dungeonAdmissionCounterBatch,
		); err != nil {
			if restoreErr := s.restoreCurrentDungeonAltarAdmission(altarAdmission); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			s.logGameEvent(session, "game-dungeon-select-blocked",
				"dungeon_id", request.DungeonID,
				"maze_index", runtime.MazeIndex,
				"reason", "dungeon_runtime_commit_failed",
				"error", err)
			return s.sendSelectDungeonFailure(session, currentSelectDungeonPreparationRejectedReason)
		}
	}
	if runtime.partyDungeonOwner != nil {
		if err := s.emitRuntimePartyDungeonStart(
			session,
			runtime,
			scene,
			body,
			passiveDropPlan,
			dungeonEntryItemUpdates,
			prepared == nil,
			retryFromSettlement,
		); err != nil {
			// The client answers a failed entry with nothing at all, so this
			// return cannot stay silent: everything before it succeeded and the
			// selected dungeon is already committed.
			s.logGameEvent(session, "game-party-dungeon-entry-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"party_id", runtime.partyDungeonOwner.partyID,
				"maze_index", runtime.MazeIndex,
				"reason", "shared_party_dungeon_start_room_emit_failed_after_commit",
				"error", err)
			return err
		}
		if fanoutParty {
			if err := s.enterRuntimePartyFollowers(session, body, preparedFollowers, partyStateAtRequest, retryFromSettlement); err != nil {
				s.logGameEvent(session, "game-party-dungeon-entry-blocked",
					"char_id", session.selectedCharacterID,
					"dungeon_id", runtime.Dungeon.ID,
					"party_id", runtime.partyDungeonOwner.partyID,
					"maze_index", runtime.MazeIndex,
					"reason", "shared_party_dungeon_follower_enter_failed_after_leader_commit",
					"error", err)
				return err
			}
		}
		s.leaveTownPresenceForDungeon(session)
		return nil
	}
	resetDungeonEntrySceneGates(session)
	if err := s.sendCurrentSceneOp9PreviewActorRemovalOnce(session, "select_dungeon_after_runtime_commit_before_op27"); err != nil {
		s.logGameEvent(session, "game-dungeon-select-blocked",
			"dungeon_id", request.DungeonID,
			"maze_index", runtime.MazeIndex,
			"reason", "select_preview_actor_removal_failed_after_runtime_commit",
			"error", err)
		return err
	}

	if prepared == nil {
		selectAckBody := buildSelectDungeonBodyForCharacter(session.selectedCharacterID)
		s.logGameEvent(session, "game-dungeon-select-ack-send",
			"msg_id", uint16(dnfenum.CmdPacketSelectDungeon),
			"body_len", len(selectAckBody)+1,
			"compatibility_mode", selectAckBody[0],
			"compatibility_object_key", binary.LittleEndian.Uint16(selectAckBody[1:3]),
			"request_body_len", len(body),
			"source", "current_exe_sub_1CFC410_generic_success_byte_trailing_body_ignored",
			"reason", "stop_rejected_op16_request_echo")
		if err := s.sendGameUpperSuccess(
			session,
			uint16(dnfenum.CmdPacketSelectDungeon),
			selectAckBody,
		); err != nil {
			return err
		}
	} else {
		s.logGameEvent(session, "game-party-dungeon-select-follower-ack-suppressed",
			"msg_id", uint16(dnfenum.CmdPacketSelectDungeon),
			"request_body_len", len(body),
			"reason", "follower_did_not_submit_op16_and_receives_leader_owned_op28_op29_only")
	}
	if altarAdmission != nil {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentGoldStateBody(altarAdmission.snapshot.GoldAfter),
			0,
		); err != nil {
			return err
		}
	}
	if len(dungeonEntryItemUpdates) != 0 {
		if err := s.sendGameUpperRawClass(session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, dungeonEntryItemUpdates), 0); err != nil {
			return err
		}
	}
	if err := s.sendCurrentDungeonResourceState(session, runtime, "select_dungeon_after_op16_ack_before_op27"); err != nil {
		return err
	}
	if err := s.sendCurrentPreDungeonContextPlayerState(session, "select_dungeon_after_op5_before_op27"); err != nil {
		return err
	}
	if !session.enterSelectDungeonContextSent {
		if err := s.sendCurrentDungeonContextOp27(session, "select_dungeon_after_pre_actor_binding"); err != nil {
			return err
		}
		session.enterSelectDungeonContextSent = true
	} else {
		s.logGameEvent(session, "game-upper-current-dungeon-context-op27-duplicate-skipped",
			"source", "select_dungeon_after_pre_actor_binding",
			"char_id", session.selectedCharacterID,
			"msg_id", currentDungeonContextMsgID,
			"reason", "town_enter_select_page_already_committed_before_real_op16")
	}
	entryPackets, err := buildCurrentDungeonEntryPacketsWithPassiveDrops(runtime, scene, passiveDropPlan.ExtraEntries)
	if err != nil {
		s.logGameEvent(session, "game-upper-current-dungeon-info-op28-deferred",
			"source", "select_dungeon_after_op27",
			"char_id", session.selectedCharacterID,
			"msg_id", currentDungeonInfoNotification,
			"dungeon_id", runtime.Dungeon.ID,
			"reason", "typed_runtime_packet_plan_failed",
			"error", err)
		return nil
	}
	s.logGameEvent(session, "game-upper-current-dungeon-info-op28-send",
		"source", "select_dungeon_after_op27",
		"char_id", session.selectedCharacterID,
		"msg_id", currentDungeonInfoNotification,
		"classification", 0,
		"body_len", len(entryPackets.DungeonInfo),
		"dungeon_id", runtime.Dungeon.ID,
		"difficulty", runtime.Request.Difficulty,
		"entry_option", runtime.Request.EntryOption,
		"maze_index", runtime.MazeIndex,
		"boss", runtime.BossCoordinate.String(),
		"body_source", "current_exe_sub_1D440C0_real_pvf_runtime",
		"start_map", "deferred_until_op28_runtime_acceptance")
	if err := s.sendGameUpperRawClass(session, currentDungeonInfoNotification, entryPackets.DungeonInfo, 0); err != nil {
		return err
	}
	if err := s.sendCurrentTowerOfDespairOpponent(session, runtime); err != nil {
		return fmt.Errorf("initialize despair tower opponent before op29: %w", err)
	}
	if entryPackets.RandomizedCreatureCount != 0 {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketVerifyCreatureQuest),
			entryPackets.RandomizedCreatureQueue,
			0,
		); err != nil {
			return err
		}
		s.logGameEvent(session, "game-dungeon-randomized-neutral-creature-queued",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"creature_count", entryPackets.RandomizedCreatureCount,
			"packet_order", "op28-selection-op479-explicit-creature-op29-room")
	}
	if isCurrentForgottenLandDungeon(runtime) {
		// The current EXE initializes the Lost Land countdown while processing
		// START_MAP, so its two-u32 PVF limit projection must precede op29.
		if err := s.sendCurrentForgottenLandTimeProjection(session, runtime); err != nil {
			return fmt.Errorf("send forgotten land time projection before op29: %w", err)
		}
		if err := s.armCurrentForgottenLandTimer(session, runtime); err != nil {
			return fmt.Errorf("arm forgotten land gameplay timer before op29: %w", err)
		}
	}
	roomSnapshot := runtime.Room.Snapshot()
	s.logGameEvent(session, "game-upper-current-dungeon-start-op29-send",
		"source", "select_dungeon_after_op28",
		"char_id", session.selectedCharacterID,
		"msg_id", currentDungeonStartMapNotificationForRuntime(runtime),
		"classification", 0,
		"body_len", len(entryPackets.StartMap),
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"seed", runtime.Seed,
		"actor_count", len(roomSnapshot.Monsters)+len(roomSnapshot.ExtendedActors),
		"body_source", "current_exe_sub_1D479A0_real_pvf_runtime")
	if err := s.sendGameUpperRawClass(session, currentDungeonStartMapNotificationForRuntime(runtime), entryPackets.StartMap, 0); err != nil {
		session.dungeon.mu.Lock()
		if session.dungeon.runtime == runtime {
			s.cancelCurrentForgottenLandTimerLocked(session, runtime, "start_map_socket_write_failed")
		}
		session.dungeon.mu.Unlock()
		return err
	}
	if err := s.publishCurrentDungeonTrapEventMonsterAfterStartMap(
		session,
		runtime,
		scene,
		runtime.Room,
		"select_dungeon_after_op29",
	); err != nil {
		return fmt.Errorf("publish current trap event-monster condition after op29: %w", err)
	}
	s.runCurrentDungeonActionObjectsLocked(session, runtime, scene, runtimeDungeonActionObjectRoomEntered, "select_dungeon_after_op29")
	if err := s.sendCurrentDungeonEpicWishSnapshot(
		session,
		runtime,
		"select_dungeon_after_op29",
	); err != nil {
		return fmt.Errorf("send current Time Rift epic-wish snapshot after op29: %w", err)
	}
	if err := s.sendCurrentTournamentEntryProjection(session, runtime, scene); err != nil {
		return fmt.Errorf("send current tournament entry projection after op29: %w", err)
	}
	announcedActors, err := runtime.Room.AnnounceAllActors(runtime.Session)
	if err != nil {
		return fmt.Errorf("commit current start-map runtime actors after op29 send: %w", err)
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		return fmt.Errorf("cache current start-map runtime room after actor announce: %w", err)
	}
	if err := commitCurrentDungeonPassiveObjectDrops(runtime, scene, passiveDropPlan); err != nil {
		return fmt.Errorf("commit current start-map passive-object drops after op29 send: %w", err)
	}
	if err := s.consumeCurrentDungeonRoomFatigueAfterCommit(session, runtime, "select_dungeon_after_op29_commit"); err != nil {
		return fmt.Errorf("consume current start-map fatigue: %w", err)
	}
	s.logGameEvent(session, "game-upper-current-dungeon-start-op29-committed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"announced_actor_count", announcedActors,
		"passive_object_item_rows", passiveDropPlan.ConfiguredRowCount,
		"passive_object_drop_count", len(passiveDropPlan.ExtraEntries))
	if err := s.sendCurrentPostStartMapPlayerPlacement(session, runtime, scene, "select_dungeon_after_op29_commit"); err != nil {
		return fmt.Errorf("send current post-start-map player state: %w", err)
	}
	// Carry the weakness into the dungeon. The condition is published once per
	// login by subtype0, and entering a dungeon rebuilds the local player, so
	// without this the health cap was dropped at the door and the penalty only
	// applied in town. Sent after the placement above, for the same reason the
	// town route sends it after its rebuild. 修炼场 and PVP are exempt: a
	// whitelisted room neither applies nor carries the penalty. This used to
	// guard on currentDungeonRuntimeIsPractice, which is the op16 protocol mode
	// and never true for the training room's own direct-entry button - see
	// currentWeaknessExemptRoom.
	if _, exempt := currentWeaknessExemptRoom(session, runtime); !exempt {
		if err := s.sendCurrentWeaknessConditionRefresh(session, "select_dungeon_after_op29_commit"); err != nil {
			return fmt.Errorf("send current weakness condition after op29: %w", err)
		}
	}
	if err := s.armCurrentCrystalContractTimer(session, runtime); err != nil {
		// Crystal Contract is optional premium state. A repository or scheduler
		// outage must not strand an otherwise valid dungeon entry after op29.
		s.logGameEvent(session, "game-crystal-contract-entry-timer-deferred",
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"error", err)
	}

	s.logGameEvent(session, "game-dungeon-runtime-prepared",
		"dungeon_id", request.DungeonID,
		"difficulty", request.Difficulty,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"map_path", scene.Map.Map.Path,
		"monsters", len(scene.Monsters),
		"hostiles", len(scene.ExpectedHostiles),
		"blocking_hostiles", len(scene.BlockingHostiles),
		"npcs", len(scene.NPCs),
		"passive_objects", len(scene.PassiveObjects)+len(scene.SpecialPassiveObjects),
		"summons", len(scene.Summons),
		"runtime_monsters", runtime.Room.MonsterCount(),
		"opaque_hostiles", runtime.Room.OpaqueHostileCount(),
		"tutorial_completed_reentry", runtime.tutorialCompletedReentry,
		"entry_sequence", "requester_class1_op16_ack_or_party_follower_no_ack_then_class0_op5_then_final_owner_mode0_actor_binding_mode1_then_op27_page_enter_runtime_recompute_gate_then_op28_optional_lost_land_op603_and_gameplay_timer_then_op29_optional_trap_event_op312_then_full_equipment_mode1_mode3_optional_pvf_tutorial_op3_user_state_op359_op356_op124_op9_op120_then_optional_crystal_contract_timer",
		"deferred", "real_hud_and_remaining_scene_objects")
	if fanoutParty {
		if err := s.enterRuntimePartyFollowers(session, body, preparedFollowers, partyStateAtRequest, retryFromSettlement); err != nil {
			return err
		}
	}
	s.leaveTownPresenceForDungeon(session)
	return nil
}

func (s *Service) commitDungeonRuntimeWithPartyAdmission(
	session *gameSession,
	runtime *runtimeDungeonState,
	preparedFollowers []*runtimePartyPreparedDungeonEntry,
	inventory *currentDungeonEntryInventoryBatch,
	counterBatches ...*currentDungeonAdmissionCounterBatch,
) error {
	var counters *currentDungeonAdmissionCounterBatch
	if len(counterBatches) > 0 {
		counters = counterBatches[0]
	}
	err := s.commitDungeonRuntime(session, runtime)
	if err == nil {
		return nil
	}
	s.rollbackRuntimePartyPreparedFollowers(preparedFollowers)
	if restoreErr := s.restoreCurrentDungeonEntryInventoryBatch(inventory); restoreErr != nil {
		err = errors.Join(err, restoreErr)
	}
	if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(counters); restoreErr != nil {
		err = errors.Join(err, restoreErr)
	}
	return err
}

func selectDungeonFailureReason(err error) (byte, bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, dnfdungeon.ErrEntryLevelTooLow):
		return currentSelectDungeonEntryLevelTooLowReason, true
	case errors.Is(err, dnfdungeon.ErrEntryPartyLimit):
		// The current client has no dedicated party-limit reason in this flow.
		// Reason 3 is the verified retryable selector rejection and is also used
		// for the solo-only Blood/Ultimate Altar implementation boundary.
		return currentSelectDungeonPreparationRejectedReason, true
	case errors.Is(err, errDungeonFatigueExhausted):
		return currentSelectDungeonFatigueExhaustedReason, true
	case errors.Is(err, errDungeonAlreadyActive):
		// A different op16 arriving after the runtime is active is a stale client
		// request, not a selector preparation failure. Replying would reopen or
		// mutate UI state while the character is already inside the dungeon.
		return 0, false
	case errors.Is(err, errAntonRaidEntryBlocked):
		return currentSelectDungeonRaidNotReadyReason, true
	case errors.Is(err, dnfhellparty.ErrRoomUnavailable), errors.Is(err, dnfhellparty.ErrRoomInvalid):
		// The current client understands this established retryable op16 rejection
		// shape and returns to the selector instead of waiting for a start-map packet.
		return currentSelectDungeonPreparationRejectedReason, true
	default:
		// Once the current client has sent SELECT_DUNGEON it waits for either
		// the normal entry stream or an op16 rejection. Returning silently on
		// an unknown PVF/map/runtime preparation error leaves the selector in
		// an infinite loading state. Reason 3 is the proven retryable rejection
		// that returns this EXE to the dungeon selector.
		return currentSelectDungeonPreparationRejectedReason, true
	}
}

func selectDungeonFatiguePromptReason(
	session *gameSession,
	state alignedcmd.PartyState,
) byte {
	if session != nil && state.PartyID > 0 &&
		state.UserID == session.selectedCharacterID &&
		len(runtimePartyMembers(state)) > 1 {
		return currentSelectDungeonPartyFatigueExhaustedReason
	}
	return currentSelectDungeonFatigueExhaustedReason
}

func (s *Service) prepareRuntimePartyDungeonEntry(
	ctx context.Context,
	leader *gameSession,
	state alignedcmd.PartyState,
	request dungeoncmd.SelectDungeonRequest,
	leaderRuntime *runtimeDungeonState,
	leaderScene worldmap.DungeonRoomScene,
	retryFromSettlement bool,
	inRunTransfer bool,
) ([]*runtimePartyPreparedDungeonEntry, error) {
	if leader == nil || state.PartyID <= 0 || state.UserID != leader.selectedCharacterID {
		return nil, nil
	}
	members := runtimePartyMembers(state)
	if len(members) <= 1 {
		return nil, nil
	}
	plan := runtimePartyDungeonEntryPlan{
		dungeonID: leaderRuntime.Dungeon.ID,
		mazeIndex: leaderRuntime.MazeIndex,
		room:      leaderScene.Coordinate,
		mapID:     leaderScene.Map.Map.ID,
		boss:      leaderRuntime.BossCoordinate,
		seed:      leaderRuntime.Seed,
		maps:      make(map[worldmap.RoomCoordinate]int64),
	}
	for _, room := range leaderRuntime.Session.Rooms() {
		if room.Map != nil {
			plan.maps[room.Coordinate] = room.Map.Map.ID
		}
	}
	prepared := make([]*runtimePartyPreparedDungeonEntry, 0, len(members)-1)
	for _, member := range members {
		if member.UserID == 0 || member.UserID == leader.selectedCharacterID {
			continue
		}
		follower, ok := s.onlineGameSession(member.UserID)
		if !ok || follower == leader {
			return nil, fmt.Errorf("party member %d has no resident game session", member.UserID)
		}
		// Resolved after the session lookup on purpose: the co-presence test
		// needs the follower's own session because opening the dungeon-selection
		// page retires town presence, so the live roster is empty exactly here.
		//
		// The one request that cannot satisfy it is the in-run transfer: entering
		// the gate removed every squad member from the town roster by design
		// (leaveTownPresenceForDungeon), so requiring a shared town anchor would
		// refuse the warp that is trying to keep the squad together. The relaxation
		// is scoped to that caller and reported, never silent.
		if coPresent, presenceReason := s.currentPartyEntryCoPresence(leader, follower); !coPresent {
			if inRunTransfer {
				s.logGameEvent(leader, "game-raid-in-run-transfer-copresence-relaxed",
					"party_id", state.PartyID,
					"leader_char_id", leader.selectedCharacterID,
					"char_id", member.UserID,
					"dungeon_id", request.DungeonID,
					"town_co_presence_reason", presenceReason,
					"reason", "the_squad_is_inside_the_raid_so_the_town_anchor_is_gone_by_design")
			} else {
				// Name the side that failed to resolve, not only the member being
				// judged: the refusal below is answered by the client with silence,
				// so this line is the whole diagnosis (see currentPartyEntryCoPresence).
				leaderPresence, leaderResolved := s.currentTownActorAreaDetachPresence(leader)
				followerPresence, followerResolved := s.currentTownActorAreaDetachPresence(follower)
				s.logGameEvent(leader, "game-party-dungeon-entry-copresence-refused",
					"party_id", state.PartyID,
					"leader_char_id", leader.selectedCharacterID,
					"char_id", member.UserID,
					"channel_id", leader.channel.ID,
					"member_channel_id", follower.channel.ID,
					"leader_anchor_resolved", leaderResolved,
					"leader_town_id", leaderPresence.TownID,
					"leader_area_id", leaderPresence.AreaID,
					"leader_instance_key", leaderPresence.InstanceKey,
					"member_anchor_resolved", followerResolved,
					"member_town_id", followerPresence.TownID,
					"member_area_id", followerPresence.AreaID,
					"member_instance_key", followerPresence.InstanceKey,
					"reason", presenceReason,
					"client_visible_behaviour", "op16_retryable_rejection_reason_3_without_any_notice")
				// The refusal is answered by the client with silence, so it is also
				// recorded as the raid's own entry_blocked row: this is the one
				// server-side event that explains a party that cannot re-enter a
				// gate after a return.
				s.recordRaidEvent(leader, "entry_blocked", map[string]any{
					"party_id":               state.PartyID,
					"dungeon_id":             request.DungeonID,
					"actor_char_id":          leader.selectedCharacterID,
					"blocked_member_id":      member.UserID,
					"channel_id":             leader.channel.ID,
					"member_channel_id":      follower.channel.ID,
					"leader_anchor_resolved": leaderResolved,
					"member_anchor_resolved": followerResolved,
					"leader_town_id":         leaderPresence.TownID,
					"leader_area_id":         leaderPresence.AreaID,
					"member_town_id":         followerPresence.TownID,
					"member_area_id":         followerPresence.AreaID,
					"result":                 "refused",
					"reason":                 presenceReason,
					"client_visible":         "op16_retryable_rejection_reason_3_without_any_notice",
				})
				return nil, fmt.Errorf("party member %d is not present in the leader town area: %s", member.UserID, presenceReason)
			}
		}
		followerState := runtimePartyStateSnapshot(follower)
		if followerState.PartyID != state.PartyID || followerState.UserID != state.UserID {
			return nil, fmt.Errorf("party member %d has stale party state", member.UserID)
		}
		if ready, reason := s.currentTownEnterSelectReady(follower); !ready {
			// The one relaxation, scoped to 再次挑战 and to one member state: the member
			// is still on its own completed run's settlement page, because only the
			// leader's client offers 再次挑战 and the member is driven by this request.
			// Every other "not ready" state keeps the old refusal, so the 选择其他地下城
			// route (where each client acts for itself) and every area/channel refusal
			// are untouched.
			//
			// The in-run transfer is the second, equally narrow case: the member is
			// standing in a live run of the gate rather than on a selector page, so
			// there is no page to be ready. It is reported the same way.
			switch {
			case inRunTransfer:
				s.logGameEvent(follower, "game-raid-in-run-transfer-selector-page-relaxed",
					"party_id", state.PartyID,
					"leader_char_id", leader.selectedCharacterID,
					"char_id", member.UserID,
					"dungeon_id", request.DungeonID,
					"member_selector_reason", reason,
					"reason", "the_member_is_inside_the_gate_not_on_a_selector_page")
			case retryFromSettlement && s.settlementRetryDrivesMemberFromItsCompletedRun(follower, member.UserID, request):
				s.logGameEvent(leader, "game-party-dungeon-retry-member-driven-by-server",
					"party_id", state.PartyID,
					"leader_char_id", leader.selectedCharacterID,
					"char_id", member.UserID,
					"dungeon_id", request.DungeonID,
					"member_selector_reason", reason,
					"reason", "only_the_leader_owns_retry_so_the_team_is_driven_by_this_request")
			default:
				return nil, fmt.Errorf("party member %d selector is not ready: %s", member.UserID, reason)
			}
		}
		followerRuntime, followerScene, err := s.prepareDungeonRuntimePlanned(ctx, follower, request, &plan)
		if err != nil {
			return nil, fmt.Errorf("prepare party member %d for dungeon %d: %w", member.UserID, request.DungeonID, err)
		}
		memberIndex, memberIndexed := runtimePartyMemberIndex(member.UserID, state)
		followerRuntime.partyMemberIndex, followerRuntime.partyMemberIndexed = memberIndex, memberIndexed
		if err := applyRuntimePartyDungeonEntryPlan(followerRuntime, followerScene, plan); err != nil {
			return nil, fmt.Errorf("apply leader dungeon selection to party member %d: %w", member.UserID, err)
		}
		if err := freezeCurrentDungeonTownReturnOrigin(follower, followerRuntime); err != nil {
			return nil, fmt.Errorf("freeze party member %d town return origin: %w", member.UserID, err)
		}
		if _, err := buildCurrentDungeonEntryPackets(followerRuntime, followerScene); err != nil {
			return nil, fmt.Errorf("build party member %d dungeon entry packets: %w", member.UserID, err)
		}
		prepared = append(prepared, &runtimePartyPreparedDungeonEntry{
			session: follower,
			runtime: followerRuntime,
			scene:   followerScene,
			plan:    plan,
		})
		s.logGameEvent(follower, "game-party-dungeon-entry-member-prepared",
			"party_id", state.PartyID,
			"leader_char_id", leader.selectedCharacterID,
			"char_id", member.UserID,
			"dungeon_id", followerRuntime.Dungeon.ID,
			"maze_index", followerRuntime.MazeIndex,
			"room", followerScene.Coordinate.String(),
			"map_id", followerScene.Map.Map.ID,
			"seed", followerRuntime.Seed)
	}
	// The selector page is only ever prepared for a member that is about to walk
	// through one. A squadmate being carried inside a raid is standing in a live run,
	// so there is no page to open and prepareRuntimePartyFollowerEnterSelect would
	// only report "selector is not ready" for a member that is exactly where it
	// should be. Its op5/op27/op28/op29 stream is driven by the entry itself, the
	// same way a 再次挑战 member with no page is driven.
	if inRunTransfer {
		return prepared, nil
	}
	// 并发预检。这里原本是「逐个成员投到它的 event loop 再同步等」：7 个队友
	// 串行累加，就是队长点「进入副本」之后在选择副本界面卡住的那几秒（现场
	// 2026-09-26 18:09，op16 SELECT_DUNGEON 1770ms / 1209ms，紧接着
	// op666 START_RAID 4218ms）。与 enterRuntimePartyFollowers 及另外四处扇出
	// 保持同一套做法：总耗时从 Σ 变成 max，整轮共用一个总预算。
	//
	// 就绪门仍然按 prepared 的原顺序逐个判定，且第一个不合格就整体中止 —— 报错
	// 文案与顺序都不变；区别只在于判定那一刻，每个成员都已经各自跑过预检。
	readyByIndex := make([]bool, len(prepared))
	indexBySession := make(map[*gameSession]int, len(prepared))
	targets := make([]sessionFanoutTarget, 0, len(prepared))
	for index, member := range prepared {
		indexBySession[member.session] = index
		targets = append(targets, sessionFanoutTarget{
			label:       fmt.Sprintf("%d", member.session.selectedCharacterID),
			characterID: member.session.selectedCharacterID,
			session:     member.session,
		})
	}
	if err := s.fanOutToGameSessions(ctx, targets, "runtime-party-dungeon-selector-preflight",
		func(target sessionFanoutTarget) error {
			index, known := indexBySession[target.session]
			if !known {
				return errors.New("party selector preflight target lost its prepared state")
			}
			var prepareErr error
			readyByIndex[index], prepareErr = s.prepareRuntimePartyFollowerEnterSelect(
				target.session,
				leader.selectedCharacterID,
				state.PartyID,
				"leader_op16_party_follower_preflight",
			)
			return prepareErr
		}); err != nil {
		return nil, fmt.Errorf("prepare party selector preflight: %w", err)
	}
	for index, member := range prepared {
		ready := readyByIndex[index]
		if !ready {
			// The second gate with the same condition as the readiness check above, and
			// the same scoped relaxation: on 再次挑战 a member that is still on its own
			// completed run's settlement page has no selector page to prepare, because
			// only the leader's client can start the retry. The fan-out below drives it
			// regardless (commitRuntimePartyPreparedFollowers +
			// enterRuntimePartyFollowers), which is 86JP's PARTY_LOADING_GATE shape:
			// push the entry to the member and wait for its FINISH_LOADING.
			if !retryFromSettlement ||
				!s.settlementRetryDrivesMemberFromItsCompletedRun(member.session, member.session.selectedCharacterID, request) {
				return nil, fmt.Errorf("party member %d selector is not ready", member.session.selectedCharacterID)
			}
			s.logGameEvent(member.session, "game-party-dungeon-retry-member-page-skipped",
				"party_id", state.PartyID,
				"leader_char_id", leader.selectedCharacterID,
				"char_id", member.session.selectedCharacterID,
				"dungeon_id", request.DungeonID,
				"reason", "the_member_is_on_its_own_completed_run_and_only_the_leader_owns_retry")
		}
	}
	return prepared, nil
}

func applyRuntimePartyDungeonEntryPlan(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	plan runtimePartyDungeonEntryPlan,
) error {
	if runtime == nil || runtime.Session == nil {
		return errors.New("party member runtime is unavailable")
	}
	if runtime.Dungeon.ID != plan.dungeonID || runtime.MazeIndex != plan.mazeIndex ||
		scene.Coordinate.X != plan.room.X || scene.Coordinate.Y != plan.room.Y ||
		scene.Map.Map.ID != plan.mapID ||
		runtime.BossCoordinate.X != plan.boss.X || runtime.BossCoordinate.Y != plan.boss.Y {
		return fmt.Errorf(
			"topology mismatch got dungeon=%d maze=%d room=%s map=%d boss=%s want dungeon=%d maze=%d room=%s map=%d boss=%s",
			runtime.Dungeon.ID, runtime.MazeIndex, scene.Coordinate.String(), scene.Map.Map.ID, runtime.BossCoordinate.String(),
			plan.dungeonID, plan.mazeIndex, plan.room.String(), plan.mapID, plan.boss.String(),
		)
	}
	resolved := runtime.Session.Rooms()
	if len(resolved) != len(plan.maps) {
		return fmt.Errorf("resolved room count mismatch got=%d want=%d", len(resolved), len(plan.maps))
	}
	for _, room := range resolved {
		wantMapID, ok := plan.maps[room.Coordinate]
		if !ok || room.Map == nil || room.Map.Map.ID != wantMapID {
			gotMapID := int64(0)
			if room.Map != nil {
				gotMapID = room.Map.Map.ID
			}
			return fmt.Errorf("room %s map mismatch got=%d want=%d known=%t", room.Coordinate, gotMapID, wantMapID, ok)
		}
	}
	runtime.Seed = plan.seed
	if visit := runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)]; visit != nil {
		visit.Seed = plan.seed
		visit.DropRNG = plan.seed
	}
	return nil
}

func (s *Service) commitRuntimePartyPreparedFollowers(prepared []*runtimePartyPreparedDungeonEntry) error {
	committed := make([]*runtimePartyPreparedDungeonEntry, 0, len(prepared))
	for _, member := range prepared {
		if member == nil || member.session == nil || member.runtime == nil {
			s.rollbackRuntimePartyPreparedFollowers(committed)
			return errors.New("prepared party member runtime is incomplete")
		}
		if err := s.commitDungeonRuntime(member.session, member.runtime); err != nil {
			s.rollbackRuntimePartyPreparedFollowers(committed)
			return fmt.Errorf("commit party member %d runtime: %w", member.session.selectedCharacterID, err)
		}
		member.committed = true
		committed = append(committed, member)
	}
	return nil
}

func (s *Service) rollbackRuntimePartyPreparedFollowers(prepared []*runtimePartyPreparedDungeonEntry) {
	for _, member := range prepared {
		if member == nil || member.session == nil || member.runtime == nil || !member.committed {
			continue
		}
		member.session.dungeon.mu.Lock()
		if member.session.dungeon.runtime == member.runtime {
			if err := s.notifyRuntimeRaidDungeonAbandonedLocked(member.session, member.runtime); err != nil {
				s.logGameEvent(member.session, "game-party-runtime-rollback-raid-abandon-publish-failed", "error", err)
			}
			s.retireCurrentDungeonRuntimeLocked(member.session, member.runtime, "party_runtime_rollback")
		}
		member.session.dungeon.mu.Unlock()
		member.committed = false
	}
}

func runtimePartyMemberIndexForSession(session *gameSession) (byte, bool) {
	if session == nil || session.selectedCharacterID == 0 {
		return 0, false
	}
	return runtimePartyMemberIndex(session.selectedCharacterID, runtimePartyStateSnapshot(session))
}

func runtimePartyMemberIndex(characterID uint16, state alignedcmd.PartyState) (byte, bool) {
	members := runtimePartyMembers(state)
	if len(members) <= 1 {
		return 0, false
	}
	for _, member := range members {
		if member.UserID == characterID && member.Slot < 8 {
			return member.Slot, true
		}
	}
	return 0, false
}

func currentDungeonRuntimePartyMemberIndex(runtime *runtimeDungeonState) byte {
	if runtime == nil || runtime.partyDungeonOwner != nil || !runtime.partyMemberIndexed {
		return 0xff
	}
	return runtime.partyMemberIndex
}

func (s *Service) enterRuntimePartyFollowers(
	leader *gameSession,
	body []byte,
	prepared []*runtimePartyPreparedDungeonEntry,
	state alignedcmd.PartyState,
	retryFromSettlement bool,
) error {
	if leader == nil || leader.selectedCharacterID == 0 {
		return nil
	}
	if state.PartyID <= 0 || state.UserID != leader.selectedCharacterID {
		return nil
	}
	if len(prepared) == 0 {
		return nil
	}
	// 校验阶段仍然整体前置：任何一个成员不合法，就在动手之前中止，避免
	// 只进了一半。校验通过后再并发执行。
	targets := make([]sessionFanoutTarget, 0, len(prepared))
	bySession := make(map[*gameSession]*runtimePartyPreparedDungeonEntry, len(prepared))
	for _, member := range prepared {
		if member == nil || member.session == nil || member.runtime == nil || !member.committed {
			return errors.New("party follower entry was not prepared and committed")
		}
		follower := member.session
		followerParty := runtimePartyStateSnapshot(follower)
		if followerParty.PartyID != state.PartyID || followerParty.UserID != state.UserID {
			return fmt.Errorf("party member %d changed party state during dungeon entry", follower.selectedCharacterID)
		}
		targets = append(targets, sessionFanoutTarget{
			label:       fmt.Sprintf("%d", follower.selectedCharacterID),
			characterID: follower.selectedCharacterID,
			session:     follower,
		})
		bySession[follower] = member
	}
	// 并发扇出。这里原本是「逐个成员现开一个 createWriteTimeout 再同步等」：
	// 8 人团最坏就是 3s x 7 = 21s 的串行累加，而那整段时间压在队长自己的
	// event loop 上 —— 进图是攻坚里最重的一步，队长一卡，全队跟着等。
	// 与 runtime-party-enter-select、party-dungeon-loading-release、
	// runtime-party-dungeon-room-transition、runtime-party-selector-return
	// 四处保持同一套做法：总耗时从 Σ 变成 max，整轮共用一个总预算。
	return s.fanOutToGameSessions(context.Background(), targets, "runtime-party-dungeon-entry",
		func(target sessionFanoutTarget) error {
			member := bySession[target.session]
			if member == nil {
				return errors.New("party follower entry target lost its prepared state")
			}
			return s.handleDungeonSelectUpperPlanned(target.session, append([]byte(nil), body...), member, false, retryFromSettlement)
		})
}

func (s *Service) leaveTownPresenceForDungeon(session *gameSession) {
	if s == nil || s.onlinePlayers == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	// Capture the authoritative party before removing the town presence. A
	// shared dungeon admission deliberately reuses the native party actors that
	// already exist in town. Current NoPack class0/op6 is not merely a visual
	// town leave: it also clears the matching party-member slot. If the follower
	// leaves the directory first and that ordinary op6 reaches the still-loading
	// leader, only the leader loses the follower and enters with a one-member
	// room while the follower keeps both actors. Preserve members of the same
	// admitted party; ordinary town observers must still retire the actor.
	partyState := runtimePartyStateSnapshot(session)
	// Entering the dungeon consumes a selector retirement rather than undoing
	// it: the character is supposed to stay out of the town roster, and the
	// memo must not survive to be replayed by a later selector return.
	clearTownSelectorRetiredPresence(session)
	departing, hadPresence := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	_, others, removed := s.onlinePlayers.LeaveAreaSession(session.selectedCharacterID, session)
	if removed && len(others) > 0 {
		s.broadcastTownPlayerDungeonTransition(session.selectedCharacterID, others, partyState)
	}
	if removed && hadPresence {
		// A dungeon transition keeps the character online but changes the friend
		// list's three-state channel marker from town-online to busy.  Do not
		// create a friend scene actor here; ordinary op6 already owns visual
		// retirement for same-area observers.
		s.notifyCurrentUnitedFriendPresence(
			departing.Name,
			currentFriendPresence{ChannelID: uint16(session.channel.ID), Busy: true},
			"dungeon_entry_friend_busy",
		)
	}
}

// broadcastTownPlayerDungeonTransition is the online-player-manager analogue
// of the same-version reference's leaveForPartyDungeon event. Party peers keep
// the established actor/slot through op2/op11/op800/op29; non-party observers
// receive the normal op6 retirement.
func (s *Service) broadcastTownPlayerDungeonTransition(
	characterID uint16,
	others []onlinePlayerInfo,
	state alignedcmd.PartyState,
) {
	if s == nil || characterID == 0 || len(others) == 0 {
		return
	}
	partyMembers := make(map[uint16]struct{}, len(runtimePartyMembers(state)))
	if state.PartyID > 0 {
		for _, member := range runtimePartyMembers(state) {
			if member.UserID != 0 && member.UserID != characterID {
				partyMembers[member.UserID] = struct{}{}
			}
		}
	}
	leaveBody := buildTownUserLeaveBody(characterID)
	for i := range others {
		peer := others[i].Session
		if peer == nil {
			continue
		}
		_, listedPartyPeer := partyMembers[others[i].CharacterID]
		peerState := runtimePartyStateSnapshot(peer)
		if listedPartyPeer && peerState.PartyID == state.PartyID && state.PartyID > 0 {
			s.logGameEvent(peer, "game-town-party-peer-preserved-for-dungeon",
				"party_id", state.PartyID,
				"departing_char_id", characterID,
				"peer_char_id", others[i].CharacterID,
				"transport", "preserve-party-actor-until-dungeon-prelude")
			continue
		}
		_ = s.sendCurrentSceneFixedClass0Packet(
			peer,
			currentTownUserLeaveMsgID,
			leaveBody,
			"town_copresence_party_dungeon_transition_nonparty_0x0006",
		)
	}
}

func (s *Service) replayDungeonSelectAckForActiveRuntime(
	session *gameSession,
	request dungeoncmd.SelectDungeonRequest,
	requestBodyLen int,
) (bool, error) {
	if session == nil {
		return false, nil
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil {
		session.dungeon.mu.Unlock()
		return false, nil
	}
	activeRequest := runtime.Request
	snapshot := runtime.Session.Snapshot().Run
	session.dungeon.mu.Unlock()

	if snapshot.Status != worldmap.DungeonRunActive ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) ||
		!sameSelectDungeonRequest(activeRequest, request) {
		return false, nil
	}
	if isCurrentDirectDungeonEntryRequest(request) {
		s.logGameEvent(session, "game-dungeon-direct-entry-duplicate-suppressed",
			"char_id", session.selectedCharacterID,
			"dungeon_id", snapshot.DungeonID,
			"maze_index", snapshot.MazeIndex,
			"room", snapshot.Current.String(),
			"request_body_len", requestBodyLen,
			"reason", "direct_dungeon_entry_has_no_op16_ack_replay")
		return true, nil
	}

	selectAckBody := buildSelectDungeonBodyForCharacter(session.selectedCharacterID)
	s.logGameEvent(session, "game-dungeon-select-duplicate-ack-send",
		"msg_id", uint16(dnfenum.CmdPacketSelectDungeon),
		"body_len", len(selectAckBody)+1,
		"mode", selectAckBody[0],
		"reader_key", binary.LittleEndian.Uint16(selectAckBody[1:3]),
		"request_body_len", requestBodyLen,
		"dungeon_id", snapshot.DungeonID,
		"maze_index", snapshot.MazeIndex,
		"room", snapshot.Current.String(),
		"replayed_packets", 0,
		"reason", "idempotent_retry_for_same_active_current_dungeon")
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketSelectDungeon),
		selectAckBody,
	); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Service) sendSelectDungeonFailure(session *gameSession, reason byte) error {
	// Current EXE op16 reads failure, reason, and a separate supplemental byte.
	return s.sendGameUpperRaw(session, uint16(dnfenum.CmdPacketSelectDungeon), buildSelectDungeonFailureBody(reason))
}

func buildSelectDungeonFailureBody(reason byte) []byte {
	return []byte{0, reason, 0}
}

func currentDungeonAdmissionCounterFailureReason(err error) byte {
	var limitErr *dnfdungeonadmission.LimitError
	if errors.As(err, &limitErr) {
		return currentSelectDungeonDailyEntryLimitReachedReason
	}
	return currentSelectDungeonPreparationRejectedReason
}

func sameSelectDungeonRequest(left, right dungeoncmd.SelectDungeonRequest) bool {
	return dnfdungeon.SameSelectRequest(left, right)
}

func (s *Service) handleDungeonMoveMap(session *gameSession, body []byte) error {
	if session != nil {
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		var owner *runtimePartyDungeonOwner
		if runtime != nil {
			owner = runtime.partyDungeonOwner
		}
		session.dungeon.mu.Unlock()
		if owner != nil && owner.leaderID != session.selectedCharacterID {
			s.logGameEvent(session, "game-party-dungeon-move-non-leader-consumed",
				"party_id", owner.partyID,
				"leader_char_id", owner.leaderID,
				"char_id", session.selectedCharacterID,
				"reason", "shared_room_transition_is_leader_owned")
			return nil
		}
		if owner != nil {
			owner.worldMu.Lock()
			sharedNextObjectKey, sharedNextErr := s.runtimePartyDungeonNextObjectKey(owner)
			if sharedNextErr != nil {
				owner.worldMu.Unlock()
				return fmt.Errorf("reserve party dungeon room object keys: %w", sharedNextErr)
			}
			before := runtimeDungeonRoomKey{}
			beforeKnown := false
			if runtime != nil && runtime.Session != nil {
				if scene, ok := runtime.Session.Scene(); ok {
					before, beforeKnown = runtimeDungeonRoomKeyFromScene(scene), true
				}
			}
			err := s.handleDungeonMoveMapPlanned(
				session,
				body,
				&runtimePartyDungeonMovePlan{firstObjectKey: sharedNextObjectKey},
				false,
			)
			after := runtimeDungeonRoomKey{}
			afterKnown := false
			afterSeed := uint32(0)
			session.dungeon.mu.Lock()
			runtime = session.dungeon.runtime
			if runtime != nil && runtime.Session != nil {
				if scene, ok := runtime.Session.Scene(); ok {
					after, afterKnown = runtimeDungeonRoomKeyFromScene(scene), true
					afterSeed = runtime.Seed
				}
			}
			session.dungeon.mu.Unlock()
			if err == nil && beforeKnown && afterKnown && before != after {
				owner.beginRoomLoading()
			}
			owner.worldMu.Unlock()
			if err != nil || !beforeKnown || !afterKnown || before == after {
				return err
			}
			// The shared transition is committed before any peer mailbox is
			// entered. Never wait for another member while holding worldMu: a
			// simultaneous op39 in that mailbox may need the same lock.
			return s.moveRuntimePartyFollowers(
				session,
				owner,
				body,
				runtimePartyDungeonMovePlan{
					target: after, targetSet: true, seed: afterSeed,
					firstObjectKey: sharedNextObjectKey,
				},
			)
		}
	}
	return s.handleDungeonMoveMapPlanned(session, body, nil, true)
}

type runtimePartyDungeonMovePlan struct {
	target         runtimeDungeonRoomKey
	targetSet      bool
	seed           uint32
	firstObjectKey uint32
}

func (s *Service) handleDungeonMoveMapPlanned(
	session *gameSession,
	body []byte,
	partyPlan *runtimePartyDungeonMovePlan,
	fanoutParty bool,
) error {
	if len(body) != dungeoncmd.MoveMapRequestSize {
		s.logGameEvent(session, "game-dungeon-move-blocked",
			"body_len", len(body),
			"expected_body_len", dungeoncmd.MoveMapRequestSize,
			"expected_layout", "current_exe_op45_plaintext_after_client_cipher_bypass",
			"reason", "current_exe_op45_plaintext_request_boundary_mismatch")
		return nil
	}
	request, err := dungeoncmd.DecodeMoveMapRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-move-blocked",
			"body_len", len(body),
			"reason", "current_exe_op45_request_malformed",
			"error", err)
		return nil
	}
	if session == nil {
		return nil
	}

	session.dungeon.mu.Lock()
	before := runtimeDungeonRoomKey{}
	beforeKnown := false
	if session.dungeon.runtime != nil && session.dungeon.runtime.Session != nil {
		if scene, ok := session.dungeon.runtime.Session.Scene(); ok {
			before, beforeKnown = runtimeDungeonRoomKeyFromScene(scene), true
		}
	}
	err = s.handleDungeonMoveRequestLocked(session, request, len(body), "client_op45", partyPlan)
	after := runtimeDungeonRoomKey{}
	afterKnown := false
	afterSeed := uint32(0)
	var partyOwner *runtimePartyDungeonOwner
	if session.dungeon.runtime != nil && session.dungeon.runtime.Session != nil {
		if scene, ok := session.dungeon.runtime.Session.Scene(); ok {
			after, afterKnown = runtimeDungeonRoomKeyFromScene(scene), true
			afterSeed = session.dungeon.runtime.Seed
		}
		partyOwner = session.dungeon.runtime.partyDungeonOwner
	}
	session.dungeon.mu.Unlock()
	if err != nil {
		return err
	}
	if fanoutParty && beforeKnown && afterKnown && before != after {
		if partyOwner != nil {
			partyOwner.beginRoomLoading()
		}
		if err := s.moveRuntimePartyFollowers(session, partyOwner, body, runtimePartyDungeonMovePlan{target: after, seed: afterSeed}); err != nil {
			return err
		}
	}
	return nil
}

// currentDungeonSameCoordinateResyncOwnedByLayer reports whether a
// same-coordinate CHANGE MAP at this room belongs to the explicit layer
// machinery instead of the ordinary-room resync. An active layer or a
// PVF-declared layer at the coordinate requires MoveKind=1 to advance, and the
// deliberate refusal there ("preventing a player from skipping a pending story
// layer") must stay byte-for-byte as it was: those requests are not re-answered
// with a current-room payload.
func (s *Service) currentDungeonSameCoordinateResyncOwnedByLayer(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
) bool {
	if runtime == nil {
		return true
	}
	if runtime.LayeredMapActive {
		return true
	}
	if runtime.LayerChains[scene.Coordinate] != nil {
		return true
	}
	return currentDungeonCoordinateDeclaresLayer(runtime, scene.Coordinate)
}

// resyncCurrentDungeonSameCoordinateRoom answers a CHANGE MAP request whose
// target is the room the player is already standing in. It re-sends the cached
// START_MAP for the current room so the client's room-change handshake always
// completes, and deliberately leaves every piece of dungeon runtime state
// untouched: the coordinate, seed, room owner, object-key watermark, drops,
// fatigue and layer chains are already committed for this room, and re-running
// them would double-charge fatigue and re-roll drops.
//
// This mirrors the reference server, which accepts a requested coordinate that
// is any known room of the maze and replies with START_MAP for that room. The
// alternative -- failing closed with no packet at all -- is what froze the live
// client, because a dropped CHANGE MAP leaves it waiting forever.
func (s *Service) resyncCurrentDungeonSameCoordinateRoom(
	session *gameSession,
	runtime *runtimeDungeonState,
	request dungeoncmd.MoveMapRequest,
	requestSource string,
	currentScene worldmap.DungeonRoomScene,
) error {
	visit, err := runtime.currentDungeonRoomVisit(currentScene)
	if err != nil {
		return fmt.Errorf("resync same-coordinate dungeon room: %w", err)
	}
	plan := currentDungeonMovePlan{
		Source:        currentScene,
		Target:        currentScene,
		SourceVisit:   visit,
		TargetVisit:   visit,
		TargetRoom:    visit.Room,
		NextObjectKey: runtime.NextObjectKey,
		Seed:          visit.Seed,
		Revisit:       true,
		LayerIndex:    -1,
		Operation:     currentDungeonStartMapOperationCurrent,
		PayloadMode:   currentDungeonStartMapPayloadCached,
	}
	startMapBody, err := buildCurrentDungeonMoveStartMapBody(runtime, plan)
	if err != nil {
		return fmt.Errorf("build same-coordinate dungeon room resync: %w", err)
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentDungeonStartMapNotificationForRuntime(runtime),
		startMapBody,
		0,
	); err != nil {
		return err
	}
	// The client answers this START_MAP with FINISH_LOADING, and that callback's
	// class0 state and op30 completion are room-scoped. Re-arm them or the
	// reload would stall on a second, already-consumed handshake.
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	s.logGameEvent(session, "game-dungeon-move-same-room-resync",
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", currentScene.Coordinate.String(),
		"map_id", currentScene.Map.Map.ID,
		"next_x", request.NextX,
		"next_y", request.NextY,
		"move_kind", request.MoveKind,
		"request_source", requestSource,
		"body_len", len(startMapBody),
		"operation", plan.Operation,
		"payload_mode", plan.PayloadMode,
		"reason", "current_exe_same_coordinate_change_map_is_a_known_room_of_this_maze",
		"state_mutation", "none",
		"sequence", "current_exe_op45_same_coordinate_request_to_class0_op29_start_map")
	return nil
}

func (s *Service) moveRuntimePartyFollowers(
	leader *gameSession,
	owner *runtimePartyDungeonOwner,
	body []byte,
	plan runtimePartyDungeonMovePlan,
) error {
	if leader == nil || leader.selectedCharacterID == 0 {
		return nil
	}
	if owner == nil || owner.leaderID != leader.selectedCharacterID {
		return nil
	}
	// 并发扇出：每个 follower 都在【它自己的】event loop 上执行。调用方已经先
	// 释放了 owner.worldMu（见 handleDungeonMoveRequestLocked 里
	// "Never wait for another member while holding worldMu" 那条注释），
	// 所以这里的并发不会与调用方互相等锁。
	//
	// 改前是在循环里给每人现开一个 createWriteTimeout 再同步等，把队长的
	// event loop 按成员数串行累加地卡住。
	targets := make([]sessionFanoutTarget, 0, len(owner.roster()))
	for _, member := range owner.roster() {
		if member.userID == leader.selectedCharacterID && member.generation == leader.characterGeneration {
			continue
		}
		identity, ok := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !ok || identity.session == nil || identity.session == leader || identity.generation != member.generation {
			return fmt.Errorf("party member %d has no current game session during room transition", member.userID)
		}
		targets = append(targets, sessionFanoutTarget{
			label:       fmt.Sprintf("%d", member.userID),
			characterID: member.userID,
			session:     identity.session,
		})
	}
	return s.fanOutToGameSessions(context.Background(), targets, "runtime-party-dungeon-room-transition",
		func(target sessionFanoutTarget) error {
			target.session.dungeon.mu.Lock()
			peerRuntime := target.session.dungeon.runtime
			target.session.dungeon.mu.Unlock()
			if peerRuntime == nil || peerRuntime.partyDungeonOwner != owner {
				return errors.New("party dungeon room-transition peer owner changed")
			}
			owner.worldMu.Lock()
			defer owner.worldMu.Unlock()
			return s.handleDungeonMoveMapPlanned(target.session, append([]byte(nil), body...), &plan, false)
		})
}

// handleDungeonMoveRequestLocked runs with session.dungeon.mu held. Every op45
// is handled exactly once: an uncleared-room request is rejected and never
// retained for a later unsolicited op29.
func (s *Service) handleDungeonMoveRequestLocked(
	session *gameSession,
	request dungeoncmd.MoveMapRequest,
	requestBodyLen int,
	requestSource string,
	partyPlan *runtimePartyDungeonMovePlan,
) error {
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.Room == nil {
		s.logGameEvent(session, "game-dungeon-move-blocked",
			"next_x", request.NextX,
			"next_y", request.NextY,
			"reason", "dungeon_runtime_missing")
		return nil
	}
	planningRuntime := runtime
	if partyPlan != nil && partyPlan.firstObjectKey != 0 && partyPlan.firstObjectKey != runtime.NextObjectKey {
		// Private party drops are intentionally rolled and numbered per member,
		// so their connection-local NextObjectKey values can diverge inside one
		// room. Plan the next shared room from the party-wide high-water mark
		// without mutating the live runtime during preflight. Every member then
		// receives the same monster/object keys while private rewards remain local.
		// An old-room reward can race the leader's already-committed transition;
		// in that case the old scene is being retired and the reserved shared key
		// still wins, rather than splitting the new room a second time.
		planningCopy := *runtime
		planningCopy.NextObjectKey = partyPlan.firstObjectKey
		planningRuntime = &planningCopy
	}
	plan, err := s.planCurrentDungeonMove(planningRuntime, request)
	if err != nil {
		currentScene, hasCurrentScene := runtime.Session.Scene()
		source := plan.Source.Coordinate.String()
		if hasCurrentScene {
			source = currentScene.Coordinate.String()
		}
		// The live client re-issues CHANGE MAP for the room it already occupies:
		// a same-coordinate op45 that the plain adjacency validator rejects on
		// its own (|dx|+|dy| == 0, not 1). The reference implementation accepts
		// any coordinate that is a known room of the maze ("known room") and
		// answers every accepted CHANGE MAP with START_MAP; this server used to
		// drop the request silently, so the client waited for a room-change reply
		// that never came and froze -- it could neither leave the room nor return
		// to town. Live 2026-09-22 05:45:51Z, char 1, dungeon 7539 maze 3:
		// current=2:1 target=2:1, one blocked request and zero server packets,
		// after which the client only emitted orphan USE_SKILL and went silent.
		//
		// The room does not change, so nothing here may touch dungeon state:
		// fatigue, drops, object keys and layer chains are all already committed
		// for this room. Only the room payload and the FINISH_LOADING handshake
		// are re-armed, exactly as an accepted ordinary move would.
		if hasCurrentScene &&
			errors.Is(err, worldmap.ErrRoomNotAdjacent) &&
			int64(request.NextX) == currentScene.Coordinate.X &&
			int64(request.NextY) == currentScene.Coordinate.Y &&
			!s.currentDungeonSameCoordinateResyncOwnedByLayer(runtime, currentScene) {
			if resyncErr := s.resyncCurrentDungeonSameCoordinateRoom(
				session,
				runtime,
				request,
				requestSource,
				currentScene,
			); resyncErr != nil {
				return resyncErr
			}
			return nil
		}
		s.logGameEvent(session, "game-dungeon-move-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"source", source,
			"next_x", request.NextX,
			"next_y", request.NextY,
			"request_source", requestSource,
			"reason", "target_room_preflight_failed",
			"error", err)
		return nil
	}
	if partyPlan != nil && partyPlan.targetSet {
		target := runtimeDungeonRoomKeyFromScene(plan.Target)
		if target != partyPlan.target {
			return fmt.Errorf("party dungeon room topology mismatch got=%+v want=%+v", target, partyPlan.target)
		}
		plan.Seed = partyPlan.seed
	}
	if runtime.altar != nil {
		if altarErr := runtime.altar.previewBindRoom(plan.Target); altarErr != nil {
			s.logGameEvent(session, "game-dungeon-altar-move-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"source", plan.Source.Coordinate.String(),
				"target", plan.Target.Coordinate.String(),
				"reason", "target_altar_room_definition_or_generation_invalid",
				"error", altarErr)
			return nil
		}
	}
	if plan.PayloadMode != currentDungeonStartMapPayloadCached {
		startDropRNG := currentPartyDungeonPrivateDropRNG(runtime, plan.Seed)
		if plan.TargetVisit != nil {
			startDropRNG = plan.TargetVisit.DropRNG
		}
		passivePlan, planErr := s.planCurrentDungeonPassiveObjectDrops(
			session,
			runtime,
			plan.Target,
			plan.NextObjectKey,
			startDropRNG,
		)
		if planErr != nil {
			s.logGameEvent(session, "game-dungeon-move-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"source", plan.Source.Coordinate.String(),
				"target", plan.Target.Coordinate.String(),
				"reason", "target_passive_object_drop_preflight_failed",
				"error", planErr)
			return nil
		}
		plan.PassiveDrops = passivePlan
	}
	// Moving away is authoritative evidence that the source room was cleared.
	// Settle pet growth against the exact fatigue amount already committed for
	// that room; the clear token makes retries and revisits idempotent.
	if plan.SourceVisit != nil {
		s.awardCurrentPetRoomClearLocked(
			session,
			runtime,
			plan.Source,
			int(plan.SourceVisit.FatigueConsumedAmount),
			"room_transition_source_clear",
		)
	}
	s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
		session,
		runtime,
		"accepted_current_exe_op45_after_pending_op24",
	)
	if plan.LayerBaseAck {
		chain := runtime.LayerChains[plan.Source.Coordinate]
		if chain == nil || !chain.Consumed || !chain.FinalAckPending {
			return fmt.Errorf("%w: layered base ACK state changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		chain.FinalAckPending = false
		s.logGameEvent(session, "game-dungeon-layer-base-ack-consumed",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", plan.Source.Coordinate.String(),
			"map_id", plan.Source.Map.Map.ID,
			"request_body_len", requestBodyLen,
			"request_source", requestSource,
			"response", "none",
			"reason", "current_exe_final_same_coordinate_change_map_confirmation_after_operation2_mode0_base_restore")
		return nil
	}
	startMapBody, err := buildCurrentDungeonMoveStartMapBody(runtime, plan)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-move-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"source", plan.Source.Coordinate.String(),
			"target", plan.Target.Coordinate.String(),
			"reason", "target_start_map_build_failed",
			"error", err)
		return nil
	}
	randomizedCreatureQueue, randomizedCreatureCount, err := buildCurrentDungeonRandomizedCreatureQueue(
		plan.Target.Coordinate,
		runtime.RandomizedObjectSelections,
	)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-move-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"source", plan.Source.Coordinate.String(),
			"target", plan.Target.Coordinate.String(),
			"reason", "target_randomized_creature_queue_failed",
			"error", err)
		return nil
	}
	targetSnapshot := plan.TargetRoom.Snapshot()
	packetActorCount := len(targetSnapshot.Monsters) + len(targetSnapshot.ExtendedActors)
	if plan.PayloadMode == currentDungeonStartMapPayloadCached {
		packetActorCount = 0
	}
	s.logGameEvent(session, "game-dungeon-move-start-map-send",
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"source", plan.Source.Coordinate.String(),
		"target", plan.Target.Coordinate.String(),
		"map_id", plan.Target.Map.Map.ID,
		"seed", plan.Seed,
		"actor_count", packetActorCount,
		"revisit", plan.Revisit,
		"layered", plan.Layered,
		"layer_return", plan.LayerReturn,
		"layer_exit", plan.LayerExit,
		"layer_base_ack", plan.LayerBaseAck,
		"layer_index", plan.LayerIndex,
		"story_stage", plan.StoryStage,
		"story_stage_index", plan.StoryStageIndex,
		"boss_entrance_map_override", plan.BossEntranceMapOverride,
		"operation", plan.Operation,
		"payload_mode", plan.PayloadMode,
		"move_kind", request.MoveKind,
		"request_body_len", requestBodyLen,
		"request_source", requestSource,
		"body_len", len(startMapBody),
		"sequence", "current_exe_op45_request_to_class0_op29_start_map")
	if plan.PayloadMode != currentDungeonStartMapPayloadCached && randomizedCreatureCount != 0 {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketVerifyCreatureQuest),
			randomizedCreatureQueue,
			0,
		); err != nil {
			return err
		}
		s.logGameEvent(session, "game-dungeon-randomized-neutral-creature-queued",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", plan.Target.Coordinate.String(),
			"creature_count", randomizedCreatureCount,
			"packet_order", "op479-explicit-creature-before-op29-room-transition")
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonStartMapNotificationForRuntime(runtime), startMapBody, 0); err != nil {
		return err
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		return fmt.Errorf("cache source dungeon room before transition commit: %w", err)
	}
	var revisitScene *worldmap.DungeonRoomScene
	if plan.Revisit && !plan.StoryStage && !plan.BossEntranceMapOverride {
		revisitScene = &plan.TargetVisit.Scene
	}
	var committedScene worldmap.DungeonRoomScene
	if plan.BossEntranceMapOverride {
		var cachedOverrideScene *worldmap.DungeonRoomScene
		if plan.TargetVisit != nil {
			cachedOverrideScene = &plan.TargetVisit.Scene
		}
		transition, commitErr := runtime.Session.CommitMoveMapOverride(
			plan.Target.Coordinate,
			plan.BossEntranceResolvedMap,
			cachedOverrideScene,
		)
		if commitErr != nil {
			return fmt.Errorf("commit boss-entrance map override after op29 send: %w", commitErr)
		}
		if transition.Revisit != plan.Revisit {
			return fmt.Errorf("%w: boss-entrance override revisit changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		committedScene = transition.Scene
	} else if plan.StoryStage {
		transition, commitErr := runtime.Session.CommitStoryStage(plan.StoryStageIndex)
		if commitErr != nil {
			return fmt.Errorf("commit current dungeon story stage after op29 send: %w", commitErr)
		}
		if transition.Revisit != plan.Revisit {
			return fmt.Errorf("%w: story-stage revisit changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		committedScene = transition.Scene
	} else if plan.Layered {
		committedScene, err = runtime.Session.CommitLayered(plan.Target.Coordinate, plan.LayerIndex)
		if err != nil {
			return fmt.Errorf("commit current layered dungeon room after op29 send: %w", err)
		}
	} else if plan.LayerReturn {
		if plan.TargetVisit == nil {
			return fmt.Errorf("%w: layered base visit missing at commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		committedScene, err = runtime.Session.CommitLayeredBase(plan.Target.Coordinate, plan.TargetVisit.Scene)
		if err != nil {
			return fmt.Errorf("commit cached layered dungeon base after op29 send: %w", err)
		}
	} else if plan.LayerExit {
		transition, moveErr := runtime.Session.MoveByteTransition(request.NextX, request.NextY, revisitScene)
		if moveErr != nil {
			return fmt.Errorf("commit terminal layered dungeon exit after op29 send: %w", moveErr)
		}
		committedScene = transition.Scene
	} else {
		transition, moveErr := runtime.Session.MoveByteTransition(request.NextX, request.NextY, revisitScene)
		if moveErr != nil {
			return fmt.Errorf("commit current dungeon room after op29 send: %w", moveErr)
		}
		committedScene = transition.Scene
	}
	if committedScene.Coordinate != plan.Target.Coordinate || committedScene.Map.Map.ID != plan.Target.Map.Map.ID {
		return fmt.Errorf("%w: planned=%s/%d committed=%s/%d",
			errDungeonMoveRuntimeOwnerMismatch,
			plan.Target.Coordinate,
			plan.Target.Map.Map.ID,
			committedScene.Coordinate,
			committedScene.Map.Map.ID,
		)
	}
	s.keepCurrentDungeonDeathReturnAcrossRoomTransitionLocked(session, runtime)
	runtime.Room = plan.TargetRoom
	runtime.Seed = plan.Seed
	if runtime.altar != nil {
		s.cancelCurrentDungeonAltarTimersLocked(runtime)
		if err := runtime.altar.bindRoom(committedScene); err != nil {
			return fmt.Errorf("bind current altar target room after op211 send: %w", err)
		}
	}
	if !plan.LayerReturn {
		if _, err := s.applyPVFTutorialBasicActionRoomToSession(
			session,
			runtime,
			committedScene,
			"room_transition_before_actor_announce",
		); err != nil {
			return fmt.Errorf("apply tutorial basic-action target room: %w", err)
		}
		committedScene, _ = runtime.Session.Scene()
	}
	if plan.StoryStage {
		if plan.StoryStageIndex != runtime.StoryStageIndex+1 {
			return fmt.Errorf("%w: story-stage index changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		runtime.StoryStageIndex = plan.StoryStageIndex
		runtime.LayeredMapIndex = -1
		runtime.LayeredMapActive = false
	} else if plan.Layered {
		if runtime.LayerChains == nil {
			runtime.LayerChains = make(map[worldmap.RoomCoordinate]*runtimeDungeonLayerChain)
		}
		chain := runtime.LayerChains[plan.Target.Coordinate]
		if chain == nil {
			chain = &runtimeDungeonLayerChain{BaseKey: plan.LayerBaseKey}
			runtime.LayerChains[plan.Target.Coordinate] = chain
		} else if chain.BaseKey != plan.LayerBaseKey {
			return fmt.Errorf("%w: layered base key changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		chain.Consumed = false
		chain.FinalAckPending = false
		runtime.LayeredMapIndex = plan.LayerIndex
		runtime.LayeredMapActive = true
	} else if plan.LayerReturn {
		chain := runtime.LayerChains[plan.Target.Coordinate]
		if chain == nil || chain.BaseKey != plan.LayerBaseKey {
			return fmt.Errorf("%w: layered return chain changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		chain.Consumed = true
		chain.FinalAckPending = true
		runtime.LayeredMapIndex = -1
		runtime.LayeredMapActive = false
	} else if plan.LayerExit {
		chain := runtime.LayerChains[plan.Source.Coordinate]
		if chain == nil || chain.BaseKey != plan.LayerBaseKey {
			return fmt.Errorf("%w: terminal layered exit chain changed before commit", errDungeonMoveRuntimeOwnerMismatch)
		}
		chain.Consumed = true
		chain.FinalAckPending = false
		runtime.LayeredMapIndex = -1
		runtime.LayeredMapActive = false
	} else {
		runtime.LayeredMapIndex = -1
		runtime.LayeredMapActive = false
	}
	announcedActors := 0
	if plan.PayloadMode != currentDungeonStartMapPayloadCached {
		announcedActors, err = runtime.Room.AnnounceAllActors(runtime.Session)
		if err != nil {
			return fmt.Errorf("commit target-room runtime actors after op29 send: %w", err)
		}
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		return fmt.Errorf("cache committed target dungeon room: %w", err)
	}
	if plan.PayloadMode != currentDungeonStartMapPayloadCached {
		if err := commitCurrentDungeonPassiveObjectDrops(runtime, committedScene, plan.PassiveDrops); err != nil {
			return fmt.Errorf("commit target-room passive-object drops after op29 send: %w", err)
		}
	}
	if err := s.consumeCurrentDungeonRoomFatigueAfterCommit(session, runtime, "room_transition_after_op29_commit"); err != nil {
		return fmt.Errorf("consume target-room fatigue: %w", err)
	}
	runtime.NextObjectKey = plan.NextObjectKey
	if runtime.partyDungeonOwner != nil {
		if _, worldErr := runtime.partyDungeonOwner.observeRuntimePartyDungeonWorldLocked(runtime); worldErr != nil {
			return fmt.Errorf("commit party shared target room: %w", worldErr)
		}
	}
	// FINISH_LOADING's main class0 state and op30 completion are room-scoped.
	// Keep the player/equipment/skill initialization gates intact: an ordinary
	// room move only needs the next room's loading completion, not a new actor.
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	s.logGameEvent(session, "game-dungeon-move-committed",
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"source", plan.Source.Coordinate.String(),
		"target", committedScene.Coordinate.String(),
		"map_id", committedScene.Map.Map.ID,
		"announced_actor_count", announcedActors,
		"revisit", plan.Revisit,
		"layered", plan.Layered,
		"layer_return", plan.LayerReturn,
		"layer_exit", plan.LayerExit,
		"layer_base_ack", plan.LayerBaseAck,
		"layer_index", plan.LayerIndex,
		"story_stage", plan.StoryStage,
		"story_stage_index", plan.StoryStageIndex,
		"boss_entrance_map_override", plan.BossEntranceMapOverride,
		"operation", plan.Operation,
		"payload_mode", plan.PayloadMode,
		"cached_defeated_actor_count", len(committedScene.DefeatedObjects),
		"passive_object_item_rows", plan.PassiveDrops.ConfiguredRowCount,
		"passive_object_drop_count", len(plan.PassiveDrops.ExtraEntries),
		"room_cleared", committedScene.Cleared,
		"request_source", requestSource,
		"finish_loading_main_state_rearmed", true)
	// 卢克的 [ON ENTER MAP]：走到王座的 boss 房才开能源储藏室（150 秒团灭钟从这里起算）。
	// 非卢克一律直接返回，不改动任何既有行为。见 raid_luke_phase_runtime.go。
	s.observeRuntimeRaidLukeEnterMapLocked(session, runtime, committedScene.Map.Map.ID)
	// 超时空的 [ON ENTER MAP]：同一个位置、同一个理由 —— 走到 BOSS 那张地图才把这一侧的
	// BOSS 机制从 state 2 推到 5（四颗星球出场、伤害窗口的前置）。原来只有副本
	// finish-loading 那一次触发、带的是起始房的地图 ID，于是 state 永远停在 2：
	// 四个球一个都不出现、罗什不给打。见 notifyRuntimeFiendWarMapEnteredLocked 的注释。
	// 非超时空、不是 BOSS 侧、或已经推进过（state != 2）时它一个字都不改。
	s.notifyRuntimeFiendWarMapEnteredLocked(session, runtime, committedScene.Map.Map.ID, "room_transition_after_op45_commit")
	if plan.PayloadMode != currentDungeonStartMapPayloadCached {
		if err := s.publishCurrentDungeonTrapEventMonsterAfterStartMap(
			session,
			runtime,
			committedScene,
			runtime.Room,
			"room_transition_after_op29_commit",
		); err != nil {
			return fmt.Errorf("publish target-room trap event-monster condition after op29: %w", err)
		}
	}
	// Runs for cached revisits too: a gate another room's kills opened since
	// the last visit has to be published the moment the player walks back in.
	// （上游 b402882）
	s.runCurrentDungeonActionObjectsLocked(session, runtime, committedScene, runtimeDungeonActionObjectRoomEntered, "room_transition_after_op29_commit")
	return nil
}
