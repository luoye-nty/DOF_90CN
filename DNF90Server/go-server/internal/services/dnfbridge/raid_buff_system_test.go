package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// runtimeRaidBuffRequestBody is the exact five-byte C2S 0x02B6 the client's own
// sender (0x010D7BB6) writes: u8 category, u16 target, u16 extra.
func runtimeRaidBuffRequestBody(category byte, target uint16) []byte {
	return []byte{category, byte(target), byte(target >> 8), 0, 0}
}

func TestDecodeRuntimeRaidBuffRequest(t *testing.T) {
	request, err := decodeRuntimeRaidBuffRequest([]byte{1, 0xE9, 0x03, 0x11, 0x22})
	if err != nil {
		t.Fatalf("live shape rejected: %v", err)
	}
	if request.Category != 1 || request.TargetKey != 1001 || request.ExtraKey != 0x2211 {
		t.Fatalf("decoded %+v", request)
	}
	// A longer target list is 86JP's shape (u8 + u16 + u16[]) and still has to pass
	// the shape checks even though this client only ever writes one entry.
	if _, err := decodeRuntimeRaidBuffRequest([]byte{2, 1, 0, 2, 0, 3, 0}); err != nil {
		t.Fatalf("multi-target shape rejected: %v", err)
	}
	for _, body := range [][]byte{
		{},
		{1, 0, 0, 0},
		{1, 0, 0, 0, 0, 0},
		{9, 1, 0, 0, 0},
		{1, 1},
	} {
		if _, err := decodeRuntimeRaidBuffRequest(body); err == nil {
			t.Fatalf("body=%x was accepted", body)
		}
	}
}

// The client's slot order and the category bytes are two different things, and the
// panel depends on both being right: slots 0/1/2 are the three squad skills and 3/4
// the two raid-wide ones, while the wire category of slot 0 is 2 (无敌) and of
// slot 3 is 1 (攻击力增加).
func TestRaidBuffSlotOrderMatchesTheClientPanel(t *testing.T) {
	want := []byte{2, 3, 4, 1, 0}
	if len(runtimeRaidClientBuffSlots) != len(want) {
		t.Fatalf("slot count=%d want=%d", len(runtimeRaidClientBuffSlots), len(want))
	}
	for index, category := range want {
		if runtimeRaidClientBuffSlots[index] != category {
			t.Fatalf("slot %d category=%d want=%d", index, runtimeRaidClientBuffSlots[index], category)
		}
	}
	// The split the client hard-codes at 0x015E27CF: the first three slots need a
	// designated squad, the last two are raid-wide.
	for slot := 0; slot < 3; slot++ {
		definition, ok := runtimeRaidBuffDefinitionFor(runtimeRaidClientBuffSlots[slot])
		if !ok || definition.RaidWide {
			t.Fatalf("slot %d must be a squad skill: %+v", slot, definition)
		}
	}
	for slot := 3; slot < 5; slot++ {
		definition, ok := runtimeRaidBuffDefinitionFor(runtimeRaidClientBuffSlots[slot])
		if !ok || !definition.RaidWide {
			t.Fatalf("slot %d must be raid-wide: %+v", slot, definition)
		}
	}
	// The PVF cooldowns, minute for minute as the client's own tooltips state them
	// (strings 30300-30304).
	for _, expected := range []struct {
		category  byte
		cooldown  uint32
		duration  uint32
		effect    uint32
		raidWide  bool
		cooldownM string
	}{
		{runtimeRaidBuffIncreaseTime, 7200, 10, 300, true, "120 minutes"},
		{runtimeRaidBuffAttackBonus, 420, 30, 15, true, "7 minutes"},
		{runtimeRaidBuffInvincible, 360, 30, 0, false, "6 minutes"},
		{runtimeRaidBuffRestore, 240, 60, 7, false, "4 minutes"},
		{runtimeRaidBuffIncreaseCoin, 300, 10, 4, false, "5 minutes"},
	} {
		definition, ok := runtimeRaidBuffDefinitionFor(expected.category)
		if !ok {
			t.Fatalf("category %d has no definition", expected.category)
		}
		if definition.CooldownSecs != expected.cooldown || definition.DurationSecs != expected.duration ||
			definition.EffectValue != expected.effect || definition.RaidWide != expected.raidWide {
			t.Fatalf("category %d definition=%+v want cooldown=%d duration=%d effect=%d raidWide=%t (%s)",
				expected.category, definition, expected.cooldown, expected.duration,
				expected.effect, expected.raidWide, expected.cooldownM)
		}
	}
}

