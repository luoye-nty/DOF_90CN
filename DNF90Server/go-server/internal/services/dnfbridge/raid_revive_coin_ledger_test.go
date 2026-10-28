package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The three tests in this file pin the lifetime of the revive-coin ledger that the
// 634 battle-situation rows report, because getting that lifetime wrong is not
// cosmetic: the client renders `its own per-gate allowance - the value we send`, so
// a ledger that resets early shows the operator coins they no longer have.
//
// Live 2026-09-23, raid with two squads: six paid revives were spent and the panel
// stayed at 10/12. Two independent defects produced that, and each one has a test
// here:
//
//   - the spend lived on runtimeDungeonState, which is allocated per RUN, so
//     re-entering the same gate (210 -> town -> 210) silently zeroed it while the
//     client's denominator stayed the gate's own. See
//     TestRuntimeRaidReviveCoinSpendAccumulatesAcrossGateReEntries.
//   - no 634 was emitted when a coin was spent at all, so the panel only moved when
//     something else happened to publish rows. See
//     TestRuntimeRaidReviveCoinSpendPublishesTheBoard.
//   - a gate whose clear quota is met has to hand its allowance back, which is
//     86JP's own participation lifetime. See
//     TestRuntimeRaidReviveCoinLedgerResetsWhenTheGateIsCleared.

// runtimeRaidReviveCoinTestKey is the ledger entry for one member's current gate.
func runtimeRaidReviveCoinTestKey(
	t *testing.T,
	service *Service,
	session *gameSession,
) runtimeRaidReviveCoinKey {
	t.Helper()
	session.dungeon.mu.Lock()
	binding := runtimeRaidDungeonBinding{}
	if session.dungeon.runtime != nil {
		binding = session.dungeon.runtime.raidDungeon
	}
	session.dungeon.mu.Unlock()
	if binding.RaidKey == 0 || binding.MapID == 0 {
		t.Fatalf("session %d is not inside a raid gate: %+v", session.selectedCharacterID, binding)
	}
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	state := service.raids[binding.RaidKey]
	if state == nil {
		t.Fatalf("raid %d is not live", binding.RaidKey)
	}
	return raidReviveCoinLedgerKey(*state, binding, session.selectedCharacterID)
}

