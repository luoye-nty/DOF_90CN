package dnfbridge

import (
	"context"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The current EXE does not spend a dungeon HP/MP potion through op44
// USE_STACKABLE. It sends class-1/op18 DELETE_ITEM to remove the dose and then,
// a few milliseconds later, class-1/op38 USE_SKILL carrying a fixed skill
// number for the drink animation. op38 has no bridge owner beyond the local
// debug strike hook, so the dose was consumed and no recovery ever came back:
// the bar did not move. This pairs the two commands and answers the second one.
const (
	// currentPotionCastSkillID is the op38 skill number the client uses for a
	// consumable, big-endian in the first two body bytes.
	currentPotionCastSkillID uint16 = 24

	// currentPotionCastWindow bounds the gap between the op18 commit and the
	// op38 cast. The pair arrives milliseconds apart; the allowance is wide
	// enough to survive a stalled frame and short enough that an unrelated cast
	// cannot claim a stale delete.
	currentPotionCastWindow = 2 * time.Second

	// currentPotionRecoveryFlatOperator and currentPotionRecoveryPercentOperator
	// are the two operators the runtime PVF actually uses in a recovery section.
	currentPotionRecoveryFlatOperator    = "+"
	currentPotionRecoveryPercentOperator = "%"

	currentPotionHPMaxStatKey = "stat_hp_max"
	currentPotionMPMaxStatKey = "stat_mp_max"
)

// currentPotionCastState records the most recent committed dungeon consumable
// delete so the cast that follows it can be identified.
type currentPotionCastState struct {
	mu      sync.Mutex
	pending *currentPendingConsumableUse
}

type currentPendingConsumableUse struct {
	itemID      uint32
	listType    byte
	slotIndex   int16
	committedAt time.Time
}

// noteCurrentConsumableDeleteFromCommand records the first entry of a committed
// op18 request. A decode failure only skips the record: this runs after the
// delete has already been committed, and the potion chain must never be able to
// disturb the inventory path it observes.
func (s *Service) noteCurrentConsumableDeleteFromCommand(session *gameSession, body []byte) {
	parsed, err := dnfinventory.DecodeDeleteRequest(body)
	if err != nil {
		return
	}
	if len(parsed.Entries) > 0 {
		entry := parsed.Entries[0]
		s.noteCurrentConsumableDelete(session, parsed.ListType, entry.SlotIndex, int64(entry.ItemID))
		return
	}
	s.noteCurrentConsumableDelete(session, parsed.ListType, parsed.SlotIndex, 0)
}

func (s *Service) noteCurrentConsumableDelete(session *gameSession, listType byte, slotIndex int16, itemID int64) {
	if session == nil || itemID <= 0 {
		return
	}
	session.potionCast.mu.Lock()
	defer session.potionCast.mu.Unlock()
	session.potionCast.pending = &currentPendingConsumableUse{
		itemID:      uint32(itemID),
		listType:    listType,
		slotIndex:   slotIndex,
		committedAt: s.gameplayNow(),
	}
}

// takeCurrentConsumableDeleteIfFresh consumes the pending record. It is taken
// whether or not it is still fresh, so one delete can never answer two casts.
func (session *gameSession) takeCurrentConsumableDeleteIfFresh(now time.Time) *currentPendingConsumableUse {
	session.potionCast.mu.Lock()
	defer session.potionCast.mu.Unlock()
	pending := session.potionCast.pending
	session.potionCast.pending = nil
	if pending == nil || now.Sub(pending.committedAt) > currentPotionCastWindow {
		return nil
	}
	return pending
}

// handleCurrentDungeonPotionCast answers an op38 cast that follows a committed
// consumable delete. It reports whether it owned the cast; an ordinary skill
// cast is left to the existing dispatch, which has no server obligation for it.
func (s *Service) handleCurrentDungeonPotionCast(session *gameSession, body []byte) (bool, error) {
	if s == nil || session == nil {
		return false, nil
	}
	skillID, ok := parseCurrentDungeonUseSkillSkillID(body)
	if !ok || skillID != currentPotionCastSkillID {
		return false, nil
	}
	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if !inDungeon {
		return false, nil
	}
	pending := session.takeCurrentConsumableDeleteIfFresh(s.gameplayNow())
	if pending == nil {
		// Skill 24 outside the pairing window is not a potion. Stay silent and
		// let the ordinary dispatch have it.
		return false, nil
	}

	block := func(reason string, fields ...any) (bool, error) {
		fields = append([]any{
			"char_id", selectedCharacterIDString(session),
			"item_id", pending.itemID,
			"list_type", pending.listType,
			"slot", pending.slotIndex,
			"reason", reason,
		}, fields...)
		s.logGameEvent(session, "game-dungeon-potion-cast-blocked", fields...)
		return true, nil
	}

	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return block("runtime_pvf_catalog_unavailable", "error", err.Error())
	}
	definition, err := catalog.ResolveItem(pending.itemID)
	if err != nil {
		return block("item_definition_unresolved", "error", err.Error())
	}
	if !currentPotionWasteStackable(definition.StackableType) {
		return block("deleted_item_is_not_a_waste_consumable", "stackable_type", definition.StackableType, "pvf_path", definition.PVFPath)
	}
	if !definition.HasHPRecovery && !definition.HasMPRecovery {
		return block("deleted_consumable_declares_no_recovery", "pvf_path", definition.PVFPath)
	}

	character, found, err := s.loadCurrentPotionDrinker(session)
	if err != nil || !found {
		reason := "drinker_record_unavailable"
		if err != nil {
			return block(reason, "error", err.Error())
		}
		return block(reason)
	}

	hpAmount, hpErr := currentPotionRecoveryAmount(
		definition.HasHPRecovery, definition.HPRecoveryOperator, definition.HPRecoveryAmount,
		character, currentPotionHPMaxStatKey)
	mpAmount, mpErr := currentPotionRecoveryAmount(
		definition.HasMPRecovery, definition.MPRecoveryOperator, definition.MPRecoveryAmount,
		character, currentPotionMPMaxStatKey)
	if hpErr != "" || mpErr != "" {
		reason := hpErr
		if reason == "" {
			reason = mpErr
		}
		return block(reason,
			"pvf_path", definition.PVFPath,
			"hp_operator", definition.HPRecoveryOperator, "hp_amount", definition.HPRecoveryAmount,
			"mp_operator", definition.MPRecoveryOperator, "mp_amount", definition.MPRecoveryAmount)
	}
	if hpAmount == 0 && mpAmount == 0 {
		return block("recovery_resolved_to_zero", "pvf_path", definition.PVFPath)
	}

	objectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	if err := s.sendCurrentDungeonPotionRecovery(session, objectKey, hpAmount, mpAmount); err != nil {
		return true, err
	}
	s.logGameEvent(session, "game-dungeon-potion-cast-applied",
		"char_id", selectedCharacterIDString(session),
		"object_key", objectKey,
		"item_id", pending.itemID,
		"pvf_path", definition.PVFPath,
		"hp_operator", definition.HPRecoveryOperator,
		"hp_declared", definition.HPRecoveryAmount,
		"hp_applied", hpAmount,
		"mp_operator", definition.MPRecoveryOperator,
		"mp_declared", definition.MPRecoveryAmount,
		"mp_applied", mpAmount,
		"msg_id", uint16(dnfenum.CmdPacketNotifyDieMonster),
		"response", "current_exe_op38_mode1_recovery")
	return true, nil
}

