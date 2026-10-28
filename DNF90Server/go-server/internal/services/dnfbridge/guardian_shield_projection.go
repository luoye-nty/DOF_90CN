package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfguardianshield "longheng.io/server/internal/modules/dnf/guardianshield"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentGuardianShieldDeckNotificationMsgID uint16 = 580

func buildCurrentGuardianShieldDeckNotificationBody(deck dnfguardianshield.Deck) []byte {
	body := make([]byte, dnfguardianshield.DeckSlotCount*4)
	for index, itemID := range deck {
		binary.LittleEndian.PutUint32(body[index*4:index*4+4], itemID)
	}
	return body
}

// sendSelectedGuardianShieldDeckInfo publishes the current EXE's independent
// class0/op580 five-dword deck state. It is sent only after an actor generation
// is bound; it is not appended to any mode0/mode1/userinfo body.
func (s *Service) sendSelectedGuardianShieldDeckInfo(session *gameSession, source string) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	if session.initialTownGuardianShieldDeckSent {
		return false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return false, fmt.Errorf("guardian shield projection character repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return false, fmt.Errorf("load guardian shield projection character: %w", err)
	}
	if !found {
		return false, fmt.Errorf("guardian shield projection character %s not found", characterID)
	}
	if accountID := strings.TrimSpace(s.accountIDForSession(session)); accountID != "" &&
		strings.TrimSpace(character.AccountID) != accountID {
		return false, fmt.Errorf("guardian shield projection character owner mismatch")
	}
	growType, err := dnfguardianshield.EligibleGrowType(character)
	if errors.Is(err, dnfguardianshield.ErrCharacterIneligible) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	deck, err := dnfguardianshield.LoadDeck(character)
	if err != nil {
		return false, err
	}
	resolver, err := s.newCurrentGuardianShieldResolver(ctx)
	if err != nil {
		return false, err
	}
	var questRecord dnfrepo.QuestRecord
	questLoaded := false
	seen := make(map[uint32]int, dnfguardianshield.DeckSlotCount)
	for index, itemID := range deck {
		if itemID == 0 {
			continue
		}
		if previous, duplicate := seen[itemID]; duplicate {
			return false, fmt.Errorf("guardian shield projection duplicate item=%d slots=%d,%d", itemID, previous, index)
		}
		seen[itemID] = index
		resolution, resolveErr := resolver(ctx, itemID, growType)
		if resolveErr != nil {
			return false, fmt.Errorf("validate guardian shield projection slot=%d item=%d: %w", index, itemID, resolveErr)
		}
		if resolution.ItemID != itemID {
			return false, fmt.Errorf("validate guardian shield projection slot=%d item=%d resolved=%d", index, itemID, resolution.ItemID)
		}
		if resolution.UnlockKind == alignedcmd.GuardianShieldUnlockQuest && !questLoaded {
			if repositories.Quest == nil {
				return false, dnfguardianshield.ErrQuestRepositoryRequired
			}
			questRecord, found, err = repositories.Quest.Load(ctx, characterID)
			if err != nil {
				return false, fmt.Errorf("load guardian shield projection clear quests: %w", err)
			}
			if !found {
				questRecord = dnfrepo.QuestRecord{CharacterID: characterID}
			}
			if strings.TrimSpace(questRecord.CharacterID) != characterID {
				return false, fmt.Errorf("guardian shield projection quest owner mismatch")
			}
			questLoaded = true
		}
		if err := dnfguardianshield.ValidateUnlock(character, questRecord, resolution); err != nil {
			return false, fmt.Errorf("validate guardian shield projection slot=%d item=%d unlock: %w", index, itemID, err)
		}
	}
	body := buildCurrentGuardianShieldDeckNotificationBody(deck)
	s.logPacketEvent("game-upper-selected-guardian-shield-deck-send",
		"conn_id", session.connID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentGuardianShieldDeckNotificationMsgID,
		"classification", 0,
		"grow_type", growType,
		"body_len", len(body),
		"body_source", "current_exe_sub_1d81b40_five_u32_deck")
	if err := s.sendGameUpperRawClass(session, currentGuardianShieldDeckNotificationMsgID, body, 0); err != nil {
		return false, err
	}
	session.initialTownGuardianShieldDeckSent = true
	return true, nil
}

// sendSelectedGuardianShieldTownAppearanceRefreshOnce rehydrates the visible
// main shield after a town actor's equipment-bearing mode1 and op580 deck have
// been accepted. Slot 24 is outside the ordinary fourteen-row mode0 appearance
// table. The current client therefore needs its complete op342 equipment group
// followed by op357's same-state redraw. The per-generation gate makes a
// transport retry resume safely without replaying an already accepted redraw.
func (s *Service) sendSelectedGuardianShieldTownAppearanceRefreshOnce(session *gameSession, source string) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	if session.initialTownGuardianShieldAppearanceSent {
		return false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		return false, fmt.Errorf("guardian shield appearance equipment repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	record, found, err := repositories.Equipment.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil {
		return false, fmt.Errorf("load guardian shield appearance equipment: %w", err)
	}
	if !found {
		record = dnfrepo.EquipmentRecord{CharacterID: strconv.Itoa(int(session.selectedCharacterID))}
	}
	itemID, equipped := dnfguardianshield.MainProjectionItemID(record)
	if !equipped {
		session.initialTownGuardianShieldAppearanceSent = true
		return false, nil
	}
	if err := s.sendSelectedActorExpandedEquipmentRefresh(session, source+"_op342"); err != nil {
		return false, err
	}
	if err := s.sendSelectedActorUserStateRefresh(session, source+"_op357_after_op342"); err != nil {
		return false, err
	}
	session.initialTownGuardianShieldAppearanceSent = true
	s.logPacketEvent("game-upper-selected-guardian-shield-town-appearance-refreshed",
		"conn_id", session.connID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"actor_slot", dnfguardianshield.ActorSlot,
		"item_id", itemID,
		"sequence", "equipment_mode1_op580_op342_op357")
	return true, nil
}

