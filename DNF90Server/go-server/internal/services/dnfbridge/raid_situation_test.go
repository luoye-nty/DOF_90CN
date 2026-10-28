package dnfbridge

import (
	"bytes"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
)

// The current EXE prefixes C2S 695 with a reserved dword and puts the value
// count at +4. The bodies below are the exact ones the live client sent on
// 2026-09-17 (packet_log.txt), so a reader that starts the count at body[0] --
// the 86JP offset -- would misread every one of them as "window opened".
func TestDecodeRaidSituationRequestAcceptsTheCurrentExeShapes(t *testing.T) {
	cases := []struct {
		body     []byte
		reserved uint32
		values   []uint32
	}{
		{body: []byte{0, 0, 0, 0, 0}, reserved: 0, values: nil},
		{body: []byte{0, 0, 0, 0, 1, 100, 0, 0, 0}, reserved: 0, values: []uint32{100}},
		{
			body:     []byte{0, 0, 0, 0, 4, 100, 0, 0, 0, 100, 0, 0, 0, 100, 0, 0, 0, 100, 0, 0, 0},
			reserved: 0,
			values:   []uint32{100, 100, 100, 100},
		},
	}
	for index, testCase := range cases {
		reserved, values, err := decodeRaidSituationRequest(testCase.body)
		if err != nil {
			t.Fatalf("case %d body=%x rejected: %v", index, testCase.body, err)
		}
		if reserved != testCase.reserved {
			t.Fatalf("case %d reserved=%d want=%d", index, reserved, testCase.reserved)
		}
		if len(values) != len(testCase.values) {
			t.Fatalf("case %d values=%v want=%v", index, values, testCase.values)
		}
		for valueIndex, want := range testCase.values {
			if values[valueIndex] != want {
				t.Fatalf("case %d value %d=%d want=%d", index, valueIndex, values[valueIndex], want)
			}
		}
	}

	for _, body := range [][]byte{
		{},
		{0, 0, 0, 0},
		{0, 0, 0, 0, 6, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0},
		{0, 0, 0, 0, 1, 100, 0},
		{0, 0, 0, 0, 2, 100, 0, 0, 0},
	} {
		if _, _, err := decodeRaidSituationRequest(body); err == nil {
			t.Fatalf("body=%x was accepted", body)
		}
	}
}

// count==0 is "the 战况表 is open and asking for the situation". The answer is
// the persistent selector projection, never the one-shot op587 state
// announcement: the client repeats this request every 7-15s, so a replay of the
// phase-start state would re-announce a phase that is already running.
func TestOnlineRaidSituationWindowRefreshAnswersWithTheSelectorProjection(t *testing.T) {
	service, session, connection, lifecycle := newRaidSituationTestFixture(t)
	connection.write.Reset()

	handled, err := service.handleOnlineRaidCommand(session, 695, []byte{0, 0, 0, 0, 0})
	if err != nil {
		t.Fatalf("situation refresh returned an error: %v", err)
	}
	if !handled {
		t.Fatal("the situation refresh was not claimed by the raid handler")
	}
	wantIDs := []uint16{
		raid.RaidObjectMsgID,
		raid.RaidMemberRefreshMsgID,
		raid.RaidSymbolMsgID,
		raid.RaidDungeonStateMsgID,
		raid.RaidTimerMsgID,
		raid.RaidRemainTimeMsgID,
	}
	stream := connection.write.Bytes()
	if len(stream) == 0 {
		t.Fatal("the situation refresh answered with no packet at all")
	}
	for index, wantID := range wantIDs {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != wantID {
			t.Fatalf("situation packet %d class=%d msg=%#04x want=%#04x",
				index, packet.Header.Classification, packet.Header.MsgID, wantID)
		}
		if packet.Header.MsgID == raid.RaidStateMsgID {
			t.Fatalf("situation refresh replayed the one-shot raid state: %x", packet.Body)
		}
	}
	if len(stream) != 0 {
		t.Fatalf("situation refresh trailing bytes=%d", len(stream))
	}
	_ = lifecycle
}

// A value report is the client's own HUD telemetry. It must not fan out to the
// squad and it must never fail the read loop that dispatched it.
func TestOnlineRaidSituationValueReportWritesNothing(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	connection.write.Reset()

	handled, err := service.handleOnlineRaidSituationMonsterHp(session, 695, []byte{0, 0, 0, 0, 1, 100, 0, 0, 0})
	if err != nil {
		t.Fatalf("value report returned an error: %v", err)
	}
	if !handled {
		t.Fatal("the value report was not claimed by the raid handler")
	}
	if wrote := connection.write.Len(); wrote != 0 {
		t.Fatalf("value report wrote %d bytes", wrote)
	}
}

