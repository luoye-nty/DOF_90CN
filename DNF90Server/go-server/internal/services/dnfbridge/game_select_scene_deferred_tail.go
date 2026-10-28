package dnfbridge

import (
	"context"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type currentDeferredSelectSceneTailPostStage byte

const (
	currentDeferredSelectSceneTailPackets currentDeferredSelectSceneTailPostStage = iota
	currentDeferredSelectSceneTailFinishGate
	currentDeferredSelectSceneTailCreature
	currentDeferredSelectSceneTailPremium
	currentDeferredSelectSceneTailCrystal
	currentDeferredSelectSceneTailRental
	currentDeferredSelectSceneTailMailbox
	currentDeferredSelectSceneTailCarryGold
	currentDeferredSelectSceneTailDamageFont
	currentDeferredSelectSceneTailRepresentName
	currentDeferredSelectSceneTailEvents
	currentDeferredSelectSceneTailSeriaLuck
	currentDeferredSelectSceneTailCharacterOptions
	currentDeferredSelectSceneTailFriends
	currentDeferredSelectSceneTailComplete
)

func resetCurrentDeferredSelectSceneTailProgress(session *gameSession) {
	if session == nil {
		return
	}
	session.sceneBootstrapActorGeneration++
	if session.sceneBootstrapActorGeneration == 0 {
		session.sceneBootstrapActorGeneration++
	}
	session.sceneBootstrapTailPacketIndex = 0
	session.sceneBootstrapTailObjectMode1Pending = false
	session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailPackets
	session.sceneBootstrapFriendListPacketIndex = 0
	session.skillSwitchInventoryAllDataCharacterID = 0
	session.skillSwitchInventoryAllDataGeneration = 0
}

// sendCurrentSelectSceneReadyBoundary is the common actor-tail boundary for the
// DPROTO callback, CheckUserConnection, and legacy type1345 routes.
func (s *Service) sendCurrentSelectSceneReadyBoundary(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	if err := s.sendDeferredSelectSceneTail(session, source); err != nil {
		return err
	}
	if session.sceneBootstrapTailDeferred || !session.sceneBootstrapTailSent {
		return s.sendInitialSkillSwitchInventoryAllDataAfterDprotoFallback(session, source)
	}
	if err := s.sendSelectedDarkKnightComboAfterFinalTownSceneOnce(
		session,
		source+"_after_final_deferred_mode1",
	); err != nil {
		return err
	}
	session.townMu.Lock()
	legacyHUDReady := session.initialTownLegacySceneReadyAccepted
	session.townMu.Unlock()
	if !legacyHUDReady {
		return nil
	}
	if err := s.sendCurrentGameOptionAfterLegacySceneReadyOnce(
		session,
		source+"_game_option_after_hud_ready_tail",
	); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	err := s.sendInitialSkillSwitchInventoryAllData(
		ctx,
		session,
		source+"_after_mode1_and_legacy_scene_ready",
	)
	cancel()
	if err != nil {
		return err
	}
	// A return-to-selector keeps the account inventory singleton alive even
	// though the selected-character bootstrap resets its containers. The full
	// pre-op24 list seeds storage, then this proved HUD-ready boundary rebinds
	// only the twelve account-owned crystal/soul rows to their surviving widgets.
	if err := s.sendCurrentAccountSharedInventoryTownUIReadyRefresh(
		session,
		source+"_account_shared_inventory_after_hud_ready",
	); err != nil {
		return err
	}
	if err := s.sendInitialTownAdventureOverheadRefresh(session, source+"_adventure_overhead_hud"); err != nil {
		return err
	}
	if err := s.sendInitialTownCombatPowerAffixes(session, source+"_combat_power_hud"); err != nil {
		return err
	}
	if err := s.sendInitialTownMoodProjection(session, source+"_mood"); err != nil {
		return err
	}
	return s.sendInitialTownActorUserStateAfterHUDReadyOnce(
		session,
		source+"_user_state_after_hud_ready_tail",
	)
}

// sendInitialSkillSwitchInventoryAllDataAfterDprotoFallback covers the live
// current-client route that completes the selected actor but never emits the
// older type1345/u32(2) acknowledgement.  It deliberately does not complete
// the deferred tail: only the established BUFF all-data projection is safe at
// this later DPROTO callback.  A later proved type1345 still owns the tail and
// is harmless because sendInitialSkillSwitchInventoryAllData is generation
// gated after this write succeeds.
func (s *Service) sendInitialSkillSwitchInventoryAllDataAfterDprotoFallback(session *gameSession, source string) error {
	if s == nil || session == nil || !strings.HasPrefix(source, "upper_dproto_") {
		return nil
	}

	session.townMu.Lock()
	ready := session.initialTownRouteCharacterID != 0 &&
		session.initialTownRouteCharacterID == session.selectedCharacterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent &&
		session.sceneBootstrapTailDeferred &&
		!session.sceneBootstrapTailSent &&
		!session.initialTownLegacySceneReadyAccepted
	session.townMu.Unlock()
	if !ready {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	err := s.sendInitialSkillSwitchInventoryAllData(
		ctx,
		session,
		source+"_after_mode1_and_op24_without_legacy_type1345",
	)
	cancel()
	return err
}

// sendInitialTownAdventureOverheadRefresh replays the authoritative account
// model only after the legacy type1345 HUD-ready boundary. The first op1340 /
// op1346 pair is necessarily sent before mode1 during actor construction; on
// the current EXE that early pair populates its internal adventure model but
// can be ignored by the already-created overhead-name widget. This one replay
// reaches the same native data path after the selected actor and HUD exist.
func (s *Service) sendInitialTownAdventureOverheadRefresh(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}
	session.townMu.Lock()
	defer session.townMu.Unlock()
	if session.initialTownAdventureOverheadRefreshSent ||
		session.initialTownRouteCharacterID == 0 ||
		session.initialTownRouteCharacterID != session.selectedCharacterID ||
		session.initialTownRouteStage < currentInitialTownRoutePlayerStateSent {
		return nil
	}
	objectKey := session.selectedCharacterID
	if err := s.sendCurrentAdventureInfoPushFromAccount(session, objectKey, source+"_op1340"); err != nil {
		return err
	}
	if err := s.sendCurrentAdventureActorRefreshFromAccount(session, objectKey, source+"_op1346"); err != nil {
		return err
	}
	name, _, _, err := s.currentRepresentAccountIdentity(context.Background(), session)
	if err != nil {
		s.logGameEvent(session, "game-adventure-overhead-name-replay-skipped",
			"source", source,
			"reason", "represent_account_identity_unavailable",
			"error", err)
	} else if strings.TrimSpace(name) != "" {
		if err := s.sendRepresentAccountNameState(session, name, false, source+"_op1371"); err != nil {
			return err
		}
	}
	session.initialTownAdventureOverheadRefreshSent = true
	return nil
}

// sendCurrentLegacyTownSceneReadyBoundary consumes the current NoPack's proved
// type1345/u32(2) callback after the first typed op24. This callback does not
// own another actor/finish-loading generation: live traffic proved that
// replaying mode0/mode1/op105/op37/op30 here leaves the client update loop with
// a null scene actor. Complete only the already-owned deferred tail. Skills
// were installed at the accepted pre-op24 boundary. The selected-town route
// deliberately does not send mode3 here: on the current EXE that packet is
// the user-initiated personal-information panel route.
func (s *Service) sendCurrentLegacyTownSceneReadyBoundary(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.townMu.Lock()
	if session.initialTownRouteCharacterID == 0 ||
		session.initialTownRouteCharacterID != session.selectedCharacterID ||
		session.initialTownRouteStage < currentInitialTownRoutePlayerStateSent {
		session.townMu.Unlock()
		return nil
	}
	session.initialTownLegacySceneReadyAccepted = true
	session.townMu.Unlock()

	if err := s.sendCurrentSelectSceneReadyBoundary(session, source); err != nil {
		return err
	}
	if err := s.sendCurrentQuestHUDReadyRefresh(session, source+"_after_confirmed_actor_and_hud"); err != nil {
		return err
	}
	session.initialTownMode3RefreshPending = false
	return nil
}

func (s *Service) sendInitialTownCombatPowerAffixes(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.townMu.Lock()
	defer session.townMu.Unlock()
	if session.initialTownCombatPowerAffixesSent ||
		session.initialTownRouteStage < currentInitialTownRouteTransitionSent {
		return nil
	}
	sent, err := s.sendSelectedCurrentCombatPowerAffixProjection(session, source)
	if err != nil {
		return err
	}
	if sent {
		session.initialTownCombatPowerAffixesSent = true
	}
	return nil
}

func (s *Service) sendInitialTownMoodProjection(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}
	session.townMu.Lock()
	if session.initialTownMoodProjectionSent || session.initialTownRouteStage < currentInitialTownRouteTransitionSent {
		session.townMu.Unlock()
		return nil
	}
	session.townMu.Unlock()
	moodValue, found, err := s.selectedCurrentMoodValue(session)
	if err != nil {
		s.logGameEvent(session, "game-mood-private-projection-deferred", "source", source, "char_id", session.selectedCharacterID, "reason", "character_load_failed", "error", err)
		return nil
	}
	if found && moodValue != 0 {
		if err := s.sendCurrentMoodProjection(session, moodValue, source); err != nil {
			return err
		}
	}
	session.townMu.Lock()
	session.initialTownMoodProjectionSent = true
	session.townMu.Unlock()
	return nil
}

