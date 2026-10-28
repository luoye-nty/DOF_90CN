package dnfbridge

import (
	"encoding/binary"
	"testing"

	dnfmonster "longheng.io/server/internal/modules/dnf/monster"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func buildCurrentDungeonBossMutationTestRequest(token, objectKey uint16, monsterID uint32) []byte {
	body := make([]byte, currentDungeonBossMutationRequestLength)
	binary.LittleEndian.PutUint32(body[0:4], currentDungeonBossMutationMagic)
	body[4] = currentDungeonBossMutationVersion
	binary.LittleEndian.PutUint16(body[6:8], token)
	binary.LittleEndian.PutUint16(body[8:10], objectKey)
	binary.LittleEndian.PutUint32(body[12:16], monsterID)
	return body
}

func TestCurrentDungeonBossMutationRequestRequiresExactEnvelope(t *testing.T) {
	body := buildCurrentDungeonBossMutationTestRequest(7, 42, 1234)
	request, ok := parseCurrentDungeonBossMutationRequest(body)
	if !ok || request.Token != 7 || request.ObjectKey != 42 || request.MonsterID != 1234 {
		t.Fatalf("parse request = %#v, %v", request, ok)
	}
	if !isCurrentDungeonBossMutationEnvelope(body) {
		t.Fatal("BMU1 envelope was not recognized")
	}
	mutations := []func([]byte){
		func(value []byte) { value[4]++ },
		func(value []byte) { value[5] = 1 },
		func(value []byte) { binary.LittleEndian.PutUint16(value[6:8], 0) },
		func(value []byte) { binary.LittleEndian.PutUint16(value[8:10], 0) },
		func(value []byte) { binary.LittleEndian.PutUint16(value[10:12], 1) },
		func(value []byte) { binary.LittleEndian.PutUint32(value[12:16], 0) },
	}
	for index, mutate := range mutations {
		candidate := append([]byte(nil), body...)
		mutate(candidate)
		if _, accepted := parseCurrentDungeonBossMutationRequest(candidate); accepted {
			t.Fatalf("mutation %d unexpectedly accepted", index)
		}
	}
	if _, accepted := parseCurrentDungeonBossMutationRequest(body[:len(body)-1]); accepted {
		t.Fatal("truncated request unexpectedly accepted")
	}
}

func TestCurrentDungeonBossMutationDecisionBody(t *testing.T) {
	body := buildCurrentDungeonBossMutationDecisionBody(currentDungeonBossMutationDecision{
		Token: 7, MonsterID: 1234, ObjectKey: 42, Allowed: true,
		Reason: currentDungeonBossMutationReasonAllowed,
	})
	if len(body) != currentDungeonBossMutationResponseLength ||
		body[0] != currentDungeonBossMutationResponseType ||
		body[1] != currentDungeonBossMutationResponseVersion ||
		binary.LittleEndian.Uint16(body[2:4]) != 7 ||
		binary.LittleEndian.Uint32(body[4:8]) != 1234 ||
		binary.LittleEndian.Uint16(body[8:10]) != 42 || body[10] != 1 || body[11] != 0 {
		t.Fatalf("decision body = %v", body)
	}
}

func newCurrentDungeonBossMutationTestRoom(rank string) *runtimeDungeonRoom {
	monster := runtimeDungeonMonster{
		ObjectKey: 42,
		Spawn: worldmap.MonsterSpawn{
			MonsterID: 1234,
			Rank:      rank,
		},
		Definition: dnfmonster.Monster{Rank: rank},
		State:      runtimeDungeonMonsterAnnounced,
	}
	return &runtimeDungeonRoom{
		monsters:    []runtimeDungeonMonster{monster},
		byObjectKey: map[uint32]int{42: 0},
	}
}

func TestRuntimeDungeonBossMutationIsOneShot(t *testing.T) {
	room := newCurrentDungeonBossMutationTestRoom("boss")
	monster, allowed, reason := room.resolveBossMutation(42, 1234, 0)
	if !allowed || reason != currentDungeonBossMutationReasonAllowed ||
		!monster.BossMutationAttempted || !monster.BossMutationAllowed {
		t.Fatalf("first decision = monster=%#v allowed=%v reason=%d", monster, allowed, reason)
	}
	monster, allowed, reason = room.resolveBossMutation(42, 1234, 0)
	if allowed || reason != currentDungeonBossMutationReasonAlreadyAttempted ||
		!monster.BossMutationAttempted || !monster.BossMutationAllowed {
		t.Fatalf("repeat decision = monster=%#v allowed=%v reason=%d", monster, allowed, reason)
	}
}

func TestRuntimeDungeonBossMutationLotteryMissConsumesAttempt(t *testing.T) {
	room := newCurrentDungeonBossMutationTestRoom("boss")
	monster, allowed, reason := room.resolveBossMutation(42, 1234, 9)
	if allowed || reason != currentDungeonBossMutationReasonLotteryMiss ||
		!monster.BossMutationAttempted || monster.BossMutationAllowed {
		t.Fatalf("miss decision = monster=%#v allowed=%v reason=%d", monster, allowed, reason)
	}
	_, allowed, reason = room.resolveBossMutation(42, 1234, 0)
	if allowed || reason != currentDungeonBossMutationReasonAlreadyAttempted {
		t.Fatalf("miss retry = allowed=%v reason=%d", allowed, reason)
	}
}

func TestRuntimeDungeonBossMutationRejectsUnprovedTargets(t *testing.T) {
	tests := []struct {
		name      string
		room      *runtimeDungeonRoom
		objectKey uint32
		monsterID int64
		want      byte
	}{
		{name: "unknown object", room: newCurrentDungeonBossMutationTestRoom("boss"), objectKey: 43, monsterID: 1234, want: currentDungeonBossMutationReasonTargetUnknown},
		{name: "wrong template", room: newCurrentDungeonBossMutationTestRoom("boss"), objectKey: 42, monsterID: 9999, want: currentDungeonBossMutationReasonTargetMismatch},
		{name: "not boss", room: newCurrentDungeonBossMutationTestRoom("normal"), objectKey: 42, monsterID: 1234, want: currentDungeonBossMutationReasonNotBoss},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, allowed, reason := test.room.resolveBossMutation(test.objectKey, test.monsterID, 0)
			if allowed || reason != test.want {
				t.Fatalf("decision = allowed=%v reason=%d, want %d", allowed, reason, test.want)
			}
		})
	}
}
