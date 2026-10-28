package dnfbridge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentUseCharacterRenameCardBodySize = 2
	currentCharacterRenameMaxNameBytes    = 30
	currentCharacterRenameMinimumLevel    = 30
	// currentCharacterRenameCooldown is the operator-confirmed retail rule: after a
	// rename the character cannot be renamed again for 90 days. The other half of
	// the rule is currentCharacterRenameReservation below - the *old name* stays
	// reserved for 180 days so nobody else can take it.
	//
	// It was temporarily lifted on 2026-09-23 while the arbitrary-card flow was
	// being verified (a character renamed minutes earlier could not be used to test
	// a second card) and is restored here with the rest of the flow.
	currentCharacterRenameCooldown    = 90 * 24 * time.Hour
	currentCharacterRenameReservation = 180 * 24 * time.Hour

	currentCharacterRenameEntitlementStat = "character_rename_entitlement_count"
	currentCharacterRenameLastUnixStat    = "character_rename_last_unix"
	// currentCharacterRenamePendingFlag is the value the current client reads
	// in the two roster bytes between the character-name DSTR and job/grow/level.
	// The 86-generation reference server (S4A12
	// Network/Builders/Init/AccountCharacterListBodyBuilder) writes this same
	// byte twice for a character whose rename card is in effect, and the
	// selection screen then renders that card as waiting for a rename instead
	// of letting the player enter the game.
	currentCharacterRenamePendingFlag     byte = 0x01
	currentCharacterRenameReservationKey       = "character_rename_reservation:"
	currentCharacterRenameRosterMarker         = "stdnf"
	currentCharacterRenameRosterWideUnits      = 12

	currentCharacterRenameFailureNameTooLong byte = 1
	currentCharacterRenameFailureDatabase    byte = 2
	currentCharacterRenameFailureUnavailable byte = 7
	currentCharacterRenameFailureInvalidName byte = 159
)

var (
	errCurrentCharacterRenameMalformed       = errors.New("character rename request is malformed")
	errCurrentCharacterRenameNameTooLong     = errors.New("character rename name is too long")
	errCurrentCharacterRenameNameInvalid     = errors.New("character rename name is invalid")
	errCurrentCharacterRenameOwnerInvalid    = errors.New("character rename owner is invalid")
	errCurrentCharacterRenameCardInvalid     = errors.New("character rename card is invalid")
	errCurrentCharacterRenameEntitlement     = errors.New("character rename entitlement is unavailable")
	errCurrentCharacterRenameLevel           = errors.New("character rename level is too low")
	errCurrentCharacterRenameCooldownActive  = errors.New("character rename cooldown is active")
	errCurrentCharacterRenameDuplicate       = errors.New("character rename target already exists")
	errCurrentCharacterRenameReserved        = errors.New("character rename target is reserved")
	errCurrentCharacterRenameOldNameMismatch = errors.New("character rename current name does not match")
)

type currentUseCharacterRenameCardRequest struct {
	Slot int16
}

type currentCharacterRenameRequest struct {
	Selector byte
	OldRaw   []byte
	NewRaw   []byte
	State    byte
}

type currentUseCharacterRenameCardResult struct {
	Update currentItemListEntry
}

func parseCurrentUseCharacterRenameCard(body []byte) (currentUseCharacterRenameCardRequest, error) {
	if len(body) != currentUseCharacterRenameCardBodySize {
		return currentUseCharacterRenameCardRequest{}, errCurrentCharacterRenameMalformed
	}
	return currentUseCharacterRenameCardRequest{Slot: int16(binary.LittleEndian.Uint16(body))}, nil
}

func parseCurrentCharacterRename(body []byte) (currentCharacterRenameRequest, error) {
	if len(body) < 11 {
		return currentCharacterRenameRequest{}, errCurrentCharacterRenameMalformed
	}
	request := currentCharacterRenameRequest{Selector: body[0]}
	oldRaw, next, err := readCurrentCharacterRenameDSTR(body, 1)
	if err != nil {
		return currentCharacterRenameRequest{}, err
	}
	newRaw, next, err := readCurrentCharacterRenameDSTR(body, next)
	if err != nil {
		return currentCharacterRenameRequest{}, err
	}
	if next+1 != len(body) {
		return currentCharacterRenameRequest{}, errCurrentCharacterRenameMalformed
	}
	request.OldRaw = oldRaw
	request.NewRaw = newRaw
	request.State = body[next]
	return request, nil
}

