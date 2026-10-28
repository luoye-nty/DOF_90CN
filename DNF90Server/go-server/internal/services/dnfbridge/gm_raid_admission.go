package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
)

const (
	gmRaidAdmissionRaidAnton    = "anton"
	gmRaidAdmissionRaidLuke     = "luke"
	gmRaidAdmissionRaidFiendWar = "fiend_war"

	gmRaidAdmissionAccountDaily    = "account_daily"
	gmRaidAdmissionAccountWeekly   = "account_weekly"
	gmRaidAdmissionCharacterWeekly = "character_weekly"
)

type GMRaidAdmissionCounter struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	Period    string `json:"period"`
	PolicyKey string `json:"policy_key"`
	Used      uint32 `json:"used"`
	Limit     uint32 `json:"limit"`
	Remaining uint32 `json:"remaining"`
	Bonus     uint32 `json:"bonus"`
}

type GMRaidAdmissionGroup struct {
	Raid     string                   `json:"raid"`
	Name     string                   `json:"name"`
	Counters []GMRaidAdmissionCounter `json:"counters"`
}

type GMRaidAdmissionState struct {
	CharacterID   string                 `json:"character_id"`
	CharacterName string                 `json:"character_name"`
	AccountID     string                 `json:"account_id"`
	Groups        []GMRaidAdmissionGroup `json:"groups"`
}

type GMRaidAdmissionResetResult struct {
	State        GMRaidAdmissionState `json:"state"`
	ResetCounter string               `json:"reset_counter"`
	Online       bool                 `json:"online"`
	Refreshed    bool                 `json:"refreshed"`
	RefreshError string               `json:"refresh_error,omitempty"`
}

type gmRaidAdmissionCounterDefinition struct {
	RaidName string
	Raid     string
	ID       string
	Label    string
	Scope    string
	Period   string
	Policy   string
}

var gmRaidAdmissionCounterDefinitions = []gmRaidAdmissionCounterDefinition{
	{RaidName: "安徒恩", Raid: gmRaidAdmissionRaidAnton, ID: gmRaidAdmissionAccountDaily, Label: "账号日入场次数", Scope: "account", Period: "daily", Policy: currentRaidAntonDailyPolicyKey},
	{RaidName: "安徒恩", Raid: gmRaidAdmissionRaidAnton, ID: gmRaidAdmissionAccountWeekly, Label: "账号周入场次数", Scope: "account", Period: "weekly", Policy: dnfdungeonadmission.AntonWeeklyAccountPolicyKey},
	{RaidName: "安徒恩", Raid: gmRaidAdmissionRaidAnton, ID: gmRaidAdmissionCharacterWeekly, Label: "角色周入场次数", Scope: "character", Period: "weekly", Policy: dnfdungeonadmission.AntonWeeklyCharacterPolicyKey},
	{RaidName: "卢克", Raid: gmRaidAdmissionRaidLuke, ID: gmRaidAdmissionAccountDaily, Label: "账号日入场次数", Scope: "account", Period: "daily", Policy: currentRaidLukeDailyPolicyKey},
	{RaidName: "卢克", Raid: gmRaidAdmissionRaidLuke, ID: gmRaidAdmissionAccountWeekly, Label: "账号周入场次数", Scope: "account", Period: "weekly", Policy: dnfdungeonadmission.LukeWeeklyAccountPolicyKey},
	{RaidName: "卢克", Raid: gmRaidAdmissionRaidLuke, ID: gmRaidAdmissionCharacterWeekly, Label: "角色周入场次数", Scope: "character", Period: "weekly", Policy: dnfdungeonadmission.LukeWeeklyCharacterPolicyKey},
	{RaidName: "超时空", Raid: gmRaidAdmissionRaidFiendWar, ID: gmRaidAdmissionAccountDaily, Label: "账号日入场次数", Scope: "account", Period: "daily", Policy: currentRaidFiendWarDailyPolicyKey},
	{RaidName: "超时空", Raid: gmRaidAdmissionRaidFiendWar, ID: gmRaidAdmissionAccountWeekly, Label: "账号周入场次数", Scope: "account", Period: "weekly", Policy: dnfdungeonadmission.FiendWarWeeklyAccountPolicyKey},
	{RaidName: "超时空", Raid: gmRaidAdmissionRaidFiendWar, ID: gmRaidAdmissionCharacterWeekly, Label: "角色周入场次数", Scope: "character", Period: "weekly", Policy: dnfdungeonadmission.FiendWarWeeklyCharacterPolicyKey},
}

