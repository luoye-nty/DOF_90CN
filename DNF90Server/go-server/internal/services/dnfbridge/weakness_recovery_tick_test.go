package dnfbridge

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Recovery is lazy and was already correct, but nothing republished it between
// rebuilds, so standing in town left the bar frozen until the player
// re-selected a character. The tick fires exactly when the next level is due,
// not on a fixed poll, so it costs one timer per weakened player.
func TestWeaknessRecoveryDueCountsDownToTheNextLevel(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	character := func(startedAt int64) dnfrepo.CharacterRecord {
		stats := map[string]int64{currentWeaknessStatKey: 4}
		if startedAt != 0 {
			stats[currentWeaknessRecoveryAtStatKey] = startedAt
		}
		return dnfrepo.CharacterRecord{CharacterID: "1", Stats: stats}
	}

	// Countdown just started: a full interval to wait.
	if got := currentWeaknessRecoveryDue(character(base.Unix()), base); got != currentWeaknessRecoveryInterval {
		t.Fatalf("due=%s want %s", got, currentWeaknessRecoveryInterval)
	}
	// Partway in: the remainder, not a fresh interval -- otherwise a player who
	// relogs often would push the next level away forever. The offset is a
	// fraction of the interval rather than a literal minute so this holds
	// whatever the interval is configured to.
	offset := currentWeaknessRecoveryInterval / 5
	if offset <= 0 {
		t.Fatalf("interval %s is too short to split into fifths", currentWeaknessRecoveryInterval)
	}
	partway := base.Add(offset)
	if got := currentWeaknessRecoveryDue(character(base.Unix()), partway); got != currentWeaknessRecoveryInterval-offset {
		t.Fatalf("due=%s want %s", got, currentWeaknessRecoveryInterval-offset)
	}
	// Exactly on a boundary owes the next whole interval, never zero, so the
	// timer cannot be scheduled to fire immediately in a loop.
	boundary := base.Add(currentWeaknessRecoveryInterval)
	if got := currentWeaknessRecoveryDue(character(base.Unix()), boundary); got != currentWeaknessRecoveryInterval {
		t.Fatalf("boundary due=%s want %s", got, currentWeaknessRecoveryInterval)
	}
	// Several intervals of arrears still resolve to a whole next interval.
	late := base.Add(3*currentWeaknessRecoveryInterval + offset)
	if got := currentWeaknessRecoveryDue(character(base.Unix()), late); got != currentWeaknessRecoveryInterval-offset {
		t.Fatalf("late due=%s want %s", got, currentWeaknessRecoveryInterval-offset)
	}
	// No countdown stored yet: owe a full interval rather than firing at once.
	if got := currentWeaknessRecoveryDue(character(0), base); got != currentWeaknessRecoveryInterval {
		t.Fatalf("missing countdown due=%s want %s", got, currentWeaknessRecoveryInterval)
	}
	// A backwards clock must not produce a negative or zero delay.
	if got := currentWeaknessRecoveryDue(character(base.Add(time.Hour).Unix()), base); got != currentWeaknessRecoveryInterval {
		t.Fatalf("backwards clock due=%s want %s", got, currentWeaknessRecoveryInterval)
	}
}

// The tick is armed for a weakened character and cancelled once healthy, so a
// fully recovered player leaves no timer behind.
func TestWeaknessRecoveryTimerArmsWhileWeakenedAndCancelsWhenHealthy(t *testing.T) {
	service, session, _, _ := prepareCurrentWeaknessTest(t, 20, 4, 0, nil)
	queue, ok := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	if !ok {
		t.Fatalf("unexpected timer queue %T", service.gameplayTimers)
	}
	name := currentWeaknessRecoveryTimerName(session.selectedCharacterID)

	character := dnfrepo.CharacterRecord{CharacterID: "5", Stats: map[string]int64{
		currentWeaknessStatKey:           4,
		currentWeaknessRecoveryAtStatKey: queue.now.Unix(),
	}}
	service.armCurrentWeaknessRecoveryTimer(session, character, 4, "test")
	if _, armed := queue.active[name]; !armed {
		t.Fatalf("no recovery tick armed: %v", queue.active)
	}

	// Healthy: the tick must be cancelled, not re-armed.
	service.armCurrentWeaknessRecoveryTimer(session, dnfrepo.CharacterRecord{CharacterID: "5"}, 0, "test")
	if _, armed := queue.active[name]; armed {
		t.Fatalf("a healthy character kept a recovery tick: %v", queue.active)
	}
}

// Publishing the cap for a weakened character has to arm the tick, and the
// callback has to re-resolve the session: the tick is the only thing that keeps
// the bar moving while the player just stands in town.
func TestWeaknessConditionRefreshArmsTheRecoveryTick(t *testing.T) {
	service, session, _, _ := prepareCurrentWeaknessTest(t, 20, 4, 0, nil)
	queue, ok := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	if !ok {
		t.Fatalf("unexpected timer queue %T", service.gameplayTimers)
	}
	if err := service.sendCurrentWeaknessConditionRefresh(session, "test"); err != nil {
		t.Fatal(err)
	}
	name := currentWeaknessRecoveryTimerName(session.selectedCharacterID)
	task := queue.active[name]
	if task == nil {
		t.Fatalf("a weakened publish armed no recovery tick: %v", queue.active)
	}
	// The tick owes the remainder of the countdown the settle just started, so
	// it must be a whole interval, never zero.
	if task.delay != currentWeaknessRecoveryInterval {
		t.Fatalf("tick delay=%s want %s", task.delay, currentWeaknessRecoveryInterval)
	}
	// The callback for a character that is not online must do nothing at all
	// rather than publish into a stale session.
	before := len(session.conn.(*bufferConn).write.Bytes())
	queue.fire(task, true)
	if after := len(session.conn.(*bufferConn).write.Bytes()); after != before {
		t.Fatalf("an offline character's tick published %d extra bytes", after-before)
	}
}
