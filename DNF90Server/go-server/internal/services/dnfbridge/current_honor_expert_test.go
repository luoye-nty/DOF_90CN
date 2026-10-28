package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentHonorExpertExperienceGainSkipsPreCapAwardsWithoutCapTable(t *testing.T) {
	got, err := currentHonorExpertExperienceGain(nil, 20, 0, 200)
	if err != nil || got != 0 {
		t.Fatalf("pre-cap gain=%d err=%v, want zero without cap-table lookup", got, err)
	}

	got, err = currentHonorExpertExperienceGain(nil, currentAdventureCharacterLevelCap, 123, 200)
	if err != nil || got != 200 {
		t.Fatalf("at-cap gain=%d err=%v, want complete award", got, err)
	}
}

func TestCurrentHonorExpertStateProjectsAcrossCurrentEXECarriers(t *testing.T) {
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		Level:       currentAdventureCharacterLevelCap,
		Stats: map[string]int64{
			"exp":                          currentAdventureCharacterLevelCap,
			currentHonorExpertLevelStatKey: 2,
			currentHonorExpertProgressExperienceStatKey: 0x0102030405060708,
		},
	}
	const wantLevel uint32 = 2
	const wantProgress uint64 = 0x0102030405060708

	service := &Service{}
	mode1 := service.buildCurrentSelectedUserInfoMode1BodyWithAdventureLevel(
		context.Background(), nil, nil, character, true, 19, 0,
	)
	if got := binary.LittleEndian.Uint32(mode1[7:11]); got != wantLevel {
		t.Fatalf("mode1 HonorExpert level=%d want=%d", got, wantLevel)
	}
	if got := binary.LittleEndian.Uint64(mode1[11:19]); got != wantProgress {
		t.Fatalf("mode1 HonorExpert progress=%x want=%x", got, wantProgress)
	}

	finishLoading := buildCurrentFinishLoadingCharacterStateBody(character, dnfrepo.SkillPointState{})
	if got := binary.LittleEndian.Uint32(finishLoading[55:59]); got != wantLevel {
		t.Fatalf("op37 HonorExpert level=%d want=%d", got, wantLevel)
	}
	if got := binary.LittleEndian.Uint64(finishLoading[59:67]); got != wantProgress {
		t.Fatalf("op37 HonorExpert progress=%x want=%x", got, wantProgress)
	}

	var tail packetWriter
	writeCurrentSceneObjectEntryTail(&tail, character, true)
	var want packetWriter
	want.writeUint32(wantLevel)
	want.writeUint64(wantProgress)
	if occurrences := bytes.Count(tail.bytes(), want.bytes()); occurrences != 1 {
		t.Fatalf("actor-tail HonorExpert occurrences=%d want=1 body=%x", occurrences, tail.bytes())
	}
}

func TestCurrentMode0CreatureSegmentDoesNotShiftHonorExpertState(t *testing.T) {
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		Level:       currentAdventureCharacterLevelCap,
		Stats: map[string]int64{
			currentHonorExpertLevelStatKey:              78,
			currentHonorExpertProgressExperienceStatKey: 12854109,
		},
	}
	creature := currentEquippedCreatureSnapshot{itemID: 63000, serialOrHandle: 37, name: "petty", aliveState: 1}

	var writer packetWriter
	writeCurrentSceneObjectEntryTailWithCreature(&writer, character, true, creature)
	level, progress := currentMode0HonorExpertStateForTest(t, writer.bytes())
	if level != 78 || progress != 12854109 {
		t.Fatalf("mode0 HonorExpert level=%d progress=%d want=78/12854109 tail=%x", level, progress, writer.bytes())
	}
}

func TestCurrentMode0OrganizationDSTRPreservesHonorExpertBoundaries(t *testing.T) {
	const guildName = "Guild"
	for _, level := range []uint32{99, 100, 199, 200} {
		t.Run(fmt.Sprintf("level_%d", level), func(t *testing.T) {
			character := dnfrepo.CharacterRecord{
				CharacterID: "19",
				Level:       currentAdventureCharacterLevelCap,
				Stats: map[string]int64{
					currentHonorExpertLevelStatKey:              int64(level),
					currentHonorExpertProgressExperienceStatKey: int64(0x0102030400000000 | uint64(level)),
				},
			}

			var writer packetWriter
			writeCurrentSceneObjectEntryTailWithCreatureAndNames(
				&writer,
				character,
				true,
				currentEquippedCreatureSnapshot{},
				"Adventure",
				guildName,
			)
			tail := writer.bytes()
			organizationPos := currentMode0OrganizationDSTRStartForTest(t, tail)
			guildNameBytes := rosterNameBytes(guildName)
			if got := binary.LittleEndian.Uint32(tail[organizationPos : organizationPos+4]); got != uint32(len(guildNameBytes)) {
				t.Fatalf("organization DSTR length=%d want=%d tail=%x", got, len(guildNameBytes), tail)
			}
			if got := tail[organizationPos+4 : organizationPos+4+len(guildNameBytes)]; !bytes.Equal(got, guildNameBytes) {
				t.Fatalf("organization DSTR=%x want=%x", got, guildNameBytes)
			}
			gotLevel, gotProgress := currentMode0HonorExpertStateForTest(t, tail)
			wantProgress := uint64(0x0102030400000000 | uint64(level))
			if gotLevel != level || gotProgress != wantProgress {
				t.Fatalf("mode0 HonorExpert level=%d progress=%x want=%d/%x tail=%x", gotLevel, gotProgress, level, wantProgress, tail)
			}
		})
	}
}

