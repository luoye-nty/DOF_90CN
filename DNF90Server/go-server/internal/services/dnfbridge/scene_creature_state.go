package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnppet "longheng.io/server/internal/modules/dnf/pet"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentCreatureStateTableMsgID uint16 = 105

// sendSelectedCreatureInitialStateAfterMode1 refreshes the existing slot-26
// object's durable item state, then installs the repository-backed creature
// table and equipped creature's growth state as soon as mode1 has created it.
func (s *Service) sendSelectedCreatureInitialStateAfterMode1(session *gameSession, source string) error {
	if err := s.sendSelectedEquippedEquipmentEffectRuneRefresh(session, source+"_equipment_effect_rune"); err != nil {
		return err
	}
	if err := s.sendSelectedEquippedCreatureItemRefresh(session, source+"_equipped_item"); err != nil {
		return err
	}
	if err := s.sendSelectedCreatureStateTable(session, source+"_state_table"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature := s.currentEquippedCreatureForCharacter(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	cancel()
	if !equippedCreature.valid() {
		return nil
	}
	return s.sendSelectedEquippedCreatureGrowthState(session, equippedCreature, source+"_equipped_growth")
}

// sendSelectedCreatureCompletedTownReturnStateAfterMode1 restores the dynamic
// creature object after a completed dungeon return. The return route reuses
// the existing actor key, so the client needs the same-template slot-26 op14
// before the absolute op105/op102 state can bind to the new town object.
//
// This path deliberately does not advance the pet growth clock. The dungeon
// runtime still owns that clock until the caller retires it after the complete
// town route has been committed.
func (s *Service) sendSelectedCreatureCompletedTownReturnStateAfterMode1(
	session *gameSession,
	source string,
) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("completed town-return creature state requires an active character")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature, err := s.currentEquippedCreatureForCharacterWithError(
		ctx,
		strconv.Itoa(int(session.selectedCharacterID)),
	)
	cancel()
	if err != nil {
		return err
	}
	// The rebuilt actor's weapons lost their persisted effect rune together
	// with the creature's card state; both live only in list-3 op14 rows.
	if err := s.sendSelectedEquippedEquipmentEffectRuneRefresh(session, source+"_equipment_effect_rune"); err != nil {
		return err
	}
	if !equippedCreature.valid() {
		s.logPacketEvent("game-completed-town-return-creature-state-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "no_valid_equipped_creature")
		return nil
	}
	if err := s.sendSelectedEquippedCreatureItemRefresh(
		session,
		source+"_op14_slot26",
	); err != nil {
		return err
	}
	if err := s.sendSelectedCreatureStateTableWithRefreshOptions(
		session,
		source+"_op105",
		true,
		false,
	); err != nil {
		return err
	}
	return s.sendSelectedEquippedCreatureGrowthState(
		session,
		equippedCreature,
		source+"_op102",
	)
}

// sendSelectedCreatureSceneReadyProjection is the deferred-tail fallback for a
// lifecycle that did not already publish the creature table after mode1. A
// normal login has already sent op105/op102 and therefore writes nothing here.
//
// Do not send class0/op2 from this bootstrap callback. The callback owns only
// the one-time creature table/growth projection. Runtime slot-26 changes let
// the native class1/op19 handler move the target-26 object, then append only
// the independent creature table/growth state.
func (s *Service) sendSelectedCreatureSceneReadyProjection(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("selected creature scene projection requires an active character")
	}
	if session.selectedCreatureStateTableSent {
		s.logPacketEvent("game-upper-current-creature-scene-ready-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "op105_and_op102_already_published_after_mode1_no_post_op24_actor_rebuild")
		return nil
	}
	if err := s.sendSelectedEquippedCreatureItemRefresh(session, source+"_equipped_item_fallback"); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature := s.currentEquippedCreatureForCharacter(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	cancel()
	if err := s.sendSelectedCreatureStateTable(session, source+"_state_table_fallback"); err != nil {
		return err
	}
	if !equippedCreature.valid() {
		return nil
	}
	return s.sendSelectedEquippedCreatureGrowthState(session, equippedCreature, source+"_growth_fallback")
}

// sendSelectedCreatureStateAfterMoveAck resolves pet state only after the
// class1/op19 handler has moved the selected actor's target-26 object. The
// absolute op105 table is idempotent for one scene lifecycle; op102 is sent
// only when a creature is currently equipped and therefore has a live target.
func (s *Service) sendSelectedCreatureStateAfterMoveAck(session *gameSession, source string) error {
	if err := s.sendSelectedEquippedCreatureItemRefresh(session, source+"_equipped_item"); err != nil {
		return err
	}
	if err := s.sendSelectedCreatureStateTableWithRefresh(session, source+"_state_table", true); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature := s.currentEquippedCreatureForCharacter(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	cancel()
	if !equippedCreature.valid() {
		return nil
	}
	return s.sendSelectedEquippedCreatureGrowthState(session, equippedCreature, source+"_growth")
}

// sendSelectedCreatureActorAppearanceRefreshInDungeon republishes the local
// dungeon actor after a worn-creature (slot 26/27-29) change.
//
// Town owns this on the already-bound town actor through an event-suppressed
// mode0 appearance refresh (PostActionRefreshSelectedActorAppearance). A live
// dungeon has no town actor generation to target - its local actor is the
// context-0 object created by the post-start-map mode1 - so the same
// event-suppressed mode0 is written in that scene's owner context. The body
// carries the actor's creature item/serial pair, and that pair is what actually
// detaches the pet layer: without this refresh an in-dungeon unequip keeps
// drawing the previous pet until the next full actor rebuild (the town return),
// which is exactly the reported symptom.
//
// Nothing else is added on this path. The dungeon generation's initialization
// packets (op19/op205/op24) and the creature table/growth projections keep
// their existing owners, no stat blob is written (mode0 carries appearance
// only), and a failed refresh never fails the already-committed item move.
func (s *Service) sendSelectedCreatureActorAppearanceRefreshInDungeon(session *gameSession, operation string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if !session.postStartMapPlayerStateSent {
		s.logGameEvent(session, "game-dungeon-creature-appearance-refresh-skipped",
			"conn_id", session.connID,
			"operation", operation,
			"char_id", session.selectedCharacterID,
			"reason", "dungeon_local_actor_not_bound")
		return nil
	}
	if _, err := s.sendSelectedActorMode0RefreshInContextWithBody(
		session,
		currentSceneObjectContext,
		operation,
		"dungeon_creature_move_existing_actor_mode0_event_suppressed",
		true,
	); err != nil {
		s.logWarn("dnfbridge deferred dungeon creature actor appearance",
			"conn_id", session.connID,
			"operation", operation,
			"char_id", session.selectedCharacterID,
			"error", err)
		return nil
	}
	s.logGameEvent(session, "game-dungeon-creature-appearance-refresh-sent",
		"conn_id", session.connID,
		"operation", operation,
		"char_id", session.selectedCharacterID,
		"owner_context", currentSceneObjectContext,
		"sequence", "town_equivalent_event_suppressed_mode0_in_dungeon_owner_context")
	return nil
}

// sendSelectedCreatureGrowthStateAfterFeed refreshes the absolute pet state
// without replaying an inventory/equipment op14 row. The current client
// applies the feed's remaining count from the op44 ACK, while a list-7 op14
// directly after that ACK can invalidate the native pet-panel object.
func (s *Service) sendSelectedCreatureGrowthStateAfterFeed(session *gameSession, source string) error {
	if err := s.sendSelectedCreatureStateTableWithRefresh(session, source+"_state_table", true); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature := s.currentEquippedCreatureForCharacter(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	cancel()
	if !equippedCreature.valid() {
		return nil
	}
	return s.sendSelectedEquippedCreatureGrowthState(session, equippedCreature, source+"_growth")
}

// sendSelectedCreatureDungeonStateAfterMode1 rehydrates the dynamic pet
// projection after dungeon mode1 recreates the local actor. Town login marks
// the absolute op105 table as sent for the character generation, but the
// dungeon scene owns a fresh client-side pet view and therefore requires an
// explicit refresh on every entry.
func (s *Service) sendSelectedCreatureDungeonStateAfterMode1(session *gameSession, source string) error {
	if err := s.sendSelectedEquippedEquipmentEffectRuneRefresh(session, source+"_equipment_effect_rune"); err != nil {
		return err
	}
	if err := s.sendSelectedEquippedCreatureItemRefresh(session, source+"_equipped_item"); err != nil {
		return err
	}
	if err := s.sendSelectedCreatureStateTableWithRefresh(session, source+"_state_table", true); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature := s.currentEquippedCreatureForCharacter(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	cancel()
	if !equippedCreature.valid() {
		return nil
	}
	return s.sendSelectedEquippedCreatureGrowthState(session, equippedCreature, source+"_equipped_growth")
}

// sendSelectedCreatureStateTable publishes the current NoPack raw class0/op105
// table only after the scene lifecycle has made the selected actor safe. It is
// deliberately idempotent within one selected-scene lifecycle: op19 itself
// still has only its proved relocation ACK.
func (s *Service) sendSelectedCreatureStateTable(session *gameSession, source string) error {
	return s.sendSelectedCreatureStateTableWithRefresh(session, source, false)
}

func (s *Service) sendSelectedCreatureStateTableWithRefresh(session *gameSession, source string, refresh bool) error {
	return s.sendSelectedCreatureStateTableWithRefreshOptions(session, source, refresh, true)
}

func (s *Service) sendSelectedCreatureStateTableWithRefreshOptions(session *gameSession, source string, refresh bool, updateGrowthClock bool) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("selected creature state requires an active character")
	}
	if session.selectedCreatureStateTableSent && !refresh {
		s.logPacketEvent("game-upper-current-creature-state-table-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", currentCreatureStateTableMsgID,
			"reason", "already_sent_for_selected_scene")
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Pet == nil {
		return fmt.Errorf("selected creature state repository is unavailable")
	}
	owner, err := dnppet.NewOwner(repositories)
	if err != nil {
		return fmt.Errorf("create selected creature state owner: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := owner.List(ctx, dnppet.ListCommand{SelectedCharacterID: session.selectedCharacterID})
	if err != nil {
		return fmt.Errorf("load selected creature state: %w", err)
	}
	wireEntries, projectedNameCount := s.currentCreatureStateEntriesForWire(result.Entries)
	body, err := dnppet.BuildCreatureListBody(wireEntries)
	if err != nil {
		return fmt.Errorf("build selected creature state: %w", err)
	}
	stateSummary := make([]string, 0, len(wireEntries))
	for _, entry := range wireEntries {
		key := strings.TrimSpace(entry.PetKey)
		if entry.CreatureKey != 0 {
			key = fmt.Sprintf("%d", entry.CreatureKey)
		}
		stateSummary = append(stateSummary, fmt.Sprintf(
			"%s:level=%d:exp=%d:satiety=%d",
			key,
			entry.Level,
			entry.Exp,
			entry.Satiety,
		))
	}
	sort.Strings(stateSummary)
	s.logPacketEvent("game-upper-current-creature-state-table-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentCreatureStateTableMsgID,
		"classification", 0,
		"refresh", refresh,
		"entry_count", result.EntryCount,
		"projected_name_count", projectedNameCount,
		"state_summary", strings.Join(stateSummary, ";"),
		"body_len", len(body),
		"body_source", "pet_repository_current_exe_sub_1D57AB0_typed_raw")
	if err := s.sendGameUpperRawClass(session, currentCreatureStateTableMsgID, body, 0); err != nil {
		return err
	}
	session.selectedCreatureStateTableSent = true
	if !updateGrowthClock {
		return nil
	}
	mode := currentPetGrowthClockTown
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	practice := currentDungeonRuntimeIsPractice(runtime)
	if runtime != nil && runtime.Session != nil && !practice {
		mode = currentPetGrowthClockDungeon
	}
	session.dungeon.mu.Unlock()
	if practice {
		s.stopCurrentPetGrowthClock(session, source+"_practice_zero_growth")
		return nil
	}
	if err := s.switchCurrentPetGrowthClock(session, mode, s.gameplayNow(), source+"_after_op105"); err != nil {
		s.logGameEvent(session, "game-pet-growth-clock-start-deferred",
			"char_id", session.selectedCharacterID,
			"mode", mode.String(),
			"source", source,
			"error", err)
	}
	return nil
}

// publishCurrentPetGrowthProjection sends the complete pet table before the
// equipped-pet growth packet. Both packets are absolute repository snapshots,
// so the pet page and the in-scene pet object converge immediately.
func (s *Service) publishCurrentPetGrowthProjection(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("current pet growth projection requires an active character")
	}
	if err := s.sendSelectedCreatureStateTableWithRefreshOptions(session, source+"_state_table", true, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	equippedCreature := s.currentEquippedCreatureForCharacter(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	cancel()
	if !equippedCreature.valid() {
		return nil
	}
	return s.sendSelectedEquippedCreatureGrowthState(session, equippedCreature, source+"_growth")
}

// currentCreatureStateEntriesForWire keeps durable custom names authoritative
// and repairs only empty legacy/imported names at the protocol boundary. PVF
// text is UTF-8 after archive decoding, while the current client consumes the
// op105 DSTR through its native Chinese code page, so the fallback is encoded
// as GB18030 before it is placed on the wire.
func (s *Service) currentCreatureStateEntriesForWire(entries []dnfrepo.PetEntry) ([]dnfrepo.PetEntry, int) {
	out := append([]dnfrepo.PetEntry(nil), entries...)
	projected := 0
	var catalog *dnppet.PVFCatalog
	catalogLoaded := false
	for index := range out {
		entry := &out[index]
		entry.NameRaw = append([]byte(nil), entry.NameRaw...)
		if len(entry.NameRaw) != 0 {
			continue
		}

		name := strings.TrimSpace(entry.Name)
		if name == "" {
			if !catalogLoaded {
				catalog, _ = s.currentPetPVFCatalog()
				catalogLoaded = true
			}
			if catalog != nil {
				if definition, err := catalog.ResolveCreature(entry.ItemID); err == nil {
					name = strings.TrimSpace(definition.Name)
				}
			}
		}
		if name == "" {
			continue
		}
		encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(name))
		if err != nil || len(encoded) == 0 || len(encoded) >= 30 {
			continue
		}
		entry.Name = name
		entry.NameRaw = encoded
		projected++
	}
	return out, projected
}

func (s *Service) sendSelectedEquippedCreatureGrowthState(
	session *gameSession,
	equippedCreature currentEquippedCreatureSnapshot,
	source string,
) error {
	if session == nil || session.selectedCharacterID == 0 || !equippedCreature.valid() {
		return fmt.Errorf("selected equipped creature growth requires an active creature")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Pet == nil {
		return fmt.Errorf("selected equipped creature growth repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	record, found, err := repositories.Pet.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil {
		return fmt.Errorf("load selected equipped creature growth: %w", err)
	}
	if !found {
		return fmt.Errorf("selected equipped creature growth record was not found")
	}
	entry, found := currentEquippedCreatureMetadataEntry(equippedCreature, record)
	if !found {
		return fmt.Errorf(
			"selected equipped creature growth entry was not found: item=%d serial=%d",
			equippedCreature.itemID,
			equippedCreature.serialOrHandle,
		)
	}
	body, err := dnppet.BuildCreatureGrowthBody(entry)
	if err != nil {
		return fmt.Errorf("build selected equipped creature growth: %w", err)
	}
	s.logPacketEvent("game-upper-current-equipped-creature-growth-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"creature_key", entry.CreatureKey,
		"item_id", entry.ItemID,
		"level", entry.Level,
		"experience", entry.Exp,
		"satiety", entry.Satiety,
		"msg_id", currentCreatureGrowthMsgID,
		"classification", 0,
		"body_len", len(body),
		"body_source", "current_exe_sub_1D5AF60_scene_op102_after_op105_target26_bind")
	return s.sendGameUpperRawClass(session, currentCreatureGrowthMsgID, body, 0)
}

// sendSelectedActorMode0AppearanceRefresh refreshes an already-bound scene
// actor in the same owner context that created it. It deliberately suppresses
// the selected-actor initialization event because the actor already exists.
func (s *Service) sendSelectedActorMode0AppearanceRefresh(session *gameSession, operation string, sequence string) error {
	return s.sendSelectedActorMode0Refresh(session, operation, sequence, true)
}

// sendSelectedActorMode0GenerationRefresh emits the initial mode0 row for a
// newly-created native actor generation. Its initialization event is required
// before the expert-job and skill managers are installed.
func (s *Service) sendSelectedActorMode0GenerationRefresh(session *gameSession, operation string, sequence string) error {
	_, err := s.sendSelectedActorMode0GenerationRefreshWithBody(session, operation, sequence)
	return err
}

func (s *Service) sendSelectedActorMode0Refresh(session *gameSession, operation string, sequence string, suppressActorStateEvent bool) error {
	_, err := s.sendSelectedActorMode0RefreshWithBody(session, operation, sequence, suppressActorStateEvent)
	return err
}

func (s *Service) sendSelectedActorMode0GenerationRefreshWithBody(session *gameSession, operation string, sequence string) ([]byte, error) {
	return s.sendSelectedActorMode0RefreshWithBody(session, operation, sequence, false)
}

func (s *Service) sendSelectedActorMode0RefreshWithBody(session *gameSession, operation string, sequence string, suppressActorStateEvent bool) ([]byte, error) {
	return s.sendSelectedActorMode0RefreshInContextWithBody(
		session,
		currentTownActorOwnerContext(session),
		operation,
		sequence,
		suppressActorStateEvent,
	)
}

// sendSelectedActorMode0RefreshInContextWithBody is the owner-explicit form of
// the existing-actor mode0 appearance refresh. Every town caller keeps
// currentTownActorOwnerContext through the wrapper above; a live dungeon owns
// its local actor in context 0 (created by the post-start-map mode1, not by the
// pre-op27 page), so a dungeon-side refresh must name that context explicitly
// instead of the town connection's channel.
func (s *Service) sendSelectedActorMode0RefreshInContextWithBody(session *gameSession, ownerChannel byte, operation string, sequence string, suppressActorStateEvent bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, charName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 || charID != session.selectedCharacterID || !hasCharacter {
		return nil, fmt.Errorf("selected actor mode0 appearance refresh could not load selected actor")
	}
	mode0Body, err := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		ctx,
		session,
		charID,
		charName,
		character,
		hasCharacter,
		ownerChannel,
	)
	if err != nil {
		return nil, err
	}
	if suppressActorStateEvent && !suppressCurrentSceneObjectActorStateEvent(mode0Body) {
		return nil, fmt.Errorf("selected actor mode0 appearance refresh could not suppress actor-state initialization event")
	}
	bodySource := "character_equipment_repository_new_actor_mode0"
	if suppressActorStateEvent {
		bodySource = "character_equipment_repository_existing_actor_mode0_event_suppressed"
	}
	s.logPacketEvent("game-aligned-command-post-action-send",
		"conn_id", session.connID,
		"operation", operation,
		"post_action", "refresh_selected_actor_appearance",
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketSetUDPIPPort),
		"classification", 0,
		"body_len", len(mode0Body),
		"body_source", bodySource,
		"owner_context", ownerChannel,
		"sequence", sequence)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode0Body, 0); err != nil {
		return nil, err
	}
	return append([]byte(nil), mode0Body...), nil
}

func (s *Service) sendSelectedActorAppearanceRefresh(session *gameSession, operation string, sequence string) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, charName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 || charID != session.selectedCharacterID || !hasCharacter {
		return fmt.Errorf("selected actor appearance refresh could not load selected actor")
	}
	ownerChannel := currentTownActorOwnerContext(session)
	mode0Body, err := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		ctx,
		session,
		charID,
		charName,
		character,
		hasCharacter,
		ownerChannel,
	)
	if err != nil {
		return err
	}
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	if repositories, ok := s.repositoryGroup(); ok {
		legacyRepo = repositories.LegacyUserInfo
	}
	mode1Body := s.buildCurrentSelectedUserInfoMode1BodyInContext(ctx, session, legacyRepo, character, hasCharacter, charID, ownerChannel)
	s.logPacketEvent("game-aligned-command-post-action-send",
		"conn_id", session.connID,
		"operation", operation,
		"post_action", "refresh_selected_actor_appearance",
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketSetUDPIPPort),
		"classification", 0,
		"body_len", len(mode0Body),
		"body_source", "character_equipment_repository_full_current_scene_object_mode0",
		"sequence", sequence)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode0Body, 0); err != nil {
		return err
	}
	s.logPacketEvent("game-aligned-command-post-action-send",
		"conn_id", session.connID,
		"operation", operation,
		"post_action", "rebind_selected_actor_equipment",
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketSetUDPIPPort),
		"classification", 0,
		"body_len", len(mode1Body),
		"body_source", "character_equipment_repository_full_current_scene_object_mode1",
		"object_key", currentSceneActorObjectKey(charID),
		"sequence", sequence)
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode1Body, 0)
}