// An untouched raid's 0x0279 is byte-identical to the idle reference: five slots,
// no rows. This is what makes wiring live rows in safe -- nothing changes until a
// skill is actually used.
func TestRaidBuffStatusBodyIsIdleUntilSomethingIsActivated(t *testing.T) {
	service, _, _, _ := newRaidSituationTestFixture(t)
	state := *service.raids[raidSituationFixtureRaidKey(t, service)]

	idle, err := runtimeRaidBuffIdleStatusBody()
	if err != nil {
		t.Fatalf("idle body: %v", err)
	}
	live, err := service.runtimeRaidBuffStatusBody(state)
	if err != nil {
		t.Fatalf("live body: %v", err)
	}
	if !bytes.Equal(live, idle) {
		t.Fatalf("idle raid body=%x want the idle reference %x", live, idle)
	}
	if want := []byte{5, 2, 0, 3, 0, 4, 0, 1, 0, 0, 0}; !bytes.Equal(idle, want) {
		t.Fatalf("idle body=%x want=%x", idle, want)
	}
}

// A leader using a raid-wide skill gets the status with the new row, and NOTHING
// else: this client registers an inbound 0x02B6 handler that reads four u32s with
// no bounds check, so the one-byte ack 86JP sent would be read out of bounds. The
// panel is driven by 0x0279, so the status is the whole answer.
func TestRaidBuffActivationPublishesTheRowAndSendsNoAck(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	state := *service.raids[raidSituationFixtureRaidKey(t, service)]
	connection.write.Reset()

	handled, err := service.handleOnlineRaidBuffSystem(
		session, 694, runtimeRaidBuffRequestBody(runtimeRaidBuffAttackBonus, 1001))
	if err != nil || !handled {
		t.Fatalf("activation handled=%t err=%v", handled, err)
	}

	activation, ok := service.runtimeRaidBuffActivationFor(state.RaidKey, runtimeRaidBuffAttackBonus)
	if !ok {
		t.Fatal("the activation was not recorded")
	}
	definition, _ := runtimeRaidBuffDefinitionFor(runtimeRaidBuffAttackBonus)

	stream := connection.write.Bytes()
	status, stream := splitGameServerUpperPacket(t, stream)
	if status.Header.Classification != 0 || status.Header.MsgID != raid.RaidBuffMsgID {
		t.Fatalf("first packet msg=%#04x want the buff status %#04x",
			status.Header.MsgID, raid.RaidBuffMsgID)
	}
	wantStatus := []byte{
		5,    // five slots
		2, 0, // slot category 2 (无敌), idle
		3, 0, // slot category 3 (恢复), idle
		4, 0, // slot category 4 (复活币 buff), idle
		1, 1, // slot category 1 (攻击力增加), one row
		1, 0, // party 1
		0xE9, 0x03, // member 1001
		0, 0, 0, 0, // active deadline, filled below
		0, 0, 0, 0, // cooldown deadline, filled below
		0, 0, // slot category 0 (攻略时间增加), idle
	}
	binary.LittleEndian.PutUint32(wantStatus[13:17], activation.ActiveUntil)
	binary.LittleEndian.PutUint32(wantStatus[17:21], activation.CooldownUntil)
	if !bytes.Equal(status.Body, wantStatus) {
		t.Fatalf("buff status body=%x want=%x", status.Body, wantStatus)
	}
	if len(stream) != 0 {
		t.Fatalf("activation sent %d trailing bytes; this opcode must produce no reply at all: %x",
			len(stream), stream)
	}

	now := uint32(time.Now().Unix())
	if activation.ActiveUntil <= now || activation.CooldownUntil <= activation.ActiveUntil {
		t.Fatalf("deadlines are not in the future: %+v (now=%d)", activation, now)
	}
	if gained := activation.CooldownUntil - now; gained > definition.CooldownSecs+2 {
		t.Fatalf("cooldown=%d exceeds the PVF value %d", gained, definition.CooldownSecs)
	}
}

