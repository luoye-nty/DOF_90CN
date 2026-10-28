package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	dnfcharacter "longheng.io/server/internal/modules/dnf/character"
	dnfcharacterdata "longheng.io/server/internal/modules/dnf/characterdata"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnffriend "longheng.io/server/internal/modules/dnf/unitedfriend"
)

const (
	currentDeleteCharacterDatabaseError = byte(2)
	currentDeleteCharacterMissingError  = byte(21)
	// currentDeleteCharacterRefusedError is the op6 refusal that is neither
	// "not found" nor "database broken". The current EXE's class-1 op6 handler
	// sub_1CFEB20 shows a dedicated text for it (failure branch 0x1CFED90 ->
	// text 0x2B5, one of the 0x2B1..0x2B5 delete-error family), and 86JP sends
	// the same 0x28 when it declines to delete a character it still considers
	// in use. Reusing the missing/database codes here would tell the player
	// their character is gone or the server is broken.
	currentDeleteCharacterRefusedError = byte(40)
)

var errCurrentCharacterDeleteGuardUnavailable = errors.New("character delete guard repository is unavailable")

// currentCharacterDeleteSameDayBlocked reports whether a character may not be
// deleted yet because it was created inside the current service day.
//
// The boundary is premium.ServiceDayID - the 06:00 Beijing reset every other
// daily counter on this server already uses - so "当天创建" means the same
// service day the rest of the game counts, not a rolling 24 hours. A record
// with no creation timestamp is never blocked: characters written before the
// column was populated must stay deletable.
func currentCharacterDeleteSameDayBlocked(createdAt, now time.Time) bool {
	if createdAt.IsZero() {
		return false
	}
	return premium.ServiceDayID(createdAt.UTC()) == premium.ServiceDayID(now.UTC())
}

func (s *Service) handleDeleteCharacter(session *gameSession, body []byte) error {
	s.logGameEvent(session, "game-character-delete-request",
		"body_len", len(body),
		"body_hex", fmt.Sprintf("%x", body))
	request, err := dnfcharacter.DecodeDeleteCharacterRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-character-delete-parse-failed", "body_len", len(body), "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterMissingError)
	}
	deleteName, err := decodeCharacterName([]byte(request.Name))
	if err != nil {
		s.logGameEvent(session, "game-character-delete-name-decode-failed",
			"slot", request.Slot,
			"name_byte_len", len(request.Name),
			"error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterMissingError)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		s.logGameEvent(session, "game-character-delete-repository-missing", "slot", request.Slot)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterDatabaseError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	owner, err := dnfcharacterdata.NewRosterOwner(repositories)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterDatabaseError)
	}
	accountID := s.accountIDForSession(session)
	slotLimit, err := s.accountCharacterSlotLimit(ctx, repositories, accountID)
	if err != nil || int(request.Slot) >= slotLimit {
		s.logGameEvent(session, "game-character-delete-slot-out-of-range",
			"account_id", accountID,
			"slot", request.Slot,
			"slot_limit", slotLimit,
			"error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterMissingError)
	}
	resolvedSlot, err := resolveCurrentDeleteCharacterSlot(ctx, repositories.Character, accountID, int(request.Slot), deleteName, slotLimit)
	if err == nil {
		blocked, target, blockErr := s.currentCharacterDeleteSameDayGuard(ctx, repositories, accountID, resolvedSlot, deleteName)
		if blockErr != nil {
			s.logGameEvent(session, "game-character-delete-same-day-check-failed",
				"account_id", accountID,
				"resolved_slot", resolvedSlot,
				"error", blockErr)
			return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterDatabaseError)
		}
		if blocked {
			s.logGameEvent(session, "game-character-delete-blocked-same-service-day",
				"account_id", accountID,
				"character_id", target.CharacterID,
				"resolved_slot", resolvedSlot,
				"created_at", target.CreatedAt.UTC(),
				"service_day_id", premium.ServiceDayID(target.CreatedAt.UTC()),
				"code", currentDeleteCharacterRefusedError,
				"reason", "character_created_in_the_current_service_day")
			return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterRefusedError)
		}
		err = owner.DeleteCharacter(ctx, accountID, resolvedSlot, deleteName)
	}
	if err != nil {
		code := currentDeleteCharacterDatabaseError
		if errors.Is(err, dnfrepo.ErrCharacterDeleteNotFound) ||
			errors.Is(err, dnfrepo.ErrCharacterDeleteNameMismatch) ||
			errors.Is(err, dnfrepo.ErrCharacterSlotMissing) {
			code = currentDeleteCharacterMissingError
		}
		s.logGameEvent(session, "game-character-delete-rejected",
			"account_id", accountID,
			"request_slot", request.Slot,
			"resolved_slot", resolvedSlot,
			"name_byte_len", len(request.Name),
			"code", code,
			"error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), code)
	}
	var friendObservers []string
	if repositories.UnitedFriend != nil {
		friendOwner, friendErr := dnffriend.NewOwner(repositories, nil)
		if friendErr == nil {
			s.friendMu.Lock()
			friendObservers, friendErr = friendOwner.DeleteCharacter(ctx, deleteName)
			s.friendMu.Unlock()
		}
		if friendErr != nil {
			s.logGameEvent(session, "game-character-delete-friend-cleanup-failed", "name", deleteName, "error", friendErr)
		}
	}
	characters, err := s.listCharacters(ctx, repositories, accountID)
	if err != nil {
		s.logGameEvent(session, "game-character-delete-refresh-list-failed", "account_id", accountID, "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDeleteCharacter), currentDeleteCharacterDatabaseError)
	}
	session.selectorAdventureInfoPending = len(characters) > 0
	session.selectorAdventureInfoSent = false
	if len(characters) > 0 {
		session.selectorAdventureInfoSlot = uint16(characters[0].Slot)
	} else {
		session.selectorAdventureInfoSlot = 0
	}
	response := make([]byte, 3)
	response[0] = 0
	binary.LittleEndian.PutUint16(response[1:], request.Slot)
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketDeleteCharacter), response); err != nil {
		return err
	}
	for _, observerName := range friendObservers {
		observerID, found, findErr := repositories.Character.FindIDByName(ctx, observerName)
		if findErr != nil || !found {
			continue
		}
		observerSession, online := s.onlineGameSession(uint16(numericCharacterStat(observerID)))
		if !online || observerSession == nil {
			continue
		}
		_ = s.sendGameUpperRawClassCodec(observerSession, currentFriendListOpcode, buildCurrentUnitedFriendDeleteBody(s.currentRegionID(), deleteName), 0, true)
		_ = s.sendCurrentUnitedFriendList(observerSession, observerName, "deleted_friend_character_cleanup")
	}
	s.logGameEvent(session, "game-character-delete-committed",
		"account_id", accountID,
		"request_slot", request.Slot,
		"resolved_slot", resolvedSlot,
		"remaining", len(characters))
	return nil
}