// GMRaidAdmissionCounters returns the live admission usage for all three raids.
// The UI uses this to show the exact counters a reset will affect before the
// operator presses one of the reset buttons.
func (s *Service) GMRaidAdmissionCounters(ctx context.Context, characterID string) (GMRaidAdmissionState, error) {
	if s == nil {
		return GMRaidAdmissionState{}, errors.New("dnf90 GM service is unavailable")
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return GMRaidAdmissionState{}, errors.New("character_id is required")
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return GMRaidAdmissionState{}, errors.New("dnf90 repository is unavailable")
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMRaidAdmissionState{}, err
	}
	if !found {
		return GMRaidAdmissionState{}, fmt.Errorf("character %s not found", characterID)
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return GMRaidAdmissionState{}, err
	}
	now := s.gameplayNow()
	state := GMRaidAdmissionState{
		CharacterID:   characterID,
		CharacterName: character.Name,
		AccountID:     character.AccountID,
		Groups:        make([]GMRaidAdmissionGroup, 0, 3),
	}
	groupIndex := make(map[string]int, 3)
	for _, definition := range gmRaidAdmissionCounterDefinitions {
		policy, found, err := s.currentDungeonAdmissionPolicy(definition.Policy)
		if err != nil {
			return GMRaidAdmissionState{}, err
		}
		if !found {
			return GMRaidAdmissionState{}, fmt.Errorf("raid admission policy %s is unavailable", definition.Policy)
		}
		snapshot, ok := currentRaidDailyAvailabilitySnapshot(ctx, owner, characterID, policy, now)
		if !ok {
			return GMRaidAdmissionState{}, fmt.Errorf("raid admission policy %s could not be read", definition.Policy)
		}
		index, found := groupIndex[definition.Raid]
		if !found {
			index = len(state.Groups)
			groupIndex[definition.Raid] = index
			state.Groups = append(state.Groups, GMRaidAdmissionGroup{
				Raid:     definition.Raid,
				Name:     definition.RaidName,
				Counters: make([]GMRaidAdmissionCounter, 0, 3),
			})
		}
		state.Groups[index].Counters = append(state.Groups[index].Counters, GMRaidAdmissionCounter{
			ID:        definition.ID,
			Label:     definition.Label,
			Scope:     definition.Scope,
			Period:    definition.Period,
			PolicyKey: definition.Policy,
			Used:      snapshot.Used,
			Limit:     snapshot.EffectiveLimit,
			Remaining: snapshot.Remaining,
			Bonus:     snapshot.Bonus,
		})
	}
	return state, nil
}

// GMResetRaidAdmissionCounter resets exactly one raid/account/character policy.
// Account policies are intentionally labelled in the web UI because resetting
// one also restores that count for every character on the same account.
func (s *Service) GMResetRaidAdmissionCounter(
	ctx context.Context,
	characterID, raid, counter string,
) (GMRaidAdmissionResetResult, error) {
	if s == nil {
		return GMRaidAdmissionResetResult{}, errors.New("dnf90 GM service is unavailable")
	}
	definition, found := gmRaidAdmissionDefinition(raid, counter)
	if !found {
		return GMRaidAdmissionResetResult{}, fmt.Errorf("unsupported raid counter raid=%q counter=%q", raid, counter)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return GMRaidAdmissionResetResult{}, errors.New("dnf90 repository is unavailable")
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return GMRaidAdmissionResetResult{}, err
	}
	policy, found, err := s.currentDungeonAdmissionPolicy(definition.Policy)
	if err != nil {
		return GMRaidAdmissionResetResult{}, err
	}
	if !found {
		return GMRaidAdmissionResetResult{}, fmt.Errorf("raid admission policy %s is unavailable", definition.Policy)
	}
	now := s.gameplayNow()
	if _, err := owner.Reset(ctx, strings.TrimSpace(characterID), []dnfdungeonadmission.Policy{policy}, now); err != nil {
		return GMRaidAdmissionResetResult{}, err
	}
	online, refreshed, refreshErr := s.refreshGMRaidAdmissionCounters(characterID)
	state, err := s.GMRaidAdmissionCounters(ctx, characterID)
	if err != nil {
		return GMRaidAdmissionResetResult{}, err
	}
	result := GMRaidAdmissionResetResult{
		State:        state,
		ResetCounter: definition.Raid + "/" + definition.ID,
		Online:       online,
		Refreshed:    refreshed,
	}
	if refreshErr != nil {
		result.RefreshError = refreshErr.Error()
	}
	return result, nil
}

func gmRaidAdmissionDefinition(raid, counter string) (gmRaidAdmissionCounterDefinition, bool) {
	raid = strings.ToLower(strings.TrimSpace(raid))
	counter = strings.ToLower(strings.TrimSpace(counter))
	for _, definition := range gmRaidAdmissionCounterDefinitions {
		if definition.Raid == raid && definition.ID == counter {
			return definition, true
		}
	}
	return gmRaidAdmissionCounterDefinition{}, false
}

func (s *Service) refreshGMRaidAdmissionCounters(characterID string) (online, refreshed bool, refreshErr error) {
	if s == nil {
		return false, false, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(characterID), 10, 16)
	if err != nil {
		return false, false, nil
	}
	session, online := s.onlineGameSession(uint16(parsed))
	if !online || session == nil {
		return false, false, nil
	}
	dailyErr := s.sendCurrentRaidDailyAvailability(session, "gm_raid_counter_reset")
	weeklyErr := s.sendCurrentRaidWeeklySchedules(session, "gm_raid_counter_reset")
	refreshErr = errors.Join(dailyErr, weeklyErr)
	return true, refreshErr == nil, refreshErr
}
