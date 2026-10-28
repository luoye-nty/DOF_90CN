// Package epicwish owns the character-scoped durable Epic wish list.
// Packet layouts, PVF item validation, dungeon routing, and reward selection
// remain outside this package.
package epicwish

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// MaximumItems is the current client's fixed Epic wish slot count.
	MaximumItems = 5
	// CountStat stores the number of serialized wish slots, including zero
	// placeholders retained by the client.
	CountStat = "epic_wish_count"
)

var itemStatKeys = [MaximumItems]string{
	"epic_wish_item_0",
	"epic_wish_item_1",
	"epic_wish_item_2",
	"epic_wish_item_3",
	"epic_wish_item_4",
}

var (
	ErrOwnerUnavailable      = errors.New("epic wish owner is unavailable")
	ErrCharacterRequired     = errors.New("epic wish character id is required")
	ErrAccountRequired       = errors.New("epic wish account id is required")
	ErrCharacterNotFound     = errors.New("epic wish character is missing")
	ErrAccountMismatch       = errors.New("epic wish character does not belong to account")
	ErrTooManyItems          = errors.New("epic wish item count exceeds client limit")
	ErrDuplicateItem         = errors.New("epic wish contains a duplicate nonzero item")
	ErrPersistedStateInvalid = errors.New("persisted epic wish state is invalid")
)

type Command struct {
	CharacterID string
	AccountID   string
	Items       []uint32
}

// Result is detached from both the repository transaction and the command.
// Callers may retain or mutate it without changing durable state.
type Result struct {
	Character dnfrepo.CharacterRecord
	Items     []uint32
	Changed   bool
}

// Owner owns only the atomic Character.Stats mutation.
type Owner struct {
	assets dnfrepo.CharacterAssetUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{assets: repositories.CharacterAssets}, nil
}

// Apply replaces the complete ordered wish list. Replaying the same canonical
// list is idempotent and does not issue another repository save.
func (o *Owner) Apply(ctx context.Context, command Command) (Result, error) {
	if o == nil || o.assets == nil {
		return Result{}, ErrOwnerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return Result{}, ErrCharacterRequired
	}
	accountID := strings.TrimSpace(command.AccountID)
	if accountID == "" {
		return Result{}, ErrAccountRequired
	}
	items, err := cloneValidatedItems(command.Items)
	if err != nil {
		return Result{}, err
	}

	var result Result
	err = o.assets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		if characters == nil {
			return ErrOwnerUnavailable
		}
		character, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return fmt.Errorf("load epic wish character: %w", loadErr)
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if strings.TrimSpace(character.AccountID) != accountID {
			return fmt.Errorf(
				"%w: account=%q character=%s",
				ErrAccountMismatch,
				accountID,
				characterID,
			)
		}

		changed := !hasCanonicalStats(character.Stats, items)
		character = dnfrepo.CloneCharacter(character)
		if changed {
			if character.Stats == nil {
				character.Stats = make(map[string]int64, MaximumItems+1)
			}
			character.Stats[CountStat] = int64(len(items))
			for index, key := range itemStatKeys {
				var value uint32
				if index < len(items) {
					value = items[index]
				}
				character.Stats[key] = int64(value)
			}
			if saveErr := dnfrepo.SaveCharacterFields(
				ctx,
				characters,
				character,
				dnfrepo.CharacterFieldStats,
			); saveErr != nil {
				return fmt.Errorf("save epic wish character stats: %w", saveErr)
			}
		}
		result = freezeResult(Result{Character: character, Items: items, Changed: changed})
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return freezeResult(result), nil
}

// ItemsFromCharacter decodes one canonical persisted wish list. Invalid
// durable values fail closed instead of being truncated into a forged client
// snapshot.
func ItemsFromCharacter(character dnfrepo.CharacterRecord) ([]uint32, error) {
	if character.Stats == nil {
		return nil, nil
	}
	count := character.Stats[CountStat]
	if count < 0 || count > MaximumItems {
		return nil, fmt.Errorf("%w: count=%d", ErrPersistedStateInvalid, count)
	}
	items := make([]uint32, int(count))
	seen := make(map[uint32]struct{}, len(items))
	for index := range items {
		value := character.Stats[itemStatKeys[index]]
		if value < 0 || value > math.MaxUint32 {
			return nil, fmt.Errorf(
				"%w: slot=%d value=%d",
				ErrPersistedStateInvalid,
				index,
				value,
			)
		}
		items[index] = uint32(value)
		if items[index] == 0 {
			continue
		}
		if _, duplicate := seen[items[index]]; duplicate {
			return nil, fmt.Errorf(
				"%w: %w: item=%d",
				ErrPersistedStateInvalid,
				ErrDuplicateItem,
				items[index],
			)
		}
		seen[items[index]] = struct{}{}
	}
	return append([]uint32(nil), items...), nil
}

func cloneValidatedItems(items []uint32) ([]uint32, error) {
	if len(items) > MaximumItems {
		return nil, fmt.Errorf("%w: count=%d", ErrTooManyItems, len(items))
	}
	cloned := append([]uint32(nil), items...)
	seen := make(map[uint32]struct{}, len(cloned))
	for _, itemID := range cloned {
		if itemID == 0 {
			continue
		}
		if _, duplicate := seen[itemID]; duplicate {
			return nil, fmt.Errorf("%w: item=%d", ErrDuplicateItem, itemID)
		}
		seen[itemID] = struct{}{}
	}
	return cloned, nil
}

func hasCanonicalStats(stats map[string]int64, items []uint32) bool {
	if stats == nil {
		return false
	}
	count, found := stats[CountStat]
	if !found || count != int64(len(items)) {
		return false
	}
	for index, key := range itemStatKeys {
		value, found := stats[key]
		if !found {
			return false
		}
		var want uint32
		if index < len(items) {
			want = items[index]
		}
		if value != int64(want) {
			return false
		}
	}
	return true
}

func freezeResult(result Result) Result {
	result.Character = dnfrepo.CloneCharacter(result.Character)
	result.Items = append([]uint32(nil), result.Items...)
	return result
}
