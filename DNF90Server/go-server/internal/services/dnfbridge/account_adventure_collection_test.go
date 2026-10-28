package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestBuildCurrentAdventureRepresentCharactersBodyGroupsExactRosterEntries(t *testing.T) {
	characters := []dnfrepo.CharacterRecord{
		{
			CharacterID: "267",
			Slot:        1,
			Name:        "pou.ut",
			Job:         "11",
			Level:       90,
			Stats: map[string]int64{
				"grow_type":          0x21,
				"server_group_id":    0,
				"honor_expert_level": 7,
			},
		},
		{
			CharacterID: "300",
			Slot:        0,
			Name:        "other",
			Job:         "11",
			Level:       90,
			Stats: map[string]int64{
				"grow_type":          0x21,
				"server_group_id":    2,
				"honor_expert_level": 3,
			},
		},
		{
			CharacterID: "301",
			Name:        "wrong-grow",
			Job:         "11",
			Level:       90,
			Stats:       map[string]int64{"grow_type": 0x22},
		},
		{
			CharacterID: "302",
			Name:        "wrong-job",
			Job:         "12",
			Level:       90,
			Stats:       map[string]int64{"grow_type": 0x21},
		},
	}

	body := buildCurrentAdventureRepresentCharactersBody(11, 1, characters)
	const wantLength = 1 + 2*(1+1) + 2*currentAdventureRepresentEntrySize
	if len(body) != wantLength {
		t.Fatalf("body length=%d want=%d: %x", len(body), wantLength, body)
	}
	if body[0] != 2 {
		t.Fatalf("group count=%d", body[0])
	}

	offset := 1
	if body[offset] != 0 || body[offset+1] != 1 {
		t.Fatalf("first group header=%x", body[offset:offset+2])
	}
	offset += 2
	first := body[offset : offset+currentAdventureRepresentEntrySize]
	if first[currentAdventureRepresentGroupOffset] != 0 {
		t.Fatalf("first row server group=%d", first[currentAdventureRepresentGroupOffset])
	}
	if got := binary.LittleEndian.Uint32(first[currentAdventureRepresentHonorOffset:]); got != 7 {
		t.Fatalf("first row HonorExpert level=%d", got)
	}
	if got := binary.LittleEndian.Uint32(first[currentAdventureRepresentLevelOffset:]); got != 90 {
		t.Fatalf("first character level=%d", got)
	}
	if got := binary.LittleEndian.Uint32(first[currentAdventureRepresentIDOffset:]); got != 267 {
		t.Fatalf("first character id=%d", got)
	}
	if got := string(first[currentAdventureRepresentNameOffset : currentAdventureRepresentNameOffset+6]); got != "pou.ut" {
		t.Fatalf("first character name=%q", got)
	}

	offset += currentAdventureRepresentEntrySize
	if body[offset] != 2 || body[offset+1] != 1 {
		t.Fatalf("second group header=%x", body[offset:offset+2])
	}
	offset += 2
	second := body[offset : offset+currentAdventureRepresentEntrySize]
	if second[currentAdventureRepresentGroupOffset] != 2 {
		t.Fatalf("second row server group=%d", second[currentAdventureRepresentGroupOffset])
	}
	if got := binary.LittleEndian.Uint32(second[currentAdventureRepresentIDOffset:]); got != 300 {
		t.Fatalf("second character id=%d", got)
	}
	if got := binary.LittleEndian.Uint32(second[currentAdventureRepresentLevelOffset:]); got != 90 {
		t.Fatalf("second character level=%d", got)
	}
	if got := binary.LittleEndian.Uint32(second[currentAdventureRepresentHonorOffset:]); got != 3 {
		t.Fatalf("second row HonorExpert level=%d", got)
	}
}

func TestAdventureGameplayModuleOwnsRepresentativeCommandsOnBothTransports(t *testing.T) {
	module := adventureGameplayModule()
	for _, opcode := range []uint16{
		uint16(dnfenum.CmdPacketGetRepresentCharacJob),
		uint16(dnfenum.CmdPacketSetRepresentCharacJob),
		uint16(dnfenum.CmdPacketRemoveRepresentCharacJob),
	} {
		if module.LegacyHandlers[opcode] == nil {
			t.Fatalf("legacy op%d handler is missing", opcode)
		}
		if module.UpperHandlers[opcode] == nil {
			t.Fatalf("upper op%d handler is missing", opcode)
		}
	}
}
