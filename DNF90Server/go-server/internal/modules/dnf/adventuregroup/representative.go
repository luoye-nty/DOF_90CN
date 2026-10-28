package adventuregroup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	representativeSettingsScopePrefix = "account:"
	representativeSettingsScopeSuffix = ":adventure_representatives"
	maximumRepresentativeJob          = byte(15)
	minimumRepresentativeAwakening    = byte(2)
	minimumRepresentativeBaseGrow     = byte(1)
	maximumRepresentativeBaseGrow     = byte(15)
	representativeRemovedValue        = "removed"
	representativeSelectedPrefix      = "selected:"
)

var (
	ErrRepresentativeRepository = errors.New("adventure-group representative repository is unavailable")
	ErrRepresentativeSlot       = errors.New("adventure-group representative slot is invalid")
	ErrRepresentativeCharacter  = errors.New("adventure-group representative character is invalid")
)

type RepresentativeSlot struct {
	Job      byte
	BaseGrow byte
}

type RepresentativeSelection struct {
	CharacterID string
	Removed     bool
}

type RepresentativeState struct {
	Selections map[RepresentativeSlot]RepresentativeSelection
}

func ValidRepresentativeSlot(slot RepresentativeSlot) bool {
	return slot.Job <= maximumRepresentativeJob &&
		slot.BaseGrow >= minimumRepresentativeBaseGrow &&
		slot.BaseGrow <= maximumRepresentativeBaseGrow
}

func RepresentativeCharacterEligible(character dnfrepo.CharacterRecord, slot RepresentativeSlot, maximumLevel int) bool {
	if !ValidRepresentativeSlot(slot) || maximumLevel <= 0 || character.Level < maximumLevel ||
		byte(numericRecordValue(character.Job)) != slot.Job {
		return false
	}
	packed := byte(character.Stats["grow_type"])
	return packed&0x0f == slot.BaseGrow && packed>>4 >= minimumRepresentativeAwakening
}

func RepresentativeSlotForCharacter(character dnfrepo.CharacterRecord, maximumLevel int) (RepresentativeSlot, bool) {
	packed := byte(character.Stats["grow_type"])
	slot := RepresentativeSlot{Job: byte(numericRecordValue(character.Job)), BaseGrow: packed & 0x0f}
	return slot, RepresentativeCharacterEligible(character, slot, maximumLevel)
}

func ProjectRepresentativeCharacters(
	characters []dnfrepo.CharacterRecord,
	state RepresentativeState,
	maximumLevel int,
) []dnfrepo.CharacterRecord {
	fallbacks := make(map[RepresentativeSlot]dnfrepo.CharacterRecord)
	eligibleByID := make(map[string]dnfrepo.CharacterRecord)
	order := make([]RepresentativeSlot, 0)
	for _, character := range characters {
		slot, eligible := RepresentativeSlotForCharacter(character, maximumLevel)
		if !eligible {
			continue
		}
		characterID := strings.TrimSpace(character.CharacterID)
		if characterID != "" {
			eligibleByID[characterID] = character
		}
		if _, exists := fallbacks[slot]; !exists {
			fallbacks[slot] = character
			order = append(order, slot)
		}
	}

	result := make([]dnfrepo.CharacterRecord, 0, len(order))
	for _, slot := range order {
		selection, selected := state.Selections[slot]
		if selected && selection.Removed {
			continue
		}
		if selected && strings.TrimSpace(selection.CharacterID) != "" {
			if character, found := eligibleByID[strings.TrimSpace(selection.CharacterID)]; found {
				characterSlot, eligible := RepresentativeSlotForCharacter(character, maximumLevel)
				if eligible && characterSlot == slot {
					result = append(result, character)
					continue
				}
			}
		}
		result = append(result, fallbacks[slot])
	}
	return result
}

type RepresentativeOwner struct {
	settings dnfrepo.SettingsRepository
}

func NewRepresentativeOwner(repositories dnfrepo.Group) (*RepresentativeOwner, error) {
	if repositories.Settings == nil {
		return nil, ErrRepresentativeRepository
	}
	return &RepresentativeOwner{settings: repositories.Settings}, nil
}

