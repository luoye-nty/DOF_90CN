package dnfbridge

import (
	"path/filepath"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// This file pins the trigger condition of the rebuilt-scene participation replay
// (raid_current_compat.go: sendRuntimeRaidDungeonParticipationOnFinishLoading ->
// replayRuntimeRaidStateForRebuiltScene -> replayRuntimeRaidParticipationRows).
//
// 服主实测 2026-09-29：在卢克/安徒恩副本里**每过一个房间**聊天框就弹一次
// 「其中1支战队已进入[诞生之圣所]地下城。」(客户端字符串 32048)。成因不是"多发了一个
// 包"，而是这一串调用在**每一次装载完成**时都跑：门内换房间 = 客户端再装载一次 =
// 重放一次参战行 = op=1 再插一次行 = 客户端 sub_17C0F20 再弹一次 32048
// (op 表见 publishRuntimeRaidDungeonParticipation 与
// `安徒恩_攻坚队技能与小队技能与战况表_参考对照_20260923.md` §3.2)。
//
// 现在按"客户端的战况表手上是哪一份参战行集"做幂等：门内换房间不改变行集(不重放)，
// 真正换门/换图会改变行集(重放)——而丢包正好发生在换门那一刻，所以自愈能力仍然在，
// 只是不再每房刷屏。两条用例一条钉"不再重放"，一条钉"该重放时仍然重放到每一个成员"。

// raidParticipationReplayFixture is one raid whose members can be walked into gates
// through the real entry path (freezeRuntimeRaidDungeonSelection) and whose wire is
// captured per connection.
type raidParticipationReplayFixture struct {
	t       *testing.T
	service *Service
	logPath string
	logger  *packetLogger
}

func newRaidParticipationReplayFixture(t *testing.T, memberCount int) (*raidParticipationReplayFixture, *stressRaidProjectionFixture) {
	t.Helper()
	raid := newStressRaidProjectionFixture(t, memberCount)
	logPath := filepath.Join(t.TempDir(), "raid-participation-replay.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	raid.service.packetLog.Store(logger)
	t.Cleanup(func() { _ = logger.Close() })
	return &raidParticipationReplayFixture{
		t:       t,
		service: raid.service,
		logPath: logPath,
		logger:  logger,
	}, raid
}

// bindGate freezes one member's session onto a real raid run in one gate, exactly
// like the dungeon-entry commit does. It deliberately does NOT publish the loaded
// event: the caller drives that through the finish-loading hook under test.
func (fixture *raidParticipationReplayFixture) bindGate(session *gameSession, runes string, mapID int, lifecycleToken uint64) *runtimeDungeonState {
	fixture.t.Helper()
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(mapID)},
		Character:      dnfrepo.CharacterRecord{CharacterID: runes},
		lifecycleToken: lifecycleToken,
	}
	session.dungeon = dungeonSessionState{runtime: runtime}
	if err := fixture.service.freezeRuntimeRaidDungeonSelection(session, runtime); err != nil {
		fixture.t.Fatalf("freeze raid gate %d for char %s: %v", mapID, runes, err)
	}
	return runtime
}

// leaveGate walks a squad out of its gate the way the real path does: the abandoned
// lifecycle event removes the gate's row from every board and releases the seat.
func (fixture *raidParticipationReplayFixture) leaveGate(session *gameSession, runtime *runtimeDungeonState) {
	fixture.t.Helper()
	session.dungeon.mu.Lock()
	err := fixture.service.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime)
	session.dungeon.mu.Unlock()
	if err != nil {
		fixture.t.Fatalf("abandon raid gate %d: %v", runtime.Request.DungeonID, err)
	}
}

// finishLoading is the production hook the client's FINISH_LOADING (op37) reaches.
func (fixture *raidParticipationReplayFixture) finishLoading(session *gameSession) {
	fixture.t.Helper()
	if err := fixture.service.sendRuntimeRaidDungeonParticipationOnFinishLoading(session); err != nil {
		fixture.t.Fatalf("finish loading: %v", err)
	}
}

func (fixture *raidParticipationReplayFixture) close() string {
	fixture.t.Helper()
	if err := fixture.logger.Close(); err != nil {
		fixture.t.Fatalf("close packet logger: %v", err)
	}
	return stressRaidReadLog(fixture.t, fixture.logPath)
}

func raidParticipationOpsContain(ops []int, want int) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}