// preSyncGuardianShieldMainProjection runs inside the select-character scene
// initialization sequence, before any mode0/list3 scene-object body is built.
// It guarantees the durable actor-slot-24 projection agrees with the deck's
// visible main shield:
//
//   - deck[0] is non-zero and the slot-24 row is missing or holds another item
//     (a legacy guardian whose deck was saved before the projection write
//     existed): re-persist the projection so the mode0 appearance table and
//     the shield warehouse render the worn shield.
//   - deck[0] is zero but a valid slot-24 projection survived (a legacy deck
//     reset that cleared deck[0] while leaving the equipment row): restore
//     deck[0] from the projection so the two sources agree again.
//
// Only guardians are considered; other professions never carry a
// guardian_shield_deck_projection row. Repair failures are logged and deferred
// instead of blocking the select flow.
func (s *Service) preSyncGuardianShieldMainProjection(
	ctx context.Context,
	session *gameSession,
	character dnfrepo.CharacterRecord,
	hasCharacter bool,
) error {
	if s == nil || session == nil || !hasCharacter {
		return nil
	}
	growType, err := dnfguardianshield.EligibleGrowType(character)
	if errors.Is(err, dnfguardianshield.ErrCharacterIneligible) {
		return nil
	}
	if err != nil {
		return err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Equipment == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	characterID := strings.TrimSpace(character.CharacterID)
	if characterID == "" {
		characterID = strconv.Itoa(int(session.selectedCharacterID))
	}
	if characterID == "" || characterID == "0" {
		return nil
	}
	deck, err := dnfguardianshield.LoadDeck(character)
	if err != nil {
		return err
	}
	mainItemID := deck[dnfguardianshield.MainDeckSlot]
	slotKey := strconv.Itoa(int(dnfguardianshield.ActorSlot))

	record, found, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("load guardian shield pre-sync equipment: %w", err)
	}
	if !found {
		record = dnfrepo.EquipmentRecord{CharacterID: characterID}
	}
	record = dnfrepo.CloneEquipment(record)
	if record.Entries == nil {
		record.Entries = make(map[string]dnfrepo.EquipmentEntry, 1)
	}

	if mainItemID > 0 {
		if projected, projectionOK := dnfguardianshield.MainProjectionItemID(record); projectionOK && projected == mainItemID {
			return nil
		}
		resolver, resolveErr := s.newCurrentGuardianShieldResolver(ctx)
		if resolveErr != nil {
			return resolveErr
		}
		resolution, resolveErr := resolver(ctx, mainItemID, growType)
		if resolveErr != nil {
			s.logGameEvent(session, "game-upper-select-guardian-shield-main-projection-resolve-deferred",
				"char_id", session.selectedCharacterID,
				"item_id", mainItemID,
				"error", resolveErr)
			return nil
		}
		if resolution.ItemID != mainItemID {
			s.logGameEvent(session, "game-upper-select-guardian-shield-main-projection-mismatch-deferred",
				"char_id", session.selectedCharacterID,
				"item_id", mainItemID,
				"resolved_item_id", resolution.ItemID)
			return nil
		}
		record.Entries[slotKey] = dnfguardianshield.BuildMainProjectionEntry(mainItemID, resolution)
		record.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveEquipmentFields(ctx, repositories.Equipment, record, dnfrepo.EquipmentFieldEntries); err != nil {
			return fmt.Errorf("save guardian shield main projection pre-sync: %w", err)
		}
		s.logGameEvent(session, "game-upper-guardian-shield-main-projection-presynced",
			"char_id", session.selectedCharacterID,
			"item_id", mainItemID,
			"source", "select_character_scene_entry_before_mode0")
		return nil
	}

	// Deck main slot is empty. Restore it when a valid slot-24 projection is
	// still present (legacy deck reset left the equipment row behind).
	projected, projectionOK := dnfguardianshield.MainProjectionItemID(record)
	if !projectionOK || projected == 0 {
		return nil
	}
	current, foundCharacter, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("load guardian shield pre-sync character: %w", err)
	}
	if !foundCharacter {
		return nil
	}
	current = dnfrepo.CloneCharacter(current)
	if current.Stats == nil {
		current.Stats = make(map[string]int64, 1)
	}
	deckKey := dnfguardianshield.DeckStatKey(dnfguardianshield.MainDeckSlot)
	if current.Stats[deckKey] == int64(projected) {
		return nil
	}
	current.Stats[deckKey] = int64(projected)
	current.UpdatedAt = time.Now().UTC()
	if err := dnfrepo.SaveCharacterFields(ctx, repositories.Character, current, dnfrepo.CharacterFieldStats); err != nil {
		return fmt.Errorf("restore guardian shield deck main from projection: %w", err)
	}
	s.logGameEvent(session, "game-upper-guardian-shield-deck-main-restored",
		"char_id", session.selectedCharacterID,
		"item_id", projected,
		"source", "select_character_scene_entry_slot24_survived")
	return nil
}
