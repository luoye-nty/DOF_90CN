package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDungeonAltarRankingMsgID             uint16 = 208
	currentDungeonAltarRewardMsgID              uint16 = 209
	currentDungeonAltarMonsterSpawnMsgID        uint16 = 210
	currentDungeonAltarStartMapMsgID            uint16 = 211
	currentDungeonAltarCrazyMonsterMsgID        uint16 = 213
	currentDungeonAltarRoundIntervalMsgID       uint16 = 215
	currentDungeonAltarDifficultyUIMsgID        uint16 = 297
	currentDungeonAltarMaximumDeathSequences           = 100
	currentDungeonAltarDifficultyTimeoutSeconds        = 10
)

func currentDungeonStartMapNotificationForRuntime(runtime *runtimeDungeonState) uint16 {
	if runtime != nil && runtime.altar != nil {
		return currentDungeonAltarStartMapMsgID
	}
	return currentDungeonStartNotification
}

var errCurrentDungeonAltarRequestMalformed = errors.New("current dungeon altar request is malformed")

type currentDungeonAltarRewardEntry struct {
	ItemID uint32
	Amount uint32
}

func decodeCurrentDungeonAltarDeathSequences(body []byte) ([]uint16, error) {
	if len(body) < 1 {
		return nil, fmt.Errorf("%w: op255 count byte is missing", errCurrentDungeonAltarRequestMalformed)
	}
	count := int(body[0])
	if count < 1 || count > currentDungeonAltarMaximumDeathSequences || len(body) != 1+count*2 {
		return nil, fmt.Errorf("%w: op255 count=%d body_len=%d", errCurrentDungeonAltarRequestMalformed, count, len(body))
	}
	seen := make(map[uint16]struct{}, count)
	sequences := make([]uint16, 0, count)
	for index := 0; index < count; index++ {
		sequence := binary.LittleEndian.Uint16(body[1+index*2 : 3+index*2])
		if sequence == 0 {
			return nil, fmt.Errorf("%w: op255 sequence[%d]=0", errCurrentDungeonAltarRequestMalformed, index)
		}
		if _, duplicate := seen[sequence]; duplicate {
			return nil, fmt.Errorf("%w: op255 duplicate sequence=%d", errCurrentDungeonAltarRequestMalformed, sequence)
		}
		seen[sequence] = struct{}{}
		sequences = append(sequences, sequence)
	}
	return sequences, nil
}

func decodeCurrentDungeonAltarDifficulty(body []byte) (worldmap.BloodAltarDifficulty, error) {
	if len(body) != 1 || (body[0] != 1 && body[0] != 2) {
		return "", fmt.Errorf("%w: op317 body=%x", errCurrentDungeonAltarRequestMalformed, body)
	}
	if body[0] == 1 {
		return worldmap.BloodAltarDifficultyA, nil
	}
	return worldmap.BloodAltarDifficultyB, nil
}

// buildCurrentDungeonAltarStartMapBody is the current EXE's class0/op211
// reader contract. A first visit consumes the complete 15-byte minimal form:
// base(8), u32 map value, empty 21-byte list, empty 20-byte list, final flag.
// A revisit consumes only the eight-byte mode-0 base.
func buildCurrentDungeonAltarStartMapBody(
	scene worldmap.DungeonRoomScene,
	seed uint32,
	revisit bool,
) ([]byte, error) {
	if scene.Coordinate.X < 0 || scene.Coordinate.X > math.MaxUint8 ||
		scene.Coordinate.Y < 0 || scene.Coordinate.Y > math.MaxUint8 ||
		scene.Map.Map.ID <= 0 || scene.Map.Map.ID > math.MaxUint32 {
		return nil, fmt.Errorf("%w: op211 room=%s map=%d", errCurrentDungeonAltarDefinitionInvalid, scene.Coordinate, scene.Map.Map.ID)
	}
	writer := packetWriter{}
	writer.writeByte(byte(scene.Coordinate.X))
	writer.writeByte(byte(scene.Coordinate.Y))
	writer.writeUint32(seed)
	writer.writeByte(0)
	if revisit {
		writer.writeByte(0)
		return writer.bytes(), nil
	}
	writer.writeByte(1)
	writer.writeUint32(uint32(scene.Map.Map.ID))
	writer.writeByte(0)
	writer.writeByte(0)
	writer.writeByte(0)
	return writer.bytes(), nil
}

