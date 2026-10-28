package dnfbridge

import (
	"context"
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentFinishLoadingCharacterStateBodySize   = 87
	currentFinishLoadingProtectedRequestBodySize = 16
	currentFinishLoadingLegacyRequestBodySize    = 8
	currentIncreaseStatusResultMsgID             = 30
	currentIncreaseStatusResultBodySize          = 5
)

func currentFinishLoadingRequestBodyAccepted(body []byte) bool {
	switch len(body) {
	case currentFinishLoadingLegacyRequestBodySize, currentFinishLoadingProtectedRequestBodySize:
		return true
	default:
		return false
	}
}

func (s *Service) sendCurrentFinishLoadingCharacterState(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	stateNeeded := !session.currentFinishLoadingStateSent
	followupsNeeded := !session.currentFinishLoadingCompletionSent ||
		!session.postFinishLoadingPlayerStateSent
	if !stateNeeded && !followupsNeeded {
		// The caller owns the final empty class0/op42 suffix after every other
		// finish-loading projection. Do not replay op37/op30 or op120 here.
		return nil
	}

	if stateNeeded {
		if currentInitialTownPlayerStateFinalized(session) && !currentFinishLoadingHasDungeonRuntime(session) {
			// The native town actor was already fully initialized by mode0/mode1,
			// op205, op19 and typed op24. Replaying the 87-byte class0/op37
			// character snapshot here is not an ACK: the current EXE routes it
			// through the secondary-profession qualification UI and opens the
			// one-shot "learned skill" dialog on every scene reconstruction.
			// Keep the client-authored op37 success ACK and the proven op30/op120
			// suffix, but do not inject this duplicate town initialization packet.
			s.logGameEvent(session, "game-main-finish-loading-town-state-suppressed",
				"source", source,
				"char_id", session.selectedCharacterID,
				"msg_id", uint16(dnfenum.CmdPacketFinishLoading),
				"classification", 0,
				"body_len", currentFinishLoadingCharacterStateBodySize,
				"reason", "initial_town_actor_already_initialized_and_op37_opens_secondary_profession_dialog")
		} else {
			if err := s.sendCurrentFinishLoadingCharacterStateSnapshot(session, source); err != nil {
				return err
			}
		}
		// Commit this gate as soon as the state packet is on the wire. A later
		// op30/post-state failure must not replay the absolute op37 snapshot.
		session.currentFinishLoadingStateSent = true
	}
	if !session.currentFinishLoadingCompletionSent {
		if err := s.sendCurrentIncreaseStatusResult(session, source); err != nil {
			return err
		}
		session.currentFinishLoadingCompletionSent = true
	}
	if err := s.sendCurrentPostFinishLoadingPlayerState(session, source+"_after_finish_loading_state"); err != nil {
		return err
	}
	return nil
}

func currentFinishLoadingHasDungeonRuntime(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	hasRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	return hasRuntime
}

