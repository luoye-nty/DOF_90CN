package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

// raidDungeonStateBodyShape reads the (rowCount, trailingDword) shape of a 0x0249
// body: reserved u8, row count u8, count x (u32 dungeonID,u8 state), then the
// trailing dword. That trailer is the infected hatchery's map id on the wire.
func raidDungeonStateBodyShape(t *testing.T, body []byte) (int, uint32) {
	t.Helper()
	if len(body) < 6 {
		t.Fatalf("dungeon state body too short: %x", body)
	}
	count := int(body[1])
	want := 2 + count*5 + 4
	if len(body) != want {
		t.Fatalf("dungeon state body len=%d want=%d body=%x", len(body), want, body)
	}
	return count, binary.LittleEndian.Uint32(body[2+count*5:])
}

// The infection is the trailing dword of 0x0249 and NOTHING else on the wire carries
// it: the current EXE's handler sub_1D39B50 hands it to sub_17B87B0, the only writer
// of manager+0x150, which every 战况表 renderer compares each gate against. A gate
// change travels as 0x024A, which has no trailer, so before this the infection only
// ever reached the client with a WHOLE table.
//
// Live 2026-09-20, raid 1507329: the trailer moved on the wire exactly twice in ten
// minutes, at 02:13:02Z and 02:19:13Z, and both were op15-driven whole tables. The
// operator's report follows from that directly -- "在城镇中打开战况表 不显示感染
// 进图以后 打开战况表才显示感染" (entering a dungeon is what sends op15), and after
// clearing the infected hatchery the board kept colouring it for the same reason.
//
// This is the regression guard for the edge: an infection change with no whole table
// going out must still ship the trailer, and must do it as a ZERO-row 0x0249, because
// a whole table would re-announce every cleared gate and this client answers that
// with the 「已通关[%s]地下城」 toast.
func TestRaidInfectionTrailerTravelsWithoutAWholeTable(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)

	// Seed the phase's row table the way phase activation does, with no infection
	// yet, so the hatchery-open edge is a change to an existing table rather than a
	// first table (a first table carries the trailer by itself and would not prove
	// anything about the row-only path).
	if err := fixture.service.broadcastRuntimeRaidPhaseSnapshot(fixture.state(t), false, fixture.queue.Now()); err != nil {
		t.Fatalf("seed phase two snapshot: %v", err)
	}
	connection, ok := fixture.sessions[1001].conn.(*bufferConn)
	if !ok {
		t.Fatalf("fixture session has no recording connection")
	}
	connection.write.Reset()

	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)

	// raidRewardRandom returns 0: 221 is omitted and 222 is the infected hatchery
	// (the same selection TestAntonPhaseTwoHatcheriesOpenInfectAndRecover asserts).
	state := fixture.state(t)
	if state.Lifecycle.InfectionMapID != 222 || state.Lifecycle.InfectionSuppressed {
		t.Fatalf("infection=%d suppressed=%v want 222/false", state.Lifecycle.InfectionMapID, state.Lifecycle.InfectionSuppressed)
	}

	var rows []int
	var tails []uint32
	stream := connection.write.Bytes()
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidDungeonStateMsgID {
			continue
		}
		count, tail := raidDungeonStateBodyShape(t, packet.Body)
		rows = append(rows, count)
		tails = append(tails, tail)
	}
	if len(tails) != 1 {
		t.Fatalf("0x0249 packets on the hatchery-open edge: rows=%v tails=%v want exactly one zero-row trailer", rows, tails)
	}
	if rows[0] != 0 || tails[0] != 222 {
		t.Fatalf("0x0249 on the hatchery-open edge rows=%d tail=%d want rows=0 tail=222", rows[0], tails[0])
	}

	// The gate rows themselves must still travel as one-row 0x024A changes: a whole
	// table here would re-announce every gate this phase already closed.
	stream = connection.write.Bytes()
	changes := 0
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification == 0 && packet.Header.MsgID == raid.RaidDungeonChangeMsgID {
			changes++
		}
	}
	if changes != antonHatcheryOpenCount {
		t.Fatalf("0x024A changes on the hatchery-open edge=%d want %d (one per opened hatchery)", changes, antonHatcheryOpenCount)
	}
}

// Clearing the infected hatchery suppresses the marker, which the client reads from
// the SAME trailer: the symbol table alone cannot clear it, because the trailer
// handler only ever SETS symbol 7 (cmp [esi+0x150],0 / je 0x17B87E6) and leaves it
// alone on a zero value. So the suppression edge has to move the trailer to 0, and
// it must do that even though the cleared gate's own row is moving in the same
// snapshot.
func TestRaidInfectionTrailerClearsWhenTheInfectedHatcheryIsSuppressed(t *testing.T) {
	fixture := newAntonPhaseTwoFixture(t)
	if err := fixture.service.broadcastRuntimeRaidPhaseSnapshot(fixture.state(t), false, fixture.queue.Now()); err != nil {
		t.Fatalf("seed phase two snapshot: %v", err)
	}
	fixture.queue.fire(fixture.task(t, runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID), false)

	connection, ok := fixture.sessions[1001].conn.(*bufferConn)
	if !ok {
		t.Fatalf("fixture session has no recording connection")
	}
	connection.write.Reset()

	fixture.clear(t, 222)
	if state := fixture.state(t); !state.Lifecycle.InfectionSuppressed {
		t.Fatalf("clearing the infected hatchery left suppressed=false infection=%d", state.Lifecycle.InfectionMapID)
	}
	if got := runtimeRaidInfectionMapIDForWire(fixture.state(t)); got != 0 {
		t.Fatalf("trailer for a suppressed infection=%d want 0", got)
	}
	// The production clear edge publishes its snapshot through
	// publishRuntimeRaidObjectiveProgress (raid_dungeon_lifecycle.go:295 for a
	// committed completion), which is the announce=false path this guard covers.
	if err := fixture.service.publishRuntimeRaidObjectiveProgress(fixture.raidKey, runtimeRaidDungeonPhaseTwo); err != nil {
		t.Fatalf("publish progress after clearing the infected hatchery: %v", err)
	}

	stream := connection.write.Bytes()
	var tails []uint32
	var rows []int
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidDungeonStateMsgID {
			continue
		}
		count, tail := raidDungeonStateBodyShape(t, packet.Body)
		rows = append(rows, count)
		tails = append(tails, tail)
	}
	if len(tails) != 1 {
		t.Fatalf("0x0249 packets on the suppression edge: rows=%v tails=%v want exactly one zero-row trailer", rows, tails)
	}
	if rows[0] != 0 || tails[0] != 0 {
		t.Fatalf("0x0249 on the suppression edge rows=%d tail=%d want rows=0 tail=0", rows[0], tails[0])
	}
}
