package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// currentTownPostTransitionStage is a durable-in-session wire cursor for the
// selected actor generation created by one typed town op24. A failed write may
// resume only at the unfinished suffix; replaying an already accepted mode0 or
// mode1 can leave the current client bound to a different native object.
type currentTownPostTransitionStage byte

const (
	currentTownPostTransitionIdle currentTownPostTransitionStage = iota
	currentTownPostTransitionPending
	currentTownPostTransitionMode0Sent
	currentTownPostTransitionMode1Sent
	currentTownPostTransitionPartyActorProjectionCaptured
	currentTownPostTransitionGuardianShieldDeckSent
	currentTownPostTransitionGuardianShieldAppearanceSent
	currentTownPostTransitionChatEmoticonCatalogSent
	currentTownPostTransitionCreatureItemSent
	currentTownPostTransitionCreatureTableSent
	currentTownPostTransitionCreatureGrowthSent
	currentTownPostTransitionFinishStateSent
	currentTownPostTransitionFinishCompletionSent
	currentTownPostTransitionHellPartyStateCleared
	currentTownPostTransitionSkillReplaySuppressed
	currentTownPostTransitionExpertJobReplaySuppressed
	currentTownPostTransitionBuffRegistrationSent
	currentTownPostTransitionDarkKnightComboSent
	currentTownPostTransitionGuardianGemSent
	currentTownPostTransitionPlacementSent
	currentTownPostTransitionActorUserStateSent
	currentTownPostTransitionComplete
)

// armCurrentTownPostTransitionPlayerState starts a new selected-actor
// generation after an accepted movement/return typed town op24. First
// selection deliberately does not use this cursor: its type1345 callback owns
// only the already-deferred tail and HUD gauge and cannot safely consume
// another actor/finish-loading generation.
func (s *Service) armCurrentTownPostTransitionPlayerState(session *gameSession, source string) {
	if session == nil || session.selectedCharacterID == 0 {
		return
	}
	session.townPostTransition.mu.Lock()
	defer session.townPostTransition.mu.Unlock()

	session.townPostTransition.generation++
	session.townPostTransition.characterID = session.selectedCharacterID
	session.townPostTransition.ownerChannel = currentTownActorOwnerContext(session)
	session.townPostTransition.stage = currentTownPostTransitionPending
	session.townPostTransition.source = source
	session.townPostTransition.mode0Body = nil
	session.townPostTransition.mode1Body = nil

	// These gates belong to the actor generation invalidated by op24. The
	// staged chain below owns their one replacement for the new generation.
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	session.postFinishLoadingPlayerStateSent = false
	session.initialTownSkillInfoSent = false
	session.expertJobInfoCharacterID = 0
	session.initialTownGuardianShieldDeckSent = false
	session.initialTownGuardianShieldAppearanceSent = false
	session.chatEmoticonCatalogSent = false

	s.logGameEvent(session, "game-town-post-transition-player-state-armed",
		"source", source,
		"char_id", session.townPostTransition.characterID,
		"owner_context", session.townPostTransition.ownerChannel,
		"generation", session.townPostTransition.generation,
		"stage", session.townPostTransition.stage,
		"sequence", "mode0_mode1_op580_optional_slot24_op342_op357_slot26_op14_op105_optional_op102_op37_op30_no_hell_party_data_suppress_op19_suppress_op205_op559_optional_op447_slot32_guardian_gem_op120")
}

// resetCurrentTownPostTransitionPlayerState drops a stale pending generation
// when character selection/session state is reset before another op24.
func resetCurrentTownPostTransitionPlayerState(session *gameSession) {
	if session == nil {
		return
	}
	session.townPostTransition.mu.Lock()
	session.townPostTransition.characterID = 0
	session.townPostTransition.ownerChannel = 0
	session.townPostTransition.stage = currentTownPostTransitionIdle
	session.townPostTransition.source = ""
	session.townPostTransition.mode0Body = nil
	session.townPostTransition.mode1Body = nil
	session.townPostTransition.mu.Unlock()
}

