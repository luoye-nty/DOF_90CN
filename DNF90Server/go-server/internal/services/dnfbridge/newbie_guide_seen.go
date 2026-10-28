package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentNewbieGuideOptionMsgID    = uint16(dnfenum.CmdPacketNewbieGuideOption)
	currentNewbieGuideOptionS2CClass = byte(1)
	currentNewbieGuideSeenKeyPrefix  = "seen:"
	currentNewbieGuideMaxOptionIndex = uint32(4095)
)

// Current-client evidence: legacy cmd=1/type=1431 carries a little-endian
// uint32 guide index. Clicking either the close button or the guide help button
// sends the same index, while the automatic alarm displays send nothing.
// Keep one account-scoped setting row per guide so dismissing one tip cannot
// suppress any other tip.
var currentNewbieGuidePersistenceMu sync.Mutex

func currentNewbieGuideScope(accountID string) string {
	return "account:" + strings.TrimSpace(accountID) + ":newbie_guide"
}

func currentNewbieGuideSeenKey(index uint32) string {
	return currentNewbieGuideSeenKeyPrefix + strconv.FormatUint(uint64(index), 10)
}

func decodeCurrentNewbieGuideOption(body []byte) (uint32, bool) {
	if len(body) != 4 {
		return 0, false
	}
	index := binary.LittleEndian.Uint32(body)
	if index > currentNewbieGuideMaxOptionIndex {
		return 0, false
	}
	return index, true
}

func loadCurrentNewbieGuideSeen(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
) ([]uint32, error) {
	accountID = strings.TrimSpace(accountID)
	if repository == nil || accountID == "" {
		return nil, nil
	}
	record, found, err := repository.Load(ctx, currentNewbieGuideScope(accountID))
	if err != nil || !found {
		return nil, err
	}
	seen := make([]uint32, 0, len(record.Values))
	for key := range record.Values {
		raw, ok := strings.CutPrefix(key, currentNewbieGuideSeenKeyPrefix)
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || value > uint64(currentNewbieGuideMaxOptionIndex) {
			continue
		}
		seen = append(seen, uint32(value))
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	return seen, nil
}

func markCurrentNewbieGuideSeen(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
	index uint32,
	now time.Time,
) (bool, error) {
	accountID = strings.TrimSpace(accountID)
	if repository == nil || accountID == "" || index > currentNewbieGuideMaxOptionIndex {
		return false, fmt.Errorf("invalid newbie guide seen state")
	}
	currentNewbieGuidePersistenceMu.Lock()
	defer currentNewbieGuidePersistenceMu.Unlock()

	scope := currentNewbieGuideScope(accountID)
	record, found, err := repository.Load(ctx, scope)
	if err != nil {
		return false, err
	}
	if !found {
		record = dnfrepo.SettingsRecord{Scope: scope}
	} else {
		record = dnfrepo.CloneSettings(record)
	}
	if record.Values == nil {
		record.Values = make(map[string]string)
	}
	key := currentNewbieGuideSeenKey(index)
	if _, exists := record.Values[key]; exists {
		return false, nil
	}
	record.Values[key] = strconv.FormatInt(now.UTC().Unix(), 10)
	record.UpdatedAt = now.UTC()
	if err := repository.Save(ctx, record); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) handleCurrentNewbieGuideOption(session *gameSession, body []byte) error {
	if s == nil || session == nil {
		return nil
	}
	index, ok := decodeCurrentNewbieGuideOption(body)
	if !ok {
		s.logGameEvent(session, "game-newbie-guide-option-ignored",
			"body_len", len(body),
			"reason", "invalid_current_exe_u32_guide_index")
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	repositories, available := s.repositoryGroup()
	if accountID == "" || !available || repositories.Settings == nil {
		s.logGameEvent(session, "game-newbie-guide-option-deferred",
			"account_id", accountID,
			"guide_index", index,
			"reason", "account_settings_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	added, err := markCurrentNewbieGuideSeen(
		ctx,
		repositories.Settings,
		accountID,
		index,
		time.Now(),
	)
	if err != nil {
		s.logGameEvent(session, "game-newbie-guide-option-save-failed",
			"account_id", accountID,
			"guide_index", index,
			"error", err)
		return nil
	}
	s.logGameEvent(session, "game-newbie-guide-option-seen",
		"account_id", accountID,
		"character_id", session.selectedCharacterID,
		"guide_index", index,
		"added", added,
		"scope", currentNewbieGuideScope(accountID),
		"storage", "account_settings_per_guide")
	return nil
}

func (s *Service) sendCurrentNewbieGuideOptions(session *gameSession) error {
	if s == nil || session == nil {
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return nil
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.Settings == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	seen, err := loadCurrentNewbieGuideSeen(ctx, repositories.Settings, accountID)
	if err != nil {
		return fmt.Errorf("load newbie guide seen options: %w", err)
	}
	for _, index := range seen {
		var body [4]byte
		binary.LittleEndian.PutUint32(body[:], index)
		if err := s.sendGameUpperRawClass(session, currentNewbieGuideOptionMsgID, body[:], currentNewbieGuideOptionS2CClass); err != nil {
			return err
		}
	}
	s.logInfo("dnfbridge newbie guide seen options synced",
		"conn_id", session.connID,
		"account_id", accountID,
		"character_id", session.selectedCharacterID,
		"count", len(seen),
		"guide_indices", fmt.Sprint(seen),
		"classification", currentNewbieGuideOptionS2CClass)
	if len(seen) != 0 {
		s.logGameEvent(session, "game-newbie-guide-seen-options-sent",
			"account_id", accountID,
			"count", len(seen),
			"guide_indices", fmt.Sprint(seen),
			"msg_id", currentNewbieGuideOptionMsgID,
			"classification", currentNewbieGuideOptionS2CClass,
			"storage", "account_settings_per_guide")
	}
	return nil
}