// attachRuntimeRaidReviveCoinRaid gives an ordinary-dungeon fixture a live raid whose
// only frozen member is the fixture's own character, and binds that character's run
// to one gate of it.
//
// The raid is built from the real Anton config and the real lifecycle constructor so
// the clear path the tests below drive is the production one, not a stub.
func attachRuntimeRaidReviveCoinRaid(
	t *testing.T,
	service *Service,
	session *gameSession,
	raidKey uint32,
	phase byte,
	mapID int,
) {
	t.Helper()
	preparedAt := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	config := runtimeRaidBridgeTestAntonConfig()
	lifecycle, err := newRuntimeRaidLifecycleStateFromAntonModeAndType(config, preparedAt, false, 0)
	if err != nil {
		t.Fatalf("create lifecycle: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = phase
	lifecycle.PhaseStartedAt = preparedAt
	lifecycle.PhaseDeadline = preparedAt.Add(40 * time.Minute)
	member := runtimeRaidMemberState{
		CharID:     session.selectedCharacterID,
		Name:       "leader",
		GroupIndex: 1,
	}
	service.raidMu.Lock()
	if service.raids == nil {
		service.raids = make(map[uint32]*runtimeRaidState)
	}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       member.CharID,
		AntonConfig:    config,
		Members:        []runtimeRaidMemberState{member},
		FrozenMembers:  []runtimeRaidMemberState{member},
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{member.CharID},
		Lifecycle:      lifecycle,
	}
	service.raidMu.Unlock()

	session.dungeon.mu.Lock()
	if session.dungeon.runtime == nil {
		session.dungeon.mu.Unlock()
		t.Fatal("the fixture session has no dungeon run to bind")
	}
	session.dungeon.runtime.raidDungeon = runtimeRaidDungeonBinding{
		RaidKey:        raidKey,
		Phase:          phase,
		MapID:          mapID,
		LifecycleToken: 1,
	}
	session.dungeon.mu.Unlock()
}

// armRuntimeRaidReviveDeathState runs the real op32 death on an ordinary dungeon run
// and returns the op41 body that revive answers to, so a test can replay it.
func armRuntimeRaidReviveDeathState(
	t *testing.T,
	service *Service,
	session *gameSession,
) []byte {
	t.Helper()
	if err := service.handleDungeonDieCharacter(
		session, make([]byte, currentDungeonDieCharacterBodySize)); err != nil {
		t.Fatalf("op32 death state: %v", err)
	}
	body := make([]byte, currentDungeonUseCoinBodySize)
	binary.LittleEndian.PutUint16(body, currentSceneActorObjectKey(session.selectedCharacterID))
	return body
}

// spendRuntimeRaidReviveCoinThroughThePacketPath runs one real op32 + op41 revive and
// returns the op41 body and the handler's error.
func spendRuntimeRaidReviveCoinThroughThePacketPath(
	t *testing.T,
	service *Service,
	session *gameSession,
) ([]byte, error) {
	t.Helper()
	body := armRuntimeRaidReviveDeathState(t, service, session)
	return body, service.handleDungeonUseCoin(session, body)
}

// fireRaidMonsterStateTasks runs every board refresh the code under test queued and
// reports how many there were.
//
// The refresh is scheduled, never sent inline (a revive holds the spending member's
// session lock, and building the rows takes each member's lock in turn), so a test
// has to fire the queue the way the gameplay timer loop would.
func fireRaidMonsterStateTasks(
	t *testing.T,
	queue *fakeCurrentDungeonDeathTimerQueue,
) int {
	t.Helper()
	tasks := queue.tasksNamed("raid-monster-state:")
	for _, task := range tasks {
		queue.fire(task, false)
	}
	return len(tasks)
}

// countRaidMonsterStatePackets counts the 634 packets inside one connection stream.
func countRaidMonsterStatePackets(t *testing.T, stream []byte) (int, []byte) {
	t.Helper()
	count := 0
	last := []byte(nil)
	for len(stream) != 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.MsgID == raid.RaidMonsterStateMsgID {
			count++
			last = packet.Body
		}
		stream = rest
	}
	return count, last
}

// A gate's allowance belongs to the GATE, so a squad that spends coins, leaves and
// re-enters the same gate still reports everything it has spent. This is the defect
// the operator reported live: two coins spent in 黑雾之源 showed as 10/12, the squad
// re-entered, and the board went back to 12/12 because the counter lived on the run.
func TestRuntimeRaidReviveCoinSpendAccumulatesAcrossGateReEntries(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	// The row builder resolves a squad's gate through the online session registry.
	service.bindGameSessionCharacter(session, session.selectedCharacterID)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("revive test repositories unavailable")
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: strconv.Itoa(int(session.selectedCharacterID)),
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: int64(csharpReviveCoinItemID), Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const raidKey = uint32(0x00c80923)
	attachRuntimeRaidReviveCoinRaid(t, service, session, raidKey, runtimeRaidDungeonPhaseTwo, 210)

	reviveBody, err := spendRuntimeRaidReviveCoinThroughThePacketPath(t, service, session)
	if err != nil {
		t.Fatalf("first revive: %v", err)
	}
	key := runtimeRaidReviveCoinTestKey(t, service, session)
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 1 {
		t.Fatalf("after one paid revive the gate ledger=%d want 1 (key %+v)", uses, key)
	}

	// The client retries op41 for a revive the server already committed. That retry
	// must not bill a second coin: the commit branch is the only place that counts,
	// and the retry finds the pending death state already resolved.
	if err := service.handleDungeonUseCoin(session, reviveBody); err != nil {
		t.Fatalf("replayed revive: %v", err)
	}
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 1 {
		t.Fatalf("a replayed revive billed the gate again: ledger=%d want 1", uses)
	}

	// Re-entering the gate allocates a NEW run for the same (raid, phase, gate). This
	// is the exact shape that used to reset the count.
	reentered := &runtimeDungeonState{
		raidDungeon: runtime.raidDungeon,
	}
	if _, _, counted := service.recordRuntimeRaidReviveCoinUseLocked(session, reentered); !counted {
		t.Fatal("the re-entered run's coin was not billed to the raid ledger")
	}
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 2 {
		t.Fatalf("the gate ledger reset on re-entry: %d want 2 (key %+v)", uses, key)
	}

	// And the board must report the gate's total, not the current run's.
	rows, ok := service.runtimeRaidMonsterStateRows(*service.raids[raidKey])
	if !ok || len(rows) != 1 {
		t.Fatalf("rows=%+v ok=%t want one row for the squad in gate 210", rows, ok)
	}
	if rows[0].Value0 != 2 {
		t.Fatalf("board V0=%d want 2 coins spent in this gate", rows[0].Value0)
	}
}