// sendCurrentFinishLoadingCharacterStateSnapshot writes the class0/op37
// character snapshot without consuming the request-owned lifecycle gates.
// Town op24 recovery uses the same authoritative body after rebuilding the
// selected actor, while ordinary client op37 handling keeps its own gates in
// sendCurrentFinishLoadingCharacterState.
func (s *Service) sendCurrentFinishLoadingCharacterStateSnapshot(session *gameSession, source string) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	stepStarted := time.Now()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	s.probeSlowStep(session, source, "selected_character_for_enter", stepStarted)
	if charID == 0 || !hasCharacter {
		s.logGameEvent(session, "game-main-finish-loading-state-deferred",
			"source", source,
			"char_id", charID,
			"reason", "selected_character_record_missing")
		return fmt.Errorf(
			"finish-loading character snapshot unavailable: selected=%d loaded=%d found=%t",
			session.selectedCharacterID,
			charID,
			hasCharacter,
		)
	}
	points := dnfrepo.SkillPointState{}
	if repositories, ok := s.repositoryGroup(); ok && repositories.Skill != nil {
		stepStarted = time.Now()
		skill, found, err := repositories.Skill.Load(ctx, character.CharacterID)
		s.probeSlowStep(session, source, "skill_load", stepStarted)
		if err != nil {
			return fmt.Errorf("load finish-loading skill points for character %s: %w", character.CharacterID, err)
		}
		if found {
			activeTree, treeErr := currentSceneActiveSkillTree(character)
			if treeErr == nil {
				if state, treeFound := dnfrepo.SkillTree(skill, int(activeTree)); treeFound {
					points = state.Points
				}
			}
			stepStarted = time.Now()
			synced, _, syncErr := s.syncCurrentSceneSkillPointLedgers(ctx, repositories, character, skill)
			s.probeSlowStep(session, source, "skill_point_ledger_sync", stepStarted)
			if syncErr == nil {
				if state, treeFound := dnfrepo.SkillTree(synced, int(activeTree)); treeFound {
					points = state.Points
				}
			} else {
				// The op37 state packet existed before PVF ledger repair and must not
				// disappear when a synthetic/test owner has no runtime PVF. Production
				// records still use the persisted ledger, while the explicit event keeps
				// the unsynchronised state observable instead of inventing point values.
				s.logGameEvent(session, "game-main-finish-loading-skill-point-sync-skipped",
					"source", source,
					"char_id", charID,
					"error", syncErr)
			}
		} else {
			s.logGameEvent(session, "game-main-finish-loading-skill-point-ledger-missing",
				"source", source,
				"char_id", charID)
		}
	} else {
		s.logGameEvent(session, "game-main-finish-loading-skill-point-repository-missing",
			"source", source,
			"char_id", charID)
	}
	stepStarted = time.Now()
	presentation, err := s.currentFinishLoadingSnapshotPresentation(ctx, character)
	s.probeSlowStep(session, source, "finish_loading_presentation", stepStarted)
	if err != nil {
		return fmt.Errorf("resolve finish-loading experience baseline for character %s: %w", character.CharacterID, err)
	}
	body := buildCurrentFinishLoadingCharacterStateBodyWithPresentation(character, points, presentation)
	if len(body) != currentFinishLoadingCharacterStateBodySize {
		return fmt.Errorf("current finish-loading state body length %d, want %d", len(body), currentFinishLoadingCharacterStateBodySize)
	}
	s.logGameEvent(session, "game-main-finish-loading-state-send",
		"source", source,
		"char_id", charID,
		"msg_id", uint16(dnfenum.CmdPacketFinishLoading),
		"classification", 0,
		"body_len", len(body),
		"level", body[0],
		"total_exp", statU32(character, "exp", 0),
		"remaining_sp", points.RemainingSP,
		"remaining_tp", points.RemainingTP,
		"dynamic_count", body[0x2e],
		"request_followups", true,
		"body_source", "current_exe_sub_1D78240_real_character_minimum_dynamic_table_empty")
	stepStarted = time.Now()
	sendErr := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketFinishLoading), body, 0)
	s.probeSlowStep(session, source, "send_finish_loading_body", stepStarted)
	return sendErr
}

func (s *Service) sendCurrentIncreaseStatusResult(session *gameSession, source string) error {
	body := buildCurrentIncreaseStatusResultBody()
	if len(body) != currentIncreaseStatusResultBodySize {
		return fmt.Errorf("current increase-status result body length %d, want %d", len(body), currentIncreaseStatusResultBodySize)
	}
	s.logGameEvent(session, "game-main-increase-status-result-send",
		"source", source,
		"msg_id", currentIncreaseStatusResultMsgID,
		"classification", 0,
		"body_len", len(body),
		"duration", 0,
		"selector", 0,
		"actor_effect", "none",
		"body_source", "current_exe_op30_increase_status_timer_ui_not_actor_commit")
	return s.sendGameUpperRawClass(session, currentIncreaseStatusResultMsgID, body, 0)
}

func buildCurrentIncreaseStatusResultBody() []byte {
	var writer packetWriter
	writer.writeUint32(0) // No persisted optional duration/status state for this fresh character.
	writer.writeByte(0)
	return writer.bytes()
}