// raidParticipationOpCount counts one op among the rows of a single gate. The number
// of op=1 rows is exactly the number of times the client pops
// 32048 「其中1支战队已进入[%s]地下城。」, so "one entry = one line" is a count pin.
func raidParticipationOpCount(ops []int, want int) int {
	count := 0
	for _, op := range ops {
		if op == want {
			count++
		}
	}
	return count
}

// TestRuntimeRaidParticipationReplayIsIdempotentInsideOneGate is the regression pin
// for the 2026-09-29 report "每过一个房间就弹一次 32048", extended 2026-10-01 for the
// follow-up "有时候进图 这个提示会刷两条".
//
// One squad, one gate. The first finish-loading (the entry) MUST re-state the rows --
// that is the self-heal that puts back a 584 row the client dropped while its scene
// was being rebuilt. The second finish-loading inside the SAME gate (what every room
// change inside a gate produces) must send NO 584 at all. Changing gate must replay
// again.
//
// The re-state must use op=2, never op=1: the client's op=1 branch (0x01D43B7A) pops
// 32048 on top of inserting the row, while op=2 (0x01D43B44) runs the SAME
// sub_17C0F20 insert and skips the print tail. The entry publish already announced
// the entry with op=1, so a replay that announced again is exactly the second line
// the operator saw at 2026-09-30 23:54:31 (packet log: pkt 3995/3996 = 0,1 from the
// entry publish, pkt 4000/4001 = 0,1 from this replay).
func TestRuntimeRaidParticipationReplayIsIdempotentInsideOneGate(t *testing.T) {
	fixture, raid := newRaidParticipationReplayFixture(t, runtimeRaidGroupSize)
	mover := raid.members[0]

	gateOne := fixture.bindGate(mover.session, mover.id, 210, 41)

	// 1. 进副本：装载完成那一次必须重放（自愈能力不许丢）。
	if err := fixture.service.sendRuntimeRaidDungeonParticipationOnFinishLoading(mover.session); err != nil {
		t.Fatal(err)
	}
	entry := raidParticipationOpsForTest(t, mover.conn.stream(), 210)
	if !raidParticipationOpsContain(entry, 2) {
		t.Fatalf("进图那一次没有重放参战行 ops=%v，自愈能力被删掉了", entry)
	}
	// 进图 = 恰好一条 32048。这两条 op=1 曾经同时出现（现场包日志 23:54:31 与
	// 00:02:18 都是 `0,1` 紧跟 `0,1`），聊天框就刷两条。
	if got := raidParticipationOpCount(entry, 1); got != 1 {
		t.Fatalf("进图那一次 op=1 出现 %d 次 ops=%v，want 1：多一次就多弹一条「已进入」", got, entry)
	}

	// 2. 同一个门里换房间：再说一次"装载完成"。
	mover.conn.reset()
	if err := fixture.service.sendRuntimeRaidDungeonParticipationOnFinishLoading(mover.session); err != nil {
		t.Fatal(err)
	}
	if ops := raidParticipationOpsForTest(t, mover.conn.stream(), 210); len(ops) != 0 {
		t.Fatalf("同一个门(210)内第二次装载完成又重放了参战行 ops=%v：客户端会再弹一次 32048", ops)
	}

	// 3. 换门 210 -> 213：行集变了，必须重放。
	fixture.leaveGate(mover.session, gateOne)
	fixture.bindGate(mover.session, mover.id, 213, 42)
	mover.conn.reset()
	if err := fixture.service.sendRuntimeRaidDungeonParticipationOnFinishLoading(mover.session); err != nil {
		t.Fatal(err)
	}
	changed := raidParticipationOpsForTest(t, mover.conn.stream(), 213)
	if !raidParticipationOpsContain(changed, 0) || !raidParticipationOpsContain(changed, 2) {
		t.Fatalf("换门之后没有重新重放参战行 ops=%v（op=0/op=2 都要）", changed)
	}
	if got := raidParticipationOpCount(changed, 1); got != 1 {
		t.Fatalf("换门那一次 op=1 出现 %d 次 ops=%v，want 1：多一次就多弹一条 32048", got, changed)
	}

	log := fixture.close()
	if got := strings.Count(log, "kind=game-raid-participation-replay-skipped"); got != 1 {
		t.Errorf("同一个门内的跳过日志出现 %d 次，want 1\n%s", got, log)
	}
	if got := strings.Count(log, "kind=game-raid-participation-replay-sent"); got != 2 {
		t.Errorf("重放日志出现 %d 次，want 2（进图 + 换门）\n%s", got, log)
	}
}