func (s *Service) loadCurrentPotionDrinker(session *gameSession) (dnfrepo.CharacterRecord, bool, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	return repositories.Character.Load(ctx, selectedCharacterIDString(session))
}

// currentPotionRecoveryAmount converts one declared recovery into the scalar the
// recovery frame carries. It returns a non-empty reason instead of guessing:
// a percentage with no known maximum on the character record would otherwise
// have to invent the drinker's HP pool.
func currentPotionRecoveryAmount(
	declared bool,
	operator string,
	amount int64,
	character dnfrepo.CharacterRecord,
	maxStatKey string,
) (uint16, string) {
	if !declared || amount <= 0 {
		// A non-positive amount is a damage or no-op declaration - the archive
		// has `% -7` on a [virtual] item - and never a heal.
		return 0, ""
	}
	switch strings.TrimSpace(operator) {
	case currentPotionRecoveryFlatOperator:
		return clampCurrentPotionAmount(amount), ""
	case currentPotionRecoveryPercentOperator:
		maximum, found := statInt64OK(character, maxStatKey)
		if !found || maximum <= 0 {
			return 0, "percent_recovery_without_known_maximum"
		}
		return clampCurrentPotionAmount(maximum * amount / 100), ""
	default:
		return 0, "unproven_recovery_operator"
	}
}

func clampCurrentPotionAmount(value int64) uint16 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint16(0)) {
		return ^uint16(0)
	}
	return uint16(value)
}

// sendCurrentDungeonPotionRecovery emits one mode-1 frame per non-zero amount.
// The frame carries a single scalar, so a consumable that restores both HP and
// MP needs two.
func (s *Service) sendCurrentDungeonPotionRecovery(session *gameSession, objectKey uint16, hpAmount, mpAmount uint16) error {
	for _, amount := range [...]uint16{hpAmount, mpAmount} {
		if amount == 0 {
			continue
		}
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketNotifyDieMonster),
			buildCurrentDungeonPotionRecoveryBody(objectKey, amount),
			0,
		); err != nil {
			return err
		}
	}
	return nil
}

// buildCurrentDungeonPotionRecoveryBody writes the mode-1 branch of the same
// class-0/op38 reader that buildCurrentDungeonDeathNotificationBody feeds with
// mode 0: target key, zero drops, no optional u16, mode, the 0xff
// "no progression update" sentinel and no optional room, then the mode-1
// payload of target key and scalar.
func buildCurrentDungeonPotionRecoveryBody(targetObjectKey uint16, amount uint16) []byte {
	var writer packetWriter
	writer.writeUint16(targetObjectKey)
	writer.writeByte(0)
	writer.writeByte(0)
	writer.writeByte(1)
	writer.writeByte(0xff)
	writer.writeByte(0)
	writer.writeUint16(targetObjectKey)
	writer.writeUint16(amount)
	return writer.bytes()
}

// parseCurrentDungeonUseSkillSkillID reads the skill number from the fixed
// 12-byte op38 body. The size is the same one the local debug strike hook
// already proves as currentLocalDebugUseSkillRequestSize; the number is
// big-endian in the first two bytes.
func parseCurrentDungeonUseSkillSkillID(body []byte) (uint16, bool) {
	if len(body) != currentLocalDebugUseSkillRequestSize {
		return 0, false
	}
	return uint16(body[0])<<8 | uint16(body[1]), true
}

func currentPotionWasteStackable(stackableType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stackableType, "`", "")))
	return strings.HasPrefix(normalized, "[waste]")
}
