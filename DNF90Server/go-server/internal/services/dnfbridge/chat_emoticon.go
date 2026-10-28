package dnfbridge

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentChatEmoticonCatalogPath             = "chatemoticon/chatemoticon.lst"
	currentChatEmoticonOrderPath               = "chatemoticon/emoticonorderlist.etc"
	currentChatEmoticonConfigMsgID      uint16 = 411
	currentChatEmoticonSaveMsgID        uint16 = 472
	currentChatEmoticonRecentMsgID      uint16 = 1446
	currentChatEmoticonRecentBodySize          = 85
	currentChatEmoticonMaximumEntries          = 256
	currentChatEmoticonMaximumTextBytes        = 519
	currentChatEmoticonValueKey                = "configured_body_base64"
	currentChatEmoticonRecentValueKey          = "recent_body_base64"
)

var errCurrentChatEmoticonInvalid = errors.New("current chat emoticon state is invalid")

type currentChatEmoticonEntry struct {
	ID           uint16
	UnlockType   string
	UnlockValue  uint16
	DisplayOrder int
}

type currentChatEmoticonConfiguredRow struct {
	ID   uint16
	Text []byte
}

func currentChatEmoticonScope(accountID string, characterID uint16) string {
	return "account:" + strings.TrimSpace(accountID) + ":character:" + strconv.Itoa(int(characterID)) + ":chat_emoticon"
}