func currentInitialTownWaitsForLegacySceneReady(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.townMu.Lock()
	waiting := session.initialTownRouteCharacterID != 0 &&
		session.initialTownRouteCharacterID == session.selectedCharacterID &&
		session.initialTownRouteStage >= currentInitialTownRouteTransitionSent &&
		!session.initialTownLegacySceneReadyAccepted
	session.townMu.Unlock()
	return waiting
}

// currentDeferredSceneTailWaitFields describes the state the scene tail is
// waiting in.
//
// A bare "the tail was deferred" line (160k of them in the measured 11 GB log)
// cannot answer which gate refused it: the two reasons are a client callback that
// never came and a route whose flags were reset under it, and the difference is
// entirely in these fields. They go into the packet log and the scene-loading
// diagnostic record (records/scene-*.jsonl), which is not a raid category: the
// tail deferral is unrelated to raids and at several lines per second it used to
// drown the real raid events and rotate the raid files out of retention.
func currentDeferredSceneTailWaitFields(session *gameSession) map[string]any {
	if session == nil {
		return map[string]any{}
	}
	session.townMu.Lock()
	fields := map[string]any{
		"route_char_id":            session.initialTownRouteCharacterID,
		"route_stage":              int(session.initialTownRouteStage),
		"legacy_ready_accepted":    session.initialTownLegacySceneReadyAccepted,
		"town_scene_ready_char_id": session.townSceneReadyCharacterID,
		"return_select_pending":    session.returnSelectTownReentryPending,
		"back_to_village_pending":  session.backToVillageEnterSelectPending,
	}
	session.townMu.Unlock()
	fields["tail_deferred"] = session.sceneBootstrapTailDeferred
	fields["tail_sent"] = session.sceneBootstrapTailSent
	fields["tail_packet_index"] = session.sceneBootstrapTailPacketIndex
	fields["tail_post_stage"] = int(session.sceneBootstrapTailPostStage)
	fields["tail_object_mode1_pending"] = session.sceneBootstrapTailObjectMode1Pending
	fields["post_start_map_player_state_sent"] = session.postStartMapPlayerStateSent
	fields["tail_actor_generation"] = session.sceneBootstrapActorGeneration
	return fields
}