// A second use inside the cooldown is refused and the panel is resynced, again with
// no ack. The client blocks its own send from the deadlines it was last given
// (string 30311 「技能冷却中」), so the server must not silently accept one.
func TestRaidBuffSecondUseInsideCooldownIsRefused(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	body := runtimeRaidBuffRequestBody(runtimeRaidBuffAttackBonus, 1001)

	if _, err := service.handleOnlineRaidBuffSystem(session, 694, body); err != nil {
		t.Fatalf("first activation: %v", err)
	}
	connection.write.Reset()
	if _, err := service.handleOnlineRaidBuffSystem(session, 694, body); err != nil {
		t.Fatalf("second activation: %v", err)
	}
	stream := connection.write.Bytes()
	status, stream := splitGameServerUpperPacket(t, stream)
	if status.Header.MsgID != raid.RaidBuffMsgID {
		t.Fatalf("refusal did not resync the panel: msg=%#04x", status.Header.MsgID)
	}
	if len(stream) != 0 {
		t.Fatalf("refusal sent %d trailing bytes; there is no ack on this opcode: %x", len(stream), stream)
	}
}

// Only the raid leader may use the panel (86JP CanUseAntonRaidBuff).
func TestRaidBuffNonLeaderIsRefused(t *testing.T) {
	service, _, leaderConnection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	member, memberConnection := addRaidSituationFixtureMember(t, service, raidKey, 1002, 2)
	enterRaidSituationFixtureGate(member, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	leaderConnection.write.Reset()
	memberConnection.write.Reset()

	handled, err := service.handleOnlineRaidBuffSystem(
		member, 694, runtimeRaidBuffRequestBody(runtimeRaidBuffAttackBonus, 1002))
	if err != nil || !handled {
		t.Fatalf("non-leader request handled=%t err=%v", handled, err)
	}
	resync, stream := splitGameServerUpperPacket(t, memberConnection.write.Bytes())
	if resync.Header.MsgID != raid.RaidBuffMsgID {
		t.Fatalf("non-leader did not get a panel resync: msg=%#04x", resync.Header.MsgID)
	}
	if len(stream) != 0 {
		t.Fatalf("non-leader refusal trailing bytes=%x", stream)
	}
	if len(leaderConnection.write.Bytes()) != 0 {
		t.Fatal("a refused request must not broadcast to the raid")
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffAttackBonus); ok {
		t.Fatal("a non-leader activation was recorded")
	}
}

// A squad skill must name a squad member who is inside a gate, which is exactly
// what the client's string 30313 「只能指定已进入地下城的小队。」 tells the player,
// and what 86JP enforces through `group.DungeonId != 0`.
func TestRaidBuffSquadSkillRequiresAMemberInsideAGate(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	connection.write.Reset()
	body := runtimeRaidBuffRequestBody(runtimeRaidBuffInvincible, 1001)
	if _, err := service.handleOnlineRaidBuffSystem(session, 694, body); err != nil {
		t.Fatalf("squad skill outside a gate: %v", err)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffInvincible); ok {
		t.Fatal("a squad skill was accepted for a member outside every gate")
	}

	// Once inside a gate it is accepted, and the row names the member's own squad.
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	connection.write.Reset()
	if _, err := service.handleOnlineRaidBuffSystem(session, 694, body); err != nil {
		t.Fatalf("squad skill inside a gate: %v", err)
	}
	activation, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffInvincible)
	if !ok {
		t.Fatal("a squad skill inside a gate was not recorded")
	}
	if activation.PartyIndex != 1 || activation.MemberKey != 1001 {
		t.Fatalf("activation target=%+v want party 1 member 1001", activation)
	}
	// The row's target is the MEMBER the panel selected, so a squad skill aimed at a
	// raid-mate must name that mate, not the leader who pressed the button.
	mate, _ := addRaidSituationFixtureMember(t, service, raidKey, 1002, 2)
	enterRaidSituationFixtureGate(mate, raidKey, runtimeRaidDungeonPhaseTwo, 221)
	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffRestore, 1002)); err != nil {
		t.Fatalf("squad skill on a mate: %v", err)
	}
	restore, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffRestore)
	if !ok {
		t.Fatal("a squad skill aimed at a raid-mate was not recorded")
	}
	if restore.PartyIndex != 2 || restore.MemberKey != 1002 {
		t.Fatalf("activation target=%+v want party 2 member 1002", restore)
	}
}