func readCurrentCharacterRenameDSTR(body []byte, offset int) ([]byte, int, error) {
	if offset < 0 || offset+4 > len(body) {
		return nil, 0, errCurrentCharacterRenameMalformed
	}
	length := uint64(binary.LittleEndian.Uint32(body[offset : offset+4]))
	if length > currentCharacterRenameMaxNameBytes {
		return nil, 0, errCurrentCharacterRenameNameTooLong
	}
	start := offset + 4
	end64 := uint64(start) + length
	if length == 0 || end64 > uint64(len(body)) {
		return nil, 0, errCurrentCharacterRenameMalformed
	}
	end := int(end64)
	return append([]byte(nil), body[start:end]...), end, nil
}

func normalizeLegacyCharacterRenameBody(body []byte) []byte {
	if _, err := parseCurrentCharacterRename(body); err == nil {
		return body
	}
	const trailerSize = 4
	if len(body) <= trailerSize {
		return body
	}
	semantic := body[:len(body)-trailerSize]
	if _, err := parseCurrentCharacterRename(semantic); err != nil {
		return body
	}
	return append([]byte(nil), semantic...)
}

func validateCurrentCharacterRenameName(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errCurrentCharacterRenameNameInvalid
	}
	if len(raw) > currentCharacterRenameMaxNameBytes {
		return "", errCurrentCharacterRenameNameTooLong
	}
	name, err := decodeValidatedCharacterName(raw)
	if err != nil {
		return "", errCurrentCharacterRenameNameInvalid
	}
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(name))
	if len(encoded) > currentCharacterRenameMaxNameBytes {
		return "", errCurrentCharacterRenameNameTooLong
	}
	return name, nil
}

// currentCharacterRenamePending reports whether this character owns an unused
// rename entitlement: a rename card was bought (auto-use product) or used and
// the rename itself has not been committed yet. It is the roster projection's
// only source of truth for the pending-rename card state.
func currentCharacterRenamePending(character dnfrepo.CharacterRecord) bool {
	return character.Stats != nil && character.Stats[currentCharacterRenameEntitlementStat] > 0
}

func characterRenameReservationMetadataKey(name string) (string, error) {
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(name))
	if err != nil || len(encoded) == 0 || len(encoded) > currentCharacterRenameMaxNameBytes {
		return "", errCurrentCharacterRenameNameInvalid
	}
	return currentCharacterRenameReservationKey + hex.EncodeToString(encoded), nil
}

func (s *Service) handleCurrentUseCharacterRenameCard(session *gameSession, body []byte) error {
	request, err := parseCurrentUseCharacterRenameCard(body)
	if err != nil {
		s.logGameEvent(session, "game-character-rename-card-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseRenameCard), currentCharacterRenameFailureUnavailable)
	}
	catalog, err := s.currentCeraShopCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseRenameCard), currentCharacterRenameFailureDatabase)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CeraShopAssets == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseRenameCard), currentCharacterRenameFailureDatabase)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.ceraShopPurchaseMu.Lock()
	result, err := commitCurrentUseCharacterRenameCard(
		ctx, repositories, catalog,
		strings.TrimSpace(s.accountIDForSession(session)),
		selectedCharacterIDString(session), request, s.gameplayNow().UTC(),
	)
	s.ceraShopPurchaseMu.Unlock()
	if err != nil {
		code := currentCharacterRenameFailureDatabase
		if errors.Is(err, errCurrentCharacterRenameCardInvalid) ||
			errors.Is(err, errCurrentCharacterRenameOwnerInvalid) ||
			errors.Is(err, errCurrentCharacterRenameEntitlement) ||
			errors.Is(err, errCurrentCharacterRenameLevel) ||
			errors.Is(err, errCurrentCharacterRenameCooldownActive) {
			code = currentCharacterRenameFailureUnavailable
		}
		s.logGameEvent(session, "game-character-rename-card-rejected", "slot", request.Slot, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseRenameCard), code)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{result.Update}), 0); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketUseRenameCard), nil); err != nil {
		return err
	}
	s.logGameEvent(session, "game-character-rename-card-accepted",
		"slot", request.Slot,
		"sequence", "item_update_sent_ack_sent_then_server_owned_return_to_select")
	// The card is consumed and the pending-rename entitlement is stored; the client
	// does not return to the character-select screen on its own, so the server does
	// it here, exactly like the 86-generation reference server does after
	// USE_RENAME_CARD.
	if err := s.returnToCharacterSelectAfterRenameCard(session); err != nil {
		return err
	}
	return nil
}