// sendSelectedActorMode1AppearanceRefresh remains available for protocol paths
// that explicitly own a standalone current-EXE mode1. This helper remains for
// flows which explicitly require a full actor rebind.
func (s *Service) sendSelectedActorMode1AppearanceRefresh(session *gameSession, operation string, sequence string) error {
	_, err := s.sendSelectedActorMode1AppearanceRefreshWithBody(session, operation, sequence)
	return err
}

func (s *Service) sendSelectedActorMode1AppearanceRefreshWithBody(session *gameSession, operation string, sequence string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 || charID != session.selectedCharacterID || !hasCharacter {
		return nil, fmt.Errorf("selected actor mode1 appearance refresh could not load selected actor")
	}
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	if repositories, ok := s.repositoryGroup(); ok {
		legacyRepo = repositories.LegacyUserInfo
	}
	body := s.buildCurrentSelectedUserInfoMode1BodyInContext(
		ctx,
		session,
		legacyRepo,
		character,
		hasCharacter,
		charID,
		currentTownActorOwnerContext(session),
	)
	s.logPacketEvent("game-aligned-command-post-action-send",
		"conn_id", session.connID,
		"operation", operation,
		"post_action", "refresh_selected_actor_appearance",
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketSetUDPIPPort),
		"classification", 0,
		"body_len", len(body),
		"body_source", "character_equipment_repository_full_current_scene_object_mode1",
		"object_key", currentSceneActorObjectKey(charID),
		"sequence", sequence)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
		return nil, err
	}
	return append([]byte(nil), body...), nil
}
