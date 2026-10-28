package main

import "testing"

func TestShouldAddEarringOnlyToLegacySpecialEquipmentFilters(t *testing.T) {
	tests := []struct {
		name  string
		types []string
		want  bool
	}{
		{name: "legacy special", types: []string{"[support]", "[magic stone]"}, want: true},
		{name: "broad legacy", types: []string{"[weapon]", "[support]", "[magic stone]"}, want: true},
		{name: "already current", types: []string{"[support]", "[magic stone]", "[earring]"}},
		{name: "weapon only", types: []string{"[weapon]"}},
		{name: "armor only", types: []string{"[coat]", "[pants]"}},
		{name: "accessory only", types: []string{"[amulet]", "[ring]", "[wrist]"}},
		{name: "unrestricted", types: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldAddEarring(test.types); got != test.want {
				t.Fatalf("shouldAddEarring(%v)=%t want=%t", test.types, got, test.want)
			}
		})
	}
}

func TestIsGoldKaleidoAcceptsCurrentPVFActionFallback(t *testing.T) {
	if !isGoldKaleido("`[gold kaleido]`", "") {
		t.Fatal("stackable type was not recognized")
	}
	if !isGoldKaleido("", "`[gold kaleido box]`") {
		t.Fatal("action fallback was not recognized")
	}
	if isGoldKaleido("[kaleido]", "[kaleido]") {
		t.Fatal("ordinary kaleido was classified as gold")
	}
}
