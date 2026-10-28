package dnfbridge

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"math/big"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	// The compatibility DLL multiplexes its private C2S controls through the
	// intentional op1456 enum hole. "BMU1" cannot collide with the existing
	// "ACK1" capability announcement.
	currentDungeonBossMutationMagic         = uint32(0x31554D42) // "BMU1"
	currentDungeonBossMutationVersion       = byte(1)
	currentDungeonBossMutationRequestLength = 16

	// Class0/op413 is already a DLL-private, exact-length-gated projection
	// channel. Type 3 is consumed by 90CN.dll before the native dispatcher.
	currentDungeonBossMutationResponseMsgID   = uint16(413)
	currentDungeonBossMutationResponseType    = byte(3)
	currentDungeonBossMutationResponseVersion = byte(1)
	currentDungeonBossMutationResponseLength  = 12
)

const (
	currentDungeonBossMutationReasonAllowed byte = iota
	currentDungeonBossMutationReasonLotteryMiss
	currentDungeonBossMutationReasonRuntimeMissing
	currentDungeonBossMutationReasonNotOrdinaryDungeon
	currentDungeonBossMutationReasonSceneMismatch
	currentDungeonBossMutationReasonTargetUnknown
	currentDungeonBossMutationReasonTargetMismatch
	currentDungeonBossMutationReasonNotBoss
	currentDungeonBossMutationReasonTargetInactive
	currentDungeonBossMutationReasonAlreadyAttempted
	currentDungeonBossMutationReasonRandomUnavailable
)

type currentDungeonBossMutationRequest struct {
	Token     uint16
	ObjectKey uint16
	MonsterID uint32
}

type currentDungeonBossMutationDecision struct {
	Token     uint16
	MonsterID uint32
	ObjectKey uint16
	Allowed   bool
	Reason    byte
}

func isCurrentDungeonBossMutationEnvelope(body []byte) bool {
	return len(body) >= 4 && binary.LittleEndian.Uint32(body[:4]) == currentDungeonBossMutationMagic
}

func parseCurrentDungeonBossMutationRequest(body []byte) (currentDungeonBossMutationRequest, bool) {
	if len(body) != currentDungeonBossMutationRequestLength ||
		binary.LittleEndian.Uint32(body[0:4]) != currentDungeonBossMutationMagic ||
		body[4] != currentDungeonBossMutationVersion || body[5] != 0 ||
		binary.LittleEndian.Uint16(body[10:12]) != 0 {
		return currentDungeonBossMutationRequest{}, false
	}
	request := currentDungeonBossMutationRequest{
		Token:     binary.LittleEndian.Uint16(body[6:8]),
		ObjectKey: binary.LittleEndian.Uint16(body[8:10]),
		MonsterID: binary.LittleEndian.Uint32(body[12:16]),
	}
	if request.Token == 0 || request.ObjectKey == 0 || request.MonsterID == 0 {
		return currentDungeonBossMutationRequest{}, false
	}
	return request, true
}

func buildCurrentDungeonBossMutationDecisionBody(decision currentDungeonBossMutationDecision) []byte {
	body := make([]byte, currentDungeonBossMutationResponseLength)
	body[0] = currentDungeonBossMutationResponseType
	body[1] = currentDungeonBossMutationResponseVersion
	binary.LittleEndian.PutUint16(body[2:4], decision.Token)
	binary.LittleEndian.PutUint32(body[4:8], decision.MonsterID)
	binary.LittleEndian.PutUint16(body[8:10], decision.ObjectKey)
	if decision.Allowed {
		body[10] = 1
	}
	body[11] = decision.Reason
	return body
}

func currentDungeonBossMutationRoll() (uint32, error) {
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(10))
	if err != nil {
		return 0, err
	}
	return uint32(value.Uint64()), nil
}