// Cooldowns cannot outlive the raid that owns them.
func TestRaidBuffActivationsArePurgedWithTheRaid(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffAttackBonus, 1001)); err != nil {
		t.Fatalf("activation: %v", err)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffAttackBonus); !ok {
		t.Fatal("nothing was recorded to purge")
	}
	service.raidMu.Lock()
	service.purgeRuntimeRaidBuffActivationsLocked(raidKey)
	service.raidMu.Unlock()
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffAttackBonus); ok {
		t.Fatal("the purge left an activation behind")
	}
}

// A body shape this client never sends is swallowed with a panel resync, never an
// error: the opcode is on the client's input path.
func TestRaidBuffMalformedBodyIsRefusedWithoutFailing(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	connection.write.Reset()
	handled, err := service.handleOnlineRaidBuffSystem(session, 694, []byte{1, 0, 0, 0})
	if err != nil || !handled {
		t.Fatalf("malformed body handled=%t err=%v", handled, err)
	}
	resync, stream := splitGameServerUpperPacket(t, connection.write.Bytes())
	if resync.Header.MsgID != raid.RaidBuffMsgID {
		t.Fatalf("malformed body did not resync the panel: msg=%#04x", resync.Header.MsgID)
	}
	if len(stream) != 0 {
		t.Fatalf("malformed body trailing bytes=%x", stream)
	}
}

