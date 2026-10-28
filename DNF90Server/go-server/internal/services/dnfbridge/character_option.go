package dnfbridge

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var errCurrentCharacterOptionRepositoryUnavailable = errors.New("character option repository is unavailable")

const (
	currentCharacterOptionLengthBytes  = 4
	currentCharacterOptionPayloadBytes = dnfrepo.CharacterOptionSnapshotSize
	currentSaveCharacterOptionBodySize = currentCharacterOptionLengthBytes + currentCharacterOptionPayloadBytes
	currentCharacterOptionRestoreMsgID = uint16(390)

	// Releases before the op390 proof stored the complete 516-byte body in this
	// scope. Restore it once as a read-only fallback, then migrate the opaque
	// payload to the canonical repository snapshot without losing user data.
	legacyCharacterOptionValueKey = "body_base64"
)

func legacyCharacterOptionScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + ":character_option"
}

func validCurrentSaveCharacterOptionBody(body []byte) bool {
	return len(body) == currentSaveCharacterOptionBodySize &&
		binary.LittleEndian.Uint32(body[:currentCharacterOptionLengthBytes]) == currentCharacterOptionPayloadBytes
}

func (s *Service) handleCurrentSaveCharacterOption(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if !validCurrentSaveCharacterOptionBody(body) {
		s.logGameEvent(session, "game-save-character-option-ignored",
			"msg_id", uint16(dnfenum.CmdPacketSaveCharacterOption),
			"body_len", len(body),
			"reason", "current_exe_op447_requires_u32_512_byte_client_option_table")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, characterID, owned, err := s.currentCharacterOptionOwner(ctx, session)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	payload := append([]byte(nil), body[currentCharacterOptionLengthBytes:]...)
	if err := dnfrepo.SaveCharacterOptionSnapshot(ctx, repositories.Settings, characterID, payload, s.gameplayNow().UTC()); err != nil {
		return fmt.Errorf("save character option snapshot: %w", err)
	}
	s.logGameEvent(session, "game-save-character-option-committed",
		"msg_id", uint16(dnfenum.CmdPacketSaveCharacterOption),
		"request_body_len", len(body),
		"payload_len", len(payload),
		"cooldown_notification_skills", currentCharacterOptionCooldownSkills(payload),
		"response_sent", false,
		"body_source", "current_exe_sub_1F9DC50_length_prefixed_opaque_snapshot")
	return nil
}

func (s *Service) sendCurrentCharacterOptionSnapshot(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, characterID, owned, err := s.currentCharacterOptionOwner(ctx, session)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	payload, found, err := dnfrepo.LoadCharacterOptionSnapshot(ctx, repositories.Settings, characterID)
	if err != nil {
		return fmt.Errorf("load character option snapshot: %w", err)
	}
	if !found {
		payload, found, err = loadLegacyCharacterOptionSnapshot(ctx, repositories.Settings, characterID)
		if err != nil {
			return fmt.Errorf("load legacy character option snapshot: %w", err)
		}
		if found {
			if err := dnfrepo.SaveCharacterOptionSnapshot(ctx, repositories.Settings, characterID, payload, s.gameplayNow().UTC()); err != nil {
				return fmt.Errorf("migrate legacy character option snapshot: %w", err)
			}
			s.logGameEvent(session, "game-character-option-legacy-snapshot-migrated",
				"source", source,
				"char_id", session.selectedCharacterID,
				"payload_len", len(payload))
		}
	}
	if !found {
		s.logGameEvent(session, "game-character-option-restore-skipped",
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "no_durable_snapshot")
		return nil
	}
	body := make([]byte, currentSaveCharacterOptionBodySize)
	binary.LittleEndian.PutUint32(body[:currentCharacterOptionLengthBytes], currentCharacterOptionPayloadBytes)
	copy(body[currentCharacterOptionLengthBytes:], payload)
	if err := s.sendGameUpperRawClass(session, currentCharacterOptionRestoreMsgID, body, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-character-option-restored",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentCharacterOptionRestoreMsgID,
		"classification", 0,
		"payload_len", len(payload),
		"cooldown_notification_skills", currentCharacterOptionCooldownSkills(payload),
		"body_source", "current_exe_op390_sub_1D39570_sub_1F9E520_length_prefixed_opaque_snapshot")
	return nil
}

func loadLegacyCharacterOptionSnapshot(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	characterID string,
) ([]byte, bool, error) {
	if repository == nil || strings.TrimSpace(characterID) == "" {
		return nil, false, nil
	}
	record, found, err := repository.Load(ctx, legacyCharacterOptionScope(characterID))
	if err != nil || !found {
		return nil, found, err
	}
	encoded := strings.TrimSpace(record.Values[legacyCharacterOptionValueKey])
	if encoded == "" {
		return nil, false, nil
	}
	body, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || !validCurrentSaveCharacterOptionBody(body) {
		return nil, false, nil
	}
	return append([]byte(nil), body[currentCharacterOptionLengthBytes:]...), true, nil
}

func (s *Service) currentCharacterOptionOwner(ctx context.Context, session *gameSession) (dnfrepo.Group, string, bool, error) {
	repositories, available := s.repositoryGroup()
	if !available || repositories.Character == nil || repositories.Settings == nil {
		return dnfrepo.Group{}, "", false, errCurrentCharacterOptionRepositoryUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.Group{}, "", false, fmt.Errorf("load selected character: %w", err)
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if !found || strings.TrimSpace(character.AccountID) != accountID {
		s.logGameEvent(session, "game-character-option-owner-rejected",
			"char_id", session.selectedCharacterID,
			"character_found", found,
			"account_id", accountID,
			"reason", "selected_character_not_owned_by_authenticated_account")
		return repositories, characterID, false, nil
	}
	return repositories, characterID, true, nil
}

func currentCharacterOptionCooldownSkills(payload []byte) []uint16 {
	if len(payload) != currentCharacterOptionPayloadBytes {
		return nil
	}
	const firstSlot = 2
	const slotCount = 40
	skills := make([]uint16, 0, slotCount)
	for slot := firstSlot; slot < firstSlot+slotCount; slot++ {
		skillID := binary.LittleEndian.Uint16(payload[slot*2 : slot*2+2])
		if skillID != 0xffff {
			skills = append(skills, skillID)
		}
	}
	return skills
}

// Older scene code clears these fields when the character changes. Retain the
// reset as a compatibility boundary even though op390 restoration is now
// driven by the deferred-tail stage rather than the former op391 cursor.
func clearCurrentCharacterOptionRestoreSnapshot(session *gameSession) {
	if session == nil {
		return
	}
	session.initialTownCharacterOptionRestoreCharacterID = 0
	session.initialTownCharacterOptionRestoreBody = nil
	session.initialTownCharacterOptionRestoreAt = time.Time{}
	session.initialTownCharacterOptionCorrectionSent = false
	session.characterOptionActorApplyCharacterID = 0
}

// sendCurrentCharacterOptionSnapshotForActor replays the saved option table
// once the local actor exists.
//
// The scene-init send alone was not enough for the skill-cooldown reminder.
// The current EXE's op390 handler sub_1F9E520 stores the 512-byte table at
// manager+0x288 and then calls sub_1F9D8D0 to rebuild the live cooldown list -
// but sub_1F9D8D0 begins with `if ([0x52B5BB0] == 0) return`, and that global
// is the local actor. At scene-init the actor is not there yet, so the table
// was saved, sent and stored, while the reminder list stayed empty until the
// player re-opened the UI and set it again, which calls the same applier with
// the actor live. Replaying the identical body after the actor exists runs the
// applier at a point where it can do its work; the handler is idempotent, so
// the extra send only re-applies what the client already holds.
func (s *Service) sendCurrentCharacterOptionSnapshotForActor(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 ||
		session.characterOptionActorApplyCharacterID == session.selectedCharacterID {
		return nil
	}
	if err := s.sendCurrentCharacterOptionSnapshot(session, source); err != nil {
		return err
	}
	session.characterOptionActorApplyCharacterID = session.selectedCharacterID
	return nil
}

func currentCharacterOptionRestoreSnapshotIsInitializationProtected(*gameSession) bool {
	return false
}