func (s *Service) sendCurrentPostFinishLoadingPlayerState(session *gameSession, source string) error {
	if session == nil || session.postFinishLoadingPlayerStateSent {
		return nil
	}
	initialTownFinalized := currentInitialTownPlayerStateFinalized(session)
	if session.selectedCharacterID == 0 {
		s.logGameEvent(session, "game-main-post-finish-loading-player-state-deferred",
			"source", source,
			"char_id", 0,
			"reason", "selected_character_record_missing")
		return nil
	}
	if initialTownFinalized {
		// A real class1/op37 request may still occur in town, so acknowledge and
		// publish its own op37/op30/op120 state. It does not own initial op19,
		// which the staged post-op24 generation installed after its own op30.
		// This prevents a later request from rebuilding the same native skill
		// manager.
		s.logGameEvent(session, "game-main-post-finish-loading-player-state-send",
			"source", source,
			"char_id", session.selectedCharacterID,
			"sequence", "op120_current_hpmp_only_initial_op19_already_installed_after_post_op24_op30",
			"state_source", "request_owned_finish_loading_does_not_replay_initial_town_skill_manager")
		placementBody := buildCurrentSceneActorPlacementBody()
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRequestBlacklist), placementBody, 0); err != nil {
			return err
		}
		if err := s.sendCurrentTrainingRoomBuffRegistration(session, source); err != nil {
			return err
		}
		session.postFinishLoadingPlayerStateSent = true
		return nil
	}
	sequence := "op19_final_actor_skills_op120_current_hpmp_then_deferred_tutorial_op3"
	if session.dungeonSceneSkillInfoSent {
		sequence = "op120_current_hpmp_then_deferred_tutorial_op3_dungeon_skill_initialized_after_mode1"
	}
	s.logGameEvent(session, "game-main-post-finish-loading-player-state-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"sequence", sequence,
		"state_source", "pre_op27_mode0_and_post_op29_mode1_already_initialized_the_local_actor")

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 || !hasCharacter {
		s.logGameEvent(session, "game-main-post-finish-loading-player-state-deferred",
			"source", source,
			"char_id", charID,
			"reason", "selected_character_record_missing")
		return nil
	}
	if !session.dungeonSceneSkillInfoSent {
		if err := s.sendCurrentSceneSkillInfo(session, ctx, character, source+"_final_actor_skills"); err != nil {
			return err
		}
		session.dungeonSceneSkillInfoSent = true
	} else {
		s.logGameEvent(session, "game-main-post-finish-loading-skill-info-skipped",
			"source", source,
			"char_id", charID,
			"reason", "dungeon_mode1_skill_projection_already_sent_before_scene_commit")
	}
	// The post-start-map path sends this after its own op19. If skill projection
	// was deferred until the client finish-loading request, this is the matching
	// safe boundary. The character-ID gate prevents a duplicate op205, and an
	// unchanged profession is not replayed at all: on this 90CN client that
	// replay is an acquisition notification, and a town generation whose op19
	// was suppressed would open the qualification banner with it.
	if err := s.sendCurrentExpertJobInfoForSceneRebuild(
		session,
		character,
		source+"_after_final_scene_skill_manager",
	); err != nil {
		return fmt.Errorf("restore dungeon expert-job state after final skill manager: %w", err)
	}
	// The saved option table needs one replay here for the same reason the
	// expert-job projection does: its client-side applier is gated on the
	// local actor, which the scene-init send predates. Without it the
	// skill-cooldown reminder stays off until the player sets it again.
	if err := s.sendCurrentCharacterOptionSnapshotForActor(session, source+"_after_final_scene_skill_manager"); err != nil {
		return fmt.Errorf("replay character option snapshot after final skill manager: %w", err)
	}
	// The current NoPack client clears the Dark Knight's separate native combo
	// manager as this final scene transition completes.  The normal op19 state
	// is intentionally not replayed when it was already sent, but job 9 still
	// requires its real class-0/op447 page restore here, after the final actor
	// skill manager exists and before the placement packet makes the scene live.
	// This is not the removed private op448 compatibility projection.
	if err := s.sendCurrentDarkKnightComboInitialization(session, ctx, character, source+"_after_final_scene_skill_manager"); err != nil {
		return err
	}
	// A newly created Creator enters its tutorial dungeon without ever crossing
	// the town type1345 HUD-ready boundary. Apply the class-owned WASD/Space and
	// 1..5 keymap here, after the final dungeon skill manager exists and before
	// placement exposes the HUD. Ordinary jobs and Dark Knight remain untouched.
	if err := s.sendCurrentCreatorKeymapInitialization(session, ctx, character, source+"_after_final_scene_skill_manager"); err != nil {
		return err
	}
	// mode3 is the full personal-information/equipment snapshot and is not part
	// of dungeon actor initialization. Replaying it after op37 opens that panel
	// over the dungeon and can replace live room actor state, so this boundary
	// deliberately emits only the skill refresh and current HP/MP placement.
	placementBody := buildCurrentSceneActorPlacementBody()
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRequestBlacklist), placementBody, 0); err != nil {
		return err
	}
	// Only PVF tutorial scenes defer state 1 to this boundary.  Ordinary
	// dungeons sent it after their proven mode0/mode1 actor binding above;
	// replaying it here suppresses the native GetItem writer on the current EXE.
	objectKey := session.deferredDungeonUserStateObjectKey
	if objectKey != 0 {
		userStateBody, err := buildCurrentDungeonUserStateBody(objectKey)
		if err != nil {
			return err
		}
		s.logGameEvent(session, "game-main-post-finish-loading-deferred-user-state-send",
			"source", source,
			"char_id", charID,
			"object_key", objectKey,
			"msg_id", uint16(dnfenum.CmdPacketNotifyUserState),
			"classification", 0,
			"user_state", currentDungeonPlayerUserState,
			"body_len", len(userStateBody),
			"body_source", "current_exe_sub_1D88A10_after_mode0_mode1_and_op120_room_object_manager_refresh")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyUserState), userStateBody, 0); err != nil {
			return err
		}
		session.deferredDungeonUserStateObjectKey = 0
	}
	if err := s.sendCurrentTrainingRoomBuffRegistration(session, source); err != nil {
		return err
	}
	session.postFinishLoadingPlayerStateSent = true
	return nil
}