// A new phase starts from a full allowance: 86JP builds fresh participation records
// per phase, so the previous phase's spend must not follow the squad into the next
// one -- and must not be erased for the phase it belongs to either.
func TestRuntimeRaidReviveCoinSpendIsPerPhase(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 210)
	setRaidSituationFixtureSpentCoins(t, service, session, 2)
	phaseTwoKey := runtimeRaidReviveCoinTestKey(t, service, session)

	service.raidMu.Lock()
	state := service.raids[raidKey]
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	service.raidMu.Unlock()
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseOne, 210)
	phaseOneKey := runtimeRaidReviveCoinTestKey(t, service, session)
	if phaseOneKey == phaseTwoKey {
		t.Fatalf("both phases produced the same ledger key %+v", phaseOneKey)
	}

	rows, ok := service.runtimeRaidMonsterStateRows(*service.raids[raidKey])
	if !ok || len(rows) != 1 {
		t.Fatalf("rows=%+v ok=%t want one row", rows, ok)
	}
	if rows[0].Value0 != 0 {
		t.Fatalf("the new phase inherited %d coins of spend", rows[0].Value0)
	}
	if uses := service.runtimeRaidReviveCoinUses(phaseTwoKey); uses != 2 {
		t.Fatalf("the finished phase's ledger was erased: %d want 2", uses)
	}
}

// A spent coin has to reach the panel by itself. Nothing else refreshes the rows in
// the middle of a gate, which is why six live revives left the board at 10/12.
//
// It also pins the two rules the refresh has to obey: it is scheduled rather than
// sent from the revive's own goroutine (which still holds that member's session
// lock), and it reaches every member.
func TestRuntimeRaidReviveCoinSpendPublishesTheBoard(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	// The board is fanned out over the raid's ONLINE members, which is the online
	// session registry, so the fixture character has to be registered in it.
	service.bindGameSessionCharacter(session, session.selectedCharacterID)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("revive test repositories unavailable")
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: strconv.Itoa(int(session.selectedCharacterID)),
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: int64(csharpReviveCoinItemID), Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const raidKey = uint32(0x00c80924)
	attachRuntimeRaidReviveCoinRaid(t, service, session, raidKey, runtimeRaidDungeonPhaseTwo, 210)

	if _, err := spendRuntimeRaidReviveCoinThroughThePacketPath(t, service, session); err != nil {
		t.Fatalf("revive: %v", err)
	}
	conn := session.conn.(*bufferConn)
	if count, _ := countRaidMonsterStatePackets(t, conn.write.Bytes()); count != 0 {
		t.Fatalf("the revive sent the board inline %d times; it must be scheduled", count)
	}
	if scheduled := fireRaidMonsterStateTasks(t, queue); scheduled != 1 {
		t.Fatalf("the revive queued %d board refreshes, want exactly 1", scheduled)
	}
	count, body := countRaidMonsterStatePackets(t, conn.write.Bytes())
	if count != 1 {
		t.Fatalf("the queued refresh produced %d board packets, want 1", count)
	}
	memberKey := make([]byte, 2)
	binary.LittleEndian.PutUint16(memberKey, currentSceneActorObjectKey(session.selectedCharacterID))
	want := []byte{
		1,    // groupCount
		1, 0, // squad 1
		1, // one member
	}
	want = append(want, memberKey...)
	// V0 = the one coin this gate has spent, the number the client subtracts from its
	// own allowance. V1 stays 0 because this squad has reported no monster HP yet, and
	// the value count is 0 for the same reason.
	want = append(want, 1, 0, 0, 0, 0, 0, 0, 0, 0)
	if !bytes.Equal(body, want) {
		t.Fatalf("board body=%x want=%x", body, want)
	}
}