// currentCharacterDeleteSameDayGuard resolves the record the delete is about
// to remove and reports whether it was created inside the current service day.
// A record the account no longer owns is left to the ordinary delete path, so
// the not-found and name-mismatch refusals keep their own error codes.
func (s *Service) currentCharacterDeleteSameDayGuard(
	ctx context.Context,
	repositories dnfrepo.Group,
	accountID string,
	slot int,
	name string,
) (bool, dnfrepo.CharacterRecord, error) {
	if s == nil || repositories.Character == nil {
		return false, dnfrepo.CharacterRecord{}, errCurrentCharacterDeleteGuardUnavailable
	}
	records, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.DefaultCharacterSlotLimit)
	if err != nil {
		return false, dnfrepo.CharacterRecord{}, err
	}
	name = strings.TrimSpace(name)
	for _, record := range records {
		if record.Slot != slot || strings.TrimSpace(record.Name) != name {
			continue
		}
		return currentCharacterDeleteSameDayBlocked(record.CreatedAt, s.gameplayNow()), record, nil
	}
	return false, dnfrepo.CharacterRecord{}, nil
}

// resolveCurrentDeleteCharacterSlot tolerates the selector's stale slot key
// after a prior successful deletion. The name remains authoritative.
func resolveCurrentDeleteCharacterSlot(ctx context.Context, repository dnfrepo.CharacterRepository, accountID string, requestSlot int, requestName string, slotLimit ...int) (int, error) {
	limit := dnfrepo.DefaultCharacterSlotLimit
	if len(slotLimit) > 0 {
		limit = normalizedCharacterSlotLimit(slotLimit[0])
	}
	records, err := repository.ListByAccount(ctx, accountID, limit)
	if err != nil {
		return 0, err
	}
	requestName = strings.TrimSpace(requestName)
	requestSlotFound := false
	matchedSlot := -1
	matchedCount := 0
	for _, record := range records {
		recordName := strings.TrimSpace(record.Name)
		if record.Slot == requestSlot {
			requestSlotFound = true
			if recordName == requestName {
				return requestSlot, nil
			}
		}
		if recordName == requestName {
			matchedSlot = record.Slot
			matchedCount++
		}
	}
	if matchedCount == 1 {
		return matchedSlot, nil
	}
	if requestSlotFound || matchedCount > 1 {
		return 0, dnfrepo.ErrCharacterDeleteNameMismatch
	}
	return 0, dnfrepo.ErrCharacterDeleteNotFound
}