func parseCurrentChatEmoticonCatalog(source initialEquipmentTextSource) ([]currentChatEmoticonEntry, error) {
	if source == nil {
		return nil, errCurrentChatEmoticonInvalid
	}
	listText, err := source.ReadText(currentChatEmoticonCatalogPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", currentChatEmoticonCatalogPath, err)
	}
	listDocument, err := dnfpvf.Parse(currentChatEmoticonCatalogPath, listText)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", currentChatEmoticonCatalogPath, err)
	}
	order := make(map[uint16]int)
	if orderText, readErr := source.ReadText(currentChatEmoticonOrderPath); readErr == nil {
		if orderDocument, parseErr := dnfpvf.Parse(currentChatEmoticonOrderPath, orderText); parseErr == nil {
			for index, value := range orderDocument.Ints("order") {
				if value <= 0 || value > math.MaxUint16 {
					continue
				}
				id := uint16(value)
				if _, duplicate := order[id]; !duplicate {
					order[id] = index
				}
			}
		}
	}
	entries := make([]currentChatEmoticonEntry, 0)
	seen := make(map[uint16]struct{})
	for _, ref := range dnfpvf.ParseList(listDocument) {
		if ref.ID <= 0 || ref.ID > math.MaxUint16 {
			continue
		}
		id := uint16(ref.ID)
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate emoticon id=%d", errCurrentChatEmoticonInvalid, id)
		}
		seen[id] = struct{}{}
		refPath := strings.TrimSpace(strings.ReplaceAll(ref.Path, "\\", "/"))
		if !strings.Contains(refPath, "/") {
			refPath = path.Join(path.Dir(currentChatEmoticonCatalogPath), refPath)
		}
		text, err := source.ReadText(refPath)
		if err != nil {
			return nil, fmt.Errorf("read emoticon id=%d path=%s: %w", id, refPath, err)
		}
		document, err := dnfpvf.Parse(refPath, text)
		if err != nil {
			return nil, fmt.Errorf("parse emoticon id=%d path=%s: %w", id, refPath, err)
		}
		unlockType, found := document.Text("type")
		if !found {
			return nil, fmt.Errorf("%w: emoticon id=%d missing [type]", errCurrentChatEmoticonInvalid, id)
		}
		unlockType = strings.ToUpper(strings.Trim(strings.TrimSpace(unlockType), "[]"))
		if unlockType != "COMMON" && unlockType != "LEVEL" {
			continue
		}
		unlockValue := uint16(0)
		if unlockType == "LEVEL" {
			value, found := document.Int("type")
			if !found || value <= 0 || value > math.MaxUint16 {
				return nil, fmt.Errorf("%w: emoticon id=%d invalid level=%d", errCurrentChatEmoticonInvalid, id, value)
			}
			unlockValue = uint16(value)
		}
		displayOrder := int(id) + currentChatEmoticonMaximumEntries
		if value, found := order[id]; found {
			displayOrder = value
		}
		entries = append(entries, currentChatEmoticonEntry{ID: id, UnlockType: unlockType, UnlockValue: unlockValue, DisplayOrder: displayOrder})
	}
	if len(entries) == 0 || len(entries) > currentChatEmoticonMaximumEntries {
		return nil, fmt.Errorf("%w: unlocked catalog entries=%d", errCurrentChatEmoticonInvalid, len(entries))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DisplayOrder != entries[j].DisplayOrder {
			return entries[i].DisplayOrder < entries[j].DisplayOrder
		}
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

func (s *Service) currentChatEmoticons() ([]currentChatEmoticonEntry, error) {
	if s == nil {
		return nil, errCurrentChatEmoticonInvalid
	}
	s.chatEmoticonCatalogMu.Lock()
	defer s.chatEmoticonCatalogMu.Unlock()
	if s.chatEmoticonCatalog != nil || s.chatEmoticonCatalogLoadErr != nil {
		return append([]currentChatEmoticonEntry(nil), s.chatEmoticonCatalog...), s.chatEmoticonCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		s.chatEmoticonCatalog, err = parseCurrentChatEmoticonCatalog(archive)
	}
	s.chatEmoticonCatalogLoadErr = err
	return append([]currentChatEmoticonEntry(nil), s.chatEmoticonCatalog...), err
}

func parseCurrentChatEmoticonConfiguredBody(body []byte) ([]currentChatEmoticonConfiguredRow, bool) {
	if len(body) < 2 {
		return nil, false
	}
	count := int(binary.LittleEndian.Uint16(body[:2]))
	if count > currentChatEmoticonMaximumEntries {
		return nil, false
	}
	rows := make([]currentChatEmoticonConfiguredRow, 0, count)
	seen := make(map[uint16]struct{}, count)
	next := 2
	for index := 0; index < count; index++ {
		if next > len(body)-2 {
			return nil, false
		}
		id := binary.LittleEndian.Uint16(body[next : next+2])
		next += 2
		text, afterText, ok := readCurrentRawDstr(body, next)
		if !ok || id == 0 || len(text) > currentChatEmoticonMaximumTextBytes || bytesContainsZero(text) {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, false
		}
		seen[id] = struct{}{}
		rows = append(rows, currentChatEmoticonConfiguredRow{ID: id, Text: text})
		next = afterText
	}
	return rows, next == len(body)
}

func bytesContainsZero(value []byte) bool {
	for _, current := range value {
		if current == 0 {
			return true
		}
	}
	return false
}

func buildCurrentChatEmoticonConfigBody(entries []currentChatEmoticonEntry, level uint16, configured []currentChatEmoticonConfiguredRow) []byte {
	configuredByID := make(map[uint16][]byte, len(configured))
	for _, row := range configured {
		configuredByID[row.ID] = append([]byte(nil), row.Text...)
	}
	eligible := make([]currentChatEmoticonEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.UnlockType == "COMMON" || (entry.UnlockType == "LEVEL" && entry.UnlockValue <= level) {
			eligible = append(eligible, entry)
		}
	}
	var writer packetWriter
	writer.writeUint16(uint16(len(eligible)))
	for _, entry := range eligible {
		writer.writeUint16(entry.UnlockValue)
		writer.writeUint16(entry.ID)
		writer.writeRawDstr(configuredByID[entry.ID])
	}
	return writer.bytes()
}

func loadCurrentChatEmoticonConfiguredRows(ctx context.Context, repository dnfrepo.SettingsRepository, scope string) ([]currentChatEmoticonConfiguredRow, error) {
	if repository == nil || strings.TrimSpace(scope) == "" {
		return nil, nil
	}
	record, found, err := repository.Load(ctx, scope)
	if err != nil || !found {
		return nil, err
	}
	encoded := strings.TrimSpace(record.Values[currentChatEmoticonValueKey])
	if encoded == "" {
		return nil, nil
	}
	body, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, errCurrentChatEmoticonInvalid
	}
	rows, ok := parseCurrentChatEmoticonConfiguredBody(body)
	if !ok {
		return nil, errCurrentChatEmoticonInvalid
	}
	return rows, nil
}