// The board refresh must not run on the reviving member's own goroutine while that
// member's packet path is still finishing, and it must not run under any session lock
// at all: building the rows reads every online member's session lock in turn, so a
// refresh that ran inline would make two members reviving at the same instant wait
// for each other -- the live 2026-09-20 08:32:02Z shape, which
// TestRuntimeRaidVolcanoClearNeverWaitsOnAMateSessionLock pins for the phase tail.
//
// This is the revive's version of that test, and it fails the same way the old
// inline shape would: the mate holds its own session lock while the revive runs, so
// an inline refresh would park on it.
func TestRuntimeRaidReviveCoinBoardRefreshNeverWaitsOnAMateSessionLock(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	service.bindGameSessionCharacter(session, session.selectedCharacterID)
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("revive test repositories unavailable")
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: strconv.Itoa(int(session.selectedCharacterID)),
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: int64(csharpReviveCoinItemID), Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const raidKey = uint32(0x00c80925)
	attachRuntimeRaidReviveCoinRaid(t, service, session, raidKey, runtimeRaidDungeonPhaseTwo, 210)
	mate, mateConn := attachRuntimeRaidReviveCoinMate(t, service, session, raidKey, 210, 100)

	mateHeld := holdMateSessionLock(t, mate)
	defer mateHeld.releaseLock()
	mateHeld.enterLock()
	select {
	case <-mateHeld.held:
	case <-time.After(3 * time.Second):
		t.Fatal("the mate never took its own session lock")
	}

	done := make(chan error, 1)
	go func() {
		_, err := spendRuntimeRaidReviveCoinThroughThePacketPath(t, service, session)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("revive with a mate holding its own session lock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("队友握着自己的 session 锁时复活包卡住了：板子刷新跑在了复活者自己的调用栈里" +
			"（构造行要逐个读在线成员的 session 锁，两人同时复活就是永久互等）。")
	}

	// The refresh still has to happen -- just not there.
	mateHeld.releaseLock()
	if scheduled := fireRaidMonsterStateTasks(t, queue); scheduled != 1 {
		t.Fatalf("the revive queued %d board refreshes, want exactly 1", scheduled)
	}
	count, body := countRaidMonsterStatePackets(t, mateConn.write.Bytes())
	if count != 1 {
		t.Fatalf("the mate received %d board packets, want 1", count)
	}
	if len(body) == 0 || body[0] != 2 {
		t.Fatalf("the mate's board row body=%x want two squads", body)
	}
}

// attachRuntimeRaidReviveCoinMate adds a second online raid member standing in its own
// gate, which is what makes the row builder take a lock other than the reviver's.
func attachRuntimeRaidReviveCoinMate(
	t *testing.T,
	service *Service,
	leader *gameSession,
	raidKey uint32,
	mapID int,
	charID uint16,
) (*gameSession, *bufferConn) {
	t.Helper()
	mate := &gameSession{
		conn:                &bufferConn{},
		connID:              "raid-revive-coin-mate",
		channel:             leader.channel,
		residentChannel:     leader.residentChannel,
		selectedCharacterID: charID,
	}
	service.bindGameSessionCharacter(mate, charID)
	if _, online := service.onlineGameSession(charID); !online {
		t.Fatalf("mate %d is not registered as online", charID)
	}
	service.raidMu.Lock()
	state := service.raids[raidKey]
	if state == nil {
		service.raidMu.Unlock()
		t.Fatalf("raid %d is not live", raidKey)
	}
	member := runtimeRaidMemberState{CharID: charID, Name: "mate", GroupIndex: 2}
	state.Members = append(state.Members, member)
	state.FrozenMembers = append(state.FrozenMembers, member)
	phase := state.Lifecycle.Phase
	service.raidMu.Unlock()

	mate.dungeon.mu.Lock()
	mate.dungeon.runtime = &runtimeDungeonState{
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey:        raidKey,
			Phase:          phase,
			MapID:          mapID,
			LifecycleToken: 1,
		},
	}
	mate.dungeon.mu.Unlock()
	return mate, mate.conn.(*bufferConn)
}