func RepresentativeSettingsScope(accountID string) string {
	return representativeSettingsScopePrefix + strings.TrimSpace(accountID) + representativeSettingsScopeSuffix
}

func (o *RepresentativeOwner) Load(ctx context.Context, accountID string) (RepresentativeState, error) {
	if o == nil || o.settings == nil {
		return RepresentativeState{}, ErrRepresentativeRepository
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return RepresentativeState{}, ErrAccountRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record, found, err := o.settings.Load(ctx, RepresentativeSettingsScope(accountID))
	if err != nil {
		return RepresentativeState{}, fmt.Errorf("load adventure-group representatives: %w", err)
	}
	state := RepresentativeState{Selections: make(map[RepresentativeSlot]RepresentativeSelection)}
	if !found {
		return state, nil
	}
	for key, value := range record.Values {
		slot, valid := parseRepresentativeSlotKey(key)
		if !valid {
			continue
		}
		switch {
		case strings.TrimSpace(value) == representativeRemovedValue:
			state.Selections[slot] = RepresentativeSelection{Removed: true}
		case strings.HasPrefix(strings.TrimSpace(value), representativeSelectedPrefix):
			characterID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), representativeSelectedPrefix))
			if characterID != "" {
				state.Selections[slot] = RepresentativeSelection{CharacterID: characterID}
			}
		}
	}
	return state, nil
}

func (o *RepresentativeOwner) Select(
	ctx context.Context,
	accountID string,
	slot RepresentativeSlot,
	characterID string,
	updatedAt time.Time,
) error {
	characterID = strings.TrimSpace(characterID)
	if !ValidRepresentativeSlot(slot) {
		return ErrRepresentativeSlot
	}
	if characterID == "" {
		return ErrRepresentativeCharacter
	}
	return o.save(ctx, accountID, slot, representativeSelectedPrefix+characterID, updatedAt)
}

func (o *RepresentativeOwner) Remove(
	ctx context.Context,
	accountID string,
	slot RepresentativeSlot,
	updatedAt time.Time,
) error {
	if !ValidRepresentativeSlot(slot) {
		return ErrRepresentativeSlot
	}
	return o.save(ctx, accountID, slot, representativeRemovedValue, updatedAt)
}

func (o *RepresentativeOwner) save(
	ctx context.Context,
	accountID string,
	slot RepresentativeSlot,
	value string,
	updatedAt time.Time,
) error {
	if o == nil || o.settings == nil {
		return ErrRepresentativeRepository
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ErrAccountRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	scope := RepresentativeSettingsScope(accountID)
	record, found, err := o.settings.Load(ctx, scope)
	if err != nil {
		return fmt.Errorf("load adventure-group representatives for update: %w", err)
	}
	if !found {
		record = dnfrepo.SettingsRecord{Scope: scope, Values: make(map[string]string)}
	} else {
		record = dnfrepo.CloneSettings(record)
		if record.Values == nil {
			record.Values = make(map[string]string)
		}
	}
	record.Values[representativeSlotKey(slot)] = value
	record.UpdatedAt = updatedAt
	if err := o.settings.Save(ctx, record); err != nil {
		return fmt.Errorf("save adventure-group representatives: %w", err)
	}
	return nil
}

func representativeSlotKey(slot RepresentativeSlot) string {
	return strconv.Itoa(int(slot.Job)) + ":" + strconv.Itoa(int(slot.BaseGrow))
}

func parseRepresentativeSlotKey(value string) (RepresentativeSlot, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return RepresentativeSlot{}, false
	}
	job, jobErr := strconv.ParseUint(parts[0], 10, 8)
	grow, growErr := strconv.ParseUint(parts[1], 10, 8)
	slot := RepresentativeSlot{Job: byte(job), BaseGrow: byte(grow)}
	return slot, jobErr == nil && growErr == nil && ValidRepresentativeSlot(slot)
}

func numericRecordValue(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed
}
