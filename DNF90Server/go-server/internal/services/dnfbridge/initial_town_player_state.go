package dnfbridge

import (
	"context"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// sendCurrentInitialTownPlayerState completes the selected town actor before
// the final op24 transition commit. No DOVE packet body and no synthetic C2S
// op37 is used here.
func (s *Service) sendCurrentInitialTownPlayerState(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.townMu.Lock()
	defer session.townMu.Unlock()
	return s.sendCurrentInitialTownPlayerStateLocked(session, source)
}

// sendCurrentInitialTownPlayerStateLocked sends every fallible town actor
// initialization phase before op24. The caller must hold session.townMu. A
// failure leaves the route below currentInitialTownRouteTransitionSent so the
// client remains on its current page and an exact retry can resume safely.
func (s *Service) sendCurrentInitialTownPlayerStateLocked(session *gameSession, source string) error {
	return s.sendCurrentTownPlayerStateLocked(
		session,
		source,
		currentInitialTownRoutePolicy{
			includeReportClientSpec: true,
			includeSecondFullMode1:  true,
		},
	)
}

// sendCurrentTownPlayerStateLocked applies the selected route policy to the
// shared scene-ready finalizer. The caller must hold session.townMu.
func (s *Service) sendCurrentTownPlayerStateLocked(
	session *gameSession,
	source string,
	policy currentInitialTownRoutePolicy,
) error {
	if session == nil {
		return nil
	}
	characterID := session.initialTownRouteCharacterID
	stage := session.initialTownRouteStage
	if characterID == 0 || characterID != session.selectedCharacterID ||
		stage < currentInitialTownRouteActorBound {
		return nil
	}
	if stage == currentInitialTownRoutePlayerStatePrepared ||
		stage >= currentInitialTownRoutePlayerStateSent {
		s.logGameEvent(session, "game-initial-town-player-state-duplicate-skipped",
			"source", source,
			"char_id", characterID,
			"route_stage", stage)
		return nil
	}
	// Do not arm a selected-actor mode3 refresh here. The current EXE treats
	// class0/op2 mode3 as the personal-information panel route, so sending it
	// during login opens that panel without user action. Mode1 already installs
	// the authoritative actor state and op205 owns the profession projection.

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, charName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID != characterID || !hasCharacter {
		return fmt.Errorf("initial town player state character mismatch: selected=%d loaded=%d found=%t", characterID, charID, hasCharacter)
	}
	objectKey := currentSceneActorObjectKey(charID)
	sequence := "actor_bound_then_op358_pages_op376_op391_cargo_reset_op13_containers_op251_item_lock_snapshot_op898_crystal_op863_aura_op1340_op19_weapon_validation_preload_full_mode1_op19_final_pet_only_op14_slot26_op105_op102_op21_acceptable_op574_active_op1346_op359_op356_op9_op120_then_typed_op24_without_op19_replay_then_scene_ready_op108_spend_time_op1206_op1303"
	if policy.includeReportClientSpec {
		sequence = "actor_bound_then_op358_pages_op376_op391_cargo_reset_op13_containers_op251_item_lock_snapshot_op898_crystal_op863_aura_op1340_op19_weapon_validation_preload_full_mode1_op19_final_pet_only_op14_slot26_op105_op102_op21_acceptable_op574_active_op1346_op359_op356_op124_op9_op120_then_typed_op24_without_op19_replay_then_scene_ready_op108_spend_time_op1206_op1303"
	}
	s.logGameEvent(session, "game-initial-town-player-state-send",
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"route_stage", stage,
		"sequence", sequence,
		"report_client_spec_sent", policy.includeReportClientSpec,
		"action_table", "selected_before_typed_op24_final_transition_commit",
		"transition_policy", "do_not_send_op24_until_full_player_state_succeeds",
		"evidence", "user_required_pretransition_initialization_with_current_exe_typed_bodies")

	if err := s.sendCurrentSceneOverseerPages(ctx, session); err != nil {
		return err
	}
	// Select the active action table after the five overseer pages and before
	// container/player state. The final op24 transition is withheld until every
	// phase in this function has succeeded.
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketPVPMissionHpPercent),
		buildCurrentActionTableStateBody(),
		0,
	); err != nil {
		return err
	}
	// Current NoPack resolves the destination container manager before it
	// installs class0/op13 rows. The actor-binding stage above must therefore
	// own the one-shot bootstrap; sending it next to the select ACK is too early
	// and leaves a visibly empty inventory even when the packet contains rows.
	// Cargo-pad state remains first so the personal-cargo capacity is valid.
	if err := s.sendCurrentCargoPadResetBeforeItemLists(session, source+"_before_full_player_state"); err != nil {
		return err
	}
	if err := s.sendCurrentSelectInventoryBootstrap(session, source+"_after_actor_before_full_player_state"); err != nil {
		return err
	}
	// Current NoPack dispatches op898 only through the class0 notification
	// table. Publish the durable crystal selection after all six native
	// inventory managers exist, but before op1340/full mode1/op24 can expose
	// the town UI. The session gate makes the later client-authored op36 path an
	// idempotent fallback instead of a second initialization.
	if err := s.sendCurrentCrystalContractStateOnce(
		session,
		source+"_after_inventory_bootstrap_before_adventure_mode1",
	); err != nil {
		return err
	}
	// The durable aura_flag is not sufficient for the current EXE to render the
	// aura appearance slot as open. Publish the marked op863 state on cold
	// login, return-select, and reconnect after the inventory managers exist.
	// This bootstrap is the sole lifecycle owner: ordinary op36 town movement
	// must not append state packets or disturb its response ordering.
	if err := s.sendCurrentAuraSkinSlotTownUIReadyState(session); err != nil {
		return err
	}
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	if repos, ok := s.repositoryGroup(); ok {
		legacyRepo = repos.LegacyUserInfo
	}
	adventureSummary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, character, hasCharacter)
	adventureLevel := uint32(adventureSummary.ManageLevel)
	// sub_C7DAF0 (op1340) populates the account-wide adventure model consumed
	// by mode1/sub_2008010 before it applies the real management-level option
	// rows, including the four basic-stat bonus. The mode0/mode1 prerequisite
	// above has already created and bound the selected actor, so op1340's
	// current-object check is satisfied before the final transition commit.
	if policy.includeSecondFullMode1 {
		if err := s.sendCurrentAdventureInfoPushFromAccount(session, objectKey, source+"_before_mode1_adventure_model"); err != nil {
			return err
		}
	}
	// Do not populate the expert-job manager before the first op19. This client
	// interprets that order as a newly acquired qualification. The durable op205
	// state is restored below, after the ordinary scene skill snapshot.
	//
	// Cross-character selection retains the previous actor's skill manager
	// until op19 is consumed. The full equipment-bearing mode1 immediately
	// creates and validates worn equipment; sending it first lets
	// weapon-mastery jobs (Weapon Master, Vagabond and similar branches) queue a
	// client-authored unequip before the correct skills arrive. Build the
	// projection once, preload only op19 before that mode1, then replay the
	// identical projection at the established final boundary so quickbars and
	// tree restoration keep their final-owner lifecycle.
	skillProjection, skillProjectionReady, err := s.buildCurrentSceneSkillInfoProjection(
		session,
		ctx,
		character,
		source+"_before_full_mode1_weapon_validation",
	)
	if err != nil {
		return err
	}
	if skillProjectionReady && policy.includeSecondFullMode1 {
		if err := s.sendCurrentSceneSkillInfoProjectionBeforeEquipment(
			session,
			skillProjection,
			source+"_before_full_mode1_weapon_validation",
		); err != nil {
			return err
		}
	}
	mode1Body := s.buildCurrentSelectedUserInfoMode1BodyWithAdventureLevelInContext(
		ctx,
		session,
		legacyRepo,
		character,
		hasCharacter,
		charID,
		adventureLevel,
		currentTownActorOwnerContext(session),
	)
	if !policy.includeSecondFullMode1 {
		// The route already created the selected actor with the complete,
		// equipment-bearing mode1 immediately after mode0. Replaying it here
		// would rebuild the same actor slots after the container bootstrap.
		s.logPacketEvent("game-initial-town-full-mode1-duplicate-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", charID,
			"object_key", objectKey,
			"owner_channel", session.townActorOwnerChannel,
			"body_len", len(mode1Body),
			"reason", "complete_mode1_was_sent_immediately_after_mode0")
	} else {
		s.logPacketEvent("game-initial-town-full-mode1-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", charID,
			"object_key", objectKey,
			"adventure_total_point", adventureSummary.TotalPoint,
			"adventure_manage_level", adventureSummary.ManageLevel,
			"adventure_manage_option", adventureSummary.ManageOption,
			"body_len", len(mode1Body),
			"stat_blob_len", currentMode1StatBlobWireSize,
			"body_source", "current_exe_sub_2008010_existing_town_actor_real_state_and_equipment_create")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode1Body, 0); err != nil {
			return err
		}
		// Channel reconnect first binds a lightweight mode1 and only reaches the
		// complete equipment-bearing owner row here. Cold login deliberately omits
		// that prerequisite mode1 to avoid replaying native qualification state, so
		// its exact already-written mode0 arrives through the route policy. In both
		// cases form the admission cache only after this complete mode1 write.
		projection, found := currentPartyActorProjectionSnapshot(
			session,
			charID,
			session.characterGeneration,
		)
		mode0Body := projection.mode0
		if !found {
			mode0Body = policy.actorMode0Body
		}
		if len(mode0Body) > 0 {
			if err := captureCurrentPartyActorProjection(
				session,
				charID,
				mode0Body,
				mode1Body,
			); err != nil {
				s.logGameEvent(session, "game-party-actor-full-projection-refresh-rejected",
					"source", source,
					"char_id", charID,
					"character_generation", session.characterGeneration,
					"mode0_len", len(mode0Body),
					"mode1_len", len(mode1Body),
					"reason", err)
			} else {
				event := "game-party-actor-full-projection-captured"
				if found {
					event = "game-party-actor-full-projection-refreshed"
				}
				s.logGameEvent(session, event,
					"source", source,
					"char_id", charID,
					"character_generation", session.characterGeneration,
					"owner_server", mode0Body[3],
					"owner_channel", mode0Body[4],
					"mode0_len", len(mode0Body),
					"mode1_len", len(mode1Body))
			}
		}
	}
	session.selectedUserInfoRefreshSent = true
	// Cold login, return-select and channel reconnect all reach their final
	// equipment-bearing mode1 in this function. The collection-box projection
	// was published earlier (the select-ACK seed, or the actor prerequisites
	// between mode0 and mode1), so refresh the selected box once here, against
	// the actor that now owns the native collect-box stat owner.
	if err := s.replayCollectboxSelectedRefreshAfterMode1(
		ctx,
		session,
		source+"_after_full_mode1",
	); err != nil {
		s.logGameEvent(session, "game-collectbox-selection-refresh-deferred",
			"char_id", charID,
			"source", source+"_after_full_mode1",
			"reason", err)
	}
	// Current NoPack owns the Guardian's five shield choices in an independent
	// class0/op580 table. Publish it only after the complete equipment-bearing
	// mode1 actor exists and before the first typed town op24 transition. A live
	// main shield then uses slot 24's native op342/op357 redraw path; it is not a
	// member of mode0's fourteen-row paper-doll table.
	if _, err := s.sendSelectedGuardianShieldDeckInfo(
		session,
		source+"_guardian_shield_deck_after_full_mode1_before_typed_op24",
	); err != nil {
		return err
	}
	if _, err := s.sendSelectedGuardianShieldTownAppearanceRefreshOnce(
		session,
		source+"_guardian_shield_appearance_after_deck_before_typed_op24",
	); err != nil {
		return err
	}
	// The current client does not build either emoticon pane from PVF by
	// itself.  Publish the proved class1/op411 catalog after the selected actor
	// exists so both the chat picker and its settings page are populated.
	if _, err := s.sendSelectedChatEmoticonCatalog(
		session,
		source+"_chat_emoticon_catalog_after_full_mode1_before_typed_op24",
	); err != nil {
		return err
	}
	// Mode1 creates the worn medal object but does not carry its guardian-gem
	// raw socket words. Rehydrate only a medal that actually has a persisted
	// guardian gem through the proved list-3 op14 carrier before the first
	// visible town transition. This is intentionally narrower than a generic
	// equipment refresh, which the current client rejects during initialization.
	if err := s.sendSelectedGuardianGemWornMedalRefresh(
		session,
		source+"_after_full_mode1_before_creature_state",
	); err != nil {
		return err
	}
	// The same boundary also owes the worn title its raw random-enchant bytes:
	// the create rows do not carry +0x61..+0x65, and nothing else in this build
	// ever publishes a worn list-3 row (see random_title_enchant_refresh.go).
	if err := s.sendSelectedWornTitleEnchantRowRefresh(
		session,
		source+"_after_full_mode1_before_creature_state",
	); err != nil {
		return err
	}
	// The complete mode1 replaces the current EXE's native skill manager. The
	// durable command list must therefore be projected after that actor-binding
	// boundary. A second mode0 here would recreate the town actor and make the
	// client visibly snap during login/reselect.
	// Keeping this before typed op24 preserves the known no-qualification-dialog
	// transition, while making cold-login state use the exact same persistence
	// source as dungeon entry and town return.
	if skillProjectionReady {
		// Full mode1 may replace the native skill manager seeded for equipment
		// validation. Replay the exact same projection here; this final op19
		// remains the sole owner of quickbar and tree restoration.
		if err := s.sendInitialTownSkillInfoProjectionBeforeTransition(
			session,
			skillProjection,
			source+"_after_final_mode0_mode1_before_typed_op24",
		); err != nil {
			return err
		}
	} else if err := s.sendInitialTownSkillInfoBeforeTransition(
		session,
		ctx,
		character,
		source+"_after_mode1_before_typed_op24",
	); err != nil {
		return err
	}
	if policy.commitTransitionEarly || session.initialTownRouteStage < currentInitialTownRouteProfessionStateSent {
		// This is state reconstruction only: op19 has already installed the normal
		// skill snapshot, so publish exactly one durable op205 without replaying a
		// quest-finish ACK or any expert-skill acquisition packet.
		if err := s.sendCurrentExpertJobInfoForCharacter(session, character, false); err != nil {
			return fmt.Errorf("restore initial-town expert-job state after skill snapshot: %w", err)
		}
		if session.initialTownRouteStage < currentInitialTownRouteProfessionStateSent {
			session.initialTownRouteStage = currentInitialTownRouteProfessionStateSent
		}
		s.logGameEvent(session, "game-initial-town-expert-job-state-restored",
			"source", source,
			"char_id", charID,
			"order", "mode0_mode1_op19_then_state_only_op205",
			"qualification_packets_replayed", false)
	}
	// mode0/mode1 have created the local actor, which is what the client's
	// option applier is gated on. The scene-init op390 landed before that and
	// stored the table without rebuilding the skill-cooldown reminder list, so
	// replay the same body once here.
	if err := s.sendCurrentCharacterOptionSnapshotForActor(session, source+"_after_mode1_local_actor"); err != nil {
		return fmt.Errorf("replay initial-town character option snapshot after mode1: %w", err)
	}
	// Full mode1 has now created the equipped creature object. Publish the
	// absolute creature table and growth state before the typed town transition
	// so the first visible pet panel already has satiety and experience.
	if err := s.sendSelectedCreatureInitialStateAfterMode1(
		session,
		source+"_after_full_mode1_before_typed_op24",
	); err != nil {
		return err
	}
	// Do not send class0/op2 mode3 during login. The current EXE routes that
	// packet through sub_2008600, which opens the personal-information panel;
	// mode1 already carries the same authoritative actor state and equipment.
	s.logPacketEvent("game-initial-town-mode3-suppressed",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"adventure_total_point", adventureSummary.TotalPoint,
		"adventure_manage_level", adventureSummary.ManageLevel,
		"stat_blob_len", 92,
		"reader", "current_exe_sub_2008600",
		"reason", "mode3_is_user_initiated_personal_information_panel_route")
	// All five real op13 containers were installed immediately after the select
	// ACK. Generic op14 remains forbidden during initialization because current
	// EXE sub_1D73120 hardcodes the item-install notification flag to one. The
	// only exceptions are the guarded list-3/slot-32 medal rehydrated above and
	// the existing list-3/slot-26 creature row: their mode1-created objects each
	// require that specific callback to restore durable dynamic state.
	// F1 "All" is rebuilt by the current op21 acceptable/definition list,
	// while op574 owns the live active triggers. Both handlers require the
	// selected actor and its complete mode1 equipment model to exist.
	if err := s.sendCurrentInitialTownQuestSnapshotsLocked(session, source+"_after_mode1_and_op19_before_typed_op24"); err != nil {
		return err
	}
	// op1340 has already populated the model before mode1 consumes it.  Keep
	// op1346 after the full actor state: it remains the actor-bound
	// notification for the account display name and overhead/UI views.
	if err := s.sendCurrentAdventureActorRefreshFromAccount(session, objectKey, source+"_after_adventure_info_actor_bound"); err != nil {
		return err
	}
	// op1340/op1346 populate account and actor adventure models but carry no
	// represent-account name text. Publish op1371 after mode1 has bound the
	// actor and before op24 makes the town scene visible.
	representName, _, _, representErr := s.currentRepresentAccountIdentity(ctx, session)
	if representErr != nil {
		s.logGameEvent(session, "game-adventure-overhead-name-initial-skipped",
			"source", source,
			"reason", "represent_account_identity_unavailable",
			"error", representErr)
	} else if representName != "" {
		if err := s.sendRepresentAccountNameState(
			session,
			representName,
			false,
			source+"_after_adventure_actor_bound_before_typed_op24",
		); err != nil {
			return err
		}
	}
	if err := s.sendSelectedCurrentEpicProductionInfo(
		session,
		source+"_after_actor_bound_before_scene_commit",
	); err != nil {
		return err
	}

	insertBody := s.buildCurrentInsertOverseerBodyForSession(ctx, session, buildCurrentInsertOverseerBody())
	insertPacket := csharpSelectInitPacket{
		class: 0,
		msgID: uint16(dnfenum.CmdPacketInsertOverseer),
		kind:  csharpLongHengSceneBootstrapKind,
	}
	if err := s.sendCSharpSelectInitPacket(session, insertPacket, insertBody); err != nil {
		return err
	}
	if policy.deferClearQuestListUntilLocationNotifications {
		s.logGameEvent(session, "game-initial-town-clear-quest-list-deferred",
			"source", source,
			"char_id", charID,
			"reason", "current_exe_op356_must_follow_position_area_notifications_before_op24")
	} else {
		clearQuestListBody, err := s.buildCurrentClearQuestListTransportBodyForSession(
			ctx,
			session,
			source+"_after_actor_and_quest_snapshots_before_scene_commit",
		)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperFixed16Transport(
			session,
			currentClearQuestListMsgID,
			clearQuestListBody,
			0,
			1,
			true,
			currentClearQuestListTransportCodec,
		); err != nil {
			return err
		}
	}
	// Keep the cancelled loading-cover boundary as a zero-duration seam.
	s.waitCurrentInitialTownSceneCommitLocked(session, characterID)
	if policy.includeReportClientSpec {
		if err := s.sendCurrentSceneLayoutCommit(session, source+"_after_mode1_before_scene_layout"); err != nil {
			return err
		}
	} else {
		s.logGameEvent(session, "game-channel-reconnect-report-client-spec-skipped",
			"source", source,
			"char_id", charID,
			"object_key", objectKey,
			"reason", "current_exe_cross_channel_page_state_crashes_in_op124_when_actor_binding_is_not_page_owned")
	}

	partyState := runtimePartyStateSnapshot(session)
	// op9 is the party/scene-manager record, not the selected actor's mode0 or
	// mode1 owner. Keep it on the connected channel so the client clears or
	// registers the global party actor in the same table used by op153/op11.
	partyOwner := currentPartySceneOwnerContext(session)
	op9Body := buildCurrentInitialTownOp9Body(session, objectKey, character, hasCharacter, charName)
	op9Kind := currentSceneOp9ActorRemoveKind
	bodySource := "current_exe_sub_1D64CA0_kind3_remove_party_manager_record"
	if partyState.PartyID > 0 {
		// Active-party sessions receive the complete member projection from the
		// runtime party broadcaster. Retain a kind-0 registration here only for
		// that explicitly active state; an ordinary partyless login must not
		// create the client's global one-person party marker.
		op9Body = buildCurrentSceneOp9ActorDisplayBodyInContext(
			objectKey,
			character,
			hasCharacter,
			charName,
			partyOwner,
		)
		op9Kind = currentSceneOp9ActorDisplayKind
		bodySource = "current_exe_sub_1D64CA0_kind0_active_party_registration"
	}
	s.logPacketEvent("game-initial-town-op9-party-state-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"owner_channel", partyOwner,
		"party_id", partyState.PartyID,
		"record_kind", op9Kind,
		"body_len", len(op9Body),
		"body_source", bodySource)
	if err := s.sendCurrentInitialTownActorDisplayAndPlacementLocked(session, source, op9Body); err != nil {
		return err
	}

	if session.initialTownRouteCharacterID == charID {
		switch {
		case session.initialTownRouteStage == currentInitialTownRouteActorBound:
			session.initialTownRouteStage = currentInitialTownRoutePlayerStatePrepared
		case session.initialTownRouteStage >= currentInitialTownRouteTransitionSent &&
			session.initialTownRouteStage < currentInitialTownRoutePlayerStateSent:
			if err := s.commitPreparedInitialTownSkillInfoAfterTransition(
				session,
				source+"_after_early_typed_op24",
			); err != nil {
				return err
			}
			session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
			session.townSceneReadyCharacterID = charID
		}
	}
	stage = session.initialTownRouteStage
	s.logGameEvent(session, "game-initial-town-player-state-finished",
		"source", source,
		"char_id", charID,
		"object_key", objectKey,
		"route_stage", stage,
		"next_owner", "typed_op24_final_transition_commit")
	return nil
}

