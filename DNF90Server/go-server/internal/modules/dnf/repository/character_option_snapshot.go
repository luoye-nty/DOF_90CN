package repository

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

const CharacterOptionSnapshotSize = 0x200

const (
	characterOptionSnapshotFormatKey   = "format"
	characterOptionSnapshotPayloadKey  = "payload_base64"
	characterOptionSnapshotFormatValue = "opaque-512-base64-v1"
)

var ErrCharacterOptionSnapshotInvalid = errors.New("character option snapshot is invalid")

func CharacterOptionSnapshotScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + ":option_snapshot"
}

func LoadCharacterOptionSnapshot(ctx context.Context, repo SettingsRepository, characterID string) ([]byte, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if repo == nil || characterID == "" {
		return nil, false, ErrCharacterOptionSnapshotInvalid
	}
	record, found, err := repo.Load(ctx, CharacterOptionSnapshotScope(characterID))
	if err != nil || !found {
		return nil, found, err
	}
	if record.Values[characterOptionSnapshotFormatKey] != characterOptionSnapshotFormatValue {
		return nil, false, fmt.Errorf("%w: unsupported format", ErrCharacterOptionSnapshotInvalid)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(record.Values[characterOptionSnapshotPayloadKey])
	if err != nil {
		return nil, false, fmt.Errorf("%w: decode payload: %v", ErrCharacterOptionSnapshotInvalid, err)
	}
	if len(payload) != CharacterOptionSnapshotSize {
		return nil, false, fmt.Errorf("%w: payload length=%d want=%d", ErrCharacterOptionSnapshotInvalid, len(payload), CharacterOptionSnapshotSize)
	}
	return payload, true, nil
}

func SaveCharacterOptionSnapshot(ctx context.Context, repo SettingsRepository, characterID string, payload []byte, updatedAt time.Time) error {
	characterID = strings.TrimSpace(characterID)
	if repo == nil || characterID == "" || len(payload) != CharacterOptionSnapshotSize {
		return ErrCharacterOptionSnapshotInvalid
	}
	return repo.Save(ctx, SettingsRecord{
		Scope: CharacterOptionSnapshotScope(characterID),
		Values: map[string]string{
			characterOptionSnapshotFormatKey:  characterOptionSnapshotFormatValue,
			characterOptionSnapshotPayloadKey: base64.StdEncoding.EncodeToString(append([]byte(nil), payload...)),
		},
		UpdatedAt: updatedAt,
	})
}
