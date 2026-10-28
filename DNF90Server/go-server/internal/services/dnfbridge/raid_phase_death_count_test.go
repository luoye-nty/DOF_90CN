package dnfbridge

import (
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// 服主 2026-09-29（单人卢克实测）：结算条上「死亡次数 0」，而他确定死了并用了复活币。
// 根因：0x0259 的 deaths 字段（`raid.RaidSettlementResult.DeathCount`）从来没被填过
// （结算只给 ResultType/PhaseIndex/ClearSeconds）。这一组测试钉住
// "计数边沿 → 编码进包 → 所有团本都带真实值" 三件事。
//
// 服主 2026-09-29 拍板：这个字段对**所有团本**都要填真实值（依据见
// raid_phase_death_count.go）。安徒恩以前恒 0，所以这里的安徒恩用例同时是
// "本次唯一有意的安徒恩变化就是这个 u16" 的逐字节对照。

// lukeSettlementDeathFixture is one raid with one online member, in the state a
// reached phase settlement implies.
func lukeSettlementDeathFixture(t *testing.T, mode byte, tailFlag byte) (*Service, *gameSession, uint32) {
	t.Helper()
	const raidKey = uint32(0x0009c001)
	channel := channelcatalog.Channel{ID: 28, Type: dnfenum.HiddenRaidType, Name: "luke", Port: 10028}
	if mode != runtimeRaidOwnerLuke {
		channel = channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 10023}
	}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	session := &gameSession{conn: &bufferConn{}, channel: channel}
	service.bindGameSessionCharacter(session, 1001)
	members := []runtimeRaidMemberState{{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0}}
	state := &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		RaidMode:       mode,
		TailFlag:       tailFlag,
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001},
		StartAttempt:   1,
		LukeConfig:     &raid.LukeConfig{},
		Lifecycle:      newRuntimeRaidLifecycleState(time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)),
	}
	service.raids[raidKey] = state
	seedRuntimeRaidPhaseClearsForTest(&state.Lifecycle, 1001)
	return service, session, raidKey
}

// lukeSettlementDeathRun is the gate binding one accepted death is recorded against.
func lukeSettlementDeathRun(raidKey uint32) *runtimeDungeonState {
	return &runtimeDungeonState{
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey: raidKey,
			Phase:   runtimeRaidDungeonPhaseOne,
			MapID:   lukeBirthSanctumMapID,
		},
	}
}

// settlementResultBodyFromDrain pulls the 0x0259 body out of one session's wire buffer.
func settlementResultBodyFromDrain(t *testing.T, data []byte) []byte {
	t.Helper()
	total := len(data)
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.MsgID == raid.RaidResultMsgID {
			return packet.Body
		}
		data = rest
	}
	t.Fatalf("no 0x0259 result in %d bytes", total)
	return nil
}

// TestLukePhaseSettlementCarriesThePhasesDeathCount is the end-to-end pin: two accepted
// deaths, one phase settlement, and the u16 the client prints as 死亡次数 must be 2.
func TestLukePhaseSettlementCarriesThePhasesDeathCount(t *testing.T) {
	service, session, raidKey := lukeSettlementDeathFixture(t, runtimeRaidOwnerLuke, 0)
	runtime := lukeSettlementDeathRun(raidKey)
	for i := 0; i < 2; i++ {
		service.recordRuntimeRaidPhaseDeath(session, runtime)
	}
	if got := service.runtimeRaidPhaseDeathCount(raidKey, runtimeRaidDungeonPhaseOne); got != 2 {
		t.Fatalf("phase deaths=%d want 2", got)
	}

	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0, ClearSeconds: 60},
		time.Date(2026, 9, 29, 3, 20, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("publish settlement: %v", err)
	}
	body := settlementResultBodyFromDrain(t, session.conn.(*bufferConn).write.Bytes())
	if len(body) != 10 {
		t.Fatalf("0x0259 body_len=%d want 10", len(body))
	}
	if got := binary.LittleEndian.Uint16(body[6:8]); got != 2 {
		t.Fatalf("0x0259 deaths=%d want 2 (body=% X)", got, body)
	}
	// Every other field keeps its old value.
	if body[0] != 0 || body[1] != 0 || binary.LittleEndian.Uint32(body[2:6]) != 60 {
		t.Fatalf("0x0259 header/clearSeconds moved: % X", body)
	}
	if body[8] != 0 || body[9] != 0 {
		t.Fatalf("0x0259 score/flags moved: % X", body)
	}
}