func (s *Service) sendSelectedChatEmoticonCatalog(session *gameSession, source string) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || session.chatEmoticonCatalogSent {
		return false, nil
	}
	entries, err := s.currentChatEmoticons()
	if err != nil {
		s.logGameEvent(session, "game-chat-emoticon-catalog-skipped", "source", source, "char_id", session.selectedCharacterID, "reason", "pvf_catalog_unavailable", "error", err)
		return false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Settings == nil {
		s.logGameEvent(session, "game-chat-emoticon-catalog-skipped", "source", source, "char_id", session.selectedCharacterID, "reason", "repository_unavailable")
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || strings.TrimSpace(character.CharacterID) != characterID ||
		strings.TrimSpace(character.AccountID) != strings.TrimSpace(s.accountIDForSession(session)) {
		s.logGameEvent(session, "game-chat-emoticon-catalog-skipped", "source", source, "char_id", session.selectedCharacterID, "reason", "character_identity_unavailable", "found", found, "error", err)
		return false, nil
	}
	configured, err := loadCurrentChatEmoticonConfiguredRows(ctx, repositories.Settings, currentChatEmoticonScope(character.AccountID, session.selectedCharacterID))
	if err != nil {
		s.logGameEvent(session, "game-chat-emoticon-catalog-skipped", "source", source, "char_id", session.selectedCharacterID, "reason", "saved_configuration_unavailable", "error", err)
		return false, nil
	}
	level := character.Level
	if level < 0 {
		level = 0
	}
	if level > math.MaxUint16 {
		level = math.MaxUint16
	}
	body := buildCurrentChatEmoticonConfigBody(entries, uint16(level), configured)
	if len(body) < 2 || binary.LittleEndian.Uint16(body[:2]) == 0 {
		s.logGameEvent(session, "game-chat-emoticon-catalog-skipped", "source", source, "char_id", session.selectedCharacterID, "reason", "no_eligible_emoticons")
		return false, nil
	}
	if err := s.sendGameUpperSuccess(session, currentChatEmoticonConfigMsgID, body); err != nil {
		return false, err
	}
	session.chatEmoticonCatalogSent = true
	s.logGameEvent(session, "game-chat-emoticon-catalog-sent", "source", source, "char_id", session.selectedCharacterID, "eligible_count", binary.LittleEndian.Uint16(body[:2]), "configured_count", len(configured), "body_source", "current_exe_class1_op411_u16_count_u16_source_u16_emoticon_dstr")
	return true, nil
}

func (s *Service) handleCurrentChatEmoticonCommand(session *gameSession, typ uint16, body []byte) (bool, error) {
	if typ != currentChatEmoticonSaveMsgID && typ != currentChatEmoticonRecentMsgID {
		return false, nil
	}
	if session == nil || session.selectedCharacterID == 0 {
		return true, nil
	}
	valid := false
	switch typ {
	case currentChatEmoticonSaveMsgID:
		_, valid = parseCurrentChatEmoticonConfiguredBody(body)
	case currentChatEmoticonRecentMsgID:
		valid = len(body) == currentChatEmoticonRecentBodySize
	}
	if !valid {
		s.logGameEvent(session, "game-chat-emoticon-save-rejected", "type", typ, "body_len", len(body), "reason", "current_exe_exact_body_required")
		return true, s.sendGameUpperFailure(session, typ, currentTownChatGenericError)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Settings == nil {
		return true, s.sendGameUpperFailure(session, typ, currentTownChatGenericError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || strings.TrimSpace(character.AccountID) != strings.TrimSpace(s.accountIDForSession(session)) {
		return true, s.sendGameUpperFailure(session, typ, currentTownChatGenericError)
	}
	key := currentChatEmoticonValueKey
	if typ == currentChatEmoticonRecentMsgID {
		key = currentChatEmoticonRecentValueKey
	}
	if err := saveCurrentGameOptionScopedValue(ctx, repositories.Settings, currentChatEmoticonScope(character.AccountID, session.selectedCharacterID), key, body); err != nil {
		return true, s.sendGameUpperFailure(session, typ, currentTownChatGenericError)
	}
	s.logGameEvent(session, "game-chat-emoticon-save-committed", "type", typ, "char_id", session.selectedCharacterID, "body_len", len(body), "body_source", currentChatEmoticonSaveBodySource(typ))
	return true, s.sendGameUpperSuccess(session, typ, nil)
}

func currentChatEmoticonSaveBodySource(typ uint16) string {
	if typ == uint16(dnfenum.CmdPacketSaveRecentEmoticonList) {
		return "current_exe_op1446_fixed_85_byte_recent_list"
	}
	return "current_exe_op472_u16_count_rows_u16_emoticon_dstr"
}