func commitCurrentUseCharacterRenameCard(
	ctx context.Context,
	repositories dnfrepo.Group,
	catalog *pvfCeraShopCatalog,
	accountID string,
	characterID string,
	request currentUseCharacterRenameCardRequest,
	now time.Time,
) (currentUseCharacterRenameCardResult, error) {
	if repositories.CeraShopAssets == nil || catalog == nil || catalog.items == nil || accountID == "" || characterID == "" || request.Slot < 0 {
		return currentUseCharacterRenameCardResult{}, errCurrentCharacterRenameOwnerInvalid
	}
	var result currentUseCharacterRenameCardResult
	err := repositories.CeraShopAssets.WithinCeraShopAssets(
		ctx, accountID, characterID, dnfrepo.CharacterContainerStateScope(characterID),
		func(accounts dnfrepo.AccountRepository, _ dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository, _ dnfrepo.SettingsRepository) error {
			account, found, err := accounts.Load(ctx, accountID)
			if err != nil {
				return err
			}
			if !found {
				return errCurrentCharacterRenameOwnerInvalid
			}
			character, found, err := characters.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(character.AccountID) != accountID || strings.TrimSpace(account.AccountID) != accountID {
				return errCurrentCharacterRenameOwnerInvalid
			}
			if character.Level < currentCharacterRenameMinimumLevel {
				return errCurrentCharacterRenameLevel
			}
			if character.Stats != nil && character.Stats[currentCharacterRenameEntitlementStat] != 0 {
				return errCurrentCharacterRenameEntitlement
			}
			// Arming the pending rename. The 90-day cooldown is checked here as well
			// as on the commit path: a player inside the window must not even be
			// able to consume a card, because the card is gone either way.
			lastRename := int64(0)
			if character.Stats != nil {
				lastRename = character.Stats[currentCharacterRenameLastUnixStat]
			}
			if lastRename < 0 || lastRename > 0 && now.Unix() < lastRename+int64(currentCharacterRenameCooldown/time.Second) {
				return errCurrentCharacterRenameCooldownActive
			}
			inventory, found, err := inventories.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
				return errCurrentCharacterRenameOwnerInvalid
			}
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.Slot)
			stack, found := inventory.Slots[key]
			if !found || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count <= 0 {
				return errCurrentCharacterRenameCardInvalid
			}
			definition, err := catalog.items.ResolveItem(uint32(stack.ItemID))
			if err != nil {
				return fmt.Errorf("%w: item=%d: %v", errCurrentCharacterRenameCardInvalid, stack.ItemID, err)
			}
			if _, card := catalog.characterRenameCardKind(definition); !card {
				return fmt.Errorf("%w: item=%d path=%s", errCurrentCharacterRenameCardInvalid, stack.ItemID, definition.PVFPath)
			}
			stack.Count--
			if stack.Count == 0 {
				delete(inventory.Slots, key)
				result.Update.patchCore(request.Slot, math.MaxUint32, 0)
			} else {
				result.Update = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.Slot, stack)
				stack.RawEntry = append([]byte(nil), result.Update.data[:]...)
				inventory.Slots[key] = stack
			}
			if character.Stats == nil {
				character.Stats = make(map[string]int64, 4)
			}
			character.Stats[currentCharacterRenameEntitlementStat] = 1
			character.UpdatedAt = now
			inventory.UpdatedAt = now
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
			return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
		},
	)
	if err != nil {
		return currentUseCharacterRenameCardResult{}, err
	}
	return result, nil
}

func (s *Service) handleCurrentChangeCharacterName(session *gameSession, body []byte) error {
	request, err := parseCurrentCharacterRename(body)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeCharacName), currentCharacterRenameFailureCode(err))
	}
	oldName, err := validateCurrentCharacterRenameName(request.OldRaw)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeCharacName), currentCharacterRenameFailureCode(err))
	}
	newName, err := validateCurrentCharacterRenameName(request.NewRaw)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeCharacName), currentCharacterRenameFailureCode(err))
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CeraShopAssets == nil || repositories.Character == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeCharacName), currentCharacterRenameFailureDatabase)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	targetCharacterID, err := currentCharacterRenameTargetID(ctx, repositories.Character, accountID, request.Selector)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeCharacName), currentCharacterRenameFailureCode(err))
	}
	s.ceraShopPurchaseMu.Lock()
	err = commitCurrentCharacterRename(ctx, repositories, accountID, targetCharacterID, oldName, newName, s.currentRegionID(), s.gameplayNow().UTC())
	s.ceraShopPurchaseMu.Unlock()
	if err != nil {
		s.logGameEvent(session, "game-character-rename-rejected", "old_name", oldName, "new_name", newName, "selector", request.Selector, "state", request.State, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeCharacName), currentCharacterRenameFailureCode(err))
	}
	s.logGameEvent(session, "game-character-rename-committed", "old_name", oldName, "new_name", newName, "selector", request.Selector, "state", request.State)
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketChangeCharacName), nil)
}

