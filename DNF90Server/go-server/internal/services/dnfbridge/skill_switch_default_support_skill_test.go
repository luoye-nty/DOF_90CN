package dnfbridge

import (
	"os"
	"reflect"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// A buffer that never opened the 换装 window has skill 0 in its persisted
// record. The current client derives the buff it applies to teammates from
// that record, so the projection must name the first learned PVF candidate in
// PVF order, and must stay 0 when nothing listed has been learned.
func TestSkillSwitchDefaultSupportSkillFollowsPVFOrderAndLearnedState(t *testing.T) {
	source := initialEquipmentMemSource{
		"character/character.lst":         "4 `Priest/Priest.chr`\n",
		"character/Priest/Priest.chr":     "[job]\n`[priest]`\n",
		"skill/switchingsupportskill.etc": "[character type]\n`priest` 1\n[skill]\n7 53 45 258\n[/skill]\n\n[character type]\n`priest` 2\n[skill]\n35\n[/skill]\n",
	}
	character := dnfrepo.CharacterRecord{Job: "4", Stats: map[string]int64{"grow_type": 1}}

	candidates, err := skillSwitchSupportSkillsByPVFSource(source, character)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint16{7, 53, 45, 258}; !reflect.DeepEqual(candidates, want) {
		t.Fatalf("priest grow 1 candidates=%v want=%v", candidates, want)
	}

	skills := dnfrepo.SkillRecord{Skills: map[int64]dnfrepo.SkillState{
		7:  {Enabled: true, Level: 12},
		53: {Enabled: true, Level: 1},
	}}
	if got := defaultSkillSwitchSupportSkill(candidates, skills.Skills); got != 7 {
		t.Fatalf("default with 勇气祝福 learned=%d want=7", got)
	}
	skills.Skills[7] = dnfrepo.SkillState{Enabled: true, Level: 0}
	if got := defaultSkillSwitchSupportSkill(candidates, skills.Skills); got != 53 {
		t.Fatalf("default skips an unlearned first candidate: got=%d want=53", got)
	}
	skills.Skills[53] = dnfrepo.SkillState{Enabled: false, Level: 1}
	if got := defaultSkillSwitchSupportSkill(candidates, skills.Skills); got != 0 {
		t.Fatalf("default without any learned candidate=%d want=0", got)
	}

	character.Stats["grow_type"] = 2
	candidates, err = skillSwitchSupportSkillsByPVFSource(source, character)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint16{35}; !reflect.DeepEqual(candidates, want) {
		t.Fatalf("priest grow 2 candidates=%v want=%v", candidates, want)
	}
	character.Stats["grow_type"] = 5
	candidates, err = skillSwitchSupportSkillsByPVFSource(source, character)
	if err != nil || candidates != nil {
		t.Fatalf("unlisted growth candidates=%v err=%v want none", candidates, err)
	}
}

func TestSkillSwitchDefaultSupportSkillRealPVFPriestCrusader(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify skill/switchingsupportskill.etc against the shipped Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	character := dnfrepo.CharacterRecord{Job: "4", Stats: map[string]int64{"grow_type": 1}}
	candidates, err := skillSwitchSupportSkillsByPVFSource(archive, character)
	if err != nil {
		t.Fatal(err)
	}
	// 勇气祝福 (Priest/Striking.skl), 天使祝福 (Priest/Bless.skl), 荣誉祝福
	// (Priest/GloriousBless.skl), then the awakening support skill.
	if want := []uint16{7, 53, 45, 258}; !reflect.DeepEqual(candidates, want) {
		t.Fatalf("real PVF priest grow 1 candidates=%v want=%v", candidates, want)
	}
}