// logDeferredSceneTail records one refused tail publication with the wait state
// attached, in both the packet log and the scene-loading diagnostic record
// (records/scene-*.jsonl; it used to go into the raid record, see recordScene).
func (s *Service) logDeferredSceneTail(session *gameSession, source string, reason string) {
	s.logDeferredSceneTailFields(session, source, "", reason)
}

// logDeferredSceneTailWithActorSource keeps the actor-readiness source visible on
// the one gate that used to report it, while still emitting the full wait state.
func (s *Service) logDeferredSceneTailWithActorSource(
	session *gameSession,
	source string,
	actorReadySource string,
	reason string,
) {
	s.logDeferredSceneTailFields(session, source, actorReadySource, reason)
}

func (s *Service) logDeferredSceneTailFields(
	session *gameSession,
	source string,
	actorReadySource string,
	reason string,
) {
	if s == nil || session == nil {
		return
	}
	fields := currentDeferredSceneTailWaitFields(session)
	args := make([]any, 0, 2*len(fields)+6)
	args = append(args,
		"source", source,
		"char_id", session.selectedCharacterID,
		"reason", reason)
	if actorReadySource != "" {
		args = append(args, "actor_ready_source", actorReadySource)
	}
	for _, key := range sortedMapKeys(fields) {
		args = append(args, key, fields[key])
	}
	s.logGameEvent(session, "game-upper-select-character-scene-tail-deferred", args...)
	fields["source"] = source
	fields["defer_reason"] = reason
	fields["char_id"] = session.selectedCharacterID
	fields["result"] = "deferred"
	if actorReadySource != "" {
		fields["actor_ready_source"] = actorReadySource
	}
	// 为什么搬去 scene 类别：这是选人选场景/进图时的引导包诊断，与团本无关；
	// 它每秒能产生好几条，写进 raid 会把真正的团本事件淹没，还会把 raid-*.jsonl
	// 顶到轮转上限、把团本记录挤出"每类保留最近 14 个文件"的保留窗口。
	// 上面的 logGameEvent（packet_log）保持不变：高负载下它会丢约 25% 的行，
	// 所以这份记录必须一条不少地留着，只是换个文件而已。
	s.recordScene(session, "scene_tail_deferred", fields)
}