func buildCurrentDungeonAltarMonsterSpawnBody(wave currentDungeonAltarWave) ([]byte, error) {
	if len(wave.Spawns) == 0 || len(wave.Spawns) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: op210 spawn_count=%d", errCurrentDungeonAltarDefinitionInvalid, len(wave.Spawns))
	}
	writer := packetWriter{}
	writer.writeUint16(uint16(len(wave.Spawns)))
	for _, spawn := range wave.Spawns {
		if spawn.Sequence == 0 || spawn.MonsterCode == 0 {
			return nil, fmt.Errorf("%w: op210 sequence=%d monster=%d", errCurrentDungeonAltarDefinitionInvalid, spawn.Sequence, spawn.MonsterCode)
		}
		writer.writeByte(spawn.Variant)
		writer.writeUint16(spawn.Sequence)
		writer.writeUint32(spawn.MonsterCode)
		writer.writeByte(spawn.MonsterType)
		writer.writeByte(spawn.Level)
		writer.writeUint16(spawn.Scale)
		writer.writeUint16(spawn.X)
		writer.writeUint16(spawn.Y)
		writer.writeUint16(spawn.Z)
	}
	writer.writeUint16(wave.Tail)
	return writer.bytes(), nil
}

func buildCurrentDungeonAltarCrazyMonsterBody(monster currentDungeonAltarCrazyMonster) ([]byte, error) {
	if monster.MonsterCode == 0 || monster.Variant > 18 {
		return nil, fmt.Errorf("%w: op213 monster=%d variant=%d", errCurrentDungeonAltarDefinitionInvalid, monster.MonsterCode, monster.Variant)
	}
	writer := packetWriter{}
	writer.writeUint32(monster.MonsterCode)
	writer.writeByte(monster.Variant)
	return writer.bytes(), nil
}

func buildCurrentDungeonAltarRoundIntervalBody(globalRound int, intervalMilliseconds int64) []byte {
	if globalRound < 0 {
		globalRound = 0
	}
	if globalRound >= math.MaxUint8 {
		globalRound = math.MaxUint8 - 1
	}
	if intervalMilliseconds < 0 {
		intervalMilliseconds = 0
	}
	if intervalMilliseconds > math.MaxUint32 {
		intervalMilliseconds = math.MaxUint32
	}
	writer := packetWriter{}
	writer.writeByte(byte(globalRound + 1))
	writer.writeUint32(uint32(intervalMilliseconds))
	return writer.bytes()
}

func buildCurrentDungeonAltarDifficultyPromptBody(completedRounds int, timeoutSeconds int) []byte {
	if completedRounds < 0 {
		completedRounds = 0
	}
	if completedRounds > math.MaxUint8 {
		completedRounds = math.MaxUint8
	}
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}
	if timeoutSeconds > math.MaxUint16 {
		timeoutSeconds = math.MaxUint16
	}
	writer := packetWriter{}
	writer.writeByte(0)
	writer.writeByte(byte(completedRounds))
	writer.writeUint16(uint16(timeoutSeconds))
	return writer.bytes()
}

func buildCurrentDungeonAltarDifficultyResolvedBody(difficulty worldmap.BloodAltarDifficulty) ([]byte, error) {
	switch difficulty {
	case worldmap.BloodAltarDifficultyA:
		return []byte{1}, nil
	case worldmap.BloodAltarDifficultyB:
		return []byte{2}, nil
	default:
		return nil, fmt.Errorf("%w: op297 difficulty=%q", errCurrentDungeonAltarDefinitionInvalid, difficulty)
	}
}

// buildCurrentDungeonAltarRankingBody follows the current EXE's op208 wire
// reader. sub_1D38CF0 reads six u32 values and passes them to sub_330E910 as
// (wire2, wire1, wire4, wire3, wire5, wire6), where the second/fourth values
// are the round counters and the first/third values are formatted as times.
func buildCurrentDungeonAltarRankingBody(
	currentRound uint32,
	playTimeMilliseconds uint32,
	bestRound uint32,
	bestTimeMilliseconds uint32,
	maxRound uint32,
	rewardExperience uint32,
) []byte {
	writer := packetWriter{}
	writer.writeUint32(playTimeMilliseconds)
	writer.writeUint32(currentRound)
	writer.writeUint32(bestTimeMilliseconds)
	writer.writeUint32(bestRound)
	writer.writeUint32(maxRound)
	writer.writeUint32(rewardExperience)
	return writer.bytes()
}

// buildCurrentDungeonAltarRewardBody keeps op209's four independent reward
// groups explicit. Keeping the proven cursor in one builder prevents the
// older one-group shape from leaking into the dedicated altar settlement.
func buildCurrentDungeonAltarRewardBody(
	currentRound byte,
	maxRound byte,
	groups [4][]currentDungeonAltarRewardEntry,
) ([]byte, error) {
	writer := packetWriter{}
	writer.writeByte(currentRound)
	writer.writeByte(maxRound)
	for groupIndex, group := range groups {
		if len(group) > math.MaxUint8 {
			return nil, fmt.Errorf("%w: op209 group=%d count=%d", errCurrentDungeonAltarDefinitionInvalid, groupIndex, len(group))
		}
		writer.writeByte(byte(len(group)))
		for _, entry := range group {
			writer.writeUint32(entry.ItemID)
			writer.writeUint32(entry.Amount)
		}
	}
	return writer.bytes(), nil
}
