package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/packageitem"
)

const (
	currentSeriaLuckGaugeMsgID uint16 = 0x019c
	currentSeriaLuckEventID    uint16 = 2037
)

func seriaLuckDisplayValue(stage int64) (byte, bool) {
	if stage < 0 || stage > packageitem.SeriaLuckValueMax {
		return 0, false
	}
	return byte(stage * 5), true
}

func buildSeriaLuckGaugeBody(before, after int64) ([]byte, bool) {
	b, ok := seriaLuckDisplayValue(before)
	if !ok {
		return nil, false
	}
	a, ok := seriaLuckDisplayValue(after)
	if !ok {
		return nil, false
	}
	return []byte{b, a}, true
}

func (s *Service) sendCurrentSeriaLuckGauge(session *gameSession, before, after int64, source string) error {
	body, ok := buildSeriaLuckGaugeBody(before, after)
	if !ok {
		return fmt.Errorf("invalid seria luck stage before=%d after=%d", before, after)
	}
	s.logPacketEvent("game-seria-luck-gauge-send", "source", source, "char_id", session.selectedCharacterID, "before", before, "after", after, "body", fmt.Sprintf("%x", body))
	return s.sendGameUpperRawClass(session, currentSeriaLuckGaugeMsgID, body, 0)
}

// sendInitialSeriaLuckGauge restores the account-owned gauge only after the
// selected scene has installed its HUD. A missing or malformed durable value
// is not equivalent to a persisted zero stage, so it is deliberately omitted.
func (s *Service) sendInitialSeriaLuckGauge(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		s.logGameEvent(session, "game-seria-luck-gauge-initial-skipped",
			"source", source,
			"reason", "account_repository_unavailable")
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		s.logGameEvent(session, "game-seria-luck-gauge-initial-skipped",
			"source", source,
			"reason", "account_id_unavailable")
		return nil
	}
	account, found, err := repositories.Account.Load(context.Background(), accountID)
	if err != nil {
		s.logGameEvent(session, "game-seria-luck-gauge-initial-skipped",
			"source", source,
			"account_id", accountID,
			"reason", "account_load_failed",
			"error", err)
		return nil
	}
	if !found {
		s.logGameEvent(session, "game-seria-luck-gauge-initial-skipped",
			"source", source,
			"account_id", accountID,
			"reason", "account_not_found")
		return nil
	}
	raw := strings.TrimSpace(account.Metadata[packageitem.SeriaLuckMetadataKey])
	if raw == "" {
		s.logGameEvent(session, "game-seria-luck-gauge-initial-skipped",
			"source", source,
			"account_id", accountID,
			"reason", "metadata_missing")
		return nil
	}
	stage, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || stage < 0 || stage > packageitem.SeriaLuckValueMax {
		s.logGameEvent(session, "game-seria-luck-gauge-initial-skipped",
			"source", source,
			"account_id", accountID,
			"reason", "metadata_invalid",
			"value", raw)
		return nil
	}
	return s.sendCurrentSeriaLuckGauge(session, stage, stage, source)
}
