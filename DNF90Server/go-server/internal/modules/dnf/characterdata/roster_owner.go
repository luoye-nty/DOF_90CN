package characterdata

import (
	"context"
	"errors"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrRosterOwnerUnavailable = errors.New("character roster owner unavailable")

// RosterOwner owns durable character-selector roster mutations.
type RosterOwner struct {
	characters dnfrepo.CharacterRepository
}

func NewRosterOwner(repositories dnfrepo.Group) (*RosterOwner, error) {
	if repositories.Character == nil {
		return nil, ErrRosterOwnerUnavailable
	}
	return &RosterOwner{characters: repositories.Character}, nil
}

// SwapSlots exchanges two occupied selector slots for one account.
func (o *RosterOwner) SwapSlots(ctx context.Context, accountID string, slotA, slotB int) error {
	if o == nil || o.characters == nil {
		return ErrRosterOwnerUnavailable
	}
	return dnfrepo.SwapCharacterSlots(ctx, o.characters, accountID, slotA, slotB)
}

// DeleteCharacter validates the selector slot/name pair and commits the
// soft-delete plus active-slot compaction through the repository boundary.
func (o *RosterOwner) DeleteCharacter(ctx context.Context, accountID string, slot int, name string) error {
	if o == nil || o.characters == nil {
		return ErrRosterOwnerUnavailable
	}
	return dnfrepo.DeleteCharacterAndCompact(ctx, o.characters, accountID, slot, name)
}
