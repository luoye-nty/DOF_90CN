package fatigue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrOwnerUnavailable  = errors.New("fatigue owner unavailable")
	ErrCharacterRequired = errors.New("fatigue character id required")
	ErrCharacterNotFound = errors.New("fatigue character not found")
	ErrAccountMismatch   = errors.New("fatigue character account mismatch")
	ErrRemainingUnknown  = errors.New("fatigue remaining value unavailable")
	ErrInsufficient      = errors.New("fatigue remaining value insufficient")
)

type ConsumeRoomCommand struct {
	AccountID   string
	CharacterID string
	Amount      int64
	// RequireFull makes the consumption all-or-nothing: the whole amount must
	// be payable or nothing is charged. PVP arcade mode prices a run up front
	// this way, so a partially payable run must not start.
	RequireFull bool
	UpdatedAt   time.Time
}

type ConsumeRoomResult struct {
	Character       dnfrepo.CharacterRecord
	Before          int64
	After           int64
	Consumed        int64
	BatteryBefore   int64
	BatteryAfter    int64
	BatteryConsumed int64
}

type ReconcileDailyCommand struct {
	AccountID   string
	CharacterID string
	UpdatedAt   time.Time
}

type ReconcileDailyResult struct {
	Character dnfrepo.CharacterRecord
	DailyStatsResult
}

type Owner struct {
	settlements dnfrepo.CharacterSettlementUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{settlements: repositories.CharacterSettlement}, nil
}

// ReconcileDaily restores fatigue and clears every fatigue-potion counter once
// per Beijing service day in one character transaction.
func (o *Owner) ReconcileDaily(ctx context.Context, command ReconcileDailyCommand) (ReconcileDailyResult, error) {
	if o == nil || o.settlements == nil {
		return ReconcileDailyResult{}, ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return ReconcileDailyResult{}, ErrCharacterRequired
	}
	accountID := strings.TrimSpace(command.AccountID)
	updatedAt := normalizedUpdateTime(command.UpdatedAt)
	var result ReconcileDailyResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if stored := strings.TrimSpace(character.AccountID); stored != "" && accountID != "" && stored != accountID {
			return fmt.Errorf("%w: character=%s", ErrAccountMismatch, characterID)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 6)
		}
		daily := ReconcileDailyStats(character.Stats, updatedAt, character.Level)
		result = ReconcileDailyResult{Character: character, DailyStatsResult: daily}
		if !daily.Changed {
			return nil
		}
		character.UpdatedAt = updatedAt
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		result.Character = character
		return nil
	})
	if err != nil {
		return ReconcileDailyResult{}, err
	}
	return result, nil
}

// ConsumeRoom atomically deducts one committed room's fatigue from the latest
// persisted character state. The transaction reload prevents concurrent
// progression or membership-limit writes from being overwritten.
func (o *Owner) ConsumeRoom(ctx context.Context, command ConsumeRoomCommand) (ConsumeRoomResult, error) {
	if o == nil || o.settlements == nil {
		return ConsumeRoomResult{}, ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return ConsumeRoomResult{}, ErrCharacterRequired
	}
	accountID := strings.TrimSpace(command.AccountID)
	var result ConsumeRoomResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if stored := strings.TrimSpace(character.AccountID); stored != "" && accountID != "" && stored != accountID {
			return fmt.Errorf("%w: character=%s", ErrAccountMismatch, characterID)
		}
		character = dnfrepo.CloneCharacter(character)
		remaining, known := remainingStat(character.Stats)
		if !known {
			return fmt.Errorf("%w: character=%s", ErrRemainingUnknown, characterID)
		}
		next, consumed := Consume(remaining, command.Amount)
		if command.RequireFull && consumed != command.Amount {
			return ErrInsufficient
		}
		battery := min(max(character.Stats[FatigueBatteryStatKey], 0), BatteryCapacity(character.Level))
		batteryNext, batteryConsumed := battery, int64(0)
		if consumed > 0 && BatteryBuffCount(battery, character.Level, false) > 0 {
			batteryNext, batteryConsumed = Consume(battery, consumed)
		}
		result = ConsumeRoomResult{
			Character: character, Before: remaining, After: next, Consumed: consumed,
			BatteryBefore: battery, BatteryAfter: batteryNext, BatteryConsumed: batteryConsumed,
		}
		if consumed == 0 {
			return nil
		}
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 3)
		}
		limit := character.Stats["fatigue_limit"]
		if limit <= 0 {
			limit = DefaultLimit
			if remaining > limit {
				limit = remaining
			}
			character.Stats["fatigue_limit"] = limit
		}
		used := limit - next
		if used < 0 {
			used = 0
		}
		character.Stats["fatigue"] = next
		character.Stats["fatigue_display_update"] = used
		character.Stats[FatigueBatteryStatKey] = batteryNext
		character.UpdatedAt = normalizedUpdateTime(command.UpdatedAt)
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		result.Character = character
		return nil
	})
	if err != nil {
		return ConsumeRoomResult{}, err
	}
	return result, nil
}

func normalizedUpdateTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func remainingStat(stats map[string]int64) (int64, bool) {
	for _, key := range []string{"fatigue", "fp", "疲劳"} {
		if value, ok := stats[key]; ok {
			return value, true
		}
	}
	return 0, false
}
