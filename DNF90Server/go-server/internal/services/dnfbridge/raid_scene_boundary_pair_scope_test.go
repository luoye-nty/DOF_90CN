package dnfbridge

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// This file pins the SCOPE of the raid mate-actor install at a dungeon lifecycle
// boundary. It is a behaviour pin, not a measurement: the measurement harness for
// the same projection is stress_raid_projection_test.go.
//
// The four boundaries (selected / loaded / completed / abandoned) used to install
// every ordered pair of frozen members -- N*(N-1) sends per boundary -- although
// the event names ONE member changing scene. That was cut to the moved member's own
// pairs (2*(N-1)) on 2026-09-27, and cut again on 2026-09-29 to the board ROW the
// moved member appears in, for the reason the operator reported against the
// 2*(N-1) form:
//
//	"1队进图了 2队在外面看 看不到是哪队进图 就显示个单 队长头像也是黑的"
//
// The 战况表 draws one cell per (dungeon, squad) row and resolves it through the
// record of the row's FIRST member, which is not the member that entered -- so the
// moved-only scope installed a record nobody drew and left the cell unresolvable
// (live 2026-09-29 12:25:33..12:31:32, raid 1507329; see
// runtimeRaidBoardRowInstallScope and _diag\_scan_phase2.txt).
//
// Measured on the live server (203.0.113.11, 2026-09-27, 600 MB of packet_log over
// ~1.7 h): the four boundaries produced 11,298 of the 11,663 peer-actor installs,
// and 88.2% of the `loaded` boundary's were an identical (target, actor, mode1
// length) triple already sent to the same target for the same actor within 10 s.
// The row scope keeps that property: it never installs a pair that belongs to a
// squad which did not move.

// raidMateActorInstall is one mate-actor install as the production packet log
// recorded it: which receiver got which actor's record, from which caller.
type raidMateActorInstall struct {
	source string
	target uint16
	actor  uint16
}

// raidMateActorInstalls parses the mate-actor installs out of a packet log.
//
// The install is logged twice -- `game-raid-peer-actor-sent` on the receiver's
// connection names the actor and the mode, and the projection it drives logs
// `game-town-copresence-actor-state-send ... target_char_id=R actor_char_id=A`,
// which names BOTH ends explicitly. The pair is taken from the second line for
// that reason.
func raidMateActorInstalls(t *testing.T, data string) []raidMateActorInstall {
	t.Helper()
	var installs []raidMateActorInstall
	for _, line := range strings.Split(data, "\n") {
		if !strings.Contains(line, "kind=game-town-copresence-actor-state-send") ||
			!strings.Contains(line, "source=raid_") {
			continue
		}
		installs = append(installs, raidMateActorInstall{
			source: raidPacketField(line, "source="),
			target: raidPacketNumber(t, line, "target_char_id="),
			actor:  raidPacketNumber(t, line, "actor_char_id="),
		})
	}
	return installs
}