// sendCurrentTrainingRoomBuffRegistration replays the durable BUFF
// registration for a training-room direct entry, which is the one dungeon
// route that never reaches the ordinary placement tail.
//
// Every other dungeon binds it from sendCurrentPostStartMapPlayerPlacement
// after the op124 layout commit, and the shared party route does the same
// after its final op19 - "otherwise party dungeons never bind the durable BUFF
// enhancement set at all". Direct entry sends neither op124 nor the placement
// tail, so before this the training room was the only map where a registered
// buff set simply did not apply. Doing it here, off the client's own finish
// loading, is the first moment its scene owns the skill-modifier rebuild that
// op559 triggers.
func (s *Service) sendCurrentTrainingRoomBuffRegistration(session *gameSession, source string) error {
	if s == nil || session == nil || !session.trainingRoomBuffRegistrationPending {
		return nil
	}
	session.trainingRoomBuffRegistrationPending = false
	if session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if err := s.sendSkillSwitchInventoryAllDataRefresh(
		ctx,
		session,
		source+"_training_room_after_finish_loading",
	); err != nil {
		return fmt.Errorf("rehydrate training-room BUFF registration: %w", err)
	}
	return nil
}

func currentInitialTownPlayerStateFinalized(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.townMu.Lock()
	finalized := session.initialTownRouteCharacterID != 0 &&
		session.initialTownRouteCharacterID == session.selectedCharacterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent
	session.townMu.Unlock()
	return finalized
}

func buildCurrentFinishLoadingCharacterStateBody(
	character dnfrepo.CharacterRecord,
	points dnfrepo.SkillPointState,
) []byte {
	return buildCurrentFinishLoadingCharacterStateBodyWithPresentation(character, points, nil)
}

type currentFinishLoadingExperiencePresentation struct {
	GrowthContractBonus uint32
	FatigueBatteryBonus uint32
	// WelcomeBonus 是回归冒险家（onlineevent returnee）的 +20% 欢迎怪物经验，
	// 在 op37 里有自己的分类字段，不能被折进 TotalExperience 里静默吃掉。
	WelcomeBonus    uint32
	TotalExperience *uint32
}