func (r *runtimeDungeonRoom) resolveBossMutation(
	objectKey uint32,
	monsterID int64,
	roll uint32,
) (runtimeDungeonMonster, bool, byte) {
	if r == nil {
		return runtimeDungeonMonster{}, false, currentDungeonBossMutationReasonRuntimeMissing
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index, ok := r.byObjectKey[objectKey]
	if !ok || index < 0 || index >= len(r.monsters) {
		return runtimeDungeonMonster{}, false, currentDungeonBossMutationReasonTargetUnknown
	}
	monster := &r.monsters[index]
	if monster.ObjectKey != objectKey || monster.Spawn.MonsterID != monsterID {
		return *monster, false, currentDungeonBossMutationReasonTargetMismatch
	}
	if monster.State != runtimeDungeonMonsterAnnounced {
		return *monster, false, currentDungeonBossMutationReasonTargetInactive
	}
	if !currentDungeonOrdinaryMonsterLooksBoss(*monster) {
		return *monster, false, currentDungeonBossMutationReasonNotBoss
	}
	if monster.BossMutationAttempted {
		return *monster, false, currentDungeonBossMutationReasonAlreadyAttempted
	}
	monster.BossMutationAttempted = true
	monster.BossMutationAllowed = roll == 0
	if monster.BossMutationAllowed {
		return *monster, true, currentDungeonBossMutationReasonAllowed
	}
	return *monster, false, currentDungeonBossMutationReasonLotteryMiss
}

func (s *Service) handleCurrentDungeonBossMutation(
	session *gameSession,
	classification byte,
	body []byte,
) error {
	if session == nil || classification != dnfproto.DefaultChannelClassification {
		if session != nil {
			s.logGameEvent(session, "game-dungeon-boss-mutation-rejected",
				"classification", classification,
				"body_len", len(body),
				"reason", "class1_private_request_required")
		}
		return nil
	}
	request, ok := parseCurrentDungeonBossMutationRequest(body)
	if !ok {
		s.logGameEvent(session, "game-dungeon-boss-mutation-rejected",
			"classification", classification,
			"body_len", len(body),
			"reason", "invalid_bmu1_envelope")
		return nil
	}

	decision := currentDungeonBossMutationDecision{
		Token:     request.Token,
		MonsterID: request.MonsterID,
		ObjectKey: request.ObjectKey,
		Reason:    currentDungeonBossMutationReasonRuntimeMissing,
	}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime != nil && runtime.Session != nil && runtime.Room != nil &&
		dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		if runtime.raidDungeon.RaidKey != 0 || runtime.tournament != nil {
			decision.Reason = currentDungeonBossMutationReasonNotOrdinaryDungeon
		} else if scene, sceneOK := runtime.Session.Scene(); !sceneOK ||
			scene.Coordinate != runtime.Room.coordinate ||
			scene.Map.Map.ID != runtime.Room.mapID {
			decision.Reason = currentDungeonBossMutationReasonSceneMismatch
		} else if roll, err := currentDungeonBossMutationRoll(); err != nil {
			decision.Reason = currentDungeonBossMutationReasonRandomUnavailable
		} else {
			monster, allowed, reason := runtime.Room.resolveBossMutation(
				uint32(request.ObjectKey), int64(request.MonsterID), roll)
			decision.Allowed = allowed
			decision.Reason = reason
			if monster.ObjectKey != 0 {
				decision.ObjectKey = uint16(monster.ObjectKey)
			}
		}
	}
	session.dungeon.mu.Unlock()

	s.logGameEvent(session, "game-dungeon-boss-mutation-decision",
		"token", decision.Token,
		"object_key", decision.ObjectKey,
		"monster_id", decision.MonsterID,
		"allowed", decision.Allowed,
		"reason_code", decision.Reason)
	return s.sendGameUpperRawClass(
		session,
		currentDungeonBossMutationResponseMsgID,
		buildCurrentDungeonBossMutationDecisionBody(decision),
		0,
	)
}