// TestAntonPhaseSettlementCarriesThePhasesDeathCount is the same end-to-end pin for
// the other faction: the u16 is the phase's whole-raid death count for Anton too
// (86JP RAID_RESULT's 4th argument; luke.etc [CONDITION FOR RANK] buckets by
// [DEAD COUNT]), so five accepted deaths must reach the wire as five.
//
// It is also the byte-for-byte control for "the death count is the ONLY Anton
// behaviour this change may move": the body is compared byte by byte against the
// exact 10 bytes the old code sent, with index 6..8 being the single difference.
func TestAntonPhaseSettlementCarriesThePhasesDeathCount(t *testing.T) {
	service, session, raidKey := lukeSettlementDeathFixture(t, runtimeRaidOwnerAnton, 0)
	runtime := lukeSettlementDeathRun(raidKey)
	for i := 0; i < 5; i++ {
		service.recordRuntimeRaidPhaseDeath(session, runtime)
	}
	if got := service.runtimeRaidPhaseDeathCount(raidKey, runtimeRaidDungeonPhaseOne); got != 5 {
		t.Fatalf("the ledger counts every raid: got %d want 5", got)
	}
	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0, ClearSeconds: 60},
		time.Date(2026, 9, 29, 3, 20, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("publish settlement: %v", err)
	}
	body := settlementResultBodyFromDrain(t, session.conn.(*bufferConn).write.Bytes())
	if len(body) != 10 {
		t.Fatalf("0x0259 body_len=%d want 10", len(body))
	}
	if got := binary.LittleEndian.Uint16(body[6:8]); got != 5 {
		t.Fatalf("Anton 0x0259 deaths=%d want 5 (body=% X)", got, body)
	}
	// 逐字节对照：这是旧代码（安徒恩恒 0）会发的那 10 个字节，唯一允许变的只有 [6:8]。
	before := []byte{0, 0, 60, 0, 0, 0, 0, 0, 0, 0}
	for index := range before {
		if index == 6 || index == 7 {
			continue
		}
		if body[index] != before[index] {
			t.Fatalf("Anton 0x0259 moved outside the death count at byte %d: body=% X before=% X",
				index, body, before)
		}
	}
	// 其余六个字段逐个点名，读失败时能直接看出是哪个字段动了。
	if body[0] != 0 || body[1] != 0 {
		t.Fatalf("Anton 0x0259 ResultType/PhaseIndex moved: % X", body)
	}
	if got := binary.LittleEndian.Uint32(body[2:6]); got != 60 {
		t.Fatalf("Anton 0x0259 ClearSeconds=%d want 60: % X", got, body)
	}
	if body[8] != 0 || body[9] != 0 {
		t.Fatalf("Anton 0x0259 Score/Flags moved: % X", body)
	}
	t.Logf("Anton 0x0259 before=% X after=% X (only [6:8] moved: %d -> %d)",
		before, body, binary.LittleEndian.Uint16(before[6:8]), binary.LittleEndian.Uint16(body[6:8]))
}

// TestAntonFailedPhaseReportCarriesThePhasesDeathCount covers the other 0x0259
// producer: the failure report. Both producers are shared, so both must carry the
// real value for every faction -- there is no "settlement only" carve-out.
func TestAntonFailedPhaseReportCarriesThePhasesDeathCount(t *testing.T) {
	service, session, raidKey := lukeSettlementDeathFixture(t, runtimeRaidOwnerAnton, 0)
	runtime := lukeSettlementDeathRun(raidKey)
	for i := 0; i < 3; i++ {
		service.recordRuntimeRaidPhaseDeath(session, runtime)
	}
	// publishFailedRuntimeRaid is the second 0x0259 producer and guards on the
	// lifecycle having already failed; the transition itself is covered by
	// TestRuntimeRaidPhaseTimeoutPublishesBeforeRemoval, so this fixture only has
	// to put the raid in the state that producer requires.
	service.raidMu.Lock()
	state := service.raids[raidKey]
	service.raidMu.Unlock()
	if state == nil {
		t.Fatal("the fixture raid disappeared")
	}
	state.Lifecycle.Status = runtimeRaidLifecycleFailed
	if err := service.publishFailedRuntimeRaid(*state); err != nil {
		t.Fatalf("publish failed raid: %v", err)
	}
	body := settlementResultBodyFromDrain(t, session.conn.(*bufferConn).write.Bytes())
	if len(body) != 10 {
		t.Fatalf("0x0259 body_len=%d want 10", len(body))
	}
	if got := binary.LittleEndian.Uint16(body[6:8]); got != 3 {
		t.Fatalf("failed Anton 0x0259 deaths=%d want 3 (body=% X)", got, body)
	}
	// 失败结果的固定形状仍然不变：ResultType=1、PhaseIndex=0、Score=0、Flags=1。
	if body[0] != runtimeRaidFailResultType || body[1] != 0 || body[8] != 0 || body[9] != 1 {
		t.Fatalf("failed 0x0259 shape moved: % X", body)
	}
}

