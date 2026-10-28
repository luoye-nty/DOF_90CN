package dnfbridge

import "testing"

func TestIsCurrentGameClientImage(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{`C:/DNF/client/DNF.exe`, true},
		{`C:\Games\dnf\dnf.EXE`, true},
		{`/c/DNF/client/DNF.exe`, true},
		{`C:/DNF/client/GameLoader.exe`, false},
		{`C:/DNF/DNF90Hub/DNF90Hub.exe`, false},
		{`C:/DNF/client/TP3Helper.exe`, false},
		{``, false},
		{`DNF.exe.bak`, false},
	} {
		if got := isCurrentGameClientImage(test.path); got != test.want {
			t.Fatalf("isCurrentGameClientImage(%q)=%t want %t", test.path, got, test.want)
		}
	}
}