// enterRaidBuffPhaseClock installs a fake gameplay clock and puts the raid inside a
// running phase with `remaining` left of its 2400-second budget, so the INCREASE
// TIME cap can be exercised at its edges.
func enterRaidBuffPhaseClock(
	t *testing.T,
	service *Service,
	raidKey uint32,
	remaining time.Duration,
) *runtimeRaidPhaseTestQueue {
	t.Helper()
	queue := &runtimeRaidPhaseTestQueue{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	service.gameplayTimers = queue
	service.raidMu.Lock()
	state := service.raids[raidKey]
	state.Lifecycle.PhaseStartedAt = queue.now.Add(-(2400*time.Second - remaining))
	state.Lifecycle.PhaseDeadline = queue.now.Add(remaining)
	service.raidMu.Unlock()
	return queue
}

// INCREASE TIME is one of the only two skills with a server-side effect. It adds
// the PVF's 300 seconds to the phase deadline and re-arms the phase expiry under the
// same timer name -- leaving the old job armed would fail the phase at the old
// instant even though the deadline moved.
func TestRaidBuffIncreaseTimeExtendsThePhaseAndReArmsTheTimer(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	queue := enterRaidBuffPhaseClock(t, service, raidKey, 30*time.Minute)
	before := service.raids[raidKey].Lifecycle.PhaseDeadline
	connection.write.Reset()

	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffIncreaseTime, 1001)); err != nil {
		t.Fatalf("increase time: %v", err)
	}

	after := service.raids[raidKey].Lifecycle.PhaseDeadline
	if got := after.Sub(before); got != 300*time.Second {
		t.Fatalf("deadline moved by %s want 300s (before=%s after=%s)", got, before, after)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffIncreaseTime); !ok {
		t.Fatal("increase time was not recorded")
	}
	if queue.scheduleCalls != 1 {
		t.Fatalf("phase expiry re-arms=%d want 1", queue.scheduleCalls)
	}
	if want := after.Sub(queue.now); queue.scheduledDelay != want {
		t.Fatalf("re-armed delay=%s want=%s", queue.scheduledDelay, want)
	}

	stream := connection.write.Bytes()
	status, stream := splitGameServerUpperPacket(t, stream)
	if status.Header.MsgID != raid.RaidBuffMsgID {
		t.Fatalf("first packet msg=%#04x want the buff status", status.Header.MsgID)
	}
	timer, stream := splitGameServerUpperPacket(t, stream)
	if timer.Header.MsgID != raid.RaidTimerMsgID {
		t.Fatalf("second packet msg=%#04x want the timer %#04x", timer.Header.MsgID, raid.RaidTimerMsgID)
	}
	remain, stream := splitGameServerUpperPacket(t, stream)
	if remain.Header.MsgID != raid.RaidRemainTimeMsgID {
		t.Fatalf("third packet msg=%#04x want the remain-time %#04x",
			remain.Header.MsgID, raid.RaidRemainTimeMsgID)
	}
	if len(stream) != 0 {
		t.Fatalf("increase time trailing bytes=%x", stream)
	}
	if want := uint32(30*time.Minute/time.Second) + 300; !bytes.Contains(remain.Body, []byte{
		byte(want), byte(want >> 8), byte(want >> 16), byte(want >> 24),
	}) {
		t.Fatalf("remain-time body=%x want the extended remaining %d", remain.Body, want)
	}
}

// The extension is capped at one full phase, and a phase whose timer is already
// full must not burn the skill's two-hour cooldown.
func TestRaidBuffIncreaseTimeRefusesAFullTimer(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidBuffPhaseClock(t, service, raidKey, 40*time.Minute)
	before := service.raids[raidKey].Lifecycle.PhaseDeadline

	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffIncreaseTime, 1001)); err != nil {
		t.Fatalf("increase time: %v", err)
	}
	if after := service.raids[raidKey].Lifecycle.PhaseDeadline; !after.Equal(before) {
		t.Fatalf("a full timer was extended: before=%s after=%s", before, after)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffIncreaseTime); ok {
		t.Fatal("a refused extension still consumed the cooldown")
	}
}

// An expired phase must not be revived: its timeout has already been scheduled and
// the raid is on its way to the failure result.
func TestRaidBuffIncreaseTimeRefusesAnExpiredPhase(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidBuffPhaseClock(t, service, raidKey, -time.Minute)

	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffIncreaseTime, 1001)); err != nil {
		t.Fatalf("increase time: %v", err)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffIncreaseTime); ok {
		t.Fatal("an expired phase was revived and the cooldown consumed")
	}
}

