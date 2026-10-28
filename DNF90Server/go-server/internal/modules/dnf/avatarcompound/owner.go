package avatarcompound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrOwnerUnavailable  = errors.New("avatar compound owner is unavailable")
	ErrAccountRequired   = errors.New("avatar compound account id is required")
	ErrCharacterRequired = errors.New("avatar compound character id is required")
	ErrCharacterNotFound = errors.New("avatar compound character is not found")
	ErrAccountMismatch   = errors.New("avatar compound character account mismatch")
	ErrInventoryNotFound = errors.New("avatar compound inventory is not found")
	ErrProjectorRequired = errors.New("avatar compound mutation projector is required")
)

type Assets struct {
	Character *dnfrepo.CharacterRecord
	Inventory *dnfrepo.InventoryRecord
}

type Projector func(*Assets) error

type Command struct {
	AccountID   string
	CharacterID string
	UpdatedAt   time.Time
	Project     Projector
}

// Owner owns the character authorization and one-character inventory
// transaction used by avatar synthesis. The bridge supplies PVF decisions;
// this owner guarantees that both source avatars, the synthesizer, and every
// produced avatar commit together or all roll back.
type Owner struct {
	assets dnfrepo.CharacterAssetUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{assets: repositories.CharacterAssets}, nil
}

func (o *Owner) Compound(ctx context.Context, command Command) error {
	if o == nil || o.assets == nil {
		return ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(command.AccountID)
	characterID := strings.TrimSpace(command.CharacterID)
	if accountID == "" {
		return ErrAccountRequired
	}
	if characterID == "" {
		return ErrCharacterRequired
	}
	if command.Project == nil {
		return ErrProjectorRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := command.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	return o.assets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		if characters == nil || inventories == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if strings.TrimSpace(character.AccountID) != accountID {
			return fmt.Errorf("%w: account=%s character=%s", ErrAccountMismatch, accountID, characterID)
		}
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID || inventory.Slots == nil {
			return fmt.Errorf("%w: character=%s", ErrInventoryNotFound, characterID)
		}
		character = dnfrepo.CloneCharacter(character)
		inventory = dnfrepo.CloneInventory(inventory)
		if err := command.Project(&Assets{Character: &character, Inventory: &inventory}); err != nil {
			return err
		}
		inventory.UpdatedAt = now
		return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
	})
}