func TestCurrentMode0ActorStateSuppressionPreservesHonorExpertAfterNamedCreatureAndGuild(t *testing.T) {
	const (
		wantLevel    uint32 = 200
		wantProgress uint64 = 0x7102030405060708
	)
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		Name:        "hero",
		Level:       currentAdventureCharacterLevelCap,
		Stats: map[string]int64{
			currentHonorExpertLevelStatKey:              int64(wantLevel),
			currentHonorExpertProgressExperienceStatKey: int64(wantProgress),
		},
	}
	creature := currentEquippedCreatureSnapshot{
		itemID:         63000,
		serialOrHandle: 37,
		name:           "petty",
		aliveState:     1,
	}
	body := buildCurrentSceneObjectListBodyWithCreatureAndNamesInContext(
		19,
		character,
		true,
		character.Name,
		creature,
		"Adventure",
		"Guild",
		currentSceneObjectContext,
	)
	eventOffset, ok := currentSceneObjectTailActorStateEventArgOffset(body)
	if !ok {
		t.Fatalf("mode0 actor-state event offset not found: %x", body)
	}
	if got := binary.LittleEndian.Uint32(body[eventOffset : eventOffset+4]); got != currentSceneActorStateEventArg {
		t.Fatalf("mode0 actor-state event=%d want=%d", got, currentSceneActorStateEventArg)
	}
	_, tailStart, ok := currentSceneObjectLevelForLog(body)
	if !ok {
		t.Fatalf("mode0 tail boundary not found: %x", body)
	}
	levelBefore, progressBefore := currentMode0HonorExpertStateForTest(t, body[tailStart:])
	before := append([]byte(nil), body...)

	if !suppressCurrentSceneObjectActorStateEvent(body) {
		t.Fatal("mode0 actor-state event suppression failed")
	}
	if got := binary.LittleEndian.Uint32(body[eventOffset : eventOffset+4]); got != 0 {
		t.Fatalf("suppressed actor-state event=%d want=0", got)
	}
	if !bytes.Equal(before[:eventOffset], body[:eventOffset]) || !bytes.Equal(before[eventOffset+4:], body[eventOffset+4:]) {
		t.Fatal("actor-state suppression modified bytes outside its u32 event field")
	}
	levelAfter, progressAfter := currentMode0HonorExpertStateForTest(t, body[tailStart:])
	if levelBefore != wantLevel || progressBefore != wantProgress || levelAfter != wantLevel || progressAfter != wantProgress {
		t.Fatalf(
			"HonorExpert changed across suppression: before=%d/%x after=%d/%x want=%d/%x",
			levelBefore,
			progressBefore,
			levelAfter,
			progressAfter,
			wantLevel,
			wantProgress,
		)
	}
}

func currentMode0HonorExpertStateForTest(t *testing.T, tail []byte) (uint32, uint64) {
	t.Helper()
	pos := currentMode0OrganizationDSTRStartForTest(t, tail)
	organizationNameLen := int(binary.LittleEndian.Uint32(tail[pos : pos+4]))
	pos += 4 + organizationNameLen
	if organizationNameLen < 0 || pos > len(tail) {
		t.Fatalf("mode0 organization name truncated len=%d at %d/%d", organizationNameLen, pos, len(tail))
	}
	pos += 4 + 4 + 1 + 2 + 5 + 7 + 4
	if pos+12 > len(tail) {
		t.Fatalf("mode0 HonorExpert state truncated at %d/%d tail=%x", pos, len(tail), tail)
	}
	return binary.LittleEndian.Uint32(tail[pos : pos+4]), binary.LittleEndian.Uint64(tail[pos+4 : pos+12])
}

func currentMode0OrganizationDSTRStartForTest(t *testing.T, tail []byte) int {
	t.Helper()
	equipEnd, ok := currentSceneObjectEquipSummaryEnd(tail, 6)
	if !ok {
		t.Fatalf("mode0 equipment boundary not found: %x", tail)
	}
	pos := equipEnd
	pos += 4 + 4 + 8 + 1 + 4 + 1
	if pos+8 > len(tail) {
		t.Fatalf("mode0 creature state truncated at %d/%d", pos, len(tail))
	}
	pos += 4
	creatureNameLen := int(binary.LittleEndian.Uint32(tail[pos : pos+4]))
	pos += 4 + creatureNameLen + 1
	if creatureNameLen < 0 || pos > len(tail) {
		t.Fatalf("mode0 creature name truncated len=%d at %d/%d", creatureNameLen, pos, len(tail))
	}
	pos += 1 + 1 + 4
	if pos+4 > len(tail) {
		t.Fatalf("mode0 organization DSTR truncated at %d/%d", pos, len(tail))
	}
	return pos
}