// A body shape this client has never produced is logged and swallowed: 86JP
// answers neither branch, so failing the caller would only tear down the
// connection for a packet the client sends on a timer.
func TestOnlineRaidSituationInvalidBodyWritesNothingAndDoesNotFail(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	connection.write.Reset()

	handled, err := service.handleOnlineRaidSituationMonsterHp(session, 695, []byte{0, 0, 0, 0, 1, 100, 0})
	if err != nil {
		t.Fatalf("invalid body returned an error: %v", err)
	}
	if !handled {
		t.Fatal("the invalid body was not claimed by the raid handler")
	}
	if wrote := connection.write.Len(); wrote != 0 {
		t.Fatalf("invalid body wrote %d bytes", wrote)
	}
}

func newRaidSituationTestFixture(t *testing.T) (*Service, *gameSession, *runtimeRaidPhaseTestConn, time.Time) {
	t.Helper()
	preparedAt := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	config := runtimeRaidBridgeTestAntonConfig()
	lifecycle, err := newRuntimeRaidLifecycleStateFromAntonModeAndType(config, preparedAt, false, 0)
	if err != nil {
		t.Fatalf("create lifecycle: %v", err)
	}
	member := runtimeRaidMemberState{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0}
	connection := &runtimeRaidPhaseTestConn{}
	const raidKey = uint32(0x00c80901)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids: map[uint32]*runtimeRaidState{
			raidKey: {
				RaidKey:        raidKey,
				LeaderID:       member.CharID,
				AntonConfig:    config,
				Members:        []runtimeRaidMemberState{member},
				FrozenMembers:  []runtimeRaidMemberState{member},
				Started:        true,
				EntryCostPaid:  true,
				ChargedMembers: []uint16{member.CharID},
				Lifecycle:      lifecycle,
			},
		},
	}
	session := &gameSession{
		conn:    connection,
		connID:  "raid-situation-test",
		channel: channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038},
	}
	service.bindGameSessionCharacter(session, member.CharID)
	if mode := runtimeRaidModeForSession(session); mode == 0 {
		t.Fatal("the fixture session is not on a raid channel")
	}
	state := service.raids[raidKey]
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	state.Lifecycle.PhaseStartedAt = preparedAt
	state.Lifecycle.PhaseDeadline = preparedAt.Add(40 * time.Minute)
	return service, session, connection, preparedAt
}

// raidSituationValueBody is the exact C2S 695 shape the live client produced:
// reserved dword, count at +4, then the values.
func raidSituationValueBody(count int) []byte {
	body := []byte{0, 0, 0, 0, byte(count)}
	for index := 0; index < count; index++ {
		body = append(body, byte(100-index), 0, 0, 0)
	}
	return body
}

func raidSituationFixtureRaidKey(t *testing.T, service *Service) uint32 {
	t.Helper()
	for key := range service.raids {
		return key
	}
	t.Fatal("fixture has no raid")
	return 0
}

// enterRaidSituationFixtureGate puts the fixture session inside one raid gate.
// Only the raid binding matters to the store, so the run itself stays minimal.
func enterRaidSituationFixtureGate(session *gameSession, raidKey uint32, phase byte, mapID int) {
	session.dungeon.mu.Lock()
	session.dungeon.runtime = &runtimeDungeonState{
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey:        raidKey,
			Phase:          phase,
			MapID:          mapID,
			LifecycleToken: 1,
		},
	}
	session.dungeon.mu.Unlock()
}

