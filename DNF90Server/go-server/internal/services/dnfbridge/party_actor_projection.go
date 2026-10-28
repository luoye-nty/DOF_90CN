package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

type currentPartyActorProjection struct {
	characterID  uint16
	generation   uint64
	ownerServer  byte
	ownerChannel byte
	mode0        []byte
	mode1        []byte
}

func validateCurrentPartyActorProjectionRows(
	characterID uint16,
	mode0 []byte,
	mode1 []byte,
) (byte, byte, error) {
	if characterID == 0 {
		return 0, 0, errors.New("party actor projection character is unavailable")
	}
	if len(mode0) < 78 || mode0[0] != 0 ||
		binary.LittleEndian.Uint16(mode0[1:3]) != 1 ||
		binary.LittleEndian.Uint16(mode0[76:78]) != characterID {
		return 0, 0, fmt.Errorf("party actor mode0 row for character %d is invalid", characterID)
	}
	// The current complete actor-binding row is never shorter than the fixed
	// mode1 base. A light identity prefix is insufficient: the peer must receive
	// the owner's accepted appearance/equipment state that its actor was built
	// from before reliable-UDP state starts.
	if len(mode1) < currentMode1BaseWireSize || mode1[0] != 1 ||
		binary.LittleEndian.Uint16(mode1[1:3]) != 1 ||
		binary.LittleEndian.Uint16(mode1[21:23]) != characterID {
		return 0, 0, fmt.Errorf("party actor mode1 row for character %d is incomplete", characterID)
	}
	if mode0[3] != mode1[3] || mode0[4] != mode1[4] {
		return 0, 0, fmt.Errorf("party actor owner for character %d changed between mode0 and mode1", characterID)
	}
	return mode0[3], mode0[4], nil
}

func captureCurrentPartyActorProjection(
	session *gameSession,
	characterID uint16,
	mode0 []byte,
	mode1 []byte,
) error {
	if session == nil || characterID == 0 ||
		session.selectedCharacterID != characterID ||
		session.characterGeneration == 0 {
		return errors.New("party actor projection session identity is unavailable")
	}
	ownerServer, ownerChannel, err := validateCurrentPartyActorProjectionRows(
		characterID,
		mode0,
		mode1,
	)
	if err != nil {
		return err
	}
	session.partyActorProjectionMu.Lock()
	session.partyActorProjection = currentPartyActorProjection{
		characterID:  characterID,
		generation:   session.characterGeneration,
		ownerServer:  ownerServer,
		ownerChannel: ownerChannel,
		mode0:        append([]byte(nil), mode0...),
		mode1:        append([]byte(nil), mode1...),
	}
	session.partyActorProjectionMu.Unlock()
	return nil
}

func currentPartyActorProjectionSnapshot(
	session *gameSession,
	characterID uint16,
	generation uint64,
) (currentPartyActorProjection, bool) {
	if session == nil || characterID == 0 || generation == 0 {
		return currentPartyActorProjection{}, false
	}
	session.partyActorProjectionMu.RLock()
	projection := session.partyActorProjection
	projection.mode0 = append([]byte(nil), projection.mode0...)
	projection.mode1 = append([]byte(nil), projection.mode1...)
	session.partyActorProjectionMu.RUnlock()
	if projection.characterID != characterID || projection.generation != generation {
		return currentPartyActorProjection{}, false
	}
	if _, _, err := validateCurrentPartyActorProjectionRows(
		characterID,
		projection.mode0,
		projection.mode1,
	); err != nil {
		return currentPartyActorProjection{}, false
	}
	return projection, true
}

// refreshCurrentPartyActorProjectionMode1FromDurableState rebuilds the selected
// actor's complete equipment-bearing mode1 row from the current durable
// character/skill state and refreshes the cached party actor projection in
// place. The cached mode0 row is reused unchanged: mode1 is the channel through
// which peers learn the owner's skill table, and a skill-page tail changed by a
// town skill edit otherwise stays frozen in the previous scene snapshot until
// the next scene transition, so a teammate entering a dungeon after the edit
// sees the previous session's levels. A refresh failure keeps the previous
// projection so callers such as party dungeon admission can still proceed.
func (s *Service) refreshCurrentPartyActorProjectionMode1FromDurableState(
	ctx context.Context,
	session *gameSession,
) bool {
	if s == nil || session == nil {
		return false
	}
	characterID := session.selectedCharacterID
	if characterID == 0 || session.characterGeneration == 0 {
		return false
	}
	projection, found := currentPartyActorProjectionSnapshot(
		session,
		characterID,
		session.characterGeneration,
	)
	if !found {
		return false
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil || repos.LegacyUserInfo == nil {
		return false
	}
	refreshCtx, cancel := context.WithTimeout(ctx, createWriteTimeout)
	defer cancel()
	record, hasCharacter, err := repos.Character.Load(
		refreshCtx,
		fmt.Sprintf("%d", characterID),
	)
	if err != nil || !hasCharacter {
		return false
	}
	mode1Body := s.buildCurrentSelectedUserInfoMode1BodyInContext(
		refreshCtx,
		session,
		repos.LegacyUserInfo,
		record,
		hasCharacter,
		characterID,
		projection.ownerChannel,
	)
	if len(mode1Body) == 0 {
		return false
	}
	if err := captureCurrentPartyActorProjection(
		session,
		characterID,
		projection.mode0,
		mode1Body,
	); err != nil {
		s.logGameEvent(session, "game-party-actor-mode1-durable-refresh-rejected",
			"char_id", characterID,
			"character_generation", session.characterGeneration,
			"mode0_len", len(projection.mode0),
			"mode1_len", len(mode1Body),
			"reason", err)
		return false
	}
	return true
}