// sendCurrentTownPostTransitionPlayerState rebuilds the exact current-client
// town generation invalidated by op24. The transport-only DLL does not rewrite
// scene owners, so both actor packets use the selected actor's committed
// context. A local actor may deliberately use context zero while party and
// remote actor records remain keyed by the CHANNELINFO connection owner.
func (s *Service) sendCurrentTownPostTransitionPlayerState(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.townPostTransition.mu.Lock()
	defer session.townPostTransition.mu.Unlock()

	stage := session.townPostTransition.stage
	if stage == currentTownPostTransitionIdle || stage == currentTownPostTransitionComplete {
		return nil
	}
	characterID := session.townPostTransition.characterID
	ownerChannel := session.townPostTransition.ownerChannel
	currentOwner := currentTownActorOwnerContext(session)
	if characterID == 0 || characterID != session.selectedCharacterID {
		return fmt.Errorf(
			"town post-transition character changed before stage %d: armed=%d selected=%d",
			stage,
			characterID,
			session.selectedCharacterID,
		)
	}
	if ownerChannel != currentOwner {
		return fmt.Errorf(
			"town post-transition owner changed before stage %d: armed=%d current=%d",
			stage,
			ownerChannel,
			currentOwner,
		)
	}
	if source == "" {
		source = session.townPostTransition.source
	}
	generation := session.townPostTransition.generation

	if session.townPostTransition.stage < currentTownPostTransitionMode0Sent {
		body, err := s.sendSelectedActorMode0GenerationRefreshWithBody(
			session,
			"town_post_transition",
			source+"_mode0_native_owner",
		)
		if err != nil {
			return err
		}
		session.townPostTransition.mode0Body = body
		session.townPostTransition.stage = currentTownPostTransitionMode0Sent
	}
	if session.townPostTransition.stage < currentTownPostTransitionMode1Sent {
		body, err := s.sendSelectedActorMode1AppearanceRefreshWithBody(
			session,
			"town_post_transition",
			source+"_mode1_after_generation_mode0",
		)
		if err != nil {
			return err
		}
		session.townPostTransition.mode1Body = body
		session.townPostTransition.stage = currentTownPostTransitionMode1Sent
	}
	if session.townPostTransition.stage < currentTownPostTransitionPartyActorProjectionCaptured {
		if err := captureCurrentPartyActorProjection(
			session,
			characterID,
			session.townPostTransition.mode0Body,
			session.townPostTransition.mode1Body,
		); err != nil {
			return fmt.Errorf("capture town post-transition party actor projection: %w", err)
		}
		session.townPostTransition.mode0Body = nil
		session.townPostTransition.mode1Body = nil
		session.townPostTransition.stage = currentTownPostTransitionPartyActorProjectionCaptured
		s.logGameEvent(session, "game-party-actor-full-projection-captured",
			"source", source+"_after_mode1",
			"char_id", characterID,
			"owner_context", ownerChannel,
			"character_generation", session.characterGeneration,
			"town_post_transition_generation", generation)
	}
	if session.townPostTransition.stage < currentTownPostTransitionGuardianShieldDeckSent {
		if _, err := s.sendSelectedGuardianShieldDeckInfo(
			session,
			source+"_guardian_shield_deck_after_mode1_before_op24",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionGuardianShieldDeckSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionGuardianShieldAppearanceSent {
		if _, err := s.sendSelectedGuardianShieldTownAppearanceRefreshOnce(
			session,
			source+"_guardian_shield_appearance_after_deck",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionGuardianShieldAppearanceSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionChatEmoticonCatalogSent {
		if _, err := s.sendSelectedChatEmoticonCatalog(
			session,
			source+"_chat_emoticon_catalog_after_mode1_before_op24",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionChatEmoticonCatalogSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionCreatureItemSent {
		// Full mode1 recreates the selected actor's slot-26 object, but the
		// current EXE consumes the creature's dynamic item/card state only from
		// its same-template list-3 op14. Publish that row before op105/op102 so
		// their native map lookup cannot observe a half-rehydrated creature.
		if err := s.sendSelectedEquippedCreatureItemRefresh(
			session,
			source+"_equipped_creature_item_after_mode1",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionCreatureItemSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionCreatureTableSent {
		// op24 created a new native actor generation, so op105 must be written
		// even when the previous generation already consumed the same table.
		session.selectedCreatureStateTableSent = false
		if err := s.sendSelectedCreatureStateTable(session, source+"_op105_after_actor_rebind"); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionCreatureTableSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionCreatureGrowthSent {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		equippedCreature, err := s.currentEquippedCreatureForCharacterWithError(
			ctx,
			strconv.Itoa(int(characterID)),
		)
		cancel()
		if err != nil {
			return err
		}
		if equippedCreature.valid() {
			if err := s.sendSelectedEquippedCreatureGrowthState(
				session,
				equippedCreature,
				source+"_op102_after_op105",
			); err != nil {
				return err
			}
		}
		session.townPostTransition.stage = currentTownPostTransitionCreatureGrowthSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionFinishStateSent {
		// The current client owns a dungeon-use gate that is released only by the
		// authoritative class0/op37 state commit on the new town actor. Historical
		// live traffic proves that omitting this frame leaves the next dungeon run
		// unable to emit op44 at all. Send it after the complete actor/creature
		// rebind, then answer a later client-authored town op37 through the existing
		// ACK-only guard so the absolute snapshot is never duplicated.
		if err := s.sendCurrentFinishLoadingCharacterStateSnapshot(
			session,
			source+"_op37_after_actor_and_creature_rebind",
		); err != nil {
			return err
		}
		session.currentFinishLoadingStateSent = true
		session.townPostTransition.stage = currentTownPostTransitionFinishStateSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionFinishCompletionSent {
		if err := s.sendCurrentIncreaseStatusResult(
			session,
			source+"_op30_after_finish_state",
		); err != nil {
			return err
		}
		session.currentFinishLoadingCompletionSent = true
		session.townPostTransition.stage = currentTownPostTransitionFinishCompletionSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionHellPartyStateCleared {
		// op679 is Hell Party encounter data, not a town reset. The authoritative
		// op37/op30 actor rebind plus retirement of the old run owns this boundary.
		session.townPostTransition.stage = currentTownPostTransitionHellPartyStateCleared
	}
	if session.townPostTransition.stage < currentTownPostTransitionSkillReplaySuppressed {
		// This route leaves and re-enters the same selected character. Replaying a
		// complete op19 here is not a skill delta; the current client interprets
		// the expert skill in it as a new qualification. Keep the existing manager
		// across the selector boundary as well; only a genuinely missing initial
		// manager may use the guarded op15 fallback.
		s.logGameEvent(session, "game-town-post-transition-skill-replay-suppressed",
			"source", source,
			"char_id", characterID,
			"msg_id", currentSkillInfoMsgID,
			"reason", "existing_character_town_rebind_must_not_replay_acquisition_sensitive_skill_snapshot")
		session.initialTownSkillInfoPrepared = false
		// The staged town rebind deliberately suppresses op19 here; leave the
		// request-driven selector fallback armed so its established op19 -> op5 ->
		// op27 boundary owns the next complete skill projection.
		session.initialTownSkillInfoSent = false
		session.initialTownSkillInfo = currentSceneSkillInfoProjection{}
		session.townPostTransition.stage = currentTownPostTransitionSkillReplaySuppressed
	}
	if session.townPostTransition.stage < currentTownPostTransitionExpertJobReplaySuppressed {
		// Native op205 is an acquisition/mutation notification on this EXE. Scene
		// reconstruction must never publish it; real qualification, abandonment,
		// recipe and machine changes retain their dedicated mutation paths.
		s.logGameEvent(session, "game-town-post-transition-expert-job-replay-suppressed",
			"source", source,
			"char_id", characterID,
			"msg_id", currentExpertJobInfoNotification,
			"reason", "scene_reconstruction_is_not_an_expert_job_mutation")
		session.expertJobInfoCharacterID = 0
		session.townPostTransition.stage = currentTownPostTransitionExpertJobReplaySuppressed
	}
	if session.townPostTransition.stage < currentTownPostTransitionBuffRegistrationSent {
		// mode1 replaces the selected actor's equipment object graph. Reapply the
		// durable BUFF enhancement registrations at the final-owner boundary.
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		err := s.sendSkillSwitchInventoryAllDataRefresh(
			ctx,
			session,
			source+"_buff_registration_after_mode1",
		)
		cancel()
		if err != nil {
			return fmt.Errorf("rehydrate town BUFF registration: %w", err)
		}
		session.townPostTransition.stage = currentTownPostTransitionBuffRegistrationSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionDarkKnightComboSent {
		if err := s.sendSelectedDarkKnightComboInitialization(
			session,
			source+"_dark_knight_combo_after_mode1",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionDarkKnightComboSent
	}
	// Non-completed dungeon exits use this staged actor rebind instead of the
	// completed-return route. Restore the collection-box owner after mode1 so
	// returning to town cannot retain the dungeon generation's cache.
	if !session.collectboxSceneProjectionSent {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		err := s.sendCollectboxSceneInitializationStates(ctx, session)
		cancel()
		if err != nil {
			s.logGameEvent(session, "game-collectbox-scene-projection-deferred",
				"char_id", characterID,
				"source", source+"_town_after_mode1",
				"reason", err)
		}
	}
	if session.townPostTransition.stage < currentTownPostTransitionGuardianGemSent {
		if err := s.sendSelectedGuardianGemWornMedalRefresh(
			session,
			source+"_guardian_gem_after_mode1",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionGuardianGemSent
	}
	// The worn title's raw random enchant is in the same create-row gap as the
	// medal's guardian-gem socket words, so it is replayed at the same boundary.
	if err := s.sendSelectedWornTitleEnchantRowRefresh(
		session,
		source+"_after_mode1",
	); err != nil {
		return err
	}
	if session.townPostTransition.stage < currentTownPostTransitionPlacementSent {
		placementBody := buildCurrentSceneActorPlacementBody()
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketRequestBlacklist),
			placementBody,
			0,
		); err != nil {
			return err
		}
		session.postFinishLoadingPlayerStateSent = true
		session.townPostTransition.stage = currentTownPostTransitionPlacementSent
	}
	if session.townPostTransition.stage < currentTownPostTransitionActorUserStateSent {
		if err := s.sendSelectedActorUserStateRefreshIfNonDefault(
			session,
			source+"_op357_after_actor_placement",
		); err != nil {
			return err
		}
		session.townPostTransition.stage = currentTownPostTransitionActorUserStateSent
	}
	session.townPostTransition.stage = currentTownPostTransitionComplete
	s.logGameEvent(session, "game-town-post-transition-player-state-sent",
		"source", source,
		"char_id", characterID,
		"owner_context", ownerChannel,
		"generation", generation,
		"stage", session.townPostTransition.stage,
		"sequence", "mode0_mode1_op580_optional_slot24_op342_op357_slot26_op14_op105_optional_op102_op37_op30_no_hell_party_data_suppress_op19_suppress_op205_op559_optional_op447_slot32_guardian_gem_op120_optional_user_state_op357")
	return nil
}