// A value report is attributed to the squad AND to the gate that squad is standing
// in, because that pair is what 634's rows are keyed by. Two gates must therefore
// leave two entries: keying by the squad alone would let a value reported inside
// 擎天之柱 be drawn in 舰炮.
func TestOnlineRaidSituationValueReportStoresPerSquadAndGate(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	connection.write.Reset()

	if _, err := service.handleOnlineRaidSituationMonsterHp(
		session, 695, raidSituationValueBody(3)); err != nil {
		t.Fatalf("value report returned an error: %v", err)
	}
	// A recorded report changes what every row on the board should draw, so it is
	// answered with the whole raid's row table. Asserting the exact bytes here is
	// deliberate: this packet REPLACES the client's table, so a wrong field order or
	// width is not a cosmetic bug.
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidMonsterStateMsgID {
		t.Fatalf("value report answered with class=%d msg=%#04x, want the board row packet %#04x",
			packet.Header.Classification, packet.Header.MsgID, raid.RaidMonsterStateMsgID)
	}
	if len(rest) != 0 {
		t.Fatalf("value report trailing bytes=%d", len(rest))
	}
	wantBody := []byte{
		1,    // groupCount
		1, 0, // A = squad 1
		1,          // memberCount
		0xE9, 0x03, // member 1001
		0, 0, 0, 0, // V0 = paid revive coins spent in this gate
		0, 0, 0, 0, // V1 = the reporting client's record-set key, echoed
		3, // valueCount
		100, 0, 0, 0, 99, 0, 0, 0, 98, 0, 0, 0,
	}
	if !bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("board row body=%x want=%x", packet.Body, wantBody)
	}

	jiandun := runtimeRaidMonsterRuntimeKey{RaidKey: raidKey, Group: 1, DungeonID: 213}
	stored := service.runtimeRaidMonsterRuntimeValuesFor(raidKey)
	values, ok := stored[jiandun]
	if !ok {
		t.Fatalf("the report was not stored under %+v; stored=%+v", jiandun, stored)
	}
	if len(values) != 3 {
		t.Fatalf("stored %d values, want 3: %v", len(values), values)
	}
	for index, want := range []uint32{100, 99, 98} {
		if values[index] != want {
			t.Fatalf("stored value %d=%d want=%d", index, values[index], want)
		}
	}

	// A second gate is a second entry, not a replacement of the first.
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 221)
	if _, err := service.handleOnlineRaidSituationMonsterHp(
		session, 695, raidSituationValueBody(1)); err != nil {
		t.Fatalf("second value report returned an error: %v", err)
	}
	stored = service.runtimeRaidMonsterRuntimeValuesFor(raidKey)
	if len(stored) != 2 {
		t.Fatalf("two gates produced %d entries, want 2: %+v", len(stored), stored)
	}
	if _, ok := stored[jiandun]; !ok {
		t.Fatalf("the first gate's entry was replaced: %+v", stored)
	}
	if _, ok := stored[runtimeRaidMonsterRuntimeKey{RaidKey: raidKey, Group: 1, DungeonID: 221}]; !ok {
		t.Fatalf("the second gate's entry is missing: %+v", stored)
	}
}

// The client sends this opcode on a 7-15s timer, so it also arrives in town and in
// ordinary dungeons. A report that cannot be attributed to a live raid gate is
// dropped, exactly like 86JP's non-participant branch (Situation.cs:233-236).
func TestOnlineRaidSituationValueReportOutsideAGateRecordsNothing(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	connection.write.Reset()

	if _, err := service.handleOnlineRaidSituationMonsterHp(
		session, 695, raidSituationValueBody(2)); err != nil {
		t.Fatalf("value report returned an error: %v", err)
	}
	if stored := service.runtimeRaidMonsterRuntimeValuesFor(raidKey); len(stored) != 0 {
		t.Fatalf("a report from outside a raid gate was stored: %+v", stored)
	}
}

// A report is only meaningful while the raid that produced it is alive; the purge
// runs where the raid itself is removed.
func TestRuntimeRaidMonsterRuntimeValuesArePurgedWithTheRaid(t *testing.T) {
	service, session, _, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	if _, err := service.handleOnlineRaidSituationMonsterHp(
		session, 695, raidSituationValueBody(2)); err != nil {
		t.Fatalf("value report returned an error: %v", err)
	}
	if stored := service.runtimeRaidMonsterRuntimeValuesFor(raidKey); len(stored) == 0 {
		t.Fatal("nothing was stored to purge")
	}

	service.raidMu.Lock()
	service.purgeRuntimeRaidMonsterRuntimeValuesLocked(raidKey)
	service.raidMu.Unlock()
	if stored := service.runtimeRaidMonsterRuntimeValuesFor(raidKey); len(stored) != 0 {
		t.Fatalf("the purge left %d entries: %+v", len(stored), stored)
	}
}

// setRaidSituationFixtureSpentCoins records how many revive coins the session's
// gate has spent, which is what the board renders as its coin row.
//
// It writes the raid-scoped ledger rather than a counter on the run, because that is
// what the board reads (runtimeRaidReviveCoinUses): the client subtracts the number
// we send from the GATE's own allowance, and a run is recreated on every re-entry,
// so a per-run counter would silently reset inside the same gate.
func setRaidSituationFixtureSpentCoins(
	t *testing.T,
	service *Service,
	session *gameSession,
	spent uint32,
) {
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
	state := service.raids[binding.RaidKey]
	if state == nil {
		service.raidMu.Unlock()
		t.Fatalf("raid %d is not live", binding.RaidKey)
	}
	if service.raidReviveUses == nil {
		service.raidReviveUses = make(map[runtimeRaidReviveCoinKey]uint32)
	}
	service.raidReviveUses[raidReviveCoinLedgerKey(*state, binding, session.selectedCharacterID)] = spent
	service.raidMu.Unlock()
}