func buildCurrentFinishLoadingCharacterStateBodyWithPresentation(
	character dnfrepo.CharacterRecord,
	points dnfrepo.SkillPointState,
	presentation *currentFinishLoadingExperiencePresentation,
) []byte {
	var writer packetWriter
	totalExperience := statU32(character, "exp", 0)
	if presentation != nil && presentation.TotalExperience != nil {
		totalExperience = *presentation.TotalExperience
	}
	writer.writeByte(currentFinishLoadingLevel(character.Level))
	writer.writeUint32(totalExperience)
	writer.writeUint32(statU32(character, "finish_loading_exp_category_0", 0))
	writer.writeUint16(currentFinishLoadingSkillPoint(points.RemainingSP))
	writer.writeUint16(currentFinishLoadingSkillPoint(points.RemainingSP))
	writer.writeUint16(currentFinishLoadingSkillPoint(points.RemainingTP))
	writer.writeUint16(currentFinishLoadingSkillPoint(points.RemainingTP))
	writer.writeUint32(statU32(character, "finish_loading_currency_slot2_total", 0))
	// Current sub_1D78240 reads +0x15 as the 0xA5F4 fatigue-buff EXP
	// category and subtracts it from the displayed base, not from total EXP.
	fatigueBatteryBonus := statU32(character, "finish_loading_exp_category_1", 0)
	if presentation != nil {
		fatigueBatteryBonus = presentation.FatigueBatteryBonus
	}
	writer.writeUint32(fatigueBatteryBonus)
	writer.writeByte(byte(statU32(character, "finish_loading_result_flag", 0)))
	writer.writeUint32(statU32(character, "finish_loading_exp_category_2", 0))
	growthContractBonus := statU32(character, "finish_loading_exp_category_3", 0)
	if presentation != nil {
		growthContractBonus = presentation.GrowthContractBonus
	}
	writer.writeUint32(growthContractBonus)
	writer.writeUint32(statU32(character, "finish_loading_independent_scalar", 0))
	writer.writeUint32(statU32(character, "finish_loading_exp_category_4", 0))
	writer.writeUint32(statU32(character, "finish_loading_exp_category_5", 0))
	writer.writeByte(0) // No persisted current dynamic EXP-category rows.
	// Offset 47 is the current client's 80009/Buff parameter. Per-monster
	// settlement values belong to op35's object-key dynamic table, never here.
	writer.writeUint32(statU32(character, "finish_loading_exp_category_6", 0))
	writer.writeUint32(statU32(character, "finish_loading_exp_category_7", 0))
	writeCurrentHonorExpertState(&writer, character)
	writer.writeUint32(statU32(character, "finish_loading_exp_category_8", 0))
	writer.writeUint32(0) // Consumed but unused by the current reader.
	// +0x4B 是原生"回归冒险家欢迎经验"分类：参考实现从这个偏移读，
	// op37 的 75 字节校验也依赖它。没有 presentation 时回落到持久化分类。
	welcomeBonus := statU32(character, "finish_loading_exp_category_9", 0)
	if presentation != nil {
		welcomeBonus = presentation.WelcomeBonus
	}
	writer.writeUint32(welcomeBonus)
	writer.writeUint32(statU32(character, "finish_loading_exp_category_10", 0))
	writer.writeUint32(statU32(character, "finish_loading_exp_category_11", 0))
	return writer.bytes()
}

func (s *Service) currentFinishLoadingSnapshotPresentation(
	ctx context.Context,
	character dnfrepo.CharacterRecord,
) (*currentFinishLoadingExperiencePresentation, error) {
	persisted := statU32(character, "exp", 0)
	if character.Level < currentAdventureCharacterLevelCap {
		return nil, nil
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	tables, err := progression.Load(ctx, archive)
	if err != nil {
		return nil, err
	}
	baseline, err := currentCharacterExperienceBaseline(tables, character.Level, persisted)
	if err != nil {
		return nil, err
	}
	return &currentFinishLoadingExperiencePresentation{TotalExperience: &baseline}, nil
}

func currentFinishLoadingLevel(level int) byte {
	switch {
	case level < 1:
		return newCharacterInitialLevel
	case level > 0xff:
		return 0xff
	default:
		return byte(level)
	}
}

func currentFinishLoadingSkillPoint(value int) uint16 {
	if value <= 0 {
		return 0
	}
	if value > int(^uint16(0)) {
		return ^uint16(0)
	}
	return uint16(value)
}
