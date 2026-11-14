package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/services/dnfrobot"
)

func currentRobotExpertMachineState(character dnfrepo.CharacterRecord, catalog robotExpertJobCatalog) (dnfrobot.ExpertMachineState, error) {
	jobType, experience, err := currentExpertJobCharacterState(&character)
	if err != nil {
		return dnfrobot.ExpertMachineState{}, err
	}
	switch jobType {
	case dnfexpertjob.DisjointerType:
		config, ok := catalog.Disjointer()
		if !ok || config == nil {
			return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrJobUnsupported
		}
		grade := config.Level(experience)
		if stored, configured := character.Stats[currentExpertJobMachineGradeStat]; configured && stored > 0 {
			grade = int(stored)
		}
		if grade > len(config.RepairRules) {
			grade = len(config.RepairRules)
		}
		rule, ok := config.RepairRule(grade)
		if !ok {
			return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrMachineInvalid
		}
		return dnfrobot.ExpertMachineState{
			Kind:         "disjoint",
			Grade:        grade,
			Endurance:    currentExpertJobStatDefault(character.Stats, currentExpertJobMachineEnduranceStat, rule.MaxEndurance),
			MaxEndurance: rule.MaxEndurance,
		}, nil
	case dnfexpertjob.EnchanterType:
		config, ok := catalog.Enchanter()
		if !ok || config == nil || config.Recipes == nil {
			return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrJobUnsupported
		}
		level := config.Recipes.Level(experience)
		if level > len(config.RepairRules) {
			level = len(config.RepairRules)
		}
		rule, ok := config.RepairRule(experience)
		if !ok {
			return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrMachineInvalid
		}
		return dnfrobot.ExpertMachineState{
			Kind:         "enchant",
			Grade:        level,
			Endurance:    currentExpertJobStatDefault(character.Stats, currentExpertJobMachineEnduranceStat, rule.MaxEndurance),
			MaxEndurance: rule.MaxEndurance,
		}, nil
	default:
		return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrJobUnsupported
	}
}

func (s *Service) RobotExpertMachineState(ctx context.Context, accountID string, characterID uint16) (dnfrobot.ExpertMachineState, error) {
	if s == nil || characterID == 0 {
		return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrCharacterRequired
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrOwnerUnavailable
	}
	catalog, err := s.currentRobotExpertJobCatalog()
	if err != nil {
		return dnfrobot.ExpertMachineState{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil {
		return dnfrobot.ExpertMachineState{}, err
	}
	if !found || strings.TrimSpace(character.AccountID) != strings.TrimSpace(accountID) {
		return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrCharacterNotFound
	}
	return currentRobotExpertMachineState(character, catalog)
}

// RestoreRobotExpertMachine performs the repair while the robot is offline.
// Automatic robot maintenance deliberately restores the configured maximum
// without consuming the robot wallet: a zero-endurance robot must be able to
// recover unattended even after it has earned no gold.
func (s *Service) RestoreRobotExpertMachine(ctx context.Context, accountID string, characterID uint16) (dnfrobot.ExpertMachineState, error) {
	if s == nil || characterID == 0 {
		return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrCharacterRequired
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dnfrobot.ExpertMachineState{}, dnfexpertjob.ErrOwnerUnavailable
	}
	owner, err := dnfexpertjob.NewOwner(repositories)
	if err != nil {
		return dnfrobot.ExpertMachineState{}, err
	}
	catalog, err := s.currentRobotExpertJobCatalog()
	if err != nil {
		return dnfrobot.ExpertMachineState{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var result dnfrobot.ExpertMachineState
	characterIDString := strconv.Itoa(int(characterID))
	err = owner.Machine(ctx, dnfexpertjob.Command{
		AccountID: accountID, CharacterID: characterIDString,
		UpdatedAt: time.Now().UTC(),
		Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
			state, stateErr := currentRobotExpertMachineState(*assets.Character, catalog)
			if stateErr != nil {
				return dnfexpertjob.Changes{}, stateErr
			}
			if assets.Character.Stats == nil {
				assets.Character.Stats = make(map[string]int64)
			}
			assets.Character.Stats[currentExpertJobMachineGradeStat] = int64(state.Grade)
			assets.Character.Stats[currentExpertJobMachineEnduranceStat] = state.MaxEndurance
			result = state
			result.Endurance = state.MaxEndurance
			return dnfexpertjob.Changes{Character: true}, nil
		},
	})
	if err != nil {
		return dnfrobot.ExpertMachineState{}, fmt.Errorf("restore robot expert machine: %w", err)
	}
	return result, nil
}
