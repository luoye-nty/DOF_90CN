package mysql

import "testing"

func TestCharacterSettlementSettingsScopeAllowsOnlySelectedCharacterAndGuildRegistry(t *testing.T) {
	repository := &characterSettlementScopedSettingsRepository{characterID: "77"}
	for _, test := range []struct {
		scope string
		want  bool
	}{
		{scope: "character:77:game-options", want: true},
		{scope: "global:guild_registry:v1", want: true},
		{scope: "character:7:game-options", want: false},
		{scope: "global:guild_registry:v2", want: false},
		{scope: "global:other", want: false},
	} {
		if got := repository.allowed(test.scope); got != test.want {
			t.Fatalf("allowed(%q)=%t want=%t", test.scope, got, test.want)
		}
	}
}
