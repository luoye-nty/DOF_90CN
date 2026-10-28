package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// TestRuntimeRaidDisconnectGraceComesFromThePVF pins the two ends of wiring
// [DISCONN MEMBER WAIT TIME] through: the shipped documents declare 300, which is
// exactly what the bare constant it replaced held (behaviour-neutral), and a
// document that declares something else is now followed instead of ignored.
func TestRuntimeRaidDisconnectGraceComesFromThePVF(t *testing.T) {
	t.Parallel()
	if runtimeRaidDisconnectGrace != 5*time.Minute {
		t.Fatalf("fallback grace=%s want 5m", runtimeRaidDisconnectGrace)
	}

	// The shipped values, read from etc/raid/anton.etc and etc/raid/luke/luke.etc
	// (both 300). Anton is here to prove the wire-up is a no-op for it.
	anton := runtimeRaidState{
		RaidMode:    runtimeRaidOwnerAnton,
		AntonConfig: &raid.AntonConfig{DisconnMemberWaitTime: 300},
	}
	if got := runtimeRaidDisconnectGraceForState(&anton); got != runtimeRaidDisconnectGrace {
		t.Fatalf("anton grace=%s want the declared 300s", got)
	}
	lukeConfig := &raid.AntonConfig{DisconnMemberWaitTime: 300}
	luke := runtimeRaidState{
		RaidMode:   runtimeRaidOwnerLuke,
		LukeConfig: &raid.LukeConfig{Raid: lukeConfig},
	}
	if got := runtimeRaidDisconnectGraceForState(&luke); got != runtimeRaidDisconnectGrace {
		t.Fatalf("luke grace=%s want the declared 300s", got)
	}

	tests := []struct {
		name  string
		state runtimeRaidState
		want  time.Duration
	}{
		{"nil state keeps the fallback", runtimeRaidState{}, runtimeRaidDisconnectGrace},
		{
			"a declared value is followed",
			runtimeRaidState{AntonConfig: &raid.AntonConfig{DisconnMemberWaitTime: 120}},
			120 * time.Second,
		},
		{
			"an omitted section keeps the fallback",
			runtimeRaidState{AntonConfig: &raid.AntonConfig{}},
			runtimeRaidDisconnectGrace,
		},
		{
			"a hand-built state keeps the fallback",
			runtimeRaidState{RaidMode: runtimeRaidOwnerLuke, Lifecycle: runtimeRaidLifecycleState{Phase: 1}},
			runtimeRaidDisconnectGrace,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := runtimeRaidDisconnectGraceForState(&test.state); got != test.want {
				t.Fatalf("grace=%s want %s", got, test.want)
			}
		})
	}
}

// TestRuntimeRaidLukeWeeklyObservationWindowAndRollover pins the observation layer
// that [WEEKLY ENTER COUNT] `20 2` is read through.
//
// It is deliberately NOT an enforcement test: nothing refuses today. What is pinned
// is that the window the server reports is the catalog's own service window (so the
// log and any future counter agree), and that a window move is reported as a
// rollover with prev/next rather than silently restarting the count.
func TestRuntimeRaidLukeWeeklyObservationWindowAndRollover(t *testing.T) {
	resetRuntimeRaidLukeWeeklyObservationsForTest()
	t.Cleanup(resetRuntimeRaidLukeWeeklyObservationsForTest)

	// The repository's established weekly boundary is Thursday 06:00 Beijing, which
	// is the node the catalog charges the Luke weekly policy to. The registry itself
	// only compares window START instants -- turning an instant into a window is
	// dungeonadmission.ServiceWindow's job, and the observation layer calls it rather
	// than re-deriving the boundary here.
	thursday := time.Date(2026, 9, 3, 6, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	nextWindow := thursday.AddDate(0, 0, 7)

	previous, hadPrevious, rolledOver, entries := runtimeRaidLukeWeeklyObservations.record(19, thursday)
	if hadPrevious || rolledOver || entries != 1 {
		t.Fatalf("first record prev=%+v had=%t rolled=%t entries=%d want had=false rolled=false entries=1",
			previous, hadPrevious, rolledOver, entries)
	}
	if _, _, rolledOver, entries = runtimeRaidLukeWeeklyObservations.record(19, thursday); rolledOver || entries != 2 {
		t.Fatalf("same-window record rolled=%t entries=%d want rolled=false entries=2", rolledOver, entries)
	}
	if _, _, rolledOver, entries = runtimeRaidLukeWeeklyObservations.record(19, thursday); rolledOver || entries != 3 {
		t.Fatalf("same-window record rolled=%t entries=%d want rolled=false entries=3", rolledOver, entries)
	}

	previous, hadPrevious, rolledOver, entries = runtimeRaidLukeWeeklyObservations.record(19, nextWindow)
	if !hadPrevious || !rolledOver {
		t.Fatalf("a new window did not roll over: had=%t rolled=%t", hadPrevious, rolledOver)
	}
	if previous.WindowStart != thursday || previous.Entries != 3 {
		t.Fatalf("rollover prev=%+v want window %s with 3 entries", previous, thursday)
	}
	if entries != 1 {
		t.Fatalf("entries after rollover=%d want 1", entries)
	}
	if snapshot, ok := runtimeRaidLukeWeeklyObservations.snapshotForTest(19); !ok || snapshot.WindowStart != nextWindow {
		t.Fatalf("stored window=%+v want %s", snapshot, nextWindow)
	}
	if _, ok := runtimeRaidLukeWeeklyObservations.snapshotForTest(20); ok {
		t.Fatal("an unrelated character picked up a weekly record")
	}
}

// TestRuntimeRaidLukeWeeklyLimitIsOffByDefault is the guard on the switch: the
// whole point of shipping this as observation is that nothing refuses until the
// reset node has been read off real traffic.
func TestRuntimeRaidLukeWeeklyLimitIsOffByDefault(t *testing.T) {
	t.Parallel()
	service := &Service{}
	if service.runtimeRaidLukeWeeklyLimitEnforced() {
		t.Fatal("weekly enforcement is on with no option set")
	}
	// The refusal path must not even read the repositories while the switch is off,
	// so a zero Service (no repositories at all) must answer "do not refuse".
	if _, _, refuse := service.runtimeRaidLukeWeeklyRefusal(19); refuse {
		t.Fatal("the refusal path refused with the switch off")
	}
	service.options.raidLukeWeeklyLimitEnforced = true
	if !service.runtimeRaidLukeWeeklyLimitEnforced() {
		t.Fatal("the option did not arm enforcement")
	}
	// Armed but with no catalog and no repositories it still must not refuse: an
	// unreadable counter is not an exhausted counter.
	if _, _, refuse := service.runtimeRaidLukeWeeklyRefusal(19); refuse {
		t.Fatal("the refusal path refused without a readable weekly counter")
	}
}
