package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The skill groups are what a peer's client applies to the remote actor
// (sub_2003420); their layout is u8 selected tree, then per tree u8 count and
// {u16 skill, u8 level} rows in ascending skill order, unlearned skills omitted.
func TestCurrentUserInfoSkillPagesProjectLearnedSkillsPerTree(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills: map[int64]dnfrepo.SkillState{
			7:   {Enabled: true, Level: 22},
			53:  {Enabled: true, Level: 19},
			45:  {Enabled: true, Level: 0},
			174: {Enabled: true, Level: 70},
			1:   {Enabled: true, Level: 1},
		},
	}
	character := dnfrepo.CharacterRecord{CharacterID: "42", Job: "4", Stats: map[string]int64{"grow_type": 1}}
	pages := currentUserInfoSkillPagesFromRecord(character, record)
	if pages.Selected != 0 {
		t.Fatalf("selected tree=%d want 0", pages.Selected)
	}
	want := []currentUserInfoSkillPageEntry{{1, 1}, {7, 22}, {53, 19}, {174, 70}}
	if len(pages.Pages[0]) != len(want) {
		t.Fatalf("tree0 rows=%v want=%v", pages.Pages[0], want)
	}
	for i := range want {
		if pages.Pages[0][i] != want[i] {
			t.Fatalf("tree0 row %d=%v want=%v", i, pages.Pages[0][i], want[i])
		}
	}
	if len(pages.Pages[1]) != 0 {
		t.Fatalf("tree1 rows=%v want none without an alternate tree", pages.Pages[1])
	}

	var w packetWriter
	writeCurrentUserInfoSkillPages(&w, pages)
	wantBytes := []byte{
		0,       // selected tree 0
		4,       // tree 0 count
		1, 0, 1, // skill 1 level 1
		7, 0, 22, // 武器祝福 level 22
		53, 0, 19, // 天使祝福 level 19
		174, 0, 70, // passive
		0, // tree 1 count
	}
	if !bytes.Equal(w.bytes(), wantBytes) {
		t.Fatalf("skill pages bytes=%x want=%x", w.bytes(), wantBytes)
	}

	// Levels above the u8 the reader stores are clamped; a missing character
	// tree index keeps the client's own selection.
	record.Skills[7] = dnfrepo.SkillState{Enabled: true, Level: 300}
	clamped := currentUserInfoSkillPagesFromRecord(dnfrepo.CharacterRecord{CharacterID: "42", Job: "4"}, record)
	if clamped.Pages[0][1].Level != 0xff {
		t.Fatalf("level 300 clamped to %d want 255", clamped.Pages[0][1].Level)
	}

	var empty packetWriter
	writeCurrentUserInfoSkillPages(&empty, emptyCurrentUserInfoSkillPages())
	if !bytes.Equal(empty.bytes(), []byte{0xff, 0, 0}) {
		t.Fatalf("empty pages bytes=%x want ff0000", empty.bytes())
	}
}

// Without a skill repository the complete mode1 row must keep the historical
// `ff 00 00` groups exactly where the reader expects them, so every existing
// tail pin stays valid.
func TestCurrentSelectedUserInfoMode1KeepsEmptySkillGroupsWithoutRepositories(t *testing.T) {
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "hero",
		Job:         "11",
		Level:       1,
		Stats:       map[string]int64{"exp": 12345},
	}
	service := Service{characterStats: testCharacterStatTable(t)}
	body := service.buildCurrentSelectedUserInfoMode1Body(context.Background(), nil, nil, character, true, 19)
	// creature {item id, serial} zero pair, then the groups, then creature level 0.
	if !bytes.Contains(body, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0, 0, 0}) {
		t.Fatalf("mode1 body lost the empty skill groups: %x", body)
	}
}
