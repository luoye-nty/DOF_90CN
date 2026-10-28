package dnfbridge

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The mode-0 actor record carries the character's sub-profession. Current EXE
// sub_2009160 -> sub_2003F20 reads it as one u8 plus one u32:
//
//	FUN_034e3970(&type, 1);        // u8
//	FUN_034e3a50(&experience);     // u32
//	...
//	*(uint *)(record + 0x368) = type;   // sub_3A4C1A0(actor)+0x368
//	<encoded u32 setter>(experience);   // sub_3A4C1A0(actor)+0x370
//
// +0x368 is the gate every profession reader uses (sub_322E590 on the op205
// continuation for professions 2 and 4, and the readers that compare it against
// 1/2/3), and no other packet the server sends writes it. The builder used to
// emit two zeros here, so the client's profession row had nothing to read even
// though op205 carried the profession correctly.
//
// These two fields are the LAST variable-length part of the mode-0 tail before
// the fixed 43 trailing bytes, so the pair is anchored at the end of the body:
// the u8 sits at len-48 and the u32 at len-47..len-43.
const (
	sceneObjectExpertJobTrailingBytes = 43
	sceneObjectExpertJobTypeFromEnd   = sceneObjectExpertJobTrailingBytes + 5
	sceneObjectExpertJobExpFromEnd    = sceneObjectExpertJobTrailingBytes + 4
)

func sceneObjectExpertJobFixture(expertJobType, expertJobExp int64) dnfrepo.CharacterRecord {
	return dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "hero",
		Job:         "11",
		Level:       90,
		Stats: map[string]int64{
			"grow_type":       0x21,
			"expert_job_type": expertJobType,
			"expert_job_exp":  expertJobExp,
		},
	}
}

func TestCurrentSceneObjectMode0CarriesExpertJobState(t *testing.T) {
	for _, test := range []struct {
		name       string
		jobType    int64
		experience int64
		wantType   byte
		wantExp    uint32
	}{
		{name: "doll_controller", jobType: 4, experience: 1234, wantType: 4, wantExp: 1234},
		{name: "enchanter", jobType: 1, experience: 0, wantType: 1, wantExp: 0},
		{name: "none", jobType: 0, experience: 0, wantType: 0, wantExp: 0},
		{name: "out_of_range_is_projected_as_none", jobType: 9, experience: 5, wantType: 0, wantExp: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			character := sceneObjectExpertJobFixture(test.jobType, test.experience)
			body := buildCurrentSceneObjectListBodyWithCreature(
				19, character, true, character.Name, currentEquippedCreatureSnapshot{})

			typeIndex := len(body) - sceneObjectExpertJobTypeFromEnd
			if typeIndex < 0 || typeIndex+5 > len(body) {
				t.Fatalf("mode0 body too short: %d", len(body))
			}
			if got := body[typeIndex]; got != test.wantType {
				t.Fatalf("mode0 record+0x368 expert-job type = %d, want %d (body=%x)",
					got, test.wantType, body)
			}
			if got := binary.LittleEndian.Uint32(body[typeIndex+1 : typeIndex+5]); got != test.wantExp {
				t.Fatalf("mode0 expert-job experience = %d, want %d (body=%x)",
					got, test.wantExp, body)
			}
		})
	}
}

// Changing the profession must move exactly the two mode-0 fields and must not
// change the body length: if the pair ever slips, every field after it in the
// actor record is read from the wrong offset and the client builds a corrupt
// actor.
func TestCurrentSceneObjectMode0ExpertJobPairMovesOnlyItsOwnBytes(t *testing.T) {
	without := buildCurrentSceneObjectListBodyWithCreature(
		19, sceneObjectExpertJobFixture(0, 0), true, "hero", currentEquippedCreatureSnapshot{})
	with := buildCurrentSceneObjectListBodyWithCreature(
		19, sceneObjectExpertJobFixture(4, 0x01020304), true, "hero", currentEquippedCreatureSnapshot{})

	if len(without) != len(with) {
		t.Fatalf("mode0 length changed with the profession: %d vs %d", len(without), len(with))
	}
	differing := make([]int, 0, 8)
	for index := range without {
		if without[index] != with[index] {
			differing = append(differing, index)
		}
	}
	want := []int{
		len(with) - sceneObjectExpertJobTypeFromEnd,
		len(with) - sceneObjectExpertJobExpFromEnd,
		len(with) - sceneObjectExpertJobExpFromEnd + 1,
		len(with) - sceneObjectExpertJobExpFromEnd + 2,
		len(with) - sceneObjectExpertJobExpFromEnd + 3,
	}
	if len(differing) != len(want) {
		t.Fatalf("mode0 expert-job pair moved %v, want %v (body=%x)", differing, want, with)
	}
	for index, offset := range differing {
		if offset != want[index] {
			t.Fatalf("mode0 differing offset %d = %d, want %d (all=%v)",
				index, offset, want[index], differing)
		}
	}
}
