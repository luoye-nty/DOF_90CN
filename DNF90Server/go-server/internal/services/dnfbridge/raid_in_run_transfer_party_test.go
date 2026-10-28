package dnfbridge

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
)

// currentPartySelectorEntryPreflightFor runs exactly the leader-side preparation
// the op16 handler runs before any member commits, with the in-run transfer
// relaxation selectable. It mirrors currentPartySelectorEntryPreflight so the two
// callers cannot drift apart.
func currentPartySelectorEntryPreflightFor(
	t *testing.T,
	service *Service,
	leader *gameSession,
	inRunTransfer bool,
) ([]*runtimePartyPreparedDungeonEntry, error) {
	t.Helper()
	selectBody := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(selectBody[:4], 700)
	request, err := dungeoncmd.DecodeSelectDungeonRequest(selectBody)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runtime, scene, err := service.prepareDungeonRuntime(ctx, leader, request)
	if err != nil {
		t.Fatalf("prepare leader runtime: %v", err)
	}
	return service.prepareRuntimePartyDungeonEntry(
		ctx, leader, runtimePartyStateSnapshot(leader), request, runtime, scene, false, inRunTransfer)
}

// Inside a raid gate the whole squad is off the town roster and every member is
// standing in a live run, so neither gate the party preflight uses can hold: the
// town co-presence anchor is gone by design (leaveTownPresenceForDungeon) and the
// member has an active runtime instead of a selector page.
//
// Live 2026-09-19 23:36:14Z, raid 1507329, party 2: those two gates are exactly why
// the squad never followed its leader through the energy-interception portal -- the
// leader entered 222 alone and the member stayed in 218 for 235 s with every room
// transition consumed, until the server force-returned it to town.
//
// The relaxation must stay scoped: an ordinary party entry in the same state keeps
// refusing, so 选择其他地下城 and every area/channel refusal are untouched.
func TestRaidInRunTransferRelaxationIsScopedToTheInRunTransfer(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		inRunTransfer bool
		wantErr       string
	}{
		{name: "ordinary party entry still refuses", inRunTransfer: false, wantErr: "is not present in the leader town area"},
		{name: "in-run transfer carries the squad", inRunTransfer: true, wantErr: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, leader, follower := prepareCurrentPartySelectorEntryTest(t)
			// Entering the gate: both members leave the town roster for real, and the
			// member that is about to follow is standing inside a live run.
			service.leaveTownPresenceForDungeon(leader)
			service.leaveTownPresenceForDungeon(follower)
			follower.dungeon.mu.Lock()
			follower.dungeon.runtime = &runtimeDungeonState{}
			follower.dungeon.mu.Unlock()

			if coPresent, _ := service.currentPartyEntryCoPresence(leader, follower); coPresent {
				t.Fatal("the fixture no longer reproduces the in-raid state: the members still share a town anchor")
			}
			if ready, reason := service.currentTownEnterSelectReady(follower); ready {
				t.Fatalf("the fixture no longer reproduces the in-raid state: the member has no active run (%s)", reason)
			}

			prepared, err := currentPartySelectorEntryPreflightFor(t, service, leader, testCase.inRunTransfer)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("err=%v prepared=%d, want the %q refusal", err, len(prepared), testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("the in-run transfer was refused: %v", err)
			}
			if len(prepared) != 1 || prepared[0].session != follower || prepared[0].runtime == nil {
				t.Fatalf("prepared followers=%d want the squadmate", len(prepared))
			}
			// The squadmate must be prepared for the very run the opener committed:
			// same dungeon and same scene, so the shared-world owner can admit both.
			if prepared[0].runtime.Dungeon.ID != prepared[0].plan.dungeonID {
				t.Fatalf("prepared dungeon=%d plan=%d", prepared[0].runtime.Dungeon.ID, prepared[0].plan.dungeonID)
			}
			if prepared[0].scene.Map.Map.ID != prepared[0].plan.mapID {
				t.Fatalf("prepared map=%d plan=%d", prepared[0].scene.Map.Map.ID, prepared[0].plan.mapID)
			}
			if prepared[0].runtime.Seed != prepared[0].plan.seed {
				t.Fatalf("prepared seed=%d plan=%d", prepared[0].runtime.Seed, prepared[0].plan.seed)
			}
		})
	}
}

// The squad-together warp commits the OPENER first and lets its squadmates adopt
// the run the opener opened. Reversing that order is not a refusal -- it silently
// hands the single run's ownership (and therefore which member publishes the
// squad's loaded/clear lifecycle) to a squadmate. This locks the contract
// enterRuntimeRaidInRunTransferFollowers relies on.
func TestRaidHatcheryRunIsOwnedByWhicheverMemberBeginsItFirst(t *testing.T) {
	fixture := newRuntimeRaidPhaseTwoBarrierFixture(t)
	leaderRun := fixture.enterGate(t, 1001, 21)
	fixture.adoptGateRun(t, 1002, 22, leaderRun)
	fixture.partyState(t, 1001, 1002)

	fixture.service.raidMu.Lock()
	state := fixture.service.raids[fixture.raidKey]
	state.ensureRuntimeRaidLifecycleMapsLocked()
	opened, _ := openRuntimeRaidHatcheriesLocked(state, func(int) int { return 0 })
	fixture.service.raidMu.Unlock()
	if len(opened) < 2 {
		t.Fatalf("the fixture opened %d hatcheries", len(opened))
	}
	target := opened[1]

	// The opener commits first, exactly like the solo warp that
	// enterRuntimeRaidInRunTransferFollowers runs before it drives the squad.
	openerToken := uint64(23)
	openerBinding, err := fixture.service.beginRuntimeRaidDungeon(1001, target, openerToken)
	if err != nil {
		t.Fatalf("the opener was refused the hatchery it walked into: %v", err)
	}
	if openerBinding.MapID != target || openerBinding.LifecycleToken != openerToken {
		t.Fatalf("the opener did not open the run: %+v", openerBinding)
	}

	// The squadmate joins the run its opener holds instead of opening a rival one.
	squadmateBinding, err := fixture.service.beginRuntimeRaidDungeon(1002, target, 24)
	if err != nil {
		t.Fatalf("the squadmate was refused the run its opener holds: %v", err)
	}
	if squadmateBinding.LifecycleToken != openerToken {
		t.Fatalf("the squadmate opened its own run: token=%d want the opener's %d",
			squadmateBinding.LifecycleToken, openerToken)
	}

	fixture.service.raidMu.Lock()
	defer fixture.service.raidMu.Unlock()
	squadRuns := 0
	for key, progress := range fixture.service.raids[fixture.raidKey].Lifecycle.Dungeons {
		if key.MapID == target {
			squadRuns++
			if progress.CharacterID != 1001 {
				t.Fatalf("the hatchery run is owned by %d, want the opener 1001", progress.CharacterID)
			}
			if progress.LifecycleToken != openerToken {
				t.Fatalf("the hatchery run token=%d want %d", progress.LifecycleToken, openerToken)
			}
		}
	}
	if squadRuns != 1 {
		t.Fatalf("the squad holds %d runs of hatchery %d, want exactly one", squadRuns, target)
	}
}