// A gate whose own clear quota is met is finished, and 86JP's participation record --
// which owns both coin counters -- ends with it, so a reopened gate hands back a full
// allowance. A gate that still has clears left must keep its spend.
func TestRuntimeRaidReviveCoinLedgerResetsWhenTheGateIsCleared(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	setRaidSituationFixtureSpentCoins(t, service, session, 3)
	key := runtimeRaidReviveCoinTestKey(t, service, session)
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 3 {
		t.Fatalf("fixture ledger=%d want 3", uses)
	}

	// One clear left on the quota: this clear finishes the gate.
	setRuntimeRaidReviveCoinTestObjective(t, service, raidKey, runtimeRaidDungeonPhaseTwo, 213, 1)
	advanceRuntimeRaidReviveCoinTestObjective(t, service, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	if err := service.afterRuntimeRaidObjectiveAdvance(raidKey, runtimeRaidDungeonPhaseTwo, 213); err != nil {
		t.Fatalf("after clear: %v", err)
	}
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 0 {
		t.Fatalf("a cleared gate kept %d coins of spend; the allowance must start over", uses)
	}

	// Two clears left: the same call must not touch the ledger.
	setRaidSituationFixtureSpentCoins(t, service, session, 3)
	setRuntimeRaidReviveCoinTestObjective(t, service, raidKey, runtimeRaidDungeonPhaseTwo, 213, 2)
	advanceRuntimeRaidReviveCoinTestObjective(t, service, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	if err := service.afterRuntimeRaidObjectiveAdvance(raidKey, runtimeRaidDungeonPhaseTwo, 213); err != nil {
		t.Fatalf("after partial clear: %v", err)
	}
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 3 {
		t.Fatalf("a gate with clears left lost its spend: %d want 3", uses)
	}

	// And the reset is visible on the board, not just in the map: after the final
	// clear the row reports no spend again.
	advanceRuntimeRaidReviveCoinTestObjective(t, service, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	if err := service.afterRuntimeRaidObjectiveAdvance(raidKey, runtimeRaidDungeonPhaseTwo, 213); err != nil {
		t.Fatalf("after last clear: %v", err)
	}
	rows, ok := service.runtimeRaidMonsterStateRows(*service.raids[raidKey])
	if !ok || len(rows) != 1 {
		t.Fatalf("rows=%+v ok=%t want one row", rows, ok)
	}
	if rows[0].Value0 != 0 {
		t.Fatalf("the finished gate still reports %d spent coins", rows[0].Value0)
	}
}

// setRuntimeRaidReviveCoinTestObjective pins one gate's quota so the clear path is
// exercised with a known number of clears left.
func setRuntimeRaidReviveCoinTestObjective(
	t *testing.T,
	service *Service,
	raidKey uint32,
	phase byte,
	mapID int,
	remainingHP int,
) {
	t.Helper()
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	state := service.raids[raidKey]
	if state == nil {
		t.Fatalf("raid %d is not live", raidKey)
	}
	if state.Lifecycle.Objectives == nil {
		state.Lifecycle.Objectives = make(map[int]runtimeRaidDungeonObjective)
	}
	objective := state.Lifecycle.Objectives[mapID]
	objective.Phase = phase
	objective.State = raid.AntonDungeonOpen
	objective.MaxHP = remainingHP
	objective.RemainingHP = remainingHP
	state.Lifecycle.Objectives[mapID] = objective
}

// advanceRuntimeRaidReviveCoinTestObjective books one clear, which is what the
// dungeon lifecycle does before it calls afterRuntimeRaidObjectiveAdvance.
func advanceRuntimeRaidReviveCoinTestObjective(
	t *testing.T,
	service *Service,
	raidKey uint32,
	phase byte,
	mapID int,
) {
	t.Helper()
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	state := service.raids[raidKey]
	if state == nil {
		t.Fatalf("raid %d is not live", raidKey)
	}
	if err := advanceRuntimeRaidObjectiveLocked(state, phase, mapID); err != nil {
		t.Fatalf("advance objective %d: %v", mapID, err)
	}
}