// TestSingleModePhaseSettlementCarriesThePhasesDeathCount covers 「单人阶段也要」:
// the same producer on a single-mode raid (TailFlag == 1) must carry the count too.
// Nothing in the path branches on the tail flag -- this pin exists so that adding
// such a branch later cannot silently zero the field for single mode.
func TestSingleModePhaseSettlementCarriesThePhasesDeathCount(t *testing.T) {
	service, session, raidKey := lukeSettlementDeathFixture(t, runtimeRaidOwnerLuke, 1)
	if state := service.raids[raidKey]; state == nil || state.TailFlag != 1 {
		t.Fatalf("the fixture raid is not single mode: %+v", service.raids[raidKey])
	}
	runtime := lukeSettlementDeathRun(raidKey)
	for i := 0; i < 4; i++ {
		service.recordRuntimeRaidPhaseDeath(session, runtime)
	}
	if err := service.publishRuntimeRaidPhaseSettlement(
		raidKey,
		runtimeRaidDungeonPhaseOne,
		raid.RaidSettlementResult{PhaseIndex: 0, ClearSeconds: 90},
		time.Date(2026, 9, 29, 3, 20, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("publish single-mode settlement: %v", err)
	}
	body := settlementResultBodyFromDrain(t, session.conn.(*bufferConn).write.Bytes())
	if len(body) != 10 {
		t.Fatalf("0x0259 body_len=%d want 10", len(body))
	}
	if got := binary.LittleEndian.Uint16(body[6:8]); got != 4 {
		t.Fatalf("single-mode 0x0259 deaths=%d want 4 (body=% X)", got, body)
	}
	if body[0] != 0 || body[1] != 0 || binary.LittleEndian.Uint32(body[2:6]) != 90 || body[8] != 0 || body[9] != 0 {
		t.Fatalf("single-mode 0x0259 moved outside the death count: % X", body)
	}
}

// TestPhaseDeathCountFollowsTheLivePhase keeps the ledger honest: a death reported for
// a phase that is no longer running is not part of any result body.
func TestPhaseDeathCountFollowsTheLivePhase(t *testing.T) {
	service, session, raidKey := lukeSettlementDeathFixture(t, runtimeRaidOwnerLuke, 0)
	service.recordRuntimeRaidPhaseDeath(session, lukeSettlementDeathRun(raidKey))

	stale := lukeSettlementDeathRun(raidKey)
	stale.raidDungeon.Phase = runtimeRaidDungeonPhaseTwo
	service.recordRuntimeRaidPhaseDeath(session, stale)
	if got := service.runtimeRaidPhaseDeathCount(raidKey, runtimeRaidDungeonPhaseTwo); got != 0 {
		t.Fatalf("a stale phase recorded %d deaths", got)
	}
	if got := service.runtimeRaidPhaseDeathCount(raidKey, runtimeRaidDungeonPhaseOne); got != 1 {
		t.Fatalf("phase one deaths=%d want 1", got)
	}

	service.raidMu.Lock()
	service.purgeRuntimeRaidPhaseDeathsLocked(raidKey)
	service.raidMu.Unlock()
	if got := service.runtimeRaidPhaseDeathCount(raidKey, runtimeRaidDungeonPhaseOne); got != 0 {
		t.Fatalf("purge left %d deaths behind", got)
	}
}

// TestAcceptedCharacterDeathIncrementsTheRaidPhaseCount drives the real C2S op40
// handler on a raid gate: the accepted death edge is what feeds the counter.
func TestAcceptedCharacterDeathIncrementsTheRaidPhaseCount(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	fixture.service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
	session, ok := fixture.service.onlineGameSession(99)
	if !ok || session == nil {
		t.Fatal("char 99 has no session")
	}
	fixture.enterGateRun(t, 99, 21, antonBlackFogSourceMapID)

	// A body of the wrong width is refused before anything is counted.
	if err := fixture.service.handleDungeonDieCharacterLocal(session, []byte{0}); err != nil {
		t.Fatalf("short op40: %v", err)
	}
	if got := fixture.service.runtimeRaidPhaseDeathCount(fixture.raidKey, runtimeRaidDungeonPhaseOne); got != 0 {
		t.Fatalf("a malformed op40 counted %d deaths", got)
	}

	if err := fixture.service.handleDungeonDieCharacterLocal(session, []byte{0, 0, 0, 0}); err != nil {
		t.Fatalf("op40: %v", err)
	}
	if got := fixture.service.runtimeRaidPhaseDeathCount(fixture.raidKey, runtimeRaidDungeonPhaseOne); got != 1 {
		t.Fatalf("accepted death counted %d deaths want 1", got)
	}
}