// sortedMapKeys keeps the log lines byte-stable for the same state, so two
// deferrals that differ only in a field value are comparable by eye.
func sortedMapKeys(fields map[string]any) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *Service) sendDeferredSelectSceneTail(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.sceneBootstrapTailMu.Lock()
	defer session.sceneBootstrapTailMu.Unlock()
	if !session.sceneBootstrapTailDeferred {
		return nil
	}
	if currentInitialTownWaitsForLegacySceneReady(session) {
		s.logDeferredSceneTail(session, source, "wait_for_current_exe_legacy_type1345_u32_2_scene_ready")
		return nil
	}
	transitionReady, transitionReadySource := s.currentSceneTransitionReadyForState(session)
	if !transitionReady {
		if err := s.resumeCurrentInitialTownRouteAfterSelectHeartbeat(session, source); err != nil {
			return err
		}
		if currentInitialTownWaitsForLegacySceneReady(session) {
			s.logDeferredSceneTail(session, source, "initial_town_transition_committed_wait_for_current_exe_legacy_type1345_u32_2")
			return nil
		}
		transitionReady, transitionReadySource = s.currentSceneTransitionReadyForState(session)
	}
	if !transitionReady {
		s.logDeferredSceneTail(session, source, "wait_for_selected_actor_and_scene_transition_owner")
		return nil
	}
	if transitionReadySource == "initial_town_actor_bound_and_transition_sent" {
		if err := s.sendCurrentInitialTownPlayerState(session, source+"_after_town_transition"); err != nil {
			return err
		}
	}
	actorReady, actorReadySource := s.currentSceneActorReadyForState(session)
	if !actorReady {
		s.logDeferredSceneTailWithActorSource(session, source, actorReadySource, "wait_for_full_player_state_and_scene_finalizer")
		return nil
	}
	if session.sceneBootstrapTailSent {
		s.logGameEvent(session, "game-upper-select-character-scene-tail-duplicate-skipped",
			"source", source,
			"char_id", session.selectedCharacterID)
		return nil
	}
	packets := deferredSelectSceneTailPackets()
	s.logGameEvent(session, "game-upper-select-character-scene-tail-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"actor_ready_source", actorReadySource,
		"packet_count", len(packets),
		"resume_packet_index", session.sceneBootstrapTailPacketIndex,
		"resume_object_mode1_pending", session.sceneBootstrapTailObjectMode1Pending,
		"resume_post_stage", session.sceneBootstrapTailPostStage)
	// 同上：引导包诊断改记 scene 类别，不占团本记录的文件轮转窗口。
	s.recordScene(session, "scene_tail_deferred", map[string]any{
		"source":                source,
		"char_id":               session.selectedCharacterID,
		"actor_ready_source":    actorReadySource,
		"result":                "sent",
		"packet_count":          len(packets),
		"resume_packet_index":   session.sceneBootstrapTailPacketIndex,
		"resume_post_stage":     int(session.sceneBootstrapTailPostStage),
		"tail_actor_generation": session.sceneBootstrapActorGeneration,
		"defer_reason":          "",
	})
	for session.sceneBootstrapTailPacketIndex < len(packets) {
		packetIndex := session.sceneBootstrapTailPacketIndex
		packet := packets[packetIndex]
		if packet.kind == csharpCurrentSceneObjectListKind &&
			!session.sceneBootstrapTailObjectMode1Pending &&
			session.currentSceneObjectListSent {
			s.logGameEvent(session, "game-upper-select-character-scene-tail-object-duplicate-skipped",
				"source", source,
				"char_id", session.selectedCharacterID,
				"packet_index", packetIndex,
				"reason", "initial_town_or_return_route_already_committed_current_object_list")
			session.sceneBootstrapTailPacketIndex++
			continue
		}
		body := packet.body
		if packet.kind == csharpCurrentSceneObjectListKind &&
			!session.sceneBootstrapTailObjectMode1Pending {
			ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			var err error
			body, err = s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
				ctx,
				session,
				0,
				"",
				dnfrepo.CharacterRecord{},
				false,
				currentTownActorOwnerContext(session),
			)
			cancel()
			if err != nil {
				return err
			}
		}
		if !session.sceneBootstrapTailObjectMode1Pending {
			if err := s.sendCSharpSelectInitPacket(session, packet, body); err != nil {
				return err
			}
			if packet.kind == csharpCurrentSceneObjectListKind {
				// The object packet is already accepted. If the paired mode1
				// write fails, resume at mode1 instead of replaying mode0.
				session.sceneBootstrapTailObjectMode1Pending = true
			} else {
				session.sceneBootstrapTailPacketIndex++
				continue
			}
		}
		if packet.kind == csharpCurrentSceneObjectListKind {
			if err := s.sendSelectedSceneUserInfo23Mode1Only(session, source+"_after_current_object_list"); err != nil {
				return err
			}
			session.sceneBootstrapTailObjectMode1Pending = false
		}
		session.sceneBootstrapTailPacketIndex++
	}
	if session.sceneBootstrapTailPostStage < currentDeferredSelectSceneTailFinishGate {
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailFinishGate
	}
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailFinishGate {
		if actorReadySource != "initial_town_player_state_finalized" {
			if err := s.sendUpperFinishLoadingGate(session, source); err != nil {
				return err
			}
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailCreature
	}
	// Do not publish skills from the generic tail. type1345 owns one explicit
	// retained op19 write only after this complete tail has committed; generic
	// CheckUserConnection/DPROTO callbacks must not consume that HUD boundary.
	// The actor-bound pre-mode1 stage already installs the complete current
	// item-list family. Replaying list 0/1/2/7/38 here rebuilds the same client
	// containers after op24 and can overwrite scene-local item state. Keep
	// scene-ready projections narrowly owned by their dedicated packets below.
	// The select-character ACK folds Devil Contract services into type 58, but
	// it cannot initialize the eight independent service deadlines consumed by
	// the coupon box and the Cera-shop checkout UI. Publish their authoritative
	// op903 state once after the town actor and HUD are ready.
	// Durable aura expansion is already projected by selected-user subtype0's
	// repository-backed aura_flag. Do not emit unsolicited op863 here: its
	// native result handler is request/UI-owned and opens the avatar panel.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailCreature {
		if err := s.sendSelectedCreatureSceneReadyProjection(session, source+"_scene_ready_creature"); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailPremium
	}
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailPremium {
		// serviceType 1 is the variant that applies the body to the contract
		// singleton the coupon box and Cera checkout read. It also consumes the
		// client's one-shot "show the contract panel" flag, which is the intended
		// login-time popup; every later send stays silent (see
		// currentPremiumServiceTypeForSession).
		if err := s.sendCurrentPremiumServiceState(session, source+"_after_scene_actor_commit", premium.ServiceTypePanel); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailCrystal
	}
	// Do not send op898 from the scene tail. The actor-bound player-state path
	// owns the cold-login class0 notification after every inventory container
	// has been installed; the first client-authored op36 is only an idempotent
	// fallback for routes that bypass that bootstrap. Sending from this earlier
	// tail would again require the rejected ABI-unsafe deferred call into
	// sub_1E7F810 and can corrupt adjacent UI state.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailCrystal {
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailRental
	}
	// The select-character container bootstrap does not carry rental points.
	// Publish the absolute wallet once, after the scene actor is committed.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailRental {
		if _, err := s.sendSelectedCurrentRentalPointState(session, source+"_after_scene_actor_commit", true); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailMailbox
	}
	// class0/op63 is the current client's mailbox-system bootstrap. It is a
	// one-WORD unread-count projection, not an unsolicited op97 mailbox list,
	// and runs only after the selected actor and UI-safe tail are complete.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailMailbox {
		if err := s.sendMailboxAlarmForSession(
			session,
			session.selectedCharacterID,
			source+"_after_scene_actor_commit",
		); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailCarryGold
	}
	// The gold-carry cap stage owns its own resume point: without it the client
	// falls back to stage 0 after a relog and would offer an already-purchased
	// upgrade again. A failed write therefore neither replays the mailbox alarm
	// nor skips the damage-font projection.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailCarryGold {
		if err := s.sendCurrentCarryGoldStage(
			session,
			source+"_after_scene_actor_commit",
		); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailDamageFont
	}
	// Damage-font ownership is independent of the inventory bootstrap. The
	// current client installs it through a class-0 NOTI 1239 projection once
	// the selected actor and other scene-owned stores are ready.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailDamageFont {
		if err := s.sendSelectedDamageFontState(session, source+"_after_scene_actor_commit"); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailRepresentName
	}
	// This is the first point at which the selected town scene has received the
	// complete bootstrap tail. First-time adventure-group registration must not
	// interrupt the role selector or any earlier actor/state initialization.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailRepresentName {
		if err := s.sendRepresentAccountNameRegistrationAfterScene(session, source+"_after_scene_init_complete"); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailEvents
	}
	// The activity calendar base rows and cumulative online/joust/Seria-luck
	// state are published together by the final compressed op108 descriptor.
	// Calendar rows remain base-only records; only the three live event families
	// are included in op108's active-state map.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailEvents {
		// 公会贡献币 earned before the last logout are delivered here, once the
		// scene-init event tail has committed. Upstream places the flush in exactly
		// this slot, ahead of the spend-time state.
		if err := s.flushCurrentGuildContribution(session); err != nil {
			return err
		}
		if err := s.sendCurrentSpendTimeInitialStateOnce(session, source+"_after_scene_init_events"); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailSeriaLuck
	}
	// BOOSTER_GAGE is the persisted left-top HUD gauge, not the random-box
	// result panel. Send it only after every selected-scene initialization
	// packet has committed; absent or invalid durable state is intentionally
	// skipped by the helper instead of being projected as a synthetic zero.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailSeriaLuck {
		if err := s.sendInitialSeriaLuckGauge(session, source+"_after_scene_init_complete"); err != nil {
			return err
		}
		if err := s.sendInitialTownMoodProjection(session, source+"_mood"); err != nil {
			return err
		}
		if err := s.sendCollectboxSceneInitializationStates(context.Background(), session); err != nil {
			s.logGameEvent(session, "game-collectbox-scene-init-status-deferred", "char_id", session.selectedCharacterID, "source", source, "reason", err)
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailCharacterOptions
	}
	// op390 rebuilds the current actor's option collections. Keep it inside the
	// retryable tail after the selected scene and local actor have replaced any
	// previous channel state; a failed write therefore resumes at op390 without
	// replaying the already committed prefix.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailCharacterOptions {
		if err := s.sendCurrentCharacterOptionSnapshot(session, source+"_after_scene_init_complete"); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailFriends
	}
	// The stock eight-zero op273 bootstrap only clears the UI. Publish the
	// durable one-way list after the final actor generation is bound; the
	// duplicate full snapshot is intentional because this EXE's first mode-0
	// pass creates hidden rows and the second pass marks existing rows visible.
	if session.sceneBootstrapTailPostStage == currentDeferredSelectSceneTailFriends {
		if err := s.sendCurrentUnitedFriendListResumable(session, "", source+"_after_scene_init_complete", &session.sceneBootstrapFriendListPacketIndex); err != nil {
			return err
		}
		session.sceneBootstrapTailPostStage = currentDeferredSelectSceneTailComplete
	}
	session.sceneBootstrapTailSent = true
	session.sceneBootstrapTailDeferred = false
	// The blacklist answer was held back while this tail was pending (a row the client
	// builds before the tail commits loses its 黑名单日期). Flush it here so a session
	// whose client never asks again still gets its list; a failure keeps the pending
	// flag set, and the retryable tail replays this point.
	if session.blacklistListAnswerPending {
		if err := s.sendCurrentBlacklistListAnswer(session, source+"_after_scene_tail", nil); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-select-scene-tail-active-finished",
		"source", source,
		"char_id", session.selectedCharacterID,
		"packet_count", len(packets)+1)
	return nil
}

func (s *Service) sendCurrentCargoPadResetBeforeItemLists(session *gameSession, source string) error {
	body := buildCurrentCancelCargoPadTransportBody()
	s.logPacketEvent("game-upper-current-cargo-pad-reset-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketCancelCargoPad),
		"classification", 0,
		"header_marker", 1,
		"body_encoded", true,
		"body_codec", currentCancelCargoPadTransportCodec,
		"expanded_body_len", currentCancelCargoPadHeaderSize+currentCancelCargoPadSlotCount*4,
		"slot_count", currentCancelCargoPadSlotCount,
		"protected_tail_len", currentCancelCargoPadProtectedTailSize,
		"state_source", "current_exe_protected_cargo_pad_reset_before_repository_item_lists")
	return s.sendCurrentProtectedClass0Packet(
		session,
		uint16(dnfenum.CmdPacketCancelCargoPad),
		body,
		currentCancelCargoPadTransportCodec,
		"initial_town_cargo_pad_reset_current_protected_state",
	)
}