func currentCharacterRenameTargetID(ctx context.Context, repository dnfrepo.CharacterRepository, accountID string, selector byte) (string, error) {
	if repository == nil || strings.TrimSpace(accountID) == "" {
		return "", errCurrentCharacterRenameOwnerInvalid
	}
	characters, err := repository.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return "", err
	}
	for _, character := range characters {
		if character.Slot == int(selector) && strings.TrimSpace(character.AccountID) == accountID && strings.TrimSpace(character.CharacterID) != "" {
			return strings.TrimSpace(character.CharacterID), nil
		}
	}
	return "", fmt.Errorf("%w: selector=%d", errCurrentCharacterRenameOwnerInvalid, selector)
}

func commitCurrentCharacterRename(ctx context.Context, repositories dnfrepo.Group, accountID, characterID, oldName, newName string, serverID byte, now time.Time) error {
	if repositories.CeraShopAssets == nil || accountID == "" || characterID == "" || oldName == "" || newName == "" {
		return errCurrentCharacterRenameOwnerInvalid
	}
	return repositories.CeraShopAssets.WithinCeraShopAssets(
		ctx, accountID, characterID, dnfrepo.CharacterContainerStateScope(characterID),
		func(accounts dnfrepo.AccountRepository, _ dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository, _ dnfrepo.SettingsRepository) error {
			account, found, err := accounts.Load(ctx, accountID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(account.AccountID) != accountID {
				return errCurrentCharacterRenameOwnerInvalid
			}
			character, found, err := characters.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(character.AccountID) != accountID {
				return errCurrentCharacterRenameOwnerInvalid
			}
			canonicalOldName := character.Name
			if oldName != canonicalOldName && oldName != currentCharacterRenameRosterName(character) {
				return errCurrentCharacterRenameOldNameMismatch
			}
			if character.Level < currentCharacterRenameMinimumLevel {
				return errCurrentCharacterRenameLevel
			}
			if character.Stats == nil || character.Stats[currentCharacterRenameEntitlementStat] <= 0 {
				return errCurrentCharacterRenameEntitlement
			}
			nowUnix := now.Unix()
			lastRename := character.Stats[currentCharacterRenameLastUnixStat]
			if lastRename > 0 && nowUnix < lastRename+int64(currentCharacterRenameCooldown/time.Second) {
				return errCurrentCharacterRenameCooldownActive
			}
			if existingID, duplicate, err := characters.FindIDByName(ctx, newName); err != nil {
				return err
			} else if duplicate {
				return fmt.Errorf("%w: character=%s", errCurrentCharacterRenameDuplicate, existingID)
			}
			newNameReservationKey, err := characterRenameReservationMetadataKey(newName)
			if err != nil {
				return err
			}
			if reservedUntil, parseErr := strconv.ParseInt(strings.TrimSpace(account.Metadata[newNameReservationKey]), 10, 64); parseErr == nil && reservedUntil > nowUnix {
				return errCurrentCharacterRenameReserved
			}
			oldNameReservationKey, err := characterRenameReservationMetadataKey(canonicalOldName)
			if err != nil {
				return err
			}
			if account.Metadata == nil {
				account.Metadata = make(map[string]string, 4)
			}
			account.Metadata[oldNameReservationKey] = strconv.FormatInt(now.Add(currentCharacterRenameReservation).Unix(), 10)
			// Record the rename for the 角色名称变更记录 window. It rides the
			// account record this transaction already saves, so a rolled-back
			// rename cannot leave a dangling record behind.
			appendCurrentCharacterRenameLog(&account, characterID, canonicalOldName, serverID, now)
			character.Name = newName
			character.Stats[currentCharacterRenameEntitlementStat] = 0
			character.Stats[currentCharacterRenameLastUnixStat] = nowUnix
			account.UpdatedAt = now
			character.UpdatedAt = now
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats)
		},
	)
}

func currentCharacterRenameFailureCode(err error) byte {
	switch {
	case errors.Is(err, errCurrentCharacterRenameNameTooLong):
		return currentCharacterRenameFailureNameTooLong
	case errors.Is(err, errCurrentCharacterRenameNameInvalid), errors.Is(err, errCurrentCharacterRenameMalformed):
		return currentCharacterRenameFailureInvalidName
	case errors.Is(err, errCurrentCharacterRenameDuplicate), errors.Is(err, errCurrentCharacterRenameReserved),
		errors.Is(err, errCurrentCharacterRenameOwnerInvalid), errors.Is(err, errCurrentCharacterRenameEntitlement),
		errors.Is(err, errCurrentCharacterRenameLevel), errors.Is(err, errCurrentCharacterRenameCooldownActive),
		errors.Is(err, errCurrentCharacterRenameOldNameMismatch):
		return currentCharacterRenameFailureUnavailable
	default:
		return currentCharacterRenameFailureDatabase
	}
}

func selectedCharacterIDString(session *gameSession) string {
	if session == nil || session.selectedCharacterID == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(session.selectedCharacterID), 10)
}
