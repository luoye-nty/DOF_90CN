package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// GMRobotCharacterProvisionRequest creates one real, repository-backed DNF
// character for the in-process robot manager. It deliberately enters through
// the same PVF initialization and aggregate transaction as native op4
// character creation; the robot manager never writes legacy SQL tables.
type GMRobotCharacterProvisionRequest struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Job       int    `json:"job"`
	GrowType  int    `json:"grow_type"`
	Level     int    `json:"level"`
	Village   int    `json:"village"`
	Area      int    `json:"area"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
}

type GMRobotCharacterProvisionResult struct {
	CharacterID uint16 `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Slot        int    `json:"slot"`
	Job         int    `json:"job"`
	GrowType    int    `json:"grow_type"`
	Level       int    `json:"level"`
}

func (s *Service) GMProvisionRobotCharacter(ctx context.Context, request GMRobotCharacterProvisionRequest) (GMRobotCharacterProvisionResult, error) {
	if s == nil {
		return GMRobotCharacterProvisionResult{}, ErrGMRepositoryUnavailable
	}
	request.AccountID = strings.TrimSpace(request.AccountID)
	request.Name = strings.TrimSpace(request.Name)
	if request.AccountID == "" || len(request.AccountID) > 96 || strings.IndexByte(request.AccountID, 0) >= 0 ||
		request.Job < 0 || request.Job > 15 || request.GrowType < 0 || request.GrowType > 0xff ||
		request.Level < 1 || request.Level > currentAdventureCharacterLevelCap ||
		request.Village < 0 || request.Village > 0xff || request.Area < 0 || request.Area > 0xff ||
		request.X < 0 || request.X > 0xffff || request.Y < 0 || request.Y > 0xffff {
		return GMRobotCharacterProvisionResult{}, ErrGMRequestInvalid
	}
	encodedName, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(request.Name))
	if err != nil {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("%w: encode character name: %v", ErrGMRequestInvalid, err)
	}
	name, err := decodeValidatedCharacterName(encodedName)
	if err != nil || name != request.Name {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("%w: character name: %v", ErrGMRequestInvalid, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, createWriteTimeout)
	defer cancel()

	s.characterCreateMu.Lock()
	defer s.characterCreateMu.Unlock()
	repositories, available := s.repositoryGroup()
	if !available || repositories.Character == nil || repositories.CharacterCreate == nil {
		return GMRobotCharacterProvisionResult{}, ErrGMRepositoryUnavailable
	}
	existing, slotLimit, err := s.listCharactersWithLimit(ctx, repositories, request.AccountID)
	if err != nil {
		return GMRobotCharacterProvisionResult{}, err
	}
	if len(existing) >= slotLimit {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("%w: account character slots are full", ErrGMRequestInvalid)
	}
	duplicated, err := s.characterNameExists(ctx, repositories, existing, name)
	if err != nil {
		return GMRobotCharacterProvisionResult{}, err
	}
	if duplicated {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("%w: character name already exists", ErrGMRequestInvalid)
	}
	characterID, err := s.nextCharacterID(ctx, repositories, existing)
	if err != nil {
		return GMRobotCharacterProvisionResult{}, err
	}
	if characterID <= 0 || characterID > upperMaxCharacter {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("%w: character id %d exceeds current protocol", ErrGMRequestInvalid, characterID)
	}
	experience, err := s.currentPVFLevelEntryExperience(ctx, request.Level)
	if err != nil {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("resolve level %d experience: %w", request.Level, err)
	}
	stats := defaultCreatedCharacterStatsFromRequest(createCharacterRequest{})
	stats["grow_type"] = int64(request.GrowType)
	stats["exp"] = int64(experience)
	// Autonomous town actors must not enter the native first-character
	// tutorial. They have no dungeon-driving state machine, and their requested
	// town coordinates are already validated above.
	stats[currentDungeonTutorialCompletedKey] = currentDungeonTutorialCompleteFlag
	stats["tutorial_reward_progress_38"] = 1
	// Automatic fleet composition advances this durable marker only after the
	// authoritative main-story closure commits in the same settlement. Keeping
	// it at zero here makes a freshly provisioned level-cap robot enter that
	// migration exactly once; unknown ad-hoc Stats are not durable in MySQL.
	stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] = 0
	stats[dnfrepo.CharacterStoryDigestMigrationVersionStatKey] = 0
	stats["ex_equip_slot_stat"] = gmAllEquipSlotsMask
	if request.Village != 0 {
		stats["town_id"] = int64(request.Village)
	}
	stats["area_id"] = int64(request.Area)
	stats["pos_x"] = int64(request.X)
	stats["pos_y"] = int64(request.Y)
	now := time.Now().UTC()
	slot := nextCharacterSlot(existing, slotLimit)
	if slot < 0 {
		return GMRobotCharacterProvisionResult{}, fmt.Errorf("%w: account character slots are full", ErrGMRequestInvalid)
	}
	record := dnfrepo.CharacterRecord{
		CharacterID: strconv.Itoa(characterID),
		AccountID:   request.AccountID,
		Slot:        slot,
		Name:        name,
		Job:         strconv.Itoa(request.Job),
		Level:       request.Level,
		Stats:       stats,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.saveNewCharacter(ctx, repositories, record); err != nil {
		return GMRobotCharacterProvisionResult{}, err
	}
	return GMRobotCharacterProvisionResult{
		CharacterID: uint16(characterID), AccountID: request.AccountID, Name: name,
		Slot: slot, Job: request.Job, GrowType: request.GrowType, Level: request.Level,
	}, nil
}