// raidReviveCoinLedgerKey is the ledger entry one member's current gate owns: the
// same (raid, phase, row, gate) key the op41 commit path bills
// (recordRuntimeRaidReviveCoinUseLocked). Caller holds s.raidMu.
func raidReviveCoinLedgerKey(
	state runtimeRaidState,
	binding runtimeRaidDungeonBinding,
	charID uint16,
) runtimeRaidReviveCoinKey {
	key := runtimeRaidReviveCoinKey{
		RaidKey:   binding.RaidKey,
		Phase:     binding.Phase,
		DungeonID: binding.MapID,
	}
	if member, ok := runtimeRaidFrozenMember(state, charID); ok {
		key.Group = runtimeRaidMemberSquad(member.GroupIndex)
		if key.Group == 0 {
			key.SoloCharID = charID
		}
	}
	return key
}

// raidReviveCoinLedgerEntries copies the spent-coin ledger so a test can assert what
// the op41 commit path recorded.
func raidReviveCoinLedgerEntries(service *Service) map[runtimeRaidReviveCoinKey]uint32 {
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	entries := make(map[runtimeRaidReviveCoinKey]uint32, len(service.raidReviveUses))
	for key, uses := range service.raidReviveUses {
		entries[key] = uses
	}
	return entries
}

// addRaidSituationFixtureMember registers another ONLINE member of the same raid in
// its own squad, so whole-table emission can be checked with more than one row.
func addRaidSituationFixtureMember(
	t *testing.T,
	service *Service,
	raidKey uint32,
	charID uint16,
	group byte,
) (*gameSession, *runtimeRaidPhaseTestConn) {
	t.Helper()
	member := runtimeRaidMemberState{CharID: charID, Name: "mate", GroupIndex: group, SlotOrder: 0}
	service.raidMu.Lock()
	state := service.raids[raidKey]
	state.Members = append(state.Members, member)
	state.FrozenMembers = append(state.FrozenMembers, member)
	service.raidMu.Unlock()

	connection := &runtimeRaidPhaseTestConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "raid-situation-test-mate",
		channel: channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038},
	}
	service.bindGameSessionCharacter(session, charID)
	if _, online := service.onlineGameSession(charID); !online {
		t.Fatalf("member %d was not registered as online", charID)
	}
	return session, connection
}

// A body with groupCount 0 is NOT "nothing to say": the client clears its entire
// row table on it (0x17BF6AC, _probe_634_report.md section 6c Q1). So a raid with
// nobody standing in a gate must send no row packet at all. This is the regression
// guard for the sharpest edge in this feature.
func TestRuntimeRaidMonsterStateIsNeverAnEmptyTable(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	state := *service.raids[raidKey]

	body, ok, err := service.runtimeRaidMonsterStateBody(state)
	if err != nil || ok || body != nil {
		t.Fatalf("nobody in a gate produced body=%x ok=%t err=%v", body, ok, err)
	}
	connection.write.Reset()
	if err := service.sendRuntimeRaidMonsterState(session, state); err != nil {
		t.Fatalf("send with nobody in a gate: %v", err)
	}
	if wrote := connection.write.Len(); wrote != 0 {
		t.Fatalf("a raid with nobody in a gate wrote %d bytes; groupCount 0 would clear the client's table", wrote)
	}
}