func raidPacketField(line, key string) string {
	index := strings.Index(line, key)
	if index < 0 {
		return ""
	}
	rest := line[index+len(key):]
	if end := strings.IndexAny(rest, " \t\r\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

func raidPacketNumber(t *testing.T, line, key string) uint16 {
	t.Helper()
	raw := raidPacketField(line, key)
	value, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		t.Fatalf("parse %s out of %q: %v", key, line, err)
	}
	return uint16(value)
}

// TestRuntimeRaidDungeonBoundaryInstallsTheMovedMembersBoardRow fails if a
// dungeon lifecycle boundary installs anything other than the board ROW the moved
// member appears in.
//
// The scope rule, in one line: a mate that stayed behind gets the record of every
// member of the moved member's 战况表 row, because the board resolves that cell
// through the row's own first member -- not through whoever happened to walk into
// the gate. Live 2026-09-29 12:25:33 (raid 1507329, 4 squads x 4 members): squad
// 1's slot-1 member entered 218, every observer received THAT member's record, and
// the squad's row (first member = the squad leader) still drew as 单人 with a black
// avatar on three of the four clients until phase activation swept again at
// 12:31:32. The move-only scope this test used to pin is what produced that.
//
// The fixture's roster is runtimeRaidGroupSize members per squad, so memberCount=8
// is two squads: the moved member sits in squad 1 and the row scope is exactly
// squad 1. That is what makes this test discriminating -- with one squad the new
// scope and the old every-pair sweep coincide.
func TestRuntimeRaidDungeonBoundaryInstallsTheMovedMembersBoardRow(t *testing.T) {
	const memberCount = 8
	fixture := newStressRaidProjectionFixture(t, memberCount)
	mover := fixture.members[0]

	rowScope := make([]uint16, 0, runtimeRaidGroupSize)
	inRow := make(map[uint16]bool, runtimeRaidGroupSize)
	for index := 0; index < runtimeRaidGroupSize; index++ {
		id := fixture.members[index].charID
		rowScope = append(rowScope, id)
		inRow[id] = true
	}
	if inRow[mover.charID] != true {
		t.Fatalf("fixture: the moved member %d is not in the row it should be", mover.charID)
	}
	squadTwo := make(map[uint16]bool, memberCount-runtimeRaidGroupSize)
	for index := runtimeRaidGroupSize; index < memberCount; index++ {
		squadTwo[fixture.members[index].charID] = true
	}

	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: mover.id},
		lifecycleToken: 41,
	}
	mover.session.dungeon = dungeonSessionState{runtime: runtime}

	logPath := filepath.Join(t.TempDir(), "raid-boundary-row-scope.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.packetLog.Store(logger)
	if err := fixture.service.freezeRuntimeRaidDungeonSelection(mover.session, runtime); err != nil {
		t.Fatalf("real selected trigger: %v", err)
	}
	if err := fixture.service.notifyRuntimeRaidDungeonLoaded(mover.session); err != nil {
		t.Fatalf("real loaded trigger: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	installs := raidMateActorInstalls(t, stressRaidReadLog(t, logPath))
	if len(installs) == 0 {
		t.Fatal("the two boundaries installed no mate actor at all")
	}

	received := make(map[uint16]map[uint16]bool, memberCount)
	perSource := map[string]int{}
	for _, install := range installs {
		perSource[install.source]++
		if install.target == install.actor {
			t.Errorf("%s installed actor %d into itself", install.source, install.actor)
		}
		// 1. The scope rule: an install into a member that STAYED belongs to the
		//    moved member's board row, and to nothing else.
		if install.target != mover.charID && !inRow[install.actor] {
			t.Errorf("%s installed actor %d into target %d, but actor %d is outside the moved member's row %v: "+
				"that pair belongs to another squad's row and neither of its scenes changed",
				install.source, install.actor, install.target, install.actor, rowScope)
		}
		// ... and never a pair wholly inside another squad (the verbatim-repeat
		// class the move-only scope removed; it must not come back through this).
		if squadTwo[install.actor] && squadTwo[install.target] {
			t.Errorf("%s installed actor %d into target %d: both are in a squad that did not move",
				install.source, install.actor, install.target)
		}
		if received[install.target] == nil {
			received[install.target] = map[uint16]bool{}
		}
		received[install.target][install.actor] = true
	}

	// 2. Every stayed member got the whole row, minus itself. This is the
	//    resolution the 战况表 cell needs: whichever of the row's members the
	//    client keys the cell by, it now has a record for it.
	for _, member := range fixture.members {
		if member.charID == mover.charID {
			continue
		}
		for _, rowID := range rowScope {
			if rowID == member.charID {
				continue
			}
			if !received[member.charID][rowID] {
				t.Errorf("member %d never received row member %d's record, so its board cell for that row stays unresolvable",
					member.charID, rowID)
			}
		}
	}

	// 3. The moved member still gets every mate: its own scene was rebuilt.
	for _, member := range fixture.members {
		if member.charID == mover.charID {
			continue
		}
		if !received[mover.charID][member.charID] {
			t.Errorf("the moved member never received mate %d's record", member.charID)
		}
	}

	// 4. The count is the scope: mates x (row - itself) + mates, per boundary.
	wantPerBoundary := 0
	for _, member := range fixture.members {
		if member.charID == mover.charID {
			continue
		}
		for _, rowID := range rowScope {
			if rowID != member.charID {
				wantPerBoundary++
			}
		}
	}
	wantPerBoundary += memberCount - 1
	for source, count := range perSource {
		if count != wantPerBoundary {
			t.Errorf("%s installed %d mate actors, want %d (row scope %d x %d stayers + %d mates into the mover); "+
				"the moved-only scope installs %d and the every-pair sweep %d",
				source, count, wantPerBoundary, len(rowScope), memberCount-1, memberCount-1,
				2*(memberCount-1), memberCount*(memberCount-1))
		}
	}
	if want := 2 * wantPerBoundary; len(installs) != want {
		t.Errorf("the two boundaries installed %d mate actors in total, want %d", len(installs), want)
	}
	t.Logf("boundary installs=%d perSource=%v row=%v (moved-only would install %d per boundary, every-pair %d)",
		len(installs), perSource, rowScope, 2*(memberCount-1), memberCount*(memberCount-1))
}

// TestRuntimeRaidSceneBoundaryFullSweepEntryPointKeepsEveryPair is the other half
// of the scope pin. Phase activation reaches the projection through
// syncRuntimeRaidMateActorsAll, whose contract is that EVERY pair of frozen
// members gets the other's actor -- it does not know about any single moving
// member, and it must keep installing every ordered pair.
func TestRuntimeRaidSceneBoundaryFullSweepEntryPointKeepsEveryPair(t *testing.T) {
	const memberCount = 4
	fixture := newStressRaidProjectionFixture(t, memberCount)

	logPath := filepath.Join(t.TempDir(), "raid-full-sweep.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.packetLog.Store(logger)
	fixture.runSceneBoundary("raid_phase_activation_raid_mate_actors", false)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	installs := raidMateActorInstalls(t, stressRaidReadLog(t, logPath))
	if want := memberCount * (memberCount - 1); len(installs) != want {
		t.Errorf("the full-sweep entry point installed %d mate actors, want every ordered pair = %d",
			len(installs), want)
	}
	for _, install := range installs {
		if install.target == install.actor {
			t.Errorf("the full sweep installed actor %d into itself", install.actor)
		}
	}
	t.Logf("full-sweep entry point installs=%d (N*(N-1) with N=%d)", len(installs), memberCount)
}
