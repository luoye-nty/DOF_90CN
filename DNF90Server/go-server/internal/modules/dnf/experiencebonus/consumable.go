package experiencebonus

import (
	"errors"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	BonusPercentMetadataKey = "experience_consumable_bonus_percent"
	ExpireAtMetadataKey     = "experience_consumable_expire_at"
	ItemIDMetadataKey       = "experience_consumable_item_id"
)

var (
	ErrEffectInvalid = errors.New("experience consumable effect is invalid")
	ErrEffectActive  = errors.New("experience consumable effect is already active")
)

type Effect struct {
	CharacterID  string
	ItemID       int64
	BonusPercent int64
	ExpireAt     time.Time
}

func Active(account dnfrepo.AccountRecord, now time.Time) (Effect, bool) {
	return activeWithKeys(account, "", now)
}

// ActiveForCharacter reads the character-scoped effect stored in account
// metadata. The account repository is retained as the durable owner, but a
// different character in the same account must never inherit the buff.
func ActiveForCharacter(account dnfrepo.AccountRecord, characterID string, now time.Time) (Effect, bool) {
	return activeWithKeys(account, strings.TrimSpace(characterID), now)
}

func activeWithKeys(account dnfrepo.AccountRecord, characterID string, now time.Time) (Effect, bool) {
	if account.Metadata == nil {
		return Effect{}, false
	}
	bonusKey, expireKey, itemKey := metadataKeys(characterID)
	percent, percentErr := strconv.ParseInt(strings.TrimSpace(account.Metadata[bonusKey]), 10, 64)
	expireUnix, expireErr := strconv.ParseInt(strings.TrimSpace(account.Metadata[expireKey]), 10, 64)
	itemID, _ := strconv.ParseInt(strings.TrimSpace(account.Metadata[itemKey]), 10, 64)
	if percentErr != nil || expireErr != nil || percent <= 0 || expireUnix <= 0 {
		return Effect{}, false
	}
	expireAt := time.Unix(expireUnix, 0).UTC()
	if !expireAt.After(now.UTC()) {
		return Effect{}, false
	}
	return Effect{CharacterID: characterID, ItemID: itemID, BonusPercent: percent, ExpireAt: expireAt}, true
}

func Activate(account *dnfrepo.AccountRecord, itemID int64, bonusPercent int64, duration time.Duration, now time.Time) (Effect, error) {
	return activateWithCharacter(account, "", itemID, bonusPercent, duration, now)
}

// ActivateForCharacter stores the effect under keys scoped to one character.
func ActivateForCharacter(account *dnfrepo.AccountRecord, characterID string, itemID int64, bonusPercent int64, duration time.Duration, now time.Time) (Effect, error) {
	return activateWithCharacter(account, strings.TrimSpace(characterID), itemID, bonusPercent, duration, now)
}

func activateWithCharacter(account *dnfrepo.AccountRecord, characterID string, itemID int64, bonusPercent int64, duration time.Duration, now time.Time) (Effect, error) {
	if account == nil || itemID <= 0 || bonusPercent <= 0 || duration <= 0 {
		return Effect{}, ErrEffectInvalid
	}
	now = now.UTC()
	if _, active := activeWithKeys(*account, characterID, now); active {
		return Effect{}, ErrEffectActive
	}
	effect := Effect{
		CharacterID:  characterID,
		ItemID:       itemID,
		BonusPercent: bonusPercent,
		ExpireAt:     now.Add(duration).UTC(),
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string, 3)
	}
	bonusKey, expireKey, itemKey := metadataKeys(characterID)
	account.Metadata[bonusKey] = strconv.FormatInt(effect.BonusPercent, 10)
	account.Metadata[expireKey] = strconv.FormatInt(effect.ExpireAt.Unix(), 10)
	account.Metadata[itemKey] = strconv.FormatInt(effect.ItemID, 10)
	account.UpdatedAt = now
	return effect, nil
}

func metadataKeys(characterID string) (bonus, expire, item string) {
	if strings.TrimSpace(characterID) == "" {
		return BonusPercentMetadataKey, ExpireAtMetadataKey, ItemIDMetadataKey
	}
	prefix := "experience_consumable." + strings.TrimSpace(characterID) + "."
	return prefix + "bonus_percent", prefix + "expire_at", prefix + "item_id"
}
