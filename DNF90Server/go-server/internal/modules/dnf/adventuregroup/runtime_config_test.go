package adventuregroup

import (
	"fmt"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// The live adventurersystem2018.etc ships 15 expert groups, and that count
// bounds the 20-row profession descriptor block (ProfessionTypeCount) rather
// than the 6 u32 content counters (ContentTypeCount). Confusing the two made
// the table look malformed and the server refused to start against the real
// PVF while every fixture-based test still passed, so this pins the real
// group count.
func TestParseProfessionClassificationsAcceptsTheFullNativeGroupCount(t *testing.T) {
	var b strings.Builder
	b.WriteString("[expert list]\n")
	for group := 0; group < 15; group++ {
		b.WriteString("[group]\n")
		fmt.Fprintf(&b, "%d `group %d`\n", group, group)
		b.WriteString("[info]\n")
		fmt.Fprintf(&b, "`[swordman]` `second awakening` %d\n", group+1)
		b.WriteString("[/info]\n")
		b.WriteString("[/group]\n")
	}
	b.WriteString("[/expert list]")
	document, err := dnfpvf.Parse(AdventureSystem2018Path, b.String())
	if err != nil {
		t.Fatal(err)
	}
	_, groups, err := parseProfessionClassifications(document)
	if err != nil {
		t.Fatalf("15 native groups rejected: %v", err)
	}
	if groups != 15 {
		t.Fatalf("groups=%d, want 15", groups)
	}
	if groups > ProfessionTypeCount {
		t.Fatalf("ProfessionTypeCount=%d cannot hold %d native groups", ProfessionTypeCount, groups)
	}
}

func TestParseProfessionClassificationsUsesOrderedExpertGroups(t *testing.T) {
	document, err := dnfpvf.Parse(AdventureSystem2018Path, `[expert list]
[group]
0 `+"`鬼剑士 (男)`"+`
[info]
`+"`[swordman]` `second awakening` 1"+`
[/info]
[info]
`+"`[swordman]` `second awakening` 4"+`
[/info]
[/group]
[group]
1 `+"`外传`"+`
[info]
`+"`[creator mage]` `second realization` 1"+`
[/info]
[info]
`+"`[demonic swordman]` `second realization` 1"+`
[/info]
[/group]
[/expert list]`)
	if err != nil {
		t.Fatal(err)
	}
	classes, groups, err := parseProfessionClassifications(document)
	if err != nil {
		t.Fatal(err)
	}
	config := RuntimeConfig{professionClasses: classes, professionGroupCount: groups}
	if config.ProfessionGroupCount() != 2 {
		t.Fatalf("groups=%d", config.ProfessionGroupCount())
	}
	for _, test := range []struct {
		job, grow int
		want      byte
	}{
		{job: 0, grow: 1, want: 0},
		{job: 0, grow: 4, want: 0},
		{job: 10, grow: 1, want: 1},
		{job: 9, grow: 1, want: 1},
	} {
		got, found := config.ProfessionClassification(test.job, test.grow)
		if !found || got != test.want {
			t.Fatalf("job=%d grow=%d classification=%d/%t want=%d", test.job, test.grow, got, found, test.want)
		}
	}
	if _, found := config.ProfessionClassification(0, 2); found {
		t.Fatal("undefined profession slot resolved")
	}
	clone := config.Clone()
	clone.professionClasses[[2]byte{0, 1}] = 9
	if original, _ := config.ProfessionClassification(0, 1); original != 0 {
		t.Fatal("runtime clone shares profession map")
	}
}

func TestParseProfessionClassificationsRejectsMalformedOrder(t *testing.T) {
	document, err := dnfpvf.Parse(AdventureSystem2018Path, `[expert list]
[group]
1 `+"`bad ordinal`"+`
[info]
`+"`[swordman]` `second awakening` 1"+`
[/info]
[/group]
[/expert list]`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseProfessionClassifications(document); err == nil {
		t.Fatal("malformed group order accepted")
	}
}
