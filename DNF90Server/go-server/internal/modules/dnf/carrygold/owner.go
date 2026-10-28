package carrygold

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrOwnerUnavailable   = errors.New("carry gold owner unavailable")
	ErrCharacterRequired  = errors.New("selected character id required")
	ErrCharacterNotFound  = errors.New("character record not found")
	ErrAccountMismatch    = errors.New("character account does not match session account")
	ErrStageOutOfSequence = errors.New("requested carry gold stage is not the next stage")
	ErrStageExhausted     = errors.New("carry gold stage is already at the client bound")
	ErrGoldInsufficient   = errors.New("carry gold upgrade cannot be paid for")
)

type Command struct {
	AccountID           string
	SelectedCharacterID uint16
	RequestedStage      byte
	UpdatedAt           time.Time
}

// Result reports the character's durable state after the call. Stage and
// GoldAfter are always the persisted values, so a refusal still carries the
// stage the client has to be told about.
type Result struct {
	CharacterID   string
	PreviousStage byte
	Stage         byte
	Cost          int64
	GoldBefore    int64
	GoldAfter     int64
	Limit         int64
	Upgraded      bool
}

// Owner owns the carry-gold stage rule and its character mutation. The bridge
// owns only the current EXE wire format and response ordering.
type Owner struct {
	characters dnfrepo.CharacterRepository
}

func NewOwner(repos dnfrepo.Group) (*Owner, error) {
	if repos.Character == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{characters: repos.Character}, nil
}

// Load reports the durable stage and limit without mutating anything. It is
// what the login projection uses.
func (o *Owner) Load(ctx context.Context, selectedCharacterID uint16) (Result, error) {
	character, characterID, err := o.load(ctx, selectedCharacterID, "")
	if err != nil {
		return Result{}, err
	}
	stage := Stage(character.Stats)
	return Result{
		CharacterID:   characterID,
		PreviousStage: stage,
		Stage:         stage,
		GoldBefore:    character.Stats["gold"],
		GoldAfter:     character.Stats["gold"],
		Limit:         LimitForCharacter(character),
	}, nil
}

// Upgrade advances the durable stage by exactly one and charges StageCost.
// Gold is checked before any mutation, so a refusal leaves the character
// untouched.
func (o *Owner) Upgrade(ctx context.Context, cmd Command) (Result, error) {
	character, characterID, err := o.load(ctx, cmd.SelectedCharacterID, cmd.AccountID)
	if err != nil {
		return Result{}, err
	}
	current := Stage(character.Stats)
	gold := character.Stats["gold"]
	result := Result{
		CharacterID:   characterID,
		PreviousStage: current,
		Stage:         current,
		Cost:          StageCost,
		GoldBefore:    gold,
		GoldAfter:     gold,
		Limit:         LimitForCharacter(character),
	}
	if current >= MaxStage {
		return result, fmt.Errorf("%w: character=%s stage=%d", ErrStageExhausted, characterID, current)
	}
	if cmd.RequestedStage != current+1 {
		return result, fmt.Errorf(
			"%w: character=%s stage=%d requested=%d",
			ErrStageOutOfSequence, characterID, current, cmd.RequestedStage,
		)
	}
	if gold < StageCost {
		return result, fmt.Errorf(
			"%w: character=%s gold=%d cost=%d",
			ErrGoldInsufficient, characterID, gold, StageCost,
		)
	}

	updated := dnfrepo.CloneCharacter(character)
	if updated.Stats == nil {
		updated.Stats = make(map[string]int64, 2)
	}
	goldAfter := gold - StageCost
	updated.Stats["gold"] = goldAfter
	updated.Stats[StageStat] = int64(cmd.RequestedStage)
	if cmd.UpdatedAt.IsZero() {
		updated.UpdatedAt = time.Now().UTC()
	} else {
		updated.UpdatedAt = cmd.UpdatedAt.UTC()
	}
	if err := dnfrepo.SaveCharacterFields(
		ctx, o.characters, updated, dnfrepo.CharacterFieldStats,
	); err != nil {
		return result, err
	}
	result.Stage = cmd.RequestedStage
	result.GoldAfter = goldAfter
	result.Limit = LimitForCharacter(updated)
	result.Upgraded = true
	return result, nil
}

func (o *Owner) load(
	ctx context.Context,
	selectedCharacterID uint16,
	accountID string,
) (dnfrepo.CharacterRecord, string, error) {
	if o == nil || o.characters == nil {
		return dnfrepo.CharacterRecord{}, "", ErrOwnerUnavailable
	}
	if selectedCharacterID == 0 {
		return dnfrepo.CharacterRecord{}, "", ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(selectedCharacterID), 10)
	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.CharacterRecord{}, characterID, err
	}
	if !found || strings.TrimSpace(character.CharacterID) != characterID {
		return dnfrepo.CharacterRecord{}, characterID, fmt.Errorf(
			"%w: character=%s", ErrCharacterNotFound, characterID)
	}
	if trimmed := strings.TrimSpace(accountID); trimmed != "" &&
		strings.TrimSpace(character.AccountID) != trimmed {
		return dnfrepo.CharacterRecord{}, characterID, fmt.Errorf(
			"%w: character=%s", ErrAccountMismatch, characterID)
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 2)
	}
	return character, characterID, nil
}