func buildCurrentInitialTownOp9Body(
	session *gameSession,
	objectKey uint16,
	character dnfrepo.CharacterRecord,
	hasCharacter bool,
	characterName string,
) []byte {
	partyOwner := currentPartySceneOwnerContext(session)
	if runtimePartyStateSnapshot(session).PartyID > 0 {
		return buildCurrentSceneOp9ActorDisplayBodyInContext(
			objectKey,
			character,
			hasCharacter,
			characterName,
			partyOwner,
		)
	}
	return buildCurrentSceneOp9ActorRemovalBodyInContext(objectKey, partyOwner)
}

func (s *Service) sendCurrentInitialTownActorDisplayAndPlacementLocked(
	session *gameSession,
	source string,
	op9Body []byte,
) error {
	if session == nil || len(op9Body) == 0 {
		return nil
	}
	if !session.initialTownActorDisplaySent {
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRecoverStamina), op9Body, 0); err != nil {
			return err
		}
		session.initialTownActorDisplaySent = true
	}
	if !session.initialTownActorPlacementSent {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketRequestBlacklist),
			buildCurrentSceneActorPlacementBody(),
			0,
		); err != nil {
			return err
		}
		session.initialTownActorPlacementSent = true
	}
	s.logGameEvent(session, "game-initial-town-actor-display-placement-committed",
		"source", source,
		"char_id", session.selectedCharacterID,
		"op9_sent", session.initialTownActorDisplaySent,
		"op120_sent", session.initialTownActorPlacementSent)
	return nil
}

// sendCurrentInitialTownClearQuestListAfterLocationLocked sends the same
// repository-backed op356 snapshot used by the old bootstrap, but at the
// current client's proven pre-op24 location boundary.
func (s *Service) sendCurrentInitialTownClearQuestListAfterLocationLocked(
	session *gameSession,
	source string,
) error {
	if session == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	body, err := s.buildCurrentClearQuestListTransportBodyForSession(ctx, session, source)
	if err != nil {
		return err
	}
	return s.sendGameUpperFixed16Transport(
		session,
		currentClearQuestListMsgID,
		body,
		0,
		1,
		true,
		currentClearQuestListTransportCodec,
	)
}
