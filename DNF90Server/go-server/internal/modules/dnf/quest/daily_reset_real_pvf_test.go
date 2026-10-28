package quest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFDailyFamilyClassification pins the daily rule against the
// live archive.
//
// The rule is "the grade contains daily", so this guard fails the moment the
// archive carries a daily-family grade the predicate stops recognising - which is
// exactly how `daily mission` and `auto daily mission` silently stayed one-shot.
// It also checks that every daily the server can now offer carries a parsable
// reward: these grades become selectable under the same rule, so a malformed one
// would hand the player a quest that cannot be settled.
func TestRealScriptPVFDailyFamilyClassification(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the live daily family")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	malformed := make([]string, 0)
	repeatableMisclassified := make([]string, 0)
	for _, definition := range catalog.Definitions() {
		grade := normalizeQuestTag(definition.Grade)
		wantDaily := strings.Contains(grade, "daily")
		if got := catalog.IsDailyRepeatable(definition.ID); got != wantDaily {
			t.Fatalf("quest %d grade=%q resets at the boundary=%t, want %t",
				definition.ID, grade, got, wantDaily)
		}
		// `normaly repeat` repeats without a boundary; it must never be swept.
		if grade == "normaly repeat" && catalog.IsDailyRepeatable(definition.ID) {
			repeatableMisclassified = append(repeatableMisclassified, fmt.Sprint(definition.ID))
		}
		if !wantDaily {
			continue
		}
		counts[grade]++
		if definition.RewardParseError != "" {
			malformed = append(malformed, fmt.Sprintf("%d(%s): %s", definition.ID, grade, definition.RewardParseError))
		}
	}
	if len(counts) == 0 {
		t.Fatal("live PVF carries no daily-grade quest")
	}
	grades := make([]string, 0, len(counts))
	for grade := range counts {
		grades = append(grades, grade)
	}
	sort.Strings(grades)
	for _, grade := range grades {
		t.Logf("daily grade %q: %d quests", grade, counts[grade])
	}
	for _, grade := range []string{"daily", "daily mission"} {
		if counts[grade] == 0 {
			t.Fatalf("live PVF no longer carries grade %q; the daily rule must be re-derived", grade)
		}
	}
	if len(repeatableMisclassified) != 0 {
		t.Fatalf("normaly repeat quests treated as daily: %v", repeatableMisclassified)
	}
	if len(malformed) != 0 {
		t.Fatalf("daily-grade quests with reward parse errors: %v", malformed)
	}
}