// The client replaces its WHOLE row table with this body, so two squads in two
// gates must arrive as ONE packet carrying BOTH rows. Two packets, or a table with
// only the reporting squad, would leave the other squad's row deleted.
//
// It also pins the two per-row values the client consumes: V0 is that squad's own
// spent revive coins (the client renders `gate allowance - V0`), and V1 is the
// record-set key its own report used, echoed back.
func TestRuntimeRaidMonsterStateBroadcastsEverySquadInOneTable(t *testing.T) {
	service, leader, leaderConn, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(leader, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	setRaidSituationFixtureSpentCoins(t, service, leader, 2)

	mate, mateConn := addRaidSituationFixtureMember(t, service, raidKey, 1002, 2)
	enterRaidSituationFixtureGate(mate, raidKey, runtimeRaidDungeonPhaseTwo, 221)
	setRaidSituationFixtureSpentCoins(t, service, mate, 5)

	leaderConn.write.Reset()
	mateConn.write.Reset()
	handled, err := service.handleOnlineRaidSituationMonsterHp(leader, 695, raidSituationValueBody(2))
	if err != nil || !handled {
		t.Fatalf("report handled=%t err=%v", handled, err)
	}

	want := []byte{
		2,    // groupCount: BOTH squads in one table
		1, 0, // squad 1
		1,          // one member
		0xE9, 0x03, // 1001
		2, 0, 0, 0, // V0: squad 1 has spent 2 coins in 舰炮
		0, 0, 0, 0, // V1 echoed from squad 1's own report
		2,                         // valueCount
		100, 0, 0, 0, 99, 0, 0, 0, // the values squad 1 reported
		2, 0, // squad 2
		1,          // one member
		0xEA, 0x03, // 1002
		5, 0, 0, 0, // V0: squad 2 has spent 5 coins in its own gate
		0, 0, 0, 0, // V1 default, because squad 2 has not reported yet
		0, // squad 2 reported nothing, so it must not overwrite its own bars
	}
	for index, conn := range []*runtimeRaidPhaseTestConn{leaderConn, mateConn} {
		packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidMonsterStateMsgID {
			t.Fatalf("recipient %d msg=%#04x want %#04x",
				index, packet.Header.MsgID, raid.RaidMonsterStateMsgID)
		}
		if len(rest) != 0 {
			t.Fatalf("recipient %d trailing bytes=%d", index, len(rest))
		}
		if !bytes.Equal(packet.Body, want) {
			t.Fatalf("recipient %d body=%x want=%x", index, packet.Body, want)
		}
	}
}

// The client's 587 handler WIPES its row table, so every sequence that announces
// raid state has to carry the rows again, and after the announcement, or an open
// board stays blank until it is closed and reopened.
func TestRaidStateBroadcastRepublishesTheBoardRows(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	setRaidSituationFixtureSpentCoins(t, service, session, 1)
	connection.write.Reset()

	if err := service.broadcastRuntimeRaidWireSequence(
		*service.raids[raidKey],
		runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body:  raid.BuildRaidStateBody(raid.RaidState{State: 3}),
		},
	); err != nil {
		t.Fatalf("raid state broadcast: %v", err)
	}

	stream := connection.write.Bytes()
	statePacket, stream := splitGameServerUpperPacket(t, stream)
	if statePacket.Header.MsgID != raid.RaidStateMsgID {
		t.Fatalf("first packet msg=%#04x want the raid state %#04x",
			statePacket.Header.MsgID, raid.RaidStateMsgID)
	}
	rowPacket, stream := splitGameServerUpperPacket(t, stream)
	if rowPacket.Header.Classification != 0 || rowPacket.Header.MsgID != raid.RaidMonsterStateMsgID {
		t.Fatalf("second packet msg=%#04x want the board rows %#04x",
			rowPacket.Header.MsgID, raid.RaidMonsterStateMsgID)
	}
	if len(stream) != 0 {
		t.Fatalf("raid state broadcast trailing bytes=%d", len(stream))
	}
	wantBody := []byte{
		1,    // groupCount
		1, 0, // squad 1
		1,          // one member
		0xE9, 0x03, // 1001
		1, 0, 0, 0, // V0 = 1 spent coin
		0, 0, 0, 0, // V1 default
		0, // nothing reported yet
	}
	if !bytes.Equal(rowPacket.Body, wantBody) {
		t.Fatalf("board row body=%x want=%x", rowPacket.Body, wantBody)
	}
}

// The rule is tied to the 587 wipe, not to broadcasting in general: a sequence that
// does not announce raid state must not grow an extra packet.
func TestNonStateBroadcastDoesNotRepublishTheBoardRows(t *testing.T) {
	service, session, connection, _ := newRaidSituationTestFixture(t)
	raidKey := raidSituationFixtureRaidKey(t, service)
	enterRaidSituationFixtureGate(session, raidKey, runtimeRaidDungeonPhaseTwo, 213)
	connection.write.Reset()

	if err := service.broadcastRuntimeRaidWireSequence(
		*service.raids[raidKey],
		runtimeRaidWireNotification{msgID: raid.RaidSymbolMsgID},
	); err != nil {
		t.Fatalf("symbol broadcast: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != raid.RaidSymbolMsgID {
		t.Fatalf("msg=%#04x want the symbol packet %#04x", packet.Header.MsgID, raid.RaidSymbolMsgID)
	}
	if len(rest) != 0 {
		t.Fatalf("a non-state broadcast grew an extra packet: %x", rest)
	}
}
