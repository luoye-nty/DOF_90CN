package dnfbridge

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) currentCharacterHotkeySlots(ctx context.Context, job byte) ([]byte, byte, string, error) {
	if job != csharpCreatorMageJob {
		return cloneCSharpDefaultHotkeySlots(), 0, "csharp_account_settings_default_hotkey_slots", nil
	}
	slots, err := s.csharpCreatorHotkeySlots(ctx)
	if err != nil {
		return nil, 0, "", fmt.Errorf("load creator hotkeys from %s: %w", csharpCreatorHotkeyPVFPath, err)
	}
	return slots, 1, "pvf_clientonly_hotkeysystemforcreator_ver2_indexed", nil
}

func characterHotkeySettingsRecord(
	characterID string,
	job byte,
	keyType byte,
	slots []byte,
	source string,
	updatedAt time.Time,
) dnfrepo.SettingsRecord {
	return dnfrepo.SettingsRecord{
		Scope: newCharacterHotkeySettingsScope(characterID),
		Values: map[string]string{
			"source":                    source,
			"job":                       strconv.Itoa(int(job)),
			"key_type":                  strconv.Itoa(int(keyType)),
			"slot_count":                strconv.Itoa(len(slots) / 2),
			"account_scoped_slot_count": strconv.Itoa(csharpHotkeyAccountScopedSlotSize),
			"slots_hex":                 hex.EncodeToString(slots),
		},
		UpdatedAt: updatedAt,
	}
}

func currentCharacterHotkeySettingsMatch(record dnfrepo.SettingsRecord, expected dnfrepo.SettingsRecord) bool {
	for _, key := range []string{
		"source",
		"job",
		"key_type",
		"slot_count",
		"account_scoped_slot_count",
		"slots_hex",
	} {
		if record.Values[key] != expected.Values[key] {
			return false
		}
	}
	return true
}