// TestRuntimeRaidParticipationReplayStillHealsEveryMemberWhenAGateChanges is the
// other half of the pin: the replay must stay a RAID-WIDE re-statement of the gate
// rows, because the client that lost a row is typically NOT the one that moved --
// 服主原文 "1队进图了 2队在外面看 看不到是哪队进图 就显示个单 队长头像也是黑的".
//
// Two squads, each standing in its own gate. A room change by either squad must
// silence the replay for BOTH clients (that is the fix); a real gate change by one
// squad must re-state the rows on BOTH clients (that is the self-heal).
func TestRuntimeRaidParticipationReplayStillHealsEveryMemberWhenAGateChanges(t *testing.T) {
	fixture, raid := newRaidParticipationReplayFixture(t, 2*runtimeRaidGroupSize)
	mover := raid.members[0]
	stayer := raid.members[runtimeRaidGroupSize]
	if stayer.charID == mover.charID {
		t.Fatal("fixture: the two squads share a member")
	}

	// Both squads enter their own gate through the real entry path, and each member's
	// own client finishes loading there. Each of these replays states BOTH rows.
	fixture.bindGate(stayer.session, stayer.id, 216, 51)
	if err := fixture.service.sendRuntimeRaidDungeonParticipationOnFinishLoading(stayer.session); err != nil {
		t.Fatal(err)
	}
	fixture.bindGate(mover.session, mover.id, 210, 52)
	if err := fixture.service.sendRuntimeRaidDungeonParticipationOnFinishLoading(mover.session); err != nil {
		t.Fatal(err)
	}
	if ops := raidParticipationOpsForTest(t, stayer.conn.stream(), 210); !raidParticipationOpsContain(ops, 2) {
		t.Fatalf("队友进 210 之后，留在 216 的那个成员没有拿到 210 的参战行 ops=%v", ops)
	} else if got := raidParticipationOpCount(ops, 1); got != 1 {
		t.Fatalf("队友进 210 那一次 op=1 出现 %d 次 ops=%v，want 1：多一次就多弹一条「已进入 210」", got, ops)
	}

	// A room change inside the move's own gate: nobody may be replayed.
	mover.conn.reset()
	stayer.conn.reset()
	fixture.finishLoading(mover.session)
	for _, peer := range []struct {
		name string
		conn *stressRaidConn
	}{{"mover", mover.conn}, {"stayer", stayer.conn}} {
		if ops := raidParticipationOpsForTest(t, peer.conn.stream(), 210); len(ops) != 0 {
			t.Errorf("%s 在同一个门内的第二次装载完成又收到 210 的参战行 ops=%v", peer.name, ops)
		}
		if ops := raidParticipationOpsForTest(t, peer.conn.stream(), 216); len(ops) != 0 {
			t.Errorf("%s 在同一个门内的第二次装载完成又收到 216 的参战行 ops=%v", peer.name, ops)
		}
	}

	// The moved squad's own client finishing loading on the far side of a real gate
	// change: every member's board has to hear the new row.
	fixture.leaveGate(mover.session, mover.session.dungeon.runtime)
	fixture.bindGate(mover.session, mover.id, 213, 53)
	mover.conn.reset()
	stayer.conn.reset()
	fixture.finishLoading(mover.session)
	if ops := raidParticipationOpsForTest(t, mover.conn.stream(), 213); !raidParticipationOpsContain(ops, 2) {
		t.Errorf("换门之后移动者自己没有再收到 213 的参战行 ops=%v", ops)
	} else if got := raidParticipationOpCount(ops, 1); got != 1 {
		t.Errorf("换门对移动者 op=1 出现 %d 次 ops=%v，want 1", got, ops)
	}
	if ops := raidParticipationOpsForTest(t, stayer.conn.stream(), 213); !raidParticipationOpsContain(ops, 2) {
		t.Errorf("换门之后留在原地的成员没有再收到 213 的参战行 ops=%v：战况表那一格又画不出来了", ops)
	} else if got := raidParticipationOpCount(ops, 1); got != 1 {
		t.Errorf("换门对留在原地的成员 op=1 出现 %d 次 ops=%v，want 1", got, ops)
	}
}