// INCREASE COIN cannot hand out coins: the client's denominator is its own per-gate
// allowance and never travels on the wire, so the server's only lever is the
// numerator. It raises the row's grant and reports `used - granted`, which makes the
// client's own `allowance - reported` come out higher. This is exactly what 86JP
// does.
func TestRaidBuffIncreaseCoinRaisesTheRowGrantAndReportsTheDifference(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	setRaidSituationFixtureSpentCoins(t, service, session, 6)
	connection.write.Reset()

	// Before the grant the row reports the whole spend.
	rows, ok := service.runtimeRaidMonsterStateRows(*service.raids[raidKey])
	if !ok || len(rows) != 1 || rows[0].Value0 != 6 {
		t.Fatalf("pre-grant rows=%+v ok=%t want one row reporting 6", rows, ok)
	}

	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffIncreaseCoin, 1001)); err != nil {
		t.Fatalf("increase coin: %v", err)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffIncreaseCoin); !ok {
		t.Fatal("increase coin was not recorded")
	}

	granted := service.runtimeRaidReviveCoinGranted(runtimeRaidReviveCoinKey{
		RaidKey: raidKey, Phase: runtimeRaidDungeonPhaseTwo, Group: 1, DungeonID: 213,
	})
	if granted != 4 {
		t.Fatalf("granted=%d want the PVF effect value 4", granted)
	}
	rows, ok = service.runtimeRaidMonsterStateRows(*service.raids[raidKey])
	if !ok || len(rows) != 1 || rows[0].Value0 != 2 {
		t.Fatalf("post-grant rows=%+v ok=%t want one row reporting 6-4=2", rows, ok)
	}

	// The grant changes what the board draws, so the row packet is re-published
	// after the panel status.
	stream := connection.write.Bytes()
	status, stream := splitGameServerUpperPacket(t, stream)
	if status.Header.MsgID != raid.RaidBuffMsgID {
		t.Fatalf("first packet msg=%#04x want the buff status", status.Header.MsgID)
	}
	board, stream := splitGameServerUpperPacket(t, stream)
	if board.Header.MsgID != raid.RaidMonsterStateMsgID {
		t.Fatalf("second packet msg=%#04x want the board rows %#04x",
			board.Header.MsgID, raid.RaidMonsterStateMsgID)
	}
	if len(stream) != 0 {
		t.Fatalf("increase coin trailing bytes=%x", stream)
	}
	wantBoard := []byte{
		1,    // groupCount
		1, 0, // squad 1
		1,          // one member
		0xE9, 0x03, // 1001
		2, 0, 0, 0, // V0 = 6 spent - 4 granted
		0, 0, 0, 0, // V1 default
		0,
	}
	if !bytes.Equal(board.Body, wantBoard) {
		t.Fatalf("board body=%x want=%x", board.Body, wantBoard)
	}
}

// A squad that has not spent beyond its grant gets nothing, and -- like 86JP -- the
// skill does not burn its cooldown for that.
func TestRaidBuffIncreaseCoinRefusesASquadThatHasNotOverspent(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	// No coins spent at all.
	if _, err := service.handleOnlineRaidBuffSystem(session, 694,
		runtimeRaidBuffRequestBody(runtimeRaidBuffIncreaseCoin, 1001)); err != nil {
		t.Fatalf("increase coin: %v", err)
	}
	if _, ok := service.runtimeRaidBuffActivationFor(raidKey, runtimeRaidBuffIncreaseCoin); ok {
		t.Fatal("a grant with nothing to cover still consumed the cooldown")
	}
}

// The grant ledger obeys the same lifetime as the raid, for both counters: the
// spend ledger and the grant ledger are the two halves of the row's coin number and
// a purge must not leave one behind.
func TestRaidReviveCoinGrantsArePurgedWithTheRaid(t *testing.T) {
	service, _, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	key := runtimeRaidReviveCoinKey{
		RaidKey: raidKey, Phase: runtimeRaidDungeonPhaseTwo, Group: 1, DungeonID: 213,
	}
	if granted := service.grantRuntimeRaidReviveCoins(key, 4); granted != 4 {
		t.Fatalf("granted=%d want 4", granted)
	}
	service.raidMu.Lock()
	if service.raidReviveUses == nil {
		service.raidReviveUses = make(map[runtimeRaidReviveCoinKey]uint32)
	}
	service.raidReviveUses[key] = 6
	service.raidMu.Unlock()
	service.raidMu.Lock()
	service.purgeRuntimeRaidReviveCoinLedgerLocked(raidKey)
	service.raidMu.Unlock()
	if granted := service.runtimeRaidReviveCoinGranted(key); granted != 0 {
		t.Fatalf("the purge left %d granted coins", granted)
	}
	if uses := service.runtimeRaidReviveCoinUses(key); uses != 0 {
		t.Fatalf("the purge left %d spent coins", uses)
	}
}
